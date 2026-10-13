package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

type adminEnv struct {
	a       *Admin
	base    string
	stopped atomic.Bool
	links   []string
}

func newAdminEnv(t *testing.T, d Backend) *adminEnv {
	t.Helper()
	e := &adminEnv{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e.a = NewAdmin(ctx, d, NewRing(10), func() { e.stopped.Store(true) }, func(l string) { e.links = append(e.links, l) }, zap.NewNop())
	ln, err := e.a.Listen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: e.a.Handler(), ReadHeaderTimeout: time.Second}
	go func() { _ = Serve(ctx, srv, ln) }()
	e.base = "http://" + ln.Addr().String()
	return e
}

func (e *adminEnv) req(t *testing.T, method, path, token, body string, hdr map[string]string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequestWithContext(t.Context(), method, e.base+path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		req.Header.Set("Origin", e.base)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *adminEnv) login(t *testing.T) string {
	t.Helper()
	link := e.a.NewLink()
	code := link[strings.LastIndex(link, "/")+1:]
	status, body := e.req(t, "POST", "/api/session", "", `{"code":"`+code+`"}`, nil)
	if status != http.StatusOK {
		t.Fatalf("session: %d %s", status, body)
	}
	var r struct{ Token string }
	_ = json.Unmarshal([]byte(body), &r)
	return r.Token
}

// 001-AC22: the local page answers only on 127.0.0.1, to the holder of a one-time link, and
// refuses requests from other origins (CSRF) and other host names (DNS rebinding).
func TestAdminAccess(t *testing.T) {
	e := newAdminEnv(t, newTestDaemon(t, nil))
	if !strings.HasPrefix(e.base, "http://127.0.0.1:") {
		t.Fatalf("listening on %s", e.base)
	}
	link := e.a.NewLink()
	if !strings.HasPrefix(link, e.base+"/s/") {
		t.Fatalf("link = %s", link)
	}
	code := link[strings.LastIndex(link, "/")+1:]

	if s, _ := e.req(t, "GET", "/s/"+code, "", "", nil); s != http.StatusOK {
		t.Fatalf("page: %d", s)
	}
	if s, _ := e.req(t, "GET", "/api/status", "", "", nil); s != http.StatusUnauthorized {
		t.Fatalf("status without session: %d", s)
	}
	if s, _ := e.req(t, "POST", "/api/session", "", `{"code":"`+code+`"}`, map[string]string{"Origin": "https://evil.example"}); s != http.StatusForbidden {
		t.Fatalf("cross-origin session: %d", s)
	}
	if s, _ := e.req(t, "POST", "/api/session", "", `{"code":"`+code+`"}`, map[string]string{"Origin": ""}); s != http.StatusForbidden {
		t.Fatalf("session without Origin: %d", s)
	}
	if s, _ := e.req(t, "GET", "/", "", "", map[string]string{"Host": "rebind.evil.example:" + e.base[len("http://127.0.0.1:"):]}); s != http.StatusForbidden {
		t.Fatalf("rebinding host: %d", s)
	}
	if s, _ := e.req(t, "POST", "/api/session", "", "code="+code, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}); s != http.StatusUnsupportedMediaType {
		t.Fatalf("form session: %d", s)
	}
	s, body := e.req(t, "POST", "/api/session", "", `{"code":"`+code+`"}`, nil)
	if s != http.StatusOK {
		t.Fatalf("session: %d %s", s, body)
	}
	if len(e.links) != 1 || e.links[0] == link {
		t.Fatalf("no fresh link announced after use: %v", e.links)
	}
	if s, _ := e.req(t, "POST", "/api/session", "", `{"code":"`+code+`"}`, nil); s != http.StatusUnauthorized {
		t.Fatalf("code reused: %d", s)
	}
	var r struct{ Token string }
	_ = json.Unmarshal([]byte(body), &r)
	if s, body := e.req(t, "GET", "/api/status", r.Token, "", nil); s != http.StatusOK || !strings.Contains(body, `"can_host"`) {
		t.Fatalf("status: %d %s", s, body)
	}
	if s, _ := e.req(t, "POST", "/api/stop", r.Token, "", map[string]string{"Origin": "null"}); s != http.StatusForbidden || e.stopped.Load() {
		t.Fatalf("stop from an opaque origin: %d", s)
	}
}

var iconLink = regexp.MustCompile(`<link rel="(icon|apple-touch-icon|manifest)" href="([^"]+)"`)

// The page links the same icons as the room pages, and each one is served with its type.
func TestAdminIcons(t *testing.T) {
	e := newAdminEnv(t, newTestDaemon(t, nil))
	types := map[string]string{".ico": "image/x-icon", ".svg": "image/svg+xml", ".png": "image/png", ".webmanifest": "application/manifest+json"}
	_, html := e.req(t, "GET", "/", "", "", nil)
	hrefs := map[string]bool{}
	for _, l := range iconLink.FindAllStringSubmatch(html, -1) {
		hrefs[l[2]] = true
	}
	for _, want := range []string{"/favicon.ico", "/static/logo.svg", "/static/icons/icon-32.png", "/static/icons/apple-touch-icon.png", "/site.webmanifest"} {
		if !hrefs[want] {
			t.Errorf("page does not link %s", want)
		}
	}
	srcs := []string{"/static/icons/icon-192.png", "/static/icons/icon-512.png"}
	for href := range hrefs {
		srcs = append(srcs, href)
	}
	for _, src := range srcs {
		req, _ := http.NewRequestWithContext(t.Context(), "GET", e.base+src, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if want := types[path.Ext(src)]; resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != want {
			t.Errorf("%s: %d %q, want %q", src, resp.StatusCode, resp.Header.Get("Content-Type"), want)
		}
	}
}

func TestAdminActions(t *testing.T) {
	e := newAdminEnv(t, newTestDaemon(t, nil))
	tok := e.login(t)
	if s, body := e.req(t, "POST", "/api/room", tok, `{"url":"http://rooms.example/r/abc"}`, nil); s != http.StatusBadRequest || !strings.Contains(body, "https") {
		t.Fatalf("plain http room: %d %s", s, body)
	}
	if s, _ := e.req(t, "POST", "/api/recheck", tok, "", nil); s != http.StatusNoContent {
		t.Fatalf("recheck: %d", s)
	}
	// A daemon serving one room has no login item: only the supervisor offers one.
	if s, _ := e.req(t, "POST", "/api/autostart", tok, `{"enabled":true}`, nil); s != http.StatusNotFound {
		t.Fatalf("autostart: %d", s)
	}
	// Nor does it check binaries or update itself.
	for path, body := range map[string]string{"/api/update": "", "/api/relax": `{"instance":"x"}`, "/api/dev-keys": `{"name":"x"}`,
		"/api/dev-keys/remove": `{"name":"x"}`, "/api/update-channel": `{"channel":"github"}`} {
		if s, _ := e.req(t, "POST", path, tok, body, nil); s != http.StatusNotFound {
			t.Fatalf("%s: %d", path, s)
		}
	}
	if _, body := e.req(t, "GET", "/api/status", tok, "", nil); strings.Contains(body, `"instances"`) || strings.Contains(body, `"autostart"`) || strings.Contains(body, `"trust"`) {
		t.Fatalf("status: %s", body)
	}
	if s, _ := e.req(t, "POST", "/api/stop", tok, "", nil); s != http.StatusNoContent || !e.stopped.Load() {
		t.Fatalf("stop: %d", s)
	}
}

// The page's import map runs under the CSP by its hash, and nothing else inline does; every
// module it maps is served as JavaScript.
func TestAdminImportMap(t *testing.T) {
	e := newAdminEnv(t, newTestDaemon(t, nil))
	req, _ := http.NewRequestWithContext(t.Context(), "GET", e.base+"/", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	m := regexp.MustCompile(`(?s)<script type="importmap">(.*?)</script>`).FindSubmatch(page)
	if m == nil {
		t.Fatal("no import map")
	}
	sum := sha256.Sum256(m[1])
	want := "script-src 'self' 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "';"
	if c := resp.Header.Get("Content-Security-Policy"); !strings.Contains(c, want) || strings.Contains(c, "unsafe") {
		t.Fatalf("CSP %q, want %q", c, want)
	}
	// The import map is the only inline script; the module and the shared kit come from the server.
	if n, src := strings.Count(string(page), "<script"), strings.Count(string(page), "<script src="); n != 3 || src != 1 {
		t.Fatalf("%d scripts in the page, %d with a src", n, src)
	}
	var im struct{ Imports map[string]string }
	if err := json.Unmarshal(m[1], &im); err != nil || len(im.Imports) != 6 {
		t.Fatalf("import map %s: %v", m[1], err)
	}
	for _, p := range append(slices.Collect(maps.Values(im.Imports)), "/static/admin.js", "/static/api.js", "/static/monitor.js", "/static/kit/ui.js") {
		req, _ := http.NewRequestWithContext(t.Context(), "GET", e.base+p, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "javascript") {
			t.Fatalf("%s: %d %s", p, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
}

// 005-AC45: after a self-update, the new binary binds the page's old port and admits its sessions:
// the open tab's next request works without a new link. A port taken meanwhile falls back to a
// random one, and an address off 127.0.0.1 is never bound.
func TestAdminPageSurvivesRestart(t *testing.T) {
	d := newTestDaemon(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	old := NewAdmin(ctx, d, NewRing(10), func() {}, func(string) {}, zap.NewNop())
	ln, err := old.Listen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: old.Handler(), ReadHeaderTimeout: time.Second}
	served := make(chan struct{})
	go func() { _ = Serve(ctx, srv, ln); close(served) }()
	e := &adminEnv{a: old, base: "http://" + ln.Addr().String()}
	tok := e.login(t)
	addr, sessions := ln.Addr().String(), old.Sessions()
	cancel()
	<-served

	e2 := &adminEnv{}
	ctx2, cancel2 := context.WithCancel(context.Background())
	t.Cleanup(cancel2)
	e2.a = NewAdmin(ctx2, d, NewRing(10), func() {}, func(string) {}, zap.NewNop())
	ln2, reused, err := e2.a.ListenAt(ctx2, addr)
	if err != nil || !reused || ln2.Addr().String() != addr {
		t.Fatalf("listen %v, reused %v, err %v", ln2, reused, err)
	}
	e2.a.Restore(sessions)
	go func() { _ = Serve(ctx2, &http.Server{Handler: e2.a.Handler(), ReadHeaderTimeout: time.Second}, ln2) }()
	e2.base = "http://" + addr
	if code, body := e2.req(t, "GET", "/api/status", tok, "", nil); code != http.StatusOK {
		t.Fatalf("old tab after restart: %d %s", code, body)
	}
	if code, _ := e2.req(t, "GET", "/api/status", "not-a-session", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("unknown token: %d", code)
	}
	// A handover only admits tokens shaped as the page hands them out.
	e2.a.Restore([]string{"x", "short"})
	for _, tok := range []string{"x", "short"} {
		if code, _ := e2.req(t, "GET", "/api/status", tok, "", nil); code != http.StatusUnauthorized {
			t.Fatalf("restored %q: %d", tok, code)
		}
	}

	// The port is taken now: a third binary gets another one.
	third := NewAdmin(ctx2, d, NewRing(10), func() {}, func(string) {}, zap.NewNop())
	ln3, reused, err := third.ListenAt(ctx2, addr)
	if err != nil || reused || ln3.Addr().String() == addr {
		t.Fatalf("taken port: %v, reused %v, err %v", ln3, reused, err)
	}
	_ = ln3.Close()
	ln4, reused, err := third.ListenAt(ctx2, "0.0.0.0:0")
	if err != nil || reused || !strings.HasPrefix(ln4.Addr().String(), "127.0.0.1:") {
		t.Fatalf("off-loopback address: %v, reused %v, err %v", ln4, reused, err)
	}
	_ = ln4.Close()
}
