package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/babs/grill-room/internal/anchor"
	"github.com/babs/grill-room/internal/render"
)

// Turn statuses.
const (
	TurnRunning = "running"
	TurnDone    = "done"
	TurnFailed  = "failed"
)

// AgentName signs what a turn whose persona is gone writes. A turn normally signs with its persona's name.
const AgentName = "Agent"

var (
	// ErrTurnInProgress is returned when a room already has a running turn.
	ErrTurnInProgress = errors.New("turn in progress")
	// ErrTurnNotRunning is returned for a result on a turn that ended or belongs to another daemon.
	ErrTurnNotRunning = errors.New("the turn is no longer running")
	// ErrSessionGap is returned when a session upload does not start where the stored copy ends.
	ErrSessionGap = errors.New("session upload does not continue the stored session")
)

// Turn is one press of the agent button and its run on the host's daemon.
type Turn struct {
	ID     int64  `json:"id"`
	RoomID string `json:"room_id"`
	// ParticipantID pressed the button.
	ParticipantID int64 `json:"participant_id"`
	DaemonID      int64 `json:"-"`
	// Host names the daemon's machine, for error messages.
	Host   string `json:"host"`
	Status string `json:"status"`
	// PersonaID is the room persona the turn runs. PersonaName, PersonaText, ReqModel and ReqEffort
	// are that persona as the turn started: a change during the turn applies to the next (002-AC15).
	PersonaID   int64  `json:"persona_id,omitempty"`
	PersonaName string `json:"persona_name,omitempty"`
	PersonaText string `json:"-"`
	ReqModel    string `json:"-"`
	ReqEffort   string `json:"-"`
	// Fresh starts a new Claude session instead of resuming Resume.
	Fresh  bool   `json:"fresh"`
	Resume string `json:"-"`
	// SessionID is the session the turn ran in, set when it is done.
	SessionID string `json:"-"`
	// Model is the Claude model the turn ran on, as its daemon reported it; empty when unknown.
	// Effort is the effort it ran with; empty for Claude Code's default.
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	// The cursors are the last message and annotation message ids the turn saw: the next turn
	// reads what came after them.
	MessageCursor    int64 `json:"-"`
	AnnotationCursor int64 `json:"-"`
	// ChangeCursor is the room's last message change seq the turn saw: the next turn flags the
	// messages up to MessageCursor changed after it.
	ChangeCursor int64      `json:"-"`
	Prompt       string     `json:"-"`
	Error        string     `json:"error,omitempty"`
	Report       TurnReport `json:"report"`
	StartedAt    time.Time  `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
}

// TurnReport is what a turn changed, and what it tried to change and was refused.
type TurnReport struct {
	Revised []string `json:"revised,omitempty"`
	Created []string `json:"created,omitempty"`
	Replies int      `json:"replies,omitempty"`
	// Closed counts threads the agent marked done; Annotated, the annotations it opened.
	Closed    int `json:"closed,omitempty"`
	Annotated int `json:"annotated,omitempty"`
	// Rejected lists workspace paths of uploads the agent changed: uploads are read-only (D30).
	Rejected []string `json:"rejected,omitempty"`
	// Ignored lists changes outside the contract, with why.
	Ignored []string `json:"ignored,omitempty"`
	// Renamed lists new agent documents stored under another name than the agent gave, as "a as b".
	Renamed []string `json:"renamed,omitempty"`
}

// agentAnnotate opens the agent's annotation a on the latest revision of its document, anchored
// where the quote reads now. It returns why not, for the turn report, when the quote cannot be
// placed; an error only when the store fails.
func agentAnnotate(ctx context.Context, tx *sql.Tx, roomID string, a AgentAnnotation) (string, error) {
	a.Anchor.Kind = anchor.KindText
	if err := a.Anchor.Validate(); err != nil {
		return err.Error(), nil
	}
	var format, body string
	var n int
	err := tx.QueryRowContext(ctx, `
		SELECT d.format, r.n, r.body FROM documents d JOIN revisions r ON r.document_id = d.id
		WHERE d.id = ? AND d.room_id = ? ORDER BY r.n DESC LIMIT 1`, a.DocID, roomID).Scan(&format, &n, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return "the document has no text to quote", nil
	}
	if err != nil {
		return "", fmt.Errorf("agent annotation: %w", err)
	}
	page := []byte(body)
	if format == "md" {
		if page, err = render.MdToHTML(page); err != nil {
			return "", fmt.Errorf("agent annotation: %w", err)
		}
	}
	doc, err := anchor.Parse(page)
	if err != nil {
		return "", fmt.Errorf("agent annotation: %w", err)
	}
	// The quote itself must be there: Reanchor's fallback between prefix and suffix is for text a
	// participant anchored and a later revision reworded, not for placing a new one.
	placed, ok := doc.Reanchor(a.Anchor)
	if !ok || placed.Exact != a.Anchor.Exact {
		return "quote not found", nil
	}
	js, err := json.Marshal(placed)
	if err != nil {
		return "", fmt.Errorf("agent annotation: %w", err)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO annotations(room_id, document_id, revision, anchor, created_at) VALUES(?,?,?,?,?)`,
		roomID, a.DocID, n, string(js), now())
	if err != nil {
		return "", fmt.Errorf("agent annotation: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return "", fmt.Errorf("agent annotation: %w", err)
	}
	if _, err := insertAnnotationMessage(ctx, tx, roomID, id, &NewMessage{Author: a.author, PersonaID: a.persona, Body: a.Body}, 0); err != nil {
		return "", err
	}
	return "", nil
}

// agentDocName is the name an agent write lands under. An agent document never takes an upload's
// name, or the room would list two documents as one (spike S5): a new one gets "-agent" before
// its extension until no upload holds the name. An existing agent document keeps its name.
func agentDocName(ctx context.Context, tx *sql.Tx, roomID, name string) (string, error) {
	for {
		var agent, upload int
		err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FILTER (WHERE kind = 'agent'), COUNT(*) FILTER (WHERE kind = 'upload')
			FROM documents WHERE room_id = ? AND name = ?`, roomID, name).Scan(&agent, &upload)
		if err != nil {
			return "", fmt.Errorf("agent document name: %w", err)
		}
		if agent > 0 || upload == 0 {
			return name, nil
		}
		ext := path.Ext(name)
		name = strings.TrimSuffix(name, ext) + "-agent" + ext
	}
}

// AgentWrite is a document the agent wrote: a new revision of the agent document of that name, or
// a new document.
type AgentWrite struct {
	Name   string
	Format string
	Body   string
}

// AgentReply is the agent's message on an annotation thread; Done also closes the thread.
type AgentReply struct {
	AnnotationID int64
	Body         string
	Done         bool
}

// AgentAnnotation is an annotation the agent opens on a text quote. DocID names the document; 0
// means the agent document named AgentDoc, which the same turn may have just written.
type AgentAnnotation struct {
	Path     string
	DocID    int64
	AgentDoc string
	Anchor   anchor.Anchor
	Body     string
	// author and persona sign it, from the turn.
	author  string
	persona int64
}

// TurnOutcome is a finished turn's result, applied all at once or not at all.
type TurnOutcome struct {
	Synthesis string
	Model     string
	Effort    string
	Documents []AgentWrite
	Replies   []AgentReply
	// Annotations are anchored after Documents are written, so they may quote this turn's text.
	Annotations []AgentAnnotation
	// Session holds the session jsonl bytes from SessionOffset on.
	SessionID     string
	SessionOffset int64
	Session       []byte
	// Report carries the refusals found before the store; CompleteTurn adds what it applied.
	Report TurnReport
}

const turnCols = `SELECT id, room_id, COALESCE(participant_id, 0), COALESCE(daemon_id, 0), host, status, fresh, resume,
	session_id, model, message_cursor, annotation_cursor, change_cursor, prompt, error, report, started_at, ended_at,
	COALESCE(persona_id, 0), persona_name, persona_text, req_model, req_effort, effort FROM turns`

func scanTurn(sc scanner) (*Turn, error) {
	var t Turn
	var report, started string
	var ended sql.NullString
	err := sc.Scan(&t.ID, &t.RoomID, &t.ParticipantID, &t.DaemonID, &t.Host, &t.Status, &t.Fresh, &t.Resume,
		&t.SessionID, &t.Model, &t.MessageCursor, &t.AnnotationCursor, &t.ChangeCursor, &t.Prompt, &t.Error, &report, &started, &ended,
		&t.PersonaID, &t.PersonaName, &t.PersonaText, &t.ReqModel, &t.ReqEffort, &t.Effort)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan turn: %w", err)
	}
	if err := json.Unmarshal([]byte(report), &t.Report); err != nil {
		return nil, fmt.Errorf("turn %d report: %w", t.ID, err)
	}
	t.StartedAt = parseTS(started)
	if ended.Valid {
		e := parseTS(ended.String)
		t.EndedAt = &e
	}
	return &t, nil
}

// StartTurn records t as the room's running turn, for its persona. ErrTurnInProgress if one is
// running already, ErrNotFound when the persona is not in the room.
func (s *Store) StartTurn(ctx context.Context, t *Turn) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("start turn: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var running int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM turns WHERE room_id = ? AND status = 'running'`, t.RoomID).Scan(&running); err != nil {
		return fmt.Errorf("start turn: %w", err)
	}
	if running > 0 {
		return ErrTurnInProgress
	}
	t.Status, t.StartedAt = TurnRunning, time.Now().UTC()
	var pid, did any
	if t.ParticipantID != 0 {
		pid = t.ParticipantID
	}
	if t.DaemonID != 0 {
		did = t.DaemonID
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO turns(room_id, participant_id, daemon_id, host, status, fresh, resume, message_cursor, annotation_cursor, change_cursor, prompt, started_at,
			persona_id, persona_name, persona_text, req_model, req_effort)
		SELECT room_id, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, id, ?, ?, ?, ? FROM room_personas WHERE id = ? AND room_id = ?`,
		pid, did, t.Host, t.Status, t.Fresh, t.Resume, t.MessageCursor, t.AnnotationCursor, t.ChangeCursor, t.Prompt, t.StartedAt.Format(time.RFC3339Nano),
		t.PersonaName, t.PersonaText, t.ReqModel, t.ReqEffort, t.PersonaID, t.RoomID)
	if err != nil {
		return fmt.Errorf("start turn: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("start turn: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	if t.ID, err = res.LastInsertId(); err != nil {
		return fmt.Errorf("start turn id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("start turn: %w", err)
	}
	return nil
}

// Turn fetches turn id of room; ErrNotFound if it is not in room.
func (s *Store) Turn(ctx context.Context, roomID string, id int64) (*Turn, error) {
	return scanTurn(s.db.QueryRowContext(ctx, turnCols+` WHERE id = ? AND room_id = ?`, id, roomID))
}

// RunningTurn returns the room's running turn; ErrNotFound if none is.
func (s *Store) RunningTurn(ctx context.Context, roomID string) (*Turn, error) {
	return scanTurn(s.db.QueryRowContext(ctx, turnCols+` WHERE room_id = ? AND status = 'running'`, roomID))
}

// LastTurn returns the room's latest turn, whatever its status; ErrNotFound if none ran.
func (s *Store) LastTurn(ctx context.Context, roomID string) (*Turn, error) {
	return scanTurn(s.db.QueryRowContext(ctx, turnCols+` WHERE room_id = ? ORDER BY id DESC LIMIT 1`, roomID))
}

// LastDoneTurn returns the latest successful turn of persona personaID in room; ErrNotFound if
// none succeeded. Each persona reads the room from its own last turn on (002-AC16).
func (s *Store) LastDoneTurn(ctx context.Context, roomID string, personaID int64) (*Turn, error) {
	return scanTurn(s.db.QueryRowContext(ctx, turnCols+` WHERE room_id = ? AND persona_id = ? AND status = 'done' ORDER BY id DESC LIMIT 1`, roomID, personaID))
}

// FailTurn marks turn id failed with reason and posts it in the room, in one transaction. It
// returns the posted message, or nil when the turn was not running: then nothing changed.
func (s *Store) FailTurn(ctx context.Context, roomID string, id int64, reason string) (*Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("fail turn: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var name string
	err = tx.QueryRowContext(ctx, `UPDATE turns SET status = 'failed', error = ?, ended_at = ? WHERE id = ? AND room_id = ? AND status = 'running'
		RETURNING persona_name`, reason, now(), id, roomID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fail turn: %w", err)
	}
	name = signer(name)
	m := &Message{RoomID: roomID, Kind: KindSystem, Sender: name, Body: name + " turn failed: " + reason}
	if err := insertMessage(ctx, tx, m); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("fail turn: %w", err)
	}
	return m, nil
}

// FailRunningTurns fails every running turn, in every room, and returns the rooms touched. At
// start no daemon is attached yet, so no running turn can ever finish.
func (s *Store) FailRunningTurns(ctx context.Context, reason string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, room_id FROM turns WHERE status = 'running'`)
	if err != nil {
		return nil, fmt.Errorf("running turns: %w", err)
	}
	type ref struct {
		id   int64
		room string
	}
	var refs []ref
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.id, &r.room); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("running turns: %w", err)
		}
		refs = append(refs, r)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("running turns: %w", err)
	}
	var rooms []string
	for _, r := range refs {
		if _, err := s.FailTurn(ctx, r.room, r.id, reason); err != nil {
			return rooms, err
		}
		rooms = append(rooms, r.room)
	}
	return rooms, nil
}

// MessageCursor returns the room's last message id, 0 for none.
func (s *Store) MessageCursor(ctx context.Context, roomID string) (int64, error) {
	var id int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM messages WHERE room_id = ?`, roomID).Scan(&id); err != nil {
		return 0, fmt.Errorf("message cursor: %w", err)
	}
	return id, nil
}

// signer is the name a turn signs with: its persona's, or AgentName for a turn from before personas.
func signer(personaName string) string {
	if personaName == "" {
		return AgentName
	}
	return personaName
}

// AgentSession returns the stored jsonl of session sid of persona personaID in room; ErrNotFound
// if there is none. Sessions belong to one persona: another never resumes it (002-AC16).
func (s *Store) AgentSession(ctx context.Context, roomID string, personaID int64, sid string) ([]byte, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM agent_sessions WHERE room_id = ? AND persona_id = ? AND session_id = ?`,
		roomID, personaID, sid).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("agent session: %w", err)
	}
	return data, nil
}

// CompleteTurn applies out for turn id, run by daemonID, in one transaction: the session bytes,
// the documents as agent revisions (re-anchoring their annotations), the annotation replies, the
// synthesis message and the report, then marks the turn done. ErrTurnNotRunning when the turn ended
// or is not this daemon's, ErrSessionGap when the session bytes do not continue the stored copy.
// Nothing is kept on error. It returns the done turn.
func (s *Store) CompleteTurn(ctx context.Context, roomID string, id, daemonID int64, out *TurnOutcome) (*Turn, error) {
	if out.SessionID == "" {
		return nil, errors.New("complete turn: no session id")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("complete turn: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	t, err := scanTurn(tx.QueryRowContext(ctx, turnCols+` WHERE id = ? AND room_id = ?`, id, roomID))
	if err != nil {
		return nil, err
	}
	if t.Status != TurnRunning || t.DaemonID != daemonID {
		return nil, ErrTurnNotRunning
	}
	if err := appendSession(ctx, tx, roomID, t.PersonaID, out); err != nil {
		return nil, err
	}
	report := out.Report
	author := signer(t.PersonaName)
	renamed := map[string]string{}
	for _, w := range out.Documents {
		name, err := agentDocName(ctx, tx, roomID, w.Name)
		if err != nil {
			return nil, err
		}
		renamed[w.Name] = name
		if name != w.Name {
			report.Renamed = append(report.Renamed, w.Name+" as "+name)
			w.Name = name
		}
		var docID int64
		var latest string
		err = tx.QueryRowContext(ctx, `
			SELECT d.id, r.body FROM documents d JOIN revisions r ON r.document_id = d.id
			WHERE d.room_id = ? AND d.kind = 'agent' AND d.name = ? ORDER BY r.n DESC LIMIT 1`, roomID, w.Name).Scan(&docID, &latest)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := createAgentDocument(ctx, tx, roomID, w.Name, w.Format, w.Body, author); err != nil {
				return nil, err
			}
			report.Created = append(report.Created, w.Name)
		case err != nil:
			return nil, fmt.Errorf("complete turn: %w", err)
		case latest == w.Body:
		default:
			if _, err := addRevision(ctx, tx, roomID, docID, w.Body, author); err != nil {
				return nil, err
			}
			report.Revised = append(report.Revised, w.Name)
		}
	}
	for _, r := range out.Replies {
		if r.Done {
			// A participant who wrote after the turn read the thread has not been answered yet.
			var unseen bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM annotation_messages
				WHERE annotation_id = ? AND author_kind = 'participant' AND id > ?)`, r.AnnotationID, t.AnnotationCursor).Scan(&unseen); err != nil {
				return nil, fmt.Errorf("complete turn: %w", err)
			}
			if unseen {
				r.Done = false
				report.Ignored = append(report.Ignored, fmt.Sprintf("closing annotation %d: a participant wrote in it during the turn", r.AnnotationID))
			}
		}
		res, err := tx.ExecContext(ctx, `UPDATE annotations SET version = version + 1,
			status = CASE WHEN ? THEN 'done' ELSE status END WHERE id = ? AND room_id = ?`, r.Done, r.AnnotationID, roomID)
		if err != nil {
			return nil, fmt.Errorf("complete turn: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return nil, fmt.Errorf("complete turn: %w", err)
		} else if n == 0 {
			report.Ignored = append(report.Ignored, fmt.Sprintf("reply to annotation %d: no such annotation", r.AnnotationID))
			continue
		}
		if _, err := insertAnnotationMessage(ctx, tx, roomID, r.AnnotationID, &NewMessage{Author: author, PersonaID: t.PersonaID, Body: r.Body}, 0); err != nil {
			return nil, err
		}
		report.Replies++
		if r.Done {
			report.Closed++
		}
	}
	for _, a := range out.Annotations {
		if a.DocID == 0 {
			name := a.AgentDoc
			if n, ok := renamed[name]; ok {
				name = n
			}
			err := tx.QueryRowContext(ctx, `SELECT id FROM documents WHERE room_id = ? AND kind = 'agent' AND name = ?`, roomID, name).Scan(&a.DocID)
			if errors.Is(err, sql.ErrNoRows) {
				report.Ignored = append(report.Ignored, fmt.Sprintf("annotation on %s: no such document", a.Path))
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("complete turn: %w", err)
			}
		}
		a.author, a.persona = author, t.PersonaID
		why, err := agentAnnotate(ctx, tx, roomID, a)
		if err != nil {
			return nil, err
		}
		if why != "" {
			report.Ignored = append(report.Ignored, fmt.Sprintf("annotation on %s: %s", a.Path, why))
			continue
		}
		report.Annotated++
	}
	if strings.TrimSpace(out.Synthesis) != "" {
		if err := insertMessage(ctx, tx, &Message{RoomID: roomID, Kind: KindAgent, Sender: author, PersonaID: t.PersonaID, Body: out.Synthesis,
			Model: out.Model, Effort: out.Effort}); err != nil {
			return nil, err
		}
	}
	if note := report.Note(author); note != "" {
		if err := insertMessage(ctx, tx, &Message{RoomID: roomID, Kind: KindSystem, Sender: author, Body: note}); err != nil {
			return nil, err
		}
	}
	rep, err := json.Marshal(report)
	if err != nil {
		return nil, fmt.Errorf("complete turn: %w", err)
	}
	ended := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE turns SET status = 'done', session_id = ?, model = ?, effort = ?, report = ?, ended_at = ? WHERE id = ?`,
		out.SessionID, out.Model, out.Effort, string(rep), ended.Format(time.RFC3339Nano), id); err != nil {
		return nil, fmt.Errorf("complete turn: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("complete turn: %w", err)
	}
	t.Status, t.SessionID, t.Model, t.Effort, t.Report, t.EndedAt = TurnDone, out.SessionID, out.Model, out.Effort, report, &ended
	return t, nil
}

// appendSession adds out's session bytes to persona's stored copy, provided they start where it ends.
func appendSession(ctx context.Context, tx *sql.Tx, roomID string, persona int64, out *TurnOutcome) error {
	var stored []byte
	err := tx.QueryRowContext(ctx, `SELECT data FROM agent_sessions WHERE persona_id = ? AND session_id = ?`, persona, out.SessionID).Scan(&stored)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("append session: %w", err)
	}
	if out.SessionOffset != int64(len(stored)) {
		return fmt.Errorf("%w: upload starts at %d, stored copy holds %d bytes", ErrSessionGap, out.SessionOffset, len(stored))
	}
	// Appended in Go: SQLite's || would turn the blob into text. Never nil: the column is NOT NULL.
	data := append(append(make([]byte, 0, len(stored)+len(out.Session)), stored...), out.Session...)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO agent_sessions(room_id, persona_id, session_id, data, updated_at) VALUES(?,?,?,?,?)
		ON CONFLICT(persona_id, session_id) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at`,
		roomID, persona, out.SessionID, data, now())
	if err != nil {
		return fmt.Errorf("append session: %w", err)
	}
	return nil
}

// Note is the room message telling what the turn of persona name refused or ignored; empty when
// nothing was.
func (r TurnReport) Note(name string) string {
	var parts []string
	if len(r.Rejected) > 0 {
		parts = append(parts, "uploads are read-only, changes refused: "+strings.Join(r.Rejected, ", "))
	}
	if len(r.Ignored) > 0 {
		parts = append(parts, "ignored: "+strings.Join(r.Ignored, "; "))
	}
	if len(r.Renamed) > 0 {
		parts = append(parts, "named like an upload, so stored "+strings.Join(r.Renamed, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return name + " turn: " + strings.Join(parts, ". ") + "."
}

// SetHost makes participant pid of room the agent host. ErrNotFound if pid is not in room.
func (s *Store) SetHost(ctx context.Context, roomID string, pid int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE rooms SET host_participant_id = ? WHERE id = ? AND EXISTS (SELECT 1 FROM participants WHERE id = ? AND room_id = ?)`,
		pid, roomID, pid, roomID)
	if err != nil {
		return fmt.Errorf("set host: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("set host: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetCanTrigger grants or revokes the agent button for participant pid of room.
func (s *Store) SetCanTrigger(ctx context.Context, roomID string, pid int64, can bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE participants SET can_trigger = ? WHERE id = ? AND room_id = ?`, can, pid, roomID)
	if err != nil {
		return fmt.Errorf("set can_trigger: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("set can_trigger: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}
