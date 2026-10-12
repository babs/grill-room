package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/sandbox"
)

// SupervisorOptions wires a Supervisor. Zero durations take the defaults.
type SupervisorOptions struct {
	Paths Paths
	// Worker is the configuration every worker starts from; the supervisor fills in its instance.
	Worker  WorkerConfig
	Version string
	// Binary returns the checked worker binary for an instance (003-AC2, 003-AC7). An errRefused
	// stops the worker until something changes; any other error is retried with backoff.
	Binary func(ctx context.Context, in Instance) (Resolved, error)
	// Bins holds the trust material: the trusted root, the dev keys and the downloads. Nil turns
	// off the self-update, the dev keys and the signature indicator.
	Bins *Binaries
	// Executable is the supervisor's own binary, which a self-update replaces.
	Executable string
	// GitHub is the project's repository URL, the default update channel.
	GitHub string
	// UpdateEvery is how often the update channel is checked.
	UpdateEvery time.Duration
	// Env is the workers' environment.
	Env []string
	// Stderr receives the workers' stderr and their log lines, which Ring keeps too.
	Stderr io.Writer
	Ring   *Ring
	Log    *zap.Logger
	// Docker removes a crashed worker's sandboxes; nil leaves them for the next run of that room.
	Docker sandbox.Runner
	// Restart ends the supervisor so that main starts the updated binary.
	Restart   func()
	Autostart *Autostart

	MaxBackoff     time.Duration // between restarts of a crashing worker
	HelloTimeout   time.Duration // for a started worker to say hello
	StopTimeout    time.Duration // for a worker to stop before it is killed
	RequestTimeout time.Duration // for a worker's reply
}

// Supervisor runs one worker per instance and serves the admin page for all of them.
type Supervisor struct {
	o   SupervisorOptions
	ctx context.Context
	wg  sync.WaitGroup
	// logMu serialises the worker log lines written to Stderr.
	logMu sync.Mutex

	mu        sync.Mutex
	workers   map[string]*worker
	current   string
	autostart bool
	set       SupervisorSettings
	upd       selfUpdate
	self      SelfSignature
	// checkNow asks the update loop for a check.
	checkNow chan struct{}
}

// worker is one instance's worker process, restarted with backoff while the supervisor runs. Its
// fields are guarded by the supervisor's mu.
type worker struct {
	s    *Supervisor
	inst Instance
	// state is "starting", "running", "restarting", "refused" or "stopped".
	state    string
	err      string
	pid      int
	proto    int
	restarts int
	status   *Status
	metrics  *Metrics
	lastTurn *TurnEvent
	conn     *Conn
	pending  map[int64]chan Msg
	nextID   int64
	// pair is a room to pair with once the worker runs.
	pair string
	// version and trust describe the binary the worker runs; served is the version its instance
	// hands out, as last seen.
	version, trust, devKey, served string
	// reload restarts the worker at once, for a new binary or a new trust policy.
	reload bool
}

// InstanceView is an instance as the admin page lists it.
type InstanceView struct {
	ID       string   `json:"id"`
	URL      string   `json:"url"`
	Rooms    []string `json:"rooms"`
	State    string   `json:"state"`
	Error    string   `json:"error,omitempty"`
	PID      int      `json:"pid,omitempty"`
	Proto    int      `json:"proto,omitempty"`
	Restarts int      `json:"restarts"`
	Version  string   `json:"version,omitempty"`
	Trust    string   `json:"trust,omitempty"`
	// AcceptUnsigned is the relax toggle: the instance's binaries run even unsigned (003-AC4).
	AcceptUnsigned bool       `json:"accept_unsigned"`
	Room           string     `json:"room,omitempty"`
	Connection     string     `json:"connection,omitempty"`
	Metrics        *Metrics   `json:"metrics,omitempty"`
	LastTurn       *TurnEvent `json:"last_turn,omitempty"`
}

// errRefused is a worker the handshake turned down; restarting it would not help.
type errRefused struct{ reason string }

func (e errRefused) Error() string { return e.reason }

// NewSupervisor builds a supervisor; Start runs it.
func NewSupervisor(o SupervisorOptions) *Supervisor {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.MaxBackoff, 30*time.Second)
	def(&o.HelloTimeout, 10*time.Second)
	def(&o.StopTimeout, 45*time.Second)
	def(&o.RequestTimeout, 30*time.Second)
	def(&o.UpdateEvery, 6*time.Hour)
	if o.GitHub == "" {
		o.GitHub = "https://github.com/babs/grill-room"
	}
	if o.Ring == nil {
		o.Ring = NewRing(200)
	}
	s := &Supervisor{o: o, workers: map[string]*worker{}, checkNow: make(chan struct{}, 1), self: SelfSignature{State: "unknown", Detail: "not checked yet"}}
	set, err := loadSupervisorSettings(o.Paths.supervisorSettings())
	if err != nil {
		o.Log.Warn("supervisor_settings_unreadable", zap.Error(err))
	}
	s.set = set
	if o.Autostart != nil {
		on, err := o.Autostart.Enabled()
		if err != nil {
			o.Log.Warn("autostart_unknown", zap.Error(err))
		}
		s.autostart = on
	}
	return s
}

// Start runs a worker for every known instance until ctx ends. The newest instance is the one
// the page shows first.
func (s *Supervisor) Start(ctx context.Context) {
	s.ctx = ctx
	insts, errs := LoadInstances(s.o.Paths)
	for _, err := range errs {
		s.o.Log.Error("instance_unreadable", zap.Error(err))
	}
	var newest time.Time
	for _, in := range insts {
		if fi, err := os.Stat(s.o.Paths.InstanceConfig(in.ID)); err == nil && !fi.ModTime().Before(newest) {
			newest, s.current = fi.ModTime(), in.ID
		}
		s.start(in, "")
	}
	if s.o.Bins != nil {
		s.wg.Go(func() { s.updateLoop(ctx) })
	}
}

// start runs a worker for in, which has none yet.
func (s *Supervisor) start(in Instance, pair string) *worker {
	w := &worker{s: s, inst: in, state: "starting", pair: pair}
	s.mu.Lock()
	s.workers[in.ID] = w
	s.mu.Unlock()
	s.wg.Go(w.run)
	return w
}

// Wait returns once every worker has stopped; cancel Start's context first.
func (s *Supervisor) Wait() { s.wg.Wait() }

func (w *worker) run() {
	s := w.s
	log := s.o.Log.With(zap.String("instance", w.inst.ID))
	backoff := time.Second
	for {
		started := time.Now()
		err := w.once(log)
		if s.ctx.Err() != nil {
			w.set(func() { w.state, w.err = "stopped", "" })
			return
		}
		var refused errRefused
		s.mu.Lock()
		// One critical section with reload: a reload is either seen here or starts a new run.
		reload := w.reload
		w.reload = false
		if !reload && errors.As(err, &refused) {
			w.state, w.err = "refused", err.Error()
			w.version, w.trust, w.devKey = "", "", ""
		}
		s.mu.Unlock()
		if reload {
			log.Info("worker_reloading", zap.NamedError("exit", err))
			backoff = time.Second
			continue
		}
		if errors.As(err, &refused) {
			log.Error("worker_refused", zap.Error(err))
			return
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		log.Warn("worker_exited", zap.Error(err), zap.Duration("restart_in", backoff))
		w.set(func() {
			w.state, w.err = "restarting", fmt.Sprintf("the worker exited (%v): restarting in %s", err, backoff)
			w.restarts++
		})
		select {
		case <-s.ctx.Done():
			w.set(func() { w.state, w.err = "stopped", "" })
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, s.o.MaxBackoff)
	}
}

func (w *worker) set(f func()) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	f()
}

// once runs the worker process until it exits.
func (w *worker) once(log *zap.Logger) error {
	s := w.s
	var in Instance
	w.set(func() { w.state, in = "starting", w.inst })
	res, err := s.o.Binary(s.ctx, in)
	if err != nil {
		return fmt.Errorf("worker binary: %w", err)
	}
	s.mu.Lock()
	w.version, w.trust, w.devKey = res.Version, res.Trust, res.DevKey
	// Unknown when the instance did not answer: the version its worker reports stays the last seen.
	if res.Served != "" {
		w.served = res.Served
	}
	if res.Version != w.inst.Version {
		w.inst.Version = res.Version
		s.save(w)
	}
	s.mu.Unlock()
	if in.Relax {
		log.Warn("worker_accepts_unsigned", zap.String("version", res.Version), zap.String("trust", res.Trust))
	}
	// Not CommandContext: a stop message lets the worker remove its sandboxes first.
	cmd := exec.Command(res.Path, "worker", "--instance", w.inst.ID) //nolint:gosec,noctx // a binary Binary checked
	// The worker may run an instance's binary: its stderr is escaped like its log lines.
	cmd.Env, cmd.Stderr = s.o.Env, &lineWriter{emit: func(b []byte) { s.logLine(in.ID, map[string]any{"stream": "stderr", "msg": string(b)}) }}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start worker: %w", err)
	}
	log.Info("worker_started", zap.Int("pid", cmd.Process.Pid), zap.String("binary", res.Path), zap.String("version", res.Version))
	conn := NewConn(stdout, stdin)
	exited := make(chan struct{})
	go func() {
		select {
		case <-exited:
			return
		case <-s.ctx.Done():
		}
		_ = conn.Send(Msg{Type: "stop"})
		_ = stdin.Close()
		select {
		case <-exited:
		case <-time.After(s.o.StopTimeout):
			log.Warn("worker_killed", zap.String("reason", "it did not stop in time"))
			_ = cmd.Process.Kill()
		}
	}()
	err = w.serve(conn, cmd, log)
	// Whatever ended the conversation, the process must end too before Wait.
	_ = stdin.Close()
	kill := time.AfterFunc(s.o.StopTimeout, func() { _ = cmd.Process.Kill() })
	_, _ = io.Copy(io.Discard, stdout)
	werr := cmd.Wait()
	kill.Stop()
	close(exited)
	w.detach()
	if werr != nil && s.o.Docker != nil {
		// A clean exit removed its sandboxes; a crash or a kill left them.
		w.s.mu.Lock()
		names := sandboxNames(s.o.Worker.SandboxRoot, w.inst.Rooms)
		w.s.mu.Unlock()
		for _, n := range names {
			removeSandbox(context.WithoutCancel(s.ctx), s.o.Docker, n, log)
		}
	}
	if err == nil {
		err = werr
	}
	if err == nil {
		err = errors.New("exit status 0")
	}
	return err
}

// serve does the handshake, then dispatches the worker's messages until its stdout closes.
func (w *worker) serve(conn *Conn, cmd *exec.Cmd, log *zap.Logger) error {
	s := w.s
	timer := time.AfterFunc(s.o.HelloTimeout, func() { _ = cmd.Process.Kill() })
	hello, err := conn.Recv()
	timer.Stop()
	if err != nil {
		return fmt.Errorf("no hello from the worker: %w", err)
	}
	if hello.Type != "hello" {
		return fmt.Errorf("the worker sent %q before hello", hello.Type)
	}
	agreed, err := Negotiate(Proto, hello.Proto)
	if err != nil {
		reason := fmt.Sprintf("worker %s refused: %v", hello.Version, err)
		_ = conn.Send(Msg{Type: "refused", Proto: Proto, Error: reason})
		return errRefused{reason}
	}
	s.mu.Lock()
	cfg := s.o.Worker
	cfg.Instance = w.inst.ID
	cfg.ConfigDir, cfg.StateDir, cfg.CacheDir = s.o.Paths.InstanceConfig(w.inst.ID), s.o.Paths.InstanceState(w.inst.ID), s.o.Paths.InstanceCache(w.inst.ID)
	cfg.Rooms, cfg.Pair = slices.Clone(w.inst.Rooms), w.pair
	w.pair = ""
	w.state, w.err, w.pid, w.proto, w.conn, w.pending = "running", "", cmd.Process.Pid, agreed, conn, map[int64]chan Msg{}
	s.mu.Unlock()
	log.Info("worker_ready", zap.String("version", hello.Version), zap.Int("proto", agreed))
	if err := conn.Send(Msg{Type: "config", Proto: Proto, Version: s.o.Version, Config: &cfg}); err != nil {
		return fmt.Errorf("send config: %w", err)
	}
	var refused error
	for {
		m, err := conn.Recv()
		if err != nil {
			if refused != nil {
				return refused
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch m.Type {
		case "status":
			w.set(func() {
				w.status = m.Status
				// The instance moved to another version: run it (003-AC7).
				if sv := m.Status.Update.Served; sv != "" && sv != w.served {
					w.served = sv
					if s.set.UpdateChannel == "instance" && s.set.UpdateInstance == w.inst.ID {
						// The supervisor follows this instance: its new version is on offer now.
						select {
						case s.checkNow <- struct{}{}:
						default:
						}
					}
					if sv != w.version {
						log.Info("instance_version_changed", zap.String("served", sv), zap.String("running", w.version))
						s.reload(w)
					}
				}
			})
		case "metrics":
			w.set(func() { w.metrics = m.Metrics })
		case "turn":
			w.set(func() { w.lastTurn = m.Turn })
		case "log":
			w.s.logLine(w.inst.ID, m.Log)
		case "reply":
			w.set(func() {
				if ch := w.pending[m.ID]; ch != nil {
					delete(w.pending, m.ID)
					ch <- m
				}
			})
		case "refused":
			refused = errRefused{"the worker refused this supervisor: " + m.Error}
		}
	}
}

// detach fails the requests still waiting on a worker that is gone.
func (w *worker) detach() {
	w.set(func() {
		for id, ch := range w.pending {
			ch <- Msg{ID: id, Error: "the worker stopped"}
		}
		w.conn, w.pending, w.pid, w.status = nil, nil, 0, nil
	})
}

// logLine writes a worker's log entry, re-encoded by safeLogLine: a worker may run a binary its
// instance served, and the terminal must not run what it prints.
func (s *Supervisor) logLine(instance string, entry any) {
	line := safeLogLine(instance, entry)
	_, _ = s.o.Ring.Write(line)
	s.logMu.Lock()
	defer s.logMu.Unlock()
	_, _ = s.o.Stderr.Write(append(line, '\n'))
}

// request sends m to the worker and waits for its reply, for a bounded time: a hung worker must
// not hang the page.
func (w *worker) request(ctx context.Context, m Msg) (Msg, error) {
	s := w.s
	ctx, cancel := context.WithTimeout(ctx, s.o.RequestTimeout)
	defer cancel()
	s.mu.Lock()
	if w.state != "running" || w.conn == nil {
		state := w.describe()
		s.mu.Unlock()
		return Msg{}, errors.New(state)
	}
	w.nextID++
	m.ID = w.nextID
	ch := make(chan Msg, 1)
	w.pending[m.ID] = ch
	conn := w.conn
	s.mu.Unlock()
	drop := func() {
		w.set(func() {
			if w.pending != nil {
				delete(w.pending, m.ID)
			}
		})
	}
	if err := conn.Send(m); err != nil {
		drop()
		return Msg{}, fmt.Errorf("the worker for %s: %w", w.inst.URL, err)
	}
	select {
	case r := <-ch:
		if r.Error != "" {
			return r, errors.New(r.Error)
		}
		return r, nil
	case <-ctx.Done():
		drop()
		return Msg{}, fmt.Errorf("the worker for %s did not answer: %w", w.inst.URL, ctx.Err())
	}
}

// describe says why the worker is not running; the caller holds mu.
func (w *worker) describe() string {
	if w.err != "" {
		return w.err
	}
	return "the worker for " + w.inst.URL + " is " + w.state
}

// currentWorker is the worker the page shows, nil before any room.
func (s *Supervisor) currentWorker() *worker {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.workers[s.current]
}

func (s *Supervisor) requestCurrent(ctx context.Context, m Msg) (Msg, error) {
	w := s.currentWorker()
	if w == nil {
		return Msg{}, errors.New("no room yet: pair with a room first")
	}
	return w.request(ctx, m)
}

// SetRoom hands room to its instance's worker, starting the worker if the instance is new.
func (s *Supervisor) SetRoom(ctx context.Context, room Room) {
	id, err := InstanceID(room.Base)
	if err != nil {
		s.o.Log.Error("room_refused", zap.Error(err))
		return
	}
	s.mu.Lock()
	w := s.workers[id]
	fresh := w == nil
	var in Instance
	if fresh {
		in = Instance{ID: id, URL: room.Base}
		in.addRoom(room.String())
	} else {
		w.inst.addRoom(room.String())
		in = w.inst
		in.Rooms = slices.Clone(in.Rooms)
	}
	s.current = id
	running := !fresh && w.state == "running"
	if !fresh && !running {
		w.pair = room.String()
	}
	if fresh {
		s.saveInstance(in)
	} else {
		s.save(w)
	}
	s.mu.Unlock()
	s.o.Log.Info("room_handed", zap.String("instance", id), zap.String("room", room.String()))
	if fresh {
		s.start(in, room.String())
		return
	}
	if running {
		if _, err := w.request(ctx, Msg{Type: "pair", Room: room.String()}); err != nil {
			s.o.Log.Warn("pair_failed", zap.String("instance", id), zap.Error(err))
		}
	}
}

// Status is the current instance's worker status, or what the page shows before any room.
func (s *Supervisor) Status() (st Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st = Status{Host: s.o.Worker.Host, Version: s.o.Version, Connection: "offline", Allowlist: []string{}}
	// The update on offer is the supervisor's own, from its channel (003-AC5).
	defer func() { st.Update = s.upd.view() }()
	w := s.workers[s.current]
	if w == nil {
		return st
	}
	if w.status != nil && w.state == "running" {
		st = *w.status
		// The page names the supervisor's version; the worker's is listed with its instance.
		st.Version = s.o.Version
		return st
	}
	if n := len(w.inst.Rooms); n > 0 {
		st.Room = w.inst.Rooms[n-1]
	}
	if w.state != "running" && w.state != "starting" {
		st.Error = w.describe()
	}
	return st
}

// Instances lists every instance, and the id of the one the page shows.
func (s *Supervisor) Instances() ([]InstanceView, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]InstanceView, 0, len(s.workers))
	for _, w := range s.workers {
		v := InstanceView{ID: w.inst.ID, URL: w.inst.URL, Rooms: slices.Clone(w.inst.Rooms), State: w.state, Error: w.err, PID: w.pid,
			Proto: w.proto, Restarts: w.restarts, Metrics: w.metrics, LastTurn: w.lastTurn, Version: w.version, Trust: w.trust,
			AcceptUnsigned: w.inst.Relax}
		if w.status != nil {
			v.Room, v.Connection = w.status.Room, w.status.Connection
		}
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b InstanceView) int { return cmp.Compare(a.URL, b.URL) })
	return out, s.current
}

// SetSettings saves the current instance's host cap and approval mode.
func (s *Supervisor) SetSettings(set Settings) error {
	set, err := set.Normalize()
	if err != nil {
		return err
	}
	_, err = s.requestCurrent(s.ctx, Msg{Type: "settings", Settings: &set})
	return err
}

// SetAllowlist saves the current instance's egress allowlist.
func (s *Supervisor) SetAllowlist(ctx context.Context, list []string) error {
	_, err := s.requestCurrent(ctx, Msg{Type: "allowlist", Allowlist: list})
	return err
}

// Denied lists the egress requests the current room's sandbox was refused.
func (s *Supervisor) Denied(ctx context.Context, n int) ([]sandbox.Denied, error) {
	if s.currentWorker() == nil {
		return []sandbox.Denied{}, nil
	}
	r, err := s.requestCurrent(ctx, Msg{Type: "denied", N: n})
	if r.Denied == nil {
		r.Denied = []sandbox.Denied{}
	}
	return r.Denied, err
}

// Recheck re-runs the current worker's readiness checks.
func (s *Supervisor) Recheck(ctx context.Context) {
	if _, err := s.requestCurrent(ctx, Msg{Type: "recheck"}); err != nil {
		s.o.Log.Warn("recheck_failed", zap.Error(err))
	}
}

// Restart ends the supervisor so that main starts the updated binary.
func (s *Supervisor) Restart() {
	if s.o.Restart != nil {
		s.o.Restart()
	}
}

// Autostart reports whether the supervisor starts at login.
func (s *Supervisor) Autostart() (bool, error) {
	if s.o.Autostart == nil {
		return false, errors.New("autostart is not available here")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.autostart, nil
}

// SetAutostart installs or removes the login item (003-AC11).
func (s *Supervisor) SetAutostart(on bool) error {
	a := s.o.Autostart
	if a == nil {
		return errors.New("autostart is not available here")
	}
	var err error
	if on {
		err = a.Enable()
	} else {
		err = a.Disable()
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.autostart = on
	s.mu.Unlock()
	s.o.Log.Info("autostart_changed", zap.Bool("enabled", on))
	return nil
}
