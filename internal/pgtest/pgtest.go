// Package pgtest gives each test a PostgreSQL database of its own on the server that
// GRILL_ROOM_TEST_DATABASE_URL names; tests skip when it is unset, and fail instead when
// GRILL_ROOM_TEST_REQUIRE_PG=1, so CI cannot go green by skipping them. Template copies a database
// made once per test binary, as CREATE DATABASE .. TEMPLATE is faster than building the schema.
package pgtest

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // the driver store uses
)

// Env names the server: a URL to a database the role may create databases from.
const Env = "GRILL_ROOM_TEST_DATABASE_URL"

// RequireEnv set to 1 turns the skip on an unset Env into a failure.
const RequireEnv = "GRILL_ROOM_TEST_REQUIRE_PG"

// CreateOptions makes a database that sorts as the server needs (017-AC6). It copies
// store.CreateDatabaseOptions, which store's tests cannot import through here; a store test checks
// they match.
const CreateOptions = "TEMPLATE template0 LC_COLLATE 'C' LC_CTYPE 'C' ENCODING 'UTF8' LOCALE_PROVIDER libc"

// stale is the age past which a test or template database is a leftover of a killed run.
const stale = 2 * time.Hour

func init() {
	// A program that is not a test must never drop databases.
	if !testing.Testing() {
		panic("pgtest imported outside a test binary")
	}
}

var (
	adminOnce sync.Once
	adminDB   *sql.DB
	adminErr  error
	seq       atomic.Int64

	tplOnce sync.Once
	tplName string
	tplErr  error
)

var nameRE = regexp.MustCompile(`^grtest_(?:tpl_)?(\d+)_`)

// URL is the server Env names; an unset Env skips t, or fails it under RequireEnv.
func URL(t testing.TB) string {
	t.Helper()
	u := os.Getenv(Env)
	if u == "" {
		if os.Getenv(RequireEnv) == "1" {
			t.Fatal(Env + " is unset and " + RequireEnv + "=1")
		}
		t.Skip(Env + " is unset: PostgreSQL tests skipped")
	}
	return u
}

// dsn is base pointed at database name.
func dsn(base, name string) string {
	if u, err := url.Parse(base); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		u.Path = "/" + name
		return u.String()
	}
	return base + " dbname=" + name
}

func timeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), time.Minute)
}

// admin is the connection to the server's own database, which creates and drops the others. The
// first use drops the leftovers of killed runs.
func admin(t testing.TB) *sql.DB {
	t.Helper()
	base := URL(t)
	adminOnce.Do(func() {
		adminDB, adminErr = sql.Open("pgx", base)
		if adminErr != nil {
			return
		}
		adminDB.SetMaxOpenConns(4)
		adminErr = sweep()
	})
	if adminErr != nil {
		t.Fatalf("pgtest: %v", adminErr)
	}
	return adminDB
}

func sweep() error {
	c, cancel := timeout()
	defer cancel()
	rows, err := adminDB.QueryContext(c, `SELECT datname FROM pg_database WHERE datname LIKE 'grtest\_%'`)
	if err != nil {
		return fmt.Errorf("list test databases: %w", err)
	}
	var old []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			_ = rows.Close()
			return fmt.Errorf("list test databases: %w", err)
		}
		if m := nameRE.FindStringSubmatch(n); m != nil {
			if ts, _ := strconv.ParseInt(m[1], 10, 64); time.Since(time.Unix(ts, 0)) > stale {
				old = append(old, n)
			}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("list test databases: %w", err)
	}
	_ = rows.Close()
	for _, n := range old {
		if _, err := adminDB.ExecContext(c, `DROP DATABASE IF EXISTS "`+n+`" WITH (FORCE)`); err != nil {
			return fmt.Errorf("drop leftover %s: %w", n, err)
		}
	}
	return nil
}

func newName(kind string) string {
	return fmt.Sprintf("grtest_%s%d_%d_%d", kind, time.Now().Unix(), os.Getpid(), seq.Add(1))
}

// Create makes a database with options (as CREATE DATABASE takes them), dropped when t ends, and
// returns its DSN.
func Create(t testing.TB, options string) string {
	t.Helper()
	db := admin(t)
	name := newName("")
	c, cancel := timeout()
	defer cancel()
	if _, err := db.ExecContext(c, `CREATE DATABASE "`+name+`" `+options); err != nil {
		t.Fatalf("pgtest: create database: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := timeout()
		defer cancel()
		if _, err := db.ExecContext(c, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`); err != nil {
			t.Errorf("pgtest: drop %s: %v", name, err)
		}
	})
	return dsn(URL(t), name)
}

// Main runs m's tests, drops the template database Template made, and exits with the tests'
// code. Call it from TestMain of every package that uses Template.
func Main(m *testing.M) {
	code := m.Run()
	if tplName != "" && adminDB != nil {
		c, cancel := timeout()
		if _, err := adminDB.ExecContext(c, `DROP DATABASE IF EXISTS "`+tplName+`" WITH (FORCE)`); err != nil {
			// The next run's sweep drops it once stale.
			_, _ = fmt.Fprintf(os.Stderr, "pgtest: drop template %s: %v\n", tplName, err)
		}
		cancel()
	}
	os.Exit(code)
}

// Fresh is an empty database with C collation, dropped when t ends; it returns its DSN.
func Fresh(t testing.TB) string {
	t.Helper()
	return Create(t, CreateOptions)
}

// Template returns the DSN of a copy of the database create built once per test binary, at the
// DSN it is given, so every call in a binary must create the same database. The copy is dropped
// when t ends.
func Template(t testing.TB, create func(dsn string) error) string {
	t.Helper()
	db := admin(t)
	tplOnce.Do(func() {
		tplName = newName("tpl_")
		c, cancel := timeout()
		defer cancel()
		if _, tplErr = db.ExecContext(c, `CREATE DATABASE "`+tplName+`" `+CreateOptions); tplErr != nil {
			return
		}
		if tplErr = create(dsn(URL(t), tplName)); tplErr != nil {
			return
		}
		// CREATE DATABASE .. TEMPLATE refuses a template anyone is connected to.
		_, tplErr = db.ExecContext(c, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, tplName)
	})
	if tplErr != nil {
		t.Fatalf("pgtest template: %v", tplErr)
	}
	return Create(t, `TEMPLATE "`+tplName+`"`)
}

// Find returns the "table.column" of every column of the database at dsn holding needle, and how
// many columns it read: bytea by its bytes, any other type as text.
func Find(t testing.TB, dsn, needle string) (hits []string, scanned int) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("pgtest find: %v", err)
	}
	defer db.Close() //nolint:errcheck // a read-only probe
	c, cancel := timeout()
	defer cancel()
	rows, err := db.QueryContext(c, `SELECT table_name, column_name, data_type FROM information_schema.columns
		WHERE table_schema = current_schema() ORDER BY table_name, column_name`)
	if err != nil {
		t.Fatalf("pgtest find: %v", err)
	}
	var cols [][3]string
	for rows.Next() {
		var col [3]string
		if err := rows.Scan(&col[0], &col[1], &col[2]); err != nil {
			t.Fatalf("pgtest find: %v", err)
		}
		cols = append(cols, col)
	}
	_ = rows.Close()
	for _, col := range cols {
		match := fmt.Sprintf(`strpos(CAST(%q AS TEXT), $1) > 0`, col[1])
		if col[2] == "bytea" {
			match = fmt.Sprintf(`position(convert_to($1, 'UTF8') IN %q) > 0`, col[1])
		}
		var n int
		if err := db.QueryRowContext(c, fmt.Sprintf(`SELECT count(*) FROM %q WHERE `, col[0])+match, needle).Scan(&n); err != nil {
			t.Fatalf("pgtest find %s.%s: %v", col[0], col[1], err)
		}
		if n > 0 {
			hits = append(hits, col[0]+"."+col[1])
		}
	}
	return hits, len(cols)
}
