package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// AgendaName is the document every persona aligns on, pinned first in the list (006-AC14).
const AgendaName = "agenda.md"

// Provenance names the starter version a room was created from (006-AC11).
type Provenance struct {
	Title   string `json:"title"`
	Version int    `json:"version"`
}

func parseProvenance(s sql.NullString) *Provenance {
	if !s.Valid || s.String == "" {
		return nil
	}
	var p Provenance
	if json.Unmarshal([]byte(s.String), &p) != nil {
		return nil
	}
	return &p
}

// RoomSetup is what a starter puts in a new room, as copies: nothing links the room back to it
// (006-AC11). The caller validated it; Files hold the blobs its documents and skills name.
type RoomSetup struct {
	From      *Provenance
	Agenda    string
	Personas  []StarterPersona
	Skills    []StarterSkill
	Documents []StarterDocument
	Files     map[string][]byte
	// SkillsPending holds the skills until the room's first host: those of a personal or shared
	// starter; a built-in's are approved already (006-AC13).
	SkillsPending bool
	Chain         StarterChain
	Glossary      string
	// UploadQuota bounds the uploads, as the room's quota bounds them later (004-AC9).
	UploadQuota int64
}

// blobBytes is what the documents and skills setup copies weigh; 0 for no setup.
func (setup *RoomSetup) blobBytes() int64 {
	if setup == nil {
		return 0
	}
	var n int64
	for _, d := range setup.Documents {
		n += int64(len(setup.Files[d.File])) + int64(len(d.Text))
	}
	for _, k := range setup.Skills {
		n += int64(len(setup.Files[k.File]))
	}
	return n
}

// setUp fills room r, just created by participant by, with setup's copies.
func setUp(ctx context.Context, tx *sql.Tx, r *Room, by *Participant, setup *RoomSetup) error {
	var from any
	if setup.From != nil {
		b, err := json.Marshal(setup.From)
		if err != nil {
			return fmt.Errorf("set up room: %w", err)
		}
		from = string(b)
		r.CreatedFrom = setup.From
	}
	r.Chain = Chain{Budget: max(setup.Chain.Budget, 1), Suspended: setup.Chain.Suspended}
	r.ParallelTurns = setup.Chain.Parallel
	if _, err := tx.ExecContext(ctx, `UPDATE rooms SET created_from = ?, chain_budget = ?, chain_suspended = ?, parallel_turns = ?, stt_glossary = ? WHERE id = ?`,
		from, r.Chain.Budget, r.Chain.Suspended, r.ParallelTurns, setup.Glossary, r.ID); err != nil {
		return fmt.Errorf("set up room: %w", err)
	}
	skills := map[string][2]int64{}
	for _, k := range setup.Skills {
		data, ok := setup.Files[k.File]
		if !ok {
			return fmt.Errorf("set up room: skill %s: no file %q", k.Name, k.File)
		}
		var id int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO skills(name, created_at, room_id, pending, from_starter) VALUES(?,?,?,?,?) RETURNING id`,
			k.Name, now(), r.ID, setup.SkillsPending, setup.SkillsPending).Scan(&id); err != nil {
			return fmt.Errorf("set up room: skill %s: %w", k.Name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO skill_versions(skill_id, n, author, description, files, size, sha256, data, created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			id, k.Version, k.Author, k.Description, k.Files, k.Size, k.SHA256, data, now()); err != nil {
			return fmt.Errorf("set up room: skill %s: %w", k.Name, err)
		}
		skills[k.Name] = [2]int64{id, int64(k.Version)}
	}
	for _, p := range setup.Personas {
		var id int64
		err := tx.QueryRowContext(ctx, `INSERT INTO room_personas(room_id, template_version, name, badge, colour, text, model, effort, every_n, self_call,
				autocompact, every_voice, every_minutes, created_at, mark_msg_id, mark_at_ms)
			VALUES(?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, `+markNow+`) RETURNING id`,
			append(append([]any{r.ID}, p.args()...), nullInt(p.EveryN), p.SelfCall, p.Autocompact, nullInt(p.EveryVoice), nullInt(p.EveryMinutes),
				now(), time.Now().UnixMilli())...).Scan(&id)
		if err != nil {
			return fmt.Errorf("set up room: persona %s: %w", p.Name, err)
		}
		for _, name := range p.Skills {
			k, ok := skills[name]
			if !ok {
				return fmt.Errorf("set up room: persona %s: no skill %q", p.Name, name)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO room_persona_skills(persona_id, skill_id, version) VALUES(?,?,?)`, id, k[0], k[1]); err != nil {
				return fmt.Errorf("set up room: persona %s: %w", p.Name, err)
			}
		}
	}
	var uploaded int64
	for _, d := range setup.Documents {
		if d.Kind == DocAgent {
			if _, err := createAgentDocument(ctx, tx, r.ID, d.Name, d.Format, d.Text, by.Name); err != nil {
				return err
			}
			continue
		}
		data, ok := setup.Files[d.File]
		if !ok {
			return fmt.Errorf("set up room: document %s: no file %q", d.Name, d.File)
		}
		if uploaded += int64(len(data)); uploaded > setup.UploadQuota {
			return fmt.Errorf("%w: the starter's documents take more than the room's %s", ErrCreationRefused, mb(setup.UploadQuota))
		}
		ts := now()
		var upID int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO uploads(room_id, participant_id, filename, content_type, size, data, created_at) VALUES(?,?,?,?,?,?,?) RETURNING id`,
			r.ID, by.ID, d.Name, d.ContentType, len(data), data, ts).Scan(&upID); err != nil {
			return fmt.Errorf("set up room: document %s: %w", d.Name, err)
		}
		doc := &Document{RoomID: r.ID, Kind: DocUpload, Name: d.Name, Format: d.Format, Author: by.Name}
		if err := insertDocument(ctx, tx, doc, upID, ts); err != nil {
			return err
		}
		if d.Format == "md" || d.Format == "html" {
			if _, err := insertRevision(ctx, tx, doc.ID, 1, d.Text, by.Name); err != nil {
				return err
			}
		}
	}
	if setup.Agenda != "" {
		if _, err := createAgentDocument(ctx, tx, r.ID, AgendaName, "md", setup.Agenda, by.Name); err != nil {
			return err
		}
	}
	if setup.From == nil {
		return nil
	}
	_, err := memberLine(ctx, tx, r.ID, by, fmt.Sprintf("created the room from the starter %q v%d.", setup.From.Title, setup.From.Version))
	return err
}

// ReleaseStarterSkills turns on the skills a starter brought into room once its host is one who
// approves without asking, and returns their names; under "approve everything" they stay for the
// host to decide (006-AC13).
func (s *Store) ReleaseStarterSkills(ctx context.Context, roomID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `UPDATE skills SET pending = 0, from_starter = 0 WHERE room_id = ? AND from_starter = 1 AND pending = 1
		AND EXISTS (SELECT 1 FROM rooms r JOIN daemons d ON d.participant_id = r.host_participant_id WHERE r.id = ? AND d.approve_all = 0)
		RETURNING name`, roomID, roomID)
	if err != nil {
		return nil, fmt.Errorf("release starter skills: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("release starter skills: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
