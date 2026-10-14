package plugin

import (
	"net/url"
	"slices"
	"strings"
)

// Change is what an update changes against the approved version (009-AC15). Components read
// "skill:<name>", "command:<name>", "agent:<name>", "hook:<event>: <command>", "mcp:<name>",
// "lsp:<name>", "bin:<name>".
type Change struct {
	ComponentsAdded   []string `json:"components_added,omitempty"`
	ComponentsRemoved []string `json:"components_removed,omitempty"`
	FilesAdded        []string `json:"files_added,omitempty"`
	FilesRemoved      []string `json:"files_removed,omitempty"`
	FilesChanged      []string `json:"files_changed,omitempty"`
}

// Diff compares two versions by their file lists and manifests; a nil manifest has no
// components.
func Diff(oldFiles, newFiles []File, oldM, newM *Manifest) Change {
	var c Change
	c.ComponentsAdded, c.ComponentsRemoved = setDiff(components(oldM), components(newM))
	was := make(map[string]string, len(oldFiles))
	for _, f := range oldFiles {
		was[f.Path] = f.SHA256
	}
	for _, f := range newFiles {
		sum, ok := was[f.Path]
		switch {
		case !ok:
			c.FilesAdded = append(c.FilesAdded, f.Path)
		case sum != f.SHA256:
			c.FilesChanged = append(c.FilesChanged, f.Path)
		}
		delete(was, f.Path)
	}
	for p := range was {
		c.FilesRemoved = append(c.FilesRemoved, p)
	}
	for _, l := range [][]string{c.FilesAdded, c.FilesRemoved, c.FilesChanged} {
		slices.Sort(l)
	}
	return c
}

func components(m *Manifest) []string {
	if m == nil {
		return nil
	}
	var out []string
	add := func(kind string, names []string) {
		for _, n := range names {
			out = append(out, kind+":"+n)
		}
	}
	add("skill", m.Skills)
	add("command", m.Commands)
	add("agent", m.Agents)
	add("bin", m.Bin)
	for _, h := range m.Hooks {
		out = append(out, "hook:"+h.Event+": "+h.Command)
	}
	for _, s := range m.MCPServers {
		out = append(out, "mcp:"+s.Name)
	}
	for _, s := range m.LSPServers {
		out = append(out, "lsp:"+s.Name)
	}
	return out
}

// setDiff returns what b has that a lacks, and the reverse, sorted.
func setDiff(a, b []string) (added, removed []string) {
	for _, x := range b {
		if !slices.Contains(a, x) {
			added = append(added, x)
		}
	}
	for _, x := range a {
		if !slices.Contains(b, x) {
			removed = append(removed, x)
		}
	}
	slices.Sort(added)
	slices.Sort(removed)
	return added, removed
}

// CompareURL links the forge's compare view between two commits, for github.com and GitLab
// hosts (gitlab.com or a host named after it); "" elsewhere, where the UI links both commits.
func CompareURL(repoURL, from, to string) string {
	u, err := url.Parse(repoURL)
	if err != nil || u.Scheme != "https" || u.User != nil || !hexRe.MatchString(from) || !hexRe.MatchString(to) {
		return ""
	}
	repo := strings.TrimSuffix(strings.TrimRight(u.EscapedPath(), "/"), ".git")
	if strings.Count(repo, "/") < 2 {
		return ""
	}
	host := strings.ToLower(u.Host)
	switch {
	case host == "github.com":
		return "https://github.com" + repo + "/compare/" + from + "..." + to
	case strings.Contains(host, "gitlab"):
		return "https://" + host + repo + "/-/compare/" + from + "..." + to
	}
	return ""
}
