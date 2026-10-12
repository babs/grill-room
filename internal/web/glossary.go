package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
)

const (
	// promptBudget is the prompt's size in estimated tokens. faster-whisper keeps the last 223 real
	// tokens of initial_prompt (005-AC23, spike S10); 240 estimated came to 203 on the S10 terms.
	promptBudget = 240
	// promptFrame is what the sentence's own words cost.
	promptFrame = 12
	// glossaryCapacity is how many terms of a usual size fit, as Manage says it: 3 tokens each on
	// the S10 terms.
	glossaryCapacity = 70
	maxGlossaryTerm  = 60
)

// estTokens over-estimates the Whisper tokens of one term and its comma: on the S10 terms it
// counts 217 where the tokenizer counts 195.
func estTokens(term string) int { return (len(term)+3)/4 + 1 }

// splitTerms splits a glossary on newlines and commas.
func splitTerms(s string) []string {
	var out []string
	for t := range strings.FieldsFuncSeq(s, func(r rune) bool { return r == '\n' || r == ',' }) {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// sttPrompt is the prompt the STT worker passes to Whisper, and how many terms it holds out of the
// room's distinct ones.
type sttPrompt struct {
	Prompt string `json:"prompt"`
	Used   int    `json:"used"`
	Total  int    `json:"total"`
}

// buildSTTPrompt writes glossary, names and docs as one sentence in initial_prompt, never as hotwords
// (spike S10). Terms are taken in that order while the budget lasts; the sentence puts them in
// reverse, so if Whisper cuts an overlong prompt from its start it drops the least wanted first.
func buildSTTPrompt(glossary, names, docs []string) sttPrompt {
	var out sttPrompt
	var kept [3][]string
	seen := map[string]bool{}
	budget, full := promptBudget-promptFrame, false
	for i, src := range [][]string{glossary, names, docs} {
		for _, t := range src {
			t = oneLine(t)
			k := strings.ToLower(t)
			if t == "" || seen[k] {
				continue
			}
			seen[k] = true
			out.Total++
			if c := estTokens(t); !full && c <= budget {
				budget -= c
				kept[i] = append(kept[i], t)
				out.Used++
			} else {
				full = true
			}
		}
	}
	var parts []string
	for _, p := range []struct {
		lead  string
		terms []string
	}{{"autour de ", kept[2]}, {"avec ", kept[1]}, {"on parle de ", kept[0]}} {
		if len(p.terms) > 0 {
			parts = append(parts, p.lead+andList(p.terms))
		}
	}
	if len(parts) > 0 {
		s := strings.Join(parts, ", ") + "."
		r, n := utf8.DecodeRuneInString(s)
		out.Prompt = string(unicode.ToUpper(r)) + s[n:]
	}
	return out
}

// andList writes "a, b et c".
func andList(ts []string) string {
	if len(ts) == 1 {
		return ts[0]
	}
	return strings.Join(ts[:len(ts)-1], ", ") + " et " + ts[len(ts)-1]
}

// roomPrompt builds room's prompt from its glossary, its participants' and personas' names, and its
// documents' titles.
func (s *Server) roomPrompt(ctx context.Context, room string) (sttPrompt, error) {
	st := s.bus.Store
	g, err := st.STTGlossary(ctx, room)
	if err != nil {
		return sttPrompt{}, err
	}
	ps, err := st.Participants(ctx, room)
	if err != nil {
		return sttPrompt{}, err
	}
	personas, err := st.RoomPersonas(ctx, room)
	if err != nil {
		return sttPrompt{}, err
	}
	docs, err := st.Documents(ctx, room)
	if err != nil {
		return sttPrompt{}, err
	}
	names := make([]string, 0, len(ps)+len(personas))
	for _, p := range ps {
		names = append(names, p.Name)
	}
	for _, p := range personas {
		names = append(names, p.Name)
	}
	titles := make([]string, 0, len(docs))
	for _, d := range docs {
		titles = append(titles, strings.TrimSuffix(d.Name, path.Ext(d.Name)))
	}
	return buildSTTPrompt(splitTerms(g), names, titles), nil
}

type glossaryResp struct {
	Glossary string `json:"glossary"`
	sttPrompt
	Capacity int `json:"capacity"`
}

func (s *Server) writeGlossary(w http.ResponseWriter, r *http.Request, room string) {
	g, err := s.bus.Store.STTGlossary(r.Context(), room)
	if err != nil {
		s.serverError(w, "glossary_failed", err)
		return
	}
	p, err := s.roomPrompt(r.Context(), room)
	if err != nil {
		s.serverError(w, "glossary_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, glossaryResp{Glossary: g, sttPrompt: p, Capacity: glossaryCapacity})
}

// getGlossary returns the room's STT glossary, the prompt it makes and how many terms it holds.
func (s *Server) getGlossary(w http.ResponseWriter, r *http.Request, sess session) {
	s.writeGlossary(w, r, sess.p.RoomID)
}

// putGlossary lets any member set the room's STT glossary, with a line in the room (005-AC23). The
// worker reads it before each utterance.
func (s *Server) putGlossary(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		Glossary string `json:"glossary"`
	}
	if !decode(w, r, &req) {
		return
	}
	g := strings.TrimSpace(req.Glossary)
	if g != "" && !store.ValidText(g, store.MaxSTTGlossary, true) {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("the glossary must be at most %d characters, without control characters", store.MaxSTTGlossary))
		return
	}
	for _, t := range splitTerms(g) {
		if utf8.RuneCountInString(t) > maxGlossaryTerm {
			httpError(w, http.StatusBadRequest, fmt.Sprintf("each term must be at most %d characters", maxGlossaryTerm))
			return
		}
	}
	m, err := s.bus.Store.SetSTTGlossary(r.Context(), sess.p.RoomID, g, sess.p)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such room")
		return
	}
	if err != nil {
		s.serverError(w, "glossary_failed", err)
		return
	}
	if m != nil {
		s.bus.Notify(sess.p.RoomID)
		s.log.Info("stt_glossary_changed", zap.String("room", sess.p.RoomID), zap.Int64("participant", sess.p.ID))
	}
	s.writeGlossary(w, r, sess.p.RoomID)
}

// sttPromptFor hands the STT worker a room's prompt.
func (s *Server) sttPromptFor(w http.ResponseWriter, r *http.Request) {
	p, err := s.roomPrompt(r.Context(), r.PathValue("room"))
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such room")
		return
	}
	if err != nil {
		s.serverError(w, "stt_prompt_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"prompt": p.Prompt})
}
