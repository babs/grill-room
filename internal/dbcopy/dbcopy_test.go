package dbcopy

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/pgtest"
	"github.com/babs/grill-room/internal/store"
)

// fixture writes a SQLite database at version, built from the committed v46 schema with a row or
// more in every table, then runs extra statements on it; it returns the file's path.
func fixture(t *testing.T, version int, extra ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "grill-room.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, f := range []string{"testdata/sqlite-v46.sql", "testdata/rows.sql"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), string(b)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	for _, q := range append(extra, "PRAGMA user_version = "+strconv.Itoa(version)) {
		if _, err := db.ExecContext(t.Context(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return path
}

func openPG(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// relations lists what the target's schema holds.
func relations(t *testing.T, dsn string) []string {
	t.Helper()
	names, err := strings1(t.Context(), openPG(t, dsn), `SELECT relname FROM pg_class WHERE relnamespace = current_schema()::regnamespace`)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// 017-AC7, 017-AC8: every table copies with its ids, text and timestamps as the store writes
// them, identities past their maximum, and the copy compares equal on both sides.
func TestCopy(t *testing.T) {
	ctx := t.Context()
	// As the server left it, in WAL mode, on a read-only mount: opening it must create no -shm.
	src := fixture(t, 46, "PRAGMA journal_mode=WAL")
	if err := os.Chmod(filepath.Dir(src), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(src), 0o700) })
	dsn := pgtest.Fresh(t)
	var out bytes.Buffer
	if err := Copy(ctx, src, dsn, &out); err != nil {
		t.Fatalf("copy: %v\n%s", err, &out)
	}
	report := out.String()
	if !strings.Contains(report, "total: 47 tables") || strings.Contains(report, "DIFFER") {
		t.Fatalf("report:\n%s", report)
	}
	for _, line := range []string{"rooms", "messages"} {
		if !strings.Contains(lineOf(report, line), "1 text values sanitized") {
			t.Errorf("%s line lacks its sanitized count:\n%s", line, report)
		}
	}
	if !strings.Contains(lineOf(report, "messages"), "1 timestamps kept unparsed") || !strings.Contains(lineOf(report, "invites"), "1 timestamps kept unparsed") {
		t.Errorf("unparsed timestamps not counted:\n%s", report)
	}
	out.Reset()
	if err := Verify(ctx, src, dsn, &out); err != nil {
		t.Fatalf("verify: %v\n%s", err, &out)
	}

	db := openPG(t, dsn)
	for _, c := range []struct{ q, want string }{
		{`SELECT name FROM rooms WHERE id = 'r2'`, "Room��!"},
		{`SELECT body FROM messages WHERE id = 20`, "hi��"},
		{`SELECT ts FROM messages WHERE id = 20`, "2026-01-02T02:04:05.100000000Z"},
		{`SELECT ts FROM messages WHERE id = 21`, "not a time"},
		{`SELECT email_nudge_until FROM profiles WHERE id = 3`, "2026-01-02T01:04:05.500000000Z"},
		{`SELECT skills_disclaimer_at FROM profiles WHERE id = 3`, ""},
		{`SELECT name FROM profiles WHERE id = 3`, "Zoé 🔥"},
		{`SELECT host_participant_id::text FROM rooms WHERE id = 'r1'`, "4"},
		{`SELECT created_by::text FROM invites WHERE id = 2`, "4"},
		{`SELECT rerun_of::text FROM turns WHERE id = 10`, "11"},
		{`SELECT scope FROM skills WHERE id = 42`, "personal"},
		{`SELECT at_ms::text FROM audit_log WHERE id = 45`, "9223372036854775807"},
		{`SELECT encode(data, 'hex') FROM uploads WHERE id = 30`, "00ff00"},
		{`SELECT encode(data, 'hex') FROM attachments WHERE id = 36`, ""},
		{`SELECT version::text FROM schema_version`, "46"},
		{`SELECT count(*)::text FROM pg_constraint WHERE contype = 'f' AND condeferrable`, "0"},
	} {
		var got string
		if err := db.QueryRowContext(ctx, c.q).Scan(&got); err != nil || got != c.want {
			t.Errorf("%s = %q, %v; want %q", c.q, got, err, c.want)
		}
	}

	// The next insert of every identity table takes the next id; messages' comes after the id
	// SQLite last handed out, a since deleted row's.
	rows, err := db.QueryContext(ctx, `SELECT c.relname, a.attname FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
		WHERE c.relnamespace = current_schema()::regnamespace AND a.attidentity <> '' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var idents [][2]string
	for rows.Next() {
		var p [2]string
		if err := rows.Scan(&p[0], &p[1]); err != nil {
			t.Fatal(err)
		}
		idents = append(idents, p)
	}
	_ = rows.Close()
	if len(idents) != 34 {
		t.Fatalf("%d identity columns, want 34", len(idents))
	}
	for _, p := range idents {
		var maxID, next int64
		if err := db.QueryRowContext(ctx, `SELECT MAX(`+ident(p[1])+`) FROM `+ident(p[0])).Scan(&maxID); err != nil {
			t.Fatalf("%s: %v", p[0], err)
		}
		if err := db.QueryRowContext(ctx, `SELECT nextval(pg_get_serial_sequence($1, $2))`, p[0], p[1]).Scan(&next); err != nil {
			t.Fatalf("%s: %v", p[0], err)
		}
		want := maxID + 1
		if p[0] == "messages" {
			want = 1001
		}
		if next != want {
			t.Errorf("%s: next id %d, want %d", p[0], next, want)
		}
	}

	s, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("store.Open on the copy: %v", err)
	}
	defer s.Close()
	msgs, err := s.Messages(ctx, "r1", 0, 100)
	if err != nil || len(msgs) != 2 || msgs[0].ID != 20 || msgs[1].ID != 21 || msgs[0].Body != "hi��" {
		t.Fatalf("messages of r1: %+v %v", msgs, err)
	}
}

func lineOf(report, table string) string {
	for _, l := range strings.Split(report, "\n") {
		if strings.HasPrefix(l, table+" ") {
			return l
		}
	}
	return ""
}

// 017-AC7: the fixture has a row in every table, and its tables and columns are the PostgreSQL
// baseline's: a migration past 46 fails this test until the copy and its fixture follow.
func TestFixtureMatchesBaseline(t *testing.T) {
	ctx := t.Context()
	db := openPG(t, pgtest.Fresh(t))
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // a probe
	if err := store.CreateBaseline(ctx, tx); err != nil {
		t.Fatal(err)
	}
	tables, err := loadSchema(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	src, err := openSource(ctx, fixture(t, 46))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if err := checkShape(ctx, src, tables); err != nil {
		t.Fatal(err)
	}
	for _, tb := range tables {
		var n int
		if err := src.QueryRowContext(ctx, `SELECT count(*) FROM `+ident(tb.name)).Scan(&n); err != nil || n == 0 {
			t.Errorf("fixture table %s: %d rows, %v", tb.name, n, err)
		}
	}
	for _, c := range []struct{ extra, want string }{
		{`ALTER TABLE rooms ADD COLUMN extra TEXT`, "column rooms.extra only in the source"},
		{`CREATE TABLE extra (a INTEGER)`, "table extra only in the source"},
		{`DROP TABLE tts_samples`, "table tts_samples only in the target"},
		{`ALTER TABLE api_tokens DROP COLUMN last_used_ip`, "column api_tokens.last_used_ip only in the target"},
	} {
		src, err := openSource(ctx, fixture(t, 46, c.extra))
		if err != nil {
			t.Fatal(err)
		}
		if err := checkShape(ctx, src, tables); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.extra, err, c.want)
		}
		_ = src.Close()
	}
}

// 017-AC7: a non-empty target, a source at another version, a source with a WAL or a value
// PostgreSQL's column cannot hold are refused, and the target is left as it was.
func TestCopyRefusals(t *testing.T) {
	ctx := t.Context()
	wal := fixture(t, 46)
	if err := os.WriteFile(wal+"-wal", []byte("frames"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, src, target, want string
	}{
		{"non-empty target", fixture(t, 46), `CREATE TABLE stray (a int)`, "target is not empty: its schema holds stray"},
		{"sequence left", fixture(t, 46), `CREATE SEQUENCE stray_seq`, "stray_seq"},
		{"version 45", fixture(t, 45), "", "schema version 45"},
		{"version 47", fixture(t, 47), "", "schema version 47"},
		{"wal", wal, "", "-wal is not empty: copy from a consistent backup taken with the server stopped"},
		{"text in a bigint", fixture(t, 46, `UPDATE audit_log SET at_ms = 'soon' WHERE id = 45`), "", "audit_log.at_ms rowid 45: a TEXT value does not fit a bigint column"},
		{"blob in a text", fixture(t, 46, `UPDATE settings SET value = X'00' WHERE key = 'empty'`), "", "settings.value rowid 2: a BLOB value does not fit a text column"},
		{"real in a bigint", fixture(t, 46, `UPDATE rooms SET chain_budget = 2.5 WHERE id = 'r1'`), "", "rooms.chain_budget rowid 1: a REAL value does not fit a bigint column"},
		{"missing file", filepath.Join(t.TempDir(), "none.db"), "", "source:"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dsn := pgtest.Fresh(t)
			if c.target != "" {
				if _, err := openPG(t, dsn).ExecContext(ctx, c.target); err != nil {
					t.Fatal(err)
				}
			}
			before := relations(t, dsn)
			err := Copy(ctx, c.src, dsn, io.Discard)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("copy: %v, want %q", err, c.want)
			}
			if after := relations(t, dsn); len(after) != len(before) {
				t.Fatalf("target changed: %v, was %v", after, before)
			}
		})
	}
}

// 017-AC8: a target row changed or gone after the copy makes verify fail naming its table.
func TestVerifyNamesChangedTables(t *testing.T) {
	ctx := t.Context()
	src := fixture(t, 46)
	dsn := pgtest.Fresh(t)
	if err := Copy(ctx, src, dsn, io.Discard); err != nil {
		t.Fatal(err)
	}
	db := openPG(t, dsn)
	if _, err := db.ExecContext(ctx, `UPDATE messages SET body = 'hi' WHERE id = 20`); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := Verify(ctx, src, dsn, &out)
	if err == nil || err.Error() != "1 of 47 tables differ: messages" || !strings.Contains(lineOf(out.String(), "messages"), "DIFFERS") {
		t.Fatalf("verify: %v\n%s", err, &out)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM room_blocks`); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	err = Verify(ctx, src, dsn, &out)
	if err == nil || err.Error() != "2 of 47 tables differ: messages, room_blocks" || !strings.Contains(out.String(), "2 DIFFER") {
		t.Fatalf("verify: %v\n%s", err, &out)
	}
}

// 017-AC8: a difference found before the commit rolls the whole copy back.
func TestCopyMismatchCommitsNothing(t *testing.T) {
	ctx := t.Context()
	beforeVerify = func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE pronunciations SET en = 'x'`)
		return err
	}
	t.Cleanup(func() { beforeVerify = nil })
	dsn := pgtest.Fresh(t)
	var out bytes.Buffer
	err := Copy(ctx, fixture(t, 46), dsn, &out)
	if err == nil || err.Error() != "1 of 47 tables differ: pronunciations; nothing committed" {
		t.Fatalf("copy: %v\n%s", err, &out)
	}
	if rel := relations(t, dsn); len(rel) != 0 {
		t.Fatalf("target holds %v after a failed copy", rel)
	}
}

func TestConvert(t *testing.T) {
	col := func(name, typ string) column { return column{name: name, typ: typ} }
	for _, c := range []struct {
		v         any
		c         column
		want      any
		err       string
		san, badT int64
	}{
		{v: nil, c: col("x", "bigint"), want: nil},
		{v: int64(math.MaxInt64), c: col("x", "bigint"), want: int64(math.MaxInt64)},
		{v: int64(1), c: col("x", "smallint"), want: int64(1)},
		{v: int64(70000), c: col("x", "smallint"), err: "a INTEGER value does not fit a smallint column"},
		{v: 1.5, c: col("x", "bigint"), err: "a REAL value does not fit a bigint column"},
		{v: int64(1), c: col("x", "double precision"), want: 1.0},
		{v: 1.15, c: col("x", "double precision"), want: 1.15},
		{v: "1", c: col("x", "double precision"), err: "a TEXT value does not fit"},
		{v: "a\x00\xff", c: col("x", "text"), want: "a��", san: 1},
		{v: "2026-01-02T03:04:05Z", c: col("created_at", "text"), want: "2026-01-02T03:04:05.000000000Z"},
		{v: "2026-01-02T03:04:05Z", c: col("name", "text"), want: "2026-01-02T03:04:05Z"},
		{v: "2026-01-02T03:04:05.1-01:00", c: col("ts", "text"), want: "2026-01-02T04:04:05.100000000Z"},
		{v: "", c: col("at", "text"), want: ""},
		{v: "soon", c: col("used_at", "text"), want: "soon", badT: 1},
		{v: int64(1), c: col("x", "text"), err: "a INTEGER value does not fit a text column"},
		{v: []byte{0, 1}, c: col("x", "bytea"), want: []byte{0, 1}},
		{v: []byte(nil), c: col("x", "bytea"), want: []byte{}},
		{v: "ab", c: col("x", "bytea"), want: []byte("ab")},
		{v: int64(1), c: col("x", "bytea"), err: "a INTEGER value does not fit a bytea column"},
	} {
		var r result
		got, err := convert(c.v, c.c, &r)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("convert(%#v, %s): %v, want %q", c.v, c.c.typ, err, c.err)
			}
			continue
		}
		if b, ok := got.([]byte); ok && b == nil {
			t.Errorf("convert(%#v) is a nil slice, which pgx writes as NULL", c.v)
		}
		var gb, wb bytes.Buffer
		_ = encode(&gb, []any{got})
		_ = encode(&wb, []any{c.want})
		if err != nil || !bytes.Equal(gb.Bytes(), wb.Bytes()) || r.sanitized != c.san || r.badTS != c.badT {
			t.Errorf("convert(%#v, %s %s) = %#v, %v, %+v; want %#v", c.v, c.c.name, c.c.typ, got, err, r, c.want)
		}
	}
}

// The canonical encoding tells apart what a plain concatenation would not.
func TestEncodeIsUnambiguous(t *testing.T) {
	enc := func(vals ...any) string {
		var b bytes.Buffer
		if err := encode(&b, vals); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	pairs := [][2][]any{
		{{"ab", "c"}, {"a", "bc"}},
		{{nil}, {""}},
		{{""}, {[]byte{}}},
		{{int64(1)}, {1.0}},
		{{"1"}, {int64(1)}},
	}
	for _, p := range pairs {
		if enc(p[0]...) == enc(p[1]...) {
			t.Errorf("%#v and %#v encode alike", p[0], p[1])
		}
	}
	if err := encode(io.Discard, []any{true}); err == nil {
		t.Error("an unexpected type encodes")
	}
}

// nextID is the id the next insert into table takes.
func nextID(t *testing.T, db *sql.DB, table string) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRowContext(t.Context(), `SELECT nextval(pg_get_serial_sequence($1, 'id'))`, table).Scan(&n); err != nil {
		t.Fatalf("%s: %v", table, err)
	}
	return n
}

// 017-AC7: SQLite leaves a sqlite_sequence row of 0 for a table emptied by a migration; it names
// no id handed out, and a seq past the largest id still counts.
func TestCopySequences(t *testing.T) {
	ctx := t.Context()
	src := fixture(t, 46, `DELETE FROM recovery_links`, `UPDATE sqlite_sequence SET seq = 0 WHERE name = 'recovery_links'`,
		`UPDATE sqlite_sequence SET seq = 500 WHERE name = 'audit_log'`)
	dsn := pgtest.Fresh(t)
	var out bytes.Buffer
	if err := Copy(ctx, src, dsn, &out); err != nil {
		t.Fatalf("copy: %v\n%s", err, &out)
	}
	db := openPG(t, dsn)
	if n := nextID(t, db, "recovery_links"); n != 1 {
		t.Errorf("recovery_links: next id %d, want 1", n)
	}
	if n := nextID(t, db, "audit_log"); n != 501 {
		t.Errorf("audit_log: next id %d, want 501", n)
	}
}

// 017-AC8: text keys sort on raw bytes in SQLite and on sanitized ones in PostgreSQL; verify
// compares them all the same.
func TestCopyTextKeysSanitized(t *testing.T) {
	ctx := t.Context()
	src := fixture(t, 46, `INSERT INTO settings(key, value) VALUES (CAST(X'7a80' AS TEXT), 'a'), (CAST(X'7ac3a9' AS TEXT), 'b'),
		(CAST(X'7900' AS TEXT), 'c'), (CAST(X'7901' AS TEXT), 'd')`)
	dsn := pgtest.Fresh(t)
	var out bytes.Buffer
	if err := Copy(ctx, src, dsn, &out); err != nil {
		t.Fatalf("copy: %v\n%s", err, &out)
	}
	out.Reset()
	if err := Verify(ctx, src, dsn, &out); err != nil {
		t.Fatalf("verify: %v\n%s", err, &out)
	}
}

// 017-AC6: copy refuses a target that does not sort as C before it writes anything.
func TestCopyRefusesNonCCollation(t *testing.T) {
	ctx := t.Context()
	probe := openPG(t, pgtest.Fresh(t))
	var icu bool
	if err := probe.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_collation WHERE collprovider = 'i')`).Scan(&icu); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{"libc en_US": "LC_COLLATE 'en_US.UTF-8' LC_CTYPE 'en_US.UTF-8' LOCALE_PROVIDER libc"}
	if icu {
		cases["icu und"] = "LC_COLLATE 'C' LC_CTYPE 'C' LOCALE_PROVIDER icu ICU_LOCALE 'und'"
	}
	src := fixture(t, 46)
	for name, opts := range cases {
		dsn := pgtest.Create(t, "TEMPLATE template0 ENCODING 'UTF8' "+opts)
		if err := Copy(ctx, src, dsn, io.Discard); err == nil || !strings.Contains(err.Error(), "the server needs C") {
			t.Errorf("%s: copy: %v", name, err)
		}
		if rel := relations(t, dsn); len(rel) != 0 {
			t.Errorf("%s: target holds %v", name, rel)
		}
	}
}
