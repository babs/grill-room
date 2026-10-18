package main

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/pgtest"
)

// dbFixture writes dbcopy's v46 fixture database and returns its path.
func dbFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "grill-room.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, f := range []string{"sqlite-v46.sql", "rows.sql"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "internal", "dbcopy", "testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), string(b)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	return path
}

func TestDBCommandUsage(t *testing.T) {
	noEnv := func(string) string { return "" }
	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{nil, 2, "usage: grill-room db copy|verify"},
		{[]string{"move"}, 2, "usage:"},
		{[]string{"copy"}, 2, "usage:"},
		{[]string{"verify", "--from", "x.db", "extra"}, 2, "usage:"},
		{[]string{"copy", "--bogus"}, 2, "flag provided but not defined"},
		{[]string{"copy", "-h"}, 0, "--to    the PostgreSQL DSN; default GRILL_ROOM_DATABASE_URL"},
		{[]string{"copy", "--from", "x.db"}, 2, "no target: pass --to or set GRILL_ROOM_DATABASE_URL"},
		{[]string{"verify", "--from", "x.db", "--to", "postgres://u:pw@[::1"}, 1, "the DSN does not parse"},
	} {
		var out, errOut bytes.Buffer
		if code := dbCommand(t.Context(), c.args, &out, &errOut, noEnv); code != c.code || !strings.Contains(errOut.String(), c.want) || strings.Contains(errOut.String(), "pw") {
			t.Errorf("db %q: exit %d, stderr %q; want %d and %q", c.args, code, errOut.String(), c.code, c.want)
		}
	}
}

// 017-AC7, 017-AC8: copy then verify exit 0; a changed target row makes verify exit 1 naming its
// table; a second copy into the now full target is refused; the DSN is never printed.
func TestDBCommandCopyVerify(t *testing.T) {
	ctx := t.Context()
	src := dbFixture(t)
	dsn := pgtest.Fresh(t)
	env := func(k string) string {
		if k == "GRILL_ROOM_DATABASE_URL" {
			return dsn
		}
		return ""
	}
	run := func(want int, args ...string) (string, string) {
		t.Helper()
		var out, errOut bytes.Buffer
		if code := dbCommand(ctx, args, &out, &errOut, env); code != want {
			t.Fatalf("db %q: exit %d, want %d\nstdout:\n%s\nstderr:\n%s", args, code, want, &out, &errOut)
		}
		if strings.Contains(out.String()+errOut.String(), dsn) {
			t.Fatalf("db %q printed the DSN", args)
		}
		return out.String(), errOut.String()
	}
	if out, _ := run(0, "copy", "--from", src, "--to", dsn); !strings.Contains(out, "total: 47 tables") {
		t.Fatalf("copy report:\n%s", out)
	}
	run(0, "verify", "--from", src)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `UPDATE rooms SET name = 'Renamed' WHERE id = 'r1'`); err != nil {
		t.Fatal(err)
	}
	if _, errOut := run(1, "verify", "--from", src); !strings.Contains(errOut, "1 of 47 tables differ: rooms") {
		t.Fatalf("verify stderr: %s", errOut)
	}
	if _, errOut := run(1, "copy", "--from", src); !strings.Contains(errOut, "target is not empty") {
		t.Fatalf("copy stderr: %s", errOut)
	}
}
