package web

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/store"
)

// voice posts a voice line by participant pid, as the STT worker's endpoint does, then runs the
// voice triggers it calls.
func (e *env) voice(t *testing.T, r *turnRoom, pid int64, body string) {
	t.Helper()
	if err := e.bus.Post(t.Context(), &store.Message{RoomID: r.room, Kind: store.KindVoice, Source: store.VoiceSourceServer, ParticipantID: pid, Sender: "Alice", Body: body}); err != nil {
		t.Fatal(err)
	}
	e.srv.voiceTriggers(t.Context(), r.room)
}

func (e *env) persona(t *testing.T, r *turnRoom, id int64) store.RoomPersona {
	t.Helper()
	p, err := e.st.RoomPersona(t.Context(), r.room, id)
	if err != nil {
		t.Fatal(err)
	}
	return *p
}

// 006-AC18: "every N voice lines" runs a persona once N voice lines came since its latest turn
// started, as a chain link with its cause; the count shows as the lines left, and starts again at
// the turn. At the auto-turn limit the link is dropped, and nothing more is said in the chat.
func TestVoiceTrigger(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	alice := r.alice
	tok := alice.p.Token
	sec := e.secretary(t, r, tok, map[string]any{"every_voice": 3})
	if sec.EveryVoice != 3 || sec.EveryN != 0 || sec.NextVoice != 3 {
		t.Fatalf("secretary: %+v", sec)
	}
	pid := alice.p.Participant.ID
	e.voice(t, r, pid, "one")
	e.voice(t, r, pid, "two")
	if p := e.persona(t, r, sec.ID); p.NextVoice != 1 {
		t.Fatalf("next voice %d", p.NextVoice)
	}
	if q := e.agent(t, r.room, tok).Queue; len(q) != 0 {
		t.Fatalf("queued early: %+v", q)
	}
	e.voice(t, r, pid, "three")
	tn := e.nextTurn(t, r, alice)
	if tn.PersonaID != sec.ID || tn.Cause != "every 3 voice lines" {
		t.Fatalf("turn: %+v", tn)
	}
	if c := e.agent(t, r.room, tok).Chain; c.Count != 1 {
		t.Fatalf("chain %+v", c)
	}
	if p := e.persona(t, r, sec.ID); p.NextVoice != 3 {
		t.Fatalf("the count did not start again: %d", p.NextVoice)
	}
	e.finish(t, r, alice, tn, "Noted.")
	if code, c := e.chain(t, r, tok, map[string]int{"budget": 1}); code != http.StatusOK || !c.Paused() {
		t.Fatalf("budget: %+v", c)
	}
	lines := func() int {
		ms, _ := e.st.Messages(t.Context(), r.room, 0, 500)
		return len(ms)
	}
	before := lines()
	for i := range 4 {
		e.voice(t, r, pid, fmt.Sprint("more ", i))
	}
	if st := e.agent(t, r.room, tok); len(st.Queue) != 0 || len(st.Running) != 0 {
		t.Fatalf("ran past the limit: %+v %+v", st.Queue, st.Running)
	}
	if got := lines(); got != before+4 {
		t.Fatalf("%d lines, want only the 4 voice lines", got-before)
	}
}

// 006-AC18: "every T minutes" fires once T minutes passed since the persona's latest turn, only
// with someone in the room and a voice or text line since that turn; otherwise it waits for a
// later tick. Its turns are chain links, stopped by the auto-turn limit. Driven by a fake clock.
func TestTimerTrigger(t *testing.T) {
	e := newEnv(t)
	c := e.clock()
	c.set(time.Now())
	r := newTurnRoom(t, e)
	alice := r.alice
	tok := alice.p.Token
	sec := e.secretary(t, r, tok, map[string]any{"every_minutes": 5})
	// The mark is the store's time when the persona was added.
	if sec.NextAt == nil || time.Until(*sec.NextAt) > 5*time.Minute || time.Until(*sec.NextAt) < 4*time.Minute {
		t.Fatalf("next at %v", sec.NextAt)
	}
	queued := func() bool {
		st := e.agent(t, r.room, tok)
		return len(st.Queue)+len(st.Running) > 0
	}
	c.add(6 * time.Minute)
	e.srv.timerTriggers(t.Context())
	if queued() {
		t.Fatal("fired in an idle room")
	}
	e.postBody(t, r, tok, "a new line")
	e.srv.timerTriggers(t.Context())
	if queued() {
		t.Fatal("fired with nobody here")
	}
	e.openEvents(t, r.room, tok, "")
	e.waitPresent(t, alice.p.Participant.ID)
	c.set(time.Now().Add(-time.Minute))
	e.srv.timerTriggers(t.Context())
	if queued() {
		t.Fatal("fired before its time")
	}
	c.set(time.Now().Add(6 * time.Minute))
	e.srv.timerTriggers(t.Context())
	tn := e.nextTurn(t, r, alice)
	if tn.PersonaID != sec.ID || tn.Cause != "every 5 minutes" {
		t.Fatalf("turn %+v", tn)
	}
	e.finish(t, r, alice, tn, "Noted.")
	// Its next tick runs from this turn: new activity, but too early.
	e.postBody(t, r, tok, "later")
	c.set(time.Now().Add(4 * time.Minute))
	e.srv.timerTriggers(t.Context())
	if queued() {
		t.Fatal("fired again before its time")
	}
	if code, ch := e.chain(t, r, tok, map[string]int{"budget": 1}); code != http.StatusOK || !ch.Paused() {
		t.Fatalf("budget: %+v", ch)
	}
	c.set(time.Now().Add(6 * time.Minute))
	e.srv.timerTriggers(t.Context())
	if queued() {
		t.Fatal("ran past the limit")
	}
}

// 006-AC18: the room API keeps one automatic trigger per persona, and says so in the chat.
func TestOneTriggerPerPersona(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	tok := r.alice.p.Token
	sec := e.secretary(t, r, tok, map[string]any{"every_n": 2})
	code, b := e.do(t, "PATCH", r.path(fmt.Sprintf("/personas/%d", sec.ID)), tok, map[string]any{"every_minutes": 10})
	got := decodeJSON[store.RoomPersona](t, b)
	if code != http.StatusOK || got.EveryN != 0 || got.EveryMinutes != 10 || got.EveryVoice != 0 {
		t.Fatalf("switch: %d %s", code, b)
	}
	ms, _ := e.st.Messages(t.Context(), r.room, 0, 100)
	if last := ms[len(ms)-1].Body; !strings.Contains(last, "every N rounds 2 → off, every N minutes off → 10") {
		t.Fatalf("line %q", last)
	}
	if code, _ := e.do(t, "PATCH", r.path(fmt.Sprintf("/personas/%d", sec.ID)), tok, map[string]any{"every_minutes": 10, "every_voice": 3}); code != http.StatusBadRequest {
		t.Fatalf("two at once: %d", code)
	}
	ps, _ := e.st.RoomPersonas(t.Context(), r.room)
	got = ps[1]
	room := e.roomOf(t, r.room)
	pre, err := e.srv.presencePreamble(t.Context(), room, &got, "")
	if err != nil || !strings.Contains(pre, "You: @Secretary, model sonnet, effort medium, on demand and every 10 minutes\n") {
		t.Fatalf("preamble: %v\n%s", err, pre)
	}
}

// 006-AC14: the preamble names agenda.md as the room's agenda once the room has one, golden; the
// room without it gets no such line (testdata/presence.golden).
func TestPresenceAgenda(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	room := e.roomOf(t, r.room)
	without, err := e.srv.presencePreamble(t.Context(), room, &r.grill, "pressed by Alice")
	if err != nil || strings.Contains(without, "Agenda") {
		t.Fatalf("without an agenda: %v\n%s", err, without)
	}
	if _, err := e.st.CreateAgentDocument(t.Context(), r.room, store.AgendaName, "md", "- [ ] one\n", "Alice"); err != nil {
		t.Fatal(err)
	}
	if docs, _ := e.st.Documents(t.Context(), r.room); docs[0].Name != store.AgendaName {
		t.Fatalf("not pinned first: %+v", docs)
	}
	with, err := e.srv.presencePreamble(t.Context(), room, &r.grill, "pressed by Alice")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "testdata/presence-agenda.golden", regexp.MustCompile(`·[0-9a-f]{4}\b|·#\d+`).ReplaceAllString(with, "·<id>"))
}

// 006-AC18: the server checks the timers on its own, every timerTick, until stopped.
func TestRunTimers(t *testing.T) {
	old := timerTick
	timerTick = time.Millisecond
	t.Cleanup(func() { timerTick = old })
	e := newEnv(t)
	c := e.clock()
	r := newTurnRoom(t, e)
	tok := r.alice.p.Token
	sec := e.secretary(t, r, tok, map[string]any{"every_minutes": 1})
	e.openEvents(t, r.room, tok, "")
	e.waitPresent(t, r.alice.p.Participant.ID)
	e.postBody(t, r, tok, "news")
	c.set(time.Now().Add(2 * time.Minute))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { e.srv.RunTimers(ctx); close(done) }()
	if tn := e.nextTurn(t, r, r.alice); tn.PersonaID != sec.ID {
		t.Fatalf("turn %+v", tn)
	}
	cancel()
	<-done
}
