package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/anchor"
	"github.com/babs/grill-room/internal/convert"
	"github.com/babs/grill-room/internal/store"
)

// maxAttachments bounds the images on one message.
const maxAttachments = 4

type attachmentReq struct {
	// Data is the image, base64 in JSON. Its type is read from the bytes, never from the client.
	Data []byte `json:"data"`
}

type messageReq struct {
	Body        string          `json:"body"`
	Attachments []attachmentReq `json:"attachments"`
}

type annotationReq struct {
	messageReq
	// Revision is the revision the anchor was taken on; ignored for a whole-document comment.
	Revision int            `json:"revision"`
	Anchor   *anchor.Anchor `json:"anchor"`
}

// messageLimit is the largest JSON body a message may take: its images in base64, plus the text.
func (s *Server) messageLimit() int64 {
	return maxAttachments*(s.opts.MaxAttachmentBytes*4/3+4) + 64<<10
}

// readMessage holds an upload slot while it reads a message body, which may carry images.
// release must be called once ok.
func (s *Server) readMessage(w http.ResponseWriter, r *http.Request, v any) (release func(), ok bool) {
	select {
	case s.uploads <- struct{}{}:
	default:
		httpError(w, http.StatusServiceUnavailable, "too many uploads in progress, retry in a moment")
		return nil, false
	}
	release = func() { <-s.uploads }
	if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadReadTimeout)); err != nil {
		release()
		s.serverError(w, "message_deadline_failed", err)
		return nil, false
	}
	if !decodeLimit(w, r, v, s.messageLimit()) {
		release()
		return nil, false
	}
	return release, true
}

// newMessage checks a message from the session's participant, or answers the error itself.
func (s *Server) newMessage(w http.ResponseWriter, req *messageReq, sess session) (*store.NewMessage, bool) {
	if len(req.Attachments) > maxAttachments {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("at most %d images per message", maxAttachments))
		return nil, false
	}
	body, ok := cleanText(req.Body, maxBodyLen, true)
	// An image alone is a message.
	if !ok && (len(req.Attachments) == 0 || strings.TrimSpace(req.Body) != "") {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("message must be 1 to %d characters", maxBodyLen))
		return nil, false
	}
	m := &store.NewMessage{ParticipantID: sess.p.ID, Author: sess.p.Name, Body: body, At: s.now()}
	for _, at := range req.Attachments {
		if int64(len(at.Data)) > s.opts.MaxAttachmentBytes {
			httpError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("image is over the %s limit", sizeText(s.opts.MaxAttachmentBytes)))
			return nil, false
		}
		ct, ok := convert.Image(at.Data)
		if !ok {
			httpError(w, http.StatusUnsupportedMediaType, "attachments must be PNG, JPEG, GIF or WebP images")
			return nil, false
		}
		m.Attachments = append(m.Attachments, store.Attachment{ContentType: ct, Data: at.Data})
	}
	return m, true
}

// annotationError answers the store errors creating or extending an annotation can return.
func (s *Server) annotationError(w http.ResponseWriter, r *http.Request, room, event string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, "no such annotation or document")
	case errors.Is(err, store.ErrStaleRevision):
		httpError(w, http.StatusConflict, "the document changed since it was opened, reload it")
	case errors.Is(err, store.ErrNotAnchorable):
		httpError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrRoomFull):
		s.roomFull(w, r, room)
	default:
		s.serverError(w, event, err)
	}
}

func (s *Server) listAnnotations(w http.ResponseWriter, r *http.Request, sess session) {
	d, ok := s.document(w, r, sess)
	if !ok {
		return
	}
	list, err := s.bus.Store.Annotations(r.Context(), d.RoomID, d.ID)
	if err != nil {
		s.serverError(w, "list_annotations_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) createAnnotation(w http.ResponseWriter, r *http.Request, sess session) {
	d, ok := s.document(w, r, sess)
	if !ok {
		return
	}
	var req annotationReq
	release, ok := s.readMessage(w, r, &req)
	if !ok {
		return
	}
	defer release()
	if req.Anchor != nil {
		if err := req.Anchor.Validate(); err != nil {
			httpError(w, http.StatusBadRequest, "bad anchor: "+err.Error())
			return
		}
	}
	msg, ok := s.newMessage(w, &req.messageReq, sess)
	if !ok {
		return
	}
	a, err := s.bus.Store.CreateAnnotation(r.Context(), d.RoomID, d.ID, req.Revision, req.Anchor, msg, s.opts.MaxRoomBytes)
	if err != nil {
		s.annotationError(w, r, sess.p.RoomID, "create_annotation_failed", err)
		return
	}
	s.bus.Notify(d.RoomID)
	s.log.Info("annotation_created", zap.String("room", d.RoomID), zap.Int64("document", d.ID), zap.Int64("annotation", a.ID))
	writeJSON(w, http.StatusCreated, struct {
		*store.Annotation
		Notices []mentionNotice `json:"notices,omitempty"`
	}{a, s.noteMentionTurns(r.Context(), sess.p, a.ID, &a.Messages[0])})
}

// annotationID reads {ann}; ok false answers 404.
func annotationID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("ann"), 10, 64)
	if err != nil {
		httpError(w, http.StatusNotFound, "no such annotation")
	}
	return id, err == nil
}

func (s *Server) postAnnotationMessage(w http.ResponseWriter, r *http.Request, sess session) {
	id, ok := annotationID(w, r)
	if !ok {
		return
	}
	var req messageReq
	release, ok := s.readMessage(w, r, &req)
	if !ok {
		return
	}
	defer release()
	msg, ok := s.newMessage(w, &req, sess)
	if !ok {
		return
	}
	m, err := s.bus.Store.AddAnnotationMessage(r.Context(), sess.p.RoomID, id, msg, s.opts.MaxRoomBytes)
	if err != nil {
		s.annotationError(w, r, sess.p.RoomID, "annotation_message_failed", err)
		return
	}
	s.bus.Notify(sess.p.RoomID)
	writeJSON(w, http.StatusCreated, struct {
		*store.AnnotationMessage
		Notices []mentionNotice `json:"notices,omitempty"`
	}{m, s.noteMentionTurns(r.Context(), sess.p, id, m)})
}

func (s *Server) setAnnotationStatus(w http.ResponseWriter, r *http.Request, sess session) {
	id, ok := annotationID(w, r)
	if !ok {
		return
	}
	var req struct {
		Status string `json:"status"`
	}
	if !decode(w, r, &req) {
		return
	}
	// "open" is only ever the first state: an annotation done then picked up again is reopened.
	if req.Status != store.StatusDone && req.Status != store.StatusReopened {
		httpError(w, http.StatusBadRequest, `status must be "done" or "reopened"`)
		return
	}
	err := s.bus.Store.SetAnnotationStatus(r.Context(), sess.p.RoomID, id, req.Status)
	if err != nil {
		s.annotationError(w, r, sess.p.RoomID, "annotation_status_failed", err)
		return
	}
	s.bus.Notify(sess.p.RoomID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getAttachment(w http.ResponseWriter, r *http.Request, sess session) {
	id, err := strconv.ParseInt(r.PathValue("att"), 10, 64)
	if err != nil {
		httpError(w, http.StatusNotFound, "no such attachment")
		return
	}
	at, err := s.bus.Store.Attachment(r.Context(), sess.p.RoomID, id)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such attachment")
		return
	}
	if err != nil {
		s.serverError(w, "get_attachment_failed", err)
		return
	}
	h := w.Header()
	h.Set("Content-Security-Policy", originalCSP)
	h.Set("Content-Type", at.ContentType)
	h.Set("Content-Disposition", "inline")
	h.Set("Content-Length", strconv.FormatInt(at.Size, 10))
	_, _ = w.Write(at.Data)
}
