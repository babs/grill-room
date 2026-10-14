package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// pluginCommand answers `claude plugin validate --json <dir>` and `claude --plugin-dir <dir>
// plugin details <name>`; ok is false for any other command line.
func pluginCommand(args []string, out io.Writer) (code int, ok bool) {
	i := slices.Index(args, "plugin")
	if i < 0 || i+2 >= len(args) {
		return 0, false
	}
	switch args[i+1] {
	case "validate":
		return validate(args[len(args)-1], out), true
	case "details":
		var dirs []string
		for j, a := range args[:i] {
			if a == "--plugin-dir" && j+1 < i {
				dirs = append(dirs, args[j+1])
			}
		}
		return details(dirs, args[len(args)-1], out), true
	}
	return 0, false
}

type problem struct {
	Path    string `json:"path"`
	Message string `json:"message"`
	Code    any    `json:"code"`
}

// manifestName reads dir's plugin.json and returns its name, or the problem Claude Code reports.
func manifestName(dir string) (string, *problem) {
	b, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json")) //nolint:gosec // a test double reading the plugin it was handed
	if err != nil {
		return "", &problem{Path: "manifest", Message: "No manifest found at .claude-plugin/plugin.json"}
	}
	var m struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return "", &problem{Path: "json", Message: "Invalid JSON syntax: " + err.Error()}
	}
	if m.Name == "" {
		return "", &problem{Path: "name", Message: "Plugin name cannot be empty"}
	}
	return m.Name, nil
}

func validate(dir string, out io.Writer) int {
	file := filepath.Join(dir, ".claude-plugin", "plugin.json")
	_, p := manifestName(dir)
	errs := []problem{}
	if p != nil {
		errs = append(errs, *p)
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{"success": p == nil, "strict": false, "target": file,
		"manifest": map[string]any{"file": file, "type": "plugin", "errors": errs, "warnings": []problem{
			{Path: "author", Message: "No author information provided. Consider adding author details for plugin attribution"}},
			"notes": []any{}, "gatingHooks": []any{}},
		"contents": []any{}, "advice": []any{}})
	if p != nil {
		return 1
	}
	return 0
}

// names lists what glob matches under dir, as name gives it, sorted.
func names(dir, glob string, name func(string) string) []string {
	ms, _ := filepath.Glob(filepath.Join(dir, glob))
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, name(m))
	}
	sort.Strings(out)
	return out
}

// keys lists the keys of the object field holds in the JSON file at path, sorted.
func keys(path, field string) []string {
	b, err := os.ReadFile(path) //nolint:gosec // see manifestName
	if err != nil {
		return nil
	}
	var doc map[string]map[string]json.RawMessage
	if json.Unmarshal(b, &doc) != nil {
		return nil
	}
	out := make([]string, 0, len(doc[field]))
	for k := range doc[field] {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func details(dirs []string, name string, out io.Writer) int {
	dir := ""
	for _, d := range dirs {
		if n, _ := manifestName(d); n == name {
			dir = d
		}
	}
	if dir == "" {
		_, _ = fmt.Fprintf(out, "Plugin %q not found. Run `claude plugin list` to see installed plugins, or pass --plugin-dir <path> to load one from disk.\n", name)
		return 1
	}
	skills := names(dir, "skills/*/SKILL.md", func(p string) string { return filepath.Base(filepath.Dir(p)) })
	agents := names(dir, "agents/*.md", func(p string) string { return strings.TrimSuffix(filepath.Base(p), ".md") })
	hooks := keys(filepath.Join(dir, "hooks", "hooks.json"), "hooks")
	mcp := keys(filepath.Join(dir, ".mcp.json"), "mcpServers")
	// A stand-in for the real estimate: a token per 40 bytes of what every session loads.
	var size int64
	for _, g := range []string{"skills/*/SKILL.md", "agents/*.md"} {
		ms, _ := filepath.Glob(filepath.Join(dir, g))
		for _, m := range ms {
			if st, err := os.Stat(m); err == nil { //nolint:gosec // see manifestName
				size += st.Size()
			}
		}
	}
	line := func(kind string, ns []string) string {
		if len(ns) == 0 {
			return fmt.Sprintf("  %s (0)\n", kind)
		}
		return fmt.Sprintf("  %s (%d)  %s\n", kind, len(ns), strings.Join(ns, ", "))
	}
	_, _ = fmt.Fprintf(out, "%s\n  Source: %s@inline\n\nComponent inventory\n%s%s%s%s  LSP servers (0)\n\nProjected token cost\n  Always-on:   ~%d tok   added to every session\n",
		name, name, line("Skills", skills), line("Agents", agents), line("Hooks", hooks), line("MCP servers", mcp), (size+39)/40)
	return 0
}

// listPlugins answers FAKE_PLUGINS: each plugin directory in --plugin-dir order, with its files
// and their sha256, sorted.
func listPlugins(dirs []string) (string, error) {
	if len(dirs) == 0 {
		return "Plugin dirs: none.", nil
	}
	parts := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		var files []string
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error { //nolint:gosec // see manifestName
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			b, err := os.ReadFile(p) //nolint:gosec // see manifestName
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(dir, p)
			sum := sha256.Sum256(b)
			files = append(files, filepath.ToSlash(rel)+"="+hex.EncodeToString(sum[:]))
			return nil
		})
		if err != nil {
			return "", err
		}
		sort.Strings(files)
		parts = append(parts, dir+" ["+strings.Join(files, ", ")+"]")
	}
	return "Plugin dirs: " + strings.Join(parts, " | ") + ".", nil
}

// readJSON decodes the JSON file at path into v; a missing or broken file leaves v as it was.
func readJSON(path string, v any) {
	if b, err := os.ReadFile(path); err == nil { //nolint:gosec // the sandbox home's own files
		_ = json.Unmarshal(b, v)
	}
}

// sessionStartHooks runs each --plugin-dir's SessionStart command hooks, as Claude Code 2.1.296
// does before any API call, in cwd with CLAUDE_PLUGIN_ROOT and one CLAUDE_PLUGIN_OPTION_<KEY> per
// option value the daemon wrote: pluginConfigs in settings.json, pluginSecrets in
// .credentials.json, both under "<name>@inline" (009-AC23). It returns what they print, trimmed.
func sessionStartHooks(dirs []string, home, cwd string) []string {
	var settings struct {
		PluginConfigs map[string]struct {
			Options map[string]any `json:"options"`
		} `json:"pluginConfigs"`
	}
	var creds struct {
		PluginSecrets map[string]map[string]any `json:"pluginSecrets"`
	}
	readJSON(filepath.Join(home, ".claude", "settings.json"), &settings)
	readJSON(filepath.Join(home, ".claude", ".credentials.json"), &creds)
	var out []string
	for _, dir := range dirs {
		var cfg struct {
			Hooks map[string][]struct {
				Hooks []struct {
					Type    string `json:"type"`
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"hooks"`
		}
		readJSON(filepath.Join(dir, "hooks", "hooks.json"), &cfg)
		if len(cfg.Hooks["SessionStart"]) == 0 {
			continue
		}
		name, _ := manifestName(dir)
		env := append(os.Environ(), "CLAUDE_PLUGIN_ROOT="+dir)
		for _, values := range []map[string]any{settings.PluginConfigs[name+"@inline"].Options, creds.PluginSecrets[name+"@inline"]} {
			for k, v := range values {
				env = append(env, "CLAUDE_PLUGIN_OPTION_"+strings.ToUpper(k)+"="+fmt.Sprint(v))
			}
		}
		for _, m := range cfg.Hooks["SessionStart"] {
			for _, h := range m.Hooks {
				if h.Type != "command" {
					continue
				}
				cmd := exec.Command("sh", "-c", h.Command) //nolint:gosec,noctx // a test double running the hook it was handed, as claude does
				cmd.Dir, cmd.Env = cwd, env
				b, _ := cmd.Output() // a failing hook prints what it printed, as in Claude Code
				out = append(out, strings.TrimSpace(string(b)))
			}
		}
	}
	return out
}
