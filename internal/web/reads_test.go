package web

import (
	"bytes"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/babs/grill-room/internal/archive"
	"github.com/babs/grill-room/internal/store"
)

// unreadOf returns the unread count and mention flag GET /api/profile gives d for room.
func unreadOf(t *testing.T, d *device, room string) (int, bool) {
	t.Helper()
	p := d.profile(t).Profile
	for _, r := range p.Rooms {
		if r.RoomID == room {
			if r.Unread == nil || r.UnreadMention == nil {
				t.Fatalf("room %s carries no unread fields: %+v", room, r)
			}
			return *r.Unread, *r.UnreadMention
		}
	}
	t.Fatalf("room %s not in the profile", room)
	return 0, false
}

// say writes n lines of body in room as participant pid, straight to the store: no turn starts.
func say(t *testing.T, e *env, room string, pid int64, n int, body string) {
	t.Helper()
	for range n {
		if err := e.st.InsertMessage(t.Context(), &store.Message{RoomID: room, Kind: store.KindText, ParticipantID: pid, Sender: "x", Body: body}); err != nil {
			t.Fatal(err)
		}
	}
}

// 041-AC11 041-AC14: GET /api/profile counts the viewer's unread lines per room, and flags a
// mention of one of its seats among the newest 100 of them, not of a persona or another person.
func TestProfileUnread(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Unread", "Alice")
	bob := e.join(t, e.createInvite(t, room.ID).Token, "Bob")
	personas, err := e.st.RoomPersonas(t.Context(), room.ID)
	if err != nil || len(personas) == 0 {
		t.Fatalf("personas %v %v", personas, err)
	}
	if n, at := unreadOf(t, alice, room.ID); n != 0 || at {
		t.Fatalf("a new room: %d %v", n, at)
	}
	steps := []struct {
		name, body string
		n          int
		want       int
		mention    bool
	}{
		{"a plain line", "hello", 1, 1, false},
		{"another person named", "@Bob ping", 1, 2, false},
		{"a persona named", "@" + personas[0].Name + " help", 1, 3, false},
		{"the viewer named", "@Alice look", 1, 4, true},
		{"the mention beyond 100", "plain", 100, 100, false},
		{"150 mentions", "@alice again", 150, 100, true},
	}
	for _, s := range steps {
		say(t, e, room.ID, bob.Participant.ID, s.n, s.body)
		if n, at := unreadOf(t, alice, room.ID); n != s.want || at != s.mention {
			t.Fatalf("%s: %d %v, want %d %v", s.name, n, at, s.want, s.mention)
		}
	}
	// named() reads only what the store hands it: at most 100 bodies a room.
	u, err := e.st.UnreadLines(t.Context(), alice.profile(t).Profile.ID)
	if err != nil || len(u[room.ID].Bodies) != 100 {
		t.Fatalf("%d bodies for the @name rule, want 100: %v", len(u[room.ID].Bodies), err)
	}
	// Other profile answers carry no unread fields.
	code, b := e.do(t, "GET", "/api/profiles", e.admin, nil)
	if code != http.StatusOK || bytes.Contains(b, []byte(`"unread`)) {
		t.Fatalf("admin profiles: %d %s", code, b)
	}
}

// 041-AC11: when the counts cannot be read, GET /api/profile answers without them and logs it.
func TestProfileUnreadFailure(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	e := newEnvLog(t, testOptions, zap.New(core))
	alice := e.device(t)
	room := alice.createRoom(t, "Broken", "Alice")
	if _, err := e.sqlDB(t).ExecContext(t.Context(), `ALTER TABLE room_reads RENAME TO room_reads_gone`); err != nil {
		t.Fatal(err)
	}
	code, b := alice.do(t, "GET", "/api/profile", nil)
	if code != http.StatusOK || !bytes.Contains(b, []byte(room.ID)) || bytes.Contains(b, []byte(`"unread`)) {
		t.Fatalf("profile without its counts: %d %s", code, b)
	}
	if n := logs.FilterMessage("unread_failed").Len(); n != 1 {
		t.Fatalf("%d unread_failed logs, want 1", n)
	}
}

// 041-AC9: PUT /api/rooms/{room}/read moves the seat's profile marker, refuses a bad body, a line
// of another room, a cross-site write and a removed seat; a seat with no profile writes nothing.
func TestPutRead(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Reads", "Alice")
	other := alice.createRoom(t, "Other", "Alice")
	path := "/api/rooms/" + room.ID + "/read"
	bob := e.join(t, e.createInvite(t, room.ID).Token, "Bob")
	say(t, e, room.ID, bob.Participant.ID, 3, "news")
	say(t, e, other.ID, 0, 1, "elsewhere")
	msgs, err := e.st.Messages(t.Context(), other.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	foreign := msgs[len(msgs)-1].ID
	newest := func(room string) int64 {
		ms, err := e.st.Messages(t.Context(), room, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		return ms[len(ms)-1].ID
	}
	for _, c := range []struct {
		name string
		body any
		hdr  []string
		code int
	}{
		{"not a number", map[string]any{"through": "x"}, nil, http.StatusBadRequest},
		{"zero", map[string]any{"through": 0}, nil, http.StatusBadRequest},
		{"missing", map[string]any{}, nil, http.StatusBadRequest},
		{"negative", map[string]any{"through": -5}, nil, http.StatusBadRequest},
		{"a line of another room", map[string]any{"through": foreign}, nil, http.StatusBadRequest},
		{"cross-site", map[string]any{"through": newest(room.ID)}, []string{"Sec-Fetch-Site", "cross-site"}, http.StatusForbidden},
	} {
		if code, b := alice.do(t, "PUT", path, c.body, c.hdr...); code != c.code {
			t.Fatalf("%s: %d %s, want %d", c.name, code, b, c.code)
		}
	}
	if n, _ := unreadOf(t, alice, room.ID); n != 3 {
		t.Fatalf("refused writes moved the marker: %d unread", n)
	}
	if code, b := alice.do(t, "PUT", path, map[string]any{"through": newest(room.ID)}); code != http.StatusNoContent {
		t.Fatalf("read: %d %s", code, b)
	}
	if n, _ := unreadOf(t, alice, room.ID); n != 0 {
		t.Fatalf("after reading: %d unread", n)
	}

	db := e.sqlDB(t)
	markers := func() (n int) {
		t.Helper()
		if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM room_reads`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	bare := e.join(t, e.createInvite(t, room.ID).Token, "Nora")
	if _, err := db.ExecContext(t.Context(), `UPDATE participants SET profile_id = NULL WHERE id = $1`, bare.Participant.ID); err != nil {
		t.Fatal(err)
	}
	before := markers()
	if code, b := e.do(t, "PUT", path, bare.Token, map[string]any{"through": newest(room.ID)}); code != http.StatusNoContent || markers() != before {
		t.Fatalf("a seat with no profile: %d %s, %d markers, want %d", code, b, markers(), before)
	}
	if code, b := e.do(t, "DELETE", fmt.Sprintf("/api/admin/rooms/%s/participants/%d", room.ID, bob.Participant.ID), e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("remove Bob: %d %s", code, b)
	}
	if code, _ := e.do(t, "PUT", path, bob.Token, map[string]any{"through": newest(room.ID)}); code != http.StatusUnauthorized {
		t.Fatalf("a removed seat: %d", code)
	}
}

// 041-AC9: the marker and the counts stay out of the room answer, the stream and the export, and
// moving the marker sends no event.
func TestReadsStayPrivate(t *testing.T) {
	e := newEnv(t)
	e.srv.opts.StagingDir = t.TempDir()
	alice := e.device(t)
	room := alice.createRoom(t, "Private", "Alice")
	bob := e.join(t, e.createInvite(t, room.ID).Token, "Bob")
	say(t, e, room.ID, bob.Participant.ID, 2, "news")
	leaks := func(where string, b []byte) {
		t.Helper()
		for _, k := range []string{"read_through", "unread", "room_reads"} {
			if bytes.Contains(b, []byte(k)) {
				t.Fatalf("%s holds %q: %s", where, k, b)
			}
		}
	}
	code, b := alice.do(t, "GET", "/api/rooms/"+room.ID, nil)
	if code != http.StatusOK {
		t.Fatalf("room: %d", code)
	}
	leaks("the room answer", b)
	ch, _ := e.openStream(t, "/api/rooms/"+room.ID+"/events", bob.Token, "")
	for _, ev := range collect(ch, 300*time.Millisecond) {
		leaks("the stream", []byte(ev.event+" "+ev.data))
	}
	ms, err := e.st.Messages(t.Context(), room.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if code, b := e.do(t, "PUT", "/api/rooms/"+room.ID+"/read", bob.Token, map[string]any{"through": ms[len(ms)-1].ID}); code != http.StatusNoContent {
		t.Fatalf("read: %d %s", code, b)
	}
	for _, ev := range collect(ch, 300*time.Millisecond) {
		if ev.event != "presence" {
			t.Fatalf("moving the marker sent %+v", ev)
		}
	}
	code, ar := e.do(t, "GET", "/api/admin/rooms/"+room.ID+"/export", e.admin, nil)
	if code != http.StatusOK {
		t.Fatalf("export: %d", code)
	}
	dir := t.TempDir()
	if _, err := archive.Unpack(bytes.NewReader(ar), dir, archive.Limits{MaxBytes: 1 << 20, MaxFiles: 100}); err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err == nil {
			leaks("export "+strings.TrimPrefix(p, dir), b)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
