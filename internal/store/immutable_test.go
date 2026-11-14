package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The workspace manifest memoises blob hashes by document and revision (web.blobSums), and the
// daemon cache is content-addressed: a rewritten revision body or upload would be served stale.
var blobRewrite = regexp.MustCompile(`(?i)\bUPDATE\s+(revisions|uploads)\b`)

func TestStoredBlobsAreNeverRewritten(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if loc := blobRewrite.FindIndex(src); loc != nil {
			t.Errorf("%s: %q rewrites a stored blob; give it a new revision instead", f, src[loc[0]:loc[1]])
		}
	}
}

func TestBlobRewritePattern(t *testing.T) {
	for _, s := range []string{"UPDATE revisions SET body = ?", "update uploads set data=$1", "`UPDATE\n\t\trevisions SET`"} {
		if !blobRewrite.MatchString(s) {
			t.Errorf("not caught: %q", s)
		}
	}
	for _, s := range []string{"UPDATE revisions_seen SET", "UPDATE documents SET name = ?", "SELECT * FROM uploads"} {
		if blobRewrite.MatchString(s) {
			t.Errorf("false alarm: %q", s)
		}
	}
}
