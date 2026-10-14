package sandbox

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
)

// redacted stands for a value the turn's output must not hold (009-AC23).
const redacted = "[redacted]"

// Redactor replaces known secrets with [redacted] in what a turn sends the room.
type Redactor struct {
	// forms are each secret as written, JSON-escaped as Claude Code writes it, JSON-escaped with
	// HTML escaping as Go and other tools write it, and escaped twice, as JSON quoted in JSON.
	forms []string
}

// NewRedactor redacts secrets; nil when there is none.
func NewRedactor(secrets []string) *Redactor {
	var forms []string
	for _, s := range secrets {
		if s == "" {
			continue
		}
		esc := jsonEscaped(s, false)
		forms = append(forms, s, esc, jsonEscaped(s, true), jsonEscaped(esc, false))
	}
	slices.Sort(forms)
	forms = slices.Compact(forms)
	if len(forms) == 0 {
		return nil
	}
	return &Redactor{forms: forms}
}

// jsonEscaped is s inside a JSON string, with or without HTML escaping.
func jsonEscaped(s string, html bool) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(html)
	_ = enc.Encode(s) // a string: cannot fail
	out := strings.TrimSuffix(b.String(), "\n")
	return out[1 : len(out)-1]
}

// Text replaces every occurrence of any form of a secret. Overlapping or touching occurrences,
// of one secret or of two, merge into one [redacted]: no part of either is left.
func (r *Redactor) Text(s string) string {
	if r == nil {
		return s
	}
	type span struct{ from, to int }
	var spans []span
	for _, f := range r.forms {
		for i := 0; ; {
			j := strings.Index(s[i:], f)
			if j < 0 {
				break
			}
			spans = append(spans, span{i + j, i + j + len(f)})
			i += j + 1
		}
	}
	if len(spans) == 0 {
		return s
	}
	slices.SortFunc(spans, func(a, b span) int { return a.from - b.from })
	var out strings.Builder
	last, cur := 0, spans[0]
	flush := func() {
		out.WriteString(s[last:cur.from])
		out.WriteString(redacted)
		last = cur.to
	}
	for _, sp := range spans[1:] {
		if sp.from <= cur.to {
			cur.to = max(cur.to, sp.to)
			continue
		}
		flush()
		cur = sp
	}
	flush()
	out.WriteString(s[last:])
	return out.String()
}

// JSON redacts the string literals of JSON text, one document or a jsonl file: each literal is
// decoded, redacted as text and encoded again, so the result parses wherever the input did. Text
// that is not JSON, a reply file the agent got wrong, is redacted as plain text.
func (r *Redactor) JSON(b []byte) []byte {
	if r == nil {
		return b
	}
	if !isJSON(b) {
		return []byte(r.Text(string(b)))
	}
	var out []byte
	last := 0
	for i := 0; i < len(b); i++ {
		if b[i] != '"' {
			continue
		}
		j := i + 1
		for j < len(b) && b[j] != '"' {
			if b[j] == '\\' {
				j++
			}
			j++
		}
		end := min(j+1, len(b))
		lit := b[i:end]
		var s string
		repl := lit
		if json.Unmarshal(lit, &s) == nil {
			if t := r.Text(s); t != s {
				var e bytes.Buffer
				enc := json.NewEncoder(&e)
				enc.SetEscapeHTML(false)
				_ = enc.Encode(t) // a string: cannot fail
				repl = bytes.TrimSuffix(e.Bytes(), []byte("\n"))
			}
		} else {
			repl = []byte(r.Text(string(lit)))
		}
		if !bytes.Equal(repl, lit) {
			out = append(append(out, b[last:i]...), repl...)
			last = end
		}
		i = end - 1
	}
	if out == nil {
		return b
	}
	return append(out, b[last:]...)
}

// isJSON reports whether b is one JSON document, or JSON lines with blank lines between.
func isJSON(b []byte) bool {
	if json.Valid(b) {
		return true
	}
	for line := range bytes.SplitSeq(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 && !json.Valid(line) {
			return false
		}
	}
	return true
}
