package web

import (
	"regexp"
	"slices"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"

	"github.com/babs/grill-room/internal/store"
)

// mentionMD parses plain CommonMark: only code spans and code blocks matter here, and Linkify
// would turn "a@b.c" into a link the walk would then skip or keep for the wrong reason.
var mentionMD = goldmark.New()

// mentionRe finds "@word" at a word start: "bob@grill" is an address, not a mention.
var mentionRe = regexp.MustCompile(`(?:^|[^\p{L}\p{N}_@])@([\p{L}\p{N}_-]+)`)

// proseText is body without its code spans, code blocks and raw HTML: "@grill" quoted as code
// calls no one (002-AC23).
func proseText(body string) string {
	src := []byte(body)
	var b strings.Builder
	doc := mentionMD.Parser().Parse(text.NewReader(src))
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		switch n := n.(type) {
		case *ast.CodeSpan, *ast.CodeBlock, *ast.FencedCodeBlock, *ast.HTMLBlock, *ast.RawHTML, *ast.AutoLink:
			if entering {
				b.WriteByte(' ')
			}
			return ast.WalkSkipChildren, nil
		case *ast.Text:
			if entering {
				b.Write(n.Segment.Value(src))
				if n.SoftLineBreak() || n.HardLineBreak() {
					b.WriteByte('\n')
				}
			}
		}
		if !entering && n.Type() == ast.TypeBlock {
			b.WriteByte('\n')
		}
		return ast.WalkContinue, nil
	})
	return b.String()
}

// mentioned returns the personas body names with "@name", case-insensitively, each once, in the
// order of their first mention (002-AC23, 002-AC24).
func mentioned(body string, personas []store.RoomPersona) []store.RoomPersona {
	if !strings.Contains(body, "@") {
		return nil
	}
	var out []store.RoomPersona
	for _, m := range mentionRe.FindAllStringSubmatch(proseText(body), -1) {
		for _, p := range personas {
			if strings.EqualFold(m[1], p.Name) && !slices.ContainsFunc(out, func(o store.RoomPersona) bool { return o.ID == p.ID }) {
				out = append(out, p)
			}
		}
	}
	return out
}
