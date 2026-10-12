package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// sumBytes adds up, in Go, the byte length of every value query returns: the reference the SQL
// aggregates are checked against.
func sumBytes(t *testing.T, s *Store, query string, args ...any) int64 {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var n int64
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		n += int64(len(b))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return n
}

// 004-AC2: the rooms view's volumes, computed by SQL aggregates, match the bytes the store holds,
// counted in Go; text is counted in bytes, not characters.
func TestRoomOverviewsMatchTheStore(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p := seed(t, s, "a")
	seed(t, s, "b")
	if _, err := s.CreateRoom(ctx, "empty", "Empty"); err != nil {
		t.Fatal(err)
	}
	// Multi-byte text: octet_length, not length, must count it.
	last := &Message{RoomID: "a", Kind: KindText, ParticipantID: p.ID, Sender: p.Name, Body: "crème brûlée 🔥"}
	if err := s.InsertMessage(ctx, last); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAgentDocument(ctx, "a", "été.md", "md", "**déjà vu** ✓", "agent"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRoomQuota(ctx, "b", 3<<20); err != nil {
		t.Fatal(err)
	}
	list, err := s.RoomOverviews(ctx, 1<<20)
	if err != nil || len(list) != 3 {
		t.Fatalf("overviews: %+v %v", list, err)
	}
	by := map[string]RoomOverview{}
	for _, o := range list {
		by[o.ID] = o
	}
	for _, room := range []string{"a", "b", "empty"} {
		want := RoomVolume{
			Uploads:   sumBytes(t, s, `SELECT data FROM uploads WHERE room_id = ?`, room),
			Revisions: sumBytes(t, s, `SELECT v.body FROM revisions v JOIN documents d ON d.id = v.document_id WHERE d.room_id = ?`, room),
			Attachments: sumBytes(t, s, `SELECT t.data FROM attachments t JOIN annotation_messages m ON m.id = t.message_id
				JOIN annotations a ON a.id = m.annotation_id WHERE a.room_id = ?`, room),
			AgentSessions: sumBytes(t, s, `SELECT data FROM agent_sessions WHERE room_id = ?`, room),
			Transcripts: sumBytes(t, s, `SELECT body FROM messages WHERE room_id = ?`, room) +
				sumBytes(t, s, `SELECT html FROM messages WHERE room_id = ? AND html IS NOT NULL`, room) +
				sumBytes(t, s, `SELECT v.body FROM message_versions v JOIN messages m ON m.id = v.message_id WHERE m.room_id = ?`, room),
		}
		want.Total = want.Uploads + want.Revisions + want.Attachments + want.AgentSessions + want.Transcripts
		got := by[room]
		if got.Volume != want {
			t.Errorf("room %s volume %+v, want %+v", room, got.Volume, want)
		}
		used, _, err := s.RoomUsage(ctx, room, 1)
		if err != nil || got.Volume.Counted() != used {
			t.Errorf("room %s counted %d, usage %d %v", room, got.Volume.Counted(), used, err)
		}
	}
	// The seeded rooms hold something of every kind but room skills, which come with 004-AC10.
	if v := by["a"].Volume; v.Uploads == 0 || v.Revisions == 0 || v.Attachments == 0 || v.AgentSessions == 0 || v.Transcripts == 0 || v.RoomSkills != 0 {
		t.Fatalf("seed lacks a kind: %+v", v)
	}
	a, b, empty := by["a"], by["b"], by["empty"]
	if a.Name != "Room a" || a.Participants != 1 || a.Personas != 1 || a.HostParticipantID != p.ID || a.QuotaBytes != 0 || !a.LastActivity.Equal(last.TS) {
		t.Fatalf("room a: %+v (last line at %v)", a, last.TS)
	}
	if b.QuotaBytes != 3<<20 || b.Quota != 3<<20 || a.Quota != 1<<20 {
		t.Fatalf("room b quota %d", b.QuotaBytes)
	}
	// A new room holds its Default persona and nothing else: no line dates its activity.
	if empty.Volume.Total != 0 || empty.Participants != 0 || empty.Personas != 1 || empty.HostParticipantID != 0 || !empty.LastActivity.Equal(empty.CreatedAt) {
		t.Fatalf("empty room: %+v", empty)
	}
	// A revoked invite takes its participant out of the count.
	if _, err := s.RevokeInvite(ctx, "a", p.InviteID); err != nil {
		t.Fatal(err)
	}
	list, _ = s.RoomOverviews(ctx, 1<<20)
	for _, o := range list {
		if o.ID == "a" && o.Participants != 0 {
			t.Fatalf("revoked participant counted: %+v", o)
		}
	}
}

// 004-AC9: uploads and attachments count against the room's own quota when it has one, else the
// default; 0 returns it to the default.
func TestRoomQuotaOverridesTheDefault(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p, doc := annotated(t, s, "x")
	const def = 1 << 20
	used, quota, err := s.RoomUsage(ctx, "a", def)
	if err != nil || quota != def {
		t.Fatal(quota, err)
	}
	if err := s.SetRoomQuota(ctx, "a", used+5); err != nil {
		t.Fatal(err)
	}
	if _, q, _ := s.RoomUsage(ctx, "a", def); q != used+5 {
		t.Fatalf("own quota %d, want %d", q, used+5)
	}
	up := func(data string) error {
		_, err := s.AddUpload(ctx, &Upload{RoomID: "a", Author: "x", Filename: "f.md", ContentType: "text/markdown", Format: "md", Data: []byte(data), Text: data}, def)
		return err
	}
	if err := up("123456"); !errors.Is(err, ErrRoomFull) {
		t.Fatalf("upload past the room's quota: %v", err)
	}
	if _, err := s.CreateAnnotation(ctx, "a", doc, 0, nil, say(p, "pic", "123456"), def); !errors.Is(err, ErrRoomFull) {
		t.Fatalf("attachment past the room's quota: %v", err)
	}
	a, err := s.CreateAnnotation(ctx, "a", doc, 0, nil, say(p, "pic", "123"), def)
	if err != nil {
		t.Fatalf("attachment within the quota: %v", err)
	}
	if _, err := s.AddAnnotationMessage(ctx, "a", a.ID, say(p, "more", "123"), def); !errors.Is(err, ErrRoomFull) {
		t.Fatalf("second attachment past the quota: %v", err)
	}
	if err := up("12"); err != nil {
		t.Fatalf("upload exactly at the quota: %v", err)
	}
	if err := s.SetRoomQuota(ctx, "a", 0); err != nil {
		t.Fatal(err)
	}
	if err := up("123456"); err != nil {
		t.Fatalf("back on the default: %v", err)
	}
	// A quota raised above the default lets the room past it.
	if err := s.SetRoomQuota(ctx, "a", 2*def); err != nil {
		t.Fatal(err)
	}
	if err := up(string(make([]byte, def))); err != nil {
		t.Fatalf("upload past the default under a larger own quota: %v", err)
	}
	if err := s.SetRoomQuota(ctx, "nope", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown room: %v", err)
	}
	if err := s.SetRoomQuota(ctx, "a", -1); err == nil {
		t.Fatal("negative quota stored")
	}
}

// 004-AC3: the profiles view names a profile by its latest join, counts the skills it owns or
// published, and dates its last activity by its latest line (its join line at least).
func TestProfilesForAdmin(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	joinWithProfile(t, s, "b", "Bob", JoinProfile{RecoveryHash: "rec-b", SessionHash: "sess-b"})
	link(s, t, a.ProfileID, "alice@example.com", MagicBind, "bind", time.Minute)
	if _, err := s.UseMagicLink(ctx, "bind", MagicBind, "", a.ProfileID); err != nil {
		t.Fatal(err)
	}
	ps, err := s.Profiles(ctx)
	if err != nil || len(ps) != 2 || ps[0].Email != "alice@example.com" || ps[0].Rooms[0].RoomID != "a" || ps[1].Email != "" ||
		ps[0].Name != "Alice" || ps[1].Name != "Bob" || ps[0].UUID == "" || ps[0].Skills != 0 || ps[0].LastActivity.Before(a.JoinedAt) {
		t.Fatalf("profiles: %+v %v", ps, err)
	}
	m := &Message{RoomID: "a", Kind: KindText, ParticipantID: a.ID, Sender: a.Name, Body: "later"}
	if err := s.InsertMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	publish(t, s, a.ProfileID, false, "mine", "v1")
	publish(t, s, a.ProfileID, true, "ours", "v1")
	ps, _ = s.Profiles(ctx)
	if !ps[0].LastActivity.Equal(m.TS) || ps[0].Skills != 2 || ps[1].Skills != 0 {
		t.Fatalf("after a line and two skills: %+v", ps[0])
	}
}
