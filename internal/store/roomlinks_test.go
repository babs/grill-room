package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// linkSetup gives Ana a seat in rooms p and l, admin of both, and Bob a plain seat in p.
func linkSetup(t *testing.T, s *Store) (ana, anaL, bob *Participant) {
	t.Helper()
	ana = joinWithProfile(t, s, "p", "Ana", JoinProfile{RecoveryHash: "rec-ana", SessionHash: "sess-ana"})
	anaL = joinWithProfile(t, s, "l", "Ana L", JoinProfile{ID: ana.ProfileID})
	makeAdmin(t, s, "p", ana.ProfileID)
	makeAdmin(t, s, "l", ana.ProfileID)
	if _, err := s.CreateInvite(t.Context(), "p", "inv-bob", 0); err != nil {
		t.Fatal(err)
	}
	bob, _, err := s.Join(t.Context(), "inv-bob", "Bob", "tok-bob", JoinProfile{RecoveryHash: "rec-bob", SessionHash: "sess-bob"})
	if err != nil {
		t.Fatal(err)
	}
	return ana, anaL, bob
}

func makeAdmin(t *testing.T, s *Store, room string, profile int64) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(), `INSERT INTO room_admins(room_id, profile_id, created_at) VALUES(?,?,?)`, room, profile, now()); err != nil {
		t.Fatal(err)
	}
}

func linkAudit(ctx context.Context, action string) context.Context {
	return WithAudit(ctx, AuditEntry{At: time.Now(), Actor: ActorProfile + "x", Action: action, TargetKind: TargetRoom, TargetID: "p"}, nil)
}

// 023-AC2 023-AC3 023-AC6: one call links one or both directions, each with a line in both rooms
// and an audit entry; a link that exists comes back as is, with no line and no entry.
func TestCreateRoomLinks(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	ana, _, _ := linkSetup(t, s)
	for _, tc := range []struct {
		name          string
		reads, readBy bool
		want          [][2]string
		created       bool
	}{
		{"one direction", true, false, [][2]string{{"p", "l"}}, true},
		{"both, one existing", true, true, [][2]string{{"p", "l"}, {"l", "p"}}, true},
		{"both existing", true, true, [][2]string{{"p", "l"}, {"l", "p"}}, false},
	} {
		before, _ := s.AuditLog(ctx, AuditQuery{Limit: 100})
		ls, created, err := s.CreateRoomLinks(linkAudit(ctx, "room_link.create"), "p", "l", tc.reads, tc.readBy, ana)
		if err != nil || created != tc.created || len(ls) != len(tc.want) {
			t.Fatalf("%s: %+v %v %v", tc.name, ls, created, err)
		}
		for i, w := range tc.want {
			// Ana's newest seat in the two rooms is the one in l; 023-AC2: the caller admins and sits in both.
			if ls[i].ReaderRoom != w[0] || ls[i].ReadRoom != w[1] || ls[i].LinkedBy != "Ana L" || !ls[i].Member || !ls[i].CanRemove {
				t.Fatalf("%s: link %d %+v", tc.name, i, ls[i])
			}
		}
		after, _ := s.AuditLog(ctx, AuditQuery{Limit: 100})
		if added := len(after) - len(before); tc.created && added != 1 || !tc.created && added != 0 {
			t.Fatalf("%s: %d audit entries added", tc.name, added)
		}
	}
	// The last link made is "l reads p".
	if got := lastBody(t, s, "l"); got != "Ana L linked this room: it now reads Room p" {
		t.Fatalf("line in l: %q", got)
	}
	if got := lastBody(t, s, "p"); got != "Ana linked Room l: it now reads this room" {
		t.Fatalf("line in p: %q", got)
	}
	es, _ := s.AuditLog(ctx, AuditQuery{Action: "room_link.create", Limit: 10})
	var d map[string]any
	if len(es) != 2 || json.Unmarshal(es[0].Details, &d) != nil || d["reader_room"] != "l" || d["read_name"] != "Room p" || d["reader_name"] != "Room l" {
		t.Fatalf("audit %+v", es)
	}
}

// 023-AC2: only a room admin of both rooms links them; a room never reads itself.
func TestCreateRoomLinksRights(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	ana, _, bob := linkSetup(t, s)
	if _, _, err := s.CreateRoomLinks(ctx, "p", "l", true, true, bob); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member of one room: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM room_admins WHERE room_id = 'l'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateRoomLinks(ctx, "p", "l", true, false, ana); !errors.Is(err, ErrForbidden) {
		t.Fatalf("admin of one room: %v", err)
	}
	if _, _, err := s.CreateRoomLinks(ctx, "p", "nope", true, false, ana); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unknown room: %v", err)
	}
	if _, _, err := s.CreateRoomLinks(ctx, "p", "p", true, false, ana); err == nil {
		t.Fatal("a room linked to itself")
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO room_links(reader_room_id, read_room_id, created_at) VALUES('p', 'p', '')`); err == nil || !strings.Contains(err.Error(), "check") {
		t.Fatalf("self link past the CHECK: %v", err)
	}
	if n := countRows(t, s, "room_links"); n != 0 {
		t.Fatalf("%d links left by refusals", n)
	}
}

// 023-AC4: a room reads at most 10 rooms; the 11th link is refused with nothing written, in
// either direction of the call.
func TestRoomLinksCap(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	ana, _, _ := linkSetup(t, s)
	for i := range MaxRoomReads + 1 {
		id := fmt.Sprintf("r%d", i)
		if _, err := s.CreateRoom(ctx, id, "Room "+id); err != nil {
			t.Fatal(err)
		}
		makeAdmin(t, s, id, ana.ProfileID)
		if i == MaxRoomReads {
			break
		}
		if _, _, err := s.CreateRoomLinks(ctx, "p", id, true, false, ana); err != nil {
			t.Fatal(err)
		}
	}
	last := fmt.Sprintf("r%d", MaxRoomReads)
	msgs := countRows(t, s, "messages")
	_, _, err := s.CreateRoomLinks(ctx, "p", last, false, true, ana)
	if err != nil {
		t.Fatalf("r10 reads p: %v", err)
	}
	_, _, err = s.CreateRoomLinks(ctx, "p", "l", true, true, ana)
	if !errors.Is(err, ErrLinkCap) || err.Error() != "This room already reads 10 rooms. Remove one first." {
		t.Fatalf("11th: %v", err)
	}
	if n := countRows(t, s, "room_links"); n != MaxRoomReads+1 {
		t.Fatalf("%d links after the refusal", n)
	}
	if n := countRows(t, s, "messages"); n != msgs+2 {
		t.Fatalf("refusal wrote lines: %d, want %d", n, msgs+2)
	}
	// The other room past its cap names itself.
	for i := range MaxRoomReads - 1 {
		if _, _, err := s.CreateRoomLinks(ctx, last, fmt.Sprintf("r%d", i), true, false, ana); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.CreateRoomLinks(ctx, "l", last, false, true, ana); !errors.Is(err, ErrLinkCap) || err.Error() != "Room r10 already reads 10 rooms. Remove one first." {
		t.Fatalf("other room's cap: %v", err)
	}
}

// 023-AC1 023-AC5 023-AC8: both rooms list a link; an admin of either removes one direction alone,
// and a plain member lists them without the right to remove.
func TestDeleteRoomLink(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	ana, anaL, bob := linkSetup(t, s)
	ls, _, err := s.CreateRoomLinks(ctx, "p", "l", true, true, ana)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.RoomLinks(ctx, "p", bob.ProfileID)
	if err != nil || len(got) != 2 || got[0].Member || got[0].CanRemove {
		t.Fatalf("bob's view: %+v %v", got, err)
	}
	if got, _ := s.RoomLinks(ctx, "p", ana.ProfileID); len(got) != 2 || !got[0].Member || !got[0].CanRemove {
		t.Fatalf("ana's view: %+v", got)
	}
	if _, err := s.DeleteRoomLink(ctx, "p", ls[0].ID, bob); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member removes: %v", err)
	}
	if _, err := s.DeleteRoomLink(ctx, "p", 999, ana); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown link: %v", err)
	}
	// An admin of the read room only removes it, from the reader room's side.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM room_admins WHERE room_id = 'p'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteRoomLink(linkAudit(ctx, "room_link.delete"), "p", ls[0].ID, ana); err != nil {
		t.Fatal(err)
	}
	if got := lastBody(t, s, "l"); got != "Ana L removed the link: Room p no longer reads this room" {
		t.Fatalf("line in l: %q", got)
	}
	if got := lastBody(t, s, "p"); got != "Ana removed the link: this room no longer reads Room l" {
		t.Fatalf("line in p: %q", got)
	}
	if got, _ := s.RoomLinks(ctx, "l", anaL.ProfileID); len(got) != 1 || got[0].ReaderRoom != "l" {
		t.Fatalf("left: %+v", got)
	}
	if es, _ := s.AuditLog(ctx, AuditQuery{Action: "room_link.delete", Limit: 10}); len(es) != 1 || !strings.Contains(string(es[0].Details), `"read_room":"l"`) {
		t.Fatalf("audit %+v", es)
	}
}

// 023-AC7: deleting a room deletes its links both ways in its transaction, tells each other room
// once, and lists them in the delete's audit entry.
func TestDeleteRoomCascadesLinks(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	ana, _, _ := linkSetup(t, s)
	if _, err := s.CreateRoom(ctx, "x", "Room x"); err != nil {
		t.Fatal(err)
	}
	makeAdmin(t, s, "x", ana.ProfileID)
	for _, other := range []string{"l", "x"} {
		if _, _, err := s.CreateRoomLinks(ctx, "p", other, true, true, ana); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.CreateRoomLinks(ctx, "l", "x", true, false, ana); err != nil {
		t.Fatal(err)
	}
	linked, err := s.DeleteRoom(linkAudit(ctx, "room.delete"), "p")
	if err != nil || !slices.Equal(linked, []string{"l", "x"}) {
		t.Fatalf("delete: %v %v", linked, err)
	}
	for _, room := range []string{"l", "x"} {
		if got := lastBody(t, s, room); got != "Room p was deleted; the link is gone" {
			t.Fatalf("line in %s: %q", room, got)
		}
		if got, _ := s.RoomLinks(ctx, room, ana.ProfileID); len(got) != 1 {
			t.Fatalf("%s lists %+v", room, got)
		}
	}
	es, _ := s.AuditLog(ctx, AuditQuery{Action: "room.delete", Limit: 1})
	var d struct {
		Links []int64 `json:"links"`
		Count int     `json:"links_count"`
	}
	if len(es) != 1 || json.Unmarshal(es[0].Details, &d) != nil || len(d.Links) != 4 || d.Count != 4 || d.Links[0] != 1 {
		t.Fatalf("audit %+v", es)
	}
}

// 023-AC7: a room read by 120 others lists its first 100 link ids and the count, and the room
// delete's entry keeps its own details: the read-by side has no cap, the entry has one.
func TestDeleteRoomAuditFitsManyLinks(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	if _, err := s.CreateRoom(ctx, "hub", "Hub"); err != nil {
		t.Fatal(err)
	}
	const readers = 120
	for i := range readers {
		id := fmt.Sprintf("reader-%03d-%s", i, strings.Repeat("x", 60))
		if _, err := s.db.ExecContext(ctx, `INSERT INTO rooms(id, name, created_at) VALUES(?,?,?)`, id, strings.Repeat("n", 100), now()); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO room_links(reader_room_id, read_room_id, created_at) VALUES(?, 'hub', ?)`, id, now()); err != nil {
			t.Fatal(err)
		}
	}
	actx := WithAudit(ctx, AuditEntry{At: time.Now(), Actor: ActorProfile + "x", Action: "room.delete", TargetKind: TargetRoom, TargetID: "hub"},
		map[string]any{"as": "Ana", "participant": 7, "via": "session"})
	if _, err := s.DeleteRoom(actx, "hub"); err != nil {
		t.Fatal(err)
	}
	es, _ := s.AuditLog(ctx, AuditQuery{Action: "room.delete", Limit: 1})
	var d struct {
		Links       []int64 `json:"links"`
		Count       int     `json:"links_count"`
		As, Via     string
		Participant int
		Truncated   bool
	}
	if len(es) != 1 || json.Unmarshal(es[0].Details, &d) != nil || d.Truncated || len(d.Links) != maxAuditLinks || d.Count != readers ||
		d.As != "Ana" || d.Via != "session" || d.Participant != 7 {
		t.Fatalf("audit %s", es[0].Details)
	}
	if n := countRows(t, s, "room_links"); n != 0 {
		t.Fatalf("%d links left", n)
	}
}

// 023 Phase 1: room_links exists at the current schema version, with its shape.
func TestRoomLinksSchema(t *testing.T) {
	s := openTest(t)
	var n int
	if err := s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM information_schema.columns WHERE table_name = 'room_links'`).Scan(&n); err != nil || n != 5 {
		t.Fatalf("room_links columns: %d %v", n, err)
	}
	if baselineVersion+len(migrations) < 62 || !strings.Contains(migrations[62-baselineVersion-1], "room_links") {
		t.Fatal("migration 62 does not create room_links")
	}
}
