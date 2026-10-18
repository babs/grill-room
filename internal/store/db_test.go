package store

import (
	"context"
	"database/sql"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRebind(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`SELECT 1`, `SELECT 1`},
		{`a = ? AND b = ?`, `a = $1 AND b = $2`},
		{`x = '?' AND y = ?`, `x = '?' AND y = $1`},
		{`x = 'it''s ?' AND y = ?`, `x = 'it''s ?' AND y = $1`},
		{`"we?ird" = ? OR "q""?" = ?`, `"we?ird" = $1 OR "q""?" = $2`},
		{"-- what? 'unclosed\nSELECT ?", "-- what? 'unclosed\nSELECT $1"},
		{`?2 + ?1 + ?`, `$2 + $1 + $3`},
		{`?1::bigint, ?1`, `$1::bigint, $1`},
		{`SELECT '?'`, `SELECT '?'`},
		{"/* why? 'unclosed */ SELECT ?", "/* why? 'unclosed */ SELECT $1"},
		{"SELECT ? /* a /* nested? */ still? */, ?", "SELECT $1 /* a /* nested? */ still? */, $2"},
		{"SELECT ? /* unterminated ?", "SELECT $1 /* unterminated ?"},
	} {
		if got := rebind(c.in); got != c.want {
			t.Errorf("rebind(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsWrite(t *testing.T) {
	for q, want := range map[string]bool{
		`INSERT INTO t VALUES(1)`:                                 true,
		"  update t SET x = 1":                                    true,
		`DELETE FROM t`:                                           true,
		`WITH c AS (SELECT 1) UPDATE t SET x = 1`:                 true,
		`WITH c AS (DELETE FROM t RETURNING id) SELECT id FROM c`: true,
		`SELECT 1`:               false,
		`SELECT 'DELETE' FROM t`: false,
		"-- INSERT\nSELECT 1":    false,
		`SELECT "insert" FROM t`: false,
		`WITH c AS (SELECT updated_at FROM t) SELECT * FROM c`:        false,
		`WITH c AS (SELECT 'INSERT' AS k) SELECT k FROM c`:            false,
		`SELECT pg_advisory_xact_lock(1)`:                             false,
		`CREATE TABLE notes (id BIGINT PRIMARY KEY, x_update BIGINT)`: false,
		"/* INSERT */ SELECT 1":                                       false,
		"/* a /* DELETE */ UPDATE */ SELECT 1":                        false,
		"/* note */ INSERT INTO t VALUES(1)":                          true,
		"WITH c AS (SELECT 1) /* x */ DELETE FROM t":                  true,
	} {
		if got := isWrite(q); got != want {
			t.Errorf("isWrite(%q) = %v, want %v", q, got, want)
		}
	}
}

// A write that skips the lock could commit out of id order; the pool refuses it, and runs the
// writes it takes under the lock.
func TestWritesTakeTheLock(t *testing.T) {
	var locks atomic.Int64
	s, err := Open(testDSN(t), WithLockWait(func(time.Duration) { locks.Add(1) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	locks.Store(0)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	for _, q := range []string{`INSERT INTO settings(key, value) VALUES(?, ?) RETURNING key`,
		`WITH x AS (SELECT 1) INSERT INTO settings(key, value) VALUES(?, ?) RETURNING key`} {
		if _, err := s.db.QueryContext(ctx, q, "k", "v"); !errors.Is(err, errWriteOutsideLock) {
			t.Fatalf("%s: %v, want the write refused", q, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?, ?)`, "a", "1"); err != nil {
		t.Fatal(err)
	}
	var key string
	if err := s.db.QueryRowContext(ctx, `INSERT INTO settings(key, value) VALUES(?, ?) RETURNING key`, "b", "2").Scan(&key); err != nil || key != "b" {
		t.Fatalf("insert returning: %q %v", key, err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, "a").Scan(&key); err != nil || key != "1" {
		t.Fatalf("read: %q %v", key, err)
	}
	if err := s.Writable(ctx); err != nil {
		t.Fatal(err)
	}
	if n := locks.Load(); n != 2 {
		t.Fatalf("%d locks for 2 writes, a read and a health probe", n)
	}
	held := func(opts *sql.TxOptions) int {
		t.Helper()
		tx, err := s.db.BeginTx(ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback() //nolint:errcheck // nothing written
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid()
			AND objid::bigint = ?`, WriteLockKey).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := held(nil); n != 1 {
		t.Fatalf("a write transaction holds %d write locks", n)
	}
	if n := held(&sql.TxOptions{ReadOnly: true}); n != 0 {
		t.Fatalf("a read-only transaction holds %d write locks", n)
	}
}

// sqliteConstructs are the SQLite-only names no server code may hold any more (017-AC3).
var sqliteConstructs = regexp.MustCompile(`PRAGMA|json_extract|json_each|json_group_array|group_concat|NOCASE|randomblob|rowid|LastInsertId|sqlite_schema|wal_checkpoint|VACUUM`)

// importsSQLite reports a Go source that imports the SQLite driver.
func importsSQLite(t *testing.T, name string, src []byte) bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(f.Imports, func(i *ast.ImportSpec) bool { return strings.Trim(i.Path.Value, `"`) == "modernc.org/sqlite" })
}

// 017-AC3: no SQLite construct is left in the non-test files of the store and the web server, and
// no store file imports the SQLite driver.
func TestNoSQLiteLeft(t *testing.T) {
	for _, sample := range []string{"PRAGMA user_version", "json_extract(x, '$.a')", "FROM json_each(?)", "json_group_array(name)",
		"group_concat(k, ',')", "name COLLATE NOCASE", "hex(randomblob(4))", "ORDER BY rowid", "res.LastInsertId()",
		"FROM sqlite_schema", "PRAGMA wal_checkpoint(TRUNCATE)", "VACUUM", "// a comment wrapped\n// on VACUUM"} {
		if !sqliteConstructs.MatchString(sample) {
			t.Errorf("the guard misses %q", sample)
		}
	}
	if !importsSQLite(t, "x.go", []byte("package x\nimport (\n\t\"fmt\"\n\t_ \"modernc.org/sqlite\"\n)\n")) ||
		!importsSQLite(t, "y.go", []byte("package y\nimport sqlite \"modernc.org/sqlite\"\n")) ||
		importsSQLite(t, "z.go", []byte("package z\nimport _ \"github.com/jackc/pgx/v5/stdlib\"\n")) {
		t.Fatal("the import guard misreads its samples")
	}
	for _, dir := range []string{".", "../web"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f) //nolint:gosec // a source file of this repo
			if err != nil {
				t.Fatal(err)
			}
			for _, loc := range sqliteConstructs.FindAllIndex(src, -1) {
				line := 1 + strings.Count(string(src[:loc[0]]), "\n")
				t.Errorf("%s:%d: %s", f, line, src[loc[0]:loc[1]])
			}
			if dir == "." && importsSQLite(t, f, src) {
				t.Errorf("%s imports the SQLite driver", f)
			}
		}
	}
}

// Text that PG TEXT refuses, NUL and invalid UTF-8, becomes U+FFFD rather than failing the write,
// so a lookup by such text finds nothing rather than the text without it; bytea keeps its bytes.
func TestTextArgsSanitized(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	if _, err := s.CreateRoom(ctx, "r", "R"); err != nil {
		t.Fatal(err)
	}
	m := &Message{RoomID: "r", Kind: KindSystem, Sender: "x", Body: "a\x00b\xffc"}
	if err := s.InsertMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	got, err := s.Message(ctx, "r", m.ID)
	if err != nil || got.Body != "a\uFFFDb\uFFFDc" {
		t.Fatalf("body: %+v %v", got, err)
	}
	data := []byte("\x00\xff\x00png")
	if _, err := s.AddUpload(ctx, &Upload{RoomID: "r", Author: "x", Filename: "i.png", ContentType: "image/png", Format: "image", Data: data}, 1<<20); err != nil {
		t.Fatal(err)
	}
	var stored []byte
	if err := s.db.QueryRowContext(ctx, `SELECT data FROM uploads WHERE room_id = ?`, "r").Scan(&stored); err != nil || string(stored) != string(data) {
		t.Fatalf("bytea: %q %v", stored, err)
	}
	if !cleanText("plain é") || cleanText("a\x00") || cleanText("\xff") {
		t.Fatal("cleanText")
	}
	for _, id := range []string{"r\x00", "\x00r", "r\xff"} {
		if _, err := s.Room(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Room(%q): %v", id, err)
		}
	}
	nul, bad := "a\x00", "b\xff"
	var out1, out2 string
	if err := s.db.QueryRowContext(ctx, `SELECT ?::text, ?::text`, &nul, &bad).Scan(&out1, &out2); err != nil || out1 != "a\uFFFD" || out2 != "b\uFFFD" {
		t.Fatalf("*string args: %q %q %v", out1, out2, err)
	}
	if nul != "a\x00" {
		t.Fatal("the caller's string changed")
	}
}

// Stored timestamps compare as text in SQL (ORDER BY created_at, at < ?): their text order must be
// their time order, and they read back as written.
func TestTimestampTextOrder(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	paris := time.FixedZone("CET", 3600)
	times := []time.Time{base.Add(-time.Second), base, base.Add(5 * time.Microsecond), base.Add(500 * time.Millisecond),
		base.In(paris).Add(time.Second), base.Add(2 * time.Second)}
	for i, tm := range times {
		s := formatTS(tm)
		if got := parseTS(s); !got.Equal(tm) {
			t.Errorf("%s reads back as %s", s, got)
		}
		if i > 0 {
			if prev := formatTS(times[i-1]); prev >= s || len(prev) != len(s) {
				t.Errorf("%q does not sort before %q", prev, s)
			}
		}
	}
}
