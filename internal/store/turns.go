package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/babs/grill-room/internal/anchor"
	"github.com/babs/grill-room/internal/render"
	"github.com/babs/grill-room/internal/turn"
)

// Turn statuses.
const (
	TurnRunning = "running"
	TurnDone    = "done"
	TurnFailed  = "failed"
	// TurnStopped is a turn someone stopped while it ran: nothing of it applies (005-AC5).
	TurnStopped = "stopped"
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

// ConflictError rejects a turn's result: it edits Doc, which another turn revised since this turn's
// workspace was built (003-AC21).
type ConflictError struct{ Doc string }

func (e *ConflictError) Error() string { return "conflict on " + e.Doc }

// Turn is one press of the agent button and its run on the host's daemon.
type Turn struct {
	ID     int64  `json:"id"`
	RoomID string `json:"room_id"`
	// ParticipantID pressed the button.
	ParticipantID int64  `json:"participant_id"`
	DaemonID      int64  `json:"-"`
	Status        string `json:"status"`
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
	// StartedAt is when the turn started running: a turn the host's daemon queued starts when it
	// leaves that queue.
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	// HostQueued is set while the host's daemon holds the turn in its local queue, waiting under
	// the host's cap: the room shows "queued (host busy)" (003-AC18).
	HostQueued bool `json:"host_queued,omitempty"`
	// RerunOf is the turn whose result a conflict rejected, which this one runs again; Cause says
	// why the turn runs (003-AC21).
	RerunOf int64  `json:"rerun_of,omitempty"`
	Cause   string `json:"cause,omitempty"`
	// Handoff is HandoffWrite for a last turn writing a handoff note before a new session, and
	// HandoffRead for that new session's first turn (003-AC25).
	Handoff string `json:"handoff,omitempty"`
	// BaseRevisions are the revisions of the agent documents in the turn's workspace, by document
	// id, pinned when the daemon first fetches it; nil before.
	BaseRevisions map[int64]int `json:"-"`
	// QueueID is the queue entry the turn comes from, 0 for a press that did not wait: the
	// mentions waiting on it follow the turn (005-AC4). Set by the caller of StartTurn only.
	QueueID int64 `json:"-"`
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
		WHERE d.id = ? AND d.room_id = ? AND d.deleted_at IS NULL ORDER BY r.n DESC LIMIT 1`, a.DocID, roomID).Scan(&format, &n, &body)
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
			FROM documents WHERE room_id = ? AND name = ? AND deleted_at IS NULL`, roomID, name).Scan(&agent, &upload)
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
	// ModelNote and ModelMismatch qualify Model as turn.Result does (005-AC10).
	ModelNote     string
	ModelMismatch bool
	Effort        string
	// Capped tells that the host cap lowered what the persona asked for: the synthesis says so.
	Capped    bool
	Documents []AgentWrite
	Replies   []AgentReply
	// Annotations are anchored after Documents are written, so they may quote this turn's text.
	Annotations []AgentAnnotation
	// Session holds the session jsonl bytes from SessionOffset on.
	SessionID     string
	SessionOffset int64
	Session       []byte
	// ContextTokens and ContextWindow are the session's context use after the turn, 0 when unknown
	// (003-AC24).
	ContextTokens int64
	ContextWindow int64
	// Report carries the refusals found before the store; CompleteTurn adds what it applied.
	Report TurnReport
}

const turnCols = `SELECT id, room_id, COALESCE(participant_id, 0), COALESCE(daemon_id, 0), status, fresh, resume,
	session_id, model, message_cursor, annotation_cursor, change_cursor, prompt, error, report, started_at, ended_at,
	COALESCE(persona_id, 0), persona_name, persona_text, req_model, req_effort, effort,
	host_queued_at IS NOT NULL, COALESCE(rerun_of, 0), cause, base_revisions, handoff FROM turns`

func scanTurn(sc scanner) (*Turn, error) {
	var t Turn
	var report, started string
	var ended, base sql.NullString
	err := sc.Scan(&t.ID, &t.RoomID, &t.ParticipantID, &t.DaemonID, &t.Status, &t.Fresh, &t.Resume,
		&t.SessionID, &t.Model, &t.MessageCursor, &t.AnnotationCursor, &t.ChangeCursor, &t.Prompt, &t.Error, &report, &started, &ended,
		&t.PersonaID, &t.PersonaName, &t.PersonaText, &t.ReqModel, &t.ReqEffort, &t.Effort,
		&t.HostQueued, &t.RerunOf, &t.Cause, &base, &t.Handoff)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan turn: %w", err)
	}
	if err := json.Unmarshal([]byte(report), &t.Report); err != nil {
		return nil, fmt.Errorf("turn %d report: %w", t.ID, err)
	}
	if base.Valid {
		if err := json.Unmarshal([]byte(base.String), &t.BaseRevisions); err != nil {
			return nil, fmt.Errorf("turn %d base revisions: %w", t.ID, err)
		}
	}
	t.StartedAt = parseTS(started)
	if ended.Valid {
		e := parseTS(ended.String)
		t.EndedAt = &e
	}
	return &t, nil
}

// StartTurn records t as a running turn of the room, for its persona. ErrTurnInProgress if one
// runs already: any turn of the room, or with parallel on, one of the same persona (003-AC20).
// ErrNotFound when the persona is not in the room or waits for the host's approval.
func (s *Store) StartTurn(ctx context.Context, t *Turn, parallel bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("start turn: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var running int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM turns WHERE room_id = ? AND status = 'running' AND (? = 0 OR persona_id = ?)`,
		t.RoomID, parallel, t.PersonaID).Scan(&running); err != nil {
		return fmt.Errorf("start turn: %w", err)
	}
	if running > 0 {
		return ErrTurnInProgress
	}
	t.Status, t.StartedAt = TurnRunning, time.Now().UTC()
	var pid, did, rerun any
	if t.ParticipantID != 0 {
		pid = t.ParticipantID
	}
	if t.DaemonID != 0 {
		did = t.DaemonID
	}
	if t.RerunOf != 0 {
		rerun = t.RerunOf
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO turns(room_id, participant_id, daemon_id, host, status, fresh, resume, message_cursor, annotation_cursor, change_cursor, prompt, started_at,
			persona_id, persona_name, persona_text, req_model, req_effort, rerun_of, cause, handoff)
		SELECT room_id, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?, id, ?, ?, ?, ?, ?, ?, ? FROM room_personas WHERE id = ? AND room_id = ? AND pending = 0`,
		pid, did, t.Status, t.Fresh, t.Resume, t.MessageCursor, t.AnnotationCursor, t.ChangeCursor, t.Prompt, t.StartedAt.Format(time.RFC3339Nano),
		t.PersonaName, t.PersonaText, t.ReqModel, t.ReqEffort, rerun, t.Cause, t.Handoff, t.PersonaID, t.RoomID)
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
	if err := pinTurnSkills(ctx, tx, t.RoomID, t.ID, t.PersonaID); err != nil {
		return err
	}
	if err := pinTurnPlugins(ctx, tx, t.ID, t.PersonaID); err != nil {
		return err
	}
	// 006-AC18: the persona's voice-line count and timer start again from what this turn reads;
	// 009-AC32: so does the notes watcher's wait.
	if _, err := tx.ExecContext(ctx, `UPDATE room_personas SET mark_msg_id = ?, mark_at_ms = ?, notes_mark = max(notes_mark, ?) WHERE id = ?`,
		t.MessageCursor, t.StartedAt.UnixMilli(), t.AnnotationCursor, t.PersonaID); err != nil {
		return fmt.Errorf("start turn: %w", err)
	}
	if t.QueueID != 0 {
		for _, table := range mentionTables {
			//nolint:gosec // table is one of mentionTables, never user input
			if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET turn_id = ? WHERE queue_id = ?`, t.ID, t.QueueID); err != nil {
				return fmt.Errorf("start turn: %w", err)
			}
		}
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

// RunningTurns lists the room's running turns, the ones its host's daemon queued included, oldest
// first. Only a room in parallel mode runs more than one (003-AC20).
func (s *Store) RunningTurns(ctx context.Context, roomID string) ([]Turn, error) {
	rows, err := s.db.QueryContext(ctx, turnCols+` WHERE room_id = ? AND status = 'running' ORDER BY id`, roomID)
	if err != nil {
		return nil, fmt.Errorf("running turns: %w", err)
	}
	defer rows.Close()
	out := []Turn{}
	for rows.Next() {
		t, err := scanTurn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// SetHostQueued records that daemonID holds running turn id in its local queue, or that the turn
// left it and starts running now (003-AC18). It reports whether that changed anything;
// ErrTurnNotRunning when the turn ended or is another daemon's.
func (s *Store) SetHostQueued(ctx context.Context, roomID string, id, daemonID int64, queued bool) (bool, error) {
	q := `UPDATE turns SET host_queued_at = ? WHERE id = ? AND room_id = ? AND daemon_id = ? AND status = 'running' AND host_queued_at IS NULL`
	if !queued {
		q = `UPDATE turns SET host_queued_at = NULL, started_at = ? WHERE id = ? AND room_id = ? AND daemon_id = ? AND status = 'running' AND host_queued_at IS NOT NULL`
	}
	res, err := s.db.ExecContext(ctx, q, now(), id, roomID, daemonID)
	if err != nil {
		return false, fmt.Errorf("set host queued: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return false, fmt.Errorf("set host queued: %w", err)
	} else if n > 0 {
		return true, nil
	}
	t, err := s.Turn(ctx, roomID, id)
	if err != nil {
		return false, err
	}
	if t.Status != TurnRunning || t.DaemonID != daemonID {
		return false, ErrTurnNotRunning
	}
	return false, nil
}

// PinBaseRevisions returns the revision of each agent document the turn's workspace holds, by
// document id. The first call pins them to the latest ones: every later fetch of the workspace,
// and the conflict check of its result, use the same (003-AC21).
func (s *Store) PinBaseRevisions(ctx context.Context, roomID string, id int64) (map[int64]int, error) {
	base := map[int64]int{}
	err := s.withTx(ctx, "pin base revisions", func(tx *sql.Tx) error {
		var pinned sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT base_revisions FROM turns WHERE id = ? AND room_id = ?`, id, roomID).Scan(&pinned); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("pin base revisions: %w", err)
		}
		if pinned.Valid {
			return json.Unmarshal([]byte(pinned.String), &base)
		}
		rows, err := tx.QueryContext(ctx, `SELECT d.id, MAX(r.n) FROM documents d JOIN revisions r ON r.document_id = d.id
			WHERE d.room_id = ? AND d.kind = 'agent' AND d.deleted_at IS NULL GROUP BY d.id`, roomID)
		if err != nil {
			return fmt.Errorf("pin base revisions: %w", err)
		}
		for rows.Next() {
			var doc int64
			var n int
			if err := rows.Scan(&doc, &n); err != nil {
				_ = rows.Close()
				return fmt.Errorf("pin base revisions: %w", err)
			}
			base[doc] = n
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("pin base revisions: %w", err)
		}
		b, err := json.Marshal(base)
		if err != nil {
			return fmt.Errorf("pin base revisions: %w", err)
		}
		_, err = tx.ExecContext(ctx, `UPDATE turns SET base_revisions = ? WHERE id = ?`, string(b), id)
		return err
	})
	return base, err
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
	var name, handoff string
	err = tx.QueryRowContext(ctx, `UPDATE turns SET status = 'failed', error = ?, ended_at = ? WHERE id = ? AND room_id = ? AND status = 'running'
		RETURNING persona_name, handoff`, reason, now(), id, roomID).Scan(&name, &handoff)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fail turn: %w", err)
	}
	name = signer(name)
	body := name + " turn failed: " + reason
	if note := handoffFailed(name, handoff); note != "" {
		body = strings.TrimSuffix(body, ".") + "." + note
	}
	m := &Message{RoomID: roomID, Kind: KindSystem, Sender: name, Body: body, TurnID: id}
	if err := insertMessage(ctx, tx, m); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("fail turn: %w", err)
	}
	return m, nil
}

// StopTurn ends running turn id as stopped by name and says so in the room, in one transaction
// (005-AC5). Its result, if one comes, is refused: nothing of it applies. ErrTurnNotRunning when
// the turn ended already.
func (s *Store) StopTurn(ctx context.Context, roomID string, id int64, name string) (*Message, error) {
	var m *Message
	err := s.withTx(ctx, "stop turn", func(tx *sql.Tx) error {
		var persona string
		err := tx.QueryRowContext(ctx, `UPDATE turns SET status = 'stopped', error = ?, ended_at = ? WHERE id = ? AND room_id = ? AND status = 'running'
			RETURNING persona_name`, "stopped by "+name, now(), id, roomID).Scan(&persona)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTurnNotRunning
		}
		if err != nil {
			return fmt.Errorf("stop turn: %w", err)
		}
		persona = signer(persona)
		m = &Message{RoomID: roomID, Kind: KindSystem, Sender: persona, Body: persona + " turn stopped by " + name + ".", TurnID: id}
		return insertMessage(ctx, tx, m)
	})
	return m, err
}

// migrateStopped checks that the migration to 'stopped' loosened the CHECK, since a schema text
// other than expected would leave the old one, then bumps schema_version as SQLite's procedure
// for a schema edit asks, so that no connection keeps the old schema cached.
func migrateStopped(ctx context.Context, tx *sql.Tx) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = 'turns' AND sql LIKE '%''stopped''%'`).Scan(&n); err != nil {
		return fmt.Errorf("check turns schema: %w", err)
	}
	if n != 1 {
		return errors.New("the turns table's status CHECK is not the expected one: 'stopped' was not added")
	}
	var v int
	if err := tx.QueryRowContext(ctx, `PRAGMA schema_version`).Scan(&v); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA schema_version = %d`, v+1)); err != nil {
		return fmt.Errorf("bump schema version: %w", err)
	}
	return nil
}

// RejectTurn ends running turn id of daemonID, whose result conflicts on doc (003-AC21). A first
// conflict queues the turn again at the head of the room's queue, to run on the new version: the
// persona's queued turn, if any, becomes that re-run. A re-run's conflict fails it. Both say so in
// the room. It returns the message posted and whether the turn runs again; ErrTurnNotRunning when
// the turn ended or is another daemon's.
func (s *Store) RejectTurn(ctx context.Context, roomID string, id, daemonID int64, doc string) (*Message, bool, error) {
	var m *Message
	var rerun bool
	err := s.withTx(ctx, "reject turn", func(tx *sql.Tx) error {
		t, err := scanTurn(tx.QueryRowContext(ctx, turnCols+` WHERE id = ? AND room_id = ?`, id, roomID))
		if err != nil {
			return err
		}
		if t.Status != TurnRunning || t.DaemonID != daemonID {
			return ErrTurnNotRunning
		}
		reason := (&ConflictError{Doc: doc}).Error()
		if _, err := tx.ExecContext(ctx, `UPDATE turns SET status = 'failed', error = ?, ended_at = ? WHERE id = ?`, reason, now(), id); err != nil {
			return fmt.Errorf("reject turn: %w", err)
		}
		name := signer(t.PersonaName)
		m = &Message{RoomID: roomID, Kind: KindSystem, Sender: name, Body: name + " turn failed: " + reason + "." + handoffFailed(name, t.Handoff), TurnID: id}
		if rerun = t.RerunOf == 0 && t.PersonaID != 0; rerun {
			m.Body = name + " turn: " + doc + " changed while it ran, so its result is set aside and it runs again on the new version."
			if err := queueRerun(ctx, tx, t, doc); err != nil {
				return err
			}
		}
		return insertMessage(ctx, tx, m)
	})
	return m, rerun, err
}

// Handoff kinds of a turn (003-AC25).
const (
	HandoffWrite = "write"
	HandoffRead  = "read"
)

// HandoffCause is why a new session after a handoff runs, as its preamble says.
const HandoffCause = "handoff from your previous session"

// handoffFailed is the sentence a failed handoff turn adds to its failure line: no new session
// starts; "" for any other turn.
func handoffFailed(name, handoff string) string {
	if handoff != HandoffWrite {
		return ""
	}
	return " No handoff: " + name + " stays on its current session."
}

// handoffNext follows handoff turn t, done with report, in its transaction: a turn that wrote the
// note queues the persona's new session in place of its queued turn, which the new session answers
// anyway; one that did not says that the persona stays on its session (003-AC25).
func handoffNext(ctx context.Context, tx *sql.Tx, t *Turn, report TurnReport) error {
	doc := turn.HandoffDoc(t.PersonaName)
	if !slices.Contains(report.Created, doc) && !slices.Contains(report.Revised, doc) {
		return insertMessage(ctx, tx, &Message{RoomID: t.RoomID, Kind: KindSystem, Sender: signer(t.PersonaName),
			Body: "No handoff: " + t.PersonaName + " did not write docs/" + doc + ", so it stays on its current session."})
	}
	var old int64
	err := tx.QueryRowContext(ctx, `DELETE FROM turn_queue WHERE persona_id = ? RETURNING id`, t.PersonaID).Scan(&old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("queue handoff: %w", err)
	}
	q := &QueuedTurn{RoomID: t.RoomID, PersonaID: t.PersonaID, ParticipantID: t.ParticipantID, Source: SourceButton,
		Fresh: true, Cause: HandoffCause, Handoff: HandoffRead}
	if err := enqueue(ctx, tx, q); err != nil {
		return err
	}
	return requeueMentions(ctx, tx, q.ID, 0, old)
}

// queueRerun puts the re-run of turn t at the head of its room's queue, in place of its persona's
// queued turn: the re-run reads everything since the persona's last turn, so it answers that one too.
func queueRerun(ctx context.Context, tx *sql.Tx, t *Turn, doc string) error {
	fresh := t.Fresh
	var queued bool
	var old int64
	err := tx.QueryRowContext(ctx, `DELETE FROM turn_queue WHERE persona_id = ? RETURNING fresh, id`, t.PersonaID).Scan(&queued, &old)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("queue re-run: %w", err)
	default:
		fresh = fresh || queued
	}
	var pid any
	source := string(LinkCall)
	if t.ParticipantID != 0 {
		pid, source = t.ParticipantID, SourceButton
	}
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO turn_queue(room_id, persona_id, participant_id, source, fresh, created_at, rerun_of, cause, handoff)
		VALUES(?,?,?,?,?,?,?,?,?) RETURNING id`, t.RoomID, t.PersonaID, pid, source, fresh, now(), t.ID, "re-run after a conflict on "+doc, t.Handoff).Scan(&id)
	if err != nil {
		return fmt.Errorf("queue re-run: %w", err)
	}
	// The re-run answers the mentions of the rejected turn and of the entry it replaces.
	return requeueMentions(ctx, tx, id, t.ID, old)
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
		var n int
		var latest string
		err = tx.QueryRowContext(ctx, `
			SELECT d.id, r.n, r.body FROM documents d JOIN revisions r ON r.document_id = d.id
			WHERE d.room_id = ? AND d.kind = 'agent' AND d.name = ? AND d.deleted_at IS NULL ORDER BY r.n DESC LIMIT 1`, roomID, w.Name).Scan(&docID, &n, &latest)
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
			// Another turn revised the document, or created it, since this turn's workspace was
			// built: this edit would overwrite it unseen (003-AC21).
			if base, ok := t.BaseRevisions[docID]; t.BaseRevisions != nil && (!ok || base != n) {
				return nil, &ConflictError{Doc: w.Name}
			}
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
			err := tx.QueryRowContext(ctx, `SELECT id FROM documents WHERE room_id = ? AND kind = 'agent' AND name = ? AND deleted_at IS NULL`, roomID, name).Scan(&a.DocID)
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
		m := &Message{RoomID: roomID, Kind: KindAgent, Sender: author, PersonaID: t.PersonaID, Body: out.Synthesis, Model: out.Model, Effort: out.Effort,
			ModelNote: out.ModelNote, ModelMismatch: out.ModelMismatch, TurnID: id}
		if out.Capped {
			m.ReqModel, m.ReqEffort = t.ReqModel, t.ReqEffort
		}
		if err := insertMessage(ctx, tx, m); err != nil {
			return nil, err
		}
	}
	if note := report.Note(author); note != "" {
		if err := insertMessage(ctx, tx, &Message{RoomID: roomID, Kind: KindSystem, Sender: author, Body: note}); err != nil {
			return nil, err
		}
	}
	if t.Handoff == HandoffWrite {
		if err := handoffNext(ctx, tx, t, report); err != nil {
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
	ts := now()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO agent_sessions(room_id, persona_id, session_id, data, updated_at, context_tokens, context_window, context_updated_at)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(persona_id, session_id) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at,
			context_tokens = excluded.context_tokens, context_window = excluded.context_window, context_updated_at = excluded.context_updated_at`,
		roomID, persona, out.SessionID, data, ts, nullID(out.ContextTokens), nullID(out.ContextWindow), ts)
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

// HandOver makes participant to the AI host of room on by's request, nil for the super-admin, and
// says so in the chat: the host handing over keeps the 003 line, a room admin taking the role says
// so, anyone else's line names who did it (009-AC34). It writes the audit entry ctx carries, with the previous and the new host. The
// caller checked by's right. It returns the line, "" when to hosts already; ErrNotFound if to is not
// in room.
func (s *Store) HandOver(ctx context.Context, roomID string, to int64, by *Participant) (string, error) {
	var line string
	err := s.withTx(ctx, "hand over", func(tx *sql.Tx) error {
		var from sql.NullInt64
		var fromName, toName sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT r.host_participant_id, h.name, p.name FROM rooms r
			JOIN participants p ON p.id = ? AND p.room_id = r.id LEFT JOIN participants h ON h.id = r.host_participant_id
			WHERE r.id = ?`, to, roomID).Scan(&from, &fromName, &toName)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("hand over: %w", err)
		}
		if from.Int64 == to {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE rooms SET host_participant_id = ? WHERE id = ?`, to, roomID); err != nil {
			return fmt.Errorf("hand over: %w", err)
		}
		var m *Message
		switch {
		case by != nil && by.ID == from.Int64:
			m = &Message{RoomID: roomID, Kind: KindSystem, ParticipantID: to, Sender: toName.String, Body: toName.String + " holds the agent(s)."}
			err = insertMessage(ctx, tx, m)
		case by == nil:
			m = &Message{RoomID: roomID, Kind: KindSystem, Sender: "admin", Body: "The admin made " + toName.String + " the AI host"}
			err = insertMessage(ctx, tx, m)
		case by.ID == to:
			m, err = memberLine(ctx, tx, roomID, by, "took the AI host role")
		default:
			m, err = memberLine(ctx, tx, roomID, by, "made "+toName.String+" the AI host")
		}
		if err != nil {
			return err
		}
		line = m.Body
		details := map[string]any{"to": to, "to_name": toName.String}
		if from.Valid {
			details["from"], details["from_name"] = from.Int64, fromName.String
		}
		return auditTx(ctx, tx, "", details)
	})
	return line, err
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

// SetParallelTurns switches the room's parallel mode and says so in the room, signed by. It
// returns nil when nothing changed (003-AC20).
func (s *Store) SetParallelTurns(ctx context.Context, roomID string, on bool, by *Participant) (*Message, error) {
	var m *Message
	err := s.withTx(ctx, "set parallel turns", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE rooms SET parallel_turns = ? WHERE id = ? AND parallel_turns != ?`, on, roomID, on)
		if err != nil {
			return fmt.Errorf("set parallel turns: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err
		}
		body := "let the personas run one at a time again."
		if on {
			body = "let the personas run at the same time, each in its own sandbox."
		}
		m, err = memberLine(ctx, tx, roomID, by, body)
		return err
	})
	return m, err
}
