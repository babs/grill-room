package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/babs/grill-room/internal/creds"
	"github.com/babs/grill-room/internal/proxy"
)

// dockerTest skips unless GRILL_ROOM_TEST_DOCKER=1: these tests need Docker and the sandbox image
// (make sandbox-image). They make no request to Anthropic.
func dockerTest(t *testing.T) string {
	t.Helper()
	if os.Getenv("GRILL_ROOM_TEST_DOCKER") != "1" {
		t.Skip("set GRILL_ROOM_TEST_DOCKER=1 to run the Docker sandbox tests")
	}
	if img := os.Getenv("GRILL_ROOM_SANDBOX_IMAGE"); img != "" {
		return img
	}
	return "grill-room-sandbox:local"
}

// fakeAnthropic answers /v1/messages like the Messages API, with text "ok", for the bearer it
// accepts, and 401 for any other. Every other path gets 200.
func fakeAnthropic(t *testing.T, accept *atomic.Value) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/messages") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+accept.Load().(string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"OAuth token has expired."}}`)
			return
		}
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		msg := map[string]any{"id": "msg_fake", "type": "message", "role": "assistant", "model": req.Model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}}
		if !req.Stream {
			msg["content"] = []any{map[string]string{"type": "text", "text": "ok"}}
			msg["stop_reason"] = "end_turn"
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(msg)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		events := []map[string]any{
			{"type": "message_start", "message": msg},
			{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}},
			{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": "ok"}},
			{"type": "content_block_stop", "index": 0},
			{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 1}},
			{"type": "message_stop"},
		}
		for _, e := range events {
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e["type"], b)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeHostCreds(t *testing.T, dir, tok string, exp time.Time) {
	t.Helper()
	body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"host-refresh","expiresAt":%d}}`, tok, exp.UnixMilli())
	path := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// accountProxy runs a bearer-swap proxy for src where a sandbox relay reaches it, as the
// supervisor does for an account, and returns the address the relay dials and the bearer it allows.
func accountProxy(t *testing.T, upstream *url.URL, src proxy.TokenSource) (addr, bearer string) {
	t.Helper()
	bindIP, gw, err := HostAddr(t.Context(), Docker)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", net.JoinHostPort(bindIP, "0"))
	if err != nil {
		t.Fatal(err)
	}
	p := proxy.New(upstream, src, zaptest.NewLogger(t, zaptest.Level(zap.WarnLevel)))
	bearer = NewBearer()
	p.Allow("test", bearer)
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return net.JoinHostPort(gw, strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)), bearer
}

// 001-AC25 001-AC26 001-AC27 003-AC14 against a fake upstream: a turn succeeds through an account
// proxy outside the sandbox, the sandbox cannot resolve or reach Anthropic, no trace of the host
// token is left in the container, its volumes or its environment, and the host credentials file
// is never written.
func TestSandboxDocker(t *testing.T) {
	img := dockerTest(t)
	ctx := context.Background()
	const hostTok = "sk-ant-oat01-HOSTTOKEN-must-never-enter-the-sandbox"
	var accept atomic.Value
	accept.Store(hostTok)
	up := fakeAnthropic(t, &accept)
	upURL, _ := url.Parse(up.URL)

	cfgDir := t.TempDir()
	writeHostCreds(t, cfgDir, hostTok, time.Now().Add(time.Hour))
	credsFile := filepath.Join(cfgDir, ".credentials.json")
	if err := os.Chmod(credsFile, 0o400); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(credsFile)

	name := fmt.Sprintf("test-%d", time.Now().UnixNano())
	addr, bearer := accountProxy(t, upURL, creds.NewSource(creds.Reader{ConfigDir: cfgDir}))
	s := New(Config{Image: img, Name: name, Proxy: addr}, Docker, zaptest.NewLogger(t, zaptest.Level(zap.WarnLevel)))
	t.Cleanup(func() { s.Close(context.Background()) })

	turn := func() (*Result, time.Duration, error) {
		tctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		start := time.Now()
		res, err := s.RunClaude(tctx, Request{Prompt: "say ok", Model: "haiku", Bearer: bearer})
		return res, time.Since(start), err
	}
	res, _, err := turn()
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if res.Result != "ok" || !sessionIDRe.MatchString(res.SessionID) {
		t.Fatalf("result = %+v", res)
	}
	if after, err := os.Stat(credsFile); err != nil || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("host credentials file changed: %v", err)
	}
	jsonl, err := s.ReadSession(ctx, res.SessionID, 0)
	if err != nil || !bytes.Contains(jsonl, []byte(res.SessionID)) {
		t.Fatalf("session: %d bytes, %v", len(jsonl), err)
	}
	tail, err := s.ReadSession(ctx, res.SessionID, int64(len(jsonl)-10))
	if err != nil || !bytes.Equal(tail, jsonl[len(jsonl)-10:]) {
		t.Fatalf("session tail = %q, %v", tail, err)
	}

	t.Run("no route but the proxy", func(t *testing.T) {
		// 002-AC35: the real claude binary ran its turn with HTTPS_PROXY set and sent nothing to
		// squid, before Isolated's own probe goes through it.
		if log, err := Docker(ctx, nil, "logs", s.egress()); err != nil || len(log) != 0 {
			t.Errorf("egress log after a claude turn: %q %v", log, err)
		}
		if err := s.Isolated(ctx); err != nil {
			t.Error(err)
		}
		// The DoD's literal check, with curl from the server image's builder (the sandbox has none).
		// Exit 6 = DNS failure, 7 = TCP connect failure, 28 = timeout.
		curl := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", s.network(), "--entrypoint", "curl",
			"golang:1.26-bookworm", "-sS", "-m", "5", "https://api.anthropic.com")
		out, err := curl.CombinedOutput()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || (ee.ExitCode() != 6 && ee.ExitCode() != 7 && ee.ExitCode() != 28) {
			t.Errorf("curl api.anthropic.com from the sandbox network: %v %s", err, out)
		}
		t.Logf("curl api.anthropic.com: %v: %s", err, bytes.TrimSpace(out))
	})

	t.Run("proxy refuses other Anthropic APIs", func(t *testing.T) {
		// Spike S16: the agent reads its dummy bearer, so the proxy must refuse it anything but model calls.
		out, err := Docker(ctx, nil, "exec", s.container(), "curl", "-s", "-o", "/dev/null", "-m", "10", "-w", "%{http_code}",
			"-H", "Authorization: Bearer "+bearer, "http://proxy:8787/v1/mcp_servers")
		if code := strings.TrimSpace(string(out)); err != nil || code != "403" {
			t.Errorf("GET /v1/mcp_servers from the sandbox: %q %v, want 403", code, err)
		}
	})

	t.Run("no host token in the container", func(t *testing.T) {
		export := exec.CommandContext(ctx, "docker", "export", s.container())
		fsTar, err := export.Output()
		if err != nil || len(fsTar) < 1<<20 {
			t.Fatalf("docker export: %d bytes, %v", len(fsTar), err)
		}
		vols, err := Docker(ctx, nil, "exec", s.container(), "tar", "-c", "-C", "/", "home/agent", "workspace")
		if err != nil {
			t.Fatal(err)
		}
		// The volumes do hold the session: the scan must see its content to be meaningful.
		if !bytes.Contains(vols, []byte(res.SessionID)) || !bytes.Contains(vols, []byte(bearer)) {
			t.Fatal("volume archive misses the session or the stubs")
		}
		inspect, err := Docker(ctx, nil, "inspect", s.container(), s.relay())
		if err != nil {
			t.Fatal(err)
		}
		for what, b := range map[string][]byte{"export": fsTar, "volumes": vols, "inspect": inspect} {
			for _, needle := range []string{hostTok, "host-refresh"} {
				if n := bytes.Count(b, []byte(needle)); n != 0 {
					t.Errorf("%s holds the host credential %d times", what, n)
				}
			}
		}
		// Scan, which `grill-room-daemon check` runs, must agree on what it sees.
		counts, err := s.Scan(ctx, []byte(bearer))
		if err != nil || counts["volumes"] == 0 || counts["filesystem"] != 0 || counts["inspect"] != 0 {
			t.Fatalf("Scan(dummy) = %v, %v", counts, err)
		}
		if !strings.Contains(string(inspect), "ANTHROPIC_BASE_URL=http://proxy:8787") {
			t.Error("inspect output lacks the sandbox env: the scan looked at the wrong thing")
		}
		listTar(t, vols)
	})

}

// listTar fails if the archive is unreadable: a scan over garbage proves nothing.
func listTar(t *testing.T, b []byte) {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(b))
	n := 0
	for {
		_, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		n++
	}
	if n == 0 {
		t.Fatal("empty archive")
	}
}

// TestRealClaude spends one real haiku request: GRILL_ROOM_TEST_REAL_CLAUDE=1 runs `claude -p
// --model haiku "say ok"` through the sandbox with this host's credentials (read-only), checks that
// nothing went through the egress proxy, then scans the container, its volumes and its inspect
// output for the real token. Only counts are printed.
func TestRealClaude(t *testing.T) {
	img := dockerTest(t)
	if os.Getenv("GRILL_ROOM_TEST_REAL_CLAUDE") != "1" {
		t.Skip("set GRILL_ROOM_TEST_REAL_CLAUDE=1 to spend one haiku request")
	}
	ctx := context.Background()
	r := creds.ForConfigDir(os.Getenv("CLAUDE_CONFIG_DIR"))
	tok, err := r.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var before os.FileInfo
	if tok.Origin == r.File() {
		if before, err = os.Stat(r.File()); err != nil {
			t.Fatal(err)
		}
	}
	upURL, _ := url.Parse("https://api.anthropic.com")
	addr, bearer := accountProxy(t, upURL, creds.NewSource(r))
	s := New(Config{Image: img, Name: fmt.Sprintf("real-%d", time.Now().UnixNano()), Proxy: addr}, Docker, zaptest.NewLogger(t, zaptest.Level(zap.WarnLevel)))
	t.Cleanup(func() { s.Close(context.Background()) })
	tctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	res, err := s.RunClaude(tctx, Request{Prompt: "say ok", Model: "haiku", Bearer: bearer})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	t.Logf("result=%q session=%s", res.Result, res.SessionID)
	// 002-AC35: the turn ran with the egress proxy in its environment, yet Claude Code sent nothing
	// through it: its base URL host is in NO_PROXY, and squid let no Anthropic host through.
	egressLog, err := Docker(ctx, nil, "logs", s.egress())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("egress log, %d bytes: %q", len(egressLog), egressLog)
	if n := anthropicTunnels(egressLog); n != 0 || bytes.Contains(egressLog, []byte(logTag)) {
		t.Errorf("Claude Code went through the egress proxy (%d Anthropic tunnels):\n%s", n, egressLog)
	}
	counts, err := s.Scan(ctx, []byte(tok.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	for what, n := range counts {
		t.Logf("%s: %d matches of the real token", what, n)
		if n != 0 {
			t.Errorf("%s holds the real token", what)
		}
	}
	if before != nil {
		after, err := os.Stat(r.File())
		if err != nil || !after.ModTime().Equal(before.ModTime()) {
			t.Errorf("host credentials file mtime changed: %v", err)
		}
	}
}
