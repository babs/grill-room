package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
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
			s := New(Config{Image: "img", Name: "t1", Host: "h", Upstream: up}, f.run, testCreds(t, "real-secret", time.Now().Add(time.Hour)), zap.NewNop())
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
	want := []string{"exec", "grill-room-t", "claude", "-p", "say ok", "--output-format", "json", "--model", "haiku", "--resume", "abc", "--allowedTools", "Read"}
	if c := f.find("exec", "grill-room-t", "claude"); !slices.Equal(c, want) {
		t.Fatalf("claude args = %v", c)
	}
	if len(f.stdin) != 1 || !strings.Contains(f.stdin[0], "profileFetchedAt") || !strings.Contains(f.stdin[0], s.dummy) || strings.Contains(f.stdin[0], "real-secret") {
		t.Fatalf("stubs tar wrong")
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
