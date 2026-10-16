package bus

import (
	"context"
	"path/filepath"
	"testing"

	_ "github.com/babs/grill-room/internal/sqlitetest"
	"github.com/babs/grill-room/internal/store"
)

func TestPostPersistsAndNotifies(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
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
