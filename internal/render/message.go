package render

import (
	"bytes"
	"net/url"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// messageMD renders chat messages. Unlike documents, messages land in the room page itself, not in
// a sandboxed iframe: no raw HTML, no images, no heading ids (they could clobber the page's own).
var messageMD = goldmark.New(
	goldmark.WithExtensions(
		extension.Linkify,
		// align attributes, not style: the page's CSP drops inline styles.
		extension.NewTable(extension.WithTableCellAlignMethod(extension.TableCellAlignAttribute)),
		extension.Strikethrough,
		extension.TaskList,
	),
	goldmark.WithParserOptions(parser.WithASTTransformers(util.Prioritized(linkGuard{}, 0))),
	goldmark.WithRendererOptions(
		// A chat line break is meant: Shift+Enter in the box.
		html.WithHardWraps(),
		renderer.WithNodeRenderers(util.Prioritized(rawAsText{}, 0)),
	),
)

// MessageHTML renders a chat message's Markdown as HTML safe to insert in the room page: raw HTML
// shows as text, only http, https and mailto URLs become links, every link opens in a new tab
// without an opener, and images show as links to their URL.
func MessageHTML(src string) string {
	var buf bytes.Buffer
	if err := messageMD.Convert([]byte(src), &buf); err != nil {
		// Convert fails only on a writer error, which a bytes.Buffer never returns.
		return ""
	}
	return buf.String()
}

// safeURL is an allowlist: a denylist misses the next scheme a browser runs.
func safeURL(dest []byte) bool {
	u, err := url.Parse(string(dest))
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "http", "https":
		return u.Host != ""
	case "mailto":
		return u.Opaque != ""
	}
	return false
}

type linkGuard struct{}

// Transform applies the URL allowlist and the new-tab attributes. Unsafe links keep their text.
func (linkGuard) Transform(doc *ast.Document, reader text.Reader, _ parser.Context) {
	src := reader.Source()
	var found []ast.Node
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if k := n.Kind(); entering && (k == ast.KindLink || k == ast.KindAutoLink || k == ast.KindImage) {
			found = append(found, n)
		}
		return ast.WalkContinue, nil
	})
	for _, n := range found {
		switch n := n.(type) {
		case *ast.Link:
			guard(n, n.Destination)
		case *ast.Image:
			// An image would load a third-party URL in every participant's browser.
			if insideLink(n) {
				unwrap(n)
				continue
			}
			l := ast.NewLink()
			l.Destination, l.Title = n.Destination, n.Title
			for c := n.FirstChild(); c != nil; {
				next := c.NextSibling()
				l.AppendChild(l, c)
				c = next
			}
			n.Parent().ReplaceChild(n.Parent(), n, l)
			guard(l, l.Destination)
		case *ast.AutoLink:
			u := n.URL(src)
			if n.AutoLinkType == ast.AutoLinkEmail && !bytes.HasPrefix(bytes.ToLower(u), []byte("mailto:")) {
				u = append([]byte("mailto:"), u...)
			}
			if safeURL(u) {
				newTab(n)
			} else {
				n.Parent().ReplaceChild(n.Parent(), n, ast.NewString(n.Label(src)))
			}
		}
	}
}

func guard(n ast.Node, dest []byte) {
	if safeURL(dest) {
		newTab(n)
	} else {
		unwrap(n)
	}
}

func newTab(n ast.Node) {
	n.SetAttributeString("rel", []byte("noopener noreferrer"))
	n.SetAttributeString("target", []byte("_blank"))
}

func insideLink(n ast.Node) bool {
	for p := n.Parent(); p != nil; p = p.Parent() {
		if p.Kind() == ast.KindLink {
			return true
		}
	}
	return false
}

// unwrap replaces n with its children.
func unwrap(n ast.Node) {
	parent := n.Parent()
	for c := n.FirstChild(); c != nil; {
		next := c.NextSibling()
		parent.InsertBefore(parent, n, c)
		c = next
	}
	parent.RemoveChild(parent, n)
}

// rawAsText shows raw HTML as the text it was typed as, where goldmark's safe mode drops it.
type rawAsText struct{}

func (rawAsText) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindRawHTML, func(w util.BufWriter, src []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			segs := node.(*ast.RawHTML).Segments
			for i := range segs.Len() {
				seg := segs.At(i)
				_, _ = w.Write(util.EscapeHTML(seg.Value(src)))
			}
		}
		return ast.WalkSkipChildren, nil
	})
	reg.Register(ast.KindHTMLBlock, func(w util.BufWriter, src []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		n := node.(*ast.HTMLBlock)
		var b bytes.Buffer
		for i := range n.Lines().Len() {
			line := n.Lines().At(i)
			b.Write(line.Value(src))
		}
		if n.HasClosure() {
			b.Write(n.ClosureLine.Value(src))
		}
		_, _ = w.WriteString("<p>")
		_, _ = w.Write(bytes.ReplaceAll(util.EscapeHTML(bytes.TrimRight(b.Bytes(), "\n")), []byte("\n"), []byte("<br>\n")))
		_, _ = w.WriteString("</p>\n")
		return ast.WalkSkipChildren, nil
	})
}
