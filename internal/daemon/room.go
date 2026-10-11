package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Room is a room server and a room on it.
type Room struct {
	// Base is the server URL with no trailing slash, e.g. https://rooms.example.
	Base string
	ID   string
}

// String is the room page URL, the key under which the daemon token is stored.
func (r Room) String() string { return r.Base + "/r/" + r.ID }

var roomIDRe = regexp.MustCompile(`^[a-z0-9]{1,64}$`)

// ParseRoomURL accepts the room page URL a participant sees in their browser, .../r/<room>.
func ParseRoomURL(raw string) (Room, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return Room{}, fmt.Errorf("%q is not a URL: paste the room page address, ending in /r/<room>", raw)
	}
	// Security: the daemon token travels on this connection; plain HTTP only stays on this machine.
	if u.Scheme != "https" && (u.Scheme != "http" || !isLoopback(u.Hostname())) {
		return Room{}, fmt.Errorf("room URL must use https (http only for localhost): %s", u.Redacted())
	}
	i := strings.LastIndex(u.Path, "/r/")
	if i < 0 {
		return Room{}, errors.New("room URL must end in /r/<room>")
	}
	id := strings.TrimSuffix(u.Path[i+3:], "/")
	if !roomIDRe.MatchString(id) {
		return Room{}, fmt.Errorf("room id %q is not valid", id)
	}
	return Room{Base: u.Scheme + "://" + u.Host + u.Path[:i], ID: id}, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Tokens persists daemon tokens per room in a 0600 file. Tokens are never shown or logged.
type Tokens struct {
	path string
	mu   sync.Mutex
}

// NewTokens keeps tokens in dir/tokens.json.
func NewTokens(dir string) *Tokens { return &Tokens{path: filepath.Join(dir, "tokens.json")} }

func (t *Tokens) load() (map[string]string, error) {
	b, err := os.ReadFile(t.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read tokens: %w", err)
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", t.path, err)
	}
	return m, nil
}

// Get returns the token for room, "" if none.
func (t *Tokens) Get(r Room) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	m, err := t.load()
	if err != nil {
		return "", err
	}
	return m[r.String()], nil
}

// Set stores tok for room; an empty tok deletes it.
func (t *Tokens) Set(r Room, tok string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	m, err := t.load()
	if err != nil {
		return err
	}
	if tok == "" {
		delete(m, r.String())
	} else {
		m[r.String()] = tok
	}
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode tokens: %w", err)
	}
	if err := writeFileAtomic(t.path, b); err != nil {
		return fmt.Errorf("write tokens: %w", err)
	}
	return nil
}
