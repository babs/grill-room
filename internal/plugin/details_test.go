package plugin

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// 009-AC13, 009-AC14: golden outputs of `claude plugin details` from Claude Code 2.1.296.
func TestParseDetailsGolden(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile("testdata/" + name)
		require.NoError(t, err)
		return string(b)
	}
	d := ParseDetails(read("details-2.1.296.txt"))
	require.Equal(t, "babs", d.Name)
	require.Equal(t, "1.18.0", d.Version)
	require.True(t, d.CostKnown)
	require.Equal(t, 1513, d.AlwaysOnTokens)
	require.Len(t, d.Components, 5)
	require.Equal(t, "Skills", d.Components[0].Kind)
	require.Equal(t, 13, d.Components[0].Count)
	require.Contains(t, d.Components[0].Names, "spec-feature")
	require.Len(t, d.Components[0].Names, 13)
	require.Equal(t, Component{Kind: "LSP servers", Count: 0}, d.Components[4])

	d = ParseDetails(read("details-rich-2.1.296.txt"))
	require.Equal(t, Details{
		Name: "rich", Version: "0.1.0", AlwaysOnTokens: 12, CostKnown: true,
		Components: []Component{
			{Kind: "Skills", Count: 2, Names: []string{"hello", "run"}},
			{Kind: "Agents", Count: 1, Names: []string{"helper"}},
			{Kind: "Hooks", Count: 1, Names: []string{"SessionStart"}},
			{Kind: "MCP servers", Count: 2, Names: []string{"docs", "local"}},
			{Kind: "LSP servers", Count: 0},
		},
	}, d)
}

func TestParseDetailsShapes(t *testing.T) {
	for out, want := range map[string]int{
		"Projected token cost\n  Always-on:   ~1.5k tok   added\n":   1500,
		"Projected token cost\r\n  Always-on:   < 20 tok\r\n":        20,
		"Projected token cost\n  Always-on:   ~12,345 tok   added\n": 12345,
	} {
		d := ParseDetails(out)
		require.True(t, d.CostKnown, out)
		require.Equal(t, want, d.AlwaysOnTokens, out)
	}
	for _, out := range []string{"", "Error: plugin not found\n", "Projected token cost\n  Always-on: unknown\n", "  Always-on:   ~12 tok\n"} {
		d := ParseDetails(out)
		require.False(t, d.CostKnown, out)
	}
}
