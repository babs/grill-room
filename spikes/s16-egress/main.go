//go:build spike

// Command s16-egress runs Claude Code turns in the production sandbox (internal/sandbox) behind the
// production bearer-swap proxy (internal/proxy), on this machine's Claude login, and saves what
// squid, the proxy and Claude Code saw. Spike S16, see spikes/S16-anthropic-egress.md.
//
//	go run -tags spike ./spikes/s16-egress -out <dir> <scenario>
//
// Scenarios: tools-prod, tools-skip-preflight (one haiku turn each), mcp (no model
// call), refresh-401 (a forged token: upstream 401, no usage), mcp-route (one haiku turn, connector S16_CONNECTOR through the proxy), connectors (no model call: lists the account's connectors and their tool names, read only).
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/creds"
	"github.com/babs/grill-room/internal/proxy"
	"github.com/babs/grill-room/internal/sandbox"
)

const (
	label    = "grill-room-spike=s16"
	toolsMsg = `Do these three steps in order, then reply with a short report.
1) Use the WebSearch tool to search the web for "Debian 13 trixie release date" and cite 2 source URLs.
2) Use the WebFetch tool on https://pypi.org/project/requests/ and give the latest version shown.
3) Use the WebFetch tool on https://example.com/ and give the page title.
For every tool call that fails, quote its exact error text. Do not use any other tool.`
)

// Security: the saved logs never hold a bearer, dummy or real.
var bearerRe = regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]+`)

func main() {
	out := flag.String("out", ".scratchpad/s16", "directory for the captured logs")
	image := flag.String("image", "grill-room-sandbox:local", "sandbox image")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: s16-egress [-out dir] [-image ref] tools-prod|tools-skip-preflight|mcp|mcp-route|refresh-401|connectors")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := run(ctx, flag.Arg(0), *out, *image)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "s16:", err)
		os.Exit(1)
	}
}

func save(dir, name string, b []byte) {
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, bearerRe.ReplaceAll(b, []byte("sk-ant-REDACTED")), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "save:", err)
		return
	}
	fmt.Println("saved", p)
}

func run(ctx context.Context, scenario, outRoot, image string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	// Read only: creds.Reader never writes the file.
	src := creds.NewSource(creds.ForConfigDir(filepath.Join(home, ".claude")))
	tok, err := src.Get(ctx)
	if err != nil {
		return fmt.Errorf("read the login: %w", err)
	}
	if tok.Expired(time.Now().Add(10 * time.Minute)) {
		return errors.New("the login expires within 10 minutes: refresh it on the host first")
	}
	dir := filepath.Join(outRoot, scenario+"-"+time.Now().UTC().Format("20060102T150405"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	upstream, _ := url.Parse("https://api.anthropic.com")
	if scenario == "connectors" {
		return connectors(ctx, src, upstream, dir)
	}

	logPath := filepath.Join(dir, "proxy.jsonl")
	zc := zap.NewProductionConfig()
	zc.OutputPaths = []string{logPath}
	log, err := zc.Build()
	if err != nil {
		return err
	}
	defer func() { _ = log.Sync() }()
	var ts proxy.TokenSource = src
	if scenario == "refresh-401" {
		ts = badToken{}
	}
	px := proxy.New(upstream, ts, log)
	bearer := sandbox.NewBearer()
	px.Allow("s16", bearer)

	bindIP, gw, err := sandbox.HostAddr(ctx, sandbox.Docker)
	if err != nil {
		return err
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", net.JoinHostPort(bindIP, "0"))
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/", px)
	// The fix under test: connector traffic routed through the bearer-swap proxy by config, no MitM.
	mux.Handle("/v1/mcp/", mcpSwap(src, bearer, log))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	name := "s16-" + strings.ReplaceAll(scenario, "-", "") + fmt.Sprint(time.Now().Unix()%100000)
	sb := sandbox.New(sandbox.Config{Image: image, Name: name, Proxy: net.JoinHostPort(gw, port), Labels: []string{label}}, sandbox.Docker, log)
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		sb.Close(cctx)
		n, err := sandbox.RemoveLabelled(cctx, sandbox.Docker, label)
		fmt.Println("cleanup: removed", n, "leftover objects", err)
	}()
	ctr := "grill-room-" + name
	dump := func() {
		if b, err := sandbox.Docker(ctx, nil, "logs", ctr+"-egress"); err == nil {
			save(dir, "squid.log", b)
		}
		if b, err := sandbox.Docker(ctx, nil, "exec", ctr, "cat", "/tmp/cc-debug.log"); err == nil {
			save(dir, "cc-debug.log", b)
		}
		if b, err := sandbox.Docker(ctx, nil, "exec", ctr, "cat", "/home/agent/.claude/.credentials.json"); err == nil {
			save(dir, "sandbox-credentials-after.json", b)
		}
		st := px.Stats()
		fmt.Printf("proxy: requests=%v unauthorized=%d\n", st.Requests, st.Unauthorized)
	}

	switch scenario {
	case "refresh-401":
		// Upstream refuses the forged token: Claude Code then tries its own refresh, through squid.
		res, err := sb.RunClaude(ctx, sandbox.Request{Prompt: "say ok", Model: "haiku", Bearer: bearer, Args: []string{"--debug-file", "/tmp/cc-debug.log"}})
		if res != nil {
			b, _ := json.MarshalIndent(res, "", "  ")
			save(dir, "result.json", b)
		}
		fmt.Println("turn error:", err)
		dump()
		return nil
	case "tools-prod", "tools-skip-preflight":
		args := []string{"--debug-file", "/tmp/cc-debug.log"}
		if scenario == "tools-skip-preflight" {
			args = append(args, "--settings", `{"skipWebFetchPreflight":true}`)
		}
		res, err := sb.RunClaude(ctx, sandbox.Request{Prompt: toolsMsg, Model: "haiku", Bearer: bearer, Args: args})
		if res != nil {
			b, _ := json.MarshalIndent(res, "", "  ")
			save(dir, "result.json", b)
			if s, err := sb.ReadSession(ctx, res.SessionID, 0); err == nil {
				save(dir, "session.jsonl", s)
			}
		}
		dump()
		return err
	case "mcp":
		return mcp(ctx, sb, ctr, bearer, dir, dump)
	case "mcp-route":
		id, err := connectorID(ctx, src, upstream, os.Getenv("S16_CONNECTOR"))
		if err != nil {
			return err
		}
		if err := sb.Start(ctx); err != nil {
			return err
		}
		cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"conn": map[string]any{
			"type": "http", "url": "http://proxy:8787/v1/mcp/" + id,
			"headers": map[string]string{"Authorization": "Bearer " + bearer, "X-Mcp-Client-Session-Id": rand.Text()}}}})
		if _, err := sandbox.Docker(ctx, nil, "exec", ctr, "bash", "-c", `printf '%s' "$1" > /tmp/mcp.json`, "-", string(cfg)); err != nil {
			return err
		}
		res, err := sb.RunClaude(ctx, sandbox.Request{Model: "haiku", Bearer: bearer,
			Prompt: "List the exact names of the tools you have from the MCP server named conn, one per line. Do not call any tool.",
			Args: []string{"--debug-file", "/tmp/cc-debug.log", "--mcp-config", "/tmp/mcp.json",
				// Security: the Calendar connector's write tools stay uncallable even if the model ignores the prompt.
				"--disallowedTools", "mcp__conn__create_event,mcp__conn__update_event,mcp__conn__delete_event,mcp__conn__respond_to_event"}})
		if res != nil {
			b, _ := json.MarshalIndent(res, "", "  ")
			save(dir, "result.json", b)
		}
		dump()
		return err
	}
	return fmt.Errorf("unknown scenario %q", scenario)
}

// mcp starts the sandbox, writes the production stubs, then runs `claude mcp list` (no model call)
// with claude.ai connectors on: once with the production stub scopes, once adding user:mcp_servers.
func mcp(ctx context.Context, sb *sandbox.Sandbox, ctr, bearer, dir string, dump func()) error {
	if err := sb.Start(ctx); err != nil {
		return err
	}
	stub := func(scopes []string) error {
		c, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{
			"accessToken": bearer, "refreshToken": "grill-room-dummy-refresh",
			"expiresAt": time.Now().AddDate(10, 0, 0).UnixMilli(), "subscriptionType": "max", "scopes": scopes}})
		a, _ := json.Marshal(map[string]any{"oauthAccount": map[string]any{
			"accountUuid": "00000000-0000-4000-8000-000000000001", "emailAddress": "agent@grill-room.invalid",
			"organizationUuid": "00000000-0000-4000-8000-000000000002", "billingType": "stripe_subscription",
			"accountCreatedAt": "2025-01-01T00:00:00Z", "subscriptionCreatedAt": "2025-01-01T00:00:00Z",
			"ccOnboardingFlags": map[string]any{}, "profileFetchedAt": time.Now().UnixMilli()}})
		script := `mkdir -p ~/.claude && printf '%s' "$1" > ~/.claude/.credentials.json && printf '%s' "$2" > ~/.claude.json`
		_, err := sandbox.Docker(ctx, nil, "exec", ctr, "bash", "-c", script, "-", string(c), string(a))
		return err
	}
	prod := []string{"user:inference", "user:profile", "user:sessions:claude_code"}
	for i, scopes := range [][]string{prod, append(prod, "user:mcp_servers")} {
		if err := stub(scopes); err != nil {
			return fmt.Errorf("stub: %w", err)
		}
		dbg := fmt.Sprintf("/tmp/mcp-%d.log", i)
		b, err := sandbox.Docker(ctx, nil, "exec", "-e", "ENABLE_CLAUDEAI_MCP_SERVERS=true", ctr,
			"timeout", "60", "claude", "--debug-file", dbg, "mcp", "list")
		save(dir, fmt.Sprintf("mcp-list-%d.out", i), append(b, []byte(fmt.Sprint("\nerr: ", err))...))
		if d, err := sandbox.Docker(ctx, nil, "exec", ctr, "cat", dbg); err == nil {
			save(dir, fmt.Sprintf("mcp-list-%d-debug.log", i), d)
		}
	}
	// What a prompt-injected agent can do: read the dummy bearer and send it to any proxy path.
	probe := `t=$(jq -r .claudeAiOauth.accessToken ~/.claude/.credentials.json); curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $t" -H 'anthropic-version: 2023-06-01' -H 'anthropic-beta: mcp-servers-2025-12-04' 'http://proxy:8787/v1/mcp_servers?limit=1'`
	b, err := sandbox.Docker(ctx, nil, "exec", ctr, "bash", "-c", probe)
	save(dir, "agent-probe.out", append(b, []byte(fmt.Sprint("\nerr: ", err))...))
	dump()
	return nil
}

// connectors lists, from the host with the real token, the account's claude.ai connectors the way
// Claude Code fetches them, then each connector's tool names on mcp-proxy.anthropic.com. Only
// initialize and tools/list are sent: no connector tool is called.
func connectors(ctx context.Context, src *creds.Source, upstream *url.URL, dir string) error {
	tok, err := src.Get(ctx)
	if err != nil {
		return err
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstream.String()+"/v1/mcp_servers?limit=1000", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("anthropic-beta", "mcp-servers-2025-12-04")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	fmt.Println("GET /v1/mcp_servers:", resp.StatusCode)
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		save(dir, "mcp_servers.raw", body)
		return fmt.Errorf("mcp_servers: %w", err)
	}
	var report bytes.Buffer
	for _, s := range list.Data {
		id, _ := s["id"].(string)
		keys := make([]string, 0, len(s))
		for k := range s {
			keys = append(keys, k)
		}
		fmt.Fprintf(&report, "server id=%s name=%v url=%v keys=%v\n", id, s["display_name"], s["url"], keys)
		names, status, err := toolNames(ctx, hc, "https://mcp-proxy.anthropic.com/v1/mcp/"+id, tok.AccessToken)
		fmt.Fprintf(&report, "  mcp-proxy initialize/tools/list status=%d err=%v tools=%d %v\n", status, err, len(names), names)
	}
	save(dir, "connectors.txt", report.Bytes())
	return nil
}

// badToken is a token upstream refuses with 401, to exercise the sandbox's refresh path.
type badToken struct{}

func (badToken) Get(context.Context) (creds.Token, error) {
	return creds.Token{AccessToken: "sk-ant-oat01-s16-invalid"}, nil //nolint:gosec // a forged token upstream must refuse
}
func (badToken) Invalidate() {}

// mcpSwap forwards /v1/mcp/<id> to mcp-proxy.anthropic.com, swapping the sandbox's dummy bearer
// for the real one; any other bearer gets 403.
func mcpSwap(src *creds.Source, bearer string, log *zap.Logger) http.Handler {
	up, _ := url.Parse("https://mcp-proxy.anthropic.com")
	rp := &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(up)
			pr.Out.Host = up.Host
			if tok, err := src.Get(pr.In.Context()); err == nil {
				pr.Out.Header.Set("Authorization", "Bearer "+tok.AccessToken)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			log.Info("mcp_upstream_response", zap.String("method", resp.Request.Method),
				zap.String("path", resp.Request.URL.Path), zap.Int("status", resp.StatusCode))
			return nil
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+bearer {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		rp.ServeHTTP(w, r)
	})
}

// connectorID returns the id of the account's connector named name.
func connectorID(ctx context.Context, src *creds.Source, upstream *url.URL, name string) (string, error) {
	tok, err := src.Get(ctx)
	if err != nil {
		return "", err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstream.String()+"/v1/mcp_servers?limit=1000", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("anthropic-beta", "mcp-servers-2025-12-04")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return "", err
	}
	for _, s := range list.Data {
		if s.Name == name {
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("no connector named %q", name)
}

// toolNames runs MCP initialize then tools/list over streamable HTTP and returns the tool names.
func toolNames(ctx context.Context, hc *http.Client, endpoint, token string) ([]string, int, error) {
	session := ""
	// mcp-proxy refuses a request without a client session id (400, measured).
	client := rand.Text()
	call := func(body string) (int, []byte, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("anthropic-beta", "mcp-servers-2025-12-04")
		req.Header.Set("X-Mcp-Client-Session-Id", client)
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, err := hc.Do(req)
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
	st, b, err := call(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"s16-spike","version":"0"}}}`)
	if err != nil || st >= 300 {
		return nil, st, fmt.Errorf("initialize: %v %.200s", err, b)
	}
	_, _, _ = call(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	st, b, err = call(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if err != nil || st >= 300 {
		return nil, st, fmt.Errorf("tools/list: %v %.200s", err, b)
	}
	var r struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, st, fmt.Errorf("tools/list: %w: %.200s", err, b)
	}
	names := make([]string, 0, len(r.Result.Tools))
	for _, t := range r.Result.Tools {
		names = append(names, t.Name)
	}
	return names, st, nil
}
