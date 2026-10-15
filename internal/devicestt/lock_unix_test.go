//go:build !windows

package devicestt

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// 010-AC6: a run holds the directory lock while a download is in flight, so a second run cannot
// sweep its temporary file.
func TestFetchHoldsLock(t *testing.T) {
	m, host, ts := fakeManifest(t)
	host.gate, host.inFlight = make(chan struct{}), make(chan struct{})
	dir := t.TempDir()
	done := make(chan error, 1)
	go func() { done <- m.Fetch(t.Context(), dir, ts.Client(), zap.NewNop()) }()
	<-host.inFlight
	f, err := os.OpenFile(filepath.Join(dir, ".fetch.lock"), os.O_RDWR, 0)
	if err != nil {
		close(host.gate)
		t.Fatal(err)
	}
	defer f.Close()
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	close(host.gate)
	if !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("lock taken during a download: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
