package web

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
)

// maxQuota bounds a room quota the super-admin sets: 1 TiB.
const maxQuota = 1 << 40

// roomOverview is a room as the rooms view shows it (004-AC2).
type roomOverview struct {
	store.RoomOverview
	// Used is what counts against Quota.
	Used       int64 `json:"used"`
	HostOnline bool  `json:"host_online"`
}

type roomsResp struct {
	Rooms        []roomOverview `json:"rooms"`
	DefaultQuota int64          `json:"default_quota"`
}

// adminRooms lists every room with its counts, volume and quota, and whether its host is online.
func (s *Server) adminRooms(w http.ResponseWriter, r *http.Request) {
	list, err := s.bus.Store.RoomOverviews(r.Context(), s.opts.MaxRoomBytes)
	if err != nil {
		s.serverError(w, "room_overviews_failed", err)
		return
	}
	online := s.onlineHosts()
	out := roomsResp{Rooms: make([]roomOverview, len(list)), DefaultQuota: s.opts.MaxRoomBytes}
	for i, o := range list {
		v := roomOverview{RoomOverview: o, Used: o.Volume.Counted()}
		v.HostOnline = o.HostParticipantID != 0 && online[o.ID][o.HostParticipantID]
		out.Rooms[i] = v
	}
	writeJSON(w, http.StatusOK, out)
}

// onlineHosts maps each room to the participants whose daemon has an open stream.
func (s *Server) onlineHosts() map[string]map[int64]bool {
	s.presence.mu.Lock()
	defer s.presence.mu.Unlock()
	out := map[string]map[int64]bool{}
	for _, p := range s.presence.m {
		if p.conns > 0 {
			if out[p.room] == nil {
				out[p.room] = map[int64]bool{}
			}
			out[p.room][p.ParticipantID] = true
		}
	}
	return out
}

// setQuota sets a room's quota, or with 0 returns it to the instance default. Only the super-admin
// may: a member, room admins included, gets a 403 (004-AC9).
func (s *Server) setQuota(w http.ResponseWriter, r *http.Request, sess *session) {
	if sess != nil {
		httpError(w, http.StatusForbidden, "only the super-admin changes a room's quota")
		return
	}
	var req struct {
		QuotaBytes *int64 `json:"quota_bytes"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.QuotaBytes == nil || *req.QuotaBytes < 0 || *req.QuotaBytes > maxQuota {
		httpError(w, http.StatusBadRequest, "quota_bytes must be 0 (the instance default) to 1 TiB")
		return
	}
	room := r.PathValue("room")
	err := s.bus.Store.SetRoomQuota(r.Context(), room, *req.QuotaBytes)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such room")
		return
	}
	if err != nil {
		s.serverError(w, "set_quota_failed", err)
		return
	}
	s.audit(r, ActionRoomQuota, store.TargetRoom, room, s.targetName(r, store.TargetRoom, room), map[string]any{"quota_bytes": *req.QuotaBytes})
	s.log.Info("room_quota_set", zap.String("room", room), zap.Int64("quota_bytes", *req.QuotaBytes))
	w.WriteHeader(http.StatusNoContent)
}

type instanceResp struct {
	Version string `json:"version"`
	// DBBytes is the database with its write-ahead log; DiskFree and DiskTotal are of the
	// filesystem holding it, -1 when the platform cannot tell.
	DBBytes   int64 `json:"db_bytes"`
	DiskFree  int64 `json:"disk_free"`
	DiskTotal int64 `json:"disk_total"`
	STT       struct {
		// Voice is LiveKit audio being on; Configured, an STT service token being set.
		Voice      bool `json:"voice"`
		Configured bool `json:"configured"`
		Workers    int  `json:"workers"`
	} `json:"stt"`
	Daemons struct {
		Connected int `json:"connected"`
		// Versions counts the connected daemons by the version they report.
		Versions map[string]int `json:"versions"`
	} `json:"daemons"`
	SMTP bool `json:"smtp"`
}

// adminInstance reports the instance's health (004-AC5).
func (s *Server) adminInstance(w http.ResponseWriter, _ *http.Request) {
	var out instanceResp
	out.Version, out.DiskFree, out.DiskTotal = s.opts.Version, -1, -1
	if p := s.opts.DBPath; p != "" {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if fi, err := os.Stat(p + suffix); err == nil {
				out.DBBytes += fi.Size()
			}
		}
		var err error
		if out.DiskFree, out.DiskTotal, err = diskSpace(filepath.Dir(p)); err != nil {
			s.log.Warn("disk_space_failed", zap.Error(err))
			out.DiskFree, out.DiskTotal = -1, -1
		}
	}
	if v := s.opts.Voice; v != nil {
		out.STT.Voice, out.STT.Configured = true, v.STTTokenHash != ""
	}
	out.STT.Workers = s.STTWorkers()
	out.Daemons.Versions = map[string]int{}
	s.presence.mu.Lock()
	for _, p := range s.presence.m {
		if p.conns > 0 {
			out.Daemons.Connected++
			out.Daemons.Versions[p.Version]++
		}
	}
	s.presence.mu.Unlock()
	out.SMTP = s.opts.SendMail != nil
	writeJSON(w, http.StatusOK, out)
}
