package web

// Personas speak in the call (spec 013): the lines the tts pod says, handed over on an event
// stream, the rooms its bots join, and the states it reports back. Shared by every server through
// the store (017 I18 to I21).

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/bus"
	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/voices"
)

const (
	// maxTTSLines bounds the lines kept for a pod that is away; each also expires (013-AC14).
	maxTTSLines = 500
	// ttsLapse is how long the pod's last heartbeat stands: past it, its states clear, which the
	// room's 1 s poll must show within AC29's 10 s.
	ttsLapse = 8 * time.Second
	// ttsPing keeps proxies from closing an idle stream.
	ttsPing = 15 * time.Second
	// recentForLang is how many lines a room's language is guessed from (013-AC16).
	recentForLang  = 50
	maxTTSPodName  = 64
	maxTTSRooms    = 1000
	maxTTSStates   = 10000
	maxTTSReasonSz = 32
	// coldQueueS is how long a line queued with no pod up (streaming and heartbeating) waits for
	// one: scaling from zero took 77 s on the lab (013-AC34).
	coldQueueS = 120
	// ttsLease is how long an open stream counts as the TTS service's after its last renewal, every
	// ttsRenew.
	ttsLease = 15 * time.Second
	ttsRenew = 5 * time.Second
	// ttsWake and lexiconWake are the bus ids that wake TTS streams: a line queued, the lexicon
	// changed, on any server.
	ttsWake     = ":tts"
	lexiconWake = ":lexicon"
)

// TTSStates are the states the pod reports per persona; TTSDropReasons why it dropped a line.
var (
	TTSStates      = []string{"waiting", "speaking", "idle", "dropped"}
	TTSDropReasons = []string{"interrupted", "stale", "no_listener", "no_service", "error"}
)

// ttsTimeouts are a room's turn-taking settings, sent with each line (013-AC25).
type ttsTimeouts struct {
	QuietMS int `json:"quiet_ms"`
	ResumeS int `json:"resume_s"`
	QueueS  int `json:"queue_s"`
}

func timeoutsOf(sp store.Speech) ttsTimeouts {
	return ttsTimeouts{QuietMS: sp.QuietMS, ResumeS: sp.ResumeS, QueueS: sp.QueueS}
}

var defaultTTSTimeouts = timeoutsOf(store.DefaultSpeech)

// ttsLine is one line a persona says in a room's call (013-AC14).
type ttsLine struct {
	Seq       int64   `json:"seq"`
	Room      string  `json:"room"`
	PersonaID int64   `json:"persona_id"`
	Persona   string  `json:"persona"`
	Voice     string  `json:"voice"`
	Rate      float64 `json:"rate"`
	Text      string  `json:"text"`
	ttsTimeouts
	QueuedAt time.Time `json:"queued_at"`
}

// ttsState is what the pod reports for one persona of a room (013-AC14).
type ttsState struct {
	Room      string `json:"room"`
	PersonaID int64  `json:"persona_id"`
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
}

// ttsHub caches the TTS service's row for the room streams and counts this server's voice lines.
// The lines, the heartbeat and the lexicon version live in the store, shared by every pod.
type ttsHub struct {
	st *store.Store
	// queued and noService count lines queued here, and lines this server dropped before any TTS
	// service took them.
	queued, noService atomic.Int64
	row               podCache[ttsRow]
}

// ttsRow is the TTS service's row with its states decoded.
type ttsRow struct {
	pod    store.TTSPod
	states []ttsState
}

func newTTSHub(st *store.Store) *ttsHub { return &ttsHub{st: st} }

// forget drops the cached row after a write here, so this pod reads its own write.
func (h *ttsHub) forget() { h.row.drop("") }

// beating reports a TTS service heartbeat within ttsLapse of now.
func beating(p store.TTSPod, now time.Time) bool {
	return p.BeatAtMS > 0 && now.UnixMilli()-p.BeatAtMS <= ttsLapse.Milliseconds()
}

// ttsPod is the TTS service's row, the cached one when the store fails.
func (s *Server) ttsPod() (store.TTSPod, []ttsState) {
	r := s.tts.row.get("", func() (ttsRow, error) {
		ctx, cancel := context.WithTimeout(context.Background(), sharedTimeout)
		defer cancel()
		r, err := s.ttsRow(ctx)
		if err != nil {
			s.log.Warn("tts_pod_read_failed", zap.Error(err))
		}
		return r, err
	})
	return r.pod, r.states
}

func (s *Server) ttsRow(ctx context.Context) (ttsRow, error) {
	p, err := s.tts.st.TTSPodState(ctx)
	if err != nil {
		return ttsRow{}, err
	}
	r := ttsRow{pod: p}
	if len(p.States) > 0 {
		if err := json.Unmarshal(p.States, &r.states); err != nil {
			return ttsRow{}, fmt.Errorf("tts pod states: %w", err)
		}
	}
	return r, nil
}

// TTSStats are the room server's own voice counters (013-AC20): lines queued, lines dropped
// because no pod took them in time, and whether a pod heartbeats. The pod exports the rest.
type TTSStats struct {
	Queued, NoService int64
	PodUp             bool
}

// TTSStats reports them, from memory; false when the server offers no voice.
func (s *Server) TTSStats() (TTSStats, bool) {
	if s.tts == nil {
		return TTSStats{}, false
	}
	return TTSStats{Queued: s.tts.queued.Load(), NoService: s.tts.noService.Load(), PodUp: s.gauges.ttsPodUp.Load()}, true
}

// addTTSLine queues l for the TTS service streaming from any server and wakes its stream.
func (s *Server) addTTSLine(ctx context.Context, l ttsLine) (int64, error) {
	now, lease := s.now(), s.lease()
	if p, _ := s.ttsPod(); !beating(p, lease) || p.StreamUntilMS <= lease.UnixMilli() {
		// The TTS service is scaling from zero, or away: the line waits for it, longer than behind
		// speech. A stream can outlive its TTS pod (no write deadline), its heartbeat cannot.
		l.QueueS = max(l.QueueS, coldQueueS)
	}
	seq, dropped, err := s.bus.Store.AddTTSLine(ctx, store.TTSLine{Room: l.Room, PersonaID: l.PersonaID, Persona: l.Persona, Voice: l.Voice,
		Rate: l.Rate, Text: l.Text, QuietMS: l.QuietMS, ResumeS: l.ResumeS, QueueS: l.QueueS, QueuedAtMS: l.QueuedAt.UnixMilli()}, maxTTSLines, now.UnixMilli())
	if err != nil {
		return 0, err
	}
	s.tts.noService.Add(dropped)
	s.tts.queued.Add(1)
	s.bus.Notify(ttsWake)
	return seq, nil
}

func lineOf(l store.TTSLine) ttsLine {
	return ttsLine{Seq: l.Seq, Room: l.Room, PersonaID: l.PersonaID, Persona: l.Persona, Voice: l.Voice, Rate: l.Rate, Text: l.Text,
		ttsTimeouts: ttsTimeouts{QuietMS: l.QuietMS, ResumeS: l.ResumeS, QueueS: l.QueueS}, QueuedAt: time.UnixMilli(l.QueuedAtMS)}
}

func (s *Server) ttsAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if tok == "" || subtle.ConstantTimeCompare([]byte(HashToken(tok)), []byte(s.opts.TTSTokenHash)) != 1 {
			httpError(w, http.StatusUnauthorized, "TTS service token required")
			return
		}
		next(w, r)
	})
}

// speakingWords is the word cap for persona's spoken line in room when it speaks in this turn:
// the server offers voice, the persona speaks in the call and a human is in the room's audio; 0
// otherwise (013-AC1).
func (s *Server) speakingWords(room *store.Room, persona *store.RoomPersona) int {
	if s.tts == nil || !persona.Speaks || !s.people.listening(s.lease())[room.ID] {
		return 0
	}
	return room.Speech.Words
}

// spokenWords is room's word cap, or the widest one: the chat's spoken text never fails a turn (013-AC40).
func (s *Server) spokenWords(ctx context.Context, room string) int {
	rm, err := s.bus.Store.Room(ctx, room)
	if err != nil {
		s.log.Warn("spoken_words_failed", zap.String("room", room), zap.Error(err))
		return store.SpeechMax.Words
	}
	return rm.Speech.Words
}

// roomLang is room's language from its recent lines, fallback when they tell none (013-AC16).
func (s *Server) roomLang(ctx context.Context, room, fallback string) string {
	texts, err := s.bus.Store.RecentText(ctx, room, recentForLang)
	if err != nil {
		s.log.Warn("room_lang_failed", zap.String("room", room), zap.Error(err))
	}
	return textLang(fallback, texts...)
}

// withVoices gives each speaking persona of ps with no voice its default and stores it, so it
// never shifts as other personas come and go (013-AC16).
func (s *Server) withVoices(ctx context.Context, room string, ps []store.RoomPersona, fallback string) {
	if !slices.ContainsFunc(ps, func(p store.RoomPersona) bool { return p.Speaks && p.Voice == "" }) {
		return
	}
	lang := s.roomLang(ctx, room, fallback)
	// Picked and stored in one transaction, from the store: another call may have stored voices
	// since ps was read, on this pod or another.
	voiceOf, err := s.bus.Store.DefaultVoices(ctx, room, func(used map[string]bool) string {
		v := voices.Default(lang, used)
		if used[v] {
			s.log.Warn("voice_shared", zap.String("room", room), zap.String("voice", v))
		}
		return v
	})
	if err != nil {
		s.log.Warn("default_voice_failed", zap.String("room", room), zap.Error(err))
		return
	}
	for i := range ps {
		v, ok := voiceOf[ps[i].ID]
		if !ok {
			continue
		}
		if ps[i].Voice == "" && v != "" {
			s.log.Info("voice_defaulted", zap.String("room", room), zap.Int64("persona", ps[i].ID), zap.String("voice", v), zap.String("lang", lang))
		}
		ps[i].Voice = v
	}
}

// queueSpeech queues what the persona of turn t says in the call: its spoken.md, or the summary
// of its actions when it wrote none (013-AC3, 013-AC4). The words never reach a log.
func (s *Server) queueSpeech(ctx context.Context, t *store.Turn, spoken, synthesis string) {
	if s.tts == nil {
		return
	}
	// Nobody in the call: nothing to say, and nothing to count as dropped.
	if !s.people.listening(s.lease())[t.RoomID] {
		s.log.Debug("speech_skipped", zap.String("room", t.RoomID), zap.Int64("turn", t.ID), zap.String("reason", "no_listener"))
		return
	}
	ps, err := s.bus.Store.RoomPersonas(ctx, t.RoomID)
	if err != nil {
		s.log.Warn("speech_skipped", zap.String("room", t.RoomID), zap.Error(err))
		return
	}
	i := slices.IndexFunc(ps, func(p store.RoomPersona) bool { return p.ID == t.PersonaID })
	if i < 0 || !ps[i].Speaks {
		s.log.Debug("speech_skipped", zap.String("room", t.RoomID), zap.Int64("turn", t.ID), zap.String("reason", "silent"))
		return
	}
	room, err := s.bus.Store.Room(ctx, t.RoomID)
	if err != nil {
		s.log.Warn("speech_skipped", zap.String("room", t.RoomID), zap.Error(err))
		return
	}
	words := room.Speech.Words
	text, source := spokenText(spoken, words), "spoken"
	if text == "" {
		text, source = capWords(turnSummary(t.Report, synthesis), words), "summary"
	}
	if text == "" {
		return
	}
	s.withVoices(ctx, t.RoomID, ps, "en")
	p := ps[i]
	seq, err := s.addTTSLine(ctx, ttsLine{Room: t.RoomID, PersonaID: p.ID, Persona: p.Name, Voice: p.Voice, Rate: p.VoiceRate, Text: text,
		ttsTimeouts: timeoutsOf(room.Speech), QueuedAt: s.now()})
	if err != nil {
		s.log.Warn("speech_skipped", zap.String("room", t.RoomID), zap.Int64("turn", t.ID), zap.Error(err))
		return
	}
	s.log.Info("speech_queued", zap.String("room", t.RoomID), zap.Int64("turn", t.ID), zap.Int64("seq", seq), zap.String("source", source),
		zap.Int("words", len(wordRE.FindAllString(text, -1))))
}

// ttsPersona is a speaking persona of a room the pod's bots join (013-AC15).
type ttsPersona struct {
	ID    int64   `json:"id"`
	Name  string  `json:"name"`
	Voice string  `json:"voice"`
	Rate  float64 `json:"rate"`
}

type ttsRoom struct {
	Room     string       `json:"room"`
	Personas []ttsPersona `json:"personas"`
}

// TTSDemand is the number of rooms with a persona that speaks and a human who has the room open,
// in the audio or not: the pod starts scaling from zero before anyone joins the call (013-AC34).
// It reads the last refreshGauges.
func (s *Server) TTSDemand() int { return int(s.gauges.ttsDemand.Load()) }

func (s *Server) ttsDemand() int {
	ctx, cancel := context.WithTimeout(context.Background(), sharedTimeout)
	defer cancel()
	n, err := s.bus.Store.SpeakingRooms(ctx, slices.Collect(maps.Keys(s.people.present(s.lease()))))
	if err != nil {
		s.log.Warn("tts_demand_read_failed", zap.Error(err))
	}
	return n
}

func speaking(p store.RoomPersona) bool { return p.Speaks && !p.Pending }

// ttsRoster lists the rooms with a human in the audio and their speaking personas (013-AC14).
func (s *Server) ttsRoster(ctx context.Context) []ttsRoom {
	out := []ttsRoom{}
	listening := s.people.listening(s.lease())
	rooms := make([]string, 0, len(listening))
	for r := range listening {
		rooms = append(rooms, r)
	}
	slices.Sort(rooms)
	for _, room := range rooms {
		ps, err := s.bus.Store.RoomPersonas(ctx, room)
		if err != nil {
			continue
		}
		s.withVoices(ctx, room, ps, "en")
		r := ttsRoom{Room: room, Personas: []ttsPersona{}}
		for _, p := range ps {
			if speaking(p) {
				r.Personas = append(r.Personas, ttsPersona{ID: p.ID, Name: p.Name, Voice: p.Voice, Rate: p.VoiceRate})
			}
		}
		if len(r.Personas) > 0 {
			out = append(out, r)
		}
	}
	return out
}

// ttsEvents streams to the pod the rooms its bots join and the lines to say, resuming after
// Last-Event-ID (013-AC14). Lines and the lexicon version come from the store, so any server
// streams them; the bus wakes the stream when one moves on any server.
func (s *Server) ttsEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	ctx := r.Context()
	live, cancel := s.bus.Subscribe(16)
	defer cancel()
	resume, top, err := s.bus.Store.TTSCursor(ctx)
	if err != nil {
		s.serverError(w, "tts_cursor_failed", err)
		return
	}
	last, err := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	switch {
	case err != nil:
		last = resume
	case last > top:
		// From a server whose clock ran ahead: every line kept here is new to it.
		last = 0
	}
	var until int64
	renew := func() {
		until = s.lease().Add(ttsLease).UnixMilli()
		if err := s.bus.Store.RenewTTSStream(ctx, until); err != nil {
			s.log.Warn("tts_stream_lease_failed", zap.Error(err))
		}
		s.tts.forget()
	}
	renew()
	// Lines queued from now on get the cold budget at once, not after the lease runs out.
	defer func() {
		if err := s.bus.Store.ReleaseTTSStream(context.WithoutCancel(ctx), until); err != nil {
			s.log.Warn("tts_stream_lease_failed", zap.Error(err))
		}
		s.tts.forget()
		s.log.Info("tts_disconnected")
	}()
	s.log.Info("tts_connected", zap.Int64("after", last))
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	sentRooms := ""
	sendRooms := func() bool {
		b, _ := json.Marshal(map[string]any{"rooms": s.ttsRoster(ctx)}) // plain values cannot fail to marshal
		if string(b) == sentRooms {
			return true
		}
		sentRooms = string(b)
		_, err := fmt.Fprintf(w, "event: rooms\ndata: %s\n\n", b)
		return err == nil
	}
	sentLexicon := ""
	sendLexicon := func() bool {
		version, err := s.bus.Store.Setting(ctx, store.LexiconVersionKey)
		if err != nil {
			s.log.Error("tts_lexicon_failed", zap.Error(err))
			return false
		}
		if version == sentLexicon {
			return true
		}
		ps, err := s.bus.Store.Pronunciations(ctx)
		if err != nil {
			s.log.Error("tts_lexicon_failed", zap.Error(err))
			return false
		}
		terms := make([]voices.Term, len(ps))
		for i, p := range ps {
			terms[i] = p.Term
		}
		b, _ := json.Marshal(map[string]any{"entries": terms}) // plain values cannot fail to marshal
		if _, err = fmt.Fprintf(w, "event: lexicon\ndata: %s\n\n", b); err != nil {
			return false
		}
		sentLexicon = version
		return true
	}
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	lease := time.NewTicker(ttsRenew)
	defer lease.Stop()
	pinged := time.Now()
	for {
		// At connect, then on each change: the TTS service speaks with the seed until it gets the lexicon.
		// Before the lines, which are marked sent as read.
		if !sendRooms() || !sendLexicon() {
			return
		}
		ls, err := s.bus.Store.TTSLinesAfter(ctx, last, s.now().UnixMilli())
		if err != nil {
			s.log.Error("tts_lines_failed", zap.Error(err))
			return
		}
		for _, sl := range ls {
			l := lineOf(sl)
			b, _ := json.Marshal(l) // plain values cannot fail to marshal
			if _, err := fmt.Fprintf(w, "id: %d\nevent: line\ndata: %s\n\n", l.Seq, b); err != nil {
				return
			}
			last = l.Seq
		}
		if time.Since(pinged) >= ttsPing {
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			pinged = time.Now()
		}
		flusher.Flush()
	wait:
		for {
			select {
			case <-ctx.Done():
				return
			case changed := <-live:
				if changed == ttsWake || changed == lexiconWake || changed == bus.All {
					break wait
				}
			case <-lease.C:
				renew()
			case <-tick.C:
				break wait
			}
		}
	}
}

// ttsHeartbeat records the pod's rooms, bots and persona states (013-AC14).
func (s *Server) ttsHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Pod    string     `json:"pod"`
		Rooms  []string   `json:"rooms"`
		Bots   []string   `json:"bots"`
		States []ttsState `json:"states"`
	}
	if !decode(w, r, &req) {
		return
	}
	if len(req.Pod) == 0 || len(req.Pod) > maxTTSPodName || len(req.Rooms) > maxTTSRooms || len(req.Bots) > maxTTSStates || len(req.States) > maxTTSStates {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("pod must be 1 to %d characters, rooms at most %d, bots and states at most %d",
			maxTTSPodName, maxTTSRooms, maxTTSStates))
		return
	}
	for _, st := range req.States {
		if !slices.Contains(TTSStates, st.State) || len(st.Reason) > maxTTSReasonSz ||
			st.State == "dropped" && !slices.Contains(TTSDropReasons, st.Reason) || st.State != "dropped" && st.Reason != "" {
			httpError(w, http.StatusBadRequest, fmt.Sprintf("state must be one of %v; a dropped one gives a reason among %v", TTSStates, TTSDropReasons))
			return
		}
	}
	states, _ := json.Marshal(req.States) // plain values cannot fail to marshal
	if err := s.bus.Store.BeatTTSPod(r.Context(), store.TTSPod{Pod: req.Pod, Rooms: req.Rooms, Bots: req.Bots, States: states}, s.lease().UnixMilli()); err != nil {
		s.serverError(w, "tts_heartbeat_failed", err)
		return
	}
	s.tts.forget()
	w.WriteHeader(http.StatusNoContent)
}

// ttsRoomEvent is the room's "tts" event: whether the pod is up, and each persona's state while it
// is; a pod silent past ttsLapse clears them all (013-AC29).
func (s *Server) ttsRoomEvent(room string) string {
	p, sts := s.ttsPod()
	up := beating(p, s.lease())
	states := map[string]ttsState{}
	if up {
		for _, st := range sts {
			if st.Room == room && st.State != "idle" {
				states[strconv.FormatInt(st.PersonaID, 10)] = st
			}
		}
	}
	b, _ := json.Marshal(map[string]any{"pod": up, "personas": states}) // plain values cannot fail to marshal
	return "event: tts\ndata: " + string(b) + "\n\n"
}
