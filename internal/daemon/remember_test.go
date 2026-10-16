package daemon

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// daemon-r2: the page lists the browsers it remembers, marks the asking one, and forgets one or
// all of them; a forgotten browser's session ends at once, in this process too. A page on another
// localhost port cannot write here (CSRF), and a token sent as a cookie opens nothing.
func TestAdminRememberedBrowsers(t *testing.T) {
	e := newAdminEnv(t, newTestDaemon(t, nil))
	path := filepath.Join(t.TempDir(), "admin-sessions.json")
	e.a.Remember(path)
	a, b := e.login(t), e.login(t)
	list := func(tok string) []browserView {
		t.Helper()
		code, body := e.req(t, "GET", "/api/browsers", tok, "", nil)
		var r struct{ Browsers []browserView }
		if err := json.Unmarshal([]byte(body), &r); err != nil || code != http.StatusOK {
			t.Fatalf("browsers: %d %s", code, body)
		}
		return r.Browsers
	}
	got := list(a)
	if len(got) != 2 || got[0].This == got[1].This || len(got[0].ID) != 16 {
		t.Fatalf("browsers %+v", got)
	}
	mine := got[0].ID
	if !got[0].This {
		mine = got[1].ID
	}

	for _, origin := range []string{"http://127.0.0.1:1", "http://localhost" + strings.TrimPrefix(e.base, "http://127.0.0.1"), "null", ""} {
		if code, _ := e.req(t, "POST", "/api/browsers/forget", a, `{"all":true}`, map[string]string{"Origin": origin}); code != http.StatusForbidden {
			t.Fatalf("forget from origin %q: %d", origin, code)
		}
	}
	if code, _ := e.req(t, "GET", "/api/status", "", "", map[string]string{"Cookie": "token=" + a}); code != http.StatusUnauthorized {
		t.Fatalf("token as a cookie: %d", code)
	}

	if code, _ := e.req(t, "POST", "/api/browsers/forget", b, `{"id":"`+mine+`"}`, nil); code != http.StatusNoContent {
		t.Fatalf("forget one: %d", code)
	}
	if code, _ := e.req(t, "GET", "/api/status", a, "", nil); code != http.StatusUnauthorized {
		t.Fatalf("forgotten browser's session: %d", code)
	}
	if code, _ := e.req(t, "POST", "/api/browsers/forget", b, `{"id":"`+mine+`"}`, nil); code != http.StatusNotFound {
		t.Fatalf("forget twice: %d", code)
	}
	// A session handed over by a self-update, never remembered, ends with "every browser" too.
	restored := "BBBBBBBBBBBBBBBBBBBBBBBBBB"
	e.a.Restore([]string{restored})
	if code, _ := e.req(t, "GET", "/api/status", restored, "", nil); code != http.StatusOK {
		t.Fatalf("restored session: %d", code)
	}
	if code, _ := e.req(t, "POST", "/api/browsers/forget", b, `{"all":true}`, nil); code != http.StatusNoContent {
		t.Fatalf("forget all: %d", code)
	}
	for _, tok := range []string{b, restored} {
		if code, _ := e.req(t, "GET", "/api/status", tok, "", nil); code != http.StatusUnauthorized {
			t.Fatalf("session after forget all: %d", code)
		}
	}
	if kept, err := os.ReadFile(path); err != nil || string(kept) != "[]" {
		t.Fatalf("file after forget all: %q %v", kept, err)
	}
}

// daemon-r2: a remembered session expires 30 days after its last use and a file of an older
// shape or a broken one admits nothing; at most 16 are kept, the oldest dropped.
func TestAdminRememberLimits(t *testing.T) {
	e := newAdminEnv(t, newTestDaemon(t, nil))
	path := filepath.Join(t.TempDir(), "admin-sessions.json")
	e.a.mu.Lock()
	port := e.a.port()
	e.a.mu.Unlock()
	old := time.Now().Add(-rememberTTL - time.Hour)
	stale, _ := json.Marshal([]rememberedSession{{Hash: tokenHash("STALE"), Port: port, Created: old, Used: old}, {Hash: "short", Port: port, Used: time.Now()}})
	if err := os.WriteFile(path, stale, 0o600); err != nil {
		t.Fatal(err)
	}
	e.a.Remember(path)
	e.a.mu.Lock()
	n, admitted := len(e.a.remembered), e.a.admits("STALE", time.Now())
	e.a.mu.Unlock()
	if n != 0 || admitted {
		t.Fatalf("%d kept, stale admitted %v", n, admitted)
	}
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.a.Remember(path)
	first := e.login(t)
	// A session of this process still refreshes its "last used", at most hourly.
	later := time.Now().Add(2 * time.Hour)
	e.a.mu.Lock()
	ok := e.a.admits(first, later)
	used := e.a.remembered[0].Used
	e.a.mu.Unlock()
	if !ok || !used.Equal(later) {
		t.Fatalf("admitted %v, last used %v, want %v", ok, used, later)
	}
	for range maxRemembered {
		e.login(t)
	}
	e.a.mu.Lock()
	n = len(e.a.remembered)
	e.a.mu.Unlock()
	if code, _ := e.req(t, "GET", "/api/status", first, "", nil); n != maxRemembered || code != http.StatusOK {
		// The first login keeps its session for this process even once no longer remembered.
		t.Fatalf("%d remembered, first session %d", n, code)
	}
}
