package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/monitor"
	"github.com/babs/grill-room/internal/sandbox"
)

// 003-AC27: a turn's end is sorted by its error into ok, cut off, conflict, expired or failed.
func TestTurnStatus(t *testing.T) {
	for err, want := range map[string]string{
		"":                monitor.StatusOK,
		errCutOff.Error(): monitor.StatusCutOff,
		"the room refused the result: conflict on docs/synthesis.md": monitor.StatusConflict,
		"conflict on docs/a.md":               monitor.StatusConflict,
		"the turn timed out after 15m0s":      monitor.StatusExpired,
		"the host's Claude login has expired": monitor.StatusExpired,
		"could not fetch the workspace: EOF":  monitor.StatusFailed,
		"the turn was abandoned":              monitor.StatusFailed,
	} {
		if got := turnStatus(err); got != want {
			t.Errorf("turnStatus(%q) = %q, want %q", err, got, want)
		}
	}
}

func kinds(as []Alert) []string {
	out := []string{}
	for _, a := range as {
		out = append(out, a.Kind)
	}
	return out
}

// 003-AC29: each alert fires on its own condition and only then: a login with a warning, a
// running unsigned worker, a served persona past 80% of its context, a turn queued over 10 minutes.
func TestAlerts(t *testing.T) {
	inst := InstanceView{ID: "i1", URL: "https://rooms.example", Rooms: []string{"https://rooms.example/r/a"}, Version: "v0.0.9"}
	ctx := func(room string, tokens int64) monitor.Context {
		return monitor.Context{Instance: "i1", Room: room, Persona: "Grill", Tokens: tokens, Window: 1000}
	}
	queued := func(ms int64) ConcurrencyView {
		return ConcurrencyView{Queued: []SlotView{{Instance: "i1", Room: "https://rooms.example/r/a", SinceMS: ms}}}
	}
	unsigned := inst
	unsigned.Unsigned = true
	for _, tc := range []struct {
		name     string
		accounts []AccountView
		insts    []InstanceView
		c        ConcurrencyView
		ctxs     []monitor.Context
		want     []string
		text     string
	}{
		{name: "none", insts: []InstanceView{inst}, ctxs: []monitor.Context{ctx("https://rooms.example/r/a", 800)}, c: queued(10 * 60e3), want: []string{}},
		{name: "account", accounts: []AccountView{{Account: Account{Name: "work"}, Warning: "the access token expires in 20m0s"}, {Account: Account{Name: "home"}}},
			want: []string{"account"}, text: "Claude account work: the access token expires in 20m0s."},
		{name: "unsigned", insts: []InstanceView{unsigned}, want: []string{"unsigned"}, text: "The app for https://rooms.example runs an unsigned binary (v0.0.9)."},
		{name: "context", insts: []InstanceView{inst}, ctxs: []monitor.Context{ctx("https://rooms.example/r/a", 801)}, want: []string{"context"},
			text: "Grill in https://rooms.example/r/a uses 80% of its context"},
		{name: "context in a room no longer served", insts: []InstanceView{inst}, ctxs: []monitor.Context{ctx("https://rooms.example/r/gone", 950)}, want: []string{}},
		{name: "queue", c: queued(10*60e3 + 1000), want: []string{"queue"}, text: "An answer in https://rooms.example/r/a has waited 10 minutes for a slot"},
	} {
		got := alerts(tc.accounts, tc.insts, tc.c, tc.ctxs)
		if !slices.Equal(kinds(got), tc.want) || (tc.text != "" && !strings.Contains(got[0].Text, tc.text)) {
			t.Errorf("%s: %+v", tc.name, got)
		}
	}
}

func openMonitor(t *testing.T, now func() time.Time) *monitor.DB {
	t.Helper()
	m, err := monitor.Open(filepath.Join(t.TempDir(), "monitor.db"), now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// 003-AC27: the supervisor records each turn its worker ends, with the room's account, sorted
// status and the worker's figures; a turn's start records nothing.
func TestSupervisorRecordsTurns(t *testing.T) {
	m := openMonitor(t, nil)
	room := Room{Base: "https://rooms.example", ID: "abc"}
	s := testSupervisor(t, "turns", nil, func(o *SupervisorOptions) { o.Monitor = m })
	s.SetRoom(context.Background(), room)
	var got []monitor.Turn
	waitFor(t, "two turns recorded", func() bool {
		got, _ = m.Turns(context.Background(), monitor.Filter{}, 0)
		return len(got) == 2
	})
	cut, ok := got[0], got[1]
	if ok.TurnID == 2 {
		cut, ok = ok, cut
	}
	id := instance(t, s).ID
	if ok.Instance != id || ok.Room != room.String() || ok.Account != "default" || ok.Persona != "Grill" || ok.Status != monitor.StatusOK ||
		ok.Cause != "pressed by Alice" || ok.AskedModel != "opus" || ok.Model != "claude-sonnet-fake" || ok.Effort != "high" ||
		ok.DurationMS != 1500 || ok.QueueMS != 200 || ok.TokensIn != 5 || ok.TokensOut != 6 || ok.CacheRead != 7 || ok.CacheWrite != 8 ||
		ok.CostUSD == nil || *ok.CostUSD != 0.25 || ok.ContextTokens != 90 || ok.ContextWindow != 100 || ok.WorkspaceBytes != 4096 {
		t.Fatalf("ok turn %+v", ok)
	}
	if cut.Status != monitor.StatusCutOff || cut.Persona != "Scribe" || cut.CostUSD != nil {
		t.Fatalf("cut-off turn %+v", cut)
	}
	// The context alert names the persona of the room this supervisor serves.
	insts, _ := s.Instances()
	if a := s.Alerts(context.Background(), nil, insts, ConcurrencyView{}); !slices.Equal(kinds(a), []string{"context"}) || !strings.Contains(a[0].Text, "Grill in "+room.String()+" uses 90%") {
		t.Fatalf("alerts %+v", a)
	}
}

// 003-AC28: the supervisor purges the history past its retention at start, and again when the
// page shortens it; the retention survives a restart.
func TestSupervisorPurges(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	clock := now.Add(-31 * 24 * time.Hour)
	m := openMonitor(t, func() time.Time { return clock })
	ctx := context.Background()
	for i, age := range []time.Duration{31 * 24 * time.Hour, 10 * 24 * time.Hour, time.Hour} {
		clock = now.Add(-age)
		if err := m.Record(ctx, monitor.Turn{Instance: "i", Room: "r", TurnID: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	clock = now
	left := func() int { ts, _ := m.Turns(ctx, monitor.Filter{}, 0); return len(ts) }
	dir := t.TempDir()
	s := testSupervisor(t, "turns", nil, func(o *SupervisorOptions) { o.Monitor = m; o.Paths = Paths{Config: dir, State: dir, Cache: dir} })
	waitFor(t, "the 31-day-old turn purged at start", func() bool { return left() == 2 })
	if s.RetentionDays() != DefaultRetentionDays {
		t.Fatalf("retention %d", s.RetentionDays())
	}
	for _, bad := range []int{0, -1, MaxRetentionDays + 1} {
		if err := s.SetRetention(bad); err == nil {
			t.Fatalf("retention %d accepted", bad)
		}
	}
	if err := s.SetRetention(7); err != nil {
		t.Fatal(err)
	}
	if n := left(); n != 1 {
		t.Fatalf("%d turns left after a 7-day retention", n)
	}
	if set, err := loadSupervisorSettings(s.o.Paths.supervisorSettings()); err != nil || set.retentionDays() != 7 {
		t.Fatalf("saved %+v %v", set, err)
	}
}

// 003-AC26: the page sets one instance's sandbox limits: refused when docker would not take them,
// saved in instance.json, and the worker restarts with them in its config.
func TestSupervisorSetLimits(t *testing.T) {
	s := testSupervisor(t, strconv.Itoa(Proto), nil, func(o *SupervisorOptions) {
		o.Worker.Limits = sandbox.Limits{CPUs: "2", Memory: "4g"}
	})
	s.SetRoom(context.Background(), Room{Base: "https://rooms.example", ID: "abc"})
	// The worker's first status report lands after it shows as running.
	waitFor(t, "the worker running and reporting", func() bool {
		in := instance(t, s)
		return in.State == "running" && in.Status != nil
	})
	in := instance(t, s)
	if in.Limits.CPUs != "2" || in.Status == nil || in.Status.Room != "https://rooms.example/r/abc" {
		t.Fatalf("instance %+v", in)
	}
	for _, bad := range []sandbox.Limits{{CPUs: "two"}, {Memory: "4 GB"}, {EgressCPUs: "-1"}, {EgressMemory: "1g; rm -rf /"}} {
		if err := s.SetLimits(in.ID, bad); err == nil {
			t.Fatalf("limits %+v accepted", bad)
		}
	}
	if err := s.SetLimits("nope", sandbox.Limits{}); err == nil {
		t.Fatal("limits for an unknown instance accepted")
	}
	want := sandbox.Limits{CPUs: "1", Memory: "2g", EgressCPUs: "0.25", EgressMemory: "128m"}
	if err := s.SetLimits(in.ID, want); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the worker restarted with the limits", func() bool {
		return slices.ContainsFunc(s.o.Ring.Lines(), func(l string) bool {
			return strings.Contains(l, `{"cpus":"1","egress_cpus":"0.25","egress_memory":"128m","memory":"2g"}`)
		})
	})
	if got := instance(t, s).Limits; got != want {
		t.Fatalf("view limits %+v", got)
	}
	b, err := os.ReadFile(filepath.Join(s.o.Paths.InstanceConfig(in.ID), instanceFile))
	var saved Instance
	if err == nil {
		err = json.Unmarshal(b, &saved)
	}
	if err != nil || saved.Limits == nil || *saved.Limits != want {
		t.Fatalf("instance.json %s %v", b, err)
	}
	if _, err := s.InstanceOps("nope"); err == nil {
		t.Fatal("ops for an unknown instance")
	}
	ops, err := s.InstanceOps(in.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the worker back", func() bool { return instance(t, s).State == "running" })
	if err := ops.SetSettings(Settings{MaxModel: "sonnet"}); err != nil {
		t.Fatal(err)
	}
}

// 003-AC26 003-AC27 003-AC28 003-AC29: the page's API lists, filters and exports the history,
// returns the alerts and retention with the status, and routes per-instance changes by id.
func TestSupervisorPageMonitoring(t *testing.T) {
	m := openMonitor(t, nil)
	s := testSupervisor(t, "turns", nil, func(o *SupervisorOptions) { o.Monitor = m })
	e := newAdminEnv(t, s)
	tok := e.login(t)
	s.SetRoom(context.Background(), Room{Base: "https://rooms.example", ID: "abc"})
	waitFor(t, "two turns recorded", func() bool { ts, _ := m.Turns(context.Background(), monitor.Filter{}, 0); return len(ts) == 2 })
	id := instance(t, s).ID

	_, body := e.req(t, "GET", "/api/status", tok, "", nil)
	var st struct {
		Alerts        []Alert `json:"alerts"`
		RetentionDays int     `json:"retention_days"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil || st.RetentionDays != 30 || !slices.Equal(kinds(st.Alerts), []string{"context"}) {
		t.Fatalf("status %+v %v", st, err)
	}
	var h struct {
		Turns  []monitor.Turn  `json:"turns"`
		Totals []monitor.Total `json:"totals"`
		Facets monitor.Facets  `json:"facets"`
	}
	get := func(q string) int {
		code, body := e.req(t, "GET", "/api/monitor/turns"+q, tok, "", nil)
		h.Turns = nil
		_ = json.Unmarshal([]byte(body), &h)
		return code
	}
	if code := get(""); code != http.StatusOK || len(h.Turns) != 2 || len(h.Totals) != 1 || h.Totals[0].Turns != 2 || h.Totals[0].UnknownCost != 1 ||
		!slices.Equal(h.Facets.Statuses, []string{"cut off", "ok"}) {
		t.Fatalf("history %d %+v", code, h)
	}
	if code := get("?status=cut+off&instance=" + id); code != http.StatusOK || len(h.Turns) != 1 || h.Turns[0].Persona != "Scribe" {
		t.Fatalf("filtered %d %+v", code, h.Turns)
	}
	if code := get("?persona=Grill&account=work"); code != http.StatusOK || len(h.Turns) != 0 {
		t.Fatalf("no match %d %+v", code, h.Turns)
	}
	if code := get("?from=last+week"); code != http.StatusBadRequest {
		t.Fatalf("bad date: %d", code)
	}
	code, csv := e.req(t, "GET", "/api/monitor/turns.csv?persona=Grill", tok, "", nil)
	if lines := strings.Split(strings.TrimSpace(csv), "\n"); code != http.StatusOK || len(lines) != 2 || !strings.HasPrefix(lines[0], "ended_at,") ||
		!strings.Contains(lines[1], ",Grill,default,") {
		t.Fatalf("csv %d %q", code, csv)
	}
	if code, _ := e.req(t, "GET", "/api/monitor/turns.csv", "", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("csv without a session: %d", code)
	}

	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/api/retention", `{"days":0}`, http.StatusBadRequest},
		{"POST", "/api/retention", `{"days":7}`, http.StatusNoContent},
		{"POST", "/api/limits", `{"instance":"` + id + `","memory":"lots"}`, http.StatusBadRequest},
		{"POST", "/api/settings", `{"instance":"nope","max_model":"sonnet"}`, http.StatusBadRequest},
		{"POST", "/api/settings", `{"instance":"` + id + `","max_model":"sonnet"}`, http.StatusNoContent},
		{"POST", "/api/allowlist", `{"instance":"nope","allowlist":[]}`, http.StatusBadRequest},
		{"POST", "/api/allowlist", `{"instance":"` + id + `","allowlist":["pypi.org"]}`, http.StatusNoContent},
		{"POST", "/api/recheck", `{"instance":"nope"}`, http.StatusBadRequest},
		{"POST", "/api/recheck", `{"instance":"` + id + `"}`, http.StatusNoContent},
		{"GET", "/api/denied?instance=nope", "", http.StatusBadRequest},
		{"GET", "/api/denied?instance=" + id, "", http.StatusOK},
	} {
		if code, b := e.req(t, c.method, c.path, tok, c.body, nil); code != c.want {
			t.Errorf("%s %s %s: %d %s", c.method, c.path, c.body, code, b)
		}
	}
	if s.RetentionDays() != 7 {
		t.Fatalf("retention %d", s.RetentionDays())
	}
}

// 009-AC19 009-AC22: the page's plugin requests reach the worker of the room's instance, named by
// the request or found from the room's link; a worker from before protocol 4 is refused them
// with why, and a link or an instance the supervisor does not serve is a 400.
func TestSupervisorPluginRequests(t *testing.T) {
	modes := []string{"plugins"}
	// Since protocol 5 a worker before 4 fails the handshake: it never gets this far.
	if protoPlugins-1 >= Proto-ProtoSkew {
		modes = append(modes, strconv.Itoa(protoPlugins-1))
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			s := testSupervisor(t, mode, nil)
			e := newAdminEnv(t, s)
			tok := e.login(t)
			s.SetRoom(context.Background(), Room{Base: "https://rooms.example", ID: "abc"})
			waitFor(t, "the worker running", func() bool { return instance(t, s).State == "running" })
			id := instance(t, s).ID
			ok := http.StatusNoContent
			if mode != "plugins" {
				ok = http.StatusBadRequest
			}
			const room = `"room":"https://rooms.example/r/abc"`
			for _, c := range []struct {
				path, body string
				want       int
			}{
				{"/api/plugin-options", `{"instance":"nope",` + room + `,"plugin":1,"values":{}}`, http.StatusBadRequest},
				{"/api/plugin-options", `{"instance":"` + id + `",` + room + `,"plugin":1,"values":{"k":"v"}}`, ok},
				{"/api/plugin-options", `{` + room + `,"plugin":1,"values":{}}`, ok},
				{"/api/plugin-options", `{"room":"not a link","plugin":1}`, http.StatusBadRequest},
				{"/api/plugin-options", `{"room":"https://other.example/r/x","plugin":1}`, http.StatusBadRequest},
				{"/api/plugin-fetch", `{"instance":"` + id + `",` + room + `,"id":3,"fetch":true}`, ok},
				{"/api/plugin-fetch", `{` + room + `,"id":3}`, ok},
				{"/api/plugin-fetch", `{"room":"not a link","id":3}`, http.StatusBadRequest},
			} {
				code, b := e.req(t, "POST", c.path, tok, c.body, nil)
				if code != c.want {
					t.Errorf("%s %s: %d %s", c.path, c.body, code, b)
				}
				if mode != "plugins" && strings.Contains(c.body, "rooms.example") && !strings.Contains(b, "too old") && !strings.Contains(c.body, "nope") {
					t.Errorf("%s %s: %s", c.path, c.body, b)
				}
			}
		})
	}
}

// 014-AC1 014-AC15: the switches and a room entry's removal reach a worker at protocol 6; one from
// before is refused them with why.
func TestSupervisorEgressSwitches(t *testing.T) {
	for _, mode := range []string{strconv.Itoa(Proto), strconv.Itoa(protoEgress - 1)} {
		t.Run(mode, func(t *testing.T) {
			s := testSupervisor(t, mode, nil)
			e := newAdminEnv(t, s)
			tok := e.login(t)
			s.SetRoom(context.Background(), Room{Base: "https://rooms.example", ID: "abc"})
			waitFor(t, "the worker running", func() bool { return instance(t, s).State == "running" })
			for _, req := range [][2]string{{"/api/egress-switches", `"all_websites":true`}, {"/api/room-allowlist-remove", `"room":"https://rooms.example/r/abc","domain":"x.example"`}} {
				code, b := e.req(t, "POST", req[0], tok, `{"instance":"`+instance(t, s).ID+`",`+req[1]+`}`, nil)
				if mode == strconv.Itoa(Proto) && code != http.StatusNoContent || mode != strconv.Itoa(Proto) && (code != http.StatusBadRequest || !strings.Contains(b, "too old")) {
					t.Fatalf("%s: %d %s", req[0], code, b)
				}
			}
		})
	}
}
