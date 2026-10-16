package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
)

// canPress reports the agent button right: the host holds it, others once the host granted it (D18).
func (s *Server) canPress(ctx context.Context, p *store.Participant) bool {
	if p.CanTrigger {
		return true
	}
	room, err := s.bus.Store.Room(ctx, p.RoomID)
	return err == nil && room.HostParticipantID == p.ID
}

// controlChain sets the room's chain budget, re-arms the chain or suspends its limit. Each lets
// agents spend more of the host's subscription, so it takes the agent button right; each change
// is a line in the chat (002-AC25, 002-AC27).
func (s *Server) controlChain(w http.ResponseWriter, r *http.Request, sess session) {
	var req struct {
		Budget    *int  `json:"budget"`
		Rearm     bool  `json:"rearm"`
		Suspended *bool `json:"suspended"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Budget != nil && (*req.Budget < 1 || *req.Budget > store.MaxChainBudget) {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("budget must be 1 to %d", store.MaxChainBudget))
		return
	}
	if !s.canPress(r.Context(), sess.p) {
		httpError(w, http.StatusForbidden, "only the host and the participants they allow can steer the agent chain")
		return
	}
	c, m, err := s.bus.Store.ControlChain(r.Context(), sess.p.RoomID, store.ChainControl{Budget: req.Budget, Rearm: req.Rearm, Suspended: req.Suspended}, sess.p)
	if err != nil {
		s.serverError(w, "control_chain_failed", err)
		return
	}
	if m != nil {
		s.bus.Notify(sess.p.RoomID)
		s.log.Info("chain_changed", zap.String("room", sess.p.RoomID), zap.Int64("participant", sess.p.ID),
			zap.Int("budget", c.Budget), zap.Int("count", c.Count), zap.Bool("suspended", c.Suspended))
	}
	writeJSON(w, http.StatusOK, c)
}

// mentionQuote is the most of a mentioning message the called persona's preamble quotes; less when
// the rest of the cause leaves less room on the line (003-AC22, 009-AC29).
const mentionQuote = 50

// mentionNotice tells only the author of a message why a persona it names will not run (005-AC3).
type mentionNotice struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Why a mention runs nothing, as its author reads it (005-AC3).
const (
	whyNoRight   = "you may not trigger agents in this room: the host can allow you"
	whyOffline   = "no host is online to run it"
	whyPending   = "it waits for the host's approval"
	whyUnknown   = "no persona or participant has that name"
	maxNoticeLen = 64
)

// mentionTurns queues a turn of each persona a human's text message m names with @name, as that
// human's press, and links m to it: only for someone allowed to press, while the host's daemon can
// run it. It resets the chain, since a human took the lead (002-AC23, 002-AC26, 005-AC4). It
// returns, for m's author only, why each named persona will not run and which names match nobody
// (005-AC3).
func (s *Server) mentionTurns(ctx context.Context, p *store.Participant, m *store.Message) []mentionNotice {
	return s.callMentioned(ctx, p, m.Body, "", "", nil, func(ctx context.Context, q *store.QueuedTurn) error {
		return s.bus.Store.EnqueueMention(ctx, m.ID, q)
	})
}

// noteMentionTurns is mentionTurns for message m of note thread ann: the same rules, a cause that
// names the note, and the link on the note message (009-AC29).
func (s *Server) noteMentionTurns(ctx context.Context, p *store.Participant, ann int64, m *store.AnnotationMessage) []mentionNotice {
	return s.callMentioned(ctx, p, m.Body, "", fmt.Sprintf(" in note #%d", ann), nil, func(ctx context.Context, q *store.QueuedTurn) error {
		return s.bus.Store.EnqueueNoteMention(ctx, m.ID, q)
	})
}

// callMentioned queues, through enqueue, a turn of each persona body names, written by p where
// says (" in note #3", or "" for the chat), and returns the notices for p. A persona or name prev
// already held, and a persona in skip, count for nothing.
func (s *Server) callMentioned(ctx context.Context, p *store.Participant, body, prev, where string, skip []int64, enqueue func(context.Context, *store.QueuedTurn) error) []mentionNotice {
	if !strings.Contains(body, "@") {
		return nil
	}
	ctx = context.WithoutCancel(ctx)
	personas, err := s.bus.Store.RoomPersonas(ctx, p.RoomID)
	if err != nil {
		s.log.Error("mention_failed", zap.String("room", p.RoomID), zap.Error(err))
		return nil
	}
	notices := s.unknownMentions(ctx, p.RoomID, body, personas)
	called := mentioned(body, personas)
	if prev != "" || len(skip) > 0 {
		before := mentionRe.FindAllStringSubmatch(proseText(prev), -1)
		notices = slices.DeleteFunc(notices, func(n mentionNotice) bool {
			return slices.ContainsFunc(before, func(m []string) bool { return strings.EqualFold(truncate(m[1], maxNoticeLen), n.Name) })
		})
		was := mentioned(prev, personas)
		called = slices.DeleteFunc(called, func(c store.RoomPersona) bool {
			return slices.Contains(skip, c.ID) || slices.ContainsFunc(was, func(w store.RoomPersona) bool { return w.ID == c.ID })
		})
	}
	if len(called) == 0 {
		return notices
	}
	room, err := s.bus.Store.Room(ctx, p.RoomID)
	if err != nil {
		s.log.Error("mention_failed", zap.String("room", p.RoomID), zap.Error(err))
		return notices
	}
	var why string
	switch h := s.hostDaemon(room.ID, room.HostParticipantID); {
	case room.HostParticipantID != p.ID && !p.CanTrigger:
		why = whyNoRight
	case h == nil || !h.CanHost:
		why = whyOffline
	}
	var runnable []store.RoomPersona
	for _, persona := range called {
		switch {
		case why != "":
			notices = append(notices, mentionNotice{persona.Name, why})
		case persona.Pending:
			notices = append(notices, mentionNotice{persona.Name, whyPending})
		default:
			runnable = append(runnable, persona)
		}
	}
	if len(runnable) == 0 {
		return notices
	}
	if err := s.bus.Store.ResetChain(ctx, room.ID, store.ResetMention); err != nil {
		s.log.Error("mention_failed", zap.String("room", room.ID), zap.Error(err))
		return notices
	}
	prefix := fmt.Sprintf("@mention%s by %s: ", where, oneLine(p.Name))
	cause := prefix + `"` + fitQuote(oneLine(body), causeMax-len([]rune(prefix))-2) + `"`
	s.turnMu.Lock()
	for _, persona := range runnable {
		q := &store.QueuedTurn{RoomID: room.ID, PersonaID: persona.ID, ParticipantID: p.ID, Cause: cause}
		err := enqueue(ctx, q)
		// ErrNotFound: removed since the list was read, so no longer a name to explain.
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.log.Error("queue_turn_failed", zap.String("room", room.ID), zap.Int64("persona", persona.ID), zap.Error(err))
		}
	}
	s.turnMu.Unlock()
	s.log.Info("mention_queued", zap.String("room", room.ID), zap.Int64("participant", p.ID), zap.Int("personas", len(runnable)))
	s.bus.Notify(room.ID)
	s.drain(ctx, room.ID)
	return notices
}

// fitQuote cuts quote to at most n runes, mentionQuote at most, its last one "…" when cut; a name too
// long to leave room still gets a few words, which the preamble's line cut then trims.
func fitQuote(quote string, n int) string {
	n = max(min(n, mentionQuote+1), 10)
	if r := []rune(quote); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return quote
}

// unknownMentions returns a notice for each distinct @name of body that names no persona of the
// room and no participant, as mentionToken spells them.
func (s *Server) unknownMentions(ctx context.Context, room, body string, personas []store.RoomPersona) []mentionNotice {
	ps, err := s.bus.Store.Participants(ctx, room)
	if err != nil {
		s.log.Error("mention_failed", zap.String("room", room), zap.Error(err))
		return nil
	}
	var out []mentionNotice
	for _, m := range mentionRe.FindAllStringSubmatch(proseText(body), -1) {
		name := m[1]
		known := slices.ContainsFunc(personas, func(p store.RoomPersona) bool { return strings.EqualFold(p.Name, name) }) ||
			slices.ContainsFunc(ps, func(p store.Participant) bool { return strings.EqualFold(mentionToken(p.Name), name) }) ||
			slices.ContainsFunc(out, func(n mentionNotice) bool { return strings.EqualFold(n.Name, truncate(name, maxNoticeLen)) })
		if !known {
			out = append(out, mentionNotice{truncate(name, maxNoticeLen), whyUnknown})
		}
	}
	return out
}

// chainLinks queues what the finished turn done triggers, each as a link of the room's chain: the
// personas its synthesis names with @name (itself only with self-call on), then those whose "every
// N rounds" is due. Links past the budget are dropped, and the room is told which (002-AC22,
// 002-AC24, 002-AC25).
func (s *Server) chainLinks(ctx context.Context, done *store.Turn, synthesis string) {
	ctx = context.WithoutCancel(ctx)
	room := done.RoomID
	personas, err := s.bus.Store.RoomPersonas(ctx, room)
	if err != nil {
		s.log.Error("chain_failed", zap.String("room", room), zap.Error(err))
		return
	}
	type link struct {
		p     store.RoomPersona
		e     store.ChainEvent
		cause string
	}
	var links []link
	for _, p := range mentioned(synthesis, personas) {
		if p.ID != done.PersonaID || p.SelfCall {
			links = append(links, link{p, store.LinkCall, "called by @" + done.PersonaName})
		}
	}
	due, err := s.bus.Store.DuePersonas(ctx, room)
	if err != nil {
		s.log.Error("chain_failed", zap.String("room", room), zap.Error(err))
	}
	for _, p := range due {
		links = append(links, link{p, store.LinkEveryN, everyRounds(p.EveryN)})
	}
	var dropped []string
	var c store.Chain
	s.turnMu.Lock()
	for _, l := range links {
		var err error
		c, err = s.bus.Store.EnqueueLink(ctx, &store.QueuedTurn{RoomID: room, PersonaID: l.p.ID, Cause: l.cause}, l.e)
		switch {
		case errors.Is(err, store.ErrChainPaused):
			if !slices.Contains(dropped, l.p.Name) {
				dropped = append(dropped, l.p.Name)
			}
		case err == nil, errors.Is(err, store.ErrAlreadyQueued), errors.Is(err, store.ErrNotFound):
		default:
			s.log.Error("queue_link_failed", zap.String("room", room), zap.Int64("persona", l.p.ID), zap.Error(err))
		}
	}
	s.turnMu.Unlock()
	if len(dropped) == 0 {
		return
	}
	s.log.Info("chain_paused", zap.String("room", room), zap.Int("budget", c.Budget), zap.Strings("dropped", dropped))
	m := &store.Message{RoomID: room, Kind: store.KindSystem, Sender: done.PersonaName,
		Body: fmt.Sprintf("The agent chain reached its budget of %d turns: %s did not run. Re-arm the chain to let agents call each other again.",
			c.Budget, strings.Join(dropped, ", "))}
	if err := s.bus.Post(ctx, m); err != nil {
		s.log.Error("post_failed", zap.String("room", room), zap.Error(err))
	}
}
