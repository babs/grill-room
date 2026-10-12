// Package web exposes rooms over HTTP: JSON API, SSE stream, embedded UI.
//
// Auth, SSE replay and Serve are derived from github.com/babs/airc pkg/web at commit b7afc63.
package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/bus"
	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
	"github.com/babs/grill-room/internal/uivendor"
)

//go:embed ui
var ui embed.FS

const (
	cookieName   = "grill_room_session"
	cookieMaxAge = 30 * 24 * 3600
	maxNameLen   = 40
	maxRoomName  = 100
	maxBodyLen   = 4000
	// maxConcurrentUploads bounds the memory uploads hold: about three copies of up to MaxUploadBytes each.
	maxConcurrentUploads = 4
	// maxConcurrentArchives bounds the rooms staged on disk at once by exports and imports.
	maxConcurrentArchives = 2
	// replayBatch bounds one store read while an SSE stream catches up.
	replayBatch = 500
)

// heartbeat keeps proxies from timing out an idle stream and re-checks access. A var for tests.
var heartbeat = 20 * time.Second

// Server serves the room API and UI.
type Server struct {
	bus     *bus.Bus
	opts    Options
	uploads chan struct{}
	// archives bounds the exports and imports running at once.
	archives  chan struct{}
	adminHash []byte
	baseURL   string
	log       *zap.Logger
	streams   atomic.Int64
	pairings  pairings
	presence  presences
	people    people
	stt       sttWorkers
	clients   clients
	// magicLimit rate-limits magic link requests per address and per client IP.
	magicLimit limiter
	// loginLimit rate-limits /admin logins per client IP.
	loginLimit limiter
	// now is the clock admin sessions and the audit log read; tests set it.
	now func() time.Time
	// electMu serialises host elections and hand-overs.
	electMu sync.Mutex
	// turnMu serialises turn starts and queue draining, so presses run in the order they came.
	// ponytail: one lock for every room; per-room locks if many busy rooms ever contend.
	turnMu sync.Mutex
	// fallbacks counts full workspace fetches by daemons with a cache, by turn.FallbackReasons.
	fallbacks map[string]*atomic.Int64
	// conflicts counts results rejected on a document conflict, by ConflictOutcomes (003-AC21).
	conflicts map[string]*atomic.Int64
}

// New builds a server. adminHash is HashToken of the admin token; baseURL prefixes invite links.
func New(b *bus.Bus, adminHash, baseURL string, opts Options, log *zap.Logger) *Server {
	if opts.TurnTimeout <= 0 {
		opts.TurnTimeout = defaultTurnTimeout
	}
	if opts.MaxResultBytes <= 0 {
		opts.MaxResultBytes = defaultMaxResult
	}
	if opts.MagicLinkTTL <= 0 {
		opts.MagicLinkTTL = DefaultMagicLinkTTL
	}
	if opts.AuditRetention <= 0 {
		opts.AuditRetention = DefaultAuditRetention
	}
	if opts.ClientRetention <= 0 {
		opts.ClientRetention = DefaultClientRetention
	}
	if opts.MaxImportBytes <= 0 {
		opts.MaxImportBytes = DefaultMaxImportBytes
	}
	s := &Server{bus: b, opts: opts, uploads: make(chan struct{}, maxConcurrentUploads), archives: make(chan struct{}, maxConcurrentArchives),
		adminHash: []byte(adminHash), baseURL: strings.TrimRight(baseURL, "/"), log: log,
		pairings: pairings{m: map[string]*pairing{}}, presence: presences{m: map[int64]*presence{}}, people: people{m: map[string]map[int64]*person{}}, stt: sttWorkers{m: map[string]*sttWorker{}}, clients: clients{m: map[int64]clientSeen{}},
		magicLimit: limiter{window: magicWindow}, loginLimit: limiter{window: loginWindow}, now: time.Now, fallbacks: fallbackCounters(),
		conflicts: map[string]*atomic.Int64{"rerun": {}, "failed": {}}}
	s.cleanStaging()
	return s
}

func fallbackCounters() map[string]*atomic.Int64 {
	m := make(map[string]*atomic.Int64, len(turn.FallbackReasons))
	for _, r := range turn.FallbackReasons {
		m[r] = new(atomic.Int64)
	}
	return m
}

// WorkspaceFallbacks returns how many full workspaces daemons fetched for reason (002-AC38).
func (s *Server) WorkspaceFallbacks(reason string) int64 { return s.fallbacks[reason].Load() }

// ConflictOutcomes are what a document conflict does to a turn: it runs again, or fails.
var ConflictOutcomes = []string{"rerun", "failed"}

// TurnConflicts counts the turn results rejected on a document conflict with outcome.
func (s *Server) TurnConflicts(outcome string) int64 { return s.conflicts[outcome].Load() }

// Streams returns the number of open event streams.
func (s *Server) Streams() int64 { return s.streams.Load() }

// NewToken returns a random 128-bit token.
func NewToken() string { return rand.Text() }

// HashToken is how tokens are stored: a leaked database grants no access.
func HashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// Handler returns the routed handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	page := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { http.ServeFileFS(w, r, ui, "ui/"+name) }
	}
	mux.HandleFunc("GET /{$}", page("join.html"))
	mux.HandleFunc("GET /join", page("join.html"))
	mux.HandleFunc("GET /favicon.ico", asset(ui, "ui/favicon.ico", "image/x-icon"))
	mux.HandleFunc("GET /site.webmanifest", asset(ui, "ui/site.webmanifest", "application/manifest+json"))
	mux.HandleFunc("GET /r/{room}", s.roomPage)
	mux.HandleFunc("GET /profile", page("profile.html"))
	mux.HandleFunc("GET /admin", s.adminPage)
	static, _ := fs.Sub(ui, "ui") // cannot fail: "ui" is a valid embedded path
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.Handle("GET "+uivendor.Prefix, uivendor.Handler())
	mux.HandleFunc("GET /install.sh", s.installScript)
	mux.HandleFunc("GET /download/{file}", s.download)
	mux.HandleFunc("GET /api/daemon/version", s.daemonVersion)

	mux.Handle("GET /api/rooms", s.admin(s.listRooms))
	mux.Handle("POST /api/rooms", s.admin(s.createRoom))
	mux.Handle("DELETE /api/rooms/{room}", s.memberOrAdmin(s.deleteRoom))
	mux.HandleFunc("POST /api/rooms/import", s.importRoom)
	mux.Handle("GET /api/rooms/{room}/export", s.memberOrAdmin(s.exportRoom))
	mux.Handle("PATCH /api/rooms/{room}", s.memberOrAdmin(s.renameRoom))
	mux.Handle("GET /api/rooms/{room}/invites", s.memberOrAdmin(s.listInvites))
	mux.Handle("POST /api/rooms/{room}/invites", s.memberOrAdmin(s.createInvite))
	mux.Handle("DELETE /api/rooms/{room}/invites/{invite}", s.memberOrAdmin(s.revokeInvite))
	mux.Handle("GET /api/profiles", s.admin(s.listProfiles))
	mux.Handle("DELETE /api/profiles/{id}", s.admin(s.deleteProfile))
	mux.Handle("PUT /api/profiles/{id}/max_rooms", s.admin(s.setProfileMaxRooms))
	mux.HandleFunc("POST /api/admin/login", s.adminLogin)
	mux.Handle("POST /api/admin/logout", s.admin(s.adminLogout))
	mux.Handle("GET /api/admin/sessions", s.admin(s.adminSessions))
	mux.Handle("DELETE /api/admin/sessions/{id}", s.admin(s.revokeAdminSession))
	mux.Handle("GET /api/admin/audit", s.admin(s.auditLog))
	// The /admin page's views: its session cookie only reaches /api/admin (004-AC2 to 004-AC5).
	adminOnly := func(h func(http.ResponseWriter, *http.Request, *session)) http.Handler {
		return s.admin(func(w http.ResponseWriter, r *http.Request) { h(w, r, nil) })
	}
	mux.Handle("GET /api/admin/rooms", s.admin(s.adminRooms))
	mux.Handle("PATCH /api/admin/rooms/{room}", adminOnly(s.renameRoom))
	mux.Handle("DELETE /api/admin/rooms/{room}", adminOnly(s.deleteRoom))
	mux.Handle("PUT /api/admin/rooms/{room}/quota", adminOnly(s.setQuota))
	mux.Handle("GET /api/admin/rooms/{room}/participants", s.admin(s.roomParticipants))
	mux.Handle("GET /api/admin/rooms/{room}/export", adminOnly(s.exportRoom))
	mux.Handle("POST /api/admin/rooms/import", s.admin(s.importRoom))
	mux.Handle("PUT /api/admin/rooms/{room}/admins/{participant}", adminOnly(s.setRoomAdmin))
	mux.Handle("DELETE /api/admin/rooms/{room}/admins/{participant}", adminOnly(s.setRoomAdmin))
	mux.Handle("PUT /api/rooms/{room}/quota", s.memberOrAdmin(s.setQuota))
	mux.Handle("GET /api/admin/rooms/{room}/invites", adminOnly(s.listInvites))
	mux.Handle("POST /api/admin/rooms/{room}/invites", adminOnly(s.createInvite))
	mux.Handle("DELETE /api/admin/rooms/{room}/invites/{invite}", adminOnly(s.revokeInvite))
	mux.Handle("GET /api/admin/profiles", s.admin(s.listProfiles))
	mux.Handle("DELETE /api/admin/profiles/{id}", s.admin(s.deleteProfile))
	mux.Handle("PUT /api/admin/profiles/{id}/max_rooms", s.admin(s.setProfileMaxRooms))
	mux.Handle("GET /api/admin/templates", s.admin(s.adminTemplates))
	mux.Handle("DELETE /api/admin/templates/{template}", s.admin(s.removeTemplate))
	mux.Handle("GET /api/admin/skills", s.admin(s.adminSkills))
	mux.Handle("DELETE /api/admin/skills/{skill}", s.admin(s.adminRemoveSkill))
	mux.Handle("GET /api/admin/instance", s.admin(s.adminInstance))
	mux.Handle("GET /api/admin/settings", s.admin(s.instanceSettings))
	mux.Handle("PUT /api/admin/settings", s.admin(s.setInstanceSettings))

	mux.HandleFunc("GET /api/profile", s.getProfile)
	mux.HandleFunc("POST /api/profile/recover", s.recoverProfile)
	mux.HandleFunc("POST /api/profile/email", s.bindEmail)
	mux.HandleFunc("POST /api/profile/signin", s.emailSignIn)
	mux.HandleFunc("POST /api/profile/magic", s.useMagicLink)
	mux.HandleFunc("GET /api/profile/skills", s.mySkills)
	mux.HandleFunc("POST /api/profile/skills", s.uploadSkill)
	mux.HandleFunc("DELETE /api/profile/skills/{skill}", s.removeMySkill)
	mux.HandleFunc("POST /api/profile/rooms", s.createProfileRoom)
	mux.Handle("GET /api/skills", s.admin(s.adminSkills))
	mux.Handle("DELETE /api/skills/{skill}", s.admin(s.adminRemoveSkill))

	mux.HandleFunc("POST /api/join", s.join)
	mux.Handle("GET /api/rooms/{room}", s.participant(s.getRoom))
	mux.Handle("GET /api/rooms/{room}/overview", s.participant(s.roomOverview))
	mux.Handle("PUT /api/rooms/{room}/admins/{participant}", s.memberOrAdmin(s.setRoomAdmin))
	mux.Handle("DELETE /api/rooms/{room}/admins/{participant}", s.memberOrAdmin(s.setRoomAdmin))
	mux.Handle("POST /api/rooms/{room}/messages", s.participant(s.postMessage))
	mux.Handle("PATCH /api/rooms/{room}/messages/{msg}", s.participant(s.editMessage))
	mux.Handle("DELETE /api/rooms/{room}/messages/{msg}", s.participant(s.deleteMessage))
	mux.Handle("GET /api/rooms/{room}/messages/{msg}/versions", s.participant(s.messageVersions))
	mux.Handle("GET /api/rooms/{room}/events", s.participant(s.events))
	mux.Handle("POST /api/rooms/{room}/presence", s.participant(s.postPresence))
	mux.Handle("GET /api/rooms/{room}/documents", s.participant(s.listDocuments))
	mux.Handle("POST /api/rooms/{room}/documents", s.participant(s.uploadDocument))
	mux.Handle("GET /api/rooms/{room}/documents/{doc}", s.participant(s.getDocument))
	mux.Handle("GET /api/rooms/{room}/documents/{doc}/view", s.participant(s.viewDocument))
	mux.Handle("GET /api/rooms/{room}/documents/{doc}/original", s.participant(s.originalDocument))
	mux.Handle("GET /api/rooms/{room}/documents/{doc}/annotations", s.participant(s.listAnnotations))
	mux.Handle("POST /api/rooms/{room}/documents/{doc}/annotations", s.participant(s.createAnnotation))
	mux.Handle("POST /api/rooms/{room}/annotations/{ann}/messages", s.participant(s.postAnnotationMessage))
	mux.Handle("PATCH /api/rooms/{room}/annotations/{ann}", s.participant(s.setAnnotationStatus))
	mux.Handle("GET /api/rooms/{room}/attachments/{att}", s.participant(s.getAttachment))

	mux.HandleFunc("POST /api/rooms/{room}/pairings", s.createPairing)
	mux.HandleFunc("GET /api/pairings/{id}", s.pollPairing)
	mux.Handle("GET /api/rooms/{room}/pairings/{id}", s.participant(s.getPairing))
	mux.Handle("POST /api/rooms/{room}/pairings/{id}", s.participant(s.answerPairing))
	mux.Handle("GET /api/rooms/{room}/daemon/events", s.daemonAuth(s.daemonEvents))
	mux.Handle("POST /api/rooms/{room}/daemon/status", s.daemonAuth(s.daemonStatus))

	mux.Handle("GET /api/rooms/{room}/agent", s.participant(s.getAgent))
	mux.Handle("PATCH /api/rooms/{room}/agent", s.participant(s.setAgentMode))
	mux.Handle("POST /api/rooms/{room}/turns", s.participant(s.startTurn))
	mux.Handle("POST /api/rooms/{room}/turns/{turn}/stop", s.participant(s.stopTurn))
	mux.Handle("PUT /api/rooms/{room}/host", s.participant(s.handOver))
	mux.Handle("PATCH /api/rooms/{room}/participants/{participant}", s.participant(s.setCanTrigger))
	mux.Handle("DELETE /api/rooms/{room}/queue/{queued}", s.participant(s.cancelQueued))
	mux.Handle("PATCH /api/rooms/{room}/chain", s.participant(s.controlChain))
	mux.Handle("GET /api/rooms/{room}/templates", s.participant(s.listTemplates))
	mux.Handle("POST /api/rooms/{room}/templates", s.participant(s.publishTemplate))
	mux.Handle("POST /api/rooms/{room}/personas", s.participant(s.addPersona))
	mux.Handle("PATCH /api/rooms/{room}/personas/{persona}", s.participant(s.editPersona))
	mux.Handle("POST /api/rooms/{room}/personas/{persona}/update", s.participant(s.updatePersona))
	mux.Handle("DELETE /api/rooms/{room}/personas/{persona}", s.participant(s.removePersona))
	mux.Handle("GET /api/rooms/{room}/skills", s.participant(s.roomSkills))
	mux.Handle("POST /api/rooms/{room}/skills", s.participant(s.copySkill))
	mux.Handle("POST /api/rooms/{room}/skills/{skill}/decision", s.participant(s.decideRoomSkill))
	mux.Handle("DELETE /api/rooms/{room}/skills/{skill}", s.participant(s.removeRoomSkill))
	mux.Handle("PUT /api/rooms/{room}/personas/{persona}/skills/{skill}", s.participant(s.setPersonaSkill))
	mux.Handle("DELETE /api/rooms/{room}/personas/{persona}/skills/{skill}", s.participant(s.setPersonaSkill))
	mux.Handle("POST /api/rooms/{room}/personas/{persona}/cap", s.participant(s.decideCap))
	mux.Handle("POST /api/rooms/{room}/changes/{change}", s.participant(s.decideChange))
	mux.Handle("GET /api/templates", s.admin(s.adminTemplates))
	mux.Handle("DELETE /api/templates/{template}", s.admin(s.removeTemplate))
	mux.Handle("GET /api/rooms/{room}/daemon/turns/{turn}", s.daemonAuth(s.getJob))
	mux.Handle("GET /api/rooms/{room}/daemon/turns/{turn}/workspace", s.daemonAuth(s.getWorkspace))
	mux.Handle("POST /api/rooms/{room}/daemon/turns/{turn}/workspace", s.daemonAuth(s.postWorkspace))
	mux.Handle("GET /api/rooms/{room}/daemon/turns/{turn}/manifest", s.daemonAuth(s.getManifest))
	mux.Handle("GET /api/rooms/{room}/daemon/turns/{turn}/skills", s.daemonAuth(s.getSkills))
	mux.Handle("POST /api/rooms/{room}/daemon/turns/{turn}", s.daemonAuth(s.postResult))
	mux.Handle("POST /api/rooms/{room}/daemon/turns/{turn}/state", s.daemonAuth(s.postTurnState))
	mux.Handle("GET /api/rooms/{room}/daemon/sessions/{session}", s.daemonAuth(s.getSession))

	mux.Handle("GET /api/rooms/{room}/voice", s.participant(s.getVoice))
	mux.Handle("POST /api/stt/heartbeat", s.sttAuth(s.sttHeartbeat))
	mux.Handle("POST /api/rooms/{room}/voice/lines", s.sttAuth(s.postVoiceLine))
	mux.Handle("GET /api/stt/rooms/{room}/prompt", s.sttAuth(s.sttPromptFor))
	mux.Handle("GET /api/rooms/{room}/glossary", s.participant(s.getGlossary))
	mux.Handle("PUT /api/rooms/{room}/glossary", s.participant(s.putGlossary))

	csp := "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'" + s.connectSrc()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		mux.ServeHTTP(w, r)
	})
}

// connectSrc is the CSP directive letting livekit-client signal over a WebSocket to LiveKit and
// probe its /rtc/validate over HTTPS; empty without voice.
func (s *Server) connectSrc() string {
	v := s.opts.Voice
	if v == nil {
		return ""
	}
	return "; connect-src 'self' " + origin(v.URL, false) + " " + origin(v.URL, true)
}

// roomCSP admits the room page's import map by its hash: its room admin panel is Preact (004-AC19).
var roomCSP = func() string {
	page, _ := ui.ReadFile("ui/room.html") // embedded
	return uivendor.CSP(page, "'self'")
}()

func (s *Server) roomPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", roomCSP+s.connectSrc())
	http.ServeFileFS(w, r, ui, "ui/room.html")
}

// asset serves an embedded file with a fixed type: Go's MIME table lacks .ico and .webmanifest,
// and the host's mime.types differs between machines and images.
func asset(fsys fs.FS, name, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ctype)
		http.ServeFileFS(w, r, fsys, name)
	}
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.bus.Store.Writable(ctx); err != nil {
		s.log.Error("healthz_store_unwritable", zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func bearer(r *http.Request) string {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(tok)
}

// cred is the hash of a token a request offers: a participant token, or with viaProfile, a
// profile session.
type cred struct {
	hash       string
	viaProfile bool
}

// session is an authenticated participant and the credentials the request offered, which every
// later re-check of its access tries again in the same order.
type session struct {
	p     *store.Participant
	creds []cred
	// adminUUID is the profile uuid of a member checked as a room admin, empty otherwise.
	adminUUID string
}

// crossSite reports a write another site or an opaque origin sent: a form, or a document in the
// sandboxed iframe, needs no preflight, but browsers label it, so refuse it whatever cookies do.
func crossSite(r *http.Request) bool {
	site := r.Header.Get("Sec-Fetch-Site")
	return r.Method != http.MethodGet && site != "" && site != "same-origin"
}

// participant authenticates by bearer token (API clients), the room's cookie (browsers:
// EventSource cannot set headers, and a cookie keeps the token out of URLs and access logs), or
// the profile cookie, which opens every room the profile is in, on any device it signed in on.
func (s *Server) participant(next func(http.ResponseWriter, *http.Request, session)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A bearer token is all an API client offers. A browser may hold a room cookie its
		// participant lost while its profile still has another participant in the room: try both.
		var sess session
		if tok := bearer(r); tok != "" {
			sess.creds = append(sess.creds, cred{hash: HashToken(tok)})
		} else {
			if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
				sess.creds = append(sess.creds, cred{hash: HashToken(c.Value)})
			}
			if c, err := r.Cookie(profileCookie); err == nil && c.Value != "" {
				sess.creds = append(sess.creds, cred{hash: HashToken(c.Value), viaProfile: true})
			}
		}
		if len(sess.creds) == 0 {
			httpError(w, http.StatusUnauthorized, "missing token")
			return
		}
		if crossSite(r) {
			httpError(w, http.StatusForbidden, "cross-site request refused")
			return
		}
		var err error
		sess.p, err = s.authorize(r.Context(), sess.creds, r.PathValue("room"))
		if errors.Is(err, store.ErrNotFound) {
			httpError(w, http.StatusUnauthorized, "invalid or revoked token")
			return
		}
		if err != nil {
			s.log.Error("auth_lookup_failed", zap.Error(err))
			httpError(w, http.StatusInternalServerError, "auth failed")
			return
		}
		s.seen(r.Context(), sess.p.ID, r)
		next(w, r, sess)
	})
}

// authorize returns the participant the first valid credential proves in room; ErrNotFound when
// none does.
func (s *Server) authorize(ctx context.Context, creds []cred, room string) (*store.Participant, error) {
	for _, c := range creds {
		var p *store.Participant
		var err error
		if c.viaProfile {
			p, err = s.bus.Store.ProfileParticipant(ctx, c.hash, room)
		} else if p, err = s.bus.Store.ParticipantByToken(ctx, c.hash); err == nil && p.RoomID != room {
			err = store.ErrNotFound
		}
		if !errors.Is(err, store.ErrNotFound) {
			return p, err
		}
	}
	return nil, store.ErrNotFound
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeLimit(w, r, v, 64<<10)
}

// decodeLimit reads a JSON body of at most limit bytes; past it the answer is a 413.
func decodeLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	// A cross-site form can only send "simple" content types: requiring JSON blocks login CSRF.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		httpError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(v)
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &mbe):
		httpError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body is over %d bytes", limit))
		return false
	case err != nil:
		httpError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return false
	}
	return true
}

// cleanText trims s and accepts it if it has 1..maxLen runes and no control characters; multiline
// text may hold newlines and tabs, single-line text no bidi controls (they spoof names).
func cleanText(s string, maxLen int, multiline bool) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || !store.ValidText(s, maxLen, multiline) {
		return "", false
	}
	return s, true
}

func (s *Server) listRooms(w http.ResponseWriter, r *http.Request) {
	rooms, err := s.bus.Store.Rooms(r.Context())
	if err != nil {
		s.serverError(w, "list_rooms_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, rooms)
}

func (s *Server) createRoom(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	name, ok := cleanText(req.Name, maxRoomName, false)
	if !ok {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("name must be 1 to %d characters", maxRoomName))
		return
	}
	id := strings.ToLower(NewToken())
	room, err := s.bus.Store.CreateRoom(s.auditing(r, ActionRoomCreate, store.TargetRoom, id, name, nil), id, name)
	if err != nil {
		s.serverError(w, "create_room_failed", err)
		return
	}
	s.log.Info("room_created", zap.String("room", room.ID))
	writeJSON(w, http.StatusCreated, room)
}

// deleteRoom deletes the room with everything it holds: the super-admin's or a room admin's to do.
func (s *Server) deleteRoom(w http.ResponseWriter, r *http.Request, sess *session) {
	if !s.asRoomAdmin(w, r, sess) {
		return
	}
	room := r.PathValue("room")
	err := s.bus.Store.DeleteRoom(s.roomAuditing(r, sess, ActionRoomDelete, room, nil), room)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such room")
		return
	}
	// Before the error check: a failed post-delete checkpoint still leaves the room gone.
	s.bus.Notify(room)
	s.closeVoice(room)
	s.people.forget(room)
	if err != nil {
		s.serverError(w, "delete_room_failed", err)
		return
	}
	s.log.Info("room_deleted", zap.String("room", room), zap.Bool("by_room_admin", sess != nil))
	w.WriteHeader(http.StatusNoContent)
}

type inviteResp struct {
	*store.Invite
	Token string `json:"token"`
	URL   string `json:"url"`
}

// renameRoom renames the room for a room admin or the super-admin, with a system line saying who
// did (004-AC8).
func (s *Server) renameRoom(w http.ResponseWriter, r *http.Request, sess *session) {
	if !s.asRoomAdmin(w, r, sess) {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	name, ok := cleanText(req.Name, maxRoomName, false)
	if !ok {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("name must be 1 to %d characters", maxRoomName))
		return
	}
	room := r.PathValue("room")
	var by *store.Participant
	if sess != nil {
		by = sess.p
	}
	m, err := s.bus.Store.RenameRoom(s.roomAuditing(r, sess, ActionRoomRename, room, nil), room, name, by)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such room")
		return
	}
	if err != nil {
		s.serverError(w, "rename_room_failed", err)
		return
	}
	if m != nil {
		s.bus.Notify(room)
		s.log.Info("room_renamed", zap.String("room", room))
	}
	got, err := s.bus.Store.Room(r.Context(), room)
	if err != nil {
		s.serverError(w, "rename_room_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

// listInvites lists the room's invites: all of them for the super-admin and room admins, a
// member's own for a plain member.
func (s *Server) listInvites(w http.ResponseWriter, r *http.Request, sess *session) {
	var by int64
	if sess != nil {
		admin, err := s.roomAdmin(r.Context(), sess)
		if err != nil {
			s.serverError(w, "list_invites_failed", err)
			return
		}
		if !admin {
			by = sess.p.ID
		}
	}
	invs, err := s.bus.Store.Invites(r.Context(), r.PathValue("room"), by)
	if err != nil {
		s.serverError(w, "list_invites_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, invs)
}

func (s *Server) createInvite(w http.ResponseWriter, r *http.Request, sess *session) {
	room := r.PathValue("room")
	tok := NewToken()
	var by int64
	if sess != nil {
		by = sess.p.ID
		// A room admin's invites are audited; a plain member's are not admin actions.
		if _, err := s.roomAdmin(r.Context(), sess); err != nil {
			s.serverError(w, "create_invite_failed", err)
			return
		}
	}
	inv, err := s.bus.Store.CreateInvite(s.roomAuditing(r, sess, ActionInviteCreate, room, nil), room, HashToken(tok), by)
	if errors.Is(err, store.ErrNotFound) && sess != nil {
		// Revoked between the auth check and the insert.
		httpError(w, http.StatusUnauthorized, "invalid or revoked token")
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such room")
		return
	}
	if err != nil {
		s.serverError(w, "create_invite_failed", err)
		return
	}
	s.log.Info("invite_created", zap.String("room", room), zap.Int64("invite", inv.ID), zap.Int64("by", by))
	// The token rides in the fragment, which browsers never send: it stays out of proxy logs.
	writeJSON(w, http.StatusCreated, inviteResp{Invite: inv, Token: tok, URL: s.baseURL + "/join#" + tok})
}

// revokeInvite revokes any invite for the super-admin and room admins, and only the ones they
// created for a plain member.
func (s *Server) revokeInvite(w http.ResponseWriter, r *http.Request, sess *session) {
	room := r.PathValue("room")
	id, err := strconv.ParseInt(r.PathValue("invite"), 10, 64)
	if err != nil {
		httpError(w, http.StatusNotFound, "no such invite")
		return
	}
	admin := sess == nil
	if !admin {
		if admin, err = s.roomAdmin(r.Context(), sess); err != nil {
			s.serverError(w, "revoke_invite_failed", err)
			return
		}
	}
	if !admin {
		inv, err := s.bus.Store.Invite(r.Context(), room, id)
		if errors.Is(err, store.ErrNotFound) {
			httpError(w, http.StatusNotFound, "no such invite")
			return
		}
		if err != nil {
			s.serverError(w, "revoke_invite_failed", err)
			return
		}
		if inv.CreatedBy != sess.p.ID {
			httpError(w, http.StatusForbidden, "you can revoke only the invites you created")
			return
		}
	}
	revoked, err := s.bus.Store.RevokeInvite(s.roomAuditing(r, sess, ActionInviteRevoke, room, nil), room, id)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such invite")
		return
	}
	if err != nil {
		s.serverError(w, "revoke_invite_failed", err)
		return
	}
	// Wakes the room's event streams, which re-check access and close.
	s.bus.Notify(room)
	// The revoke stands either way: a failed lookup only leaves the browser to leave audio itself.
	for _, inv := range revoked {
		if ids, err := s.bus.Store.InviteParticipantIDs(r.Context(), room, inv); err != nil {
			s.log.Error("voice_kick_lookup_failed", zap.String("room", room), zap.Error(err))
		} else {
			s.kickFromVoice(room, ids)
		}
	}
	s.log.Info("invite_revoked", zap.String("room", room), zap.Int64("invite", id), zap.Int64s("with", revoked))
	w.WriteHeader(http.StatusNoContent)
}

type joinResp struct {
	Participant *store.Participant `json:"participant"`
	// Token is for API clients; browsers use the cookie set alongside.
	Token string `json:"token"`
	// RecoveryURL signs another device into the profile this join created. Shown once: only its
	// hash is kept.
	RecoveryURL string `json:"recovery_url,omitempty"`
}

func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Invite string `json:"invite"`
		Name   string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Invite == "" {
		httpError(w, http.StatusUnauthorized, "missing invite")
		return
	}
	name, ok := cleanText(req.Name, maxNameLen, false)
	if !ok {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("display name must be 1 to %d characters", maxNameLen))
		return
	}
	var prof store.JoinProfile
	if c, err := r.Cookie(profileCookie); err == nil && c.Value != "" {
		got, err := s.bus.Store.ProfileBySession(r.Context(), HashToken(c.Value))
		switch {
		case err == nil:
			prof.ID = got.ID
		case !errors.Is(err, store.ErrNotFound):
			s.serverError(w, "join_failed", err)
			return
		}
	}
	var recovery, profileTok string
	if prof.ID == 0 {
		recovery, profileTok = NewToken(), NewToken()
		prof.RecoveryHash, prof.SessionHash = HashToken(recovery), HashToken(profileTok)
	}
	tok := NewToken()
	p, m, err := s.bus.Store.Join(r.Context(), HashToken(req.Invite), name, HashToken(tok), prof)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusUnauthorized, "invalid or revoked invite")
		return
	}
	if err != nil {
		s.serverError(w, "join_failed", err)
		return
	}
	if err := s.bus.Store.ParticipantJoined(r.Context(), p.ID, s.clientIP(r), userAgent(r), s.now()); err != nil {
		s.log.Warn("participant_joined_client_failed", zap.Int64("participant", p.ID), zap.Error(err))
	}
	s.bus.Notify(m.RoomID)
	s.log.Info("participant_joined", zap.String("room", p.RoomID), zap.Int64("participant", p.ID), zap.Int64("invite", p.InviteID))
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    tok,
		Path:     "/api/rooms/" + p.RoomID,
		MaxAge:   cookieMaxAge,
		HttpOnly: true,
		Secure:   secure(r),
		SameSite: http.SameSiteStrictMode,
	})
	resp := joinResp{Participant: p, Token: tok}
	if profileTok != "" {
		setProfileCookie(w, r, profileTok)
		// The token rides in the fragment, which browsers never send: it stays out of proxy logs.
		resp.RecoveryURL = s.baseURL + "/profile#recover=" + recovery
	}
	writeJSON(w, http.StatusCreated, resp)
}

type roomResp struct {
	Room         *store.Room         `json:"room"`
	Me           *store.Participant  `json:"me"`
	Participants []store.Participant `json:"participants"`
	// Daemons are the connected ones, with whether each can host.
	Daemons []presence `json:"daemons"`
	// Voice tells whether the server offers LiveKit audio.
	Voice bool `json:"voice"`
}

func (s *Server) getRoom(w http.ResponseWriter, r *http.Request, sess session) {
	room, err := s.bus.Store.Room(r.Context(), sess.p.RoomID)
	if err != nil {
		s.serverError(w, "get_room_failed", err)
		return
	}
	ps, err := s.bus.Store.Participants(r.Context(), room.ID)
	if err != nil {
		s.serverError(w, "get_room_failed", err)
		return
	}
	for _, p := range ps {
		if p.ID == sess.p.ID {
			sess.p.Admin = p.Admin
		}
	}
	writeJSON(w, http.StatusOK, roomResp{Room: room, Me: sess.p, Participants: ps, Daemons: s.connectedDaemons(room.ID), Voice: s.opts.Voice != nil})
}

func (s *Server) postMessage(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		Body string `json:"body"`
	}
	if !decode(w, r, &req) {
		return
	}
	body, ok := cleanText(req.Body, maxBodyLen, true)
	if !ok {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("message must be 1 to %d characters", maxBodyLen))
		return
	}
	m := &store.Message{RoomID: sess.p.RoomID, Kind: store.KindText, ParticipantID: sess.p.ID, Sender: sess.p.Name, Body: body}
	if err := s.bus.Post(r.Context(), m); err != nil {
		s.serverError(w, "post_message_failed", err)
		return
	}
	notices := s.mentionTurns(r.Context(), sess.p, m)
	writeJSON(w, http.StatusCreated, struct {
		store.Message
		// Notices go to the author alone: nobody else is told (005-AC3).
		Notices []mentionNotice `json:"mention_notices,omitempty"`
	}{viewMessage(*m), notices})
}

// events streams the room's messages after ?since= or Last-Event-ID, then live ones, a
// "message_edited" event carrying a message edited or deleted past change seq ?edits=, a
// "documents" event when a document or revision is added, an "annotations" event when an
// annotation is added or changes, an "agent" event carrying the agent state at open and whenever
// it changes, a "presence" event carrying who is here and their audio state at open and whenever it
// changes (005-AC6), and with voice on, an "stt" event at open and whenever transcription comes or
// goes. ?tab= names the page the stream belongs to, as its presence heartbeats do. The stream ends with a "revoked" event as soon as the participant's
// access is gone.
func (s *Server) events(w http.ResponseWriter, r *http.Request, sess session) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	room := sess.p.RoomID
	last, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	// EventSource sends Last-Event-ID on reconnect, so a dropped connection resumes without a gap.
	if id, err := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64); err == nil && id > last {
		last = id
	}
	// Without ?edits=, a resumed stream replays every change, which a client applies idempotently;
	// a fresh one gets every message as it stands now.
	edits, err := strconv.ParseInt(r.URL.Query().Get("edits"), 10, 64)
	if err != nil && last == 0 {
		if edits, err = s.bus.Store.ChangeCursor(r.Context(), room); err != nil {
			s.serverError(w, "events_failed", err)
			return
		}
	}

	// Read before the headers go out: a client lists the documents once the stream is open, and
	// any change after this point reaches it as a "documents" event.
	docsVersion, err := s.bus.Store.DocumentsVersion(r.Context(), room)
	if err != nil {
		s.serverError(w, "events_failed", err)
		return
	}
	annVersion, err := s.bus.Store.AnnotationsVersion(r.Context(), room)
	if err != nil {
		s.serverError(w, "events_failed", err)
		return
	}
	s.streams.Add(1)
	defer s.streams.Add(-1)
	release := s.people.open(room, sess.p.ID, r.URL.Query().Get("tab"), s.now())
	defer func() { release(s.now()) }()
	// The live channel is only a wake-up signal: every send reads from the store after the last id,
	// so a dropped or out-of-order signal can never leave a gap in the stream.
	live, cancel := s.bus.Subscribe(64)
	defer cancel()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	var agentSent string
	// sync re-checks access, then sends what the store holds past last. False ends the stream.
	sync := func() bool {
		if _, err := s.authorize(r.Context(), sess.creds, room); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				_, _ = io.WriteString(w, "event: revoked\ndata: {}\n\n")
				flusher.Flush()
			}
			return false
		}
		for {
			msgs, err := s.bus.Store.Messages(r.Context(), room, last, replayBatch)
			if err != nil {
				return false
			}
			for i := range msgs {
				b, err := json.Marshal(viewMessage(msgs[i]))
				if err != nil {
					return false
				}
				if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", msgs[i].ID, b); err != nil {
					return false
				}
				last = msgs[i].ID
			}
			flusher.Flush()
			if len(msgs) < replayBatch {
				break
			}
		}
		// No id: line, so Last-Event-ID keeps naming the last message sent.
		for {
			msgs, err := s.bus.Store.ChangedMessages(r.Context(), room, edits, replayBatch)
			if err != nil {
				return false
			}
			for i := range msgs {
				b, err := json.Marshal(viewMessage(msgs[i]))
				if err != nil {
					return false
				}
				if _, err := fmt.Fprintf(w, "event: message_edited\ndata: %s\n\n", b); err != nil {
					return false
				}
				edits = msgs[i].ChangeSeq
			}
			flusher.Flush()
			if len(msgs) < replayBatch {
				break
			}
		}
		agent, err := s.agentState(r.Context(), room)
		if err != nil {
			return false
		}
		b, err := json.Marshal(agent)
		if err != nil {
			return false
		}
		if string(b) != agentSent {
			agentSent = string(b)
			if _, err := fmt.Fprintf(w, "event: agent\ndata: %s\n\n", b); err != nil {
				return false
			}
			flusher.Flush()
		}
		for _, c := range []struct {
			event string
			seen  *string
			read  func(context.Context, string) (string, error)
		}{
			{"documents", &docsVersion, s.bus.Store.DocumentsVersion},
			{"annotations", &annVersion, s.bus.Store.AnnotationsVersion},
		} {
			v, err := c.read(r.Context(), room)
			if err != nil {
				return false
			}
			if v != *c.seen {
				*c.seen = v
				if _, err := io.WriteString(w, "event: "+c.event+"\ndata: {}\n\n"); err != nil {
					return false
				}
				flusher.Flush()
			}
		}
		return true
	}
	if !sync() {
		return
	}
	// A worker's absence and a participant's grace running out show as nothing happening, which
	// no bus signal announces: poll for them.
	ps := s.people.snapshot(room, s.now())
	if _, err := io.WriteString(w, presenceEvent(ps)); err != nil {
		return
	}
	var stt sttState
	if s.opts.Voice != nil {
		stt = s.stt.state(room, time.Now())
		if _, err := io.WriteString(w, sttEvent(stt)); err != nil {
			return
		}
	}
	flusher.Flush()
	poll := time.NewTicker(pollEvery)
	defer poll.Stop()
	tick := time.NewTicker(heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-poll.C:
			if now := s.people.snapshot(room, s.now()); !slices.EqualFunc(now, ps, samePresence) {
				ps = now
				if _, err := io.WriteString(w, presenceEvent(ps)); err != nil {
					return
				}
				flusher.Flush()
			}
			if s.opts.Voice == nil {
				continue
			}
			if now := s.stt.state(room, time.Now()); !now.equal(stt) {
				stt = now
				if _, err := io.WriteString(w, sttEvent(stt)); err != nil {
					return
				}
				flusher.Flush()
			}
		case <-tick.C:
			// The heartbeat doubles as a safety net for a signal lost to a full buffer.
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			if !sync() {
				return
			}
		case changed := <-live:
			if changed == room && !sync() {
				return
			}
		}
	}
}

func (s *Server) serverError(w http.ResponseWriter, event string, err error) {
	s.log.Error(event, zap.Error(err))
	httpError(w, http.StatusInternalServerError, "internal error")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// Serve runs srv until ctx ends, then drains it.
func Serve(ctx context.Context, srv *http.Server, log *zap.Logger) error {
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", zap.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
		close(errc)
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// SSE streams never finish on their own and browsers reconnect by themselves, so the drain
	// is short: it only has to let in-flight POSTs complete.
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return srv.Close()
	}
	return nil
}

// origin returns the scheme://host of a ws(s):// URL, as http(s):// when asHTTP is set.
func origin(wsURL string, asHTTP bool) string {
	u, err := url.Parse(wsURL)
	if err != nil {
		return ""
	}
	if asHTTP {
		u.Scheme = strings.Replace(u.Scheme, "ws", "http", 1)
	}
	return u.Scheme + "://" + u.Host
}
