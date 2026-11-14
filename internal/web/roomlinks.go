package web

import (
	"errors"
	"net/http"
	"strconv"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
)

// Audit actions of room links (023-AC6).
const (
	ActionRoomLinkCreate = store.ActionRoomLinkCreate
	ActionRoomLinkDelete = "room_link.delete"
)

const (
	msgLinkBothAdmin   = "You must be a room admin of both rooms"
	msgLinkEitherAdmin = "You must be a room admin of either room"
	msgLinkSelf        = "A room cannot read itself: pick another room"
)

// listRoomLinks lists the room's links both ways, for any member (023-AC1). With ?participant=<id>
// it marks the rooms reading this one where that person sits, among those the caller sits in,
// for a room admin only, who may remove them (023-AC15): anyone else could map seats across rooms.
func (s *Server) listRoomLinks(w http.ResponseWriter, r *http.Request, sess session) {
	q := r.URL.Query().Get("participant")
	if q != "" && !s.asRoomAdmin(w, r, &sess) {
		return
	}
	ls, err := s.bus.Store.RoomLinks(r.Context(), sess.p.RoomID, sess.p.ProfileID)
	if err != nil {
		s.serverError(w, "list_room_links_failed", err)
		return
	}
	if q != "" {
		pid, err := strconv.ParseInt(q, 10, 64)
		if err != nil {
			httpError(w, http.StatusBadRequest, "participant must be an id")
			return
		}
		seated, err := s.bus.Store.SeatedRooms(r.Context(), sess.p.RoomID, pid)
		if err != nil {
			s.serverError(w, "list_room_links_failed", err)
			return
		}
		for i := range ls {
			ls[i].PersonMember = ls[i].Member && ls[i].ReadRoom == sess.p.RoomID && seated[ls[i].ReaderRoom]
		}
	}
	writeJSON(w, http.StatusOK, ls)
}

// createRoomLinks links the room with another, one or both ways, for a room admin of both
// (023-AC2): 201 when a link is new, 200 when all existed (023-AC3).
func (s *Server) createRoomLinks(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		Room   string `json:"room"`
		Reads  bool   `json:"reads"`
		ReadBy bool   `json:"read_by"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Room == "" || !req.Reads && !req.ReadBy {
		httpError(w, http.StatusBadRequest, "pick a room and at least one direction")
		return
	}
	room := sess.p.RoomID
	if req.Room == room {
		httpError(w, http.StatusBadRequest, msgLinkSelf)
		return
	}
	if ok, err := s.roomAdmin(r.Context(), &sess); err != nil {
		s.serverError(w, "room_admin_lookup_failed", err)
		return
	} else if !ok {
		httpError(w, http.StatusForbidden, msgLinkBothAdmin)
		return
	}
	ls, created, err := s.bus.Store.CreateRoomLinks(s.roomAuditing(r, &sess, ActionRoomLinkCreate, room, nil), room, req.Room, req.Reads, req.ReadBy, sess.p)
	switch {
	case errors.Is(err, store.ErrForbidden):
		httpError(w, http.StatusForbidden, msgLinkBothAdmin)
	case errors.Is(err, store.ErrLinkCap):
		httpError(w, http.StatusConflict, err.Error())
	case err != nil:
		s.serverError(w, "create_room_links_failed", err)
	case !created:
		writeJSON(w, http.StatusOK, ls)
	default:
		s.bus.Notify(room)
		s.bus.Notify(req.Room)
		s.log.Info("room_links_created", zap.String("room", room), zap.String("other", req.Room), zap.Int("links", len(ls)))
		writeJSON(w, http.StatusCreated, ls)
	}
}

// deleteRoomLink removes one link, for a room admin of either room (023-AC5).
func (s *Server) deleteRoomLink(w http.ResponseWriter, r *http.Request, sess session) {
	id, err := strconv.ParseInt(r.PathValue("link"), 10, 64)
	if err != nil {
		httpError(w, http.StatusNotFound, "no such link")
		return
	}
	room := sess.p.RoomID
	l, err := s.bus.Store.RoomLink(r.Context(), room, id)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such link")
		return
	}
	if err != nil {
		s.serverError(w, "room_link_lookup_failed", err)
		return
	}
	// The audit actor is the profile, admin of whichever side it is.
	for _, side := range []string{l.ReaderRoom, l.ReadRoom} {
		if sess.adminUUID != "" || sess.p.ProfileID == 0 {
			break
		}
		uuid, err := s.bus.Store.RoomAdmin(r.Context(), side, sess.p.ProfileID)
		if err != nil && !errors.Is(err, store.ErrForbidden) {
			s.serverError(w, "room_admin_lookup_failed", err)
			return
		}
		sess.adminUUID = uuid
	}
	if sess.adminUUID == "" {
		httpError(w, http.StatusForbidden, msgLinkEitherAdmin)
		return
	}
	l, err = s.bus.Store.DeleteRoomLink(s.roomAuditing(r, &sess, ActionRoomLinkDelete, room, nil), room, id, sess.p)
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, "no such link")
	case errors.Is(err, store.ErrForbidden):
		httpError(w, http.StatusForbidden, msgLinkEitherAdmin)
	case err != nil:
		s.serverError(w, "delete_room_link_failed", err)
	default:
		s.bus.Notify(l.ReaderRoom)
		s.bus.Notify(l.ReadRoom)
		s.log.Info("room_link_deleted", zap.String("room", room), zap.Int64("link", id))
		w.WriteHeader(http.StatusNoContent)
	}
}
