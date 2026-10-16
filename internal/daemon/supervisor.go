package daemon

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/monitor"
	"github.com/babs/grill-room/internal/sandbox"
)

// SupervisorOptions wires a Supervisor. Zero durations take the defaults.
type SupervisorOptions struct {
	Paths Paths
	// Host is this machine's name, shown on the admin page only.
	Host string
	// Accounts are the Claude accounts and their proxies; nil runs workers with no Claude access.
	Accounts *Accounts
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
	// Monitor records every turn the workers end (003-AC27); nil records none.
	Monitor *monitor.DB
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

	mu      sync.Mutex
	workers map[string]*worker
	// removing holds the rooms a RemoveRoom is taking off: SetRoom refuses them meanwhile.
	removing  map[string]bool
	current   string
	autostart bool
	// autoMu orders changes of the login item: the page's switch and a self-update's move.
	autoMu sync.Mutex
	set    SupervisorSettings
	// gate holds the global cap on concurrent turns (003-AC18).
	gate *gate
	upd  selfUpdate
	self SelfSignature
	// checkNow asks the update loop for a check.
	checkNow chan struct{}
	// cleaned closes once the orphans of a killed supervisor are gone: workers wait for it.
	cleaned chan struct{}
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
	// unsigned is a binary nobody signed, run under the relax toggle.
	unsigned bool
	// reload restarts the worker at once, for a new binary or a new trust policy.
	reload bool
	// sentAccess is the last access the worker took, as JSON; accessMu orders the pushes.
	sentAccess []byte
	accessMu   sync.Mutex
	// ended closes once the current run returns: a refused or stopped worker not restarted. A
	// refused worker started again gets a new one.
	ended chan struct{}
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
	AcceptUnsigned bool `json:"accept_unsigned"`
	// Account is the instance's default account; RoomAccounts the rooms that use another (003-AC13).
	Account      string            `json:"account"`
	RoomAccounts map[string]string `json:"room_accounts,omitempty"`
	// RoomStates are the rooms the worker serves, with their connection and turns.
	RoomStates []RoomStatus `json:"room_states,omitempty"`
	// Requests counts what the worker sent through the accounts' proxies; CutOff shuts it out (003-AC15).
	Requests   int64      `json:"requests"`
	CutOff     bool       `json:"cut_off,omitempty"`
	Room       string     `json:"room,omitempty"`
	Connection string     `json:"connection,omitempty"`
	Metrics    *Metrics   `json:"metrics,omitempty"`
	LastTurn   *TurnEvent `json:"last_turn,omitempty"`
	// Unsigned is a running worker whose binary nobody signed (003-AC29).
	Unsigned bool `json:"unsigned,omitempty"`
	// Limits are its sandboxes' CPUs and memory (003-AC26).
	Limits sandbox.Limits `json:"limits"`
	// Status is the running worker's own view: readiness, host cap, allowlist (003-AC26).
	Status *Status `json:"status,omitempty"`
	// Paused are the rooms the host paused (003b-AC5).
	Paused []string `json:"paused,omitempty"`
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
	s := &Supervisor{o: o, workers: map[string]*worker{}, removing: map[string]bool{}, checkNow: make(chan struct{}, 1), cleaned: make(chan struct{}), self: SelfSignature{State: "unknown", Detail: "not checked yet"}}
	set, err := loadSupervisorSettings(o.Paths.supervisorSettings())
	if err != nil {
		o.Log.Warn("supervisor_settings_unreadable", zap.Error(err))
	}
	s.set = set
	s.gate = newGate(set.maxTurns())
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
// the page shows first. The workers start once the sandboxes a killed supervisor left are
// removed; the page does not wait for those Docker calls.
func (s *Supervisor) Start(ctx context.Context) {
	s.ctx = ctx
	insts, errs := LoadInstances(s.o.Paths)
	for _, err := range errs {
		s.o.Log.Error("instance_unreadable", zap.Error(err))
	}
	s.wg.Go(func() {
		defer close(s.cleaned)
		s.removeOrphans(ctx, insts)
	})
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
	if s.o.Accounts != nil {
		s.wg.Go(func() { s.accountsLoop(ctx) })
	}
	if s.o.Monitor != nil {
		s.wg.Go(func() { s.purgeLoop(ctx) })
	}
}

// start runs a worker for in, which has none yet.
func (s *Supervisor) start(in Instance, pair string) *worker {
	w := &worker{s: s, inst: in, state: "starting", pair: pair, ended: make(chan struct{})}
	s.mu.Lock()
	s.workers[in.ID] = w
	ended := w.ended
	s.mu.Unlock()
	s.wg.Go(func() { w.run(ended) })
	return w
}

// Wait returns once every worker has stopped; cancel Start's context first.
func (s *Supervisor) Wait() { s.wg.Wait() }

// run runs the worker until it is refused or the supervisor stops, then closes ended.
func (w *worker) run(ended chan struct{}) {
	defer close(ended)
	s := w.s
	select {
	case <-s.cleaned:
	case <-s.ctx.Done():
		w.set(func() { w.state = "stopped" })
		return
	}
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
			w.version, w.trust, w.devKey, w.unsigned = "", "", "", false
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
	w.version, w.trust, w.devKey, w.unsigned = res.Version, res.Trust, res.DevKey, res.Relaxed
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
		in := w.inst
		in.Rooms = slices.Clone(in.Rooms)
		w.s.mu.Unlock()
		s.removeSandboxes(context.WithoutCancel(s.ctx), log, []Instance{in}, InstanceLabel(in.ID))
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
	// Before the worker shows as running: a request sent ahead of the config would kill it, and
	// finding where the proxies listen can take a Docker call. A room added meanwhile gets its
	// proxy with the next access push.
	var access *Access
	if agreed >= 2 && s.o.Accounts != nil {
		s.mu.Lock()
		in := w.inst
		in.Rooms, in.RoomAccounts, in.Paused = slices.Clone(in.Rooms), maps.Clone(in.RoomAccounts), slices.Clone(in.Paused)
		s.mu.Unlock()
		a := s.access(in)
		access = &a
	}
	s.mu.Lock()
	cfg := s.o.Worker
	cfg.Instance = w.inst.ID
	cfg.ConfigDir, cfg.StateDir, cfg.CacheDir = s.o.Paths.InstanceConfig(w.inst.ID), s.o.Paths.InstanceState(w.inst.ID), s.o.Paths.InstanceCache(w.inst.ID)
	cfg.Rooms, cfg.Pair, cfg.Access = slices.Clone(w.inst.Rooms), w.pair, access
	if w.inst.Limits != nil {
		cfg.Limits = *w.inst.Limits
	}
	w.pair = ""
	w.state, w.err, w.pid, w.proto, w.conn, w.pending = "running", "", cmd.Process.Pid, agreed, conn, map[int64]chan Msg{}
	w.sentAccess = nil
	if access != nil {
		w.sentAccess, _ = json.Marshal(access)
	}
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
			// Docker may have come up, or the room changed: the proxies follow.
			go s.pushAccess(w)
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
		case "acquire":
			room, turn := "", int64(0)
			if m.Turn != nil {
				room, turn = m.Turn.Room, m.Turn.ID
			}
			s.gate.acquire(slotKey{w.inst.ID, m.Slot}, room, turn, conn.Send)
		case "release":
			s.gate.release(slotKey{w.inst.ID, m.Slot})
		case "metrics":
			w.set(func() { w.metrics = m.Metrics })
		case "turn":
			w.set(func() { w.lastTurn = m.Turn })
			s.record(w, m.Turn)
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

// removeOrphans removes the sandboxes, egress proxies and networks a killed supervisor left: every
// Docker object carrying this state root's label, and the first sandbox of each known room, as
// workers before the labels named it. Another state root's, or another project's, carry neither.
func (s *Supervisor) removeOrphans(ctx context.Context, insts []Instance) {
	if s.o.Docker == nil {
		return
	}
	if ready := sandbox.Check(ctx, s.o.Docker); !ready.OK {
		return
	}
	s.removeSandboxes(ctx, s.o.Log, insts)
}

// removeSandboxes removes the Docker objects labelled with this state root and labels, and the
// first sandbox of each room of insts.
func (s *Supervisor) removeSandboxes(ctx context.Context, log *zap.Logger, insts []Instance, labels ...string) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	n, err := sandbox.RemoveLabelled(ctx, s.o.Docker, append([]string{RootLabel(s.o.Worker.SandboxRoot)}, labels...)...)
	if err != nil {
		log.Warn("sandbox_cleanup_failed", zap.Error(err))
	}
	if n > 0 {
		log.Info("sandboxes_removed", zap.Int("objects", n))
	}
	for _, in := range insts {
		for _, name := range sandboxNames(s.o.Worker.SandboxRoot, in.Rooms) {
			removeSandbox(ctx, s.o.Docker, name, log)
		}
	}
}

// detach fails the requests still waiting on a worker that is gone, and frees its slots.
func (w *worker) detach() {
	w.s.gate.drop(w.inst.ID)
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
		return Msg{}, fmt.Errorf("the app for %s: %w", w.inst.URL, err)
	}
	select {
	case r := <-ch:
		if r.Error != "" {
			return r, errors.New(r.Error)
		}
		return r, nil
	case <-ctx.Done():
		drop()
		return Msg{}, fmt.Errorf("the app for %s did not answer: %w", w.inst.URL, ctx.Err())
	}
}

func (w *worker) isStarting() bool {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	return w.state == "starting"
}

// describe says why the worker is not running; the caller holds mu.
func (w *worker) describe() string {
	if w.err != "" {
		return w.err
	}
	return "the app for " + w.inst.URL + " is " + w.state
}

// currentWorker is the worker the page shows, nil before any room.
func (s *Supervisor) currentWorker() *worker {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.workers[s.current]
}

// instanceOps is what the page changes on one instance: its host cap, its egress allowlist, its
// denied list and its readiness checks (003-AC26).
type instanceOps interface {
	SetSettings(s Settings) error
	SetAllowlist(ctx context.Context, list []string) error
	Egress(ctx context.Context, n int) (sandbox.Egress, error)
	Recheck(ctx context.Context)
	// SetPluginOptions and PluginFetch act on a room's plugins (009-AC19, 009-AC22).
	SetPluginOptions(ctx context.Context, room string, plugin int64, values map[string]any, clear []string) error
	PluginFetch(ctx context.Context, room string, id int64, accept bool) error
}

// workerOps drives one instance's worker; an empty id is the instance the page shows.
type workerOps struct {
	s  *Supervisor
	id string
}

// InstanceOps is the page's handle on instance id.
func (s *Supervisor) InstanceOps(id string) (instanceOps, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workers[id] == nil {
		return nil, errors.New("no such server")
	}
	return workerOps{s, id}, nil
}

func (o workerOps) worker() *worker {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	if o.id == "" {
		return o.s.workers[o.s.current]
	}
	return o.s.workers[o.id]
}

func (o workerOps) request(ctx context.Context, m Msg) (Msg, error) {
	w := o.worker()
	if w == nil {
		return Msg{}, errors.New("no room yet: pair with a room first")
	}
	return w.request(ctx, m)
}

// SetSettings saves the instance's host cap and approval mode.
func (o workerOps) SetSettings(set Settings) error {
	set, err := set.Normalize()
	if err != nil {
		return err
	}
	_, err = o.request(o.s.ctx, Msg{Type: "settings", Settings: &set})
	return err
}

// SetAllowlist saves the instance's egress allowlist.
func (o workerOps) SetAllowlist(ctx context.Context, list []string) error {
	_, err := o.request(ctx, Msg{Type: "allowlist", Allowlist: list})
	return err
}

// Egress lists the egress requests the instance's sandboxes were refused, and those let through by
// host; a worker from before 003b sends no hits.
func (o workerOps) Egress(ctx context.Context, n int) (sandbox.Egress, error) {
	out := sandbox.Egress{Denied: []sandbox.Denied{}, Hits: map[string]int{}}
	// No worker, or one still starting, has refused nothing yet.
	if w := o.worker(); w == nil || w.isStarting() {
		return out, nil
	}
	r, err := o.request(ctx, Msg{Type: "denied", N: n})
	if r.Denied != nil {
		out.Denied = r.Denied
	}
	if r.Hits != nil {
		out.Hits = r.Hits
	}
	return out, err
}

// pluginRequest sends a plugin request to a worker that takes it: one from before spec 009 Phase 3
// would answer "unknown request".
func (o workerOps) pluginRequest(ctx context.Context, m Msg) error {
	w := o.worker()
	if w == nil {
		return errors.New("no room yet: pair with a room first")
	}
	o.s.mu.Lock()
	agreed := w.proto
	o.s.mu.Unlock()
	if agreed < protoPlugins {
		return errors.New("this server's app is too old for room plugins: update the AI host app")
	}
	_, err := o.request(ctx, m)
	return err
}

// SetPluginOptions saves the values the host gave a room plugin's options (009-AC22).
func (o workerOps) SetPluginOptions(ctx context.Context, room string, plugin int64, values map[string]any, clear []string) error {
	return o.pluginRequest(ctx, Msg{Type: "plugin_options", Room: room, Plugin: plugin, Values: values, Clear: clear})
}

// PluginFetch fetches or declines a room's request that this host fetch a repo (009-AC19).
func (o workerOps) PluginFetch(ctx context.Context, room string, id int64, accept bool) error {
	return o.pluginRequest(ctx, Msg{Type: "plugin_fetch", Room: room, Fetch: id, Accept: accept})
}

// Recheck re-runs the worker's readiness checks.
func (o workerOps) Recheck(ctx context.Context) {
	if _, err := o.request(ctx, Msg{Type: "recheck"}); err != nil {
		o.s.o.Log.Warn("recheck_failed", zap.Error(err))
	}
}

// SetRoom hands room to its instance's worker, starting the worker if the instance is new. A
// room being removed is refused: its pairing would outlive the removal.
func (s *Supervisor) SetRoom(ctx context.Context, room Room) error {
	id, err := InstanceID(room.Base)
	if err != nil {
		s.o.Log.Error("room_refused", zap.Error(err))
		return err
	}
	s.mu.Lock()
	if s.removing[room.String()] {
		s.mu.Unlock()
		return errRemoving
	}
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
		return nil
	}
	if running {
		if _, err := w.request(ctx, Msg{Type: "pair", Room: room.String()}); err != nil {
			s.o.Log.Warn("pair_failed", zap.String("instance", id), zap.Error(err))
			return err
		}
	}
	return nil
}

// Status is the current instance's worker status, or what the page shows before any room.
func (s *Supervisor) Status() (st Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st = Status{Host: s.o.Host, Version: s.o.Version, Connection: "offline", Allowlist: []string{}}
	// The update on offer is the supervisor's own, from its channel (003-AC5).
	defer func() { st.Update, st.Host = s.upd.view(), s.o.Host }()
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
			AcceptUnsigned: w.inst.Relax, Account: w.inst.accountFor(""), RoomAccounts: maps.Clone(w.inst.RoomAccounts), CutOff: w.inst.CutOff,
			Unsigned: w.unsigned && w.state == "running", Limits: s.o.Worker.Limits, Paused: slices.Clone(w.inst.Paused)}
		if w.inst.Limits != nil {
			v.Limits = *w.inst.Limits
		}
		if s.o.Accounts != nil {
			v.Requests = s.o.Accounts.Requests(w.inst.ID)
		}
		if w.status != nil {
			v.Room, v.Connection, v.RoomStates = w.status.Room, w.status.Connection, w.status.Rooms
			st := *w.status
			v.Status = &st
		}
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b InstanceView) int { return cmp.Compare(a.URL, b.URL) })
	return out, s.current
}

// SetSettings saves the current instance's host cap and approval mode.
func (s *Supervisor) SetSettings(set Settings) error { return workerOps{s: s}.SetSettings(set) }

// SetAllowlist saves the current instance's egress allowlist.
func (s *Supervisor) SetAllowlist(ctx context.Context, list []string) error {
	return workerOps{s: s}.SetAllowlist(ctx, list)
}

// Egress lists the egress requests of the current instance's sandboxes.
func (s *Supervisor) Egress(ctx context.Context, n int) (sandbox.Egress, error) {
	return workerOps{s: s}.Egress(ctx, n)
}

// Recheck re-runs the current worker's readiness checks.
func (s *Supervisor) Recheck(ctx context.Context) { workerOps{s: s}.Recheck(ctx) }

// roomOps is the worker of room's instance.
func (s *Supervisor) roomOps(room string) (workerOps, error) {
	r, err := ParseRoomURL(room)
	if err != nil {
		return workerOps{}, err
	}
	id, err := InstanceID(r.Base)
	if err != nil {
		return workerOps{}, err
	}
	return workerOps{s, id}, nil
}

// SetPluginOptions saves a room plugin's option values on the worker of the room's instance.
func (s *Supervisor) SetPluginOptions(ctx context.Context, room string, plugin int64, values map[string]any, clear []string) error {
	o, err := s.roomOps(room)
	if err != nil {
		return err
	}
	return o.SetPluginOptions(ctx, room, plugin, values, clear)
}

// PluginFetch answers a room's fetch request on the worker of the room's instance.
func (s *Supervisor) PluginFetch(ctx context.Context, room string, id int64, accept bool) error {
	o, err := s.roomOps(room)
	if err != nil {
		return err
	}
	return o.PluginFetch(ctx, room, id, accept)
}

// Restart ends the supervisor so that main starts the updated binary.
func (s *Supervisor) Restart() {
	if s.o.Restart != nil {
		s.o.Restart()
	}
}

// Autostart reports whether the supervisor starts at login.
func (s *Supervisor) Autostart() (bool, error) {
	if s.autostartItem() == nil {
		return false, errors.New("autostart is not available here")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.autostart, nil
}

// autostartItem is the login item, moved by a self-update that renames the binary.
func (s *Supervisor) autostartItem() *Autostart {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.o.Autostart
}

// SetAutostart installs or removes the login item (003-AC11).
func (s *Supervisor) SetAutostart(on bool) error {
	// A self-update moving the login item must not interleave with this change.
	s.autoMu.Lock()
	defer s.autoMu.Unlock()
	a := s.autostartItem()
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
