// Package monitor keeps the supervisor's local turn history, monitor.db (003-AC27): one row per
// turn its workers ran, filtered, totalled per account and day, exported as CSV, and purged past
// the retention (003-AC28). Nothing in it leaves the host's machine.
package monitor

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go: the daemon stays a static binary
)

// Statuses a turn ends with.
const (
	StatusOK       = "ok"
	StatusFailed   = "failed"
	StatusConflict = "conflict"
	StatusExpired  = "expired"
	StatusCutOff   = "cut off"
	StatusStopped  = "stopped"
)

// Turn is one recorded turn. Day is At's date in At's location: the supervisor's local day.
type Turn struct {
	ID          int64     `json:"id"`
	At          time.Time `json:"at"`
	Day         string    `json:"day"`
	Instance    string    `json:"instance"`
	Room        string    `json:"room"`
	TurnID      int64     `json:"turn_id"`
	Persona     string    `json:"persona"`
	Account     string    `json:"account"`
	AskedModel  string    `json:"asked_model"`
	AskedEffort string    `json:"asked_effort"`
	Model       string    `json:"model"`
	// ModelNote and ModelMismatch qualify Model as the room's synthesis header does (005-AC10).
	ModelNote     string `json:"model_note"`
	ModelMismatch bool   `json:"model_mismatch"`
	Effort        string `json:"effort"`
	Cause         string `json:"cause"`
	DurationMS    int64  `json:"duration_ms"`
	QueueMS       int64  `json:"queue_ms"`
	Status        string `json:"status"`
	Error         string `json:"error,omitempty"`
	TokensIn      int64  `json:"tokens_in"`
	TokensOut     int64  `json:"tokens_out"`
	CacheRead     int64  `json:"tokens_cache_read"`
	CacheWrite    int64  `json:"tokens_cache_write"`
	// CostUSD is claude's estimate; nil when it reported none.
	CostUSD        *float64 `json:"cost_usd"`
	ContextTokens  int64    `json:"context_tokens"`
	ContextWindow  int64    `json:"context_window"`
	WorkspaceBytes int64    `json:"workspace_bytes"`
}

// DB is monitor.db. Its methods are safe for concurrent use.
type DB struct {
	db  *sql.DB
	now func() time.Time
}

const schema = `
CREATE TABLE turns (
	id INTEGER PRIMARY KEY,
	at INTEGER NOT NULL,
	day TEXT NOT NULL,
	instance TEXT NOT NULL,
	room TEXT NOT NULL,
	turn_id INTEGER NOT NULL,
	persona TEXT NOT NULL,
	account TEXT NOT NULL,
	asked_model TEXT NOT NULL,
	asked_effort TEXT NOT NULL,
	model TEXT NOT NULL,
	effort TEXT NOT NULL,
	cause TEXT NOT NULL,
	duration_ms INTEGER NOT NULL,
	queue_ms INTEGER NOT NULL,
	status TEXT NOT NULL,
	error TEXT NOT NULL,
	tokens_in INTEGER NOT NULL,
	tokens_out INTEGER NOT NULL,
	tokens_cache_read INTEGER NOT NULL,
	tokens_cache_write INTEGER NOT NULL,
	cost_usd REAL,
	context_tokens INTEGER NOT NULL,
	context_window INTEGER NOT NULL,
	workspace_bytes INTEGER NOT NULL
);
CREATE INDEX turns_at ON turns (at);
CREATE INDEX turns_day ON turns (day);
CREATE INDEX turns_persona ON turns (instance, room, persona, at);
`

// schemaV2 adds what qualifies a turn's model (005-AC10).
const schemaV2 = `
ALTER TABLE turns ADD COLUMN model_note TEXT NOT NULL DEFAULT '';
ALTER TABLE turns ADD COLUMN model_mismatch INTEGER NOT NULL DEFAULT 0;
PRAGMA user_version = 2;
`

// Open opens, creating it owner-only if needed, the history at path. now is the clock; nil is
// time.Now.
func Open(path string, now func() time.Time) (*DB, error) {
	if now == nil {
		now = time.Now
	}
	// Security: room and persona names are the host's private data; SQLite's -wal and -shm files
	// copy the database file's mode.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // the supervisor's own state dir
	if err != nil {
		return nil, fmt.Errorf("create monitor db: %w", err)
	}
	_ = f.Close()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("open monitor db: %w", err)
	}
	db.SetMaxOpenConns(1)
	var version int
	err = db.QueryRowContext(context.Background(), `PRAGMA user_version`).Scan(&version)
	if err == nil && version == 0 {
		_, err = db.ExecContext(context.Background(), schema+`PRAGMA user_version = 1;`)
		version = 1
	}
	if err == nil && version == 1 {
		err = upgrade(db, schemaV2)
	}
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("monitor db schema: %w", err)
	}
	return &DB{db: db, now: now}, nil
}

// upgrade runs a schema change and its version bump all or nothing.
func upgrade(db *sql.DB, change string) error {
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(context.Background(), change); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Close closes the database.
func (d *DB) Close() error { return d.db.Close() }

// maxText bounds each stored text: a worker may run a binary its instance served, and sends what
// it likes.
const maxText = 500

func clip(s *string) {
	if r := []rune(*s); len(r) > maxText {
		*s = string(r[:maxText])
	}
}

// Record stores t, ended now.
func (d *DB) Record(ctx context.Context, t Turn) error {
	t.At = d.now()
	for _, s := range []*string{&t.Instance, &t.Room, &t.Persona, &t.Account, &t.AskedModel, &t.AskedEffort, &t.Model, &t.Effort,
		&t.Cause, &t.Status, &t.Error, &t.ModelNote} {
		clip(s)
	}
	_, err := d.db.ExecContext(ctx, `INSERT INTO turns (at, day, instance, room, turn_id, persona, account, asked_model,
		asked_effort, model, effort, cause, duration_ms, queue_ms, status, error, tokens_in, tokens_out, tokens_cache_read,
		tokens_cache_write, cost_usd, context_tokens, context_window, workspace_bytes, model_note, model_mismatch)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.At.UnixMilli(), t.At.Format(time.DateOnly), t.Instance, t.Room, t.TurnID, t.Persona, t.Account, t.AskedModel, t.AskedEffort,
		t.Model, t.Effort, t.Cause, t.DurationMS, t.QueueMS, t.Status, t.Error, t.TokensIn, t.TokensOut, t.CacheRead, t.CacheWrite,
		t.CostUSD, t.ContextTokens, t.ContextWindow, t.WorkspaceBytes, t.ModelNote, t.ModelMismatch)
	if err != nil {
		return fmt.Errorf("record turn: %w", err)
	}
	return nil
}

// Filter narrows the turns; empty fields match everything. From and To are local dates,
// YYYY-MM-DD, both included.
type Filter struct {
	Instance string `json:"instance"`
	Room     string `json:"room"`
	Persona  string `json:"persona"`
	Account  string `json:"account"`
	Status   string `json:"status"`
	From     string `json:"from"`
	To       string `json:"to"`
}

// where is f as a WHERE clause and its arguments.
func (f Filter) where() (string, []any, error) {
	var conds []string
	var args []any
	for _, c := range []struct{ col, v string }{
		{"instance", f.Instance}, {"room", f.Room}, {"persona", f.Persona}, {"account", f.Account}, {"status", f.Status},
	} {
		if c.v != "" {
			conds, args = append(conds, c.col+" = ?"), append(args, c.v)
		}
	}
	for _, c := range []struct{ op, v string }{{">=", f.From}, {"<=", f.To}} {
		if c.v == "" {
			continue
		}
		if _, err := time.Parse(time.DateOnly, c.v); err != nil {
			return "", nil, fmt.Errorf("date %q: want YYYY-MM-DD", c.v)
		}
		conds, args = append(conds, "day "+c.op+" ?"), append(args, c.v)
	}
	if len(conds) == 0 {
		return "", nil, nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args, nil
}

// ErrFilter is a filter the page sent wrong.
var ErrFilter = errors.New("bad filter")

const turnCols = `id, at, day, instance, room, turn_id, persona, account, asked_model, asked_effort, model, effort, cause,
	duration_ms, queue_ms, status, error, tokens_in, tokens_out, tokens_cache_read, tokens_cache_write, cost_usd,
	context_tokens, context_window, workspace_bytes, model_note, model_mismatch`

// Turns lists the turns f matches, newest first, at most limit of them; limit 0 lists them all.
func (d *DB) Turns(ctx context.Context, f Filter, limit int) ([]Turn, error) {
	where, args, err := f.where()
	if err != nil {
		return nil, errors.Join(ErrFilter, err)
	}
	q := `SELECT ` + turnCols + ` FROM turns` + where + ` ORDER BY at DESC, id DESC` //nolint:gosec // where holds placeholders only
	if limit > 0 {
		q += ` LIMIT ` + strconv.Itoa(limit)
	}
	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list turns: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only
	out := []Turn{}
	for rows.Next() {
		var t Turn
		var at int64
		if err := rows.Scan(&t.ID, &at, &t.Day, &t.Instance, &t.Room, &t.TurnID, &t.Persona, &t.Account, &t.AskedModel, &t.AskedEffort,
			&t.Model, &t.Effort, &t.Cause, &t.DurationMS, &t.QueueMS, &t.Status, &t.Error, &t.TokensIn, &t.TokensOut, &t.CacheRead,
			&t.CacheWrite, &t.CostUSD, &t.ContextTokens, &t.ContextWindow, &t.WorkspaceBytes, &t.ModelNote, &t.ModelMismatch); err != nil {
			return nil, fmt.Errorf("list turns: %w", err)
		}
		t.At = time.UnixMilli(at).In(d.now().Location())
		out = append(out, t)
	}
	return out, rows.Err()
}

// Total is one account's turns on one day. CostUSD sums the known costs; UnknownCost counts the
// turns claude gave none for.
type Total struct {
	Day         string  `json:"day"`
	Account     string  `json:"account"`
	Turns       int64   `json:"turns"`
	TokensIn    int64   `json:"tokens_in"`
	TokensOut   int64   `json:"tokens_out"`
	CacheRead   int64   `json:"tokens_cache_read"`
	CacheWrite  int64   `json:"tokens_cache_write"`
	CostUSD     float64 `json:"cost_usd"`
	UnknownCost int64   `json:"unknown_cost"`
}

// Totals sums the turns f matches per day and account, newest day first.
func (d *DB) Totals(ctx context.Context, f Filter) ([]Total, error) {
	where, args, err := f.where()
	if err != nil {
		return nil, errors.Join(ErrFilter, err)
	}
	//nolint:gosec // where holds placeholders only
	rows, err := d.db.QueryContext(ctx, `SELECT day, account, count(*), sum(tokens_in), sum(tokens_out), sum(tokens_cache_read),
		sum(tokens_cache_write), coalesce(sum(cost_usd), 0), count(*) - count(cost_usd) FROM turns`+where+`
		GROUP BY day, account ORDER BY day DESC, account`, args...)
	if err != nil {
		return nil, fmt.Errorf("totals: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only
	out := []Total{}
	for rows.Next() {
		var t Total
		if err := rows.Scan(&t.Day, &t.Account, &t.Turns, &t.TokensIn, &t.TokensOut, &t.CacheRead, &t.CacheWrite, &t.CostUSD, &t.UnknownCost); err != nil {
			return nil, fmt.Errorf("totals: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Facets are the values the page's filters offer: those the history holds.
type Facets struct {
	Instances []string `json:"instances"`
	Rooms     []string `json:"rooms"`
	Personas  []string `json:"personas"`
	Accounts  []string `json:"accounts"`
	Statuses  []string `json:"statuses"`
}

// Facets lists the distinct values of each filtered column.
func (d *DB) Facets(ctx context.Context) (Facets, error) {
	var f Facets
	for _, c := range []struct {
		col string
		out *[]string
	}{{"instance", &f.Instances}, {"room", &f.Rooms}, {"persona", &f.Personas}, {"account", &f.Accounts}, {"status", &f.Statuses}} {
		rows, err := d.db.QueryContext(ctx, `SELECT DISTINCT `+c.col+` FROM turns WHERE `+c.col+` != '' ORDER BY 1`) //nolint:gosec // c.col is a fixed column name
		if err != nil {
			return f, fmt.Errorf("facets: %w", err)
		}
		*c.out = []string{}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				_ = rows.Close()
				return f, fmt.Errorf("facets: %w", err)
			}
			*c.out = append(*c.out, v)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return f, fmt.Errorf("facets: %w", err)
		}
	}
	return f, nil
}

// Context is a persona session's context use, as of its latest turn that reported one.
type Context struct {
	Instance string    `json:"instance"`
	Room     string    `json:"room"`
	Persona  string    `json:"persona"`
	Tokens   int64     `json:"tokens"`
	Window   int64     `json:"window"`
	At       time.Time `json:"at"`
}

// Contexts lists each persona's latest known context use.
func (d *DB) Contexts(ctx context.Context) ([]Context, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT instance, room, persona, context_tokens, context_window, at FROM turns t
		WHERE persona != '' AND context_window > 0 AND id = (SELECT id FROM turns u WHERE u.instance = t.instance AND u.room = t.room
		AND u.persona = t.persona AND u.context_window > 0 ORDER BY at DESC, id DESC LIMIT 1) ORDER BY instance, room, persona`)
	if err != nil {
		return nil, fmt.Errorf("contexts: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only
	var out []Context
	for rows.Next() {
		var c Context
		var at int64
		if err := rows.Scan(&c.Instance, &c.Room, &c.Persona, &c.Tokens, &c.Window, &at); err != nil {
			return nil, fmt.Errorf("contexts: %w", err)
		}
		c.At = time.UnixMilli(at).In(d.now().Location())
		out = append(out, c)
	}
	return out, rows.Err()
}

// Purge deletes the turns that ended more than retention ago, and returns how many.
func (d *DB) Purge(ctx context.Context, retention time.Duration) (int64, error) {
	res, err := d.db.ExecContext(ctx, `DELETE FROM turns WHERE at < ?`, d.now().Add(-retention).UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("purge: %w", err)
	}
	return res.RowsAffected()
}

// csvHeader names WriteCSV's columns.
var csvHeader = []string{"ended_at", "day", "instance", "room", "turn_id", "persona", "account", "asked_model", "asked_effort", "model",
	"effort", "cause", "duration_ms", "queue_ms", "status", "error", "tokens_in", "tokens_out", "tokens_cache_read",
	"tokens_cache_write", "cost_usd", "context_tokens", "context_window", "context_pct", "workspace_bytes", "model_note", "model_mismatch"}

// WriteCSV writes ts as CSV, with a header line. An unknown cost or context is an empty cell.
func WriteCSV(w io.Writer, ts []Turn) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(csvHeader); err != nil {
		return err
	}
	n := func(v int64) string { return strconv.FormatInt(v, 10) }
	for _, t := range ts {
		cost, pct := "", ""
		if t.CostUSD != nil {
			cost = strconv.FormatFloat(*t.CostUSD, 'f', -1, 64)
		}
		if t.ContextWindow > 0 {
			pct = n(t.ContextTokens * 100 / t.ContextWindow)
		}
		if err := cw.Write([]string{t.At.Format(time.RFC3339), t.Day, cell(t.Instance), cell(t.Room), n(t.TurnID), cell(t.Persona),
			cell(t.Account), cell(t.AskedModel), cell(t.AskedEffort), cell(t.Model), cell(t.Effort), cell(t.Cause), n(t.DurationMS),
			n(t.QueueMS), t.Status, cell(t.Error), n(t.TokensIn), n(t.TokensOut), n(t.CacheRead), n(t.CacheWrite), cost,
			n(t.ContextTokens), n(t.ContextWindow), pct, n(t.WorkspaceBytes), cell(t.ModelNote), strconv.FormatBool(t.ModelMismatch)}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// cell defuses a text a spreadsheet would run as a formula: persona names, causes and errors come
// from rooms other people write in.
func cell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}
