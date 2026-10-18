package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrNameTaken is returned when a room already has a persona of that name, whatever its case.
	ErrNameTaken = errors.New("the room already has a persona with that name")
	// ErrPersonaBusy is returned when a persona is removed while its turn runs.
	ErrPersonaBusy = errors.New("the persona's turn is running")
	// ErrPreset is returned when the admin removes a shipped template.
	ErrPreset = errors.New("shipped templates cannot be removed")
	// ErrAlreadyQueued is returned when a persona already has a turn waiting in the queue.
	ErrAlreadyQueued = errors.New("this persona is already queued")
	// ErrLimit is returned past one of the limits below.
	ErrLimit = errors.New("limit reached")
)

// Every member may add personas and every profile may publish: these bound what one of them can
// make the room stream to everyone, or the library hold.
const (
	MaxRoomPersonas     = 12
	MaxProfileTemplates = 50
	MaxTemplateVersions = 100
)

// overLimit returns an ErrLimit saying what is full when query counts limit rows or more.
func overLimit(ctx context.Context, tx *txn, limit int, what, query string, args ...any) error {
	var n int
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return fmt.Errorf("count %s: %w", what, err)
	}
	if n >= limit {
		return fmt.Errorf("%w: at most %d %s", ErrLimit, limit, what)
	}
	return nil
}

// PresetDefault is the shipped template every room starts with.
const PresetDefault = "default"

// DefaultModel and DefaultEffort are what the shipped templates ask for, and what a persona from
// before 005-AC9, which left them to the host, asks for once migrated or imported.
const (
	DefaultModel  = "sonnet"
	DefaultEffort = "medium"
)

// presetAuthor signs the shipped template versions.
const presetAuthor = "grill-room"

// PersonaFields are a persona: who it is (name, badge, colour) and how it works (text, model,
// effort). A template version holds them as published, a room persona as the room set them.
type PersonaFields struct {
	Name   string `json:"name"`
	Badge  string `json:"badge"`
	Colour string `json:"colour"`
	// Text is the persona's system prompt.
	Text string `json:"text"`
	// Model and Effort are what the persona asks for, never empty (005-AC9).
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

func (f PersonaFields) args() []any {
	return []any{f.Name, f.Badge, f.Colour, f.Text, f.Model, f.Effort}
}

const grillText = `You are Grill, the challenger of this grill session. The people in the room bring a plan; your job is to find where it breaks before reality does.

- Ask the questions they would rather avoid: missing pieces, unstated assumptions, risks, costs, vague words, and gaps between what was said and what is written.
- Push on one or two points per turn, the ones that matter most, and say why they matter.
- When an answer settles a point, accept it and move on. Never ask again what was answered.
- Keep a synthesis document in docs/ up to date: decisions, open questions, risks, and what was ruled out and why.
- Be direct and brief. No flattery, no filler.

Answer in the language the room speaks. If it mixes languages, use the one of the latest messages.`

const secretaryText = `You are Secretary, the note taker of this session. You record; you never challenge, judge or suggest.

- Keep docs/minutes.md up to date with four sections: Summary (a few lines), Decisions, Actions (who, what, and the date if one was said), Open questions.
- Write only what was said or decided in the room. Mark anything unclear "(to confirm)".
- Name the person behind each decision and each action.
- Your synthesis says in a few lines what changed in the minutes since your last turn.

Write in the language the room speaks. If it mixes languages, use the one of the latest messages.`

// presets are the shipped templates. Changing one here publishes a new version at the next start.
// Their texts carry no trailing newline: the room trims persona texts, so one would read as an edit.
var presets = []struct {
	key, title string
	f          PersonaFields
}{
	{PresetDefault, "Default", PersonaFields{Name: "Grill", Badge: "🔥", Colour: "#d9480f", Text: grillText, Model: DefaultModel, Effort: DefaultEffort}},
	{"secretary", "Secretary", PersonaFields{Name: "Secretary", Badge: "📝", Colour: "#1971c2", Text: secretaryText, Model: DefaultModel, Effort: DefaultEffort}},
}

// ensurePresets creates the shipped templates and adds a version to any whose shipped fields
// changed since its latest version.
func ensurePresets(ctx context.Context, db querier) error {
	for _, p := range presets {
		if _, err := db.ExecContext(ctx, `INSERT INTO persona_templates(preset, title, created_at) VALUES(?,?,?) ON CONFLICT(preset) DO NOTHING`,
			p.key, p.title, now()); err != nil {
			return fmt.Errorf("preset %s: %w", p.key, err)
		}
		var id int64
		var n int
		var f PersonaFields
		err := db.QueryRowContext(ctx, `
			SELECT t.id, COALESCE(v.n, 0), COALESCE(v.name, ''), COALESCE(v.badge, ''), COALESCE(v.colour, ''), COALESCE(v.text, ''),
				COALESCE(v.model, ''), COALESCE(v.effort, '')
			FROM persona_templates t LEFT JOIN persona_template_versions v ON v.template_id = t.id
			WHERE t.preset = ? ORDER BY v.n DESC LIMIT 1`, p.key).Scan(&id, &n, &f.Name, &f.Badge, &f.Colour, &f.Text, &f.Model, &f.Effort)
		if err != nil {
			return fmt.Errorf("preset %s: %w", p.key, err)
		}
		if n > 0 && f == p.f {
			continue
		}
		if err := insertVersion(ctx, db, id, n+1, presetAuthor, p.f); err != nil {
			return fmt.Errorf("preset %s: %w", p.key, err)
		}
	}
	return nil
}

func insertVersion(ctx context.Context, db execer, templateID int64, n int, author string, f PersonaFields) error {
	_, err := db.ExecContext(ctx, `INSERT INTO persona_template_versions(template_id, n, author, name, badge, colour, text, model, effort, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, append(append([]any{templateID, n, author}, f.args()...), now())...)
	return err
}

// Template is a template of the server-wide library, as its latest version has it.
type Template struct {
	ID int64 `json:"id"`
	// Preset names a shipped template; empty for one a profile published.
	Preset    string `json:"preset,omitempty"`
	ProfileID int64  `json:"-"`
	// Mine tells the profile asking that it published the template, so it may add versions.
	Mine    bool   `json:"mine"`
	Title   string `json:"title"`
	Version int    `json:"version"`
	// Author is who published the latest version: a display name, or grill-room for presets.
	Author string `json:"author"`
	PersonaFields
	CreatedAt time.Time `json:"created_at"`
}

const templateCols = `SELECT t.id, COALESCE(t.preset, ''), COALESCE(t.profile_id, 0), t.title, v.n, v.author,
	v.name, v.badge, v.colour, v.text, v.model, v.effort, v.created_at
	FROM persona_templates t JOIN persona_template_versions v ON v.template_id = t.id
	AND v.n = (SELECT MAX(n) FROM persona_template_versions WHERE template_id = t.id) `

func scanTemplate(sc scanner) (*Template, error) {
	var t Template
	var created string
	err := sc.Scan(&t.ID, &t.Preset, &t.ProfileID, &t.Title, &t.Version, &t.Author,
		&t.Name, &t.Badge, &t.Colour, &t.Text, &t.Model, &t.Effort, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan template: %w", err)
	}
	t.CreatedAt = parseTS(created)
	return &t, nil
}

// Templates lists the library, presets first; Mine is set on those profileID published.
func (s *Store) Templates(ctx context.Context, profileID int64) ([]Template, error) {
	rows, err := s.db.QueryContext(ctx, templateCols+`ORDER BY t.preset IS NULL, t.id`)
	if err != nil {
		return nil, fmt.Errorf("templates: %w", err)
	}
	defer rows.Close()
	out := []Template{}
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		t.Mine = profileID != 0 && t.ProfileID == profileID
		out = append(out, *t)
	}
	return out, rows.Err()
}

// Template returns template id at its latest version.
func (s *Store) Template(ctx context.Context, id int64) (*Template, error) {
	return scanTemplate(s.db.QueryRowContext(ctx, templateCols+`WHERE t.id = ?`, id))
}

// PublishTemplate adds a template owned by profileID, with f as its version 1 signed author.
func (s *Store) PublishTemplate(ctx context.Context, profileID int64, author, title string, f PersonaFields) (*Template, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("publish template: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if err := overLimit(ctx, tx, MaxProfileTemplates, "templates per profile", `SELECT count(*) FROM persona_templates WHERE profile_id = ?`, profileID); err != nil {
		return nil, err
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO persona_templates(profile_id, title, created_at) VALUES(?,?,?) RETURNING id`,
		profileID, title, now()).Scan(&id); err != nil {
		return nil, fmt.Errorf("publish template: %w", err)
	}
	if err := insertVersion(ctx, tx, id, 1, author, f); err != nil {
		return nil, fmt.Errorf("publish template: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("publish template: %w", err)
	}
	return s.Template(ctx, id)
}

// PublishVersion adds f as the next version of template id, which only its owner may do:
// ErrForbidden otherwise, presets included. Earlier versions never change.
func (s *Store) PublishVersion(ctx context.Context, profileID, id int64, author string, f PersonaFields) (*Template, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("publish version: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var owner int64
	var n int
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(t.profile_id, 0), (SELECT MAX(n) FROM persona_template_versions WHERE template_id = t.id)
		FROM persona_templates t WHERE t.id = ?`, id).Scan(&owner, &n)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("publish version: %w", err)
	}
	if owner == 0 || owner != profileID {
		return nil, ErrForbidden
	}
	if n >= MaxTemplateVersions {
		return nil, fmt.Errorf("%w: at most %d versions per template", ErrLimit, MaxTemplateVersions)
	}
	if err := insertVersion(ctx, tx, id, n+1, author, f); err != nil {
		return nil, fmt.Errorf("publish version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("publish version: %w", err)
	}
	return s.Template(ctx, id)
}

// RemoveTemplate deletes template id with its versions. Room personas made from it keep their
// copy. ErrPreset for a shipped template.
func (s *Store) RemoveTemplate(ctx context.Context, id int64) error {
	return s.withTx(ctx, "remove template", func(tx *txn) error {
		var preset sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT preset FROM persona_templates WHERE id = ?`, id).Scan(&preset)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("remove template %d: %w", id, err)
		}
		if preset.Valid {
			return ErrPreset
		}
		if err := auditTx(ctx, tx, "", nil); err != nil {
			return err
		}
		return execOne(ctx, tx, `DELETE FROM persona_templates WHERE id = ?`, id)
	})
}

// RoomPersona is a room's copy of a template, with the room's own changes.
type RoomPersona struct {
	ID         int64  `json:"id"`
	RoomID     string `json:"-"`
	TemplateID int64  `json:"template_id,omitempty"`
	// TemplateVersion is the version the copy was made from or last updated to.
	TemplateVersion int `json:"template_version"`
	// UpdateVersion is the template's latest version when newer than the copy: offered to the room,
	// never applied by itself (002-AC14).
	UpdateVersion int `json:"update_version,omitempty"`
	PersonaFields
	// EveryN runs the persona after that many completed turns of any persona since its own last
	// one, 0 for off (002-AC22). SelfCall lets its synthesis queue itself with @name (002-AC24).
	EveryN   int  `json:"every_n,omitempty"`
	SelfCall bool `json:"self_call"`
	// EveryVoice runs it after that many voice lines since its last turn, EveryMinutes that long
	// after it, once the room had activity and someone is here; at most one of the three is set
	// (006-AC18). NextVoice is the voice lines left before it, NextAt when the timer is due.
	EveryVoice   int        `json:"every_voice,omitempty"`
	EveryMinutes int        `json:"every_minutes,omitempty"`
	NextVoice    int        `json:"next_voice,omitempty"`
	NextAt       *time.Time `json:"next_at,omitempty"`
	// resetMark starts the count and the timer from now: a voice or timer trigger was turned on.
	resetMark bool
	// WatchesNotes makes it the room's notes watcher, one at most (009-AC31). NotesMark is the
	// last note message its latest turn read: newer ones may wake it (009-AC32).
	WatchesNotes bool  `json:"watches_notes,omitempty"`
	NotesMark    int64 `json:"-"`
	// watchFrom is the label of the persona that watched the notes before this one took over.
	watchFrom string
	// Autocompact is the context size, in tokens, at which Claude Code compacts the persona's
	// session; 0 leaves it to Claude Code (auto). Passed to claude -p --autocompact (003-AC24).
	Autocompact int `json:"autocompact"`
	// ContextTokens is what its current session's last request sent, against the model's
	// ContextWindow; 0 when unknown (003-AC24).
	ContextTokens int64 `json:"context_tokens,omitempty"`
	ContextWindow int64 `json:"context_window,omitempty"`
	// Pending is a persona added while the host approves everything: it cannot run until the host
	// approves it (002-AC21).
	Pending bool `json:"pending,omitempty"`
	// Cap is the host's last decision on this persona asking above the cap (002-AC20).
	Cap CapDecision `json:"-"`
	// Skills are its active skills, by name, each pinned at a version (002-AC31).
	Skills    []PersonaSkill `json:"skills"`
	CreatedAt time.Time      `json:"created_at"`
	// Speaks puts the persona's spoken line in the audio call (013-AC26). Voice is its catalogue
	// voice, empty until it first speaks (013-AC16); VoiceRate its speaking rate (013-AC37).
	Speaks    bool    `json:"speaks"`
	Voice     string  `json:"voice"`
	VoiceRate float64 `json:"voice_rate"`
}

const personaCols = `SELECT p.id, p.room_id, COALESCE(p.template_id, 0), p.template_version,
	COALESCE((SELECT MAX(n) FROM persona_template_versions WHERE template_id = p.template_id), 0),
	p.name, p.badge, p.colour, p.text, p.model, p.effort, COALESCE(p.every_n, 0), p.self_call, p.created_at,
	p.pending, p.cap_decision, COALESCE(p.cap_daemon_id, 0), p.cap_model, p.cap_effort, p.autocompact,
	COALESCE(s.context_tokens, 0), COALESCE(s.context_window, 0), COALESCE(p.every_voice, 0), COALESCE(p.every_minutes, 0), p.mark_at_ms,
	CASE WHEN p.every_voice IS NULL THEN 0 ELSE (SELECT COUNT(*) FROM messages m WHERE m.room_id = p.room_id AND m.kind = 'voice'
		AND m.deleted_at IS NULL AND m.id > p.mark_msg_id) END, p.watches_notes, p.notes_mark, p.speaks, p.voice, p.voice_rate FROM room_personas p
	LEFT JOIN agent_sessions s ON s.persona_id = p.id
		AND s.session_id = (SELECT session_id FROM turns WHERE persona_id = p.id AND status = 'done' ORDER BY id DESC LIMIT 1) `

func scanPersona(sc scanner) (*RoomPersona, error) {
	var p RoomPersona
	var created string
	var markMS int64
	var voices int
	err := sc.Scan(&p.ID, &p.RoomID, &p.TemplateID, &p.TemplateVersion, &p.UpdateVersion,
		&p.Name, &p.Badge, &p.Colour, &p.Text, &p.Model, &p.Effort, &p.EveryN, &p.SelfCall, &created,
		&p.Pending, &p.Cap.Status, &p.Cap.DaemonID, &p.Cap.Level.Model, &p.Cap.Level.Effort, &p.Autocompact,
		&p.ContextTokens, &p.ContextWindow, &p.EveryVoice, &p.EveryMinutes, &markMS, &voices, &p.WatchesNotes, &p.NotesMark, &p.Speaks, &p.Voice, &p.VoiceRate)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan persona: %w", err)
	}
	if p.UpdateVersion <= p.TemplateVersion {
		p.UpdateVersion = 0
	}
	if p.EveryVoice > 0 {
		p.NextVoice = max(p.EveryVoice-voices, 0)
	}
	if p.EveryMinutes > 0 {
		at := time.UnixMilli(markMS + int64(p.EveryMinutes)*60_000).UTC()
		p.NextAt = &at
	}
	p.CreatedAt = parseTS(created)
	return &p, nil
}

// RoomPersonas lists the room's personas in the order they were added.
func (s *Store) RoomPersonas(ctx context.Context, roomID string) ([]RoomPersona, error) {
	rows, err := s.db.QueryContext(ctx, personaCols+`WHERE p.room_id = ? ORDER BY p.id`, roomID)
	if err != nil {
		return nil, fmt.Errorf("room personas: %w", err)
	}
	defer rows.Close()
	out := []RoomPersona{}
	for rows.Next() {
		p, err := scanPersona(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("room personas: %w", err)
	}
	_ = rows.Close()
	return out, loadSkills(ctx, s.db, roomID, out)
}

// RoomPersona returns persona id of room.
func (s *Store) RoomPersona(ctx context.Context, roomID string, id int64) (*RoomPersona, error) {
	return withSkills(ctx, s.db, s.db.QueryRowContext(ctx, personaCols+`WHERE p.id = ? AND p.room_id = ?`, id, roomID), roomID)
}

func roomPersona(ctx context.Context, tx *txn, roomID string, id int64) (*RoomPersona, error) {
	return withSkills(ctx, tx, tx.QueryRowContext(ctx, personaCols+`WHERE p.id = ? AND p.room_id = ?`, id, roomID), roomID)
}

// withSkills scans one persona with its active skills.
func withSkills(ctx context.Context, db rowsQuerier, row *row, roomID string) (*RoomPersona, error) {
	p, err := scanPersona(row)
	if err != nil {
		return nil, err
	}
	ps := []RoomPersona{*p}
	if err := loadSkills(ctx, db, roomID, ps); err != nil {
		return nil, err
	}
	return &ps[0], nil
}

// freeName is name, or name-2, name-3… when the room already uses it.
func freeName(ctx context.Context, tx *txn, roomID, name string) (string, error) {
	for i := 1; ; i++ {
		try := name
		if i > 1 {
			try = name + "-" + strconv.Itoa(i)
		}
		var taken bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM room_personas WHERE room_id = ? AND lower(name) = lower(?))`,
			roomID, try).Scan(&taken); err != nil {
			return "", fmt.Errorf("persona name: %w", err)
		}
		if !taken {
			return try, nil
		}
	}
}

// addPersona copies the latest version of template templateID into room, under a free name. It
// returns the persona and the template's title.
func addPersona(ctx context.Context, tx *txn, roomID string, templateID int64) (*RoomPersona, string, error) {
	t, err := scanTemplate(tx.QueryRowContext(ctx, templateCols+`WHERE t.id = ?`, templateID))
	if err != nil {
		return nil, "", err
	}
	if t.Name, err = freeName(ctx, tx, roomID, t.Name); err != nil {
		return nil, "", err
	}
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO room_personas(room_id, template_id, template_version, name, badge, colour, text, model, effort, created_at,
			mark_msg_id, mark_at_ms)
		SELECT id, ?, ?, ?, ?, ?, ?, ?, ?, ?, `+markNow+` FROM rooms WHERE id = ? RETURNING id`,
		append(append([]any{t.ID, t.Version}, t.PersonaFields.args()...), now(), time.Now().UnixMilli(), roomID)...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("add persona: %w", err)
	}
	p, err := roomPersona(ctx, tx, roomID, id)
	return p, t.Title, err
}

func presetID(ctx context.Context, tx *txn, key string) (int64, error) {
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM persona_templates WHERE preset = ?`, key).Scan(&id); err != nil {
		return 0, fmt.Errorf("preset %s: %w", key, err)
	}
	return id, nil
}

// markNow is a new persona's trigger mark: the latest message, and the time its ? receives in ms.
const markNow = `(SELECT COALESCE(MAX(id), 0) FROM messages), ?`

// memberLine records a member's change (a persona, the chain) in the room, signed by them.
func memberLine(ctx context.Context, tx *txn, roomID string, by *Participant, body string) (*Message, error) {
	m := &Message{RoomID: roomID, Kind: KindSystem, ParticipantID: by.ID, Sender: by.Name, Body: by.Name + " " + body}
	return m, insertMessage(ctx, tx, m)
}

func (p *RoomPersona) label() string { return p.Badge + " " + p.Name }

// withTx runs fn in a transaction, committed when fn returns no error.
func (s *Store) withTx(ctx context.Context, what string, fn func(tx *txn) error) error {
	var fnErr error
	err := s.db.locked(ctx, func(tx *txn) error { fnErr = fn(tx); return fnErr })
	if err != nil && fnErr == nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return err
}

// AddPersona adds a copy of template templateID's latest version to room, recorded in the chat.
// A name the room uses already gets a number. While the host approves everything, the persona waits
// for the host, Pending (002-AC21). ErrNotFound for an unknown room or template.
func (s *Store) AddPersona(ctx context.Context, roomID string, templateID int64, by *Participant) (*RoomPersona, *Message, error) {
	var p *RoomPersona
	var m *Message
	err := s.withTx(ctx, "add persona", func(tx *txn) error {
		var title string
		err := overLimit(ctx, tx, MaxRoomPersonas, "personas per room", `SELECT count(*) FROM room_personas WHERE room_id = ?`, roomID)
		if err != nil {
			return err
		}
		if p, title, err = addPersona(ctx, tx, roomID, templateID); err != nil {
			return err
		}
		body := fmt.Sprintf("added the persona %s, from the template %q v%d.", p.label(), title, p.TemplateVersion)
		held, err := heldBy(ctx, tx, roomID, by)
		if err != nil {
			return err
		}
		if held {
			if _, err := tx.ExecContext(ctx, `UPDATE room_personas SET pending = 1 WHERE id = ?`, p.ID); err != nil {
				return fmt.Errorf("add persona: %w", err)
			}
			if err := holdChange(ctx, tx, roomID, p, ChangeAdd, PersonaPatch{}, by); err != nil {
				return err
			}
			p.Pending, body = true, body+waitsNote
		}
		m, err = memberLine(ctx, tx, roomID, by, body)
		return err
	})
	return p, m, err
}

// PersonaPatch is a change to a room persona; nil fields stay as they are.
type PersonaPatch struct {
	Name, Badge, Colour, Text, Model, Effort *string
	// EveryN, EveryVoice and EveryMinutes 0 turn their trigger off; the room API sends the other two
	// as 0 when it sets one (006-AC18).
	EveryN       *int
	EveryVoice   *int `json:",omitempty"`
	EveryMinutes *int `json:",omitempty"`
	SelfCall     *bool
	WatchesNotes *bool `json:",omitempty"`
	// Autocompact 0 leaves compaction to Claude Code.
	Autocompact *int `json:",omitempty"`
	// TemplateVersion is set by a held template update.
	TemplateVersion *int
	// Skills turns skills on at a version, or off, by skill id.
	Skills map[int64]SkillPin `json:",omitempty"`
	// Speaks, Voice and VoiceRate: the persona in the call (013-AC26, 013-AC37); the caller checks
	// the voice against the catalogue and the rate's range.
	Speaks    *bool    `json:",omitempty"`
	Voice     *string  `json:",omitempty"`
	VoiceRate *float64 `json:",omitempty"`
}

func orDefault(s string) string {
	if s == "" {
		return "default"
	}
	return s
}

// apply changes p and describes each change for the chat.
func (c PersonaPatch) apply(p *RoomPersona) []string {
	var out []string
	set := func(dst *string, v *string, label string, show bool) {
		if v == nil || *v == *dst {
			return
		}
		if show {
			out = append(out, fmt.Sprintf("%s %s → %s", label, orDefault(*dst), orDefault(*v)))
		} else {
			out = append(out, label)
		}
		*dst = *v
	}
	set(&p.Name, c.Name, "name", true)
	set(&p.Badge, c.Badge, "badge", true)
	set(&p.Colour, c.Colour, "colour", true)
	set(&p.Model, c.Model, "model", true)
	set(&p.Effort, c.Effort, "effort", true)
	set(&p.Text, c.Text, "persona text", false)
	if c.EveryN != nil && *c.EveryN != p.EveryN {
		out = append(out, fmt.Sprintf("every N rounds %s → %s", everyN(p.EveryN), everyN(*c.EveryN)))
		p.EveryN = *c.EveryN
	}
	if c.EveryVoice != nil && *c.EveryVoice != p.EveryVoice {
		out = append(out, fmt.Sprintf("every N voice lines %s → %s", everyN(p.EveryVoice), everyN(*c.EveryVoice)))
		p.resetMark = p.resetMark || p.EveryVoice == 0
		p.EveryVoice = *c.EveryVoice
	}
	if c.EveryMinutes != nil && *c.EveryMinutes != p.EveryMinutes {
		out = append(out, fmt.Sprintf("every N minutes %s → %s", everyN(p.EveryMinutes), everyN(*c.EveryMinutes)))
		p.resetMark = p.resetMark || p.EveryMinutes == 0
		p.EveryMinutes = *c.EveryMinutes
	}
	if c.SelfCall != nil && *c.SelfCall != p.SelfCall {
		out = append(out, fmt.Sprintf("self-call %s → %s", onOff(p.SelfCall), onOff(*c.SelfCall)))
		p.SelfCall = *c.SelfCall
	}
	if c.WatchesNotes != nil && *c.WatchesNotes != p.WatchesNotes {
		out = append(out, watchText(*c.WatchesNotes, p.watchFrom))
		p.WatchesNotes = *c.WatchesNotes
	}
	if c.Autocompact != nil && *c.Autocompact != p.Autocompact {
		out = append(out, fmt.Sprintf("autocompact %s → %s", autocompact(p.Autocompact), autocompact(*c.Autocompact)))
		p.Autocompact = *c.Autocompact
	}
	if c.Speaks != nil && *c.Speaks != p.Speaks {
		out = append(out, fmt.Sprintf("speaks in the call %s → %s", onOff(p.Speaks), onOff(*c.Speaks)))
		p.Speaks = *c.Speaks
	}
	set(&p.Voice, c.Voice, "voice", true)
	if c.VoiceRate != nil && *c.VoiceRate != p.VoiceRate {
		out = append(out, fmt.Sprintf("speaking rate %g → %g", p.VoiceRate, *c.VoiceRate))
		p.VoiceRate = *c.VoiceRate
	}
	if c.TemplateVersion != nil && *c.TemplateVersion != p.TemplateVersion {
		out = append(out, fmt.Sprintf("template v%d → v%d", p.TemplateVersion, *c.TemplateVersion))
		p.TemplateVersion = *c.TemplateVersion
	}
	return append(out, applySkills(p, c.Skills)...)
}

func autocompact(n int) string {
	if n == 0 {
		return "auto"
	}
	return strconv.Itoa(n/1000) + "k"
}

func everyN(n int) string {
	if n == 0 {
		return "off"
	}
	return strconv.Itoa(n)
}

// watchText says the notes watcher moved to a persona, from persona from if one watched, or left it.
func watchText(on bool, from string) string {
	switch {
	case !on:
		return "no longer watches the notes"
	case from != "":
		return "now watches the notes (was " + from + ")"
	}
	return "now watches the notes"
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func updatePersona(ctx context.Context, tx *txn, p *RoomPersona) error {
	if p.WatchesNotes {
		// One watcher per room: taking the role takes it from the previous one, with the notes it
		// has not read yet; a first watcher starts from now (009-AC31, 009-AC32).
		if _, err := tx.ExecContext(ctx, `UPDATE room_personas SET notes_mark = GREATEST(notes_mark, COALESCE((SELECT o.notes_mark FROM room_personas o
			WHERE o.room_id = room_personas.room_id AND o.id != room_personas.id AND o.watches_notes = 1), `+latestNote+`))
			WHERE id = ? AND watches_notes = 0`, p.ID); err != nil {
			return fmt.Errorf("update persona %d: %w", p.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE room_personas SET watches_notes = 0 WHERE room_id = ? AND id != ? AND watches_notes = 1`, p.RoomID, p.ID); err != nil {
			return fmt.Errorf("update persona %d: %w", p.ID, err)
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE room_personas SET template_version = ?, name = ?, badge = ?, colour = ?, text = ?, model = ?, effort = ?,
		every_n = ?, self_call = ?, pending = ?, autocompact = ?, every_voice = ?, every_minutes = ?, watches_notes = ?, speaks = ?, voice = ?, voice_rate = ? WHERE id = ?`,
		append(append([]any{p.TemplateVersion}, p.args()...), nullInt(p.EveryN), p.SelfCall, p.Pending, p.Autocompact,
			nullInt(p.EveryVoice), nullInt(p.EveryMinutes), p.WatchesNotes, p.Speaks, p.Voice, p.VoiceRate, p.ID)...)
	if isUnique(err) {
		return ErrNameTaken
	}
	if err != nil {
		return fmt.Errorf("update persona %d: %w", p.ID, err)
	}
	if p.resetMark {
		// A trigger just turned on counts from now, not from a turn long past (006-AC18).
		if _, err := tx.ExecContext(ctx, `UPDATE room_personas SET mark_msg_id = (SELECT COALESCE(MAX(id), 0) FROM messages), mark_at_ms = ? WHERE id = ?`, time.Now().UnixMilli(), p.ID); err != nil {
			return fmt.Errorf("update persona %d: %w", p.ID, err)
		}
	}
	return saveSkills(ctx, tx, p)
}

// EditPersona applies c to persona id of room and records what changed in the chat. A running turn
// keeps the persona it started with: the change applies from the next turn (002-AC15). While the
// host approves everything, the change waits for the host instead, and held is true (002-AC21). A
// patch changing nothing returns a nil message. ErrNameTaken, ErrNotFound.
func (s *Store) EditPersona(ctx context.Context, roomID string, id int64, c PersonaPatch, by *Participant) (p *RoomPersona, m *Message, held bool, err error) {
	err = s.withTx(ctx, "edit persona", func(tx *txn) error {
		var err error
		if p, err = roomPersona(ctx, tx, roomID, id); err != nil {
			return err
		}
		p, m, held, err = changePersona(ctx, tx, p, c, by, func(before string, changes []string) string {
			return fmt.Sprintf("changed %s: %s.", before, strings.Join(changes, ", "))
		})
		return err
	})
	return p, m, held, err
}

// changePersona applies c to p, or holds it for the host, and says so in the chat: applied words
// the line of an applied change from p's label before it and the changes. p is returned as the room
// now has it.
func changePersona(ctx context.Context, tx *txn, p *RoomPersona, c PersonaPatch, by *Participant, applied func(before string, changes []string) string) (*RoomPersona, *Message, bool, error) {
	before := p.label()
	held, err := heldBy(ctx, tx, p.RoomID, by)
	if err != nil {
		return nil, nil, false, err
	}
	// A role the persona has already, or lacks already, is no change: held, it would flip the role
	// back at approval after someone moved it (009-AC31).
	if c.WatchesNotes != nil && *c.WatchesNotes == p.WatchesNotes {
		c.WatchesNotes = nil
	}
	if err := otherWatcher(ctx, tx, p, c); err != nil {
		return nil, nil, false, err
	}
	target := p
	if held {
		cp := *p
		target = &cp
	}
	changes := c.apply(target)
	if len(changes) == 0 {
		return p, nil, false, nil
	}
	var body string
	if held {
		if err := holdChange(ctx, tx, p.RoomID, p, ChangeEdit, c, by); err != nil {
			return nil, nil, false, err
		}
		body = fmt.Sprintf("asked to change %s: %s.%s", before, strings.Join(changes, ", "), waitsNote)
	} else {
		if err := updatePersona(ctx, tx, p); err != nil {
			return nil, nil, false, err
		}
		// Read back: the next trigger is the store's to count (006-AC18).
		updated, err := roomPersona(ctx, tx, p.RoomID, p.ID)
		if err != nil {
			return nil, nil, false, err
		}
		p, body = updated, applied(before, changes)
	}
	m, err := memberLine(ctx, tx, p.RoomID, by, body)
	return p, m, held, err
}

// UpdatePersona takes the text, model and effort of the template's latest version into persona id
// of room; its name, badge and colour stay the room's. It is a persona change, held like an edit
// while the host approves everything. A persona already up to date returns a nil message. ErrNotFound.
func (s *Store) UpdatePersona(ctx context.Context, roomID string, id int64, by *Participant) (p *RoomPersona, m *Message, held bool, err error) {
	err = s.withTx(ctx, "update persona", func(tx *txn) error {
		var err error
		if p, err = roomPersona(ctx, tx, roomID, id); err != nil {
			return err
		}
		if p.UpdateVersion == 0 {
			return nil
		}
		t, err := scanTemplate(tx.QueryRowContext(ctx, templateCols+`WHERE t.id = ?`, p.TemplateID))
		if err != nil {
			return err
		}
		c := PersonaPatch{Text: &t.Text, Model: &t.Model, Effort: &t.Effort, TemplateVersion: &t.Version}
		p, m, held, err = changePersona(ctx, tx, p, c, by, func(before string, _ []string) string {
			return fmt.Sprintf("updated %s to version %d of the template %q.", before, t.Version, t.Title)
		})
		if err == nil && !held {
			p.UpdateVersion = 0
		}
		return err
	})
	return p, m, held, err
}

// RemovePersona deletes persona id of room with its sessions and queued turns; what it wrote stays.
// ErrPersonaBusy while its turn runs, ErrNotFound.
func (s *Store) RemovePersona(ctx context.Context, roomID string, id int64, by *Participant) (*Message, error) {
	var m *Message
	err := s.withTx(ctx, "remove persona", func(tx *txn) error {
		p, err := roomPersona(ctx, tx, roomID, id)
		if err != nil {
			return err
		}
		var busy bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM turns WHERE persona_id = ? AND status = 'running')`, id).Scan(&busy); err != nil {
			return fmt.Errorf("remove persona: %w", err)
		}
		if busy {
			return ErrPersonaBusy
		}
		if err := auditTx(ctx, tx, "", map[string]any{"persona": id, "name": p.Name}); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM room_personas WHERE id = ?`, id); err != nil {
			return fmt.Errorf("remove persona: %w", err)
		}
		m, err = memberLine(ctx, tx, roomID, by, "removed the persona "+p.label()+".")
		return err
	})
	return m, err
}

// Queue sources: who put a turn in the queue.
const (
	SourceButton  = "button"
	SourceMention = "mention"
)

// QueuedTurn is a turn waiting for the room to be free: a persona's button pressed while another
// turn ran, a human's @mention, or an agent-triggered link of the chain (Source every_n, call or
// notes_watch).
type QueuedTurn struct {
	ID        int64  `json:"id"`
	RoomID    string `json:"-"`
	PersonaID int64  `json:"persona_id"`
	// ParticipantID is who pressed or mentioned; 0 for a turn an agent triggered.
	ParticipantID int64     `json:"participant_id,omitempty"`
	Source        string    `json:"source"`
	Fresh         bool      `json:"fresh"`
	CreatedAt     time.Time `json:"created_at"`
	// RerunOf is the turn a conflict rejected, which this entry runs again ahead of the others;
	// Cause says why (003-AC21).
	RerunOf int64  `json:"rerun_of,omitempty"`
	Cause   string `json:"cause,omitempty"`
	// Handoff is HandoffWrite for the turn writing a handoff note, HandoffRead for the new session
	// reading it (003-AC25).
	Handoff string `json:"handoff,omitempty"`
}

// queueOrder runs re-runs first, then in the order turns were queued.
const queueOrder = ` ORDER BY rerun_of IS NULL, id`

const queueFields = `id, room_id, persona_id, COALESCE(participant_id, 0), source, fresh, created_at, COALESCE(rerun_of, 0), cause, handoff`

const queueCols = `SELECT ` + queueFields + ` FROM turn_queue `

func scanQueued(sc scanner) (*QueuedTurn, error) {
	var q QueuedTurn
	var created string
	err := sc.Scan(&q.ID, &q.RoomID, &q.PersonaID, &q.ParticipantID, &q.Source, &q.Fresh, &created, &q.RerunOf, &q.Cause, &q.Handoff)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan queued turn: %w", err)
	}
	q.CreatedAt = parseTS(created)
	return &q, nil
}

// Enqueue puts q at the end of its room's queue, as a press when q.Source is empty. ErrAlreadyQueued
// when its persona waits already, ErrNotFound when the persona is not in the room or waits for the
// host's approval.
func (s *Store) Enqueue(ctx context.Context, q *QueuedTurn) error {
	if q.Source == "" {
		q.Source = SourceButton
	}
	return enqueue(ctx, s.db, q)
}

func enqueue(ctx context.Context, db querier, q *QueuedTurn) error {
	q.CreatedAt = time.Now().UTC()
	var pid any
	if q.ParticipantID != 0 {
		pid = q.ParticipantID
	}
	err := db.QueryRowContext(ctx, `INSERT INTO turn_queue(room_id, persona_id, participant_id, source, fresh, created_at, cause, handoff)
		SELECT room_id, id, ?, ?, ?, ?, ?, ? FROM room_personas WHERE id = ? AND room_id = ? AND pending = 0
		ON CONFLICT (persona_id) DO NOTHING RETURNING id`,
		pid, q.Source, q.Fresh, formatTS(q.CreatedAt), q.Cause, q.Handoff, q.PersonaID, q.RoomID).Scan(&q.ID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("enqueue: %w", err)
	}
	// No row: the persona may not run here, or it is queued already. A failed INSERT would abort
	// the caller's transaction, which goes on with the queued turn (EnqueueMention).
	var queued bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM room_personas p JOIN turn_queue q ON q.persona_id = p.id
		WHERE p.id = ? AND p.room_id = ? AND p.pending = 0)`, q.PersonaID, q.RoomID).Scan(&queued); err != nil {
		return fmt.Errorf("enqueue: %w", err)
	}
	if queued {
		return ErrAlreadyQueued
	}
	return ErrNotFound
}

// Queue lists the room's queued turns, first to run first.
func (s *Store) Queue(ctx context.Context, roomID string) ([]QueuedTurn, error) {
	rows, err := s.db.QueryContext(ctx, queueCols+`WHERE room_id = ?`+queueOrder, roomID)
	if err != nil {
		return nil, fmt.Errorf("queue: %w", err)
	}
	defer rows.Close()
	out := []QueuedTurn{}
	for rows.Next() {
		q, err := scanQueued(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *q)
	}
	return out, rows.Err()
}

// CancelQueued drops queued turn id of room. Participant by may drop what they queued, and with
// canPress, the turns agents queued. ErrForbidden, ErrNotFound.
func (s *Store) CancelQueued(ctx context.Context, roomID string, id, by int64, canPress bool) error {
	q, err := scanQueued(s.db.QueryRowContext(ctx, queueCols+`WHERE id = ? AND room_id = ?`, id, roomID))
	if err != nil {
		return err
	}
	if q.ParticipantID != by && (q.ParticipantID != 0 || !canPress) {
		return ErrForbidden
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM turn_queue WHERE id = ?`, id); err != nil {
		return fmt.Errorf("cancel queued turn %d: %w", id, err)
	}
	return nil
}

// PopQueued removes and returns the head of the room's queue; with parallel on, the first entry
// whose persona runs no turn (003-AC20). ErrNotFound when there is none.
func (s *Store) PopQueued(ctx context.Context, roomID string, parallel bool) (*QueuedTurn, error) {
	return scanQueued(s.db.QueryRowContext(ctx, `DELETE FROM turn_queue WHERE id = (SELECT id FROM turn_queue q WHERE room_id = ?
			AND (? = 0 OR NOT EXISTS (SELECT 1 FROM turns t WHERE t.persona_id = q.persona_id AND t.status = 'running'))`+queueOrder+` LIMIT 1)
		RETURNING `+queueFields, roomID, parallel))
}

// DuePersonas lists the room's personas whose "every N rounds" is due: at least N turns of any
// persona completed since that persona's own last turn, whatever its outcome (002-AC22). A failed
// turn counts as the persona's last, so a persona that keeps failing does not retry every round.
func (s *Store) DuePersonas(ctx context.Context, roomID string) ([]RoomPersona, error) {
	rows, err := s.db.QueryContext(ctx, personaCols+`WHERE p.room_id = ? AND p.every_n IS NOT NULL AND p.pending = 0
		AND (SELECT COUNT(*) FROM turns t WHERE t.room_id = p.room_id AND t.status = 'done'
			AND t.id > COALESCE((SELECT MAX(id) FROM turns WHERE persona_id = p.id), 0)) >= p.every_n
		ORDER BY p.id`, roomID)
	if err != nil {
		return nil, fmt.Errorf("due personas: %w", err)
	}
	defer rows.Close()
	var out []RoomPersona
	for rows.Next() {
		p, err := scanPersona(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// PersonaStat is what the presence preamble says about a persona's past: its completed turns and
// its latest synthesis.
type PersonaStat struct {
	Turns int
	Last  string
}

// PersonaStats returns the stat of each persona of the room, by persona id.
func (s *Store) PersonaStats(ctx context.Context, roomID string) (map[int64]PersonaStat, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.id,
		(SELECT COUNT(*) FROM turns t WHERE t.persona_id = p.id AND t.status = 'done'),
		COALESCE((SELECT body FROM messages m WHERE m.persona_id = p.id AND m.kind = 'agent' AND m.deleted_at IS NULL ORDER BY m.id DESC LIMIT 1), '')
		FROM room_personas p WHERE p.room_id = ?`, roomID)
	if err != nil {
		return nil, fmt.Errorf("persona stats: %w", err)
	}
	defer rows.Close()
	out := map[int64]PersonaStat{}
	for rows.Next() {
		var id int64
		var st PersonaStat
		if err := rows.Scan(&id, &st.Turns, &st.Last); err != nil {
			return nil, fmt.Errorf("persona stats: %w", err)
		}
		out[id] = st
	}
	return out, rows.Err()
}

// DefaultVoice stores voice as persona id's voice when it has none yet, without a chat line: a
// default is not a change anyone made (013-AC16). It reports whether it stored it.
func (s *Store) DefaultVoice(ctx context.Context, roomID string, id int64, voice string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE room_personas SET voice = ? WHERE room_id = ? AND id = ? AND voice = ''`, voice, roomID, id)
	if err != nil {
		return false, fmt.Errorf("default voice: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
