package bus

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/pgtest"
	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/store/storetest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestPostPersistsAndNotifies(t *testing.T) {
	st, err := store.Open(storetest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.CreateRoom(ctx, "r", "R"); err != nil {
		t.Fatal(err)
	}
	b := New(st)
	ch, cancel := b.Subscribe(1)
	defer cancel()
	m := &store.Message{RoomID: "r", Kind: store.KindSystem, Sender: "x", Body: "hi"}
	if err := b.Post(ctx, m); err != nil {
		t.Fatal(err)
	}
	if m.ID == 0 {
		t.Fatal("ID not filled")
	}
	if got := <-ch; got != "r" {
		t.Fatalf("signal = %q", got)
	}
}

func TestFullSubscriberDropsAndCancelUnsubscribes(t *testing.T) {
	b := New(nil)
	ch, cancel := b.Subscribe(1)
	b.Notify("a")
	b.Notify("b") // buffer full
	if b.Dropped() != 1 {
		t.Fatalf("dropped = %d", b.Dropped())
	}
	<-ch
	cancel()
	b.Notify("c")
	select {
	case got := <-ch:
		t.Fatalf("cancelled subscriber got %q", got)
	default:
	}
}

// pod is one server's bus with Run running, and a subscriber that saw its first LISTEN start.
type pod struct {
	bus *Bus
	sub <-chan string
}

// startPod starts a pod; onErr, if any, is its OnListenError.
func startPod(t *testing.T, dsn string, onErr ...func(error)) pod {
	t.Helper()
	st, err := store.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	b := New(st)
	if len(onErr) > 0 {
		b.OnListenError = onErr[0]
	}
	sub, unsub := b.Subscribe(16)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- b.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
		unsub()
		_ = st.Close()
	})
	p := pod{bus: b, sub: sub}
	p.expect(t, All)
	return p
}

// expect reads the subscriber's next signals and wants them to be ids, in order.
func (p pod) expect(t *testing.T, ids ...string) {
	t.Helper()
	for _, want := range ids {
		select {
		case got := <-p.sub:
			if got != want {
				t.Fatalf("signal %q, want %q", got, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("no signal %q within 10 s", want)
		}
	}
}

// 017-AC20: a notify on one pod wakes the other's subscribers, once each, and each pod counts
// what it sent and received; a pod drops its own echo.
func TestNotifyCrossesPods(t *testing.T) {
	dsn := storetest.DSN(t)
	a, b := startPod(t, dsn), startPod(t, dsn)
	b.bus.Notify("x")
	b.expect(t, "x")
	a.expect(t, "x")
	// NOTIFY delivers in commit order: b's echo of x reaches b before y would.
	a.bus.Notify("y")
	a.expect(t, "y")
	b.expect(t, "y")
	select {
	case got := <-b.sub:
		t.Fatalf("extra signal %q", got)
	default:
	}
	// The sender counts once pg_notify returns, which may be after the listener woke.
	for deadline := time.After(10 * time.Second); a.bus.NotifySent() == 0; {
		select {
		case <-time.After(5 * time.Millisecond):
		case <-deadline:
			t.Fatal("a never counted its notify")
		}
	}
	for name, n := range map[string]uint64{
		"a sent": a.bus.NotifySent(), "b sent": b.bus.NotifySent(), "a received": a.bus.NotifyReceived(), "b received": b.bus.NotifyReceived(),
	} {
		if n != 1 {
			t.Errorf("%s %d, want 1", name, n)
		}
	}
}

// 017-AC20: a LISTEN connection lost is reported and counted, made again, and wakes every
// subscriber, as the pod may have missed any change meanwhile.
func TestListenReconnectWakesAll(t *testing.T) {
	dsn := storetest.DSN(t)
	lost := make(chan error, 1)
	a, b := startPod(t, dsn), startPod(t, dsn, func(err error) { lost <- err })
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE application_name = $1 AND datname = current_database()`, store.ListenApplication); err != nil {
		t.Fatal(err)
	}
	a.expect(t, All)
	b.expect(t, All)
	if n, e := b.bus.ListenReconnects(), b.bus.ListenErrors(); n != 1 || e != 1 {
		t.Fatalf("reconnects %d, errors %d; want 1 each", n, e)
	}
	if err := <-lost; err == nil {
		t.Fatal("the hook got no error")
	}
	a.bus.Notify("r")
	b.expect(t, "r")
}

// Without Run, a bus wakes its own subscribers only and sends nothing.
func TestNotifyWithoutRun(t *testing.T) {
	b := New(nil)
	ch, cancel := b.Subscribe(1)
	defer cancel()
	b.Notify("r")
	if got := <-ch; got != "r" || b.NotifySent() != 0 {
		t.Fatalf("signal %q, sent %d", got, b.NotifySent())
	}
}
