// Package dbcopy copies the database of the last SQLite server into an empty PostgreSQL database,
// once, and compares the two table by table (spec 017, 017-AC7, 017-AC8).
package dbcopy

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite" // the source is the SQLite server's file

	"github.com/babs/grill-room/internal/store"
)

// sourceVersion is the SQLite schema version store's PostgreSQL baseline reproduces.
const sourceVersion = 46

// sinceSource is what the store's migrations added past sourceVersion, which no source holds:
// verify skips it on a target the server has since migrated. A table maps to nil, else to its
// added columns; addedRows filters out the rows a migration wrote.
var (
	sinceSource = map[string][]string{
		"daemon_connections": nil, "daemon_status": nil, "presence": nil, "stt_workers": nil, "tts_pod": nil, "pairings": nil,
		"tts_lines": nil, "rate_events": nil, "rooms": {"host_moved_line"}, "turns": {"delivered"},
	}
	addedRows = map[string]string{"settings": `key <> 'lexicon_version'`}
)

// beforeVerify lets a test change the target between the inserts and the check.
var beforeVerify func(ctx context.Context, tx *sql.Tx) error

type column struct {
	name, typ string
	notNull   bool
	generated bool
	identity  bool
}

type foreignKey struct {
	cols   []string
	parent string
}

type table struct {
	name string
	cols []column
	pk   []string
	fks  []foreignKey
}

// result is one table on both sides.
type result struct {
	table            string
	srcRows, dstRows int64
	srcSum, dstSum   []byte
	// sanitized counts text values that held NUL or invalid UTF-8; badTS timestamps left as they
	// were because they do not parse.
	sanitized, badTS int64
}

// digest folds a table's rows into one sha256. A table with a text key part is sorted in Go on
// the converted key: SQLite orders raw bytes, PostgreSQL the sanitized ones.
type digest struct {
	h      hash.Hash
	sorted bool
	keys   []int
	rows   []keyedRow
}

type keyedRow struct{ key, sum []byte }

func newDigest(t *table) *digest {
	d := &digest{h: sha256.New()}
	for j, c := range t.cols {
		if len(t.pk) == 0 {
			// Without a key the whole row is the order: its own hash sorts it.
			d.sorted = d.sorted || c.typ == "text"
		} else if slices.Contains(t.pk, c.name) {
			d.keys = append(d.keys, j)
			d.sorted = d.sorted || c.typ == "text"
		}
	}
	return d
}

func (d *digest) add(vals []any) error {
	if !d.sorted {
		return encode(d.h, vals)
	}
	var key bytes.Buffer
	keyVals := make([]any, len(d.keys))
	for i, j := range d.keys {
		keyVals[i] = vals[j]
	}
	if err := encode(&key, keyVals); err != nil {
		return err
	}
	h := sha256.New()
	if err := encode(h, vals); err != nil {
		return err
	}
	d.rows = append(d.rows, keyedRow{key: key.Bytes(), sum: h.Sum(nil)})
	return nil
}

func (d *digest) sum() []byte {
	slices.SortFunc(d.rows, func(a, b keyedRow) int {
		return cmp.Or(bytes.Compare(a.key, b.key), bytes.Compare(a.sum, b.sum))
	})
	for _, r := range d.rows {
		_, _ = d.h.Write(r.sum)
	}
	return d.h.Sum(nil)
}

func (r *result) ok() bool { return r.srcRows == r.dstRows && bytes.Equal(r.srcSum, r.dstSum) }

// Copy copies the SQLite database at from into the empty PostgreSQL database at dsn in one
// transaction holding the store's write lock, and commits only when every table compares equal;
// it writes the comparison to out.
func Copy(ctx context.Context, from, dsn string, out io.Writer) error {
	dst, err := openTarget(dsn)
	if err != nil {
		return err
	}
	defer dst.Close() //nolint:errcheck // the transaction's outcome is checked
	if err := store.CheckCollation(ctx, dst); err != nil {
		return fmt.Errorf("target: %w", err)
	}
	src, err := openSource(ctx, from)
	if err != nil {
		return err
	}
	defer src.Close() //nolint:errcheck // read-only
	tx, err := dst.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, store.WriteLockKey); err != nil {
		return fmt.Errorf("target: write lock: %w", err)
	}
	if err := requireEmpty(ctx, tx); err != nil {
		return err
	}
	if err := store.CreateBaseline(ctx, tx); err != nil {
		return fmt.Errorf("target: %w", err)
	}
	tables, err := loadSchema(ctx, tx)
	if err != nil {
		return err
	}
	if err := checkShape(ctx, src, tables); err != nil {
		return err
	}
	results, err := copyRows(ctx, src, tx, tables)
	if err != nil {
		return err
	}
	if beforeVerify != nil {
		if err := beforeVerify(ctx, tx); err != nil {
			return err
		}
	}
	for i, t := range tables {
		if err := targetSum(ctx, tx, t, results[i]); err != nil {
			return err
		}
	}
	if err := report(out, results); err != nil {
		return fmt.Errorf("%w; nothing committed", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("target: commit: %w", err)
	}
	return nil
}

// Verify compares the SQLite database at from with the PostgreSQL database at dsn, table by
// table, as Copy does before it commits, and writes the comparison to out.
func Verify(ctx context.Context, from, dsn string, out io.Writer) error {
	dst, err := openTarget(dsn)
	if err != nil {
		return err
	}
	defer dst.Close() //nolint:errcheck // read-only
	src, err := openSource(ctx, from)
	if err != nil {
		return err
	}
	defer src.Close() //nolint:errcheck // read-only
	// One snapshot for every table.
	tx, err := dst.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only
	tables, err := loadSchema(ctx, tx)
	if err != nil {
		return err
	}
	if err := checkShape(ctx, src, tables); err != nil {
		return err
	}
	results := make([]*result, len(tables))
	for i, t := range tables {
		results[i] = &result{table: t.name}
		if err := readSource(ctx, src, t, results[i], nil); err != nil {
			return err
		}
		if err := targetSum(ctx, tx, t, results[i]); err != nil {
			return err
		}
	}
	return report(out, results)
}

func openSource(ctx context.Context, path string) (*sql.DB, error) {
	// A read-only open ignores what a WAL holds past the last checkpoint.
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() > 0 {
		return nil, fmt.Errorf("source: %s-wal is not empty: copy from a consistent backup taken with the server stopped", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	// immutable: a read-only open of a WAL-mode file otherwise creates -shm, which a read-only
	// mount refuses; the empty WAL checked above leaves nothing outside the main file.
	u := url.URL{Scheme: "file", Path: "/" + strings.TrimPrefix(filepath.ToSlash(abs), "/"), RawQuery: "mode=ro&immutable=1&_pragma=query_only(1)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	var v int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("source: %w", err)
	}
	if v != sourceVersion {
		_ = db.Close()
		return nil, fmt.Errorf("source: schema version %d, the copy reads version %d only", v, sourceVersion)
	}
	return db, nil
}

func openTarget(dsn string) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		// pgx's message quotes the DSN.
		return nil, errors.New("target: the DSN does not parse")
	}
	// A server still running holds the write lock: fail rather than wait for good.
	cfg.RuntimeParams["lock_timeout"] = "30000"
	return stdlib.OpenDB(*cfg), nil
}

func requireEmpty(ctx context.Context, tx *sql.Tx) error {
	names, err := strings1(ctx, tx, `SELECT relname FROM pg_class WHERE relnamespace = current_schema()::regnamespace ORDER BY relname LIMIT 5`)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	if len(names) > 0 {
		return fmt.Errorf("target is not empty: its schema holds %s: copy into a new database", strings.Join(names, ", "))
	}
	return nil
}

// strings1 runs a query of one text column.
func strings1(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// loadSchema reads the target's tables in creation order, but schema_version, which the copy
// sets rather than copies.
func loadSchema(ctx context.Context, tx *sql.Tx) ([]*table, error) {
	rows, err := tx.QueryContext(ctx, `SELECT c.relname, a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull,
			a.attgenerated <> '', a.attidentity <> ''
		FROM pg_class c JOIN pg_attribute a ON a.attrelid = c.oid
		WHERE c.relnamespace = current_schema()::regnamespace AND c.relkind = 'r' AND c.relname <> 'schema_version'
			AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY c.oid, a.attnum`)
	if err != nil {
		return nil, fmt.Errorf("target schema: %w", err)
	}
	var tables []*table
	byName := map[string]*table{}
	for rows.Next() {
		var name string
		var c column
		if err := rows.Scan(&name, &c.name, &c.typ, &c.notNull, &c.generated, &c.identity); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("target schema: %w", err)
		}
		if added, ok := sinceSource[name]; ok && (added == nil || slices.Contains(added, c.name)) {
			continue
		}
		switch c.typ {
		case "bigint", "smallint", "double precision", "text", "bytea":
		default:
			_ = rows.Close()
			return nil, fmt.Errorf("target schema: %s.%s is %s, which the copy does not map", name, c.name, c.typ)
		}
		t := byName[name]
		if t == nil {
			t = &table{name: name}
			byName[name] = t
			tables = append(tables, t)
		}
		t.cols = append(t.cols, c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("target schema: %w", err)
	}
	_ = rows.Close()
	rows, err = tx.QueryContext(ctx, `SELECT k.contype::text, k.conname, c.relname, COALESCE(p.relname, ''), a.attname
		FROM pg_constraint k JOIN pg_class c ON c.oid = k.conrelid
		LEFT JOIN pg_class p ON p.oid = k.confrelid
		CROSS JOIN LATERAL unnest(k.conkey) WITH ORDINALITY u(n, i)
		JOIN pg_attribute a ON a.attrelid = k.conrelid AND a.attnum = u.n
		WHERE k.contype IN ('p', 'f') AND c.relnamespace = current_schema()::regnamespace
		ORDER BY c.relname, k.conname, u.i`)
	if err != nil {
		return nil, fmt.Errorf("target schema: %w", err)
	}
	defer rows.Close()
	last := ""
	for rows.Next() {
		var kind, con, name, parent, col string
		if err := rows.Scan(&kind, &con, &name, &parent, &col); err != nil {
			return nil, fmt.Errorf("target schema: %w", err)
		}
		t := byName[name]
		switch {
		case t == nil:
		case kind == "p":
			t.pk = append(t.pk, col)
		case con+"\x00"+name == last:
			t.fks[len(t.fks)-1].cols = append(t.fks[len(t.fks)-1].cols, col)
		default:
			t.fks = append(t.fks, foreignKey{cols: []string{col}, parent: parent})
		}
		last = con + "\x00" + name
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("target schema: %w", err)
	}
	return tables, nil
}

// checkShape refuses a source whose tables or columns differ from the target's.
func checkShape(ctx context.Context, src *sql.DB, tables []*table) error {
	names, err := strings1(ctx, src, `SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		return fmt.Errorf("source schema: %w", err)
	}
	var diffs []string
	inSource := map[string]bool{}
	for _, n := range names {
		inSource[n] = true
	}
	for _, t := range tables {
		if !inSource[t.name] {
			diffs = append(diffs, "table "+t.name+" only in the target")
			continue
		}
		delete(inSource, t.name)
		cols, err := strings1(ctx, src, `SELECT name FROM pragma_table_xinfo(?) ORDER BY name`, t.name)
		if err != nil {
			return fmt.Errorf("source schema: %w", err)
		}
		srcCols := map[string]bool{}
		for _, c := range cols {
			srcCols[c] = true
		}
		for _, c := range t.cols {
			if !srcCols[c.name] {
				diffs = append(diffs, "column "+t.name+"."+c.name+" only in the target")
			}
			delete(srcCols, c.name)
		}
		for _, c := range cols {
			if srcCols[c] {
				diffs = append(diffs, "column "+t.name+"."+c+" only in the source")
			}
		}
	}
	for _, n := range names {
		if inSource[n] {
			diffs = append(diffs, "table "+n+" only in the source")
		}
	}
	if len(diffs) > 0 {
		return fmt.Errorf("source and target schemas differ: %s", strings.Join(diffs, "; "))
	}
	return nil
}

// pending is a row's deferred foreign key values, set once every row is in.
type pending struct {
	key, vals []any
}

// copyRows inserts every source row, tables in creation order: an inline foreign key names a table
// created before its own, so only the keys added later and self-references point ahead; those go
// in NULL and are set once every row is in.
func copyRows(ctx context.Context, src *sql.DB, tx *sql.Tx, tables []*table) ([]*result, error) {
	seqs, err := sqliteSequences(ctx, src)
	if err != nil {
		return nil, err
	}
	pos := map[string]int{}
	for i, t := range tables {
		pos[t.name] = i
	}
	results := make([]*result, len(tables))
	type deferredFKs struct {
		t    *table
		cols []string
		rows []pending
	}
	var later []*deferredFKs
	for i, t := range tables {
		deferred := map[string]bool{}
		for _, fk := range t.fks {
			if pos[fk.parent] < i {
				continue
			}
			for _, c := range fk.cols {
				if colOf(t, c).notNull || len(t.pk) == 0 {
					return nil, fmt.Errorf("target schema: %s.%s references %s, created later, and is NOT NULL or its table has no key: the copy cannot order it", t.name, c, fk.parent)
				}
				deferred[c] = true
			}
		}
		var names, marks, dcols []string
		var idx, kidx []int
		for j, c := range t.cols {
			if c.generated {
				continue
			}
			if deferred[c.name] {
				dcols = append(dcols, c.name)
			}
			names = append(names, ident(c.name))
			marks = append(marks, "$"+strconv.Itoa(len(names)))
			idx = append(idx, j)
		}
		for _, k := range t.pk {
			for j, c := range t.cols {
				if c.name == k {
					kidx = append(kidx, j)
				}
			}
		}
		d := &deferredFKs{t: t, cols: dcols}
		//nolint:gosec // names from the target's catalog, quoted
		stmt, err := tx.PrepareContext(ctx, "INSERT INTO "+ident(t.name)+" ("+strings.Join(names, ", ")+") VALUES ("+strings.Join(marks, ", ")+")")
		if err != nil {
			return nil, fmt.Errorf("insert into %s: %w", t.name, err)
		}
		results[i] = &result{table: t.name}
		err = readSource(ctx, src, t, results[i], func(vals []any) error {
			args := make([]any, len(idx))
			for a, j := range idx {
				args[a] = vals[j]
			}
			var p pending
			for a, j := range idx {
				if deferred[t.cols[j].name] {
					p.vals = append(p.vals, vals[j])
					args[a] = nil
				}
			}
			if slices.ContainsFunc(p.vals, func(v any) bool { return v != nil }) {
				for _, j := range kidx {
					p.key = append(p.key, vals[j])
				}
				d.rows = append(d.rows, p)
			}
			_, err := stmt.ExecContext(ctx, args...)
			return err
		})
		_ = stmt.Close()
		if err != nil {
			return nil, fmt.Errorf("copy %s: %w", t.name, err)
		}
		if len(d.rows) > 0 {
			later = append(later, d)
		}
	}
	for _, d := range later {
		set := make([]string, len(d.cols))
		for i, c := range d.cols {
			set[i] = ident(c) + " = $" + strconv.Itoa(i+1)
		}
		where := make([]string, len(d.t.pk))
		for i, k := range d.t.pk {
			where[i] = ident(k) + " = $" + strconv.Itoa(len(d.cols)+i+1)
		}
		//nolint:gosec // names from the target's catalog, quoted
		q := "UPDATE " + ident(d.t.name) + " SET " + strings.Join(set, ", ") + " WHERE " + strings.Join(where, " AND ")
		for _, p := range d.rows {
			if _, err := tx.ExecContext(ctx, q, append(p.vals, p.key...)...); err != nil {
				return nil, fmt.Errorf("copy %s: set %s: %w", d.t.name, strings.Join(d.cols, ", "), err)
			}
		}
	}
	for _, t := range tables {
		for _, c := range t.cols {
			if !c.identity {
				continue
			}
			var seq any
			if n, ok := seqs[t.name]; ok {
				seq = n
			}
			// SQLite's AUTOINCREMENT never reuses an id past the largest it handed out, even deleted.
			//nolint:gosec // names from the target's catalog, quoted
			if _, err := tx.ExecContext(ctx, `SELECT setval(pg_get_serial_sequence($1, $2), COALESCE(GREATEST(MAX(`+ident(c.name)+`), $3::bigint), 1),
				GREATEST(MAX(`+ident(c.name)+`), $3::bigint) IS NOT NULL) FROM `+ident(t.name), ident(t.name), c.name, seq); err != nil {
				return nil, fmt.Errorf("identity of %s: %w", t.name, err)
			}
		}
	}
	return results, nil
}

func colOf(t *table, name string) column {
	for _, c := range t.cols {
		if c.name == name {
			return c
		}
	}
	return column{}
}

func sqliteSequences(ctx context.Context, src *sql.DB) (map[string]int64, error) {
	rows, err := src.QueryContext(ctx, `SELECT name, seq FROM sqlite_sequence`)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var seq int64
		if err := rows.Scan(&name, &seq); err != nil {
			return nil, fmt.Errorf("source: %w", err)
		}
		// SQLite leaves 0 for a table emptied by a migration: no id was handed out.
		if seq > 0 {
			out[name] = seq
		}
	}
	return out, rows.Err()
}

func ident(s string) string { return pgx.Identifier{s}.Sanitize() }

// orderBy is the primary key, or every column without one, in an order both engines agree on.
func orderBy(t *table, pg bool) string {
	keys := t.pk
	if len(keys) == 0 {
		for _, c := range t.cols {
			keys = append(keys, c.name)
		}
	}
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = ident(k)
		if colOf(t, k).typ == "text" {
			out[i] += map[bool]string{true: ` COLLATE "C"`, false: ` COLLATE BINARY`}[pg]
		}
		if pg {
			out[i] += " NULLS FIRST"
		}
	}
	return strings.Join(out, ", ")
}

func selectCols(t *table) string {
	out := make([]string, len(t.cols))
	for i, c := range t.cols {
		out[i] = ident(c.name)
	}
	return strings.Join(out, ", ")
}

// readSource reads t's source rows in key order, as the target takes them, into r's count and
// digest, and hands each to row unless nil.
func readSource(ctx context.Context, src *sql.DB, t *table, r *result, row func(vals []any) error) error {
	//nolint:gosec // names from the target's catalog, quoted
	rows, err := src.QueryContext(ctx, "SELECT _rowid_, "+selectCols(t)+" FROM "+ident(t.name)+" ORDER BY "+orderBy(t, false))
	if err != nil {
		return fmt.Errorf("source %s: %w", t.name, err)
	}
	defer rows.Close()
	d := newDigest(t)
	raw := make([]any, len(t.cols)+1)
	ptrs := make([]any, len(raw))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return fmt.Errorf("source %s: %w", t.name, err)
		}
		vals := make([]any, len(t.cols))
		for i, c := range t.cols {
			v, err := convert(raw[i+1], c, r)
			if err != nil {
				return fmt.Errorf("source %s.%s rowid %v: %w", t.name, c.name, raw[0], err)
			}
			vals[i] = v
		}
		if err := d.add(vals); err != nil {
			return fmt.Errorf("source %s: %w", t.name, err)
		}
		r.srcRows++
		if row != nil {
			if err := row(vals); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("source %s: %w", t.name, err)
	}
	r.srcSum = d.sum()
	return nil
}

func targetSum(ctx context.Context, tx *sql.Tx, t *table, r *result) error {
	where := ""
	if w, ok := addedRows[t.name]; ok {
		where = " WHERE " + w
	}
	//nolint:gosec // names from the target's catalog, quoted; where from addedRows
	rows, err := tx.QueryContext(ctx, "SELECT "+selectCols(t)+" FROM "+ident(t.name)+where+" ORDER BY "+orderBy(t, true))
	if err != nil {
		return fmt.Errorf("target %s: %w", t.name, err)
	}
	defer rows.Close()
	d := newDigest(t)
	vals := make([]any, len(t.cols))
	ptrs := make([]any, len(vals))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return fmt.Errorf("target %s: %w", t.name, err)
		}
		if err := d.add(vals); err != nil {
			return fmt.Errorf("target %s: %w", t.name, err)
		}
		r.dstRows++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("target %s: %w", t.name, err)
	}
	r.dstSum = d.sum()
	return nil
}

// isTimestamp reports a text column the store writes with its fixed-width timestamp layout.
func isTimestamp(name string) bool {
	return name == "ts" || name == "at" || name == "email_nudge_until" || strings.HasSuffix(name, "_at")
}

// convert maps a SQLite value to what c holds in PostgreSQL (017 Q3), text cleaned as the store
// cleans it; a value of another type is an error, as SQLite takes any type in any column.
func convert(v any, c column, r *result) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch c.typ {
	case "bigint", "smallint":
		if n, ok := v.(int64); ok && (c.typ == "bigint" || (n >= math.MinInt16 && n <= math.MaxInt16)) {
			return n, nil
		}
	case "double precision":
		switch x := v.(type) {
		case float64:
			return x, nil
		case int64:
			return float64(x), nil
		}
	case "text":
		if s, ok := v.(string); ok {
			if clean := store.Sanitize(s); clean != s {
				r.sanitized++
				s = clean
			}
			if isTimestamp(c.name) && s != "" {
				ts, ok := store.NormalizeTS(s)
				if !ok {
					r.badTS++
				}
				s = ts
			}
			return s, nil
		}
	case "bytea":
		switch x := v.(type) {
		case []byte:
			// The driver reads an empty BLOB as a nil slice, which pgx would write as NULL.
			if x == nil {
				x = []byte{}
			}
			return x, nil
		case string:
			// SQLite keeps text written to a BLOB column as text; its bytes are the value.
			return []byte(x), nil
		}
	}
	return nil, fmt.Errorf("a %s value does not fit a %s column", sqliteType(v), c.typ)
}

func sqliteType(v any) string {
	switch v.(type) {
	case int64:
		return "INTEGER"
	case float64:
		return "REAL"
	case string:
		return "TEXT"
	case []byte:
		return "BLOB"
	}
	return fmt.Sprintf("%T", v)
}

// encode writes a row in a canonical form both sides share: a type tag per value, then 8 bytes
// for numbers, or a length and the bytes.
func encode(h io.Writer, vals []any) error {
	var b [1 + binary.MaxVarintLen64]byte
	for _, v := range vals {
		switch x := v.(type) {
		case nil:
			_, _ = h.Write([]byte{'N'})
		case int64:
			b[0] = 'I'
			binary.BigEndian.PutUint64(b[1:9], uint64(x)) //nolint:gosec // the bits, not the value
			_, _ = h.Write(b[:9])
		case float64:
			b[0] = 'F'
			binary.BigEndian.PutUint64(b[1:9], math.Float64bits(x))
			_, _ = h.Write(b[:9])
		case string:
			b[0] = 'T'
			_, _ = h.Write(b[:1+binary.PutUvarint(b[1:], uint64(len(x)))])
			_, _ = io.WriteString(h, x)
		case []byte:
			b[0] = 'B'
			_, _ = h.Write(b[:1+binary.PutUvarint(b[1:], uint64(len(x)))])
			_, _ = h.Write(x)
		default:
			return fmt.Errorf("unexpected %T value", v)
		}
	}
	return nil
}

// report writes one aligned line per table and a total, and names the tables that differ.
func report(out io.Writer, results []*result) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "TABLE\tSOURCE ROWS\tTARGET ROWS\tSOURCE SHA256\tTARGET SHA256\tSTATUS\tNOTES")
	var bad []string
	var src, dst int64
	short := func(b []byte) string { return hex.EncodeToString(b)[:16] }
	for _, r := range results {
		status := "ok"
		if !r.ok() {
			status = "DIFFERS"
			bad = append(bad, r.table)
		}
		var notes []string
		if r.sanitized > 0 {
			notes = append(notes, fmt.Sprintf("%d text values sanitized", r.sanitized))
		}
		if r.badTS > 0 {
			notes = append(notes, fmt.Sprintf("%d timestamps kept unparsed", r.badTS))
		}
		src, dst = src+r.srcRows, dst+r.dstRows
		_, _ = fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\t%s\t%s\n", r.table, r.srcRows, r.dstRows, short(r.srcSum), short(r.dstSum), status, strings.Join(notes, ", "))
	}
	status := "ok"
	if len(bad) > 0 {
		status = fmt.Sprintf("%d DIFFER", len(bad))
	}
	_, _ = fmt.Fprintf(w, "total: %d tables\t%d\t%d\t\t\t%s\t\n", len(results), src, dst, status)
	if err := w.Flush(); err != nil {
		return fmt.Errorf("report: %w", err)
	}
	if len(bad) > 0 {
		return fmt.Errorf("%d of %d tables differ: %s", len(bad), len(results), strings.Join(bad, ", "))
	}
	return nil
}
