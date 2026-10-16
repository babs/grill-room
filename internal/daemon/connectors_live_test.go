package daemon

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/babs/grill-room/internal/sandbox"
)

// TestRealConnector spends one real haiku request, with GRILL_ROOM_TEST_DOCKER=1 and
// GRILL_ROOM_TEST_REAL_CLAUDE=1, on this host's login (CLAUDE_CONFIG_DIR, read only). 014-AC18
// 014-AC20 014-AC23 014-AC25: the supervisor's accounts list the login's connectors on the host; a
// room with one on, GRILL_ROOM_TEST_CONNECTOR or the first ready, reads only, gets it through the
// production proxy in a production sandbox, and the model lists its tools without calling any.
// Afterwards no copy of the real token is in the sandbox, its volumes or its inspect output, the
// sandbox's credentials are the dummy ones written before the turn, and the egress log holds no
// Anthropic CONNECT. Only names and counts are printed.
func TestRealConnector(t *testing.T) {
	if os.Getenv("GRILL_ROOM_TEST_DOCKER") != "1" || os.Getenv("GRILL_ROOM_TEST_REAL_CLAUDE") != "1" {
		t.Skip("set GRILL_ROOM_TEST_DOCKER=1 and GRILL_ROOM_TEST_REAL_CLAUDE=1 to spend one haiku request")
	}
	img := os.Getenv("GRILL_ROOM_SANDBOX_IMAGE")
	if img == "" {
		img = "grill-room-sandbox:local"
	}
	ctx := context.Background()
	claude := os.Getenv("CLAUDE_CONFIG_DIR")
	credFile := filepath.Join(claude, ".credentials.json")
	before, err := os.Stat(credFile)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	up, _ := url.Parse("https://api.anthropic.com")
	a, err := LoadAccounts(AccountsOptions{Paths: Paths{Config: dir, State: dir, Cache: dir}, ClaudeConfigDir: claude, Upstream: up,
		Docker: sandbox.Docker, Log: zaptest.NewLogger(t, zaptest.Level(zap.WarnLevel))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	list, err := a.Connectors(ctx, "default", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range list {
		t.Logf("connector %q: %s, %d tools", c.Name, c.State, len(c.Tools))
	}
	want := os.Getenv("GRILL_ROOM_TEST_CONNECTOR")
	i := slices.IndexFunc(list, func(c ConnectorInfo) bool { return c.State == ConnectorReady && (want == "" || c.Name == want) })
	if i < 0 {
		t.Fatalf("no ready connector named %q", want)
	}
	const room = "https://live.example/r/connector"
	px := a.Access(ctx, "live", map[string]string{room: "default"}, map[string][]RoomConnector{room: {{ID: list[i].ID}}}, false).Proxies[room]
	if px.Error != "" || len(px.Connectors) != 1 {
		t.Fatalf("access: %q, %d connectors", px.Error, len(px.Connectors))
	}
	g := px.Connectors[0]
	var reads []string
	for _, tool := range list[i].Tools {
		if tool.ReadOnly {
			reads = append(reads, tool.Name)
		}
	}
	t.Logf("%s: %d read tools, %d disallowed", g.Name, len(reads), len(g.Disallowed))
	name := fmt.Sprintf("conn-%d", time.Now().UnixNano())
	s := sandbox.New(sandbox.Config{Image: img, Name: name, Proxy: px.Addr}, sandbox.Docker,
		zaptest.NewLogger(t, zaptest.Level(zap.WarnLevel)))
	t.Cleanup(func() { s.Close(context.Background()) })
	tctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	res, err := s.RunClaude(tctx, sandbox.Request{Model: "haiku", Bearer: px.Bearer, ConnectorBearer: px.ConnectorBearer,
		Connectors: []sandbox.Connector{{Key: g.Key, ID: g.ID, Disallowed: g.Disallowed}},
		Prompt:     "List the exact names of the tools you have from the MCP server named " + g.Key + ", one per line. Do not call any tool."})
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	named := 0
	for _, r := range reads {
		if strings.Contains(res.Result, r) {
			named++
		}
	}
	t.Logf("the model named %d of %d read tools", named, len(reads))
	if named == 0 {
		t.Errorf("the model listed none of the connector's read tools: %q", res.Result)
	}
	tok, err := a.Token(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	counts, err := s.Scan(ctx, []byte(tok.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	for what, n := range counts {
		t.Logf("%s: %d copies of the real token", what, n)
		if n != 0 {
			t.Errorf("%s holds the real token", what)
		}
	}
	creds, err := sandbox.Docker(ctx, nil, "exec", "grill-room-"+name, "cat", "/home/agent/.claude/.credentials.json")
	if err != nil || !bytes.Contains(creds, []byte(px.Bearer)) {
		t.Errorf("sandbox credentials changed: %v", err)
	}
	log, err := sandbox.Docker(ctx, nil, "logs", "grill-room-"+name+"-egress")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(log, []byte("anthropic.com")) || bytes.Contains(log, []byte("claude.")) {
		t.Errorf("the egress log names an Anthropic host:\n%s", log)
	}
	after, err := os.Stat(credFile)
	if err != nil || !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("host credentials file changed: %v", err)
	}
}
