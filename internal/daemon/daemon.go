// Package daemon is the host daemon: it pairs with a room, keeps an outbound connection to it,
// reports whether this machine can host the agent, and serves a local admin page.
package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/sandbox"
)

// Options wires a Daemon. Zero durations take the defaults. Nothing here names the machine, its
// user or its Claude account: what the daemon tells a room is free of them (003-AC17).
type Options struct {
	Version string
	Tokens  *Tokens
	Docker  sandbox.Runner
	HTTP    *http.Client
	Log     *zap.Logger
	// Access is the rooms' proxies at start (Daemon.SetAccess).
	Access Access
	// NewAgent builds a sandbox of room r, from image, when a turn finds none free: slot 0 is the
	// room's first, the others serve its parallel turns (003-AC20). proxy is the address of the
	// room's account proxy.
	NewAgent func(r Room, slot int, image, proxy string) Agent
	// Settings are the host cap and approval mode at start; SettingsPath is where the page saves
	// them, "" to keep them in memory.
	Settings     Settings
	SettingsPath string
	// Allowlist is the egress allowlist at start, nil for the default; AllowlistPath is where the
	// page saves it, "" to keep it in memory.
	Allowlist     []string
	AllowlistPath string
	// Image pins the sandbox image; empty takes the room server's, or builds the embedded one.
	Image  string
	Stream sandbox.Streamer
	// CacheDir holds the per-room workspace caches, CacheMax their total size in bytes; an empty
	// dir or a cap of 0 fetches every workspace whole (002-AC37).
	CacheDir string
	CacheMax int64
	// PluginCacheDir keeps plugin snapshots by sha256; empty fetches each turn's (009-AC17).
	PluginCacheDir string
	// Labels, "key=value", mark the plugin inspection containers as the sandboxes' Labels do.
	Labels []string
	// OnTurn hears of every turn starting and ending; nil for none.
	OnTurn func(TurnEvent)

	CheckEvery  time.Duration // readiness re-check
	StatusEvery time.Duration // heartbeat to the room
	PollEvery   time.Duration // pairing answer poll
	MaxBackoff  time.Duration
}

// Status is what the local page shows. The room fields are those of the room handed over last.
type Status struct {
	// Host is this machine's name, set by the supervisor for its page only.
	Host    string `json:"host"`
	Version string `json:"version"`
	Room    string `json:"room,omitempty"`
	// Pairing is "", "requesting", "pending", "paired".
	Pairing    string `json:"pairing"`
	Code       string `json:"code,omitempty"`
	ApproveURL string `json:"approve_url,omitempty"`
	// Connection is "offline", "connecting" or "connected".
	Connection string            `json:"connection"`
	Error      string            `json:"error,omitempty"`
	Docker     sandbox.Readiness `json:"docker"`
	Image      Image             `json:"image"`
	Update     Update            `json:"update"`
	CanHost    bool              `json:"can_host"`
	Reason     string            `json:"reason,omitempty"`
	Settings   Settings          `json:"settings"`
	// Allowlist is the domains the sandbox may reach through its egress proxy.
	Allowlist []string `json:"allowlist"`
	// Rooms are every room the daemon serves, by link.
	Rooms []RoomStatus `json:"rooms,omitempty"`
}

// RoomStatus is one served room's pairing, connection and turns.
type RoomStatus struct {
	Room string `json:"room"`
	// Name is the room's name from the stream's hello, empty from a room server before 005-AC42.
	Name       string `json:"name,omitempty"`
	Pairing    string `json:"pairing"`
	Code       string `json:"code,omitempty"`
	ApproveURL string `json:"approve_url,omitempty"`
	Connection string `json:"connection"`
	Error      string `json:"error,omitempty"`
	CanHost    bool   `json:"can_host"`
	Reason     string `json:"reason,omitempty"`
	// Running counts the room's turns running now; Queued, those waiting for a slot under the
	// host's global cap (003-AC18).
	Running int `json:"running,omitempty"`
	Queued  int `json:"queued,omitempty"`
	// Host says whether this daemon hosts the room's agent, nil until the room says (a room server
	// from before spec 003 phase 7 never does); ParallelTurns is the room's parallel mode (003-AC26).
	Host          *bool `json:"host,omitempty"`
	ParallelTurns bool  `json:"parallel_turns,omitempty"`
}

// Image is the sandbox image turns run in.
type Image struct {
	// State is "" (no room server answer yet), "pending" (waiting for Docker), "checking",
	// "pulling", "building", "ready" or "failed".
	State string `json:"state"`
	// Ref is the image in use once ready.
	Ref      string `json:"ref,omitempty"`
	Progress string `json:"progress,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Update is the self-update the paired room server offers.
type Update struct {
	// Available is the newer version on offer.
	Available string `json:"available,omitempty"`
	// Served is the version the worker's room server hands out; the supervisor restarts a worker
	// whose instance changed version, so that it runs the new one (003-AC7).
	Served string `json:"served,omitempty"`
	// From names where Available comes from: GitHub or an instance's URL.
	From    string `json:"from,omitempty"`
	Running bool   `json:"running"`
	Error   string `json:"error,omitempty"`
}

// Daemon holds the pairing and connection state of every room it serves.
type Daemon struct {
	o  Options
	mu sync.Mutex
	// st holds what the rooms share; the room fields come from cur.
	st Status
	// rooms are the room runs by room link, and cur the one handed over last, guarded by mu.
	rooms map[string]*roomRun
	cur   *roomRun
	// ready is each room's readiness as last told, "" when it can host, guarded by mu.
	ready map[string]string
	// changed is closed, and replaced, whenever the settings or a room's readiness change: it
	// wakes every heartbeat. Guarded by mu.
	changed chan struct{}
	// loops counts running room loops, which remove their sandbox as they end, and image jobs.
	loops sync.WaitGroup
	// img is the sandbox image job, guarded by mu.
	img        imageJob
	settingsMu sync.Mutex
	// acquire waits for a slot under the host's global cap, set by ServeWorker before any room
	// starts; nil runs turns at once.
	acquire func(ctx context.Context, room string, turn int64, onWait func()) (release func(), err error)
	// cache is nil when disabled.
	cache *wsCache
	// turns and failedTurns count the turns that ended, for the worker metrics.
	turns, failedTurns atomic.Int64
	// access is the rooms' proxies and the cut-off, guarded by mu.
	access Access
	// running cancels the running turns by id, guarded by mu.
	running map[int64]context.CancelCauseFunc
	// inspectMu runs one plugin inspection at a time; inspected holds the versions reported, by
	// room link and id, guarded by inspectMu (009-AC13).
	inspectMu sync.Mutex
	inspected map[string]bool
}

// New builds a daemon.
func New(o Options) *Daemon {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.CheckEvery, 30*time.Second)
	def(&o.StatusEvery, 15*time.Second)
	def(&o.PollEvery, 2*time.Second)
	def(&o.MaxBackoff, 30*time.Second)
	if o.Allowlist == nil {
		o.Allowlist = slices.Clone(sandbox.DefaultAllowlist)
	}
	d := &Daemon{o: o, st: Status{Version: o.Version, Connection: "offline", Settings: o.Settings, Allowlist: o.Allowlist},
		rooms: map[string]*roomRun{}, ready: map[string]string{}, changed: make(chan struct{}), access: o.Access,
		running: map[int64]context.CancelCauseFunc{}, inspected: map[string]bool{}}
	if o.CacheDir != "" && o.CacheMax > 0 {
		d.cache = &wsCache{dir: o.CacheDir, max: o.CacheMax}
	}
	if o.Image != "" {
		d.img.known = true
		d.st.Image = Image{State: "ready", Ref: o.Image}
	}
	return d
}

func (d *Daemon) turnEvent(e TurnEvent) {
	if e.Phase == "end" {
		d.turns.Add(1)
		if e.Error != "" {
			d.failedTurns.Add(1)
		}
	}
	if d.o.OnTurn != nil {
		d.o.OnTurn(e)
	}
}

// Status returns a snapshot: what the rooms share, every room, and the current room's fields.
func (d *Daemon) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.st
	st.Rooms = make([]RoomStatus, 0, len(d.rooms))
	for _, r := range d.rooms {
		rs := r.st
		rs.CanHost, rs.Reason = d.readinessLocked(rs.Room)
		st.Rooms = append(st.Rooms, rs)
	}
	slices.SortFunc(st.Rooms, func(a, b RoomStatus) int { return strings.Compare(a.Room, b.Room) })
	if c := d.cur; c != nil {
		st.Room, st.Pairing, st.Code, st.ApproveURL, st.Connection, st.Error = c.st.Room, c.st.Pairing, c.st.Code, c.st.ApproveURL, c.st.Connection, c.st.Error
	}
	st.CanHost, st.Reason = d.readinessLocked(st.Room)
	return st
}

// update applies f, then recomputes each room's readiness under the same lock, so the status
// never shows a readiness its parts contradict. A change, or a new image state, which plugin
// inspections wait on, wakes the heartbeats.
func (d *Daemon) update(f func(*Status)) {
	d.mu.Lock()
	image := d.st.Image.State
	f(&d.st)
	imageChanged := d.st.Image.State != image
	changed := map[string]string{}
	for room := range d.rooms {
		_, reason := d.readinessLocked(room)
		if old, ok := d.ready[room]; !ok || old != reason {
			changed[room] = reason
		}
		d.ready[room] = reason
	}
	d.mu.Unlock()
	for room, reason := range changed {
		d.o.Log.Info("readiness_changed", zap.String("room", room), zap.Bool("can_host", reason == ""), zap.String("reason", reason))
	}
	if len(changed) > 0 || imageChanged {
		d.wake()
	}
}

// wake tells every heartbeat that something it posts changed.
func (d *Daemon) wake() {
	d.mu.Lock()
	close(d.changed)
	d.changed = make(chan struct{})
	d.mu.Unlock()
}

// changes is closed at the next wake.
func (d *Daemon) changes() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.changed
}

// readiness is whether this machine can host room now, and why not.
func (d *Daemon) readiness(room string) (bool, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.readinessLocked(room)
}

// Run re-checks readiness until ctx ends.
func (d *Daemon) Run(ctx context.Context) {
	tick := time.NewTicker(d.o.CheckEvery)
	defer tick.Stop()
	for {
		d.Refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Refresh re-checks Docker now.
func (d *Daemon) Refresh(ctx context.Context) {
	dk := sandbox.Check(ctx, d.o.Docker)
	d.update(func(s *Status) { s.Docker = dk })
	d.maybePrepare()
}

// errCutOff ends the turns of a worker the host cut off (003-AC15). Its text reaches the room.
var errCutOff = errors.New("the host cut this agent off from Claude for now")

// errStopped ends a turn someone stopped in the room (005-AC5).
var errStopped = errors.New("stopped in the room")

// stopTurn kills turn id's claude, or ends it while it waits, when the room stopped it; a turn this
// daemon no longer runs is left alone.
func (d *Daemon) stopTurn(id int64) {
	d.mu.Lock()
	cut := d.running[id]
	d.mu.Unlock()
	if cut != nil {
		cut(errStopped)
	}
}

// SetAccess takes the rooms' proxies the supervisor hands out. Cutting off ends the running turns.
func (d *Daemon) SetAccess(a Access) {
	d.mu.Lock()
	cut := a.CutOff && !d.access.CutOff
	d.access = a
	var cancels []context.CancelCauseFunc
	if cut {
		for _, c := range d.running {
			cancels = append(cancels, c)
		}
	}
	d.mu.Unlock()
	for _, c := range cancels {
		c(errCutOff)
	}
	if cut {
		d.o.Log.Warn("cut_off", zap.Int("turns_ended", len(cancels)))
	}
	d.update(func(*Status) {})
}

// proxyFor is room's account proxy, or why the room cannot reach Claude.
func (d *Daemon) proxyFor(room string) (Proxy, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.proxyLocked(room)
}

func (d *Daemon) proxyLocked(room string) (Proxy, error) {
	if d.access.CutOff {
		return Proxy{}, errCutOff
	}
	p, ok := d.access.Proxies[room]
	switch {
	case !ok:
		return Proxy{}, errors.New("the host has no Claude account ready for this room yet")
	case p.Error != "":
		return Proxy{}, errors.New(p.Error)
	}
	return p, nil
}

// readinessLocked is whether this machine can host room, and why not; the caller holds mu.
func (d *Daemon) readinessLocked(room string) (bool, string) {
	s := &d.st
	_, accessErr := d.proxyLocked(room)
	switch {
	case !s.Docker.OK:
		return false, s.Docker.Reason
	case accessErr != nil:
		return false, accessErr.Error()
	case s.Image.State == "failed":
		return false, "the sandbox image is unavailable on the host's machine: " + s.Image.Error
	case s.Image.State != "ready":
		return false, "the sandbox image is not ready yet on the host's machine"
	}
	return true, ""
}

// SetRoom serves room beside the others, pairing first if no token is stored for it, and makes it
// the one the page shows. A room served already keeps its connection; one whose run ended, revoked
// or failed, starts again.
func (d *Daemon) SetRoom(ctx context.Context, room Room) {
	key := room.String()
	d.mu.Lock()
	old := d.rooms[key]
	if old != nil && !old.ended() {
		d.cur = old
		d.mu.Unlock()
		return
	}
	if old != nil {
		old.cancel()
	}
	ctx, cancel := context.WithCancel(ctx) //nolint:gosec // kept in the run: cancelled when it is replaced, or with ctx
	r := &roomRun{d: d, room: room, ctx: ctx, cancel: cancel, done: make(chan struct{}), st: RoomStatus{Room: key, Connection: "offline"}}
	d.rooms[key], d.cur = r, r
	d.mu.Unlock()
	// Readiness depends on the room's account.
	d.update(func(*Status) {})
	d.loops.Go(func() {
		defer close(r.done)
		r.loop(ctx)
	})
}

// Wait returns once every room loop has ended and removed its sandboxes; cancel their context first.
func (d *Daemon) Wait() { d.loops.Wait() }

// runs lists the room runs.
func (d *Daemon) runs() []*roomRun {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Collect(maps.Values(d.rooms))
}

// roomRun is one room's pairing, connection and sandboxes.
type roomRun struct {
	d    *Daemon
	room Room
	// ctx lasts until the run is replaced, as the image job it started may serve the other rooms;
	// cancel ends it. done closes once its loop returned.
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	// st is the room's status, guarded by d.mu.
	st RoomStatus

	agentMu sync.Mutex
	// sandboxes are the room's, one per turn running at once (003-AC20).
	sandboxes []*sandboxSlot
	// lastTurn is the highest turn id taken: a stream reconnecting mid-turn announces it again.
	lastTurn int64
}

// sandboxSlot is one of a room's sandboxes; busy while a turn runs in it.
type sandboxSlot struct {
	ag Agent
	// proxy is the proxy address ag was built with.
	proxy string
	busy  bool
}

// update changes the room's status.
func (r *roomRun) update(f func(*RoomStatus)) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	f(&r.st)
}

// ended reports a run whose loop returned.
func (r *roomRun) ended() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

var errRevoked = errors.New("the room revoked this daemon's access: pair again")

func (r *roomRun) loop(ctx context.Context) {
	d, room := r.d, r.room
	defer r.closeAgent(ctx)
	r.checkServer(ctx)
	fail := func(err error) {
		d.o.Log.Warn("room_failed", zap.String("room", room.String()), zap.Error(err))
		r.update(func(s *RoomStatus) { s.Error, s.Connection = err.Error(), "offline" })
	}
	tok, err := d.o.Tokens.Get(room)
	if err != nil {
		fail(err)
		return
	}
	if tok == "" {
		if tok, err = r.pair(ctx); err != nil {
			if ctx.Err() == nil {
				fail(err)
			}
			return
		}
		if err := d.o.Tokens.Set(room, tok); err != nil {
			fail(err)
			return
		}
	}
	r.update(func(s *RoomStatus) { s.Pairing, s.Code, s.ApproveURL = "paired", "", "" })
	err = r.connect(ctx, tok)
	if errors.Is(err, errRevoked) {
		if serr := d.o.Tokens.Set(room, ""); serr != nil {
			d.o.Log.Error("token_delete_failed", zap.Error(serr))
		}
		if d.cache != nil {
			if serr := d.cache.forget(room); serr != nil {
				d.o.Log.Error("workspace_cache_delete_failed", zap.Error(serr))
			}
		}
		r.update(func(s *RoomStatus) { s.Pairing = "" })
		fail(err)
	}
}

func (d *Daemon) roomRequest(ctx context.Context, method, url, token string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode: %w", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := d.o.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	return resp, nil
}

// apiError reads the room's {"error": ...} body.
func apiError(resp *http.Response) error {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&e)
	if e.Error == "" {
		e.Error = resp.Status
	}
	return fmt.Errorf("room server: %s", e.Error)
}

// pair asks the room for a pairing and waits for a participant's answer.
func (r *roomRun) pair(ctx context.Context) (string, error) {
	d, room := r.d, r.room
	r.update(func(s *RoomStatus) { s.Pairing = "requesting" })
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	// A fixed name, not this machine's: room servers before spec 003 require one (003-AC17).
	resp, err := d.roomRequest(rctx, http.MethodPost, room.Base+"/api/rooms/"+room.ID+"/pairings", "", map[string]string{"name": "grill-room daemon"})
	if err != nil {
		cancel()
		return "", err
	}
	var p struct {
		ID        string    `json:"id"`
		Secret    string    `json:"secret"`
		Code      string    `json:"code"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if resp.StatusCode != http.StatusCreated {
		err = apiError(resp)
	} else {
		err = json.NewDecoder(resp.Body).Decode(&p)
	}
	_ = resp.Body.Close()
	cancel()
	if err != nil {
		return "", fmt.Errorf("pairing request: %w", err)
	}
	// Built here, not taken from the reply: the page links to it, so it must be the room's own page.
	approve := room.String() + "?pair=" + url.QueryEscape(p.ID)
	r.update(func(s *RoomStatus) { s.Pairing, s.Code, s.ApproveURL = "pending", p.Code, approve })
	d.o.Log.Info("pairing_pending", zap.String("room", room.String()), zap.String("code", p.Code))

	tick := time.NewTicker(d.o.PollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-tick.C:
		}
		status, tok, err := d.pollPairing(ctx, room, p.ID, p.Secret)
		switch {
		case errors.Is(err, errPairingGone):
			return "", err
		case err != nil && time.Now().After(p.ExpiresAt):
			return "", fmt.Errorf("pairing request expired: %w", err)
		case err != nil:
			d.o.Log.Warn("pairing_poll_failed", zap.Error(err))
		case status == "approved":
			d.o.Log.Info("pairing_approved", zap.String("room", room.String()))
			return tok, nil
		case status == "denied":
			return "", errors.New("pairing denied in the room")
		}
	}
}

var errPairingGone = errors.New("the pairing request expired: pair again")

func (d *Daemon) pollPairing(ctx context.Context, room Room, id, secret string) (status, token string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := d.roomRequest(ctx, http.MethodGet, room.Base+"/api/pairings/"+id, secret, nil)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", "", errPairingGone
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", apiError(resp)
	}
	var r struct {
		Status string `json:"status"`
		Token  string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", "", fmt.Errorf("pairing poll: %w", err)
	}
	return r.Status, r.Token, nil
}

// connect keeps the room's daemon stream open, reconnecting with backoff, until ctx ends or the
// room revokes the token.
func (r *roomRun) connect(ctx context.Context, tok string) error {
	d := r.d
	backoff := time.Second
	for {
		r.update(func(s *RoomStatus) { s.Connection = "connecting" })
		start := time.Now()
		err := r.stream(ctx, tok)
		if ctx.Err() != nil {
			r.update(func(s *RoomStatus) { s.Connection = "offline" })
			return ctx.Err()
		}
		if errors.Is(err, errRevoked) {
			return err
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		// Jitter: daemons cut by the same server restart must not reconnect in lockstep.
		wait := backoff/2 + rand.N(backoff) //nolint:gosec // jitter needs no crypto randomness
		r.update(func(s *RoomStatus) {
			s.Connection, s.Error = "offline", fmt.Sprintf("%v (retry in %s)", err, wait.Round(time.Second))
		})
		d.o.Log.Warn("room_disconnected", zap.Error(err), zap.Duration("retry_in", wait))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		backoff = min(backoff*2, d.o.MaxBackoff)
	}
}

// stream holds one connection: it reads the room's events and posts the heartbeat beside them.
func (r *roomRun) stream(ctx context.Context, tok string) error {
	d, room := r.d, r.room
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	resp, err := d.roomRequest(ctx, http.MethodGet, room.Base+"/api/rooms/"+room.ID+"/daemon/events", tok, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return errRevoked
	case resp.StatusCode != http.StatusOK:
		return apiError(resp)
	}
	// The room pings every heartbeat; silence for three of them means a dead connection.
	silence := 3 * 20 * time.Second
	watchdog := time.AfterFunc(silence, cancel)
	defer watchdog.Stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()

	sc := bufio.NewScanner(resp.Body)
	var event string
	for sc.Scan() {
		watchdog.Reset(silence)
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = line[len("event: "):]
		case strings.HasPrefix(line, "data: ") && event == "hello":
			var h struct {
				Heartbeat int    `json:"heartbeat_seconds"`
				RoomName  string `json:"room_name"`
			}
			if json.Unmarshal([]byte(line[len("data: "):]), &h) == nil && h.Heartbeat > 0 {
				silence = 3 * time.Duration(h.Heartbeat) * time.Second
				watchdog.Reset(silence)
			}
			r.update(func(s *RoomStatus) { s.Connection, s.Error, s.Name = "connected", "", h.RoomName })
			d.o.Log.Info("room_connected", zap.String("room", room.String()))
			wg.Go(func() { r.heartbeat(ctx, tok) })
			// A reconnect often follows a room server upgrade: ask again what it hands out.
			d.loops.Go(func() { r.checkServer(r.ctx) })
		case strings.HasPrefix(line, "data: ") && event == "turn":
			var t struct {
				ID int64 `json:"id"`
			}
			if json.Unmarshal([]byte(line[len("data: "):]), &t) == nil && r.takeTurn(t.ID) {
				wg.Go(func() { r.runTurn(ctx, tok, t.ID) })
			}
		case strings.HasPrefix(line, "data: ") && event == "inspect":
			var v struct {
				ID int64 `json:"id"`
			}
			if json.Unmarshal([]byte(line[len("data: "):]), &v) == nil && v.ID > 0 {
				wg.Go(func() { r.inspect(ctx, tok, v.ID) })
			}
		case strings.HasPrefix(line, "data: ") && event == "stop":
			var t struct {
				ID int64 `json:"id"`
			}
			if json.Unmarshal([]byte(line[len("data: "):]), &t) == nil {
				d.stopTurn(t.ID)
			}
		case strings.HasPrefix(line, "data: ") && event == "room":
			var info struct {
				Host     bool `json:"host"`
				Parallel bool `json:"parallel_turns"`
			}
			if json.Unmarshal([]byte(line[len("data: "):]), &info) == nil {
				r.update(func(s *RoomStatus) { s.Host, s.ParallelTurns = &info.Host, info.Parallel })
			}
		case strings.HasPrefix(line, "data: ") && event == "revoked":
			return errRevoked
		case line == "":
			event = ""
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("room stream: %w", err)
	}
	return errors.New("room stream ended")
}

// heartbeat posts readiness now, then every StatusEvery and whenever it changes.
func (r *roomRun) heartbeat(ctx context.Context, tok string) {
	d, room := r.d, r.room
	tick := time.NewTicker(d.o.StatusEvery)
	defer tick.Stop()
	for {
		changed := d.changes()
		st := d.Status()
		canHost, reason := d.readiness(room.String())
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		resp, err := d.roomRequest(rctx, http.MethodPost, room.Base+"/api/rooms/"+room.ID+"/daemon/status", tok,
			map[string]any{"can_host": canHost, "reason": reason, "version": d.o.Version,
				"max_model": st.Settings.MaxModel, "max_effort": st.Settings.MaxEffort, "approve_all": st.Settings.ApproveAll})
		if err == nil {
			if resp.StatusCode != http.StatusNoContent {
				err = apiError(resp)
			}
			_ = resp.Body.Close()
		}
		cancel()
		if err != nil && ctx.Err() == nil {
			d.o.Log.Warn("status_post_failed", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-changed:
		}
	}
}

// takeTurn reports whether turn id is new to this room run.
func (r *roomRun) takeTurn(id int64) bool {
	r.agentMu.Lock()
	defer r.agentMu.Unlock()
	if id <= r.lastTurn {
		return false
	}
	r.lastTurn = id
	return true
}
