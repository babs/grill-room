package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	pluginpkg "github.com/babs/grill-room/internal/plugin"
)

// MaxStarterTitle bounds a starter's title (006-AC15).
const MaxStarterTitle = 60

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
// (006-AC11). The caller validated it; Files hold the blobs its documents, skills and plugins name.
type RoomSetup struct {
	From     *Provenance
	Agenda   string
	Personas []StarterPersona
	Skills   []StarterSkill
	// Plugins land waiting for the AI host's inspection; then the host's mode decides, as for
	// a plugin a room admin adds (009-AC28, 006-AC13).
	Plugins   []StarterPlugin
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

// blobBytes is what the documents and skills setup copies weigh; 0 for no setup. The plugins it
// keeps are setUp's to count, as it may leave some out.
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

// setUp fills room r, just created by participant by, with setup's copies, and returns the bytes
// of the plugin snapshots it kept.
func setUp(ctx context.Context, tx *sql.Tx, r *Room, by *Participant, setup *RoomSetup) (int64, error) {
	var from any
	if setup.From != nil {
		b, err := json.Marshal(setup.From)
		if err != nil {
			return 0, fmt.Errorf("set up room: %w", err)
		}
		from = string(b)
		r.CreatedFrom = setup.From
	}
	r.Chain = Chain{Budget: max(setup.Chain.Budget, 1), Suspended: setup.Chain.Suspended}
	r.ParallelTurns = setup.Chain.Parallel
	if _, err := tx.ExecContext(ctx, `UPDATE rooms SET created_from = ?, chain_budget = ?, chain_suspended = ?, parallel_turns = ?, stt_glossary = ? WHERE id = ?`,
		from, r.Chain.Budget, r.Chain.Suspended, r.ParallelTurns, setup.Glossary, r.ID); err != nil {
		return 0, fmt.Errorf("set up room: %w", err)
	}
	skills := map[string][2]int64{}
	for _, k := range setup.Skills {
		data, ok := setup.Files[k.File]
		if !ok {
			return 0, fmt.Errorf("set up room: skill %s: no file %q", k.Name, k.File)
		}
		var id int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO skills(name, created_at, room_id, pending, from_starter) VALUES(?,?,?,?,?) RETURNING id`,
			k.Name, now(), r.ID, setup.SkillsPending, setup.SkillsPending).Scan(&id); err != nil {
			return 0, fmt.Errorf("set up room: skill %s: %w", k.Name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO skill_versions(skill_id, n, author, description, files, size, sha256, data, created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			id, k.Version, k.Author, k.Description, k.Files, k.Size, k.SHA256, data, now()); err != nil {
			return 0, fmt.Errorf("set up room: skill %s: %w", k.Name, err)
		}
		skills[k.Name] = [2]int64{id, int64(k.Version)}
	}
	plugins, kept, left, err := setUpPlugins(ctx, tx, r.ID, by, setup)
	if err != nil {
		return 0, err
	}
	for _, p := range setup.Personas {
		var id int64
		err := tx.QueryRowContext(ctx, `INSERT INTO room_personas(room_id, template_version, name, badge, colour, text, model, effort, every_n, self_call,
				autocompact, every_voice, every_minutes, watches_notes, created_at, mark_msg_id, mark_at_ms)
			VALUES(?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, `+markNow+`) RETURNING id`,
			append(append([]any{r.ID}, p.args()...), nullInt(p.EveryN), p.SelfCall, p.Autocompact, nullInt(p.EveryVoice), nullInt(p.EveryMinutes),
				p.WatchesNotes, now(), time.Now().UnixMilli())...).Scan(&id)
		if err != nil {
			return 0, fmt.Errorf("set up room: persona %s: %w", p.Name, err)
		}
		for _, name := range p.Skills {
			k, ok := skills[name]
			if !ok {
				return 0, fmt.Errorf("set up room: persona %s: no skill %q", p.Name, name)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO room_persona_skills(persona_id, skill_id, version) VALUES(?,?,?)`, id, k[0], k[1]); err != nil {
				return 0, fmt.Errorf("set up room: persona %s: %w", p.Name, err)
			}
		}
		for _, name := range p.Plugins {
			pid, ok := plugins[name]
			if !ok {
				continue // left out by setUpPlugins, or never in the starter
			}
			if err := activatePlugin(ctx, tx, id, pid, name); err != nil {
				return 0, fmt.Errorf("set up room: persona %s: %w", p.Name, err)
			}
		}
	}
	// The plugins' snapshots count in the room's quota, as an upload does (009-AC9).
	uploaded := kept
	for _, d := range setup.Documents {
		if d.Kind == DocAgent {
			if _, err := createAgentDocument(ctx, tx, r.ID, d.Name, d.Format, d.Text, by.Name); err != nil {
				return 0, err
			}
			continue
		}
		data, ok := setup.Files[d.File]
		if !ok {
			return 0, fmt.Errorf("set up room: document %s: no file %q", d.Name, d.File)
		}
		uploaded += int64(len(data))
		ts := now()
		var upID int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO uploads(room_id, participant_id, filename, content_type, size, data, created_at) VALUES(?,?,?,?,?,?,?) RETURNING id`,
			r.ID, by.ID, d.Name, d.ContentType, len(data), data, ts).Scan(&upID); err != nil {
			return 0, fmt.Errorf("set up room: document %s: %w", d.Name, err)
		}
		doc := &Document{RoomID: r.ID, Kind: DocUpload, Name: d.Name, Format: d.Format, Author: by.Name}
		if err := insertDocument(ctx, tx, doc, upID, ts); err != nil {
			return 0, err
		}
		if d.Format == "md" || d.Format == "html" {
			if _, err := insertRevision(ctx, tx, doc.ID, 1, d.Text, by.Name); err != nil {
				return 0, err
			}
		}
	}
	if uploaded > setup.UploadQuota {
		return 0, fmt.Errorf("%w: the starter's documents and plugins take more than the room's %s", ErrCreationRefused, mb(setup.UploadQuota))
	}
	if setup.Agenda != "" {
		if _, err := createAgentDocument(ctx, tx, r.ID, AgendaName, "md", setup.Agenda, by.Name); err != nil {
			return 0, err
		}
	}
	if setup.From == nil {
		return kept, nil
	}
	if _, err = memberLine(ctx, tx, r.ID, by, fmt.Sprintf("created the room from the starter %q v%d.", setup.From.Title, setup.From.Version)); err != nil {
		return 0, err
	}
	for _, why := range left {
		m := &Message{RoomID: r.ID, Kind: KindSystem, ParticipantID: by.ID, Sender: by.Name, Body: why}
		if err := insertMessage(ctx, tx, m); err != nil {
			return 0, err
		}
	}
	return kept, nil
}

// setUpPlugins adds setup's plugins to room at their version 1, waiting for the AI host's
// inspection, and returns their ids by name with the bytes they take. Each snapshot is read again,
// as an import reads it: the manifest and file list come from its files. A plugin past the
// instance's current cap, the room's plugin limit or its quota is left out, and the personas go
// without it: a starter's plugin never fails the room's creation (006-AC13). left holds a chat
// line for each.
func setUpPlugins(ctx context.Context, tx *sql.Tx, roomID string, by *Participant, setup *RoomSetup) (ids map[string]int64, kept int64, left []string, err error) {
	ids = map[string]int64{}
	if len(setup.Plugins) == 0 {
		return ids, 0, nil, nil
	}
	set, err := instanceSettings(ctx, tx)
	if err != nil {
		return nil, 0, nil, err
	}
	leave := func(name, why string) {
		left = append(left, fmt.Sprintf("The room starts without the starter's plugin %s: %s.", name, why))
	}
	for _, p := range setup.Plugins {
		data, ok := setup.Files[p.File]
		if !ok {
			return nil, 0, nil, fmt.Errorf("set up room: plugin %s: no file %q", p.Name, p.File)
		}
		if len(ids) == MaxRoomPlugins {
			leave(p.Name, fmt.Sprintf("a room holds at most %d plugins", MaxRoomPlugins))
			continue
		}
		if kept+int64(len(data)) > setup.UploadQuota {
			leave(p.Name, "it would take the room past its quota")
			continue
		}
		sn, err := pluginpkg.ValidateTar(bytes.NewReader(data), int64(set.PluginMaxMB)<<20)
		var m *pluginpkg.Manifest
		if err == nil {
			m, err = sn.Manifest()
		}
		if err != nil {
			leave(p.Name, err.Error())
			continue
		}
		files, _ := json.Marshal(sn.Files) // plain structs: cannot fail
		manifest, _ := json.Marshal(m)     // idem
		var id int64
		err = tx.QueryRowContext(ctx, `INSERT INTO room_plugins(room_id, name, source_url, ref, ref_name, entry, private, created_by_name, created_at)
			VALUES(?,?,?,?,?,?,?,?,?) RETURNING id`, roomID, p.Name, p.URL, p.Ref, p.RefName, p.Entry, p.Private, by.Name, now()).Scan(&id)
		if err != nil {
			return nil, 0, nil, fmt.Errorf("set up room: plugin %s: %w", p.Name, err)
		}
		v := PluginSnapshot{Commit: p.Commit, SHA256: sn.SHA256, Size: sn.Size, Files: len(sn.Files), FileList: files, Manifest: manifest, Data: sn.Tar}
		if err := insertPluginVersion(ctx, tx, id, 1, v, by); err != nil {
			return nil, 0, nil, err
		}
		// Files an import brought stay the archive's word through a starter (009-AC27).
		if p.Imported {
			if _, err := tx.ExecContext(ctx, `UPDATE plugin_versions SET hold = 1 WHERE plugin_id = ?`, id); err != nil {
				return nil, 0, nil, fmt.Errorf("set up room: plugin %s: %w", p.Name, err)
			}
		}
		ids[p.Name] = id
		kept += int64(len(sn.Tar))
	}
	return ids, kept, left, nil
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

// RoomDraft is room as a starter would hold it, for "Save as starter" (006-AC15): its name, its
// agenda.md, its personas with their triggers, active skills and plugins, the skills those use and
// the room's own, its plugins at their running version, its documents at their current revision,
// its auto-turn settings and words to recognise. Personas and skills waiting for the host are left
// out. Messages, transcripts,
// sessions, participants, invites, annotations and older revisions never are part of it
// (006-AC17). RoomDraftFiles reads the blobs it names.
func (s *Store) RoomDraft(ctx context.Context, roomID string) (*StarterManifest, error) {
	r, err := s.Room(ctx, roomID)
	if err != nil {
		return nil, err
	}
	glossary, err := s.STTGlossary(ctx, roomID)
	if err != nil {
		return nil, err
	}
	title := r.Name
	if rs := []rune(title); len(rs) > MaxStarterTitle {
		title = string(rs[:MaxStarterTitle])
	}
	m := &StarterManifest{Title: title, RoomName: r.Name, Fields: []StarterField{}, Personas: []StarterPersona{}, Skills: []StarterSkill{},
		Documents: []StarterDocument{}, Chain: StarterChain{Budget: r.Chain.Budget, Suspended: r.Chain.Suspended, Parallel: r.ParallelTurns}, Glossary: glossary}
	// Skills: those the personas have on, at their pins, then the room's own; approved ones only.
	rows, err := s.db.QueryContext(ctx, `SELECT v.id, s.name, v.n, v.author, v.description, v.files, v.size, v.sha256 FROM (
			SELECT k.skill_id, k.version, 0 AS o, p.id AS pid FROM room_persona_skills k JOIN room_personas p ON p.id = k.persona_id WHERE p.room_id = ? AND p.pending = 0
			UNION ALL SELECT s.id, (SELECT MAX(n) FROM skill_versions WHERE skill_id = s.id), 1, 0 FROM skills s WHERE s.room_id = ? AND NOT s.pending
		) x JOIN skills s ON s.id = x.skill_id AND NOT s.pending JOIN skill_versions v ON v.skill_id = x.skill_id AND v.n = x.version ORDER BY x.o, x.pid, s.name`,
		roomID, roomID)
	if err != nil {
		return nil, fmt.Errorf("room draft skills: %w", err)
	}
	for rows.Next() {
		var k StarterSkill
		var vid int64
		if err := rows.Scan(&vid, &k.Name, &k.Version, &k.Author, &k.Description, &k.Files, &k.Size, &k.SHA256); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("room draft skills: %w", err)
		}
		// A name the personas share keeps its first version: a persona names its skills.
		if slices.ContainsFunc(m.Skills, func(x StarterSkill) bool { return x.Name == k.Name }) {
			continue
		}
		k.File = fmt.Sprintf("skill/%d", vid)
		m.Skills = append(m.Skills, k)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("room draft skills: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("room draft skills: %w", err)
	}
	// Plugins at their running version: one with none has nothing a room could load (009-AC28).
	rps, err := s.RoomPlugins(ctx, roomID)
	if err != nil {
		return nil, err
	}
	on := map[int64][]string{}
	m.Plugins = []StarterPlugin{}
	for _, p := range rps {
		v := p.Running()
		if v == nil {
			continue
		}
		m.Plugins = append(m.Plugins, StarterPlugin{Name: p.Name, URL: p.URL, Ref: p.Ref, RefName: p.RefName, Entry: p.Entry, Private: p.Private,
			Commit: v.Commit, SHA256: v.SHA256, Size: v.Size, Files: v.Files, File: fmt.Sprintf("plugin/%d", v.ID), Imported: v.Imported})
		for _, pid := range p.Personas {
			on[pid] = append(on[pid], p.Name)
		}
	}
	ps, err := s.RoomPersonas(ctx, roomID)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		if p.Pending {
			continue
		}
		sp := StarterPersona{PersonaFields: p.PersonaFields, EveryN: p.EveryN, EveryVoice: p.EveryVoice, EveryMinutes: p.EveryMinutes,
			SelfCall: p.SelfCall, WatchesNotes: p.WatchesNotes, Autocompact: p.Autocompact, Plugins: on[p.ID]}
		for _, k := range p.Skills {
			// A pending room skill is not in the draft: the persona goes without it.
			if slices.ContainsFunc(m.Skills, func(x StarterSkill) bool { return x.Name == k.Name }) {
				sp.Skills = append(sp.Skills, k.Name)
			}
		}
		m.Personas = append(m.Personas, sp)
	}
	docs, err := s.Documents(ctx, roomID)
	if err != nil {
		return nil, err
	}
	for _, d := range docs {
		var text string
		if d.Revisions > 0 {
			rev, err := s.Revision(ctx, d.ID, d.Revisions)
			if err != nil {
				return nil, err
			}
			text = rev.Body
		}
		if d.Kind == DocAgent && d.Name == AgendaName {
			m.Agenda = text
			continue
		}
		sd := StarterDocument{Name: d.Name, Kind: d.Kind, Format: d.Format, ContentType: d.ContentType, Text: text, Size: d.Size}
		if d.Kind == DocUpload {
			sd.File = fmt.Sprintf("doc/%d", d.ID)
		}
		m.Documents = append(m.Documents, sd)
	}
	return m, nil
}

// RoomDraftFiles reads the blobs of room's draft that names names: an upload's original, a skill
// version's tar or a plugin version's snapshot. A name the room does not hold is ErrNotFound.
func (s *Store) RoomDraftFiles(ctx context.Context, roomID string, names []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, name := range names {
		kind, id, ok := strings.Cut(name, "/")
		n, err := strconv.ParseInt(id, 10, 64)
		if !ok || err != nil {
			return nil, ErrNotFound
		}
		var data []byte
		switch kind {
		case "doc":
			data, err = s.Original(ctx, roomID, n)
		case "plugin":
			err = s.db.QueryRowContext(ctx, `SELECT sn.data FROM plugin_snapshots sn JOIN plugin_versions v ON v.id = sn.version_id
				JOIN room_plugins p ON p.id = v.plugin_id WHERE v.id = ? AND p.room_id = ? AND p.removed_at IS NULL`, n, roomID).Scan(&data)
			if errors.Is(err, sql.ErrNoRows) {
				err = ErrNotFound
			}
		case "skill":
			err = s.db.QueryRowContext(ctx, `SELECT v.data FROM skill_versions v JOIN skills k ON k.id = v.skill_id WHERE v.id = ?
				AND (k.room_id = ? OR v.id IN (SELECT sv.id FROM room_persona_skills ps JOIN room_personas p ON p.id = ps.persona_id
					JOIN skill_versions sv ON sv.skill_id = ps.skill_id AND sv.n = ps.version WHERE p.room_id = ?))`, n, roomID, roomID).Scan(&data)
			if errors.Is(err, sql.ErrNoRows) {
				err = ErrNotFound
			}
		default:
			err = ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		out[name] = data
	}
	return out, nil
}
