package store

import (
	"fmt"
	"testing"
)

// 005-AC24: a participant's subscriptions upsert by endpoint and keep the newest ten; a revoked
// invite hides them; they go with their participant's room.
func TestPushSubscriptions(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := joinWithProfile(t, s, "r", "Alice", JoinProfile{RecoveryHash: "rec", SessionHash: "sess"})
	sub := func(n int, all bool) PushSubscription {
		return PushSubscription{ParticipantID: alice.ID, Endpoint: fmt.Sprintf("https://push.example/%d", n), P256dh: "k", Auth: "a", AllMessages: all}
	}
	for i := range MaxPushSubscriptions + 2 {
		if err := s.SavePushSubscription(ctx, sub(i, false)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SavePushSubscription(ctx, sub(MaxPushSubscriptions+1, true)); err != nil {
		t.Fatal(err)
	}
	got, err := s.PushSubscriptions(ctx, "r")
	if err != nil || len(got) != MaxPushSubscriptions || got[0].Endpoint != "https://push.example/2" || !got[len(got)-1].AllMessages {
		t.Fatalf("subscriptions %+v %v", got, err)
	}
	if err := s.DeletePushSubscription(ctx, alice.ID, "https://push.example/2"); err != nil {
		t.Fatal(err)
	}
	if err := s.DropPushSubscription(ctx, got[1].ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.PushSubscriptions(ctx, "r"); len(got) != MaxPushSubscriptions-2 {
		t.Fatalf("after delete and drop: %d", len(got))
	}
	if _, err := s.RevokeInvite(ctx, "r", alice.InviteID, true, "Admin"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.PushSubscriptions(ctx, "r"); len(got) != 0 {
		t.Fatalf("a revoked participant keeps %d", len(got))
	}
	if err := s.DeleteRoom(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM push_subscriptions`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows left after the room: %d %v", n, err)
	}
	_ = s.Close()
	for name, call := range map[string]func() error{
		"save":   func() error { return s.SavePushSubscription(ctx, sub(0, false)) },
		"delete": func() error { return s.DeletePushSubscription(ctx, 1, "x") },
		"drop":   func() error { return s.DropPushSubscription(ctx, 1) },
		"list":   func() error { _, err := s.PushSubscriptions(ctx, "r"); return err },
		"last":   func() error { _, err := s.LastMessageID(ctx); return err },
		"init":   func() error { _, err := s.SettingOrInit(ctx, "k", "v"); return err },
	} {
		if call() == nil {
			t.Errorf("%s on a closed store: no error", name)
		}
	}
}

// SettingOrInit keeps the first value written.
func TestSettingOrInit(t *testing.T) {
	s := openTest(t)
	for _, v := range []string{"first", "second"} {
		if got, err := s.SettingOrInit(t.Context(), "k", v); err != nil || got != "first" {
			t.Fatalf("%s: %q %v", v, got, err)
		}
	}
}
