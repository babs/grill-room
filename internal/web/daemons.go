package web

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/bus"
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

// liveDaemon is a daemon with a stream open on any pod, as the room sees it.
type liveDaemon struct {
	ParticipantID int64 `json:"participant_id"`
	CanHost       bool  `json:"can_host"`
	// Reason says why the daemon cannot host (Docker missing, credentials unreadable).
	Reason  string `json:"reason,omitempty"`
	Version string `json:"version,omitempty"`
	// Egress says the daemon applies website approvals from the room (014-AC9).
	Egress bool `json:"egress,omitempty"`
	// Connectors are the claude.ai connectors its host turned on for the room (014-AC19).
	Connectors []roomConnector `json:"connectors,omitempty"`
	room       string
	id         int64
	// since is when the current run of connections started: the earliest eligible one hosts.
	since time.Time
}

// daemonLease is how long a daemon connection counts without a renewal: a pod gone without
// closing its streams leaves rows that lapse. Each heartbeat renews it.
var daemonLease = time.Minute

// liveDaemons lists the daemons of room, every room for "", with a connection open on any pod.
func (s *Server) liveDaemons(room string) []liveDaemon {
	ctx, cancel := context.WithTimeout(context.Background(), sharedTimeout)
	defer cancel()
	ds, err := s.bus.Store.LiveDaemons(ctx, room, s.lease().UnixMilli())
	if err != nil {
		s.log.Warn("live_daemons_read_failed", zap.String("room", room), zap.Error(err))
		return []liveDaemon{}
	}
	out := make([]liveDaemon, len(ds))
	for i, d := range ds {
		var conns []roomConnector
		_ = json.Unmarshal([]byte(d.Connectors), &conns) // written by daemonStatus from a clean slice
		if len(conns) == 0 {
			conns = nil
		}
		out[i] = liveDaemon{ParticipantID: d.ParticipantID, CanHost: d.CanHost, Reason: d.Reason, Version: d.Version, Egress: d.Egress,
			Connectors: conns, room: d.RoomID, id: d.ID, since: d.Since}
	}
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
	// The secret only answers the poll, which mints the daemon's token: a leaked secret is no token.
	secret := NewToken()
	now := s.lease()
	p := &store.Pairing{ID: NewToken(), RoomID: room, Code: pairCode(), SecretHash: HashToken(secret), ExpiresAt: now.Add(pairingTTL)}
	ok, err := s.bus.Store.CreatePairing(r.Context(), p, maxPairingsPerRoom, maxPairings, now.UnixMilli())
	if err != nil {
		s.serverError(w, "create_pairing_failed", err)
		return
	}
	if !ok {
		httpError(w, http.StatusTooManyRequests, "too many pending pairing requests, retry in a few minutes")
		return
	}
	s.log.Info("pairing_requested", zap.String("room", room))
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": p.ID, "secret": secret, "code": p.Code, "expires_at": p.ExpiresAt.UTC(),
		"approve_url": s.baseURL + "/r/" + room + "?pair=" + p.ID,
	})
}

// pollPairing tells the daemon holding the request's secret whether it was answered. On approval
// it pairs the daemon and hands it its token, once.
func (s *Server) pollPairing(w http.ResponseWriter, r *http.Request) {
	tok := bearer(r)
	token := NewToken()
	status, d, err := s.bus.Store.PollPairing(r.Context(), r.PathValue("id"), HashToken(tok), HashToken(token), s.lease().UnixMilli())
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, "no such pairing request, or it expired")
	case err != nil:
		s.serverError(w, "poll_pairing_failed", err)
	case status == "approved":
		// The replaced daemon can no longer post its result.
		s.failDaemonTurn(r.Context(), d, "the host daemon was paired again")
		// Re-pairing replaced the previous daemon's token: close its stream.
		s.bus.Notify(d.RoomID)
		s.log.Info("daemon_paired", zap.String("room", d.RoomID), zap.Int64("participant", d.ParticipantID), zap.Int64("daemon", d.ID))
		writeJSON(w, http.StatusOK, map[string]string{"status": "approved", "token": token})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": status})
	}
}

func (s *Server) getPairing(w http.ResponseWriter, r *http.Request, sess session) {
	p, err := s.bus.Store.PendingPairing(r.Context(), sess.p.RoomID, r.PathValue("id"), s.lease().UnixMilli())
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "this pairing request expired or was already answered")
		return
	}
	if err != nil {
		s.serverError(w, "get_pairing_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": p.ID, "code": p.Code, "expires_at": p.ExpiresAt.UTC()})
}

// answerPairing pairs the daemon to the participant answering, or denies it.
func (s *Server) answerPairing(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		Approve bool `json:"approve"`
	}
	if !decode(w, r, &req) {
		return
	}
	room, id, now := sess.p.RoomID, r.PathValue("id"), s.lease().UnixMilli()
	if !req.Approve {
		if err := s.bus.Store.DenyPairing(r.Context(), room, id, now); errors.Is(err, store.ErrNotFound) {
			httpError(w, http.StatusNotFound, "this pairing request expired or was already answered")
			return
		} else if err != nil {
			s.serverError(w, "deny_pairing_failed", err)
			return
		}
		s.log.Info("pairing_denied", zap.String("room", room), zap.Int64("participant", sess.p.ID))
		writeJSON(w, http.StatusOK, map[string]string{"status": "denied"})
		return
	}
	err := s.bus.Store.ApprovePairing(r.Context(), room, id, sess.p.ID, now)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "this pairing request expired or was already answered")
		return
	}
	if err != nil {
		s.serverError(w, "pair_daemon_failed", err)
		return
	}
	s.log.Info("pairing_approved", zap.String("room", room), zap.Int64("participant", sess.p.ID))
	writeJSON(w, http.StatusOK, map[string]string{"status": "approved"})
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
		// Removing itself only lowers access: an expired seat's daemon may still (021-AC8).
		if errors.Is(err, errReauth) && r.Pattern == "DELETE /api/rooms/{room}/daemon" {
			next(w, r, d, hash)
			return
		}
		// 403, not 401: daemons wipe their pairing on a 401, and the pairing must outlive the expiry (021-AC8).
		if errors.Is(err, errReauth) {
			reasonError(w, http.StatusForbidden, "reauth", msgDaemonReauth)
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
	// A daemon acts through its seat: it lapses with its profile's sign-in (021-AC8). The daemon
	// comes with errReauth, for the one route it keeps.
	if s.reauthDue(d.OIDCBinding) {
		return d, errReauth
	}
	return d, nil
}

// daemonStatus is the daemon's heartbeat: whether it can host right now, and why not.
func (s *Server) daemonStatus(w http.ResponseWriter, r *http.Request, d *store.Daemon, _ string) {
	var req struct {
		CanHost bool   `json:"can_host"`
		Reason  string `json:"reason"`
		Version string `json:"version"`
		Egress  bool   `json:"egress"`
		// Connectors comes from a daemon of 014 on; a missing one keeps none.
		Connectors []roomConnector `json:"connectors"`
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
	conns := "[]"
	if c := cleanConnectors(req.Connectors); len(c) > 0 {
		b, _ := json.Marshal(c)
		conns = string(b)
	}
	changed, err := s.bus.Store.SetDaemonStatus(r.Context(), d.ID, store.DaemonStatus{CanHost: req.CanHost, Reason: reason, Version: version, Egress: req.Egress, Connectors: conns})
	if err != nil {
		s.serverError(w, "daemon_status_failed", err)
		return
	}
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

// removeDaemon is the host removing the room from their computer: the daemon's token stops
// working, its stream ends with "revoked", its running turn fails and the room elects another host
// (003b-AC9).
func (s *Server) removeDaemon(w http.ResponseWriter, r *http.Request, d *store.Daemon, _ string) {
	ctx := r.Context()
	if err := s.unhostDaemon(ctx, d, "the AI host removed this room from their computer"); err != nil {
		s.serverError(w, "remove_daemon_failed", err)
		return
	}
	if err := s.bus.Store.RemoveDaemon(ctx, d.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.serverError(w, "remove_daemon_failed", err)
		return
	}
	s.log.Info("daemon_removed", zap.String("room", d.RoomID), zap.Int64("daemon", d.ID))
	// Wakes the daemon's stream, which then finds its token gone, and the room's.
	s.agentChanged(ctx, d.RoomID)
	w.WriteHeader(http.StatusNoContent)
}

// unhostDaemon readies daemon d's row for deletion: out of the room's daemons first, so the drain
// that failing the turn runs hands the next queued turn elsewhere, then its running turn fails,
// found by the daemon_id the delete clears.
func (s *Server) unhostDaemon(ctx context.Context, d *store.Daemon, why string) error {
	if err := s.bus.Store.DropDaemonConnections(ctx, d.ID); err != nil {
		return err
	}
	s.failDaemonTurn(ctx, d, why)
	return nil
}

// daemonGone runs once daemon d holds no live connection on any pod: its running turn fails and
// the room elects its host again.
func (s *Server) daemonGone(ctx context.Context, d *store.Daemon) {
	// A re-pair's drain may have handed this id a turn, so fail always; an ended seat fails as its
	// cleanup does, whichever of the two runs first (016-AC9).
	why := "the host's daemon disconnected"
	if _, err := s.bus.Store.RoomParticipant(ctx, d.RoomID, d.ParticipantID); errors.Is(err, store.ErrNotFound) {
		why = whyHostLeft
	} else if err != nil {
		s.log.Error("participant_lookup_failed", zap.String("room", d.RoomID), zap.Error(err))
	}
	s.failDaemonTurn(ctx, d, why)
	s.agentChanged(ctx, d.RoomID)
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
	// One row per stream: the daemon is gone only once no pod holds a live one (017-AC12).
	conn, opened := NewToken(), s.lease()
	renew := func() error {
		return s.bus.Store.DaemonConnected(r.Context(), conn, d, s.bus.Pod(), opened.UnixMilli(), s.lease().Add(daemonLease).UnixMilli())
	}
	// The app holds one stream per room and drops its running turns with it, before it opens the
	// next: a turn of its still running that a stream marked delivered came over one whose pod may
	// have died without the cleanup. One no stream wrote, pressed while a dead pod's row still
	// counted, is announced on this one. Read before this stream counts: a turn started on it is
	// not among them.
	earlier, err := s.bus.Store.RunningTurns(r.Context(), d.RoomID)
	if err == nil {
		err = renew()
	}
	if err != nil {
		s.serverError(w, "daemon_connect_failed", err)
		return
	}
	s.log.Info("daemon_connected", zap.String("room", d.RoomID), zap.Int64("daemon", d.ID))
	s.agentChanged(r.Context(), d.RoomID)
	dropped := false
	for _, t := range earlier {
		if t.DaemonID == d.ID && t.Delivered {
			s.endTurn(r.Context(), d.RoomID, t.ID, "the host's daemon disconnected")
			dropped = true
		}
	}
	if dropped {
		s.drain(r.Context(), d.RoomID)
	}
	// A stopping pod ends the stream at once, so its cleanup runs within Serve's drain, before the
	// process exits: the app drops its running turn with the stream, and the turn fails now.
	stopped := podStopping(r)
	defer func() {
		// The request context is done: the cleanup must still reach the store.
		ctx := context.WithoutCancel(r.Context())
		gone, err := s.bus.Store.DaemonDisconnected(ctx, conn, d.ID, s.lease().UnixMilli())
		if err != nil {
			s.log.Error("daemon_disconnect_failed", zap.String("room", d.RoomID), zap.Int64("daemon", d.ID), zap.Error(err))
		}
		s.log.Info("daemon_disconnected", zap.String("room", d.RoomID), zap.Int64("daemon", d.ID))
		if gone {
			s.daemonGone(ctx, d)
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
			// Marked before it is written: the app takes an id once, so a turn it may hold must fail
			// with its stream, never wait to be announced again. Unmarked, it must not be written.
			if !t.Delivered {
				if err := s.bus.Store.TurnDelivered(r.Context(), d.RoomID, t.ID, d.ID); err != nil {
					s.log.Error("turn_delivered_failed", zap.String("room", d.RoomID), zap.Int64("turn", t.ID), zap.Error(err))
					return false
				}
			}
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
	// sendEgress hands the room's host daemon each website approval waiting for it, once per
	// stream; one dropped since, decided or refused, may come again (014-AC10).
	egress := map[int64]bool{}
	sendEgress := func() bool {
		host, err := s.bus.Store.HostDaemon(r.Context(), d.RoomID)
		if err != nil || host.ID != d.ID {
			clear(egress)
			return true
		}
		pending, err := s.bus.Store.PendingAllows(r.Context(), d.RoomID)
		if err != nil {
			return true
		}
		live := map[int64]bool{}
		for _, p := range pending {
			live[p.ID] = true
			if egress[p.ID] {
				continue
			}
			egress[p.ID] = true
			b, _ := json.Marshal(p)
			if _, err := fmt.Fprintf(w, "event: egress_allow\ndata: %s\n\n", b); err != nil {
				return false
			}
		}
		for id := range egress {
			if !live[id] {
				delete(egress, id)
			}
		}
		flusher.Flush()
		return true
	}
	if !sendTurn() || !sendRoom() || !sendInspect() || !sendPlugins() || !sendEgress() {
		return
	}
	// alive re-checks the token: a revoked invite, a deleted room or a re-pairing ends the stream.
	alive := func() bool {
		if _, err := s.daemonFor(r.Context(), hash, d.RoomID); err != nil {
			// An expiry just closes the stream: daemons wipe their pairing on "revoked" (021-AC8).
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
		case <-stopped:
			return
		case <-tick.C:
			// The bus drops a signal when this stream's buffer is full of other rooms' ones: the
			// heartbeat re-reads the room, so a turn, a stop or an inspection is at most that late.
			// A failed renewal is a removed daemon, its row gone: the stream ends.
			if !alive() || renew() != nil || !sendTurn() || !sendRoom() || !sendInspect() || !sendPlugins() || !sendEgress() {
				return
			}
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case changed := <-live:
			if (changed == d.RoomID || changed == bus.All) && (!alive() || !sendTurn() || !sendRoom() || !sendInspect() || !sendPlugins() || !sendEgress()) {
				return
			}
		}
	}
}
