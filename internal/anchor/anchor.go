// Package anchor locates annotations in a rendered document. A text anchor is a W3C
// TextQuoteSelector (exact, prefix, suffix); an element anchor is an nth-of-type path from body
// plus a fingerprint of the element's text.
//
// The text model and the quote matching are ported from github.com/babs/grip-live-diff
// defaults/static/js/annotate.js at commit 36209e6. internal/web/ui/doc/annotate.js runs the same
// rules in the browser: change both together.
package anchor

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// Kinds of anchor.
const (
	KindText    = "text"
	KindElement = "element"
)

// Limits, in runes. The browser counts UTF-16 units, never fewer than runes, so whatever it
// accepts passes here.
const (
	MaxExact       = 4000
	MaxContext     = 64
	MaxFingerprint = 128
	// contextLen is how much context a re-anchored quote keeps on each side, as the browser does.
	contextLen = 32
	// maxSteps and maxPath bound an element path: 64 levels of nesting is already absurd HTML.
	maxSteps = 64
	maxPath  = 4096
	// fingerprintLen is where the browser cuts an element's text.
	fingerprintLen = 80
)

// Anchor ties an annotation to part of a document. A nil *Anchor is the whole document.
type Anchor struct {
	Kind   string `json:"kind"`
	Exact  string `json:"exact,omitempty"`
	Prefix string `json:"prefix,omitempty"`
	Suffix string `json:"suffix,omitempty"`
	// Path is "body > div:nth-of-type(2) > p:nth-of-type(1)": a CSS selector, walked step by step
	// rather than handed to a selector engine, so the browser and the server read it the same way.
	Path        string `json:"path,omitempty"`
	Tag         string `json:"tag,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

var (
	tagRe  = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9-]{0,31}$`)
	pathRe = regexp.MustCompile(`^body(?: > [a-zA-Z][a-zA-Z0-9-]{0,31}:nth-of-type\([1-9][0-9]{0,3}\))*$`)
	stepRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9-]*):nth-of-type\(([0-9]+)\)`)
)

func runes(s string, maxRunes int) bool {
	return utf8.ValidString(s) && utf8.RuneCountInString(s) <= maxRunes
}

// Validate checks a's shape and sizes. It does not check that a points at anything.
func (a *Anchor) Validate() error {
	switch a.Kind {
	case KindText:
		if a.Path != "" || a.Tag != "" || a.Fingerprint != "" {
			return errors.New("a text anchor has no path, tag or fingerprint")
		}
		if strings.TrimSpace(a.Exact) == "" || !runes(a.Exact, MaxExact) {
			return fmt.Errorf("exact must be 1 to %d characters", MaxExact)
		}
		if !runes(a.Prefix, MaxContext) || !runes(a.Suffix, MaxContext) {
			return fmt.Errorf("prefix and suffix must be at most %d characters", MaxContext)
		}
	case KindElement:
		if a.Exact != "" || a.Prefix != "" || a.Suffix != "" {
			return errors.New("an element anchor has no exact, prefix or suffix")
		}
		if !tagRe.MatchString(a.Tag) || len(a.Path) > maxPath || !pathRe.MatchString(a.Path) || strings.Count(a.Path, " > ") > maxSteps {
			return errors.New("bad element path or tag")
		}
		if last := a.Path[strings.LastIndex(a.Path, " ")+1:]; strings.Split(last, ":")[0] != a.Tag {
			return errors.New("tag must be the path's last element")
		}
		if !runes(a.Fingerprint, MaxFingerprint) {
			return fmt.Errorf("fingerprint must be at most %d characters", MaxFingerprint)
		}
	default:
		return errors.New(`kind must be "text" or "element"`)
	}
	return nil
}

// Doc is a parsed page: its body and the folded text annotations quote.
type Doc struct {
	body *html.Node
	text string
}

// Parse reads a page or a fragment, as rendered for the document frame.
func Parse(page []byte) (*Doc, error) {
	root, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return nil, fmt.Errorf("parse page: %w", err)
	}
	d := &Doc{body: find(root, func(n *html.Node) bool { return n.Type == html.ElementNode && n.Data == "body" })}
	if d.body == nil {
		// A frameset page has no body: nothing in it can be annotated.
		d.body = &html.Node{Type: html.ElementNode, Data: "body"}
	}
	d.text = fold(textOf(d.body))
	return d, nil
}

func find(n *html.Node, match func(*html.Node) bool) *html.Node {
	if match(n) {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if f := find(c, match); f != nil {
			return f
		}
	}
	return nil
}

func hasClass(n *html.Node, class string) bool {
	for _, a := range n.Attr {
		if a.Key == "class" && slices.Contains(strings.Fields(a.Val), class) {
			return true
		}
	}
	return false
}

func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Namespace == "" && a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

// skip mirrors the browser's SKIP selector: text that is not the document (diff deletions,
// diagrams and formulas whose text the source lacks, scripts, buttons, hidden parts).
func skip(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	switch n.Data {
	case "script", "style", "noscript", "template", "button", "mjx-container":
		return true
	case "del":
		if hasClass(n, "gg-del") {
			return true
		}
	}
	if _, hidden := attr(n, "hidden"); hidden {
		return true
	}
	return hasClass(n, "mermaid") || hasClass(n, "math") || hasClass(n, "gr-annot-ui")
}

// textOf concatenates the text under root, leaving out skipped descendants (root itself counts).
func textOf(root *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			switch {
			case c.Type == html.TextNode:
				b.WriteString(c.Data)
			case c.Type == html.ElementNode && !skip(c):
				walk(c)
			}
		}
	}
	walk(root)
	return b.String()
}

// isSpace is JavaScript's \s: Go's unicode.IsSpace plus U+FEFF, minus U+0085.
func isSpace(r rune) bool {
	return r == 0xfeff || (r != 0x85 && unicode.IsSpace(r))
}

// fold collapses whitespace runs to one space and drops leading whitespace, as the browser's
// fold does: a re-wrapped paragraph still matches its quote.
func fold(raw string) string {
	var b strings.Builder
	space := false
	for _, r := range raw {
		if isSpace(r) {
			if !space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// context counts how many bytes of prefix end at idx, plus how many of suffix start at end.
func context(text string, idx, end int, prefix, suffix string) int {
	n, m := 0, 0
	for n < len(prefix) && idx-1-n >= 0 && text[idx-1-n] == prefix[len(prefix)-1-n] {
		n++
	}
	for m < len(suffix) && end+m < len(text) && text[end+m] == suffix[m] {
		m++
	}
	return n + m
}

// locate finds a text anchor: exact hits first (prefix and suffix pick among several), then the
// Hypothesis fallback between prefix and suffix. An empty prefix or suffix stands for the start or
// the end of the document, so a reworded first heading still re-anchors.
func locate(text string, a *Anchor) (start, end int, ok bool) {
	best, score := -1, -1
	for from := 0; ; {
		i := strings.Index(text[from:], a.Exact)
		if i < 0 {
			break
		}
		i += from
		if s := context(text, i, i+len(a.Exact), a.Prefix, a.Suffix); s > score {
			best, score = i, s
		}
		_, w := utf8.DecodeRuneInString(text[i:])
		from = i + w
	}
	if best >= 0 {
		return best, best + len(a.Exact), true
	}
	start, end = 0, len(text)
	if a.Prefix != "" {
		p := strings.Index(text, a.Prefix)
		if p < 0 {
			return 0, 0, false
		}
		start = p + len(a.Prefix)
	}
	if a.Suffix != "" {
		q := strings.Index(text[start:], a.Suffix)
		if q < 0 {
			return 0, 0, false
		}
		end = start + q
	}
	if end <= start || utf8.RuneCountInString(text[start:end]) > 3*utf8.RuneCountInString(a.Exact) {
		return 0, 0, false
	}
	return start, end, true
}

func lastRunes(s string, n int) string {
	r := []rune(s)
	return string(r[max(0, len(r)-n):])
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	return string(r[:min(len(r), n)])
}

// Reanchor finds a in d and returns it rewritten against d: the quote and its context as they
// read now, or the element's current path. ok is false when a is gone from d (orphaned); a is
// then returned unchanged so it stays readable.
func (d *Doc) Reanchor(a Anchor) (Anchor, bool) {
	switch a.Kind {
	case KindText:
		start, end, ok := locate(d.text, &a)
		if !ok {
			return a, false
		}
		exact := d.text[start:end]
		start += len(exact) - len(strings.TrimLeft(exact, " "))
		exact = strings.TrimSpace(exact)
		end = start + len(exact)
		if exact == "" || !runes(exact, MaxExact) {
			return a, false
		}
		return Anchor{Kind: KindText, Exact: exact,
			Prefix: lastRunes(d.text[:start], contextLen), Suffix: firstRunes(d.text[end:], contextLen)}, true
	case KindElement:
		n := d.resolve(&a)
		if n == nil {
			return a, false
		}
		return Anchor{Kind: KindElement, Path: pathOf(n), Tag: n.Data, Fingerprint: fingerprint(n)}, true
	}
	return a, false
}

// fingerprint is the element's folded text cut to 80 runes; for an element with no text (an
// image), its first non-empty alt, aria-label, title or src, marked with "@".
func fingerprint(n *html.Node) string {
	if t := strings.TrimSpace(fold(textOf(n))); t != "" {
		return firstRunes(t, fingerprintLen)
	}
	for _, k := range []string{"alt", "aria-label", "title", "src"} {
		if v, _ := attr(n, k); strings.TrimSpace(v) != "" {
			return "@" + firstRunes(strings.TrimSpace(fold(v)), fingerprintLen)
		}
	}
	return ""
}

func elementChildren(n *html.Node, yield func(*html.Node) bool) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && !yield(c) {
			return
		}
	}
}

// walk follows an nth-of-type path from body; nil when a step has no such element.
func (d *Doc) walk(path string) *html.Node {
	n := d.body
	for _, m := range stepRe.FindAllStringSubmatch(path, -1) {
		k, _ := strconv.Atoi(m[2])
		var next *html.Node
		elementChildren(n, func(c *html.Node) bool {
			if c.Data == m[1] {
				if k--; k == 0 {
					next = c
					return false
				}
			}
			return true
		})
		if next == nil {
			return nil
		}
		n = next
	}
	return n
}

func pathOf(n *html.Node) string {
	var steps []string
	for ; n != nil && n.Data != "body"; n = n.Parent {
		k := 1
		for s := n.PrevSibling; s != nil; s = s.PrevSibling {
			if s.Type == html.ElementNode && s.Data == n.Data {
				k++
			}
		}
		steps = append(steps, fmt.Sprintf("%s:nth-of-type(%d)", n.Data, k))
	}
	slices.Reverse(steps)
	return strings.Join(append([]string{"body"}, steps...), " > ")
}

// resolve finds an element anchor: at its path when the element there still has its tag and
// fingerprint, else the first element of that tag with that fingerprint. An element with no
// fingerprint is only looked for at its path: any empty element of its tag would match.
func (d *Doc) resolve(a *Anchor) *html.Node {
	if n := d.walk(a.Path); n != nil && n.Data == a.Tag && fingerprint(n) == a.Fingerprint {
		return n
	}
	if a.Fingerprint == "" {
		return nil
	}
	// ponytail: first match in document order, and a fingerprint per candidate is quadratic on
	// deeply nested pages; prefer the candidate nearest the old path if duplicates bite.
	return find(d.body, func(n *html.Node) bool {
		return n.Type == html.ElementNode && n.Data == a.Tag && fingerprint(n) == a.Fingerprint
	})
}
