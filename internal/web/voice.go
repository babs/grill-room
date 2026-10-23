package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/devicestt"
	"github.com/babs/grill-room/internal/livekit"
	"github.com/babs/grill-room/internal/store"
)

const (
	// voiceTokenTTL is short because LiveKit cannot revoke a token: a kicked participant could
	// rejoin with it until it expires. LiveKit refreshes the token of a connected client itself.
	voiceTokenTTL = time.Minute
	maxSTTWorkers = 100
	maxSTTRooms   = 1000
	maxSTTVoices  = 10000
	maxWorkerName = 64
	maxEngineName = 32
	// maxVoiceGap caps a line's gap_s (spec 018): any longer pause ends a paragraph just the same.
	maxVoiceGap = 3600.0
	// livekitCallTimeout bounds a kick on revoke or delete, so a LiveKit outage cannot hang the admin.
	livekitCallTimeout = 5 * time.Second
)

// sttTimeout is how long a worker counts as up after its last heartbeat.
const sttTimeout = 15 * time.Second

// Voice configures LiveKit audio. A nil Voice in Options turns voice off.
type Voice struct {
	// URL is LiveKit's signalling URL as browsers reach it (wss://...).
	URL         string
	Key, Secret string
	// API kicks revoked participants and closes deleted rooms; nil skips both.
	API *livekit.Client
	// STTTokenHash is HashToken of the STT worker's service token; empty refuses every worker.
	STTTokenHash string
}

// sttWorkers reads the STT workers' heartbeats from the table every pod shares (017-AC14), through
// one copy per pod.
type sttWorkers struct {
	st  *store.Store
	log *zap.Logger
	c   *podCache[[]store.STTWorker]
}

func newSTTWorkers(st *store.Store, log *zap.Logger) sttWorkers {
	return sttWorkers{st: st, log: log, c: &podCache[[]store.STTWorker]{}}
}

// live returns the workers live at now.
func (w sttWorkers) live(now time.Time) []store.STTWorker {
	all := w.c.get("", func() ([]store.STTWorker, error) {
		ctx, cancel := context.WithTimeout(context.Background(), sharedTimeout)
		defer cancel()
		ws, err := w.st.STTWorkers(ctx, now.UnixMilli())
		if err != nil {
			w.log.Warn("stt_workers_read_failed", zap.Error(err))
		}
		return ws, err
	})
	return slices.DeleteFunc(slices.Clone(all), func(wk store.STTWorker) bool { return wk.ExpiresMS <= now.UnixMilli() })
}

// sttState is what a room's page shows about transcription.
type sttState struct {
	// Available is true while at least one worker heartbeats.
	Available bool `json:"available"`
	// Listening is true while a worker is in the room's audio.
	Listening bool `json:"listening"`
	// Transcribed lists the participants whose track a worker is subscribed to, sorted: the voices
	// it transcribes (005-AC8).
	Transcribed []int64 `json:"transcribed"`
	// Engines lists the live workers' engines, sorted: the audio menu credits the model (007-AC13).
	Engines []string `json:"engines"`
	// Glossary is true while a live worker reads room glossaries: Room settings shows the tab only
	// then (007-AC14).
	Glossary bool `json:"glossary"`
	// Device is the device transcription offer, absent when the server makes none (010-AC5).
	Device *devicestt.Offer `json:"device,omitempty"`
}

// equal compares Device by pointer: a server holds one offer for its lifetime.
func (a sttState) equal(b sttState) bool {
	return a.Available == b.Available && a.Listening == b.Listening && a.Glossary == b.Glossary &&
		slices.Equal(a.Transcribed, b.Transcribed) && slices.Equal(a.Engines, b.Engines) && a.Device == b.Device
}

// sttState is the room's transcription state with the server's device offer.
func (s *Server) sttState(room string, now time.Time) sttState {
	st := s.stt.state(room, now)
	if s.device != nil {
		st.Device = s.device.offer
	}
	return st
}

func (w sttWorkers) state(room string, now time.Time) sttState {
	st := sttState{Transcribed: []int64{}, Engines: []string{}}
	for _, wk := range w.live(now) {
		st.Available = true
		st.Listening = st.Listening || slices.Contains(wk.Rooms, room)
		st.Transcribed = append(st.Transcribed, wk.Voices[room]...)
		st.Glossary = st.Glossary || wk.Glossary
		if wk.Engine != "" {
			st.Engines = append(st.Engines, wk.Engine)
		}
	}
	slices.Sort(st.Transcribed)
	st.Transcribed = slices.Compact(st.Transcribed)
	slices.Sort(st.Engines)
	st.Engines = slices.Compact(st.Engines)
	return st
}

// STTWorkers returns the number of STT workers heartbeating, as of the last refreshGauges.
func (s *Server) STTWorkers() int { return int(s.gauges.sttWorkers.Load()) }

// getVoice mints the participant's LiveKit token: one LiveKit room per grill room, identity the
// participant id, microphone the only source it may publish.
func (s *Server) getVoice(w http.ResponseWriter, _ *http.Request, sess session) {
	v := s.opts.Voice
	if v == nil {
		httpError(w, http.StatusNotFound, "voice is not enabled on this server")
		return
	}
	yes := true
	tok, err := livekit.Token(v.Key, v.Secret, strconv.FormatInt(sess.p.ID, 10), sess.p.Name, livekit.Grant{
		Room: sess.p.RoomID, RoomJoin: true, CanPublish: &yes, CanSubscribe: &yes,
		CanPublishData: new(bool), CanPublishSources: []string{"microphone"},
	}, voiceTokenTTL, time.Now())
	if err != nil {
		s.serverError(w, "voice_token_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": v.URL, "token": tok})
}

// sttAuth admits the STT worker by its service token, never the admin or a participant token.
func (s *Server) sttAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := s.opts.Voice
		tok := bearer(r)
		if v == nil || v.STTTokenHash == "" || tok == "" ||
			subtle.ConstantTimeCompare([]byte(HashToken(tok)), []byte(v.STTTokenHash)) != 1 {
			httpError(w, http.StatusUnauthorized, "STT service token required")
			return
		}
		next(w, r)
	})
}

// sttHeartbeat records that a worker is up, which rooms' audio it is in, and whose tracks it is
// subscribed to there. A worker from before 005-AC8 sends no voices, one from before 007 no engine:
// it counts as not reading glossaries. It answers, for the worker's rooms, the participants who
// transcribe on their device (010-AC14) or turned their transcription off (010-AC23): the worker
// skips their tracks.
func (s *Server) sttHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Worker   string             `json:"worker"`
		Rooms    []string           `json:"rooms"`
		Voices   map[string][]int64 `json:"voices"`
		Engine   string             `json:"engine"`
		Glossary bool               `json:"glossary"`
	}
	if !decode(w, r, &req) {
		return
	}
	engine := strings.TrimSpace(req.Engine)
	engineOK := engine == "" || store.ValidText(engine, maxEngineName, false)
	name, ok := cleanText(req.Worker, maxWorkerName, false)
	n := 0
	for _, ids := range req.Voices {
		n += len(ids)
	}
	if !ok || !engineOK || len(req.Rooms) > maxSTTRooms || len(req.Voices) > maxSTTRooms || n > maxSTTVoices {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("worker must be 1 to %d characters, engine at most %d, rooms at most %d, voices at most %d",
			maxWorkerName, maxEngineName, maxSTTRooms, maxSTTVoices))
		return
	}
	now := s.lease()
	ctx, cancel := context.WithTimeout(r.Context(), sharedTimeout)
	defer cancel()
	ok, err := s.stt.st.BeatSTTWorker(ctx, store.STTWorker{Name: name, Rooms: req.Rooms, Voices: req.Voices, Engine: engine, Glossary: req.Glossary,
		ExpiresMS: now.Add(sttTimeout).UnixMilli()}, maxSTTWorkers, now.UnixMilli())
	s.stt.c.drop("")
	if err != nil {
		s.serverError(w, "stt_heartbeat_failed", err)
		return
	}
	if !ok {
		httpError(w, http.StatusTooManyRequests, "too many STT workers")
		return
	}
	skip := s.people.deviceFlags(req.Rooms, now)
	off, err := s.stt.st.STTOff(ctx, req.Rooms)
	if err != nil {
		s.serverError(w, "stt_heartbeat_failed", err)
		return
	}
	for room, ids := range off {
		ids = append(skip[room], ids...)
		slices.Sort(ids)
		skip[room] = slices.Compact(ids)
	}
	writeJSON(w, http.StatusOK, map[string]any{"device": skip})
}

// postVoiceLine stores one utterance the STT worker transcribed as a voice message from the
// participant who spoke.
func (s *Server) postVoiceLine(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ParticipantID int64    `json:"participant_id"`
		Body          string   `json:"body"`
		GapS          *float64 `json:"gap_s"`
	}
	if !decode(w, r, &req) {
		return
	}
	body, ok := cleanText(req.Body, maxBodyLen, true)
	if !ok {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("body must be 1 to %d characters", maxBodyLen))
		return
	}
	if !validGap(w, req.GapS) {
		return
	}
	p, ok := s.voiceSpeaker(w, r, req.ParticipantID)
	if !ok || sttOff(w, p) {
		return
	}
	s.storeVoiceLine(w, r, p, body, store.VoiceSourceServer)
}

// postDeviceLine stores one utterance the participant's own device transcribed, under their name
// whatever the body says, like a chat message (010-AC10).
func (s *Server) postDeviceLine(w http.ResponseWriter, r *http.Request, sess session) {
	if s.device == nil {
		httpError(w, http.StatusNotFound, "device transcription is not offered on this server")
		return
	}
	if sttOff(w, sess.p) {
		return
	}
	var req struct {
		Body string   `json:"body"`
		GapS *float64 `json:"gap_s"`
	}
	if !decode(w, r, &req) {
		return
	}
	body, ok := cleanText(req.Body, maxBodyLen, true)
	if !ok {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("body must be 1 to %d characters", maxBodyLen))
		return
	}
	if !validGap(w, req.GapS) {
		return
	}
	s.storeVoiceLine(w, r, sess.p, body, store.VoiceSourceDevice)
}

// sttOff refuses a line of p while p has their transcription off, true once it answered: a worker
// or a tab that has not caught up yet must not write it down (010-AC23).
func sttOff(w http.ResponseWriter, p *store.Participant) bool {
	if p.STTOff {
		httpError(w, http.StatusConflict, "this participant turned the transcription of their voice off")
	}
	return p.STTOff
}

// putVoiceTranscribe turns the transcription of the caller's own voice on or off, kept with their
// seat: the worker skips their track from its next heartbeat and their lines are refused (010-AC23).
func (s *Server) putVoiceTranscribe(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		On *bool `json:"on"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.On == nil {
		httpError(w, http.StatusBadRequest, "on is required")
		return
	}
	if err := s.bus.Store.SetSTTOff(r.Context(), sess.p.RoomID, sess.p.ID, !*req.On); err != nil {
		s.serverError(w, "voice_transcribe_failed", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validGap accepts a line's optional gap_s, the pause before it (018-AC1, 018-AC2), capped at
// maxVoiceGap: a long pause must not lose the line. Absent, the line is stored as before 018 (018-AC8).
func validGap(w http.ResponseWriter, gap *float64) bool {
	if gap == nil {
		return true
	}
	if !(*gap >= 0) || math.IsInf(*gap, 1) {
		httpError(w, http.StatusBadRequest, "gap_s must be a number of seconds, 0 or more")
		return false
	}
	*gap = min(*gap, maxVoiceGap)
	return true
}

// storeVoiceLine stores p's line from source and runs the voice triggers, unless it repeats a line
// of p from the other source (010-AC16).
func (s *Server) storeVoiceLine(w http.ResponseWriter, r *http.Request, p *store.Participant, body, source string) {
	m := &store.Message{RoomID: p.RoomID, Kind: store.KindVoice, ParticipantID: p.ID, Sender: p.Name, Body: body, Source: source}
	norm := normLine(body)
	stored, err := s.bus.Store.PostVoiceLine(r.Context(), m, s.now(), dedupeWindow, func(other []string) bool {
		return slices.ContainsFunc(other, func(o string) bool { return similar(normLine(o), norm) })
	})
	if err != nil {
		s.serverError(w, "voice_line_failed", err)
		return
	}
	if !stored {
		s.voiceDeduped[source].Add(1)
		writeJSON(w, http.StatusOK, map[string]bool{"deduped": true})
		return
	}
	s.bus.Notify(p.RoomID)
	s.voiceLines[source].Add(1)
	s.voiceTriggers(r.Context(), p.RoomID)
	writeJSON(w, http.StatusCreated, m)
}

// VoiceSources are where a voice line is transcribed: the participant's device or the server.
var VoiceSources = []string{store.VoiceSourceDevice, store.VoiceSourceServer}

// VoiceLines counts the voice lines stored from source (010-AC20).
func (s *Server) VoiceLines(source string) int64 { return s.voiceLines[source].Load() }

// VoiceLinesDeduped counts the voice lines from source dropped as another source's repeat (010-AC16).
func (s *Server) VoiceLinesDeduped(source string) int64 { return s.voiceDeduped[source].Load() }

// DeviceSTTParticipants returns how many participants transcribe on their device (010-AC20), as of
// the last refreshGauges.
func (s *Server) DeviceSTTParticipants() int { return int(s.gauges.deviceSTT.Load()) }

// voiceSpeaker is the room's participant a worker names; false once it answered the error.
func (s *Server) voiceSpeaker(w http.ResponseWriter, r *http.Request, id int64) (*store.Participant, bool) {
	p, err := s.bus.Store.RoomParticipant(r.Context(), r.PathValue("room"), id)
	if errors.Is(err, store.ErrNotFound) {
		// A participant revoked while speaking: the line is dropped with their access.
		httpError(w, http.StatusNotFound, "no such participant in this room")
		return nil, false
	}
	if err != nil {
		s.serverError(w, "voice_speaker_failed", err)
		return nil, false
	}
	return p, true
}

// postVoiceDropped says in the room that the STT worker's full queue lost a line of a speaker
// (007-AC8): a drop is never silent. The worker spaces them out per speaker.
func (s *Server) postVoiceDropped(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ParticipantID int64 `json:"participant_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	p, ok := s.voiceSpeaker(w, r, req.ParticipantID)
	if !ok {
		return
	}
	m := &store.Message{RoomID: p.RoomID, Kind: store.KindSystem, Sender: "Transcription",
		Body: "Transcription overloaded: one line lost for " + p.Name + "."}
	if err := s.bus.Post(r.Context(), m); err != nil {
		s.serverError(w, "voice_dropped_failed", err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

// kickFromVoice removes participants from the room's LiveKit audio. Best effort: LiveKit being down
// must not fail a revoke, and the browser leaves on its own once its event stream ends.
func (s *Server) kickFromVoice(room string, ids []int64) {
	v := s.opts.Voice
	if v == nil || v.API == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), livekitCallTimeout)
	defer cancel()
	for _, id := range ids {
		if err := v.API.RemoveParticipant(ctx, room, strconv.FormatInt(id, 10)); err != nil {
			s.log.Warn("voice_kick_failed", zap.String("room", room), zap.Int64("participant", id), zap.Error(err))
		}
	}
}

// closeVoice ends a deleted room's LiveKit room. Best effort, as kickFromVoice.
func (s *Server) closeVoice(room string) {
	v := s.opts.Voice
	if v == nil || v.API == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), livekitCallTimeout)
	defer cancel()
	if err := v.API.DeleteRoom(ctx, room); err != nil {
		s.log.Warn("voice_close_failed", zap.String("room", room), zap.Error(err))
	}
}

// sttEvent renders the "stt" SSE event.
func sttEvent(st sttState) string {
	b, _ := json.Marshal(st) // plain fields cannot fail to marshal
	return "event: stt\ndata: " + string(b) + "\n\n"
}
