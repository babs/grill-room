// Package creds reads the host's Claude Code OAuth access token without ever writing it back:
// the macOS Keychain first, then <config dir>/.credentials.json.
//
// Derived from github.com/babs/claude-quota credentials.go, credentials_darwin.go and
// credentials_other.go at commit 45b0113.
package creds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// KeychainTimeout bounds one Keychain read: a locked Keychain waits for a password prompt forever.
const KeychainTimeout = 3 * time.Second

// Token is the host's OAuth access token. Never log it.
type Token struct {
	AccessToken string
	// ExpiresAt is zero when the source carries no expiry.
	ExpiresAt time.Time
	// Origin names where the token came from ("Keychain" or the file path), for status display.
	Origin string
}

// Expired reports whether upstream would refuse the token at now.
func (t Token) Expired(now time.Time) bool {
	return !t.ExpiresAt.IsZero() && !now.Before(t.ExpiresAt)
}

// KeychainFunc returns the Keychain item holding Claude Code's credentials JSON for service.
type KeychainFunc func(ctx context.Context, service, account string) ([]byte, error)

// Reader reads credentials. Keychain is nil where no Keychain exists (every OS but macOS).
type Reader struct {
	ConfigDir       string
	Keychain        KeychainFunc
	KeychainTimeout time.Duration
	// Service and Account name the Keychain item Claude Code stores the credentials in.
	Service, Account string
}

// New returns the Reader for this OS and environment.
func New() Reader {
	return Reader{ConfigDir: ConfigDir(os.Getenv, home()), Keychain: platformKeychain, KeychainTimeout: KeychainTimeout,
		Service: KeychainService(os.Getenv("CLAUDE_CONFIG_DIR")), Account: keychainAccount(os.Getenv("USER"), currentUser())}
}

func home() string {
	h, _ := os.UserHomeDir() // empty home leaves a relative path that fails to read, with its name in the error
	return h
}

// ConfigDir is Claude Code's config directory: $CLAUDE_CONFIG_DIR, else ~/.claude.
func ConfigDir(getenv func(string) string, home string) string {
	if d := getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	return filepath.Join(home, ".claude")
}

var accountRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// keychainAccount mirrors Claude Code 2.1.296: $USER, else the OS user name, and a fixed name
// when either holds characters it refuses.
func keychainAccount(envUser, osUser string) string {
	u := envUser
	if u == "" {
		u = osUser
	}
	if !accountRe.MatchString(u) {
		return "claude-code-user"
	}
	return u
}

// KeychainService mirrors Claude Code 2.1.296: with CLAUDE_CONFIG_DIR set, the item name carries
// the first 8 hex digits of the SHA-256 of that directory, so each config dir has its own item.
// ponytail: Claude Code NFC-normalises the path first; a non-ASCII path in NFD misses the item.
func KeychainService(configDirEnv string) string {
	if configDirEnv == "" {
		return "Claude Code-credentials"
	}
	h := sha256.Sum256([]byte(configDirEnv))
	return "Claude Code-credentials-" + hex.EncodeToString(h[:])[:8]
}

// File is the credentials file path.
func (r Reader) File() string { return filepath.Join(r.ConfigDir, ".credentials.json") }

type credentialsFile struct {
	ClaudeAiOauth struct {
		AccessToken string `json:"accessToken"`
		ExpiresAt   int64  `json:"expiresAt"` // ms since epoch
	} `json:"claudeAiOauth"`
}

func parse(raw []byte) (Token, error) {
	var f credentialsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		// The decoder's message can quote the input, which holds the token.
		return Token{}, errors.New("credentials are not valid JSON")
	}
	if f.ClaudeAiOauth.AccessToken == "" {
		return Token{}, errors.New("credentials hold no claudeAiOauth.accessToken")
	}
	t := Token{AccessToken: f.ClaudeAiOauth.AccessToken}
	if f.ClaudeAiOauth.ExpiresAt > 0 {
		t.ExpiresAt = time.UnixMilli(f.ClaudeAiOauth.ExpiresAt)
	}
	return t, nil
}

// Read returns the token: from the Keychain if one exists and answers in time, else from the file.
func (r Reader) Read(ctx context.Context) (Token, error) {
	var kcErr error
	if r.Keychain != nil {
		raw, err := r.readKeychain(ctx)
		if err == nil {
			t, perr := parse(raw)
			if perr == nil {
				t.Origin = "Keychain"
				return t, nil
			}
			err = perr
		}
		kcErr = fmt.Errorf("macOS Keychain: %w", err)
	}
	t, err := r.readFile()
	if err != nil {
		return Token{}, errors.Join(kcErr, err)
	}
	return t, nil
}

func (r Reader) readFile() (Token, error) {
	p := r.File()
	raw, err := os.ReadFile(p) //nolint:gosec // the path is the user's own Claude config dir
	if err != nil {
		return Token{}, fmt.Errorf("read %s: %w (run 'claude' and log in first)", p, err)
	}
	t, err := parse(raw)
	if err != nil {
		return Token{}, fmt.Errorf("%s: %w", p, err)
	}
	t.Origin = p
	return t, nil
}

// readKeychain gives up after KeychainTimeout even if the runner ignores its context.
func (r Reader) readKeychain(ctx context.Context) ([]byte, error) {
	timeout := r.KeychainTimeout
	if timeout <= 0 {
		timeout = KeychainTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type result struct {
		raw []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		raw, err := r.Keychain(ctx, r.Service, r.Account)
		done <- result{raw, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			return nil, res.err
		}
		return []byte(strings.TrimSpace(string(res.raw))), nil
	case <-ctx.Done():
		return nil, fmt.Errorf("no answer within %s, the Keychain may be locked: unlock it and retry", timeout)
	}
}

// Source caches the token for the proxy. It re-reads when the file's mtime changes, after
// Invalidate, and while the cached token is expired: the host's Claude Code may have refreshed it.
type Source struct {
	r     Reader
	mu    sync.Mutex
	tok   Token
	mtime time.Time
	ok    bool
	now   func() time.Time
}

// NewSource wraps r.
func NewSource(r Reader) *Source { return &Source{r: r, now: time.Now} }

// Reader returns the wrapped reader.
func (s *Source) Reader() Reader { return s.r }

// Get returns the current token.
func (s *Source) Get(ctx context.Context) (Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var mtime time.Time
	if st, err := os.Stat(s.r.File()); err == nil {
		mtime = st.ModTime()
	}
	if s.ok && mtime.Equal(s.mtime) && !s.tok.Expired(s.now()) {
		return s.tok, nil
	}
	t, err := s.r.Read(ctx)
	if err != nil {
		s.ok = false
		return Token{}, err
	}
	s.tok, s.mtime, s.ok = t, mtime, true
	return t, nil
}

// Invalidate forces the next Get to re-read.
func (s *Source) Invalidate() {
	s.mu.Lock()
	s.ok = false
	s.mu.Unlock()
}
