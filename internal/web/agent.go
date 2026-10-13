package web

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

// agentState is what the room shows about the agent: who hosts it, whether its daemon is there
// (the LED), the last turn, and who may press the button. Sent as the "agent" event.
type agentState struct {
	HostID int64 `json:"host_participant_id,omitempty"`
	// Online is true when the host's daemon is connected and can host (D8).
	Online bool `json:"online"`
	// Reason says why the agent is offline.
	Reason string `json:"reason,omitempty"`
	// Turn is the room's latest turn; Running, the turns running now, those the host's daemon
	// queued under its cap included (003-AC18).
	Turn    *store.Turn  `json:"turn"`
	Running []store.Turn `json:"running"`
	// ParallelTurns lets personas run side by side (003-AC20).
	ParallelTurns bool `json:"parallel_turns"`
	// CanTrigger lists the participants the host granted the button to (D18).
	CanTrigger []int64 `json:"can_trigger"`
	// Daemons are the connected daemons: the host can hand over to one that can host.
	Daemons []presence `json:"daemons"`
	// Personas are the room's agents, each with its button; Queue the presses waiting their turn.
	Personas []store.RoomPersona `json:"personas"`
	Queue    []store.QueuedTurn  `json:"queue"`
	// Models and Efforts are what a persona may ask for, for the pickers.
	Models  []string `json:"models"`
	Efforts []string `json:"efforts"`
	// Cap is the host daemon's cap and approval mode, nil while the host has no daemon.
	Cap *store.DaemonSettings `json:"cap"`
	// CapRequests are the personas asking above the host's cap that the host has not answered
	// (002-AC19); Changes, the persona changes waiting for the host's approval (002-AC21).
	CapRequests []capRequest          `json:"cap_requests"`
	Changes     []store.PersonaChange `json:"changes"`
	// RoomSkills are the room's own skills, those waiting for the host included (004-AC10).
	RoomSkills []store.Skill `json:"room_skills"`
	// Chain is the count of agent-triggered turns against the room's budget (002-AC25).
	Chain store.Chain `json:"chain"`
	// Mentions are where the turns of the room's latest mentions stand, newest first (005-AC4).
	Mentions []store.MentionState `json:"mentions"`
}

// mentionStates bounds the mentions the agent state carries.
// ponytail: older mentions lose their turn state on the page; page them in if rooms outgrow it.
const mentionStates = 200

// capRequest is a persona asking for more than the host cap: it runs at Runs until the host
// approves Asks.
type capRequest struct {
	PersonaID int64      `json:"persona_id"`
	Asks      turn.Level `json:"asks"`
	Runs      turn.Level `json:"runs"`
}

// capped returns what persona p runs at on host daemon d, and whether that is below what it asks.
func capped(d *store.Daemon, p *store.RoomPersona) (turn.Level, bool) {
	return turn.Clamp(p.Asks(), turn.Level{Model: d.Settings.MaxModel, Effort: d.Settings.MaxEffort}, p.Cap.Approved(d.ID))
}

// capRequests lists the personas the host d still has to answer on. The answers are kept per host
// daemon: after a hand-over the new host's cap and answers apply.
func capRequests(d *store.Daemon, ps []store.RoomPersona) []capRequest {
	out := []capRequest{}
	if d == nil {
		return out
	}
	for _, p := range ps {
		if p.Pending {
			continue
		}
		if runs, low := capped(d, &p); low && !p.Cap.Settles(d.ID, p.Asks()) {
			out = append(out, capRequest{PersonaID: p.ID, Asks: p.Asks(), Runs: runs})
		}
	}
	return out
}

// hostDaemon returns the connected daemon of participant pid in room, nil if there is none.
func (s *Server) hostDaemon(room string, pid int64) *presence {
	s.presence.mu.Lock()
	defer s.presence.mu.Unlock()
	for _, p := range s.presence.m {
		if p.room == room && p.conns > 0 && p.ParticipantID == pid {
			c := *p
			return &c
		}
	}
	return nil
}

func (s *Server) agentState(ctx context.Context, room string) (*agentState, error) {
	r, err := s.bus.Store.Room(ctx, room)
	if err != nil {
		return nil, err
	}
	ps, err := s.bus.Store.Participants(ctx, room)
	if err != nil {
		return nil, err
	}
	st := &agentState{HostID: r.HostParticipantID, CanTrigger: []int64{}, Daemons: s.connectedDaemons(room), Models: turn.Models, Efforts: turn.Efforts,
		Chain: r.Chain, ParallelTurns: r.ParallelTurns}
	for _, p := range ps {
		if p.CanTrigger {
			st.CanTrigger = append(st.CanTrigger, p.ID)
		}
	}
	switch h := s.hostDaemon(room, r.HostParticipantID); {
	case h == nil:
		st.Reason = "No one runs the AI for this room right now."
	case !h.CanHost:
		st.Reason = h.Reason
	default:
		st.Online = true
	}
	if st.Turn, err = s.bus.Store.LastTurn(ctx, room); err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if st.Running, err = s.bus.Store.RunningTurns(ctx, room); err != nil {
		return nil, err
	}
	if st.Personas, err = s.bus.Store.RoomPersonas(ctx, room); err != nil {
		return nil, err
	}
	if st.Queue, err = s.bus.Store.Queue(ctx, room); err != nil {
		return nil, err
	}
	host, err := s.bus.Store.HostDaemon(ctx, room)
	switch {
	case errors.Is(err, store.ErrNotFound):
		host = nil
	case err != nil:
		return nil, err
	default:
		st.Cap = &host.Settings
	}
	st.CapRequests = capRequests(host, st.Personas)
	if st.Changes, err = s.bus.Store.PersonaChanges(ctx, room); err != nil {
		return nil, err
	}
	if st.RoomSkills, err = s.bus.Store.RoomSkills(ctx, room); err != nil {
		return nil, err
	}
	if st.Mentions, err = s.bus.Store.MentionStates(ctx, room, mentionStates); err != nil {
		return nil, err
	}
	return st, nil
}

// agentChanged re-runs the host election, wakes the room's streams, and starts a queued turn the
// new host can run. Called whenever a daemon connects, drops or changes its readiness.
func (s *Server) agentChanged(ctx context.Context, room string) {
	if err := s.elect(ctx, room); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.Error("host_election_failed", zap.String("room", room), zap.Error(err))
	}
	s.bus.Notify(room)
	s.drain(ctx, room)
}

// elect keeps the host while its daemon is connected and can host; otherwise the participant
// whose eligible daemon connected first becomes host (D7). With none eligible the host stays, offline.
func (s *Server) elect(ctx context.Context, room string) error {
	s.electMu.Lock()
	defer s.electMu.Unlock()
	r, err := s.bus.Store.Room(ctx, room)
	if err != nil {
		return err
	}
	s.presence.mu.Lock()
	var eligible []presence
	for _, p := range s.presence.m {
		if p.room == room && p.conns > 0 && p.CanHost {
			eligible = append(eligible, *p)
		}
	}
	s.presence.mu.Unlock()
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].since.Before(eligible[j].since) })
	for _, p := range eligible {
		if p.ParticipantID == r.HostParticipantID {
			return nil
		}
	}
	if len(eligible) == 0 {
		return nil
	}
	return s.setHost(ctx, room, eligible[0])
}

// setHost records the new host and says so in the room.
func (s *Server) setHost(ctx context.Context, room string, d presence) error {
	p, err := s.bus.Store.RoomParticipant(ctx, room, d.ParticipantID)
	if err != nil {
		return err
	}
	if err := s.bus.Store.SetHost(ctx, room, p.ID); err != nil {
		return err
	}
	s.log.Info("host_elected", zap.String("room", room), zap.Int64("participant", p.ID))
	return s.bus.Post(ctx, &store.Message{RoomID: room, Kind: store.KindSystem, ParticipantID: p.ID, Sender: p.Name,
		Body: p.Name + " holds the agent(s)."})
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request, sess session) {
	st, err := s.agentState(r.Context(), sess.p.RoomID)
	if err != nil {
		s.serverError(w, "get_agent_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// setAgentMode is the host switching the room's parallel mode (003-AC20). Turning it on starts the
// queued turns of personas that do not run.
func (s *Server) setAgentMode(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		ParallelTurns *bool `json:"parallel_turns"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.ParallelTurns == nil {
		httpError(w, http.StatusBadRequest, "parallel_turns is required")
		return
	}
	room, ok := s.asHost(w, r, sess)
	if !ok {
		return
	}
	m, err := s.bus.Store.SetParallelTurns(r.Context(), room.ID, *req.ParallelTurns, sess.p)
	if err != nil {
		s.serverError(w, "set_agent_mode_failed", err)
		return
	}
	if m != nil {
		s.log.Info("parallel_turns_changed", zap.String("room", room.ID), zap.Bool("on", *req.ParallelTurns))
		s.bus.Notify(room.ID)
		s.drain(r.Context(), room.ID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// asHost loads the room and answers 403 unless the session's participant hosts it.
func (s *Server) asHost(w http.ResponseWriter, r *http.Request, sess session) (*store.Room, bool) {
	room, err := s.bus.Store.Room(r.Context(), sess.p.RoomID)
	if err != nil {
		s.serverError(w, "host_check_failed", err)
		return nil, false
	}
	if room.HostParticipantID != sess.p.ID {
		httpError(w, http.StatusForbidden, "only the agent host can do this")
		return nil, false
	}
	return room, true
}

// handOver lets the host pass the role to a participant whose daemon is connected and can host.
func (s *Server) handOver(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		ParticipantID int64 `json:"participant_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	room, ok := s.asHost(w, r, sess)
	if !ok {
		return
	}
	if req.ParticipantID == room.HostParticipantID {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Held across the check and the write: an election in between would undo the hand-over.
	s.electMu.Lock()
	defer s.electMu.Unlock()
	d := s.hostDaemon(room.ID, req.ParticipantID)
	if d == nil || !d.CanHost {
		httpError(w, http.StatusConflict, "that participant has no connected daemon able to host")
		return
	}
	if err := s.setHost(r.Context(), room.ID, *d); err != nil {
		s.serverError(w, "hand_over_failed", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setCanTrigger lets the host grant or revoke the agent button for a participant (D18).
func (s *Server) setCanTrigger(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		CanTrigger *bool `json:"can_trigger"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.CanTrigger == nil {
		httpError(w, http.StatusBadRequest, "can_trigger is required")
		return
	}
	room, ok := s.asHost(w, r, sess)
	if !ok {
		return
	}
	pid, err := strconv.ParseInt(r.PathValue("participant"), 10, 64)
	if err != nil {
		httpError(w, http.StatusNotFound, "no such participant")
		return
	}
	err = s.bus.Store.SetCanTrigger(r.Context(), room.ID, pid, *req.CanTrigger)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such participant")
		return
	}
	if err != nil {
		s.serverError(w, "set_can_trigger_failed", err)
		return
	}
	s.bus.Notify(room.ID)
	w.WriteHeader(http.StatusNoContent)
}
