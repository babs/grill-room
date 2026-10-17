package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
)

// whyHostLeft fails the turn a removed or leaving AI host's app was running (016-AC9).
const whyHostLeft = "the AI host left the room"

// removeParticipant is a room admin, or the super-admin, removing a person from the room, blocked
// from rejoining when asked (016-AC7, 016-AC10, Q2); with the caller's own id it is a leave
// (016-AC12). Either way the person's seats end at once (016-AC8, 016-AC9).
func (s *Server) removeParticipant(w http.ResponseWriter, r *http.Request, sess *session) {
	var req struct {
		Block bool `json:"block"`
	}
	if r.ContentLength != 0 && !decode(w, r, &req) {
		return
	}
	target, err := strconv.ParseInt(r.PathValue("participant"), 10, 64)
	if err != nil {
		httpError(w, http.StatusNotFound, "no such participant in this room")
		return
	}
	room := r.PathValue("room")
	ctx := r.Context()
	e := store.Ending{Block: req.Block}
	if sess != nil {
		e.By = sess.p
		e.Leave = sess.p.ID == target
	}
	if !e.Leave {
		if !s.asRoomAdmin(w, r, sess) {
			return
		}
		ctx = s.roomAuditing(r, sess, ActionParticipantRemove, room, nil)
	}
	ended, err := s.bus.Store.EndSeats(ctx, room, target, e)
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, "no such participant in this room")
		return
	case errors.Is(err, store.ErrIsRoomAdmin) && e.Leave:
		httpError(w, http.StatusConflict, "Step down as room admin first")
		return
	case errors.Is(err, store.ErrIsRoomAdmin):
		httpError(w, http.StatusConflict, "Remove their room admin role first")
		return
	case err != nil:
		s.serverError(w, "remove_participant_failed", err)
		return
	}
	s.log.Info("participant_removed", zap.String("room", room), zap.Int64s("seats", ended.Seats), zap.Bool("left", e.Leave),
		zap.Bool("blocked", ended.Blocked), zap.Bool("host", ended.Host))
	s.seatsEnded(context.WithoutCancel(ctx), room, ended.Seats, ended.Daemons)
	w.WriteHeader(http.StatusNoContent)
}

// seatsEnded applies seats' end to what is live: their apps, daemons, leave the room's daemons
// and the turns they run fail, their audio ends, the host is elected again, and every stream wakes
// to re-check its access, theirs closing with "revoked" (016-AC8, 016-AC9). An app's stream that
// closed first left its turns to this.
func (s *Server) seatsEnded(ctx context.Context, room string, seats, daemons []int64) {
	s.presence.mu.Lock()
	for _, id := range daemons {
		delete(s.presence.m, id)
	}
	s.presence.mu.Unlock()
	for _, id := range daemons {
		s.failDaemonTurn(ctx, &store.Daemon{ID: id, RoomID: room}, whyHostLeft)
	}
	s.kickFromVoice(room, seats)
	s.agentChanged(ctx, room)
}

// listBlocks answers the profiles blocked from rejoining the room, for its admins (016-AC14).
func (s *Server) listBlocks(w http.ResponseWriter, r *http.Request, sess *session) {
	if !s.asRoomAdmin(w, r, sess) {
		return
	}
	bs, err := s.bus.Store.Blocks(r.Context(), r.PathValue("room"))
	if err != nil {
		s.serverError(w, "list_blocks_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, bs)
}

// unblock lifts a block, for the room's admins (016-AC14).
func (s *Server) unblock(w http.ResponseWriter, r *http.Request, sess *session) {
	if !s.asRoomAdmin(w, r, sess) {
		return
	}
	room := r.PathValue("room")
	profile, err := strconv.ParseInt(r.PathValue("profile"), 10, 64)
	if err != nil {
		httpError(w, http.StatusNotFound, "no such block")
		return
	}
	var by *store.Participant
	if sess != nil {
		by = sess.p
	}
	err = s.bus.Store.Unblock(s.roomAuditing(r, sess, ActionParticipantUnblock, room, nil), room, profile, by)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such block")
		return
	}
	if err != nil {
		s.serverError(w, "unblock_failed", err)
		return
	}
	s.bus.Notify(room)
	s.log.Info("participant_unblocked", zap.String("room", room), zap.Int64("profile", profile))
	w.WriteHeader(http.StatusNoContent)
}
