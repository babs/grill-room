package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Platform holds the OS facts and side effects the daemon depends on. Injected for tests.
type Platform struct {
	GOOS     string
	Getenv   func(string) string
	ReadFile func(string) ([]byte, error)
	LookPath func(string) (string, error)
	// Start launches a command without waiting for it.
	Start func(name string, args ...string) error
}

// Host is the real platform.
func Host() Platform {
	return Platform{GOOS: runtime.GOOS, Getenv: os.Getenv, ReadFile: os.ReadFile, LookPath: exec.LookPath, Start: start}
}

func start(name string, args ...string) error {
	cmd := exec.Command(name, args...) //nolint:gosec,noctx // a fixed opener program; it outlives any request
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	// explorer.exe exits 1 even when it opened the URL, so the exit status is ignored.
	go func() { _ = cmd.Wait() }()
	return nil
}

// WSL reports whether this Linux runs inside Windows Subsystem for Linux.
func (p Platform) WSL() bool {
	if p.GOOS != "linux" {
		return false
	}
	if p.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	b, err := p.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && strings.Contains(strings.ToLower(string(b)), "microsoft")
}

// ErrNoOpener means no way to open a browser was found: the caller prints the URL instead.
var ErrNoOpener = errors.New("no browser opener found")

// OpenCommand is the command that opens url in the default browser.
func (p Platform) OpenCommand(url string) ([]string, error) {
	switch {
	case p.GOOS == "darwin":
		return []string{"open", url}, nil
	case p.WSL():
		// wslview (wslu) opens in the Windows browser; explorer.exe is there on every WSL.
		if _, err := p.LookPath("wslview"); err == nil {
			return []string{"wslview", url}, nil
		}
		return []string{"explorer.exe", url}, nil
	case p.GOOS == "linux":
		if _, err := p.LookPath("xdg-open"); err == nil {
			return []string{"xdg-open", url}, nil
		}
	}
	return nil, ErrNoOpener
}

// OpenURL opens url in the default browser.
func (p Platform) OpenURL(url string) error {
	cmd, err := p.OpenCommand(url)
	if err != nil {
		return err
	}
	return p.Start(cmd[0], cmd[1:]...)
}
