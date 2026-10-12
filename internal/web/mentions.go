package web

import (
	"context"
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

// notToken is what a participant's name holds besides the characters an @name takes.
var notToken = regexp.MustCompile(`[^\p{L}\p{N}_-]+`)

// mentionToken is how "@" spells a participant's name: "Ana María" is @Ana_María. The room page's
// autocompletion writes it the same way (005-AC2).
func mentionToken(name string) string {
	return strings.Trim(notToken.ReplaceAllString(name, "_"), "_")
}

// named returns the @names of body that name one of personas or of people, once each as written,
// in order: a persona first, as a mention calls it before a person of the same name (005-AC27).
// The one rule for every mention: chips, notices, turns and pushes.
func named(body string, personas []store.RoomPersona, people []store.Participant) []store.Named {
	if !strings.Contains(body, "@") {
		return nil
	}
	var out []store.Named
	for _, m := range mentionRe.FindAllStringSubmatch(proseText(body), -1) {
		at := "@" + m[1]
		if slices.ContainsFunc(out, func(n store.Named) bool { return n.At == at }) {
			continue
		}
		if i := slices.IndexFunc(personas, func(p store.RoomPersona) bool { return strings.EqualFold(p.Name, m[1]) }); i >= 0 {
			out = append(out, store.Named{At: at, PersonaID: personas[i].ID})
		} else if i := slices.IndexFunc(people, func(p store.Participant) bool { return strings.EqualFold(mentionToken(p.Name), m[1]) }); i >= 0 {
			out = append(out, store.Named{At: at, ParticipantID: people[i].ID})
		}
	}
	return out
}

// nameMentions fills in the @names of msgs, all lines of room, for the page to draw (005-AC27).
func (s *Server) nameMentions(ctx context.Context, room string, msgs []store.Message) error {
	if !slices.ContainsFunc(msgs, func(m store.Message) bool { return strings.Contains(m.Body, "@") }) {
		return nil
	}
	personas, err := s.bus.Store.RoomPersonas(ctx, room)
	if err != nil {
		return err
	}
	people, err := s.bus.Store.Participants(ctx, room)
	if err != nil {
		return err
	}
	for i := range msgs {
		if msgs[i].Kind != store.KindSystem && msgs[i].DeletedAt == nil {
			msgs[i].Named = named(msgs[i].Body, personas, people)
		}
	}
	return nil
}
