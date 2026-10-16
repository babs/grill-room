package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/babs/grill-room/internal/sandbox"
	"github.com/babs/grill-room/internal/store"
)

// 002-AC36: the page edits the egress allowlist; a bad list is refused and changes nothing; the
// saved list survives a restart, and the default applies while the page saved none. With no
// sandbox yet the denied list is empty.
func TestAdminAllowlist(t *testing.T) {
	d := newTestDaemon(t, nil)
	d.o.AllowlistPath = filepath.Join(t.TempDir(), "state", "allowlist.json")
	e := newAdminEnv(t, d)
	tok := e.login(t)
	if !slices.Equal(d.Status().Allowlist, sandbox.DefaultAllowlist) {
		t.Fatalf("initial allowlist = %v", d.Status().Allowlist)
	}
	for _, path := range []string{"/api/allowlist", "/api/denied"} {
		method := "POST"
		if path == "/api/denied" {
			method = "GET"
		}
		if code, _ := e.req(t, method, path, "", `{"allowlist":[]}`, nil); code != http.StatusUnauthorized {
			t.Fatalf("%s without a session: %d", path, code)
		}
	}
	for _, bad := range []string{`{"allowlist":["*.example.com"]}`, `{"allowlist":["api.anthropic.com"]}`, `{"allowlist":["a b"]}`, `{}`, `{"allowlist":"pypi.org"}`, `nope`} {
		if code, body := e.req(t, "POST", "/api/allowlist", tok, bad, nil); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", bad, code, body)
		}
	}
	if !slices.Equal(d.Status().Allowlist, sandbox.DefaultAllowlist) {
		t.Fatalf("a refused list applied: %v", d.Status().Allowlist)
	}
	if _, err := os.Stat(d.o.AllowlistPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused list was saved: %v", err)
	}
	if code, body := e.req(t, "POST", "/api/allowlist", tok, `{"allowlist":["Example.com",".example.org","www.example.org"]}`, nil); code != http.StatusNoContent {
		t.Fatalf("allowlist: %d %s", code, body)
	}
	want := []string{".example.org", "example.com"}
	code, body := e.req(t, "GET", "/api/status", tok, "", nil)
	var st struct {
		Status Status `json:"status"`
	}
	if err := json.Unmarshal([]byte(body), &st); code != http.StatusOK || err != nil || !slices.Equal(st.Status.Allowlist, want) {
		t.Fatalf("status: %d %s", code, body)
	}
	if got, err := LoadEgress(d.o.AllowlistPath); err != nil || !slices.Equal(domains(got.Server), want) {
		t.Fatalf("after a restart: %v %v", got, err)
	}
	assertPerm(t, d.o.AllowlistPath, 0o600)
	// Removing every domain is a choice, kept as such.
	if code, body := e.req(t, "POST", "/api/allowlist", tok, `{"allowlist":[]}`, nil); code != http.StatusNoContent {
		t.Fatalf("empty allowlist: %d %s", code, body)
	}
	if got, err := LoadEgress(d.o.AllowlistPath); err != nil || got.Server == nil || len(got.Server) != 0 {
		t.Fatalf("empty list after a restart: %#v %v", got, err)
	}
	if got, err := LoadEgress(filepath.Join(t.TempDir(), "none.json")); err != nil || !slices.Equal(domains(got.Server), sandbox.DefaultAllowlist) || got.AllWebsites {
		t.Fatalf("no file: %v %v", got, err)
	}
	for name, content := range map[string]string{"corrupt": "[", "bad entry": `["*.x.com"]`, "bad object entry": `{"server":[{"domain":"api.anthropic.com"}]}`,
		"bad room entry": `{"server":[],"rooms":{"r":[{"domain":"10.0.0.1"}]}}`} {
		p := filepath.Join(t.TempDir(), "allowlist.json")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadEgress(p); err == nil {
			t.Errorf("%s file loaded", name)
		}
	}
	code, body = e.req(t, "GET", "/api/denied", tok, "", nil)
	if code != http.StatusOK || strings.TrimSpace(body) != `{"denied":[],"hits":{},"room_hits":{}}` {
		t.Fatalf("denied with no sandbox: %d %s", code, body)
	}
}

// 002-AC34 002-AC36: the sandbox starts with the list saved before its first turn, a later save
// reaches the running sandbox, and the denied list is the sandbox's.
func TestDaemonAllowlistReachesSandbox(t *testing.T) {
	rs := newRoomServer(t)
	ag := &fakeAgent{}
	ag.run = func(_ context.Context, in sandbox.TurnInput, _ map[string]string) (*sandbox.TurnOutput, error) {
		return &sandbox.TurnOutput{Result: &sandbox.Result{Result: "ok", SessionID: "33333333-3333-4333-8333-333333333333"}, Session: []byte("x\n"),
			SessionOffset: in.SessionSize}, nil
	}
	alice, d, _ := pairedDaemon(t, rs, "Alice", ag)
	ctx := context.Background()
	if got, err := d.Egress(ctx, 5); err != nil || len(got.Denied) != 0 || len(got.Hits) != 0 {
		t.Fatalf("denied before the first turn: %v %v", got, err)
	}
	if err := d.SetAllowlist(ctx, []string{"pypi.org"}); err != nil {
		t.Fatal(err)
	}
	var started store.Turn
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/turns", alice, map[string]int64{"persona_id": rs.grill(t).ID}, http.StatusCreated, &started)
	waitFor(t, "turn done", func() bool { v := rs.agent(t, alice); return v.Turn.ID == started.ID && v.Turn.Status != "running" })
	ag.mu.Lock()
	first := ag.allow
	ag.mu.Unlock()
	if !slices.Equal(first, []string{"pypi.org"}) {
		t.Fatalf("sandbox started with %v", first)
	}
	if err := d.SetAllowlist(ctx, []string{"github.com", "pypi.org"}); err != nil {
		t.Fatal(err)
	}
	ag.mu.Lock()
	second := ag.allow
	ag.mu.Unlock()
	if !slices.Equal(second, []string{"github.com", "pypi.org"}) {
		t.Fatalf("running sandbox has %v", second)
	}
	// 003b-AC16 014-AC3: the hits come with the denied list, by host and by room.
	if got, err := d.Egress(ctx, 5); err != nil || len(got.Denied) != 1 || got.Denied[0].Host != "example.com" || got.Hits["pypi.org"] != 3 ||
		got.RoomHits[d.Status().Rooms[0].Room]["pypi.org"] != 3 {
		t.Fatalf("denied = %v %v", got, err)
	}
}

// 002-AC36: a sandbox that cannot take the list fails its turn rather than run with no list; a
// running one that refuses a new list, or whose log cannot be read, is reported on the page as a
// failure of the sandbox, not of the request; a list that cannot be saved is not applied.
func TestDaemonAllowlistFailures(t *testing.T) {
	rs := newRoomServer(t)
	ag := &fakeAgent{allowErr: errors.New("squid is down")}
	ag.run = func(_ context.Context, in sandbox.TurnInput, _ map[string]string) (*sandbox.TurnOutput, error) {
		return &sandbox.TurnOutput{Result: &sandbox.Result{Result: "ok", SessionID: "33333333-3333-4333-8333-333333333333"}, Session: []byte("x\n"),
			SessionOffset: in.SessionSize}, nil
	}
	alice, d, _ := pairedDaemon(t, rs, "Alice", ag)
	press := func() (status, errText string) {
		t.Helper()
		var started store.Turn
		rs.call(t, "POST", "/api/rooms/"+rs.room+"/turns", alice, map[string]int64{"persona_id": rs.grill(t).ID}, http.StatusCreated, &started)
		waitFor(t, "turn ended", func() bool { v := rs.agent(t, alice); return v.Turn.ID == started.ID && v.Turn.Status != "running" })
		tr := rs.agent(t, alice).Turn
		return tr.Status, tr.Error
	}
	if st, msg := press(); st != "failed" || !strings.Contains(msg, "egress allowlist: squid is down") {
		t.Fatalf("turn with a sandbox refusing the list: %s %q", st, msg)
	}
	ag.mu.Lock()
	ag.allowErr = nil
	ag.mu.Unlock()
	if st, msg := press(); st != "done" {
		t.Fatalf("turn: %s %q", st, msg)
	}
	ag.mu.Lock()
	ag.allowErr, ag.deniedErr = errors.New("squid is down"), errors.New("no log")
	ag.mu.Unlock()
	e := newAdminEnv(t, d)
	tok := e.login(t)
	if code, body := e.req(t, "POST", "/api/allowlist", tok, `{"allowlist":["pypi.org"]}`, nil); code != http.StatusBadGateway || !strings.Contains(body, "kept its old list: squid is down") {
		t.Fatalf("refused by the sandbox: %d %s", code, body)
	}
	if code, body := e.req(t, "GET", "/api/denied", tok, "", nil); code != http.StatusBadGateway || !strings.Contains(body, "no log") {
		t.Fatalf("denied with no log: %d %s", code, body)
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d.o.AllowlistPath = filepath.Join(blocker, "allowlist.json")
	if err := d.SetAllowlist(context.Background(), []string{"github.com"}); err == nil || !strings.Contains(err.Error(), "save allowlist") {
		t.Fatalf("unsaveable list: %v", err)
	}
	if slices.Contains(d.Status().Allowlist, "github.com") {
		t.Fatalf("an unsaved list applied: %v", d.Status().Allowlist)
	}
}

// okTurn answers every turn with a short synthesis.
func okTurn(_ context.Context, in sandbox.TurnInput, _ map[string]string) (*sandbox.TurnOutput, error) {
	return &sandbox.TurnOutput{Result: &sandbox.Result{Result: "ok", SessionID: "33333333-3333-4333-8333-333333333333"}, Session: []byte("x\n"),
		SessionOffset: in.SessionSize}, nil
}

// pressGrill starts a turn of the room's Grill and waits until it ended.
func pressGrill(t *testing.T, rs *roomServer, tok string) {
	t.Helper()
	var started store.Turn
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/turns", tok, map[string]int64{"persona_id": rs.grill(t).ID}, http.StatusCreated, &started)
	waitFor(t, "turn ended", func() bool { v := rs.agent(t, tok); return v.Turn.ID == started.ID && v.Turn.Status != "running" })
}

// 014-AC1 014-AC26: the page's switches reach the running sandboxes and survive a restart; the
// local network never comes on without every website, and goes off with it.
func TestEgressSwitches(t *testing.T) {
	rs := newRoomServer(t)
	ag := &fakeAgent{run: okTurn}
	alice, d, _ := pairedDaemon(t, rs, "Alice", ag, func(d *Daemon) { d.o.AllowlistPath = filepath.Join(t.TempDir(), "allowlist.json") })
	pressGrill(t, rs, alice)
	e := newAdminEnv(t, d)
	tok := e.login(t)
	policy := func() sandbox.Policy { ag.mu.Lock(); defer ag.mu.Unlock(); return ag.policy }
	for _, tc := range []struct {
		body       string
		all, local bool
	}{
		{`{"local_network":true}`, false, false},
		{`{"all_websites":true}`, true, false},
		{`{"all_websites":true,"local_network":true}`, true, true},
		{`{"local_network":true}`, false, false},
	} {
		if code, body := e.req(t, "POST", "/api/egress-switches", tok, tc.body, nil); code != http.StatusNoContent {
			t.Fatalf("%s: %d %s", tc.body, code, body)
		}
		if p := policy(); p.AllWebsites != tc.all || p.LocalNetwork != tc.local || !slices.Equal(p.Allow, sandbox.DefaultAllowlist) {
			t.Fatalf("%s: running sandbox has %+v", tc.body, p)
		}
		st, err := LoadEgress(d.o.AllowlistPath)
		if err != nil || st.AllWebsites != tc.all || st.LocalNetwork != tc.local || !slices.Equal(domains(st.Server), sandbox.DefaultAllowlist) {
			t.Fatalf("%s: after a restart %+v %v", tc.body, st, err)
		}
		if e := d.Status().Egress; e.AllWebsites != tc.all || e.LocalNetwork != tc.local {
			t.Fatalf("%s: status %+v", tc.body, e)
		}
	}
	if code, _ := e.req(t, "POST", "/api/egress-switches", "", `{"all_websites":true}`, nil); code != http.StatusUnauthorized {
		t.Fatalf("switches without a session: %d", code)
	}
	// 014-AC29: the hold time, kept within bounds, survives a restart and a switch saved without it.
	for _, c := range []struct {
		body string
		want int
	}{{`{"hold_seconds":50}`, 50}, {`{"hold_seconds":99}`, HoldMax}, {`{"all_websites":false}`, HoldMax}} {
		if code, b := e.req(t, "POST", "/api/egress-switches", tok, c.body, nil); code != http.StatusNoContent {
			t.Fatalf("%s: %d %s", c.body, code, b)
		}
		if st, err := LoadEgress(d.o.AllowlistPath); err != nil || st.HoldSeconds != c.want {
			t.Fatalf("%s: saved %d %v", c.body, st.HoldSeconds, err)
		}
	}
	// 014-AC15: the page removes a room's entry; one not on the room's list is an error.
	link := d.Status().Rooms[0].Room
	if err := d.AllowEgress(t.Context(), link, "docs.rs", "room", "Alice"); err != nil {
		t.Fatal(err)
	}
	if code, b := e.req(t, "POST", "/api/room-allowlist-remove", tok, `{"room":"`+link+`","domain":"docs.rs"}`, nil); code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", code, b)
	}
	if code, b := e.req(t, "POST", "/api/room-allowlist-remove", tok, `{"room":"`+link+`","domain":"docs.rs"}`, nil); code != http.StatusBadRequest || !strings.Contains(b, "not on this room") {
		t.Fatalf("remove again: %d %s", code, b)
	}
	if p := policy(); slices.Contains(p.Allow, "docs.rs") {
		t.Fatalf("removed entry still on the sandbox: %+v", p)
	}
}

// 014: allowlist.json from before 014 holds a bare list, loaded as the server's entries; the
// object form round-trips with who added an entry; a page save keeps that for the domains it keeps.
func TestLoadEgressForms(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "legacy.json")
	if err := os.WriteFile(legacy, []byte(`["PyPI.org","github.com"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, err := LoadEgress(legacy); err != nil || !slices.Equal(domains(st.Server), []string{"github.com", "pypi.org"}) || st.AllWebsites || st.Rooms != nil {
		t.Fatalf("legacy: %+v %v", st, err)
	}
	at := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	want := EgressState{AllWebsites: true, LocalNetwork: true, Server: []AllowEntry{{Domain: "example.com", By: "Alice", At: &at}, {Domain: "pypi.org"}},
		Rooms: map[string][]AllowEntry{"https://x/r/a": {{Domain: "docs.rs", By: "Bob", At: &at}}}}
	d := newTestDaemon(t, nil)
	d.o.AllowlistPath = filepath.Join(dir, "allowlist.json")
	d.settingsMu.Lock()
	err := d.setEgress(context.Background(), want)
	d.settingsMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := LoadEgress(d.o.AllowlistPath); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if err := d.SetAllowlist(context.Background(), []string{"example.com", "github.com"}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadEgress(d.o.AllowlistPath)
	if err != nil || len(got.Server) != 2 || got.Server[0].By != "Alice" || !got.Server[0].At.Equal(at) || got.Server[1].By != "" || !reflect.DeepEqual(got.Rooms, want.Rooms) || !got.AllWebsites {
		t.Fatalf("after a page save: %+v %v", got, err)
	}
	// A local network saved without every website does not load on.
	half := filepath.Join(dir, "half.json")
	if err := os.WriteFile(half, []byte(`{"local_network":true,"server":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, err := LoadEgress(half); err != nil || st.LocalNetwork {
		t.Fatalf("half: %+v %v", st, err)
	}
}

// 014-AC3: after a turn the daemon logs one egress line per request its sandbox's proxy saw since
// the turn ran, with room, persona, host and verdict; the worker's logger adds the server.
func TestEgressLogLines(t *testing.T) {
	rs := newRoomServer(t)
	at := time.Unix(1791690692, 0).UTC()
	ag := &fakeAgent{run: okTurn, requests: []sandbox.EgressRequest{
		{Host: "pypi.org", Method: "CONNECT", Allowed: true, Time: at}, {Host: "example.com", Method: "CONNECT", Time: at}}}
	core, logs := observer.New(zap.InfoLevel)
	alice, d, _ := pairedDaemon(t, rs, "Alice", ag, func(d *Daemon) { d.o.Log = zap.New(core) })
	before := time.Now()
	pressGrill(t, rs, alice)
	waitFor(t, "egress lines", func() bool { return logs.FilterMessage("egress").Len() == 2 })
	room := d.Status().Rooms[0].Room
	for i, want := range []map[string]any{
		{"room": room, "persona": "Grill", "host": "pypi.org", "method": "CONNECT", "verdict": "allowed"},
		{"room": room, "persona": "Grill", "host": "example.com", "method": "CONNECT", "verdict": "denied"},
	} {
		if got := logs.FilterMessage("egress").All()[i].ContextMap(); !reflect.DeepEqual(got, want) {
			t.Errorf("line %d = %v, want %v", i, got, want)
		}
	}
	ag.mu.Lock()
	since := ag.since
	ag.mu.Unlock()
	if len(since) != 1 || since[0].Before(before) {
		t.Fatalf("requests asked since %v, turn pressed at %v", since, before)
	}
}

// 014-AC8: a turn's refusals give the room each domain once; IPs, Anthropic hosts, other ports and
// methods, and tunnels let through give none.
func TestBlockedDomains(t *testing.T) {
	q := func(host, method string, allowed bool) sandbox.EgressRequest {
		return sandbox.EgressRequest{Host: host, Method: method, Allowed: allowed}
	}
	got := blockedDomains([]sandbox.EgressRequest{
		q("example.com", "CONNECT", false), q("pypi.org", "CONNECT", true), q("example.com", "CONNECT", false),
		q("10.0.0.1", "CONNECT", false), q("[::1]", "CONNECT", false), q("api.anthropic.com", "CONNECT", false),
		q("platform.claude.com", "CONNECT", false), q("example.org:8443", "CONNECT", false), q("plain.example", "GET", false),
		q("docs.rs", "CONNECT", false), q("code.claude.com", "CONNECT", false),
	})
	if want := []string{"example.com", "docs.rs", "code.claude.com"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// 014-AC10 014-AC11 014-AC15 014-AC16 014-AC17: the host approves a refused website from the room;
// a room's approval reaches its own sandboxes only, an everywhere one every room's; the page's
// removal applies at once and the room hears of it; a room's list holds 100 entries; a removed
// room's list goes; the lists survive a restart; the plugin assist counts the room's list.
func TestRoomEgressLists(t *testing.T) {
	rs := newRoomServer(t)
	desk := map[string]*fakeAgent{}
	var deskMu sync.Mutex
	d := newTestDaemon(t, dockerUp)
	d.o.AllowlistPath = filepath.Join(t.TempDir(), "allowlist.json")
	d.o.NewAgent = func(r Room, _ int, _, _, _ string) Agent {
		deskMu.Lock()
		defer deskMu.Unlock()
		ag := &fakeAgent{run: okTurn}
		// The first room's sandbox, A's, was refused two websites.
		if len(desk) == 0 {
			ag.requests = []sandbox.EgressRequest{{Host: "example.com", Method: "CONNECT"}, {Host: "docs.rs", Method: "CONNECT"}}
		}
		desk[r.String()] = ag
		return ag
	}
	d.Refresh(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); d.Wait() })
	a := rs.hostRoom(t, d, ctx, "A")
	b := rs.hostRoom(t, d, ctx, "B")
	linkA, linkB := rs.ts.URL+"/r/"+a.id, rs.ts.URL+"/r/"+b.id
	for _, r := range []hostedRoom{a, b} {
		rs.waitDone(t, r, rs.press(t, r, rs.personaOf(t, r)).ID)
	}
	agentOf := func(link string) *fakeAgent { deskMu.Lock(); defer deskMu.Unlock(); return desk[link] }
	allowed := func(link, dom string) bool {
		ag := agentOf(link)
		ag.mu.Lock()
		defer ag.mu.Unlock()
		return covered(ag.policy, dom)
	}
	// The first room's turn was refused two websites: two lines.
	var lines []store.Message
	waitFor(t, "blocked lines", func() bool {
		msgs, _ := rs.st.Messages(t.Context(), a.id, 0, 1000)
		lines = lines[:0]
		for _, m := range msgs {
			if m.Blocked != nil {
				lines = append(lines, m)
			}
		}
		return len(lines) == 2
	})
	approve := func(id int64, scope string) {
		t.Helper()
		rs.call(t, "POST", fmt.Sprintf("/api/rooms/%s/blocked/%d/allow", a.id, id), a.host, map[string]string{"scope": scope}, http.StatusAccepted, nil)
		waitFor(t, "the approval confirmed", func() bool {
			m, err := rs.st.Message(t.Context(), a.id, id)
			return err == nil && m.Blocked.Decision == scope
		})
	}
	approve(lines[0].ID, "room")
	if !allowed(linkA, "example.com") || allowed(linkB, "example.com") {
		t.Fatal("a room's approval reached the wrong sandboxes")
	}
	if e := d.Status().Egress.Rooms[linkA]; len(e) != 1 || e[0].By != "Alice" || e[0].At == nil {
		t.Fatalf("room entries %+v", e)
	}
	approve(lines[1].ID, "server")
	if !allowed(linkA, "docs.rs") || !allowed(linkB, "docs.rs") {
		t.Fatal("an everywhere approval missed a room")
	}
	// 014-AC17: the plugin assist counts the room's own list.
	if p := d.policy(linkA); !covered(p, "example.com") || covered(d.policy(linkB), "example.com") {
		t.Fatalf("assist policy %+v", p)
	}
	// The page removes the room's entry: the sandbox loses it now, the room is told.
	if err := d.RemoveRoomEgress(ctx, linkA, "example.com"); err != nil {
		t.Fatal(err)
	}
	if allowed(linkA, "example.com") {
		t.Fatal("a removed entry stayed")
	}
	waitFor(t, "the revocation line", func() bool {
		msgs, _ := rs.st.Messages(t.Context(), a.id, 0, 1000)
		return msgs[len(msgs)-1].Body == "Alice removed example.com from this room's websites"
	})
	if err := d.RemoveRoomEgress(ctx, linkA, "example.com"); err == nil {
		t.Fatal("removed twice")
	}
	// A server entry added from a room: its removal on the page is told to the rooms.
	if err := d.SetAllowlist(ctx, sandbox.DefaultAllowlist); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the everywhere revocation line", func() bool {
		msgs, _ := rs.st.Messages(t.Context(), b.id, 0, 1000)
		return msgs[len(msgs)-1].Body == "Alice removed docs.rs from every room"
	})
	// 014-AC16: 100 entries a room.
	for i := range 100 {
		if err := d.AllowEgress(ctx, linkB, fmt.Sprintf("h%d.example", i), "room", "Alice"); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.AllowEgress(ctx, linkB, "h100.example", "room", "Alice"); err == nil || !strings.Contains(err.Error(), "at most 100") {
		t.Fatalf("101st entry: %v", err)
	}
	for _, bad := range []string{"10.0.0.1", "api.anthropic.com", ".dotted.example", "*.x"} {
		if err := d.AllowEgress(ctx, linkB, bad, "room", "Alice"); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if err := d.AllowEgress(ctx, linkB, "x.example", "planet", "Alice"); err == nil {
		t.Error("bad scope accepted")
	}
	// A full room list: the host's approval fails, and the room's line says why.
	for i := range 100 {
		if err := d.AllowEgress(ctx, linkA, fmt.Sprintf("a%d.example", i), "room", "Alice"); err != nil {
			t.Fatal(err)
		}
	}
	full, err := rs.st.RecordBlocked(t.Context(), a.id, "Grill", []string{"full.example"})
	if err != nil {
		t.Fatal(err)
	}
	rs.call(t, "POST", fmt.Sprintf("/api/rooms/%s/blocked/%d/allow", a.id, full[0].ID), a.host, map[string]string{"scope": "room"}, http.StatusAccepted, nil)
	waitFor(t, "the refusal on the line", func() bool {
		m, err := rs.st.Message(t.Context(), a.id, full[0].ID)
		return err == nil && strings.Contains(m.Blocked.Error, "at most 100")
	})
	saved, err := LoadEgress(d.o.AllowlistPath)
	if err != nil || len(saved.Rooms[linkB]) != 100 {
		t.Fatalf("after a restart: %d entries, %v", len(saved.Rooms[linkB]), err)
	}
	if err := d.RemoveRoom(ctx, linkB, true); err != nil {
		t.Fatal(err)
	}
	if saved, err := LoadEgress(d.o.AllowlistPath); err != nil || saved.Rooms[linkB] != nil {
		t.Fatalf("a removed room's list stayed: %v", err)
	}
}

// 014-AC3 014-AC8: a sandbox whose log cannot be read leaves the turn with no blocked domain and a
// warning, not a failure.
func TestReadEgressFailure(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	d := newTestDaemon(t, nil)
	d.o.Log = zap.New(core)
	r := &roomRun{d: d, room: Room{Base: "https://h", ID: "a"}}
	if got := r.readEgress(t.Context(), &fakeAgent{requestsErr: errors.New("no log")}, time.Now(), "Grill"); got != nil {
		t.Fatalf("blocked = %v", got)
	}
	if logs.FilterMessage("egress_read_failed").Len() != 1 {
		t.Fatal("no warning")
	}
}
