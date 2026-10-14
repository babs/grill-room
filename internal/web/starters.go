package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

// Audit actions of the starter library (006-AC5).
const (
	ActionStarterPublish = "starter.publish"
	ActionStarterDelete  = "starter.delete"
)

func starterID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("starter"), 10, 64)
	return id
}

// starterError answers a starter library refusal with its status; false when err is nil.
func (s *Server) starterError(w http.ResponseWriter, event string, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, "no such starter")
	case errors.Is(err, store.ErrBuiltin):
		httpError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, store.ErrForbidden):
		httpError(w, http.StatusForbidden, "only the starter's author can do this")
	case errors.Is(err, store.ErrLimit):
		httpError(w, http.StatusConflict, err.Error())
	default:
		s.serverError(w, event, err)
	}
	return true
}

// profileAudit returns r's context carrying the entry of a profile's action on starter id.
func (s *Server) profileAudit(r *http.Request, p *store.Profile, action string, id int64) context.Context {
	e := store.AuditEntry{At: s.now(), Actor: ActorProfile + p.UUID, Action: action, TargetKind: store.TargetStarter, TargetID: strconv.FormatInt(id, 10)}
	return store.WithAudit(r.Context(), e, nil)
}

// myStarters lists the starters a profile sees: the built-ins, its own and the instance's (006-AC1).
func (s *Server) myStarters(w http.ResponseWriter, r *http.Request) {
	p := s.profileFor(w, r)
	if p == nil {
		return
	}
	ls, err := s.bus.Store.Starters(r.Context(), p.ID)
	if s.starterError(w, "list_starters_failed", err) {
		return
	}
	writeJSON(w, http.StatusOK, ls)
}

// getStarter answers a starter, 0 for Blank, with the manifest of its version `v`, the latest by default, for
// the picker's preview and the editor (006-AC8).
func (s *Server) getStarter(w http.ResponseWriter, r *http.Request) {
	p := s.profileFor(w, r)
	if p == nil {
		return
	}
	if starterID(r) == 0 {
		m, err := s.bus.Store.BlankManifest(r.Context())
		if s.starterError(w, "get_starter_failed", err) {
			return
		}
		writeJSON(w, http.StatusOK, starterView{&store.Starter{Library: "blank", Title: m.Title, Description: m.Description}, m, turn.Models, turn.Efforts})
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("v"))
	st, m, err := s.bus.Store.StarterVersion(r.Context(), starterID(r), n, p.ID)
	if s.starterError(w, "get_starter_failed", err) {
		return
	}
	writeJSON(w, http.StatusOK, starterView{st, m, turn.Models, turn.Efforts})
}

// starterView is a starter version for the editor, with the models and efforts it offers.
type starterView struct {
	*store.Starter
	Manifest *store.StarterManifest `json:"manifest"`
	Models   []string               `json:"models"`
	Efforts  []string               `json:"efforts"`
}

// duplicateStarter copies a starter the profile sees into its personal library (006-AC2).
func (s *Server) duplicateStarter(w http.ResponseWriter, r *http.Request) {
	p := s.profileFor(w, r)
	if p == nil {
		return
	}
	st, err := s.bus.Store.DuplicateStarter(r.Context(), p.ID, starterID(r))
	if s.starterError(w, "duplicate_starter_failed", err) {
		return
	}
	s.log.Info("starter_duplicated", zap.Int64("starter", st.ID), zap.Int64("from", starterID(r)), zap.Int64("profile", p.ID))
	writeJSON(w, http.StatusCreated, st)
}

// publishStarter moves a personal starter to the instance library, audited (006-AC3, 006-AC5).
func (s *Server) publishStarter(w http.ResponseWriter, r *http.Request) {
	p := s.profileFor(w, r)
	if p == nil {
		return
	}
	id := starterID(r)
	if s.starterError(w, "publish_starter_failed", s.bus.Store.PublishStarter(s.profileAudit(r, p, ActionStarterPublish, id), p.ID, id)) {
		return
	}
	s.log.Info("starter_published", zap.Int64("starter", id), zap.Int64("profile", p.ID))
	w.WriteHeader(http.StatusNoContent)
}

// deleteMyStarter deletes a starter the profile owns, personal or published (006-AC3).
func (s *Server) deleteMyStarter(w http.ResponseWriter, r *http.Request) {
	p := s.profileFor(w, r)
	if p == nil {
		return
	}
	id := starterID(r)
	if s.starterError(w, "delete_starter_failed", s.bus.Store.DeleteStarter(s.profileAudit(r, p, ActionStarterDelete, id), p.ID, id)) {
		return
	}
	s.log.Info("starter_deleted", zap.Int64("starter", id), zap.Int64("profile", p.ID))
	w.WriteHeader(http.StatusNoContent)
}

// adminStarters lists the instance library for /admin Libraries (006-AC5).
func (s *Server) adminStarters(w http.ResponseWriter, r *http.Request) {
	ls, err := s.bus.Store.Starters(r.Context(), 0)
	if s.starterError(w, "list_starters_failed", err) {
		return
	}
	out := []store.Starter{}
	for _, st := range ls {
		if st.Library == store.LibraryInstance {
			out = append(out, st)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// adminDeleteStarter is the super-admin removing an instance starter, audited (006-AC5).
func (s *Server) adminDeleteStarter(w http.ResponseWriter, r *http.Request) {
	id := starterID(r)
	ctx := s.auditing(r, ActionStarterDelete, store.TargetStarter, strconv.FormatInt(id, 10), "", nil)
	if s.starterError(w, "delete_starter_failed", s.bus.Store.DeleteStarter(ctx, 0, id)) {
		return
	}
	s.log.Info("starter_deleted", zap.Int64("starter", id), zap.String("by", store.ActorSuperAdmin))
	w.WriteHeader(http.StatusNoContent)
}

const (
	maxFieldValue = 100
	maxAgenda     = 64 << 10
)

// fieldRe is a {{key}} in an agenda or a room name (006-AC10, 006-AC16).
var fieldRe = regexp.MustCompile(`\{\{([a-z0-9_]+)\}\}`)

// starterRoomReq is "Create room" from the editor (006-AC9): the starter version it started from
// (none for Blank), the fields' values, and what the editor changed. Documents and skills name
// what the room keeps of the starter's; nil keeps them all.
type starterRoomReq struct {
	Starter *struct {
		ID      int64 `json:"id"`
		Version int   `json:"version"`
	} `json:"starter"`
	Fields    map[string]string       `json:"fields"`
	Agenda    *string                 `json:"agenda"`
	Personas  *[]store.StarterPersona `json:"personas"`
	Documents []string                `json:"documents"`
	Skills    []string                `json:"skills"`
}

// badRequest is a refusal the editor shows as is.
type badRequest string

func (b badRequest) Error() string { return string(b) }

// starterSetup checks q against the starter version it names and returns what the new room gets,
// with name, its room name, once the fields are put in (006-AC9 to 006-AC13). A starter the profile
// does not see is store.ErrNotFound; anything else wrong is a badRequest.
func (s *Server) starterSetup(ctx context.Context, profileID int64, name string, q *starterRoomReq) (string, *store.RoomSetup, error) {
	var m *store.StarterManifest
	var files map[string][]byte
	setup := &store.RoomSetup{UploadQuota: s.opts.MaxRoomBytes}
	if q.Starter == nil {
		var err error
		if m, err = s.bus.Store.BlankManifest(ctx); err != nil {
			return "", nil, err
		}
	} else {
		st, man, err := s.bus.Store.StarterVersion(ctx, q.Starter.ID, q.Starter.Version, profileID)
		if err != nil {
			return "", nil, err
		}
		if files, err = s.bus.Store.StarterFiles(ctx, st.ID, st.Version); err != nil {
			return "", nil, err
		}
		m, setup.From, setup.SkillsPending = man, &store.Provenance{Title: man.Title, Version: st.Version}, st.Library != store.LibraryBuiltin
	}
	values := map[string]string{}
	for _, f := range m.Fields {
		v := strings.TrimSpace(q.Fields[f.Key])
		if v != "" && !store.ValidText(v, maxFieldValue, false) {
			return "", nil, badRequest(fmt.Sprintf("%s must be at most %d characters on one line", f.Label, maxFieldValue))
		}
		if v == "" && f.Required {
			return "", nil, badRequest("fill in " + f.Label)
		}
		values[f.Key] = v
	}
	// An empty optional field becomes an empty string; an undeclared {{x}} stays as typed.
	fill := func(text string) string {
		return fieldRe.ReplaceAllStringFunc(text, func(k string) string {
			if v, ok := values[fieldRe.FindStringSubmatch(k)[1]]; ok {
				return v
			}
			return k
		})
	}
	room, ok := cleanText(strings.Join(strings.Fields(fill(name)), " "), maxRoomName, false)
	if !ok {
		return "", nil, badRequest(fmt.Sprintf("name must be 1 to %d characters", maxRoomName))
	}
	agenda := m.Agenda
	if q.Agenda != nil {
		agenda = *q.Agenda
	}
	agenda = strings.TrimSpace(strings.ReplaceAll(fill(agenda), "\r\n", "\n"))
	if len(agenda) > maxAgenda || (agenda != "" && !store.ValidText(agenda, maxAgenda, true)) {
		return "", nil, badRequest(fmt.Sprintf("the agenda must be at most %d KB of text", maxAgenda>>10))
	}
	setup.Agenda, setup.Chain, setup.Glossary, setup.Files = agenda, m.Chain, m.Glossary, files
	var err error
	if setup.Documents, err = keep(m.Documents, q.Documents, func(d store.StarterDocument) string { return d.Name }); err != nil {
		return "", nil, err
	}
	if agenda != "" && slices.ContainsFunc(setup.Documents, func(d store.StarterDocument) bool { return d.Name == store.AgendaName }) {
		return "", nil, badRequest("a document is named " + store.AgendaName + " already: leave it out, or empty the agenda")
	}
	if setup.Skills, err = keep(m.Skills, q.Skills, func(k store.StarterSkill) string { return k.Name }); err != nil {
		return "", nil, err
	}
	personas := m.Personas
	if q.Personas != nil {
		personas = *q.Personas
	}
	if len(personas) > store.MaxRoomPersonas {
		return "", nil, badRequest(fmt.Sprintf("a room holds at most %d personas", store.MaxRoomPersonas))
	}
	for i := range personas {
		p, err := cleanPersona(personas[i])
		if err != nil {
			return "", nil, badRequest("persona " + personas[i].Name + ": " + err.Error())
		}
		if slices.ContainsFunc(personas[:i], func(o store.StarterPersona) bool { return strings.EqualFold(o.Name, p.Name) }) {
			return "", nil, badRequest("two personas are named " + p.Name)
		}
		for _, k := range p.Skills {
			if !slices.ContainsFunc(setup.Skills, func(x store.StarterSkill) bool { return x.Name == k }) {
				return "", nil, badRequest(p.Name + " uses the skill " + k + ": keep it, or turn it off for " + p.Name)
			}
		}
		personas[i] = p
	}
	if err := oneWatcher(personas); err != nil {
		return "", nil, err
	}
	setup.Personas = personas
	return room, setup, nil
}

// oneWatcher refuses personas of which more than one watches the notes (009-AC31).
func oneWatcher(ps []store.StarterPersona) error {
	if len(slices.DeleteFunc(slices.Clone(ps), func(p store.StarterPersona) bool { return !p.WatchesNotes })) > 1 {
		return badRequest("only one persona may watch the notes")
	}
	return nil
}

// keep returns the entries of all whose name is in names, all of them for nil names; a name of no
// entry is a badRequest.
func keep[T any](all []T, names []string, name func(T) string) ([]T, error) {
	if names == nil {
		return all, nil
	}
	// Empty, not nil: a manifest lists none as [], which the pages read as a list.
	out := []T{}
	for j, n := range names {
		if slices.Contains(names[:j], n) {
			return nil, badRequest(n + " is named twice")
		}
		i := slices.IndexFunc(all, func(x T) bool { return name(x) == n })
		if i < 0 {
			return nil, badRequest("the starter has no " + n)
		}
		out = append(out, all[i])
	}
	return out, nil
}

const (
	maxStarterDesc = 300
	maxFields      = 10
	maxFieldLabel  = 40
	maxFieldHint   = 100
)

// fieldKeyRe is a field's key, which the editor derives from its label (006-AC16).
var fieldKeyRe = regexp.MustCompile(`^[a-z0-9_]{1,30}$`)

// starterSaveReq is a starter from the editor (006-AC15, 006-AC16): what it holds of its source,
// a room or a version, the names of the documents and skills to keep included.
type starterSaveReq struct {
	Title       string                 `json:"title"`
	Description string                 `json:"description"`
	RoomName    string                 `json:"room_name"`
	Agenda      string                 `json:"agenda"`
	Fields      []store.StarterField   `json:"fields"`
	Personas    []store.StarterPersona `json:"personas"`
	Documents   []string               `json:"documents"`
	Skills      []string               `json:"skills"`
}

// starterFrom checks q against base, the room or version it is saved from, and returns the
// manifest to store with the names of the blobs it keeps. An undeclared {{x}} in the agenda or
// the room name is refused, naming it; a field used nowhere is only the editor's warning.
func starterFrom(base *store.StarterManifest, q *starterSaveReq) (*store.StarterManifest, []string, error) {
	m := &store.StarterManifest{Chain: base.Chain, Glossary: base.Glossary}
	var ok bool
	if m.Title, ok = cleanText(q.Title, store.MaxStarterTitle, false); !ok {
		return nil, nil, badRequest(fmt.Sprintf("the title must be 1 to %d characters", store.MaxStarterTitle))
	}
	if q.Description = strings.TrimSpace(q.Description); q.Description != "" && !store.ValidText(q.Description, maxStarterDesc, true) {
		return nil, nil, badRequest(fmt.Sprintf("the description must be at most %d characters", maxStarterDesc))
	}
	m.Description = q.Description
	if m.RoomName, ok = cleanText(q.RoomName, 2*maxRoomName, false); !ok {
		return nil, nil, badRequest(fmt.Sprintf("the room name must be 1 to %d characters", 2*maxRoomName))
	}
	m.Agenda = strings.TrimSpace(strings.ReplaceAll(q.Agenda, "\r\n", "\n"))
	if len(m.Agenda) > maxAgenda || (m.Agenda != "" && !store.ValidText(m.Agenda, maxAgenda, true)) {
		return nil, nil, badRequest(fmt.Sprintf("the agenda must be at most %d KB of text", maxAgenda>>10))
	}
	if len(q.Fields) > maxFields {
		return nil, nil, badRequest(fmt.Sprintf("a starter asks at most %d fields", maxFields))
	}
	m.Fields = []store.StarterField{}
	for _, f := range q.Fields {
		label, ok := cleanText(f.Label, maxFieldLabel, false)
		if !ok {
			return nil, nil, badRequest(fmt.Sprintf("a field's label must be 1 to %d characters", maxFieldLabel))
		}
		if !fieldKeyRe.MatchString(f.Key) || slices.ContainsFunc(m.Fields, func(o store.StarterField) bool { return o.Key == f.Key }) {
			return nil, nil, badRequest("field " + label + ": its key must be unique, lowercase letters, digits or _")
		}
		hint := strings.TrimSpace(f.Hint)
		if hint != "" && !store.ValidText(hint, maxFieldHint, false) {
			return nil, nil, badRequest(fmt.Sprintf("field %s: its hint must be at most %d characters on one line", label, maxFieldHint))
		}
		m.Fields = append(m.Fields, store.StarterField{Label: label, Key: f.Key, Required: f.Required, Hint: hint})
	}
	for where, text := range map[string]string{"the room name": m.RoomName, "the agenda": m.Agenda} {
		for _, k := range fieldRe.FindAllStringSubmatch(text, -1) {
			if !slices.ContainsFunc(m.Fields, func(f store.StarterField) bool { return f.Key == k[1] }) {
				return nil, nil, badRequest("{{" + k[1] + "}} in " + where + " is no field: declare it, or remove it")
			}
		}
	}
	var err error
	if m.Documents, err = keep(base.Documents, q.Documents, func(d store.StarterDocument) string { return d.Name }); err != nil {
		return nil, nil, err
	}
	if m.Agenda != "" && slices.ContainsFunc(m.Documents, func(d store.StarterDocument) bool { return d.Name == store.AgendaName }) {
		return nil, nil, badRequest("a document is named " + store.AgendaName + " already: leave it out, or empty the agenda")
	}
	if m.Skills, err = keep(base.Skills, q.Skills, func(k store.StarterSkill) string { return k.Name }); err != nil {
		return nil, nil, err
	}
	if len(q.Personas) > store.MaxRoomPersonas {
		return nil, nil, badRequest(fmt.Sprintf("a room holds at most %d personas", store.MaxRoomPersonas))
	}
	m.Personas = []store.StarterPersona{}
	for _, p := range q.Personas {
		p, err := cleanPersona(p)
		if err != nil {
			return nil, nil, badRequest("persona " + p.Name + ": " + err.Error())
		}
		if slices.ContainsFunc(m.Personas, func(o store.StarterPersona) bool { return strings.EqualFold(o.Name, p.Name) }) {
			return nil, nil, badRequest("two personas are named " + p.Name)
		}
		for _, k := range p.Skills {
			if !slices.ContainsFunc(m.Skills, func(x store.StarterSkill) bool { return x.Name == k }) {
				return nil, nil, badRequest(p.Name + " uses the skill " + k + ": keep it, or turn it off for " + p.Name)
			}
		}
		m.Personas = append(m.Personas, p)
	}
	if err := oneWatcher(m.Personas); err != nil {
		return nil, nil, err
	}
	var kept []string
	for _, d := range m.Documents {
		if d.File != "" {
			kept = append(kept, d.File)
		}
	}
	for _, k := range m.Skills {
		kept = append(kept, k.File)
	}
	return m, kept, nil
}

// saved answers a starter save: the starter, or the refusal.
func (s *Server) saved(w http.ResponseWriter, st *store.Starter, err error, from string) {
	var bad badRequest
	switch {
	case errors.As(err, &bad):
		httpError(w, http.StatusBadRequest, bad.Error())
	case s.starterError(w, "save_starter_failed", err):
	default:
		s.log.Info("starter_saved", zap.Int64("starter", st.ID), zap.Int("version", st.Version), zap.String("from", from))
		writeJSON(w, http.StatusCreated, st)
	}
}

// starterDraft is the room as "Save as starter" opens it, for a room admin (006-AC15).
func (s *Server) starterDraft(w http.ResponseWriter, r *http.Request, sess session) {
	if !s.asRoomAdmin(w, r, &sess) {
		return
	}
	m, err := s.bus.Store.RoomDraft(r.Context(), sess.p.RoomID)
	if err != nil {
		s.serverError(w, "starter_draft_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, starterView{&store.Starter{Library: "room", Title: m.Title}, m, turn.Models, turn.Efforts})
}

// saveRoomStarter saves the room, as the editor left it, as a new starter in the room admin's
// personal library (006-AC15 to 006-AC17).
func (s *Server) saveRoomStarter(w http.ResponseWriter, r *http.Request, sess session) {
	var q starterSaveReq
	if !decode(w, r, &q) || !s.asRoomAdmin(w, r, &sess) {
		return
	}
	if sess.p.ProfileID == 0 {
		httpError(w, http.StatusForbidden, "saving a starter needs a profile")
		return
	}
	base, err := s.bus.Store.RoomDraft(r.Context(), sess.p.RoomID)
	if err != nil {
		s.serverError(w, "save_starter_failed", err)
		return
	}
	// Only the blobs kept are read: a room's uploads may weigh its whole quota.
	m, names, err := starterFrom(base, &q)
	var files map[string][]byte
	if err == nil {
		files, err = s.bus.Store.RoomDraftFiles(r.Context(), sess.p.RoomID, names)
		if errors.Is(err, store.ErrNotFound) {
			httpError(w, http.StatusConflict, "a document or skill left the room meanwhile: open Save as starter again")
			return
		}
	}
	var st *store.Starter
	if err == nil {
		st, err = s.bus.Store.SaveStarter(r.Context(), sess.p.ProfileID, 0, m, files)
	}
	s.saved(w, st, err, "room")
}

// saveStarterVersion saves the editor's changes to one of the profile's starters as its next
// version (006-AC3); documents and skills come from the version it was opened on, `version`.
func (s *Server) saveStarterVersion(w http.ResponseWriter, r *http.Request) {
	p := s.profileFor(w, r)
	if p == nil {
		return
	}
	var q struct {
		starterSaveReq
		Version int `json:"version"`
	}
	if !decode(w, r, &q) {
		return
	}
	id := starterID(r)
	st, base, err := s.bus.Store.StarterVersion(r.Context(), id, q.Version, p.ID)
	if s.starterError(w, "save_starter_failed", err) {
		return
	}
	if !st.Mine {
		httpError(w, http.StatusForbidden, "only the starter's author can do this")
		return
	}
	// The editor saves on top of the version it opened: another tab may have saved since.
	if q.Version != 0 && q.Version != st.Versions {
		httpError(w, http.StatusConflict, fmt.Sprintf("version %d exists: reopen the starter to edit it", st.Versions))
		return
	}
	m, names, err := starterFrom(base, &q.starterSaveReq)
	var files map[string][]byte
	if err == nil {
		files, err = s.bus.Store.StarterFiles(r.Context(), id, st.Version)
	}
	kept := map[string][]byte{}
	for _, n := range names {
		kept[n] = files[n]
	}
	var saved *store.Starter
	if err == nil {
		saved, err = s.bus.Store.SaveStarter(r.Context(), p.ID, id, m, kept)
	}
	s.saved(w, saved, err, "version")
}
