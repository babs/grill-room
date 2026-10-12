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
func overLimit(ctx context.Context, tx *sql.Tx, limit int, what, query string, args ...any) error {
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
	// Model and Effort are what the persona asks for; empty leaves the choice to the host.
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
	{PresetDefault, "Default", PersonaFields{Name: "Grill", Badge: "🔥", Colour: "#d9480f", Text: grillText}},
	{"secretary", "Secretary", PersonaFields{Name: "Secretary", Badge: "📝", Colour: "#1971c2", Text: secretaryText}},
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
	var preset sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT preset FROM persona_templates WHERE id = ?`, id).Scan(&preset)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("remove template %d: %w", id, err)
	}
	if preset.Valid {
		return ErrPreset
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM persona_templates WHERE id = ?`, id); err != nil {
		return fmt.Errorf("remove template %d: %w", id, err)
	}
	return nil
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
	// Pending is a persona added while the host approves everything: it cannot run until the host
	// approves it (002-AC21).
	Pending bool `json:"pending,omitempty"`
	// Cap is the host's last decision on this persona asking above the cap (002-AC20).
	Cap CapDecision `json:"-"`
	// Skills are its active skills, by name, each pinned at a version (002-AC31).
	Skills    []PersonaSkill `json:"skills"`
	CreatedAt time.Time      `json:"created_at"`
}

const personaCols = `SELECT p.id, p.room_id, COALESCE(p.template_id, 0), p.template_version,
	COALESCE((SELECT MAX(n) FROM persona_template_versions WHERE template_id = p.template_id), 0),
	p.name, p.badge, p.colour, p.text, p.model, p.effort, COALESCE(p.every_n, 0), p.self_call, p.created_at,
	p.pending, p.cap_decision, COALESCE(p.cap_daemon_id, 0), p.cap_model, p.cap_effort FROM room_personas p `

func scanPersona(sc scanner) (*RoomPersona, error) {
	var p RoomPersona
	var created string
	err := sc.Scan(&p.ID, &p.RoomID, &p.TemplateID, &p.TemplateVersion, &p.UpdateVersion,
		&p.Name, &p.Badge, &p.Colour, &p.Text, &p.Model, &p.Effort, &p.EveryN, &p.SelfCall, &created,
		&p.Pending, &p.Cap.Status, &p.Cap.DaemonID, &p.Cap.Level.Model, &p.Cap.Level.Effort)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan persona: %w", err)
	}
	if p.UpdateVersion <= p.TemplateVersion {
		p.UpdateVersion = 0
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

func roomPersona(ctx context.Context, tx *sql.Tx, roomID string, id int64) (*RoomPersona, error) {
	return withSkills(ctx, tx, tx.QueryRowContext(ctx, personaCols+`WHERE p.id = ? AND p.room_id = ?`, id, roomID), roomID)
}

// withSkills scans one persona with its active skills.
func withSkills(ctx context.Context, db rowsQuerier, row *sql.Row, roomID string) (*RoomPersona, error) {
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
func freeName(ctx context.Context, tx *sql.Tx, roomID, name string) (string, error) {
	for i := 1; ; i++ {
		try := name
		if i > 1 {
			try = name + "-" + strconv.Itoa(i)
		}
		var taken bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM room_personas WHERE room_id = ? AND name = ? COLLATE NOCASE)`,
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
func addPersona(ctx context.Context, tx *sql.Tx, roomID string, templateID int64) (*RoomPersona, string, error) {
	t, err := scanTemplate(tx.QueryRowContext(ctx, templateCols+`WHERE t.id = ?`, templateID))
	if err != nil {
		return nil, "", err
	}
	if t.Name, err = freeName(ctx, tx, roomID, t.Name); err != nil {
		return nil, "", err
	}
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO room_personas(room_id, template_id, template_version, name, badge, colour, text, model, effort, created_at)
		SELECT id, ?, ?, ?, ?, ?, ?, ?, ?, ? FROM rooms WHERE id = ? RETURNING id`,
		append(append([]any{t.ID, t.Version}, t.PersonaFields.args()...), now(), roomID)...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("add persona: %w", err)
	}
	p, err := roomPersona(ctx, tx, roomID, id)
	return p, t.Title, err
}

func presetID(ctx context.Context, tx *sql.Tx, key string) (int64, error) {
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM persona_templates WHERE preset = ?`, key).Scan(&id); err != nil {
		return 0, fmt.Errorf("preset %s: %w", key, err)
	}
	return id, nil
}

// memberLine records a member's change (a persona, the chain) in the room, signed by them.
func memberLine(ctx context.Context, tx *sql.Tx, roomID string, by *Participant, body string) (*Message, error) {
	m := &Message{RoomID: roomID, Kind: KindSystem, ParticipantID: by.ID, Sender: by.Name, Body: by.Name + " " + body}
	return m, insertMessage(ctx, tx, m)
}

func (p *RoomPersona) label() string { return p.Badge + " " + p.Name }

// withTx runs fn in a transaction, committed when fn returns no error.
func (s *Store) withTx(ctx context.Context, what string, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// AddPersona adds a copy of template templateID's latest version to room, recorded in the chat.
// A name the room uses already gets a number. While the host approves everything, the persona waits
// for the host, Pending (002-AC21). ErrNotFound for an unknown room or template.
func (s *Store) AddPersona(ctx context.Context, roomID string, templateID int64, by *Participant) (*RoomPersona, *Message, error) {
	var p *RoomPersona
	var m *Message
	err := s.withTx(ctx, "add persona", func(tx *sql.Tx) error {
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
	// EveryN 0 turns the trigger off.
	EveryN   *int
	SelfCall *bool
	// TemplateVersion is set by a held template update.
	TemplateVersion *int
	// Skills turns skills on at a version, or off, by skill id.
	Skills map[int64]SkillPin `json:",omitempty"`
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
	if c.SelfCall != nil && *c.SelfCall != p.SelfCall {
		out = append(out, fmt.Sprintf("self-call %s → %s", onOff(p.SelfCall), onOff(*c.SelfCall)))
		p.SelfCall = *c.SelfCall
	}
	if c.TemplateVersion != nil && *c.TemplateVersion != p.TemplateVersion {
		out = append(out, fmt.Sprintf("template v%d → v%d", p.TemplateVersion, *c.TemplateVersion))
		p.TemplateVersion = *c.TemplateVersion
	}
	return append(out, applySkills(p, c.Skills)...)
}

func everyN(n int) string {
	if n == 0 {
		return "off"
	}
	return strconv.Itoa(n)
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

func updatePersona(ctx context.Context, tx *sql.Tx, p *RoomPersona) error {
	_, err := tx.ExecContext(ctx, `UPDATE room_personas SET template_version = ?, name = ?, badge = ?, colour = ?, text = ?, model = ?, effort = ?,
		every_n = ?, self_call = ?, pending = ? WHERE id = ?`, append(append([]any{p.TemplateVersion}, p.args()...), nullInt(p.EveryN), p.SelfCall, p.Pending, p.ID)...)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return ErrNameTaken
	}
	if err != nil {
		return fmt.Errorf("update persona %d: %w", p.ID, err)
	}
	return saveSkills(ctx, tx, p)
}

// EditPersona applies c to persona id of room and records what changed in the chat. A running turn
// keeps the persona it started with: the change applies from the next turn (002-AC15). While the
// host approves everything, the change waits for the host instead, and held is true (002-AC21). A
// patch changing nothing returns a nil message. ErrNameTaken, ErrNotFound.
func (s *Store) EditPersona(ctx context.Context, roomID string, id int64, c PersonaPatch, by *Participant) (p *RoomPersona, m *Message, held bool, err error) {
	err = s.withTx(ctx, "edit persona", func(tx *sql.Tx) error {
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
func changePersona(ctx context.Context, tx *sql.Tx, p *RoomPersona, c PersonaPatch, by *Participant, applied func(before string, changes []string) string) (*RoomPersona, *Message, bool, error) {
	before := p.label()
	held, err := heldBy(ctx, tx, p.RoomID, by)
	if err != nil {
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
		body = applied(before, changes)
	}
	m, err := memberLine(ctx, tx, p.RoomID, by, body)
	return p, m, held, err
}

// UpdatePersona takes the text, model and effort of the template's latest version into persona id
// of room; its name, badge and colour stay the room's. It is a persona change, held like an edit
// while the host approves everything. A persona already up to date returns a nil message. ErrNotFound.
func (s *Store) UpdatePersona(ctx context.Context, roomID string, id int64, by *Participant) (p *RoomPersona, m *Message, held bool, err error) {
	err = s.withTx(ctx, "update persona", func(tx *sql.Tx) error {
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
	err := s.withTx(ctx, "remove persona", func(tx *sql.Tx) error {
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
// turn ran, a human's @mention, or an agent-triggered link of the chain (Source every_n or call).
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
}

// queueOrder runs re-runs first, then in the order turns were queued.
const queueOrder = ` ORDER BY rerun_of IS NULL, id`

const queueCols = `SELECT id, room_id, persona_id, COALESCE(participant_id, 0), source, fresh, created_at, COALESCE(rerun_of, 0), cause FROM turn_queue `

func scanQueued(sc scanner) (*QueuedTurn, error) {
	var q QueuedTurn
	var created string
	err := sc.Scan(&q.ID, &q.RoomID, &q.PersonaID, &q.ParticipantID, &q.Source, &q.Fresh, &created, &q.RerunOf, &q.Cause)
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
	err := db.QueryRowContext(ctx, `INSERT INTO turn_queue(room_id, persona_id, participant_id, source, fresh, created_at)
		SELECT room_id, id, ?, ?, ?, ? FROM room_personas WHERE id = ? AND room_id = ? AND pending = 0 RETURNING id`,
		pid, q.Source, q.Fresh, q.CreatedAt.Format(time.RFC3339Nano), q.PersonaID, q.RoomID).Scan(&q.ID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotFound
	case err != nil && strings.Contains(err.Error(), "UNIQUE"):
		return ErrAlreadyQueued
	case err != nil:
		return fmt.Errorf("enqueue: %w", err)
	}
	return nil
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
		RETURNING id, room_id, persona_id, COALESCE(participant_id, 0), source, fresh, created_at, COALESCE(rerun_of, 0), cause`, roomID, parallel))
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

// migratePersonas moves a pre-002 database to personas (002-AC42): every room gets the Default
// template's persona, which takes over the room's turns, session and everything signed "Agent".
func migratePersonas(ctx context.Context, tx *sql.Tx) error {
	if err := ensurePresets(ctx, tx); err != nil {
		return err
	}
	def, err := presetID(ctx, tx, PresetDefault)
	if err != nil {
		return err
	}
	var name string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM persona_template_versions WHERE template_id = ? AND n = 1`, def).Scan(&name); err != nil {
		return fmt.Errorf("migrate personas: %w", err)
	}
	persona := `(SELECT id FROM room_personas p WHERE p.room_id = %s)`
	steps := []struct {
		q    string
		args []any
	}{
		{`INSERT INTO room_personas(room_id, template_id, template_version, name, badge, colour, text, model, effort, created_at)
			SELECT r.id, v.template_id, v.n, v.name, v.badge, v.colour, v.text, v.model, v.effort, ? FROM rooms r
			JOIN persona_template_versions v ON v.template_id = ? AND v.n = 1`, []any{now(), def}},
		{`UPDATE turns SET persona_id = ` + fmt.Sprintf(persona, "turns.room_id") + `, persona_name = ?`, []any{name}},
		{`INSERT INTO agent_sessions_by_persona(room_id, persona_id, session_id, data, updated_at)
			SELECT room_id, ` + fmt.Sprintf(persona, "s.room_id") + `, session_id, data, updated_at FROM agent_sessions s`, nil},
		{`DROP TABLE agent_sessions`, nil},
		{`ALTER TABLE agent_sessions_by_persona RENAME TO agent_sessions`, nil},
		{`UPDATE messages SET sender = ?, persona_id = ` + fmt.Sprintf(persona, "messages.room_id") + ` WHERE kind = 'agent'`, []any{name}},
		// Turn failures and reports: signed "Agent" by the server, with no participant.
		{`UPDATE messages SET sender = ?, body = CASE WHEN body LIKE 'Agent turn%' THEN ? || substr(body, 6) ELSE body END
			WHERE kind = 'system' AND participant_id IS NULL AND sender = 'Agent'`, []any{name, name}},
		{`UPDATE annotation_messages SET author = ?, persona_id = (SELECT p.id FROM room_personas p JOIN annotations a ON a.room_id = p.room_id
			WHERE a.id = annotation_messages.annotation_id) WHERE author_kind = 'agent'`, []any{name}},
		{`UPDATE documents SET author = ? WHERE kind = 'agent'`, []any{name}},
		{`UPDATE revisions SET author = ? WHERE document_id IN (SELECT id FROM documents WHERE kind = 'agent')`, []any{name}},
	}
	for _, st := range steps {
		if _, err := tx.ExecContext(ctx, st.q, st.args...); err != nil {
			return fmt.Errorf("migrate personas: %w", err)
		}
	}
	return nil
}
