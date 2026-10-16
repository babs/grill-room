package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"go.uber.org/zap"
)

// accountsEvery is how often the supervisor probes due setup tokens and refreshes the workers'
// access, which follows the accounts' expiry.
var accountsEvery = 30 * time.Second

// access is what in's worker gets: each room's account proxy (003-AC13, 003-AC14).
func (s *Supervisor) access(in Instance) Access {
	rooms := make(map[string]string, len(in.Rooms))
	for _, r := range in.Rooms {
		rooms[r] = in.accountFor(r)
	}
	a := s.o.Accounts.Access(s.ctx, in.ID, rooms, in.CutOff)
	// A paused room keeps its account's bearer, so an answer running as it pauses finishes; the
	// error makes the worker report it cannot host, which the room shows (003b-AC5).
	for _, r := range in.Paused {
		if _, ok := rooms[r]; ok && !a.CutOff {
			a.Proxies[r] = Proxy{Error: pausedReason}
		}
	}
	return a
}

// pausedReason is what a paused room shows, word for word from 003b-AC5.
const pausedReason = "paused on the host's machine"

// pushAccess sends w its access when it changed. One push at a time per worker, in order.
func (s *Supervisor) pushAccess(w *worker) {
	if s.o.Accounts == nil {
		return
	}
	w.accessMu.Lock()
	defer w.accessMu.Unlock()
	s.mu.Lock()
	ready := w.state == "running" && w.conn != nil && w.proto >= 2
	in := w.inst
	in.Rooms, in.RoomAccounts, in.Paused = slices.Clone(in.Rooms), maps.Clone(in.RoomAccounts), slices.Clone(in.Paused)
	s.mu.Unlock()
	if !ready {
		return
	}
	a := s.access(in)
	b, err := json.Marshal(a)
	if err != nil {
		return
	}
	s.mu.Lock()
	same := bytes.Equal(b, w.sentAccess)
	s.mu.Unlock()
	if same {
		return
	}
	if _, err := w.request(s.ctx, Msg{Type: "access", Access: &a}); err != nil {
		s.o.Log.Warn("access_push_failed", zap.String("instance", in.ID), zap.Error(err))
		return
	}
	w.set(func() { w.sentAccess = b })
}

func (s *Supervisor) pushAll() {
	s.mu.Lock()
	ws := slices.Collect(maps.Values(s.workers))
	s.mu.Unlock()
	for _, w := range ws {
		s.pushAccess(w)
	}
}

func (s *Supervisor) accountsLoop(ctx context.Context) {
	defer s.o.Accounts.Close()
	tick := time.NewTicker(accountsEvery)
	defer tick.Stop()
	for {
		s.o.Accounts.ProbeDue(ctx)
		s.pushAll()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (s *Supervisor) accounts() (*Accounts, error) {
	if s.o.Accounts == nil {
		return nil, errors.New("accounts are not available here")
	}
	return s.o.Accounts, nil
}

// AccountViews lists the accounts for the page.
func (s *Supervisor) AccountViews(ctx context.Context) []AccountView {
	if s.o.Accounts == nil {
		return []AccountView{}
	}
	return s.o.Accounts.Views(ctx)
}

// AddAccount adds a config dir or a setup token account (003-AC12).
func (s *Supervisor) AddAccount(ctx context.Context, acc Account, token string) error {
	a, err := s.accounts()
	if err != nil {
		return err
	}
	if err := a.Add(ctx, acc, token); err != nil {
		return err
	}
	// A room may have waited for an account of that name.
	go s.pushAll()
	return nil
}

// RemoveAccount removes an account no instance or room uses.
func (s *Supervisor) RemoveAccount(name string) error {
	a, err := s.accounts()
	if err != nil {
		return err
	}
	s.mu.Lock()
	for _, w := range s.workers {
		used := w.inst.accountFor("") == name
		for _, r := range w.inst.Rooms {
			used = used || w.inst.accountFor(r) == name
		}
		if used {
			s.mu.Unlock()
			return fmt.Errorf("%s uses this account: pick another one there first", w.inst.URL)
		}
	}
	s.mu.Unlock()
	return a.Remove(name)
}

// ProbeAccount checks a setup token now.
func (s *Supervisor) ProbeAccount(ctx context.Context, name string) error {
	a, err := s.accounts()
	if err != nil {
		return err
	}
	if err := a.Probe(ctx, name); err != nil {
		return err
	}
	go s.pushAll()
	return nil
}

// SetAccount sets instance id's default account, or with a room, that room's own; an empty
// account for a room goes back to the instance's (003-AC13).
func (s *Supervisor) SetAccount(id, room, account string) error {
	a, err := s.accounts()
	if err != nil {
		return err
	}
	if (account != "" || room == "") && !a.Has(account) {
		return errors.New("no such account")
	}
	s.mu.Lock()
	w := s.workers[id]
	if w == nil {
		s.mu.Unlock()
		return errors.New("no such instance")
	}
	switch {
	case room == "":
		w.inst.Account = account
	case !slices.Contains(w.inst.Rooms, room):
		s.mu.Unlock()
		return errors.New("no such room on this instance")
	case account == "":
		delete(w.inst.RoomAccounts, room)
	default:
		if w.inst.RoomAccounts == nil {
			w.inst.RoomAccounts = map[string]string{}
		}
		w.inst.RoomAccounts[room] = account
	}
	s.save(w)
	s.mu.Unlock()
	s.o.Log.Info("account_mapped", zap.String("instance", id), zap.Bool("room", room != ""), zap.String("account", account))
	s.pushAccess(w)
	return nil
}

// SetCutOff cuts instance id's worker off from every account's proxy, or lets it back (003-AC15).
// Its running turns end with an error in their rooms, and it starts none until let back.
func (s *Supervisor) SetCutOff(id string, on bool) error {
	a, err := s.accounts()
	if err != nil {
		return err
	}
	s.mu.Lock()
	w := s.workers[id]
	if w == nil {
		s.mu.Unlock()
		return errors.New("no such instance")
	}
	w.inst.CutOff = on
	s.save(w)
	s.mu.Unlock()
	if on {
		// At the proxy first: a worker that ignores the access message is shut out all the same.
		a.Revoke(id)
	}
	s.o.Log.Warn("worker_cut_off", zap.String("instance", id), zap.Bool("cut_off", on))
	s.pushAccess(w)
	return nil
}

// SetPaused pauses room of instance id, or resumes it: a paused room stays paired and connected but
// reports that it cannot host, with pausedReason, so the room moves the AI host role elsewhere
// (003b-AC5). The pause is saved with the instance.
func (s *Supervisor) SetPaused(id, room string, paused bool) error {
	s.mu.Lock()
	w := s.workers[id]
	switch {
	case w == nil:
		s.mu.Unlock()
		return errors.New("no such server")
	case !slices.Contains(w.inst.Rooms, room):
		s.mu.Unlock()
		return errors.New("no such room on this server")
	}
	w.inst.Paused = slices.DeleteFunc(w.inst.Paused, func(r string) bool { return r == room })
	if paused {
		w.inst.Paused = append(w.inst.Paused, room)
	}
	s.save(w)
	s.mu.Unlock()
	s.o.Log.Info("room_paused", zap.String("instance", id), zap.Bool("paused", paused))
	s.pushAccess(w)
	return nil
}

// RemoveRoom takes room off this computer: its worker revokes the token on the room server and
// deletes the room's state and sandboxes, then the instance forgets it (003b-AC6). force removes it
// here even when the room server does not answer, or the worker does not run.
func (s *Supervisor) RemoveRoom(ctx context.Context, room string, force bool) error {
	o, err := s.roomOps(room)
	if err != nil {
		return err
	}
	s.mu.Lock()
	w := s.workers[o.id]
	if w == nil || !slices.Contains(w.inst.Rooms, room) {
		s.mu.Unlock()
		return errors.New("no such room on this computer")
	}
	running, agreed := w.state == "running" && w.conn != nil, w.proto
	s.mu.Unlock()
	switch {
	case running && agreed < protoRemove:
		return errors.New("this server's app is too old to remove a room: update the AI host app")
	case running:
		if _, err := w.request(ctx, Msg{Type: "remove_room", Room: room, Force: force}); err != nil {
			return err
		}
	case !force:
		return errors.New("the app for this server is not running, so the room server still lists this computer")
	}
	s.mu.Lock()
	w.inst.dropRoom(room)
	s.save(w)
	s.mu.Unlock()
	s.o.Log.Info("room_dropped", zap.String("instance", o.id))
	s.pushAccess(w)
	return nil
}
