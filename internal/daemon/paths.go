package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/babs/grill-room/internal/sandbox"
)

// Paths are the supervisor's roots; each instance has its own instances/<id>/ under every root.
type Paths struct {
	Config string
	State  string
	Cache  string
}

const (
	appDir    = "grill-room"
	legacyApp = "grill-room-daemon"
)

// ResolvePaths returns the platform's roots and the directory a single-room daemon kept its state
// in (003-AC11): XDG on Linux and WSL, ~/Library on macOS, %APPDATA% and %LOCALAPPDATA% on
// Windows. GRILL_ROOM_DAEMON_STATE_DIR, when set, holds everything, as it held the single-room
// daemon's state.
func ResolvePaths(goos string, getenv func(string) string, home string) (p Paths, legacy string, err error) {
	if dir := getenv("GRILL_ROOM_DAEMON_STATE_DIR"); dir != "" {
		return Paths{Config: dir, State: dir, Cache: dir}, dir, nil
	}
	switch goos {
	case "windows":
		roaming, local := getenv("APPDATA"), getenv("LOCALAPPDATA")
		if roaming == "" || local == "" {
			return Paths{}, "", errors.New("%APPDATA% or %LOCALAPPDATA% is not set: set GRILL_ROOM_DAEMON_STATE_DIR")
		}
		return Paths{Config: winJoin(roaming, appDir), State: winJoin(local, appDir), Cache: winJoin(local, appDir)}, winJoin(roaming, legacyApp), nil
	case "darwin":
		if home == "" {
			return Paths{}, "", errors.New("no home directory: set GRILL_ROOM_DAEMON_STATE_DIR")
		}
		support := path.Join(home, "Library", "Application Support")
		return Paths{Config: path.Join(support, appDir), State: path.Join(support, appDir), Cache: path.Join(home, "Library", "Caches", appDir)},
			path.Join(support, legacyApp), nil
	}
	// The XDG spec says to ignore a relative value.
	var missing bool
	xdg := func(key string, def ...string) string {
		if v := getenv(key); path.IsAbs(v) {
			return v
		}
		missing = missing || home == ""
		return path.Join(append([]string{home}, def...)...)
	}
	config := xdg("XDG_CONFIG_HOME", ".config")
	p = Paths{Config: path.Join(config, appDir), State: path.Join(xdg("XDG_STATE_HOME", ".local", "state"), appDir), Cache: path.Join(xdg("XDG_CACHE_HOME", ".cache"), appDir)}
	if missing {
		return Paths{}, "", errors.New("no home directory: set GRILL_ROOM_DAEMON_STATE_DIR")
	}
	return p, path.Join(config, legacyApp), nil
}

// winJoin joins Windows paths whatever the OS running it; filepath.Clean would turn a UNC
// \\server\share prefix, a common redirected %APPDATA%, into a rooted path.
func winJoin(dir, name string) string { return strings.TrimRight(dir, `\/`) + `\` + name }

func (p Paths) instance(root, id string) string { return filepath.Join(root, "instances", id) }

// InstanceConfig holds instance.json, settings.json and allowlist.json.
func (p Paths) InstanceConfig(id string) string { return p.instance(p.Config, id) }

// supervisorSettings is the supervisor's own settings file.
func (p Paths) supervisorSettings() string { return filepath.Join(p.Config, "supervisor.json") }

// Bin holds the instances' worker binaries, one directory per version.
func (p Paths) Bin() string { return filepath.Join(p.State, "bin") }

// Trust holds the TUF cache and the last good trusted root.
func (p Paths) Trust() string { return filepath.Join(p.State, "trust") }

// DevKeys holds the trusted dev public keys, one <name>.pub file each.
func (p Paths) DevKeys() string { return filepath.Join(p.Config, "dev-keys") }

// InstanceState holds tokens.json.
func (p Paths) InstanceState(id string) string { return p.instance(p.State, id) }

// InstanceCache holds workspace-cache/.
func (p Paths) InstanceCache(id string) string { return p.instance(p.Cache, id) }

// InstanceID names a room server: the first 12 hex digits of the sha256 of its normalised scheme,
// host and port. Two rooms on one server share an instance and its worker.
func InstanceID(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%q is not a room server URL", base)
	}
	scheme, port := strings.ToLower(u.Scheme), u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[scheme]
	}
	sum := sha256.Sum256([]byte(scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)))
	return hex.EncodeToString(sum[:6]), nil
}

// Instance is a room server this machine hosts rooms for, kept in instance.json.
type Instance struct {
	ID string `json:"id"`
	// URL is the room server's base URL, as in its rooms' links.
	URL string `json:"url"`
	// Rooms are its room links, oldest first.
	Rooms []string `json:"rooms"`
	// Account names the Claude account its rooms use; empty is "default" (003-AC13).
	Account string `json:"account,omitempty"`
	// RoomAccounts overrides Account for some rooms, by room link (003-AC13).
	RoomAccounts map[string]string `json:"room_accounts,omitempty"`
	// CutOff shuts the instance's worker out of every account's proxy until the host lets it back (003-AC15).
	CutOff bool `json:"cut_off,omitempty"`
	// Version is the binary its worker ran last, kept by the binary store's collection (003-AC7).
	Version string `json:"version,omitempty"`
	// Relax runs its binaries even unsigned: off by default, switched on by typing its host (003-AC4).
	Relax bool `json:"accept_unsigned,omitempty"`
	// Limits replace the supervisor's sandbox limits for this instance, set on the page (003-AC26).
	Limits *sandbox.Limits `json:"limits,omitempty"`
	// Paused are the room links the host paused: they stay paired but report they cannot host (003b-AC5).
	Paused []string `json:"paused,omitempty"`
}

// accountFor names the account room's turns use (003-AC13).
func (i *Instance) accountFor(room string) string {
	if a := i.RoomAccounts[room]; a != "" {
		return a
	}
	if i.Account != "" {
		return i.Account
	}
	return "default"
}

// dropRoom forgets room: its place, its account and its pause (003b-AC6).
func (i *Instance) dropRoom(room string) {
	i.Rooms = slices.DeleteFunc(i.Rooms, func(r string) bool { return r == room })
	i.Paused = slices.DeleteFunc(i.Paused, func(r string) bool { return r == room })
	delete(i.RoomAccounts, room)
}

// addRoom puts room last, as the newest.
func (i *Instance) addRoom(room string) {
	i.Rooms = append(slices.DeleteFunc(i.Rooms, func(r string) bool { return r == room }), room)
}

const instanceFile = "instance.json"

func loadInstance(dir string) (Instance, error) {
	var in Instance
	b, err := os.ReadFile(filepath.Join(dir, instanceFile)) //nolint:gosec // our own state dir
	if err != nil {
		return in, err
	}
	if err := json.Unmarshal(b, &in); err != nil {
		return in, fmt.Errorf("parse %s: %w", filepath.Join(dir, instanceFile), err)
	}
	return in, nil
}

func saveInstance(p Paths, in Instance) error {
	b, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(p.InstanceConfig(in.ID), instanceFile), append(b, '\n'))
}

// LoadInstances reads every instance under p, skipping and reporting the unreadable ones.
func LoadInstances(p Paths) ([]Instance, []error) {
	dirs, err := os.ReadDir(filepath.Join(p.Config, "instances"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []error{err}
	}
	var out []Instance
	var errs []error
	for _, e := range dirs {
		in, err := loadInstance(p.InstanceConfig(e.Name()))
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			errs = append(errs, err)
		case in.ID != e.Name():
			errs = append(errs, fmt.Errorf("%s: instance id %q does not match its directory", p.InstanceConfig(e.Name()), in.ID))
		default:
			out = append(out, in)
		}
	}
	return out, errs
}
