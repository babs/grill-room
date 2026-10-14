package plugin

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/babs/grill-room/internal/turn"
)

// MarketplacePath is where a repo lists the plugins it holds (009-AC6).
const MarketplacePath = ".claude-plugin/marketplace.json"

// ReasonNotInRepo is why an entry whose source is not a path in the repo cannot be picked.
const ReasonNotInRepo = "only plugins stored in this repo can be added"

// Marketplace is a repo's plugin list.
type Marketplace struct {
	Name    string
	Plugins []MarketEntry
}

// MarketEntry is one plugin of a marketplace. Root is its folder in the repo, "" for the repo
// root; it is set only when Pickable. Reason says why an entry cannot be picked.
type MarketEntry struct {
	Name        string
	Description string
	Version     string
	Root        string
	Pickable    bool
	Reason      string
	raw         map[string]json.RawMessage
}

// ReadMarketplace parses the tree's marketplace; nil when it has none, then the repo is one
// plugin rooted at its root.
func ReadMarketplace(tree []Entry) (*Marketplace, error) {
	byPath := make(map[string]Entry, len(tree))
	for _, e := range tree {
		byPath[e.Path] = e
	}
	e, ok := byPath[MarketplacePath]
	if !ok {
		return nil, nil
	}
	e, err := resolve(byPath, e)
	if err != nil {
		return nil, err
	}
	return ParseMarketplace(e.Data)
}

// ParseMarketplace reads marketplace.json. Only string sources, relative to the repo, resolve
// inside it; any other source is listed with ReasonNotInRepo.
func ParseMarketplace(data []byte) (*Marketplace, error) {
	var doc struct {
		Name     string `json:"name"`
		Metadata struct {
			PluginRoot string `json:"pluginRoot"`
		} `json:"metadata"`
		Plugins []map[string]json.RawMessage `json:"plugins"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, invalid("%s: %v", MarketplacePath, err)
	}
	m := &Marketplace{Name: doc.Name}
	for i, raw := range doc.Plugins {
		e := MarketEntry{raw: raw}
		str(raw, "name", &e.Name)
		str(raw, "description", &e.Description)
		str(raw, "version", &e.Version)
		if e.Name == "" {
			return nil, invalid("%s: plugin %d has no name", MarketplacePath, i+1)
		}
		var src string
		switch {
		case !str(raw, "source", &src):
			e.Reason = ReasonNotInRepo
		case strings.HasPrefix(src, "./") || src == ".":
			e.Root = src
		case doc.Metadata.PluginRoot != "" && !strings.Contains(src, ":"):
			e.Root = path.Join(doc.Metadata.PluginRoot, src)
		default:
			e.Reason = ReasonNotInRepo
		}
		if e.Reason == "" {
			if p, err := rootPrefix(e.Root); err != nil {
				e.Root, e.Reason = "", "its source leaves the repo"
			} else {
				e.Root, e.Pickable = strings.TrimSuffix(p, "/"), true
			}
		}
		m.Plugins = append(m.Plugins, e)
	}
	return m, nil
}

// str decodes raw[key] into s when it is a JSON string.
func str(raw map[string]json.RawMessage, key string, s *string) bool {
	v, ok := raw[key]
	return ok && json.Unmarshal(v, s) == nil
}

// PluginJSON builds the plugin.json Claude Code would read from this entry when the plugin has
// none: the entry minus the fields only a marketplace has.
func (e MarketEntry) PluginJSON() []byte {
	out := make(map[string]json.RawMessage, len(e.raw))
	for k, v := range e.raw {
		switch k {
		case "source", "category", "tags", "strict":
		default:
			out[k] = v
		}
	}
	b, _ := json.Marshal(out) // raw values decoded once already: cannot fail
	return b
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// CheckName refuses a plugin name that is not a safe folder name, or that is grill-room's own
// (009-AC10).
func CheckName(name string) error {
	switch {
	case strings.EqualFold(name, turn.PluginName):
		return invalid("the name %q is grill-room's own", name)
	case !nameRe.MatchString(name) || strings.Contains(name, ".."):
		return invalid("the name %q must be 1 to 64 letters, digits, dots, dashes or underscores", name)
	}
	return nil
}

// Option is one userConfig option a plugin declares.
type Option struct {
	Key         string `json:"key"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"`
	Sensitive   bool   `json:"sensitive,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Default     any    `json:"default,omitempty"`
}

// Hook is one hook command, as the approval sheet shows it.
type Hook struct {
	Event   string `json:"event"`
	Command string `json:"command"`
}

// Server is an MCP or LSP server: the command it runs or the URL it reaches.
type Server struct {
	Name    string `json:"name"`
	Command string `json:"command,omitempty"`
	URL     string `json:"url,omitempty"`
}

// Manifest is what a plugin declares and holds, read from its files without running it
// (009-AC14).
type Manifest struct {
	Name        string   `json:"name"`
	Version     string   `json:"version,omitempty"`
	Description string   `json:"description,omitempty"`
	Options     []Option `json:"options,omitempty"`
	Skills      []string `json:"skills,omitempty"`
	Commands    []string `json:"commands,omitempty"`
	Agents      []string `json:"agents,omitempty"`
	Hooks       []Hook   `json:"hooks,omitempty"`
	MCPServers  []Server `json:"mcp_servers,omitempty"`
	LSPServers  []Server `json:"lsp_servers,omitempty"`
	Bin         []string `json:"bin,omitempty"`
	Domains     []string `json:"domains,omitempty"`
	// Problems lists what could not be read: the approval sheet shows them, since a hook or a
	// server the reader missed would still run.
	Problems []string `json:"problems,omitempty"`
}

// Manifest reads the snapshot's plugin.json and the component files Claude Code loads by
// default or from the paths plugin.json names.
func (s *Snapshot) Manifest() (*Manifest, error) {
	pj, ok := s.data[ManifestPath]
	if !ok {
		return nil, invalid("no %s", ManifestPath)
	}
	var doc struct {
		Name        string `json:"name"`
		Version     string `json:"version"`
		Description string `json:"description"`
		UserConfig  map[string]struct {
			Type        string `json:"type"`
			Title       string `json:"title"`
			Description string `json:"description"`
			Sensitive   bool   `json:"sensitive"`
			Required    bool   `json:"required"`
			Default     any    `json:"default"`
		} `json:"userConfig"`
		Commands   json.RawMessage `json:"commands"`
		Agents     json.RawMessage `json:"agents"`
		Skills     json.RawMessage `json:"skills"`
		Hooks      json.RawMessage `json:"hooks"`
		MCPServers json.RawMessage `json:"mcpServers"`
		LSPServers json.RawMessage `json:"lspServers"`
	}
	if err := json.Unmarshal(pj, &doc); err != nil {
		return nil, invalid("%s: %v", ManifestPath, err)
	}
	if err := CheckName(doc.Name); err != nil {
		return nil, err
	}
	m := &Manifest{Name: doc.Name, Version: doc.Version, Description: doc.Description}
	for k, o := range doc.UserConfig {
		m.Options = append(m.Options, Option{Key: k, Title: o.Title, Description: o.Description, Type: o.Type,
			Sensitive: o.Sensitive, Required: o.Required, Default: o.Default})
	}
	sort.Slice(m.Options, func(i, j int) bool { return m.Options[i].Key < m.Options[j].Key })

	for _, d := range append([]string{"skills"}, paths(doc.Skills)...) {
		if _, ok := s.data[d+"/SKILL.md"]; ok {
			m.Skills = append(m.Skills, path.Base(d))
			continue
		}
		for p := range s.data {
			if rest, ok := strings.CutPrefix(p, d+"/"); ok {
				if name, ok := strings.CutSuffix(rest, "/SKILL.md"); ok && !strings.Contains(name, "/") {
					m.Skills = append(m.Skills, name)
				}
			}
		}
	}
	m.Commands = s.markdown(append([]string{"commands"}, paths(doc.Commands)...), ":")
	m.Agents = s.markdown(append([]string{"agents"}, paths(doc.Agents)...), "")
	for p := range s.data {
		if b, ok := strings.CutPrefix(p, "bin/"); ok && !strings.Contains(b, "/") {
			m.Bin = append(m.Bin, b)
		}
	}

	for _, cfg := range s.configs("hooks/hooks.json", "hooks", doc.Hooks, &m.Problems) {
		m.Hooks = append(m.Hooks, hooks(cfg, &m.Problems)...)
	}
	m.MCPServers = servers(s.configs(".mcp.json", "mcpServers", doc.MCPServers, &m.Problems), "mcpServers", &m.Problems)
	m.LSPServers = servers(s.configs(".lsp.json", "lspServers", doc.LSPServers, &m.Problems), "lspServers", &m.Problems)
	for _, sv := range m.MCPServers {
		if sv.URL == "" {
			continue
		}
		u, err := url.Parse(sv.URL)
		if err != nil || u.Hostname() == "" || strings.ContainsAny(u.Host, "${}") {
			m.Problems = append(m.Problems, fmt.Sprintf("MCP server %s: unresolved domain in %s", sv.Name, sv.URL))
			continue
		}
		m.Domains = append(m.Domains, strings.ToLower(u.Hostname()))
	}
	for _, l := range []*[]string{&m.Skills, &m.Commands, &m.Agents, &m.Bin, &m.Domains} {
		slices.Sort(*l)
		*l = slices.Compact(*l)
	}
	return m, nil
}

// paths reads a plugin.json path field, a string or a list, as clean paths inside the plugin.
func paths(raw json.RawMessage) []string {
	var list []string
	var one string
	if json.Unmarshal(raw, &one) == nil {
		list = []string{one}
	} else if json.Unmarshal(raw, &list) != nil {
		return nil
	}
	out := list[:0]
	for _, p := range list {
		if c, err := rootPrefix(p); err == nil && c != "" {
			out = append(out, strings.TrimSuffix(c, "/"))
		}
	}
	return out
}

// markdown names the .md files at each path, a file or a folder; nested folders join with sep,
// or only the file's base name counts when sep is empty.
func (s *Snapshot) markdown(dirs []string, sep string) []string {
	var out []string
	for _, d := range dirs {
		if name, ok := strings.CutSuffix(d, ".md"); ok {
			if _, ok := s.data[d]; ok {
				out = append(out, path.Base(name))
			}
			continue
		}
		for p := range s.data {
			rest, ok := strings.CutPrefix(p, d+"/")
			name, md := strings.CutSuffix(rest, ".md")
			if !ok || !md {
				continue
			}
			if sep == "" {
				name = path.Base(name)
			}
			out = append(out, strings.ReplaceAll(name, "/", sep))
		}
	}
	return out
}

// config is one JSON object a component comes from, and where it was read.
type config struct {
	src  string
	data json.RawMessage
}

// configs returns a component's configs: its default file, then plugin.json's field, which
// names files or holds the object inline.
func (s *Snapshot) configs(def, field string, raw json.RawMessage, problems *[]string) []config {
	var out []config
	if b, ok := s.data[def]; ok {
		out = append(out, config{def, b})
	}
	src := ManifestPath + " " + field
	switch {
	case len(raw) == 0 || string(raw) == "null":
	case raw[0] == '{':
		out = append(out, config{src, raw})
	default:
		ps := paths(raw)
		if ps == nil {
			*problems = append(*problems, src+": not a path, a list of paths or an object")
		}
		for _, p := range ps {
			if b, ok := s.data[p]; !ok {
				*problems = append(*problems, fmt.Sprintf("%s: %s is not in the plugin", src, p))
			} else if p != def {
				out = append(out, config{p, b})
			}
		}
	}
	return out
}

// jsonObject decodes a config as a JSON object, unwrapping key when the object holds it.
func jsonObject(cfg config, key string, problems *[]string) map[string]json.RawMessage {
	var all map[string]json.RawMessage
	if json.Unmarshal(cfg.data, &all) != nil {
		*problems = append(*problems, cfg.src+": not a JSON object")
		return nil
	}
	if inner, ok := all[key]; ok {
		all = nil
		if json.Unmarshal(inner, &all) != nil {
			*problems = append(*problems, fmt.Sprintf("%s: %q is not a JSON object", cfg.src, key))
			return nil
		}
	}
	return all
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// hooks lists a hooks config's commands. What it cannot read becomes a problem, never silence.
func hooks(cfg config, problems *[]string) []Hook {
	events := jsonObject(cfg, "hooks", problems)
	var out []Hook
	for _, ev := range sortedKeys(events) {
		var groups []struct {
			Hooks []map[string]json.RawMessage `json:"hooks"`
		}
		if json.Unmarshal(events[ev], &groups) != nil {
			*problems = append(*problems, fmt.Sprintf("%s: the %s hooks cannot be read: %s", cfg.src, ev, truncate(events[ev])))
			continue
		}
		for _, g := range groups {
			for _, h := range g.Hooks {
				var typ, cmd string
				str(h, "type", &typ)
				switch {
				case str(h, "command", &cmd):
				case str(h, "url", &cmd), str(h, "prompt", &cmd):
					cmd = typ + ": " + cmd
				default:
					raw, _ := json.Marshal(h)
					cmd = string(raw)
					*problems = append(*problems, fmt.Sprintf("%s: a %s hook has no readable command", cfg.src, ev))
				}
				out = append(out, Hook{Event: ev, Command: cmd})
			}
		}
	}
	return out
}

// servers lists the MCP or LSP servers of configs; a config may wrap them in key or not. A
// server with odd fields is still listed, with what it holds as its command.
func servers(cfgs []config, key string, problems *[]string) []Server {
	var out []Server
	seen := map[string]string{}
	for _, cfg := range cfgs {
		all := jsonObject(cfg, key, problems)
		for _, name := range sortedKeys(all) {
			if first, dup := seen[name]; dup {
				*problems = append(*problems, fmt.Sprintf("server %s is defined in %s and in %s", name, first, cfg.src))
			}
			seen[name] = cfg.src
			out = append(out, server(cfg.src, name, all[name], problems))
		}
	}
	return out
}

func server(src, name string, raw json.RawMessage, problems *[]string) Server {
	sv := Server{Name: name}
	var f map[string]json.RawMessage
	if json.Unmarshal(raw, &f) != nil {
		*problems = append(*problems, fmt.Sprintf("%s: server %s is not a JSON object", src, name))
		sv.Command = truncate(raw)
		return sv
	}
	odd := func(field string) {
		*problems = append(*problems, fmt.Sprintf("%s: server %s has an unreadable %q", src, name, field))
	}
	var parts []string
	if v, ok := f["command"]; ok {
		var c string
		if json.Unmarshal(v, &c) != nil {
			odd("command")
			c = string(v)
		}
		parts = append(parts, c)
	}
	if v, ok := f["args"]; ok {
		var args []string
		if json.Unmarshal(v, &args) != nil {
			odd("args")
			args = []string{string(v)}
		}
		parts = append(parts, args...)
	}
	sv.Command = strings.Join(parts, " ")
	if v, ok := f["url"]; ok && !str(f, "url", &sv.URL) {
		odd("url")
		sv.URL = string(v)
	}
	return sv
}
