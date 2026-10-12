package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// 003-AC32: every profile has a random v4 uuid, from its creation or, for one made before, from
// the migration; participants carry their profile's, guests none.
func TestProfileUUIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// The schema just before profiles got their uuid.
	before := slices.IndexFunc(migrations, func(m string) bool { return strings.Contains(m, "ALTER TABLE profiles ADD COLUMN uuid") })
	if err := migrateTo(ctx, db, before); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"r1", "r2"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO profiles(recovery_hash, created_at) VALUES(?, '2026-01-01T00:00:00Z')`, h); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	joinWithProfile(t, s, "room", "Alice", JoinProfile{RecoveryHash: "r3", SessionHash: "s3"})
	if _, err := s.CreateInvite(ctx, "room", "inv-guest", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Join(ctx, "inv-guest", "Bob", "tok-guest", JoinProfile{}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT uuid FROM profiles`)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			t.Fatal(err)
		}
		if !uuidRe.MatchString(u) || seen[u] {
			t.Fatalf("uuid %q (seen %v)", u, seen)
		}
		seen[u] = true
	}
	if rows.Err() != nil || len(seen) != 3 {
		t.Fatalf("uuids %v %v", seen, rows.Err())
	}
	ps, err := s.Participants(ctx, "room")
	if err != nil || len(ps) != 2 {
		t.Fatalf("participants %+v %v", ps, err)
	}
	if !seen[ps[0].ProfileUUID] || ps[0].ProfileID == 0 || ps[1].ProfileUUID != "" || ps[1].ProfileID != 0 {
		t.Fatalf("participants %+v", ps)
	}
}

// 003-AC24: autocompact holds auto (0) or a count within claude's range, nothing else.
func TestAutocompactRange(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.CreateRoom(ctx, "r", "R"); err != nil {
		t.Fatal(err)
	}
	for n, ok := range map[int]bool{0: true, 100_000: true, 1_000_000: true, 99_999: false, 1_000_001: false, -1: false} {
		_, err := s.db.ExecContext(ctx, `UPDATE room_personas SET autocompact = ? WHERE room_id = 'r'`, n)
		if (err == nil) != ok {
			t.Errorf("autocompact %d: %v", n, err)
		}
	}
}

// 003-AC25: after a handoff turn that wrote its note, the new session takes the place of the
// persona's queued turn, fresh, with its cause; without the note, the room is told and nothing queues.
func TestHandoffNext(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p := joinWithProfile(t, s, "room", "Alice", JoinProfile{})
	ps, err := s.RoomPersonas(ctx, "room")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue(ctx, &QueuedTurn{RoomID: "room", PersonaID: ps[0].ID, ParticipantID: p.ID, Cause: "pressed by Alice"}); err != nil {
		t.Fatal(err)
	}
	tn := &Turn{RoomID: "room", PersonaID: ps[0].ID, PersonaName: ps[0].Name, ParticipantID: p.ID}
	if err := s.withTx(ctx, "t", func(tx *sql.Tx) error { return handoffNext(ctx, tx, tn, TurnReport{Created: []string{"other.md"}}) }); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Queue(ctx, "room"); len(got) != 1 || got[0].Handoff != "" {
		t.Fatalf("queue after no note %+v", got)
	}
	ms, _ := s.Messages(ctx, "room", 0, 100)
	if last := ms[len(ms)-1].Body; last != "No handoff: Grill did not write docs/_handoff-Grill.md, so it stays on its current session." {
		t.Fatalf("message %q", last)
	}
	if err := s.withTx(ctx, "t", func(tx *sql.Tx) error {
		return handoffNext(ctx, tx, tn, TurnReport{Revised: []string{"_handoff-Grill.md"}})
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Queue(ctx, "room")
	if err != nil || len(got) != 1 || !got[0].Fresh || got[0].Handoff != HandoffRead || got[0].Cause != HandoffCause || got[0].ParticipantID != p.ID {
		t.Fatalf("queue %+v %v", got, err)
	}
	popped, err := s.PopQueued(ctx, "room", false)
	if err != nil || popped.Handoff != HandoffRead || popped.Cause != HandoffCause {
		t.Fatalf("popped %+v %v", popped, err)
	}
}
