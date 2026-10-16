package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// 010-AC10: the migration marks every voice line from before 010 as the server's, and leaves the
// other kinds without a source.
func TestMigrationToVoiceSource(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "old.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	ctx := context.Background()
	// The migration before 010's: later ones append after it.
	before := slices.IndexFunc(migrations, func(m string) bool { return strings.Contains(m, "010-AC10") })
	if err := migrateTo(ctx, db, before); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO rooms(id, name, created_at) VALUES('a', 'R', '2026-01-01T00:00:00Z')`,
		`INSERT INTO messages(room_id, ts, kind, sender, body) VALUES('a', '2026-01-01T00:00:00Z', 'voice', 'A', 'v1')`,
		`INSERT INTO messages(room_id, ts, kind, sender, body) VALUES('a', '2026-01-01T00:00:01Z', 'text', 'A', 't1')`,
		`INSERT INTO messages(room_id, ts, kind, sender, body) VALUES('a', '2026-01-01T00:00:02Z', 'voice', 'A', 'v2')`,
		`INSERT INTO messages(room_id, ts, kind, sender, body) VALUES('a', '2026-01-01T00:00:03Z', 'system', 'S', 's1')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateTo(ctx, db, len(migrations)); err != nil {
		t.Fatal(err)
	}
	voice, server, other, null := sourceCounts(t, db)
	if voice != 2 || server != 2 || other != 2 || null != 2 {
		t.Fatalf("voice %d with server %d, other kinds %d with NULL %d", voice, server, other, null)
	}
	if _, err := db.ExecContext(ctx, `UPDATE messages SET source = 'phone' WHERE kind = 'voice'`); err == nil {
		t.Fatal("unknown source accepted")
	}
}

// sourceCounts returns the voice lines, those marked server, the other lines, and those with no source.
func sourceCounts(t *testing.T, db *sql.DB) (voice, server, other, null int) {
	t.Helper()
	if err := db.QueryRowContext(t.Context(), `SELECT
		count(*) FILTER (WHERE kind = 'voice'), count(*) FILTER (WHERE kind = 'voice' AND source = 'server'),
		count(*) FILTER (WHERE kind != 'voice'), count(*) FILTER (WHERE kind != 'voice' AND source IS NULL) FROM messages`).
		Scan(&voice, &server, &other, &null); err != nil {
		t.Fatal(err)
	}
	return voice, server, other, null
}

// 010-AC10: a one-off check of the migration on a copy of a real database, named by
// GRILL_ROOM_SOURCE_MIGRATION_DB; skipped otherwise. It migrates the copy in place.
func TestMigrationToVoiceSourceOnCopy(t *testing.T) {
	path := os.Getenv("GRILL_ROOM_SOURCE_MIGRATION_DB")
	if path == "" {
		t.Skip("GRILL_ROOM_SOURCE_MIGRATION_DB unset")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	var from int
	if err := db.QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&from); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var to int
	var check string
	if err := s.db.QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&to); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(t.Context(), `PRAGMA integrity_check`).Scan(&check); err != nil {
		t.Fatal(err)
	}
	voice, server, other, null := sourceCounts(t, s.db)
	t.Logf("user_version %d -> %d, voice %d (server %d), other %d (NULL %d), integrity %s", from, to, voice, server, other, null, check)
	if to != len(migrations) || to <= from || voice != server || other != null || check != "ok" {
		t.Fatal("migration on the copy did not hold")
	}
}

// 010-AC11: an export carries each voice line's source and an import keeps it; an archive from
// before 010 has voice lines without one, which come in as the server's; an unknown source, or one
// on a line that is not a voice line, is refused.
func TestExportImportVoiceSource(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice, _ := richRoom(t, s, "orig")
	for _, src := range []string{VoiceSourceDevice, VoiceSourceServer} {
		m := &Message{RoomID: "orig", Kind: KindVoice, ParticipantID: alice.ID, Sender: alice.Name, Body: "said on " + src, Source: src}
		if err := s.InsertMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
		if got, err := s.Message(ctx, "orig", m.ID); err != nil || got.Source != src {
			t.Fatalf("stored %+v %v, want source %s", got, err, src)
		}
	}
	// The store refuses a voice line without a source, and a source on any other line.
	for _, m := range []*Message{
		{RoomID: "orig", Kind: KindVoice, ParticipantID: alice.ID, Sender: alice.Name, Body: "no source"},
		{RoomID: "orig", Kind: KindText, ParticipantID: alice.ID, Sender: alice.Name, Body: "typed", Source: VoiceSourceDevice},
		{RoomID: "orig", Kind: KindSystem, Sender: "S", Body: "said", Source: VoiceSourceServer},
	} {
		if err := s.InsertMessage(ctx, m); err == nil || m.ID != 0 {
			t.Fatalf("stored %+v", m)
		}
	}
	dir := export(t, s, "orig")
	raw, err := os.ReadFile(filepath.Join(dir, fileMessages))
	if err != nil {
		t.Fatal(err)
	}
	lines := string(raw)
	if strings.Count(lines, `"source":`) != 2 || !strings.Contains(lines, `"body":"said on device","source":"device"`) ||
		!strings.Contains(lines, `"body":"said on server","source":"server"`) {
		t.Fatalf("export lines:\n%s", lines)
	}
	sources := func(room string) map[string]string {
		t.Helper()
		msgs, err := s.Messages(ctx, room, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, m := range msgs {
			out[m.Body] = m.Kind + "/" + m.Source
		}
		return out
	}
	if _, err := s.ImportRoom(ctx, dir, importOpts("copy")); err != nil {
		t.Fatal(err)
	}
	got := sources("copy")
	if got["said on device"] != "voice/device" || got["said on server"] != "voice/server" || got["hello, third time"] != "text/" {
		t.Fatalf("copy %v", got)
	}

	withLines := func(edit func(string) string) string {
		t.Helper()
		d := t.TempDir()
		if err := os.CopyFS(d, os.DirFS(dir)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, fileMessages), []byte(edit(lines)), 0o600); err != nil {
			t.Fatal(err)
		}
		return d
	}
	old := withLines(func(b string) string {
		return strings.NewReplacer(`,"source":"device"`, "", `,"source":"server"`, "").Replace(b)
	})
	if _, err := s.ImportRoom(ctx, old, importOpts("old")); err != nil {
		t.Fatal(err)
	}
	if got := sources("old"); got["said on device"] != "voice/server" || got["said on server"] != "voice/server" || got["hello, third time"] != "text/" {
		t.Fatalf("archive without sources %v", got)
	}
	for name, c := range map[string]struct{ edit func(string) string }{
		"unknown source":   {func(b string) string { return strings.Replace(b, `"source":"device"`, `"source":"phone"`, 1) }},
		"empty-ish source": {func(b string) string { return strings.Replace(b, `"source":"device"`, `"source":" device"`, 1) }},
		"source on text": {func(b string) string {
			return strings.Replace(b, `"body":"hello, third time"`, `"body":"hello, third time","source":"server"`, 1)
		}},
		"source on system": {func(b string) string {
			return strings.Replace(b, `"kind":"system",`, `"kind":"system","source":"device",`, 1)
		}},
	} {
		d := withLines(c.edit)
		if raw, _ := os.ReadFile(filepath.Join(d, fileMessages)); string(raw) == lines {
			t.Fatalf("%s: the edit changed nothing", name)
		}
		if _, err := s.ImportRoom(ctx, d, importOpts("bad")); !errors.Is(err, ErrImportRefused) || !strings.Contains(err.Error(), "source") {
			t.Errorf("%s: %v, want a refused source", name, err)
		}
	}
}
