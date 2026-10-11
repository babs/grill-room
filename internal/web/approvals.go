package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

// decideCap is the host's answer to the banner of a persona asking above the cap (002-AC20). The
// request names the level the host saw: a persona changed since is not decided blind. Approving
// lets the persona run as asked on the host's daemon from its next turn; the daemon's own cap
// stays as it is.
func (s *Server) decideCap(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		Approve bool `json:"approve"`
		turn.Level
	}
	if !decode(w, r, &req) {
		return
	}
	room, ok := s.asHost(w, r, sess)
	if !ok {
		return
	}
	host, err := s.bus.Store.HostDaemon(r.Context(), room.ID)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusConflict, "the host has no daemon, so no cap")
		return
	}
	if err != nil {
		s.serverError(w, "decide_cap_failed", err)
		return
	}
	p, err := s.bus.Store.RoomPersona(r.Context(), room.ID, personaID(r))
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such persona")
		return
	}
	if err != nil {
		s.serverError(w, "decide_cap_failed", err)
		return
	}
	runs, low := capped(host, p)
	if !low {
		httpError(w, http.StatusConflict, "this persona runs as it asks already")
		return
	}
	m, err := s.bus.Store.DecideCap(r.Context(), room.ID, p.ID, host, req.Level, runs, req.Approve, sess.p)
	s.decided(w, sess, m, err)
}

// decideChange is the host's answer to a persona change held while the host approves everything
// (002-AC21).
func (s *Server) decideChange(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		Approve bool `json:"approve"`
	}
	if !decode(w, r, &req) {
		return
	}
	room, ok := s.asHost(w, r, sess)
	if !ok {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("change"), 10, 64)
	m, err := s.bus.Store.DecideChange(r.Context(), room.ID, id, req.Approve, sess.p)
	s.decided(w, sess, m, err)
}

func (s *Server) decided(w http.ResponseWriter, sess session, m *store.Message, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, "no such request: it was decided already")
	case errors.Is(err, store.ErrForbidden):
		httpError(w, http.StatusForbidden, "only the agent host can do this")
	case errors.Is(err, store.ErrStale), errors.Is(err, store.ErrNameTaken):
		httpError(w, http.StatusConflict, err.Error())
	case err != nil:
		s.serverError(w, "decide_failed", err)
	default:
		s.bus.Notify(sess.p.RoomID)
		writeJSON(w, http.StatusOK, m)
	}
}
