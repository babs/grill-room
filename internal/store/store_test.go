package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/anchor"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seed creates a room with its Grill persona, an invite, a participant with its own profile, an
// edited text message, an upload, an agent document with two revisions, an annotation with a
// message carrying an image, a paired daemon hosting, a queued turn, and a done turn with its
// session.
func seed(t *testing.T, s *Store, room string) *Participant {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateRoom(ctx, room, "Room "+room); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInvite(ctx, room, "inv-"+room, 0); err != nil {
		t.Fatal(err)
	}
	p, _, err := s.Join(ctx, "inv-"+room, "Alice", "tok-"+room, JoinProfile{RecoveryHash: "rec-" + room, SessionHash: "sess-" + room})
	if err != nil {
		t.Fatal(err)
	}
	hello := &Message{RoomID: room, Kind: KindText, ParticipantID: p.ID, Sender: p.Name, Body: "hello"}
	if err := s.InsertMessage(ctx, hello); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EditMessage(ctx, room, hello.ID, p.ID, "hello again"); err != nil {
		t.Fatal(err)
	}
	up := &Upload{RoomID: room, ParticipantID: p.ID, Author: p.Name, Filename: "plan.md", ContentType: "text/markdown", Format: "md", Data: []byte("# Plan"), Text: "# Plan"}
	if _, err := s.AddUpload(ctx, up, 1<<20); err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateAgentDocument(ctx, room, "synthesis.md", "md", "v1", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddRevision(ctx, room, d.ID, "v2", "agent"); err != nil {
		t.Fatal(err)
	}
	msg := &NewMessage{ParticipantID: p.ID, Author: p.Name, Body: "see", Attachments: []Attachment{{ContentType: "image/png", Data: []byte("png")}}}
	if _, err := s.CreateAnnotation(ctx, room, d.ID, 2, &anchor.Anchor{Kind: anchor.KindText, Exact: "v2"}, msg, 1<<20); err != nil {
		t.Fatal(err)
	}
	dm, err := s.PairDaemon(ctx, room, p.ID, "dmn-"+room)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetHost(ctx, room, p.ID); err != nil {
		t.Fatal(err)
	}
	ps, err := s.RoomPersonas(ctx, room)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue(ctx, &QueuedTurn{RoomID: room, PersonaID: ps[0].ID, ParticipantID: p.ID}); err != nil {
		t.Fatal(err)
	}
	turn := &Turn{RoomID: room, ParticipantID: p.ID, DaemonID: dm.ID, Prompt: "go", PersonaID: ps[0].ID, PersonaName: ps[0].Name}
	if err := s.StartTurn(ctx, turn, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteTurn(ctx, room, turn.ID, dm.ID, &TurnOutcome{SessionID: "sid-" + room, Session: []byte("{}\n")}); err != nil {
		t.Fatal(err)
	}
	return p
}

// seedHeld has the seeded room's host approve everything, and a second member add a persona that
// waits for the host, with a held change request (002-AC21).
func seedHeld(t *testing.T, s *Store, room string) *Participant {
	t.Helper()
	ctx := context.Background()
	host, err := s.HostDaemon(ctx, room)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDaemonSettings(ctx, host.ID, DaemonSettings{ApproveAll: true}); err != nil {
		t.Fatal(err)
	}
	bob, _, err := s.Join(ctx, "inv-"+room, "Bob", "tok-bob-"+room, JoinProfile{})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := s.AddPersona(ctx, room, templateByPreset(t, s, "secretary").ID, bob)
	if err != nil || !p.Pending {
		t.Fatalf("held persona: %+v %v", p, err)
	}
	return bob
}

func TestMigrateIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	for range 2 {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var v int
		if err := s.db.QueryRowContext(context.Background(), `PRAGMA user_version`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v != len(migrations) {
			t.Fatalf("user_version = %d, want %d", v, len(migrations))
		}
		_ = s.Close()
	}
}

// serverWide are the tables the server itself owns: settings, the persona template and starter
// libraries, whose presets and built-ins belong to no profile while the rest cascade from theirs, the skill library,
// whose shared skills outlive their publisher while personal ones cascade from theirs, the admin
// sessions, and the audit log, whose entries outlive the rooms and profiles they name (004-AC6).
var serverWide = []string{"settings", "persona_templates", "persona_template_versions", "skills", "skill_versions", "admin_sessions", "audit_log",
	"starters", "starter_versions", "starter_files"}

// uncascadedTables returns the tables that neither a DELETE on rooms nor one on profiles would
// purge: those without a chain of NOT NULL, ON DELETE CASCADE foreign keys leading to an owner.
// serverWide tables are skipped.
func uncascadedTables(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT m.name, COALESCE(f."table", ''), COALESCE(f.on_delete, ''), COALESCE(c."notnull", 0)
		FROM sqlite_master m LEFT JOIN pragma_foreign_key_list(m.name) f
		LEFT JOIN pragma_table_info(m.name) c ON c.name = f."from"
		WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%' AND m.name NOT IN ('rooms', 'profiles')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cascadesTo := map[string][]string{}
	for rows.Next() {
		var table, parent, onDelete string
		var notNull bool
		if err := rows.Scan(&table, &parent, &onDelete, &notNull); err != nil {
			return nil, err
		}
		if slices.Contains(serverWide, table) {
			continue
		}
		if _, ok := cascadesTo[table]; !ok {
			cascadesTo[table] = nil
		}
		// A nullable foreign key cascades nothing for the rows where it is NULL.
		if notNull && strings.EqualFold(onDelete, "CASCADE") {
			cascadesTo[table] = append(cascadesTo[table], parent)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	purged := map[string]bool{"rooms": true, "profiles": true}
	for changed := true; changed; {
		changed = false
		for table, parents := range cascadesTo {
			if !purged[table] && slices.ContainsFunc(parents, func(p string) bool { return purged[p] }) {
				purged[table], changed = true, true
			}
		}
	}
	var out []string
	for table := range cascadesTo {
		if !purged[table] {
			out = append(out, table)
		}
	}
	slices.Sort(out)
	return out, nil
}

// 001-AC4: later phases add tables; each must be purged by DeleteRoom or DeleteProfile.
func TestEveryTableCascadesFromAnOwner(t *testing.T) {
	s := openTest(t)
	bad, err := uncascadedTables(context.Background(), s.db)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Fatalf("tables purged by neither DeleteRoom nor DeleteProfile (need an ON DELETE CASCADE chain to rooms or profiles): %v", bad)
	}
}

// profileRefViolations returns table.column for every foreign key to profiles that a profile
// delete would neither purge (NOT NULL, CASCADE) nor detach (SET NULL).
func profileRefViolations(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT m.name, f."from", f.on_delete, COALESCE(c."notnull", 0)
		FROM sqlite_master m JOIN pragma_foreign_key_list(m.name) f
		LEFT JOIN pragma_table_info(m.name) c ON c.name = f."from"
		WHERE m.type = 'table' AND f."table" = 'profiles'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var table, col, onDelete string
		var notNull bool
		if err := rows.Scan(&table, &col, &onDelete, &notNull); err != nil {
			return nil, err
		}
		// A server-wide table's NULL rows are the server's; the others go with their profile.
		owned := (notNull || slices.Contains(serverWide, table)) && strings.EqualFold(onDelete, "CASCADE")
		detached := !notNull && strings.EqualFold(onDelete, "SET NULL")
		if !owned && !detached {
			out = append(out, table+"."+col)
		}
	}
	slices.Sort(out)
	return out, rows.Err()
}

// 002-AC1: a profile delete purges what the profile owns and only detaches the rest.
func TestEveryProfileReferenceCascadesOrDetaches(t *testing.T) {
	s := openTest(t)
	bad, err := profileRefViolations(context.Background(), s.db)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Fatalf("references to profiles that DeleteProfile would block or leave dangling: %v", bad)
	}
	for name, tc := range map[string]struct {
		ddl  string
		want []string
	}{
		"no action":         {`CREATE TABLE notes (id INTEGER PRIMARY KEY, profile_id INTEGER REFERENCES profiles(id))`, []string{"notes.profile_id"}},
		"restrict":          {`CREATE TABLE notes (id INTEGER PRIMARY KEY, profile_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE RESTRICT)`, []string{"notes.profile_id"}},
		"nullable cascade":  {`CREATE TABLE notes (id INTEGER PRIMARY KEY, profile_id INTEGER REFERENCES profiles(id) ON DELETE CASCADE)`, []string{"notes.profile_id"}},
		"not null set null": {`CREATE TABLE notes (id INTEGER PRIMARY KEY, profile_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE SET NULL)`, []string{"notes.profile_id"}},
		"owned ok":          {`CREATE TABLE notes (id INTEGER PRIMARY KEY, profile_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE)`, nil},
	} {
		t.Run(name, func(t *testing.T) {
			s := openTest(t)
			if _, err := s.db.ExecContext(context.Background(), tc.ddl); err != nil {
				t.Fatal(err)
			}
			got, err := profileRefViolations(context.Background(), s.db)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// The checker itself must flag every shape of table a room delete would leave behind.
func TestUncascadedTablesDetectsBadShapes(t *testing.T) {
	cases := map[string]struct {
		ddl  string
		want []string
	}{
		"no foreign key":       {`CREATE TABLE docs (id INTEGER PRIMARY KEY, room_id TEXT)`, []string{"docs"}},
		"set null":             {`CREATE TABLE docs (id INTEGER PRIMARY KEY, room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE SET NULL)`, []string{"docs"}},
		"default no action":    {`CREATE TABLE docs (id INTEGER PRIMARY KEY, room_id TEXT NOT NULL REFERENCES rooms(id))`, []string{"docs"}},
		"chain through orphan": {`CREATE TABLE docs (id INTEGER PRIMARY KEY); CREATE TABLE revs (id INTEGER PRIMARY KEY, doc_id INTEGER NOT NULL REFERENCES docs(id) ON DELETE CASCADE)`, []string{"docs", "revs"}},
		"nullable cascade":     {`CREATE TABLE docs (id INTEGER PRIMARY KEY, room_id TEXT REFERENCES rooms(id) ON DELETE CASCADE)`, []string{"docs"}},
		"cascade chain ok":     {`CREATE TABLE docs (id INTEGER PRIMARY KEY, room_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE CASCADE); CREATE TABLE revs (id INTEGER PRIMARY KEY, doc_id INTEGER NOT NULL REFERENCES docs(id) ON DELETE CASCADE)`, nil},
		"profile owned ok":     {`CREATE TABLE keys (id INTEGER PRIMARY KEY, profile_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE)`, nil},
		"profile set null":     {`CREATE TABLE keys (id INTEGER PRIMARY KEY, profile_id INTEGER REFERENCES profiles(id) ON DELETE SET NULL)`, []string{"keys"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := openTest(t)
			if _, err := s.db.ExecContext(context.Background(), tc.ddl); err != nil {
				t.Fatal(err)
			}
			got, err := uncascadedTables(context.Background(), s.db)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// roomRows counts the rows referencing room in every table with a room_id column.
func roomRows(t *testing.T, s *Store, room string) map[string]int {
	t.Helper()
	ctx := context.Background()
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.name FROM sqlite_master m JOIN pragma_table_info(m.name) c
		WHERE m.type = 'table' AND c.name = 'room_id'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	_ = rows.Close()
	tables = append(tables, "rooms")
	out := map[string]int{}
	for _, tb := range tables {
		col := "room_id"
		if tb == "rooms" {
			col = "id"
		}
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM "`+tb+`" WHERE `+col+` = ?`, room).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[tb] = n
	}
	return out
}

// 001-AC4 002-AC9 002-AC24
func TestDeleteRoomPurgesEverything(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	roomSkill := map[string]int64{}
	for _, room := range []string{"a", "b"} {
		p := seed(t, s, room)
		seedHeld(t, s, room)
		// 014-AC8: a website a turn was refused.
		if _, err := s.RecordBlocked(ctx, room, "Grill", []string{"example.com"}); err != nil {
			t.Fatal(err)
		}
		// A turn an agent queued: no participant holds it, only the room (002-AC24).
		sec, _, err := s.AddPersona(ctx, room, templateByPreset(t, s, "secretary").ID, p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.EnqueueLink(ctx, &QueuedTurn{RoomID: room, PersonaID: sec.ID}, LinkCall); err != nil {
			t.Fatal(err)
		}
		// 003-AC21: a turn pinned to its base revisions, whose result conflicted, waits to run again.
		dm, err := s.HostDaemon(ctx, room)
		if err != nil {
			t.Fatal(err)
		}
		grill, err := s.RoomPersonas(ctx, room)
		if err != nil {
			t.Fatal(err)
		}
		rejected := &Turn{RoomID: room, ParticipantID: p.ID, DaemonID: dm.ID, Prompt: "go", PersonaID: grill[0].ID, PersonaName: grill[0].Name}
		if err := s.StartTurn(ctx, rejected, false); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PinBaseRevisions(ctx, room, rejected.ID); err != nil {
			t.Fatal(err)
		}
		if _, rerun, err := s.RejectTurn(ctx, room, rejected.ID, dm.ID, "synthesis.md"); err != nil || !rerun {
			t.Fatalf("reject: %v %v", rerun, err)
		}
		// 002-AC31: a persona's active skills go with the room; the skills stay in the library.
		k := publish(t, s, p.ProfileID, true, "shared-"+room, "v1")
		if _, _, _, err := s.SetPersonaSkill(ctx, room, sec.ID, k.ID, true, true, p); err != nil {
			t.Fatal(err)
		}
		// 004-AC8 004-AC10: room admins and the room's own skills, versions and pins go with it.
		if _, err := s.db.ExecContext(ctx, `INSERT INTO room_admins(room_id, profile_id, created_at) VALUES(?,?,?)`, room, p.ProfileID, now()); err != nil {
			t.Fatal(err)
		}
		own := publish(t, s, p.ProfileID, false, "own-"+room, "v1")
		rk, _, err := s.CopySkillToRoom(ctx, room, own.ID, p)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := s.SetPersonaSkill(ctx, room, sec.ID, rk.ID, true, true, p); err != nil {
			t.Fatal(err)
		}
		roomSkill[room] = rk.ID
		// 009-AC12: the room's plugins, their versions and activations go with it.
		if _, err := s.AddPlugins(ctx, room, []NewPlugin{newPlugin("plug-"+room, "c1")}, []int64{sec.ID}, p, 1<<20); err != nil {
			t.Fatal(err)
		}
		// 009-AC19: and its requests to the AI host.
		if _, _, err := s.AddPluginFetch(ctx, room, NewPluginFetch{URL: "git@example.com:o/r.git"}, p); err != nil {
			t.Fatal(err)
		}
	}
	// These reach rooms through another table, so roomRows cannot see them.
	indirect := []string{"revisions", "annotation_messages", "attachments", "message_versions", "room_persona_skills", "plugin_versions", "plugin_snapshots", "room_persona_plugins"}
	count := func(table string) int {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM "`+table+`"`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	perRoom := map[string]int{}
	for _, tb := range indirect {
		if perRoom[tb] = count(tb) / 2; perRoom[tb] == 0 {
			t.Fatalf("seed wrote no %s", tb)
		}
	}
	if err := s.DeleteRoom(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	for _, tb := range indirect {
		if n := count(tb); n != perRoom[tb] {
			t.Errorf("%s: %d left, want the other room's %d", tb, n, perRoom[tb])
		}
	}
	for table, n := range roomRows(t, s, "a") {
		if n != 0 {
			t.Errorf("%s still holds %d rows of the deleted room", table, n)
		}
	}
	for table, n := range roomRows(t, s, "b") {
		if n == 0 {
			t.Errorf("%s lost the other room's rows", table)
		}
	}
	if err := s.DeleteRoom(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: got %v, want ErrNotFound", err)
	}
	for room, want := range map[string]int{"a": 0, "b": 1} {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM skill_versions WHERE skill_id = ?`, roomSkill[room]).Scan(&n); err != nil || n != want {
			t.Errorf("room %s skill versions: %d, want %d", room, n, want)
		}
	}
	// The personal skills copied from stay with their profile.
	var own int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM skills WHERE name = 'own-a' AND scope = 'personal'`).Scan(&own); err != nil || own != 1 {
		t.Errorf("the personal source of a room skill went with the room")
	}
	// A profile outlives its rooms: it only stops listing them.
	if prof, err := s.ProfileBySession(ctx, "sess-a"); err != nil || len(prof.Rooms) != 0 {
		t.Fatalf("profile after its room went: %+v %v", prof, err)
	}
}

// 001-AC3
func TestRevokedInviteInvalidatesItsParticipants(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p := seed(t, s, "a")
	inv2, err := s.CreateInvite(ctx, "a", "inv2", 0)
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := s.Join(ctx, "inv2", "Bob", "tok-bob", JoinProfile{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeInvite(ctx, "a", p.InviteID, true, "Admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeInvite(ctx, "a", p.InviteID, true, "Admin"); err != nil {
		t.Fatalf("revoke is idempotent, got %v", err)
	}
	if _, err := s.ParticipantByToken(ctx, "tok-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked participant: got %v, want ErrNotFound", err)
	}
	if _, _, err := s.Join(ctx, "inv-a", "Carol", "tok-carol", JoinProfile{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("join with revoked invite: got %v, want ErrNotFound", err)
	}
	if got, err := s.ParticipantByToken(ctx, "tok-bob"); err != nil || got.ID != other.ID {
		t.Fatalf("other invite's participant: got %v, %v", got, err)
	}
	ps, err := s.Participants(ctx, "a")
	if err != nil || len(ps) != 1 || ps[0].ID != other.ID {
		t.Fatalf("participants after revoke: %v, %v", ps, err)
	}
	if _, err := s.RevokeInvite(ctx, "b", inv2.ID, true, "Admin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke through the wrong room: got %v, want ErrNotFound", err)
	}
}

func TestCreateInviteUnknownRoom(t *testing.T) {
	s := openTest(t)
	if _, err := s.CreateInvite(context.Background(), "nope", "h", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestJoinUnknownInvite(t *testing.T) {
	s := openTest(t)
	if _, _, err := s.Join(context.Background(), "nope", "Alice", "t", JoinProfile{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestMessagesKindAndPaging(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p := seed(t, s, "a") // a system "joined" line then a text line
	if err := s.InsertMessage(ctx, &Message{RoomID: "a", Kind: "shout", Sender: p.Name, Body: "x"}); err == nil {
		t.Fatal("unknown kind accepted")
	}
	for _, m := range []*Message{{Kind: KindVoice, Source: VoiceSourceServer}, {Kind: KindAgent}} {
		k := m.Kind
		m.RoomID, m.Sender, m.Body = "a", "x", k
		if err := s.InsertMessage(ctx, m); err != nil {
			t.Fatalf("kind %s: %v", k, err)
		}
	}
	all, err := s.Messages(ctx, "a", 0, 100)
	if err != nil || len(all) != 4 {
		t.Fatalf("messages: %d, %v", len(all), err)
	}
	if all[0].Kind != KindSystem || all[0].Body != "Alice joined" || all[1].ParticipantID != p.ID || all[3].ParticipantID != 0 {
		t.Fatalf("unexpected messages: %+v", all)
	}
	page, err := s.Messages(ctx, "a", all[1].ID, 1)
	if err != nil || len(page) != 1 || page[0].ID != all[2].ID {
		t.Fatalf("page after %d: %+v, %v", all[1].ID, page, err)
	}
}

// 001-AC5
func TestWritable(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.Writable(ctx); err != nil {
		t.Fatalf("fresh store: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA query_only = 1`); err != nil {
		t.Fatal(err)
	}
	if err := s.Writable(ctx); err == nil {
		t.Fatal("read-only store reported writable")
	}
}

func TestRoomsAndSettings(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seed(t, s, "a")
	rooms, err := s.Rooms(ctx)
	if err != nil || len(rooms) != 1 || rooms[0].ID != "a" || rooms[0].Chain.Budget != DefaultChainBudget {
		t.Fatalf("rooms: %v, %v", rooms, err)
	}
	if _, err := s.Room(ctx, "zz"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing room: %v", err)
	}
	if v, err := s.Setting(ctx, "k"); err != nil || v != "" {
		t.Fatalf("missing setting: %q, %v", v, err)
	}
	if err := s.SetSetting(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Setting(ctx, "k"); v != "v" {
		t.Fatalf("setting = %q", v)
	}
}

func TestRoomParticipant(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p := seed(t, s, "a")
	seed(t, s, "b")
	got, err := s.RoomParticipant(ctx, "a", p.ID)
	if err != nil || got.Name != "Alice" || got.RoomID != "a" {
		t.Fatalf("participant: %+v %v", got, err)
	}
	if _, err := s.RoomParticipant(ctx, "b", p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("through another room: %v", err)
	}
	if _, err := s.RevokeInvite(ctx, "a", p.InviteID, true, "Admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RoomParticipant(ctx, "a", p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed: %v", err)
	}
}

// serverWideRoomRefs returns table.column for every reference from a server-wide table to rooms
// that a room delete would not cascade.
func serverWideRoomRefs(ctx context.Context, db *sql.DB, tables []string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT m.name, f."from", f.on_delete FROM sqlite_master m JOIN pragma_foreign_key_list(m.name) f
		WHERE m.type = 'table' AND f."table" = 'rooms'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var table, col, onDelete string
		if err := rows.Scan(&table, &col, &onDelete); err != nil {
			return nil, err
		}
		if slices.Contains(tables, table) && !strings.EqualFold(onDelete, "CASCADE") {
			out = append(out, table+"."+col)
		}
	}
	return out, rows.Err()
}

// 004-AC10: a server-wide table holding rows of a room, like the room skills, cascades them with it.
func TestServerWideRoomReferencesCascade(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if bad, err := serverWideRoomRefs(ctx, s.db, serverWide); err != nil || len(bad) > 0 {
		t.Fatalf("server-wide references a room delete leaves behind: %v %v", bad, err)
	}
	for _, ddl := range []string{
		`CREATE TABLE notes (id INTEGER PRIMARY KEY, room_id TEXT REFERENCES rooms(id))`,
		`CREATE TABLE notes (id INTEGER PRIMARY KEY, room_id TEXT REFERENCES rooms(id) ON DELETE SET NULL)`,
	} {
		s := openTest(t)
		if _, err := s.db.ExecContext(ctx, ddl); err != nil {
			t.Fatal(err)
		}
		if bad, _ := serverWideRoomRefs(ctx, s.db, append(slices.Clone(serverWide), "notes")); !slices.Equal(bad, []string{"notes.room_id"}) {
			t.Errorf("%s: %v", ddl, bad)
		}
	}
}
