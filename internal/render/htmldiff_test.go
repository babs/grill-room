// Derived from github.com/babs/grip-live-diff internal/htmldiff_test.go at commit 36209e6.

package render

import (
	"strings"
	"testing"
)

func TestDiffHTML(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		old         string
		new         string
		want        string   // exact output, when it is worth pinning
		wantContain []string // otherwise substrings that must appear
		wantAbsent  []string
	}{
		{
			name: "unchanged document is returned untouched",
			old:  "<h1>Title</h1>\n<p>Some text here.</p>\n",
			new:  "<h1>Title</h1>\n<p>Some text here.</p>\n",
			want: "<h1>Title</h1>\n<p>Some text here.</p>\n",
		},
		{
			name: "only the changed words of a line are annotated",
			old:  "<p>the quick brown fox jumps over the lazy dog</p>\n",
			new:  "<p>the quick red fox leaps over the lazy dog</p>\n",
			want: `<p>the quick <del class="gg-del">brown </del><ins class="gg-ins">red</ins> fox ` +
				`<del class="gg-del">jumps </del><ins class="gg-ins">leaps</ins> over the lazy dog</p>` + "\n",
		},
		{
			name: "re-wrapping a paragraph is not a change",
			old:  "<p>bonjour monsieur x, aze\nqsd aze</p>\n",
			new:  "<p>bonjour monsieur x, aze qsd aze</p>\n",
			want: "<p>bonjour monsieur x, aze qsd aze</p>\n",
		},
		{
			name: "wrapping a paragraph is not a change either",
			old:  "<p>the quick brown fox jumps over the lazy dog</p>\n",
			new:  "<p>the quick brown fox\njumps over\nthe lazy dog</p>\n",
			want: "<p>the quick brown fox\njumps over\nthe lazy dog</p>\n",
		},
		{
			name: "a real edit is still caught inside a re-wrapped paragraph",
			old:  "<p>the quick brown fox\njumps over the lazy dog</p>\n",
			new:  "<p>the quick red fox jumps over the lazy dog</p>\n",
			want: `<p>the quick <del class="gg-del">brown </del><ins class="gg-ins">red</ins> fox ` +
				"jumps over the lazy dog</p>\n",
		},
		{
			name:        "inside a code block the whitespace still counts",
			old:         "<pre><code>if x:\n    return 1\n</code></pre>\n",
			new:         "<pre><code>if x:\n        return 1\n</code></pre>\n",
			wantContain: []string{`class="gg-ins"`},
		},
		{
			name: "a tag whose name merely starts with pre does not turn whitespace exact",
			old:  "<preload>\n<p>bonjour monsieur x, aze\nqsd aze</p>\n",
			new:  "<preload>\n<p>bonjour monsieur x, aze qsd aze</p>\n",
			want: "<preload>\n<p>bonjour monsieur x, aze qsd aze</p>\n",
		},
		{
			name: "a self-closing pre opens nothing",
			old:  "<pre/>\n<p>bonjour monsieur x, aze\nqsd aze</p>\n",
			new:  "<pre/>\n<p>bonjour monsieur x, aze qsd aze</p>\n",
			want: "<pre/>\n<p>bonjour monsieur x, aze qsd aze</p>\n",
		},
		{
			name: "a spaced self-closing pre-prefixed tag opens nothing either",
			old:  "<prefetch />\n<p>bonjour monsieur x, aze\nqsd aze</p>\n",
			new:  "<prefetch />\n<p>bonjour monsieur x, aze qsd aze</p>\n",
			want: "<prefetch />\n<p>bonjour monsieur x, aze qsd aze</p>\n",
		},
		{
			name:        "an attributed pre still keeps its whitespace exact",
			old:         `<pre class="x"><code>if x:` + "\n    return 1\n</code></pre>\n",
			new:         `<pre class="x"><code>if x:` + "\n        return 1\n</code></pre>\n",
			wantContain: []string{`class="gg-ins"`},
		},
		{
			name:        "added paragraph is fully marked as inserted",
			old:         "<p>first</p>\n",
			new:         "<p>first</p>\n<p>second paragraph</p>\n",
			wantContain: []string{`<p>first</p>`, `<p><ins class="gg-ins">second paragraph</ins></p>`},
		},
		{
			name:        "removed paragraph survives as deleted text",
			old:         "<p>keep</p>\n<p>gone for good</p>\n",
			new:         "<p>keep</p>\n",
			wantContain: []string{`<p>keep</p>`, "<del class=\"gg-del\">gone for good\n</del>"},
			wantAbsent:  []string{"gg-ins"},
		},
		{
			name:        "intra-word change inside a heading",
			old:         "<h2>Installation</h2>\n",
			new:         "<h2>Configuration</h2>\n",
			wantContain: []string{`<h2><del class="gg-del">Installation</del><ins class="gg-ins">Configuration</ins></h2>`},
		},
		{
			name: "changed mermaid block is never annotated inside",
			old:  "<pre class=\"mermaid\">graph TD;\nA--&gt;B;\n</pre>\n",
			new:  "<pre class=\"mermaid\">graph TD;\nA--&gt;C;\n</pre>\n",
			wantContain: []string{
				"<pre class=\"mermaid\">graph TD;\nA--&gt;C;\n</pre>",
				"gg-ins",
			},
			wantAbsent: []string{"<pre class=\"mermaid\"><ins", "B;\n</pre>"},
		},
		{
			name: "math span is compared as a whole",
			old:  `<p><span class="math inline">\(a+b\)</span> is fine</p>` + "\n",
			new:  `<p><span class="math inline">\(a+c\)</span> is fine</p>` + "\n",
			wantContain: []string{
				`<ins class="gg-ins"><span class="math inline">\(a+c\)</span></ins>`,
				`<del class="gg-del">\(a+b\)</del>`,
				"is fine",
			},
		},
		{
			name:        "code block content is diffed at word level",
			old:         `<pre class="chroma"><code><span class="line">fmt.Println("hello")</span></code></pre>`,
			new:         `<pre class="chroma"><code><span class="line">fmt.Println("world")</span></code></pre>`,
			wantContain: []string{`<span class="line">`, `<ins class="gg-ins">fmt.Println("world")</ins>`},
		},
		{
			name:        "trailing whitespace stays outside the marker",
			old:         "<p>alpha beta</p>\n",
			new:         "<p>alpha gamma beta</p>\n",
			wantContain: []string{`<ins class="gg-ins">gamma</ins> beta`},
		},
		{
			name:        "removed image stays visible as a deletion",
			old:         `<p>before</p>` + "\n" + `<p><img src="x.png" alt="a diagram"></p>` + "\n" + `<p>after</p>` + "\n",
			new:         "<p>before</p>\n<p>after</p>\n",
			wantContain: []string{`<del class="gg-del"><img src="x.png" alt="a diagram">`},
		},
		{
			name:        "added image is marked as an insertion",
			old:         "<p>before</p>\n",
			new:         "<p>before</p>\n<p><img src=\"y.png\" alt=\"new pic\"></p>\n",
			wantContain: []string{`<ins class="gg-ins"><img src="y.png" alt="new pic"></ins>`},
		},
		{
			name:        "swapped image is both deleted and inserted",
			old:         `<p><img src="x.png" alt="old"></p>` + "\n",
			new:         `<p><img src="y.png" alt="new"></p>` + "\n",
			wantContain: []string{`<del class="gg-del"><img src="x.png" alt="old"></del>`, `<ins class="gg-ins"><img src="y.png" alt="new"></ins>`},
		},
		{
			name:        "empty baseline marks the whole document as new",
			old:         "",
			new:         "<p>brand new</p>\n",
			wantContain: []string{`<p><ins class="gg-ins">brand new</ins></p>`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := string(DiffHTML([]byte(tt.old), []byte(tt.new)))

			if tt.want != "" && got != tt.want {
				t.Fatalf("unexpected output\n got: %q\nwant: %q", got, tt.want)
			}
			for _, want := range tt.wantContain {
				if !strings.Contains(got, want) {
					t.Fatalf("expected output to contain %q, got %q", want, got)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(got, absent) {
					t.Fatalf("expected output not to contain %q, got %q", absent, got)
				}
			}
		})
	}
}

// The new document's own markup must survive the diff untouched: stripping the
// annotations has to give back the new HTML exactly.
func TestDiffHTMLPreservesNewDocument(t *testing.T) {
	t.Parallel()

	old := "<h1>Title</h1>\n<p>one two three</p>\n<ul>\n<li>a</li>\n<li>b</li>\n</ul>\n"
	next := "<h1>Title</h1>\n<p>one four three</p>\n<ul>\n<li>a</li>\n</ul>\n<p>tail</p>\n"

	got := string(DiffHTML([]byte(old), []byte(next)))
	got = removeElement(got, "del")
	got = strings.ReplaceAll(got, `<ins class="gg-ins">`, "")
	got = strings.ReplaceAll(got, "</ins>", "")

	if got != next {
		t.Fatalf("stripped diff does not match the new document\n got: %q\nwant: %q", got, next)
	}
}

func removeElement(s, tag string) string {
	open, closing := "<"+tag+" ", "</"+tag+">"
	for {
		start := strings.Index(s, open)
		if start < 0 {
			return s
		}
		end := strings.Index(s[start:], closing)
		if end < 0 {
			return s
		}
		s = s[:start] + s[start+end+len(closing):]
	}
}
