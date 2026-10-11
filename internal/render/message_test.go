package render

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
	xhtml "golang.org/x/net/html"
)

// allowedAttrs is every element and attribute a rendered message may hold.
var allowedAttrs = map[string][]string{
	"p": nil, "br": nil, "em": nil, "strong": nil, "del": nil, "code": {"class"}, "pre": nil, "blockquote": nil, "hr": nil,
	"ul": nil, "ol": {"start"}, "li": nil, "h1": nil, "h2": nil, "h3": nil, "h4": nil, "h5": nil, "h6": nil,
	"table": nil, "thead": nil, "tbody": nil, "tr": nil, "th": {"align"}, "td": {"align"},
	"a": {"href", "title", "rel", "target"}, "input": {"type", "checked", "disabled"},
}

// violations parses out as a browser would and lists what a safe message must not hold.
func violations(t *testing.T, out string) []string {
	t.Helper()
	doc, err := xhtml.Parse(strings.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	var bad []string
	var walk func(n *xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode && n.Data != "html" && n.Data != "head" && n.Data != "body" {
			allowed, ok := allowedAttrs[n.Data]
			if !ok {
				bad = append(bad, "tag "+n.Data)
			}
			attrs := map[string]string{}
			for _, a := range n.Attr {
				attrs[a.Key] = a.Val
				if ok && !strings.Contains(","+strings.Join(allowed, ",")+",", ","+a.Key+",") {
					bad = append(bad, "attr "+a.Key)
				}
			}
			switch n.Data {
			case "a":
				if !safeURL([]byte(attrs["href"])) {
					bad = append(bad, "href "+attrs["href"])
				}
				if attrs["rel"] != "noopener noreferrer" || attrs["target"] != "_blank" {
					bad = append(bad, "rel or target missing")
				}
			case "input":
				if attrs["type"] != "checkbox" {
					bad = append(bad, "input type "+attrs["type"])
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return bad
}

// unsanitised is the message renderer with its sanitising removed: raw HTML on, no link guard.
func unsanitised(src string) string {
	md := goldmark.New(
		goldmark.WithExtensions(extension.Linkify, extension.Table, extension.Strikethrough, extension.TaskList),
		goldmark.WithRendererOptions(html.WithUnsafe()),
	)
	var buf bytes.Buffer
	_ = md.Convert([]byte(src), &buf)
	return buf.String()
}

// 002-AC8: hostile messages render inert. Each case names what the unsanitised renderer lets
// through, which proves the check would catch that input if sanitising were removed.
func TestMessageHTMLXSS(t *testing.T) {
	cases := []struct{ name, src, unsafe string }{
		{"script tag", `<script>alert(1)</script>`, "tag script"},
		{"img onerror", `<img src=x onerror=alert(1)>`, "tag img"},
		{"raw anchor handler", `<a href="https://ok.example" onclick="alert(1)">x</a>`, "attr onclick"},
		{"raw anchor javascript", `<a href="javascript:alert(1)">x</a>`, "href javascript:"},
		{"javascript link", `[x](javascript:alert(1))`, "href javascript:"},
		{"mixed case scheme", `[x](JaVaScRiPt:alert(1))`, "href javascript:"},
		{"entity in scheme", `[x](java&#x73;cript:alert(1))`, "href javascript:"},
		{"angle destination", `[x](<javascript:alert(1)>)`, "href javascript:"},
		{"javascript autolink", `<javascript:alert(1)>`, "href javascript:"},
		{"vbscript link", `[x](vbscript:msgbox(1))`, "href vbscript:"},
		{"file link", `[x](file:///etc/passwd)`, "href file:"},
		{"data html link", `[x](data:text/html;base64,PHNjcmlwdD4=)`, "href data:"},
		// goldmark's own safe mode lets data:image links through: only the allowlist stops it.
		{"data image link", `[x](data:image/png;base64,iVBORw0KGgo=)`, "href data:"},
		{"protocol relative", `[x](//evil.example/a)`, "href //evil.example"},
		{"remote image", `![x](https://evil.example/pixel.png)`, "tag img"},
		{"svg data image", `![x](data:image/svg+xml;base64,PHN2Zz4=)`, "tag img"},
		{"iframe block", `<iframe src="https://evil.example"></iframe>`, "tag iframe"},
		{"svg onload", `<svg onload=alert(1)>`, "tag svg"},
		{"style block", `<style>body{display:none}</style>`, "tag style"},
		{"meta refresh", `<meta http-equiv="refresh" content="0;url=https://evil.example">`, "tag meta"},
		{"base href", `<base href="https://evil.example/">`, "tag base"},
		{"details ontoggle", `<details open ontoggle=alert(1)>x</details>`, "tag details"},
		{"form", `<form action="https://evil.example"><button>go</button></form>`, "tag form"},
		{"script after linkify", `https://ok.example/?q=<script>alert(1)</script>`, "tag script"},
		{"link without new tab", `[ok](https://ok.example)`, "rel or target missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if bad := violations(t, MessageHTML(tc.src)); len(bad) > 0 {
				t.Errorf("MessageHTML(%q) = %q: %v", tc.src, MessageHTML(tc.src), bad)
			}
			bad := violations(t, unsanitised(tc.src))
			if !strings.Contains(strings.ToLower(strings.Join(bad, "\n")), tc.unsafe) {
				t.Errorf("the check misses %q unsanitised: got %v, want %q", tc.src, bad, tc.unsafe)
			}
		})
	}
}

// 002-AC8
func TestMessageHTMLMarkdown(t *testing.T) {
	src := "*em* **strong** ~~del~~ `code`\nnext line\n\n- one\n- two\n\n1. first\n\n```\nblock <b>\n```\n\n" +
		"[site](https://ok.example/a?b=1) https://auto.example <me@ok.example>\n\n| a | b |\n|:--|--:|\n| 1 | 2 |\n\n- [x] done\n\n<b>raw</b>\n"
	got := MessageHTML(src)
	for _, want := range []string{
		"<em>em</em>", "<strong>strong</strong>", "<del>del</del>", "<code>code</code>", "<br>\nnext line",
		"<li>one</li>", "<ol>\n<li>first</li>", "<pre><code>block &lt;b&gt;\n</code></pre>",
		`<a href="https://ok.example/a?b=1" rel="noopener noreferrer" target="_blank">site</a>`,
		`<a href="https://auto.example" rel="noopener noreferrer" target="_blank">https://auto.example</a>`,
		`<a href="mailto:me@ok.example" rel="noopener noreferrer" target="_blank">me@ok.example</a>`,
		`<th align="left">a</th>`, `<td align="right">2</td>`, `<input checked="" disabled="" type="checkbox"`,
		"<p>&lt;b&gt;raw&lt;/b&gt;</p>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if bad := violations(t, got); len(bad) > 0 {
		t.Errorf("violations: %v", bad)
	}
	// Unsafe links and images keep their text; an image inside a link loses its own URL.
	for src, want := range map[string]string{
		"[click](javascript:alert(1))":                           "<p>click</p>\n",
		"![alt](https://ok.example/i.png)":                       `<p><a href="https://ok.example/i.png" rel="noopener noreferrer" target="_blank">alt</a></p>` + "\n",
		"[![alt](https://ok.example/i.png)](https://ok.example)": `<p><a href="https://ok.example" rel="noopener noreferrer" target="_blank">alt</a></p>` + "\n",
		"[![alt](https://ok.example/i.png)](javascript:x)":       `<p><a href="https://ok.example/i.png" rel="noopener noreferrer" target="_blank">alt</a></p>` + "\n",
		"<ftp://ok.example/f>":                                   "<p>ftp://ok.example/f</p>\n",
		"[m](mailto:)":                                           "<p>m</p>\n",
	} {
		if got := MessageHTML(src); got != want {
			t.Errorf("MessageHTML(%q) = %q, want %q", src, got, want)
		}
	}
}

// One shared converter serves every request: -race checks it holds no per-call state.
func TestMessageHTMLConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for range 50 {
				if got := MessageHTML("*x* [l](https://ok.example) | a |\n|--|\n| " + strings.Repeat("y", i) + " |"); !strings.Contains(got, "<em>x</em>") {
					t.Errorf("got %q", got)
				}
			}
		})
	}
	wg.Wait()
}
