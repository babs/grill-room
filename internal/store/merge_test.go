package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type mergeFixture struct {
	s            *Store
	a, b         *Participant // A's seat in room a, B's seat in room b
	aBoth, bBoth *Participant // both seats in room both
	aMsg, bMsg   int64        // a line by each seat in room both
	bShared      int64        // B's shared skill
}

// newMergeFixture seeds profile A (Alice) and profile B (Bob): a room each, a room both sit in, with a
// line by each seat; personal skills that collide (x, dup), a shared skill, a template, a starter, a
// room B created, room admin roles (both admins of room both), B's session, links and disclaimer.
func newMergeFixture(t *testing.T) *mergeFixture {
	t.Helper()
	s := openTest(t)
	ctx := t.Context()
	f := &mergeFixture{s: s}
	f.a = joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	f.b = joinWithProfile(t, s, "b", "Bob", JoinProfile{RecoveryHash: "rec-b", SessionHash: "sess-b"})
	if _, err := s.CreateRoom(ctx, "both", "Room both"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInvite(ctx, "both", "inv-both", 0); err != nil {
		t.Fatal(err)
	}
	var err error
	if f.aBoth, _, err = s.Join(ctx, "inv-both", "Alice", "tok-a-both", JoinProfile{ID: f.a.ProfileID}); err != nil {
		t.Fatal(err)
	}
	if f.bBoth, _, err = s.Join(ctx, "inv-both", "Bob", "tok-b-both", JoinProfile{ID: f.b.ProfileID}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		p  *Participant
		id *int64
	}{{f.aBoth, &f.aMsg}, {f.bBoth, &f.bMsg}} {
		msg := &Message{RoomID: "both", Kind: KindText, ParticipantID: m.p.ID, Sender: m.p.Name, Body: "hi from " + m.p.Name}
		if err := s.InsertMessage(ctx, msg); err != nil {
			t.Fatal(err)
		}
		*m.id = msg.ID
	}
	for _, n := range []string{"x", "dup", "dup-2"} {
		publish(t, s, f.a.ProfileID, false, n, "a")
	}
	for _, n := range []string{"x", "dup", "z"} {
		publish(t, s, f.b.ProfileID, false, n, "b")
	}
	f.bShared = publish(t, s, f.b.ProfileID, true, "pub", "b").ID
	if _, err := s.PublishTemplate(ctx, f.b.ProfileID, "Bob", "Critic", PersonaFields{Name: "Critic", Badge: "🧐", Colour: "#112233", Text: "v1", Model: "sonnet", Effort: "medium"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveStarter(ctx, f.b.ProfileID, 0, testManifest("Bob's"), map[string][]byte{"doc/0": []byte("pdf")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProfileRoom(ctx, "bmade", "Bob made", f.b.ProfileID, "Bob", "inv-bmade", "tok-bmade", "link-bmade", "linkh-bmade", nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*Participant{f.aBoth, f.bBoth} {
		if _, err := s.SetRoomAdmin(ctx, "both", p.ID, true, nil); err != nil {
			t.Fatal(err)
		}
	}
	link(s, t, f.b.ProfileID, "bob-pending@example.com", MagicBind, "bob-magic", time.Minute)
	if _, err := s.CreateRecoveryLink(ctx, f.b.ProfileID, "bob-recovery", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE profiles SET skills_disclaimer_at = ? WHERE id = ?`, now(), f.b.ProfileID); err != nil {
		t.Fatal(err)
	}
	return f
}

func mergeAudit(at time.Time, keep int64) context.Context {
	return WithAudit(context.Background(), AuditEntry{At: at, Actor: ActorSuperAdmin, Action: "profile.merge", TargetKind: TargetProfile,
		TargetID: strconv.FormatInt(keep, 10)}, nil)
}

// 011-AC10: the preview names both sides, marks rooms where both sit, lists skills with their
// renames (-2, then -3 when -2 is taken), counts, admin roles and the email A ends with.
func TestMergePreview(t *testing.T) {
	f := newMergeFixture(t)
	ctx := t.Context()
	p, err := f.s.MergePreview(ctx, f.a.ProfileID, f.b.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Keep.Name != "Alice" || p.Merge.Name != "Bob" || p.Keep.UUID == "" || p.Merge.UUID == "" {
		t.Fatalf("sides: %+v %+v", p.Keep, p.Merge)
	}
	rooms := map[string][2]bool{}
	for _, r := range p.Rooms {
		rooms[r.RoomID] = [2]bool{r.Keep, r.Merge}
	}
	want := map[string][2]bool{"a": {true, false}, "b": {false, true}, "both": {true, true}, "bmade": {false, true}}
	if fmt.Sprint(rooms) != fmt.Sprint(want) || len(p.Rooms) != len(want) {
		t.Fatalf("rooms: %+v", p.Rooms)
	}
	if fmt.Sprint(p.Skills.Renamed) != "[{dup dup-3} {x x-2}]" || !slices.Equal(p.Skills.Merge, []string{"dup", "x", "z"}) {
		t.Fatalf("skills: %+v", p.Skills)
	}
	if p.Seats != (MergeCount{2, 3}) {
		t.Fatalf("seats: %+v", p.Seats)
	}
	if p.SharedSkills != (MergeCount{0, 1}) || p.Templates != (MergeCount{0, 1}) || p.Starters != (MergeCount{0, 1}) || p.RoomsCreated != (MergeCount{0, 1}) {
		t.Fatalf("counts: %+v %+v %+v %+v", p.SharedSkills, p.Templates, p.Starters, p.RoomsCreated)
	}
	if !slices.Equal(p.RoomAdmin.Keep, []string{"Room both"}) || !slices.Equal(p.RoomAdmin.Merge, []string{"Bob made", "Room both"}) {
		t.Fatalf("room admin: %+v", p.RoomAdmin)
	}
	if len(p.OverLimits) != 0 || p.EmailAfter != "" {
		t.Fatalf("limits, email: %+v %q", p.OverLimits, p.EmailAfter)
	}
	if _, err := f.s.MergePreview(ctx, f.a.ProfileID, f.a.ProfileID); !errors.Is(err, ErrSelfMerge) {
		t.Fatalf("self: %v", err)
	}
	if _, err := f.s.MergePreview(ctx, f.a.ProfileID, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}

// 011-AC11: a renamed skill stays a valid name: cut to 64 characters, past every name taken.
func TestSkillRenamesStayValidNames(t *testing.T) {
	long := strings.Repeat("a", 63) + "b"
	got := skillRenames([]string{long, "k", "k-2", "k-3"}, []string{long, "k", "k-2"})
	want := []MergeRename{{long, strings.Repeat("a", 62) + "-2"}, {"k", "k-4"}, {"k-2", "k-2-2"}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// 011-AC11, 011-AC12, 011-AC13, 011-AC14: everything of B moves to A in one transaction, B is
// deleted, the room list is the union, and one audit entry holds the uuids and counts, no email.
func TestMergeProfiles(t *testing.T) {
	f := newMergeFixture(t)
	s, ctx := f.s, t.Context()
	A, B := f.a.ProfileID, f.b.ProfileID
	max := 3
	if err := s.SetProfileMaxRooms(ctx, A, &max); err != nil {
		t.Fatal(err)
	}
	snooze := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if err := s.SnoozeEmailNudge(ctx, A, snooze); err != nil {
		t.Fatal(err)
	}
	pre, err := s.MergePreview(ctx, A, B)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	kept, moved, err := s.MergeProfiles(mergeAudit(t0, A), A, B)
	if err != nil {
		t.Fatal(err)
	}
	if kept.ID != A || kept.Name != "Alice" || kept.UUID != pre.Keep.UUID {
		t.Fatalf("kept: %+v", kept)
	}
	wantMoved := MergeMoved{Seats: 3, Skills: 3, SharedSkills: 1, Templates: 1, Starters: 1, RoomsCreated: 1, RoomAdmins: 2, Sessions: 1}
	if moved != wantMoved || moved.Seats != int64(pre.Seats.Merge) {
		t.Fatalf("moved %+v, want %+v", moved, wantMoved)
	}
	// A chose no name: it keeps the one it showed, not the name of B's newer seats (011-AC12).
	var shown string
	if err := s.db.QueryRowContext(ctx, `SELECT `+displayNameSQL+` FROM profiles p WHERE p.id = ?`, A).Scan(&shown); err != nil || shown != "Alice" {
		t.Errorf("A shows %q %v, want Alice", shown, err)
	}
	// Both seats of room both stay, on A, and each line keeps its author seat.
	for _, m := range []struct{ msg, seat int64 }{{f.aMsg, f.aBoth.ID}, {f.bMsg, f.bBoth.ID}} {
		if n := count(t, s, `SELECT count(*) FROM messages WHERE id = ? AND participant_id = ?`, m.msg, m.seat); n != 1 {
			t.Errorf("message %d lost its author seat %d", m.msg, m.seat)
		}
	}
	if n := count(t, s, `SELECT count(*) FROM participants WHERE room_id = 'both' AND profile_id = ?`, A); n != 2 {
		t.Errorf("seats of room both on A: %d, want 2", n)
	}
	var personal []string
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM skills WHERE profile_id = ? ORDER BY name`, A)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		personal = append(personal, n)
	}
	_ = rows.Close()
	if !slices.Equal(personal, []string{"dup", "dup-2", "dup-3", "x", "x-2", "z"}) {
		t.Errorf("A's personal skills: %v", personal)
	}
	for _, c := range []struct {
		q    string
		args []any
	}{
		{`SELECT count(*) FROM skills WHERE id = ? AND publisher_id = ?`, []any{f.bShared, A}},
		{`SELECT count(*) FROM skill_versions WHERE skill_id = ? AND author_id = ?`, []any{f.bShared, A}},
		{`SELECT count(*) FROM persona_templates WHERE profile_id = ?`, []any{A}},
		{`SELECT count(*) FROM starters WHERE profile_id = ?`, []any{A}},
		{`SELECT count(*) FROM rooms WHERE id = 'bmade' AND creator_profile_id = ?`, []any{A}},
	} {
		if n := count(t, s, c.q, c.args...); n != 1 {
			t.Errorf("%s: %d, want 1", c.q, n)
		}
	}
	if n := count(t, s, `SELECT count(*) FROM room_admins WHERE room_id = 'both'`); n != 1 {
		t.Errorf("room both keeps %d admin rows, want 1", n)
	}
	if n := count(t, s, `SELECT count(*) FROM room_admins WHERE room_id = 'bmade' AND profile_id = ?`, A); n != 1 {
		t.Errorf("B's admin role did not move")
	}
	if prof, err := s.ProfileBySession(ctx, "sess-b"); err != nil || prof.ID != A {
		t.Errorf("B's session: %+v %v", prof, err)
	}
	var ids []string
	if prof, err := s.ProfileBySession(ctx, "sess-a"); err == nil {
		for _, r := range prof.Rooms {
			ids = append(ids, r.RoomID)
		}
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"a", "b", "bmade", "both"}) {
		t.Errorf("A's rooms, once each: %v", ids)
	}
	for _, q := range []string{`SELECT count(*) FROM magic_links WHERE token_hash = 'bob-magic'`, `SELECT count(*) FROM recovery_links WHERE token_hash = 'bob-recovery'`,
		`SELECT count(*) FROM profiles WHERE id = ` + strconv.FormatInt(B, 10)} {
		if n := count(t, s, q); n != 0 {
			t.Errorf("%s: %d left", q, n)
		}
	}
	var maxRooms int
	var nudge, disclaimer string
	if err := s.db.QueryRowContext(ctx, `SELECT max_rooms, email_nudge_until, skills_disclaimer_at FROM profiles WHERE id = ?`, A).Scan(&maxRooms, &nudge, &disclaimer); err != nil {
		t.Fatal(err)
	}
	if maxRooms != 3 || !parseTS(nudge).Equal(snooze) || disclaimer == "" {
		t.Errorf("A's own settings: max %d, nudge %s, disclaimer %q", maxRooms, nudge, disclaimer)
	}
	log, err := s.AuditLog(ctx, AuditQuery{Action: "profile.merge", Limit: 5})
	if err != nil || len(log) != 1 {
		t.Fatalf("audit: %+v %v", log, err)
	}
	var d struct {
		KeepUUID  string     `json:"keep_uuid"`
		KeepName  string     `json:"keep_name"`
		MergeUUID string     `json:"merge_uuid"`
		MergeName string     `json:"merge_name"`
		Moved     MergeMoved `json:"moved"`
	}
	if err := json.Unmarshal(log[0].Details, &d); err != nil {
		t.Fatal(err)
	}
	if d.KeepUUID != pre.Keep.UUID || d.MergeUUID != pre.Merge.UUID || d.KeepName != "Alice" || d.MergeName != "Bob" || d.Moved != wantMoved ||
		log[0].TargetID != strconv.FormatInt(A, 10) || log[0].TargetName != "Alice" || strings.Contains(string(log[0].Details), "@") {
		t.Fatalf("audit entry: %+v %s", log[0], log[0].Details)
	}
	if _, _, err := s.MergeProfiles(ctx, A, A); !errors.Is(err, ErrSelfMerge) {
		t.Fatalf("self merge: %v", err)
	}
	if _, _, err := s.MergeProfiles(ctx, A, B); !errors.Is(err, ErrNotFound) {
		t.Fatalf("merge of a gone profile: %v", err)
	}
}

// 011-AC12: A keeps its email; without one it takes B's; B's other address goes with B.
func TestMergeEmailPrecedence(t *testing.T) {
	for _, c := range []struct{ name, a, b, want string }{
		{"A only", "a@example.com", "", "a@example.com"},
		{"B only", "", "b@example.com", "b@example.com"},
		{"both", "a@example.com", "b@example.com", "a@example.com"},
		{"neither", "", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := openTest(t)
			ctx := t.Context()
			a := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
			b := joinWithProfile(t, s, "b", "Bob", JoinProfile{RecoveryHash: "rec-b", SessionHash: "sess-b"})
			for id, e := range map[int64]string{a.ProfileID: c.a, b.ProfileID: c.b} {
				if _, err := s.db.ExecContext(ctx, `UPDATE profiles SET email = NULLIF(?, '') WHERE id = ?`, e, id); err != nil {
					t.Fatal(err)
				}
			}
			p, err := s.MergePreview(ctx, a.ProfileID, b.ProfileID)
			if err != nil || p.EmailAfter != c.want {
				t.Fatalf("preview: %q %v, want %q", p.EmailAfter, err, c.want)
			}
			if _, _, err := s.MergeProfiles(ctx, a.ProfileID, b.ProfileID); err != nil {
				t.Fatal(err)
			}
			var got string
			if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(email, '') FROM profiles WHERE id = ?`, a.ProfileID).Scan(&got); err != nil || got != c.want {
				t.Fatalf("email %q %v, want %q", got, err, c.want)
			}
			if n := count(t, s, `SELECT count(*) FROM profiles WHERE email = 'b@example.com' AND id != ?`, a.ProfileID); n != 0 {
				t.Fatal("B's email outlived B")
			}
		})
	}
}

// mergeState is every row a merge may touch, as text.
func mergeState(t *testing.T, s *Store) string {
	t.Helper()
	var out strings.Builder
	for _, q := range []string{
		`SELECT id, COALESCE(email, ''), COALESCE(skills_disclaimer_at, '') FROM profiles`,
		`SELECT id, COALESCE(profile_id, 0) FROM participants`,
		`SELECT id, name, COALESCE(profile_id, 0), COALESCE(publisher_id, 0) FROM skills`,
		`SELECT id, COALESCE(author_id, 0) FROM skill_versions`,
		`SELECT id, COALESCE(profile_id, 0) FROM persona_templates`,
		`SELECT id, COALESCE(profile_id, 0) FROM starters`,
		`SELECT id, COALESCE(creator_profile_id, 0) FROM rooms`,
		`SELECT room_id, profile_id FROM room_admins`,
		`SELECT id, profile_id FROM profile_sessions`,
		`SELECT id, profile_id FROM magic_links`,
		`SELECT id, profile_id FROM recovery_links`,
		`SELECT id, participant_id FROM messages`,
		`SELECT count(*), 0 FROM audit_log`,
	} {
		rows, err := s.db.QueryContext(t.Context(), q+` ORDER BY 1, 2`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintln(&out, q, vals)
		}
		_ = rows.Close()
	}
	return out.String()
}

// 011-AC13: a failure mid-merge leaves both profiles and every row as they were, with no entry.
func TestMergeFailureRollsBack(t *testing.T) {
	f := newMergeFixture(t)
	if _, err := f.s.db.ExecContext(t.Context(), `UPDATE profiles SET email = 'b@example.com' WHERE id = ?`, f.b.ProfileID); err != nil {
		t.Fatal(err)
	}
	before := mergeState(t, f.s)
	boom := errors.New("boom")
	mergeFault = func() error { return boom }
	t.Cleanup(func() { mergeFault = nil })
	if _, _, err := f.s.MergeProfiles(mergeAudit(time.Now(), f.a.ProfileID), f.a.ProfileID, f.b.ProfileID); !errors.Is(err, boom) {
		t.Fatalf("merge: %v", err)
	}
	if after := mergeState(t, f.s); after != before {
		t.Fatalf("rows changed by a failed merge:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if prof, err := f.s.ProfileBySession(t.Context(), "sess-b"); err != nil || prof.ID != f.b.ProfileID {
		t.Fatalf("B after a failed merge: %+v %v", prof, err)
	}
}

// 011-AC14: a merged profile above a per-profile limit keeps everything and adds no more of that
// kind; the preview names each limit it passes.
func TestMergeOverLimits(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	a := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	b := joinWithProfile(t, s, "b", "Bob", JoinProfile{RecoveryHash: "rec-b", SessionHash: "sess-b"})
	tpl := PersonaFields{Name: "Critic", Badge: "🧐", Colour: "#112233", Text: "v1", Model: "sonnet", Effort: "medium"}
	for _, p := range []*Participant{a, b} {
		for i := range MaxProfileSkills/2 + 1 {
			publish(t, s, p.ProfileID, i%2 == 0, fmt.Sprintf("k%d-%d", p.ProfileID, i), "x")
		}
		for i := range MaxProfileTemplates/2 + 1 {
			if _, err := s.PublishTemplate(ctx, p.ProfileID, "x", fmt.Sprint("T", i), tpl); err != nil {
				t.Fatal(err)
			}
		}
		for i := range MaxProfileStarters/2 + 1 {
			if _, err := s.SaveStarter(ctx, p.ProfileID, 0, testManifest(fmt.Sprint("S", i)), map[string][]byte{"doc/0": []byte("pdf")}); err != nil {
				t.Fatal(err)
			}
		}
		id := fmt.Sprint("made", p.ProfileID)
		if _, err := s.CreateProfileRoom(ctx, id, id, p.ProfileID, "x", "inv-"+id, "tok-"+id, "l-"+id, "lh-"+id, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := putSettings(ctx, s, InstanceSettings{RoomsPerProfile: 1, RoomCreation: CreationAnyone, PluginMaxMB: DefaultPluginMaxMB, PurgeEmptyProfiles: true}); err != nil {
		t.Fatal(err)
	}
	pre, err := s.MergePreview(ctx, a.ProfileID, b.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	want := []MergeLimit{{"skills", 2 * (MaxProfileSkills/2 + 1), MaxProfileSkills}, {"templates", 2 * (MaxProfileTemplates/2 + 1), MaxProfileTemplates},
		{"starters", 2 * (MaxProfileStarters/2 + 1), MaxProfileStarters}, {"rooms_created", 2, 1}}
	if !slices.Equal(pre.OverLimits, want) {
		t.Fatalf("over limits: %+v, want %+v", pre.OverLimits, want)
	}
	if _, _, err := s.MergeProfiles(ctx, a.ProfileID, b.ProfileID); err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]int{
		`SELECT count(*) FROM skills WHERE profile_id = ? OR publisher_id = ?1`: 2 * (MaxProfileSkills/2 + 1),
		`SELECT count(*) FROM persona_templates WHERE profile_id = ?`:           2 * (MaxProfileTemplates/2 + 1),
		`SELECT count(*) FROM starters WHERE profile_id = ?`:                    2 * (MaxProfileStarters/2 + 1),
		`SELECT count(*) FROM rooms WHERE creator_profile_id = ?`:               2,
	} {
		if n := count(t, s, q, a.ProfileID); n != want {
			t.Errorf("%s: %d, want %d", q, n, want)
		}
	}
	if _, err := s.PublishSkill(ctx, a.ProfileID, false, skillUpload("one-more", "x")); !errors.Is(err, ErrLimit) {
		t.Errorf("one more skill: %v", err)
	}
	if _, err := s.PublishTemplate(ctx, a.ProfileID, "x", "one more", tpl); !errors.Is(err, ErrLimit) {
		t.Errorf("one more template: %v", err)
	}
	if _, err := s.SaveStarter(ctx, a.ProfileID, 0, testManifest("one more"), map[string][]byte{"doc/0": []byte("pdf")}); !errors.Is(err, ErrLimit) {
		t.Errorf("one more starter: %v", err)
	}
	if _, err := s.CreateProfileRoom(ctx, "more", "more", a.ProfileID, "x", "inv-more", "tok-more", "l-more", "lh-more", nil); !errors.Is(err, ErrCreationRefused) {
		t.Errorf("one more room: %v", err)
	}
}

// ageProfile moves every activity of profile id back by d: creation, joins, lines and sign-ins.
func ageProfile(t *testing.T, s *Store, id int64, d time.Duration) {
	t.Helper()
	mod := fmt.Sprintf("-%d seconds", int64(d/time.Second))
	for _, q := range []string{
		`UPDATE profiles SET created_at = strftime('%Y-%m-%dT%H:%M:%fZ', created_at, ?1) WHERE id = ?2`,
		`UPDATE participants SET joined_at = strftime('%Y-%m-%dT%H:%M:%fZ', joined_at, ?1) WHERE profile_id = ?2`,
		`UPDATE messages SET ts = strftime('%Y-%m-%dT%H:%M:%fZ', ts, ?1) WHERE participant_id IN (SELECT id FROM participants WHERE profile_id = ?2)`,
		`UPDATE profile_sessions SET created_at = strftime('%Y-%m-%dT%H:%M:%fZ', created_at, ?1) WHERE profile_id = ?2`,
	} {
		if _, err := s.db.ExecContext(t.Context(), q, mod, id); err != nil {
			t.Fatal(err)
		}
	}
}

func newProfile(t *testing.T, s *Store, tag string) int64 {
	t.Helper()
	id, err := createProfile(t.Context(), s.db, "rec-"+tag, "sess-"+tag)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func purgeAudit(at time.Time) context.Context {
	return WithAudit(context.Background(), AuditEntry{At: at, Actor: ActorSystem, Action: "profile.purge", TargetKind: TargetInstance}, nil)
}

// 011-AC15, 011-AC16: an empty profile goes once its last activity is over 24 h old; anything a
// person would miss keeps it at any age; one audit entry per run that deletes, none otherwise.
func TestPurgeEmptyProfiles(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	uuidOf := func(id int64) string {
		var u string
		if err := s.db.QueryRowContext(ctx, `SELECT uuid FROM profiles WHERE id = ?`, id).Scan(&u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	empty := newProfile(t, s, "empty")
	revoked := joinWithProfile(t, s, "rv", "Rev", JoinProfile{RecoveryHash: "rec-rv", SessionHash: "sess-rv"})
	if _, err := s.RevokeInvite(ctx, "rv", revoked.InviteID, true, "Admin"); err != nil {
		t.Fatal(err)
	}
	keepers := map[string]int64{}
	keepers["email"] = newProfile(t, s, "email")
	if _, err := s.db.ExecContext(ctx, `UPDATE profiles SET email = 'k@example.com' WHERE id = ?`, keepers["email"]); err != nil {
		t.Fatal(err)
	}
	keepers["seat"] = joinWithProfile(t, s, "seat", "Seat", JoinProfile{RecoveryHash: "rec-seat", SessionHash: "sess-seat"}).ProfileID
	keepers["skill"] = newProfile(t, s, "skill")
	publish(t, s, keepers["skill"], false, "mine", "x")
	keepers["shared"] = newProfile(t, s, "shared")
	publish(t, s, keepers["shared"], true, "ours", "x")
	keepers["template"] = newProfile(t, s, "template")
	if _, err := s.PublishTemplate(ctx, keepers["template"], "x", "T", PersonaFields{Name: "C", Badge: "🧐", Colour: "#112233", Text: "v1", Model: "sonnet", Effort: "medium"}); err != nil {
		t.Fatal(err)
	}
	keepers["starter"] = newProfile(t, s, "starter")
	if _, err := s.SaveStarter(ctx, keepers["starter"], 0, testManifest("S"), map[string][]byte{"doc/0": []byte("pdf")}); err != nil {
		t.Fatal(err)
	}
	// A room admin whose seat's invite was revoked: the role alone keeps the profile.
	adm := joinWithProfile(t, s, "adm", "Adm", JoinProfile{RecoveryHash: "rec-adm", SessionHash: "sess-adm"})
	if _, err := s.SetRoomAdmin(ctx, "adm", adm.ID, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeInvite(ctx, "adm", adm.InviteID, true, "Admin"); err != nil {
		t.Fatal(err)
	}
	keepers["admin"] = adm.ProfileID
	// Recent activity keeps an otherwise empty profile: a line by its revoked seat, a sign-in.
	talker := joinWithProfile(t, s, "tk", "Talker", JoinProfile{RecoveryHash: "rec-tk", SessionHash: "sess-tk"})
	if _, err := s.RevokeInvite(ctx, "tk", talker.InviteID, true, "Admin"); err != nil {
		t.Fatal(err)
	}
	signer := newProfile(t, s, "signer")

	now := time.Now()
	for _, id := range append(slices.Collect(func(yield func(int64) bool) {
		for _, id := range keepers {
			yield(id)
		}
	}), empty, revoked.ProfileID, talker.ProfileID, signer) {
		ageProfile(t, s, id, 23*time.Hour)
	}
	got, err := s.PurgeEmptyProfiles(purgeAudit(now), now, EmptyProfileIdle)
	if err != nil || len(got) != 0 {
		t.Fatalf("at 23 h: %v %v", got, err)
	}
	if log, _ := s.AuditLog(ctx, AuditQuery{Limit: 5}); len(log) != 0 {
		t.Fatalf("a run that deleted nothing wrote %+v", log)
	}
	for _, id := range []int64{empty, revoked.ProfileID, talker.ProfileID, signer} {
		ageProfile(t, s, id, 2*time.Hour)
	}
	for _, id := range keepers {
		ageProfile(t, s, id, 1000*time.Hour)
	}
	msg := &Message{RoomID: "tk", Kind: KindText, ParticipantID: talker.ID, Sender: "Talker", Body: "still here"}
	if err := s.InsertMessage(ctx, msg); err != nil {
		t.Fatal(err)
	}
	if err := createSession(ctx, s.db, signer, "sess-signer-2"); err != nil {
		t.Fatal(err)
	}
	wantGone := []string{uuidOf(empty), uuidOf(revoked.ProfileID)}
	got, err = s.PurgeEmptyProfiles(purgeAudit(now), now, EmptyProfileIdle)
	if err != nil || !slices.Equal(got, wantGone) {
		t.Fatalf("at 25 h: %v %v, want %v", got, err, wantGone)
	}
	for name, id := range keepers {
		if n := count(t, s, `SELECT count(*) FROM profiles WHERE id = ?`, id); n != 1 {
			t.Errorf("profile with a %s was purged", name)
		}
	}
	for _, id := range []int64{talker.ProfileID, signer} {
		if n := count(t, s, `SELECT count(*) FROM profiles WHERE id = ?`, id); n != 1 {
			t.Errorf("profile %d with recent activity was purged", id)
		}
	}
	// As DeleteProfile: the seat stays, detached.
	if n := count(t, s, `SELECT count(*) FROM participants WHERE id = ? AND profile_id IS NULL`, revoked.ID); n != 1 {
		t.Errorf("the purged profile's seat was not detached")
	}
	log, err := s.AuditLog(ctx, AuditQuery{Limit: 5})
	if err != nil || len(log) != 1 || log[0].Actor != ActorSystem {
		t.Fatalf("audit: %+v %v", log, err)
	}
	var d struct {
		Count int      `json:"count"`
		UUIDs []string `json:"uuids"`
	}
	if err := json.Unmarshal(log[0].Details, &d); err != nil || d.Count != 2 || !slices.Equal(d.UUIDs, wantGone) {
		t.Fatalf("audit details: %s %v", log[0].Details, err)
	}
	if got, err = s.PurgeEmptyProfiles(purgeAudit(now), now, EmptyProfileIdle); err != nil || len(got) != 0 {
		t.Fatalf("second run: %v %v", got, err)
	}
	if log, _ := s.AuditLog(ctx, AuditQuery{Limit: 5}); len(log) != 1 {
		t.Fatalf("an empty run wrote an entry: %+v", log)
	}
}

// 011-AC16: a run deletes at most purgeBatch profiles, so its one entry keeps every uuid.
func TestPurgeEmptyProfilesBatch(t *testing.T) {
	s := openTest(t)
	for i := range purgeBatch + 1 {
		ageProfile(t, s, newProfile(t, s, strconv.Itoa(i)), 25*time.Hour)
	}
	now := time.Now()
	first, err := s.PurgeEmptyProfiles(purgeAudit(now), now, EmptyProfileIdle)
	if err != nil || len(first) != purgeBatch {
		t.Fatalf("first run: %d %v", len(first), err)
	}
	log, err := s.AuditLog(t.Context(), AuditQuery{Limit: 5})
	if err != nil || len(log) != 1 || len(log[0].Details) > MaxAuditDetails || !strings.Contains(string(log[0].Details), first[purgeBatch-1]) {
		t.Fatalf("entry: %+v %v", log, err)
	}
	if rest, err := s.PurgeEmptyProfiles(purgeAudit(now), now, EmptyProfileIdle); err != nil || len(rest) != 1 {
		t.Fatalf("second run: %v %v", rest, err)
	}
}
