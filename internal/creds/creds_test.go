package creds

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCreds(t *testing.T, dir, tok string, exp time.Time) {
	t.Helper()
	body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"r","expiresAt":%d}}`, tok, exp.UnixMilli())
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func kcJSON(tok string) []byte {
	return []byte(fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":%d}}`+"\n", tok, time.Now().Add(time.Hour).UnixMilli()))
}

// 001-AC23: CLAUDE_CONFIG_DIR wins over ~/.claude, and it names its own Keychain item.
func TestConfigDirAndService(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	if got := ConfigDir(getenv, "/home/u"); got != filepath.Join("/home/u", ".claude") {
		t.Fatalf("default = %q", got)
	}
	env["CLAUDE_CONFIG_DIR"] = "/cfg/claude"
	if got := ConfigDir(getenv, "/home/u"); got != "/cfg/claude" {
		t.Fatalf("env = %q", got)
	}
	if got := KeychainService(""); got != "Claude Code-credentials" {
		t.Fatalf("service = %q", got)
	}
	// sha256("/cfg/claude")[:8], as Claude Code 2.1.296 computes it.
	if got := KeychainService("/cfg/claude"); got != "Claude Code-credentials-499a9d85" {
		t.Fatalf("service with config dir = %q", got)
	}
	for _, tc := range []struct{ env, os, want string }{
		{"alice", "bob", "alice"},
		{"", "bob", "bob"},
		{"", `DOMAIN\bob`, "claude-code-user"},
		{"a b", "bob", "claude-code-user"},
	} {
		if got := keychainAccount(tc.env, tc.os); got != tc.want {
			t.Errorf("keychainAccount(%q, %q) = %q, want %q", tc.env, tc.os, got, tc.want)
		}
	}
}

// 001-AC23: macOS reads the Keychain first and falls back to the file; elsewhere the file only.
func TestRead(t *testing.T) {
	exp := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	fails := func(context.Context, string, string) ([]byte, error) { return nil, errors.New("exit status 44") }
	for _, tc := range []struct {
		name     string
		keychain KeychainFunc
		file     bool
		want     string
		origin   string
		wantErr  string
	}{
		{"file only", nil, true, "file-tok", "file", ""},
		{"keychain first", func(context.Context, string, string) ([]byte, error) { return kcJSON("kc-tok"), nil }, true, "kc-tok", "Keychain", ""},
		{"keychain fails, file", fails, true, "file-tok", "file", ""},
		{"keychain empty token, file", func(context.Context, string, string) ([]byte, error) { return []byte(`{}`), nil }, true, "file-tok", "file", ""},
		{"neither", fails, false, "", "", "macOS Keychain: exit status 44"},
		{"no file", nil, false, "", "", "run 'claude' and log in first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.file {
				writeCreds(t, dir, "file-tok", exp)
			}
			r := Reader{ConfigDir: dir, Keychain: tc.keychain, Service: "svc", Account: "acct"}
			tok, err := r.Read(context.Background())
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tok.AccessToken != tc.want {
				t.Fatalf("token = %q, want %q", tok.AccessToken, tc.want)
			}
			if tc.origin == "file" && tok.Origin != r.File() || tc.origin == "Keychain" && tok.Origin != "Keychain" {
				t.Fatalf("origin = %q", tok.Origin)
			}
			if tc.origin == "file" && !tok.ExpiresAt.Equal(exp) {
				t.Fatalf("expiresAt = %v, want %v", tok.ExpiresAt, exp)
			}
		})
	}
}

// 001-AC24: a Keychain that never answers (locked, waiting on a password prompt) fails the read
// within 5 s with a message saying so, even when the runner ignores its context.
func TestLockedKeychainFailsFast(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	hang := func(context.Context, string, string) ([]byte, error) { <-block; return nil, nil }
	r := Reader{ConfigDir: t.TempDir(), Keychain: hang, KeychainTimeout: KeychainTimeout}
	start := time.Now()
	_, err := r.Read(context.Background())
	if d := time.Since(start); d >= 5*time.Second {
		t.Fatalf("read took %s", d)
	}
	if err == nil || !strings.Contains(err.Error(), "Keychain may be locked") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseErrorsNeverQuoteInput(t *testing.T) {
	_, err := parse([]byte(`{"claudeAiOauth":{"accessToken":"sk-ant-secret`))
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v", err)
	}
}

func TestExpired(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		exp  time.Time
		want bool
	}{{time.Time{}, false}, {now.Add(time.Second), false}, {now, true}, {now.Add(-time.Second), true}} {
		if got := (Token{ExpiresAt: tc.exp}).Expired(now); got != tc.want {
			t.Errorf("Expired(%v) = %v", tc.exp, got)
		}
	}
}

func TestSourceRereads(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Unix(1_000_000, 0)
	setFile := func(tok string, exp time.Time, mtime time.Time) {
		writeCreds(t, dir, tok, exp)
		if err := os.Chtimes(filepath.Join(dir, ".credentials.json"), mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	future := time.Now().Add(time.Hour)
	setFile("a", future, t0)
	s := NewSource(Reader{ConfigDir: dir})
	get := func(want string) {
		t.Helper()
		tok, err := s.Get(context.Background())
		if err != nil || tok.AccessToken != want {
			t.Fatalf("Get = %q, %v; want %q", tok.AccessToken, err, want)
		}
	}
	get("a")
	setFile("b", future, t0) // same mtime: cached
	get("a")
	s.Invalidate()
	get("b")
	setFile("c", future, t0.Add(time.Second)) // mtime changed
	get("c")
	setFile("d", time.Now().Add(-time.Minute), t0.Add(2*time.Second))
	get("d")
	setFile("e", future, t0.Add(2*time.Second)) // same mtime, but the cached token expired
	get("e")
	if err := os.Remove(filepath.Join(dir, ".credentials.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(context.Background()); err == nil {
		t.Fatal("Get after removal succeeded")
	}
}

// 001-AC23: the real constructor reads CLAUDE_CONFIG_DIR, both for the file and the Keychain item.
func TestNewHonoursConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("USER", "alice")
	r := New()
	if r.ConfigDir != dir || r.File() != filepath.Join(dir, ".credentials.json") || r.Service != KeychainService(dir) || r.Account != "alice" {
		t.Fatalf("New() = %+v", r)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if r := New(); filepath.Base(r.ConfigDir) != ".claude" || r.Service != "Claude Code-credentials" {
		t.Fatalf("New() without env = %+v", r)
	}
	if NewSource(r).Reader().ConfigDir != r.ConfigDir {
		t.Fatal("Source.Reader lost the config dir")
	}
}
