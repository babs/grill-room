package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/babs/grill-room/internal/pgtest"
)

// snapshot lists what Open made in the database at dsn: tables, columns, indexes, constraints,
// triggers, functions, the schema version and each table's row count.
func snapshot(t *testing.T, dsn string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var out []string
	lines := func(q string) {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var a, b, c sql.NullString
			if err := rows.Scan(&a, &b, &c); err != nil {
				t.Fatal(err)
			}
			out = append(out, a.String+" | "+b.String+" | "+c.String)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	lines(`SELECT table_name || '.' || column_name, data_type || ' ' || is_nullable || ' ' || is_identity || ' ' || is_generated,
		COALESCE(column_default, '') || COALESCE(generation_expression, '') FROM information_schema.columns WHERE table_schema = 'public' ORDER BY 1`)
	lines(`SELECT tablename, indexname, indexdef FROM pg_indexes WHERE schemaname = 'public' ORDER BY 2`)
	lines(`SELECT conrelid::regclass::text, conname, pg_get_constraintdef(oid) FROM pg_constraint WHERE connamespace = 'public'::regnamespace ORDER BY 1, 2`)
	lines(`SELECT tgrelid::regclass::text, tgname, pg_get_triggerdef(oid) FROM pg_trigger WHERE NOT tgisinternal ORDER BY 1, 2`)
	lines(`SELECT proname, '', prosrc FROM pg_proc WHERE pronamespace = 'public'::regnamespace ORDER BY 1`)
	lines(`SELECT 'schema_version', '', version::text FROM schema_version`)
	var tables []string
	rows, err := db.QueryContext(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	_ = rows.Close()
	for _, tb := range tables {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM "`+tb+`"`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("rows %s %d", tb, n))
	}
	return out
}

func diff(a, b []string) string {
	var d []string
	for _, l := range a {
		if !slices.Contains(b, l) {
			d = append(d, "- "+l)
		}
	}
	for _, l := range b {
		if !slices.Contains(a, l) {
			d = append(d, "+ "+l)
		}
	}
	return strings.Join(d, "\n")
}

// 017-AC1: two servers opening one empty database at once make the schema and the boot data once,
// as one server does; opening it again changes nothing.
func TestConcurrentOpensMakeOneSchema(t *testing.T) {
	ref := pgtest.Fresh(t)
	s, err := Open(ref)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	want := snapshot(t, ref)
	if !slices.Contains(want, "schema_version |  | 46") || !slices.Contains(want, "rows pronunciations 129") || !slices.Contains(want, "rows starters 3") {
		t.Fatalf("a single open made:\n%s", strings.Join(want, "\n"))
	}

	dsn := pgtest.Fresh(t)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() {
			s, err := Open(dsn)
			if err == nil {
				err = s.Close()
			}
			errs[i] = err
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, dsn); !slices.Equal(got, want) {
		t.Fatalf("two opens at once differ from one:\n%s", diff(want, got))
	}
	if s, err = Open(dsn); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if got := snapshot(t, dsn); !slices.Equal(got, want) {
		t.Fatalf("a third open changed:\n%s", diff(want, got))
	}
}

// 017-AC5: no column is a 32-bit integer, and the columns that were BLOB in the SQLite schema are
// bytea, and only they.
func TestColumnTypes(t *testing.T) {
	s := openTest(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cols := func(dataType string) []string {
		t.Helper()
		rows, err := s.db.QueryContext(ctx, `SELECT table_name || '.' || column_name FROM information_schema.columns
			WHERE table_schema = 'public' AND data_type = ? ORDER BY 1`, dataType)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := []string{}
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				t.Fatal(err)
			}
			out = append(out, c)
		}
		return out
	}
	if got := cols("integer"); len(got) != 0 {
		t.Errorf("integer columns: %v", got)
	}
	blobs := []string{"agent_sessions.data", "attachments.data", "plugin_snapshots.data", "skill_versions.data", "starter_files.data",
		"tts_samples.data", "uploads.data"}
	if got := cols("bytea"); !slices.Equal(got, blobs) {
		t.Errorf("bytea columns %v, want %v", got, blobs)
	}
}

// 017-AC6: a database that does not sort as C is refused, with the way to make one that does: a
// libc collation or ctype other than C, or an ICU one whatever its LC_COLLATE.
func TestOpenRefusesNonCCollation(t *testing.T) {
	if CreateDatabaseOptions != pgtest.CreateOptions {
		t.Fatalf("pgtest makes databases with %q, the server asks for %q", pgtest.CreateOptions, CreateDatabaseOptions)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	probe, err := sql.Open("pgx", pgtest.Fresh(t))
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	var icu bool
	if err := probe.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_collation WHERE collprovider = 'i')`).Scan(&icu); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"libc en_US":      "LC_COLLATE 'en_US.UTF-8' LC_CTYPE 'en_US.UTF-8' LOCALE_PROVIDER libc",
		"libc ctype only": "LC_COLLATE 'C' LC_CTYPE 'en_US.UTF-8' LOCALE_PROVIDER libc",
	}
	if icu {
		cases["icu und"] = "LC_COLLATE 'C' LC_CTYPE 'C' LOCALE_PROVIDER icu ICU_LOCALE 'und'"
	}
	for name, opts := range cases {
		dsn := pgtest.Create(t, "TEMPLATE template0 ENCODING 'UTF8' "+opts)
		s, err := Open(dsn)
		if err == nil {
			_ = s.Close()
			t.Errorf("%s: opened", name)
			continue
		}
		if !strings.Contains(err.Error(), "CREATE DATABASE \"grtest_") || !strings.Contains(err.Error(), CreateDatabaseOptions) {
			t.Errorf("%s: the error does not name the fix: %v", name, err)
		}
	}
}

// holdWriteLock takes the write lock in a session of its own, as a pod stuck mid-write would,
// until release.
func holdWriteLock(t *testing.T, dsn string) (raw *sql.DB, release func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	// Not ctx: database/sql rolls a transaction back when its context ends.
	tx, err := raw.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, WriteLockKey); err != nil {
		t.Fatal(err)
	}
	done := false
	release = func() {
		if !done {
			done = true
			_ = tx.Rollback()
		}
	}
	t.Cleanup(func() { release(); _ = raw.Close() })
	return raw, release
}

// waitLockWaiters waits until n sessions wait for the write lock.
func waitLockWaiters(t *testing.T, raw *sql.DB, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var got int
		if err := raw.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND objid::bigint = $1 AND NOT granted`,
			WriteLockKey).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got >= n {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%d sessions wait for the write lock, want %d", got, n)
		case <-tick.C:
		}
	}
}

// isLockTimeout reports PostgreSQL's lock_timeout error.
func isLockTimeout(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "55P03"
}

// A session that holds the write lock and never lets go fails the writes and the boots that wait
// for it after lock_timeout, instead of blocking them for good; a transaction left idle ends.
func TestLockTimeout(t *testing.T) {
	dsn := testDSN(t)
	var waits atomic.Int32
	s, err := Open(dsn, withLockTimeout(time.Second), WithLockWait(func(time.Duration) { waits.Add(1) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	for q, want := range map[string]string{"lock_timeout": "1s", "idle_in_transaction_session_timeout": "1min"} {
		var got string
		if err := s.db.QueryRowContext(ctx, `SELECT current_setting(?)`, q).Scan(&got); err != nil || got != want {
			t.Errorf("%s = %q %v, want %q", q, got, err, want)
		}
	}
	holdWriteLock(t, dsn)
	waits.Store(0) // Open's boot took the lock once
	if err := s.SetSetting(ctx, "k", "v"); !isLockTimeout(err) {
		t.Fatalf("write: %v, want a lock timeout", err)
	}
	// A writer queued on the writer slot, not on the advisory lock, gets the same bound.
	s.db.writer <- struct{}{}
	err = s.SetSetting(ctx, "k", "v")
	<-s.db.writer
	if !isLockTimeout(err) || !strings.Contains(err.Error(), "writer slot") {
		t.Fatalf("queued write: %v, want the writer slot's lock timeout", err)
	}
	// Failed waits are measured too: they are the longest ones.
	if n := waits.Load(); n != 2 {
		t.Errorf("lock waits measured: %d, want 2 (advisory lock timeout, writer slot timeout)", n)
	}
	if s2, err := Open(dsn, withLockTimeout(time.Second)); !isLockTimeout(err) {
		if err == nil {
			_ = s2.Close()
		}
		t.Fatalf("boot: %v, want a lock timeout", err)
	}
}

// Writers queued on the write lock wait outside the pool: reads and the health probe, which skips
// the lock, still get a connection (017-AC22).
func TestQueuedWritersLeaveReadsAConnection(t *testing.T) {
	dsn := testDSN(t)
	s, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	raw, release := holdWriteLock(t, dsn)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	const n = 20
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { errs[i] = s.SetSetting(ctx, fmt.Sprint("k", i), "v") })
	}
	waitLockWaiters(t, raw, 1)
	quick, cancelQuick := context.WithTimeout(ctx, 10*time.Second)
	defer cancelQuick()
	if _, err := s.Room(quick, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("read: %v", err)
	}
	if err := s.Writable(quick); err != nil {
		t.Errorf("writable: %v", err)
	}
	release()
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
}

// Open refuses a schema newer than the server, and runs each migration past the stored version
// once.
func TestMigrate(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	version := func(dsn string) (v int) {
		t.Helper()
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if err := db.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	newer := testDSN(t)
	s := openAt(t, newer)
	if _, err := s.db.ExecContext(ctx, `UPDATE schema_version SET version = 99`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if s, err := Open(newer); err == nil || !strings.Contains(err.Error(), "schema version 99 is newer than this server's 46") {
		if err == nil {
			_ = s.Close()
		}
		t.Fatalf("a newer schema: %v", err)
	}

	defer func(m []string) { migrations = m }(migrations)
	migrations = []string{`CREATE TABLE migrated (id BIGINT)`, `INSERT INTO migrated VALUES (1)`}
	dsn := testDSN(t)
	for range 2 {
		_ = openAt(t, dsn).Close()
		if v := version(dsn); v != baselineVersion+2 {
			t.Fatalf("version %d after the migrations", v)
		}
	}
	s = openAt(t, dsn)
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM migrated`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the migrations ran %d times: %v", n, err)
	}
}

// An export reads a snapshot without the write lock, so writes go on meanwhile; only its audit
// entry, which takes an id, waits for the lock.
func TestExportSkipsTheWriteLock(t *testing.T) {
	dsn := testDSN(t)
	s := openAt(t, dsn)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	seed(t, s, "r")
	raw, release := holdWriteLock(t, dsn)
	if _, err := s.ExportRoom(ctx, "r", t.TempDir()); err != nil {
		t.Fatalf("an export without audit: %v", err)
	}
	dir := t.TempDir()
	audited := WithAudit(ctx, AuditEntry{Actor: ActorSuperAdmin, Action: "room.export", TargetKind: TargetRoom, TargetID: "r"}, nil)
	done := make(chan error, 1)
	go func() { _, err := s.ExportRoom(audited, "r", dir); done <- err }()
	waitLockWaiters(t, raw, 1)
	if _, err := os.Stat(filepath.Join(dir, fileMessages)); err != nil {
		t.Fatalf("the export waits for the lock before its read: %v", err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_log WHERE action = 'room.export'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d audit entries: %v", n, err)
	}
}

func openAt(t *testing.T, dsn string) *Store {
	t.Helper()
	s, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// 017-AC4: two servers on one database keep what the single SQLite connection kept: one change
// seq per edit, the persona limit, and ids that commit in order, so an "id > last" cursor misses
// none.
func TestTwoStoresKeepTheInvariants(t *testing.T) {
	dsn := testDSN(t)
	stores := [2]*Store{}
	for i := range stores {
		s, err := Open(dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		stores[i] = s
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	a, b := stores[0], stores[1]
	p := joinWithProfile(t, a, "r", "Alice", JoinProfile{})

	t.Run("edits", func(t *testing.T) {
		const n = 50
		ids := make([]int64, n)
		for i := range ids {
			m := &Message{RoomID: "r", Kind: KindText, ParticipantID: p.ID, Sender: p.Name, Body: fmt.Sprint("line ", i)}
			if err := a.InsertMessage(ctx, m); err != nil {
				t.Fatal(err)
			}
			ids[i] = m.ID
		}
		seqs := make([]int64, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i, id := range ids {
			wg.Go(func() {
				m, _, err := stores[i%2].EditMessage(ctx, "r", id, p.ID, fmt.Sprint("edited ", i))
				if err == nil {
					seqs[i] = m.ChangeSeq
				}
				errs[i] = err
			})
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			t.Fatal(err)
		}
		slices.Sort(seqs)
		if seqs = slices.Compact(seqs); len(seqs) != n {
			t.Fatalf("%d distinct change seqs for %d edits", len(seqs), n)
		}
	})

	t.Run("persona limit", func(t *testing.T) {
		tpl := templateByPreset(t, a, "secretary").ID
		var wg sync.WaitGroup
		errs := make([]error, 2*MaxRoomPersonas)
		for i := range errs {
			wg.Go(func() { _, _, errs[i] = stores[i%2].AddPersona(ctx, "r", tpl, p) })
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil && !errors.Is(err, ErrLimit) {
				t.Fatal(err)
			}
		}
		if ps, err := b.RoomPersonas(ctx, "r"); err != nil || len(ps) != MaxRoomPersonas {
			t.Fatalf("%d personas, limit %d: %v", len(ps), MaxRoomPersonas, err)
		}
	})

	t.Run("insert order", func(t *testing.T) {
		const n = 200
		q := joinWithProfile(t, a, "s", "Bob", JoinProfile{})
		// The join line is in the room already: the cursor starts past it.
		start, err := b.Messages(ctx, "s", 0, 10)
		if err != nil || len(start) != 1 {
			t.Fatalf("join line: %v %v", start, err)
		}
		last := start[0].ID
		inserted := make([]int64, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range inserted {
			wg.Go(func() {
				m := &Message{RoomID: "s", Kind: KindText, ParticipantID: q.ID, Sender: q.Name, Body: fmt.Sprint("line ", i)}
				errs[i] = stores[i%2].InsertMessage(ctx, m)
				inserted[i] = m.ID
			})
		}
		writersDone := make(chan struct{})
		go func() { wg.Wait(); close(writersDone) }()
		var seen []int64
		for done := false; ; {
			select {
			case <-writersDone:
				done = true
			default:
			}
			page, err := b.Messages(ctx, "s", last, 1000)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range page {
				seen = append(seen, m.ID)
				last = m.ID
			}
			// A poll that starts once every writer has committed reads the rest.
			if done && len(page) == 0 {
				break
			}
		}
		if err := errors.Join(errs...); err != nil {
			t.Fatal(err)
		}
		slices.Sort(inserted)
		if !slices.IsSorted(seen) || !slices.Equal(seen, inserted) {
			t.Fatalf("the cursor saw %d lines of %d, sorted %v", len(seen), n, slices.IsSorted(seen))
		}
	})
}

// 005-AC9: the schema refuses a persona or a template version without a model or an effort, as
// the SQLite triggers did.
func TestPersonaLevelTriggers(t *testing.T) {
	s := openTest(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if _, err := s.CreateRoom(ctx, "r", "R"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE room_personas SET model = '' WHERE room_id = 'r'`,
		`UPDATE room_personas SET effort = '' WHERE room_id = 'r'`,
		`INSERT INTO room_personas(room_id, template_version, name, badge, colour, text, model, effort, created_at)
			VALUES('r', 1, 'X', 'x', '#000000', '', '', 'medium', '')`,
		`INSERT INTO persona_template_versions(template_id, n, author, name, badge, colour, text, model, effort, created_at)
			SELECT id, 99, '', 'X', 'x', '#000000', '', 'sonnet', '', '' FROM persona_templates LIMIT 1`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err == nil || !strings.Contains(err.Error(), "a persona needs a model and an effort") {
			t.Errorf("%s: %v", q, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE room_personas SET name = 'Renamed' WHERE room_id = 'r'`); err != nil {
		t.Fatalf("an update of another column: %v", err)
	}
}

// A burst of reads, as an "agent" event waking every page of a room, keeps its connections
// for the next one instead of closing all but two and authenticating again.
func TestPoolKeepsIdleConnections(t *testing.T) {
	s := openTest(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	const n = 8
	var conns []*sql.Conn
	for range n {
		c, err := s.db.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.PingContext(ctx); err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if st := s.db.db.Stats(); st.Idle < n || st.MaxIdleClosed != 0 {
		t.Fatalf("after %d concurrent reads: %d idle, %d closed as over the idle limit", n, st.Idle, st.MaxIdleClosed)
	}
}

// The lab manifest keeps credentials out of GRILL_ROOM_DATABASE_URL and passes them as PGUSER and
// PGPASSWORD from a secret (deploy/k8s/room.yaml): pgx must read them for a URL that has none.
func TestOpenTakesCredentialsFromEnv(t *testing.T) {
	u, err := url.Parse(pgtest.Fresh(t))
	if err != nil {
		t.Fatal(err)
	}
	user, pass := u.User.Username(), ""
	if p, ok := u.User.Password(); ok {
		pass = p
	}
	if user == "" {
		t.Skip(pgtest.Env + " names no user to move into PGUSER")
	}
	u.User = nil
	t.Setenv("PGUSER", user)
	t.Setenv("PGPASSWORD", pass)
	cfg, err := pgconn.ParseConfig(u.String())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.User != user || cfg.Password != pass {
		t.Fatalf("ParseConfig(%s) took user %q and a %d-byte password, want %q from PGUSER and PGPASSWORD", u.Redacted(), cfg.User, len(cfg.Password), user)
	}
	openAt(t, u.String())
}
