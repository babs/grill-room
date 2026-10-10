// Derived from github.com/babs/grip-live-diff internal/parser.go (CodeCSS: internal/server.go) at
// commit 36209e6.

// Package render turns Markdown into GitHub-style HTML and diffs two HTML renders word by word.
package render

import (
	"bytes"
	"strings"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/styles"

	"github.com/yuin/goldmark"
	emoji "github.com/yuin/goldmark-emoji"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
	"go.abhg.dev/goldmark/mermaid"

	"github.com/babs/grill-room/internal/render/alert"
	"github.com/babs/grill-room/internal/render/footnote"
	"github.com/babs/grill-room/internal/render/frontmatter"
	"github.com/babs/grill-room/internal/render/highlighting"
	"github.com/babs/grill-room/internal/render/mathjax"
	"github.com/babs/grill-room/internal/render/tasklist"
)

// newMarkdown builds a converter per call, as upstream does: extensions are not audited for
// concurrent use. Upstream also loads ghissue (links need a local git remote), details (inline
// session-storage script) and hashtag (not GitHub syntax): none applies to a room document.
func newMarkdown() goldmark.Markdown {
	return goldmark.New(
		goldmark.WithExtensions(
			extension.Linkify,
			extension.Table,
			extension.Strikethrough,
			footnote.Footnote,
			tasklist.TaskList,
			emoji.Emoji,
			alert.New(),
			highlighting.Highlighting,
			&mermaid.Extender{RenderMode: mermaid.RenderModeClient, NoScript: true},
			mathjax.MathJax,
		),
		goldmark.WithParserOptions(
			parser.WithAutoHeadingID(),
		),
		goldmark.WithRendererOptions(
			// Raw HTML passes through as on GitHub: documents are only ever shown in a sandboxed iframe.
			html.WithUnsafe(),
		),
	)
}

// MdToHTML renders Markdown, with YAML front matter shown as a table above the body.
func MdToHTML(input []byte) ([]byte, error) {
	var prefix []byte
	if fm, body, ok := frontmatter.Extract(input); ok {
		if table, err := frontmatter.RenderTable(fm); err == nil {
			prefix = table
			input = body
		}
	}
	var buf bytes.Buffer
	if err := newMarkdown().Convert(input, &buf); err != nil {
		return nil, err
	}
	return append(prefix, buf.Bytes()...), nil
}

// CodeCSS returns the stylesheet for highlighted code blocks in a chroma style ("github",
// "github-dark").
func CodeCSS(style string) string {
	var b strings.Builder
	_ = chromahtml.New(chromahtml.WithClasses(true)).WriteCSS(&b, styles.Get(style))
	return b.String()
}
