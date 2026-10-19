package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/babs/grill-room/internal/pgtest"
)

// sharedTables are the UNLOGGED tables of v47; tts_lines and rate_events stay logged.
var sharedTables = []string{"daemon_connections", "daemon_status", "pairings", "presence", "stt_workers", "tts_pod"}

// A database at v46, as Phase 2's cutover left the lab, migrates to the latest version once, with
// v47's shared tables UNLOGGED and the lexicon version seeded.
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
	// A turn running at the upgrade and one ended: v48 counts only the first as delivered.
	if _, err := tx.ExecContext(ctx, `INSERT INTO rooms(id, name, created_at) VALUES ('r', 'R', '2026-01-01T00:00:00Z');
		INSERT INTO turns(id, room_id, host, status, fresh, resume, message_cursor, annotation_cursor, prompt, started_at)
		VALUES (1, 'r', 'h', 'running', 0, '', 0, 0, 'p', '2026-01-01T00:00:00Z'), (2, 'r', 'h', 'done', 0, '', 0, 0, 'p', '2026-01-01T00:00:00Z')`); err != nil {
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
		if v != baselineVersion+len(migrations) || lex != 1 {
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
	var delivered string
	if err := db.QueryRowContext(ctx, `SELECT string_agg(id || ':' || delivered, ',' ORDER BY id) FROM turns`).Scan(&delivered); err != nil || delivered != "1:1,2:0" {
		t.Fatalf("delivered after the upgrade: %q %v", delivered, err)
	}
	var moved string
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('tts_lines')::text || to_regclass('rate_events')::text || count(host_moved_line) FROM rooms`).Scan(&moved); err != nil || moved != "tts_linesrate_events1" {
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
	// gone never connected; stale's only connection expired: the sweep drops both statuses.
	var gone, stale *Daemon
	for _, x := range []struct {
		d    **Daemon
		name string
	}{{&gone, "gone"}, {&stale, "stale"}} {
		q, _, err := s.Join(ctx, "inv-r", x.name, "tok-"+x.name, JoinProfile{})
		if err != nil {
			t.Fatal(err)
		}
		if *x.d, err = s.PairDaemon(ctx, "r", q.ID, "dmn-"+x.name); err != nil {
			t.Fatal(err)
		}
	}
	const now, cutoff = int64(10_000), int64(5_000)
	err = s.db.unlocked(ctx, func(tx *txn) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO daemon_connections(id, daemon_id, room_id, pod, connected_at_ms, expires_at_ms) VALUES('stale', ?, 'r', 'p', 0, ?)`, stale.ID, now); err != nil {
			return err
		}
		for _, id := range []int64{d.ID, gone.ID, stale.ID} {
			if _, err := tx.ExecContext(ctx, `INSERT INTO daemon_status(daemon_id) VALUES(?)`, id); err != nil {
				return err
			}
		}
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
	left, err := s.SweepExpired(ctx, now, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	// Only stale lost its last live connection: d keeps one, gone had none to lose.
	if len(left) != 1 || left[0].ID != stale.ID || left[0].RoomID != "r" || left[0].ParticipantID != stale.ParticipantID {
		t.Errorf("gone daemons = %+v, want only %d", left, stale.ID)
	}
	if again, err := s.SweepExpired(ctx, now, cutoff); err != nil || len(again) != 0 {
		t.Errorf("second sweep: %+v %v", again, err)
	}
	var n, kept int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*), max(daemon_id) FROM daemon_status`).Scan(&n, &kept); err != nil || n != 1 || kept != d.ID {
		t.Errorf("daemon_status: %d rows, left %d, %v; want only %d", n, kept, err, d.ID)
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
		`INSERT INTO rate_events(scope, key_hash, at_ms) VALUES('login', 'k', 1)`,
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

// The conditional inserts behind the pairing, tab and STT worker caps admit exactly the cap when
// requests race from two stores (pods) at once.
func TestCapsExactUnderConcurrency(t *testing.T) {
	dsn := testDSN(t)
	pods := []*Store{openAt(t, dsn), openAt(t, dsn)}
	ctx := t.Context()
	if _, err := pods[0].CreateRoom(ctx, "r", "R"); err != nil {
		t.Fatal(err)
	}
	if _, err := pods[0].CreateInvite(ctx, "r", "inv-r", 0); err != nil {
		t.Fatal(err)
	}
	p, _, err := pods[0].Join(ctx, "inv-r", "Alice", "tok-a", JoinProfile{})
	if err != nil {
		t.Fatal(err)
	}
	q, _, err := pods[0].Join(ctx, "inv-r", "Bob", "tok-b", JoinProfile{})
	if err != nil {
		t.Fatal(err)
	}
	const capN, tries, now = 3, 20, int64(1_000_000)
	at := PresenceTimes{Now: now, Grace: 20_000, Forget: 3_600_000, Lapse: 3_000}
	for name, try := range map[string]func(st *Store, i int) (bool, error){
		"pairings": func(st *Store, i int) (bool, error) {
			return st.CreatePairing(ctx, &Pairing{ID: fmt.Sprint("p", i), RoomID: "r", ExpiresAt: time.UnixMilli(now + 60_000)}, capN, 100, now)
		},
		"tabs": func(st *Store, i int) (bool, error) {
			return st.PresenceBeat(ctx, "r", p.ID, fmt.Sprint("t", i), TabState{}, false, capN, at)
		},
		"device tabs": func(st *Store, i int) (bool, error) {
			return st.PresenceDevice(ctx, "r", q.ID, fmt.Sprint("d", i), true, capN, at)
		},
		"stt workers": func(st *Store, i int) (bool, error) {
			return st.BeatSTTWorker(ctx, STTWorker{Name: fmt.Sprint("w", i), ExpiresMS: now + 60_000}, capN, now)
		},
	} {
		var wg sync.WaitGroup
		var passed atomic.Int32
		for i := range tries {
			wg.Go(func() {
				ok, err := try(pods[i%2], i)
				if err != nil {
					t.Error(err)
				}
				if ok {
					passed.Add(1)
				}
			})
		}
		wg.Wait()
		if n := passed.Load(); n != capN {
			t.Errorf("%s: %d of %d racing inserts passed a cap of %d", name, n, tries, capN)
		}
	}
}

// Two unlocked transactions taking two rows in opposite orders deadlock: PostgreSQL aborts one,
// which runs again and commits.
func TestUnlockedRetriesDeadlock(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	for _, k := range []string{"dl-a", "dl-b"} {
		if err := s.SetSetting(ctx, k, "0"); err != nil {
			t.Fatal(err)
		}
	}
	var both sync.WaitGroup
	both.Add(2)
	var runs atomic.Int32
	write := func(first, second string) error {
		var once sync.Once
		return s.db.unlocked(ctx, func(tx *txn) error {
			runs.Add(1)
			if _, err := tx.ExecContext(ctx, `UPDATE settings SET value = ? WHERE key = ?`, first, first); err != nil {
				return err
			}
			// Each holds its first row before either asks for its second.
			once.Do(func() { both.Done(); both.Wait() })
			_, err := tx.ExecContext(ctx, `UPDATE settings SET value = ? WHERE key = ?`, first, second)
			return err
		})
	}
	errs := make(chan error, 2)
	go func() { errs <- write("dl-a", "dl-b") }()
	go func() { errs <- write("dl-b", "dl-a") }()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("a deadlocked transaction failed: %v", err)
		}
	}
	if n := runs.Load(); n != 3 {
		t.Errorf("%d runs, want 3: one deadlock victim run again", n)
	}
}

// unlocked runs again only a transaction PostgreSQL aborted for a deadlock or a serialization
// failure, at most unlockedTries times.
func TestUnlockedRetryBounds(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	for _, c := range []struct {
		err  error
		runs int
	}{
		{&pgconn.PgError{Code: "40P01"}, unlockedTries},
		{fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "40001"}), unlockedTries},
		{&pgconn.PgError{Code: "23505"}, 1},
		{errors.New("plain"), 1},
	} {
		runs := 0
		err := s.db.unlocked(ctx, func(*txn) error { runs++; return c.err })
		if !errors.Is(err, c.err) || runs != c.runs {
			t.Errorf("%v: %d runs, returned %v; want %d runs", c.err, runs, err, c.runs)
		}
	}
}

// The sweep deletes expired presence past one batch, and keeps the live rows.
func TestSweepPresenceBatches(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	p := seed(t, s, "a")
	const now = int64(10_000)
	err := s.db.unlocked(ctx, func(tx *txn) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO presence(room_id, participant_id, tab, expires_at_ms)
			SELECT 'a', ?, 't' || i, CASE WHEN i % 10 = 0 THEN ? + 1 ELSE ? END FROM generate_series(1, ?) i`, p.ID, now, now, 2*sweepBatch+5)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SweepExpired(ctx, now, 0); err != nil {
		t.Fatal(err)
	}
	var left, live int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*), count(*) FILTER (WHERE expires_at_ms > ?) FROM presence`, now).Scan(&left, &live); err != nil {
		t.Fatal(err)
	}
	if left != (2*sweepBatch+5)/10 || live != left {
		t.Errorf("%d rows left, %d live; want the %d live ones alone", left, live, (2*sweepBatch+5)/10)
	}
}

// Every shared-state call fails with its own context once the database is gone: the callers log
// or answer 500 with it.
func TestSharedStateErrors(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	_ = s.Close()
	at := presenceTimes(1)
	d := &Daemon{ID: 1, RoomID: "r"}
	for want, call := range map[string]func() error{
		"presence open":           func() error { return s.PresenceOpen(ctx, "r", 1, "t", at) },
		"presence release":        func() error { return s.PresenceRelease(ctx, "r", 1, "t", at) },
		"presence beat":           func() error { _, err := s.PresenceBeat(ctx, "r", 1, "t", TabState{}, false, 1, at); return err },
		"presence device":         func() error { _, err := s.PresenceDevice(ctx, "r", 1, "t", true, 1, at); return err },
		"presence:":               func() error { _, err := s.Presence(ctx, "r", 1); return err },
		"stt worker:":             func() error { _, err := s.BeatSTTWorker(ctx, STTWorker{Name: "w"}, 1, 1); return err },
		"stt workers:":            func() error { _, err := s.STTWorkers(ctx, 1); return err },
		"daemon connected":        func() error { return s.DaemonConnected(ctx, "c", d, "p", 1, 2) },
		"daemon disconnected":     func() error { _, err := s.DaemonDisconnected(ctx, "c", 1, 1); return err },
		"drop daemon connections": func() error { return s.DropDaemonConnections(ctx, 1) },
		"set daemon status":       func() error { _, err := s.SetDaemonStatus(ctx, 1, DaemonStatus{}); return err },
		"live daemons":            func() error { _, err := s.LiveDaemons(ctx, "r", 1); return err },
		"elect host":              func() error { _, err := s.ElectHost(ctx, "r", 1); return err },
		"create pairing":          func() error { _, err := s.CreatePairing(ctx, &Pairing{ID: "p", RoomID: "r"}, 1, 1, 1); return err },
		"pending pairing":         func() error { _, err := s.PendingPairing(ctx, "r", "p", 1); return err },
		"approve pairing":         func() error { return s.ApprovePairing(ctx, "r", "p", 1, 1) },
		"deny pairing":            func() error { return s.DenyPairing(ctx, "r", "p", 1) },
		"poll pairing":            func() error { _, _, err := s.PollPairing(ctx, "p", "s", "t", 1); return err },
		"rate hit":                func() error { _, _, err := s.RateHit(ctx, "login", "k", 1, 1, 0, 1); return err },
		"rate unhit":              func() error { return s.RateUnhit(ctx, 1) },
		"rate reset":              func() error { return s.RateReset(ctx, "login", "k") },
		"sweep expired":           func() error { _, err := s.SweepExpired(ctx, 1, 1); return err },
		"add tts line":            func() error { _, _, err := s.AddTTSLine(ctx, ttsLine("r", 1), 1, 1); return err },
		"sweep tts lines":         func() error { _, err := s.SweepTTSLines(ctx, 1); return err },
		"tts lines:":              func() error { _, err := s.TTSLinesAfter(ctx, 0, 1); return err },
		"tts cursor":              func() error { _, _, err := s.TTSCursor(ctx); return err },
		"tts heartbeat":           func() error { return s.BeatTTSPod(ctx, TTSPod{}, 1) },
		"tts stream lease":        func() error { return s.RenewTTSStream(ctx, 1) },
		"tts stream release":      func() error { return s.ReleaseTTSStream(ctx, 1) },
		"tts pod:":                func() error { _, err := s.TTSPodState(ctx); return err },
		"speaking rooms":          func() error { _, err := s.SpeakingRooms(ctx, []string{"r"}); return err },
		"default voices": func() error {
			_, err := s.DefaultVoices(ctx, "r", func(map[string]bool) string { return "" })
			return err
		},
		"post voice line": func() error { _, err := s.PostVoiceLine(ctx, &Message{}, time.Now(), time.Second, nil); return err },
	} {
		if err := call(); err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
}

// The lock kinds are a contract between pods of two versions during a rolling update: a value
// that moved would let two pods in one critical section.
func TestLockKindValues(t *testing.T) {
	for name, c := range map[string]struct{ got, want int32 }{
		"LockTurns": {LockTurns, 1}, "LockPlugin": {LockPlugin, 2}, "LockRate": {LockRate, 3}, "LockDaemon": {LockDaemon, 4},
		"LockPairing": {LockPairing, 5}, "LockPresence": {LockPresence, 6}, "LockSTTWorkers": {LockSTTWorkers, 7}, "LockLeader": {LockLeader, 8},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", name, c.got, c.want)
		}
	}
}

// 017-AC17: of two stores campaigning on one database, one leads. Once its lock session is
// terminated it learns it lost and campaigns again on a new session; one of the two takes the lock
// and leaves it when its context ends, to the other.
func TestLeader(t *testing.T) {
	dsn := testDSN(t)
	type campaign struct {
		app     string
		changes chan bool
		errs    chan error
		cancel  func()
	}
	run := func(pod string) *campaign {
		s := openAt(t, dsn)
		ctx, cancel := context.WithCancel(t.Context())
		c := &campaign{app: LeaderApplication + " " + pod, changes: make(chan bool, 8), errs: make(chan error, 8), cancel: cancel}
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.Leader(ctx, pod, 20*time.Millisecond, func(b bool) { c.changes <- b }, func(err error) { c.errs <- err })
		}()
		t.Cleanup(func() { cancel(); <-done })
		return c
	}
	next := func(c *campaign, want bool) {
		t.Helper()
		select {
		case got := <-c.changes:
			if got != want {
				t.Fatalf("%s: leader %v, want %v", c.app, got, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: no change to %v within 10 s", c.app, want)
		}
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	holder := func() string {
		t.Helper()
		var app string
		if err := db.QueryRowContext(t.Context(), `SELECT a.application_name FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
			WHERE l.locktype = 'advisory' AND l.classid = $1 AND l.objid = 0 AND l.objsubid = 2 AND l.granted
			AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())`, LockLeader).Scan(&app); err != nil {
			t.Fatal(err)
		}
		return app
	}
	a := run("pod-a")
	next(a, true)
	b := run("pod-b")
	if h := holder(); h != a.app {
		t.Fatalf("lock held by %q", h)
	}
	if _, err := db.ExecContext(t.Context(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE application_name = $1 AND datname = current_database()`, a.app); err != nil {
		t.Fatal(err)
	}
	next(a, false)
	if err := <-a.errs; err == nil {
		t.Fatal("no error for the lost session")
	}
	lead, other := a, b
	select {
	case <-a.changes:
	case <-b.changes:
		lead, other = b, a
	case <-time.After(10 * time.Second):
		t.Fatal("no leader within 10 s of the terminate")
	}
	if h := holder(); h != lead.app {
		t.Fatalf("%s leads, the lock is %q's", lead.app, h)
	}
	lead.cancel()
	next(lead, false)
	next(other, true)
	if h := holder(); h != other.app {
		t.Fatalf("%s leads, the lock is %q's", other.app, h)
	}
}
