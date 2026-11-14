package web

import (
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Every API route the server registers has a row in the README's API table (021-AC14, 026-AC6): a
// route missing there is an undocumented endpoint. Path parameters compare by position, as the table
// names them freely.
func TestEveryRouteIsInTheReadme(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	param := regexp.MustCompile(`\{[^}]*\}`)
	norm := func(s string) string { return param.ReplaceAllString(s, "{}") }
	doc := norm(string(readme))
	routes := regexp.MustCompile(`mux\.Handle(?:Func)?\("(?:GET|POST|PUT|PATCH|DELETE) (/api/[^"]+)"`).FindAllStringSubmatch(string(src), -1)
	if len(routes) < 50 {
		t.Fatalf("found %d routes: the pattern no longer matches server.go", len(routes))
	}
	documented := func(p string) bool {
		return strings.Contains(doc, "`"+p+"`") || strings.Contains(doc, "`"+p+",") || strings.Contains(doc, " "+p+"`")
	}
	for _, r := range routes {
		p := norm(r[1])
		// One README row covers the /api/admin copies of the room routes, where the page's cookie reaches.
		alias, isAlias := strings.CutPrefix(p, "/api/admin/")
		if !documented(p) && (!isAlias || !documented("/api/"+alias)) {
			t.Errorf("%s has no row in the README", r[1])
		}
	}
}

// The README's MCP paragraph counts and names the catalogue: its tool count, the read tools and the
// full-token ones in their own groups, and its destructive tool count (023-AC22).
func TestReadmeListsTheMCPTools(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	_, after, _ := strings.Cut(string(b), "### Agent access (MCP)")
	sec, _, _ := strings.Cut(after, "\n## ")
	// Names count only in their group of the list sentence: other sentences mention some again.
	_, rest, _ := strings.Cut(sec, "read your rooms (")
	read, rest, _ := strings.Cut(rest, ") and set them up (")
	full, _, _ := strings.Cut(rest, ").")
	name := regexp.MustCompile("`([a-z_]+)`")
	names := func(s string) []string {
		var out []string
		for _, m := range name.FindAllStringSubmatch(s, -1) {
			out = append(out, m[1])
		}
		slices.Sort(out)
		return out
	}
	var wantRead, wantFull []string
	destr := 0
	for _, tool := range mcpTools {
		if tool.destr {
			destr++
		}
		if tool.full {
			wantFull = append(wantFull, tool.name)
		} else {
			wantRead = append(wantRead, tool.name)
		}
	}
	slices.Sort(wantRead)
	slices.Sort(wantFull)
	if got := names(read); !slices.Equal(got, wantRead) {
		t.Errorf("README read tools %v, catalogue %v", got, wantRead)
	}
	if got := names(full); !slices.Equal(got, wantFull) {
		t.Errorf("README full-token tools %v, catalogue %v", got, wantFull)
	}
	if m := regexp.MustCompile(`Its (\d+) tools`).FindStringSubmatch(sec); m == nil || m[1] != strconv.Itoa(len(mcpTools)) {
		t.Errorf("README tool count %v, catalogue %d", m, len(mcpTools))
	}
	words := map[string]int{"four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8}
	if m := regexp.MustCompile(`The (\w+) destructive tools`).FindStringSubmatch(sec); m == nil || words[m[1]] != destr {
		t.Errorf("README destructive count %v, catalogue %d", m, destr)
	}
}
