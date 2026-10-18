package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// hostRoom is room r with Alice and Bob, each with a paired daemon, no host yet.
func hostRoom(t *testing.T, s *Store) (alice, bob *Participant, da, db *Daemon) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateRoom(ctx, "r", "R"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInvite(ctx, "r", "inv-r", 0); err != nil {
		t.Fatal(err)
	}
	var ps [2]*Participant
	var ds [2]*Daemon
	for i, name := range []string{"Alice", "Bob"} {
		p, _, err := s.Join(ctx, "inv-r", name, "tok-"+name, JoinProfile{})
		if err != nil {
			t.Fatal(err)
		}
		d, err := s.PairDaemon(ctx, "r", p.ID, "dmn-"+name)
		if err != nil {
			t.Fatal(err)
		}
		ps[i], ds[i] = p, d
	}
	return ps[0], ps[1], ds[0], ds[1]
}

// 017-AC12: a daemon with connections on two pods is gone only once both end; a lapsed lease
// counts as none; an ended seat's daemon is not live.
func TestDaemonConnections(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice, _, da, _ := hostRoom(t, s)
	const now = int64(1_000_000)
	for _, c := range []string{"on-a", "on-b"} {
		if err := s.DaemonConnected(ctx, c, da, "pod", now, now+60_000); err != nil {
			t.Fatal(err)
		}
	}
	if changed, err := s.SetDaemonStatus(ctx, da.ID, DaemonStatus{CanHost: true, Version: "1"}); err != nil || !changed {
		t.Fatalf("first status: %v %v", changed, err)
	}
	if changed, err := s.SetDaemonStatus(ctx, da.ID, DaemonStatus{CanHost: true, Version: "2"}); err != nil || changed {
		t.Fatalf("a new version alone: %v %v", changed, err)
	}
	live, err := s.LiveDaemons(ctx, "r", now)
	if err != nil || len(live) != 1 || live[0].ID != da.ID || !live[0].CanHost || live[0].Version != "2" || live[0].Since.UnixMilli() != now {
		t.Fatalf("live = %+v %v", live, err)
	}
	if gone, err := s.DaemonDisconnected(ctx, "on-a", da.ID, now); err != nil || gone {
		t.Fatalf("one of two ended: gone %v %v", gone, err)
	}
	if live, _ := s.LiveDaemons(ctx, "", now); len(live) != 1 || !live[0].CanHost {
		t.Fatalf("still live = %+v", live)
	}
	if live, _ := s.LiveDaemons(ctx, "r", now+60_000); len(live) != 0 {
		t.Fatalf("lapsed lease live = %+v", live)
	}
	if gone, err := s.DaemonDisconnected(ctx, "on-b", da.ID, now); err != nil || !gone {
		t.Fatalf("last ended: gone %v %v", gone, err)
	}
	// Its status went with it: a new connection cannot host until it says so.
	if err := s.DaemonConnected(ctx, "again", da, "pod", now, now+60_000); err != nil {
		t.Fatal(err)
	}
	if live, _ := s.LiveDaemons(ctx, "r", now); len(live) != 1 || live[0].CanHost {
		t.Fatalf("reconnected = %+v", live)
	}
	if _, err := s.EndSeats(ctx, "r", alice.ID, Ending{Leave: true}); err != nil {
		t.Fatal(err)
	}
	if live, _ := s.LiveDaemons(ctx, "r", now); len(live) != 0 {
		t.Fatalf("an ended seat's daemon live = %+v", live)
	}
	if err := s.RemoveDaemon(ctx, da.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DaemonConnected(ctx, "again", da, "pod", now, now+120_000); err == nil {
		t.Fatal("renewed the connection of a removed daemon")
	}
}

// 017-AC12: the earliest eligible daemon becomes host, once; a live able host stays.
func TestElectHost(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice, bob, da, db := hostRoom(t, s)
	const now = int64(1_000_000)
	if m, err := s.ElectHost(ctx, "r", now); err != nil || m != nil {
		t.Fatalf("no daemon: %+v %v", m, err)
	}
	if _, err := s.ElectHost(ctx, "nope", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown room: %v", err)
	}
	for i, d := range []*Daemon{db, da} {
		if err := s.DaemonConnected(ctx, d.RoomID+string(rune('a'+i)), d, "pod", now+int64(i), now+60_000); err != nil {
			t.Fatal(err)
		}
	}
	if m, err := s.ElectHost(ctx, "r", now); err != nil || m != nil {
		t.Fatalf("none able: %+v %v", m, err)
	}
	for _, d := range []*Daemon{da, db} {
		if _, err := s.SetDaemonStatus(ctx, d.ID, DaemonStatus{CanHost: true}); err != nil {
			t.Fatal(err)
		}
	}
	m, err := s.ElectHost(ctx, "r", now)
	if err != nil || m == nil || m.Body != "Bob holds the agent(s)." || m.ParticipantID != bob.ID {
		t.Fatalf("first eligible: %+v %v", m, err)
	}
	if m, err := s.ElectHost(ctx, "r", now); err != nil || m != nil {
		t.Fatalf("again: %+v %v", m, err)
	}
	if _, err := s.SetDaemonStatus(ctx, db.ID, DaemonStatus{}); err != nil {
		t.Fatal(err)
	}
	if m, err := s.ElectHost(ctx, "r", now); err != nil || m == nil || m.ParticipantID != alice.ID {
		t.Fatalf("host unable: %+v %v", m, err)
	}
}

// 017 I12: a guarded hand-over refuses a host that moved meanwhile and a target with no live able
// daemon; the line of a move by someone other than the host stays on the room until an election.
func TestHandOverGuard(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice, bob, _, db := hostRoom(t, s)
	now := time.Now().UnixMilli()
	if err := s.SetHost(ctx, "r", alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HandOver(ctx, "r", bob.ID, alice, &HostGuard{From: alice.ID, NowMS: now}); !errors.Is(err, ErrNoLiveHost) {
		t.Fatalf("no live daemon: %v", err)
	}
	if err := s.DaemonConnected(ctx, "c", db, "pod", now, now+60_000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HandOver(ctx, "r", bob.ID, alice, &HostGuard{From: alice.ID, NowMS: now}); !errors.Is(err, ErrNoLiveHost) {
		t.Fatalf("live, unable: %v", err)
	}
	if _, err := s.SetDaemonStatus(ctx, db.ID, DaemonStatus{CanHost: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HandOver(ctx, "r", bob.ID, alice, &HostGuard{From: bob.ID, NowMS: now}); !errors.Is(err, ErrHostMoved) {
		t.Fatalf("host moved: %v", err)
	}
	line := func() string {
		r, err := s.Room(ctx, "r")
		if err != nil {
			t.Fatal(err)
		}
		return r.HostMovedLine
	}
	if _, err := s.HandOver(ctx, "r", bob.ID, alice, &HostGuard{From: alice.ID, NowMS: now}); err != nil || line() != "" {
		t.Fatalf("by the host: %v %q", err, line())
	}
	moved, err := s.HandOver(ctx, "r", alice.ID, nil, nil)
	if err != nil || moved == "" || line() != moved {
		t.Fatalf("by the admin: %q %v %q", moved, err, line())
	}
	if err := s.SetHost(ctx, "r", bob.ID); err != nil || line() != "" {
		t.Fatalf("elected: %v %q", err, line())
	}
}

// 017-AC13: a pairing request answers its own secret only, its answer once; the caps count the
// unexpired requests.
func TestPairings(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice, _, _, _ := hostRoom(t, s)
	now := time.Now()
	nowMS := now.UnixMilli()
	p := &Pairing{ID: "p1", RoomID: "r", Code: "123-456", SecretHash: "h1", ExpiresAt: now.Add(time.Minute)}
	if ok, err := s.CreatePairing(ctx, p, 2, 3, nowMS); !ok || err != nil {
		t.Fatalf("create: %v %v", ok, err)
	}
	if st, _, err := s.PollPairing(ctx, "p1", "other", "t1", nowMS); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another secret: %q %v", st, err)
	}
	if st, d, err := s.PollPairing(ctx, "p1", "h1", "t1", nowMS); err != nil || st != "pending" || d != nil {
		t.Fatalf("pending: %q %+v %v", st, d, err)
	}
	if got, err := s.PendingPairing(ctx, "r", "p1", nowMS); err != nil || got.Code != "123-456" || got.ExpiresAt.UnixMilli() != p.ExpiresAt.UnixMilli() {
		t.Fatalf("pending pairing: %+v %v", got, err)
	}
	if err := s.ApprovePairing(ctx, "r", "p1", alice.ID, nowMS); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DaemonByToken(ctx, "h1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the secret works as a token: %v", err)
	}
	if err := s.DenyPairing(ctx, "r", "p1", nowMS); !errors.Is(err, ErrNotFound) {
		t.Fatalf("answered twice: %v", err)
	}
	st, d, err := s.PollPairing(ctx, "p1", "h1", "t1", nowMS)
	if err != nil || st != "approved" || d == nil || d.ParticipantID != alice.ID {
		t.Fatalf("approved: %q %+v %v", st, d, err)
	}
	if got, err := s.DaemonByToken(ctx, "t1"); err != nil || got.ID != d.ID {
		t.Fatalf("the minted token: %+v %v", got, err)
	}
	if _, _, err := s.PollPairing(ctx, "p1", "h1", "t2", nowMS); !errors.Is(err, ErrNotFound) {
		t.Fatalf("handed twice: %v", err)
	}
	// An approver whose seat ended before the poll pairs nothing.
	if ok, err := s.CreatePairing(ctx, &Pairing{ID: "p0", RoomID: "r", SecretHash: "h0", ExpiresAt: now.Add(time.Minute)}, 2, 3, nowMS); !ok || err != nil {
		t.Fatalf("create p0: %v %v", ok, err)
	}
	if err := s.ApprovePairing(ctx, "r", "p0", alice.ID, nowMS); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EndSeats(ctx, "r", alice.ID, Ending{Leave: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PollPairing(ctx, "p0", "h0", "t0", nowMS); !errors.Is(err, ErrNotFound) {
		t.Fatalf("approver gone: %v", err)
	}
	if err := s.DenyPairing(ctx, "r", "p0", nowMS); !errors.Is(err, ErrNotFound) {
		t.Fatalf("p0 answered: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM pairings WHERE id = 'p0'`); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"p2", "p3"} {
		ok, err := s.CreatePairing(ctx, &Pairing{ID: id, RoomID: "r", SecretHash: id, ExpiresAt: now.Add(time.Duration(i+1) * time.Second)}, 2, 3, nowMS)
		if !ok || err != nil {
			t.Fatalf("create %s: %v %v", id, ok, err)
		}
	}
	if ok, err := s.CreatePairing(ctx, &Pairing{ID: "p4", RoomID: "r", ExpiresAt: now.Add(time.Minute)}, 2, 3, nowMS); ok || err != nil {
		t.Fatalf("over the room cap: %v %v", ok, err)
	}
	// p2 lapsed: one free place.
	later := nowMS + 1000
	if ok, err := s.CreatePairing(ctx, &Pairing{ID: "p4", RoomID: "r", ExpiresAt: now.Add(time.Minute)}, 2, 1, later); ok || err != nil {
		t.Fatalf("over the server cap: %v %v", ok, err)
	}
	if ok, err := s.CreatePairing(ctx, &Pairing{ID: "p4", RoomID: "r", ExpiresAt: now.Add(time.Minute)}, 2, 3, later); !ok || err != nil {
		t.Fatalf("after expiry: %v %v", ok, err)
	}
	if err := s.DenyPairing(ctx, "r", "p4", later); err != nil {
		t.Fatal(err)
	}
	if st, _, err := s.PollPairing(ctx, "p4", "", "t4", later); err != nil || st != "denied" {
		t.Fatalf("denied: %q %v", st, err)
	}
	if _, err := s.PendingPairing(ctx, "r", "p3", nowMS+2000); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired pending: %v", err)
	}
}

// 017-AC12: pods electing at once for the same change post one line.
func TestElectHostConcurrent(t *testing.T) {
	dsn := testDSN(t)
	s := openAt(t, dsn)
	other := openAt(t, dsn)
	ctx := t.Context()
	_, _, da, _ := hostRoom(t, s)
	now := time.Now().UnixMilli()
	if err := s.DaemonConnected(ctx, "c", da, "pod", now, now+60_000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDaemonStatus(ctx, da.ID, DaemonStatus{CanHost: true}); err != nil {
		t.Fatal(err)
	}
	lines := make(chan *Message, 10)
	var wg sync.WaitGroup
	for i := range cap(lines) {
		st := []*Store{s, other}[i%2]
		wg.Go(func() {
			m, err := st.ElectHost(ctx, "r", now)
			if err != nil {
				t.Error(err)
			}
			lines <- m
		})
	}
	wg.Wait()
	close(lines)
	n := 0
	for m := range lines {
		if m != nil {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d host lines, want 1", n)
	}
}

// Two connections of a daemon closing at once: exactly one close finds the daemon gone, whatever
// the interleaving.
func TestDaemonDisconnectRace(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	_, _, da, _ := hostRoom(t, s)
	const now = int64(1_000_000)
	for round := range 30 {
		conns := []string{fmt.Sprint("a", round), fmt.Sprint("b", round)}
		for _, c := range conns {
			if err := s.DaemonConnected(ctx, c, da, "pod", now, now+60_000); err != nil {
				t.Fatal(err)
			}
		}
		var wg sync.WaitGroup
		var gone atomic.Int32
		for _, c := range conns {
			wg.Go(func() {
				g, err := s.DaemonDisconnected(ctx, c, da.ID, now)
				if err != nil {
					t.Error(err)
				}
				if g {
					gone.Add(1)
				}
			})
		}
		wg.Wait()
		if n := gone.Load(); n != 1 {
			t.Fatalf("round %d: %d closes found the daemon gone, want 1", round, n)
		}
	}
}

// A daemon reconnecting while its old connection is still live keeps its place in the election:
// the new row takes the old one's start, which outlives the old row.
func TestDaemonReconnectKeepsSince(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	_, _, da, _ := hostRoom(t, s)
	const first, second = int64(1_000_000), int64(1_050_000)
	if err := s.DaemonConnected(ctx, "old", da, "pod", first, first+60_000); err != nil {
		t.Fatal(err)
	}
	if err := s.DaemonConnected(ctx, "new", da, "pod", second, second+60_000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DaemonDisconnected(ctx, "old", da.ID, second); err != nil {
		t.Fatal(err)
	}
	if live, err := s.LiveDaemons(ctx, "r", second); err != nil || len(live) != 1 || live[0].Since.UnixMilli() != first {
		t.Fatalf("live = %+v %v, want since %d", live, err, first)
	}
	// Without an overlap the new connection starts its own run.
	if _, err := s.DaemonDisconnected(ctx, "new", da.ID, second); err != nil {
		t.Fatal(err)
	}
	if err := s.DaemonConnected(ctx, "later", da, "pod", second+1, second+60_000); err != nil {
		t.Fatal(err)
	}
	if live, err := s.LiveDaemons(ctx, "r", second+1); err != nil || len(live) != 1 || live[0].Since.UnixMilli() != second+1 {
		t.Fatalf("after a gap = %+v %v", live, err)
	}
}

// A removed daemon leaves the room's daemons on every pod at once: its connections and status go.
func TestDropDaemonConnections(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	_, _, da, dbob := hostRoom(t, s)
	const now = int64(1_000_000)
	for _, c := range []struct {
		id string
		d  *Daemon
	}{{"a-1", da}, {"a-2", da}, {"b-1", dbob}} {
		if err := s.DaemonConnected(ctx, c.id, c.d, "pod", now, now+60_000); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetDaemonStatus(ctx, c.d.ID, DaemonStatus{CanHost: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DropDaemonConnections(ctx, da.ID); err != nil {
		t.Fatal(err)
	}
	if live, err := s.LiveDaemons(ctx, "r", now); err != nil || len(live) != 1 || live[0].ID != dbob.ID || !live[0].CanHost {
		t.Fatalf("live = %+v %v, want Bob's daemon alone, still able to host", live, err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM daemon_status WHERE daemon_id = ?`, da.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("dropped daemon's status rows: %d %v", n, err)
	}
}
