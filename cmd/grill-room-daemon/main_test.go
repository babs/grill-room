package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"regexp"
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
	if err != nil || cfg.StateDir != "/state" || cfg.Browser != "auto" || cfg.Image != "" ||
		cfg.Limits != (sandbox.Limits{CPUs: "2", Memory: "4g", EgressCPUs: "0.5", EgressMemory: "256m"}) {
		t.Fatalf("cfg = %+v, %v", cfg, err)
	}
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
	} {
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

// A restarted daemon must find its own sandbox again; two daemons must never share one.
func TestSandboxName(t *testing.T) {
	a := daemon.Room{Base: "https://rooms.example", ID: "abc"}
	b := daemon.Room{Base: "https://rooms.example", ID: "abd"}
	n := sandboxName("/state/one", a)
	if len(n) != 12 || n != sandboxName("/state/one", a) {
		t.Fatalf("name %q not stable", n)
	}
	for _, other := range []string{sandboxName("/state/two", a), sandboxName("/state/one", b)} {
		if other == n {
			t.Fatalf("names collide: %q", n)
		}
	}
}
