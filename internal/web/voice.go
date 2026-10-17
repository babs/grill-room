package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/devicestt"
	"github.com/babs/grill-room/internal/livekit"
	"github.com/babs/grill-room/internal/store"
)

const (
	// voiceTokenTTL is short because LiveKit cannot revoke a token: a kicked participant could
	// rejoin with it until it expires. LiveKit refreshes the token of a connected client itself.
	voiceTokenTTL = 5 * time.Minute
	maxSTTWorkers = 100
	maxSTTRooms   = 1000
	maxSTTVoices  = 10000
	maxWorkerName = 64
	maxEngineName = 32
	// livekitCallTimeout bounds a kick on revoke or delete, so a LiveKit outage cannot hang the admin.
	livekitCallTimeout = 5 * time.Second
)

// sttTimeout is how long a worker counts as up after its last heartbeat.
const sttTimeout = 15 * time.Second

// pollEvery is how often an event stream checks presence and transcription for a change. A var for
// tests.
var pollEvery = time.Second

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

// sttWorker is the last heartbeat of one STT worker: the rooms whose audio it is in, in each the
// participants whose track it is subscribed to, its engine and whether it reads room glossaries.
type sttWorker struct {
	at       time.Time
	rooms    map[string]bool
	voices   map[string][]int64
	engine   string
	glossary bool
}

type sttWorkers struct {
	mu sync.Mutex
	m  map[string]*sttWorker
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

func (w *sttWorkers) state(room string, now time.Time) sttState {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := sttState{Transcribed: []int64{}, Engines: []string{}}
	for id, wk := range w.m {
		if now.Sub(wk.at) > sttTimeout {
			delete(w.m, id)
			continue
		}
		st.Available = true
		st.Listening = st.Listening || wk.rooms[room]
		st.Transcribed = append(st.Transcribed, wk.voices[room]...)
		st.Glossary = st.Glossary || wk.glossary
		if wk.engine != "" {
			st.Engines = append(st.Engines, wk.engine)
		}
	}
	slices.Sort(st.Transcribed)
	st.Transcribed = slices.Compact(st.Transcribed)
	slices.Sort(st.Engines)
	st.Engines = slices.Compact(st.Engines)
	return st
}

// STTWorkers returns the number of STT workers heartbeating.
func (s *Server) STTWorkers() int {
	s.stt.state("", s.now()) // sweeps the stale ones
	s.stt.mu.Lock()
	defer s.stt.mu.Unlock()
	return len(s.stt.m)
}

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
// transcribe on their device: the worker skips their tracks (010-AC14).
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
	rooms := make(map[string]bool, len(req.Rooms))
	for _, id := range req.Rooms {
		rooms[id] = true
	}
	now := s.now()
	s.stt.state("", now) // sweeps the stale ones before the bound applies
	s.stt.mu.Lock()
	if s.stt.m[name] == nil && len(s.stt.m) >= maxSTTWorkers {
		s.stt.mu.Unlock()
		httpError(w, http.StatusTooManyRequests, "too many STT workers")
		return
	}
	s.stt.m[name] = &sttWorker{at: now, rooms: rooms, voices: req.Voices, engine: engine, glossary: req.Glossary}
	s.stt.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"device": s.people.deviceFlags(req.Rooms, now)})
}

// postVoiceLine stores one utterance the STT worker transcribed as a voice message from the
// participant who spoke.
func (s *Server) postVoiceLine(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ParticipantID int64  `json:"participant_id"`
		Body          string `json:"body"`
	}
	if !decode(w, r, &req) {
		return
	}
	body, ok := cleanText(req.Body, maxBodyLen, true)
	if !ok {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("body must be 1 to %d characters", maxBodyLen))
		return
	}
	p, ok := s.voiceSpeaker(w, r, req.ParticipantID)
	if !ok {
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
	var req struct {
		Body string `json:"body"`
	}
	if !decode(w, r, &req) {
		return
	}
	body, ok := cleanText(req.Body, maxBodyLen, true)
	if !ok {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("body must be 1 to %d characters", maxBodyLen))
		return
	}
	s.storeVoiceLine(w, r, sess.p, body, store.VoiceSourceDevice)
}

// storeVoiceLine stores p's line from source and runs the voice triggers, unless it repeats a line
// of p from the other source (010-AC16).
func (s *Server) storeVoiceLine(w http.ResponseWriter, r *http.Request, p *store.Participant, body, source string) {
	m := &store.Message{RoomID: p.RoomID, Kind: store.KindVoice, ParticipantID: p.ID, Sender: p.Name, Body: body, Source: source}
	norm := normLine(body)
	// ponytail: one lock over every room's check and insert; per-participant locks if voice
	// lines ever contend.
	s.voiceMu.Lock()
	now := s.now()
	if s.voiceRecent.dup(p.ID, source, norm, now) {
		s.voiceMu.Unlock()
		s.voiceDeduped[source].Add(1)
		writeJSON(w, http.StatusOK, map[string]bool{"deduped": true})
		return
	}
	err := s.bus.Post(r.Context(), m)
	if err == nil {
		s.voiceRecent.add(p.ID, source, norm, now)
	}
	s.voiceMu.Unlock()
	if err != nil {
		s.serverError(w, "voice_line_failed", err)
		return
	}
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

// DeviceSTTParticipants returns how many participants transcribe on their device now (010-AC20).
func (s *Server) DeviceSTTParticipants() int { return s.people.deviceCount(s.now()) }

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
