package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/creds"
	"github.com/babs/grill-room/internal/proxy"
)

// fakeDocker records calls and answers from a table keyed by the first args.
type fakeDocker struct {
	mu    sync.Mutex
	calls [][]string
	stdin []string
	reply func(args []string) ([]byte, error)
}

func (f *fakeDocker) run(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, args)
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		f.stdin = append(f.stdin, string(b))
	}
	f.mu.Unlock()
	if f.reply != nil {
		return f.reply(args)
	}
	return nil, nil
}

func (f *fakeDocker) find(prefix ...string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if len(c) >= len(prefix) && slices.Equal(c[:len(prefix)], prefix) {
			return c
		}
	}
	return nil
}

// 001-AC28: a missing or stopped Docker is reported as such, so the daemon does not offer to host.
func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		name    string
		out     string
		err     error
		ok      bool
		desktop bool
		reason  string
	}{
		{"native", "29.7.2|Ubuntu 26.04 LTS\n", nil, true, false, ""},
		{"desktop", "28.1.0|Docker Desktop\n", nil, true, true, ""},
		{"not installed", "", ErrNoDocker, false, false, "Docker is not installed"},
		{"not running", "", errors.New("Cannot connect to the Docker daemon"), false, false, "Docker is not running: Cannot connect"},
		{"empty answer", "|\n", nil, false, false, "Docker is not running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Check(context.Background(), func(context.Context, io.Reader, ...string) ([]byte, error) { return []byte(tc.out), tc.err })
			if r.OK != tc.ok || r.Desktop != tc.desktop || !strings.HasPrefix(r.Reason, tc.reason) {
				t.Fatalf("got %+v", r)
			}
		})
	}
}

func testCreds(t *testing.T, tok string, exp time.Time) *creds.Source {
	t.Helper()
	dir := t.TempDir()
	body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":%d}}`, tok, exp.UnixMilli())
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return creds.NewSource(creds.Reader{ConfigDir: dir})
}

func TestStartArgs(t *testing.T) {
	for _, desktop := range []bool{false, true} {
		t.Run(fmt.Sprint("desktop=", desktop), func(t *testing.T) {
			f := &fakeDocker{reply: func(args []string) ([]byte, error) {
				switch {
				case args[0] == "info" && desktop:
					return []byte("28|Docker Desktop"), nil
				case args[0] == "info":
					return []byte("29|Linux"), nil
				case args[0] == "network" && args[1] == "inspect":
					return []byte("127.0.0.1\n"), nil
				}
				return nil, nil
			}}
			up, _ := url.Parse("https://api.anthropic.com")
			s := New(Config{Image: "img", Name: "t1", Host: "h", Upstream: up, Limits: Limits{CPUs: "2", Memory: "4g", EgressCPUs: "0.5", EgressMemory: "256m"}},
				f.run, testCreds(t, "real-secret", time.Now().Add(time.Hour)), zap.NewNop())
			if err := s.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer s.Close(context.Background())

			// 001-AC26: the sandbox sits on an internal network only.
			if c := f.find("network", "create"); !slices.Contains(c, "--internal") {
				t.Fatalf("network create = %v", c)
			}
			run := f.find("run", "-d", "--init")
			for _, want := range []string{"--network", "grill-room-t1", "--cap-drop", "ALL", "--read-only", "no-new-privileges",
				"ANTHROPIC_BASE_URL=http://proxy:8787", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "ENABLE_CLAUDEAI_MCP_SERVERS=false"} {
				if !slices.Contains(run, want) {
					t.Errorf("sandbox run lacks %q: %v", want, run)
				}
			}
			relay := f.find("run", "-d", "--name", "grill-room-t1-relay")
			wantGW := "host.docker.internal:127.0.0.1"
			if desktop {
				wantGW = "host.docker.internal:host-gateway"
			}
			if !slices.Contains(relay, wantGW) {
				t.Errorf("relay = %v, want %s", relay, wantGW)
			}
			// 002-AC34 002-AC35: tools reach squid as "egress", which alone has a route out; Claude
			// Code's base URL host stays off the proxy. Both containers are capped.
			if c := f.find("network", "create", "--label", Label+"=t1", "grill-room-t1-egress"); c == nil {
				t.Errorf("no egress network: %v", f.calls)
			}
			if c := f.find("network", "connect", "--alias", "egress", "grill-room-t1", "grill-room-t1-egress"); c == nil {
				t.Error("squid is not on the sandbox network as egress")
			}
			egress := f.find("run", "-d", "--name", "grill-room-t1-egress")
			for _, want := range []string{"--network", "grill-room-t1-egress", "--read-only", "--cpus", "0.5", "--memory", "256m", "local"} {
				if !slices.Contains(egress, want) {
					t.Errorf("egress run lacks %q: %v", want, egress)
				}
			}
			for _, want := range []string{"HTTPS_PROXY=http://egress:3128", "https_proxy=http://egress:3128", "HTTP_PROXY=http://egress:3128",
				"http_proxy=http://egress:3128", "NO_PROXY=proxy,localhost,127.0.0.1", "no_proxy=proxy,localhost,127.0.0.1", "4g", "--memory-swap"} {
				if !slices.Contains(run, want) {
					t.Errorf("sandbox run lacks %q: %v", want, run)
				}
			}
			// 001-AC25: no argument of any docker call carries the real token.
			for _, c := range f.calls {
				if strings.Contains(strings.Join(c, " "), "real-secret") {
					t.Fatalf("token in docker args: %v", c)
				}
			}
		})
	}
}

func TestStartFailsWithoutDocker(t *testing.T) {
	f := &fakeDocker{reply: func([]string) ([]byte, error) { return nil, ErrNoDocker }}
	up, _ := url.Parse("https://x")
	s := New(Config{Image: "img", Name: "t", Upstream: up}, f.run, testCreds(t, "x", time.Now().Add(time.Hour)), zap.NewNop())
	if err := s.Start(context.Background()); err == nil || err.Error() != "Docker is not installed" {
		t.Fatalf("err = %v", err)
	}
}

// 001-AC29: an expired host token fails the turn at once, before any container work.
func TestRunClaudeExpiredToken(t *testing.T) {
	f := &fakeDocker{}
	up, _ := url.Parse("https://x")
	s := New(Config{Image: "img", Name: "t", Host: "laptop", Upstream: up}, f.run, testCreds(t, "x", time.Now().Add(-time.Minute)), zap.NewNop())
	_, err := s.RunClaude(context.Background(), Request{Prompt: "say ok"})
	if !errors.Is(err, ErrCredentialsExpired) || err.Error() != "Claude credentials expired on laptop" {
		t.Fatalf("err = %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("docker called: %v", f.calls)
	}
}

func TestRunClaudeArgsAndStubs(t *testing.T) {
	f := &fakeDocker{reply: func(args []string) ([]byte, error) {
		switch {
		case args[0] == "info":
			return []byte("29|Linux"), nil
		case args[0] == "network" && args[1] == "inspect":
			return []byte("127.0.0.1"), nil
		case args[0] == "exec" && slices.Contains(args, "claude"):
			return []byte(`{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"s1"}`), nil
		}
		return nil, nil
	}}
	up, _ := url.Parse("https://x")
	s := New(Config{Image: "img", Name: "t", Upstream: up}, f.run, testCreds(t, "real-secret", time.Now().Add(time.Hour)), zap.NewNop())
	defer s.Close(context.Background())
	res, err := s.RunClaude(context.Background(), Request{Prompt: "say ok", Model: "haiku", Resume: "abc", Args: []string{"--allowedTools", "Read"}})
	if err != nil || res.Result != "ok" || res.SessionID != "s1" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	want := []string{"exec", "-i", "grill-room-t", "claude", "-p", "--output-format", "json", "--model", "haiku", "--resume", "abc", "--allowedTools", "Read"}
	if c := f.find("exec", "-i", "grill-room-t", "claude"); !slices.Equal(c, want) {
		t.Fatalf("claude args = %v", c)
	}
	if len(f.stdin) != 2 || f.stdin[1] != "say ok" || !strings.Contains(f.stdin[0], "profileFetchedAt") || !strings.Contains(f.stdin[0], s.dummy) || strings.Contains(f.stdin[0], "real-secret") {
		t.Fatalf("stubs tar wrong")
	}
}

// 002-AC18: a persona's turn passes its effort, and its text as an appended system prompt file
// written in the sandbox home, with the snapshot off so a resumed session gets the current text.
func TestRunClaudePersona(t *testing.T) {
	f := &fakeDocker{reply: func(args []string) ([]byte, error) {
		switch {
		case args[0] == "info":
			return []byte("29|Linux"), nil
		case args[0] == "network" && args[1] == "inspect":
			return []byte("127.0.0.1"), nil
		case args[0] == "exec" && slices.Contains(args, "claude"):
			return []byte(`{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"s1"}`), nil
		}
		return nil, nil
	}}
	up, _ := url.Parse("https://x")
	s := New(Config{Image: "img", Name: "t", Upstream: up}, f.run, testCreds(t, "real-secret", time.Now().Add(time.Hour)), zap.NewNop())
	defer s.Close(context.Background())
	if _, err := s.RunClaude(context.Background(), Request{Prompt: "go", System: "You are Secretary.", Model: "sonnet", Effort: "xhigh"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "-i", "grill-room-t", "claude", "-p", "--output-format", "json", "--model", "sonnet", "--effort", "xhigh",
		"--append-system-prompt-file", "/home/agent/.grill-room/persona.md", "--system-prompt-snapshot", "off"}
	if c := f.find("exec", "-i", "grill-room-t", "claude"); !slices.Equal(c, want) {
		t.Fatalf("claude args = %v", c)
	}
	if len(f.stdin) != 3 || !strings.Contains(f.stdin[1], ".grill-room/persona.md") || !strings.Contains(f.stdin[1], "You are Secretary.") || f.stdin[2] != "go" {
		t.Fatalf("persona file not written before the run: %q", f.stdin)
	}
}

func TestReadSessionValidates(t *testing.T) {
	f := &fakeDocker{}
	s := New(Config{Name: "t", Upstream: &url.URL{}}, f.run, nil, zap.NewNop())
	for _, id := range []string{"", "../../etc/passwd", "60DC4912-36EC-4225-A44A-6A6C1ED85B1B"} {
		if _, err := s.ReadSession(context.Background(), id, 0); err == nil {
			t.Errorf("id %q accepted", id)
		}
	}
	if _, err := s.ReadSession(context.Background(), "60dc4912-36ec-4225-a44a-6a6c1ed85b1b", 128); err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "grill-room-t", "tail", "-c", "+129", "/home/agent/.claude/projects/-workspace/60dc4912-36ec-4225-a44a-6a6c1ed85b1b.jsonl"}
	if !slices.Equal(f.calls[0], want) {
		t.Fatalf("args = %v", f.calls[0])
	}
}

func TestFailedStartCleansUpWithExpiredContext(t *testing.T) {
	f := &fakeDocker{reply: func(args []string) ([]byte, error) {
		switch args[0] {
		case "info":
			return []byte("29|Linux"), nil
		case "network":
			if args[1] == "inspect" {
				return []byte("127.0.0.1"), nil
			}
		case "run":
			return nil, errors.New("boom")
		}
		return nil, nil
	}}
	var cleanupCtxErr error
	run := func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		if args[0] == "rm" {
			cleanupCtxErr = ctx.Err()
		}
		return f.run(ctx, stdin, args...)
	}
	up, _ := url.Parse("https://x")
	s := New(Config{Image: "img", Name: "t", Upstream: up}, run, testCreds(t, "x", time.Now().Add(time.Hour)), zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	f.reply = func(next func([]string) ([]byte, error)) func([]string) ([]byte, error) {
		return func(args []string) ([]byte, error) {
			if args[0] == "run" {
				cancel() // the turn's deadline passes while the relay starts
			}
			return next(args)
		}
	}(f.reply)
	if err := s.Start(ctx); err == nil {
		t.Fatal("Start succeeded")
	}
	if cleanupCtxErr != nil {
		t.Fatalf("cleanup ran with a dead context: %v", cleanupCtxErr)
	}
}

// fakeSandbox answers info and the bridge inspect, and claude with reply.
func fakeSandbox(t *testing.T, tok string, exp time.Time, claude func(ctx context.Context) ([]byte, error)) (*Sandbox, *fakeDocker) {
	t.Helper()
	f := &fakeDocker{}
	f.reply = func(args []string) ([]byte, error) {
		switch {
		case args[0] == "info":
			return []byte("29|Linux"), nil
		case args[0] == "network" && args[1] == "inspect":
			return []byte("127.0.0.1"), nil
		}
		return nil, nil
	}
	run := func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		if args[0] == "exec" && slices.Contains(args, "claude") && claude != nil {
			f.run(ctx, stdin, args...)
			return claude(ctx)
		}
		return f.run(ctx, stdin, args...)
	}
	up, _ := url.Parse("https://x")
	s := New(Config{Image: "img", Name: "t", Host: "laptop", Upstream: up}, run, testCreds(t, tok, exp), zap.NewNop())
	t.Cleanup(func() { s.Close(context.Background()) })
	return s, f
}

// 001-AC29: a turn that fails after the proxy saw an upstream 401 reports expired credentials;
// other failures keep their own message.
func TestRunClaudeFailures(t *testing.T) {
	future := time.Now().Add(time.Hour)
	for _, tc := range []struct {
		name    string
		out     string
		err     error
		saw401  bool
		want    string
		wantRes bool
	}{
		{"401 then exit 1", `{"is_error":true,"result":"OAuth token invalid"}`, errors.New("exit status 1"), true, "Claude credentials expired on laptop", false},
		{"401 then is_error", `{"is_error":true,"subtype":"success","result":"API Error: 401"}`, nil, true, "Claude credentials expired on laptop", false},
		{"exit without 401", `garbage`, errors.New("exit status 2"), false, "claude: exit status 2", false},
		{"not json", `garbage`, nil, false, "claude output:", false},
		{"is_error without 401", `{"is_error":true,"subtype":"error_max_turns","result":"stopped"}`, nil, false, "claude: error_max_turns: stopped", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := fakeSandbox(t, "real", future, func(context.Context) ([]byte, error) { return []byte(tc.out), tc.err })
			// The turn's request goes through a real proxy to an upstream answering 401.
			if tc.saw401 {
				up401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
				defer up401.Close()
				upURL, _ := url.Parse(up401.URL)
				s.proxy = proxy.New(upURL, s.src, s.dummy, zap.NewNop())
				inner := s.run
				s.run = func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
					if args[0] == "exec" && slices.Contains(args, "claude") {
						req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/messages", http.NoBody)
						req.Header.Set("Authorization", "Bearer "+s.dummy)
						s.proxy.ServeHTTP(httptest.NewRecorder(), req)
					}
					return inner(ctx, stdin, args...)
				}
			}
			res, err := s.RunClaude(context.Background(), Request{Prompt: "p"})
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if (res != nil) != tc.wantRes {
				t.Fatalf("res = %+v", res)
			}
		})
	}
}

func TestRunClaudeCancelKillsClaude(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s, f := fakeSandbox(t, "real", time.Now().Add(time.Hour), func(context.Context) ([]byte, error) {
		cancel()
		return nil, context.Canceled
	})
	if _, err := s.RunClaude(ctx, Request{Prompt: "p"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if f.find("exec", "grill-room-t", "pkill", "-x", "claude") == nil {
		t.Fatalf("claude not killed: %v", f.calls)
	}
}

func TestRunClaudeUnreadableCreds(t *testing.T) {
	f := &fakeDocker{}
	s := New(Config{Name: "t", Host: "laptop", Upstream: &url.URL{}}, f.run, creds.NewSource(creds.Reader{ConfigDir: t.TempDir()}), zap.NewNop())
	_, err := s.RunClaude(context.Background(), Request{Prompt: "p"})
	if err == nil || !strings.HasPrefix(err.Error(), "Claude credentials unavailable on laptop") || len(f.calls) != 0 {
		t.Fatalf("err = %v, calls = %v", err, f.calls)
	}
}

func TestScanAndIsolated(t *testing.T) {
	secret := []byte("tok-123")
	replies := map[string][]byte{"export": []byte("a tok-123 b tok-123"), "tar": []byte("x"), "inspect": []byte("tok-123")}
	reachable := map[string]bool{"proxy/8787": true}
	resolves := false
	// egressCode is what squid answers a CONNECT to api.anthropic.com; "000" is no answer.
	egressCode := "403"
	f := &fakeDocker{reply: func(args []string) ([]byte, error) {
		switch {
		case slices.Contains(args, "curl"):
			if egressCode != "403" {
				return []byte(egressCode), errors.New("exit status 7")
			}
			return []byte(egressCode), errors.New("exit status 56")
		case args[0] == "export":
			return replies["export"], nil
		case args[0] == "inspect":
			return replies["inspect"], nil
		case slices.Contains(args, "tar"):
			return replies["tar"], nil
		case slices.Contains(args, "getent"):
			if resolves {
				return []byte("1.2.3.4 api.anthropic.com"), nil
			}
			return nil, errors.New("exit status 2")
		case slices.Contains(args, "bash"):
			target := strings.TrimPrefix(args[len(args)-1], "exec 3<>/dev/tcp/")
			if reachable[target] {
				return nil, nil
			}
			return nil, errors.New("exit status 1")
		}
		return nil, nil
	}}
	s := New(Config{Name: "t", Upstream: &url.URL{}}, f.run, nil, zap.NewNop())
	counts, err := s.Scan(context.Background(), secret)
	if err != nil || counts["filesystem"] != 2 || counts["volumes"] != 0 || counts["inspect"] != 1 {
		t.Fatalf("Scan = %v, %v", counts, err)
	}
	if _, err := s.Scan(context.Background(), nil); err == nil {
		t.Fatal("empty secret accepted")
	}
	if err := s.Isolated(context.Background()); err != nil {
		t.Fatalf("isolated sandbox: %v", err)
	}
	resolves = true
	if err := s.Isolated(context.Background()); err == nil || !strings.Contains(err.Error(), "resolves") {
		t.Fatalf("resolving: %v", err)
	}
	resolves = false
	reachable["1.1.1.1/443"] = true
	if err := s.Isolated(context.Background()); err == nil || !strings.Contains(err.Error(), "1.1.1.1") {
		t.Fatalf("public IP reachable: %v", err)
	}
	// 002-AC35: squid down, or letting Anthropic through, fails the check.
	delete(reachable, "1.1.1.1/443")
	for _, code := range []string{"000", "200", ""} {
		egressCode = code
		if err := s.Isolated(context.Background()); err == nil || !strings.Contains(err.Error(), "egress proxy answered") {
			t.Fatalf("egress answered %q: %v", code, err)
		}
	}
	reachable = map[string]bool{}
	if err := s.Isolated(context.Background()); err == nil || !strings.Contains(err.Error(), "proxy is unreachable") {
		t.Fatalf("proxy down: %v", err)
	}
}

// The model a turn ran on is the one that wrote most of the answer: Claude Code calls a small model
// on the side, and reports none when the run failed early.
func TestResultModel(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{}`, ""},
		{`{"modelUsage": {}}`, ""},
		{`{"modelUsage": {"claude-opus-5": {"outputTokens": 812}}}`, "claude-opus-5"},
		{`{"modelUsage": {"claude-haiku-5-5": {"outputTokens": 40}, "claude-opus-5": {"outputTokens": 812}}}`, "claude-opus-5"},
		{`{"modelUsage": {"b": {"outputTokens": 0}, "a": {"outputTokens": 0}}}`, "a"},
	} {
		var r Result
		if err := json.Unmarshal([]byte(tc.in), &r); err != nil {
			t.Fatal(err)
		}
		if got := r.Model(); got != tc.want {
			t.Errorf("%s: Model() = %q, want %q", tc.in, got, tc.want)
		}
	}
}
