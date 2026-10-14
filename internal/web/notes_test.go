package web

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

type noteReply struct {
	store.AnnotationMessage
	Notices []mentionNotice `json:"notices"`
}

// note posts body in thread ann as tok and returns the message with its notices.
func (e *env) note(t *testing.T, r *turnRoom, tok string, ann int64, body string) noteReply {
	t.Helper()
	code, b := e.do(t, "POST", r.path(fmt.Sprintf("/annotations/%d/messages", ann)), tok, map[string]string{"body": body})
	if code != http.StatusCreated {
		t.Fatalf("note: %d %s", code, b)
	}
	return decodeJSON[noteReply](t, b)
}

// thread opens a whole-document thread on r's document as tok and returns its id.
func (e *env) thread(t *testing.T, r *turnRoom, tok, body string) int64 {
	t.Helper()
	code, b := e.do(t, "POST", annPath(r.room, r.doc), tok, map[string]any{"body": body})
	if code != http.StatusCreated {
		t.Fatalf("thread: %d %s", code, b)
	}
	return decodeJSON[store.Annotation](t, b).ID
}

// replies finishes tn with the replies.json entries given, and returns the room's last line.
func (e *env) replies(t *testing.T, r *turnRoom, tn store.Turn, entries string) string {
	t.Helper()
	res := turn.Result{Synthesis: "Answered.", SessionID: sid1, Session: []byte("x\n"),
		Changes: []turn.Change{{Path: "replies.json", Content: []byte(entries)}}}
	if data, err := e.st.AgentSession(t.Context(), r.room, tn.PersonaID, sid1); err == nil {
		res.SessionOffset = int64(len(data))
	}
	if code, b := e.result(t, r, r.alice, tn.ID, res); code != http.StatusOK {
		t.Fatalf("result: %d %s", code, b)
	}
	ms, _ := e.st.Messages(t.Context(), r.room, 0, 500)
	return ms[len(ms)-1].Body
}

// threadMessage returns message id of thread ann as the Notes pane reads it.
func (e *env) threadMessage(t *testing.T, r *turnRoom, tok string, id int64) store.AnnotationMessage {
	t.Helper()
	code, b := e.do(t, "GET", annPath(r.room, r.doc), tok, nil)
	if code != http.StatusOK {
		t.Fatalf("annotations: %d %s", code, b)
	}
	for _, a := range decodeJSON[[]store.Annotation](t, b) {
		for _, m := range a.Messages {
			if m.ID == id {
				return m
			}
		}
	}
	t.Fatalf("no note message %d", id)
	return store.AnnotationMessage{}
}

// 009-AC29: an @name in a note queues that persona as a chat mention does, with its cause and a
// chip on the note message that follows the turn and stays after a reload; its prompt puts that
// thread first. A participant without "Can ask the AI" is told why, as in the chat, and so is
// whoever names nobody; a new note naming a persona queues it too.
func TestNoteMentions(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	tok := r.alice.p.Token
	sec := e.secretary(t, r, tok, nil)
	plain := e.thread(t, r, tok, "Is the intro too long?")
	bob := e.join(t, e.createInvite(t, r.room).Token, "Bob")

	refused := e.note(t, r, bob.Token, r.ann, "@Secretary note this")
	if len(refused.Notices) != 1 || refused.Notices[0] != (mentionNotice{"Secretary", whyNoRight}) {
		t.Fatalf("notices for Bob: %+v", refused.Notices)
	}
	if st := e.agent(t, r.room, tok); len(st.Queue)+len(st.Running) != 0 || len(st.NoteMentions) != 0 {
		t.Fatalf("Bob's mention queued: %+v %+v", st.Queue, st.NoteMentions)
	}

	m := e.note(t, r, tok, r.ann, "@Secretary which fox is it, the brown one or the red one? Ask @Nobody")
	if len(m.Notices) != 1 || m.Notices[0] != (mentionNotice{"Nobody", whyUnknown}) {
		t.Fatalf("notices for Alice: %+v", m.Notices)
	}
	tn := e.nextTurn(t, r, r.alice)
	want := fmt.Sprintf(`@mention in note #%d by Alice: "@Secretary which fox is it, the brown one or t…"`, r.ann)
	if tn.PersonaID != sec.ID || tn.Cause != want {
		t.Fatalf("turn %+v, want cause %s", tn, want)
	}
	if st := e.agent(t, r.room, tok); len(st.NoteMentions) != 1 || st.NoteMentions[0].MessageID != m.ID || st.NoteMentions[0].Status != "running" {
		t.Fatalf("live chip: %+v", st.NoteMentions)
	}
	prompt := e.job(t, r, r.alice, tn.ID).Prompt
	first, other := strings.Index(prompt, fmt.Sprintf("## You were mentioned in note #%d\n\n### Annotation %d ", r.ann, r.ann)),
		strings.Index(prompt, fmt.Sprintf("## Annotation threads with new messages\n\n### Annotation %d ", plain))
	if first < 0 || other < first || strings.Count(prompt, fmt.Sprintf("### Annotation %d ", r.ann)) != 1 {
		t.Fatalf("mentioned thread not first:\n%s", prompt)
	}
	golden(t, "testdata/prompt-note-mention.golden", promptTS.ReplaceAllString(prompt[first:], "<ts>"))
	e.finish(t, r, r.alice, tn, "Noted the fox.")
	chip := e.threadMessage(t, r, tok, m.ID).Mentions
	ms, _ := e.st.Messages(t.Context(), r.room, 0, 500)
	if len(chip) != 1 || chip[0].PersonaID != sec.ID || chip[0].Status != "done" || chip[0].LineID != ms[len(ms)-1].ID {
		t.Fatalf("chip after the turn: %+v", chip)
	}

	code, b := e.do(t, "POST", annPath(r.room, r.doc), tok, map[string]any{"body": "@grill is this whole page needed?"})
	if code != http.StatusCreated {
		t.Fatalf("new note: %d %s", code, b)
	}
	opened := decodeJSON[store.Annotation](t, b)
	tn = e.nextTurn(t, r, r.alice)
	if tn.PersonaID != r.grill.ID || !strings.HasPrefix(tn.Cause, fmt.Sprintf("@mention in note #%d by Alice: ", opened.ID)) {
		t.Fatalf("new note's turn %+v", tn)
	}
	e.finish(t, r, r.alice, tn, "It is.")
}

// 009-AC30: a thread whose participants named a persona is that persona's: another persona's
// prompt leaves it out of the new threads, annotations.md keeps it, and that persona's reply or
// close there is refused and reported. An @name in code, or in a persona's own reply, claims
// nothing, and the named persona still answers.
func TestThreadOwnership(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	tok := r.alice.p.Token
	sec := e.secretary(t, r, tok, nil)
	bob := e.join(t, e.createInvite(t, r.room).Token, "Bob")
	// Bob may not call the AI, yet his @name still says whose thread it is.
	e.note(t, r, bob.Token, r.ann, "@Secretary this one is for the minutes")
	code := e.thread(t, r, tok, "Rename it `@Secretary` in the doc?")
	free := e.thread(t, r, tok, "Anyone?")

	tn := e.press(t, r, r.alice, tok, false)
	prompt := e.job(t, r, r.alice, tn.ID).Prompt
	has := func(id int64) bool { return strings.Contains(prompt, fmt.Sprintf("### Annotation %d ", id)) }
	if has(r.ann) || !has(code) || !has(free) {
		t.Fatalf("Grill's prompt: Secretary's thread %v, code thread %v, free thread %v\n%s", has(r.ann), has(code), has(free), prompt)
	}
	if ws := e.workspace(t, r, r.alice, tn.ID); !strings.Contains(ws["annotations.md"].data, "this one is for the minutes") {
		t.Fatalf("annotations.md lost the thread:\n%s", ws["annotations.md"].data)
	}
	line := e.replies(t, r, tn, fmt.Sprintf(`[{"annotation": %d, "body": "Done.", "status": "done"}, {"annotation": %d, "body": "Me, @Secretary can confirm."}]`, r.ann, free))
	if want := fmt.Sprintf("ignored: reply to annotation %d: it belongs to @Secretary", r.ann); !strings.Contains(line, want) {
		t.Fatalf("report %q, want %q", line, want)
	}
	anns, _ := e.st.Annotations(t.Context(), r.room, r.doc)
	for _, a := range anns {
		last := a.Messages[len(a.Messages)-1]
		switch a.ID {
		case r.ann:
			if last.By == store.ByAgent || a.Status == store.StatusDone {
				t.Fatalf("Grill wrote in Secretary's thread: %+v", a)
			}
		case free:
			if last.PersonaID != r.grill.ID {
				t.Fatalf("Grill's reply was not kept: %+v", a)
			}
		}
	}

	// Grill named Secretary in its own reply: the thread stays nobody's, for Grill as for Secretary.
	e.note(t, r, tok, free, "Thanks.")
	tn = e.press(t, r, r.alice, tok, false)
	if prompt := e.job(t, r, r.alice, tn.ID).Prompt; !strings.Contains(prompt, fmt.Sprintf("### Annotation %d ", free)) {
		t.Fatalf("a persona's own @name claimed the thread:\n%s", prompt)
	}
	e.finish(t, r, r.alice, tn, "Welcome.")
	tn = e.pressPersona(t, r, tok, sec.ID)
	prompt = e.job(t, r, r.alice, tn.ID).Prompt
	if !strings.Contains(prompt, fmt.Sprintf("### Annotation %d ", r.ann)) || !strings.Contains(prompt, fmt.Sprintf("### Annotation %d ", free)) {
		t.Fatalf("Secretary's prompt:\n%s", prompt)
	}
	if line := e.replies(t, r, tn, fmt.Sprintf(`[{"annotation": %d, "body": "Minuted.", "status": "done"}]`, r.ann)); strings.Contains(line, "ignored") {
		t.Fatalf("Secretary's own thread refused: %q", line)
	}
}

// pressPersona starts a turn of persona id as tok and returns it once the host daemon has it.
func (e *env) pressPersona(t *testing.T, r *turnRoom, tok string, id int64) store.Turn {
	t.Helper()
	if code, b := e.do(t, "POST", r.path("/turns"), tok, map[string]any{"persona_id": id}); code != http.StatusCreated {
		t.Fatalf("press: %d %s", code, b)
	}
	return e.nextTurn(t, r, r.alice)
}

// 009-AC31 009-AC32: the notes watcher answers threads that name no persona, once the delay passes
// with no new message: each message restarts the wait. Its own replies and threads naming a
// persona start nothing; another persona's new note does. Its turns are chain links: the limit
// stops them, and a watcher queued or running is not queued twice. Driven by a fake clock.
func TestNotesWatcher(t *testing.T) {
	e := newEnv(t)
	c := e.clock()
	c.set(time.Now())
	r := newTurnRoom(t, e)
	tok := r.alice.p.Token
	bob := e.join(t, e.createInvite(t, r.room).Token, "Bob")
	sec := e.secretary(t, r, tok, map[string]any{"watches_notes": true})
	if !sec.WatchesNotes {
		t.Fatalf("not watching: %+v", sec)
	}
	idle := func() bool {
		st := e.agent(t, r.room, tok)
		return len(st.Queue)+len(st.Running) == 0
	}
	tick := func(d time.Duration) {
		c.add(d)
		e.srv.notesWatch(t.Context())
	}

	e.note(t, r, tok, r.ann, "first")
	tick(10 * time.Second)
	e.note(t, r, tok, r.ann, "second, 10 s later")
	if tick(19 * time.Second); !idle() {
		t.Fatal("ran before 20 s passed since the second message")
	}
	tick(time.Second)
	tn := e.nextTurn(t, r, r.alice)
	if tn.PersonaID != sec.ID || tn.Cause != fmt.Sprintf("new notes: #%d", r.ann) {
		t.Fatalf("watcher turn %+v", tn)
	}
	if st := e.agent(t, r.room, tok); st.Chain.Count != 1 {
		t.Fatalf("not a chain link: %+v", st.Chain)
	}
	// Running: a new message waits for its next turn.
	e.note(t, r, tok, r.ann, "third, during the turn")
	if tick(time.Minute); len(e.agent(t, r.room, tok).Queue) != 0 {
		t.Fatal("queued while running")
	}
	e.replies(t, r, tn, fmt.Sprintf(`[{"annotation": %d, "body": "Two foxes."}]`, r.ann))
	tick(time.Second)
	tn = e.nextTurn(t, r, r.alice)
	if !strings.Contains(e.job(t, r, r.alice, tn.ID).Prompt, "third, during the turn") {
		t.Fatal("the next turn misses the message")
	}
	// Its own reply starts nothing.
	e.replies(t, r, tn, fmt.Sprintf(`[{"annotation": %d, "body": "And a third."}]`, r.ann))
	if tick(time.Minute); !idle() {
		t.Fatal("its own reply woke it")
	}
	// A thread naming a persona is that persona's, even when the mention could not run.
	named := e.thread(t, r, bob.Token, "@Grill why this order?")
	e.note(t, r, tok, named, "good question")
	if tick(time.Minute); !idle() {
		t.Fatal("a thread naming a persona woke it")
	}
	// Another persona's new note wakes it, even naming a persona: only participants claim a thread.
	press := e.press(t, r, r.alice, tok, false)
	res := turn.Result{Synthesis: "Flagged.", SessionID: sid2, Session: []byte("y\n"), Changes: []turn.Change{{Path: "annotations-new.json",
		Content: []byte(`[{"doc": "docs/synthesis.md", "exact": "quick", "body": "Too quick, @Secretary?"}]`)}}}
	if code, b := e.result(t, r, r.alice, press.ID, res); code != http.StatusOK {
		t.Fatalf("grill result: %d %s", code, b)
	}
	tick(30 * time.Second)
	tn = e.nextTurn(t, r, r.alice)
	if tn.PersonaID != sec.ID || !strings.HasPrefix(tn.Cause, "new notes: #") || strings.Contains(tn.Cause, "#"+strconv.FormatInt(named, 10)) {
		t.Fatalf("turn for Grill's note %+v", tn)
	}
	e.replies(t, r, tn, "[]")

	// The auto-turn limit stops it; lifting it lets the waiting notes through.
	if code, ch := e.chain(t, r, tok, map[string]int{"budget": 1}); code != http.StatusOK || !ch.Paused() {
		t.Fatalf("budget: %+v", ch)
	}
	e.note(t, r, tok, r.ann, "past the limit")
	if tick(time.Minute); !idle() {
		t.Fatal("ran past the limit")
	}
	if code, _ := e.chain(t, r, tok, map[string]bool{"rearm": true}); code != http.StatusOK {
		t.Fatal("rearm")
	}
	tick(time.Second)
	tn = e.nextTurn(t, r, r.alice)
	if tn.PersonaID != sec.ID {
		t.Fatalf("after the re-arm %+v", tn)
	}
	// Moving the role leaves one watcher, and the preamble says who watches.
	if code, b := e.do(t, "PATCH", r.path(fmt.Sprintf("/personas/%d", r.grill.ID)), tok, map[string]bool{"watches_notes": true}); code != http.StatusOK {
		t.Fatalf("move: %d %s", code, b)
	}
	ps, _ := e.st.RoomPersonas(t.Context(), r.room)
	if n := slices.IndexFunc(ps, func(p store.RoomPersona) bool { return p.WatchesNotes }); n < 0 || ps[n].ID != r.grill.ID ||
		slices.ContainsFunc(ps[n+1:], func(p store.RoomPersona) bool { return p.WatchesNotes }) {
		t.Fatalf("watchers: %+v", ps)
	}
	if pre, err := e.srv.presencePreamble(t.Context(), e.roomOf(t, r.room), &ps[0], ""); err != nil || !strings.Contains(pre, "You: @Grill, model sonnet, effort medium, on demand, watches the notes\n") {
		t.Fatalf("preamble: %v\n%s", err, pre)
	}
}

// 009-AC31: a starter or a new room carries one notes watcher at most.
func TestOneWatcherPerStarter(t *testing.T) {
	w := store.StarterPersona{WatchesNotes: true}
	if err := oneWatcher([]store.StarterPersona{w, {}}); err != nil {
		t.Fatalf("one watcher: %v", err)
	}
	if err := oneWatcher([]store.StarterPersona{w, {}, w}); err == nil || !strings.Contains(err.Error(), "only one persona may watch the notes") {
		t.Fatalf("two watchers: %v", err)
	}
}

// 009-AC29: the Why line of a note mention fits the preamble line whole: a 5-letter name, a 3-digit
// note id and a long message still end in the quote's closing mark, the quote cut with "…".
func TestNoteMentionWhyFits(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	tok := r.alice.p.Token
	sec := e.secretary(t, r, tok, nil)
	var ann int64
	for ann < 100 {
		a, err := e.st.CreateAnnotation(t.Context(), r.room, r.doc, 0, nil, &store.NewMessage{ParticipantID: r.alice.p.Participant.ID, Author: "Alice", Body: "filler"}, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		ann = a.ID
	}
	e.note(t, r, tok, ann, "@Secretary "+strings.Repeat("what does the rollback cost us ", 4))
	tn := e.nextTurn(t, r, r.alice)
	why := e.why(t, r, tn.ID)
	if tn.PersonaID != sec.ID || !strings.HasSuffix(why, `…"`) || len([]rune(why)) > presenceLineMax ||
		!strings.HasPrefix(why, fmt.Sprintf(`Why you are running: @mention in note #%d by Alice: "@Secretary what`, ann)) {
		t.Fatalf("why line (%d runes): %s", len([]rune(why)), why)
	}
}

// 009-AC32: the watcher's cause lists the threads that fit the line, then ", …".
func TestNotesCause(t *testing.T) {
	if got := notesCause([]int64{3, 5}); got != "new notes: #3, #5" {
		t.Fatalf("short: %q", got)
	}
	var ids []int64
	for i := range 40 {
		ids = append(ids, int64(1000+i))
	}
	got := notesCause(ids)
	if !strings.HasSuffix(got, "#1008, …") || len([]rune(got)) > causeMax {
		t.Fatalf("long (%d runes): %q", len([]rune(got)), got)
	}
	exact := []int64{1, 2}
	for len([]rune(notesCause(exact))) < causeMax-6 {
		exact = append(exact, int64(len(exact)+1))
	}
	if got := notesCause(exact); strings.Contains(got, "…") || len([]rune(got)) > causeMax {
		t.Fatalf("all fit, none cut: %q", got)
	}
}

// 009-AC32: a watcher a mention queued already is not queued again when its delay passes.
func TestNotesWatcherQueuedOnce(t *testing.T) {
	e := newEnv(t)
	c := e.clock()
	c.set(time.Now())
	r := newTurnRoom(t, e)
	tok := r.alice.p.Token
	sec := e.secretary(t, r, tok, map[string]any{"watches_notes": true})
	if st := e.agent(t, r.room, tok); st.NotesWatchDelay != 20 {
		t.Fatalf("delay in the agent state: %d", st.NotesWatchDelay)
	}
	busy := e.press(t, r, r.alice, tok, false)
	e.postBody(t, r, tok, "@Secretary note this")
	e.note(t, r, tok, r.ann, "a note while it waits")
	c.add(time.Minute)
	e.srv.notesWatch(t.Context())
	if q := e.agent(t, r.room, tok).Queue; len(q) != 1 || q[0].PersonaID != sec.ID || q[0].Source != store.SourceMention {
		t.Fatalf("queue %+v", q)
	}
	e.finish(t, r, r.alice, busy, "Done.")
	if tn := e.nextTurn(t, r, r.alice); tn.PersonaID != sec.ID || !strings.Contains(e.job(t, r, r.alice, tn.ID).Prompt, "a note while it waits") {
		t.Fatalf("the queued turn misses the note: %+v", tn)
	}
}

// 003-AC22 009-AC29: a cause quotes the message raw: a `"` or a backslash stays as written. A
// control character, which would end or garble the preamble line, becomes a space; the message API
// refuses one already, so oneLine is checked on its own.
func TestMentionCauseQuotesRaw(t *testing.T) {
	if got := oneLine("a\x07b\x1b[31mc\u0085d\r\ne\tf"); got != "a b [31mc d e f" {
		t.Fatalf("oneLine: %q", got)
	}
	e := newEnv(t)
	r := newTurnRoom(t, e)
	tok := r.alice.p.Token
	e.postBody(t, r, tok, "@Grill is \"C:\\tmp\" the path?")
	tn := e.nextTurn(t, r, r.alice)
	if want := `@mention by Alice: "@Grill is "C:\tmp" the path?"`; tn.Cause != want {
		t.Fatalf("cause %q, want %q", tn.Cause, want)
	}
}
