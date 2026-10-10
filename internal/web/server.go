// Package web exposes rooms over HTTP: JSON API, SSE stream, embedded UI.
//
// Auth, SSE replay and Serve are derived from github.com/babs/airc pkg/web at commit b7afc63.
package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/bus"
	"github.com/babs/grill-room/internal/store"
)

//go:embed ui
var ui embed.FS

const (
	cookieName   = "grill_room_session"
	cookieMaxAge = 30 * 24 * 3600
	maxNameLen   = 40
	maxRoomName  = 100
	maxBodyLen   = 4000
	// replayBatch bounds one store read while an SSE stream catches up.
	replayBatch = 500
)

// heartbeat keeps proxies from timing out an idle stream and re-checks access. A var for tests.
var heartbeat = 20 * time.Second

// Server serves the room API and UI.
type Server struct {
	bus       *bus.Bus
	adminHash []byte
	baseURL   string
	log       *zap.Logger
	streams   atomic.Int64
}

// New builds a server. adminHash is HashToken of the admin token; baseURL prefixes invite links.
func New(b *bus.Bus, adminHash, baseURL string, log *zap.Logger) *Server {
	return &Server{bus: b, adminHash: []byte(adminHash), baseURL: strings.TrimRight(baseURL, "/"), log: log}
}

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
	mux.HandleFunc("GET /favicon.ico", page("logo.svg"))
	mux.HandleFunc("GET /r/{room}", page("room.html"))
	static, _ := fs.Sub(ui, "ui") // cannot fail: "ui" is a valid embedded path
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

	mux.Handle("GET /api/rooms", s.admin(s.listRooms))
	mux.Handle("POST /api/rooms", s.admin(s.createRoom))
	mux.Handle("DELETE /api/rooms/{room}", s.admin(s.deleteRoom))
	mux.Handle("POST /api/rooms/{room}/invites", s.admin(s.createInvite))
	mux.Handle("DELETE /api/rooms/{room}/invites/{invite}", s.admin(s.revokeInvite))

	mux.HandleFunc("POST /api/join", s.join)
	mux.Handle("GET /api/rooms/{room}", s.participant(s.getRoom))
	mux.Handle("POST /api/rooms/{room}/messages", s.participant(s.postMessage))
	mux.Handle("GET /api/rooms/{room}/events", s.participant(s.events))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		mux.ServeHTTP(w, r)
	})
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

func (s *Server) admin(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if tok == "" || subtle.ConstantTimeCompare([]byte(HashToken(tok)), s.adminHash) != 1 {
			httpError(w, http.StatusUnauthorized, "admin token required")
			return
		}
		next(w, r)
	})
}

// session is an authenticated participant and the hash of the token that proved it.
type session struct {
	p         *store.Participant
	tokenHash string
}

// participant authenticates by bearer token (API clients) or cookie (browsers: EventSource cannot
// set headers, and a cookie keeps the token out of URLs and access logs).
func (s *Server) participant(next func(http.ResponseWriter, *http.Request, session)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if tok == "" {
			if c, err := r.Cookie(cookieName); err == nil {
				tok = c.Value
			}
		}
		if tok == "" {
			httpError(w, http.StatusUnauthorized, "missing token")
			return
		}
		sess := session{tokenHash: HashToken(tok)}
		var err error
		sess.p, err = s.authorize(r.Context(), sess.tokenHash, r.PathValue("room"))
		if errors.Is(err, store.ErrNotFound) {
			httpError(w, http.StatusUnauthorized, "invalid or revoked token")
			return
		}
		if err != nil {
			s.log.Error("auth_lookup_failed", zap.Error(err))
			httpError(w, http.StatusInternalServerError, "auth failed")
			return
		}
		next(w, r, sess)
	})
}

// authorize returns the participant for tokenHash if it is valid for room; ErrNotFound otherwise.
func (s *Server) authorize(ctx context.Context, tokenHash, room string) (*store.Participant, error) {
	p, err := s.bus.Store.ParticipantByToken(ctx, tokenHash)
	if err != nil {
		return nil, err
	}
	if p.RoomID != room {
		return nil, store.ErrNotFound
	}
	return p, nil
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	// A cross-site form can only send "simple" content types: requiring JSON blocks login CSRF.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		httpError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
		httpError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return false
	}
	return true
}

// cleanText trims s and accepts it if it has 1..maxLen runes and no control characters; multiline
// text may hold newlines and tabs, single-line text no bidi controls (they spoof names).
func cleanText(s string, maxLen int, multiline bool) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxLen {
		return "", false
	}
	for _, c := range s {
		if multiline && (c == '\n' || c == '\t') {
			continue
		}
		if unicode.IsControl(c) || (!multiline && unicode.Is(unicode.Bidi_Control, c)) {
			return "", false
		}
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
	room, err := s.bus.Store.CreateRoom(r.Context(), strings.ToLower(NewToken()), name)
	if err != nil {
		s.serverError(w, "create_room_failed", err)
		return
	}
	s.log.Info("room_created", zap.String("room", room.ID))
	writeJSON(w, http.StatusCreated, room)
}

func (s *Server) deleteRoom(w http.ResponseWriter, r *http.Request) {
	room := r.PathValue("room")
	err := s.bus.Store.DeleteRoom(r.Context(), room)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such room")
		return
	}
	// Before the error check: a failed post-delete checkpoint still leaves the room gone.
	s.bus.Notify(room)
	if err != nil {
		s.serverError(w, "delete_room_failed", err)
		return
	}
	s.log.Info("room_deleted", zap.String("room", room))
	w.WriteHeader(http.StatusNoContent)
}

type inviteResp struct {
	*store.Invite
	Token string `json:"token"`
	URL   string `json:"url"`
}

func (s *Server) createInvite(w http.ResponseWriter, r *http.Request) {
	room := r.PathValue("room")
	tok := NewToken()
	inv, err := s.bus.Store.CreateInvite(r.Context(), room, HashToken(tok))
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such room")
		return
	}
	if err != nil {
		s.serverError(w, "create_invite_failed", err)
		return
	}
	s.log.Info("invite_created", zap.String("room", room), zap.Int64("invite", inv.ID))
	// The token rides in the fragment, which browsers never send: it stays out of proxy logs.
	writeJSON(w, http.StatusCreated, inviteResp{Invite: inv, Token: tok, URL: s.baseURL + "/join#" + tok})
}

func (s *Server) revokeInvite(w http.ResponseWriter, r *http.Request) {
	room := r.PathValue("room")
	id, err := strconv.ParseInt(r.PathValue("invite"), 10, 64)
	if err != nil {
		httpError(w, http.StatusNotFound, "no such invite")
		return
	}
	err = s.bus.Store.RevokeInvite(r.Context(), room, id)
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
	s.log.Info("invite_revoked", zap.String("room", room), zap.Int64("invite", id))
	w.WriteHeader(http.StatusNoContent)
}

type joinResp struct {
	Participant *store.Participant `json:"participant"`
	// Token is for API clients; browsers use the cookie set alongside.
	Token string `json:"token"`
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
	tok := NewToken()
	p, m, err := s.bus.Store.Join(r.Context(), HashToken(req.Invite), name, HashToken(tok))
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusUnauthorized, "invalid or revoked invite")
		return
	}
	if err != nil {
		s.serverError(w, "join_failed", err)
		return
	}
	s.bus.Notify(m.RoomID)
	s.log.Info("participant_joined", zap.String("room", p.RoomID), zap.Int64("participant", p.ID), zap.Int64("invite", p.InviteID))
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    tok,
		Path:     "/api/rooms/" + p.RoomID,
		MaxAge:   cookieMaxAge,
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusCreated, joinResp{Participant: p, Token: tok})
}

type roomResp struct {
	Room         *store.Room         `json:"room"`
	Me           *store.Participant  `json:"me"`
	Participants []store.Participant `json:"participants"`
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
	writeJSON(w, http.StatusOK, roomResp{Room: room, Me: sess.p, Participants: ps})
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
	writeJSON(w, http.StatusCreated, m)
}

// events streams the room's messages after ?since= or Last-Event-ID, then live ones. The stream
// ends with a "revoked" event as soon as the participant's access is gone.
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

	s.streams.Add(1)
	defer s.streams.Add(-1)
	// The live channel is only a wake-up signal: every send reads from the store after the last id,
	// so a dropped or out-of-order signal can never leave a gap in the stream.
	live, cancel := s.bus.Subscribe(64)
	defer cancel()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// sync re-checks access, then sends what the store holds past last. False ends the stream.
	sync := func() bool {
		if _, err := s.authorize(r.Context(), sess.tokenHash, room); err != nil {
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
				b, err := json.Marshal(&msgs[i])
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
				return true
			}
		}
	}
	if !sync() {
		return
	}
	tick := time.NewTicker(heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
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
