package web

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/skill"
	"github.com/babs/grill-room/internal/store"
)

// profileFor answers the request itself and returns nil unless the profile cookie names a profile;
// a write must also come from this site.
func (s *Server) profileFor(w http.ResponseWriter, r *http.Request) *store.Profile {
	if crossSite(r) {
		httpError(w, http.StatusForbidden, "cross-site request refused")
		return nil
	}
	p, err := s.profileFromCookie(r)
	if err != nil {
		s.serverError(w, "profile_lookup_failed", err)
		return nil
	}
	if p == nil {
		httpError(w, http.StatusUnauthorized, "no profile on this browser")
	}
	return p
}

func skillID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("skill"), 10, 64)
	return id
}

// mySkills lists what the profile uploaded: its personal skills and the shared ones it published.
func (s *Server) mySkills(w http.ResponseWriter, r *http.Request) {
	p := s.profileFor(w, r)
	if p == nil {
		return
	}
	ks, err := s.bus.Store.Skills(r.Context(), p.ID, "", store.SkillsMine)
	if err != nil {
		s.serverError(w, "list_skills_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, ks)
}

// uploadSkill takes a skill folder as a zip body: a new skill, or the next version of the one of
// that name the profile owns (personal) or published (shared, with ?scope=shared). Versions never
// change once stored (002-AC29, 002-AC30).
func (s *Server) uploadSkill(w http.ResponseWriter, r *http.Request) {
	p := s.profileFor(w, r)
	if p == nil {
		return
	}
	// A cross-site form cannot send this type: no upload rides a visitor's cookie.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/zip" {
		httpError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/zip")
		return
	}
	// The body, its files and their tar sit in memory: skill uploads share the documents' slots.
	select {
	case s.uploads <- struct{}{}:
		defer func() { <-s.uploads }()
	default:
		httpError(w, http.StatusServiceUnavailable, "too many uploads in progress, retry in a moment")
		return
	}
	if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadReadTimeout)); err != nil {
		s.serverError(w, "upload_deadline_failed", err)
		return
	}
	shared := r.URL.Query().Get("scope") == "shared"
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, skill.MaxZipBytes))
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &mbe):
		httpError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the zip is over %d MB", skill.MaxZipBytes>>20))
		return
	case err != nil:
		httpError(w, http.StatusBadRequest, "could not read the upload")
		return
	}
	sk, err := skill.Parse(body)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	k, err := s.bus.Store.PublishSkill(r.Context(), p.ID, shared, sk)
	switch {
	case errors.Is(err, store.ErrForbidden):
		httpError(w, http.StatusConflict, "another profile published a shared skill named "+sk.Name)
	case errors.Is(err, store.ErrSameVersion), errors.Is(err, store.ErrLimit):
		httpError(w, http.StatusConflict, err.Error())
	case err != nil:
		s.serverError(w, "upload_skill_failed", err)
	default:
		s.log.Info("skill_published", zap.Int64("skill", k.ID), zap.String("name", k.Name), zap.Int("version", k.Version),
			zap.Bool("shared", k.Shared), zap.Int64("profile", p.ID))
		writeJSON(w, http.StatusCreated, k)
	}
}

// removeMySkill deletes one of the profile's personal skills; shared ones are the admin's to remove.
func (s *Server) removeMySkill(w http.ResponseWriter, r *http.Request) {
	p := s.profileFor(w, r)
	if p == nil {
		return
	}
	s.removeSkill(w, r, p.ID)
}

func (s *Server) adminSkills(w http.ResponseWriter, r *http.Request) {
	ks, err := s.bus.Store.Skills(r.Context(), 0, "", store.SkillsAll)
	if err != nil {
		s.serverError(w, "list_skills_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, ks)
}

// adminRemoveSkill removes any skill of the library with its versions (002-AC29).
func (s *Server) adminRemoveSkill(w http.ResponseWriter, r *http.Request) { s.removeSkill(w, r, 0) }

func (s *Server) removeSkill(w http.ResponseWriter, r *http.Request, profileID int64) {
	id := skillID(r)
	ctx := r.Context()
	if profileID == 0 {
		ctx = s.auditing(r, ActionSkillRemove, store.TargetSkill, strconv.FormatInt(id, 10), "", nil)
	}
	err := s.bus.Store.RemoveSkill(ctx, id, profileID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, "no such skill of yours")
	case err != nil:
		s.serverError(w, "remove_skill_failed", err)
	default:
		s.log.Info("skill_removed", zap.Int64("skill", id), zap.Int64("profile", profileID))
		w.WriteHeader(http.StatusNoContent)
	}
}

// roomSkills lists what the member may activate in the room: the shared library and their own
// personal skills (002-AC30), and whether their profile still has to accept the disclaimer (002-AC32).
func (s *Server) roomSkills(w http.ResponseWriter, r *http.Request, sess session) {
	resp := struct {
		Skills []store.Skill `json:"skills"`
		// Disclaimer is true until the member's profile accepts it; NoProfile, when it has none.
		Disclaimer bool `json:"disclaimer"`
		NoProfile  bool `json:"no_profile,omitempty"`
	}{NoProfile: sess.p.ProfileID == 0}
	var err error
	if resp.Skills, err = s.bus.Store.Skills(r.Context(), sess.p.ProfileID, sess.p.RoomID, store.SkillsLibrary); err != nil {
		s.serverError(w, "list_skills_failed", err)
		return
	}
	if !resp.NoProfile {
		accepted, err := s.bus.Store.SkillsDisclaimerAccepted(r.Context(), sess.p.ProfileID)
		if err != nil {
			s.serverError(w, "list_skills_failed", err)
			return
		}
		resp.Disclaimer = !accepted
	}
	writeJSON(w, http.StatusOK, resp)
}

// setPersonaSkill turns a skill on for a persona at its latest version, which also takes an
// offered update, or with DELETE turns it off. Any member may; it is a persona change (002-AC31).
func (s *Server) setPersonaSkill(w http.ResponseWriter, r *http.Request, sess session) {
	on := r.Method == http.MethodPut
	var req struct {
		AcceptDisclaimer bool `json:"accept_disclaimer"`
	}
	if on && !decode(w, r, &req) {
		return
	}
	p, m, held, err := s.bus.Store.SetPersonaSkill(r.Context(), sess.p.RoomID, personaID(r), skillID(r), on, req.AcceptDisclaimer, sess.p)
	switch {
	case errors.Is(err, store.ErrForbidden):
		httpError(w, http.StatusForbidden, "activating a skill needs a profile")
	case errors.Is(err, store.ErrDisclaimer):
		httpError(w, http.StatusPreconditionRequired, err.Error())
	case errors.Is(err, store.ErrSkillName):
		httpError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, "no such persona, or no such skill offered to you")
	default:
		s.personaDone(w, sess, p, m, err, heldStatus(held))
	}
}

// getSkills streams the turn's skills as the plugin directory claude -p loads with --plugin-dir:
// the versions pinned when the turn started (002-AC31, docs/agent-workspace.md).
func (s *Server) getSkills(w http.ResponseWriter, r *http.Request, d *store.Daemon, _ string) {
	t, ok := s.daemonTurn(w, r, d)
	if !ok {
		return
	}
	ks, err := s.bus.Store.TurnSkills(r.Context(), d.RoomID, t.ID, true)
	if err != nil {
		s.serverError(w, "turn_skills_failed", err)
		return
	}
	packed := make([]skill.Packed, len(ks))
	for i, k := range ks {
		packed[i] = skill.Packed{Name: k.Name, Tar: k.Data}
	}
	w.Header().Set("Content-Type", "application/x-tar")
	if err := skill.WritePlugin(w, packed); err != nil {
		// Headers are gone: the daemon sees a truncated tar and fails the turn.
		s.log.Error("turn_skills_failed", zap.String("room", d.RoomID), zap.Int64("turn", t.ID), zap.Error(err))
	}
}

// copySkill copies one of the member's personal skills into the room, frozen at its latest
// version: the room's from then on, whoever leaves. Held for the host like a persona change
// (004-AC10).
func (s *Server) copySkill(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		SkillID int64 `json:"skill_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	k, m, err := s.bus.Store.CopySkillToRoom(r.Context(), sess.p.RoomID, req.SkillID, sess.p)
	switch {
	case errors.Is(err, store.ErrForbidden):
		httpError(w, http.StatusForbidden, "copying a skill needs a profile")
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, "no such personal skill of yours")
	case errors.Is(err, store.ErrRoomSkillName), errors.Is(err, store.ErrLimit):
		httpError(w, http.StatusConflict, err.Error())
	case err != nil:
		s.serverError(w, "copy_skill_failed", err)
	default:
		s.bus.Notify(m.RoomID)
		s.log.Info("skill_copied_to_room", zap.String("room", sess.p.RoomID), zap.Int64("skill", k.ID), zap.Int64("from", req.SkillID), zap.Bool("pending", k.Pending))
		status := http.StatusCreated
		if k.Pending {
			status = http.StatusAccepted
		}
		writeJSON(w, status, k)
	}
}

// decideRoomSkill is the host's answer to a room skill copied in while they approve everything.
func (s *Server) decideRoomSkill(w http.ResponseWriter, r *http.Request, sess session) {
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
	m, err := s.bus.Store.DecideRoomSkill(r.Context(), room.ID, skillID(r), req.Approve, sess.p)
	s.decided(w, sess, m, err)
}

// removeRoomSkill removes one of the room's own skills: a room admin's to do (004-AC8, 004-AC10).
func (s *Server) removeRoomSkill(w http.ResponseWriter, r *http.Request, sess session) {
	if !s.asRoomAdmin(w, r, &sess) {
		return
	}
	id := skillID(r)
	m, err := s.bus.Store.RemoveRoomSkill(s.roomAuditing(r, &sess, ActionRoomSkillRemove, sess.p.RoomID, nil), sess.p.RoomID, id, sess.p)
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, "no such room skill")
	case err != nil:
		s.serverError(w, "remove_room_skill_failed", err)
	default:
		s.bus.Notify(m.RoomID)
		s.log.Info("room_skill_removed", zap.String("room", sess.p.RoomID), zap.Int64("skill", id))
		w.WriteHeader(http.StatusNoContent)
	}
}
