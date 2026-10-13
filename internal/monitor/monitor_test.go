package monitor

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func open(t *testing.T) (*DB, *clock) {
	t.Helper()
	c := &clock{time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)}
	d, err := Open(filepath.Join(t.TempDir(), "monitor.db"), c.now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d, c
}

func cost(v float64) *float64 { return &v }

// 003-AC27: a recorded turn comes back whole, with its end time and day from the clock; an
// unknown cost stays unknown, and every text is clipped.
func TestRecord(t *testing.T) {
	d, c := open(t)
	in := Turn{Instance: "i1", Room: "https://a/r/1", TurnID: 7, Persona: "Grill", Account: "default", AskedModel: "opus", AskedEffort: "high",
		Model: "sonnet", ModelNote: "+haiku 3%", Effort: "medium", Cause: "pressed by Alice", DurationMS: 1200, QueueMS: 300, Status: StatusOK,
		TokensIn: 1, TokensOut: 2, CacheRead: 3, CacheWrite: 4, CostUSD: cost(0.5), ContextTokens: 170000, ContextWindow: 200000, WorkspaceBytes: 4096}
	if err := d.Record(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	got, err := d.Turns(t.Context(), Filter{}, 0)
	if err != nil || len(got) != 1 {
		t.Fatalf("turns: %v %v", got, err)
	}
	want := in
	want.ID, want.At, want.Day = got[0].ID, c.t, "2026-10-01"
	if g := got[0]; g.CostUSD == nil || *g.CostUSD != 0.5 || !g.At.Equal(want.At) {
		t.Fatalf("got %+v", g)
	}
	got[0].CostUSD, want.CostUSD, got[0].At, want.At = nil, nil, time.Time{}, time.Time{}
	if got[0] != want {
		t.Fatalf("got %+v\nwant %+v", got[0], want)
	}

	// A second turn with the same id, as after a room server's database was recreated, is a new row.
	long := string(bytes.Repeat([]byte("é"), 600))
	in.Status, in.CostUSD = StatusFailed, nil
	in.Instance, in.Room, in.Persona, in.Account, in.AskedModel, in.AskedEffort, in.Model, in.Effort, in.Cause, in.Error =
		long, long, long, long, long, long, long, long, long, long
	if err := d.Record(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	got, _ = d.Turns(t.Context(), Filter{}, 0)
	if len(got) != 2 || got[0].Status != StatusFailed || got[0].CostUSD != nil {
		t.Fatalf("second record: %+v", got)
	}
	g := got[0]
	for i, v := range []string{g.Instance, g.Room, g.Persona, g.Account, g.AskedModel, g.AskedEffort, g.Model, g.Effort, g.Cause, g.Error} {
		if n := len([]rune(v)); n != maxText {
			t.Errorf("text %d kept %d runes", i, n)
		}
	}
}

func seed(t *testing.T, d *DB, c *clock) {
	t.Helper()
	start := c.t
	for i, r := range []Turn{
		{Instance: "i1", Room: "r1", Persona: "Grill", Account: "work", Status: StatusOK, TokensIn: 10, TokensOut: 1, CostUSD: cost(0.25)},
		{Instance: "i1", Room: "r1", Persona: "Scribe", Account: "work", Status: StatusFailed, TokensIn: 20, TokensOut: 2},
		{Instance: "i2", Room: "r2", Persona: "Grill", Account: "home", Status: StatusConflict, TokensIn: 30, TokensOut: 3, CostUSD: cost(1)},
		{Instance: "i1", Room: "r1", Persona: "Grill", Account: "work", Status: StatusOK, TokensIn: 40, TokensOut: 4, CostUSD: cost(0.5)},
	} {
		// Two turns on 1 October, then one a day.
		c.t = start.Add(time.Duration(max(i-1, 0)) * 24 * time.Hour).Add(time.Duration(i) * time.Minute)
		r.TurnID = int64(i + 1)
		if err := d.Record(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
}

func ids(ts []Turn) []int64 {
	out := []int64{}
	for _, t := range ts {
		out = append(out, t.TurnID)
	}
	return out
}

// 003-AC27: the list filters by instance, room, persona, account, status and date range, newest
// first, and a bad date is refused.
func TestFilters(t *testing.T) {
	d, c := open(t)
	seed(t, d, c)
	for _, tc := range []struct {
		f    Filter
		want []int64
	}{
		{Filter{}, []int64{4, 3, 2, 1}},
		{Filter{Instance: "i1"}, []int64{4, 2, 1}},
		{Filter{Room: "r2"}, []int64{3}},
		{Filter{Persona: "Grill"}, []int64{4, 3, 1}},
		{Filter{Account: "work", Status: StatusOK}, []int64{4, 1}},
		{Filter{Status: StatusConflict}, []int64{3}},
		{Filter{From: "2026-10-02"}, []int64{4, 3}},
		{Filter{To: "2026-10-01"}, []int64{2, 1}},
		{Filter{From: "2026-10-02", To: "2026-10-02"}, []int64{3}},
		{Filter{Persona: "Nobody"}, []int64{}},
	} {
		got, err := d.Turns(t.Context(), tc.f, 0)
		if err != nil || !slices.Equal(ids(got), tc.want) {
			t.Errorf("%+v: %v %v, want %v", tc.f, ids(got), err, tc.want)
		}
	}
	if got, _ := d.Turns(t.Context(), Filter{}, 2); !slices.Equal(ids(got), []int64{4, 3}) {
		t.Errorf("limit 2: %v", ids(got))
	}
	for _, bad := range []string{"yesterday", "2026-13-01", "1 OR 1=1"} {
		if _, err := d.Turns(t.Context(), Filter{From: bad}, 0); !errors.Is(err, ErrFilter) {
			t.Errorf("from %q: %v", bad, err)
		}
		if _, err := d.Totals(t.Context(), Filter{To: bad}); !errors.Is(err, ErrFilter) {
			t.Errorf("totals to %q: %v", bad, err)
		}
	}
	f, err := d.Facets(t.Context())
	if err != nil || !slices.Equal(f.Instances, []string{"i1", "i2"}) || !slices.Equal(f.Personas, []string{"Grill", "Scribe"}) ||
		!slices.Equal(f.Accounts, []string{"home", "work"}) || !slices.Equal(f.Statuses, []string{"conflict", "failed", "ok"}) {
		t.Errorf("facets: %+v %v", f, err)
	}
}

// 003-AC27: daily totals per account sum tokens and the known costs, and count the turns with none.
func TestTotals(t *testing.T) {
	d, c := open(t)
	seed(t, d, c)
	got, err := d.Totals(t.Context(), Filter{})
	if err != nil {
		t.Fatal(err)
	}
	want := []Total{
		{Day: "2026-10-03", Account: "work", Turns: 1, TokensIn: 40, TokensOut: 4, CostUSD: 0.5},
		{Day: "2026-10-02", Account: "home", Turns: 1, TokensIn: 30, TokensOut: 3, CostUSD: 1},
		{Day: "2026-10-01", Account: "work", Turns: 2, TokensIn: 30, TokensOut: 3, CostUSD: 0.25, UnknownCost: 1},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("totals:\n%+v\nwant\n%+v", got, want)
	}
	if got, _ := d.Totals(t.Context(), Filter{Account: "home"}); len(got) != 1 || got[0].Account != "home" {
		t.Fatalf("filtered totals: %+v", got)
	}
}

// 003-AC28: a purge drops the turns older than the retention, measured on the clock, and keeps
// the rest; purging again drops nothing.
func TestPurge(t *testing.T) {
	d, c := open(t)
	seed(t, d, c) // turns end 1 Oct 10:00 and 10:01, 2 Oct 10:02, 3 Oct 10:03
	c.t = time.Date(2026, 10, 31, 10, 1, 30, 0, time.UTC)
	n, err := d.Purge(t.Context(), 30*24*time.Hour)
	if err != nil || n != 2 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if got, _ := d.Turns(t.Context(), Filter{}, 0); !slices.Equal(ids(got), []int64{4, 3}) {
		t.Fatalf("kept %v", ids(got))
	}
	if n, _ := d.Purge(t.Context(), 30*24*time.Hour); n != 0 {
		t.Fatalf("second purge dropped %d", n)
	}
	if n, _ := d.Purge(t.Context(), 24*time.Hour); n != 2 {
		t.Fatalf("one-day purge dropped %d", n)
	}
}

// 003-AC29: each persona's context is its latest turn that reported one; a later turn with no
// context (a failure) does not hide it, and a later fresh session replaces it.
func TestContexts(t *testing.T) {
	d, c := open(t)
	rec := func(persona string, tokens, window int64) {
		c.t = c.t.Add(time.Minute)
		if err := d.Record(t.Context(), Turn{Instance: "i", Room: "r", TurnID: c.t.Unix(), Persona: persona, ContextTokens: tokens, ContextWindow: window}); err != nil {
			t.Fatal(err)
		}
	}
	rec("Grill", 100, 1000)
	rec("Grill", 850, 1000)
	rec("Grill", 0, 0)
	rec("Scribe", 900, 1000)
	rec("Scribe", 50, 1000)
	got, err := d.Contexts(t.Context())
	if err != nil || len(got) != 2 || got[0].Persona != "Grill" || got[0].Tokens != 850 || got[1].Persona != "Scribe" || got[1].Tokens != 50 {
		t.Fatalf("contexts: %+v %v", got, err)
	}
}

// 003-AC27: the CSV has a header, empty cells for an unknown cost or context, and defuses texts a
// spreadsheet would run as formulas.
func TestWriteCSV(t *testing.T) {
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	var b bytes.Buffer
	err := WriteCSV(&b, []Turn{
		{At: at, Day: "2026-10-01", Persona: "=HYPERLINK(\"x\")", Cause: "@mention by +Bob", Error: "-1", Status: StatusOK, CostUSD: cost(0.0125),
			ContextTokens: 50, ContextWindow: 200},
		{At: at, Day: "2026-10-01", Persona: "Grill", Status: StatusFailed},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&b).ReadAll()
	if err != nil || len(rows) != 3 {
		t.Fatalf("csv: %v %v", rows, err)
	}
	col := func(row int, name string) string { return rows[row][slices.Index(rows[0], name)] }
	for _, c := range []struct {
		row        int
		name, want string
	}{
		{1, "persona", `'=HYPERLINK("x")`}, {1, "cause", "'@mention by +Bob"}, {1, "error", "'-1"}, {1, "cost_usd", "0.0125"},
		{1, "context_pct", "25"}, {1, "ended_at", "2026-10-01T10:00:00Z"}, {2, "persona", "Grill"}, {2, "cost_usd", ""}, {2, "context_pct", ""},
	} {
		if got := col(c.row, c.name); got != c.want {
			t.Errorf("row %d %s = %q, want %q", c.row, c.name, got, c.want)
		}
	}
}

// The history holds the host's private data: owner-only on disk.
func TestOpenOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file modes do not apply; %LOCALAPPDATA% is the user's own")
	}
	path := filepath.Join(t.TempDir(), "monitor.db")
	d, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Record(t.Context(), Turn{Instance: "i", Room: "r", TurnID: 1}); err != nil {
		t.Fatal(err)
	}
	_ = d.Close()
	for _, p := range []string{path, path + "-wal"} {
		if fi, err := os.Stat(p); err == nil && fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v", p, fi.Mode().Perm())
		}
	}
	// Reopening keeps the schema and the rows.
	d, err = Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close() //nolint:errcheck // test
	if got, _ := d.Turns(t.Context(), Filter{}, 0); len(got) != 1 {
		t.Fatalf("after reopen: %v", got)
	}
}

// 005-AC10: a history from before the model note upgrades in place: its turns stay, with no note,
// and new turns record theirs.
func TestOpenUpgradesV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitor.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), schema+`PRAGMA user_version = 1;
		INSERT INTO turns (at, day, instance, room, turn_id, persona, account, asked_model, asked_effort, model, effort, cause,
		duration_ms, queue_ms, status, error, tokens_in, tokens_out, tokens_cache_read, tokens_cache_write, context_tokens,
		context_window, workspace_bytes) VALUES (1, '2026-10-01', 'i', 'r', 1, 'Grill', 'default', '', '', 'claude-opus-5', '', '',
		0, 0, 'ok', '', 0, 0, 0, 0, 0, 0, 0);`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	for range 2 { // and opens again once upgraded
		d, err := Open(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Record(t.Context(), Turn{Model: "sonnet", ModelNote: "opus 100%", ModelMismatch: true, Status: StatusOK}); err != nil {
			t.Fatal(err)
		}
		ts, err := d.Turns(t.Context(), Filter{}, 0)
		_ = d.Close()
		if err != nil || ts[len(ts)-1].Model != "claude-opus-5" || ts[len(ts)-1].ModelNote != "" || !ts[0].ModelMismatch || ts[0].ModelNote != "opus 100%" {
			t.Fatalf("turns %+v: %v", ts, err)
		}
	}
}

// An upgrade that fails leaves the history at its version: no half-applied schema.
func TestOpenUpgradeFailsWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitor.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// Version 1 with a turns table that already has one of the new columns: the second ALTER fails.
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE turns (id INTEGER PRIMARY KEY, model_mismatch INTEGER); PRAGMA user_version = 1;`); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, nil); err == nil {
		t.Fatal("upgrade of a broken history passed")
	}
	var version int
	var cols string
	if err := db.QueryRowContext(t.Context(), `SELECT (SELECT user_version FROM pragma_user_version), group_concat(name, ' ') FROM pragma_table_info('turns')`).Scan(&version, &cols); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if version != 1 || cols != "id model_mismatch" {
		t.Fatalf("after a failed upgrade: version %d, columns %q", version, cols)
	}
}
