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
)

//go:embed ui
var ui embed.FS

// Admin is the local admin page. Only someone holding a link this process printed or opened can
// use it: the link's one-time code is traded for a session token the page keeps in sessionStorage
// and sends as a bearer. No cookie, so no request from another site carries a credential.
type Admin struct {
	ctx  context.Context
	d    *Daemon
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
func NewAdmin(ctx context.Context, d *Daemon, logs *Ring, stop func(), announce func(string), log *zap.Logger) *Admin {
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
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(v); err != nil {
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
	writeJSON(w, http.StatusOK, map[string]any{"status": a.d.Status(), "logs": a.logs.Lines()})
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
	if a.d.o.Restart != nil {
		a.d.o.Restart()
	}
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
