package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeRunner records the commands it is asked to run; fail makes the ones starting with it fail.
type fakeRunner struct {
	calls []string
	fail  string
}

func (f *fakeRunner) run(name string, args ...string) error {
	c := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, c)
	if f.fail != "" && strings.HasPrefix(c, f.fail) {
		return errors.New(c + ": failed")
	}
	return nil
}

func newAutostart(t *testing.T, goos string) (Autostart, *fakeRunner) {
	t.Helper()
	home := t.TempDir()
	r := &fakeRunner{}
	return Autostart{GOOS: goos, Home: home, ConfigHome: filepath.Join(home, ".config"), Exe: "/opt/grill room/grill-room-daemon",
		Env: []string{"GRILL_ROOM_DAEMON_BROWSER=none", "CLAUDE_CONFIG_DIR=/home/al/.claude-100%", "PATH=/usr/bin:/bin"}, Run: r.run}, r
}

// 003-AC11: on Linux the login item is a systemd user unit, enabled through systemctl --user and
// removed again; nothing is started now.
func TestAutostartLinux(t *testing.T) {
	a, r := newAutostart(t, "linux")
	if on, err := a.Enabled(); on || err != nil {
		t.Fatalf("enabled before install: %v %v", on, err)
	}
	if err := a.Enable(); err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(filepath.Join(a.ConfigHome, "systemd", "user", "grill-room.service"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`ExecStart="/opt/grill room/grill-room-daemon"`, `Environment="CLAUDE_CONFIG_DIR=/home/al/.claude-100%%"`,
		`Environment="GRILL_ROOM_DAEMON_BROWSER=none"`, "WantedBy=default.target"} {
		if !strings.Contains(string(unit), want) {
			t.Errorf("unit lacks %s:\n%s", want, unit)
		}
	}
	if strings.Contains(string(unit), "Restart=") {
		t.Error("a unit that restarts would fight a supervisor started by hand")
	}
	if want := []string{"systemctl --user daemon-reload", "systemctl --user enable grill-room.service"}; !slices.Equal(r.calls, want) {
		t.Fatalf("enable ran %q", r.calls)
	}
	if on, _ := a.Enabled(); !on {
		t.Fatal("not enabled after install")
	}
	r.calls = nil
	if err := a.Disable(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"systemctl --user disable grill-room.service", "systemctl --user daemon-reload"}; !slices.Equal(r.calls, want) {
		t.Fatalf("disable ran %q", r.calls)
	}
	if on, _ := a.Enabled(); on {
		t.Fatal("still enabled after removal")
	}
	// No systemd user instance, as on a WSL without systemd: the page shows why.
	r.fail = "systemctl --user enable"
	if err := a.Enable(); err == nil || !strings.Contains(err.Error(), "enable grill-room.service: failed") {
		t.Fatalf("err = %v", err)
	}
}

// 003b-AC22: the unit caps its memory with MemoryMax; no cap writes no line; a value systemd would
// misread, or one smuggling another directive, is refused before the unit is written.
func TestAutostartMemoryMax(t *testing.T) {
	a, _ := newAutostart(t, "linux")
	path := filepath.Join(a.ConfigHome, "systemd", "user", "grill-room.service")
	for _, tc := range []struct{ max, want string }{{DefaultMemoryMax, "MemoryMax=4G\n"}, {"infinity", "MemoryMax=infinity\n"}, {"75%", "MemoryMax=75%\n"}, {"", ""}} {
		a.MemoryMax = tc.max
		if err := a.Enable(); err != nil {
			t.Fatal(err)
		}
		unit, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(string(unit), "MemoryMax="); tc.want == "" && got != 0 || tc.want != "" && !strings.Contains(string(unit), tc.want) {
			t.Errorf("MemoryMax %q gave:\n%s", tc.max, unit)
		}
	}
	for _, bad := range []string{"4 GB", "4G\nExecStartPre=/bin/true", "-1", "lots"} {
		a.MemoryMax = bad
		if err := a.Enable(); err == nil {
			t.Errorf("MemoryMax %q accepted", bad)
		}
	}
}

// systemd specifiers and variables in a path cannot change what the unit runs.
func TestSystemdQuote(t *testing.T) {
	for in, want := range map[string]string{`/a b/c`: `"/a b/c"`, `/x/%h$HOME"\`: `"/x/%%h$$HOME\"\\"`} {
		if got, err := systemdQuote(in, true); err != nil || got != want {
			t.Errorf("systemdQuote(%q) = %q %v, want %q", in, got, err, want)
		}
	}
	if got, _ := systemdQuote("K=$v", false); got != `"K=$v"` {
		t.Errorf("environment value: %q", got)
	}
	if _, err := systemdQuote("a\nExecStartPre=/bin/evil", true); err == nil {
		t.Error("a line break went into the unit")
	}
	a, _ := newAutostart(t, "linux")
	a.Env = []string{"X=a\nb"}
	if err := a.Enable(); err == nil {
		t.Error("a line break in the environment went into the unit")
	}
}

// 003-AC11: on macOS the login item is a LaunchAgent in ~/Library/LaunchAgents, loaded by launchd at
// the next login; removing it leaves the running supervisor alone.
func TestAutostartDarwin(t *testing.T) {
	a, r := newAutostart(t, "darwin")
	a.Exe = "/Users/al/bin/grill-room-daemon&<x>"
	if err := a.Enable(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(a.Home, "Library", "LaunchAgents", "io.github.babs.grill-room.plist")
	plist, err := os.ReadFile(p) //nolint:gosec // test temp file
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<string>io.github.babs.grill-room</string>", "<string>/Users/al/bin/grill-room-daemon&amp;&lt;x&gt;</string>",
		"<key>RunAtLoad</key><true/>", "<key>PATH</key><string>/usr/bin:/bin</string>", "<key>CLAUDE_CONFIG_DIR</key><string>/home/al/.claude-100%</string>"} {
		if !strings.Contains(string(plist), want) {
			t.Errorf("plist lacks %s:\n%s", want, plist)
		}
	}
	if on, _ := a.Enabled(); !on {
		t.Fatal("not enabled")
	}
	if err := a.Disable(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("plist left: %v", err)
	}
	if err := a.Disable(); err != nil {
		t.Fatalf("second removal: %v", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("launchctl ran: %q", r.calls)
	}
	a.Env = []string{"NOEQUALS"}
	if err := a.Enable(); err == nil {
		t.Error("a malformed variable went into the plist")
	}
}

// 003-AC11: on Windows the login item is a scheduled task at logon, created, queried and deleted
// with schtasks.
func TestAutostartWindows(t *testing.T) {
	a, r := newAutostart(t, "windows")
	a.Exe = `C:\Users\al\grill room\grill-room-daemon.exe`
	if a.Path() != "" {
		t.Fatalf("path %q", a.Path())
	}
	r.fail = "schtasks /Query"
	if on, err := a.Enabled(); on || err != nil {
		t.Fatalf("enabled with no task: %v %v", on, err)
	}
	r.fail = ""
	if err := a.Enable(); err != nil {
		t.Fatal(err)
	}
	if on, _ := a.Enabled(); !on {
		t.Fatal("not enabled")
	}
	if err := a.Disable(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"schtasks /Query /TN grill-room",
		`schtasks /Create /F /SC ONLOGON /RL LIMITED /TN grill-room /TR "C:\Users\al\grill room\grill-room-daemon.exe"`,
		"schtasks /Query /TN grill-room",
		"schtasks /Delete /F /TN grill-room",
	}
	if !slices.Equal(r.calls, want) {
		t.Fatalf("ran %q", r.calls)
	}
}
