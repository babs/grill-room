package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
	if got, err := LoadAllowlist(d.o.AllowlistPath); err != nil || !slices.Equal(got, want) {
		t.Fatalf("after a restart: %v %v", got, err)
	}
	assertPerm(t, d.o.AllowlistPath, 0o600)
	// Removing every domain is a choice, kept as such.
	if code, body := e.req(t, "POST", "/api/allowlist", tok, `{"allowlist":[]}`, nil); code != http.StatusNoContent {
		t.Fatalf("empty allowlist: %d %s", code, body)
	}
	if got, err := LoadAllowlist(d.o.AllowlistPath); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty list after a restart: %#v %v", got, err)
	}
	if got, err := LoadAllowlist(filepath.Join(t.TempDir(), "none.json")); err != nil || !slices.Equal(got, sandbox.DefaultAllowlist) {
		t.Fatalf("no file: %v %v", got, err)
	}
	for name, content := range map[string]string{"corrupt": "[", "bad entry": `["*.x.com"]`} {
		p := filepath.Join(t.TempDir(), "allowlist.json")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadAllowlist(p); err == nil {
			t.Errorf("%s file loaded", name)
		}
	}
	code, body = e.req(t, "GET", "/api/denied", tok, "", nil)
	if code != http.StatusOK || strings.TrimSpace(body) != `{"denied":[],"hits":{}}` {
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
	// 003b-AC16: the hits come with the denied list.
	if got, err := d.Egress(ctx, 5); err != nil || len(got.Denied) != 1 || got.Denied[0].Host != "example.com" || got.Hits["pypi.org"] != 3 {
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
