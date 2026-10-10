package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
)

const (
	pairingTTL = 10 * time.Minute
	// Pairing requests need no token, so their count is bounded per room and overall.
	maxPairingsPerRoom = 10
	maxPairings        = 1000
	maxDaemonName      = 64
	maxReason          = 200
)

// pairing is a daemon's request to pair, waiting for a participant's answer. Kept in memory: a
// restart only makes the daemon ask again.
type pairing struct {
	id, room, name, code string
	secretHash           string
	expires              time.Time
	// token is set on approval and handed to the daemon once.
	token  string
	denied bool
}

type pairings struct {
	mu sync.Mutex
	m  map[string]*pairing
}

// sweepLocked drops expired requests.
func (ps *pairings) sweepLocked(now time.Time) {
	for id, p := range ps.m {
		if now.After(p.expires) {
			delete(ps.m, id)
		}
	}
}

// presence is a connected daemon as the room sees it.
type presence struct {
	ParticipantID int64  `json:"participant_id"`
	Name          string `json:"name"`
	CanHost       bool   `json:"can_host"`
	// Reason says why the daemon cannot host (Docker missing, credentials unreadable).
	Reason  string `json:"reason,omitempty"`
	Version string `json:"version,omitempty"`
	room    string
	conns   int
}

type presences struct {
	mu sync.Mutex
	m  map[int64]*presence // by daemon id
}

// connectedDaemons lists the room's daemons with an open stream, by participant.
func (s *Server) connectedDaemons(room string) []presence {
	s.presence.mu.Lock()
	defer s.presence.mu.Unlock()
	out := []presence{}
	for _, p := range s.presence.m {
		if p.room == room && p.conns > 0 {
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ParticipantID < out[j].ParticipantID })
	return out
}

// pairCode is shown both in the daemon's local UI and on the approval page, so the participant
// can tell their own daemon's request from one someone else sent them a link to.
func pairCode() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1_000_000)) // crypto/rand never fails on supported platforms
	s := fmt.Sprintf("%06d", n.Int64())
	return s[:3] + "-" + s[3:]
}

// createPairing is called by a daemon that has no token yet.
func (s *Server) createPairing(w http.ResponseWriter, r *http.Request) {
	room := r.PathValue("room")
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	name, ok := cleanText(req.Name, maxDaemonName, false)
	if !ok {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("name must be 1 to %d characters", maxDaemonName))
		return
	}
	if _, err := s.bus.Store.Room(r.Context(), room); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpError(w, http.StatusNotFound, "no such room")
			return
		}
		s.serverError(w, "create_pairing_failed", err)
		return
	}
	secret := NewToken()
	p := &pairing{id: NewToken(), room: room, name: name, code: pairCode(), secretHash: HashToken(secret), expires: time.Now().Add(pairingTTL)}
	s.pairings.mu.Lock()
	s.pairings.sweepLocked(time.Now())
	inRoom := 0
	for _, q := range s.pairings.m {
		if q.room == room {
			inRoom++
		}
	}
	if inRoom >= maxPairingsPerRoom || len(s.pairings.m) >= maxPairings {
		s.pairings.mu.Unlock()
		httpError(w, http.StatusTooManyRequests, "too many pending pairing requests, retry in a few minutes")
		return
	}
	s.pairings.m[p.id] = p
	s.pairings.mu.Unlock()
	s.log.Info("pairing_requested", zap.String("room", room), zap.String("name", name))
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": p.id, "secret": secret, "code": p.code, "expires_at": p.expires.UTC(),
		"approve_url": s.baseURL + "/r/" + room + "?pair=" + p.id,
	})
}

// pollPairing tells the daemon holding the request's secret whether it was answered. On approval
// it hands over the daemon token, once.
func (s *Server) pollPairing(w http.ResponseWriter, r *http.Request) {
	tok := bearer(r)
	s.pairings.mu.Lock()
	defer s.pairings.mu.Unlock()
	s.pairings.sweepLocked(time.Now())
	p := s.pairings.m[r.PathValue("id")]
	if p == nil || subtle.ConstantTimeCompare([]byte(HashToken(tok)), []byte(p.secretHash)) != 1 {
		httpError(w, http.StatusNotFound, "no such pairing request, or it expired")
		return
	}
	switch {
	case p.token != "":
		delete(s.pairings.m, p.id)
		writeJSON(w, http.StatusOK, map[string]string{"status": "approved", "token": p.token})
	case p.denied:
		delete(s.pairings.m, p.id)
		writeJSON(w, http.StatusOK, map[string]string{"status": "denied"})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "pending"})
	}
}

// pendingPairing returns the room's unanswered request id; nil if there is none.
func (s *Server) pendingPairing(room, id string) *pairing {
	s.pairings.sweepLocked(time.Now())
	p := s.pairings.m[id]
	if p == nil || p.room != room || p.token != "" || p.denied {
		return nil
	}
	return p
}

func (s *Server) getPairing(w http.ResponseWriter, r *http.Request, sess session) {
	s.pairings.mu.Lock()
	defer s.pairings.mu.Unlock()
	p := s.pendingPairing(sess.p.RoomID, r.PathValue("id"))
	if p == nil {
		httpError(w, http.StatusNotFound, "this pairing request expired or was already answered")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": p.id, "name": p.name, "code": p.code, "expires_at": p.expires.UTC()})
}

// answerPairing pairs the daemon to the participant answering, or denies it.
func (s *Server) answerPairing(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		Approve bool `json:"approve"`
	}
	if !decode(w, r, &req) {
		return
	}
	s.pairings.mu.Lock()
	defer s.pairings.mu.Unlock()
	p := s.pendingPairing(sess.p.RoomID, r.PathValue("id"))
	if p == nil {
		httpError(w, http.StatusNotFound, "this pairing request expired or was already answered")
		return
	}
	if !req.Approve {
		p.denied = true
		s.log.Info("pairing_denied", zap.String("room", p.room), zap.Int64("participant", sess.p.ID))
		writeJSON(w, http.StatusOK, map[string]string{"status": "denied"})
		return
	}
	tok := NewToken()
	d, err := s.bus.Store.PairDaemon(r.Context(), sess.p.RoomID, sess.p.ID, p.name, HashToken(tok))
	if err != nil {
		s.serverError(w, "pair_daemon_failed", err)
		return
	}
	p.token = tok
	// Re-pairing replaced the previous daemon's token: close its stream.
	s.bus.Notify(p.room)
	s.log.Info("daemon_paired", zap.String("room", p.room), zap.Int64("participant", sess.p.ID), zap.Int64("daemon", d.ID))
	writeJSON(w, http.StatusOK, map[string]any{"status": "approved", "daemon": d})
}

// daemonAuth authenticates a paired daemon by its bearer token, for its own room only.
func (s *Server) daemonAuth(next func(http.ResponseWriter, *http.Request, *store.Daemon, string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if tok == "" {
			httpError(w, http.StatusUnauthorized, "missing daemon token")
			return
		}
		hash := HashToken(tok)
		d, err := s.daemonFor(r.Context(), hash, r.PathValue("room"))
		if errors.Is(err, store.ErrNotFound) {
			httpError(w, http.StatusUnauthorized, "invalid or revoked daemon token")
			return
		}
		if err != nil {
			s.log.Error("daemon_auth_failed", zap.Error(err))
			httpError(w, http.StatusInternalServerError, "auth failed")
			return
		}
		next(w, r, d, hash)
	})
}

func (s *Server) daemonFor(ctx context.Context, hash, room string) (*store.Daemon, error) {
	d, err := s.bus.Store.DaemonByToken(ctx, hash)
	if err != nil {
		return nil, err
	}
	if d.RoomID != room {
		return nil, store.ErrNotFound
	}
	return d, nil
}

// daemonStatus is the daemon's heartbeat: whether it can host right now, and why not.
func (s *Server) daemonStatus(w http.ResponseWriter, r *http.Request, d *store.Daemon, _ string) {
	var req struct {
		CanHost bool   `json:"can_host"`
		Reason  string `json:"reason"`
		Version string `json:"version"`
	}
	if !decode(w, r, &req) {
		return
	}
	reason, _ := cleanText(truncate(req.Reason, maxReason), maxReason, false)
	version, _ := cleanText(truncate(req.Version, 40), 40, false)
	s.presence.mu.Lock()
	p := s.presence.m[d.ID]
	if p == nil {
		p = &presence{}
		s.presence.m[d.ID] = p
	}
	p.room, p.ParticipantID, p.Name = d.RoomID, d.ParticipantID, d.Name
	p.CanHost, p.Reason, p.Version = req.CanHost, reason, version
	s.presence.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// daemonEvents is the daemon's outbound connection: a stream that opens with a "hello" event and
// carries a ping every heartbeat. It ends with a "revoked" event once the daemon's access is gone.
func (s *Server) daemonEvents(w http.ResponseWriter, r *http.Request, d *store.Daemon, hash string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	s.presence.mu.Lock()
	p := s.presence.m[d.ID]
	if p == nil {
		p = &presence{}
		s.presence.m[d.ID] = p
	}
	// Re-pairing keeps the daemon id but may rename it.
	p.room, p.ParticipantID, p.Name = d.RoomID, d.ParticipantID, d.Name
	p.conns++
	s.presence.mu.Unlock()
	s.log.Info("daemon_connected", zap.String("room", d.RoomID), zap.Int64("daemon", d.ID))
	defer func() {
		s.presence.mu.Lock()
		if p.conns--; p.conns == 0 {
			delete(s.presence.m, d.ID)
		}
		s.presence.mu.Unlock()
		s.log.Info("daemon_disconnected", zap.String("room", d.RoomID), zap.Int64("daemon", d.ID))
	}()

	live, cancel := s.bus.Subscribe(16)
	defer cancel()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	hello, _ := json.Marshal(map[string]any{"daemon": d, "heartbeat_seconds": int(heartbeat / time.Second)})
	if _, err := fmt.Fprintf(w, "event: hello\ndata: %s\n\n", hello); err != nil {
		return
	}
	flusher.Flush()
	// alive re-checks the token: a revoked invite, a deleted room or a re-pairing ends the stream.
	alive := func() bool {
		if _, err := s.daemonFor(r.Context(), hash, d.RoomID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				_, _ = io.WriteString(w, "event: revoked\ndata: {}\n\n")
				flusher.Flush()
			}
			return false
		}
		return true
	}
	tick := time.NewTicker(heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if !alive() {
				return
			}
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case changed := <-live:
			if changed == d.RoomID && !alive() {
				return
			}
		}
	}
}
