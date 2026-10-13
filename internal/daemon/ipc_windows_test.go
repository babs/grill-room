package daemon

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// 003-AC6 on Windows (M3): a launch hands its room over only to a pipe served by its own user.
func TestPipeServerOwner(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ln, err := listenLocal(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	c, err := dialLocal(context.Background(), dir)
	if err != nil {
		t.Fatalf("own supervisor's pipe refused: %v", err)
	}
	_ = c.Close()

	// The same pipe, as a launch running under another account would see it.
	c, err = winio.DialPipeContext(context.Background(), pipeName(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := checkPipeServer(c, system); !errors.Is(err, errForeignPipe) || !strings.Contains(err.Error(), user.User.Sid.String()) {
		t.Fatalf("pipe of another user accepted: %v", err)
	}
	// A process of another user: System (pid 4) cannot be opened, or runs as SYSTEM.
	if err := checkProcessOwner(4, user.User.Sid); !errors.Is(err, errForeignPipe) {
		t.Fatal("System process accepted as this user's")
	}
	if err := checkProcessOwner(uint32(os.Getpid()), user.User.Sid); err != nil { //nolint:gosec // pids fit in 32 bits on Windows
		t.Fatalf("own process refused: %v", err)
	}
}

// Close returns even when a client hangs up while an Accept is pending: go-winio's own Close can
// then wait forever, which once hung this package's test run for 10 minutes.
func TestPipeCloseDuringHangup(t *testing.T) {
	for i := range 50 {
		dir := t.TempDir()
		ln, err := listenLocal(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}()
		c, err := winio.DialPipeContext(context.Background(), pipeName(dir))
		if err == nil {
			go func() { _ = c.Close() }()
		}
		closed := make(chan struct{})
		go func() {
			_ = ln.Close()
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(10 * time.Second):
			t.Fatalf("round %d: Close did not return", i)
		}
	}
}
