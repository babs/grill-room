package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/voices"
)

// Audit actions of the lexicon (013-AC31).
const (
	ActionLexiconAdd    = "tts.lexicon.add"
	ActionLexiconEdit   = "tts.lexicon.edit"
	ActionLexiconDelete = "tts.lexicon.delete"
)

const (
	maxTermRunes = 40
	maxFormRunes = 80
)

// lexiconTerm reads and checks an entry; false when it answered the refusal.
func lexiconTerm(w http.ResponseWriter, r *http.Request) (voices.Term, bool) {
	var t voices.Term
	if !decode(w, r, &t) {
		return t, false
	}
	t.Term, t.FrEn, t.FrFr, t.En = strings.TrimSpace(t.Term), strings.TrimSpace(t.FrEn), strings.TrimSpace(t.FrFr), strings.TrimSpace(t.En)
	bad := func(s string, max int) bool {
		return utf8.RuneCountInString(s) > max || strings.IndexFunc(s, unicode.IsControl) >= 0
	}
	var msg string
	switch {
	case t.Term == "" || bad(t.Term, maxTermRunes):
		msg = "term must be 1 to 40 characters"
	case t.Style != "en" && t.Style != "fr":
		msg = `style must be "en" or "fr"`
	case t.FrEn == "" && t.FrFr == "" && t.En == "":
		msg = "set at least one spoken form"
	case bad(t.FrEn, maxFormRunes) || bad(t.FrFr, maxFormRunes) || bad(t.En, maxFormRunes):
		msg = "a spoken form is at most 80 characters"
	}
	if msg != "" {
		httpError(w, http.StatusBadRequest, msg)
		return t, false
	}
	return t, true
}

func (s *Server) listPronunciations(w http.ResponseWriter, r *http.Request) {
	ps, err := s.bus.Store.Pronunciations(r.Context())
	if err != nil {
		s.serverError(w, "list_pronunciations_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": ps, "max": store.MaxPronunciations})
}

func (s *Server) addPronunciation(w http.ResponseWriter, r *http.Request) {
	t, ok := lexiconTerm(w, r)
	if !ok {
		return
	}
	p, err := s.bus.Store.AddPronunciation(s.auditing(r, ActionLexiconAdd, store.TargetPronunciation, "", "", nil), t, store.ActorSuperAdmin)
	if s.lexiconError(w, err) {
		return
	}
	s.lexiconChanged("lexicon_added", t)
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) editPronunciation(w http.ResponseWriter, r *http.Request) {
	t, ok := lexiconTerm(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpError(w, http.StatusNotFound, "no such term")
		return
	}
	p, err := s.bus.Store.UpdatePronunciation(s.auditing(r, ActionLexiconEdit, store.TargetPronunciation, r.PathValue("id"), "", nil), id, t, store.ActorSuperAdmin)
	if s.lexiconError(w, err) {
		return
	}
	s.lexiconChanged("lexicon_edited", t)
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) deletePronunciation(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		err = s.bus.Store.DeletePronunciation(s.auditing(r, ActionLexiconDelete, store.TargetPronunciation, r.PathValue("id"), "", nil), id)
	} else {
		err = store.ErrNotFound
	}
	if s.lexiconError(w, err) {
		return
	}
	s.lexiconChanged("lexicon_deleted", voices.Term{})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) lexiconError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, "no such term")
	case errors.Is(err, store.ErrTermTaken), errors.Is(err, store.ErrLexiconFull):
		httpError(w, http.StatusBadRequest, err.Error())
	default:
		s.serverError(w, "lexicon_change_failed", err)
	}
	return true
}

// lexiconChanged hands the lexicon to the pod for its next line (013-AC31).
func (s *Server) lexiconChanged(event string, t voices.Term) {
	if s.tts != nil {
		s.tts.lexiconChanged()
	}
	s.log.Info(event, zap.String("term", t.Term), zap.String("style", t.Style))
}
