package plugin

import (
	"regexp"
	"strconv"
	"strings"
)

// Component is one line of `claude plugin details`' inventory: "Skills (2)  hello, run".
type Component struct {
	Kind  string   `json:"kind"`
	Count int      `json:"count"`
	Names []string `json:"names,omitempty"`
}

// Details is what the daemon reads from `claude --plugin-dir <p> plugin details <name>`. The
// command has no --json in Claude Code 2.1.296, so this parses its text; CostKnown is false when
// the token line is missing or reads differently, and the room then shows "cost unknown".
type Details struct {
	Name           string      `json:"name,omitempty"`
	Version        string      `json:"version,omitempty"`
	Components     []Component `json:"components,omitempty"`
	AlwaysOnTokens int         `json:"always_on_tokens"`
	CostKnown      bool        `json:"cost_known"`
}

var (
	inventoryRe = regexp.MustCompile(`^\s+(\S.*?) \((\d+)\)(?:\s+(.*))?$`)
	alwaysOnRe  = regexp.MustCompile(`^\s*Always-on:\s+[~<]?\s*([\d][\d,.]*)(k?)\s+tok\b`)
	// noteRe is the remark some inventory lines end with: "  (harness-only — ...)".
	noteRe = regexp.MustCompile(`\s{2,}\(.*\)$`)
)

// ParseDetails reads `claude plugin details` output (golden: Claude Code 2.1.296). It never
// fails: what it cannot read stays empty.
func ParseDetails(out string) Details {
	var d Details
	section := ""
	for i, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		if i == 0 {
			if f := strings.Fields(line); len(f) == 2 {
				d.Name, d.Version = f[0], f[1]
			}
		}
		if line != "" && line[0] != ' ' {
			section = strings.TrimSpace(line)
			continue
		}
		if section == "Component inventory" {
			if m := inventoryRe.FindStringSubmatch(line); m != nil {
				n, _ := strconv.Atoi(m[2])
				c := Component{Kind: m[1], Count: n}
				if names := strings.TrimSpace(noteRe.ReplaceAllString(m[3], "")); names != "" {
					c.Names = strings.Split(names, ", ")
				}
				d.Components = append(d.Components, c)
			}
		}
		if m := alwaysOnRe.FindStringSubmatch(line); m != nil && section == "Projected token cost" {
			v, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", ""), 64)
			if err == nil {
				if m[2] == "k" {
					v *= 1000
				}
				d.AlwaysOnTokens, d.CostKnown = int(v+0.5), true
			}
		}
	}
	return d
}
