package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
)

// ActorProfile prefixes the profile uuid of a room admin as an audit actor (004-AC6).
const ActorProfile = store.ActorProfile

const (
	// DefaultAnonymousRoomsPerHour is how many rooms one client IP creates without a profile in
	// anonRoomWindow (019-AC4).
	DefaultAnonymousRoomsPerHour = 3
	anonRoomWindow               = time.Hour
)

// Audit actions of room admins and instance settings (004-AC6 to 004-AC8).
const (
	ActionRoomAdminAdd    = "room_admin.add"
	ActionRoomAdminRemove = "room_admin.remove"
	ActionRoomSkillRemove = "room_skill.remove"
	ActionSettings        = "instance.settings"
	ActionProfileMaxRooms = "profile.max_rooms"
	ActionRename          = "participant.rename"
	ActionDocumentDelete  = "document.delete"
	ActionDocumentRestore = "document.restore"
	ActionHostTransfer    = "room.host_transfer"
	// 016-AC10 016-AC14: a person removed from a room, a block lifted.
	ActionParticipantRemove  = "participant.remove"
	ActionParticipantUnblock = "participant.unblock"
	// ActionPersonaRemove is audited for API token calls only (012-AC20).
	ActionPersonaRemove = "persona.remove"
)

// maxStorageQuota bounds the instance storage quota: 1 PiB.
const maxStorageQuota = 1 << 50

// maxPluginMB bounds the plugin snapshot cap (009-AC9).
const maxPluginMB = 200

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
// member's profile when sess was checked as a room admin or came with an API token (012-AC20), and
// not audited otherwise: a plain member's own invites are no admin action.
func (s *Server) roomAuditing(r *http.Request, sess *session, action, room string, details map[string]any) context.Context {
	if sess == nil {
		return s.auditing(r, action, store.TargetRoom, room, "", details)
	}
	uuid := sess.adminUUID
	if t := apiTokenOf(r); uuid == "" && t != nil {
		uuid = t.ProfileUUID
	}
	if uuid == "" {
		return r.Context()
	}
	if details == nil {
		details = map[string]any{}
	}
	details["as"], details["participant"] = sess.p.Name, sess.p.ID
	e := store.AuditEntry{At: s.now(), Actor: ActorProfile + uuid, Action: action, TargetKind: store.TargetRoom, TargetID: room}
	return store.WithAudit(r.Context(), e, details)
}

// setRoomAdmin appoints (PUT) or removes (DELETE) the profile of a participant as a room admin.
// Only a room admin or the super-admin may; the last one cannot step down (004-AC8).
func (s *Server) setRoomAdmin(w http.ResponseWriter, r *http.Request, sess *session) {
	if !s.asRoomAdmin(w, r, sess) {
		return
	}
	on := r.Method == http.MethodPut
	target, _ := strconv.ParseInt(r.PathValue("participant"), 10, 64)
	action := ActionRoomAdminRemove
	if on {
		action = ActionRoomAdminAdd
	}
	room := r.PathValue("room")
	var by *store.Participant
	if sess != nil {
		by = sess.p
	}
	m, err := s.bus.Store.SetRoomAdmin(s.roomAuditing(r, sess, action, room, nil), room, target, on, by)
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
			s.log.Info("room_admin_set", zap.String("room", room), zap.Int64("participant", target), zap.Bool("admin", on), zap.Bool("super_admin", by == nil))
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// adminParticipant is a participant as the super-admin appoints room admins among them.
type adminParticipant struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Admin      bool   `json:"admin"`
	HasProfile bool   `json:"has_profile"`
	// Client is where the participant connected from (005-AC20): the super-admin's view only.
	Client *store.Client `json:"client,omitempty"`
	// Host is the AI host; CanHost, a connected daemon of theirs can take the role (009-AC33).
	Host    bool `json:"host"`
	CanHost bool `json:"can_host"`
}

// roomParticipants lists a room's participants for the super-admin, who appoints room admins among
// those with a profile (004-AC8).
func (s *Server) roomParticipants(w http.ResponseWriter, r *http.Request) {
	room := r.PathValue("room")
	rm, err := s.bus.Store.Room(r.Context(), room)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such room")
		return
	}
	if err != nil {
		s.serverError(w, "room_participants_failed", err)
		return
	}
	ps, err := s.bus.Store.Participants(r.Context(), room)
	if err != nil {
		s.serverError(w, "room_participants_failed", err)
		return
	}
	cs, err := s.bus.Store.RoomClients(r.Context(), room)
	if err != nil {
		s.serverError(w, "room_participants_failed", err)
		return
	}
	out := make([]adminParticipant, len(ps))
	for i, p := range ps {
		out[i] = adminParticipant{ID: p.ID, Name: p.Name, Admin: p.Admin, HasProfile: p.ProfileID != 0, Host: p.ID == rm.HostParticipantID}
		out[i].CanHost = s.canHostDaemon(room, p.ID) != nil
		if c, ok := cs[p.ID]; ok {
			out[i].Client = &c
		}
	}
	writeJSON(w, http.StatusOK, out)
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
	if p == nil || s.refuseExternal(w, p) {
		return
	}
	var req struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		starterRoomReq
	}
	if !decode(w, r, &req) {
		return
	}
	name, ok := cleanText(req.Name, maxRoomName, false)
	// The editor sends what a starter brings (006-AC9); a request without it gets the Default persona.
	var setup *store.RoomSetup
	if req.Starter != nil || req.Personas != nil {
		var err error
		name, setup, err = s.starterSetup(r.Context(), p.ID, req.Name, &req.starterRoomReq)
		var bad badRequest
		switch {
		case errors.As(err, &bad):
			httpError(w, http.StatusBadRequest, bad.Error())
			return
		case errors.Is(err, store.ErrNotFound):
			httpError(w, http.StatusNotFound, "no such starter")
			return
		case err != nil:
			s.serverError(w, "create_room_failed", err)
			return
		}
		ok = true
	}
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
	details := map[string]any{"as": display}
	if setup != nil && setup.From != nil {
		details["starter"] = setup.From
	}
	ctx := store.WithAudit(r.Context(), e, details)
	link := NewToken()
	room, err := s.bus.Store.CreateProfileRoom(ctx, id, name, p.ID, display, HashToken(NewToken()), HashToken(NewToken()), link, HashToken(link), setup)
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

// createAnonymousRoom creates, for a browser without a profile, a profile and a blank room it joins
// as room admin, when the operator allows it, a few times per client IP an hour (019-AC1 to 019-AC7).
func (s *Server) createAnonymousRoom(w http.ResponseWriter, r *http.Request) {
	if !s.opts.AnonymousRooms {
		http.NotFound(w, r)
		return
	}
	if crossSite(r) {
		httpError(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	if p, err := s.profileFromCookie(r); err != nil {
		s.serverError(w, "create_room_failed", err)
		return
	} else if p != nil {
		httpError(w, http.StatusConflict, "You already have a profile: create the room from your profile page")
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
	// The creation mode, a settings read, is checked before the limiter: a refusal costs no write; the
	// quota scan comes after it, so an IP over its limit cannot run it; a quota refusal gives its slot
	// back (019-AC4, 019-AC5). ponytail: at the quota an IP under its limit repeats the scan; cache
	// "at quota" for a few seconds if that ever shows up.
	if err := s.bus.Store.AnonymousCreationAllowed(r.Context()); errors.Is(err, store.ErrCreationRefused) {
		httpError(w, http.StatusForbidden, err.Error())
		return
	} else if err != nil {
		s.serverError(w, "create_room_failed", err)
		return
	}
	hit, retry, err := s.anonLimit.allow(r.Context(), "ip:"+rateIP(s.clientIP(r)), s.opts.AnonymousRoomsPerHour, s.now())
	if err != nil {
		s.serverError(w, "anon_room_limit_failed", err)
		return
	}
	if hit == 0 {
		wait := retryAfter(w, retry)
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"reason": "rate_limited", "retry_after_s": wait,
			"error": "too many rooms created from your network, retry in " + strconv.FormatInt((wait+59)/60, 10) + " min"})
		return
	}
	id := strings.ToLower(NewToken())
	e := store.AuditEntry{At: s.now(), Action: ActionRoomCreate, TargetKind: store.TargetRoom, TargetID: id, TargetName: name}
	ctx := store.WithAudit(r.Context(), e, map[string]any{"as": display, "anonymous": true})
	recovery, profileTok, tok, link := NewToken(), NewToken(), NewToken(), NewToken()
	room, profileID, err := s.bus.Store.CreateAnonymousRoom(ctx, id, name, display, HashToken(recovery), HashToken(profileTok),
		HashToken(NewToken()), HashToken(tok), link, HashToken(link))
	if err != nil {
		// A refused or failed creation spends no allowance (019-AC4).
		if err := s.anonLimit.unhit(context.WithoutCancel(r.Context()), hit); err != nil {
			s.log.Warn("anon_room_unhit_failed", zap.Error(err))
		}
		if errors.Is(err, store.ErrCreationRefused) {
			httpError(w, http.StatusForbidden, err.Error())
		} else {
			s.serverError(w, "create_room_failed", err)
		}
		return
	}
	s.log.Info("room_created", zap.String("room", room.ID), zap.Int64("profile", profileID), zap.Bool("anonymous", true))
	setRoomCookie(w, r, room.ID, tok)
	setProfileCookie(w, r, profileTok)
	writeJSON(w, http.StatusCreated, struct {
		*store.Room
		RecoveryURL string `json:"recovery_url"`
	}{room, s.recoveryURL(recovery)})
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

// setInstanceSettings sets the guardrails on rooms profiles create (004-AC7) and the empty
// profiles purge (011-AC17).
func (s *Server) setInstanceSettings(w http.ResponseWriter, r *http.Request) {
	var patch json.RawMessage
	if !decode(w, r, &patch) {
		return
	}
	// A field the request leaves out keeps its value: a page from before plugin_max_mb sends none.
	v, err := s.bus.Store.UpdateInstanceSettings(s.auditing(r, ActionSettings, store.TargetInstance, "", "", nil), func(v *store.InstanceSettings) error {
		if err := json.Unmarshal(patch, v); err != nil {
			return badRequest("bad json: " + err.Error())
		}
		switch {
		case v.RoomsPerProfile < 1 || v.RoomsPerProfile > 1000:
			return badRequest("rooms_per_profile must be 1 to 1000")
		case v.StorageQuota < 0 || v.StorageQuota > maxStorageQuota:
			return badRequest("storage_quota_bytes must be 0 (no quota) to 1 PiB")
		case v.RoomCreation != store.CreationAnyone && v.RoomCreation != store.CreationSuperAdmin:
			return badRequest("room_creation must be anyone or super-admin")
		case v.PluginMaxMB < 1 || v.PluginMaxMB > maxPluginMB:
			return badRequest(fmt.Sprintf("plugin_max_mb must be 1 to %d", maxPluginMB))
		}
		return nil
	})
	var bad badRequest
	switch {
	case errors.As(err, &bad):
		httpError(w, http.StatusBadRequest, bad.Error())
		return
	case err != nil:
		s.serverError(w, "set_instance_settings_failed", err)
		return
	}
	s.log.Info("instance_settings_set", zap.Int("rooms_per_profile", v.RoomsPerProfile), zap.Int64("storage_quota_bytes", v.StorageQuota),
		zap.String("room_creation", v.RoomCreation), zap.Int("plugin_max_mb", v.PluginMaxMB),
		zap.Bool("purge_empty_profiles", v.PurgeEmptyProfiles))
	writeJSON(w, http.StatusOK, v)
}

// renameParticipant sets a participant's display name, with the validation of a join: their own
// (005-AC1), or anyone's by a room admin or the super-admin (005-AC21), who are audited.
func (s *Server) renameParticipant(w http.ResponseWriter, r *http.Request, sess *session) {
	var req struct {
		Name string `json:"name"`
		// Profile also makes it the name of the renamer's own profile.
		Profile bool `json:"profile"`
	}
	if !decode(w, r, &req) {
		return
	}
	name, ok := cleanText(req.Name, maxNameLen, false)
	if !ok {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("display name must be 1 to %d characters", maxNameLen))
		return
	}
	target, _ := strconv.ParseInt(r.PathValue("participant"), 10, 64)
	room := r.PathValue("room")
	ctx := r.Context()
	var by *store.Participant
	if sess != nil {
		by = sess.p
	}
	if by == nil || by.ID != target {
		if !s.asRoomAdmin(w, r, sess) {
			return
		}
		ctx = s.roomAuditing(r, sess, ActionRename, room, nil)
	}
	m, err := s.bus.Store.RenameParticipant(ctx, room, target, name, by, req.Profile)
	switch {
	case errors.Is(err, store.ErrForbidden):
		httpError(w, http.StatusForbidden, "only a room admin can do this")
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, "no such participant in this room")
	case err != nil:
		s.serverError(w, "rename_participant_failed", err)
	default:
		if m != nil {
			s.bus.Notify(room)
			s.log.Info("participant_renamed", zap.String("room", room), zap.Int64("participant", target), zap.Bool("self", by != nil && by.ID == target))
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
