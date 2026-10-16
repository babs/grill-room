package web

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

// agentState is what the room shows about the agent: who hosts it, whether its daemon is there
// (the LED), the last turn, and who may press the button. Sent as the "agent" event.
type agentState struct {
	HostID int64 `json:"host_participant_id,omitempty"`
	// YouHost, for the participant asking, is set when they decide as the AI host: the host's
	// seat, or another seat of the host's profile in the room (daemon-r2). It names no seat, so it
	// tells no one else which seats share a profile.
	YouHost bool `json:"you_host,omitempty"`
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
	// NoteMentions are the same for the room's latest note mentions, by note message (009-AC29).
	NoteMentions []store.MentionState `json:"note_mentions"`
	// HostBy is the chat line of the latest move of the role by someone other than the previous
	// host, "Alice made Bob the AI host", for the previous host's notice (009-AC34).
	HostBy string `json:"host_by,omitempty"`
	// NotesWatchDelay is how long, in seconds, the notes watcher waits after the last new note (009-AC32).
	NotesWatchDelay int `json:"notes_watch_delay"`
	// EgressAdmins lets the room's admins allow websites for the room (014-AC12); EgressHost says
	// why no one can now, "" when the AI host's app takes approvals (014-AC9).
	EgressAdmins bool   `json:"egress_admins_allow"`
	EgressHost   string `json:"egress_host,omitempty"`
	// Connectors are the claude.ai connectors the host turned on for the room, as their app last
	// said; none while it is offline (014-AC19).
	Connectors []roomConnector `json:"connectors"`
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

// canHostDaemon returns the first connected daemon of participant pid in room able to host, nil if
// none (009-AC33).
func (s *Server) canHostDaemon(room string, pid int64) *presence {
	s.presence.mu.Lock()
	defer s.presence.mu.Unlock()
	for _, p := range s.presence.m {
		if p.room == room && p.conns > 0 && p.ParticipantID == pid && p.CanHost {
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
		Chain: r.Chain, ParallelTurns: r.ParallelTurns, NotesWatchDelay: int(s.opts.NotesWatchDelay / time.Second),
		EgressAdmins: r.EgressAdmins, EgressHost: s.hostEgress(room, r.HostParticipantID)}
	if v, ok := s.hostBy.Load(room); ok {
		st.HostBy, _ = v.(string)
	}
	for _, p := range ps {
		if p.CanTrigger {
			st.CanTrigger = append(st.CanTrigger, p.ID)
		}
	}
	st.Connectors = []roomConnector{}
	if h := s.hostDaemon(room, r.HostParticipantID); h != nil && h.Connectors != nil {
		st.Connectors = h.Connectors
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
		// A host with no daemon row removed the room from their app: pairing again keeps the row (003b-AC10).
		if !st.Online && r.HostParticipantID != 0 {
			st.Reason = "The AI host removed this room from their computer."
		}
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
	if st.NoteMentions, err = s.bus.Store.NoteMentionStates(ctx, room, mentionStates); err != nil {
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
	s.releaseStarterSkills(ctx, room)
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

// releaseStarterSkills turns on the skills the room's starter brought once a host who approves
// without asking is online; under "approve everything" they wait for that host (006-AC13).
func (s *Server) releaseStarterSkills(ctx context.Context, room string) {
	r, err := s.bus.Store.Room(ctx, room)
	if err != nil {
		return
	}
	if h := s.hostDaemon(room, r.HostParticipantID); h == nil || !h.CanHost {
		return
	}
	names, err := s.bus.Store.ReleaseStarterSkills(ctx, room)
	if err != nil {
		s.log.Error("release_starter_skills_failed", zap.String("room", room), zap.Error(err))
		return
	}
	if len(names) == 0 {
		return
	}
	p, err := s.bus.Store.RoomParticipant(ctx, room, r.HostParticipantID)
	if err != nil {
		return
	}
	s.log.Info("starter_skills_released", zap.String("room", room), zap.Strings("skills", names))
	if err := s.bus.Post(ctx, &store.Message{RoomID: room, Kind: store.KindSystem, ParticipantID: p.ID, Sender: p.Name,
		Body: "The starter's skills are on, as " + p.Name + "'s AI host approves without asking: " + strings.Join(names, ", ") + "."}); err != nil {
		s.log.Error("post_failed", zap.String("room", room), zap.Error(err))
	}
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
	s.hostBy.Delete(room)
	s.log.Info("host_elected", zap.String("room", room), zap.Int64("participant", p.ID))
	return s.bus.Post(ctx, &store.Message{RoomID: room, Kind: store.KindSystem, ParticipantID: p.ID, Sender: p.Name,
		Body: p.Name + " holds the agent(s)."})
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request, sess session) {
	st, err := s.agentState(r.Context(), sess.p.RoomID)
	if err == nil {
		st.YouHost, err = s.bus.Store.HostsAs(r.Context(), sess.p.RoomID, sess.p)
	}
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

// asApprover is asHost for an approval: any seat of the host's profile in the room answers it
// (daemon-r2).
func (s *Server) asApprover(w http.ResponseWriter, r *http.Request, sess session) (*store.Room, bool) {
	room, err := s.bus.Store.Room(r.Context(), sess.p.RoomID)
	if err != nil {
		s.serverError(w, "host_check_failed", err)
		return nil, false
	}
	host, err := s.bus.Store.HostsAs(r.Context(), room.ID, sess.p)
	if err != nil {
		s.serverError(w, "host_check_failed", err)
		return nil, false
	}
	if !host {
		httpError(w, http.StatusForbidden, "only the agent host can do this")
		return nil, false
	}
	return room, true
}

// handOver passes the AI host role to a participant whose daemon is connected and can host: the
// host does it (003), so does a room admin, host or not, and the super-admin, sess nil (009-AC33).
// A turn already running stays on its daemon, which alone may answer it; the queue drains to the
// new host (009-AC34).
func (s *Server) handOver(w http.ResponseWriter, r *http.Request, sess *session) {
	var req struct {
		ParticipantID int64 `json:"participant_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	room := r.PathValue("room")
	if s.transferHost(w, r, sess, room, req.ParticipantID) {
		s.agentChanged(r.Context(), room)
	}
}

// transferHost checks the right and moves the role, answering the request; true when it moved.
func (s *Server) transferHost(w http.ResponseWriter, r *http.Request, sess *session, room string, to int64) bool {
	// Held across the checks and the write: an election in between would undo the hand-over.
	s.electMu.Lock()
	defer s.electMu.Unlock()
	rm, err := s.bus.Store.Room(r.Context(), room)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such room")
		return false
	}
	if err != nil {
		s.serverError(w, "hand_over_failed", err)
		return false
	}
	byHost := sess != nil && rm.HostParticipantID == sess.p.ID
	if sess != nil {
		// Looked up for the host too: a room admin's hand-over is audited either way.
		admin, err := s.roomAdmin(r.Context(), sess)
		if err != nil {
			s.serverError(w, "hand_over_failed", err)
			return false
		}
		if !byHost && !admin {
			httpError(w, http.StatusForbidden, "only the AI host or a room admin can do this")
			return false
		}
	}
	if to == rm.HostParticipantID {
		w.WriteHeader(http.StatusNoContent)
		return false
	}
	if s.canHostDaemon(room, to) == nil {
		httpError(w, http.StatusConflict, "that participant has no connected daemon able to host")
		return false
	}
	var by *store.Participant
	if sess != nil {
		by = sess.p
	}
	line, err := s.bus.Store.HandOver(s.roomAuditing(r, sess, ActionHostTransfer, room, nil), room, to, by)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such participant in this room")
		return false
	}
	if err != nil {
		s.serverError(w, "hand_over_failed", err)
		return false
	}
	if byHost {
		s.hostBy.Delete(room)
	} else {
		s.hostBy.Store(room, line)
	}
	s.log.Info("host_handed_over", zap.String("room", room), zap.Int64("from", rm.HostParticipantID), zap.Int64("to", to), zap.Bool("by_host", byHost),
		zap.Bool("super_admin", sess == nil))
	w.WriteHeader(http.StatusNoContent)
	return true
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
