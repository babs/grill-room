package daemon

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/sandbox"
	"github.com/babs/grill-room/internal/turn"
)

//go:embed ui
var ui embed.FS

// Backend is what the page drives: the supervisor, or a Daemon on its own.
type Backend interface {
	Status() Status
	SetRoom(ctx context.Context, r Room)
	SetSettings(s Settings) error
	SetAllowlist(ctx context.Context, list []string) error
	Denied(ctx context.Context, n int) ([]sandbox.Denied, error)
	Recheck(ctx context.Context)
	Update(ctx context.Context) error
	Restart()
}

// hostBackend is a backend that runs instances and can start at login: the supervisor.
type hostBackend interface {
	Instances() ([]InstanceView, string)
	Autostart() (bool, error)
	SetAutostart(on bool) error
}

// Admin is the local admin page. Only someone holding a link this process printed or opened can
// use it: the link's one-time code is traded for a session token the page keeps in sessionStorage
// and sends as a bearer. No cookie, so no request from another site carries a credential.
type Admin struct {
	ctx  context.Context
	d    Backend
	logs *Ring
	stop func()
	log  *zap.Logger
	// announce shows a fresh link to the user, on the daemon's terminal only.
	announce func(link string)

	mu       sync.Mutex
	origin   string
	codes    map[string]bool
	sessions map[string]bool
}

// NewAdmin serves d's page; stop ends the daemon. ctx bounds the room work the page starts.
// announce receives a new link each time one is used, so a closed tab can be reopened.
func NewAdmin(ctx context.Context, d Backend, logs *Ring, stop func(), announce func(string), log *zap.Logger) *Admin {
	return &Admin{ctx: ctx, d: d, logs: logs, stop: stop, announce: announce, log: log, codes: map[string]bool{}, sessions: map[string]bool{}}
}

// Listen binds a random port on 127.0.0.1.
func (a *Admin) Listen(ctx context.Context) (net.Listener, error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.origin = "http://" + ln.Addr().String()
	a.mu.Unlock()
	return ln, nil
}

// NewLink returns a one-time link to the page.
func (a *Admin) NewLink() string {
	code := rand.Text()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.codes[code] = true
	return a.origin + "/s/" + code
}

// Handler routes the page and its API.
func (a *Admin) Handler() http.Handler {
	mux := http.NewServeMux()
	index := func(w http.ResponseWriter, r *http.Request) { http.ServeFileFS(w, r, ui, "ui/index.html") }
	mux.HandleFunc("GET /{$}", index)
	mux.HandleFunc("GET /s/{code}", index)
	mux.HandleFunc("GET /favicon.ico", asset("ui/favicon.ico", "image/x-icon"))
	mux.HandleFunc("GET /site.webmanifest", asset("ui/site.webmanifest", "application/manifest+json"))
	static, _ := fs.Sub(ui, "ui") // cannot fail: "ui" is a valid embedded path
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("POST /api/session", a.session)
	mux.Handle("GET /api/status", a.auth(a.status))
	mux.Handle("POST /api/room", a.auth(a.setRoom))
	mux.Handle("POST /api/recheck", a.auth(a.recheck))
	mux.Handle("POST /api/stop", a.auth(a.stopDaemon))
	mux.Handle("POST /api/update", a.auth(a.update))
	mux.Handle("POST /api/settings", a.auth(a.settings))
	mux.Handle("POST /api/allowlist", a.auth(a.allowlist))
	mux.Handle("GET /api/denied", a.auth(a.denied))
	mux.Handle("POST /api/autostart", a.auth(a.setAutostart))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		a.mu.Lock()
		origin := a.origin
		a.mu.Unlock()
		// Security: a DNS-rebinding page reaches 127.0.0.1 under its own host name; refuse it.
		if "http://"+r.Host != origin {
			http.Error(w, "wrong host", http.StatusForbidden)
			return
		}
		// Security: a page on another origin may send simple requests here; only ours may write.
		if r.Method != http.MethodGet && r.Header.Get("Origin") != origin {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// asset serves an embedded file with a fixed type: Go's MIME table lacks .ico and .webmanifest,
// and the host's mime.types differs between machines.
func asset(name, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ctype)
		http.ServeFileFS(w, r, ui, name)
	}
}

func (a *Admin) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		a.mu.Lock()
		ok := tok != "" && a.sessions[tok]
		a.mu.Unlock()
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "this page session ended: open the latest link the daemon printed in its terminal"})
			return
		}
		next(w, r)
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "Content-Type must be application/json"})
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// session trades a one-time code for a session token.
func (a *Admin) session(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	a.mu.Lock()
	ok := a.codes[req.Code]
	delete(a.codes, req.Code)
	tok := rand.Text()
	if ok {
		a.sessions[tok] = true
	}
	a.mu.Unlock()
	if !ok {
		a.log.Warn("admin_link_refused")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "this page link was already used: open the latest link the daemon printed in its terminal"})
		return
	}
	a.log.Info("admin_session_opened")
	a.announce(a.NewLink())
	writeJSON(w, http.StatusOK, map[string]string{"token": tok})
}

func (a *Admin) status(w http.ResponseWriter, _ *http.Request) {
	v := map[string]any{"status": a.d.Status(), "logs": a.logs.Lines(), "models": turn.Models, "efforts": turn.Efforts}
	if h, ok := a.d.(hostBackend); ok {
		v["instances"], v["instance"] = h.Instances()
		if on, err := h.Autostart(); err == nil {
			v["autostart"] = on
		}
	}
	writeJSON(w, http.StatusOK, v)
}

// setAutostart installs or removes the login item (003-AC11).
func (a *Admin) setAutostart(w http.ResponseWriter, r *http.Request) {
	h, ok := a.d.(hostBackend)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := h.SetAutostart(req.Enabled); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Admin) setRoom(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	room, err := ParseRoomURL(req.URL)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.d.SetRoom(a.ctx, room)
	w.WriteHeader(http.StatusNoContent)
}

// settings saves the host cap and approval mode; the room hears of them at once (002-AC36).
func (a *Admin) settings(w http.ResponseWriter, r *http.Request) {
	var req Settings
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := a.d.SetSettings(req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// allowlist saves the egress allowlist; the running sandbox takes it at once (002-AC36).
func (a *Admin) allowlist(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Allowlist []string `json:"allowlist"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Allowlist == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "allowlist missing"})
		return
	}
	if _, err := sandbox.NormalizeAllowlist(req.Allowlist); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := a.d.SetAllowlist(r.Context(), req.Allowlist); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deniedShown is how many refused egress requests the page lists.
const deniedShown = 50

// denied lists the egress requests the proxy refused, newest first (002-AC36).
func (a *Admin) denied(w http.ResponseWriter, r *http.Request) {
	list, err := a.d.Denied(r.Context(), deniedShown)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"denied": list})
}

func (a *Admin) recheck(w http.ResponseWriter, r *http.Request) {
	a.d.Recheck(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// update applies the room server's newer daemon, then restarts into it. Nothing updates without
// this approval (AC37).
func (a *Admin) update(w http.ResponseWriter, _ *http.Request) {
	// The daemon's context, not the request's: a closed tab must not leave a half-done download.
	if err := a.d.Update(a.ctx); err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, errNoUpdate) {
			code = http.StatusConflict
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
	a.d.Restart()
}

func (a *Admin) stopDaemon(w http.ResponseWriter, _ *http.Request) {
	a.log.Info("stop_requested")
	w.WriteHeader(http.StatusNoContent)
	a.stop()
}

// Serve runs srv on ln until ctx ends.
func Serve(ctx context.Context, srv *http.Server, ln net.Listener) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}
