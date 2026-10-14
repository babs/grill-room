package store

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// A room export is a directory of files: JSON for the room, JSON Lines for its rows, and one file
// per blob or text body, so that no row of unbounded size lives in a JSON line and no step holds
// the room in memory (004-AC11). docs/export-format.md is the reference. Refs are the exporting
// instance's row ids: an import maps each to a new id.
const (
	fileRoom               = "room.json"
	fileParticipants       = "participants.jsonl"
	fileMessages           = "messages.jsonl"
	fileMessageVersions    = "message_versions.jsonl"
	fileTranscript         = "transcript.md"
	fileUploads            = "uploads.jsonl"
	fileDocuments          = "documents.jsonl"
	fileRevisions          = "revisions.jsonl"
	fileAnnotations        = "annotations.jsonl"
	fileAnnotationMessages = "annotation_messages.jsonl"
	fileAttachments        = "attachments.jsonl"
	fileSkills             = "skills.jsonl"
	fileSkillVersions      = "skill_versions.jsonl"
	fileSessions           = "sessions.jsonl"
	fileMentions           = "mentions.jsonl"
	fileNoteMentions       = "note_mentions.jsonl"
	filePlugins            = "plugins.jsonl"
	filePluginVersions     = "plugin_versions.jsonl"
)

type xRoom struct {
	Name          string `json:"name"`
	CreatedAt     string `json:"created_at"`
	Chain         Chain  `json:"chain"`
	ParallelTurns bool   `json:"parallel_turns"`
	STTGlossary   string `json:"stt_glossary,omitempty"`
	// CreatedFrom is optional: an archive from before 006 has none, and the format stays (006-AC11).
	CreatedFrom *Provenance `json:"created_from,omitempty"`
	Personas    []xPersona  `json:"personas"`
}

type xPersona struct {
	Ref int64 `json:"ref"`
	PersonaFields
	// Template is the library template the persona was copied from: its ref, and its preset key
	// for a shipped one, which every instance has.
	Template        int64  `json:"template,omitempty"`
	TemplatePreset  string `json:"template_preset,omitempty"`
	TemplateVersion int    `json:"template_version"`
	EveryN          int    `json:"every_n,omitempty"`
	EveryVoice      int    `json:"every_voice,omitempty"`
	EveryMinutes    int    `json:"every_minutes,omitempty"`
	SelfCall        bool   `json:"self_call"`
	// WatchesNotes is optional: an archive from before 009 has none, and the format stays (009-AC31).
	WatchesNotes bool   `json:"watches_notes,omitempty"`
	Autocompact  int    `json:"autocompact"`
	CreatedAt    string `json:"created_at"`
	Skills       []xPin `json:"skills"`
	// Plugins are the refs of the room plugins the persona has on (009-AC26, format 3).
	Plugins []int64  `json:"plugins,omitempty"`
	Resume  *xResume `json:"resume,omitempty"`
}

// xPin is a skill a persona has on, at a version.
type xPin struct {
	Skill   int64 `json:"skill"`
	Version int   `json:"version"`
}

// xResume is where a persona's next turn picks up: its last successful turn's session and cursors.
type xResume struct {
	SessionID        string `json:"session_id"`
	MessageCursor    int64  `json:"message_cursor"`
	AnnotationCursor int64  `json:"annotation_cursor"`
	ChangeCursor     int64  `json:"change_cursor"`
	Model            string `json:"model,omitempty"`
	Effort           string `json:"effort,omitempty"`
	EndedAt          string `json:"ended_at"`
}

type xParticipant struct {
	Ref        int64  `json:"ref"`
	Name       string `json:"name"`
	JoinedAt   string `json:"joined_at"`
	CanTrigger bool   `json:"can_trigger"`
	// Active is false for a participant whose invite was revoked: it signs old lines, nothing more.
	Active      bool   `json:"active"`
	ProfileUUID string `json:"profile_uuid,omitempty"`
	Admin       bool   `json:"admin,omitempty"`
	// PreviousName is the name before the latest rename (009-AC36).
	PreviousName string `json:"previous_name,omitempty"`
}

type xMessage struct {
	Ref         int64   `json:"ref"`
	TS          string  `json:"ts"`
	Kind        string  `json:"kind"`
	Participant int64   `json:"participant,omitempty"`
	Persona     int64   `json:"persona,omitempty"`
	Sender      string  `json:"sender"`
	Body        string  `json:"body"`
	Model       string  `json:"model,omitempty"`
	ModelNote   string  `json:"model_note,omitempty"`
	Mismatch    bool    `json:"model_mismatch,omitempty"`
	Effort      string  `json:"effort,omitempty"`
	ReqModel    string  `json:"requested_model,omitempty"`
	ReqEffort   string  `json:"requested_effort,omitempty"`
	EditedAt    *string `json:"edited_at,omitempty"`
	DeletedAt   *string `json:"deleted_at,omitempty"`
	ChangeSeq   int64   `json:"change_seq,omitempty"`
}

type xMessageVersion struct {
	Message   int64  `json:"message"`
	Seq       int64  `json:"seq"`
	Body      string `json:"body"`
	WrittenAt string `json:"written_at"`
}

type xUpload struct {
	Ref         int64  `json:"ref"`
	Participant int64  `json:"participant,omitempty"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	CreatedAt   string `json:"created_at"`
	File        string `json:"file"`
}

type xDocument struct {
	Ref       int64  `json:"ref"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Format    string `json:"format"`
	Upload    int64  `json:"upload,omitempty"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
	// DeletedAt flags a document a room admin deleted (005-AC26).
	DeletedAt string `json:"deleted_at,omitempty"`
}

// xMention is how a line's @mention of a persona ended (005-AC4).
type xMention struct {
	Message int64  `json:"message"`
	Persona int64  `json:"persona"`
	Status  string `json:"status"`
	Line    int64  `json:"line,omitempty"`
}

type xRevision struct {
	Document  int64  `json:"document"`
	N         int    `json:"n"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
	File      string `json:"file"`
}

type xAnnotation struct {
	Ref       int64           `json:"ref"`
	Document  int64           `json:"document"`
	Revision  int             `json:"revision"`
	Anchor    json.RawMessage `json:"anchor"`
	Orphaned  bool            `json:"orphaned"`
	Status    string          `json:"status"`
	Version   int             `json:"version"`
	CreatedAt string          `json:"created_at"`
}

type xAnnotationMessage struct {
	Ref         int64  `json:"ref"`
	Annotation  int64  `json:"annotation"`
	By          string `json:"by"`
	Participant int64  `json:"participant,omitempty"`
	Persona     int64  `json:"persona,omitempty"`
	Author      string `json:"author"`
	Body        string `json:"body"`
	CreatedAt   string `json:"created_at"`
}

type xAttachment struct {
	Ref         int64  `json:"ref"`
	Message     int64  `json:"message"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	CreatedAt   string `json:"created_at"`
	File        string `json:"file"`
}

type xSkill struct {
	Ref  int64  `json:"ref"`
	Name string `json:"name"`
	// Scope is the skill's scope on the exporting instance; a shared or personal one carries only
	// the versions the room's personas pin.
	Scope     string `json:"scope"`
	Pending   bool   `json:"pending,omitempty"`
	CreatedAt string `json:"created_at"`
}

type xSkillVersion struct {
	Skill       int64  `json:"skill"`
	N           int    `json:"n"`
	Author      string `json:"author"`
	Description string `json:"description"`
	Files       int    `json:"files"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	CreatedAt   string `json:"created_at"`
	File        string `json:"file"`
}

// xPlugin is a room plugin (009-AC26). GitRef is the ref its admin typed, "" for the default
// branch: Ref is the row's ref, as in every file.
type xPlugin struct {
	Ref       int64  `json:"ref"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	GitRef    string `json:"git_ref"`
	RefName   string `json:"ref_name"`
	Entry     string `json:"entry,omitempty"`
	Private   bool   `json:"private,omitempty"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
}

// xPluginVersion is one version of a plugin. File is empty once the version's snapshot was
// dropped, superseded by a newer approved one.
type xPluginVersion struct {
	Plugin    int64           `json:"plugin"`
	N         int             `json:"n"`
	Commit    string          `json:"commit"`
	SHA256    string          `json:"sha256"`
	Size      int64           `json:"size"`
	Files     int             `json:"files"`
	FileList  json.RawMessage `json:"file_list"`
	Manifest  json.RawMessage `json:"manifest"`
	Report    json.RawMessage `json:"report,omitempty"`
	State     string          `json:"state"`
	Reason    string          `json:"reason,omitempty"`
	AddedBy   string          `json:"added_by"`
	AddedAt   string          `json:"added_at"`
	DecidedBy string          `json:"decided_by,omitempty"`
	DecidedAt string          `json:"decided_at,omitempty"`
	File      string          `json:"file,omitempty"`
}

type xSession struct {
	Persona          int64   `json:"persona"`
	SessionID        string  `json:"session_id"`
	UpdatedAt        string  `json:"updated_at"`
	ContextTokens    *int64  `json:"context_tokens,omitempty"`
	ContextWindow    *int64  `json:"context_window,omitempty"`
	ContextUpdatedAt *string `json:"context_updated_at,omitempty"`
	Size             int64   `json:"size"`
	File             string  `json:"file"`
}

// exportDir writes an export's files under one directory.
type exportDir struct {
	dir   string
	files []*os.File
}

func (d *exportDir) create(name string) (*os.File, error) {
	p := filepath.Join(d.dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, fmt.Errorf("export %s: %w", name, err)
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // name is one of this file's own
	if err != nil {
		return nil, fmt.Errorf("export %s: %w", name, err)
	}
	d.files = append(d.files, f)
	return f, nil
}

// blob writes data as file name.
func (d *exportDir) blob(name string, data []byte) error {
	f, err := d.create(name)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("export %s: %w", name, err)
	}
	return nil
}

// lines opens a JSON Lines file.
func (d *exportDir) lines(name string) (*jsonLines, error) {
	f, err := d.create(name)
	if err != nil {
		return nil, err
	}
	w := bufio.NewWriter(f)
	return &jsonLines{name: name, w: w, enc: json.NewEncoder(w)}, nil
}

func (d *exportDir) close() error {
	var errs []error
	for _, f := range d.files {
		errs = append(errs, f.Close())
	}
	return errors.Join(errs...)
}

type jsonLines struct {
	name string
	w    *bufio.Writer
	enc  *json.Encoder
}

func (l *jsonLines) add(v any) error {
	if err := l.enc.Encode(v); err != nil {
		return fmt.Errorf("export %s: %w", l.name, err)
	}
	return nil
}

func (l *jsonLines) flush() error {
	if err := l.w.Flush(); err != nil {
		return fmt.Errorf("export %s: %w", l.name, err)
	}
	return nil
}

// eachRow runs query in tx and hands each row to scan, so that the rows stream from SQLite.
func eachRow(ctx context.Context, tx *sql.Tx, scan func(*sql.Rows) error, query string, args ...any) error {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("export: %w", err)
	}
	return nil
}

// dump writes the rows of query to JSON Lines file name, one line per row as row reads it. row may
// write the row's blob first.
func dump(ctx context.Context, tx *sql.Tx, d *exportDir, name string, row func(*sql.Rows) (any, error), query string, args ...any) error {
	l, err := d.lines(name)
	if err != nil {
		return err
	}
	err = eachRow(ctx, tx, func(rows *sql.Rows) error {
		v, err := row(rows)
		if err != nil {
			return fmt.Errorf("export %s: %w", name, err)
		}
		return l.add(v)
	}, query, args...)
	if err != nil {
		return err
	}
	return l.flush()
}

// ExportRoom writes room roomID's export files into dir, an empty directory, from one consistent
// read of the store, and returns the room. The audit entry ctx carries is written in the same
// transaction, so that a room is exported only if that is recorded (004-AC6). Blobs are read one
// at a time, never the whole room. ErrNotFound if the room does not exist.
// ponytail: SQLite materialises one blob at a time (no incremental blob I/O in the driver), so the
// peak is the largest single upload or session, not the room.
func (s *Store) ExportRoom(ctx context.Context, roomID, dir string) (*Room, error) {
	d := &exportDir{dir: dir}
	var room *Room
	err := s.withTx(ctx, "export room", func(tx *sql.Tx) error {
		var err error
		if room, err = exportRoom(ctx, tx, roomID, d); err != nil {
			return err
		}
		return auditTx(ctx, tx, "", nil)
	})
	if cerr := d.close(); err == nil && cerr != nil {
		err = fmt.Errorf("export room: %w", cerr)
	}
	if err != nil {
		return nil, err
	}
	return room, nil
}

func exportRoom(ctx context.Context, tx *sql.Tx, roomID string, d *exportDir) (*Room, error) {
	room := &Room{ID: roomID}
	x := xRoom{Personas: []xPersona{}}
	var from sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT name, created_at, chain_budget, chain_count, chain_suspended, parallel_turns, stt_glossary, created_from FROM rooms WHERE id = ?`, roomID).
		Scan(&x.Name, &x.CreatedAt, &x.Chain.Budget, &x.Chain.Count, &x.Chain.Suspended, &x.ParallelTurns, &x.STTGlossary, &from)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("export room: %w", err)
	}
	x.CreatedFrom = parseProvenance(from)
	room.Name, room.CreatedAt, room.Chain, room.ParallelTurns, room.CreatedFrom = x.Name, parseTS(x.CreatedAt), x.Chain, x.ParallelTurns, x.CreatedFrom
	if x.Personas, err = exportPersonas(ctx, tx, roomID); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(x, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("export room: %w", err)
	}
	if err := d.blob(fileRoom, b); err != nil {
		return nil, err
	}
	for _, step := range []func(context.Context, *sql.Tx, string, *exportDir) error{
		exportParticipants, exportMessages, exportMentions, exportDocuments, exportAnnotations, exportNoteMentions, exportSkills, exportPlugins, exportSessions,
	} {
		if err := step(ctx, tx, roomID, d); err != nil {
			return nil, err
		}
	}
	return room, nil
}

// exportMentions writes how each line's @mentions ended, for the personas the export carries.
func exportMentions(ctx context.Context, tx *sql.Tx, roomID string, d *exportDir) error {
	ms, err := queryMentions(ctx, tx, mentionCols+` JOIN room_personas p ON p.id = mt.persona_id
		WHERE m.room_id = ? AND p.pending = 0 ORDER BY mt.message_id, mt.persona_id`, roomID)
	if err != nil {
		return err
	}
	l, err := d.lines(fileMentions)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if err := l.add(xMention{Message: m.MessageID, Persona: m.PersonaID, Status: m.Status, Line: m.LineID}); err != nil {
			return err
		}
	}
	return l.flush()
}

// exportNoteMentions writes how each note message's @mentions ended, as exportMentions does for
// lines, on the documents the export carries (009-AC29).
func exportNoteMentions(ctx context.Context, tx *sql.Tx, roomID string, d *exportDir) error {
	ms, err := queryMentions(ctx, tx, noteMentionCols+` JOIN room_personas p ON p.id = mt.persona_id
		WHERE a.room_id = ? AND p.pending = 0 ORDER BY mt.message_id, mt.persona_id`, roomID)
	if err != nil {
		return err
	}
	l, err := d.lines(fileNoteMentions)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if err := l.add(xMention{Message: m.MessageID, Persona: m.PersonaID, Status: m.Status, Line: m.LineID}); err != nil {
			return err
		}
	}
	return l.flush()
}

// exportPersonas lists the room's personas with their pins and resume points. A persona still
// waiting for the host's approval is left out, with the held changes: they are requests, not room
// content, and the importing room has another host.
func exportPersonas(ctx context.Context, tx *sql.Tx, roomID string) ([]xPersona, error) {
	var out []xPersona
	err := eachRow(ctx, tx, func(rows *sql.Rows) error {
		var p xPersona
		var tpl sql.NullInt64
		var preset sql.NullString
		if err := rows.Scan(&p.Ref, &tpl, &preset, &p.TemplateVersion, &p.Name, &p.Badge, &p.Colour, &p.Text, &p.Model, &p.Effort,
			&p.EveryN, &p.SelfCall, &p.Autocompact, &p.CreatedAt, &p.EveryVoice, &p.EveryMinutes, &p.WatchesNotes); err != nil {
			return fmt.Errorf("export personas: %w", err)
		}
		p.Template, p.TemplatePreset, p.Skills = tpl.Int64, preset.String, []xPin{}
		out = append(out, p)
		return nil
	}, `SELECT p.id, p.template_id, t.preset, p.template_version, p.name, p.badge, p.colour, p.text, p.model, p.effort,
		COALESCE(p.every_n, 0), p.self_call, p.autocompact, p.created_at, COALESCE(p.every_voice, 0), COALESCE(p.every_minutes, 0), p.watches_notes
		FROM room_personas p LEFT JOIN persona_templates t ON t.id = p.template_id WHERE p.room_id = ? AND p.pending = 0 ORDER BY p.id`, roomID)
	if err != nil {
		return nil, err
	}
	for i := range out {
		p := &out[i]
		err := eachRow(ctx, tx, func(rows *sql.Rows) error {
			var k xPin
			if err := rows.Scan(&k.Skill, &k.Version); err != nil {
				return fmt.Errorf("export persona skills: %w", err)
			}
			p.Skills = append(p.Skills, k)
			return nil
		}, `SELECT k.skill_id, k.version FROM room_persona_skills k JOIN skills s ON s.id = k.skill_id WHERE k.persona_id = ? ORDER BY s.name, k.skill_id`, p.Ref)
		if err != nil {
			return nil, err
		}
		err = eachRow(ctx, tx, func(rows *sql.Rows) error {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return fmt.Errorf("export persona plugins: %w", err)
			}
			p.Plugins = append(p.Plugins, id)
			return nil
		}, `SELECT k.plugin_id FROM room_persona_plugins k JOIN room_plugins g ON g.id = k.plugin_id WHERE k.persona_id = ? AND g.removed_at IS NULL
			ORDER BY k.plugin_id`, p.Ref)
		if err != nil {
			return nil, err
		}
		var r xResume
		var ended sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT session_id, message_cursor, annotation_cursor, change_cursor, model, effort, ended_at FROM turns
			WHERE room_id = ? AND persona_id = ? AND status = 'done' ORDER BY id DESC LIMIT 1`, roomID, p.Ref).
			Scan(&r.SessionID, &r.MessageCursor, &r.AnnotationCursor, &r.ChangeCursor, &r.Model, &r.Effort, &ended)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return nil, fmt.Errorf("export resume point: %w", err)
		default:
			r.EndedAt = ended.String
			p.Resume = &r
		}
	}
	return out, nil
}

func exportParticipants(ctx context.Context, tx *sql.Tx, roomID string, d *exportDir) error {
	return dump(ctx, tx, d, fileParticipants, func(rows *sql.Rows) (any, error) {
		var p xParticipant
		return p, rows.Scan(&p.Ref, &p.Name, &p.JoinedAt, &p.CanTrigger, &p.Active, &p.ProfileUUID, &p.Admin, &p.PreviousName)
	}, `SELECT p.id, p.name, p.joined_at, p.can_trigger, i.revoked_at IS NULL, COALESCE(f.uuid, p.profile_uuid_hint, ''),
		EXISTS (SELECT 1 FROM room_admins a WHERE a.room_id = p.room_id AND a.profile_id = p.profile_id), COALESCE(p.previous_name, '')
		FROM participants p JOIN invites i ON i.id = p.invite_id LEFT JOIN profiles f ON f.id = p.profile_id
		WHERE p.room_id = ? ORDER BY p.id`, roomID)
}

// exportMessages writes the lines, their earlier versions and the readable transcript in one pass
// over the lines joined with their versions.
func exportMessages(ctx context.Context, tx *sql.Tx, roomID string, d *exportDir) error {
	msgs, err := d.lines(fileMessages)
	if err != nil {
		return err
	}
	vers, err := d.lines(fileMessageVersions)
	if err != nil {
		return err
	}
	tf, err := d.create(fileTranscript)
	if err != nil {
		return err
	}
	tw := bufio.NewWriter(tf)
	var name string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM rooms WHERE id = ?`, roomID).Scan(&name); err != nil {
		return fmt.Errorf("export transcript: %w", err)
	}
	if _, err := fmt.Fprintf(tw, "# %s\n\nEarlier versions of an edited line follow it, oldest first.\n\n", name); err != nil {
		return fmt.Errorf("export transcript: %w", err)
	}
	var cur *xMessage
	var earlier []xMessageVersion
	flush := func() error {
		if cur == nil {
			return nil
		}
		if err := msgs.add(cur); err != nil {
			return err
		}
		for _, v := range earlier {
			if err := vers.add(v); err != nil {
				return err
			}
		}
		if _, err := tw.WriteString(transcriptLine(cur, earlier)); err != nil {
			return fmt.Errorf("export transcript: %w", err)
		}
		return nil
	}
	err = eachRow(ctx, tx, func(rows *sql.Rows) error {
		var m xMessage
		var pid, persona, seq sql.NullInt64
		var vbody, vwritten sql.NullString
		if err := rows.Scan(&m.Ref, &m.TS, &m.Kind, &pid, &persona, &m.Sender, &m.Body, &m.Model, &m.Effort, &m.ReqModel, &m.ReqEffort,
			&m.ModelNote, &m.Mismatch, &m.EditedAt, &m.DeletedAt, &m.ChangeSeq, &seq, &vbody, &vwritten); err != nil {
			return fmt.Errorf("export messages: %w", err)
		}
		if cur == nil || cur.Ref != m.Ref {
			if err := flush(); err != nil {
				return err
			}
			m.Participant, m.Persona = pid.Int64, persona.Int64
			cur, earlier = &m, earlier[:0]
		}
		if seq.Valid {
			earlier = append(earlier, xMessageVersion{Message: m.Ref, Seq: seq.Int64, Body: vbody.String, WrittenAt: vwritten.String})
		}
		return nil
	}, `SELECT m.id, m.ts, m.kind, m.participant_id, m.persona_id, m.sender, m.body, m.model, m.effort, m.req_model, m.req_effort,
		m.model_note, m.model_mismatch, m.edited_at, m.deleted_at, m.change_seq, v.seq, v.body, v.written_at
		FROM messages m LEFT JOIN message_versions v ON v.message_id = m.id WHERE m.room_id = ? ORDER BY m.id, v.seq`, roomID)
	if err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("export transcript: %w", err)
	}
	if err := msgs.flush(); err != nil {
		return err
	}
	return vers.flush()
}

// transcriptLine renders one line of the readable transcript: who, when, what, and what it was.
func transcriptLine(m *xMessage, earlier []xMessageVersion) string {
	var b strings.Builder
	state := ""
	switch {
	case m.DeletedAt != nil:
		state = " (deleted " + *m.DeletedAt + ")"
	case m.EditedAt != nil:
		state = " (edited " + *m.EditedAt + ")"
	}
	fmt.Fprintf(&b, "- **%s** [%s] %s%s:\n%s\n", m.Sender, m.Kind, m.TS, state, indent(m.Body, "  "))
	for _, v := range earlier {
		fmt.Fprintf(&b, "  - earlier, %s:\n%s\n", v.WrittenAt, indent(v.Body, "    "))
	}
	return b.String()
}

func indent(s, pad string) string {
	return pad + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n"+pad)
}

func exportDocuments(ctx context.Context, tx *sql.Tx, roomID string, d *exportDir) error {
	err := dump(ctx, tx, d, fileUploads, func(rows *sql.Rows) (any, error) {
		var u xUpload
		var pid sql.NullInt64
		var data sql.RawBytes
		err := rows.Scan(&u.Ref, &pid, &u.Filename, &u.ContentType, &u.Size, &u.CreatedAt, &data)
		u.Participant, u.File = pid.Int64, "uploads/"+strconv.FormatInt(u.Ref, 10)
		if err == nil {
			err = d.blob(u.File, data)
		}
		return u, err
	}, `SELECT id, participant_id, filename, content_type, size, created_at, data FROM uploads WHERE room_id = ? ORDER BY id`, roomID)
	if err == nil {
		err = dump(ctx, tx, d, fileDocuments, func(rows *sql.Rows) (any, error) {
			var x xDocument
			var up sql.NullInt64
			var deleted sql.NullString
			err := rows.Scan(&x.Ref, &x.Kind, &x.Name, &x.Format, &up, &x.Author, &x.CreatedAt, &deleted)
			x.Upload, x.DeletedAt = up.Int64, deleted.String
			return x, err
		}, `SELECT id, kind, name, format, upload_id, author, created_at, deleted_at FROM documents WHERE room_id = ? ORDER BY id`, roomID)
	}
	if err == nil {
		err = dump(ctx, tx, d, fileRevisions, func(rows *sql.Rows) (any, error) {
			var x xRevision
			var format string
			var body sql.RawBytes
			err := rows.Scan(&x.Document, &x.N, &x.Author, &x.CreatedAt, &format, &body)
			x.File = fmt.Sprintf("documents/%d/%d.%s", x.Document, x.N, format)
			if err == nil {
				err = d.blob(x.File, body)
			}
			return x, err
		}, `SELECT r.document_id, r.n, r.author, r.created_at, d.format, r.body FROM revisions r JOIN documents d ON d.id = r.document_id
			WHERE d.room_id = ? ORDER BY r.document_id, r.n`, roomID)
	}
	return err
}

func exportAnnotations(ctx context.Context, tx *sql.Tx, roomID string, d *exportDir) error {
	err := dump(ctx, tx, d, fileAnnotations, func(rows *sql.Rows) (any, error) {
		var a xAnnotation
		var anchor sql.NullString
		err := rows.Scan(&a.Ref, &a.Document, &a.Revision, &anchor, &a.Orphaned, &a.Status, &a.Version, &a.CreatedAt)
		a.Anchor = json.RawMessage("null")
		if anchor.Valid {
			a.Anchor = json.RawMessage(anchor.String)
		}
		return a, err
	}, `SELECT id, document_id, revision, anchor, orphaned, status, version, created_at FROM annotations WHERE room_id = ? ORDER BY id`, roomID)
	if err == nil {
		err = dump(ctx, tx, d, fileAnnotationMessages, func(rows *sql.Rows) (any, error) {
			var m xAnnotationMessage
			var pid, persona sql.NullInt64
			err := rows.Scan(&m.Ref, &m.Annotation, &m.By, &pid, &persona, &m.Author, &m.Body, &m.CreatedAt)
			m.Participant, m.Persona = pid.Int64, persona.Int64
			return m, err
		}, `SELECT m.id, m.annotation_id, m.author_kind, m.participant_id, m.persona_id, m.author, m.body, m.created_at
			FROM annotation_messages m JOIN annotations a ON a.id = m.annotation_id WHERE a.room_id = ? ORDER BY m.id`, roomID)
	}
	if err == nil {
		err = dump(ctx, tx, d, fileAttachments, func(rows *sql.Rows) (any, error) {
			var x xAttachment
			var data sql.RawBytes
			err := rows.Scan(&x.Ref, &x.Message, &x.ContentType, &x.Size, &x.CreatedAt, &data)
			x.File = "attachments/" + strconv.FormatInt(x.Ref, 10)
			if err == nil {
				err = d.blob(x.File, data)
			}
			return x, err
		}, `SELECT t.id, t.message_id, t.content_type, t.size, t.created_at, t.data FROM attachments t
			JOIN annotation_messages m ON m.id = t.message_id JOIN annotations a ON a.id = m.annotation_id WHERE a.room_id = ? ORDER BY t.id`, roomID)
	}
	return err
}

// exportSkills writes the room's own skills with every version, and the shared and personal
// skills its exported personas pin, with the pinned versions only.
func exportSkills(ctx context.Context, tx *sql.Tx, roomID string, d *exportDir) error {
	const pinned = `SELECT k.skill_id, k.version FROM room_persona_skills k JOIN room_personas p ON p.id = k.persona_id
		WHERE p.room_id = ? AND p.pending = 0`
	err := dump(ctx, tx, d, fileSkills, func(rows *sql.Rows) (any, error) {
		var k xSkill
		return k, rows.Scan(&k.Ref, &k.Name, &k.Scope, &k.Pending, &k.CreatedAt)
	}, `SELECT s.id, s.name, s.scope, s.pending, s.created_at FROM skills s
		WHERE s.room_id = ? OR s.id IN (SELECT skill_id FROM (`+pinned+`)) ORDER BY s.name, s.id`, roomID, roomID)
	if err != nil {
		return err
	}
	return dump(ctx, tx, d, fileSkillVersions, func(rows *sql.Rows) (any, error) {
		var v xSkillVersion
		var data sql.RawBytes
		err := rows.Scan(&v.Skill, &v.N, &v.Author, &v.Description, &v.Files, &v.Size, &v.SHA256, &v.CreatedAt, &data)
		v.File = fmt.Sprintf("skills/%d/%d.tar", v.Skill, v.N)
		if err == nil {
			err = d.blob(v.File, data)
		}
		return v, err
	}, `SELECT v.skill_id, v.n, v.author, v.description, v.files, v.size, v.sha256, v.created_at, v.data
		FROM skill_versions v JOIN skills s ON s.id = v.skill_id
		WHERE s.room_id = ? OR (v.skill_id, v.n) IN (`+pinned+`) ORDER BY s.name, v.skill_id, v.n`, roomID, roomID)
}

// exportPlugins writes the room's plugins with every version and the snapshots still held
// (009-AC26). Requests to the AI host and option values stay behind: the first are transient,
// the second never reach the server.
func exportPlugins(ctx context.Context, tx *sql.Tx, roomID string, d *exportDir) error {
	err := dump(ctx, tx, d, filePlugins, func(rows *sql.Rows) (any, error) {
		var p xPlugin
		return p, rows.Scan(&p.Ref, &p.Name, &p.URL, &p.GitRef, &p.RefName, &p.Entry, &p.Private, &p.CreatedBy, &p.CreatedAt)
	}, `SELECT id, name, source_url, ref, ref_name, entry, private, created_by_name, created_at FROM room_plugins
		WHERE room_id = ? AND removed_at IS NULL ORDER BY id`, roomID)
	if err != nil {
		return err
	}
	// Two versions of a plugin may hold the same files: one blob serves both.
	written := map[string]bool{}
	return dump(ctx, tx, d, filePluginVersions, func(rows *sql.Rows) (any, error) {
		var v xPluginVersion
		var files, manifest string
		var report, decidedBy, decidedAt sql.NullString
		var data sql.RawBytes
		err := rows.Scan(&v.Plugin, &v.N, &v.Commit, &v.SHA256, &v.Size, &v.Files, &files, &manifest, &report, &v.State, &v.Reason,
			&v.AddedBy, &v.AddedAt, &decidedBy, &decidedAt, &data)
		if err != nil {
			return v, err
		}
		v.FileList, v.Manifest, v.DecidedBy, v.DecidedAt = json.RawMessage(files), json.RawMessage(manifest), decidedBy.String, decidedAt.String
		if report.Valid {
			v.Report = json.RawMessage(report.String)
		}
		if len(data) == 0 {
			return v, nil
		}
		v.File = fmt.Sprintf("plugins/%d/%s.tar", v.Plugin, v.SHA256)
		if written[v.File] {
			return v, nil
		}
		written[v.File] = true
		return v, d.blob(v.File, data)
	}, `SELECT v.plugin_id, v.n, v.commit_sha, v.sha256, v.size, v.files, v.file_list, v.manifest, v.report, v.state, v.reason,
		v.added_by_name, v.added_at, v.decided_by_name, v.decided_at, sn.data
		FROM plugin_versions v JOIN room_plugins p ON p.id = v.plugin_id LEFT JOIN plugin_snapshots sn ON sn.version_id = v.id
		WHERE p.room_id = ? AND p.removed_at IS NULL ORDER BY v.plugin_id, v.n`, roomID)
}

func exportSessions(ctx context.Context, tx *sql.Tx, roomID string, d *exportDir) error {
	n := 0
	return dump(ctx, tx, d, fileSessions, func(rows *sql.Rows) (any, error) {
		var x xSession
		var data sql.RawBytes
		err := rows.Scan(&x.Persona, &x.SessionID, &x.UpdatedAt, &x.ContextTokens, &x.ContextWindow, &x.ContextUpdatedAt, &data)
		n++
		x.Size, x.File = int64(len(data)), fmt.Sprintf("sessions/%d/%d.jsonl", x.Persona, n)
		if err == nil {
			err = d.blob(x.File, data)
		}
		return x, err
	}, `SELECT s.persona_id, s.session_id, s.updated_at, s.context_tokens, s.context_window, s.context_updated_at, s.data
		FROM agent_sessions s JOIN room_personas p ON p.id = s.persona_id WHERE s.room_id = ? AND p.pending = 0 ORDER BY s.persona_id, s.session_id`, roomID)
}
