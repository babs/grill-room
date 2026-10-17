package sqlitetest

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

func TestSynchronousOff(t *testing.T) {
	for _, dsn := range []string{"%s", "file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"} {
		db, err := sql.Open("sqlite", fmt.Sprintf(dsn, filepath.Join(t.TempDir(), "t.db")))
		if err != nil {
			t.Fatal(err)
		}
		var mode int
		err = db.QueryRowContext(t.Context(), "PRAGMA synchronous").Scan(&mode)
		_ = db.Close()
		if err != nil || mode != 0 {
			t.Fatalf("%s: synchronous %d, %v; want 0 (off)", dsn, mode, err)
		}
	}
}

// The template is made once, and each test gets a copy of its own.
func TestTemplate(t *testing.T) {
	made := 0
	create := func(p string) error {
		made++
		db, err := sql.Open("sqlite", p)
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		_, err = db.ExecContext(t.Context(), "CREATE TABLE x (v TEXT)")
		return err
	}
	var m template
	a, b := m.copy(t, "a.db", create), m.copy(t, "b.db", create)
	if made != 1 || a == b {
		t.Fatalf("made %d, paths %s %s", made, a, b)
	}
	count := func(p string, insert bool) int {
		db, err := sql.Open("sqlite", p)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		if insert {
			if _, err := db.ExecContext(t.Context(), "INSERT INTO x VALUES ('a')"); err != nil {
				t.Fatal(err)
			}
		}
		var n int
		if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM x").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count(a, true) != 1 || count(b, false) != 0 {
		t.Fatal("the copies share their rows")
	}
}
