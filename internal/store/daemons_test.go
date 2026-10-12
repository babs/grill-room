package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// 001-AC22: a paired daemon is found by its token hash only; pairing again replaces the token, and
// revoking the participant's invite or deleting the room ends it.
func TestDaemons(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p := seed(t, s, "r1")
	seed(t, s, "r2")
	if _, err := s.PairDaemon(ctx, "r2", p.ID, "h1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pair into another room: %v", err)
	}
	d, err := s.PairDaemon(ctx, "r1", p.ID, "h1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.DaemonByToken(ctx, "h1")
	if err != nil || got.ID != d.ID || got.RoomID != "r1" || got.ParticipantID != p.ID {
		t.Fatalf("by token = %+v, %v", got, err)
	}
	d2, err := s.PairDaemon(ctx, "r1", p.ID, "h2")
	if err != nil || d2.ID != d.ID {
		t.Fatalf("re-pair = %+v, %v", d2, err)
	}
	if _, err := s.DaemonByToken(ctx, "h1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old token still valid: %v", err)
	}
	if _, err := s.DaemonByToken(ctx, "h2"); err != nil {
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

// 003-AC17: the migration drops the machine names a room kept: the host-elected messages read
// "<name> holds the agent(s).", and the daemons' and turns' host columns are blank. A participant's
// text that happens to hold the phrase stays as written.
func TestMigrationDropsHostnames(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seed(t, s, "r1")
	for _, q := range []string{
		`UPDATE daemons SET name = 'laptop-of-alice'`,
		`UPDATE turns SET host = 'laptop-of-alice'`,
		`INSERT INTO messages(room_id, kind, sender, body, ts) VALUES ('r1', 'system', 'Alice', 'Alice hosts the agent, on laptop-of-alice.', '2026-01-01T00:00:00Z')`,
		`INSERT INTO messages(room_id, kind, sender, body, ts) VALUES ('r1', 'text', 'Bob', 'Bob hosts the agent, on laptop-of-alice.', '2026-01-01T00:00:00Z')`,
		fmt.Sprintf(`PRAGMA user_version = %d`, len(migrations)-1),
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := migrateTo(ctx, s.db, len(migrations)); err != nil {
		t.Fatal(err)
	}
	var system, text string
	if err := s.db.QueryRowContext(ctx, `SELECT body FROM messages WHERE kind = 'system' AND sender = 'Alice' ORDER BY id DESC`).Scan(&system); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT body FROM messages WHERE kind = 'text' AND sender = 'Bob'`).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if system != "Alice holds the agent(s)." || text != "Bob hosts the agent, on laptop-of-alice." {
		t.Fatalf("system %q, text %q", system, text)
	}
	var left int
	if err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM daemons WHERE name != '') + (SELECT count(*) FROM turns WHERE host != '')`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("names left: %d %v", left, err)
	}
}
