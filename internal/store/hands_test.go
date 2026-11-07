package store

import (
	"context"
	"slices"
	"testing"
)

// handIDs is room's hands live at now as participant ids, in queue order.
func handIDs(t *testing.T, s *Store, room string, now int64) []int64 {
	t.Helper()
	hs, err := s.Hands(context.Background(), room, now, presenceTimes(now).Grace)
	if err != nil {
		t.Fatal(err)
	}
	out := []int64{}
	for _, h := range hs {
		out = append(out, h.ParticipantID)
	}
	return out
}

// handRows counts the hand rows, live or not.
func handRows(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), `SELECT count(*) FROM hands`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 031-AC1 031-AC2: raising is idempotent and keeps the first raise time, the queue goes by raise
// time, lowering is idempotent; a removed participant loses their hand.
func TestHandsRaiseLowerRemove(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	alice := seed(t, s, "a")
	bob, _, err := s.Join(ctx, "inv-a", "Bob", "tok-bob", JoinProfile{RecoveryHash: "rec-bob", SessionHash: "sess-bob"})
	if err != nil {
		t.Fatal(err)
	}
	carol, _, err := s.Join(ctx, "inv-a", "Carol", "tok-carol", JoinProfile{RecoveryHash: "rec-carol", SessionHash: "sess-carol"})
	if err != nil {
		t.Fatal(err)
	}
	const t0, now = int64(1_000_000), int64(1_001_000)
	for _, pid := range []int64{alice.ID, bob.ID, carol.ID} {
		if err := s.PresenceOpen(ctx, "a", pid, "t1", presenceTimes(t0)); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		pid, at int64
	}{{bob.ID, t0 + 100}, {alice.ID, t0 + 200}, {carol.ID, t0 + 300}, {bob.ID, t0 + 400}} {
		if err := s.RaiseHand(ctx, "a", c.pid, c.at); err != nil {
			t.Fatal(err)
		}
	}
	hs, err := s.Hands(ctx, "a", now, presenceTimes(now).Grace)
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 3 || hs[0] != (Hand{bob.ID, t0 + 100}) || hs[1] != (Hand{alice.ID, t0 + 200}) || hs[2] != (Hand{carol.ID, t0 + 300}) {
		t.Fatalf("queue %+v, want Bob at 100, Alice, Carol", hs)
	}
	for range 2 {
		if err := s.LowerHand(ctx, "a", alice.ID); err != nil {
			t.Fatal(err)
		}
	}
	if got := handIDs(t, s, "a", now); !slices.Equal(got, []int64{bob.ID, carol.ID}) {
		t.Fatalf("after Alice lowers: %v", got)
	}
	if _, err := s.EndSeats(ctx, "a", carol.ID, Ending{By: alice}); err != nil {
		t.Fatal(err)
	}
	if got := handIDs(t, s, "a", now); !slices.Equal(got, []int64{bob.ID}) {
		t.Fatalf("after Carol's removal: %v", got)
	}
	if err := s.DeleteRoom(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if n := handRows(t, s); n != 0 {
		t.Fatalf("after the room's deletion: %d hand rows", n)
	}
}

// 031-AC6: a hand raised before its participant's current stay lapsed: raising again starts it
// anew, and the sweep drops it, as it drops one whose participant has had no live tab past the
// grace, a crash included; a hand of the current stay stays, through a reload within the grace.
func TestHandsLapse(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	alice := seed(t, s, "a")
	bob, _, err := s.Join(ctx, "inv-a", "Bob", "tok-bob", JoinProfile{RecoveryHash: "rec-bob", SessionHash: "sess-bob"})
	if err != nil {
		t.Fatal(err)
	}
	open := func(pid int64, tab string, at int64) {
		t.Helper()
		if err := s.PresenceOpen(ctx, "a", pid, tab, presenceTimes(at)); err != nil {
			t.Fatal(err)
		}
	}
	sweep := func(at int64) {
		t.Helper()
		if _, err := s.SweepExpired(ctx, at, presenceTimes(at).Grace, 0); err != nil {
			t.Fatal(err)
		}
	}
	const t0 = int64(1_000_000)
	for _, pid := range []int64{alice.ID, bob.ID} {
		open(pid, "t1", t0)
		if err := s.RaiseHand(ctx, "a", pid, t0+1_000); err != nil {
			t.Fatal(err)
		}
	}
	// Alice comes back past the grace: a new stay. Bob's stream renews his tab meanwhile.
	const t1 = t0 + 30_000
	open(bob.ID, "t1", t0+15_000)
	open(bob.ID, "t1", t1)
	open(alice.ID, "t2", t1)
	sweep(t1)
	if got := handIDs(t, s, "a", t1); !slices.Equal(got, []int64{bob.ID}) || handRows(t, s) != 1 {
		t.Fatalf("after the sweep: %v, want Bob's alone", got)
	}
	if err := s.RaiseHand(ctx, "a", bob.ID, t1); err != nil {
		t.Fatal(err)
	}
	if hs, err := s.Hands(ctx, "a", t1, presenceTimes(t1).Grace); err != nil || len(hs) != 1 || hs[0].RaisedMS != t0+1_000 {
		t.Fatalf("Bob raising again in the same stay: %+v %v", hs, err)
	}
	// Bob's stay restarts while his hand row is still there: raising again starts it anew.
	const t2 = t1 + 100_000
	open(bob.ID, "t3", t2)
	if err := s.RaiseHand(ctx, "a", bob.ID, t2); err != nil {
		t.Fatal(err)
	}
	if hs, err := s.Hands(ctx, "a", t2, presenceTimes(t2).Grace); err != nil || len(hs) != 1 || hs[0].RaisedMS != t2 {
		t.Fatalf("Bob raising in a new stay: %+v %v", hs, err)
	}
	// Bob's page closes: its stream releases the tab and its last beat drops the tab row. Within the
	// grace the hand stays, a reload's window; past it the sweep drops it.
	if err := s.PresenceRelease(ctx, "a", bob.ID, "t3", presenceTimes(t2)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PresenceBeat(ctx, "a", bob.ID, "t3", TabState{}, true, 4, presenceTimes(t2)); err != nil {
		t.Fatal(err)
	}
	sweep(t2 + 10_000)
	if got := handIDs(t, s, "a", t2+10_000); !slices.Equal(got, []int64{bob.ID}) || handRows(t, s) != 1 {
		t.Fatalf("within the grace: %v, want Bob's", got)
	}
	sweep(t2 + 20_001)
	if got := handIDs(t, s, "a", t2+20_001); len(got) != 0 || handRows(t, s) != 0 {
		t.Fatalf("past the grace: %v, want none", got)
	}
	// A PostgreSQL crash empties the unlogged presence: the next open starts a new stay, and the
	// hand lapses.
	const tc = t2 + 30_000
	open(alice.ID, "t4", tc)
	if err := s.RaiseHand(ctx, "a", alice.ID, tc+1_000); err != nil {
		t.Fatal(err)
	}
	if got := handIDs(t, s, "a", tc+1_000); !slices.Equal(got, []int64{alice.ID}) {
		t.Fatalf("before the crash: %v, want Alice's", got)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM presence`); err != nil {
		t.Fatal(err)
	}
	open(alice.ID, "t5", tc+2_000)
	sweep(tc + 2_000)
	if got := handIDs(t, s, "a", tc+2_000); len(got) != 0 || handRows(t, s) != 0 {
		t.Fatalf("after a crash: %v, want none", got)
	}
	// A goodbye arriving after the grace, with no sweep in between, must not bring a lapsed hand back.
	const tg = tc + 10_000
	if err := s.RaiseHand(ctx, "a", alice.ID, tg); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PresenceBeat(ctx, "a", alice.ID, "t5", TabState{}, true, 4, presenceTimes(tg+45_000)); err != nil {
		t.Fatal(err)
	}
	if got := handIDs(t, s, "a", tg+45_000); len(got) != 0 || handRows(t, s) != 0 {
		t.Fatalf("after a late goodbye: %v, want none", got)
	}
	// A stream released late, past the grace, drops the hand too; one released within it keeps it.
	const tr = tg + 100_000
	open(alice.ID, "t6", tr)
	if err := s.RaiseHand(ctx, "a", alice.ID, tr+1_000); err != nil {
		t.Fatal(err)
	}
	if err := s.PresenceRelease(ctx, "a", alice.ID, "t6", presenceTimes(tr+5_000)); err != nil {
		t.Fatal(err)
	}
	open(alice.ID, "t7", tr+24_000)
	if got := handIDs(t, s, "a", tr+24_000); !slices.Equal(got, []int64{alice.ID}) {
		t.Fatalf("reload within the grace: %v, want Alice's", got)
	}
	if err := s.PresenceRelease(ctx, "a", alice.ID, "t7", presenceTimes(tr+70_000)); err != nil {
		t.Fatal(err)
	}
	if got := handIDs(t, s, "a", tr+70_000); len(got) != 0 || handRows(t, s) != 0 {
		t.Fatalf("after a late release: %v, want none", got)
	}
}

// 031-AC1: a raise stamped before the stay began, by a skewed pod clock or a click before the
// stream opened, counts from the stay's start.
func TestHandsRaiseBeforeStay(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	alice := seed(t, s, "a")
	const t0 = int64(1_000_000)
	if err := s.PresenceOpen(ctx, "a", alice.ID, "t1", presenceTimes(t0)); err != nil {
		t.Fatal(err)
	}
	if err := s.RaiseHand(ctx, "a", alice.ID, t0-300); err != nil {
		t.Fatal(err)
	}
	if hs, err := s.Hands(ctx, "a", t0, presenceTimes(t0).Grace); err != nil || len(hs) != 1 || hs[0].RaisedMS != t0 {
		t.Fatalf("hands %+v %v, want Alice's at the stay's start", hs, err)
	}
	if _, err := s.SweepExpired(ctx, t0+1_000, 20_000, 0); err != nil {
		t.Fatal(err)
	}
	if got := handIDs(t, s, "a", t0+1_000); !slices.Equal(got, []int64{alice.ID}) {
		t.Fatalf("after the sweep: %v, want Alice's", got)
	}
}
