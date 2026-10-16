package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/sandbox"
)

// How long a refused CONNECT waits for the AI host's answer, in seconds: WebFetch was measured
// alive after a 45 s hold (014-AC29).
const (
	HoldDefault = 45
	HoldMin     = 30
	HoldMax     = 60
)

// holdSeconds is n within HoldMin and HoldMax, HoldDefault for 0.
func holdSeconds(n int) int {
	if n == 0 {
		return HoldDefault
	}
	return min(max(n, HoldMin), HoldMax)
}

// HoldDir is the host folder of the hold socket of room's sandbox slot under root: short, as a
// socket path is bounded.
func HoldDir(root string, r Room, slot int) string {
	sum := sha256.Sum256([]byte(r.String()))
	return filepath.Join(root, fmt.Sprintf("%s-%d", hex.EncodeToString(sum[:6]), slot))
}

// maxSocketPath bounds a unix socket's path on Linux.
const maxSocketPath = 107

// holdLocked reports whether the sandboxes ask before refusing a public name: a native Docker on
// Linux, whose containers mount a host socket; the caller holds d.mu (014-AC29).
func (d *Daemon) holdLocked() bool {
	return d.o.HoldRoot != "" && runtime.GOOS == "linux" && !d.st.Docker.Desktop
}

// holdWait is one domain's question to the room: the requests that join it share its answer.
type holdWait struct {
	done chan struct{}
	ok   bool
}

// startHold serves the hold socket of the room's sandbox slot, for squid's helper in its egress
// proxy, and returns its folder; "" when the sandboxes do not hold.
func (r *roomRun) startHold(slot int, sl *sandboxSlot) string {
	r.d.mu.Lock()
	on := r.d.holdLocked()
	r.d.mu.Unlock()
	if !on {
		return ""
	}
	// Absolute: docker run reads a relative -v source as a volume name.
	dir, err := filepath.Abs(HoldDir(r.d.o.HoldRoot, r.room, slot))
	var ln net.Listener
	if err == nil {
		ln, err = listenHold(dir, filepath.Join(dir, sandbox.HoldSocket))
	}
	if err != nil {
		r.d.o.Log.Warn("hold_unavailable", zap.String("room", r.room.String()), zap.Error(err))
		return ""
	}
	sl.hold = ln
	go r.serveHold(ln, sl)
	return dir
}

func listenHold(dir, path string) (net.Listener, error) {
	if len(path) > maxSocketPath {
		return nil, fmt.Errorf("socket path too long: %s", path)
	}
	// The root stays private: only its leaf, the mounted folder, opens to the egress proxy.
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(filepath.Dir(dir), 0o700); err != nil { //nolint:gosec // a folder: 0700 is its owner-only mode
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// The egress proxy's user is not this one: the folder and socket are open to it, the state
	// folder above them is not open to anyone else.
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // see above
		return nil, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil { //nolint:gosec // see above
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// serveHold answers squid's helper: "<id> <host> ..." lines, each answered "<id> OK" or "<id> ERR"
// as its decision comes, out of order.
func (r *roomRun) serveHold(ln net.Listener, sl *sandboxSlot) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go r.holdConn(c, sl)
	}
}

func (r *roomRun) holdConn(c net.Conn, sl *sandboxSlot) {
	defer func() { _ = c.Close() }()
	var wmu sync.Mutex
	var wg sync.WaitGroup
	defer wg.Wait()
	sc := bufio.NewScanner(io.LimitReader(c, 1<<20))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		id, host := f[0], strings.ToLower(f[1])
		r.agentMu.Lock()
		persona, running := sl.persona, sl.busy
		r.agentMu.Unlock()
		r.holdBegin()
		wg.Go(func() {
			defer r.holdEnd()
			answer := "ERR"
			// Outside a turn no answer waits on it: refused as before.
			if running && r.holdDecision(r.ctx, host, persona) {
				answer = "OK"
			}
			wmu.Lock()
			defer wmu.Unlock()
			_, _ = fmt.Fprintf(c, "%s %s\n", id, answer)
		})
	}
}

// holdDecision asks the room about domain, refused to persona's turn, and waits for the AI host's
// answer: true once allowed. One question per domain at a time; a request for the same domain
// joins it (014-AC29).
func (r *roomRun) holdDecision(ctx context.Context, domain, persona string) bool {
	if covered(r.d.policy(r.room.String()), domain) {
		return true
	}
	r.holdMu.Lock()
	if r.holds == nil {
		r.holds, r.reported = map[string]*holdWait{}, map[string]time.Time{}
	}
	if w := r.holds[domain]; w != nil {
		r.holdMu.Unlock()
		select {
		case <-w.done:
			return w.ok
		case <-ctx.Done():
			return false
		}
	}
	w := &holdWait{done: make(chan struct{})}
	r.holds[domain], r.reported[domain] = w, time.Now()
	r.holdMu.Unlock()
	w.ok = r.askHold(ctx, domain, persona)
	r.holdMu.Lock()
	delete(r.holds, domain)
	r.holdMu.Unlock()
	close(w.done)
	return w.ok
}

// askHold posts the question and waits for the room's answer: "allowed", or why not.
func (r *roomRun) askHold(ctx context.Context, domain, persona string) bool {
	if _, err := sandbox.NormalizeAllowlist([]string{domain}); err != nil {
		return false
	}
	r.d.mu.Lock()
	connected := r.st.Connection == "connected"
	secs := holdSeconds(r.d.st.Egress.HoldSeconds)
	r.d.mu.Unlock()
	if !connected {
		return false
	}
	tok, err := r.d.o.Tokens.Get(r.room)
	if err != nil || tok == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(secs+15)*time.Second)
	defer cancel()
	resp, err := r.d.roomRequest(ctx, http.MethodPost, r.daemonURL("egress-hold"), tok, map[string]any{"domain": domain, "persona": persona, "seconds": secs})
	var out struct {
		Decision string `json:"decision"`
	}
	if err == nil {
		if resp.StatusCode != http.StatusOK {
			err = apiError(resp)
		} else {
			err = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&out)
		}
		_ = resp.Body.Close()
	}
	r.d.o.Log.Info("egress_hold", zap.String("room", r.room.String()), zap.String("domain", domain), zap.String("decision", out.Decision), zap.Error(err))
	return err == nil && out.Decision == "allowed"
}

// reportedSince drops from domains those a hold put before the room since since: they have their
// line already (014-AC8).
func (r *roomRun) reportedSince(domains []string, since time.Time) []string {
	r.holdMu.Lock()
	defer r.holdMu.Unlock()
	var out []string
	for _, d := range domains {
		if at, ok := r.reported[d]; !ok || at.Before(since) {
			out = append(out, d)
		}
	}
	return out
}

// HoldRootFor is where a worker of stateDir keeps its hold sockets: a short folder of its own in
// the user's runtime folder, or the temporary one, since a socket path is bounded and a state dir
// may be deep.
func HoldRootFor(stateDir string) string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.TempDir()
	}
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		abs = stateDir
	}
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join(base, "grill-room-hold-"+hex.EncodeToString(sum[:4]))
}

func (r *roomRun) holdBegin() {
	r.holdMu.Lock()
	r.answering++
	r.holdMu.Unlock()
}

// holdEnd counts an answer written; the last one applies a policy change held back meanwhile.
func (r *roomRun) holdEnd() {
	r.holdMu.Lock()
	r.answering--
	due := r.answering == 0 && r.policyDue
	if due {
		r.policyDue = false
	}
	r.holdMu.Unlock()
	if due {
		r.applyPolicy(r.ctx)
	}
}

// deferPolicy holds a policy change back while answers are pending, true when it did: squid's
// reconfigure drops the lookups in flight, a held request among them.
func (r *roomRun) deferPolicy() bool {
	r.holdMu.Lock()
	defer r.holdMu.Unlock()
	if r.answering == 0 {
		return false
	}
	r.policyDue = true
	return true
}

// applyPolicy hands the room's sandboxes their policy now.
func (r *roomRun) applyPolicy(ctx context.Context) {
	r.agentMu.Lock()
	defer r.agentMu.Unlock()
	p := r.d.policy(r.room.String())
	for _, sl := range r.sandboxes {
		if sl.ag != nil {
			if err := sl.ag.SetPolicy(ctx, p); err != nil {
				r.d.o.Log.Warn("policy_apply_failed", zap.String("room", r.room.String()), zap.Error(err))
			}
		}
	}
}
