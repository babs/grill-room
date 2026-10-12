package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/render"
	"github.com/babs/grill-room/internal/store"
)

// viewMessage is a message as participants get it. A deleted one comes without its body, which
// stays in the store for the agent.
func viewMessage(m store.Message) store.Message {
	switch {
	case m.DeletedAt != nil:
		m.Body, m.HTML = "", ""
	case m.HTML == "" && m.Kind != store.KindSystem:
		// Written before messages were rendered on write.
		m.HTML = render.MessageHTML(m.Body)
	}
	return m
}

// messageID parses {msg}; false once answered with a 404.
func messageID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("msg"), 10, 64)
	if err != nil {
		httpError(w, http.StatusNotFound, "no such message")
		return 0, false
	}
	return id, true
}

// messageChangeError answers a refused edit or delete; false when err is not one.
func messageChangeError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, "no such message")
	case errors.Is(err, store.ErrForbidden):
		httpError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, store.ErrMessageDeleted):
		httpError(w, http.StatusConflict, err.Error())
	default:
		return false
	}
	return true
}

// editMessage lets an author rewrite their text or voice line; the old body stays as a version.
func (s *Server) editMessage(w http.ResponseWriter, r *http.Request, sess session) {
	id, ok := messageID(w, r)
	if !ok {
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
		httpError(w, http.StatusBadRequest, fmt.Sprintf("message must be 1 to %d characters", maxBodyLen))
		return
	}
	m, err := s.bus.Store.EditMessage(r.Context(), sess.p.RoomID, id, sess.p.ID, body)
	if messageChangeError(w, err) {
		return
	}
	if err != nil {
		s.serverError(w, "edit_message_failed", err)
		return
	}
	s.bus.Notify(sess.p.RoomID)
	s.log.Info("message_edited", zap.String("room", sess.p.RoomID), zap.Int64("message", id), zap.Int64("participant", sess.p.ID))
	// The page redraws the line from this answer: with its chips (005-AC27).
	edited := []store.Message{*m}
	if err := s.nameMentions(r.Context(), sess.p.RoomID, edited); err != nil {
		s.log.Warn("name_mentions_failed", zap.String("room", sess.p.RoomID), zap.Error(err))
	}
	writeJSON(w, http.StatusOK, viewMessage(edited[0]))
}

// deleteMessage soft-deletes an author's text or voice line.
func (s *Server) deleteMessage(w http.ResponseWriter, r *http.Request, sess session) {
	id, ok := messageID(w, r)
	if !ok {
		return
	}
	_, err := s.bus.Store.DeleteMessage(r.Context(), sess.p.RoomID, id, sess.p.ID)
	if messageChangeError(w, err) {
		return
	}
	if err != nil {
		s.serverError(w, "delete_message_failed", err)
		return
	}
	s.bus.Notify(sess.p.RoomID)
	s.log.Info("message_deleted", zap.String("room", sess.p.RoomID), zap.Int64("message", id), zap.Int64("participant", sess.p.ID))
	w.WriteHeader(http.StatusNoContent)
}

// messageVersions lists an edited message's earlier bodies, oldest first. A deleted message has
// none to show.
func (s *Server) messageVersions(w http.ResponseWriter, r *http.Request, sess session) {
	id, ok := messageID(w, r)
	if !ok {
		return
	}
	m, err := s.bus.Store.Message(r.Context(), sess.p.RoomID, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && m.DeletedAt != nil) {
		httpError(w, http.StatusNotFound, "no such message")
		return
	}
	if err != nil {
		s.serverError(w, "message_versions_failed", err)
		return
	}
	vs, err := s.bus.Store.MessageVersions(r.Context(), sess.p.RoomID, id)
	if err != nil {
		s.serverError(w, "message_versions_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, vs)
}
