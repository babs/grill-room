package web

import (
	"os"
	"regexp"
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
