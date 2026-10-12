package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
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

// testProxy and testBearer stand for the supervisor's account proxy (003-AC14).
const (
	testProxy  = "172.17.0.1:4321"
	testBearer = "sk-ant-oat01-grill-room-dummy-test"
)

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
			s := New(Config{Image: "img", Name: "t1", Proxy: testProxy, Limits: Limits{CPUs: "2", Memory: "4g", EgressCPUs: "0.5", EgressMemory: "256m"}},
				f.run, zap.NewNop())
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
			// 003-AC14: the relay dials the supervisor's account proxy it was given.
			relay := f.find("run", "-d", "--name", "grill-room-t1-relay")
			if !slices.Contains(relay, "host.docker.internal:172.17.0.1") || !slices.Contains(relay, "TCP:host.docker.internal:4321") {
				t.Errorf("relay = %v", relay)
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
			// 001-AC25: no argument of any docker call carries a bearer, dummy or not.
			for _, c := range f.calls {
				if strings.Contains(strings.Join(c, " "), "sk-ant") {
					t.Fatalf("bearer in docker args: %v", c)
				}
			}
		})
	}
}

func TestStartFailsWithoutDocker(t *testing.T) {
	f := &fakeDocker{reply: func([]string) ([]byte, error) { return nil, ErrNoDocker }}
	s := New(Config{Image: "img", Name: "t", Proxy: testProxy}, f.run, zap.NewNop())
	if err := s.Start(context.Background()); err == nil || err.Error() != "Docker is not installed" {
		t.Fatalf("err = %v", err)
	}
}

// 003-AC14: a sandbox with no proxy or no bearer runs nothing: the worker had no account access.
func TestRunClaudeWithoutAccess(t *testing.T) {
	for _, tc := range []struct {
		name, proxy, bearer, want string
	}{
		{"no bearer", testProxy, "", "no Claude account proxy for this sandbox"},
		{"no proxy", "", testBearer, "no Claude account proxy for this sandbox"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeDocker{reply: func(args []string) ([]byte, error) {
				if args[0] == "info" {
					return []byte("29|Linux"), nil
				}
				return nil, nil
			}}
			s := New(Config{Image: "img", Name: "t", Proxy: tc.proxy}, f.run, zap.NewNop())
			_, err := s.RunClaude(context.Background(), Request{Prompt: "say ok", Bearer: tc.bearer})
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) || f.find("run") != nil {
				t.Fatalf("err = %v, calls = %v", err, f.calls)
			}
		})
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
	s := New(Config{Image: "img", Name: "t", Proxy: testProxy}, f.run, zap.NewNop())
	defer s.Close(context.Background())
	res, err := s.RunClaude(context.Background(), Request{Prompt: "say ok", Model: "haiku", Resume: "abc", Args: []string{"--allowedTools", "Read"}, Bearer: testBearer})
	if err != nil || res.Result != "ok" || res.SessionID != "s1" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	want := []string{"exec", "-i", "grill-room-t", "claude", "-p", "--output-format", "json", "--model", "haiku", "--resume", "abc", "--allowedTools", "Read"}
	if c := f.find("exec", "-i", "grill-room-t", "claude"); !slices.Equal(c, want) {
		t.Fatalf("claude args = %v", c)
	}
	if len(f.stdin) != 2 || f.stdin[1] != "say ok" || !strings.Contains(f.stdin[0], "profileFetchedAt") || !strings.Contains(f.stdin[0], testBearer) {
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
	s := New(Config{Image: "img", Name: "t", Proxy: testProxy}, f.run, zap.NewNop())
	defer s.Close(context.Background())
	if _, err := s.RunClaude(context.Background(), Request{Prompt: "go", System: "You are Secretary.", Model: "sonnet", Effort: "xhigh", Bearer: testBearer}); err != nil {
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

// 003-AC24: a persona's autocompact reaches claude -p as --autocompact; 0 passes none, leaving
// Claude Code on auto.
func TestRunClaudeAutocompact(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want []string
	}{{0, nil}, {200_000, []string{"--autocompact", "200000"}}} {
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
		s := New(Config{Image: "img", Name: "t", Proxy: testProxy}, f.run, zap.NewNop())
		if _, err := s.RunClaude(context.Background(), Request{Prompt: "go", Autocompact: tc.n, Bearer: testBearer}); err != nil {
			t.Fatal(err)
		}
		s.Close(context.Background())
		c := f.find("exec", "-i", "grill-room-t", "claude")
		i := slices.Index(c, "--autocompact")
		if (tc.want == nil) != (i < 0) || (i >= 0 && !slices.Equal(c[i:i+2], tc.want)) {
			t.Fatalf("autocompact %d: claude args = %v", tc.n, c)
		}
	}
}

// 003-AC24: the context a session holds is its last request's input, cache read and cache creation
// tokens, against the window claude reports for the model that answered; unknown without them.
func TestResultContext(t *testing.T) {
	for _, tc := range []struct {
		in             string
		tokens, window int64
	}{
		{`{}`, 0, 0},
		{`{"usage": {"input_tokens": 2, "cache_read_input_tokens": 24948, "cache_creation_input_tokens": 50, "output_tokens": 11},
			"modelUsage": {"claude-haiku-5-5": {"outputTokens": 16, "contextWindow": 1000000, "cacheReadInputTokens": 46239}}}`, 25000, 1000000},
		{`{"usage": {"input_tokens": 10}, "modelUsage": {"claude-side": {"outputTokens": 3, "contextWindow": 200000},
			"claude-opus-5": {"outputTokens": 900, "contextWindow": 1000000}}}`, 10, 1000000},
		{`{"usage": {"input_tokens": 10}, "modelUsage": {"claude-opus-5": {"outputTokens": 900}}}`, 10, 0},
	} {
		var r Result
		if err := json.Unmarshal([]byte(tc.in), &r); err != nil {
			t.Fatal(err)
		}
		if tokens, window := r.Context(); tokens != tc.tokens || window != tc.window {
			t.Errorf("%s: Context() = %d, %d, want %d, %d", tc.in, tokens, window, tc.tokens, tc.window)
		}
	}
}

func TestReadSessionValidates(t *testing.T) {
	f := &fakeDocker{}
	s := New(Config{Name: "t", Proxy: testProxy}, f.run, zap.NewNop())
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
	s := New(Config{Image: "img", Name: "t", Proxy: testProxy}, run, zap.NewNop())
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
func fakeSandbox(t *testing.T, claude func(ctx context.Context) ([]byte, error)) (*Sandbox, *fakeDocker) {
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
	s := New(Config{Image: "img", Name: "t", Proxy: testProxy}, run, zap.NewNop())
	t.Cleanup(func() { s.Close(context.Background()) })
	return s, f
}

// A failed claude run keeps its own message.
func TestRunClaudeFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		out     string
		err     error
		want    string
		wantRes bool
	}{
		{"exit", `garbage`, errors.New("exit status 2"), "claude: exit status 2", false},
		{"not json", `garbage`, nil, "claude output:", false},
		{"is_error", `{"is_error":true,"subtype":"error_max_turns","result":"stopped"}`, nil, "claude: error_max_turns: stopped", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := fakeSandbox(t, func(context.Context) ([]byte, error) { return []byte(tc.out), tc.err })
			res, err := s.RunClaude(context.Background(), Request{Prompt: "p", Bearer: testBearer})
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
	s, f := fakeSandbox(t, func(context.Context) ([]byte, error) {
		cancel()
		return nil, context.Canceled
	})
	if _, err := s.RunClaude(ctx, Request{Prompt: "p", Bearer: testBearer}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if f.find("exec", "grill-room-t", "pkill", "-x", "claude") == nil {
		t.Fatalf("claude not killed: %v", f.calls)
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
	s := New(Config{Name: "t", Proxy: testProxy}, f.run, zap.NewNop())
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

// 003-AC14: an account proxy listens where the relays reach it: the host's loopback behind Docker
// Desktop's host-gateway, the bridge gateway on a native engine; nowhere without Docker.
func TestHostAddr(t *testing.T) {
	for _, tc := range []struct {
		name, info, bridge     string
		err                    error
		bind, gateway, wantErr string
	}{
		{"desktop", "28|Docker Desktop", "", nil, "127.0.0.1", "host-gateway", ""},
		{"native", "29|Linux", "172.17.0.1\n", nil, "172.17.0.1", "172.17.0.1", ""},
		{"bad bridge", "29|Linux", "nope", nil, "", "", "is not an IP"},
		{"no docker", "", "", ErrNoDocker, "", "", "Docker is not installed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeDocker{reply: func(args []string) ([]byte, error) {
				if args[0] == "info" {
					return []byte(tc.info), tc.err
				}
				return []byte(tc.bridge), nil
			}}
			bind, gw, err := HostAddr(context.Background(), f.run)
			if bind != tc.bind || gw != tc.gateway || (err == nil) != (tc.wantErr == "") || (err != nil && !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("HostAddr = %q %q %v", bind, gw, err)
			}
		})
	}
}

// 003-AC8: RemoveLabelled removes the containers, then the networks and volumes, carrying every
// label given, goes on past a failing kind, and refuses to run with no label at all.
func TestRemoveLabelled(t *testing.T) {
	f := &fakeDocker{reply: func(args []string) ([]byte, error) {
		switch strings.Join(args[:2], " ") {
		case "container ls":
			return []byte("c1\nc2\n"), nil
		case "network ls":
			return nil, errors.New("daemon busy")
		case "volume ls":
			return []byte("v1\n"), nil
		}
		return nil, nil
	}}
	n, err := RemoveLabelled(context.Background(), f.run, "a=1", "b=2")
	if n != 3 || err == nil || !strings.Contains(err.Error(), "daemon busy") {
		t.Fatalf("removed %d, %v", n, err)
	}
	if got := f.find("container", "ls"); !slices.Equal(got, []string{"container", "ls", "-aq", "--filter", "label=a=1", "--filter", "label=b=2"}) {
		t.Fatalf("container list %q", got)
	}
	if f.find("container", "rm", "-f", "-v", "c1", "c2") == nil || f.find("volume", "rm", "-f", "v1") == nil || f.find("network", "rm") != nil {
		t.Fatalf("calls %q", f.calls)
	}
	if _, err := RemoveLabelled(context.Background(), f.run); err == nil {
		t.Fatal("no label would match every object")
	}
}

// Config.Labels mark every object a sandbox creates.
func TestSandboxLabels(t *testing.T) {
	f := &fakeDocker{reply: func(args []string) ([]byte, error) {
		if args[0] == "info" {
			return []byte("29.7.2|Ubuntu\n"), nil
		}
		return nil, nil
	}}
	s := New(Config{Image: "img", Name: "n", Proxy: "172.17.0.1:9", Labels: []string{"grill-room-root=r", "grill-room-instance=i"}}, f.run, zap.NewNop())
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	created := 0
	for _, c := range f.calls {
		if c[0] == "run" || slices.Equal(c[:2], []string{"network", "create"}) || slices.Equal(c[:2], []string{"volume", "create"}) {
			created++
			j := strings.Join(c, " ")
			if !strings.Contains(j, "--label "+Label+"=n") || !strings.Contains(j, "--label grill-room-root=r") || !strings.Contains(j, "--label grill-room-instance=i") {
				t.Fatalf("unlabelled: %q", c)
			}
		}
	}
	if created != 7 {
		t.Fatalf("%d objects created", created)
	}
}

// 003-AC27: a turn's tokens sum every model claude called, and its cost is claude's
// total_cost_usd, unknown when claude reports none (a zero cost stays known).
func TestResultTokensAndCost(t *testing.T) {
	for _, tc := range []struct {
		in                    string
		tin, tout, read, writ int64
		cost                  string
	}{
		{`{}`, 0, 0, 0, 0, "unknown"},
		{`{"total_cost_usd": 0.00093391, "modelUsage": {"claude-haiku-5-5": {"inputTokens": 2, "outputTokens": 4,
			"cacheReadInputTokens": 21291, "cacheCreationInputTokens": 3594, "costUSD": 0.00093391}}}`, 2, 4, 21291, 3594, "0.00093391"},
		{`{"total_cost_usd": 0, "modelUsage": {"a": {"inputTokens": 1, "outputTokens": 2, "cacheReadInputTokens": 3, "cacheCreationInputTokens": 4},
			"b": {"inputTokens": 10, "outputTokens": 20, "cacheReadInputTokens": 30, "cacheCreationInputTokens": 40}}}`, 11, 22, 33, 44, "0"},
	} {
		var r Result
		if err := json.Unmarshal([]byte(tc.in), &r); err != nil {
			t.Fatal(err)
		}
		in, out, read, write := r.Tokens()
		cost := "unknown"
		if r.TotalCostUSD != nil {
			cost = strconv.FormatFloat(*r.TotalCostUSD, 'f', -1, 64)
		}
		if in != tc.tin || out != tc.tout || read != tc.read || write != tc.writ || cost != tc.cost {
			t.Errorf("%s: Tokens() = %d %d %d %d, cost %s", tc.in, in, out, read, write, cost)
		}
	}
}

// 003-AC26: limits the page sends become docker arguments: only docker's own syntax passes.
func TestLimitsValidate(t *testing.T) {
	for _, l := range []Limits{{}, {CPUs: "2", Memory: "4g", EgressCPUs: "0.5", EgressMemory: "256m"}, {CPUs: "0", Memory: "0"}, {Memory: "512M"}} {
		if err := l.Validate(); err != nil {
			t.Errorf("%+v: %v", l, err)
		}
	}
	for _, l := range []Limits{{CPUs: "two"}, {CPUs: "-1"}, {CPUs: "1.5.2"}, {Memory: "4 GB"}, {Memory: "4t"}, {EgressCPUs: "--privileged"},
		{EgressMemory: "1g --network host"}} {
		if err := l.Validate(); err == nil {
			t.Errorf("%+v accepted", l)
		}
	}
}
