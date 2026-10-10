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
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/creds"
	"github.com/babs/grill-room/internal/sandbox"
)

// Options wires a Daemon. Zero durations take the defaults.
type Options struct {
	// Name identifies this machine in the room and in error messages.
	Name    string
	Version string
	Tokens  *Tokens
	Docker  sandbox.Runner
	Creds   *creds.Source
	HTTP    *http.Client
	Log     *zap.Logger

	CheckEvery  time.Duration // readiness re-check
	StatusEvery time.Duration // heartbeat to the room
	PollEvery   time.Duration // pairing answer poll
	MaxBackoff  time.Duration
}

// Credentials is the credentials readiness, never the token itself.
type Credentials struct {
	OK        bool       `json:"ok"`
	Origin    string     `json:"origin,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Expired   bool       `json:"expired"`
	Error     string     `json:"error,omitempty"`
}

// Status is what the local page shows.
type Status struct {
	Host    string `json:"host"`
	Version string `json:"version"`
	Room    string `json:"room,omitempty"`
	// Pairing is "", "requesting", "pending", "paired".
	Pairing    string `json:"pairing"`
	Code       string `json:"code,omitempty"`
	ApproveURL string `json:"approve_url,omitempty"`
	// Connection is "offline", "connecting" or "connected".
	Connection  string            `json:"connection"`
	Error       string            `json:"error,omitempty"`
	Docker      sandbox.Readiness `json:"docker"`
	Credentials Credentials       `json:"credentials"`
	CanHost     bool              `json:"can_host"`
	Reason      string            `json:"reason,omitempty"`
}

// Daemon holds the pairing and connection state.
type Daemon struct {
	o    Options
	mu   sync.Mutex
	st   Status
	stop context.CancelFunc // ends the current room's pairing and connection
	// gen numbers SetRoom calls: a replaced room's loop must not write the status any more.
	gen int
	// changed wakes the heartbeat when readiness changes.
	changed chan struct{}
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
	return &Daemon{o: o, st: Status{Host: o.Name, Version: o.Version, Connection: "offline"}, changed: make(chan struct{}, 1)}
}

// Status returns a snapshot.
func (d *Daemon) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.st
}

func (d *Daemon) update(f func(*Status)) {
	d.mu.Lock()
	f(&d.st)
	d.mu.Unlock()
}

// roomUpdater writes the status only while gen is the current room.
func (d *Daemon) roomUpdater(gen int) func(func(*Status)) {
	return func(f func(*Status)) {
		d.mu.Lock()
		if d.gen == gen {
			f(&d.st)
		}
		d.mu.Unlock()
	}
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

// Refresh re-checks Docker and the credentials now.
func (d *Daemon) Refresh(ctx context.Context) {
	dk := sandbox.Check(ctx, d.o.Docker)
	var c Credentials
	tok, err := d.o.Creds.Get(ctx)
	if err != nil {
		c.Error = err.Error()
	} else {
		c.OK, c.Origin = true, tok.Origin
		if !tok.ExpiresAt.IsZero() {
			exp := tok.ExpiresAt
			c.ExpiresAt = &exp
		}
		c.Expired = tok.Expired(time.Now())
	}
	canHost, reason := true, ""
	switch {
	case !dk.OK:
		canHost, reason = false, dk.Reason
	case !c.OK:
		canHost, reason = false, "Claude credentials unreadable on "+d.o.Name
	case c.Expired:
		canHost, reason = false, "Claude credentials expired on "+d.o.Name
	}
	var changed bool
	d.update(func(s *Status) {
		changed = s.CanHost != canHost || s.Reason != reason
		s.Docker, s.Credentials, s.CanHost, s.Reason = dk, c, canHost, reason
	})
	if changed {
		d.o.Log.Info("readiness_changed", zap.Bool("can_host", canHost), zap.String("reason", reason))
		select {
		case d.changed <- struct{}{}:
		default:
		}
	}
}

// SetRoom pairs with room if no token is stored for it, then keeps connected to it. It replaces
// any previous room.
func (d *Daemon) SetRoom(ctx context.Context, room Room) {
	ctx, cancel := context.WithCancel(ctx)
	d.mu.Lock()
	if d.stop != nil {
		d.stop()
	}
	d.stop = cancel
	d.gen++
	r := &roomRun{d: d, room: room, update: d.roomUpdater(d.gen)}
	d.st.Room, d.st.Pairing, d.st.Code, d.st.ApproveURL, d.st.Error, d.st.Connection = room.String(), "", "", "", "", "offline"
	d.mu.Unlock()
	go r.loop(ctx)
}

// roomRun is one SetRoom's pairing and connection.
type roomRun struct {
	d      *Daemon
	room   Room
	update func(func(*Status))
}

var errRevoked = errors.New("the room revoked this daemon's access: pair again")

func (r *roomRun) loop(ctx context.Context) {
	d, room := r.d, r.room
	fail := func(err error) {
		d.o.Log.Warn("room_failed", zap.String("room", room.String()), zap.Error(err))
		r.update(func(s *Status) { s.Error, s.Connection = err.Error(), "offline" })
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
	r.update(func(s *Status) { s.Pairing, s.Code, s.ApproveURL = "paired", "", "" })
	err = r.connect(ctx, tok)
	if errors.Is(err, errRevoked) {
		if serr := d.o.Tokens.Set(room, ""); serr != nil {
			d.o.Log.Error("token_delete_failed", zap.Error(serr))
		}
		r.update(func(s *Status) { s.Pairing = "" })
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
	r.update(func(s *Status) { s.Pairing = "requesting" })
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	resp, err := d.roomRequest(rctx, http.MethodPost, room.Base+"/api/rooms/"+room.ID+"/pairings", "", map[string]string{"name": d.o.Name})
	if err != nil {
		cancel()
		return "", err
	}
	var p struct {
		ID         string    `json:"id"`
		Secret     string    `json:"secret"`
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
	r.update(func(s *Status) { s.Pairing, s.Code, s.ApproveURL = "pending", p.Code, approve })
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
		r.update(func(s *Status) { s.Connection = "connecting" })
		start := time.Now()
		err := r.stream(ctx, tok)
		if ctx.Err() != nil {
			r.update(func(s *Status) { s.Connection = "offline" })
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
		r.update(func(s *Status) { s.Connection, s.Error = "offline", fmt.Sprintf("%v (retry in %s)", err, wait.Round(time.Second)) })
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
				Heartbeat int `json:"heartbeat_seconds"`
			}
			if json.Unmarshal([]byte(line[len("data: "):]), &h) == nil && h.Heartbeat > 0 {
				silence = 3 * time.Duration(h.Heartbeat) * time.Second
				watchdog.Reset(silence)
			}
			r.update(func(s *Status) { s.Connection, s.Error = "connected", "" })
			d.o.Log.Info("room_connected", zap.String("room", room.String()))
			wg.Go(func() { r.heartbeat(ctx, tok) })
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
		st := d.Status()
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		resp, err := d.roomRequest(rctx, http.MethodPost, room.Base+"/api/rooms/"+room.ID+"/daemon/status", tok,
			map[string]any{"can_host": st.CanHost, "reason": st.Reason, "version": d.o.Version})
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
		case <-d.changed:
		}
	}
}
