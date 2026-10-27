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
	p, err := s.bus.Store.MergePreview(r.Context(), keep, merge, s.orgIssuer())
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
	kept, moved, err := s.mergeProfilesOf(r.Context(), ctx, req.Keep, req.Merge)
	if s.mergeError(w, "merge_profiles_failed", err) {
		return
	}
	s.log.Info("profiles_merged", zap.Int64("keep", req.Keep), zap.Int64("merge", req.Merge), zap.Any("moved", moved))
	writeJSON(w, http.StatusOK, mergeResp{Keep: mergeKept{ID: kept.ID, UUID: kept.UUID, Name: kept.Name}, Moved: moved})
}

// mergeProfilesOf merges as MergeProfiles does, under the audit entry actx carries. When the
// result is bound, the other side's credentials end: its daemons fail their running turn first,
// then its rooms are told (026-AC4).
func (s *Server) mergeProfilesOf(ctx, actx context.Context, keep, merge int64) (*store.MergeProfile, store.MergeMoved, error) {
	// A refused merge must not unhost anything.
	if keep == merge {
		return nil, store.MergeMoved{}, store.ErrSelfMerge
	}
	daemons, err := s.bus.Store.MergeDaemons(ctx, keep, merge, s.orgIssuer())
	if err == nil {
		err = s.unhostAll(ctx, daemons, "the AI host's profile was merged into an organisation account")
	}
	var kept *store.MergeProfile
	var moved store.MergeMoved
	var ended store.EndedCredentials
	if err == nil {
		kept, moved, ended, err = s.bus.Store.MergeProfiles(actx, keep, merge, s.orgIssuer())
	}
	if err != nil {
		return nil, store.MergeMoved{}, err
	}
	s.credentialsEnded(ctx, daemons, ended)
	return kept, moved, nil
}

// unhostAll fails the running turn of each daemon about to be removed, as the host removing the
// room does (021-AC10, 026-AC4, 026 Decisions).
// ponytail: one paired between the listing and the removal loses its row without a failed turn;
// the turn sweep ends it, as for an unbind.
func (s *Server) unhostAll(ctx context.Context, daemons []store.Daemon, why string) error {
	for i := range daemons {
		if err := s.unhostDaemon(ctx, &daemons[i], why); err != nil {
			return err
		}
	}
	return nil
}

// credentialsEnded tells the rooms of what an unbind or endCredentials ended: the removed daemons'
// streams find their token gone and their rooms elect a host again; the seats leave the audio, off
// the request as LiveKit may be slow, their open pages told at once (021-AC10, 026-AC4, 026 Decisions).
func (s *Server) credentialsEnded(ctx context.Context, daemons []store.Daemon, ended store.EndedCredentials) {
	rooms := ended.DaemonRooms
	for _, d := range daemons {
		rooms = append(rooms, d.RoomID)
	}
	woken := map[string]bool{}
	for _, room := range rooms {
		if !woken[room] {
			woken[room] = true
			s.agentChanged(ctx, room)
		}
	}
	byRoom := map[string][]int64{}
	for _, p := range ended.Seats {
		byRoom[p.RoomID] = append(byRoom[p.RoomID], p.ID)
	}
	for room, ids := range byRoom {
		s.bus.Notify(room)
		go s.kickFromVoice(room, ids) //nolint:gosec // outlives the request on purpose: the answer does not wait for LiveKit
	}
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
	return s.bus.Store.PurgeEmptyProfiles(ctx, now, idle, only...)
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
