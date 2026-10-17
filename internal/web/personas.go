package web

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
	"github.com/babs/grill-room/internal/voices"
)

const (
	maxPersonaText  = 8000
	maxBadge        = 8
	maxTemplateName = 60
	maxEveryN       = 100
	maxEveryVoice   = 500
	maxEveryMinutes = 240
	// A persona's speaking rate (013-AC37).
	minVoiceRate = 0.9
	maxVoiceRate = 1.3
)

var (
	// A persona name is one word, so "@name" can call it (spec 002 phase 5).
	personaNameRe = regexp.MustCompile(`^[\p{L}\p{N}_-]{1,30}$`)
	colourRe      = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

// personaReq is a persona's fields in a request; absent fields are nil.
type personaReq struct {
	Name     *string `json:"name"`
	Badge    *string `json:"badge"`
	Colour   *string `json:"colour"`
	Text     *string `json:"text"`
	Model    *string `json:"model"`
	Effort   *string `json:"effort"`
	EveryN   *int    `json:"every_n"`
	SelfCall *bool   `json:"self_call"`
	// EveryVoice and EveryMinutes are the other automatic triggers; one at most (006-AC18).
	EveryVoice   *int `json:"every_voice"`
	EveryMinutes *int `json:"every_minutes"`
	// Autocompact 0 is auto: Claude Code picks when to compact.
	Autocompact *int `json:"autocompact"`
	// WatchesNotes makes it the room's notes watcher, in place of any other (009-AC31).
	WatchesNotes *bool `json:"watches_notes"`
	// Speaks, Voice and VoiceRate: the persona in the audio call (013-AC26, 013-AC11, 013-AC37).
	Speaks    *bool    `json:"speaks"`
	Voice     *string  `json:"voice"`
	VoiceRate *float64 `json:"voice_rate"`
}

// check cleans the fields present and says what is wrong with the first bad one, "" when none is.
func (q *personaReq) check() string {
	if q.Name != nil && !personaNameRe.MatchString(*q.Name) {
		return "name must be 1 to 30 letters, digits, - or _"
	}
	if q.Badge != nil {
		b, ok := cleanText(*q.Badge, maxBadge, false)
		if !ok {
			return fmt.Sprintf("badge must be 1 to %d characters", maxBadge)
		}
		*q.Badge = b
	}
	if q.Colour != nil && !colourRe.MatchString(*q.Colour) {
		return "colour must be like #1a2b3c"
	}
	if q.Text != nil {
		t, ok := cleanText(strings.ReplaceAll(*q.Text, "\r\n", "\n"), maxPersonaText, true)
		if !ok {
			return fmt.Sprintf("text must be 1 to %d characters", maxPersonaText)
		}
		*q.Text = t
	}
	if q.Voice != nil && *q.Voice != "" && voices.Known(*q.Voice) == "" {
		return "voice must be a catalogue voice id, or empty for the default"
	}
	if q.VoiceRate != nil {
		if r := math.Round(*q.VoiceRate*100) / 100; r >= minVoiceRate && r <= maxVoiceRate {
			*q.VoiceRate = r
		} else {
			return fmt.Sprintf("voice_rate must be %g to %g", minVoiceRate, maxVoiceRate)
		}
	}
	// 005-AC9: no "default" model or effort, which only the host could read.
	if q.Model != nil && !slices.Contains(turn.Models, *q.Model) {
		return fmt.Sprintf("model must be one of %v", turn.Models)
	}
	if q.Effort != nil && !slices.Contains(turn.Efforts, *q.Effort) {
		return fmt.Sprintf("effort must be one of %v", turn.Efforts)
	}
	if q.EveryN != nil && (*q.EveryN < 0 || *q.EveryN > maxEveryN) {
		return fmt.Sprintf("every_n must be 0 (off) to %d", maxEveryN)
	}
	if q.EveryVoice != nil && (*q.EveryVoice < 0 || *q.EveryVoice > maxEveryVoice) {
		return fmt.Sprintf("every_voice must be 0 (off) to %d", maxEveryVoice)
	}
	if q.EveryMinutes != nil && (*q.EveryMinutes < 0 || *q.EveryMinutes > maxEveryMinutes) {
		return fmt.Sprintf("every_minutes must be 0 (off) to %d", maxEveryMinutes)
	}
	// One automatic trigger at most: setting one turns the other two off (006-AC18).
	var on []*int
	for _, v := range []*int{q.EveryN, q.EveryVoice, q.EveryMinutes} {
		if v != nil && *v > 0 {
			on = append(on, v)
		}
	}
	if len(on) > 1 {
		return "set one of every_n, every_voice and every_minutes, not several"
	}
	if len(on) == 1 {
		zero := func(p **int) {
			if *p == nil || **p == 0 {
				*p = new(int)
			}
		}
		zero(&q.EveryN)
		zero(&q.EveryVoice)
		zero(&q.EveryMinutes)
	}
	if a := q.Autocompact; a != nil && *a != 0 && (*a < turn.MinAutocompact || *a > turn.MaxAutocompact) {
		return fmt.Sprintf("autocompact must be 0 (auto) or %d to %d tokens", turn.MinAutocompact, turn.MaxAutocompact)
	}
	return ""
}

// fields returns the persona the request describes in full; false when a field is missing.
func (q *personaReq) fields() (store.PersonaFields, bool) {
	if q.Name == nil || q.Badge == nil || q.Colour == nil || q.Text == nil || q.Model == nil || q.Effort == nil {
		return store.PersonaFields{}, false
	}
	return store.PersonaFields{Name: *q.Name, Badge: *q.Badge, Colour: *q.Colour, Text: *q.Text, Model: *q.Model, Effort: *q.Effort}, true
}

// listTemplates shows the library to a room member, with the templates their profile published.
func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request, sess session) {
	ts, err := s.bus.Store.Templates(r.Context(), sess.p.ProfileID)
	if err != nil {
		s.serverError(w, "list_templates_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, ts)
}

// adminTemplates lists the library for the admin, who removes templates by id.
func (s *Server) adminTemplates(w http.ResponseWriter, r *http.Request) {
	ts, err := s.bus.Store.Templates(r.Context(), 0)
	if err != nil {
		s.serverError(w, "list_templates_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, ts)
}

// publishTemplate adds a template to the library under the member's profile, or with template_id
// a new version of one that profile published (002-AC13). The member's name in the room signs it.
func (s *Server) publishTemplate(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		personaReq
		TemplateID int64  `json:"template_id"`
		Title      string `json:"title"`
	}
	if !decode(w, r, &req) {
		return
	}
	if sess.p.ProfileID == 0 {
		httpError(w, http.StatusForbidden, "publishing a template needs a profile")
		return
	}
	if msg := req.check(); msg != "" {
		httpError(w, http.StatusBadRequest, msg)
		return
	}
	f, ok := req.fields()
	if !ok {
		httpError(w, http.StatusBadRequest, "name, badge, colour, text, model and effort are required")
		return
	}
	var t *store.Template
	var err error
	if req.TemplateID != 0 {
		t, err = s.bus.Store.PublishVersion(r.Context(), sess.p.ProfileID, req.TemplateID, sess.p.Name, f)
	} else {
		title := f.Name
		if req.Title != "" {
			if title, ok = cleanText(req.Title, maxTemplateName, false); !ok {
				httpError(w, http.StatusBadRequest, fmt.Sprintf("title must be 1 to %d characters", maxTemplateName))
				return
			}
		}
		t, err = s.bus.Store.PublishTemplate(r.Context(), sess.p.ProfileID, sess.p.Name, title, f)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, "no such template")
	case errors.Is(err, store.ErrForbidden):
		httpError(w, http.StatusForbidden, "only the profile that published a template can add a version")
	case errors.Is(err, store.ErrLimit):
		httpError(w, http.StatusConflict, err.Error())
	case err != nil:
		s.serverError(w, "publish_template_failed", err)
	default:
		t.Mine = true
		s.log.Info("template_published", zap.Int64("template", t.ID), zap.Int("version", t.Version), zap.Int64("profile", sess.p.ProfileID))
		writeJSON(w, http.StatusCreated, t)
	}
}

// removeTemplate is the admin's: rooms keep their copies, shipped templates stay.
func (s *Server) removeTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("template"), 10, 64)
	if err != nil {
		httpError(w, http.StatusNotFound, "no such template")
		return
	}
	err = s.bus.Store.RemoveTemplate(s.auditing(r, ActionTemplateRemove, store.TargetTemplate, strconv.FormatInt(id, 10), "", nil), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, "no such template")
	case errors.Is(err, store.ErrPreset):
		httpError(w, http.StatusConflict, err.Error())
	case err != nil:
		s.serverError(w, "remove_template_failed", err)
	default:
		s.log.Info("template_removed", zap.Int64("template", id))
		w.WriteHeader(http.StatusNoContent)
	}
}

// personaDone answers a persona change: the persona, or the store error as its status. A change
// posted a line in the chat, which wakes every stream.
func (s *Server) personaDone(w http.ResponseWriter, sess session, p *store.RoomPersona, m *store.Message, err error, status int) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, "no such persona or template")
	case errors.Is(err, store.ErrNameTaken), errors.Is(err, store.ErrPersonaBusy), errors.Is(err, store.ErrLimit):
		httpError(w, http.StatusConflict, err.Error())
	case err != nil:
		s.serverError(w, "persona_change_failed", err)
	default:
		if m != nil {
			s.bus.Notify(sess.p.RoomID)
		}
		if p == nil {
			w.WriteHeader(status)
			return
		}
		if status == http.StatusAccepted {
			writeJSON(w, status, struct {
				*store.RoomPersona
				Held bool `json:"held"`
			}{p, true})
			return
		}
		writeJSON(w, status, p)
	}
}

// heldStatus answers a change the host must approve first with 202 and the persona as it still is
// (002-AC21).
func heldStatus(held bool) int {
	if held {
		return http.StatusAccepted
	}
	return http.StatusOK
}

func personaID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("persona"), 10, 64)
	return id
}

// addPersona copies a template into the room; any member may (002-AC14).
func (s *Server) addPersona(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		TemplateID int64 `json:"template_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	p, m, err := s.bus.Store.AddPersona(r.Context(), sess.p.RoomID, req.TemplateID, sess.p)
	s.personaDone(w, sess, p, m, err, http.StatusCreated)
}

// editPersona changes a room persona; any member may, and a running turn keeps the persona it
// started with (002-AC15).
func (s *Server) editPersona(w http.ResponseWriter, r *http.Request, sess session) {
	var req personaReq
	if !decode(w, r, &req) {
		return
	}
	if msg := req.check(); msg != "" {
		httpError(w, http.StatusBadRequest, msg)
		return
	}
	patch := store.PersonaPatch{Name: req.Name, Badge: req.Badge, Colour: req.Colour, Text: req.Text, Model: req.Model, Effort: req.Effort,
		EveryN: req.EveryN, EveryVoice: req.EveryVoice, EveryMinutes: req.EveryMinutes, SelfCall: req.SelfCall, Autocompact: req.Autocompact,
		WatchesNotes: req.WatchesNotes, Speaks: req.Speaks, Voice: req.Voice, VoiceRate: req.VoiceRate}
	p, m, held, err := s.bus.Store.EditPersona(r.Context(), sess.p.RoomID, personaID(r), patch, sess.p)
	s.personaDone(w, sess, p, m, err, heldStatus(held))
}

// updatePersona takes the template's newer version the room was offered (002-AC14).
func (s *Server) updatePersona(w http.ResponseWriter, r *http.Request, sess session) {
	p, m, held, err := s.bus.Store.UpdatePersona(r.Context(), sess.p.RoomID, personaID(r), sess.p)
	s.personaDone(w, sess, p, m, err, heldStatus(held))
}

// removePersona removes a room persona; any member may. Audited only when done with an API token
// (012-AC20): the room's line says who did it otherwise.
func (s *Server) removePersona(w http.ResponseWriter, r *http.Request, sess session) {
	ctx := r.Context()
	if apiTokenOf(r) != nil {
		ctx = s.roomAuditing(r, &sess, ActionPersonaRemove, sess.p.RoomID, nil)
	}
	m, err := s.bus.Store.RemovePersona(ctx, sess.p.RoomID, personaID(r), sess.p)
	s.personaDone(w, sess, nil, m, err, http.StatusNoContent)
}
