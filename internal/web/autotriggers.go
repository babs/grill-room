package web

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
)

// timerTick is how often the timer triggers are checked (006-AC18). A var for tests.
var timerTick = 15 * time.Second

// queueAuto queues each persona due, as due reads them, as an automatic chain link, cause naming
// its trigger, then runs the queue. due runs under turnMu and the room's turn lock, as turns start:
// a persona whose turn just started is no longer due, so two triggers at once queue it once. A link
// past the auto-turn limit is dropped quietly: the status strip already says the auto turns are
// paused, and a line per voice line or tick would flood the chat.
func (s *Server) queueAuto(ctx context.Context, room string, e store.ChainEvent, due func() ([]store.RoomPersona, error), cause func(store.RoomPersona) string) {
	queued := 0
	unlock, err := s.lockTurns(ctx, room)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("auto_triggers_failed", zap.String("room", room), zap.Error(err))
		}
		return
	}
	ps, err := due()
	if err != nil {
		unlock()
		if ctx.Err() == nil {
			s.log.Error("auto_triggers_failed", zap.String("room", room), zap.Error(err))
		}
		return
	}
	for _, p := range ps {
		_, err := s.bus.Store.EnqueueLink(ctx, &store.QueuedTurn{RoomID: room, PersonaID: p.ID, Cause: cause(p)}, e)
		switch {
		case err == nil:
			queued++
		case errors.Is(err, store.ErrChainPaused), errors.Is(err, store.ErrAlreadyQueued), errors.Is(err, store.ErrNotFound):
		default:
			s.log.Error("queue_link_failed", zap.String("room", room), zap.Int64("persona", p.ID), zap.Error(err))
		}
	}
	unlock()
	if queued == 0 {
		return
	}
	s.log.Info("auto_turns_queued", zap.String("room", room), zap.Int("personas", queued))
	s.bus.Notify(room)
	s.drain(ctx, room)
}

// voiceTriggers queues the room's personas whose "every N voice lines" is due (006-AC18), counting
// closed paragraphs only (018-AC7).
func (s *Server) voiceTriggers(ctx context.Context, room string) {
	ctx = context.WithoutCancel(ctx)
	s.queueAuto(ctx, room, store.LinkEveryN, func() ([]store.RoomPersona, error) {
		return s.bus.Store.DueVoicePersonas(ctx, s.now(), s.voiceCount(), []string{room})
	}, voiceCause)
}

func (s *Server) voiceCount() store.VoiceCount {
	return store.VoiceCount{Gap: s.opts.VoiceParagraphGap, Close: s.opts.VoiceParagraphClose}
}

func voiceCause(p store.RoomPersona) string { return everyVoice(p.EveryVoice) }

// voiceTick queues, in rooms where someone is here, the personas whose "every N voice lines" a
// paragraph closed since the last voice line made due: by the close delay passing, or by a message
// other than a voice line (018-AC7). It runs only while paragraphs are on: off, every line is
// closed when stored. Each tick reads every room with someone here, so none is missed after an
// absence or a leader change.
func (s *Server) voiceTick(ctx context.Context) {
	if s.opts.VoiceParagraphGap <= 0 {
		return
	}
	var rooms []string
	for room := range s.people.present(s.lease()) {
		rooms = append(rooms, room)
	}
	if len(rooms) == 0 {
		return
	}
	now := s.now()
	due, err := s.bus.Store.DueVoicePersonas(ctx, now, s.voiceCount(), rooms)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("voice_triggers_failed", zap.Error(err))
		}
		return
	}
	queued := map[string]bool{}
	for _, p := range due {
		if !queued[p.RoomID] {
			queued[p.RoomID] = true
			room := p.RoomID
			s.queueAuto(ctx, room, store.LinkEveryN, func() ([]store.RoomPersona, error) {
				return s.bus.Store.DueVoicePersonas(ctx, now, s.voiceCount(), []string{room})
			}, voiceCause)
		}
	}
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
		if !rooms[p.RoomID] && s.people.anyoneHere(p.RoomID, s.lease()) {
			rooms[p.RoomID] = true
			room := p.RoomID
			s.queueAuto(ctx, room, store.LinkEveryN, func() ([]store.RoomPersona, error) { return s.bus.Store.DueTimerPersonas(ctx, now, room) },
				func(p store.RoomPersona) string { return everyMinutes(p.EveryMinutes) })
		}
	}
}

// DefaultNotesWatchDelay is how long the notes watcher waits after the last new note message
// (009-AC32).
const DefaultNotesWatchDelay = 20 * time.Second

// notesWatchTick is how often the notes watchers are checked. A var for tests.
var notesWatchTick = 2 * time.Second

// notesWatch queues each notes watcher due at s.now() as an automatic chain link (009-AC32).
func (s *Server) notesWatch(ctx context.Context) {
	ws, err := s.bus.Store.NotesWatchers(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("notes_watch_failed", zap.Error(err))
		}
		return
	}
	for _, w := range ws {
		var cause string
		s.queueAuto(ctx, w.RoomID, store.LinkNotesWatch, func() ([]store.RoomPersona, error) {
			p, c, err := s.watcherDue(ctx, w.RoomID, w.ID)
			if p == nil || err != nil {
				return nil, err
			}
			cause = c
			return []store.RoomPersona{*p}, nil
		}, func(store.RoomPersona) string { return cause })
	}
}

// watcherDue returns persona id of room when, as the notes watcher, it is due at s.now(), with its
// cause: note messages someone else wrote since its latest turn started, in threads that name no
// persona, the last one NotesWatchDelay ago, and no turn of its own running. Messages that cannot
// wake it are skipped for good. It runs under turnMu, as turns start.
func (s *Server) watcherDue(ctx context.Context, room string, id int64) (*store.RoomPersona, string, error) {
	st := s.bus.Store
	p, err := st.RoomPersona(ctx, room, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, "", nil
	}
	if err != nil || !p.WatchesNotes || p.Pending {
		return nil, "", err
	}
	running, err := st.RunningTurns(ctx, room)
	if err != nil || slices.ContainsFunc(running, func(t store.Turn) bool { return t.PersonaID == id }) {
		return nil, "", err
	}
	news, err := st.NotesNews(ctx, p)
	if err != nil || len(news) == 0 {
		return nil, "", err
	}
	var ids []int64
	for _, n := range news {
		if !slices.Contains(ids, n.AnnotationID) {
			ids = append(ids, n.AnnotationID)
		}
	}
	bodies, err := st.ParticipantNotes(ctx, room, ids)
	if err != nil {
		return nil, "", err
	}
	personas, err := st.RoomPersonas(ctx, room)
	if err != nil {
		return nil, "", err
	}
	var last time.Time
	var threads []int64
	for _, n := range news {
		if len(threadOwners(bodies[n.AnnotationID], personas)) > 0 {
			continue
		}
		if n.At.After(last) {
			last = n.At
		}
		if !slices.Contains(threads, n.AnnotationID) {
			threads = append(threads, n.AnnotationID)
		}
	}
	if len(threads) == 0 {
		return nil, "", st.SkipNotes(ctx, id, news[len(news)-1].MessageID)
	}
	if s.now().Sub(last) < s.opts.NotesWatchDelay {
		return nil, "", nil
	}
	return p, notesCause(threads), nil
}

// notesCause is "new notes: #3, #5", as many ids as fit the preamble's line, then ", …".
func notesCause(ids []int64) string {
	cause := "new notes: "
	for i, id := range ids {
		next := fmt.Sprintf("#%d", id)
		if i > 0 {
			next = ", " + next
		}
		// Room for ", …" unless this is the last id.
		if room := causeMax - len(cause) - len(next); room < 0 || (i < len(ids)-1 && room < len([]rune(", …"))) {
			return cause + ", …"
		}
		cause += next
	}
	return cause
}

// RunTimers checks the timer and voice triggers every timerTick, and the notes watchers every
// notesWatchTick, until ctx ends.
func (s *Server) RunTimers(ctx context.Context) {
	t := time.NewTicker(timerTick)
	defer t.Stop()
	w := time.NewTicker(notesWatchTick)
	defer w.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.timerTriggers(ctx)
			s.voiceTick(ctx)
		case <-w.C:
			s.notesWatch(ctx)
		}
	}
}
