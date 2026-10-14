package web

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
)

// timerTick is how often the timer triggers are checked (006-AC18). A var for tests.
var timerTick = 15 * time.Second

// queueAuto queues each persona due, as due reads them, as an automatic chain link, cause naming
// its trigger, then runs the queue. due runs under turnMu, as turns start: a persona whose turn
// just started is no longer due, so two triggers at once queue it once. A link past the auto-turn
// limit is dropped quietly: the status strip already says the auto turns are paused, and a line
// per voice line or tick would flood the chat.
func (s *Server) queueAuto(ctx context.Context, room string, due func() ([]store.RoomPersona, error), cause func(store.RoomPersona) string) {
	queued := 0
	s.turnMu.Lock()
	ps, err := due()
	if err != nil {
		s.turnMu.Unlock()
		if ctx.Err() == nil {
			s.log.Error("auto_triggers_failed", zap.String("room", room), zap.Error(err))
		}
		return
	}
	for _, p := range ps {
		_, err := s.bus.Store.EnqueueLink(ctx, &store.QueuedTurn{RoomID: room, PersonaID: p.ID, Cause: cause(p)}, store.LinkEveryN)
		switch {
		case err == nil:
			queued++
		case errors.Is(err, store.ErrChainPaused), errors.Is(err, store.ErrAlreadyQueued), errors.Is(err, store.ErrNotFound):
		default:
			s.log.Error("queue_link_failed", zap.String("room", room), zap.Int64("persona", p.ID), zap.Error(err))
		}
	}
	s.turnMu.Unlock()
	if queued == 0 {
		return
	}
	s.log.Info("auto_turns_queued", zap.String("room", room), zap.Int("personas", queued))
	s.bus.Notify(room)
	s.drain(ctx, room)
}

// voiceTriggers queues the room's personas whose "every N voice lines" a new voice line made due
// (006-AC18).
func (s *Server) voiceTriggers(ctx context.Context, room string) {
	ctx = context.WithoutCancel(ctx)
	s.queueAuto(ctx, room, func() ([]store.RoomPersona, error) { return s.bus.Store.DueVoicePersonas(ctx, room) },
		func(p store.RoomPersona) string { return everyVoice(p.EveryVoice) })
}

// timerTriggers queues the personas whose "every T minutes" is due at s.now(), in rooms where
// someone is here; the others wait for a later tick (006-AC18).
func (s *Server) timerTriggers(ctx context.Context) {
	now := s.now()
	due, err := s.bus.Store.DueTimerPersonas(ctx, now, "")
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("timer_triggers_failed", zap.Error(err))
		}
		return
	}
	rooms := map[string]bool{}
	for _, p := range due {
		if !rooms[p.RoomID] && s.people.anyoneHere(p.RoomID, now) {
			rooms[p.RoomID] = true
			room := p.RoomID
			s.queueAuto(ctx, room, func() ([]store.RoomPersona, error) { return s.bus.Store.DueTimerPersonas(ctx, now, room) },
				func(p store.RoomPersona) string { return everyMinutes(p.EveryMinutes) })
		}
	}
}

// RunTimers checks the timer triggers every timerTick until ctx ends.
func (s *Server) RunTimers(ctx context.Context) {
	t := time.NewTicker(timerTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.timerTriggers(ctx)
		}
	}
}
