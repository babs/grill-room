package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every GRILL_ROOM_* variable the server reads has a row in the README's variable table (## Run): a
// missing one is a setting operators cannot find (019-AC11, 021-AC14). The test harness's own
// GRILL_ROOM_TEST_* are not settings.
func TestEveryEnvVarIsInTheReadme(t *testing.T) {
	doc, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	// The server's table only: the host daemon's has rows of the same names.
	_, run, _ := strings.Cut(string(doc), "\n## Run\n")
	run, _, _ = strings.Cut(run, "\n## ")
	if run == "" {
		t.Fatal("no ## Run section in the README")
	}
	readme := []byte(run)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	name := regexp.MustCompile(`"(GRILL_ROOM_[A-Z0-9_]+)"`)
	seen := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range name.FindAllStringSubmatch(string(src), -1) {
			if strings.HasPrefix(m[1], "GRILL_ROOM_TEST_") {
				continue
			}
			seen++
			// The first cell of a table row, alone or among the names it groups: a mention in prose is not a row.
			row := regexp.MustCompile("(?m)^\\| (`[A-Z0-9_]+`, )*`" + regexp.QuoteMeta(m[1]) + "`(, `[A-Z0-9_]+`)* \\|")
			if !row.Match(readme) {
				t.Errorf("%s (%s) has no row in the README", m[1], f)
			}
		}
	}
	if seen < 30 {
		t.Fatalf("found %d variables: the pattern no longer matches the sources", seen)
	}
}
