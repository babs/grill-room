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
	"github.com/babs/grill-room/internal/turn"
)

const (
	pairingTTL = 10 * time.Minute
	// Pairing requests need no token, so their count is bounded per room and overall.
	maxPairingsPerRoom = 10
	maxPairings        = 1000
	maxReason          = 200
)

// pairing is a daemon's request to pair, waiting for a participant's answer. Kept in memory: a
// restart only makes the daemon ask again.
type pairing struct {
	id, room, code string
	secretHash     string
	expires        time.Time
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
	ParticipantID int64 `json:"participant_id"`
	CanHost       bool  `json:"can_host"`
	// Reason says why the daemon cannot host (Docker missing, credentials unreadable).
	Reason  string `json:"reason,omitempty"`
	Version string `json:"version,omitempty"`
	room    string
	conns   int
	id      int64
	// since is when the current run of connections started: the earliest eligible one hosts.
	since time.Time
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

// createPairing is called by a daemon that has no token yet. A name it sends, the machine's
// name for daemons before spec 003, is dropped unread (003-AC17).
func (s *Server) createPairing(w http.ResponseWriter, r *http.Request) {
	room := r.PathValue("room")
	var req struct{}
	if !decode(w, r, &req) {
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
	p := &pairing{id: NewToken(), room: room, code: pairCode(), secretHash: HashToken(secret), expires: time.Now().Add(pairingTTL)}
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
	s.log.Info("pairing_requested", zap.String("room", room))
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
	writeJSON(w, http.StatusOK, map[string]any{"id": p.id, "code": p.code, "expires_at": p.expires.UTC()})
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
	d, err := s.bus.Store.PairDaemon(r.Context(), sess.p.RoomID, sess.p.ID, HashToken(tok))
	if err != nil {
		s.serverError(w, "pair_daemon_failed", err)
		return
	}
	p.token = tok
	// The replaced daemon can no longer post its result.
	s.failDaemonTurn(r.Context(), d, "the host daemon was paired again")
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
		store.DaemonSettings
	}
	if !decode(w, r, &req) {
		return
	}
	// A daemon from before the host cap sends none: no cap, nothing held.
	set := store.DaemonSettings{MaxModel: turn.ModelFamily(req.MaxModel), MaxEffort: knownOr(req.MaxEffort, turn.Efforts), ApproveAll: req.ApproveAll}
	settingsChanged, err := s.bus.Store.SetDaemonSettings(r.Context(), d.ID, set)
	if err != nil {
		s.serverError(w, "daemon_status_failed", err)
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
	changed := p.CanHost != req.CanHost || p.Reason != reason
	p.room, p.ParticipantID, p.id = d.RoomID, d.ParticipantID, d.ID
	p.CanHost, p.Reason, p.Version = req.CanHost, reason, version
	s.presence.mu.Unlock()
	switch {
	case changed:
		s.agentChanged(r.Context(), d.RoomID)
	case settingsChanged:
		// Turning "approve everything" off lets a starter's held skills through (006-AC13).
		s.releaseStarterSkills(r.Context(), d.RoomID)
		s.bus.Notify(d.RoomID)
	}
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
	p.room, p.ParticipantID, p.id = d.RoomID, d.ParticipantID, d.ID
	if p.conns++; p.conns == 1 {
		p.since = time.Now()
	}
	s.presence.mu.Unlock()
	s.log.Info("daemon_connected", zap.String("room", d.RoomID), zap.Int64("daemon", d.ID))
	s.agentChanged(r.Context(), d.RoomID)
	defer func() {
		s.presence.mu.Lock()
		gone := false
		if p.conns--; p.conns == 0 {
			delete(s.presence.m, d.ID)
			gone = true
		}
		s.presence.mu.Unlock()
		s.log.Info("daemon_disconnected", zap.String("room", d.RoomID), zap.Int64("daemon", d.ID))
		if gone {
			// The request context is done: the cleanup must still reach the store.
			ctx := context.WithoutCancel(r.Context())
			s.failDaemonTurn(ctx, d, "the host's daemon disconnected")
			s.agentChanged(ctx, d.RoomID)
		}
	}()

	live, cancel := s.bus.Subscribe(16)
	defer cancel()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// The room's name lets the daemon's page show it instead of its link (005-AC42).
	var roomName string
	if room, err := s.bus.Store.Room(r.Context(), d.RoomID); err == nil {
		roomName = room.Name
	}
	hello, _ := json.Marshal(map[string]any{"daemon": d, "heartbeat_seconds": int(heartbeat / time.Second), "room_name": roomName})
	if _, err := fmt.Fprintf(w, "event: hello\ndata: %s\n\n", hello); err != nil {
		return
	}
	flusher.Flush()
	// sendTurn hands this daemon each running turn of the room that is its own, once. Ids grow, so
	// the last one sent tells which are new; with parallel on there may be several (003-AC20). A
	// turn handed over that ended here without its result, stopped or failed, gets a "stop": the
	// daemon kills its claude (005-AC5). Daemons from before ignore the event; the room refuses their result.
	var sent int64
	held := map[int64]bool{}
	sendTurn := func() bool {
		ts, err := s.bus.Store.RunningTurns(r.Context(), d.RoomID)
		if err != nil {
			return true
		}
		running := map[int64]bool{}
		for _, t := range ts {
			if t.DaemonID != d.ID {
				continue
			}
			running[t.ID] = true
			if t.ID <= sent {
				continue
			}
			sent, held[t.ID] = t.ID, true
			if _, err := fmt.Fprintf(w, "event: turn\ndata: {\"id\":%d}\n\n", t.ID); err != nil { //nolint:gosec // an integer id on an event stream, never HTML
				return false
			}
		}
		for id := range held {
			if running[id] {
				continue
			}
			delete(held, id)
			// A done turn's daemon posted its result: it has nothing left to kill.
			if t, err := s.bus.Store.Turn(r.Context(), d.RoomID, id); err == nil && t.Status == store.TurnDone {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: stop\ndata: {\"id\":%d}\n\n", id); err != nil { //nolint:gosec // an integer id on an event stream, never HTML
				return false
			}
		}
		flusher.Flush()
		return true
	}
	// sendRoom tells this daemon whether it hosts the room's agent and whether the room runs
	// personas in parallel, when that changed; its admin page shows both (003-AC26).
	var sentRoom string
	sendRoom := func() bool {
		room, err := s.bus.Store.Room(r.Context(), d.RoomID)
		if err != nil {
			return true
		}
		host, err := s.bus.Store.HostDaemon(r.Context(), d.RoomID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return true
		}
		b, _ := json.Marshal(map[string]bool{"host": host != nil && host.ID == d.ID, "parallel_turns": room.ParallelTurns})
		if string(b) == sentRoom {
			return true
		}
		sentRoom = string(b)
		if _, err := fmt.Fprintf(w, "event: room\ndata: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	// sendInspect hands the room's host daemon each plugin version waiting for its inspection,
	// once per stream (009-AC13); a daemon that becomes host gets them then.
	inspect := map[int64]bool{}
	sendInspect := func() bool {
		host, err := s.bus.Store.HostDaemon(r.Context(), d.RoomID)
		if err == nil && host.ID != d.ID || errors.Is(err, store.ErrNotFound) {
			// Not the host: should it become host again, it gets every waiting version anew.
			clear(inspect)
			return true
		}
		if err != nil {
			return true
		}
		ids, err := s.bus.Store.InspectingVersions(r.Context(), d.RoomID)
		if err != nil {
			return true
		}
		for _, id := range ids {
			if inspect[id] {
				continue
			}
			inspect[id] = true
			if _, err := fmt.Fprintf(w, "event: inspect\ndata: {\"id\":%d}\n\n", id); err != nil { //nolint:gosec // an integer id on an event stream, never HTML
				return false
			}
		}
		flusher.Flush()
		return true
	}
	// sendPlugins tells the room's host daemon that the room's plugins or its requests to the AI
	// host changed, and once as the stream opens: its page reads them again (009-AC19, 009-AC22).
	sentPlugins, pluginsSent := "", false
	sendPlugins := func() bool {
		host, err := s.bus.Store.HostDaemon(r.Context(), d.RoomID)
		if err == nil && host.ID != d.ID || errors.Is(err, store.ErrNotFound) {
			pluginsSent = false
			return true
		}
		if err != nil {
			return true
		}
		v, err := s.bus.Store.PluginsVersion(r.Context(), d.RoomID)
		if err != nil || pluginsSent && v == sentPlugins {
			return true
		}
		sentPlugins, pluginsSent = v, true
		if _, err := io.WriteString(w, "event: plugins\ndata: {}\n\n"); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !sendTurn() || !sendRoom() || !sendInspect() || !sendPlugins() {
		return
	}
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
			// The bus drops a signal when this stream's buffer is full of other rooms' ones: the
			// heartbeat re-reads the room, so a turn, a stop or an inspection is at most that late.
			if !alive() || !sendTurn() || !sendRoom() || !sendInspect() || !sendPlugins() {
				return
			}
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case changed := <-live:
			if changed == d.RoomID && (!alive() || !sendTurn() || !sendRoom() || !sendInspect() || !sendPlugins()) {
				return
			}
		}
	}
}
