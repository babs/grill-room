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
	"sync"
	"time"

	"go.uber.org/zap"

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
	// livekitCallTimeout bounds a kick on revoke or delete, so a LiveKit outage cannot hang the admin.
	livekitCallTimeout = 5 * time.Second
)

// sttTimeout is how long a worker counts as up after its last heartbeat; pollEvery is how often an
// event stream checks presence and transcription for a change. Vars for tests.
var (
	sttTimeout = 15 * time.Second
	pollEvery  = time.Second
)

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

// sttWorker is the last heartbeat of one STT worker: the rooms whose audio it is in, and in each the
// participants whose track it is subscribed to.
type sttWorker struct {
	at     time.Time
	rooms  map[string]bool
	voices map[string][]int64
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
}

func (a sttState) equal(b sttState) bool {
	return a.Available == b.Available && a.Listening == b.Listening && slices.Equal(a.Transcribed, b.Transcribed)
}

func (w *sttWorkers) state(room string, now time.Time) sttState {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := sttState{Transcribed: []int64{}}
	for id, wk := range w.m {
		if now.Sub(wk.at) > sttTimeout {
			delete(w.m, id)
			continue
		}
		st.Available = true
		st.Listening = st.Listening || wk.rooms[room]
		st.Transcribed = append(st.Transcribed, wk.voices[room]...)
	}
	slices.Sort(st.Transcribed)
	st.Transcribed = slices.Compact(st.Transcribed)
	return st
}

// STTWorkers returns the number of STT workers heartbeating.
func (s *Server) STTWorkers() int {
	s.stt.state("", time.Now()) // sweeps the stale ones
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
// subscribed to there. A worker from before 005-AC8 sends no voices.
func (s *Server) sttHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Worker string             `json:"worker"`
		Rooms  []string           `json:"rooms"`
		Voices map[string][]int64 `json:"voices"`
	}
	if !decode(w, r, &req) {
		return
	}
	name, ok := cleanText(req.Worker, maxWorkerName, false)
	n := 0
	for _, ids := range req.Voices {
		n += len(ids)
	}
	if !ok || len(req.Rooms) > maxSTTRooms || len(req.Voices) > maxSTTRooms || n > maxSTTVoices {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("worker must be 1 to %d characters, rooms at most %d, voices at most %d",
			maxWorkerName, maxSTTRooms, maxSTTVoices))
		return
	}
	rooms := make(map[string]bool, len(req.Rooms))
	for _, id := range req.Rooms {
		rooms[id] = true
	}
	now := time.Now()
	s.stt.state("", now) // sweeps the stale ones before the bound applies
	s.stt.mu.Lock()
	defer s.stt.mu.Unlock()
	if s.stt.m[name] == nil && len(s.stt.m) >= maxSTTWorkers {
		httpError(w, http.StatusTooManyRequests, "too many STT workers")
		return
	}
	s.stt.m[name] = &sttWorker{at: now, rooms: rooms, voices: req.Voices}
	w.WriteHeader(http.StatusNoContent)
}

// postVoiceLine stores one transcribed utterance as a voice message from the participant who spoke.
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
	p, err := s.bus.Store.RoomParticipant(r.Context(), r.PathValue("room"), req.ParticipantID)
	if errors.Is(err, store.ErrNotFound) {
		// A participant revoked while speaking: the line is dropped with their access.
		httpError(w, http.StatusNotFound, "no such participant in this room")
		return
	}
	if err != nil {
		s.serverError(w, "voice_line_failed", err)
		return
	}
	m := &store.Message{RoomID: p.RoomID, Kind: store.KindVoice, ParticipantID: p.ID, Sender: p.Name, Body: body}
	if err := s.bus.Post(r.Context(), m); err != nil {
		s.serverError(w, "voice_line_failed", err)
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
	b, _ := json.Marshal(st) // two bools cannot fail to marshal
	return "event: stt\ndata: " + string(b) + "\n\n"
}
