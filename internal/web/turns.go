package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/anchor"
	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

const (
	defaultTurnTimeout = 15 * time.Minute
	defaultMaxResult   = 32 << 20
	maxError           = 500
)

// turnGrace lets the daemon report its own timeout before the server's backstop fails the turn. A
// var for tests.
var turnGrace = time.Minute

var sessionIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// startTurn is a persona's button (D12): the host, or a participant the host allowed, runs one
// turn of that persona on the host's daemon. While another turn runs, or others wait, the press
// joins the room's queue and the answer is 202 (002-AC17).
func (s *Server) startTurn(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		Fresh bool `json:"fresh"`
		// PersonaID 0 picks the room's first persona.
		PersonaID int64 `json:"persona_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	room, err := s.bus.Store.Room(ctx, sess.p.RoomID)
	if err != nil {
		s.serverError(w, "start_turn_failed", err)
		return
	}
	isHost := room.HostParticipantID == sess.p.ID
	host := s.hostDaemon(room.ID, room.HostParticipantID)
	switch {
	case host == nil || !host.CanHost:
		httpError(w, http.StatusConflict, "the agent is offline: no host daemon is connected")
		return
	case !isHost && !sess.p.CanTrigger:
		httpError(w, http.StatusForbidden, "only the host and the participants they allow can press the agent button")
		return
	case req.Fresh && !isHost:
		httpError(w, http.StatusForbidden, "only the host can start a new session")
		return
	}
	persona, ok := s.pressedPersona(w, r, room.ID, req.PersonaID)
	if !ok {
		return
	}
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	busy, err := s.roomBusy(ctx, room.ID)
	if err != nil {
		s.serverError(w, "start_turn_failed", err)
		return
	}
	if busy {
		q := &store.QueuedTurn{RoomID: room.ID, PersonaID: persona.ID, ParticipantID: sess.p.ID, Fresh: req.Fresh}
		err := s.bus.Store.Enqueue(ctx, q)
		switch {
		case errors.Is(err, store.ErrAlreadyQueued):
			httpError(w, http.StatusConflict, err.Error())
		case errors.Is(err, store.ErrNotFound):
			httpError(w, http.StatusNotFound, "no such persona")
		case err != nil:
			s.serverError(w, "queue_turn_failed", err)
		default:
			s.bus.Notify(room.ID)
			s.log.Info("turn_queued", zap.String("room", room.ID), zap.Int64("persona", persona.ID), zap.Int64("participant", sess.p.ID))
			writeJSON(w, http.StatusAccepted, q)
		}
		return
	}
	t, err := s.launch(ctx, room, persona, req.Fresh, sess.p.ID, host)
	if errors.Is(err, store.ErrTurnInProgress) {
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		s.serverError(w, "start_turn_failed", err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

// pressedPersona loads persona id of room, or the room's first one for 0; a persona waiting for the
// host's approval cannot run (002-AC21). It answers the error itself.
func (s *Server) pressedPersona(w http.ResponseWriter, r *http.Request, room string, id int64) (*store.RoomPersona, bool) {
	if id != 0 {
		p, err := s.bus.Store.RoomPersona(r.Context(), room, id)
		if errors.Is(err, store.ErrNotFound) {
			httpError(w, http.StatusNotFound, "no such persona")
			return nil, false
		}
		if err != nil {
			s.serverError(w, "start_turn_failed", err)
			return nil, false
		}
		if p.Pending {
			httpError(w, http.StatusConflict, p.Name+" waits for the host's approval")
			return nil, false
		}
		return p, true
	}
	ps, err := s.bus.Store.RoomPersonas(r.Context(), room)
	if err != nil {
		s.serverError(w, "start_turn_failed", err)
		return nil, false
	}
	for _, p := range ps {
		if !p.Pending {
			return &p, true
		}
	}
	httpError(w, http.StatusConflict, "the room has no persona: add one first")
	return nil, false
}

// roomBusy reports a running turn or a waiting queue: a new press then waits its turn.
func (s *Server) roomBusy(ctx context.Context, room string) (bool, error) {
	if _, err := s.bus.Store.RunningTurn(ctx, room); err == nil {
		return true, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	q, err := s.bus.Store.Queue(ctx, room)
	return len(q) > 0, err
}

// launch starts a turn of persona on host's daemon, pressed by participant pid. The caller holds
// turnMu.
func (s *Server) launch(ctx context.Context, room *store.Room, persona *store.RoomPersona, fresh bool, pid int64, host *presence) (*store.Turn, error) {
	t, err := s.prepareTurn(ctx, room, persona, fresh)
	if err != nil {
		return nil, err
	}
	t.ParticipantID, t.DaemonID, t.Host = pid, host.id, host.Name
	if err := s.bus.Store.StartTurn(ctx, t); err != nil {
		return nil, err
	}
	// The daemon may have dropped after the presence check, its cleanup finding no turn to fail.
	if h := s.hostDaemon(room.ID, room.HostParticipantID); h == nil || h.id != host.id {
		s.endTurn(ctx, room.ID, t.ID, "the host daemon on "+host.Name+" disconnected")
	}
	// Backstop for a daemon that neither answers nor drops its stream.
	time.AfterFunc(s.opts.TurnTimeout+turnGrace, func() {
		s.failTurn(context.Background(), room.ID, t.ID, "timed out after "+s.opts.TurnTimeout.String())
	})
	s.bus.Notify(room.ID)
	s.log.Info("turn_started", zap.String("room", room.ID), zap.Int64("turn", t.ID), zap.Int64("persona", persona.ID),
		zap.Int64("participant", pid), zap.Bool("fresh", fresh), zap.Bool("resume", t.Resume != ""))
	return t, nil
}

// drain starts the room's next queued turn once no turn runs and the host's daemon is there; the
// queue waits otherwise.
func (s *Server) drain(ctx context.Context, roomID string) {
	// A caller's request may end right after it: the queue must still move.
	ctx = context.WithoutCancel(ctx)
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	if _, err := s.bus.Store.RunningTurn(ctx, roomID); !errors.Is(err, store.ErrNotFound) {
		return
	}
	room, err := s.bus.Store.Room(ctx, roomID)
	if err != nil {
		return
	}
	host := s.hostDaemon(room.ID, room.HostParticipantID)
	if host == nil || !host.CanHost {
		return
	}
	q, err := s.bus.Store.PopQueued(ctx, room.ID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error("dequeue_failed", zap.String("room", room.ID), zap.Error(err))
		}
		return
	}
	persona, err := s.bus.Store.RoomPersona(ctx, room.ID, q.PersonaID)
	if err == nil {
		_, err = s.launch(ctx, room, persona, q.Fresh, q.ParticipantID, host)
	}
	if err != nil {
		s.log.Error("queued_turn_failed", zap.String("room", room.ID), zap.Int64("persona", q.PersonaID), zap.Error(err))
		m := &store.Message{RoomID: room.ID, Kind: store.KindSystem, Sender: "grill-room", Body: "A queued turn could not start."}
		if persona != nil {
			m.Sender, m.Body = persona.Name, "The queued turn of "+persona.Name+" could not start."
		}
		if err := s.bus.Post(ctx, m); err != nil {
			s.log.Error("post_failed", zap.String("room", room.ID), zap.Error(err))
		}
	}
}

// prepareTurn reads what the turn of persona needs: the session to resume, the cursors, and the
// prompt with everything said and annotated since that persona's last successful turn. The
// persona's text, model and effort are copied onto the turn: a change during it waits for the next.
func (s *Server) prepareTurn(ctx context.Context, room *store.Room, persona *store.RoomPersona, fresh bool) (*store.Turn, error) {
	st := s.bus.Store
	t := &store.Turn{RoomID: room.ID, Fresh: fresh, PersonaID: persona.ID, PersonaName: persona.Name, PersonaText: persona.Text,
		ReqModel: persona.Model, ReqEffort: persona.Effort}
	prev, err := st.LastDoneTurn(ctx, room.ID, persona.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		prev = &store.Turn{}
	case err != nil:
		return nil, err
	}
	if !fresh && prev.SessionID != "" {
		if _, err := st.AgentSession(ctx, room.ID, persona.ID, prev.SessionID); err == nil {
			t.Resume = prev.SessionID
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	// Read before the messages: an edit landing in between is flagged again next turn, never lost.
	if t.ChangeCursor, err = st.ChangeCursor(ctx, room.ID); err != nil {
		return nil, err
	}
	if t.MessageCursor, err = st.MessageCursor(ctx, room.ID); err != nil {
		return nil, err
	}
	changes, err := st.MessageChanges(ctx, room.ID, prev.MessageCursor, prev.ChangeCursor, t.ChangeCursor)
	if err != nil {
		return nil, err
	}
	// An edit back to the text the agent saw changes nothing for it.
	changes = slices.DeleteFunc(changes, func(c store.MessageChange) bool { return c.DeletedAt == nil && c.Before == c.Body })
	var msgs []store.Message
	for after := prev.MessageCursor; after < t.MessageCursor; {
		batch, err := st.Messages(ctx, room.ID, after, replayBatch)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		for _, m := range batch {
			// The persona's own synthesis is in its session already; the others' are news to it.
			if m.ID <= t.MessageCursor && (m.Kind != store.KindAgent || m.PersonaID != persona.ID) {
				msgs = append(msgs, m)
			}
		}
		after = batch[len(batch)-1].ID
	}
	anns, cursor, err := st.AnnotationsSince(ctx, room.ID, prev.AnnotationCursor)
	if err != nil {
		return nil, err
	}
	t.AnnotationCursor = cursor
	// Threads whose only new messages are the persona's own replies hold nothing new for it.
	anns = slices.DeleteFunc(anns, func(a store.Annotation) bool {
		return !slices.ContainsFunc(a.Messages, func(m store.AnnotationMessage) bool {
			return m.ID > prev.AnnotationCursor && !ownMessage(m, persona.ID)
		})
	})
	docs, err := st.Documents(ctx, room.ID)
	if err != nil {
		return nil, err
	}
	// The presence preamble of spec 002 phase 5 (002-AC28) goes in Preamble.
	t.Prompt = buildPrompt(promptInput{Room: room.Name, Persona: persona.Name, Self: persona.ID, NewSession: t.Resume == "",
		Msgs: msgs, Changes: changes, Anns: anns, AnnCursor: prev.AnnotationCursor, Paths: docPaths(docs)})
	return t, nil
}

// endTurn fails a running turn and wakes the room; a turn already ended is left alone.
func (s *Server) endTurn(ctx context.Context, room string, id int64, reason string) {
	m, err := s.bus.Store.FailTurn(ctx, room, id, reason)
	if err != nil {
		s.log.Error("fail_turn_failed", zap.String("room", room), zap.Int64("turn", id), zap.Error(err))
		return
	}
	if m != nil {
		s.log.Warn("turn_failed", zap.String("room", room), zap.Int64("turn", id), zap.String("reason", reason))
		s.bus.Notify(room)
	}
}

// failTurn fails a running turn, then starts the next queued one.
func (s *Server) failTurn(ctx context.Context, room string, id int64, reason string) {
	s.endTurn(ctx, room, id, reason)
	s.drain(ctx, room)
}

// failDaemonTurn fails the room's running turn when d runs it (D6: a dropped host loses its turn).
func (s *Server) failDaemonTurn(ctx context.Context, d *store.Daemon, reason string) {
	t, err := s.bus.Store.RunningTurn(ctx, d.RoomID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error("running_turn_lookup_failed", zap.String("room", d.RoomID), zap.Error(err))
		}
		return
	}
	if t.DaemonID == d.ID {
		s.failTurn(ctx, d.RoomID, t.ID, reason)
	}
}

// cancelQueued drops a queued turn; only the participant who queued it may (002-AC17).
func (s *Server) cancelQueued(w http.ResponseWriter, r *http.Request, sess session) {
	id, err := strconv.ParseInt(r.PathValue("queued"), 10, 64)
	if err != nil {
		httpError(w, http.StatusNotFound, "no such queued turn")
		return
	}
	err = s.bus.Store.CancelQueued(r.Context(), sess.p.RoomID, id, sess.p.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, "no such queued turn")
	case errors.Is(err, store.ErrForbidden):
		httpError(w, http.StatusForbidden, "only who queued a turn can cancel it")
	case err != nil:
		s.serverError(w, "cancel_queued_failed", err)
	default:
		s.bus.Notify(sess.p.RoomID)
		w.WriteHeader(http.StatusNoContent)
	}
}

// daemonTurn loads the {turn} of the daemon's room, provided it is running on this daemon.
func (s *Server) daemonTurn(w http.ResponseWriter, r *http.Request, d *store.Daemon) (*store.Turn, bool) {
	id, err := strconv.ParseInt(r.PathValue("turn"), 10, 64)
	if err != nil {
		httpError(w, http.StatusNotFound, "no such turn")
		return nil, false
	}
	t, err := s.bus.Store.Turn(r.Context(), d.RoomID, id)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such turn")
		return nil, false
	}
	if err != nil {
		s.serverError(w, "daemon_turn_failed", err)
		return nil, false
	}
	if t.Status != store.TurnRunning || t.DaemonID != d.ID {
		httpError(w, http.StatusConflict, store.ErrTurnNotRunning.Error())
		return nil, false
	}
	return t, true
}

// getJob hands the daemon its turn: the prompt and the session to resume.
func (s *Server) getJob(w http.ResponseWriter, r *http.Request, d *store.Daemon, _ string) {
	t, ok := s.daemonTurn(w, r, d)
	if !ok {
		return
	}
	job := turn.Job{ID: t.ID, Prompt: t.Prompt, Resume: t.Resume, TimeoutSeconds: int(s.opts.TurnTimeout / time.Second),
		Persona: t.PersonaName, System: t.PersonaText, Model: t.ReqModel, Effort: t.ReqEffort}
	// The host's approval holds for this persona on the daemon it was given for only (002-AC20).
	if p, err := s.bus.Store.RoomPersona(r.Context(), d.RoomID, t.PersonaID); err == nil {
		job.Approved = p.Cap.Approved(d.ID)
	} else if !errors.Is(err, store.ErrNotFound) {
		s.serverError(w, "get_job_failed", err)
		return
	}
	if t.Resume != "" {
		data, err := s.bus.Store.AgentSession(r.Context(), d.RoomID, t.PersonaID, t.Resume)
		if err != nil {
			s.serverError(w, "get_job_failed", err)
			return
		}
		sum := sha256.Sum256(data)
		job.SessionSize, job.SessionSHA256 = int64(len(data)), hex.EncodeToString(sum[:])
	}
	writeJSON(w, http.StatusOK, job)
}

// getWorkspace streams the turn's workspace as a tar (D10, D20).
func (s *Server) getWorkspace(w http.ResponseWriter, r *http.Request, d *store.Daemon, _ string) {
	t, ok := s.daemonTurn(w, r, d)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	if err := s.writeWorkspace(r.Context(), w, t); err != nil {
		// Headers are gone: the daemon sees a truncated tar and fails the turn.
		s.log.Error("workspace_failed", zap.String("room", d.RoomID), zap.Int64("turn", t.ID), zap.Error(err))
	}
}

// getSession serves the stored jsonl of the session the daemon's running turn resumes (D6, D23).
func (s *Server) getSession(w http.ResponseWriter, r *http.Request, d *store.Daemon, _ string) {
	sid := r.PathValue("session")
	t, err := s.bus.Store.RunningTurn(r.Context(), d.RoomID)
	if err != nil || t.DaemonID != d.ID || t.Resume != sid {
		httpError(w, http.StatusNotFound, "no such session for this daemon's turn")
		return
	}
	data, err := s.bus.Store.AgentSession(r.Context(), d.RoomID, t.PersonaID, sid)
	if err != nil {
		s.serverError(w, "get_session_failed", err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data) //nolint:gosec // served as ndjson to a daemon with nosniff, never rendered by a browser
}

// postResult ends the daemon's turn: an error fails it; otherwise its changes, synthesis, replies
// and session bytes apply atomically, or the turn fails with nothing kept (D6, D23, D30).
func (s *Server) postResult(w http.ResponseWriter, r *http.Request, d *store.Daemon, _ string) {
	t, ok := s.daemonTurn(w, r, d)
	if !ok {
		return
	}
	var res turn.Result
	if !decodeLimit(w, r, &res, s.opts.MaxResultBytes) {
		// Answered already (413 or 400): the turn cannot finish with what the daemon sent.
		s.failTurn(r.Context(), d.RoomID, t.ID, fmt.Sprintf("the result from %s was refused: too large or malformed", d.Name))
		return
	}
	if res.Error != "" {
		s.failTurn(r.Context(), d.RoomID, t.ID, sanitize(res.Error, maxError))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !sessionIDRe.MatchString(res.SessionID) || res.SessionOffset < 0 {
		s.failTurn(r.Context(), d.RoomID, t.ID, "the daemon sent no valid session id")
		httpError(w, http.StatusBadRequest, "session_id must be a UUID and session_offset >= 0")
		return
	}
	o := turn.Classify(res.Changes)
	out := &store.TurnOutcome{
		Synthesis: sanitize(res.Synthesis, turn.MaxSynthesis), Model: sanitize(res.Model, turn.MaxModel), SessionID: res.SessionID,
		// An effort claude does not know is not shown as if it ran.
		Effort:        knownOr(res.Effort, turn.Efforts),
		Capped:        res.Capped,
		SessionOffset: res.SessionOffset, Session: res.Session,
		Report: store.TurnReport{Rejected: o.Rejected, Ignored: o.Ignored},
	}
	for _, doc := range o.Docs {
		out.Documents = append(out.Documents, store.AgentWrite{Name: doc.Name, Format: doc.Format, Body: doc.Body})
	}
	for _, rp := range o.Replies {
		out.Replies = append(out.Replies, store.AgentReply{AnnotationID: rp.Annotation, Body: rp.Body, Done: rp.Status == store.StatusDone})
	}
	if len(o.Annotations) > 0 {
		docs, err := s.bus.Store.Documents(r.Context(), d.RoomID)
		if err != nil {
			s.log.Error("complete_turn_failed", zap.String("room", d.RoomID), zap.Int64("turn", t.ID), zap.Error(err))
			s.failTurn(r.Context(), d.RoomID, t.ID, "the room could not apply the result")
			httpError(w, http.StatusInternalServerError, "internal error")
			return
		}
		ids := map[string]int64{}
		for id, p := range docPaths(docs) {
			ids[p] = id
		}
		for _, a := range o.Annotations {
			// A docs/ path no document has yet may be one this turn writes: the store looks it up after.
			agentDoc := ""
			if name, ok := strings.CutPrefix(a.Doc, turn.DocsDir+"/"); ok {
				agentDoc = name
			}
			out.Annotations = append(out.Annotations, store.AgentAnnotation{Path: a.Doc, DocID: ids[a.Doc], AgentDoc: agentDoc,
				Anchor: anchor.Anchor{Exact: a.Exact, Prefix: a.Prefix, Suffix: a.Suffix}, Body: sanitize(a.Body, turn.MaxReplyLen)})
		}
	}
	done, err := s.bus.Store.CompleteTurn(r.Context(), d.RoomID, t.ID, d.ID, out)
	switch {
	case errors.Is(err, store.ErrTurnNotRunning):
		httpError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, store.ErrSessionGap):
		s.failTurn(r.Context(), d.RoomID, t.ID, err.Error())
		httpError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.log.Error("complete_turn_failed", zap.String("room", d.RoomID), zap.Int64("turn", t.ID), zap.Error(err))
		s.failTurn(r.Context(), d.RoomID, t.ID, "the room could not apply the result")
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.bus.Notify(d.RoomID)
	s.drain(r.Context(), d.RoomID)
	s.log.Info("turn_done", zap.String("room", d.RoomID), zap.Int64("turn", t.ID), zap.Strings("revised", done.Report.Revised),
		zap.Strings("created", done.Report.Created), zap.Strings("rejected", done.Report.Rejected), zap.Int("annotated", done.Report.Annotated),
		zap.Int("closed", done.Report.Closed), zap.String("model", done.Model), zap.String("effort", done.Effort), zap.Int("session_bytes", len(res.Session)))
	writeJSON(w, http.StatusOK, done)
}

// sanitize keeps text the agent or a daemon wrote displayable: no control character but newline
// and tab, at most n runes.
func sanitize(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, strings.ToValidUTF8(s, "�"))
	return truncate(strings.TrimSpace(s), n)
}

// knownOr returns v when it is one of known, else "".
func knownOr(v string, known []string) string {
	if slices.Contains(known, v) {
		return v
	}
	return ""
}
