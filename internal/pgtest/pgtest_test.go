package pgtest

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) { Main(m) }

func query(t *testing.T, dsn, q string, args ...any) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRowContext(ctx, q, args...).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// The template is made once, and each test gets a copy of its own.
func TestTemplate(t *testing.T) {
	made := 0
	create := func(dsn string) error {
		made++
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return err
		}
		defer db.Close()
		_, err = db.ExecContext(t.Context(), `CREATE TABLE x (v TEXT); INSERT INTO x VALUES ('a')`)
		return err
	}
	a, b := Template(t, create), Template(t, create)
	if made != 1 || a == b {
		t.Fatalf("made %d, copies %q %q", made, a, b)
	}
	query(t, a, `INSERT INTO x VALUES ('b') RETURNING v`)
	if n := query(t, b, `SELECT count(*)::text FROM x`); n != "1" {
		t.Fatalf("the copies share rows: %s in b", n)
	}
}

func TestFreshSortsAsC(t *testing.T) {
	if c := query(t, Fresh(t), `SELECT datcollate || ' ' || datlocprovider::text FROM pg_database WHERE datname = current_database()`); c != "C c" {
		t.Fatalf("collation %q", c)
	}
}

// TestRequireChild is the test TestRequire runs in a child process, with Env unset.
func TestRequireChild(t *testing.T) {
	if os.Getenv("PGTEST_CHILD") != "1" {
		t.Skip("run by TestRequire")
	}
	Fresh(t)
}

// With Env unset, a PG test skips, or fails when RequireEnv is 1: CI cannot pass by skipping (017-AC25).
func TestRequire(t *testing.T) {
	for _, tc := range []struct {
		require string
		fail    bool
		out     string
	}{
		{"1", true, RequireEnv + "=1"},
		{"", false, "PostgreSQL tests skipped"},
		{"0", false, "PostgreSQL tests skipped"},
	} {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRequireChild$", "-test.v")
		cmd.Env = append(os.Environ(), "PGTEST_CHILD=1", Env+"=", RequireEnv+"="+tc.require)
		out, err := cmd.CombinedOutput()
		cancel()
		if failed := err != nil; failed != tc.fail || !strings.Contains(string(out), tc.out) {
			t.Errorf("%s=%q: failed %v (%v), output:\n%s", RequireEnv, tc.require, failed, err, out)
		}
	}
}
