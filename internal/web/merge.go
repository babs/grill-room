package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
)

// Merge and purge actions (011-AC13, 011-AC16).
const (
	ActionProfileMerge = "profile.merge"
	ActionProfilePurge = "profile.purge"
)

// profilePurgeEvery is how often empty profiles are purged: 24 h is honoured within an hour. A var
// for tests.
var profilePurgeEvery = time.Hour

// mergeError answers a merge or preview that failed, and reports whether it did.
func (s *Server) mergeError(w http.ResponseWriter, event string, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrSelfMerge):
		httpError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, "no such profile")
	default:
		s.serverError(w, event, err)
	}
	return true
}

// mergePreview shows what merging ?merge= into ?keep= would do (011-AC10).
func (s *Server) mergePreview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	keep, err1 := strconv.ParseInt(q.Get("keep"), 10, 64)
	merge, err2 := strconv.ParseInt(q.Get("merge"), 10, 64)
	if err1 != nil || err2 != nil {
		httpError(w, http.StatusNotFound, "no such profile")
		return
	}
	p, err := s.bus.Store.MergePreview(r.Context(), keep, merge)
	if s.mergeError(w, "merge_preview_failed", err) {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

type mergeResp struct {
	Keep  mergeKept        `json:"keep"`
	Moved store.MergeMoved `json:"moved"`
}

type mergeKept struct {
	ID   int64  `json:"id"`
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

// mergeProfiles moves everything of profile merge to profile keep and deletes merge (011-AC11).
func (s *Server) mergeProfiles(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Keep  int64 `json:"keep"`
		Merge int64 `json:"merge"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx := s.auditing(r, ActionProfileMerge, store.TargetProfile, strconv.FormatInt(req.Keep, 10), "", nil)
	kept, moved, err := s.bus.Store.MergeProfiles(ctx, req.Keep, req.Merge)
	if s.mergeError(w, "merge_profiles_failed", err) {
		return
	}
	s.checkpoint(context.WithoutCancel(r.Context()))
	s.log.Info("profiles_merged", zap.Int64("keep", req.Keep), zap.Int64("merge", req.Merge), zap.Any("moved", moved))
	writeJSON(w, http.StatusOK, mergeResp{Keep: mergeKept{ID: kept.ID, UUID: kept.UUID, Name: kept.Name}, Moved: moved})
}

// purgeProfiles runs one purge pass of the profiles empty and idle for idle, unless the instance
// switch is off, and returns the deleted uuids (011-AC15 to 011-AC17). only restricts it to those ids.
func (s *Server) purgeProfiles(ctx context.Context, idle time.Duration, only ...int64) ([]string, error) {
	v, err := s.bus.Store.InstanceSettings(ctx)
	if err != nil || !v.PurgeEmptyProfiles {
		return []string{}, err
	}
	now := s.now()
	ctx = store.WithAudit(ctx, store.AuditEntry{At: now, Actor: store.ActorSystem, Action: ActionProfilePurge, TargetKind: store.TargetInstance}, nil)
	uuids, err := s.bus.Store.PurgeEmptyProfiles(ctx, now, idle, only...)
	if err == nil && len(uuids) > 0 {
		s.checkpoint(ctx)
	}
	return uuids, err
}

// checkpoint clears the WAL after a committed delete, merge or purge; a failure leaves the change
// done, so it is only logged.
func (s *Server) checkpoint(ctx context.Context) {
	if err := s.bus.Store.Checkpoint(ctx); err != nil && ctx.Err() == nil {
		s.log.Error("checkpoint_failed", zap.Error(err))
	}
}

// PurgeProfiles purges empty profiles now, then every profilePurgeEvery, until ctx ends (011-AC16).
func (s *Server) PurgeProfiles(ctx context.Context) {
	for {
		uuids, err := s.purgeProfiles(ctx, store.EmptyProfileIdle)
		switch {
		case err != nil && ctx.Err() == nil:
			s.log.Error("profile_purge_failed", zap.Error(err))
		case len(uuids) > 0:
			s.log.Info("profiles_purged", zap.Int("profiles", len(uuids)))
		}
		t := time.NewTimer(profilePurgeEvery)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}
