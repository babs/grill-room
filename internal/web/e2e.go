//go:build e2e

package web

import (
	"net/http"
	"time"

	"go.uber.org/zap"
)

// In the e2e build only, POST /api/admin/test/purge {"after_s": n} runs one purge pass with an
// inactivity threshold of n seconds, so a browser test sees a purged profile without waiting 24 h.
// "profile_ids", when set, limits it to those profiles: parallel specs keep their own.
func init() {
	e2eRoutes = func(s *Server, mux *http.ServeMux) {
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
