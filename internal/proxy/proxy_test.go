package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/babs/grill-room/internal/creds"
)

func writeCreds(t *testing.T, dir, tok string, mtime time.Time) {
	t.Helper()
	path := filepath.Join(dir, ".credentials.json")
	body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":%d}}`, tok, time.Now().Add(time.Hour).UnixMilli())
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestProxy(t *testing.T) {
	var gotAuth string
	upstreamStatus := http.StatusOK
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(upstreamStatus)
	}))
	defer up.Close()
	upURL, _ := url.Parse(up.URL)

	dir := t.TempDir()
	t0 := time.Unix(1_000_000, 0)
	writeCreds(t, dir, "real-1", t0)
	core, logs := observer.New(zap.DebugLevel)
	p := New(upURL, creds.NewSource(creds.Reader{ConfigDir: dir}), zap.New(core))
	p.Allow("w1", "dummy")
	p.Allow("w2", "old")
	p.Allow("w2", "other-worker")
	px := httptest.NewServer(p)
	defer px.Close()

	tests := []struct {
		name       string
		setup      func()
		auth       string
		wantStatus int
		wantUp     string // Authorization seen upstream, "" = request must not reach upstream
	}{
		{"swap", nil, "Bearer dummy", 200, "Bearer real-1"},
		{"no bearer", nil, "", 403, ""},
		{"wrong bearer", nil, "Bearer other", 403, ""},
		{"replaced bearer", nil, "Bearer old", 403, ""},
		{"second client", nil, "Bearer other-worker", 200, "Bearer real-1"},
		{"real token as bearer", nil, "Bearer real-1", 403, ""},
		{"file changed reloads", func() { writeCreds(t, dir, "real-2", t0.Add(time.Second)) }, "Bearer dummy", 200, "Bearer real-2"},
		// Same mtime: only the 401 invalidation can surface the new token.
		{"401 invalidates", func() { upstreamStatus = 401 }, "Bearer dummy", 401, "Bearer real-2"},
		{"after 401 rereads same mtime", func() {
			upstreamStatus = 200
			writeCreds(t, dir, "real-3", t0.Add(time.Second))
		}, "Bearer dummy", 200, "Bearer real-3"},
		{"unreadable creds forward no bearer", func() { _ = os.Remove(filepath.Join(dir, ".credentials.json")) }, "Bearer dummy", 200, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup()
			}
			gotAuth = ""
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, px.URL+"/v1/messages", http.NoBody)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tc.wantStatus || gotAuth != tc.wantUp {
				t.Fatalf("status=%d upstreamAuth=%q, want %d %q", resp.StatusCode, gotAuth, tc.wantStatus, tc.wantUp)
			}
		})
	}
	st := p.Stats()
	// 003-AC15: each client's forwarded requests are counted apart; refused ones are not.
	if st.Unauthorized != 1 || st.Requests["w1"] != 5 || st.Requests["w2"] != 1 || st.LastUse.IsZero() {
		t.Fatalf("stats = %+v", st)
	}
	// 003-AC15: a revoked client is refused at once; the others are not.
	p.Revoke("w2")
	for bearer, want := range map[string]int{"other-worker": 403, "dummy": 200} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, px.URL+"/v1/messages", http.NoBody)
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("after revoke, %s: %d, want %d", bearer, resp.StatusCode, want)
		}
	}
	// No log line may carry a real token, whatever field it would hide in.
	if logs.Len() == 0 {
		t.Fatal("no logs captured")
	}
	for _, e := range logs.All() {
		line := fmt.Sprint(e.Message, e.ContextMap())
		if strings.Contains(line, "real-") {
			t.Fatalf("token logged: %s", line)
		}
	}
}

// 003-AC15: revoking a client ends its request in flight, such as a long stream, at once.
func TestRevokeEndsStreams(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer up.Close()
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) } // a failed run must not hang on the open stream
	upURL, _ := url.Parse(up.URL)
	dir := t.TempDir()
	writeCreds(t, dir, "real", time.Now())
	p := New(upURL, creds.NewSource(creds.Reader{ConfigDir: dir}), zap.NewNop())
	p.Allow("w1", "b1")
	p.Allow("w2", "b2")
	px := httptest.NewServer(p)
	defer px.Close()
	defer free()
	stream := func(bearer string) chan error {
		done := make(chan error, 1)
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, px.URL+"/v1/messages", http.NoBody)
		req.Header.Set("Authorization", "Bearer "+bearer)
		go func() {
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				_, err = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			done <- err
		}()
		return done
	}
	done := stream("b1")
	<-started
	p.Revoke("w2")
	select {
	case <-done:
		free()
		t.Fatal("revoking another client ended this stream")
	case <-time.After(200 * time.Millisecond):
	}
	p.Revoke("w1")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		free()
		t.Fatal("the stream outlived its client's revocation")
	}
}

// The sandbox can read its dummy bearer: only Claude Code's own calls may reach upstream with the
// real token (spike S16).
func TestPathAllowlist(t *testing.T) {
	var reached []string
	up := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = append(reached, r.Method+" "+r.URL.RequestURI())
	}))
	defer up.Close()
	upURL, _ := url.Parse(up.URL)
	dir := t.TempDir()
	writeCreds(t, dir, "real-tok", time.Now())
	core, logs := observer.New(zap.DebugLevel)
	p := New(upURL, creds.NewSource(creds.Reader{ConfigDir: dir}), zap.New(core))
	p.Allow("w1", "dummy")

	long := "/v1/messages/" + strings.Repeat("a", 64<<10)
	tests := []struct {
		method, target string
		want           int
	}{
		{"POST", "/v1/messages", 200},
		{"POST", "/v1/messages?beta=true", 200},
		{"POST", "/v1/messages/count_tokens?beta=true", 200},
		{"HEAD", "/api/hello", 200},
		{"GET", "/v1/mcp_servers", 403},
		{"POST", "/v1/mcp_servers", 403},
		{"GET", "/v1/organizations/org-1/usage", 403},
		{"POST", "/api/oauth/claude_cli/create_api_key", 403},
		{"GET", "/api/oauth/profile", 403},
		{"POST", "/v1/mcp/srv_1", 403},
		{"POST", "/v1/messages/../mcp_servers", 403},
		{"POST", "/v1/messages/%2e%2e/mcp_servers", 403},
		{"POST", "/v1/messages/%2E%2E/%2E%2E/api/oauth/profile", 403},
		{"POST", "//v1/messages", 403},
		{"POST", "/v1//messages", 403},
		{"POST", "/v1%2Fmessages", 403},
		{"POST", "/v1/messages%2F..%2Fmcp_servers", 403},
		{"POST", "/v1/m%65ssages", 403},
		{"POST", "/v1/messages/", 403},
		{"POST", "/V1/messages", 403},
		{"POST", "/v1/messages;x", 403},
		{"POST", "http://api.anthropic.com/v1/mcp_servers", 403},
		{"GET", "/v1/messages", 403},
		{"PUT", "/v1/messages", 403},
		{"DELETE", "/v1/messages", 403},
		{"post", "/v1/messages", 403},
		{"POST", "/api/hello", 403},
		{"GET", "/v1/messages/count_tokens", 403},
		{"POST", long, 403},
		{strings.Repeat("X", 4096), "/v1/messages", 403},
	}
	var denied int64
	for _, tc := range tests {
		t.Run(tc.method[:min(len(tc.method), 16)]+" "+tc.target[:min(len(tc.target), 60)], func(t *testing.T) {
			reached = nil
			// httptest.NewRequestWithContext parses the target as the server does, so no client normalises it.
			req := httptest.NewRequestWithContext(t.Context(), tc.method, tc.target, http.NoBody)
			req.Header.Set("Authorization", "Bearer dummy")
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d", rec.Code, tc.want)
			}
			if tc.want == 403 {
				denied++
				if len(reached) != 0 {
					t.Fatalf("reached upstream: %v", reached)
				}
				if !strings.Contains(rec.Body.String(), "only Claude Code's model calls") {
					t.Fatalf("body %q", rec.Body.String())
				}
			} else if len(reached) != 1 || reached[0] != tc.method+" "+tc.target {
				t.Fatalf("upstream saw %v", reached)
			}
		})
	}
	if st := p.Stats(); st.Denied["w1"] != denied || st.Requests["w1"] != int64(len(tests))-denied {
		t.Fatalf("stats = %+v, want %d denied", st, denied)
	}
	entries := logs.FilterMessage("proxy_path_denied").All()
	if int64(len(entries)) != denied {
		t.Fatalf("%d proxy_path_denied lines, want %d", len(entries), denied)
	}
	for _, e := range entries {
		m := e.ContextMap()
		if m["worker"] != "w1" || m["method"] == "" || len(m["path"].(string)) > maxLogged+3 || len(m["method"].(string)) > maxLogged+3 {
			t.Fatalf("log fields %v", m)
		}
	}
	for _, e := range logs.All() {
		if line := fmt.Sprint(e.Message, e.ContextMap()); strings.Contains(line, "real-tok") || strings.Contains(line, "dummy") {
			t.Fatalf("bearer logged: %s", line)
		}
	}

	// An unknown bearer is refused before the path is looked at, and counts for no worker.
	req := httptest.NewRequestWithContext(t.Context(), "GET", "/v1/mcp_servers", http.NoBody)
	req.Header.Set("Authorization", "Bearer nope")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != 403 || len(p.Stats().Denied) != 1 {
		t.Fatalf("unknown bearer: %d, denied %v", rec.Code, p.Stats().Denied)
	}
}

// invalidations counts the token source's Invalidate calls.
type invalidations struct {
	TokenSource
	n int
}

func (s *invalidations) Invalidate() { s.n++ }

// 014-AC21 014-AC22 014-AC24: a room's connector bearer reaches POST /v1/mcp/<id> of the ids on for
// that room only, with the real token and the session header kept; anything else, another room's
// id or the inference bearer on the route, is 403 and counted; a relay 401 neither invalidates the
// token nor counts as unauthorized, and is reported by room and id; an id turned off is refused
// from the next request; a revoked worker loses its grants.
func TestConnectorRoute(t *testing.T) {
	type seen struct{ method, uri, auth, session string }
	var reached []seen
	relayStatus := http.StatusOK
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = append(reached, seen{r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), r.Header.Get("X-Mcp-Client-Session-Id")})
		w.WriteHeader(relayStatus)
	}))
	defer relay.Close()
	relayURL, _ := url.Parse(relay.URL)
	inference := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the inference upstream got %s %s", r.Method, r.URL)
	}))
	defer inference.Close()
	infURL, _ := url.Parse(inference.URL)
	dir := t.TempDir()
	writeCreds(t, dir, "real-tok", time.Now())
	src := &invalidations{TokenSource: creds.NewSource(creds.Reader{ConfigDir: dir})}
	p := New(infURL, src, zap.NewNop())
	p.SetMCPUpstream(relayURL, src)
	var lapsed []string
	p.OnConnectorUnauthorized = func(key, id string) { lapsed = append(lapsed, key+" "+id) }
	p.Allow("w1", "inference")
	p.AllowConnectors("w1", "w1|roomA", "connA", []string{"srv_cal", "srv_drive"})
	p.AllowConnectors("w1", "w1|roomB", "connB", []string{"srv_mail"})

	call := func(method, target, bearer string) int {
		req := httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("X-Mcp-Client-Session-Id", "sess-1")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := call("POST", "/v1/mcp/srv_cal", "connA"); code != 200 || len(reached) != 1 ||
		reached[0] != (seen{"POST", "/v1/mcp/srv_cal", "Bearer real-tok", "sess-1"}) {
		t.Fatalf("enabled id: %d %+v", code, reached)
	}
	denied := []struct{ method, target, bearer string }{
		{"POST", "/v1/mcp/srv_unknown", "connA"},
		{"POST", "/v1/mcp/srv_mail", "connA"}, // room B's
		{"GET", "/v1/mcp/srv_cal", "connA"},
		{"POST", "/v1/mcp/srv_cal/tools", "connA"},
		{"POST", "/v1/mcp/../mcp/srv_cal", "connA"},
		{"POST", "/v1/mcp/%2e%2e/srv_cal", "connA"},
		{"POST", "/v1/mcp/srv%2Fcal", "connA"},
		{"POST", "/v1/mcp/srv_cal%2F..%2Fsrv_mail", "connA"},
		{"POST", "/v1/mcp/", "connA"},
		{"POST", "/v1/messages", "connA"},
		{"POST", "/v1/mcp_servers", "connA"},
		{"POST", "/v1/mcp/srv_cal", "inference"},
		{"POST", "/v1/mcp/srv_cal", "connB"},
	}
	reached = nil
	for _, d := range denied {
		if code := call(d.method, d.target, d.bearer); code != http.StatusForbidden {
			t.Errorf("%s %s as %s: %d", d.method, d.target, d.bearer, code)
		}
	}
	if len(reached) != 0 {
		t.Fatalf("denied calls reached the relay: %+v", reached)
	}
	if st := p.Stats(); st.Denied["w1"] != int64(len(denied)) || st.Requests["w1"] != 1 {
		t.Fatalf("stats %+v", st)
	}
	// The connector's own login lapsed.
	relayStatus = http.StatusUnauthorized
	if code := call("POST", "/v1/mcp/srv_drive", "connA"); code != 401 {
		t.Fatalf("relay 401: %d", code)
	}
	if st := p.Stats(); st.Unauthorized != 0 || src.n != 0 || len(lapsed) != 1 || lapsed[0] != "w1|roomA srv_drive" {
		t.Fatalf("after a relay 401: unauthorized %d, invalidations %d, lapsed %v", st.Unauthorized, src.n, lapsed)
	}
	relayStatus = http.StatusOK
	// Off from the next request.
	p.AllowConnectors("w1", "w1|roomA", "connA", []string{"srv_drive"})
	if code := call("POST", "/v1/mcp/srv_cal", "connA"); code != http.StatusForbidden {
		t.Fatalf("turned off: %d", code)
	}
	p.AllowConnectors("w1", "w1|roomA", "connA", nil)
	if code := call("POST", "/v1/mcp/srv_drive", "connA"); code != http.StatusForbidden {
		t.Fatalf("none on: %d", code)
	}
	p.Revoke("w1")
	if code := call("POST", "/v1/mcp/srv_mail", "connB"); code != http.StatusForbidden {
		t.Fatalf("revoked worker: %d", code)
	}
}

// failingSource has no token to give.
type failingSource struct{}

func (failingSource) Get(context.Context) (creds.Token, error) {
	return creds.Token{}, errors.New("locked")
}
func (failingSource) Invalidate() {}

// 014-AC21: a connector call with no token to swap in goes without one, and a relay that does not
// answer is a 502; neither touches the account's counters.
func TestConnectorRouteFailures(t *testing.T) {
	var auth []string
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { auth = append(auth, r.Header.Get("Authorization")) }))
	relayURL, _ := url.Parse(relay.URL)
	p := New(relayURL, failingSource{}, zap.NewNop())
	p.SetMCPUpstream(relayURL, failingSource{})
	p.Allow("w1", "inference")
	p.AllowConnectors("w1", "w1|r", "conn", []string{"srv_cal"})
	call := func() int {
		req := httptest.NewRequestWithContext(t.Context(), "POST", "/v1/mcp/srv_cal", http.NoBody)
		req.Header.Set("Authorization", "Bearer conn")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := call(); code != http.StatusOK || len(auth) != 1 || auth[0] != "" {
		t.Fatalf("no token: %d %q", code, auth)
	}
	relay.Close()
	if code := call(); code != http.StatusBadGateway {
		t.Fatalf("relay down: %d", code)
	}
	if st := p.Stats(); st.Unauthorized != 0 || st.Requests["w1"] != 2 {
		t.Fatalf("stats %+v", st)
	}
}
