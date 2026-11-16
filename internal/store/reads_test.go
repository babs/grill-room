package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// readsFixture is a store with helpers to seat people and write lines.
type readsFixture struct {
	t *testing.T
	s *Store
}

func newReads(t *testing.T) *readsFixture { return &readsFixture{t: t, s: openTest(t)} }

// room creates room id with an invite "inv-<id>".
func (f *readsFixture) room(id string) {
	f.t.Helper()
	if _, err := f.s.CreateRoom(f.t.Context(), id, "Room "+id); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.s.CreateInvite(f.t.Context(), id, "inv-"+id, 0, ""); err != nil {
		f.t.Fatal(err)
	}
}

// seat joins room as name, on profile when not 0, else on a new profile.
func (f *readsFixture) seat(room, name string, profile int64) *Participant {
	f.t.Helper()
	prof := JoinProfile{ID: profile}
	if profile == 0 {
		tok := fmt.Sprintf("%s-%s-%d", room, name, time.Now().UnixNano())
		prof = JoinProfile{RecoveryHash: "rec-" + tok, SessionHash: "sess-" + tok}
	}
	p, _, err := f.s.Join(f.t.Context(), "inv-"+room, name, fmt.Sprintf("tok-%s-%s-%d", room, name, time.Now().UnixNano()), prof)
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

// lines writes n lines of kind to room by participant pid (0: none, as a persona's answer) and
// returns the newest id.
func (f *readsFixture) lines(room, kind string, pid int64, n int, body string) int64 {
	f.t.Helper()
	var id int64
	err := f.s.db.QueryRowContext(f.t.Context(), `WITH ins AS (INSERT INTO messages(room_id, ts, kind, participant_id, sender, body)
		SELECT ?1::text, '2026-01-01T00:00:00Z', ?2::text, NULLIF(?3::bigint, 0), 'x', ?5::text FROM generate_series(1, ?4::int) RETURNING id)
		SELECT MAX(id) FROM ins`, room, kind, pid, n, body).Scan(&id)
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *readsFixture) marker(profile int64, room string) int64 {
	f.t.Helper()
	var v int64
	if err := f.s.db.QueryRowContext(f.t.Context(), `SELECT read_through FROM room_reads WHERE profile_id = ? AND room_id = ?`, profile, room).Scan(&v); err != nil {
		f.t.Fatalf("marker of %d in %s: %v", profile, room, err)
	}
	return v
}

func (f *readsFixture) newest(room string) int64 {
	f.t.Helper()
	return int64(count(f.t, f.s, `SELECT COALESCE(MAX(id), 0) FROM messages WHERE room_id = ?`, room))
}

func (f *readsFixture) unread(profile int64) map[string]Unread {
	f.t.Helper()
	u, err := f.s.UnreadLines(f.t.Context(), profile)
	if err != nil {
		f.t.Fatal(err)
	}
	return u
}

// 041-AC10: text and agent lines count, persona answers too; system and voice lines, deleted
// lines and the lines of either seat of the profile do not.
func TestUnreadKinds(t *testing.T) {
	f := newReads(t)
	f.room("r")
	a1 := f.seat("r", "Alice", 0)
	a2 := f.seat("r", "Alice2", a1.ProfileID)
	bob := f.seat("r", "Bob", 0)
	f.lines("r", KindText, bob.ID, 1, "hi")
	f.lines("r", KindAgent, 0, 1, "persona answer")
	f.lines("r", KindVoice, bob.ID, 1, "spoken")
	f.lines("r", KindSystem, 0, 1, "Carol joined")
	f.lines("r", KindText, a1.ID, 1, "mine")
	f.lines("r", KindText, a2.ID, 1, "mine too")
	gone := f.lines("r", KindText, bob.ID, 1, "deleted")
	if _, err := f.s.DeleteMessage(t.Context(), "r", gone, bob.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.unread(a1.ProfileID)["r"]; got.Count != 2 || len(got.Bodies) != 0 {
		t.Fatalf("Alice's unread %+v, want 2 and no body", got)
	}
	// Bob's own text line is out of his count, Alice's two are in.
	if got := f.unread(bob.ProfileID)["r"].Count; got != 3 {
		t.Fatalf("Bob's unread %d, want 3", got)
	}
}

// 041-AC11: the count stops at 100, and a room with no marker reads as nothing unread.
func TestUnreadCap(t *testing.T) {
	f := newReads(t)
	for _, c := range []struct{ n, want int }{{99, 99}, {100, 100}, {101, 100}, {250, 100}} {
		room := fmt.Sprintf("cap%d", c.n)
		f.room(room)
		a := f.seat(room, "Alice", 0)
		f.lines(room, KindText, 0, c.n, "line")
		if got := f.unread(a.ProfileID)[room].Count; got != c.want {
			t.Errorf("%d lines: %d unread, want %d", c.n, got, c.want)
		}
		if _, err := f.s.db.ExecContext(t.Context(), `DELETE FROM room_reads WHERE room_id = ?`, room); err != nil {
			t.Fatal(err)
		}
		if got, ok := f.unread(a.ProfileID)[room]; ok {
			t.Errorf("%d lines, no marker: %+v, want nothing", c.n, got)
		}
	}
}

// 041-AC11: an "@" line among the newest 100 unread ones comes back for the @name rule; one past
// them does not.
func TestUnreadMentionBodies(t *testing.T) {
	f := newReads(t)
	f.room("r")
	a := f.seat("r", "Alice", 0)
	f.lines("r", KindText, 0, 1, "@Alice early")
	f.lines("r", KindText, 0, 100, "plain")
	if got := f.unread(a.ProfileID)["r"]; got.Count != 100 || len(got.Bodies) != 0 {
		t.Fatalf("mention beyond 100: %+v", got)
	}
	f.lines("r", KindAgent, 0, 1, "@Alice late")
	if got := f.unread(a.ProfileID)["r"]; got.Count != 100 || len(got.Bodies) != 1 || got.Bodies[0] != "@Alice late" {
		t.Fatalf("mention within 100: %+v", got)
	}
}

// 041-AC9: the marker is clamped to the room's newest line, refuses a line of another room, keeps
// the greatest of reordered and concurrent writes, and is inserted when missing.
func TestMarkRead(t *testing.T) {
	f := newReads(t)
	ctx := t.Context()
	f.room("r")
	f.room("other")
	a := f.seat("r", "Alice", 0)
	f.seat("other", "Bob", 0)
	foreign := f.lines("other", KindText, 0, 1, "elsewhere")
	top := f.lines("r", KindText, 0, 50, "line")
	if err := f.s.MarkRead(ctx, a.ProfileID, "r", top+1000); err != nil || f.marker(a.ProfileID, "r") != top {
		t.Fatalf("clamp: %d %v, want %d", f.marker(a.ProfileID, "r"), err, top)
	}
	if _, err := f.s.db.ExecContext(ctx, `UPDATE room_reads SET read_through = 0`); err != nil {
		t.Fatal(err)
	}
	if err := f.s.MarkRead(ctx, a.ProfileID, "r", foreign); !errors.Is(err, ErrForeignLine) || f.marker(a.ProfileID, "r") != 0 {
		t.Fatalf("foreign line: %v, marker %d", err, f.marker(a.ProfileID, "r"))
	}
	for _, through := range []int64{top - 10, top - 20} {
		if err := f.s.MarkRead(ctx, a.ProfileID, "r", through); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.marker(a.ProfileID, "r"); got != top-10 {
		t.Fatalf("a lower write after a higher one: %d, want %d", got, top-10)
	}
	if got := f.unread(a.ProfileID)["r"].Count; got != 10 {
		t.Fatalf("%d unread past the marker, want 10", got)
	}
	if _, err := f.s.db.ExecContext(ctx, `DELETE FROM room_reads`); err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 40)
	for i := range ids {
		ids[i] = top - 39 + int64(i)
	}
	rand.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
	var wg sync.WaitGroup
	errs := make(chan error, len(ids))
	for _, id := range ids {
		wg.Go(func() { errs <- f.s.MarkRead(ctx, a.ProfileID, "r", id) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := f.marker(a.ProfileID, "r"); got != top {
		t.Fatalf("concurrent writes from a missing row: %d, want %d", got, top)
	}
	// A profile merged away or a room deleted mid-request leaves nothing to keep, and no error.
	for _, c := range []struct {
		profile int64
		room    string
	}{{a.ProfileID + 1000, "r"}, {a.ProfileID, "gone"}} {
		if err := f.s.MarkRead(ctx, c.profile, c.room, top+1000); err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		if n := count(t, f.s, `SELECT count(*) FROM room_reads WHERE profile_id = ? AND room_id = ?`, c.profile, c.room); n != 0 {
			t.Fatalf("%+v: %d rows", c, n)
		}
	}
}

// Every table written outside the write lock is named in the architecture notes, room_reads
// included (041-AC9).
func TestUnlockedWritesAreDocumented(t *testing.T) {
	doc, err := os.ReadFile("../../docs/architecture.md")
	if err != nil {
		t.Fatal(err)
	}
	for table := range unlockedWrites {
		if !regexp.MustCompile("`" + table + "[`(]").Match(doc) {
			t.Errorf("docs/architecture.md does not name %s, written outside the write lock", table)
		}
	}
}

// 041-AC7: a merge leaves one marker per room, the greater of the two, so the lines either side
// read stay read and the absorbed profile's unread lines stay unread.
func TestMergeKeepsReads(t *testing.T) {
	f := newReads(t)
	ctx := t.Context()
	for _, r := range []string{"both", "bonly", "hi", "lo"} {
		f.room(r)
	}
	a := f.seat("both", "Alice", 0)
	b := f.seat("both", "Alice phone", 0)
	A, B := a.ProfileID, b.ProfileID
	f.seat("bonly", "Alice phone", B)
	f.seat("hi", "Alice", A)
	f.seat("hi", "Alice phone", B)
	f.seat("lo", "Alice", A)
	f.seat("lo", "Alice phone", B)
	carol := f.seat("bonly", "Carol", 0)
	f.lines("bonly", KindText, carol.ID, 3, "news")
	for _, r := range []string{"both", "hi", "lo"} {
		f.lines(r, KindText, 0, 10, "news")
	}
	hi, lo := f.newest("hi"), f.newest("lo")
	for _, m := range []struct {
		profile int64
		room    string
		through int64
	}{{A, "hi", hi - 2}, {B, "hi", hi - 6}, {A, "lo", lo - 6}, {B, "lo", lo - 2}} {
		if err := f.s.MarkRead(ctx, m.profile, m.room, m.through); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.unread(B)["bonly"].Count; got != 3 {
		t.Fatalf("before the merge: %d unread, want 3", got)
	}
	if _, _, _, err := f.s.MergeProfiles(mergeAudit(time.Now(), A), A, B, ""); err != nil {
		t.Fatal(err)
	}
	if n := count(t, f.s, `SELECT count(*) FROM room_reads WHERE profile_id = ?`, B); n != 0 {
		t.Fatalf("%d markers left on the absorbed profile", n)
	}
	got := f.unread(A)
	for room, want := range map[string]int{"bonly": 3, "hi": 2, "lo": 2, "both": 10} {
		if got[room].Count != want {
			t.Errorf("%s after the merge: %d unread, want %d", room, got[room].Count, want)
		}
	}
	if f.marker(A, "hi") != hi-2 || f.marker(A, "lo") != lo-2 {
		t.Fatalf("markers %d %d, want the greater %d %d", f.marker(A, "hi"), f.marker(A, "lo"), hi-2, lo-2)
	}
	// Two seats of one profile now share one marker: reading through either clears it.
	if err := f.s.MarkRead(ctx, A, "both", f.newest("both")); err != nil {
		t.Fatal(err)
	}
	if n := count(t, f.s, `SELECT count(*) FROM room_reads WHERE room_id = 'both'`); n != 1 || f.unread(A)["both"].Count != 0 {
		t.Fatalf("%d markers in both, %d unread", n, f.unread(A)["both"].Count)
	}
}

// 041-AC13: a join starts the marker at the room's newest line, 0 for a room's first seat; a
// rejoin keeps the marker; an import starts its seats' markers once its lines are in.
func TestJoinStartsTheMarker(t *testing.T) {
	f := newReads(t)
	ctx := t.Context()
	f.room("r")
	a := f.seat("r", "Alice", 0)
	if got := f.marker(a.ProfileID, "r"); got != 0 {
		t.Fatalf("first seat: %d, want 0", got)
	}
	f.lines("r", KindText, a.ID, 5, "line")
	newest := f.newest("r")
	b := f.seat("r", "Bob", 0)
	if got := f.marker(b.ProfileID, "r"); got != newest {
		t.Fatalf("a later seat: %d, want %d", got, newest)
	}
	if _, err := f.s.EndSeats(ctx, "r", b.ID, Ending{By: b}); err != nil {
		t.Fatal(err)
	}
	f.lines("r", KindText, a.ID, 5, "more")
	if got, ok := f.unread(b.ProfileID)["r"]; ok {
		t.Fatalf("a room Bob left: %+v unread", got)
	}
	f.seat("r", "Bob again", b.ProfileID)
	if got := f.marker(b.ProfileID, "r"); got != newest {
		t.Fatalf("rejoin: %d, want the old %d", got, newest)
	}
	if got := f.unread(b.ProfileID)["r"].Count; got != 5 {
		t.Fatalf("rejoin: %d unread, want the 5 lines since", got)
	}

	carol := f.seat("r", "Carol", 0)
	if _, err := f.s.EndSeats(ctx, "r", carol.ID, Ending{By: carol}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ImportRoom(ctx, export(t, f.s, "r"), importOpts("copy")); err != nil {
		t.Fatal(err)
	}
	if n := count(t, f.s, `SELECT count(*) FROM room_reads WHERE room_id = 'copy'`); n != 2 {
		t.Fatalf("%d markers in the import, want Alice's and Bob's", n)
	}
	if n := count(t, f.s, `SELECT count(*) FROM participants WHERE room_id = 'copy' AND profile_id = ? AND removed_at IS NOT NULL`, carol.ProfileID); n != 1 {
		t.Fatalf("%d removed seats of Carol in the import, want 1", n)
	}
	if got := f.marker(a.ProfileID, "copy"); got != f.newest("copy") || got == 0 {
		t.Fatalf("import marker %d, want its newest line %d", got, f.newest("copy"))
	}
	if got, ok := f.unread(b.ProfileID)["copy"]; ok {
		t.Fatalf("imported lines unread: %+v", got)
	}
}

// 041-AC13: migration 66 on a database at 65 starts every live seat's profile at its room's newest
// line, nothing unread; a removed seat and a seat with no profile get none.
func TestMigrateRoomReads(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	dsn := testDSN(t)
	f := &readsFixture{t: t, s: openAt(t, dsn)}
	f.room("r")
	f.room("s")
	a := f.seat("r", "Alice", 0)
	f.seat("s", "Alice", a.ProfileID)
	gone := f.seat("r", "Gone", 0)
	bare := f.seat("s", "Bare", 0)
	f.lines("r", KindText, gone.ID, 3, "line")
	f.lines("s", KindAgent, 0, 3, "line")
	if _, err := f.s.EndSeats(ctx, "r", gone.ID, Ending{By: gone}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.ExecContext(ctx, `UPDATE participants SET profile_id = NULL WHERE id = ?`, bare.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.ExecContext(ctx, `DROP TABLE room_reads; DROP INDEX messages_room_written; UPDATE schema_version SET version = 65`); err != nil {
		t.Fatal(err)
	}
	_ = f.s.Close()
	f.s = openAt(t, dsn)
	if n := count(t, f.s, `SELECT count(*) FROM room_reads`); n != 2 {
		t.Fatalf("%d markers after the migration, want Alice's two", n)
	}
	for _, r := range []string{"r", "s"} {
		if got := f.marker(a.ProfileID, r); got != f.newest(r) {
			t.Fatalf("%s: marker %d, want %d", r, got, f.newest(r))
		}
	}
	if u := f.unread(a.ProfileID); len(u) != 0 {
		t.Fatalf("unread after the upgrade: %+v", u)
	}
	if n := count(t, f.s, `SELECT count(*) FROM pg_indexes WHERE indexname = 'messages_room_written'`); n != 1 {
		t.Fatal("no written lines' index after the migration")
	}
}

// scannedMessages sums, over the plan nodes that read messages, the rows each visits: those it
// returns and those its filter removed, times its loops (PostgreSQL averages them per loop).
func scannedMessages(plan map[string]any) float64 {
	var n float64
	rel, _ := plan["Relation Name"].(string)
	idx, _ := plan["Index Name"].(string)
	if rel == "messages" || strings.HasPrefix(idx, "messages") {
		num := func(k string) float64 { v, _ := plan[k].(float64); return v }
		n += (num("Actual Rows") + num("Rows Removed by Filter") + num("Rows Removed by Index Recheck")) * num("Actual Loops")
	}
	kids, _ := plan["Plans"].([]any)
	for _, k := range kids {
		if m, ok := k.(map[string]any); ok {
			n += scannedMessages(m)
		}
	}
	return n
}

// 041-AC14: with 1,000 voice lines newest after 1,000 written ones in each of 50 rooms, the count
// visits at most 100 lines per room; without the written lines' index it visits far more.
func TestUnreadQueryBound(t *testing.T) {
	f := newReads(t)
	ctx := t.Context()
	const rooms = 50
	var profile int64
	for i := range rooms {
		room := fmt.Sprintf("b%02d", i)
		f.room(room)
		profile = f.seat(room, "Alice", profile).ProfileID
	}
	if _, err := f.s.db.ExecContext(ctx, `INSERT INTO messages(room_id, ts, kind, sender, body)
		SELECT r.id, '2026-01-01T00:00:00Z', CASE WHEN g <= 1000 THEN 'text' ELSE 'voice' END, 'Bob', 'line'
		FROM rooms r CROSS JOIN generate_series(1, 2000) g WHERE r.id LIKE 'b%' ORDER BY g, r.id;
		ANALYZE messages`); err != nil {
		t.Fatal(err)
	}
	u := f.unread(profile)
	if len(u) != rooms {
		t.Fatalf("%d rooms unread, want %d", len(u), rooms)
	}
	for room, r := range u {
		if r.Count != 100 {
			t.Fatalf("%s: %d unread, want 100", room, r.Count)
		}
	}
	visited := func() float64 {
		t.Helper()
		var out string
		if err := f.s.db.QueryRowContext(ctx, `EXPLAIN (ANALYZE, FORMAT JSON) `+unreadSQL, profile).Scan(&out); err != nil {
			t.Fatal(err)
		}
		var plans []struct {
			Plan map[string]any `json:"Plan"`
		}
		if err := json.Unmarshal([]byte(out), &plans); err != nil || len(plans) != 1 {
			t.Fatalf("plan %s: %v", out, err)
		}
		return scannedMessages(plans[0].Plan)
	}
	n := visited()
	t.Logf("visited with the index: %.0f", n)
	if n > 100*rooms {
		t.Fatalf("the count visits %.0f lines, over 100 per room", n)
	}
	// The guard holds only with the index: without it the voice lines are visited too.
	if _, err := f.s.db.ExecContext(ctx, `DROP INDEX messages_room_written`); err != nil {
		t.Fatal(err)
	}
	n = visited()
	t.Logf("visited without the index: %.0f", n)
	if n <= 100*rooms {
		t.Fatalf("without the index the count visits %.0f lines: the bound no longer tells", n)
	}
}
