//go:build e2e

package web

import (
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
)

// In the e2e build only, GET /api/admin/test/device-stt answers the server's device transcription
// offer, or null: the e2e server has no voice, so no "stt" event carries it and specs replay it.
//
// POST /api/admin/test/purge {"after_s": n} runs one purge pass with an
// inactivity threshold of n seconds, so a browser test sees a purged profile without waiting 24 h.
// "profile_ids", when set, limits it to those profiles: parallel specs keep their own.
//
// POST /api/admin/test/speak {"room", "persona", "spoken"} queues what the room's persona of that
// name says, as if its turn had written spoken.md: the voice e2e needs no daemon (spec 013).
func init() {
	e2eRoutes = func(s *Server, mux *http.ServeMux) {
		mux.Handle("GET /api/admin/test/device-stt", s.admin(func(w http.ResponseWriter, r *http.Request) {
			if s.device == nil {
				writeJSON(w, http.StatusOK, nil)
				return
			}
			writeJSON(w, http.StatusOK, s.device.offer)
		}))
		mux.Handle("POST /api/admin/test/speak", s.admin(func(w http.ResponseWriter, r *http.Request) {
			var req struct{ Room, Persona, Spoken string }
			if !decode(w, r, &req) {
				return
			}
			ps, err := s.bus.Store.RoomPersonas(r.Context(), req.Room)
			if err != nil {
				s.serverError(w, "test_speak_failed", err)
				return
			}
			for _, p := range ps {
				if p.Name == req.Persona {
					s.queueSpeech(r.Context(), &store.Turn{RoomID: req.Room, PersonaID: p.ID}, req.Spoken, "")
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
			httpError(w, http.StatusNotFound, "no such persona")
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
