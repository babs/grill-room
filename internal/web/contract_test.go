package web

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

// why returns the "Why you are running" line of turn id's prompt.
func (e *env) why(t *testing.T, r *turnRoom, id int64) string {
	t.Helper()
	p := e.job(t, r, r.alice, id).Prompt
	for _, l := range strings.Split(p, "\n") {
		if strings.HasPrefix(l, "Why you are running: ") {
			return l
		}
	}
	t.Fatalf("no why line in:\n%s", p)
	return ""
}

// 003-AC22 003-AC25 009-AC29: every way a turn starts names its cause in the preamble, golden: a
// press, a persona's call, a human @mention quoting the message's start, in the chat or a note,
// every N rounds, a re-run after a conflict, and both turns of a handoff. The handoff's new
// session starts fresh and is told to read the note its last turn wrote.
func TestTurnCauses(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	tok := r.alice.p.Token
	sec := e.secretary(t, r, tok, nil)
	var got []string
	record := func(source string, tn store.Turn) {
		got = append(got, source+": "+e.why(t, r, tn.ID))
		if tn.Cause == "" {
			t.Errorf("%s: turn %d has no cause", source, tn.ID)
		}
	}

	press := e.press(t, r, r.alice, tok, false)
	record("button", press)
	e.finish(t, r, r.alice, press, "Over to @Secretary.")
	call := e.nextTurn(t, r, r.alice)
	record("call", call)
	e.finish(t, r, r.alice, call, "Minutes.")
	// 009-AC29: a note naming a persona, quoted the same way.
	e.note(t, r, tok, r.ann, "@Secretary is the brown fox in the minutes, or did it get lost on the way?")
	noted := e.nextTurn(t, r, r.alice)
	record("note mention", noted)
	e.finish(t, r, r.alice, noted, "Minuted.")

	e.postBody(t, r, tok, "@Grill is the rollback ready for Friday, and who signs the release notes before we ship?")
	mention := e.nextTurn(t, r, r.alice)
	record("mention", mention)
	setSec := func(n int) {
		if code, b := e.do(t, "PATCH", r.path(fmt.Sprintf("/personas/%d", sec.ID)), tok, map[string]int{"every_n": n}); code != http.StatusOK {
			t.Fatalf("every_n: %d %s", code, b)
		}
	}
	setSec(1)
	e.finish(t, r, r.alice, mention, "Checked.")
	every := e.nextTurn(t, r, r.alice)
	record("every_n", every)
	setSec(0)
	e.finish(t, r, r.alice, every, "Minutes again.")

	conflicted := e.press(t, r, r.alice, tok, false)
	e.workspace(t, r, r.alice, conflicted.ID)
	if _, err := e.st.AddRevision(t.Context(), r.room, r.doc, "# Synthesis\n\nA lazy dog.\n", "Secretary"); err != nil {
		t.Fatal(err)
	}
	data, _ := e.st.AgentSession(t.Context(), r.room, r.grill.ID, sid1)
	if code, b := e.result(t, r, r.alice, conflicted.ID, turn.Result{SessionID: sid1, SessionOffset: int64(len(data)), Session: []byte("x\n"), Synthesis: "edited",
		Changes: []turn.Change{{Path: "docs/synthesis.md", Content: []byte("# Synthesis\n\nThe quick red fox.\n")}}}); code != http.StatusConflict || !strings.Contains(string(b), "conflict on") {
		t.Fatalf("conflict: %d %s", code, b)
	}
	rerun := e.nextTurn(t, r, r.alice)
	record("rerun", rerun)
	e.finish(t, r, r.alice, rerun, "Again.")

	code, b := e.do(t, "POST", r.path("/turns"), tok, map[string]any{"persona_id": r.grill.ID, "handoff": true})
	if code != http.StatusCreated {
		t.Fatalf("handoff: %d %s", code, b)
	}
	write := e.nextTurn(t, r, r.alice)
	record("handoff write", write)
	if p := e.job(t, r, r.alice, write.ID); p.Resume != sid1 || !strings.Contains(p.Prompt, "## Handoff\n") || !strings.Contains(p.Prompt, "Write docs/_handoff-Grill.md for it") {
		t.Fatalf("handoff turn job: resume %q\n%s", p.Resume, p.Prompt)
	}
	data, _ = e.st.AgentSession(t.Context(), r.room, r.grill.ID, sid1)
	if code, b := e.result(t, r, r.alice, write.ID, turn.Result{SessionID: sid1, SessionOffset: int64(len(data)), Session: []byte("x\n"), Synthesis: "Note written.",
		Changes: []turn.Change{{Path: "docs/_handoff-Grill.md", Content: []byte("# Handoff\n\nShip on Friday.\n")}}}); code != http.StatusOK {
		t.Fatalf("handoff result: %d %s", code, b)
	}
	read := e.nextTurn(t, r, r.alice)
	record("handoff read", read)
	job := e.job(t, r, r.alice, read.ID)
	if job.Resume != "" || !read.Fresh || read.Handoff != store.HandoffRead ||
		!strings.Contains(job.Prompt, "This is a new session: you remember no earlier turn. Your previous session wrote docs/_handoff-Grill.md for you: read it first") {
		t.Fatalf("new session job: resume %q, turn %+v\n%s", job.Resume, read, job.Prompt)
	}
	if ws := e.workspace(t, r, r.alice, read.ID); ws["docs/_handoff-Grill.md"].data != "# Handoff\n\nShip on Friday.\n" {
		t.Fatalf("the new session's workspace lacks the note: %+v", ws["docs/_handoff-Grill.md"])
	}
	if code, b := e.result(t, r, r.alice, read.ID, turn.Result{SessionID: sid2, Session: []byte("y\n"), Synthesis: "Read the note."}); code != http.StatusOK {
		t.Fatalf("new session result: %d %s", code, b)
	}
	if next := e.press(t, r, r.alice, tok, false); e.job(t, r, r.alice, next.ID).Resume != sid2 {
		t.Fatal("the turn after the handoff does not resume the new session")
	}
	golden(t, "testdata/causes.golden", strings.Join(got, "\n")+"\n")
}

// 003-AC25: a handoff turn that writes no note, or fails, leaves the persona on its session and
// says so; only the host asks for one, and only for a persona with a session to hand over.
func TestHandoffFailures(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	tok := r.alice.p.Token
	handoff := func(tok string) (int, []byte) {
		return e.do(t, "POST", r.path("/turns"), tok, map[string]any{"persona_id": r.grill.ID, "handoff": true})
	}
	if code, b := handoff(tok); code != http.StatusConflict || !strings.Contains(string(b), "no session to hand over") {
		t.Fatalf("no session yet: %d %s", code, b)
	}
	e.finish(t, r, r.alice, e.press(t, r, r.alice, tok, false), "First.")
	bob := e.daemonFor(t, r.room, "Bob")
	if err := e.st.SetCanTrigger(t.Context(), r.room, bob.p.Participant.ID, true); err != nil {
		t.Fatal(err)
	}
	if code, _ := handoff(bob.p.Token); code != http.StatusForbidden {
		t.Fatalf("a guest asked for a handoff: %d", code)
	}
	if code, _ := e.do(t, "POST", r.path("/turns"), tok, map[string]any{"handoff": true, "fresh": true}); code != http.StatusBadRequest {
		t.Fatalf("fresh and handoff: %d", code)
	}
	lastSystem := func() string {
		ms, err := e.st.Messages(t.Context(), r.room, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		for i := len(ms) - 1; i >= 0; i-- {
			if ms[i].Kind == store.KindSystem {
				return ms[i].Body
			}
		}
		return ""
	}
	if code, b := handoff(tok); code != http.StatusCreated {
		t.Fatalf("handoff: %d %s", code, b)
	}
	noNote := e.nextTurn(t, r, r.alice)
	e.finish(t, r, r.alice, noNote, "I forgot.")
	if got := lastSystem(); got != "No handoff: Grill did not write docs/_handoff-Grill.md, so it stays on its current session." {
		t.Fatalf("no note: %q", got)
	}
	if code, b := handoff(tok); code != http.StatusCreated {
		t.Fatalf("handoff: %d %s", code, b)
	}
	failed := e.nextTurn(t, r, r.alice)
	if code, _ := e.result(t, r, r.alice, failed.ID, turn.Result{Error: "claude crashed"}); code != http.StatusNoContent {
		t.Fatalf("error result: %d", code)
	}
	if got := lastSystem(); got != "Grill turn failed: claude crashed. No handoff: Grill stays on its current session." {
		t.Fatalf("failed: %q", got)
	}
	st := e.agent(t, r.room, tok)
	if len(st.Queue) != 0 || len(st.Running) != 0 {
		t.Fatalf("a new session was queued: %+v", st)
	}
	if next := e.press(t, r, r.alice, tok, false); e.job(t, r, r.alice, next.ID).Resume != sid1 {
		t.Fatal("the persona left its session after a failed handoff")
	}
}

// 003-AC32: each human's short id, golden. Two profiles alike on 4 hex digits make the room show 6;
// a guest without a profile shows "#" and its participant id; one person in a room twice keeps one id.
func TestHumanTags(t *testing.T) {
	ps := []store.Participant{
		{ID: 3, Name: "Alice", ProfileUUID: "a1b2c3d4-0000-4000-8000-000000000001"},
		{ID: 4, Name: "Alice", ProfileUUID: "a1b2ffee-0000-4000-8000-000000000002"},
		{ID: 7, Name: "Bob"},
		{ID: 9, Name: "Carol", ProfileUUID: "0f0f0f0f-0000-4000-8000-000000000003"},
		{ID: 12, Name: "Carol again", ProfileUUID: "0f0f0f0f-0000-4000-8000-000000000003"},
	}
	var b strings.Builder
	for _, set := range [][]store.Participant{ps, {ps[0], ps[2], ps[3]}, {ps[0], {ID: 5, Name: "Dan", ProfileUUID: "a1b2c3ff-0000-4000-8000-000000000004"}}} {
		tags := humanTags(set)
		var names []string
		for _, p := range set {
			names = append(names, tagged(p.Name, tags[p.ID]))
		}
		v := presenceView{Room: "R", Self: presencePersona{Name: "Grill", Level: "model default, effort default", Trigger: "on demand"},
			Why: "pressed by " + names[0], Chain: "unlimited", Humans: names, Host: names[0]}
		out, err := renderPresence(v)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "Humans present: ") || strings.HasPrefix(l, "Why ") {
				b.WriteString(l + "\n")
			}
		}
	}
	golden(t, "testdata/humans.golden", b.String())
}

// 003-AC24: a persona's autocompact is set on the persona, checked against claude's range, said in
// the chat, and handed to the daemon with the turn; 0 is auto.
func TestAutocompact(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	tok := r.alice.p.Token
	path := r.path(fmt.Sprintf("/personas/%d", r.grill.ID))
	for _, bad := range []int{50_000, 99_999, 1_000_001, -1} {
		if code, _ := e.do(t, "PATCH", path, tok, map[string]int{"autocompact": bad}); code != http.StatusBadRequest {
			t.Fatalf("autocompact %d: %d", bad, code)
		}
	}
	code, b := e.do(t, "PATCH", path, tok, map[string]int{"autocompact": 200_000})
	if code != http.StatusOK || decodeJSON[store.RoomPersona](t, b).Autocompact != 200_000 {
		t.Fatalf("set: %d %s", code, b)
	}
	ms, _ := e.st.Messages(t.Context(), r.room, 0, 100)
	if last := ms[len(ms)-1].Body; !strings.HasSuffix(last, "autocompact auto → 200k.") {
		t.Fatalf("chat line %q", last)
	}
	tn := e.press(t, r, r.alice, tok, false)
	if got := e.job(t, r, r.alice, tn.ID).Autocompact; got != 200_000 {
		t.Fatalf("job autocompact %d", got)
	}
	e.finish(t, r, r.alice, tn, "Done.")
	if code, b := e.do(t, "PATCH", path, tok, map[string]int{"autocompact": 0}); code != http.StatusOK {
		t.Fatalf("auto: %d %s", code, b)
	}
	if tn := e.press(t, r, r.alice, tok, false); e.job(t, r, r.alice, tn.ID).Autocompact != 0 {
		t.Fatal("auto is not 0")
	}
}

// 003-AC24: the context use a turn reports is kept on its session and shown with the persona; a
// window smaller than the tokens is dropped, and a new session starts with its own figure.
func TestContextUse(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	tok := r.alice.p.Token
	grill := func() store.RoomPersona {
		st := e.agent(t, r.room, tok)
		i := slices.IndexFunc(st.Personas, func(p store.RoomPersona) bool { return p.ID == r.grill.ID })
		return st.Personas[i]
	}
	if p := grill(); p.ContextTokens != 0 || p.ContextWindow != 0 {
		t.Fatalf("before any turn: %+v", p)
	}
	tn := e.press(t, r, r.alice, tok, false)
	if code, b := e.result(t, r, r.alice, tn.ID, turn.Result{SessionID: sid1, Session: []byte("x\n"), Synthesis: "One.",
		ContextTokens: 170_000, ContextWindow: 200_000}); code != http.StatusOK {
		t.Fatalf("result: %d %s", code, b)
	}
	if p := grill(); p.ContextTokens != 170_000 || p.ContextWindow != 200_000 {
		t.Fatalf("after a turn: %+v", p)
	}
	tn = e.press(t, r, r.alice, tok, false)
	if code, b := e.result(t, r, r.alice, tn.ID, turn.Result{SessionID: sid1, SessionOffset: 2, Session: []byte("x\n"), Synthesis: "Two.",
		ContextTokens: 300_000, ContextWindow: 200_000}); code != http.StatusOK {
		t.Fatalf("result: %d %s", code, b)
	}
	if p := grill(); p.ContextTokens != 0 || p.ContextWindow != 0 {
		t.Fatalf("a window under the tokens was kept: %+v", p)
	}
	tn = e.press(t, r, r.alice, tok, true)
	if code, b := e.result(t, r, r.alice, tn.ID, turn.Result{SessionID: sid2, Session: []byte("y\n"), Synthesis: "Fresh.",
		ContextTokens: 20_000, ContextWindow: 1_000_000}); code != http.StatusOK {
		t.Fatalf("result: %d %s", code, b)
	}
	if p := grill(); p.ContextTokens != 20_000 || p.ContextWindow != 1_000_000 {
		t.Fatalf("new session: %+v", p)
	}
}

// 003-AC23: the transcript is a folder per day of chunks of turn.TranscriptChunk messages; on the
// next turn a full chunk holds the same bytes unless one of its messages was edited, and new
// messages land in the last chunk only.
func TestTranscriptChunks(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	tok := r.alice.p.Token
	pid := r.alice.p.Participant.ID
	var first int64
	for i := range 2*turn.TranscriptChunk + 50 {
		m := &store.Message{RoomID: r.room, Kind: store.KindText, ParticipantID: pid, Sender: "Alice", Body: fmt.Sprintf("line %d", i)}
		if err := e.st.InsertMessage(t.Context(), m); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = m.ID
		}
	}
	day := "transcript/" + time.Now().UTC().Format(time.DateOnly) + "/"
	hashes := func(id int64) map[string]string {
		code, b := e.do(t, "GET", r.daemonPath(fmt.Sprintf("turns/%d/manifest", id)), r.alice.tok, nil)
		if code != http.StatusOK {
			t.Fatalf("manifest: %d %s", code, b)
		}
		out := map[string]string{}
		for _, f := range decodeJSON[turn.Manifest](t, b).Files {
			if strings.HasPrefix(f.Path, "transcript/") {
				out[f.Path] = f.SHA256
			}
		}
		return out
	}
	tn := e.press(t, r, r.alice, tok, false)
	ws := e.workspace(t, r, r.alice, tn.ID)
	for n, want := range map[string]int{"001.md": turn.TranscriptChunk, "002.md": turn.TranscriptChunk, "003.md": 0} {
		got := strings.Count(ws[day+n].data, "\n- ")
		if want > 0 && got != want {
			t.Errorf("%s holds %d messages, want %d", n, got, want)
		}
		if !strings.HasPrefix(ws[day+n].data, "# "+strings.TrimSuffix(strings.TrimPrefix(day, "transcript/"), "/")+" (UTC), part ") {
			t.Errorf("%s header: %.40q", n, ws[day+n].data)
		}
	}
	if !strings.Contains(ws[day+"001.md"].data, "**Alice**: line 0\n") || !strings.HasSuffix(ws[day+"003.md"].data, "**Alice**: line 449\n") {
		t.Errorf("chunks out of order:\n%s", ws[day+"003.md"].data)
	}
	before := hashes(tn.ID)
	e.finish(t, r, r.alice, tn, "Read it all.")
	if _, _, err := e.st.EditMessage(t.Context(), r.room, first, pid, "line 0, edited"); err != nil {
		t.Fatal(err)
	}
	e.postBody(t, r, tok, "One more.")
	after := hashes(e.press(t, r, r.alice, tok, false).ID)
	var changed []string
	for p, h := range after {
		if before[p] != h {
			changed = append(changed, strings.TrimPrefix(p, day))
		}
	}
	slices.Sort(changed)
	if !slices.Equal(changed, []string{"001.md", "003.md"}) || len(after) != 3 {
		t.Fatalf("changed chunks %v of %v", changed, after)
	}
}
