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

// startTurn is the agent button (D12): the host, or a participant the host allowed, starts one
// turn on the host's daemon. 409 "turn in progress" while one runs.
func (s *Server) startTurn(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		Fresh bool `json:"fresh"`
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
	if _, err := s.bus.Store.RunningTurn(ctx, room.ID); err == nil {
		httpError(w, http.StatusConflict, store.ErrTurnInProgress.Error())
		return
	}
	t, err := s.prepareTurn(ctx, room, req.Fresh)
	if err != nil {
		s.serverError(w, "start_turn_failed", err)
		return
	}
	t.ParticipantID, t.DaemonID, t.Host = sess.p.ID, host.id, host.Name
	err = s.bus.Store.StartTurn(ctx, t)
	if errors.Is(err, store.ErrTurnInProgress) {
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		s.serverError(w, "start_turn_failed", err)
		return
	}
	// The daemon may have dropped after the presence check, its cleanup finding no turn to fail.
	if h := s.hostDaemon(room.ID, room.HostParticipantID); h == nil || h.id != host.id {
		s.failTurn(ctx, room.ID, t.ID, "the host daemon on "+host.Name+" disconnected")
	}
	// Backstop for a daemon that neither answers nor drops its stream.
	time.AfterFunc(s.opts.TurnTimeout+turnGrace, func() {
		s.failTurn(context.Background(), room.ID, t.ID, "timed out after "+s.opts.TurnTimeout.String())
	})
	s.bus.Notify(room.ID)
	s.log.Info("turn_started", zap.String("room", room.ID), zap.Int64("turn", t.ID), zap.Int64("participant", sess.p.ID),
		zap.Bool("fresh", req.Fresh), zap.Bool("resume", t.Resume != ""))
	writeJSON(w, http.StatusCreated, t)
}

// prepareTurn reads what the turn needs: the session to resume, the cursors, and the prompt with
// everything said and annotated since the last successful turn.
func (s *Server) prepareTurn(ctx context.Context, room *store.Room, fresh bool) (*store.Turn, error) {
	st := s.bus.Store
	t := &store.Turn{RoomID: room.ID, Fresh: fresh}
	prev, err := st.LastDoneTurn(ctx, room.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		prev = &store.Turn{}
	case err != nil:
		return nil, err
	}
	if !fresh && prev.SessionID != "" {
		if _, err := st.AgentSession(ctx, room.ID, prev.SessionID); err == nil {
			t.Resume = prev.SessionID
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	if t.MessageCursor, err = st.MessageCursor(ctx, room.ID); err != nil {
		return nil, err
	}
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
			// The agent's own synthesis is in its session already.
			if m.ID <= t.MessageCursor && m.Kind != store.KindAgent {
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
	// Threads whose only new messages are the agent's own replies hold nothing new for it.
	anns = slices.DeleteFunc(anns, func(a store.Annotation) bool {
		return !slices.ContainsFunc(a.Messages, func(m store.AnnotationMessage) bool {
			return m.ID > prev.AnnotationCursor && m.By != store.ByAgent
		})
	})
	docs, err := st.Documents(ctx, room.ID)
	if err != nil {
		return nil, err
	}
	t.Prompt = buildPrompt(room.Name, t.Resume == "", msgs, anns, prev.AnnotationCursor, docPaths(docs))
	return t, nil
}

// failTurn fails a running turn and wakes the room; a turn already ended is left alone.
func (s *Server) failTurn(ctx context.Context, room string, id int64, reason string) {
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
	job := turn.Job{ID: t.ID, Prompt: t.Prompt, Resume: t.Resume, TimeoutSeconds: int(s.opts.TurnTimeout / time.Second)}
	if t.Resume != "" {
		data, err := s.bus.Store.AgentSession(r.Context(), d.RoomID, t.Resume)
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
	data, err := s.bus.Store.AgentSession(r.Context(), d.RoomID, sid)
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
	s.log.Info("turn_done", zap.String("room", d.RoomID), zap.Int64("turn", t.ID), zap.Strings("revised", done.Report.Revised),
		zap.Strings("created", done.Report.Created), zap.Strings("rejected", done.Report.Rejected), zap.Int("annotated", done.Report.Annotated),
		zap.Int("closed", done.Report.Closed), zap.String("model", done.Model), zap.Int("session_bytes", len(res.Session)))
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
