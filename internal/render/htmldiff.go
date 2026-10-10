// Derived from github.com/babs/grip-live-diff internal/htmldiff.go at commit 36209e6.

package render

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sergi/go-diff/diffmatchpatch"
)

const (
	insClass = "gg-ins"
	delClass = "gg-del"
	spaces   = " \t\n\r\f\v"
)

type tokenKind uint8

const (
	kindText   tokenKind = iota // words, the only tokens that get annotated
	kindTag                     // markup, passed through untouched
	kindAtomic                  // mermaid/math/script/style, changed as a whole
)

type token struct {
	s    string
	kind tokenKind
	// exact keeps the whitespace significant when comparing: true inside <pre>, where a
	// re-indentation is a real change, false in prose, where the line wrapping is not.
	exact bool
}

// DiffHTML returns newHTML with the passages that differ from oldHTML wrapped in
// <ins>/<del>. Equal and inserted tokens concatenate back to newHTML exactly, so the
// document structure is always that of the new version; deletions only ever inject text.
func DiffHTML(oldHTML, newHTML []byte) []byte {
	oldTokens := tokenize(string(oldHTML))
	newTokens := tokenize(string(newHTML))

	v := newVocab()
	dmp := diffmatchpatch.New()
	// Past this budget the algorithm returns a coarser diff. One second is meant for a
	// server under load; a local preview can afford to keep the diff precise.
	dmp.DiffTimeout = 5 * time.Second
	// No semantic cleanup: it is tuned for characters and here one rune is a whole word,
	// so it would swallow the untouched words sitting between two edits of the same line.
	diffs := dmp.DiffMainRunes(v.encode(oldTokens), v.encode(newTokens), false)

	var out strings.Builder
	out.Grow(len(newHTML))
	// Walk both token slices alongside the diff: two tokens can be equal yet spelled
	// differently, so the output must take its text from the new document itself.
	oi, ni := 0, 0
	for _, d := range diffs {
		n := utf8.RuneCountInString(d.Text)
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			for _, t := range newTokens[ni : ni+n] {
				out.WriteString(t.s)
			}
			oi, ni = oi+n, ni+n
		case diffmatchpatch.DiffInsert:
			writeInserted(&out, newTokens[ni:ni+n])
			ni += n
		case diffmatchpatch.DiffDelete:
			writeDeleted(&out, oldTokens[oi:oi+n])
			oi += n
		}
	}
	return []byte(out.String())
}

func writeInserted(out *strings.Builder, tokens []token) {
	var run []token
	for _, t := range tokens {
		// Tags carry the structure of the new document: emitting them inside <ins> would
		// nest the annotation wrongly, so they cut the run instead.
		if t.kind == kindTag || isBlank(t.s) {
			flushInserted(out, run)
			run = run[:0]
			out.WriteString(t.s)
			continue
		}
		run = append(run, t)
	}
	flushInserted(out, run)
}

func writeDeleted(out *strings.Builder, tokens []token) {
	var run []token
	for _, t := range tokens {
		switch {
		case t.kind == kindTag:
			// Dropped: the removed markup has no place in the new document tree.
			continue
		case t.kind == kindAtomic:
			run = append(run, token{s: atomicInner(t.s), kind: kindText})
		case isBlank(t.s):
			if len(run) > 0 {
				run = append(run, t)
			}
		default:
			run = append(run, t)
		}
	}
	flushDeleted(out, run)
}

// flushInserted keeps the trailing whitespace outside the marker: it belongs to the new
// document, and the highlight should stop at the last visible character.
func flushInserted(out *strings.Builder, run []token) {
	s := joinTokens(run)
	trimmed := strings.TrimRight(s, spaces)
	if trimmed == "" {
		out.WriteString(s)
		return
	}
	out.WriteString(`<ins class="` + insClass + `">` + trimmed + "</ins>" + s[len(trimmed):])
}

// flushDeleted keeps everything inside the marker, whitespace included: removing the
// element must leave the new document byte-for-byte intact, and the space still separates
// the removed words from the surrounding text.
func flushDeleted(out *strings.Builder, run []token) {
	s := joinTokens(run)
	if strings.Trim(s, spaces) == "" {
		return
	}
	out.WriteString(`<del class="` + delClass + `">` + s + "</del>")
}

func joinTokens(run []token) string {
	var b strings.Builder
	for _, t := range run {
		b.WriteString(t.s)
	}
	return b.String()
}

func tokenize(html string) []token {
	var out []token
	pre := 0
	for i := 0; i < len(html); {
		if html[i] != '<' {
			next := strings.IndexByte(html[i:], '<')
			if next < 0 {
				next = len(html) - i
			}
			out = appendWords(out, html[i:i+next], pre > 0)
			i += next
			continue
		}

		end := strings.IndexByte(html[i:], '>')
		if end < 0 {
			out = appendWords(out, html[i:], pre > 0)
			break
		}
		tag := html[i : i+end+1]
		if stop := atomicEnd(html, i+end+1, tag); stop > 0 {
			out = append(out, token{s: html[i:stop], kind: kindAtomic})
			i = stop
			continue
		}
		switch {
		case isTag(tag, "pre") && !strings.HasSuffix(tag, "/>"):
			pre++
		case isTag(tag, "/pre") && pre > 0:
			pre--
		}
		out = append(out, token{s: tag, kind: kindTag})
		i += end + 1
	}
	return out
}

// appendWords splits text into tokens of one word plus the whitespace that follows it.
func appendWords(out []token, text string, exact bool) []token {
	for i := 0; i < len(text); {
		j := i
		for j < len(text) && !isSpaceByte(text[j]) {
			j++
		}
		for j < len(text) && isSpaceByte(text[j]) {
			j++
		}
		out = append(out, token{s: text[i:j], kind: kindText, exact: exact})
		i = j
	}
	return out
}

// isTag reports whether tag opens (or closes, with a leading slash in name) that element
// and not merely one whose name starts with it: <preload> must not count as a <pre>.
func isTag(tag, name string) bool {
	rest := strings.TrimPrefix(tag, "<"+name)
	if rest == "" || len(rest) == len(tag) {
		return false
	}
	return rest == ">" || rest == "/>" || isSpaceByte(rest[0])
}

// atomicEnd returns the offset just past the element opened by openTag when its content
// must never be annotated (annotating it would break mermaid, MathJax or a script), or -1.
func atomicEnd(html string, contentStart int, openTag string) int {
	var name string
	switch {
	case strings.HasPrefix(openTag, "<img"):
		// Void element: without this it would be a plain tag, hence dropped when removed
		// and left unmarked when added — an invisible change.
		return contentStart
	case strings.HasPrefix(openTag, "<script"):
		name = "script"
	case strings.HasPrefix(openTag, "<style"):
		name = "style"
	case strings.HasPrefix(openTag, "<video"):
		name = "video"
	case strings.HasPrefix(openTag, "<audio"):
		name = "audio"
	case strings.HasPrefix(openTag, "<iframe"):
		name = "iframe"
	case strings.HasPrefix(openTag, "<pre") && strings.Contains(openTag, "mermaid"):
		name = "pre"
	case strings.HasPrefix(openTag, "<span") && strings.Contains(openTag, `class="math`):
		name = "span"
	default:
		return -1
	}
	if strings.HasSuffix(openTag, "/>") {
		return contentStart
	}

	closing, opening := "</"+name+">", "<"+name
	for i, depth := contentStart, 1; depth > 0; {
		next := strings.Index(html[i:], closing)
		if next < 0 {
			return -1
		}
		if nested := strings.Index(html[i:i+next], opening); nested >= 0 {
			depth++
			i += nested + len(opening)
			continue
		}
		depth--
		i += next + len(closing)
		if depth == 0 {
			return i
		}
	}
	return -1
}

func atomicInner(s string) string {
	start := strings.IndexByte(s, '>')
	end := strings.LastIndex(s, "</")
	if start >= 0 && end > start {
		return s[start+1 : end]
	}
	return s
}

func isBlank(s string) bool {
	return strings.Trim(s, spaces) == ""
}

func isSpaceByte(b byte) bool {
	return strings.IndexByte(spaces, b) >= 0
}

// vocab maps every distinct token to a rune so a character-level diff becomes a
// token-level one.
// vkey identifies a token without allocating: word carries the text as it must be
// compared, exact tells verbatim tokens from those whose trailing whitespace was dropped,
// so the two families can never collide.
type vkey struct {
	word  string
	space bool
	exact bool
}

type vocab struct {
	ids  map[vkey]rune
	next rune
}

func newVocab() *vocab {
	return &vocab{ids: make(map[vkey]rune), next: 1}
}

func (v *vocab) encode(tokens []token) []rune {
	out := make([]rune, 0, len(tokens))
	for _, t := range tokens {
		k := key(t)
		r, ok := v.ids[k]
		if !ok {
			r = v.next
			v.next++
			// Surrogates are not valid runes: they would collapse to U+FFFD once
			// diffmatchpatch turns the rune slices back into strings.
			if v.next == 0xD800 {
				v.next = 0xE000
			}
			v.ids[k] = r
		}
		out = append(out, r)
	}
	return out
}

// key identifies a token for the comparison. Prose words drop the shape of the trailing
// whitespace, so re-wrapping a paragraph — a newline becoming a space — is not a change;
// only its presence is kept, since that is what separates two words.
func key(t token) vkey {
	if t.kind != kindText || t.exact {
		return vkey{word: t.s, exact: true}
	}
	word := strings.TrimRight(t.s, spaces)
	return vkey{word: word, space: word != t.s}
}
