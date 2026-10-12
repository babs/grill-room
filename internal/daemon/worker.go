package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/sandbox"
)

// How often a worker compares its status with the last one sent, and sends its metrics.
var (
	statusEvery  = 250 * time.Millisecond
	metricsEvery = 30 * time.Second
)

// WorkerHandshake says hello as a worker of instance at version, and waits for the supervisor's
// config (003-AC9). Either side refuses a peer outside its supported protocol range.
func WorkerHandshake(conn *Conn, instance, version string) (WorkerConfig, error) {
	if err := conn.Send(Msg{Type: "hello", Proto: Proto, Version: version, Instance: instance}); err != nil {
		return WorkerConfig{}, fmt.Errorf("hello: %w", err)
	}
	m, err := conn.Recv()
	switch {
	case err != nil:
		return WorkerConfig{}, fmt.Errorf("no answer from the supervisor: %w", err)
	case m.Type == "refused":
		return WorkerConfig{}, fmt.Errorf("the supervisor refused this worker: %s", m.Error)
	case m.Type != "config" || m.Config == nil:
		return WorkerConfig{}, fmt.Errorf("the supervisor sent %q instead of a config", m.Type)
	}
	if _, err := Negotiate(Proto, m.Proto); err != nil {
		_ = conn.Send(Msg{Type: "refused", Proto: Proto, Error: fmt.Sprintf("worker %s: supervisor %v", version, err)})
		return WorkerConfig{}, fmt.Errorf("this worker refuses the supervisor %s: %w", m.Version, err)
	}
	if m.Config.Instance != instance {
		return WorkerConfig{}, fmt.Errorf("the supervisor sent the config of instance %s to the worker of %s", m.Config.Instance, instance)
	}
	return *m.Config, nil
}

// ServeWorker is the worker side of docs/daemon-protocol.md once the handshake is done: it runs d
// for the supervisor on conn. It returns once the supervisor says stop or closes the pipe, or ctx
// ends, and every room loop has removed its sandbox.
func ServeWorker(ctx context.Context, conn *Conn, d *Daemon, cfg WorkerConfig) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	started := time.Now()
	var wg sync.WaitGroup
	wg.Go(func() { d.Run(ctx) })
	if room, ok := d.startRoom(cfg); ok {
		d.SetRoom(ctx, room)
	}
	wg.Go(func() { pushStatus(ctx, conn, d) })
	wg.Go(func() { pushMetrics(ctx, conn, d, started) })

	// The reader outlives this function when stdin stays open; the process exits right after.
	msgs := make(chan Msg)
	go func() {
		defer close(msgs)
		for {
			m, err := conn.Recv()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					d.o.Log.Warn("supervisor_pipe_failed", zap.Error(err))
				}
				return
			}
			select {
			case msgs <- m:
			case <-ctx.Done():
				return
			}
		}
	}()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case m, ok := <-msgs:
			if !ok || m.Type == "stop" {
				break loop
			}
			wg.Go(func() {
				if err := conn.Send(d.handle(ctx, m)); err != nil {
					d.o.Log.Warn("reply_failed", zap.String("request", m.Type), zap.Error(err))
				}
			})
		}
	}
	d.o.Log.Info("worker_stopping")
	cancel()
	wg.Wait()
	d.Wait()
	return nil
}

// startRoom is the room to serve at start: the one to pair, else the newest one holding a token.
// ponytail: one room per worker until workers serve several rooms (spec 003 phase 5).
func (d *Daemon) startRoom(cfg WorkerConfig) (Room, bool) {
	if cfg.Pair != "" {
		r, err := ParseRoomURL(cfg.Pair)
		if err != nil {
			d.o.Log.Warn("pair_refused", zap.Error(err))
			return Room{}, false
		}
		return r, true
	}
	for i := len(cfg.Rooms) - 1; i >= 0; i-- {
		r, err := ParseRoomURL(cfg.Rooms[i])
		if err != nil {
			continue
		}
		if tok, err := d.o.Tokens.Get(r); err == nil && tok != "" {
			return r, true
		}
	}
	return Room{}, false
}

// handle answers one supervisor request.
func (d *Daemon) handle(ctx context.Context, m Msg) Msg {
	out := Msg{Type: "reply", ID: m.ID}
	var err error
	switch m.Type {
	case "pair":
		var r Room
		if r, err = ParseRoomURL(m.Room); err == nil {
			d.SetRoom(ctx, r)
		}
	case "settings":
		if m.Settings == nil {
			err = errors.New("settings missing")
		} else {
			err = d.SetSettings(*m.Settings)
		}
	case "allowlist":
		// An empty list travels as no field at all.
		list := m.Allowlist
		if list == nil {
			list = []string{}
		}
		err = d.SetAllowlist(ctx, list)
	case "denied":
		out.Denied, err = d.Denied(ctx, m.N)
	case "recheck":
		d.Recheck(ctx)
	case "access":
		if m.Access == nil {
			err = errors.New("access missing")
		} else {
			d.SetAccess(*m.Access)
		}
	case "update":
		// A worker runs a copy the supervisor verified: only the supervisor updates (003-AC5).
		err = errors.New("updates go through the supervisor")
	default:
		err = fmt.Errorf("unknown request %q", m.Type)
	}
	if err != nil {
		out.Error = err.Error()
	}
	return out
}

// pushStatus sends the status whenever it changes.
func pushStatus(ctx context.Context, conn *Conn, d *Daemon) {
	tick := time.NewTicker(statusEvery)
	defer tick.Stop()
	var last []byte
	for {
		st := d.Status()
		if b, err := json.Marshal(st); err == nil && !bytes.Equal(b, last) {
			if err := conn.Send(Msg{Type: "status", Status: &st}); err != nil {
				return
			}
			last = b
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func pushMetrics(ctx context.Context, conn *Conn, d *Daemon, started time.Time) {
	tick := time.NewTicker(metricsEvery)
	defer tick.Stop()
	for {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		m := Metrics{UptimeSeconds: int64(time.Since(started).Seconds()), Turns: d.turns.Load(), FailedTurns: d.failedTurns.Load(),
			HeapBytes: ms.HeapAlloc, Goroutines: runtime.NumGoroutine()}
		if err := conn.Send(Msg{Type: "metrics", Metrics: &m}); err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// LogWriter sends each log entry zap writes, one JSON object per Write, as a log message.
type LogWriter struct{ Conn *Conn }

func (w LogWriter) Write(p []byte) (int, error) {
	line := bytes.TrimRight(p, "\n")
	if !json.Valid(line) {
		b, _ := json.Marshal(map[string]string{"msg": string(line)})
		line = b
	}
	if err := w.Conn.Send(Msg{Type: "log", Log: bytes.Clone(line)}); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Sync is a no-op: each entry is sent as it is written.
func (LogWriter) Sync() error { return nil }

// SandboxName is stable per state root and room: a restarted worker finds and replaces the
// containers a crash left, and two supervisors on one machine never share one.
func SandboxName(root string, r Room) string {
	sum := sha256.Sum256([]byte(root + "\x00" + r.String()))
	return hex.EncodeToString(sum[:6])
}

// sandboxNames lists the sandboxes the rooms may have, for the cleanup after a worker crash.
func sandboxNames(root string, rooms []string) []string {
	var names []string
	for _, u := range rooms {
		if r, err := ParseRoomURL(u); err == nil {
			names = append(names, SandboxName(root, r))
		}
	}
	return names
}

// removeSandbox removes every Docker object of sandbox name, as its worker would have on a clean stop.
func removeSandbox(ctx context.Context, run sandbox.Runner, name string, log *zap.Logger) {
	sandbox.New(sandbox.Config{Name: name}, run, log).Close(ctx)
}
