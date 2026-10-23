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
	"sync"
	"time"
	"unicode"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/anchor"
	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

// MinTurnTimeout is the shortest turn timeout a server takes: 017-AC18's bound on a vanished
// app's turn, the turn timeout plus 30 s, needs the daemon lease and the sweep to fit in it.
const MinTurnTimeout = time.Minute

const (
	defaultTurnTimeout = 15 * time.Minute
	defaultMaxResult   = 32 << 20
	maxError           = 500
)

// turnGrace lets the daemon report its own timeout before the turn sweeper fails the turn. A var
// for tests.
var turnGrace = time.Minute

var sessionIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// startTurn is a persona's button (D12): the host, or a participant the host allowed, runs one
// turn of that persona on the host's daemon. While another turn runs, or others wait, the press
// joins the room's queue and the answer is 202 (002-AC17).
func (s *Server) startTurn(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		Fresh bool `json:"fresh"`
		// Handoff runs a last turn that writes a handoff note, then a new session reading it (003-AC25).
		Handoff bool `json:"handoff"`
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
	case (req.Fresh || req.Handoff) && !isHost:
		httpError(w, http.StatusForbidden, "only the host can start a new session")
		return
	case req.Fresh && req.Handoff:
		httpError(w, http.StatusBadRequest, "a handoff resumes the current session: fresh and handoff do not go together")
		return
	}
	persona, ok := s.pressedPersona(w, r, room.ID, req.PersonaID)
	if !ok {
		return
	}
	press := &store.QueuedTurn{RoomID: room.ID, PersonaID: persona.ID, ParticipantID: sess.p.ID, Source: store.SourceButton, Fresh: req.Fresh,
		Cause: "pressed by " + oneLine(sess.p.Name)}
	if req.Handoff {
		// A handoff writes its note from the session it replaces: with none, New session is the way.
		prev, err := s.bus.Store.LastDoneTurn(ctx, room.ID, persona.ID)
		if err == nil {
			_, err = s.bus.Store.AgentSession(ctx, room.ID, persona.ID, prev.SessionID)
		}
		if errors.Is(err, store.ErrNotFound) {
			httpError(w, http.StatusConflict, persona.Name+" has no session to hand over: start a new session instead")
			return
		} else if err != nil {
			s.serverError(w, "start_turn_failed", err)
			return
		}
		press.Handoff, press.Cause = store.HandoffWrite, "the host "+oneLine(sess.p.Name)+" asked for a handoff note before a new session"
	}
	// A human took the lead: the agent chain starts over (002-AC26).
	if err := s.bus.Store.ResetChain(ctx, room.ID, store.ResetButton); err != nil {
		s.serverError(w, "start_turn_failed", err)
		return
	}
	unlock, err := s.lockTurns(ctx, room.ID)
	if err != nil {
		s.serverError(w, "start_turn_failed", err)
		return
	}
	defer unlock()
	busy, err := s.roomBusy(ctx, room, persona.ID)
	if err != nil {
		s.serverError(w, "start_turn_failed", err)
		return
	}
	if busy {
		q := press
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
	t, err := s.launch(ctx, room, persona, press, host)
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

// roomBusy reports whether a press of persona must wait: while a turn runs or others wait. With
// parallel on, only the persona's own running or waiting turn holds it back (003-AC20).
func (s *Server) roomBusy(ctx context.Context, room *store.Room, persona int64) (bool, error) {
	running, err := s.bus.Store.RunningTurns(ctx, room.ID)
	if err != nil {
		return false, err
	}
	q, err := s.bus.Store.Queue(ctx, room.ID)
	if err != nil {
		return false, err
	}
	if !room.ParallelTurns {
		return len(running) > 0 || len(q) > 0, nil
	}
	return slices.ContainsFunc(running, func(t store.Turn) bool { return t.PersonaID == persona }) ||
		slices.ContainsFunc(q, func(t store.QueuedTurn) bool { return t.PersonaID == persona }), nil
}

// lockTurns takes turnMu and the room's turn lock, its counterpart across pods; unlock frees both.
// The caller holds no write transaction. turnMu is never held waiting for the room lock, which
// would stall every room of this pod behind one another pod holds: under turnMu the lock is only
// tried, and a busy one is waited for outside it, behind the room's guard in turnWaits, so the pod
// pins one connection per room waiting, not one per press. turnWait bounds the whole wait, the
// guard's included, so a queue of waiters never adds up their timeouts.
func (s *Server) lockTurns(ctx context.Context, room string) (unlock func(), err error) {
	ctx, cancel := context.WithTimeout(ctx, s.turnWait)
	defer cancel()
	s.turnMu.Lock()
	release, ok, err := s.bus.Store.LockRoom(ctx, store.LockTurns, room, true)
	if err != nil || !ok {
		s.turnMu.Unlock()
		if err != nil {
			return nil, err
		}
		guard, err := s.turnWaits.lock(ctx, room)
		if err != nil {
			return nil, err
		}
		locked, _, err := s.bus.Store.LockRoom(ctx, store.LockTurns, room, false)
		if err != nil {
			guard()
			return nil, err
		}
		release = func() { locked(); guard() }
		s.turnMu.Lock()
	}
	return func() {
		release()
		s.turnMu.Unlock()
	}, nil
}

// launch starts a turn of persona on host's daemon, as q asks: pressed by its participant, fresh,
// the re-run of a rejected turn, or a step of a handoff. The caller holds turnMu and the turn lock.
func (s *Server) launch(ctx context.Context, room *store.Room, persona *store.RoomPersona, q *store.QueuedTurn, host *liveDaemon) (*store.Turn, error) {
	t, err := s.prepareTurn(ctx, room, persona, q)
	if err != nil {
		return nil, err
	}
	t.ParticipantID, t.DaemonID, t.RerunOf, t.QueueID = q.ParticipantID, host.id, q.RerunOf, q.ID
	if err := s.bus.Store.StartTurn(ctx, t, room.ParallelTurns); err != nil {
		return nil, err
	}
	// The daemon may have dropped after the presence check, its cleanup finding no turn to fail.
	if h := s.hostDaemon(room.ID, room.HostParticipantID); h == nil || h.id != host.id {
		s.endTurn(ctx, room.ID, t.ID, "the host's daemon disconnected")
	}
	s.bus.Notify(room.ID)
	s.log.Info("turn_started", zap.String("room", room.ID), zap.Int64("turn", t.ID), zap.Int64("persona", persona.ID),
		zap.Int64("participant", q.ParticipantID), zap.Bool("fresh", q.Fresh), zap.Bool("resume", t.Resume != ""), zap.Int64("rerun_of", t.RerunOf),
		zap.String("source", q.Source), zap.String("handoff", t.Handoff))
	return t, nil
}

// drain starts the room's queued turns while the host's daemon is there: the next one once no
// turn runs, or with parallel on, every one whose persona is not running (003-AC20).
func (s *Server) drain(ctx context.Context, roomID string) {
	// A caller's request may end right after it: the queue must still move.
	ctx = context.WithoutCancel(ctx)
	unlock, err := s.lockTurns(ctx, roomID)
	if err != nil {
		s.log.Error("dequeue_failed", zap.String("room", roomID), zap.Error(err))
		return
	}
	defer unlock()
	room, err := s.bus.Store.Room(ctx, roomID)
	if err != nil {
		return
	}
	host := s.hostDaemon(room.ID, room.HostParticipantID)
	if host == nil || !host.CanHost {
		return
	}
	for {
		running, err := s.bus.Store.RunningTurns(ctx, room.ID)
		if err != nil || (!room.ParallelTurns && len(running) > 0) {
			return
		}
		q, err := s.bus.Store.PopQueued(ctx, room.ID, room.ParallelTurns)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				s.log.Error("dequeue_failed", zap.String("room", room.ID), zap.Error(err))
			}
			return
		}
		persona, err := s.bus.Store.RoomPersona(ctx, room.ID, q.PersonaID)
		if err == nil {
			_, err = s.launch(ctx, room, persona, q, host)
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
}

// prepareTurn reads what the turn of persona that q asks for needs: the session to resume, the
// cursors, and the prompt with why it runs and everything said and annotated since that persona's
// last successful turn. The persona's text, model and effort are copied onto the turn: a change
// during it waits for the next.
func (s *Server) prepareTurn(ctx context.Context, room *store.Room, persona *store.RoomPersona, q *store.QueuedTurn) (*store.Turn, error) {
	st := s.bus.Store
	fresh := q.Fresh
	t := &store.Turn{RoomID: room.ID, Fresh: fresh, PersonaID: persona.ID, PersonaName: persona.Name, PersonaText: persona.Text,
		ReqModel: persona.Model, ReqEffort: persona.Effort, Cause: q.Cause, Handoff: q.Handoff}
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
	var named []int64
	if len(anns) > 0 {
		// A thread naming other personas is theirs to answer (009-AC30).
		personas, err := st.RoomPersonas(ctx, room.ID)
		if err != nil {
			return nil, err
		}
		anns = slices.DeleteFunc(anns, func(a store.Annotation) bool { return othersThread(participantBodies(a), personas, persona.ID) })
		if q.ID != 0 {
			if named, err = st.NoteMentionThreads(ctx, q.ID, persona.ID); err != nil {
				return nil, err
			}
		}
	}
	docs, err := st.Documents(ctx, room.ID)
	if err != nil {
		return nil, err
	}
	var renamed []string
	// A resumed session remembers the old paths (005-AC25); a new one reads the workspace afresh.
	if t.Resume != "" {
		if renamed, err = s.renamedPaths(ctx, room.ID, prev.MessageCursor, t.MessageCursor, docs); err != nil {
			return nil, err
		}
	}
	preamble, err := s.presencePreamble(ctx, room, persona, t.Cause)
	if err != nil {
		return nil, err
	}
	t.Prompt = buildPrompt(promptInput{Preamble: preamble, Room: room.Name, Persona: persona.Name, Self: persona.ID, NewSession: t.Resume == "",
		Handoff: t.Handoff, Msgs: msgs, Changes: changes, Anns: anns, Named: named, AnnCursor: prev.AnnotationCursor, Paths: docPaths(docs), Renamed: renamed,
		SpokenWords: s.speakingWords(room, persona)})
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

// failDaemonTurn fails the room's running turns that d runs (D6: a dropped host loses its turns).
func (s *Server) failDaemonTurn(ctx context.Context, d *store.Daemon, reason string) {
	ts, err := s.bus.Store.RunningTurns(ctx, d.RoomID)
	if err != nil {
		s.log.Error("running_turn_lookup_failed", zap.String("room", d.RoomID), zap.Error(err))
		return
	}
	for _, t := range ts {
		if t.DaemonID == d.ID {
			s.endTurn(ctx, d.RoomID, t.ID, reason)
		}
	}
	s.drain(ctx, d.RoomID)
}

// rejectTurn sets aside the result of turn id, which conflicts on doc, and queues its re-run or
// fails it (003-AC21).
func (s *Server) rejectTurn(ctx context.Context, room string, id, daemon int64, doc string) {
	_, rerun, err := s.bus.Store.RejectTurn(ctx, room, id, daemon, doc)
	if err != nil {
		if !errors.Is(err, store.ErrTurnNotRunning) {
			s.log.Error("reject_turn_failed", zap.String("room", room), zap.Int64("turn", id), zap.Error(err))
		}
		return
	}
	outcome := "failed"
	if rerun {
		outcome = "rerun"
	}
	s.conflicts[outcome].Add(1)
	s.log.Warn("turn_conflict", zap.String("room", room), zap.Int64("turn", id), zap.String("doc", doc), zap.String("outcome", outcome))
	s.bus.Notify(room)
	s.drain(ctx, room)
}

// pinBase pins the base revisions of the turn's workspace on t, answering the error itself: every
// fetch of one turn's workspace holds the same documents (003-AC21). A daemon fetches the
// workspace once its turn runs: a turn still shown queued on its host lost that state change.
func (s *Server) pinBase(w http.ResponseWriter, r *http.Request, t *store.Turn) bool {
	base, err := s.bus.Store.PinBaseRevisions(r.Context(), t.RoomID, t.ID)
	if err != nil {
		s.serverError(w, "workspace_failed", err)
		return false
	}
	t.BaseRevisions = base
	if t.HostQueued {
		if err := s.hostQueued(r.Context(), t.RoomID, t.ID, t.DaemonID, false); err != nil {
			s.log.Warn("turn_state_failed", zap.String("room", t.RoomID), zap.Int64("turn", t.ID), zap.Error(err))
		}
	}
	return true
}

// hostQueued records that the turn waits in its host's queue, or runs: the room is told, and a
// running turn's timeout counts from now (003-AC18).
func (s *Server) hostQueued(ctx context.Context, room string, id, daemon int64, queued bool) error {
	changed, err := s.bus.Store.SetHostQueued(ctx, room, id, daemon, queued)
	if err != nil || !changed {
		return err
	}
	s.bus.Notify(room)
	s.log.Info("turn_host_queued", zap.String("room", room), zap.Int64("turn", id), zap.Bool("queued", queued))
	return nil
}

// postTurnState is the daemon saying that it holds the turn in its local queue under the host's
// cap, or that the turn left it and runs now (003-AC18).
func (s *Server) postTurnState(w http.ResponseWriter, r *http.Request, d *store.Daemon, _ string) {
	t, ok := s.daemonTurn(w, r, d)
	if !ok {
		return
	}
	var req struct {
		HostQueued bool `json:"host_queued"`
	}
	if !decode(w, r, &req) {
		return
	}
	switch err := s.hostQueued(r.Context(), d.RoomID, t.ID, d.ID, req.HostQueued); {
	case errors.Is(err, store.ErrTurnNotRunning):
		httpError(w, http.StatusConflict, err.Error())
	case err != nil:
		s.serverError(w, "turn_state_failed", err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// cancelQueued drops a queued turn: the participant who queued it may, and for a turn an agent
// queued, anyone allowed to press (002-AC17, 002-AC24).
func (s *Server) cancelQueued(w http.ResponseWriter, r *http.Request, sess session) {
	id, err := strconv.ParseInt(r.PathValue("queued"), 10, 64)
	if err != nil {
		httpError(w, http.StatusNotFound, "no such queued turn")
		return
	}
	err = s.bus.Store.CancelQueued(r.Context(), sess.p.RoomID, id, sess.p.ID, s.canPress(r.Context(), sess.p))
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpError(w, http.StatusNotFound, "no such queued turn")
	case errors.Is(err, store.ErrForbidden):
		httpError(w, http.StatusForbidden, "only who queued a turn, or for an agent's, who may press, can cancel it")
	case err != nil:
		s.serverError(w, "cancel_queued_failed", err)
	default:
		s.bus.Notify(sess.p.RoomID)
		w.WriteHeader(http.StatusNoContent)
	}
}

// stopTurn stops a running {turn}: whoever started it, the host or a room admin may. The turn ends
// at once, its result is refused when it comes, and the host's daemon is told to kill claude
// (005-AC5).
func (s *Server) stopTurn(w http.ResponseWriter, r *http.Request, sess session) {
	// The JSON body, empty, is what a cross-site form cannot send.
	if !decode(w, r, &struct{}{}) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("turn"), 10, 64)
	if err != nil {
		httpError(w, http.StatusNotFound, "no such turn")
		return
	}
	ctx, room := r.Context(), sess.p.RoomID
	t, err := s.bus.Store.Turn(ctx, room, id)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such turn")
		return
	}
	if err != nil {
		s.serverError(w, "stop_turn_failed", err)
		return
	}
	if t.Status != store.TurnRunning {
		httpError(w, http.StatusConflict, store.ErrTurnNotRunning.Error())
		return
	}
	if allowed, err := s.mayStop(ctx, &sess, t); err != nil {
		s.serverError(w, "stop_turn_failed", err)
		return
	} else if !allowed {
		httpError(w, http.StatusForbidden, "only who started the turn, the host or a room admin can stop it")
		return
	}
	switch _, err := s.bus.Store.StopTurn(ctx, room, id, oneLine(sess.p.Name)); {
	case errors.Is(err, store.ErrTurnNotRunning):
		httpError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.serverError(w, "stop_turn_failed", err)
		return
	}
	s.log.Info("turn_stopped", zap.String("room", room), zap.Int64("turn", id), zap.Int64("participant", sess.p.ID))
	s.bus.Notify(room)
	s.drain(ctx, room)
	w.WriteHeader(http.StatusNoContent)
}

// mayStop reports whether the member may stop turn t: who started it, the host, or a room admin.
func (s *Server) mayStop(ctx context.Context, sess *session, t *store.Turn) (bool, error) {
	if t.ParticipantID == sess.p.ID {
		return true, nil
	}
	room, err := s.bus.Store.Room(ctx, sess.p.RoomID)
	if err != nil {
		return false, err
	}
	if room.HostParticipantID == sess.p.ID {
		return true, nil
	}
	return s.roomAdmin(ctx, sess)
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
		Persona: t.PersonaName, System: t.PersonaText, Model: t.ReqModel, Effort: t.ReqEffort, Cause: t.Cause}
	// The host's approval holds for this persona on the daemon it was given for only (002-AC20).
	if p, err := s.bus.Store.RoomPersona(r.Context(), d.RoomID, t.PersonaID); err == nil {
		job.Approved, job.Autocompact = p.Cap.Approved(d.ID), p.Autocompact
	} else if !errors.Is(err, store.ErrNotFound) {
		s.serverError(w, "get_job_failed", err)
		return
	}
	ks, err := s.bus.Store.TurnSkills(r.Context(), d.RoomID, t.ID, false)
	if err != nil {
		s.serverError(w, "get_job_failed", err)
		return
	}
	for _, k := range ks {
		job.Skills = append(job.Skills, turn.Skill{Name: k.Name, Version: k.Version})
	}
	ps, err := s.bus.Store.TurnPlugins(r.Context(), d.RoomID, t.ID)
	if err != nil {
		s.serverError(w, "get_job_failed", err)
		return
	}
	for _, p := range ps {
		tp := turn.Plugin{ID: p.PluginID, Name: p.Name, SHA256: p.SHA256, Version: p.N}
		if m := manifestOf(p.Manifest); m != nil {
			for _, o := range m.Options {
				tp.Options = append(tp.Options, turn.PluginOption{Key: o.Key, Sensitive: o.Sensitive, Required: o.Required, Default: o.Default})
			}
		}
		job.Plugins = append(job.Plugins, tp)
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

// getWorkspace streams the turn's whole workspace as a tar (D10, D20). A daemon with a cache asks
// for it only as a fallback, and says why (002-AC38).
func (s *Server) getWorkspace(w http.ResponseWriter, r *http.Request, d *store.Daemon, _ string) {
	t, ok := s.daemonTurn(w, r, d)
	if !ok || !s.pinBase(w, r, t) {
		return
	}
	if reason := r.URL.Query().Get("fallback"); reason != "" {
		if n := s.fallbacks[reason]; n != nil {
			n.Add(1)
		}
		s.log.Warn("workspace_fallback", zap.String("room", d.RoomID), zap.Int64("turn", t.ID), zap.Int64("daemon", d.ID),
			zap.String("reason", truncate(reason, 32)))
	}
	w.Header().Set("Content-Type", "application/x-tar")
	if err := s.writeWorkspace(r.Context(), w, t, nil); err != nil {
		// Headers are gone: the daemon sees a truncated tar and fails the turn.
		s.log.Error("workspace_failed", zap.String("room", d.RoomID), zap.Int64("turn", t.ID), zap.Error(err))
	}
}

// getManifest lists the turn's workspace files with their sha256 and size (002-AC37).
func (s *Server) getManifest(w http.ResponseWriter, r *http.Request, d *store.Daemon, _ string) {
	t, ok := s.daemonTurn(w, r, d)
	if !ok || !s.pinBase(w, r, t) {
		return
	}
	m, err := s.manifest(r.Context(), t)
	if err != nil {
		s.serverError(w, "workspace_manifest_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// maxPathsRequest bounds the list of paths a daemon asks for: thousands of workspace paths.
const maxPathsRequest = 1 << 20

// postWorkspace streams a tar of the workspace files the daemon lists, the ones its cache lacks
// (002-AC37). Every path must be one of this turn's workspace.
func (s *Server) postWorkspace(w http.ResponseWriter, r *http.Request, d *store.Daemon, _ string) {
	t, ok := s.daemonTurn(w, r, d)
	if !ok || !s.pinBase(w, r, t) {
		return
	}
	var req turn.PathsRequest
	if !decodeLimit(w, r, &req, maxPathsRequest) {
		return
	}
	only := make(map[string]bool, len(req.Paths))
	for _, p := range req.Paths {
		only[p] = true
	}
	// Listing the paths reads no blob; only the requested files are loaded below.
	found := 0
	err := s.walkWorkspace(r.Context(), t, func(name string, _ int64, _ func() ([]byte, error)) error {
		if only[name] {
			found++
		}
		return nil
	})
	if err != nil {
		s.serverError(w, "workspace_failed", err)
		return
	}
	if found != len(only) {
		httpError(w, http.StatusBadRequest, "a path is not in this turn's workspace")
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	if err := s.writeWorkspace(r.Context(), w, t, only); err != nil {
		s.log.Error("workspace_failed", zap.String("room", d.RoomID), zap.Int64("turn", t.ID), zap.Error(err))
	}
}

// getSession serves the stored jsonl of the session the daemon's running turn resumes (D6, D23).
func (s *Server) getSession(w http.ResponseWriter, r *http.Request, d *store.Daemon, _ string) {
	sid := r.PathValue("session")
	running, err := s.bus.Store.RunningTurns(r.Context(), d.RoomID)
	if err != nil {
		s.serverError(w, "get_session_failed", err)
		return
	}
	i := slices.IndexFunc(running, func(t store.Turn) bool { return t.DaemonID == d.ID && t.Resume == sid })
	if i < 0 {
		httpError(w, http.StatusNotFound, "no such session for this daemon's turn")
		return
	}
	t := &running[i]
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
		s.failTurn(r.Context(), d.RoomID, t.ID, "the result from the host's daemon was refused: too large or malformed")
		return
	}
	s.recordSetup(r.Context(), d.RoomID, t.ID, res.SkippedPlugins)
	s.recordBlocked(r.Context(), d.RoomID, t.PersonaName, res.Blocked)
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
		ModelNote: sanitize(res.ModelNote, turn.MaxModelNote), ModelMismatch: res.ModelMismatch,
		// An effort claude does not know is not shown as if it ran.
		Effort:        knownOr(res.Effort, turn.Efforts),
		Capped:        res.Capped,
		SessionOffset: res.SessionOffset, Session: res.Session,
		Report: store.TurnReport{Rejected: o.Rejected, Ignored: o.Ignored},
	}
	if o.Spoken != "" {
		out.Spoken = spokenText(o.Spoken, s.spokenWords(r.Context(), d.RoomID))
	}
	// A window under the tokens it holds is a daemon's mistake: the room shows no percentage then.
	if res.ContextTokens > 0 && res.ContextWindow >= res.ContextTokens {
		out.ContextTokens, out.ContextWindow = res.ContextTokens, res.ContextWindow
	} else if res.ContextTokens > 0 && res.ContextWindow == 0 {
		out.ContextTokens = res.ContextTokens
	}
	for _, doc := range o.Docs {
		out.Documents = append(out.Documents, store.AgentWrite{Name: doc.Name, Format: doc.Format, Body: doc.Body})
	}
	for _, rp := range o.Replies {
		out.Replies = append(out.Replies, store.AgentReply{AnnotationID: rp.Annotation, Body: sanitize(rp.Body, turn.MaxReplyLen), Done: rp.Status == store.StatusDone})
	}
	if err := s.refuseOthersThreads(r.Context(), t, out); err != nil {
		s.log.Error("complete_turn_failed", zap.String("room", d.RoomID), zap.Int64("turn", t.ID), zap.Error(err))
		s.failTurn(r.Context(), d.RoomID, t.ID, "the room could not apply the result")
		httpError(w, http.StatusInternalServerError, "internal error")
		return
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
	var conflict *store.ConflictError
	switch {
	case errors.As(err, &conflict):
		s.rejectTurn(r.Context(), d.RoomID, t.ID, d.ID, conflict.Doc)
		httpError(w, http.StatusConflict, err.Error())
		return
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
	if out.ModelMismatch {
		s.log.Warn("turn_model_mismatch", zap.String("room", d.RoomID), zap.Int64("turn", t.ID), zap.String("asked", out.Model), zap.String("reported", out.ModelNote))
	}
	s.chainLinks(r.Context(), done, out.Synthesis)
	s.queueSpeech(r.Context(), done, o.Spoken, out.Synthesis)
	s.bus.Notify(d.RoomID)
	s.drain(r.Context(), d.RoomID)
	s.log.Info("turn_done", zap.String("room", d.RoomID), zap.Int64("turn", t.ID), zap.Strings("revised", done.Report.Revised),
		zap.Strings("created", done.Report.Created), zap.Strings("rejected", done.Report.Rejected), zap.Int("annotated", done.Report.Annotated),
		zap.Int("closed", done.Report.Closed), zap.String("model", done.Model), zap.String("effort", done.Effort), zap.Int("session_bytes", len(res.Session)))
	writeJSON(w, http.StatusOK, done)
}

// recordSetup marks the turn's plugins the daemon left out for an option the host has not set,
// and clears the mark on the others it pinned: the Capabilities tab says which (009-AC24). A
// failure only logs: the turn's result matters more.
func (s *Server) recordSetup(ctx context.Context, room string, id int64, skipped []turn.SkippedPlugin) {
	ps, err := s.bus.Store.TurnPlugins(ctx, room, id)
	if err != nil || len(ps) == 0 {
		return
	}
	pinned := make([]string, len(ps))
	for i, p := range ps {
		pinned[i] = p.Name
	}
	var names []string
	for _, k := range skipped {
		if k.Reason == turn.NeedsHostSetup {
			names = append(names, k.Name)
		}
	}
	changed, err := s.bus.Store.SetPluginsSetup(ctx, room, pinned, names)
	if err != nil {
		s.log.Warn("plugin_setup_record_failed", zap.String("room", room), zap.Int64("turn", id), zap.Error(err))
		return
	}
	if changed {
		s.bus.Notify(room)
	}
}

// refuseOthersThreads drops the replies and closes of turn t to threads that belong to other
// personas, and lists each in its report (009-AC30).
func (s *Server) refuseOthersThreads(ctx context.Context, t *store.Turn, out *store.TurnOutcome) error {
	if len(out.Replies) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(out.Replies))
	for _, r := range out.Replies {
		ids = append(ids, r.AnnotationID)
	}
	bodies, err := s.bus.Store.ParticipantNotes(ctx, t.RoomID, ids)
	if err != nil {
		return err
	}
	personas, err := s.bus.Store.RoomPersonas(ctx, t.RoomID)
	if err != nil {
		return err
	}
	out.Replies = slices.DeleteFunc(out.Replies, func(r store.AgentReply) bool {
		owners := threadOwners(bodies[r.AnnotationID], personas)
		if len(owners) == 0 || slices.Contains(owners, t.PersonaID) {
			return false
		}
		var names []string
		for _, p := range personas {
			if slices.Contains(owners, p.ID) {
				names = append(names, "@"+p.Name)
			}
		}
		out.Report.Ignored = append(out.Report.Ignored, fmt.Sprintf("reply to annotation %d: it belongs to %s", r.AnnotationID, strings.Join(names, ", ")))
		return true
	})
	return nil
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

// roomGuards is a one-slot semaphore per room, kept while someone holds or waits for it.
type roomGuards struct {
	mu sync.Mutex
	m  map[string]*roomGuard
}

type roomGuard struct {
	slot chan struct{}
	// n counts the holder and the waiters; guarded by roomGuards.mu.
	n int
}

// lock takes room's guard, or fails once ctx ends; unlock frees it. The room is forgotten once
// nobody holds or waits for it.
func (g *roomGuards) lock(ctx context.Context, room string) (unlock func(), err error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = map[string]*roomGuard{}
	}
	e := g.m[room]
	if e == nil {
		e = &roomGuard{slot: make(chan struct{}, 1)}
		g.m[room] = e
	}
	e.n++
	g.mu.Unlock()
	leave := func() {
		g.mu.Lock()
		if e.n--; e.n == 0 {
			delete(g.m, room)
		}
		g.mu.Unlock()
	}
	select {
	case e.slot <- struct{}{}:
		return func() { <-e.slot; leave() }, nil
	case <-ctx.Done():
		leave()
		return nil, fmt.Errorf("room lock: %w", ctx.Err())
	}
}
