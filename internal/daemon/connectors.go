package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/proxy"
	"github.com/babs/grill-room/internal/sandbox"
)

// RoomConnector is a claude.ai connector of the room's account that the host turned on for the
// room; Actions lets its tools without readOnlyHint run (014-AC18, 014-AC23).
type RoomConnector struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Actions bool   `json:"actions,omitempty"`
}

// ConnectorGrant is a connector on for a room as its turns get it: Key names its MCP server in the
// turn, Disallowed are its tools a turn may not call, as mcp__<key>__<tool> (014-AC20, 014-AC23).
type ConnectorGrant struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Key        string   `json:"key"`
	Actions    bool     `json:"actions,omitempty"`
	Disallowed []string `json:"disallowed,omitempty"`
}

// Connector states, as the daemon page shows them (014-AC18, 014-AC22).
const (
	ConnectorReady      = "ready"
	ConnectorNeedsLogin = "needs login on claude.ai"
	ConnectorError      = "error"
)

// ConnectorTool is one tool of a connector; ReadOnly is its readOnlyHint.
type ConnectorTool struct {
	Name     string `json:"name"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// ConnectorInfo is one connector of an account, as listed with the account's token on the host.
// Tools is nil when they could not be read.
type ConnectorInfo struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	State  string          `json:"state"`
	Detail string          `json:"detail,omitempty"`
	Tools  []ConnectorTool `json:"tools,omitempty"`
}

// connectorsTTL is how long an account's connector list serves before it is read again.
const connectorsTTL = 10 * time.Minute

// connectorList is an account's cached list; reading is set while a refresh runs.
type connectorList struct {
	at      time.Time
	list    []ConnectorInfo
	err     error
	reading bool
}

// connectorCache holds each account's list; mu guards it.
type connectorCache struct {
	mu sync.Mutex
	m  map[string]*connectorList
}

// Connectors lists account's claude.ai connectors with its token, from this host, never from a
// sandbox: the list, then each one's tools over MCP, initialize and tools/list only; no tool is
// called (014-AC18). A list older than connectorsTTL is served while a read in the background
// renews it: the workers' access waits on no network. refresh reads it now.
func (a *Accounts) Connectors(ctx context.Context, account string, refresh bool) ([]ConnectorInfo, error) {
	a.mu.Lock()
	ap := a.px[account]
	a.mu.Unlock()
	if ap == nil {
		return nil, errors.New("no such account")
	}
	c := &a.conn
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]*connectorList{}
	}
	l := c.m[account]
	if l != nil && !refresh {
		if a.o.Now().Sub(l.at) >= connectorsTTL && !l.reading {
			l.reading = true
			// The error is cached with the list: the next read returns it.
			go func() { _, _ = a.readConnectors(context.WithoutCancel(ctx), account, ap) }()
		}
		c.mu.Unlock()
		return slices.Clone(l.list), l.err
	}
	c.mu.Unlock()
	return a.readConnectors(ctx, account, ap)
}

// readConnectors reads account's list and caches it.
func (a *Accounts) readConnectors(ctx context.Context, account string, ap *accountProxy) ([]ConnectorInfo, error) {
	list, err := a.listConnectors(ctx, ap)
	c := &a.conn
	c.mu.Lock()
	c.m[account] = &connectorList{at: a.o.Now(), list: list, err: err}
	c.mu.Unlock()
	return slices.Clone(list), err
}

// connectorLapsed marks a connector the relay refused the account's token for (014-AC22).
func (a *Accounts) connectorLapsed(account, id string) {
	c := &a.conn
	c.mu.Lock()
	defer c.mu.Unlock()
	if l := c.m[account]; l != nil {
		for i := range l.list {
			if l.list[i].ID == id {
				l.list[i].State, l.list[i].Detail = ConnectorNeedsLogin, ""
			}
		}
	}
	a.o.Log.Warn("connector_login_lapsed", zap.String("account", account), zap.String("connector", id))
}

func (a *Accounts) listConnectors(ctx context.Context, ap *accountProxy) ([]ConnectorInfo, error) {
	tok, err := ap.src.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the account's token: %w", err)
	}
	lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(lctx, http.MethodGet, a.o.Upstream.JoinPath("/v1/mcp_servers").String()+"?limit=1000", nil) //nolint:gosec // Anthropic's fixed API URL
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "mcp-servers-2025-12-04")
	resp, err := a.o.HTTP.Do(req) //nolint:gosec // Anthropic's fixed API URL
	if err != nil {
		return nil, fmt.Errorf("list connectors: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list connectors: %s: this account's login may lack connector access; sign in with claude on the host", resp.Status)
	}
	var body struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("list connectors: %w", err)
	}
	out := make([]ConnectorInfo, 0, len(body.Data))
	for _, d := range body.Data {
		if !connectorIDRe.MatchString(d.ID) {
			continue
		}
		out = append(out, ConnectorInfo{ID: d.ID, Name: d.Name})
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i := range out {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i].Tools, out[i].State, out[i].Detail = a.connectorTools(ctx, tok.AccessToken, out[i].ID)
		})
	}
	wg.Wait()
	slices.SortFunc(out, func(x, y ConnectorInfo) int { return strings.Compare(strings.ToLower(x.Name), strings.ToLower(y.Name)) })
	return out, nil
}

var connectorIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// connectorTools runs MCP initialize then tools/list on the relay, as Claude Code does, and returns
// the tools with their read-only hint and the connector's state.
func (a *Accounts) connectorTools(ctx context.Context, token, id string) ([]ConnectorTool, string, string) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	endpoint := a.mcpUpstream().JoinPath("/v1/mcp/" + id).String()
	client, session := rand.Text(), ""
	call := func(body string) (int, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body)) //nolint:gosec // the relay's fixed URL and a checked id
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Anthropic-Beta", "mcp-servers-2025-12-04")
		// The relay answers 400 without a client session id (S16).
		req.Header.Set("X-Mcp-Client-Session-Id", client)
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, err := a.o.HTTP.Do(req) //nolint:gosec // see NewRequestWithContext
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
			session = s
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			var data []byte
			sc := bufio.NewScanner(bytes.NewReader(b))
			sc.Buffer(make([]byte, 64<<10), 4<<20)
			for sc.Scan() {
				if d, ok := strings.CutPrefix(sc.Text(), "data:"); ok {
					data = append(data, strings.TrimSpace(d)...)
				}
			}
			b = data
		}
		return resp.StatusCode, b, nil
	}
	st, _, err := call(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"grill-room","version":"0"}}}`)
	switch {
	case err != nil:
		return nil, ConnectorError, err.Error()
	case st == http.StatusUnauthorized:
		return nil, ConnectorNeedsLogin, ""
	case st >= 300:
		return nil, ConnectorError, fmt.Sprintf("initialize: HTTP %d", st)
	}
	_, _, _ = call(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	st, b, err := call(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if err != nil || st >= 300 {
		return nil, ConnectorError, fmt.Sprintf("tools/list: HTTP %d %v", st, err)
	}
	var r struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Annotations struct {
					ReadOnly bool `json:"readOnlyHint"`
				} `json:"annotations"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, ConnectorError, "tools/list: " + err.Error()
	}
	tools := make([]ConnectorTool, 0, len(r.Result.Tools))
	for _, t := range r.Result.Tools {
		tools = append(tools, ConnectorTool{Name: t.Name, ReadOnly: t.Annotations.ReadOnly})
	}
	return tools, ConnectorReady, ""
}

func (a *Accounts) mcpUpstream() *url.URL {
	if a.o.MCPUpstream != nil {
		return a.o.MCPUpstream
	}
	u, _ := url.Parse(proxy.DefaultMCPUpstream) // a constant URL: cannot fail
	return u
}

var keyNonWord = regexp.MustCompile(`[^a-z0-9]+`)

// connectorKey names a connector's MCP server in a turn, from its name: lowercase words joined by
// "_", unique among taken.
func connectorKey(name string, taken map[string]bool) string {
	k := strings.Trim(keyNonWord.ReplaceAllString(strings.ToLower(name), "_"), "_")
	if k == "" {
		k = "connector"
	}
	k = k[:min(len(k), 40)]
	out := k
	for i := 2; taken[out]; i++ {
		out = fmt.Sprintf("%s_%d", k, i)
	}
	taken[out] = true
	return out
}

// grants turns a room's connectors into what its turns get, from the account's list. A connector
// whose tools are unknown is left out unless its actions are allowed: its write tools could not be
// disallowed (014-AC23).
func grants(on []RoomConnector, list []ConnectorInfo) []ConnectorGrant {
	var out []ConnectorGrant
	taken := map[string]bool{}
	for _, rc := range on {
		i := slices.IndexFunc(list, func(c ConnectorInfo) bool { return c.ID == rc.ID })
		if i < 0 || list[i].Tools == nil && !rc.Actions {
			continue
		}
		g := ConnectorGrant{ID: rc.ID, Name: list[i].Name, Key: connectorKey(list[i].Name, taken), Actions: rc.Actions}
		if !rc.Actions {
			for _, t := range list[i].Tools {
				if !t.ReadOnly {
					g.Disallowed = append(g.Disallowed, "mcp__"+g.Key+"__"+t.Name)
				}
			}
		}
		out = append(out, g)
	}
	return out
}

// connectorBearer is the dummy bearer of room's connectors on instance's worker, made on first use;
// it differs from the worker's inference bearer, so the proxy tells rooms apart (014-AC20).
func (a *Accounts) connectorBearer(instance, room string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.connBearers == nil {
		a.connBearers = map[string]string{}
	}
	key := instance + "|" + room
	b := a.connBearers[key]
	if b == "" {
		b = sandbox.NewBearer()
		a.connBearers[key] = b
	}
	return b
}

// grantConnectors gives room the connectors g on account ap's proxy, and takes them off the proxy
// the room used before; no g revokes them (014-AC21, 014-AC24).
func (a *Accounts) grantConnectors(instance, room string, ap *accountProxy, g []ConnectorGrant) string {
	key := instance + "|" + room
	a.mu.Lock()
	prev := a.connAccount[key]
	a.mu.Unlock()
	if prev != "" && (ap == nil || prev != ap.acc.Name) {
		a.mu.Lock()
		old := a.px[prev]
		a.mu.Unlock()
		if old != nil {
			old.p.AllowConnectors(instance, key, "", nil)
		}
	}
	if ap == nil || len(g) == 0 {
		if ap != nil {
			ap.p.AllowConnectors(instance, key, "", nil)
		}
		a.mu.Lock()
		delete(a.connAccount, key)
		a.mu.Unlock()
		return ""
	}
	b := a.connectorBearer(instance, room)
	ids := make([]string, len(g))
	for i := range g {
		ids[i] = g[i].ID
	}
	ap.p.AllowConnectors(instance, key, b, ids)
	a.mu.Lock()
	if a.connAccount == nil {
		a.connAccount = map[string]string{}
	}
	a.connAccount[key] = ap.acc.Name
	a.mu.Unlock()
	return b
}

// dropConnectorRooms revokes the connector grants of instance's rooms gone from roomAccount.
func (a *Accounts) dropConnectorRooms(instance string, roomAccount map[string]string) {
	a.mu.Lock()
	var gone []string
	for key := range a.connAccount {
		inst, room, _ := strings.Cut(key, "|")
		if _, ok := roomAccount[room]; inst == instance && !ok {
			gone = append(gone, room)
		}
	}
	a.mu.Unlock()
	for _, room := range gone {
		a.grantConnectors(instance, room, nil, nil)
	}
}

// ConnectorEvent is a change the host made to a room's connectors, told to the room (014-AC19,
// 014-AC23): Kind is "enable", "disable" or "actions".
type ConnectorEvent struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Actions bool   `json:"actions,omitempty"`
}

// ConnectorView is one connector of a room's account as the daemon page shows it (014-AC18).
type ConnectorView struct {
	ConnectorInfo
	On      bool `json:"on"`
	Actions bool `json:"actions"`
}
