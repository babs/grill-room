package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path"
	"regexp"
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
	if _, body := e.req(t, "GET", "/api/status", tok, "", nil); strings.Contains(body, `"instances"`) || strings.Contains(body, `"autostart"`) {
		t.Fatalf("status: %s", body)
	}
	if s, _ := e.req(t, "POST", "/api/stop", tok, "", nil); s != http.StatusNoContent || !e.stopped.Load() {
		t.Fatalf("stop: %d", s)
	}
}
