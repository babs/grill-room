package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/babs/grill-room/internal/skill"
)

var (
	// ErrSameVersion is returned when an upload holds the same files as the skill's latest version.
	ErrSameVersion = errors.New("this upload is the skill's latest version already")
	// ErrSkillName is returned when a persona already has another skill of that name active.
	ErrSkillName = errors.New("the persona already has another skill with that name")
	// ErrDisclaimer is returned when a profile activates its first skill without accepting the
	// skills disclaimer (002-AC32).
	ErrDisclaimer = errors.New("accept the skills disclaimer first")
)

// Bounds on the library: every profile may upload.
const (
	MaxProfileSkills = 20
	MaxSkillVersions = 20
	MaxPersonaSkills = 10
)

// Skill listings.
const (
	// SkillsLibrary is what a member may activate in a room: the shared library and their own
	// personal skills (002-AC30).
	SkillsLibrary = "library"
	// SkillsMine is what a profile uploaded: its personal skills and the shared ones it published.
	SkillsMine = "mine"
	// SkillsAll is every skill, for the admin.
	SkillsAll = "all"
)

// Skill scopes: who owns a skill.
const (
	ScopeShared   = "shared"
	ScopePersonal = "personal"
	ScopeRoom     = "room"
)

// Skill is a skill of the library at its latest version. A shared skill belongs to the server and
// outlives its publisher; a personal one belongs to its profile; a room skill belongs to its room
// and goes with it (004-AC10).
type Skill struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Shared bool   `json:"shared"`
	Scope  string `json:"scope"`
	// Pending marks a room skill copied in while the host approves everything: it waits for the
	// host, and no persona can turn it on yet.
	Pending bool `json:"pending,omitempty"`
	// FromStarter marks a pending room skill the room's starter brought (006-AC13).
	FromStarter bool `json:"from_starter,omitempty"`
	// Mine tells the asking profile that it owns the skill (personal) or published it (shared):
	// it may upload versions.
	Mine        bool      `json:"mine"`
	ProfileID   int64     `json:"-"`
	PublisherID int64     `json:"-"`
	Version     int       `json:"version"`
	Author      string    `json:"author"`
	Description string    `json:"description"`
	Files       int       `json:"files"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	UpdatedAt   time.Time `json:"updated_at"`
}

const skillCols = `SELECT s.id, s.name, s.scope, s.pending, s.from_starter, COALESCE(s.profile_id, 0), COALESCE(s.publisher_id, 0),
	v.n, v.author, v.description, v.files, v.size, v.sha256, v.created_at
	FROM skills s JOIN skill_versions v ON v.skill_id = s.id AND v.n = (SELECT MAX(n) FROM skill_versions WHERE skill_id = s.id) `

func scanSkill(sc scanner) (*Skill, error) {
	var k Skill
	var updated string
	err := sc.Scan(&k.ID, &k.Name, &k.Scope, &k.Pending, &k.FromStarter, &k.ProfileID, &k.PublisherID, &k.Version, &k.Author, &k.Description, &k.Files, &k.Size, &k.SHA256, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan skill: %w", err)
	}
	k.Shared, k.UpdatedAt = k.Scope == ScopeShared, parseTS(updated)
	return &k, nil
}

func (k *Skill) mineFor(profileID int64) {
	k.Mine = profileID != 0 && (k.ProfileID == profileID || k.PublisherID == profileID)
}

// Skills lists, by name, the skills listing which shows to profileID. The library a member may
// activate in room roomID holds the room's own skills too, but for those waiting for the host.
func (s *Store) Skills(ctx context.Context, profileID int64, roomID, which string) ([]Skill, error) {
	var where string
	var args []any
	switch which {
	case SkillsLibrary:
		where, args = `WHERE s.scope = 'shared' OR s.profile_id = ? OR (s.room_id = ? AND s.pending = 0)`, []any{profileID, roomID}
	case SkillsMine:
		where, args = `WHERE s.profile_id = ? OR s.publisher_id = ?`, []any{profileID, profileID}
	case SkillsAll:
	default:
		return nil, fmt.Errorf("skills: unknown listing %q", which)
	}
	rows, err := s.db.QueryContext(ctx, skillCols+where+` ORDER BY s.name, s.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("skills: %w", err)
	}
	defer rows.Close()
	out := []Skill{}
	for rows.Next() {
		k, err := scanSkill(rows)
		if err != nil {
			return nil, err
		}
		k.mineFor(profileID)
		out = append(out, *k)
	}
	return out, rows.Err()
}

// PublishSkill stores an upload of profileID: a new version of the skill of that name it owns
// (personal) or published (shared), or a new skill. Versions never change once stored (002-AC29).
// The profile's name (005-AC1), else the latest it joined a room with, signs the version.
// ErrForbidden when another profile published the shared skill of that name or profileID is 0,
// ErrSameVersion, ErrLimit.
func (s *Store) PublishSkill(ctx context.Context, profileID int64, shared bool, up *skill.Skill) (*Skill, error) {
	if profileID == 0 {
		return nil, ErrForbidden
	}
	var id int64
	err := s.withTx(ctx, "publish skill", func(tx *txn) error {
		var publisher int64
		q, args := `SELECT id, 0 FROM skills WHERE profile_id = ? AND name = ?`, []any{profileID, up.Name}
		if shared {
			q, args = `SELECT id, COALESCE(publisher_id, 0) FROM skills WHERE scope = 'shared' AND name = ?`, []any{up.Name}
		}
		err := tx.QueryRowContext(ctx, q, args...).Scan(&id, &publisher)
		n := 1
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if err := overLimit(ctx, tx, MaxProfileSkills, "skills per profile", `SELECT count(*) FROM skills WHERE profile_id = ? OR publisher_id = ?`,
				profileID, profileID); err != nil {
				return err
			}
			owner, pub := nullID(profileID), any(nil)
			if shared {
				owner, pub = nil, profileID
			}
			if err := tx.QueryRowContext(ctx, `INSERT INTO skills(name, profile_id, publisher_id, created_at) VALUES(?,?,?,?) RETURNING id`,
				up.Name, owner, pub, now()).Scan(&id); err != nil {
				return fmt.Errorf("publish skill: %w", err)
			}
		case err != nil:
			return fmt.Errorf("publish skill: %w", err)
		case shared && publisher != profileID:
			return ErrForbidden
		default:
			var sum string
			if err := tx.QueryRowContext(ctx, `SELECT n, sha256 FROM skill_versions WHERE skill_id = ? ORDER BY n DESC LIMIT 1`, id).Scan(&n, &sum); err != nil {
				return fmt.Errorf("publish skill: %w", err)
			}
			if sum == up.SHA256 {
				return fmt.Errorf("%w (v%d)", ErrSameVersion, n)
			}
			if n >= MaxSkillVersions {
				return fmt.Errorf("%w: at most %d versions per skill", ErrLimit, MaxSkillVersions)
			}
			n++
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO skill_versions(skill_id, n, author, author_id, description, files, size, sha256, data, created_at)
			VALUES(?, ?, COALESCE((SELECT name FROM profiles WHERE id = ?), (SELECT name FROM participants WHERE profile_id = ? ORDER BY id DESC LIMIT 1), 'someone'), ?, ?, ?, ?, ?, ?, ?)`,
			id, n, profileID, profileID, profileID, up.Description, up.Files, up.Size, up.SHA256, up.Tar, now())
		if err != nil {
			return fmt.Errorf("publish skill: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	k, err := scanSkill(s.db.QueryRowContext(ctx, skillCols+`WHERE s.id = ?`, id))
	if err != nil {
		return nil, err
	}
	k.mineFor(profileID)
	return k, nil
}

// RemoveSkill deletes skill id with its versions; the personas that had it active lose it. The
// admin (profileID 0) removes any skill (002-AC29); a profile only its personal ones. ErrNotFound.
func (s *Store) RemoveSkill(ctx context.Context, id, profileID int64) error {
	return s.withTx(ctx, "remove skill", func(tx *txn) error {
		if err := auditTx(ctx, tx, "", nil); err != nil {
			return err
		}
		return execOne(ctx, tx, `DELETE FROM skills WHERE id = ? AND (? = 0 OR profile_id = ?)`, id, profileID, profileID)
	})
}

// SkillsDisclaimerAccepted reports whether profile id accepted the skills disclaimer (002-AC32).
func (s *Store) SkillsDisclaimerAccepted(ctx context.Context, id int64) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx, `SELECT skills_disclaimer_at IS NOT NULL FROM profiles WHERE id = ?`, id).Scan(&ok)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("skills disclaimer: %w", err)
	}
	return ok, nil
}

// PersonaSkill is a skill a room persona has active, pinned at Version (002-AC31).
type PersonaSkill struct {
	SkillID int64  `json:"skill_id"`
	Name    string `json:"name"`
	Shared  bool   `json:"shared"`
	Scope   string `json:"scope"`
	Version int    `json:"version"`
	// UpdateVersion is the skill's latest version when newer than the pin: offered to the room,
	// never applied by itself.
	UpdateVersion int `json:"update_version,omitempty"`
}

// SkillPin is a skill turned on at Version, or off with Version 0, in a persona change.
type SkillPin struct {
	Name    string
	Shared  bool
	Scope   string `json:",omitempty"`
	Version int
}

type rowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// loadSkills fills in the active skills of ps, all of room roomID.
func loadSkills(ctx context.Context, db rowsQuerier, roomID string, ps []RoomPersona) error {
	if len(ps) == 0 {
		return nil
	}
	rows, err := db.QueryContext(ctx, `SELECT k.persona_id, s.id, s.name, s.scope, k.version,
		(SELECT MAX(n) FROM skill_versions WHERE skill_id = s.id)
		FROM room_persona_skills k JOIN room_personas p ON p.id = k.persona_id JOIN skills s ON s.id = k.skill_id
		WHERE p.room_id = ? ORDER BY s.name, s.id`, roomID)
	if err != nil {
		return fmt.Errorf("persona skills: %w", err)
	}
	defer rows.Close()
	byID := map[int64][]PersonaSkill{}
	for rows.Next() {
		var pid int64
		var k PersonaSkill
		if err := rows.Scan(&pid, &k.SkillID, &k.Name, &k.Scope, &k.Version, &k.UpdateVersion); err != nil {
			return fmt.Errorf("persona skills: %w", err)
		}
		k.Shared = k.Scope == ScopeShared
		if k.UpdateVersion <= k.Version {
			k.UpdateVersion = 0
		}
		byID[pid] = append(byID[pid], k)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("persona skills: %w", err)
	}
	for i := range ps {
		ps[i].Skills = byID[ps[i].ID]
		if ps[i].Skills == nil {
			ps[i].Skills = []PersonaSkill{}
		}
	}
	return nil
}

// applySkills lays pins over p's skills and describes each change for the chat.
func applySkills(p *RoomPersona, pins map[int64]SkillPin) []string {
	if len(pins) == 0 {
		return nil
	}
	// A held change describes itself on a copy of the persona: its slice must not be shared.
	p.Skills = slices.Clone(p.Skills)
	ids := make([]int64, 0, len(pins))
	for id := range pins {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return pins[ids[i]].Name < pins[ids[j]].Name || pins[ids[i]].Name == pins[ids[j]].Name && ids[i] < ids[j]
	})
	var out []string
	for _, id := range ids {
		pin := pins[id]
		i := slices.IndexFunc(p.Skills, func(k PersonaSkill) bool { return k.SkillID == id })
		switch {
		case pin.Version == 0 && i >= 0:
			out = append(out, "skill "+pin.Name+" off")
			p.Skills = slices.Delete(p.Skills, i, i+1)
		case pin.Version > 0 && i < 0:
			out = append(out, fmt.Sprintf("skill %s v%d on", pin.Name, pin.Version))
			p.Skills = append(p.Skills, PersonaSkill{SkillID: id, Name: pin.Name, Shared: pin.Shared, Scope: pin.Scope, Version: pin.Version})
		case pin.Version > 0 && p.Skills[i].Version != pin.Version:
			out = append(out, fmt.Sprintf("skill %s v%d → v%d", pin.Name, p.Skills[i].Version, pin.Version))
			p.Skills[i].Version = pin.Version
			if p.Skills[i].UpdateVersion <= pin.Version {
				p.Skills[i].UpdateVersion = 0
			}
		}
	}
	sort.SliceStable(p.Skills, func(i, j int) bool { return p.Skills[i].Name < p.Skills[j].Name })
	return out
}

// mergePins lays later's pins over earlier's: a held change keeps every skill asked for.
func mergePins(later, earlier map[int64]SkillPin) map[int64]SkillPin {
	if len(later)+len(earlier) == 0 {
		return nil
	}
	out := map[int64]SkillPin{}
	for id, p := range earlier {
		out[id] = p
	}
	for id, p := range later {
		out[id] = p
	}
	return out
}

// saveSkills makes p's active skills the stored ones. A skill removed from the library since a
// change was held is skipped.
func saveSkills(ctx context.Context, tx *txn, p *RoomPersona) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM room_persona_skills WHERE persona_id = ?`, p.ID); err != nil {
		return fmt.Errorf("save persona skills: %w", err)
	}
	for _, k := range p.Skills {
		if _, err := tx.ExecContext(ctx, `INSERT INTO room_persona_skills(persona_id, skill_id, version)
			SELECT ?, skill_id, n FROM skill_versions WHERE skill_id = ? AND n = ?`, p.ID, k.SkillID, k.Version); err != nil {
			return fmt.Errorf("save persona skills: %w", err)
		}
	}
	return nil
}

// SetPersonaSkill turns skill skillID on for persona id of room at the skill's latest version,
// which also takes an update the room was offered, or turns it off. It is a persona change: in the
// chat, and held while the host approves everything (002-AC15, 002-AC21). Turning a skill on needs
// a profile that accepted the disclaimer, or accepts it now (002-AC32), and only offers the shared
// library and the member's own personal skills (002-AC30). A change of nothing returns a nil
// message. ErrForbidden without a profile, ErrDisclaimer, ErrSkillName, ErrLimit, ErrNotFound.
func (s *Store) SetPersonaSkill(ctx context.Context, roomID string, id, skillID int64, on, accept bool, by *Participant) (p *RoomPersona, m *Message, held bool, err error) {
	err = s.withTx(ctx, "set persona skill", func(tx *txn) error {
		var err error
		if p, err = roomPersona(ctx, tx, roomID, id); err != nil {
			return err
		}
		i := slices.IndexFunc(p.Skills, func(k PersonaSkill) bool { return k.SkillID == skillID })
		var pin SkillPin
		if !on {
			if i < 0 {
				return nil
			}
			pin = SkillPin{Name: p.Skills[i].Name}
		} else if pin, err = pinLatest(ctx, tx, p, i, skillID, accept, by); err != nil {
			return err
		}
		p, m, held, err = changePersona(ctx, tx, p, PersonaPatch{Skills: map[int64]SkillPin{skillID: pin}}, by, func(before string, changes []string) string {
			return fmt.Sprintf("changed %s: %s.", before, strings.Join(changes, ", "))
		})
		return err
	})
	return p, m, held, err
}

// pinLatest checks that by may turn skill skillID on for p, whose active skills hold it at i (-1
// for none), records the disclaimer by accepted, and returns the pin of its latest version.
func pinLatest(ctx context.Context, tx *txn, p *RoomPersona, i int, skillID int64, accept bool, by *Participant) (SkillPin, error) {
	if by.ProfileID == 0 {
		return SkillPin{}, ErrForbidden
	}
	var pin SkillPin
	var owner int64
	var room string
	var pending bool
	err := tx.QueryRowContext(ctx, `SELECT name, scope, COALESCE(profile_id, 0), COALESCE(room_id, ''), pending, (SELECT MAX(n) FROM skill_versions WHERE skill_id = s.id)
		FROM skills s WHERE id = ?`, skillID).Scan(&pin.Name, &pin.Scope, &owner, &room, &pending, &pin.Version)
	pin.Shared = pin.Scope == ScopeShared
	// The library offered in the room: shared skills, by's personal ones, the room's own approved.
	offered := pin.Shared || (pin.Scope == ScopePersonal && owner == by.ProfileID) || (pin.Scope == ScopeRoom && room == p.RoomID && !pending)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !offered) {
		return SkillPin{}, ErrNotFound
	}
	if err != nil {
		return SkillPin{}, fmt.Errorf("set persona skill: %w", err)
	}
	var accepted bool
	if err := tx.QueryRowContext(ctx, `SELECT skills_disclaimer_at IS NOT NULL FROM profiles WHERE id = ?`, by.ProfileID).Scan(&accepted); err != nil {
		return SkillPin{}, fmt.Errorf("set persona skill: %w", err)
	}
	if !accepted {
		if !accept {
			return SkillPin{}, ErrDisclaimer
		}
		if _, err := tx.ExecContext(ctx, `UPDATE profiles SET skills_disclaimer_at = ? WHERE id = ?`, now(), by.ProfileID); err != nil {
			return SkillPin{}, fmt.Errorf("set persona skill: %w", err)
		}
	}
	if i < 0 && len(p.Skills) >= MaxPersonaSkills {
		return SkillPin{}, fmt.Errorf("%w: at most %d skills per persona", ErrLimit, MaxPersonaSkills)
	}
	if slices.ContainsFunc(p.Skills, func(k PersonaSkill) bool { return k.Name == pin.Name && k.SkillID != skillID }) {
		return SkillPin{}, ErrSkillName
	}
	return pin, nil
}

// TurnSkill is a skill a turn loads, at the version pinned when it started.
type TurnSkill struct {
	Name    string
	Version int
	Data    []byte
}

// pinTurnSkills copies the persona's pinned skills onto turn id as it starts: a change during the
// turn applies to the next (002-AC15). A personal skill rides only while its owner is in the room
// (002-AC30); a room skill rides whoever copied it in (004-AC10), once the host approved it
// (004-AC10, 006-AC13).
func pinTurnSkills(ctx context.Context, tx *txn, roomID string, id, personaID int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE turns SET skills = (SELECT COALESCE(jsonb_agg(v.id ORDER BY s.name), '[]')::text
			FROM room_persona_skills k JOIN skills s ON s.id = k.skill_id
			JOIN skill_versions v ON v.skill_id = k.skill_id AND v.n = k.version
			WHERE k.persona_id = ? AND (s.scope = 'shared' OR (s.scope = 'room' AND s.room_id = ? AND s.pending = 0)
				OR EXISTS (SELECT 1 FROM participants p
				WHERE p.room_id = ? AND p.profile_id = s.profile_id AND p.removed_at IS NULL)))
		WHERE id = ?`, personaID, roomID, roomID, id)
	if err != nil {
		return fmt.Errorf("pin turn skills: %w", err)
	}
	return nil
}

// TurnSkills returns the skills turn id of room loads, by name, with their files when withData.
// A skill the admin removed since the turn started is gone from it.
func (s *Store) TurnSkills(ctx context.Context, roomID string, id int64, withData bool) ([]TurnSkill, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.name, v.n, CASE WHEN ? = 1 THEN v.data ELSE ''::bytea END
		FROM turns t CROSS JOIN jsonb_array_elements_text(t.skills::jsonb) j(value)
		JOIN skill_versions v ON v.id = j.value::bigint JOIN skills s ON s.id = v.skill_id
		WHERE t.id = ? AND t.room_id = ? ORDER BY s.name`, withData, id, roomID)
	if err != nil {
		return nil, fmt.Errorf("turn skills: %w", err)
	}
	defer rows.Close()
	var out []TurnSkill
	for rows.Next() {
		var k TurnSkill
		if err := rows.Scan(&k.Name, &k.Version, &k.Data); err != nil {
			return nil, fmt.Errorf("turn skills: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// MaxRoomSkills bounds a room's own skills.
const MaxRoomSkills = 20

// ErrRoomSkillName is returned when copying a skill into a room that has one of that name.
var ErrRoomSkillName = errors.New("the room has a skill with that name already")

// RoomSkills lists the room's own skills by name, pending ones included.
func (s *Store) RoomSkills(ctx context.Context, roomID string) ([]Skill, error) {
	rows, err := s.db.QueryContext(ctx, skillCols+`WHERE s.room_id = ? ORDER BY s.name`, roomID)
	if err != nil {
		return nil, fmt.Errorf("room skills: %w", err)
	}
	defer rows.Close()
	out := []Skill{}
	for rows.Next() {
		k, err := scanSkill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

// CopySkillToRoom copies the latest version of skill skillID, one of by's personal skills, into
// room as a room skill frozen at that version, and says so in the chat. The copy stays the room's
// whoever leaves. While the host approves everything, it waits for the host as pending
// (004-AC10). ErrForbidden without a profile, ErrNotFound for a skill that is not by's personal
// one, ErrRoomSkillName, ErrLimit.
func (s *Store) CopySkillToRoom(ctx context.Context, roomID string, skillID int64, by *Participant) (*Skill, *Message, error) {
	if by.ProfileID == 0 {
		return nil, nil, ErrForbidden
	}
	var id int64
	var m *Message
	err := s.withTx(ctx, "copy skill to room", func(tx *txn) error {
		var name string
		var n int
		err := tx.QueryRowContext(ctx, `SELECT name, (SELECT MAX(n) FROM skill_versions WHERE skill_id = s.id) FROM skills s
			WHERE id = ? AND scope = 'personal' AND profile_id = ?`, skillID, by.ProfileID).Scan(&name, &n)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("copy skill to room: %w", err)
		}
		if err := overLimit(ctx, tx, MaxRoomSkills, "skills per room", `SELECT count(*) FROM skills WHERE room_id = ?`, roomID); err != nil {
			return err
		}
		held, err := heldBy(ctx, tx, roomID, by)
		if err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, `INSERT INTO skills(name, created_at, room_id, pending) VALUES(?,?,?,?) RETURNING id`, name, now(), roomID, held).Scan(&id)
		if isUnique(err) {
			return ErrRoomSkillName
		}
		if err != nil {
			return fmt.Errorf("copy skill to room: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO skill_versions(skill_id, n, author, author_id, description, files, size, sha256, data, created_at)
			SELECT ?, n, author, author_id, description, files, size, sha256, data, ? FROM skill_versions WHERE skill_id = ? AND n = ?`,
			id, now(), skillID, n); err != nil {
			return fmt.Errorf("copy skill to room: %w", err)
		}
		body := fmt.Sprintf("copied the skill %s v%d into the room.", name, n)
		if held {
			body += waitsNote
		}
		m, err = memberLine(ctx, tx, roomID, by, body)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	k, err := scanSkill(s.db.QueryRowContext(ctx, skillCols+`WHERE s.id = ?`, id))
	return k, m, err
}

// DecideRoomSkill is the host's answer to a room skill copied in while they approve everything:
// approved, personas may turn it on; refused, it is removed. Either way the chat says so
// (004-AC10). ErrForbidden unless by hosts the room, ErrNotFound unless the skill waits in it.
func (s *Store) DecideRoomSkill(ctx context.Context, roomID string, id int64, approve bool, by *Participant) (*Message, error) {
	var m *Message
	err := s.withTx(ctx, "decide room skill", func(tx *txn) error {
		if err := asHost(ctx, tx, roomID, by); err != nil {
			return err
		}
		k, err := scanSkill(tx.QueryRowContext(ctx, skillCols+`WHERE s.id = ? AND s.room_id = ? AND s.pending = 1`, id, roomID))
		if err != nil {
			return err
		}
		what := fmt.Sprintf("%s's copy of the skill %s v%d", k.Author, k.Name, k.Version)
		if k.FromStarter {
			what = fmt.Sprintf("the starter's skill %s v%d", k.Name, k.Version)
		}
		body := "approved " + what + "."
		q := `UPDATE skills SET pending = 0, from_starter = 0 WHERE id = ?`
		if !approve {
			body = "refused " + what + ": it is removed."
			q = `DELETE FROM skills WHERE id = ?`
		}
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return fmt.Errorf("decide room skill: %w", err)
		}
		m, err = memberLine(ctx, tx, roomID, by, body)
		return err
	})
	return m, err
}

// RemoveRoomSkill deletes room skill id of room with its versions, for a room admin by; the
// personas that had it on lose it, and the chat says so. ErrNotFound.
func (s *Store) RemoveRoomSkill(ctx context.Context, roomID string, id int64, by *Participant) (*Message, error) {
	var m *Message
	err := s.withTx(ctx, "remove room skill", func(tx *txn) error {
		var name string
		err := tx.QueryRowContext(ctx, `DELETE FROM skills WHERE id = ? AND room_id = ? RETURNING name`, id, roomID).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("remove room skill: %w", err)
		}
		if m, err = memberLine(ctx, tx, roomID, by, "removed the room skill "+name+"."); err != nil {
			return err
		}
		return auditTx(ctx, tx, "", map[string]any{"skill": id, "name": name})
	})
	return m, err
}
