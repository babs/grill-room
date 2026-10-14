package plugin

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 009-AC15: the change against the approved version, components and files by hash.
func TestDiff(t *testing.T) {
	old := []File{{Path: "a", SHA256: "1"}, {Path: "b", SHA256: "2"}, {Path: "gone", SHA256: "3"}}
	cur := []File{{Path: "a", SHA256: "1"}, {Path: "b", SHA256: "9"}, {Path: "new", SHA256: "4"}}
	oldM := &Manifest{Skills: []string{"x", "y"}, Hooks: []Hook{{"Stop", "a.sh"}}, MCPServers: []Server{{Name: "m"}}}
	newM := &Manifest{Skills: []string{"y", "z"}, Hooks: []Hook{{"Stop", "b.sh"}}, MCPServers: []Server{{Name: "m"}},
		LSPServers: []Server{{Name: "go"}}, Commands: []string{"c"}, Agents: []string{"g"}, Bin: []string{"t"}}
	require.Equal(t, Change{
		ComponentsAdded:   []string{"agent:g", "bin:t", "command:c", "hook:Stop: b.sh", "lsp:go", "skill:z"},
		ComponentsRemoved: []string{"hook:Stop: a.sh", "skill:x"},
		FilesAdded:        []string{"new"},
		FilesRemoved:      []string{"gone"},
		FilesChanged:      []string{"b"},
	}, Diff(old, cur, oldM, newM))
	require.Equal(t, Change{}, Diff(old, old, nil, nil))
}

// 009-AC15: compare links on GitHub and GitLab, none elsewhere.
func TestCompareURL(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for repo, want := range map[string]string{
		"https://github.com/babs/skills":        "https://github.com/babs/skills/compare/" + a + "..." + b,
		"https://github.com/babs/skills.git/":   "https://github.com/babs/skills/compare/" + a + "..." + b,
		"https://gitlab.com/group/sub/repo.git": "https://gitlab.com/group/sub/repo/-/compare/" + a + "..." + b,
		"https://gitlab.example.org/g/r":        "https://gitlab.example.org/g/r/-/compare/" + a + "..." + b,
		"https://codeberg.org/o/r":              "",
		"https://github.com/onlyowner":          "",
		"http://github.com/o/r":                 "",
		"https://u:p@github.com/o/r":            "",
		"::bad":                                 "",
	} {
		require.Equal(t, want, CompareURL(repo, a, b), repo)
	}
	require.Empty(t, CompareURL("https://github.com/o/r", "main", b))
}

// 009-AC5: the room links the pinned commit on GitHub and GitLab, nothing elsewhere.
func TestCommitURL(t *testing.T) {
	c := strings.Repeat("c", 40)
	for repo, want := range map[string]string{
		"https://github.com/babs/skills.git":    "https://github.com/babs/skills/commit/" + c,
		"https://gitlab.com/group/sub/repo.git": "https://gitlab.com/group/sub/repo/-/commit/" + c,
		"https://codeberg.org/o/r":              "",
		"https://u:p@github.com/o/r":            "",
	} {
		require.Equal(t, want, CommitURL(repo, c), repo)
	}
	require.Empty(t, CommitURL("https://github.com/o/r", "main"))
}
