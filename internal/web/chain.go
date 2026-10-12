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

// mentionQuote is how much of a mentioning message the called persona's preamble quotes, short
// enough for the line to fit presenceLineMax with a short name (003-AC22).
const mentionQuote = 50

// mentionTurns queues a turn of each persona a human's text message names with @name, as that
// human's press: only for someone allowed to press, while the host's daemon can run it. It resets
// the chain, since a human took the lead (002-AC23, 002-AC26).
func (s *Server) mentionTurns(ctx context.Context, p *store.Participant, body string) {
	if !strings.Contains(body, "@") {
		return
	}
	ctx = context.WithoutCancel(ctx)
	personas, err := s.bus.Store.RoomPersonas(ctx, p.RoomID)
	if err != nil {
		s.log.Error("mention_failed", zap.String("room", p.RoomID), zap.Error(err))
		return
	}
	called := mentioned(body, personas)
	if len(called) == 0 {
		return
	}
	room, err := s.bus.Store.Room(ctx, p.RoomID)
	if err != nil {
		s.log.Error("mention_failed", zap.String("room", p.RoomID), zap.Error(err))
		return
	}
	if room.HostParticipantID != p.ID && !p.CanTrigger {
		return
	}
	if h := s.hostDaemon(room.ID, room.HostParticipantID); h == nil || !h.CanHost {
		return
	}
	if err := s.bus.Store.ResetChain(ctx, room.ID, store.ResetMention); err != nil {
		s.log.Error("mention_failed", zap.String("room", room.ID), zap.Error(err))
		return
	}
	quote := oneLine(body)
	if r := []rune(quote); len(r) > mentionQuote {
		quote = string(r[:mentionQuote]) + "…"
	}
	cause := fmt.Sprintf("@mention by %s: %q", oneLine(p.Name), quote)
	s.turnMu.Lock()
	for _, persona := range called {
		q := &store.QueuedTurn{RoomID: room.ID, PersonaID: persona.ID, ParticipantID: p.ID, Source: store.SourceMention, Cause: cause}
		// A persona waiting already, or for the host's approval, is not queued again.
		if err := s.bus.Store.Enqueue(ctx, q); err != nil && !errors.Is(err, store.ErrAlreadyQueued) && !errors.Is(err, store.ErrNotFound) {
			s.log.Error("queue_turn_failed", zap.String("room", room.ID), zap.Int64("persona", persona.ID), zap.Error(err))
		}
	}
	s.turnMu.Unlock()
	s.log.Info("mention_queued", zap.String("room", room.ID), zap.Int64("participant", p.ID), zap.Int("personas", len(called)))
	s.bus.Notify(room.ID)
	s.drain(ctx, room.ID)
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
		links = append(links, link{p, store.LinkEveryN, fmt.Sprintf("every %d rounds", p.EveryN)})
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
