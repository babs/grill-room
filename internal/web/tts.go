package web

// Personas speak in the call (spec 013): the lines the tts pod says, handed over on an event
// stream, the rooms its bots join, and the states it reports back. In memory: a spoken line may be
// lost on a restart, the chat keeps its text.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

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
	sent     bool
}

// ttsState is what the pod reports for one persona of a room (013-AC14).
type ttsState struct {
	Room      string `json:"room"`
	PersonaID int64  `json:"persona_id"`
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
}

// ttsHub holds the lines waiting for the pod and the pod's last heartbeat.
type ttsHub struct {
	mu sync.Mutex
	// seq starts from the clock, so a pod resuming with Last-Event-ID after a server restart asks
	// for lines below every new one instead of skipping them.
	seq   int64
	lines []*ttsLine
	// wake is closed and replaced when a line is added.
	wake    chan struct{}
	streams int
	beatAt  time.Time
	pod     string
	rooms   []string
	bots    []string
	states  []ttsState
	// queued and noService count lines queued, and lines that expired before any pod took them.
	queued, noService atomic.Int64
	// voiceMu makes picking and storing default voices one step: two personas picked at once from
	// two snapshots would get the same voice (013-AC16).
	voiceMu sync.Mutex
}

func newTTSHub(now time.Time) *ttsHub {
	return &ttsHub{seq: now.UnixMicro(), wake: make(chan struct{})}
}

// sweep drops the lines past their room's queue maximum; one no pod took counts as no_service.
// The caller holds the lock.
func (h *ttsHub) sweep(now time.Time) {
	h.lines = slices.DeleteFunc(h.lines, func(l *ttsLine) bool {
		if now.Sub(l.QueuedAt) <= time.Duration(l.QueueS)*time.Second {
			return false
		}
		if !l.sent {
			h.noService.Add(1)
		}
		return true
	})
}

func (h *ttsHub) add(l ttsLine, now time.Time) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sweep(now)
	h.seq++
	l.Seq = h.seq
	h.lines = append(h.lines, &l)
	if n := len(h.lines) - maxTTSLines; n > 0 {
		for _, old := range h.lines[:n] {
			if !old.sent {
				h.noService.Add(1)
			}
		}
		h.lines = slices.Delete(h.lines, 0, n)
	}
	h.queued.Add(1)
	close(h.wake)
	h.wake = make(chan struct{})
	return l.Seq
}

// next returns the lines after seq, marked sent, and the channel closed by the next add.
func (h *ttsHub) next(seq int64, now time.Time) ([]ttsLine, <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sweep(now)
	var out []ttsLine
	for _, l := range h.lines {
		if l.Seq > seq {
			l.sent = true
			out = append(out, *l)
		}
	}
	return out, h.wake
}

// resumePoint is where a pod with no Last-Event-ID starts: after the newest line any pod took. A
// restarted pod would otherwise say again what its previous run said.
func (h *ttsHub) resumePoint() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	last := h.seq
	if len(h.lines) > 0 {
		last = h.lines[0].Seq - 1
	}
	for _, l := range h.lines {
		if l.sent {
			last = l.Seq
		}
	}
	return last
}

// podUp reports a pod heartbeat within ttsLapse.
func (h *ttsHub) podUp(now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.beatAt.IsZero() && now.Sub(h.beatAt) <= ttsLapse
}

// noServiceDropped counts the lines that expired before any pod took them.
func (h *ttsHub) noServiceDropped(now time.Time) int64 {
	h.mu.Lock()
	h.sweep(now)
	h.mu.Unlock()
	return h.noService.Load()
}

// TTSStats are the room server's own voice counters (013-AC20): lines queued, lines dropped
// because no pod took them in time, and whether a pod heartbeats. The pod exports the rest.
type TTSStats struct {
	Queued, NoService int64
	PodUp             bool
}

// TTSStats reports them; false when the server offers no voice.
func (s *Server) TTSStats() (TTSStats, bool) {
	if s.tts == nil {
		return TTSStats{}, false
	}
	now := s.now()
	return TTSStats{Queued: s.tts.queued.Load(), NoService: s.tts.noServiceDropped(now), PodUp: s.tts.podUp(now)}, true
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
	if s.tts == nil || !persona.Speaks || !s.people.listening(s.now())[room.ID] {
		return 0
	}
	return room.Speech.Words
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
	s.tts.voiceMu.Lock()
	defer s.tts.voiceMu.Unlock()
	// Under the lock, from the store: another call may have stored voices since ps was read.
	fresh, err := s.bus.Store.RoomPersonas(ctx, room)
	if err != nil {
		s.log.Warn("default_voice_failed", zap.String("room", room), zap.Error(err))
		return
	}
	voiceOf := map[int64]string{}
	for _, p := range fresh {
		voiceOf[p.ID] = p.Voice
	}
	for i := range ps {
		if v, ok := voiceOf[ps[i].ID]; ok {
			ps[i].Voice = v
		}
	}
	lang := ""
	for i := range ps {
		p := &ps[i]
		if !p.Speaks || p.Voice != "" {
			continue
		}
		if lang == "" {
			lang = s.roomLang(ctx, room, fallback)
		}
		used := map[string]bool{}
		for _, o := range ps {
			used[o.Voice] = o.Voice != ""
		}
		v := voices.Default(lang, used)
		if used[v] {
			s.log.Warn("voice_shared", zap.String("room", room), zap.Int64("persona", p.ID), zap.String("voice", v))
		}
		stored, err := s.bus.Store.DefaultVoice(ctx, room, p.ID, v)
		if err != nil {
			s.log.Warn("default_voice_failed", zap.String("room", room), zap.Int64("persona", p.ID), zap.Error(err))
			continue
		}
		if stored {
			s.log.Info("voice_defaulted", zap.String("room", room), zap.Int64("persona", p.ID), zap.String("voice", v), zap.String("lang", lang))
			p.Voice = v
			continue
		}
		// Someone set it meanwhile: theirs stands.
		if q, err := s.bus.Store.RoomPersona(ctx, room, p.ID); err == nil {
			p.Voice = q.Voice
		}
	}
}

// queueSpeech queues what the persona of turn t says in the call: its spoken.md, or the summary
// of its actions when it wrote none (013-AC3, 013-AC4). The words never reach a log.
func (s *Server) queueSpeech(ctx context.Context, t *store.Turn, spoken, synthesis string) {
	if s.tts == nil {
		return
	}
	// Nobody in the call: nothing to say, and nothing to count as dropped.
	if !s.people.listening(s.now())[t.RoomID] {
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
	seq := s.tts.add(ttsLine{Room: t.RoomID, PersonaID: p.ID, Persona: p.Name, Voice: p.Voice, Rate: p.VoiceRate, Text: text,
		ttsTimeouts: timeoutsOf(room.Speech), QueuedAt: s.now()}, s.now())
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

// ttsRoster lists the rooms with a human in the audio and their speaking personas (013-AC14).
func (s *Server) ttsRoster(ctx context.Context) []ttsRoom {
	out := []ttsRoom{}
	listening := s.people.listening(s.now())
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
			if p.Speaks && !p.Pending {
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
// Last-Event-ID (013-AC14).
func (s *Server) ttsEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	last, err := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	if err != nil {
		last = s.tts.resumePoint()
	}
	s.tts.mu.Lock()
	s.tts.streams++
	if last > s.tts.seq {
		// From a server whose clock ran ahead: every line kept here is new to it.
		last = 0
	}
	s.tts.mu.Unlock()
	defer func() {
		s.tts.mu.Lock()
		s.tts.streams--
		s.tts.mu.Unlock()
		s.log.Info("tts_disconnected")
	}()
	s.log.Info("tts_connected", zap.Int64("after", last))
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	ctx := r.Context()
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
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	pinged := time.Now()
	for {
		if !sendRooms() {
			return
		}
		lines, wake := s.tts.next(last, s.now())
		for _, l := range lines {
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
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-tick.C:
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
	s.tts.mu.Lock()
	s.tts.beatAt, s.tts.pod, s.tts.rooms, s.tts.bots, s.tts.states = s.now(), req.Pod, req.Rooms, req.Bots, req.States
	s.tts.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// ttsRoomEvent is the room's "tts" event: whether the pod is up, and each persona's state while it
// is; a pod silent past ttsLapse clears them all (013-AC29).
func (s *Server) ttsRoomEvent(room string) string {
	h := s.tts
	h.mu.Lock()
	up := !h.beatAt.IsZero() && s.now().Sub(h.beatAt) <= ttsLapse
	states := map[string]ttsState{}
	if up {
		for _, st := range h.states {
			if st.Room == room && st.State != "idle" {
				states[strconv.FormatInt(st.PersonaID, 10)] = st
			}
		}
	}
	h.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"pod": up, "personas": states}) // plain values cannot fail to marshal
	return "event: tts\ndata: " + string(b) + "\n\n"
}
