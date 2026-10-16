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
	agreed, err := Negotiate(Proto, m.Proto)
	if err != nil {
		_ = conn.Send(Msg{Type: "refused", Proto: Proto, Error: fmt.Sprintf("worker %s: supervisor %v", version, err)})
		return WorkerConfig{}, fmt.Errorf("this worker refuses the supervisor %s: %w", m.Version, err)
	}
	if m.Config.Instance != instance {
		return WorkerConfig{}, fmt.Errorf("the supervisor sent the config of instance %s to the worker of %s", m.Config.Instance, instance)
	}
	cfg := *m.Config
	cfg.agreed = agreed
	return cfg, nil
}

// ServeWorker is the worker side of docs/daemon-protocol.md once the handshake is done: it runs d
// for the supervisor on conn. It returns once the supervisor says stop or closes the pipe, or ctx
// ends, and every room loop has removed its sandbox.
func ServeWorker(ctx context.Context, conn *Conn, d *Daemon, cfg WorkerConfig) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	started := time.Now()
	var sl *slots
	// A supervisor before version 3 knows no slots: the turns then run outside the host's cap.
	if cfg.agreed >= protoSlots {
		sl = &slots{conn: conn, waiting: map[int64]chan bool{}}
		d.acquire = sl.acquire
	}
	var wg sync.WaitGroup
	wg.Go(func() { d.Run(ctx) })
	// No removal runs before the requests loop: SetRoom cannot refuse here.
	for _, room := range d.startRooms(cfg) {
		_ = d.SetRoom(ctx, room)
	}
	st := &statusSender{conn: conn, d: d}
	wg.Go(func() { st.push(ctx) })
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
			if (m.Type == "wait" || m.Type == "grant") && sl != nil {
				sl.answer(m)
				continue
			}
			wg.Go(func() {
				reply := d.handle(ctx, m)
				// The status goes before the reply: whoever reloads on the reply reads the change.
				if err := st.send(); err != nil {
					d.o.Log.Warn("status_failed", zap.String("request", m.Type), zap.Error(err))
				}
				if err := conn.Send(reply); err != nil {
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

// startRooms are the rooms to serve at start: every one holding a token, oldest first, then the
// one to pair, which the page shows.
func (d *Daemon) startRooms(cfg WorkerConfig) []Room {
	var out []Room
	for _, u := range cfg.Rooms {
		r, err := ParseRoomURL(u)
		if err != nil || u == cfg.Pair {
			continue
		}
		if tok, err := d.o.Tokens.Get(r); err == nil && tok != "" {
			out = append(out, r)
		}
	}
	if cfg.Pair != "" {
		r, err := ParseRoomURL(cfg.Pair)
		if err != nil {
			d.o.Log.Warn("pair_refused", zap.Error(err))
			return out
		}
		out = append(out, r)
	}
	return out
}

// slots is the worker's side of the host's global cap (003-AC18): before each turn it asks the
// supervisor for a slot, and waits in the supervisor's queue until one is granted.
type slots struct {
	conn *Conn
	mu   sync.Mutex
	next int64
	// waiting hears false for "wait" and true for "grant", by slot.
	waiting map[int64]chan bool
}

// acquire waits for a slot for turn of room; onWait runs once the supervisor queues it. The
// returned function frees the slot, or leaves the queue.
func (s *slots) acquire(ctx context.Context, room string, turn int64, onWait func()) (func(), error) {
	s.mu.Lock()
	s.next++
	id := s.next
	ch := make(chan bool, 2)
	s.waiting[id] = ch
	s.mu.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.waiting, id)
			s.mu.Unlock()
			_ = s.conn.Send(Msg{Type: "release", Slot: id})
		})
	}
	if err := s.conn.Send(Msg{Type: "acquire", Slot: id, Turn: &TurnEvent{Room: room, ID: turn}}); err != nil {
		release()
		return nil, err
	}
	waited := false
	for {
		select {
		case granted := <-ch:
			if granted {
				return release, nil
			}
			if !waited {
				waited = true
				onWait()
			}
		case <-ctx.Done():
			release()
			return nil, context.Cause(ctx)
		}
	}
}

// answer hands the supervisor's wait or grant to the turn asking.
func (s *slots) answer(m Msg) {
	s.mu.Lock()
	ch := s.waiting[m.Slot]
	s.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- m.Type == "grant":
	default:
	}
}

// handle answers one supervisor request.
func (d *Daemon) handle(ctx context.Context, m Msg) Msg {
	out := Msg{Type: "reply", ID: m.ID}
	var err error
	switch m.Type {
	case "pair":
		var r Room
		if r, err = ParseRoomURL(m.Room); err == nil {
			err = d.SetRoom(ctx, r)
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
		var eg sandbox.Egress
		eg, err = d.Egress(ctx, m.N)
		out.Denied, out.Hits, out.RoomHits = eg.Denied, eg.Hits, eg.RoomHits
	case "connector_event":
		if m.Connector == nil {
			err = errors.New("connector missing")
		} else {
			d.tellConnector(ctx, m.Room, *m.Connector)
		}
	case "room_allowlist_remove":
		err = d.RemoveRoomEgress(ctx, m.Room, m.Domain)
	case "egress_switches":
		if m.Switches == nil {
			err = errors.New("switches missing")
		} else {
			err = d.SetEgressSwitches(ctx, *m.Switches)
		}
	case "recheck":
		d.Recheck(ctx)
	case "access":
		if m.Access == nil {
			err = errors.New("access missing")
		} else {
			d.SetAccess(*m.Access)
		}
	case "plugin_options":
		err = d.SetPluginOptions(ctx, m.Room, m.Plugin, m.Values, m.Clear)
	case "plugin_fetch":
		err = d.PluginFetch(ctx, m.Room, m.Fetch, m.Accept)
	case "remove_room":
		err = d.RemoveRoom(ctx, m.Room, m.Force)
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

// statusSender sends the worker's status. One lock spans reading and sending it: a status sent
// later was read later, so a ticker's older copy never lands after a request's newer one.
type statusSender struct {
	conn *Conn
	d    *Daemon
	mu   sync.Mutex
	last []byte
}

// send sends the status if it changed since the last one sent.
func (s *statusSender) send() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.d.Status()
	b, err := json.Marshal(st)
	if err != nil || bytes.Equal(b, s.last) {
		return nil
	}
	if err := s.conn.Send(Msg{Type: "status", Status: &st}); err != nil {
		return err
	}
	s.last = b
	return nil
}

// push sends the status whenever it changes.
func (s *statusSender) push(ctx context.Context) {
	tick := time.NewTicker(statusEvery)
	defer tick.Stop()
	for {
		if s.send() != nil {
			return
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

// SlotSandboxName names sandbox slot of a room: the room's first keeps SandboxName, the ones its
// parallel turns add get a suffix (003-AC20).
func SlotSandboxName(root string, r Room, slot int) string {
	if slot == 0 {
		return SandboxName(root, r)
	}
	return fmt.Sprintf("%s-%d", SandboxName(root, r), slot)
}

// RootLabel marks every Docker object of the sandboxes under one state root, so that a
// supervisor finds the ones a crash left, and never another's (003-AC8).
func RootLabel(root string) string {
	sum := sha256.Sum256([]byte(root))
	return "grill-room-root=" + hex.EncodeToString(sum[:6])
}

// InstanceLabel marks the Docker objects of one instance's sandboxes.
func InstanceLabel(instance string) string { return "grill-room-instance=" + instance }

// sandboxNames lists the first sandbox of each room, for the cleanup of workers that predate the
// labels.
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
