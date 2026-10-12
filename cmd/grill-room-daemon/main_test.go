package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/daemon"
	"github.com/babs/grill-room/internal/sandbox"
)

func TestConfig(t *testing.T) {
	t.Setenv("GRILL_ROOM_DAEMON_STATE_DIR", "/state")
	t.Setenv("GRILL_ROOM_DAEMON_BROWSER", "")
	t.Setenv("GRILL_ROOM_SANDBOX_IMAGE", "")
	cfg, err := loadConfig()
	if err != nil || cfg.Paths != (daemon.Paths{Config: "/state", State: "/state", Cache: "/state"}) || cfg.Legacy != "/state" || cfg.Browser != "auto" || cfg.Image != "" || cfg.CacheMB != 1024 ||
		cfg.Limits != (sandbox.Limits{CPUs: "2", Memory: "4g", EgressCPUs: "0.5", EgressMemory: "256m"}) {
		t.Fatalf("cfg = %+v, %v", cfg, err)
	}
	// 002-AC37: the workspace cache cap is a size in MiB, 0 to disable it.
	t.Setenv("GRILL_ROOM_DAEMON_CACHE_MB", "0")
	if cfg, err := loadConfig(); err != nil || cfg.CacheMB != 0 {
		t.Fatalf("cache disabled: %+v %v", cfg, err)
	}
	for _, bad := range []string{"-1", "1g", "lots"} {
		t.Setenv("GRILL_ROOM_DAEMON_CACHE_MB", bad)
		if _, err := loadConfig(); err == nil {
			t.Fatalf("cache %q accepted", bad)
		}
	}
	t.Setenv("GRILL_ROOM_DAEMON_CACHE_MB", "")
	for _, tc := range []struct {
		cfg Config
		ok  bool
	}{
		{Config{Browser: "auto", LogLevel: "info"}, true},
		{Config{Browser: "none", LogLevel: "debug"}, true},
		{Config{Browser: "firefox", LogLevel: "info"}, false},
		{Config{Browser: "auto", LogLevel: "loud"}, false},
		// 002-AC19: a cap names a known model family and effort.
		{Config{Browser: "auto", LogLevel: "info", Settings: daemon.Settings{MaxModel: "claude-sonnet-5-5", MaxEffort: "high"}}, true},
		{Config{Browser: "auto", LogLevel: "info", Settings: daemon.Settings{MaxModel: "gpt-5"}}, false},
		{Config{Browser: "auto", LogLevel: "info", Settings: daemon.Settings{MaxEffort: "turbo"}}, false},
		// Sandbox and egress limits are docker --cpus and --memory values, 0 for none.
		{Config{Browser: "auto", LogLevel: "info", Limits: sandbox.Limits{CPUs: "1.5", Memory: "4g", EgressCPUs: "0", EgressMemory: "512M"}}, true},
		{Config{Browser: "auto", LogLevel: "info", Limits: sandbox.Limits{CPUs: "two"}}, false},
		{Config{Browser: "auto", LogLevel: "info", Limits: sandbox.Limits{EgressCPUs: "-1"}}, false},
		{Config{Browser: "auto", LogLevel: "info", Limits: sandbox.Limits{Memory: "4 GB"}}, false},
		{Config{Browser: "auto", LogLevel: "info", Limits: sandbox.Limits{EgressMemory: "--privileged"}}, false},
		// 003-AC5: the GitHub update channel needs a repository URL.
		{Config{Browser: "auto", LogLevel: "info", GitHub: "http://127.0.0.1:8080/repo"}, true},
		{Config{Browser: "auto", LogLevel: "info", GitHub: "ftp://github.com/babs/grill-room"}, false},
		{Config{Browser: "auto", LogLevel: "info", GitHub: "github.com/babs/grill-room"}, false},
	} {
		if tc.cfg.GitHub == "" {
			tc.cfg.GitHub = "https://github.com/babs/grill-room"
		}
		if err := tc.cfg.Validate(); (err == nil) != tc.ok {
			t.Errorf("Validate(%+v) = %v", tc.cfg, err)
		}
	}
	t.Setenv("GRILL_ROOM_MAX_MODEL", "opus")
	t.Setenv("GRILL_ROOM_MAX_EFFORT", "xhigh")
	t.Setenv("GRILL_ROOM_APPROVE_ALL", "true")
	if cfg, err := loadConfig(); err != nil || cfg.Settings != (daemon.Settings{MaxModel: "opus", MaxEffort: "xhigh", ApproveAll: true}) {
		t.Fatalf("settings from the environment: %+v %v", cfg.Settings, err)
	}
	t.Setenv("GRILL_ROOM_APPROVE_ALL", "sometimes")
	if _, err := loadConfig(); err == nil {
		t.Fatal("a bad GRILL_ROOM_APPROVE_ALL loaded")
	}
}

// syncBuf is a stderr the daemon writes from several goroutines.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestRunErrors(t *testing.T) {
	t.Setenv("GRILL_ROOM_DAEMON_STATE_DIR", t.TempDir())
	for _, tc := range []struct {
		args []string
		env  map[string]string
		want string
	}{
		{[]string{"-nope"}, nil, "flag provided but not defined"},
		{[]string{"http://rooms.example/r/abc"}, nil, "https"},
		{nil, map[string]string{"LOG_LEVEL": "loud"}, "LOG_LEVEL"},
		// No docker CLI on PATH: check stops before reading credentials.
		{[]string{"check"}, map[string]string{"PATH": t.TempDir()}, "Docker is not installed"},
	} {
		for k, v := range tc.env {
			t.Setenv(k, v)
		}
		err := run(context.Background(), tc.args, &syncBuf{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("run(%v) = %v, want %q", tc.args, err, tc.want)
		}
		for k := range tc.env {
			t.Setenv(k, "")
		}
	}
}

// The binary starts its admin page on 127.0.0.1, prints a one-time link, serves the page's
// status, and returns when its context ends.
func TestRunServesAdminPage(t *testing.T) {
	t.Setenv("GRILL_ROOM_DAEMON_STATE_DIR", t.TempDir())
	t.Setenv("GRILL_ROOM_DAEMON_BROWSER", "none")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	stderr := &syncBuf{}
	done := make(chan error, 1)
	go func() { done <- run(ctx, nil, stderr) }()
	re := regexp.MustCompile(`Admin page: (http://127\.0\.0\.1:\d+)/s/(\S+)`)
	var m []string
	deadline := time.Now().Add(5 * time.Second)
	for m = re.FindStringSubmatch(stderr.String()); m == nil; m = re.FindStringSubmatch(stderr.String()) {
		if time.Now().After(deadline) {
			t.Fatalf("no admin link in: %s", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	req, _ := http.NewRequestWithContext(t.Context(), "POST", m[1]+"/api/session", strings.NewReader(`{"code":"`+m[2]+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", m[1])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var s struct{ Token string }
	_ = json.NewDecoder(resp.Body).Decode(&s)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || s.Token == "" {
		t.Fatalf("session: %d", resp.StatusCode)
	}
	if strings.Count(stderr.String(), "Admin page:") != 2 {
		t.Fatalf("no fresh link printed after use: %s", stderr.String())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel")
	}
	if strings.Contains(stderr.String(), `"token"`) {
		t.Fatal("a token reached the logs")
	}
}

// A state dir too deep for a Unix socket path keeps the supervisor and its page up, without the
// handoff socket.
func TestRunWithoutHandoffSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipes have no path limit")
	}
	t.Setenv("GRILL_ROOM_DAEMON_STATE_DIR", filepath.Join(t.TempDir(), strings.Repeat("x", 120)))
	t.Setenv("GRILL_ROOM_DAEMON_BROWSER", "none")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	stderr := &syncBuf{}
	done := make(chan error, 1)
	go func() { done <- run(ctx, nil, stderr) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(stderr.String(), "Admin page:") {
		if time.Now().After(deadline) {
			t.Fatalf("no admin link in: %s", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil || !strings.Contains(stderr.String(), "handoff_socket_failed") {
		t.Fatalf("run: %v\n%s", err, stderr.String())
	}
}

// 003-AC14: workers get the supervisor's environment minus anything that may carry a Claude token
// or name an account.
func TestWorkerEnv(t *testing.T) {
	got := workerEnv([]string{"PATH=/bin", "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-x", "ANTHROPIC_API_KEY=k", "ANTHROPIC_AUTH_TOKEN=t",
		"CLAUDE_CONFIG_DIR=/home/a/.claude-work", "GRILL_ROOM_DAEMON_STATE_DIR=/s", "HOME=/home/a"})
	if strings.Join(got, " ") != "PATH=/bin GRILL_ROOM_DAEMON_STATE_DIR=/s HOME=/home/a" {
		t.Fatalf("worker env %v", got)
	}
}
