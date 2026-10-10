package daemon

import (
	"strings"
	"sync"
)

// Ring keeps the last lines written to it, for the local page. zap writes one entry per Write.
type Ring struct {
	mu    sync.Mutex
	lines []string
	next  int
	full  bool
}

// NewRing keeps n lines.
func NewRing(n int) *Ring { return &Ring{lines: make([]string, n)} }

func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		r.lines[r.next] = l
		r.next = (r.next + 1) % len(r.lines)
		r.full = r.full || r.next == 0
	}
	return len(p), nil
}

// Sync is a no-op: the ring is memory.
func (r *Ring) Sync() error { return nil }

// Lines returns the kept lines, oldest first.
func (r *Ring) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		return append([]string(nil), r.lines[:r.next]...)
	}
	return append(append([]string(nil), r.lines[r.next:]...), r.lines[:r.next]...)
}
