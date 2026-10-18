package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func call(args ...string) (code int, stdout, stderr string) {
	var o, e bytes.Buffer
	code = run(args, &o, &e)
	return code, o.String(), e.String()
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"fresh", "postgres://h/postgres"},
		{"create", "postgres://h/postgres", "e2e_x"},
		{"drop", "postgres://h/postgres", "e2e_x"},
		// Names that would change the DDL, that PostgreSQL would truncate or fold, or that are no e2e
		// database.
		{"fresh", "postgres://h/postgres", "e2e_x; DROP DATABASE postgres"},
		{"fresh", "postgres://h/postgres", `e2e_x" WITH (FORCE)`},
		{"fresh", "postgres://h/postgres", "E2E_X"},
		{"fresh", "postgres://h/postgres", "e2e_"},
		{"fresh", "postgres://h/postgres", ""},
		{"fresh", "postgres://h/postgres", "postgres"},
		{"fresh", "postgres://h/postgres", "grill_room"},
		{"fresh", "postgres://h/postgres", "x_e2e_1"},
		{"fresh", "postgres://h/postgres", "e2e_" + strings.Repeat("a", 60)},
		{"url", "postgres://h/postgres"},
		{"url", "postgres://h/postgres", "postgres"},
		{"url", "postgres://h/postgres", "e2e_x; DROP DATABASE postgres"},
	} {
		if code, out, _ := call(args...); code != 2 || out != "" {
			t.Errorf("%q: code %d, stdout %q", args, code, out)
		}
	}
}

func TestDSN(t *testing.T) {
	for _, tc := range []struct{ server, want string }{
		{"postgres://u:p@127.0.0.1:5432/postgres?sslmode=disable", "postgres://u:p@127.0.0.1:5432/e2e_1?sslmode=disable"},
		{"postgresql://u@127.0.0.1", "postgresql://u@127.0.0.1/e2e_1"},
		{"host=127.0.0.1 user=u dbname=postgres", "host=127.0.0.1 user=u dbname=postgres dbname=e2e_1"},
	} {
		if got, err := dsn(tc.server, "e2e_1"); err != nil || got != tc.want {
			t.Errorf("dsn(%q) = %q, %v; want %q", tc.server, got, err, tc.want)
		}
	}
}

// A failed connection names neither the URL nor its password.
func TestErrorHidesPassword(t *testing.T) {
	code, out, errOut := call("fresh", "postgres://u:s3cret-pw@127.0.0.1:1/postgres?connect_timeout=5", "e2e_x")
	if code != 1 || out != "" || errOut == "" || strings.Contains(errOut, "s3cret-pw") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func scalar(t *testing.T, dsn, q string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRowContext(ctx, q).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

func TestFresh(t *testing.T) {
	server := pgtest.URL(t)
	name := fmt.Sprintf("e2e_pgdb_test_%d", os.Getpid())
	drop := func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		db, err := sql.Open("pgx", server)
		if err != nil {
			t.Error(err)
			return
		}
		defer db.Close()
		if _, err := db.ExecContext(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(drop)

	code, out, errOut := call("fresh", server, name)
	if code != 0 {
		t.Fatalf("fresh: %d %s", code, errOut)
	}
	db := strings.TrimSpace(out)
	if c := scalar(t, db, `SELECT datcollate || ' ' || datctype || ' ' || datlocprovider::text || ' ' || current_database() FROM pg_database WHERE datname = current_database()`); c != "C C c "+name {
		t.Fatalf("database %q", c)
	}

	// fresh again replaces it, even with a session still open on it.
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	held, err := sql.Open("pgx", db)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if _, err := held.ExecContext(ctx, `CREATE TABLE left_over (v int)`); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := call("fresh", server, name); code != 0 {
		t.Fatalf("fresh again: %d %s", code, errOut)
	}
	if n := scalar(t, db, `SELECT count(*)::text FROM pg_tables WHERE tablename = 'left_over'`); n != "0" {
		t.Fatalf("fresh kept the old tables: %s", n)
	}

	// url names the same database and keeps what it holds; a missing one fails.
	if _, err := held.ExecContext(ctx, `CREATE TABLE kept (v int)`); err != nil {
		t.Fatal(err)
	}
	if code, again, errOut := call("url", server, name); code != 0 || again != out {
		t.Fatalf("url: %d %q %s; want %q", code, again, errOut, out)
	}
	if n := scalar(t, db, `SELECT count(*)::text FROM pg_tables WHERE tablename = 'kept'`); n != "1" {
		t.Fatalf("url dropped the database: %s", n)
	}
	if code, got, _ := call("url", server, name+"_missing"); code != 1 || got != "" {
		t.Fatalf("url of a missing database: %d %q", code, got)
	}
}
