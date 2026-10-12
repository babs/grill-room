package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/monitor"
	"github.com/babs/grill-room/internal/sandbox"
	"github.com/babs/grill-room/internal/turn"
)

//go:embed ui
var ui embed.FS

// csp lets the page run its own scripts and its import map, inline in index.html, by hash.
var csp = func() string {
	page, _ := ui.ReadFile("ui/index.html") // embedded above
	m := regexp.MustCompile(`(?s)<script type="importmap">(.*?)</script>`).FindSubmatch(page)
	if m == nil {
		panic("ui/index.html has no import map")
	}
	sum := sha256.Sum256(m[1])
	return "default-src 'self'; script-src 'self' 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) +
		"'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'"
}()

// Backend is what the page drives: the supervisor, or a Daemon on its own. Its instanceOps act
// on the instance the page shows.
type Backend interface {
	Status() Status
	SetRoom(ctx context.Context, r Room)
	instanceOps
}

// hostBackend is a backend that runs instances, checks and updates binaries, and can start at
// login: the supervisor.
type hostBackend interface {
	Instances() ([]InstanceView, string)
	Autostart() (bool, error)
	SetAutostart(on bool) error
	Trust() TrustView
	AddDevKey(ctx context.Context, name string, pem []byte) error
	RemoveDevKey(ctx context.Context, name string) error
	SetRelax(id string, on bool, host string) error
	SetUpdateChannel(channel, instance string) error
	Update(ctx context.Context) error
	Restart()
	AccountViews(ctx context.Context) []AccountView
	AddAccount(ctx context.Context, acc Account, token string) error
	RemoveAccount(name string) error
	ProbeAccount(ctx context.Context, name string) error
	SetAccount(instance, room, account string) error
	SetCutOff(instance string, on bool) error
	Concurrency() ConcurrencyView
	SetMaxTurns(n int) error
	InstanceOps(id string) (instanceOps, error)
	SetLimits(id string, l sandbox.Limits) error
	Monitor() *monitor.DB
	RetentionDays() int
	SetRetention(days int) error
	Alerts(ctx context.Context, accounts []AccountView, insts []InstanceView, c ConcurrencyView) []Alert
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
	mux.Handle("POST /api/dev-keys", a.auth(a.host(a.addDevKey)))
	mux.Handle("POST /api/dev-keys/remove", a.auth(a.host(a.removeDevKey)))
	mux.Handle("POST /api/relax", a.auth(a.host(a.relax)))
	mux.Handle("POST /api/update-channel", a.auth(a.host(a.updateChannel)))
	mux.Handle("POST /api/accounts", a.auth(a.host(a.addAccount)))
	mux.Handle("POST /api/accounts/remove", a.auth(a.host(a.removeAccount)))
	mux.Handle("POST /api/accounts/probe", a.auth(a.host(a.probeAccount)))
	mux.Handle("POST /api/instance-account", a.auth(a.host(a.instanceAccount)))
	mux.Handle("POST /api/cut-off", a.auth(a.host(a.cutOff)))
	mux.Handle("POST /api/max-turns", a.auth(a.host(a.maxTurns)))
	mux.Handle("POST /api/limits", a.auth(a.host(a.limits)))
	mux.Handle("POST /api/retention", a.auth(a.host(a.retention)))
	mux.Handle("GET /api/monitor/turns", a.auth(a.host(a.monitorTurns)))
	mux.Handle("GET /api/monitor/turns.csv", a.auth(a.host(a.monitorCSV)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
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

func (a *Admin) status(w http.ResponseWriter, r *http.Request) {
	v := map[string]any{"status": a.d.Status(), "logs": a.logs.Lines(), "models": turn.Models, "efforts": turn.Efforts}
	if h, ok := a.d.(hostBackend); ok {
		insts, current := h.Instances()
		accounts, conc := h.AccountViews(r.Context()), h.Concurrency()
		v["instances"], v["instance"], v["accounts"], v["concurrency"] = insts, current, accounts, conc
		if on, err := h.Autostart(); err == nil {
			v["autostart"] = on
		}
		v["trust"] = h.Trust()
		v["retention_days"] = h.RetentionDays()
		v["alerts"] = h.Alerts(r.Context(), accounts, insts, conc)
	}
	writeJSON(w, http.StatusOK, v)
}

// ops is the instance a request names, or the one the page shows when it names none.
func (a *Admin) ops(id string) (instanceOps, error) {
	if h, ok := a.d.(hostBackend); ok && id != "" {
		return h.InstanceOps(id)
	}
	return a.d, nil
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
	var req struct {
		Settings
		Instance string `json:"instance"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	d, err := a.ops(req.Instance)
	if err == nil {
		err = d.SetSettings(req.Settings)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// allowlist saves the egress allowlist; the running sandbox takes it at once (002-AC36).
func (a *Admin) allowlist(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Allowlist []string `json:"allowlist"`
		Instance  string   `json:"instance"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	d, err := a.ops(req.Instance)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
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
	if err := d.SetAllowlist(r.Context(), req.Allowlist); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deniedShown is how many refused egress requests the page lists.
const deniedShown = 50

// denied lists the egress requests the proxy refused, newest first (002-AC36).
func (a *Admin) denied(w http.ResponseWriter, r *http.Request) {
	d, err := a.ops(r.URL.Query().Get("instance"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	list, err := d.Denied(r.Context(), deniedShown)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"denied": list})
}

func (a *Admin) recheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Instance string `json:"instance"`
	}
	// An empty body rechecks the instance the page shows.
	if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	d, err := a.ops(req.Instance)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	d.Recheck(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// update applies the supervisor's update, then restarts into it. Nothing updates without this
// approval (001-AC37, 003-AC5).
func (a *Admin) update(w http.ResponseWriter, r *http.Request) {
	h, ok := a.d.(hostBackend)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// The daemon's context, not the request's: a closed tab must not leave a half-done download.
	if err := h.Update(a.ctx); err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, errNoUpdate) {
			code = http.StatusConflict
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
	h.Restart()
}

// hostHandler is a handler for the supervisor's own settings.
type hostHandler func(w http.ResponseWriter, r *http.Request, h hostBackend)

// host serves next with the supervisor, or 404 for a backend that is not one.
func (a *Admin) host(next hostHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h, ok := a.d.(hostBackend)
		if !ok {
			http.NotFound(w, r)
			return
		}
		next(w, r, h)
	}
}

func reply(w http.ResponseWriter, err error) {
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// addDevKey trusts a cosign public key for dev builds (003-AC4).
func (a *Admin) addDevKey(w http.ResponseWriter, r *http.Request, h hostBackend) {
	var req struct {
		Name string `json:"name"`
		PEM  string `json:"pem"`
	}
	if decodeJSON(w, r, &req) {
		reply(w, h.AddDevKey(a.ctx, req.Name, []byte(req.PEM)))
	}
}

func (a *Admin) removeDevKey(w http.ResponseWriter, r *http.Request, h hostBackend) {
	var req struct {
		Name string `json:"name"`
	}
	if decodeJSON(w, r, &req) {
		reply(w, h.RemoveDevKey(a.ctx, req.Name))
	}
}

// relax switches an instance's "accept unsigned" toggle; on needs its host typed (003-AC4).
func (a *Admin) relax(w http.ResponseWriter, r *http.Request, h hostBackend) {
	var req struct {
		Instance string `json:"instance"`
		Enabled  bool   `json:"enabled"`
		Host     string `json:"host"`
	}
	if decodeJSON(w, r, &req) {
		reply(w, h.SetRelax(req.Instance, req.Enabled, req.Host))
	}
}

// updateChannel picks GitHub or an instance as the supervisor's update source (003-AC5).
func (a *Admin) updateChannel(w http.ResponseWriter, r *http.Request, h hostBackend) {
	var req struct {
		Channel  string `json:"channel"`
		Instance string `json:"instance"`
	}
	if decodeJSON(w, r, &req) {
		reply(w, h.SetUpdateChannel(req.Channel, req.Instance))
	}
}

// addAccount adds a Claude account. The token is never logged or sent back (003-AC12).
func (a *Admin) addAccount(w http.ResponseWriter, r *http.Request, h hostBackend) {
	var req struct {
		Name      string `json:"name"`
		Kind      string `json:"kind"`
		ConfigDir string `json:"config_dir"`
		Token     string `json:"token"`
	}
	if decodeJSON(w, r, &req) {
		reply(w, h.AddAccount(a.ctx, Account{Name: req.Name, Kind: req.Kind, ConfigDir: req.ConfigDir}, req.Token))
	}
}

func (a *Admin) removeAccount(w http.ResponseWriter, r *http.Request, h hostBackend) {
	var req struct {
		Name string `json:"name"`
	}
	if decodeJSON(w, r, &req) {
		reply(w, h.RemoveAccount(req.Name))
	}
}

// probeAccount checks a setup token now, on demand (003-AC12).
func (a *Admin) probeAccount(w http.ResponseWriter, r *http.Request, h hostBackend) {
	var req struct {
		Name string `json:"name"`
	}
	if decodeJSON(w, r, &req) {
		reply(w, h.ProbeAccount(r.Context(), req.Name))
	}
}

// instanceAccount maps an instance, or one of its rooms, to an account (003-AC13).
func (a *Admin) instanceAccount(w http.ResponseWriter, r *http.Request, h hostBackend) {
	var req struct {
		Instance string `json:"instance"`
		Room     string `json:"room"`
		Account  string `json:"account"`
	}
	if decodeJSON(w, r, &req) {
		reply(w, h.SetAccount(req.Instance, req.Room, req.Account))
	}
}

// cutOff cuts a worker off from Claude, or lets it back (003-AC15).
func (a *Admin) cutOff(w http.ResponseWriter, r *http.Request, h hostBackend) {
	var req struct {
		Instance string `json:"instance"`
		CutOff   bool   `json:"cut_off"`
	}
	if decodeJSON(w, r, &req) {
		reply(w, h.SetCutOff(req.Instance, req.CutOff))
	}
}

// maxTurns sets the global cap on concurrent turns (003-AC18).
func (a *Admin) maxTurns(w http.ResponseWriter, r *http.Request, h hostBackend) {
	var req struct {
		MaxTurns int `json:"max_turns"`
	}
	if decodeJSON(w, r, &req) {
		reply(w, h.SetMaxTurns(req.MaxTurns))
	}
}

// limits sets an instance's sandbox CPUs and memory; its worker restarts (003-AC26).
func (a *Admin) limits(w http.ResponseWriter, r *http.Request, h hostBackend) {
	var req struct {
		Instance string `json:"instance"`
		sandbox.Limits
	}
	if decodeJSON(w, r, &req) {
		reply(w, h.SetLimits(req.Instance, req.Limits))
	}
}

// retention sets how many days the turn history keeps (003-AC28).
func (a *Admin) retention(w http.ResponseWriter, r *http.Request, h hostBackend) {
	var req struct {
		Days int `json:"days"`
	}
	if decodeJSON(w, r, &req) {
		reply(w, h.SetRetention(req.Days))
	}
}

// turnsShown bounds the turn list on the page; the CSV holds them all.
const turnsShown = 500

// monitorFilter reads the turn filter from the query.
func monitorFilter(r *http.Request) monitor.Filter {
	q := r.URL.Query()
	return monitor.Filter{Instance: q.Get("instance"), Room: q.Get("room"), Persona: q.Get("persona"), Account: q.Get("account"),
		Status: q.Get("status"), From: q.Get("from"), To: q.Get("to")}
}

// monitorError answers a history query that failed: the filter, or the database.
func monitorError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	if errors.Is(err, monitor.ErrFilter) {
		code = http.StatusBadRequest
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// monitorTurns lists the recorded turns the filter matches, the daily totals per account, and the
// values each filter offers (003-AC27).
func (a *Admin) monitorTurns(w http.ResponseWriter, r *http.Request, h hostBackend) {
	m := h.Monitor()
	if m == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "this supervisor keeps no turn history"})
		return
	}
	f := monitorFilter(r)
	turns, err := m.Turns(r.Context(), f, turnsShown)
	if err != nil {
		monitorError(w, err)
		return
	}
	totals, err := m.Totals(r.Context(), f)
	if err != nil {
		monitorError(w, err)
		return
	}
	facets, err := m.Facets(r.Context())
	if err != nil {
		monitorError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"turns": turns, "totals": totals, "facets": facets, "limit": turnsShown})
}

// monitorCSV exports every recorded turn the filter matches (003-AC27).
func (a *Admin) monitorCSV(w http.ResponseWriter, r *http.Request, h hostBackend) {
	m := h.Monitor()
	if m == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "this supervisor keeps no turn history"})
		return
	}
	turns, err := m.Turns(r.Context(), monitorFilter(r), 0)
	if err != nil {
		monitorError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="grill-room-turns.csv"`)
	if err := monitor.WriteCSV(w, turns); err != nil {
		a.log.Warn("monitor_csv_failed", zap.Error(err))
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
