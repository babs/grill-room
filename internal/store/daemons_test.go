package store

import (
	"context"
	"errors"
	"testing"
)

// 001-AC22: a paired daemon is found by its token hash only; pairing again replaces the token, and
// revoking the participant's invite or deleting the room ends it.
func TestDaemons(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p := seed(t, s, "r1")
	seed(t, s, "r2")
	if _, err := s.PairDaemon(ctx, "r2", p.ID, "laptop", "h1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pair into another room: %v", err)
	}
	d, err := s.PairDaemon(ctx, "r1", p.ID, "laptop", "h1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.DaemonByToken(ctx, "h1")
	if err != nil || got.ID != d.ID || got.RoomID != "r1" || got.ParticipantID != p.ID || got.Name != "laptop" {
		t.Fatalf("by token = %+v, %v", got, err)
	}
	d2, err := s.PairDaemon(ctx, "r1", p.ID, "desktop", "h2")
	if err != nil || d2.ID != d.ID {
		t.Fatalf("re-pair = %+v, %v", d2, err)
	}
	if _, err := s.DaemonByToken(ctx, "h1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old token still valid: %v", err)
	}
	if got, err := s.DaemonByToken(ctx, "h2"); err != nil || got.Name != "desktop" {
		t.Fatalf("new token = %+v, %v", got, err)
	}
	if _, err := s.RevokeInvite(ctx, "r1", p.InviteID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DaemonByToken(ctx, "h2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked participant's daemon still valid: %v", err)
	}
	if err := s.DeleteRoom(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM daemons WHERE room_id = 'r1'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("daemons left after room delete: %d %v", n, err)
	}
}
