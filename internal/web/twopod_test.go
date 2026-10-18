package web

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/store/storetest"
)

// newPodEnv is a server on the store database at dsn, as one pod of several: its bus carries
// notifications to the other servers of the database. It returns once the bus listens.
func newPodEnv(t *testing.T, dsn, admin string, opts Options) *env {
	t.Helper()
	e := newEnvAt(t, dsn, admin, opts, zap.NewNop())
	ready, unsub := e.bus.Subscribe(1)
	defer unsub()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.bus.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("bus: %v", err)
		}
	})
	// Run wakes every subscriber with bus.All once it listens.
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("the bus did not listen within 10 s")
	}
	return e
}

// newPair is two pods on one database, both listening, with one admin token.
func newPair(t *testing.T, opts Options) (a, b *env) {
	t.Helper()
	dsn := storetest.DSN(t)
	a = newPodEnv(t, dsn, NewToken(), opts)
	return a, newPodEnv(t, dsn, a.admin, opts)
}

// 017-AC10, 017-AC20: a line posted through pod A shows on a room stream open on pod B within 1 s,
// 20 lines in order; each pod counts the notifications it sent and received.
func TestTwoPodsRoomLine(t *testing.T) {
	a, b := newPair(t, testOptions)
	room := a.createRoom(t, "Two pods")
	alice := a.join(t, a.createInvite(t, room.ID).Token, "Alice")
	events := b.openEvents(t, room.ID, alice.Token, "")
	if m := nextMessage(t, events); m.Body != "Alice joined" {
		t.Fatalf("first line %q", m.Body)
	}
	for i := range 20 {
		body := fmt.Sprintf("line %d", i)
		start := time.Now()
		if code, out := a.do(t, "POST", "/api/rooms/"+room.ID+"/messages", alice.Token, map[string]string{"body": body}); code != http.StatusCreated {
			t.Fatalf("post: %d %s", code, out)
		}
		if m := nextMessage(t, events); m.Body != body {
			t.Fatalf("got %q, want %q", m.Body, body)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("%s took %v to reach pod B", body, d)
		}
	}
	// A's sender counts once pg_notify returns, which may be after B woke.
	waitFor(t, func() bool { return a.bus.NotifySent() >= 20 && b.bus.NotifyReceived() >= 20 })
}

// 017-AC20: lost LISTEN connections come back, and a line posted through A meanwhile reaches B's
// stream: B wakes every stream once it listens again. The terminate matches both pods' listeners.
func TestTwoPodsLostListen(t *testing.T) {
	a, b := newPair(t, testOptions)
	room := a.createRoom(t, "Lost listen")
	alice := a.join(t, a.createInvite(t, room.ID).Token, "Alice")
	events := b.openEvents(t, room.ID, alice.Token, "")
	nextMessage(t, events)
	db, err := sql.Open("pgx", b.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE application_name = $1 AND datname = current_database()`, store.ListenApplication); err != nil {
		t.Fatal(err)
	}
	if code, out := a.do(t, "POST", "/api/rooms/"+room.ID+"/messages", alice.Token, map[string]string{"body": "while away"}); code != http.StatusCreated {
		t.Fatalf("post: %d %s", code, out)
	}
	if m := nextMessage(t, events); m.Body != "while away" {
		t.Fatalf("got %q", m.Body)
	}
	// The line may have ridden the old connection, which ends a moment after the terminate.
	waitFor(t, func() bool { return a.bus.ListenReconnects() > 0 && b.bus.ListenReconnects() > 0 })
	if na, nb := a.bus.ListenReconnects(), b.bus.ListenReconnects(); na != 1 || nb != 1 {
		t.Fatalf("A reconnected %d times, B %d; want 1 each", na, nb)
	}
}
