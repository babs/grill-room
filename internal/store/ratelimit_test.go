package store

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The window holds maxHits events, frees one when the oldest leaves it, and unhit and reset touch
// only their own events.
func TestRateEvents(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	const window = 60_000
	hit := func(scope, key string, now int64) (int64, int64) {
		t.Helper()
		id, oldest, err := s.RateHit(ctx, scope, key, 2, 100, now-window, now)
		if err != nil {
			t.Fatal(err)
		}
		return id, oldest
	}
	first, _ := hit("login", "k", 1000)
	second, _ := hit("login", "k", 2000)
	if first == 0 || second == 0 {
		t.Fatal("hits under the cap refused")
	}
	if id, oldest := hit("login", "k", 3000); id != 0 || oldest != 1000 {
		t.Fatalf("past the cap: id %d, oldest %d", id, oldest)
	}
	if id, _ := hit("magic", "k", 3000); id == 0 {
		t.Fatal("another scope shares the count")
	}
	// 019-AC4: migration 53 admits the anonymous rooms' scope.
	if id, _ := hit("anon-room", "k", 3000); id == 0 {
		t.Fatal("anon-room shares the count")
	}
	if id, _ := hit("login", "other", 3000); id == 0 {
		t.Fatal("another key shares the count")
	}
	// At 1000+window the first event is out: now - at >= window.
	third, _ := hit("login", "k", 1000+window)
	if third == 0 {
		t.Fatal("the oldest event did not leave the window")
	}
	if err := s.RateUnhit(ctx, third); err != nil {
		t.Fatal(err)
	}
	if id, _ := hit("login", "k", 1000+window); id == 0 {
		t.Fatal("unhit did not free its event")
	}
	if err := s.RateReset(ctx, "login", "k"); err != nil {
		t.Fatal(err)
	}
	if id, _ := hit("login", "k", 1000+window); id == 0 {
		t.Fatal("reset left events")
	}
	if id, oldest := hit("magic", "k", 3000); id == 0 || oldest != 3000 {
		t.Fatalf("reset of login cleared magic: id %d, oldest %d", id, oldest)
	}
}

// 017-AC16: concurrent hits from two stores on one database never pass the cap together.
func TestRateHitConcurrentPods(t *testing.T) {
	dsn := testDSN(t)
	pods := []*Store{openAt(t, dsn), openAt(t, dsn)}
	var wg sync.WaitGroup
	var mu sync.Mutex
	passed := 0
	for i := range 20 {
		wg.Go(func() {
			id, _, err := pods[i%2].RateHit(t.Context(), "login", "k", 10, 100, 0, 1000)
			if err != nil {
				t.Error(err)
				return
			}
			if id != 0 {
				mu.Lock()
				passed++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if passed != 10 {
		t.Fatalf("%d of 20 concurrent hits passed a cap of 10", passed)
	}
}

// A hit, under its key's cap or over it, never takes the write lock: a flood of refused requests
// cannot stall the room writes queued behind it.
func TestRateHitTakesNoWriteLock(t *testing.T) {
	var locks atomic.Int64
	s, err := Open(testDSN(t), WithLockWait(func(time.Duration) { locks.Add(1) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	locks.Store(0)
	for i := range 5 {
		id, _, err := s.RateHit(t.Context(), "login", "k", 2, 100, 0, int64(1000+i))
		if err != nil || (id != 0) != (i < 2) {
			t.Fatalf("hit %d: id %d, err %v", i, id, err)
		}
	}
	if n := locks.Load(); n != 0 {
		t.Fatalf("rate hits took the write lock %d times", n)
	}
}

// A scope holding scopeCap events in the window refuses every new key, and frees up as they leave it.
func TestRateHitScopeCap(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	for _, k := range []string{"a", "b"} {
		if id, _, err := s.RateHit(ctx, "magic", k, 5, 2, 0, 1000); err != nil || id == 0 {
			t.Fatalf("%s: id %d, err %v", k, id, err)
		}
	}
	if _, _, err := s.RateHit(ctx, "magic", "c", 5, 2, 0, 1000); !errors.Is(err, ErrRateFull) {
		t.Fatalf("past the scope cap: %v, want ErrRateFull", err)
	}
	if id, _, err := s.RateHit(ctx, "login", "c", 5, 2, 0, 1000); err != nil || id == 0 {
		t.Fatalf("another scope: id %d, err %v", id, err)
	}
	if id, _, err := s.RateHit(ctx, "magic", "c", 5, 2, 1000, 2000); err != nil || id == 0 {
		t.Fatalf("events past the window still count: id %d, err %v", id, err)
	}
}
