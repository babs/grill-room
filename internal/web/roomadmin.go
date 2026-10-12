package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
)

// ActorProfile prefixes the profile uuid of a room admin as an audit actor (004-AC6).
const ActorProfile = "profile:"

// Audit actions of room admins and instance settings (004-AC6 to 004-AC8).
const (
	ActionRoomAdminAdd    = "room_admin.add"
	ActionRoomAdminRemove = "room_admin.remove"
	ActionRoomSkillRemove = "room_skill.remove"
	ActionSettings        = "instance.settings"
	ActionProfileMaxRooms = "profile.max_rooms"
)

// maxStorageQuota bounds the instance storage quota: 1 PiB.
const maxStorageQuota = 1 << 50

// roomAdmin reports whether the member's profile administers the room, and remembers its uuid in
// sess for the audit log.
func (s *Server) roomAdmin(ctx context.Context, sess *session) (bool, error) {
	if sess.adminUUID != "" {
		return true, nil
	}
	if sess.p.ProfileID == 0 {
		return false, nil
	}
	uuid, err := s.bus.Store.RoomAdmin(ctx, sess.p.RoomID, sess.p.ProfileID)
	if errors.Is(err, store.ErrForbidden) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	sess.adminUUID = uuid
	return true, nil
}

// asRoomAdmin answers a 403 and returns false unless sess is nil (the super-admin) or a room admin.
func (s *Server) asRoomAdmin(w http.ResponseWriter, r *http.Request, sess *session) bool {
	if sess == nil {
		return true
	}
	ok, err := s.roomAdmin(r.Context(), sess)
	if err != nil {
		s.serverError(w, "room_admin_lookup_failed", err)
		return false
	}
	if !ok {
		httpError(w, http.StatusForbidden, "only a room admin can do this")
	}
	return ok
}

// roomAuditing is auditing for an action on room: by the super-admin when sess is nil, by the
// member's profile when sess was checked as a room admin, and not audited otherwise: a plain
// member's own invites are no admin action.
func (s *Server) roomAuditing(r *http.Request, sess *session, action, room string, details map[string]any) context.Context {
	if sess == nil {
		return s.auditing(r, action, store.TargetRoom, room, "", details)
	}
	if sess.adminUUID == "" {
		return r.Context()
	}
	if details == nil {
		details = map[string]any{}
	}
	details["as"], details["participant"] = sess.p.Name, sess.p.ID
	e := store.AuditEntry{At: s.now(), Actor: ActorProfile + sess.adminUUID, Action: action, TargetKind: store.TargetRoom, TargetID: room}
	return store.WithAudit(r.Context(), e, details)
}

// setRoomAdmin appoints (PUT) or removes (DELETE) the profile of a participant as a room admin.
// Only a room admin may; the last one cannot step down (004-AC8).
func (s *Server) setRoomAdmin(w http.ResponseWriter, r *http.Request, sess session) {
	if !s.asRoomAdmin(w, r, &sess) {
		return
	}
	on := r.Method == http.MethodPut
	target, _ := strconv.ParseInt(r.PathValue("participant"), 10, 64)
	action := ActionRoomAdminRemove
	if on {
		action = ActionRoomAdminAdd
	}
	room := sess.p.RoomID
	m, err := s.bus.Store.SetRoomAdmin(s.roomAuditing(r, &sess, action, room, nil), room, target, on, sess.p)
	switch {
	case errors.Is(err, store.ErrForbidden):
		httpError(w, http.StatusForbidden, "only a room admin can do this")
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, "no such participant in this room")
	case errors.Is(err, store.ErrNoProfile), errors.Is(err, store.ErrLastAdmin):
		httpError(w, http.StatusConflict, err.Error())
	case err != nil:
		s.serverError(w, "set_room_admin_failed", err)
	default:
		if m != nil {
			s.bus.Notify(room)
			s.log.Info("room_admin_set", zap.String("room", room), zap.Int64("participant", target), zap.Bool("admin", on), zap.Int64("by", sess.p.ID))
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// roomOverview is the room's volume and quota, for its admins and the super-admin (004-AC8).
func (s *Server) roomOverview(w http.ResponseWriter, r *http.Request, sess session) {
	if !s.asRoomAdmin(w, r, &sess) {
		return
	}
	o, err := s.bus.Store.RoomOverview(r.Context(), sess.p.RoomID, s.opts.MaxRoomBytes)
	if err != nil {
		s.serverError(w, "room_overview_failed", err)
		return
	}
	v := roomOverview{RoomOverview: *o, Used: o.Volume.Counted()}
	v.HostOnline = o.HostParticipantID != 0 && s.onlineHosts()[o.ID][o.HostParticipantID]
	writeJSON(w, http.StatusOK, v)
}

// createProfileRoom creates a room for the profile of the browser, which joins it and becomes its
// room admin, within the instance's guardrails (004-AC7).
func (s *Server) createProfileRoom(w http.ResponseWriter, r *http.Request) {
	p := s.profileFor(w, r)
	if p == nil {
		return
	}
	var req struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
	}
	if !decode(w, r, &req) {
		return
	}
	name, ok := cleanText(req.Name, maxRoomName, false)
	if !ok {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("name must be 1 to %d characters", maxRoomName))
		return
	}
	display, ok := cleanText(req.DisplayName, maxNameLen, false)
	if !ok {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("display name must be 1 to %d characters", maxNameLen))
		return
	}
	id := strings.ToLower(NewToken())
	e := store.AuditEntry{At: s.now(), Actor: ActorProfile + p.UUID, Action: ActionRoomCreate, TargetKind: store.TargetRoom, TargetID: id, TargetName: name}
	ctx := store.WithAudit(r.Context(), e, map[string]any{"as": display})
	room, err := s.bus.Store.CreateProfileRoom(ctx, id, name, p.ID, display, HashToken(NewToken()), HashToken(NewToken()))
	switch {
	case errors.Is(err, store.ErrCreationRefused):
		httpError(w, http.StatusForbidden, err.Error())
	case err != nil:
		s.serverError(w, "create_room_failed", err)
	default:
		s.log.Info("room_created", zap.String("room", room.ID), zap.Int64("profile", p.ID))
		writeJSON(w, http.StatusCreated, room)
	}
}

// setProfileMaxRooms sets a profile's own limit on the rooms it creates: `max_rooms` 1 to 1000,
// -1 for unlimited, or null for the instance's (004-AC7).
func (s *Server) setProfileMaxRooms(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MaxRooms json.RawMessage `json:"max_rooms"`
	}
	if !decode(w, r, &req) {
		return
	}
	var n *int
	if len(req.MaxRooms) == 0 || (string(req.MaxRooms) != "null" && (json.Unmarshal(req.MaxRooms, &n) != nil || n == nil ||
		(*n != store.UnlimitedRooms && (*n < 1 || *n > 1000)))) {
		httpError(w, http.StatusBadRequest, "max_rooms must be 1 to 1000, -1 for unlimited, or null for the instance's limit")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		err = s.bus.Store.SetProfileMaxRooms(s.auditing(r, ActionProfileMaxRooms, store.TargetProfile, strconv.FormatInt(id, 10), "",
			map[string]any{"max_rooms": n}), id, n)
	}
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, strconv.ErrSyntax) || errors.Is(err, strconv.ErrRange) {
		httpError(w, http.StatusNotFound, "no such profile")
		return
	}
	if err != nil {
		s.serverError(w, "set_profile_max_rooms_failed", err)
		return
	}
	s.log.Info("profile_max_rooms_set", zap.Int64("profile", id), zap.Any("max_rooms", n))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) instanceSettings(w http.ResponseWriter, r *http.Request) {
	v, err := s.bus.Store.InstanceSettings(r.Context())
	if err != nil {
		s.serverError(w, "instance_settings_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// setInstanceSettings sets the guardrails on rooms profiles create (004-AC7).
func (s *Server) setInstanceSettings(w http.ResponseWriter, r *http.Request) {
	var v store.InstanceSettings
	if !decode(w, r, &v) {
		return
	}
	switch {
	case v.RoomsPerProfile < 1 || v.RoomsPerProfile > 1000:
		httpError(w, http.StatusBadRequest, "rooms_per_profile must be 1 to 1000")
		return
	case v.StorageQuota < 0 || v.StorageQuota > maxStorageQuota:
		httpError(w, http.StatusBadRequest, "storage_quota_bytes must be 0 (no quota) to 1 PiB")
		return
	case v.RoomCreation != store.CreationAnyone && v.RoomCreation != store.CreationSuperAdmin:
		httpError(w, http.StatusBadRequest, "room_creation must be anyone or super-admin")
		return
	}
	details := map[string]any{"rooms_per_profile": v.RoomsPerProfile, "storage_quota_bytes": v.StorageQuota, "room_creation": v.RoomCreation}
	if err := s.bus.Store.SetInstanceSettings(s.auditing(r, ActionSettings, store.TargetInstance, "", "", details), v); err != nil {
		s.serverError(w, "set_instance_settings_failed", err)
		return
	}
	s.log.Info("instance_settings_set", zap.Int("rooms_per_profile", v.RoomsPerProfile), zap.Int64("storage_quota_bytes", v.StorageQuota),
		zap.String("room_creation", v.RoomCreation))
	writeJSON(w, http.StatusOK, v)
}
