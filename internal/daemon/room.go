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
	"slices"
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

// ParseRoomURL accepts any link inside a room a participant sees in their browser: .../r/<room>,
// its docs window .../r/<room>/docs[/<doc>], any trailing path, query or fragment. It returns the
// room. The command line, a second launch, the admin page and the worker all parse with it.
func ParseRoomURL(raw string) (Room, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return Room{}, fmt.Errorf("%q is not a URL: paste the address of a room page, with /r/<room> in it", raw)
	}
	// Security: the daemon token travels on this connection; plain HTTP only stays on this machine.
	if u.Scheme != "https" && (u.Scheme != "http" || !isLoopback(u.Hostname())) {
		return Room{}, fmt.Errorf("room URL must use https (http only for localhost): %s", u.Redacted())
	}
	// Security: user info would hide the real host behind an allowed-looking name.
	if u.User != nil {
		return Room{}, fmt.Errorf("a room link holds no user name or password: %s", u.Redacted())
	}
	// The first /r/<id> segment pair is the room; what follows is a page inside it.
	segs := strings.Split(u.Path, "/")
	i := slices.Index(segs, "r")
	if i < 0 || i+1 >= len(segs) || segs[i+1] == "" {
		return Room{}, fmt.Errorf("%s is not a room link: paste the address of a room page, with /r/<room> in it", u.Redacted())
	}
	if id := segs[i+1]; !roomIDRe.MatchString(id) {
		return Room{}, fmt.Errorf("room id %q is not valid", id)
	}
	return Room{Base: u.Scheme + "://" + u.Host + strings.Join(segs[:i], "/"), ID: segs[i+1]}, nil
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
	b, err := os.ReadFile(t.path) //nolint:gosec // G703: the instance's own state dir, from the supervisor's config
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
