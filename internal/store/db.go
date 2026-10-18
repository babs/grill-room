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
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// WriteLockKey is the pg_advisory_xact_lock every write takes ("grlm"); db copy takes it too.
const WriteLockKey int64 = 0x67726c6d

// errWriteOutsideLock refuses a write that would commit without the write lock.
var errWriteOutsideLock = errors.New("write outside the write lock: run it in withTx")

// database wraps *sql.DB with no embedding, so every call site goes through the rebind and the
// write lock below.
type database struct {
	db *sql.DB
	// writer lets one write transaction of this process hold a pool connection at a time: writers
	// queued on the lock would otherwise take every connection and starve the readers.
	writer      chan struct{}
	lockWait    func(time.Duration)
	lockTimeout time.Duration
	// cfg is the parsed DSN, for the connections the pool cannot give: LISTEN's.
	cfg *pgx.ConnConfig
}

// txn is a transaction that holds the write lock unless it was begun read-only or by unlocked.
type txn struct {
	tx *sql.Tx
	// writer is the database's writer slot this transaction holds, nil once released.
	writer chan struct{}
	// shared marks a transaction of unlocked, whose writes must stay in unlockedWrites.
	shared bool
}

// errUnlockedWrite refuses a write unlocked may not run: it would commit out of id order.
var errUnlockedWrite = errors.New("write outside the write lock to a table not in unlockedWrites")

// unlockedWrites are the tables a transaction of unlocked may write, by table then verb ("" for
// every verb): no id cursor reads them, and rate_events only loses rows past every window.
var unlockedWrites = map[string]string{
	"presence": "", "daemon_connections": "", "daemon_status": "", "stt_workers": "", "tts_pod": "", "pairings": "", "settings": "",
	"rate_events": "DELETE",
}

// check refuses a statement of a shared transaction that writes outside unlockedWrites.
func (t *txn) check(c compiled) error {
	if !t.shared || !c.write {
		return nil
	}
	if verb, ok := unlockedWrites[c.table]; ok && (verb == "" || verb == c.verb) {
		return nil
	}
	return errUnlockedWrite
}

// row is what QueryRowContext returns, from a transaction or from the pool; a write through the
// pool runs in its own locked transaction when scanned.
type row struct {
	r    *sql.Row
	scan func(dest ...any) error
}

// Scan copies the row's columns into dest, as sql.Row.Scan.
func (r *row) Scan(dest ...any) error {
	if r.scan != nil {
		return r.scan(dest...)
	}
	return r.r.Scan(dest...)
}

// Err reports the query's error, as sql.Row.Err; a deferred write reports it from Scan.
func (r *row) Err() error {
	if r.scan != nil {
		return nil
	}
	return r.r.Err()
}

// compiled is a query rebound for PostgreSQL, whether it writes, and the verb and table of a
// plain INSERT, UPDATE or DELETE, empty for any other statement.
type compiled struct {
	q           string
	write       bool
	verb, table string
}

// ponytail: unbounded, but keyed by query text, and the package builds a finite set of them.
var compiledQueries sync.Map

func compile(query string) compiled {
	if v, ok := compiledQueries.Load(query); ok {
		return v.(compiled)
	}
	c := compiled{q: rebind(query), write: isWrite(query)}
	c.verb, c.table = target(query)
	compiledQueries.Store(query, c)
	return c
}

// pgArgs turns Go bools into the 0/1 integers the SMALLINT flags hold (017 Q3), and cleans text.
func pgArgs(args []any) []any {
	var out []any
	set := func(i int, v any) {
		if out == nil {
			out = slices.Clone(args)
		}
		out[i] = v
	}
	for i, a := range args {
		switch v := a.(type) {
		case bool:
			n := int64(0)
			if v {
				n = 1
			}
			set(i, n)
		case string:
			if !cleanText(v) {
				set(i, Sanitize(v))
			}
		case *string:
			if v != nil && !cleanText(*v) {
				c := Sanitize(*v)
				set(i, &c)
			}
		}
	}
	if out == nil {
		return args
	}
	return out
}

// cleanText reports text PG TEXT takes as is: it refuses NUL and invalid UTF-8, which
// JSON-decoded text (\u0000) or an import can carry.
func cleanText(s string) bool { return strings.IndexByte(s, 0) < 0 && utf8.ValidString(s) }

// Sanitize replaces invalid UTF-8 and NUL with U+FFFD, so such a write stores, not fails, and a
// lookup of "r1\x00" does not find "r1".
func Sanitize(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "\uFFFD"), "\x00", "\uFFFD")
}

// BeginTx begins a transaction that holds the write lock, unless opts asks for a read-only one.
// ponytail: one writer cluster-wide, so ids commit in id order and the "id > last" stream cursors
// skip nothing; per-room locks plus commit-order-safe cursors if writes contend.
func (d *database) BeginTx(ctx context.Context, opts *sql.TxOptions) (*txn, error) {
	if opts != nil && opts.ReadOnly {
		tx, err := d.db.BeginTx(ctx, opts)
		if err != nil {
			return nil, err
		}
		return &txn{tx: tx}, nil
	}
	start := time.Now()
	// lock_timeout bounds only the advisory lock: writers queued here behind it get the same bound.
	slot := time.NewTimer(d.lockTimeout)
	defer slot.Stop()
	select {
	case d.writer <- struct{}{}:
	case <-ctx.Done():
		d.waited(start)
		return nil, fmt.Errorf("write lock: %w", ctx.Err())
	case <-slot.C:
		d.waited(start)
		return nil, fmt.Errorf("write lock: %w", &pgconn.PgError{Code: "55P03", Message: "writer slot not free within lock_timeout"})
	}
	tx, err := d.db.BeginTx(ctx, opts)
	if err != nil {
		<-d.writer
		return nil, err
	}
	t := &txn{tx: tx, writer: d.writer}
	_, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, WriteLockKey)
	d.waited(start)
	if err != nil {
		_ = t.Rollback()
		return nil, fmt.Errorf("write lock: %w", err)
	}
	return t, nil
}

func (d *database) waited(start time.Time) {
	if d.lockWait != nil {
		d.lockWait(time.Since(start))
	}
}

// locked runs fn in a transaction of its own that holds the write lock.
func (d *database) locked(ctx context.Context, fn func(t *txn) error) error {
	t, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer t.Rollback() //nolint:errcheck // no-op after Commit
	if err := fn(t); err != nil {
		return err
	}
	return t.Commit()
}

// unlocked runs fn in a transaction without the write lock, for writes to the tables no id cursor
// reads, so they need no commit order; it refuses any other write.
func (d *database) unlocked(ctx context.Context, fn func(t *txn) error) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	t := &txn{tx: tx, shared: true}
	defer t.Rollback() //nolint:errcheck // no-op after Commit
	if err := fn(t); err != nil {
		return err
	}
	return t.Commit()
}

// ExecContext runs query in a transaction of its own that holds the write lock.
func (d *database) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	var res sql.Result
	err := d.locked(ctx, func(t *txn) error {
		var err error
		res, err = t.ExecContext(ctx, query, args...)
		return err
	})
	return res, err
}

// QueryContext runs a read; a write must go through a transaction, which holds the lock.
func (d *database) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	st := compile(query)
	// Rows outlive any transaction this call could open and commit for a write.
	if st.write {
		return nil, errWriteOutsideLock
	}
	return d.db.QueryContext(ctx, st.q, pgArgs(args)...)
}

// QueryRowContext runs a read directly, and a write, such as an INSERT .. RETURNING, in a
// transaction of its own that holds the write lock.
func (d *database) QueryRowContext(ctx context.Context, query string, args ...any) *row {
	st := compile(query)
	if !st.write {
		return &row{r: d.db.QueryRowContext(ctx, st.q, pgArgs(args)...)}
	}
	return &row{scan: func(dest ...any) error {
		return d.locked(ctx, func(t *txn) error { return t.QueryRowContext(ctx, query, args...).Scan(dest...) })
	}}
}

// PingContext checks the connection.
func (d *database) PingContext(ctx context.Context) error { return d.db.PingContext(ctx) }

// Close closes the pool.
func (d *database) Close() error { return d.db.Close() }

// ExecContext runs query in the transaction.
func (t *txn) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	c := compile(query)
	if err := t.check(c); err != nil {
		return nil, err
	}
	return t.tx.ExecContext(ctx, c.q, pgArgs(args)...)
}

// QueryContext runs query in the transaction.
func (t *txn) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	c := compile(query)
	if err := t.check(c); err != nil {
		return nil, err
	}
	return t.tx.QueryContext(ctx, c.q, pgArgs(args)...)
}

// QueryRowContext runs query in the transaction.
func (t *txn) QueryRowContext(ctx context.Context, query string, args ...any) *row {
	c := compile(query)
	if err := t.check(c); err != nil {
		return &row{scan: func(...any) error { return err }}
	}
	return &row{r: t.tx.QueryRowContext(ctx, c.q, pgArgs(args)...)}
}

// Commit commits the transaction, releasing the write lock.
func (t *txn) Commit() error {
	defer t.release()
	return t.tx.Commit()
}

// Rollback aborts the transaction, releasing the write lock.
func (t *txn) Rollback() error {
	defer t.release()
	return t.tx.Rollback()
}

func (t *txn) release() {
	if t.writer != nil {
		<-t.writer
		t.writer = nil
	}
}

// isUnique reports a unique violation (SQLSTATE 23505).
func isUnique(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// sqlScanner walks SQL text, telling code apart from string literals, quoted identifiers and
// comments.
type sqlScanner struct {
	s string
	i int
}

// next returns the next run of code (inCode true) or of literal/identifier/comment text.
func (sc *sqlScanner) next() (text string, inCode bool) {
	s, start := sc.s, sc.i
	switch {
	case s[start] == '\'' || s[start] == '"':
		q := s[start]
		i := start + 1
		for i < len(s) {
			if s[i] == q {
				if i+1 < len(s) && s[i+1] == q {
					i += 2
					continue
				}
				i++
				break
			}
			i++
		}
		sc.i = i
		return s[start:i], false
	case strings.HasPrefix(s[start:], "--"):
		i := strings.IndexByte(s[start:], '\n')
		if i < 0 {
			sc.i = len(s)
		} else {
			sc.i = start + i
		}
		return s[start:sc.i], false
	case strings.HasPrefix(s[start:], "/*"):
		// PostgreSQL block comments nest.
		depth, i := 1, start+2
		for i < len(s) && depth > 0 {
			switch {
			case strings.HasPrefix(s[i:], "/*"):
				depth, i = depth+1, i+2
			case strings.HasPrefix(s[i:], "*/"):
				depth, i = depth-1, i+2
			default:
				i++
			}
		}
		sc.i = i
		return s[start:i], false
	}
	i := start
	for i < len(s) && s[i] != '\'' && s[i] != '"' && !strings.HasPrefix(s[i:], "--") && !strings.HasPrefix(s[i:], "/*") {
		i++
	}
	sc.i = i
	return s[start:i], true
}

func (sc *sqlScanner) done() bool { return sc.i >= len(sc.s) }

// rebind turns the ? and ?N placeholders the queries are written with into PostgreSQL's $N; a bare
// ? takes one more than the largest number before it.
func rebind(q string) string {
	if !strings.Contains(q, "?") {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 16)
	n := 0
	sc := sqlScanner{s: q}
	for !sc.done() {
		text, code := sc.next()
		if !code {
			b.WriteString(text)
			continue
		}
		for i := 0; i < len(text); i++ {
			if text[i] != '?' {
				b.WriteByte(text[i])
				continue
			}
			j := i + 1
			for j < len(text) && text[j] >= '0' && text[j] <= '9' {
				j++
			}
			k := n + 1
			if j > i+1 {
				k, _ = strconv.Atoi(text[i+1 : j])
			}
			n = max(n, k)
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(k))
			i = j - 1
		}
	}
	return b.String()
}

// tokens are q's code words, upper-cased, and ";" for each statement end; a literal or a quoted
// identifier is one "" token, a comment none.
func tokens(q string) []string {
	var out []string
	sc := sqlScanner{s: q}
	for !sc.done() {
		text, code := sc.next()
		switch {
		case code:
			out = append(out, strings.FieldsFunc(strings.ToUpper(strings.ReplaceAll(text, ";", " ; ")), func(r rune) bool {
				return (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != ';'
			})...)
		case text[0] == '\'' || text[0] == '"':
			out = append(out, "")
		}
	}
	return out
}

// target returns the verb and lower-cased table of an INSERT INTO t, UPDATE t or DELETE FROM t;
// empty for any other statement: a WITH, several statements, a quoted table name.
func target(q string) (verb, table string) {
	w := tokens(q)
	if isMulti(w) {
		return "", ""
	}
	switch {
	case len(w) >= 3 && (w[0] == "INSERT" && w[1] == "INTO" || w[0] == "DELETE" && w[1] == "FROM"):
		return w[0], strings.ToLower(w[2])
	case len(w) >= 2 && w[0] == "UPDATE":
		return w[0], strings.ToLower(w[1])
	}
	return "", ""
}

// isMulti reports a statement end followed by more of the query: a second statement.
func isMulti(w []string) bool {
	i := slices.Index(w, ";")
	return i >= 0 && slices.ContainsFunc(w[i+1:], func(t string) bool { return t != ";" })
}

// isWrite reports a statement that changes rows: one starting with INSERT, UPDATE, DELETE, MERGE or
// TRUNCATE, or a WITH holding one, outside literals and comments. Several statements count as a
// write, whatever they hold.
func isWrite(q string) bool {
	words := tokens(q)
	if len(words) == 0 {
		return false
	}
	if isMulti(words) {
		return true
	}
	dml := func(w string) bool {
		return w == "INSERT" || w == "UPDATE" || w == "DELETE" || w == "MERGE" || w == "TRUNCATE"
	}
	if words[0] == "WITH" {
		return slices.ContainsFunc(words, dml)
	}
	return dml(words[0])
}
