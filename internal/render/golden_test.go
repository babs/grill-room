package render

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files from the current output")

func golden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/render -update)", err)
	}
	if string(got) != string(want) {
		t.Errorf("%s differs from the output; review with -update and git diff\ngot:\n%s", path, got)
	}
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 001-AC11: headings, tables, code, mermaid, math, alerts, task lists, footnotes, front matter.
func TestRenderGolden(t *testing.T) {
	paths, err := filepath.Glob("testdata/render/*.md")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no inputs: %v", err)
	}
	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			out, err := MdToHTML(read(t, p))
			if err != nil {
				t.Fatal(err)
			}
			golden(t, strings.TrimSuffix(p, ".md")+".golden.html", out)
		})
	}
	// The golden file is only as good as what it pins: check the AC11 features are in it.
	out, _ := MdToHTML(read(t, "testdata/render/github.md"))
	for _, want := range []string{
		`<h1 id="rollout-plan">`, `<table>`, `<th style="text-align:center">Owner</th>`,
		`<span class="nx">fmt</span>`, `<pre class="mermaid">`, `class="math inline"`, `class="math display"`,
		`class="markdown-alert markdown-alert-warning"`, `type="checkbox"`, `data-footnote-ref`,
		`class="frontmatter-table"`, `<details>`, `&#x1f389;`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("render lacks %s", want)
		}
	}
}

// 001-AC17: a word-level diff between two revisions, Markdown rendered then compared, and raw HTML.
func TestDiffGolden(t *testing.T) {
	t.Run("markdown", func(t *testing.T) {
		oldHTML, err := MdToHTML(read(t, "testdata/diff/plan.old.md"))
		if err != nil {
			t.Fatal(err)
		}
		newHTML, err := MdToHTML(read(t, "testdata/diff/plan.new.md"))
		if err != nil {
			t.Fatal(err)
		}
		golden(t, "testdata/diff/plan.golden.html", DiffHTML(oldHTML, newHTML))
	})
	t.Run("html", func(t *testing.T) {
		golden(t, "testdata/diff/page.golden.html", DiffHTML(read(t, "testdata/diff/page.old.html"), read(t, "testdata/diff/page.new.html")))
	})
}

func TestCodeCSS(t *testing.T) {
	light, dark := CodeCSS("github"), CodeCSS("github-dark")
	if !strings.Contains(light, ".chroma") || light == dark {
		t.Fatalf("code css: light %d bytes, dark %d bytes", len(light), len(dark))
	}
}
