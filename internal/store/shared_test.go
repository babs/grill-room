package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/babs/grill-room/internal/pgtest"
)

// sharedTables are the UNLOGGED tables of v47; tts_lines and rate_events stay logged.
var sharedTables = []string{"daemon_connections", "daemon_status", "pairings", "presence", "stt_workers", "tts_pod"}

// A database at v46, as Phase 2's cutover left the lab, migrates to v47 once, with the shared
// tables UNLOGGED and the lexicon version seeded.
func TestMigrateFrom46(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	dsn := pgtest.Fresh(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := CreateBaseline(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		_ = openAt(t, dsn).Close()
		var v, lex int
		if err := db.QueryRowContext(ctx, `SELECT version, (SELECT count(*) FROM settings WHERE key = 'lexicon_version' AND value = '0') FROM schema_version`).Scan(&v, &lex); err != nil {
			t.Fatal(err)
		}
		if v != 47 || lex != 1 {
			t.Fatalf("version %d, %d lexicon versions", v, lex)
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT relname FROM pg_class WHERE relkind = 'r' AND relpersistence = 'u'
		AND relnamespace = 'public'::regnamespace ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var unlogged []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		unlogged = append(unlogged, n)
	}
	if !slices.Equal(unlogged, sharedTables) {
		t.Fatalf("unlogged tables %v, want %v", unlogged, sharedTables)
	}
	var moved string
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('tts_lines')::text || to_regclass('rate_events')::text || count(host_moved_line) FROM rooms`).Scan(&moved); err != nil || moved != "tts_linesrate_events0" {
		t.Fatalf("logged tables and rooms.host_moved_line: %q %v", moved, err)
	}
}

// waiting reports whether a session waits for the advisory lock (kind, room).
func waiting(t *testing.T, s *Store, kind int32, room string) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted
		AND classid = ?::oid AND objid = hashtext(?)::oid AND objsubid = 2 AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`,
		kind, room).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func lockRoom(t *testing.T, s *Store, kind int32, room string, try bool) (func(), bool) {
	t.Helper()
	release, ok, err := s.LockRoom(t.Context(), kind, room, try)
	if err != nil {
		t.Fatal(err)
	}
	return release, ok
}

// Two stores on one database: a room lock excludes its other holders, on whichever pod, and only
// them; a waiter gets it once the holder releases.
func TestLockRoom(t *testing.T) {
	dsn := testDSN(t)
	a, b := openAt(t, dsn), openAt(t, dsn)
	release, ok := lockRoom(t, a, 1, "r1", false)
	if !ok {
		t.Fatal("first lock refused")
	}
	if _, ok := lockRoom(t, b, 1, "r1", true); ok {
		t.Fatal("a second holder took the room's lock")
	}
	if _, ok := lockRoom(t, a, 1, "r1", true); ok {
		t.Fatal("a second session of the same pod took the room's lock")
	}
	for _, other := range []struct {
		kind int32
		room string
	}{{2, "r1"}, {1, "r2"}} {
		r, ok := lockRoom(t, b, other.kind, other.room, true)
		if !ok {
			t.Fatalf("lock %d %s refused while only 1 r1 is held", other.kind, other.room)
		}
		r()
	}

	got := make(chan func())
	go func() {
		r, ok, err := b.LockRoom(context.Background(), 1, "r1", false)
		if err != nil || !ok {
			t.Errorf("waiting lock: %v %v", ok, err)
		}
		got <- r
	}()
	for !waiting(t, a, 1, "r1") {
		select {
		case r := <-got:
			r()
			t.Fatal("the waiter took a held lock")
		case <-time.After(10 * time.Millisecond):
		}
	}
	release()
	select {
	case r := <-got:
		r()
	case <-time.After(10 * time.Second):
		t.Fatal("the waiter never got the released lock")
	}
	if r, ok := lockRoom(t, a, 1, "r1", true); !ok {
		t.Fatal("released lock still held")
	} else {
		r()
	}
}

// A wait for a room lock ends at lock_timeout, as the write lock's does.
func TestLockRoomWaitsLockTimeout(t *testing.T) {
	dsn := testDSN(t)
	a := openAt(t, dsn)
	b, err := Open(dsn, withLockTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	release, _ := lockRoom(t, a, 1, "r1", false)
	defer release()
	var pg *pgconn.PgError
	if _, _, err := b.LockRoom(t.Context(), 1, "r1", false); !errors.As(err, &pg) || pg.Code != "55P03" {
		t.Fatalf("wait past lock_timeout: %v", err)
	}
}

// A release whose unlock fails discards the connection, which frees the lock, rather than pool a
// session that may still hold it.
func TestLockRoomDiscardsConnOnUnlockFailure(t *testing.T) {
	dsn := testDSN(t)
	a := openAt(t, dsn)
	b, err := Open(dsn, withLockTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	defer func(q string) { unlockSQL = q }(unlockSQL)
	unlockSQL = `SELECT false WHERE $1::int4 IS NOT NULL AND $2::text IS NOT NULL`
	release, _ := lockRoom(t, a, 1, "r1", false)
	release()
	// A pooled session would keep the lock past lock_timeout; a closed one frees it.
	r, ok, err := b.LockRoom(t.Context(), 1, "r1", false)
	if err != nil || !ok {
		t.Fatalf("the lock outlived its release: %v", err)
	}
	r()
}

// Listen hands over every notification on its channel, after ready, until its context ends.
func TestNotifyListen(t *testing.T) {
	s := openTest(t)
	ctx, cancel := context.WithCancel(t.Context())
	ready, got, done := make(chan struct{}), make(chan string, 4), make(chan error)
	go func() {
		done <- s.Listen(ctx, "chan_a", func() { close(ready) }, func(p string) { got <- p })
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatal(err)
	}
	for _, c := range []string{"chan_b", "chan_a"} {
		if err := s.Notify(t.Context(), c, "on "+c); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case p := <-got:
		if p != "on chan_a" {
			t.Fatalf("got %q", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no notification")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("listen ended with %v", err)
	}
}

// SweepExpired drops the leases that ended and the rate events past the longest window, and keeps
// the rest.
func TestSweepExpired(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	if _, err := s.CreateRoom(ctx, "r", "R"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInvite(ctx, "r", "inv-r", 0); err != nil {
		t.Fatal(err)
	}
	p, _, err := s.Join(ctx, "inv-r", "Alice", "tok-r", JoinProfile{})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.PairDaemon(ctx, "r", p.ID, "dmn-r")
	if err != nil {
		t.Fatal(err)
	}
	const now, cutoff = int64(10_000), int64(5_000)
	err = s.db.unlocked(ctx, func(tx *txn) error {
		for _, at := range []int64{now, now + 1} {
			id := strconv.FormatInt(at, 10)
			for _, q := range []struct {
				sql  string
				args []any
			}{
				{`INSERT INTO presence(room_id, participant_id, tab, expires_at_ms) VALUES('r', ?, ?, ?)`, []any{p.ID, id, at}},
				{`INSERT INTO daemon_connections(id, daemon_id, room_id, pod, connected_at_ms, expires_at_ms) VALUES(?, ?, 'r', 'p', 0, ?)`, []any{id, d.ID, at}},
				{`INSERT INTO stt_workers(worker, rooms, voices, engine, glossary, expires_at_ms) VALUES(?, '', '', '', 0, ?)`, []any{id, at}},
				{`INSERT INTO pairings(id, room_id, code, secret_hash, expires_at_ms) VALUES(?, 'r', '', '', ?)`, []any{id, at}},
			} {
				if _, err := tx.ExecContext(ctx, q.sql, q.args...); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []int64{cutoff - 1, cutoff} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO rate_events(scope, key_hash, at_ms) VALUES('login', 'k', ?)`, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SweepExpired(ctx, now, cutoff); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int64{"presence": now + 1, "daemon_connections": now + 1, "stt_workers": now + 1, "pairings": now + 1, "rate_events": cutoff} {
		col := "expires_at_ms"
		if table == "rate_events" {
			col = "at_ms"
		}
		var n, left int64
		//nolint:gosec // names from the map above
		if err := s.db.QueryRowContext(ctx, `SELECT count(*), max(`+col+`) FROM `+table).Scan(&n, &left); err != nil || n != 1 || left != want {
			t.Errorf("%s: %d rows, left %d, %v; want 1 at %d", table, n, left, err, want)
		}
	}
}

// unlocked runs only the writes that need no commit order: a write to a table an id cursor reads,
// or an id-taking insert, would commit out of id order without the write lock.
func TestUnlockedRefusesOtherWrites(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	run := func(q string) error {
		return s.db.unlocked(ctx, func(tx *txn) error {
			_, err := tx.ExecContext(ctx, q)
			return err
		})
	}
	for _, q := range []string{
		`INSERT INTO messages(room_id, ts, kind, sender, body) VALUES('r', '', 'text', 'x', 'y')`,
		`insert into "messages" select * from messages`,
		`UPDATE rooms SET name = 'x'`,
		`INSERT INTO rate_events(scope, key_hash, at_ms) VALUES('login', 'k', 1)`,
		`WITH x AS (DELETE FROM presence RETURNING 1) DELETE FROM messages`,
		`/* INSERT INTO presence */ DELETE FROM messages`,
		`UPDATE "rooms" presence SET name = 'x'`,
		`UPDATE presence2 SET x = 1`,
		`DELETE FROM rate_events2`,
		`DELETE FROM presence; UPDATE rooms SET name = 'pwn'`,
		`SELECT 1; UPDATE rooms SET name = 'pwn2'`,
		`TRUNCATE rate_events, audit_log`,
		`MERGE INTO presence p USING (SELECT 1) s ON false WHEN NOT MATCHED THEN DO NOTHING`,
	} {
		if err := run(q); !errors.Is(err, errUnlockedWrite) {
			t.Errorf("%s: %v, want refused", q, err)
		}
	}
	// Nor does the pool run several statements outside a transaction.
	for _, q := range []string{`SELECT 1; UPDATE rooms SET name = 'pwn2'`, `TRUNCATE rate_events`} {
		if _, err := s.db.QueryContext(ctx, q); !errors.Is(err, errWriteOutsideLock) {
			t.Errorf("pool %s: %v, want refused", q, err)
		}
	}
	err := s.db.unlocked(ctx, func(tx *txn) error {
		return tx.QueryRowContext(ctx, `INSERT INTO audit_log(at_ms) VALUES(1) RETURNING id`).Scan(new(int64))
	})
	if !errors.Is(err, errUnlockedWrite) {
		t.Errorf("insert returning: %v, want refused", err)
	}
	for _, q := range []string{
		`DELETE FROM rate_events`,
		`UPDATE tts_pod SET pod = 'x'`,
		`INSERT INTO settings(key, value) VALUES('mail_failed_at', '1') ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		`SELECT count(*) FROM messages`,
	} {
		if err := run(q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
	if err := s.Writable(ctx); err != nil {
		t.Fatal(err)
	}
}

// The keepalive ping between notifications leaves the LISTEN session as it was: every notification
// after a ping arrives, on the first connection.
func TestListenSurvivesPing(t *testing.T) {
	s := openTest(t)
	pinged := make(chan struct{}, 1)
	defer func(d time.Duration, f func()) { listenPing, listenPinged = d, f }(listenPing, listenPinged)
	listenPing = 20 * time.Millisecond
	listenPinged = func() {
		select {
		case pinged <- struct{}{}:
		default:
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	ready, got, done := make(chan struct{}, 2), make(chan string, 16), make(chan error, 1)
	go func() {
		done <- s.Listen(ctx, "chan_p", func() { ready <- struct{}{} }, func(p string) { got <- p })
	}()
	<-ready
	wait := func(what string, ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case err := <-done:
			t.Fatalf("listen ended waiting for %s: %v", what, err)
		case <-time.After(10 * time.Second):
			t.Fatalf("no %s within 10 s", what)
		}
	}
	for i := range 10 {
		select { // drop a ping that ran before this round
		case <-pinged:
		default:
		}
		wait("ping", pinged)
		want := strconv.Itoa(i)
		if err := s.Notify(t.Context(), "chan_p", want); err != nil {
			t.Fatal(err)
		}
		select {
		case p := <-got:
			if p != want {
				t.Fatalf("got %q, want %q", p, want)
			}
		case err := <-done:
			t.Fatalf("listen ended: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatalf("notification %d lost", i)
		}
	}
	cancel()
	<-done
	if len(ready) != 0 {
		t.Fatal("listen connected again")
	}
}
