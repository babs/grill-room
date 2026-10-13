package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
)

// Audit actions of the starter library (006-AC5).
const (
	ActionStarterPublish = "starter.publish"
	ActionStarterDelete  = "starter.delete"
)

func starterID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("starter"), 10, 64)
	return id
}

// starterError answers a starter library refusal with its status; false when err is nil.
func (s *Server) starterError(w http.ResponseWriter, event string, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, "no such starter")
	case errors.Is(err, store.ErrBuiltin):
		httpError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, store.ErrForbidden):
		httpError(w, http.StatusForbidden, "only the starter's author can do this")
	case errors.Is(err, store.ErrLimit):
		httpError(w, http.StatusConflict, err.Error())
	default:
		s.serverError(w, event, err)
	}
	return true
}

// profileAudit returns r's context carrying the entry of a profile's action on starter id.
func (s *Server) profileAudit(r *http.Request, p *store.Profile, action string, id int64) context.Context {
	e := store.AuditEntry{At: s.now(), Actor: ActorProfile + p.UUID, Action: action, TargetKind: store.TargetStarter, TargetID: strconv.FormatInt(id, 10)}
	return store.WithAudit(r.Context(), e, nil)
}

// myStarters lists the starters a profile sees: the built-ins, its own and the instance's (006-AC1).
func (s *Server) myStarters(w http.ResponseWriter, r *http.Request) {
	p := s.profileFor(w, r)
	if p == nil {
		return
	}
	ls, err := s.bus.Store.Starters(r.Context(), p.ID)
	if s.starterError(w, "list_starters_failed", err) {
		return
	}
	writeJSON(w, http.StatusOK, ls)
}

// getStarter answers a starter with the manifest of its version `v`, the latest by default, for
// the picker's preview and the editor (006-AC8).
func (s *Server) getStarter(w http.ResponseWriter, r *http.Request) {
	p := s.profileFor(w, r)
	if p == nil {
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("v"))
	st, m, err := s.bus.Store.StarterVersion(r.Context(), starterID(r), n, p.ID)
	if s.starterError(w, "get_starter_failed", err) {
		return
	}
	writeJSON(w, http.StatusOK, struct {
		*store.Starter
		Manifest *store.StarterManifest `json:"manifest"`
	}{st, m})
}

// duplicateStarter copies a starter the profile sees into its personal library (006-AC2).
func (s *Server) duplicateStarter(w http.ResponseWriter, r *http.Request) {
	p := s.profileFor(w, r)
	if p == nil {
		return
	}
	st, err := s.bus.Store.DuplicateStarter(r.Context(), p.ID, starterID(r))
	if s.starterError(w, "duplicate_starter_failed", err) {
		return
	}
	s.log.Info("starter_duplicated", zap.Int64("starter", st.ID), zap.Int64("from", starterID(r)), zap.Int64("profile", p.ID))
	writeJSON(w, http.StatusCreated, st)
}

// publishStarter moves a personal starter to the instance library, audited (006-AC3, 006-AC5).
func (s *Server) publishStarter(w http.ResponseWriter, r *http.Request) {
	p := s.profileFor(w, r)
	if p == nil {
		return
	}
	id := starterID(r)
	if s.starterError(w, "publish_starter_failed", s.bus.Store.PublishStarter(s.profileAudit(r, p, ActionStarterPublish, id), p.ID, id)) {
		return
	}
	s.log.Info("starter_published", zap.Int64("starter", id), zap.Int64("profile", p.ID))
	w.WriteHeader(http.StatusNoContent)
}

// deleteMyStarter deletes a starter the profile owns, personal or published (006-AC3).
func (s *Server) deleteMyStarter(w http.ResponseWriter, r *http.Request) {
	p := s.profileFor(w, r)
	if p == nil {
		return
	}
	id := starterID(r)
	if s.starterError(w, "delete_starter_failed", s.bus.Store.DeleteStarter(s.profileAudit(r, p, ActionStarterDelete, id), p.ID, id)) {
		return
	}
	s.log.Info("starter_deleted", zap.Int64("starter", id), zap.Int64("profile", p.ID))
	w.WriteHeader(http.StatusNoContent)
}

// adminStarters lists the instance library for /admin Libraries (006-AC5).
func (s *Server) adminStarters(w http.ResponseWriter, r *http.Request) {
	ls, err := s.bus.Store.Starters(r.Context(), 0)
	if s.starterError(w, "list_starters_failed", err) {
		return
	}
	out := []store.Starter{}
	for _, st := range ls {
		if st.Library == store.LibraryInstance {
			out = append(out, st)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// adminDeleteStarter is the super-admin removing an instance starter, audited (006-AC5).
func (s *Server) adminDeleteStarter(w http.ResponseWriter, r *http.Request) {
	id := starterID(r)
	ctx := s.auditing(r, ActionStarterDelete, store.TargetStarter, strconv.FormatInt(id, 10), "", nil)
	if s.starterError(w, "delete_starter_failed", s.bus.Store.DeleteStarter(ctx, 0, id)) {
		return
	}
	s.log.Info("starter_deleted", zap.Int64("starter", id), zap.String("by", store.ActorSuperAdmin))
	w.WriteHeader(http.StatusNoContent)
}
