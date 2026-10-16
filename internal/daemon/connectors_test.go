package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

// fakeConnectors is Anthropic's connector list and its MCP relay, for the token cfg-secret: Google
// Calendar has a read tool and a write tool, Gmail only a read tool, Lapsed answers 401.
type fakeConnectors struct {
	srv          *httptest.Server
	lists, calls atomic.Int64
	mu           sync.Mutex
	relayed      []string
}

func newFakeConnectors(t *testing.T) *fakeConnectors {
	t.Helper()
	f := &fakeConnectors{}
	tools := map[string]string{
		"srv_cal":  `[{"name":"list_events","annotations":{"readOnlyHint":true}},{"name":"create_event"}]`,
		"srv_mail": `[{"name":"search","annotations":{"readOnlyHint":true}}]`,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cfg-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/mcp_servers" {
			f.lists.Add(1)
			if r.Header.Get("Anthropic-Beta") != "mcp-servers-2025-12-04" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"data":[{"id":"srv_cal","display_name":"Google Calendar"},{"id":"srv_mail","display_name":"Gmail"},
				{"id":"srv_x","display_name":"Lapsed"},{"id":"bad/id","display_name":"Bad"}]}`)
			return
		}
		id, ok := strings.CutPrefix(r.URL.Path, "/v1/mcp/")
		if !ok || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.mu.Lock()
		f.relayed = append(f.relayed, id)
		f.mu.Unlock()
		if r.Header.Get("X-Mcp-Client-Session-Id") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if tools[id] == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var call struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&call)
		switch call.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		case "tools/list":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"tools\":%s}}\n\n", tools[id])
		case "tools/call":
			f.calls.Add(1)
			_, _ = io.WriteString(w, `{}`)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeConnectors) accounts(t *testing.T, claude string) *Accounts {
	t.Helper()
	u, _ := url.Parse(f.srv.URL)
	dir := t.TempDir()
	a, err := LoadAccounts(AccountsOptions{Paths: Paths{Config: dir, State: dir, Cache: dir}, ClaudeConfigDir: claude, Upstream: u, MCPUpstream: u,
		Docker: desktopDocker, Keyring: &memKeyring{m: map[string]string{}}, Log: zap.NewNop()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

// mcpPost calls the connector route of proxy p as a sandbox would.
func mcpPost(t *testing.T, p Proxy, bearer, id string) int {
	t.Helper()
	base := "http://127.0.0.1:" + p.Addr[strings.LastIndex(p.Addr, ":")+1:]
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/mcp/"+id, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("X-Mcp-Client-Session-Id", "turn-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// 014-AC18 014-AC20 014-AC21 014-AC22 014-AC23 014-AC24: the host lists the account's connectors
// with its token, never calling a tool; a room gets the connectors on for it, write tools
// disallowed unless its actions are, through a bearer of its own; the proxy forwards only those;
// one turned off is refused from the next request; a lapsed login shows on the page and costs the
// account nothing.
func TestAccountsConnectors(t *testing.T) {
	claude := t.TempDir()
	writeClaudeCreds(t, claude, "cfg-secret", time.Now().Add(time.Hour))
	f := newFakeConnectors(t)
	a := f.accounts(t, claude)
	ctx := context.Background()
	list, err := a.Connectors(ctx, "default", false)
	if err != nil {
		t.Fatal(err)
	}
	want := []ConnectorInfo{
		{ID: "srv_mail", Name: "Gmail", State: ConnectorReady, Tools: []ConnectorTool{{Name: "search", ReadOnly: true}}},
		{ID: "srv_cal", Name: "Google Calendar", State: ConnectorReady, Tools: []ConnectorTool{{Name: "list_events", ReadOnly: true}, {Name: "create_event"}}},
		{ID: "srv_x", Name: "Lapsed", State: ConnectorNeedsLogin},
	}
	if !reflect.DeepEqual(list, want) {
		t.Fatalf("list %+v", list)
	}
	if _, err := a.Connectors(ctx, "default", false); err != nil || f.lists.Load() != 1 {
		t.Fatalf("cached list read again: %d", f.lists.Load())
	}
	if _, err := a.Connectors(ctx, "default", true); err != nil || f.lists.Load() != 2 {
		t.Fatalf("refresh: %d", f.lists.Load())
	}
	if _, err := a.Connectors(ctx, "nobody", false); err == nil {
		t.Fatal("listed an unknown account")
	}

	const roomA, roomB = "https://a/r/1", "https://a/r/2"
	rooms := map[string]string{roomA: "default", roomB: "default"}
	on := map[string][]RoomConnector{roomA: {{ID: "srv_cal"}, {ID: "srv_x"}}}
	acc := a.Access(ctx, "inst", rooms, on, false)
	pA, pB := acc.Proxies[roomA], acc.Proxies[roomB]
	if !reflect.DeepEqual(pA.Connectors, []ConnectorGrant{{ID: "srv_cal", Name: "Google Calendar", Key: "google_calendar",
		Disallowed: []string{"mcp__google_calendar__create_event"}}}) {
		t.Fatalf("room A grants %+v", pA.Connectors)
	}
	if pA.ConnectorBearer == "" || pA.ConnectorBearer == pA.Bearer || len(pB.Connectors) != 0 || pB.ConnectorBearer != "" {
		t.Fatalf("bearers: A %+v B %+v", pA, pB)
	}
	if code := mcpPost(t, pA, pA.ConnectorBearer, "srv_cal"); code != http.StatusOK {
		t.Fatalf("A's connector: %d", code)
	}
	for _, c := range []struct {
		bearer, id string
	}{{pA.ConnectorBearer, "srv_mail"}, {pA.ConnectorBearer, "srv_x"}, {pA.Bearer, "srv_cal"}, {pB.Bearer, "srv_cal"}} {
		if code := mcpPost(t, pA, c.bearer, c.id); code != http.StatusForbidden {
			t.Errorf("%s with %s: %d", c.id, c.bearer[:20], code)
		}
	}
	// Actions allowed: no tool disallowed; a lapsed connector's 401 shows on the page and leaves
	// the account's token alone.
	on[roomA] = []RoomConnector{{ID: "srv_cal", Actions: true}, {ID: "srv_x", Actions: true}}
	acc = a.Access(ctx, "inst", rooms, on, false)
	if g := acc.Proxies[roomA].Connectors; len(g) != 2 || g[0].Disallowed != nil || !g[0].Actions || g[1].Key != "lapsed" {
		t.Fatalf("with actions %+v", g)
	}
	if code := mcpPost(t, acc.Proxies[roomA], pA.ConnectorBearer, "srv_x"); code != http.StatusUnauthorized {
		t.Fatalf("lapsed: %d", code)
	}
	if v := view(t, a, "default"); v.State == "refused" {
		t.Fatalf("a connector's 401 marked the account: %+v", v)
	}
	// 014-AC24: off, the next request is refused.
	acc = a.Access(ctx, "inst", rooms, nil, false)
	if code := mcpPost(t, acc.Proxies[roomA], pA.ConnectorBearer, "srv_cal"); code != http.StatusForbidden || acc.Proxies[roomA].ConnectorBearer != "" {
		t.Fatalf("off: %d %+v", code, acc.Proxies[roomA])
	}
	// A room gone from the worker loses its grant.
	acc = a.Access(ctx, "inst", rooms, map[string][]RoomConnector{roomB: {{ID: "srv_mail"}}}, false)
	pB = acc.Proxies[roomB]
	if code := mcpPost(t, pB, pB.ConnectorBearer, "srv_mail"); code != http.StatusOK {
		t.Fatalf("B's connector: %d", code)
	}
	a.Access(ctx, "inst", map[string]string{roomA: "default"}, nil, false)
	if code := mcpPost(t, pB, pB.ConnectorBearer, "srv_mail"); code != http.StatusForbidden {
		t.Fatalf("removed room: %d", code)
	}
	if n := f.calls.Load(); n != 0 {
		t.Fatalf("%d tool calls", n)
	}
}

// 014-AC18 014-AC19: the page lists a room's connectors, turns one on or off and allows its
// actions; the change is saved, reaches the worker's access and is told to it; a worker before
// protocol 6 is refused with why.
func TestSupervisorConnectors(t *testing.T) {
	claude := t.TempDir()
	writeClaudeCreds(t, claude, "cfg-secret", time.Now().Add(time.Hour))
	f := newFakeConnectors(t)
	withAccounts := func(o *SupervisorOptions) { o.Accounts = f.accounts(t, claude) }
	for _, mode := range []string{strconv.Itoa(Proto), strconv.Itoa(protoEgress - 1)} {
		t.Run(mode, func(t *testing.T) {
			s := testSupervisor(t, mode, nil, withAccounts)
			e := newAdminEnv(t, s)
			tok := e.login(t)
			room := Room{Base: "https://rooms.example", ID: "abc"}
			s.SetRoom(context.Background(), room)
			waitFor(t, "the worker running", func() bool { return instance(t, s).State == "running" })
			id, link := instance(t, s).ID, room.String()
			code, b := e.req(t, "GET", "/api/connectors?instance="+id+"&room="+url.QueryEscape(link), tok, "", nil)
			var got struct{ Connectors []ConnectorView }
			if err := json.Unmarshal([]byte(b), &got); code != http.StatusOK || err != nil || len(got.Connectors) != 3 || got.Connectors[1].On {
				t.Fatalf("list: %d %s", code, b)
			}
			set := func(body string) (int, string) {
				return e.req(t, "POST", "/api/connectors", tok, `{"instance":"`+id+`","room":"`+link+`",`+body+`}`, nil)
			}
			code, b = set(`"id":"srv_cal","on":true`)
			if mode != strconv.Itoa(Proto) {
				if code != http.StatusBadRequest || !strings.Contains(b, "too old") {
					t.Fatalf("old worker: %d %s", code, b)
				}
				return
			}
			if code != http.StatusNoContent {
				t.Fatalf("on: %d %s", code, b)
			}
			waitFor(t, "the connector in the access", func() bool {
				a := lastAccess(s)
				return a != nil && len(a.Proxies[link].Connectors) == 1 && a.Proxies[link].Connectors[0].Disallowed != nil
			})
			if code, b := set(`"id":"srv_cal","on":true,"actions":true`); code != http.StatusNoContent {
				t.Fatalf("actions: %d %s", code, b)
			}
			waitFor(t, "actions in the access", func() bool { a := lastAccess(s); return a != nil && a.Proxies[link].Connectors[0].Actions })
			saved, err := loadInstance(s.o.Paths.InstanceConfig(id))
			if err != nil || !slices.Equal(saved.Connectors[link], []RoomConnector{{ID: "srv_cal", Name: "Google Calendar", Actions: true}}) {
				t.Fatalf("saved %+v %v", saved.Connectors, err)
			}
			for _, bad := range []string{`"id":"srv_nope","on":true`, `"id":"srv_cal","on":true,"room":"https://other/r/x"`, `"id":"srv_cal","on":true,"instance":"nope"`} {
				if code, _ := set(bad); code != http.StatusBadRequest {
					t.Errorf("%s: %d", bad, code)
				}
			}
			if code, _ := set(`"id":"srv_cal","on":true,"actions":true`); code != http.StatusNoContent {
				t.Errorf("no change: %d", code)
			}
			if code, _ := e.req(t, "GET", "/api/connectors?instance=nope&room="+url.QueryEscape(link), tok, "", nil); code != http.StatusBadGateway {
				t.Errorf("unknown server: %d", code)
			}
			if code, _ := set(`"id":"srv_cal","on":false`); code != http.StatusNoContent {
				t.Fatal(code)
			}
			waitFor(t, "the connector gone", func() bool { a := lastAccess(s); return a != nil && len(a.Proxies[link].Connectors) == 0 })
		})
	}
}

// 014-AC18: a login that cannot list connectors says so; a relay error marks the connector, not the
// list; an unreadable login is an error; connector keys stay unique and plain.
func TestConnectorsFailures(t *testing.T) {
	claude := t.TempDir()
	writeClaudeCreds(t, claude, "cfg-secret", time.Now().Add(time.Hour))
	listSay := atomic.Int64{}
	listSay.Store(http.StatusForbidden)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/mcp_servers" && listSay.Load() != http.StatusOK:
			w.WriteHeader(int(listSay.Load()))
		case r.URL.Path == "/v1/mcp_servers":
			_, _ = io.WriteString(w, `{"data":[{"id":"srv_a","display_name":"Broken"},{"id":"srv_b","display_name":"Garbled"}]}`)
		case r.URL.Path == "/v1/mcp/srv_a":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			_, _ = io.WriteString(w, `not json`)
		}
	}))
	t.Cleanup(srv.Close)
	f := &fakeConnectors{srv: srv}
	a := f.accounts(t, claude)
	ctx := context.Background()
	if _, err := a.Connectors(ctx, "default", false); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("refused list: %v", err)
	}
	listSay.Store(http.StatusOK)
	list, err := a.Connectors(ctx, "default", true)
	if err != nil || len(list) != 2 || list[0].State != ConnectorError || list[1].State != ConnectorError || list[0].Detail == "" {
		t.Fatalf("relay errors: %+v %v", list, err)
	}
	if g := grants([]RoomConnector{{ID: "srv_a"}, {ID: "srv_b", Actions: true}}, list); len(g) != 1 || g[0].ID != "srv_b" {
		t.Fatalf("grants with unknown tools: %+v", g)
	}
	noLogin := f.accounts(t, t.TempDir())
	if _, err := noLogin.Connectors(ctx, "default", false); err == nil {
		t.Fatal("listed with no login")
	}
	taken := map[string]bool{}
	if k := []string{connectorKey("Google Calendar", taken), connectorKey("google  calendar", taken), connectorKey("!!!", taken)}; !slices.Equal(k, []string{"google_calendar", "google_calendar_2", "connector"}) {
		t.Fatalf("keys %v", k)
	}
}

// 014-AC18: a list past its time is served as it is while a read in the background renews it.
func TestConnectorsStaleWhileRenewed(t *testing.T) {
	claude := t.TempDir()
	writeClaudeCreds(t, claude, "cfg-secret", time.Now().Add(time.Hour))
	f := newFakeConnectors(t)
	a := f.accounts(t, claude)
	clk := &clock{t: time.Now()}
	a.o.Now = clk.now
	ctx := context.Background()
	if _, err := a.Connectors(ctx, "default", false); err != nil || f.lists.Load() != 1 {
		t.Fatalf("first read: %v %d", err, f.lists.Load())
	}
	clk.set(clk.now().Add(connectorsTTL + time.Second))
	if list, err := a.Connectors(ctx, "default", false); err != nil || len(list) != 3 {
		t.Fatalf("stale list: %+v %v", list, err)
	}
	waitFor(t, "the background read", func() bool { return f.lists.Load() == 2 })
}
