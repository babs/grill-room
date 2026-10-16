package web

import (
	"context"
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
	m, prev, err := s.bus.Store.EditMessage(r.Context(), sess.p.RoomID, id, sess.p.ID, body)
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
	var notices []mentionNotice
	if m.Body != prev {
		notices = s.editMentions(r.Context(), sess.p, m, prev)
	}
	writeJSON(w, http.StatusOK, struct {
		store.Message
		Notices []mentionNotice `json:"mention_notices,omitempty"`
	}{viewMessage(edited[0]), notices})
}

// editMentions is mentionTurns for line m edited from body prev: only the personas m names that
// prev did not and that the line never queued run; unknown-name notices cover the new names only.
// An edit never re-runs a persona: named, edited out, then edited back (016-AC19, 016-AC20).
func (s *Server) editMentions(ctx context.Context, p *store.Participant, m *store.Message, prev string) []mentionNotice {
	states, err := s.bus.Store.MessageMentions(ctx, m.RoomID, m.ID-1, m.ID)
	if err != nil {
		s.log.Error("mention_failed", zap.String("room", m.RoomID), zap.Error(err))
		return nil
	}
	var linked []int64
	for _, st := range states[m.ID] {
		linked = append(linked, st.PersonaID)
	}
	return s.callMentioned(ctx, p, m.Body, prev, "", linked, func(ctx context.Context, q *store.QueuedTurn) error {
		return s.bus.Store.EnqueueMention(ctx, m.ID, q)
	})
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

// maxMessagesPage bounds one page of GET .../messages.
const maxMessagesPage = 500

// listMessages answers the room's lines after ?since=, at most ?limit= (200 by default), oldest
// first, as text without their HTML, for agents: the room reads them through its event stream
// (012, get_transcript). next is the since= of the following page, 0 on the last one.
func (s *Server) listMessages(w http.ResponseWriter, r *http.Request, sess session) {
	q := r.URL.Query()
	since, limit := int64(0), 200
	var err error
	if v := q.Get("since"); v != "" {
		if since, err = strconv.ParseInt(v, 10, 64); err != nil || since < 0 {
			httpError(w, http.StatusBadRequest, "since must be a message id from 0")
			return
		}
	}
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > maxMessagesPage {
			httpError(w, http.StatusBadRequest, fmt.Sprintf("limit must be 1 to %d", maxMessagesPage))
			return
		}
	}
	ms, err := s.bus.Store.Messages(r.Context(), sess.p.RoomID, since, limit)
	if err != nil {
		s.serverError(w, "list_messages_failed", err)
		return
	}
	out := make([]store.Message, len(ms))
	for i, m := range ms {
		out[i] = viewMessage(m)
		out[i].HTML = ""
	}
	var next int64
	if len(ms) == limit {
		next = ms[len(ms)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": out, "next": next})
}
