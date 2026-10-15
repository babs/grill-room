//go:build e2e

package web

import (
	"net/http"
	"time"

	"go.uber.org/zap"
)

// In the e2e build only, GET /api/admin/test/device-stt answers the server's device transcription
// offer, or null: the e2e server has no voice, so no "stt" event carries it and specs replay it.
//
// POST /api/admin/test/purge {"after_s": n} runs one purge pass with an
// inactivity threshold of n seconds, so a browser test sees a purged profile without waiting 24 h.
// "profile_ids", when set, limits it to those profiles: parallel specs keep their own.
func init() {
	e2eRoutes = func(s *Server, mux *http.ServeMux) {
		mux.Handle("GET /api/admin/test/device-stt", s.admin(func(w http.ResponseWriter, r *http.Request) {
			if s.device == nil {
				writeJSON(w, http.StatusOK, nil)
				return
			}
			writeJSON(w, http.StatusOK, s.device.offer)
		}))
		mux.Handle("POST /api/admin/test/purge", s.admin(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				AfterS     int64   `json:"after_s"`
				ProfileIDs []int64 `json:"profile_ids"`
			}
			if !decode(w, r, &req) {
				return
			}
			deleted, err := s.purgeProfiles(r.Context(), time.Duration(req.AfterS)*time.Second, req.ProfileIDs...)
			if err != nil {
				s.serverError(w, "test_purge_failed", err)
				return
			}
			s.log.Warn("test_purge", zap.Int("profiles", len(deleted)))
			writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
		}))
	}
}
