package daemon

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Autostart starts the supervisor at login, opt-in from the page (003-AC11): a systemd user unit
// on Linux and WSL, a LaunchAgent on macOS, a scheduled task on Windows. Disabling removes the
// login item and leaves the running supervisor alone.
type Autostart struct {
	GOOS string
	Home string
	// ConfigHome is where systemd looks for user units: $XDG_CONFIG_HOME, else ~/.config.
	ConfigHome string
	Exe        string
	// Env is carried into the unit and the LaunchAgent as KEY=VALUE; a scheduled task has none.
	Env []string
	// Run runs a command and returns its error with its output.
	Run func(name string, args ...string) error
	// MemoryMax caps the systemd unit's memory, "" for none (003b-AC22): a leak then kills the app,
	// not the user's session.
	MemoryMax string
}

// DefaultMemoryMax is the unit's cap unless GRILL_ROOM_DAEMON_MEMORY_MAX says otherwise. The
// sandboxes run under Docker, outside it.
const DefaultMemoryMax = "4G"

// memoryMax is what systemd takes for MemoryMax: bytes with an optional K, M, G or T, a
// percentage, or infinity.
var memoryMax = regexp.MustCompile(`^(infinity|[0-9]+[KMGT]?|[0-9]{1,3}%)$`)

const (
	autostartName = "grill-room"
	launchLabel   = "io.github.babs.grill-room"
)

// Path is the login item's file, "" on Windows where it is a scheduled task.
func (a Autostart) Path() string {
	switch a.GOOS {
	case "windows":
		return ""
	case "darwin":
		return filepath.Join(a.Home, "Library", "LaunchAgents", launchLabel+".plist")
	}
	return filepath.Join(a.ConfigHome, "systemd", "user", autostartName+".service")
}

// Enabled reports whether the login item exists.
func (a Autostart) Enabled() (bool, error) {
	if a.GOOS == "windows" {
		return a.Run("schtasks", "/Query", "/TN", autostartName) == nil, nil
	}
	_, err := os.Stat(a.Path())
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// Enable installs the login item. It does not start a second supervisor now.
func (a Autostart) Enable() error {
	switch a.GOOS {
	case "windows":
		return a.Run("schtasks", "/Create", "/F", "/SC", "ONLOGON", "/RL", "LIMITED", "/TN", autostartName, "/TR", `"`+a.Exe+`"`)
	case "darwin":
		b, err := a.plist()
		if err != nil {
			return err
		}
		// launchd loads the agents in this directory at the next login.
		return writeFileAtomic(a.Path(), b)
	}
	unit, err := a.unit()
	if err != nil {
		return err
	}
	if err := writeFileAtomic(a.Path(), unit); err != nil {
		return fmt.Errorf("write %s: %w", a.Path(), err)
	}
	if err := a.Run("systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	return a.Run("systemctl", "--user", "enable", autostartName+".service")
}

// Disable removes the login item.
func (a Autostart) Disable() error {
	switch a.GOOS {
	case "windows":
		return a.Run("schtasks", "/Delete", "/F", "/TN", autostartName)
	case "darwin":
		return removeIfThere(a.Path())
	}
	// disable first: it removes the symlink that points at the unit file.
	if err := a.Run("systemctl", "--user", "disable", autostartName+".service"); err != nil {
		return err
	}
	if err := removeIfThere(a.Path()); err != nil {
		return err
	}
	return a.Run("systemctl", "--user", "daemon-reload")
}

func removeIfThere(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// systemdQuote quotes s for a unit file: % starts a specifier, and in ExecStart $ expands a variable.
func systemdQuote(s string, exec bool) (string, error) {
	if strings.ContainsAny(s, "\n\r") {
		return "", fmt.Errorf("%q: a line break cannot go in a systemd unit", s)
	}
	r := []string{`\`, `\\`, `"`, `\"`, "%", "%%"}
	if exec {
		r = append(r, "$", "$$")
	}
	return `"` + strings.NewReplacer(r...).Replace(s) + `"`, nil
}

func (a Autostart) unit() ([]byte, error) {
	exe, err := systemdQuote(a.Exe, true)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "[Unit]\nDescription=grill-room daemon supervisor\n\n[Service]\nExecStart=%s\n", exe)
	if a.MemoryMax != "" {
		if !memoryMax.MatchString(a.MemoryMax) {
			return nil, fmt.Errorf("memory cap %q: use bytes with K, M, G or T, a percentage, or infinity", a.MemoryMax)
		}
		fmt.Fprintf(&b, "MemoryMax=%s\n", a.MemoryMax)
	}
	for _, kv := range a.Env {
		q, err := systemdQuote(kv, false)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "Environment=%s\n", q)
	}
	b.WriteString("\n[Install]\nWantedBy=default.target\n")
	return b.Bytes(), nil
}

func (a Autostart) plist() ([]byte, error) {
	esc := func(s string) string {
		var b strings.Builder
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + launchLabel + `</string>
  <key>ProgramArguments</key><array><string>` + esc(a.Exe) + `</string></array>
  <key>RunAtLoad</key><true/>
  <key>EnvironmentVariables</key>
  <dict>
`)
	for _, kv := range a.Env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("%q is not KEY=VALUE", kv)
		}
		fmt.Fprintf(&b, "    <key>%s</key><string>%s</string>\n", esc(k), esc(v))
	}
	b.WriteString("  </dict>\n</dict>\n</plist>\n")
	return b.Bytes(), nil
}
