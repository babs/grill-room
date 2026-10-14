package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/plugin"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for p, c := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}
	}
	return dir
}

// 009-AC13: the fake answers `claude plugin validate --json` as 2.1.296 does: success with a
// plugin.json naming the plugin, failure with the first problem when it is missing, invalid or
// names none.
func TestFakeValidate(t *testing.T) {
	for _, tc := range []struct {
		name, manifest, path string
		ok                   bool
	}{
		{"valid", `{"name":"rich"}`, "", true},
		{"missing", "", "manifest", false},
		{"invalid JSON", `{bad`, "json", false},
		{"empty name", `{"name":""}`, "name", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{"skills/x/SKILL.md": "x"}
			if tc.manifest != "" {
				files[".claude-plugin/plugin.json"] = tc.manifest
			}
			dir := writeTree(t, files)
			var buf bytes.Buffer
			code := run([]string{"plugin", "validate", "--json", dir}, strings.NewReader(""), &buf, "/", "/")
			var v struct {
				Success  bool `json:"success"`
				Manifest struct {
					Errors []struct{ Path string } `json:"errors"`
				} `json:"manifest"`
			}
			if err := json.Unmarshal(buf.Bytes(), &v); err != nil {
				t.Fatalf("%s: %v", buf.String(), err)
			}
			if v.Success != tc.ok || (code == 0) != tc.ok {
				t.Fatalf("success %v, exit %d, want ok %v", v.Success, code, tc.ok)
			}
			if !tc.ok && (len(v.Manifest.Errors) != 1 || v.Manifest.Errors[0].Path != tc.path) {
				t.Fatalf("errors %+v, want %s", v.Manifest.Errors, tc.path)
			}
		})
	}
}

// 009-AC13: `claude plugin details` lists the folder's components and a token cost that
// plugin.ParseDetails reads; an unknown name fails.
func TestFakeDetails(t *testing.T) {
	dir := writeTree(t, map[string]string{
		".claude-plugin/plugin.json": `{"name":"rich"}`,
		"skills/run/SKILL.md":        strings.Repeat("r", 100),
		"skills/hello/SKILL.md":      "h",
		"agents/helper.md":           "a",
		"hooks/hooks.json":           `{"hooks":{"SessionStart":[]}}`,
		".mcp.json":                  `{"mcpServers":{"local":{},"docs":{}}}`,
	})
	other := writeTree(t, map[string]string{".claude-plugin/plugin.json": `{"name":"other"}`})
	var buf bytes.Buffer
	if code := run([]string{"--plugin-dir", other, "--plugin-dir", dir, "plugin", "details", "rich"}, strings.NewReader(""), &buf, "/", "/"); code != 0 {
		t.Fatalf("exit %d: %s", code, buf.String())
	}
	d := plugin.ParseDetails(buf.String())
	want := []plugin.Component{{Kind: "Skills", Count: 2, Names: []string{"hello", "run"}}, {Kind: "Agents", Count: 1, Names: []string{"helper"}},
		{Kind: "Hooks", Count: 1, Names: []string{"SessionStart"}}, {Kind: "MCP servers", Count: 2, Names: []string{"docs", "local"}}, {Kind: "LSP servers"}}
	got, _ := json.Marshal(d.Components)
	exp, _ := json.Marshal(want)
	if !bytes.Equal(got, exp) || !d.CostKnown || d.AlwaysOnTokens != 3 {
		t.Fatalf("details %+v from\n%s", d, buf.String())
	}
	buf.Reset()
	if code := run([]string{"--plugin-dir", other, "plugin", "details", "rich"}, strings.NewReader(""), &buf, "/", "/"); code != 1 || !strings.Contains(buf.String(), `"rich" not found`) {
		t.Fatalf("unknown plugin: exit %d %s", code, buf.String())
	}
}

// 009-AC17: FAKE_PLUGINS lists each --plugin-dir in order with its files and their sha256,
// sorted; with none it says so.
func TestFakePlugins(t *testing.T) {
	cwd, home := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := writeTree(t, map[string]string{"skills/x/SKILL.md": "x"})
	b := writeTree(t, map[string]string{"z.txt": "z", ".claude-plugin/plugin.json": "{}"})
	h := func(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
	o, code := call(t, cwd, home, "FAKE_PLUGINS", "--plugin-dir", a, "--plugin-dir", b)
	want := "Plugin dirs: " + a + " [skills/x/SKILL.md=" + h("x") + "] | " + b + " [.claude-plugin/plugin.json=" + h("{}") + ", z.txt=" + h("z") + "]."
	if code != 0 || o.Result != want {
		t.Fatalf("got %q (%d), want %q", o.Result, code, want)
	}
	if o, _ := call(t, cwd, home, "FAKE_PLUGINS"); o.Result != "Plugin dirs: none." {
		t.Fatalf("no plugin: %q", o.Result)
	}
}
