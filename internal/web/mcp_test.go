package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/babs/grill-room/internal/store"
)

const mcpVersion = "2025-11-25"

type rpcResp struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// rpc posts one raw JSON-RPC body to /mcp with token, as an MCP client after initialize does.
func (e *env) rpc(t *testing.T, token, body string, hdr ...string) (int, http.Header, []byte) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), "POST", e.ts.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", mcpVersion)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

type toolResult struct {
	IsError           bool            `json:"isError"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	Content           []struct {
		Text string `json:"text"`
	} `json:"content"`
}

// call runs tool with args through /mcp and returns its result.
func (e *env) call(t *testing.T, token, tool string, args any) toolResult {
	t.Helper()
	a, _ := json.Marshal(args)
	code, _, b := e.rpc(t, token, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, tool, a))
	if code != http.StatusOK {
		t.Fatalf("%s: HTTP %d %s", tool, code, b)
	}
	r := decodeJSON[rpcResp](t, b)
	if r.Error != nil {
		t.Fatalf("%s: rpc error %d %s", tool, r.Error.Code, r.Error.Message)
	}
	return decodeJSON[toolResult](t, r.Result)
}

func (r toolResult) text() string {
	if len(r.Content) == 0 {
		return ""
	}
	return r.Content[0].Text
}

// mcpEnv is a room Alice created (room admin) with Bob as a plain member, and their tokens.
type mcpEnv struct {
	*env
	room                       store.Room
	alice, bob                 *device
	aliceFull, aliceRead, bobF string
}

func newMCPEnv(t *testing.T) *mcpEnv {
	t.Helper()
	e := newEnv(t)
	m := &mcpEnv{env: e, alice: e.device(t), bob: e.device(t)}
	m.room = m.alice.createRoom(t, "Pricing", "Alice")
	m.bob.join(t, m.room.ID, "Bob")
	m.aliceFull, m.aliceRead, m.bobF = m.alice.newToken(t, "full", 0).Token, m.alice.newToken(t, "read", 0).Token, m.bob.newToken(t, "full", 0).Token
	return m
}

// aliceHostsApprovingAll pairs a daemon to Alice's seat, which hosts the room's AI and approves
// every change.
func (m *mcpEnv) aliceHostsApprovingAll(t *testing.T) {
	t.Helper()
	p := m.requestPairing(t, m.room.ID, "alice-laptop")
	if code, b := m.alice.do(t, "POST", "/api/rooms/"+m.room.ID+"/pairings/"+p.ID, map[string]bool{"approve": true}); code != http.StatusOK {
		t.Fatalf("approve pairing: %d %s", code, b)
	}
	_, r := m.poll(t, p)
	d := &testDaemon{tok: r.Token}
	m.connect(t, m.room.ID, d, true)
	t.Cleanup(d.drop)
	m.settings(t, m.room.ID, d, store.DaemonSettings{ApproveAll: true})
	m.waitAgent(t, m.room.ID, m.aliceRead, func(st agentState) bool { return st.Cap != nil && st.Cap.ApproveAll })
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// 012-AC13 012-AC14 012-AC15: the handshake, the catalogue by scope, the JSON-RPC errors and the
// HTTP refusals.
func TestMCPProtocol(t *testing.T) {
	m := newMCPEnv(t)
	code, hdr, b := m.rpc(t, m.aliceFull, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	var init struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if r := decodeJSON[rpcResp](t, b); code != http.StatusOK || r.Error != nil || json.Unmarshal(r.Result, &init) != nil ||
		init.ProtocolVersion != mcpVersion || init.Capabilities["tools"] == nil || init.ServerInfo.Name != "grill-room" {
		t.Fatalf("initialize: %d %s", code, b)
	}
	if !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") || hdr.Get("Mcp-Session-Id") != "" {
		t.Fatalf("initialize headers: %v", hdr)
	}
	if code, _, b := m.rpc(t, m.aliceFull, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != http.StatusAccepted {
		t.Fatalf("initialized: %d %s", code, b)
	}
	list := func(token string) []map[string]any {
		_, _, b := m.rpc(t, token, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
		var res struct {
			Tools []map[string]any `json:"tools"`
		}
		if err := json.Unmarshal(decodeJSON[rpcResp](t, b).Result, &res); err != nil {
			t.Fatalf("tools/list: %s", b)
		}
		return res.Tools
	}
	full, read := list(m.aliceFull), list(m.aliceRead)
	if len(full) != 27 || len(read) != 12 {
		t.Fatalf("tools: %d full, %d read", len(full), len(read))
	}
	// The SDK lists tools by name: compare the sets.
	names := func(ts []map[string]any) []string {
		out := []string{}
		for _, x := range ts {
			out = append(out, x["name"].(string))
		}
		slices.Sort(out)
		return out
	}
	var wantFull, wantRead []string
	for _, tool := range mcpTools {
		wantFull = append(wantFull, tool.name)
		if !tool.full {
			wantRead = append(wantRead, tool.name)
		}
	}
	slices.Sort(wantFull)
	slices.Sort(wantRead)
	if !slices.Equal(names(full), wantFull) || !slices.Equal(names(read), wantRead) {
		t.Fatalf("tools: %v / %v", names(full), names(read))
	}
	for _, tool := range full {
		s, _ := tool["inputSchema"].(map[string]any)
		if s["type"] != "object" || s["additionalProperties"] != false || s["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
			t.Errorf("%s schema: %v", tool["name"], s)
		}
		ann, _ := tool["annotations"].(map[string]any)
		for _, h := range []string{"readOnlyHint", "destructiveHint", "idempotentHint"} {
			if _, ok := ann[h]; !ok {
				t.Errorf("%s has no %s: %v", tool["name"], h, ann)
			}
		}
	}

	for _, c := range []struct {
		body string
		code int
	}{
		{`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"post_message","arguments":{}}}`, -32602},
		{`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"create_room","arguments":{"display_name":"x"}}}`, -32602},
	} {
		tok := m.aliceFull
		if strings.Contains(c.body, "create_room") {
			tok = m.aliceRead
		}
		_, _, b := m.rpc(t, tok, c.body)
		if r := decodeJSON[rpcResp](t, b); r.Error == nil || r.Error.Code != c.code {
			t.Errorf("%s: %s, want %d", c.body, b, c.code)
		}
	}
	// The SDK answers a body that is not JSON with a bare 400, before any JSON-RPC.
	if code, _, b := m.rpc(t, m.aliceFull, `not json`); code != http.StatusBadRequest {
		t.Errorf("not json: %d %s", code, b)
	}
	// Likewise an unknown method: the SDK checks the method before dispatch.
	if code, _, b := m.rpc(t, m.aliceFull, `{"jsonrpc":"2.0","id":3,"method":"nope/nope"}`); code != http.StatusBadRequest || !strings.Contains(string(b), `"nope/nope" unsupported`) {
		t.Errorf("unknown method: %d %s", code, b)
	}
	if r := m.call(t, m.aliceFull, "get_room", map[string]any{"room": 7}); !r.IsError || !strings.Contains(r.text(), "room") {
		t.Errorf("wrong type: %+v", r)
	}
	if r := m.call(t, m.aliceFull, "get_room", map[string]any{"room": m.room.ID, "extra": 1}); !r.IsError {
		t.Errorf("unknown argument: %+v", r)
	}
	if r := m.call(t, m.aliceFull, "list_rooms", map[string]any{}); r.IsError || !strings.Contains(string(r.StructuredContent), m.room.ID) || r.text() != string(r.StructuredContent) {
		t.Errorf("list_rooms: %+v", r)
	}

	for _, method := range []string{"GET", "DELETE"} {
		req, _ := http.NewRequestWithContext(t.Context(), method, m.ts.URL+"/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+m.aliceFull)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d", method, resp.StatusCode)
		}
	}
	if code, _, b := m.rpc(t, m.aliceFull, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "Origin", "https://evil.example", "Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
		t.Errorf("foreign origin: %d %s", code, b)
	}
	if code, _, b := m.rpc(t, m.aliceFull, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "Host", "rooms.example"); code != http.StatusOK {
		t.Errorf("public Host on loopback: %d %s", code, b)
	}
	// 012-AC14: no OAuth metadata on any refusal, so a client starts no OAuth flow.
	cookie := func() []string {
		jar := m.alice.c.Jar.Cookies(mustURL(t, m.ts.URL+"/"))
		var parts []string
		for _, c := range jar {
			parts = append(parts, c.Name+"="+c.Value)
		}
		return []string{"Cookie", strings.Join(parts, "; ")}
	}()
	for name, c := range map[string]struct {
		token string
		hdr   []string
	}{
		"none": {}, "admin token": {token: m.admin}, "cookie alone": {hdr: cookie},
		"unknown": {token: "grt_AAAAAAAAAAAAAAAAAAAAAAAAAA"},
	} {
		code, hdr, b := m.rpc(t, c.token, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, c.hdr...)
		if code != http.StatusUnauthorized || strings.Contains(hdr.Get("WWW-Authenticate"), "resource_metadata") {
			t.Errorf("%s: %d %v %s", name, code, hdr, b)
		}
	}
}

// 012-AC13: the SDK's own client lists the tools and calls one, through the 2026-07-28
// discovery and the 2025-11-25 handshake it falls back to.
func TestMCPSDKClient(t *testing.T) {
	m := newMCPEnv(t)
	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	transport := &mcp.StreamableClientTransport{Endpoint: m.ts.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerRT(m.aliceRead)}}
	cs, err := client.Connect(t.Context(), transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil || len(tools.Tools) != 12 {
		t.Fatalf("list tools: %v %v", tools, err)
	}
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_room", Arguments: map[string]any{"room": m.room.ID}})
	if err != nil || res.IsError {
		t.Fatalf("get_room: %+v %v", res, err)
	}
	b, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(b), `"Pricing"`) || !strings.Contains(string(b), `"personas"`) {
		t.Fatalf("get_room content: %s", b)
	}
}

type bearerRT string

func (tok bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(tok))
	return http.DefaultTransport.RoundTrip(r)
}

// 012-AC16 012-AC17 012-AC18 012-AC19: tools act with the caller's rights: a plain member gets the
// route's refusal, a read token only reads, and host approval holds a member's persona.
func TestMCPToolsAsTheCaller(t *testing.T) {
	m := newMCPEnv(t)
	room := m.room.ID
	if r := m.call(t, m.bobF, "check_plugin", map[string]any{"room": room, "url": "https://example.com/x.git"}); !r.IsError || r.text() != "only a room admin can do this" {
		t.Errorf("member check_plugin: %+v", r)
	}
	if r := m.call(t, m.bobF, "delete_room", map[string]any{"room": room, "confirm": "Pricing"}); !r.IsError || r.text() != "only a room admin can do this" {
		t.Errorf("member delete_room: %+v", r)
	}
	for _, tool := range []string{"get_room", "list_documents", "list_invites", "list_templates", "list_plugins", "list_skills", "get_transcript"} {
		if r := m.call(t, m.aliceRead, tool, map[string]any{"room": room}); r.IsError {
			t.Errorf("read %s: %+v", tool, r)
		}
	}
	for _, tool := range []string{"list_starters", "list_rooms", "list_skills"} {
		if r := m.call(t, m.aliceRead, tool, map[string]any{}); r.IsError {
			t.Errorf("read %s: %+v", tool, r)
		}
	}
	if r := m.call(t, m.aliceRead, "get_starter", map[string]any{"starter": 0}); r.IsError {
		t.Errorf("get_starter: %+v", r)
	}
	if r := m.call(t, m.aliceFull, "get_room", map[string]any{"room": "nope"}); !r.IsError || r.text() != msgTokenNotHere {
		t.Errorf("another room: %+v", r)
	}

	// Documents: upload as text, read back, list, the transcript shows the line.
	up := m.call(t, m.aliceFull, "upload_document", map[string]any{"room": room, "name": "plan.md", "text": "# Plan\n\nship it\n"})
	if up.IsError {
		t.Fatalf("upload: %+v", up)
	}
	var doc struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(up.StructuredContent, &doc)
	rd := m.call(t, m.aliceRead, "read_document", map[string]any{"room": room, "document": doc.ID})
	if rd.IsError || !strings.Contains(string(rd.StructuredContent), "ship it") {
		t.Fatalf("read_document: %+v %s", rd, up.StructuredContent)
	}
	if r := m.call(t, m.aliceFull, "upload_document", map[string]any{"room": room, "name": "x.md"}); !r.IsError || r.text() != "give exactly one of text and content_base64" {
		t.Errorf("upload without content: %+v", r)
	}
	if r := m.call(t, m.aliceFull, "upload_document", map[string]any{"room": room, "name": "b.md", "content_base64": "aGVsbG8K"}); r.IsError {
		t.Errorf("upload base64: %+v", r)
	}
	if r := m.call(t, m.aliceRead, "list_notes", map[string]any{"room": room, "document": doc.ID}); r.IsError {
		t.Errorf("list_notes: %+v", r)
	}

	// Personas: the Secretary template, added by Bob under "approve everything", waits.
	tpls := m.call(t, m.aliceRead, "list_templates", map[string]any{"room": room})
	var ts struct {
		Items []struct {
			ID    int64  `json:"id"`
			Title string `json:"title"`
		} `json:"items"`
	}
	_ = json.Unmarshal(tpls.StructuredContent, &ts)
	i := slices.IndexFunc(ts.Items, func(x struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
	}) bool {
		return x.Title == "Secretary"
	})
	if i < 0 {
		t.Fatalf("templates: %s", tpls.StructuredContent)
	}
	m.aliceHostsApprovingAll(t)
	held := m.call(t, m.bobF, "add_persona", map[string]any{"room": room, "template": ts.Items[i].ID})
	if held.IsError || !strings.Contains(string(held.StructuredContent), `"held":true`) || !strings.Contains(string(held.StructuredContent), msgMCPHeld) {
		t.Fatalf("member add_persona: %+v %s", held, held.StructuredContent)
	}
	applied := m.call(t, m.aliceFull, "add_persona", map[string]any{"room": room, "template": ts.Items[i].ID})
	if applied.IsError || strings.Contains(string(applied.StructuredContent), `"held"`) {
		t.Fatalf("host add_persona: %+v %s", applied, applied.StructuredContent)
	}

	// 012-AC18: two calls, one shareable link.
	before := len(m.call(t, m.aliceRead, "list_invites", map[string]any{"room": room}).items(t))
	l1 := m.call(t, m.aliceFull, "get_invite_link", map[string]any{"room": room})
	l2 := m.call(t, m.aliceFull, "get_invite_link", map[string]any{"room": room})
	after := len(m.call(t, m.aliceRead, "list_invites", map[string]any{"room": room}).items(t))
	if l1.IsError || string(l1.StructuredContent) != string(l2.StructuredContent) || after > before+1 {
		t.Fatalf("invite link: %s %s, invites %d → %d", l1.StructuredContent, l2.StructuredContent, before, after)
	}
}

func (r toolResult) items(t *testing.T) []json.RawMessage {
	t.Helper()
	var v struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(r.StructuredContent, &v); err != nil {
		t.Fatalf("items: %s", r.StructuredContent)
	}
	return v.Items
}

// 012-AC20: a destructive call needs confirm equal to the target's name, byte for byte; a refusal
// changes nothing and names nothing; the exact name acts and leaves one audit entry with the token.
func TestMCPConfirm(t *testing.T) {
	m := newMCPEnv(t)
	room := m.room.ID
	up := m.call(t, m.aliceFull, "upload_document", map[string]any{"room": room, "name": "Notes.md", "text": "x"})
	var doc struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(up.StructuredContent, &doc)
	inv := m.call(t, m.aliceFull, "get_invite_link", map[string]any{"room": room})
	var link struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(inv.StructuredContent, &link)
	agent := m.call(t, m.aliceRead, "get_room", map[string]any{"room": room})
	var st struct {
		Agent struct {
			Personas []store.RoomPersona `json:"personas"`
		} `json:"agent"`
	}
	_ = json.Unmarshal(agent.StructuredContent, &st)
	if len(st.Agent.Personas) == 0 {
		t.Fatalf("no persona: %s", agent.StructuredContent)
	}
	persona := st.Agent.Personas[0]
	auditCount := func() int { return len(m.auditLog(t, "?limit=200").Entries) }
	for _, c := range []struct {
		tool, name, kind string
		args             map[string]any
	}{
		{"remove_persona", persona.Name, "persona", map[string]any{"room": room, "persona": persona.ID}},
		{"delete_document", "Notes.md", "document", map[string]any{"room": room, "document": doc.ID}},
		{"revoke_invite", fmt.Sprint(link.ID), "invite", map[string]any{"room": room, "invite": link.ID}},
		{"delete_room", "Pricing", "room", map[string]any{"room": room}},
	} {
		n := auditCount()
		bad := []any{nil, strings.ToUpper(c.name), c.name + " ", "1", strings.ToLower(c.name) + "x"}
		if c.kind != "invite" {
			bad = append(bad, fmt.Sprint(c.args[c.kind]))
		}
		for _, confirm := range bad {
			if confirm == c.name {
				continue
			}
			args := map[string]any{}
			for k, v := range c.args {
				args[k] = v
			}
			if confirm != nil {
				args["confirm"] = confirm
			}
			r := m.call(t, m.aliceFull, c.tool, args)
			if confirm == nil {
				if !r.IsError {
					t.Errorf("%s without confirm: %+v", c.tool, r)
				}
				continue
			}
			if !r.IsError || !strings.HasPrefix(r.text(), "confirm must be the exact name of the "+c.kind+"; read it with ") || strings.Contains(r.text(), c.name) {
				t.Errorf("%s confirm %q: %+v", c.tool, confirm, r)
			}
		}
		if auditCount() != n {
			t.Fatalf("%s: refusals wrote audit entries", c.tool)
		}
		args := map[string]any{"confirm": c.name}
		for k, v := range c.args {
			args[k] = v
		}
		if r := m.call(t, m.aliceFull, c.tool, args); r.IsError {
			t.Fatalf("%s with the name: %+v", c.tool, r)
		}
		got := m.auditLog(t, "?limit=200").Entries
		if len(got) != n+1 || !strings.Contains(string(got[0].Details), `"api_token"`) {
			t.Fatalf("%s audit: %d → %d, %s", c.tool, n, len(got), got[0].Details)
		}
	}
}

// 012-AC21: each tool reaches only its catalogue routes, never a route that posts, runs a turn or
// decides an approval.
func TestMCPToolRoutes(t *testing.T) {
	m := newMCPEnv(t)
	registered := map[string]bool{}
	for _, p := range m.srv.routes {
		registered[p] = true
	}
	for _, tool := range mcpTools {
		for _, p := range tool.routes {
			if !registered[p] {
				t.Errorf("%s lists %q, which no route registers", tool.name, p)
			}
			for _, banned := range []string{"/messages", "/turns", "/decision", "/changes/", "/cap"} {
				if strings.Contains(p, banned) && p != "GET /api/rooms/{room}/messages" {
					t.Errorf("%s reaches %s", tool.name, p)
				}
			}
		}
	}
	// A tool whose path leads elsewhere is refused before the route runs.
	c := &mcpCall{s: m.srv, r: httpRequest(t, m.aliceFull), tool: mcpTools[0]}
	if _, _, err := c.send(http.MethodPost, "/api/rooms/"+m.room.ID+"/messages", map[string]string{"body": "hi"}); err != errMCPServer {
		t.Fatalf("off-catalogue route: %v", err)
	}
}

func httpRequest(t *testing.T, token string) *http.Request {
	t.Helper()
	r, _ := http.NewRequestWithContext(t.Context(), "POST", "/mcp", bytes.NewReader(nil))
	r.Header.Set("Authorization", "Bearer "+token)
	r.RemoteAddr = "127.0.0.1:1"
	return r
}

// 012-AC16: a tool's outcome is its route's for the same caller: success where the route says
// 2xx, the route's own message where it refuses.
func TestMCPParity(t *testing.T) {
	m := newMCPEnv(t)
	room := m.room.ID
	st := m.agent(t, room, m.aliceRead)
	persona := st.Personas[0].ID
	pp := fmt.Sprintf("/api/rooms/%s/personas/%d", room, persona)
	for _, c := range []struct {
		tool         string
		args         map[string]any
		method, path string
		body         any
	}{
		{"get_room", map[string]any{"room": room}, "GET", "/api/rooms/" + room, nil},
		{"list_invites", map[string]any{"room": room}, "GET", "/api/rooms/" + room + "/invites", nil},
		{"edit_persona", map[string]any{"room": room, "persona": persona, "badge": "🧪"}, "PATCH", pp, map[string]any{"badge": "🧪"}},
		{"edit_persona", map[string]any{"room": room, "persona": persona, "name": "two words"}, "PATCH", pp, map[string]any{"name": "two words"}},
		{"set_persona_plugin", map[string]any{"room": room, "persona": persona, "plugin": 999, "on": true}, "PUT", pp + "/plugins/999", nil},
		{"check_plugin", map[string]any{"room": room, "url": "https://127.0.0.1/x.git"}, "POST", "/api/rooms/" + room + "/plugins/check", map[string]any{"url": "https://127.0.0.1/x.git"}},
		{"get_invite_link", map[string]any{"room": room}, "POST", "/api/rooms/" + room + "/invites/share", nil},
	} {
		for who, tok := range map[string]string{"member": m.bobF, "admin": m.aliceFull, "read": m.aliceRead} {
			if who == "read" && slices.ContainsFunc(mcpTools, func(x *mcpTool) bool { return x.name == c.tool && x.full }) {
				continue // not listed for a read token: TestMCPProtocol
			}
			code, b := m.do(t, c.method, c.path, tok, c.body)
			r := m.call(t, tok, c.tool, c.args)
			if r.IsError != (code >= 400) || (code >= 400 && code < 500 && r.text() != errorOf(t, b)) {
				t.Errorf("%s as %s: tool %+v, route %d %s", c.tool, who, r, code, b)
			}
		}
	}
	if m.srv.MCPToolCalls("get_room", "ok") == 0 || m.srv.MCPToolCalls("edit_persona", "error") == 0 {
		t.Fatal("tool calls not counted")
	}
}
