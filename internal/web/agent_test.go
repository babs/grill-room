package web

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/anchor"
	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

// hostDaemon is a test daemon: its token, its participant, its event stream.
type testDaemon struct {
	tok    string
	p      joined
	events <-chan sseEvent
	drop   context.CancelFunc
}

// daemonFor pairs a daemon for a new participant of room.
func (e *env) daemonFor(t *testing.T, room, name string) *testDaemon {
	t.Helper()
	tok, p := e.pairedDaemon(t, room, name)
	return &testDaemon{tok: tok, p: p}
}

// connect opens the daemon's stream and reports its readiness.
func (e *env) connect(t *testing.T, room string, d *testDaemon, canHost bool) {
	t.Helper()
	d.events, d.drop = e.openStream(t, "/api/rooms/"+room+"/daemon/events", d.tok, "")
	if ev := next(t, d.events); ev.event != "hello" {
		t.Fatalf("got %+v, want hello", ev)
	}
	e.status(t, room, d, canHost)
}

func (e *env) status(t *testing.T, room string, d *testDaemon, canHost bool) {
	t.Helper()
	reason := ""
	if !canHost {
		reason = "Docker is not running"
	}
	if code, b := e.do(t, "POST", "/api/rooms/"+room+"/daemon/status", d.tok, map[string]any{"can_host": canHost, "reason": reason}); code != http.StatusNoContent {
		t.Fatalf("status: %d %s", code, b)
	}
}

func (e *env) agent(t *testing.T, room, tok string) agentState {
	t.Helper()
	code, b := e.do(t, "GET", "/api/rooms/"+room+"/agent", tok, nil)
	if code != http.StatusOK {
		t.Fatalf("agent: %d %s", code, b)
	}
	return decodeJSON[agentState](t, b)
}

// waitAgent polls the agent state until ok holds.
func (e *env) waitAgent(t *testing.T, room, tok string, ok func(agentState) bool) agentState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := e.agent(t, room, tok)
		if ok(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent state never matched: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func hostIs(id int64, online bool) func(agentState) bool {
	return func(st agentState) bool { return st.HostID == id && st.Online == online }
}

// 001-AC30: the first participant whose daemon connects and can host becomes host; the LED is off
// with no host connected; when the host drops, the next eligible daemon, in connection order, hosts.
func TestHostElection(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "R").ID
	alice, bob, carol := e.daemonFor(t, room, "Alice"), e.daemonFor(t, room, "Bob"), e.daemonFor(t, room, "Carol")
	ch, _ := e.openStream(t, "/api/rooms/"+room+"/events", alice.p.Token, "")
	first := decodeJSON[agentState](t, []byte(nextEvent(t, ch, "agent").data))
	if first.Online || first.HostID != 0 || first.Reason != "No host daemon is connected." {
		t.Fatalf("initial state %+v", first)
	}

	// A daemon that cannot host is not elected.
	e.connect(t, room, carol, false)
	if st := e.agent(t, room, alice.p.Token); st.HostID != 0 || st.Online || len(st.Daemons) != 1 {
		t.Fatalf("carol elected while unable to host: %+v", st)
	}
	e.connect(t, room, alice, true)
	e.connect(t, room, bob, true)
	st := e.agent(t, room, alice.p.Token)
	if !hostIs(alice.p.Participant.ID, true)(st) {
		t.Fatalf("alice should host: %+v", st)
	}
	// The stream tells the LED went green, without a reload.
	for !strings.Contains(nextEvent(t, ch, "agent").data, `"online":true`) {
	}
	alice.drop()
	e.waitAgent(t, room, alice.p.Token, hostIs(bob.p.Participant.ID, true))
	// Alice back: Bob keeps the role.
	e.connect(t, room, alice, true)
	e.status(t, room, carol, true)
	if st := e.agent(t, room, alice.p.Token); st.HostID != bob.p.Participant.ID {
		t.Fatalf("host moved on reconnect: %+v", st)
	}
	// Bob's daemon loses Docker: the earliest eligible daemon, Carol's, takes over.
	e.status(t, room, bob, false)
	e.waitAgent(t, room, alice.p.Token, hostIs(carol.p.Participant.ID, true))
	carol.drop()
	e.waitAgent(t, room, alice.p.Token, hostIs(alice.p.Participant.ID, true))
	alice.drop()
	bob.drop()
	off := e.waitAgent(t, room, alice.p.Token, hostIs(alice.p.Participant.ID, false))
	if off.Reason != "No host daemon is connected." || len(off.Daemons) != 0 {
		t.Fatalf("offline state %+v", off)
	}
	msgs, _ := e.st.Messages(t.Context(), room, 0, 100)
	var hosts []string
	for _, m := range msgs {
		if strings.Contains(m.Body, "hosts the agent") {
			hosts = append(hosts, m.Sender)
		}
	}
	if want := []string{"Alice", "Bob", "Carol", "Alice"}; !slices.Equal(hosts, want) {
		t.Fatalf("host announcements %v, want %v", hosts, want)
	}
	// 001-AC30: transcripts keep recording with the agent off.
	if code, _ := e.do(t, "POST", "/api/rooms/"+room+"/messages", bob.p.Token, map[string]string{"body": "still here"}); code != http.StatusCreated {
		t.Fatalf("message with the agent off: %d", code)
	}
	if code, b := e.do(t, "POST", "/api/rooms/"+room+"/turns", alice.p.Token, map[string]bool{}); code != http.StatusConflict || !strings.Contains(string(b), "offline") {
		t.Fatalf("press with the agent off: %d %s", code, b)
	}
}

// nextEvent returns the next event named name, skipping others.
func nextEvent(t *testing.T, ch <-chan sseEvent, name string) sseEvent {
	t.Helper()
	for {
		if ev := next(t, ch); ev.event == name {
			return ev
		}
	}
}

// 001-AC31: the host hands over to a participant with a connected daemon, and grants or revokes
// the button; nobody else can.
func TestHostRights(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "R").ID
	alice, bob := e.daemonFor(t, room, "Alice"), e.daemonFor(t, room, "Bob")
	carol := e.join(t, e.createInvite(t, room).Token, "Carol")
	e.connect(t, room, alice, true)
	carolPath := "/api/rooms/" + room + "/participants/" + strconv.FormatInt(carol.Participant.ID, 10)

	for _, tc := range []struct {
		method, path, tok string
		body              any
		code              int
	}{
		{"PATCH", carolPath, bob.p.Token, map[string]bool{"can_trigger": true}, http.StatusForbidden},
		{"PUT", "/api/rooms/" + room + "/host", bob.p.Token, map[string]int64{"participant_id": bob.p.Participant.ID}, http.StatusForbidden},
		{"POST", "/api/rooms/" + room + "/turns", carol.Token, map[string]bool{}, http.StatusForbidden},
		{"PATCH", carolPath, alice.p.Token, map[string]any{}, http.StatusBadRequest},
		{"PATCH", "/api/rooms/" + room + "/participants/999", alice.p.Token, map[string]bool{"can_trigger": true}, http.StatusNotFound},
		{"PATCH", "/api/rooms/" + room + "/participants/x", alice.p.Token, map[string]bool{"can_trigger": true}, http.StatusNotFound},
		// Bob's daemon is not connected yet, Carol has none.
		{"PUT", "/api/rooms/" + room + "/host", alice.p.Token, map[string]int64{"participant_id": bob.p.Participant.ID}, http.StatusConflict},
		{"PUT", "/api/rooms/" + room + "/host", alice.p.Token, map[string]int64{"participant_id": carol.Participant.ID}, http.StatusConflict},
		{"PUT", "/api/rooms/" + room + "/host", alice.p.Token, map[string]int64{"participant_id": alice.p.Participant.ID}, http.StatusNoContent},
		{"PATCH", carolPath, alice.p.Token, map[string]bool{"can_trigger": true}, http.StatusNoContent},
	} {
		if code, b := e.do(t, tc.method, tc.path, tc.tok, tc.body); code != tc.code {
			t.Fatalf("%s %s as %s: %d %s, want %d", tc.method, tc.path, tc.tok[:4], code, b, tc.code)
		}
	}
	if st := e.agent(t, room, carol.Token); !slices.Equal(st.CanTrigger, []int64{carol.Participant.ID}) {
		t.Fatalf("can_trigger %v", st.CanTrigger)
	}
	// Carol may press now, but not start a new session.
	if code, b := e.do(t, "POST", "/api/rooms/"+room+"/turns", carol.Token, map[string]bool{"fresh": true}); code != http.StatusForbidden {
		t.Fatalf("fresh by a non-host: %d %s", code, b)
	}
	if code, b := e.do(t, "POST", "/api/rooms/"+room+"/turns", carol.Token, map[string]bool{}); code != http.StatusCreated {
		t.Fatalf("press by an allowed participant: %d %s", code, b)
	}
	e.connect(t, room, bob, true)
	if code, b := e.do(t, "PUT", "/api/rooms/"+room+"/host", alice.p.Token, map[string]int64{"participant_id": bob.p.Participant.ID}); code != http.StatusNoContent {
		t.Fatalf("hand over: %d %s", code, b)
	}
	if st := e.agent(t, room, alice.p.Token); !hostIs(bob.p.Participant.ID, true)(st) {
		t.Fatalf("after hand-over: %+v", st)
	}
	// Alice is no longer host: she lost the right to grant.
	if code, _ := e.do(t, "PATCH", carolPath, alice.p.Token, map[string]bool{"can_trigger": false}); code != http.StatusForbidden {
		t.Fatalf("former host granted: %d", code)
	}
	if code, _ := e.do(t, "PATCH", carolPath, bob.p.Token, map[string]bool{"can_trigger": false}); code != http.StatusNoContent {
		t.Fatalf("new host revoke: %d", code)
	}
	if st := e.agent(t, room, carol.Token); len(st.CanTrigger) != 0 {
		t.Fatalf("can_trigger after revoke %v", st.CanTrigger)
	}
}

// turnRoom has Alice hosting with her daemon connected, a message, an upload of each kind, an
// agent document with an annotation carrying an image.
type turnRoom struct {
	room   string
	alice  *testDaemon
	doc    int64
	ann    int64
	pdf    int64
	office int64
}

func newTurnRoom(t *testing.T, e *env) *turnRoom {
	t.Helper()
	ctx := t.Context()
	r := &turnRoom{room: e.createRoom(t, "Grill").ID}
	r.alice = e.daemonFor(t, r.room, "Alice")
	e.connect(t, r.room, r.alice, true)
	e.do(t, "POST", "/api/rooms/"+r.room+"/messages", r.alice.p.Token, map[string]string{"body": "We ship on Friday.\nNo rollback plan yet."})
	pid := r.alice.p.Participant.ID
	pdf, err := e.st.AddUpload(ctx, &store.Upload{RoomID: r.room, ParticipantID: pid, Author: "Alice", Filename: "brief.pdf", ContentType: "application/pdf", Format: "pdf", Data: []byte("%PDF-1.4")}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	office, err := e.st.AddUpload(ctx, &store.Upload{RoomID: r.room, ParticipantID: pid, Author: "Alice", Filename: "plan.docx",
		ContentType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", Format: "md", Data: []byte("PK..."), Text: "# Converted plan\n"}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := e.st.CreateAgentDocument(ctx, r.room, "synthesis.md", "md", "# Synthesis\n\nThe quick brown fox.\n", store.AgentName)
	if err != nil {
		t.Fatal(err)
	}
	ann, err := e.st.CreateAnnotation(ctx, r.room, doc.ID, 1, &anchor.Anchor{Kind: anchor.KindText, Exact: "brown fox"},
		&store.NewMessage{ParticipantID: pid, Author: "Alice", Body: "which fox?", Attachments: []store.Attachment{{ContentType: "image/png", Data: []byte("PNGDATA")}}}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	r.doc, r.ann, r.pdf, r.office = doc.ID, ann.ID, pdf.ID, office.ID
	return r
}

func (r *turnRoom) daemonPath(s string) string { return "/api/rooms/" + r.room + "/daemon/" + s }

// press starts a turn as tok and returns it once the host daemon got the turn event.
func (e *env) press(t *testing.T, r *turnRoom, d *testDaemon, tok string, fresh bool) store.Turn {
	t.Helper()
	code, b := e.do(t, "POST", "/api/rooms/"+r.room+"/turns", tok, map[string]bool{"fresh": fresh})
	if code != http.StatusCreated {
		t.Fatalf("press: %d %s", code, b)
	}
	tn := decodeJSON[store.Turn](t, b)
	if ev := nextEvent(t, d.events, "turn"); ev.data != fmt.Sprintf(`{"id":%d}`, tn.ID) {
		t.Fatalf("turn event %+v for turn %d", ev, tn.ID)
	}
	return tn
}

func (e *env) job(t *testing.T, r *turnRoom, d *testDaemon, id int64) turn.Job {
	t.Helper()
	code, b := e.do(t, "GET", r.daemonPath("turns/"+strconv.FormatInt(id, 10)), d.tok, nil)
	if code != http.StatusOK {
		t.Fatalf("job: %d %s", code, b)
	}
	return decodeJSON[turn.Job](t, b)
}

type tarFile struct {
	mode int64
	data string
}

func (e *env) workspace(t *testing.T, r *turnRoom, d *testDaemon, id int64) map[string]tarFile {
	t.Helper()
	code, b := e.do(t, "GET", r.daemonPath("turns/"+strconv.FormatInt(id, 10)+"/workspace"), d.tok, nil)
	if code != http.StatusOK {
		t.Fatalf("workspace: %d %s", code, b)
	}
	files := map[string]tarFile{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		files[h.Name] = tarFile{mode: h.Mode, data: string(data)}
	}
}

func (e *env) result(t *testing.T, r *turnRoom, d *testDaemon, id int64, res turn.Result) (int, []byte) {
	t.Helper()
	return e.do(t, "POST", r.daemonPath("turns/"+strconv.FormatInt(id, 10)), d.tok, res)
}

const sid1 = "11111111-2222-4333-8444-555555555555"

// 001-AC32 001-AC33 001-AC16 001-AC12 001-AC20: a press runs one turn on the host daemon. The
// daemon gets the prompt with what is new and the workspace (documents, uploads read-only with
// their Markdown copy, transcript, threads with their images); its result becomes agent
// revisions, a synthesis, replies, and the stored session; the upload it changed is refused.
func TestTurnLifecycle(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	alice := r.alice
	tn := e.press(t, r, alice, alice.p.Token, false)
	if code, b := e.do(t, "POST", "/api/rooms/"+r.room+"/turns", alice.p.Token, map[string]bool{}); code != http.StatusConflict || !strings.Contains(string(b), "turn in progress") {
		t.Fatalf("second press: %d %s", code, b)
	}
	job := e.job(t, r, alice, tn.ID)
	if job.Resume != "" || job.TimeoutSeconds != 900 {
		t.Fatalf("first job %+v", job)
	}
	for _, want := range []string{"We ship on Friday.\n  No rollback plan yet.", "**Alice**: ", "This is a new session",
		fmt.Sprintf("### Annotation %d on docs/synthesis.md (open)", r.ann), `On the text "brown fox".`, "**Alice**: which fox?",
		"image: attachments/", "replies.json"} {
		if !strings.Contains(job.Prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, job.Prompt)
		}
	}
	ws := e.workspace(t, r, alice, tn.ID)
	pdf, office := fmt.Sprintf("uploads/%d-brief.pdf", r.pdf), fmt.Sprintf("uploads/%d-plan.docx", r.office)
	for path, want := range map[string]tarFile{
		"docs/synthesis.md": {0o644, "# Synthesis\n\nThe quick brown fox.\n"},
		pdf:                 {0o444, "%PDF-1.4"},
		office:              {0o444, "PK..."},
		office + ".md":      {0o444, "# Converted plan\n"},
	} {
		if got := ws[path]; got != want {
			t.Errorf("%s = %+v, want %+v", path, got, want)
		}
	}
	day := time.Now().UTC().Format(time.DateOnly)
	if tr := ws["transcript/"+day+".md"].data; !strings.HasPrefix(tr, "# Grill, "+day+" (UTC)\n\n") || !strings.Contains(tr, "_Alice joined_") || !strings.Contains(tr, "**Alice**: We ship on Friday.") {
		t.Errorf("transcript:\n%s", tr)
	}
	if a := ws["annotations.md"].data; !strings.Contains(a, fmt.Sprintf("### Annotation %d on docs/synthesis.md", r.ann)) || strings.Contains(a, "(new)") {
		t.Errorf("annotations.md:\n%s", a)
	}
	var att string
	for p, f := range ws {
		if strings.HasPrefix(p, "attachments/") && strings.HasSuffix(p, ".png") {
			att = f.data
		}
	}
	if att != "PNGDATA" {
		t.Errorf("attachment missing: %q", att)
	}

	code, b := e.result(t, r, alice, tn.ID, turn.Result{
		Synthesis: "Friday is risky.\x07", SessionID: sid1, Session: []byte("one\n"),
		Changes: []turn.Change{
			{Path: "docs/synthesis.md", Content: []byte("# Synthesis\n\nThe slow brown fox.\n")},
			{Path: "docs/rollback.md", Content: []byte("# Rollback\n")},
			{Path: pdf},
			{Path: "replies.json", Content: []byte(fmt.Sprintf(`[{"annotation": %d, "body": "the brown one"}]`, r.ann))},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("result: %d %s", code, b)
	}
	done := decodeJSON[store.Turn](t, b)
	if done.Status != store.TurnDone || !slices.Equal(done.Report.Rejected, []string{pdf}) || done.Report.Replies != 1 ||
		!slices.Equal(done.Report.Revised, []string{"synthesis.md"}) || !slices.Equal(done.Report.Created, []string{"rollback.md"}) {
		t.Fatalf("done turn %+v", done)
	}
	if rev, err := e.st.Revision(t.Context(), r.doc, 2); err != nil || rev.Author != store.AgentName {
		t.Fatalf("revision 2 %+v, %v", rev, err)
	}
	msgs, _ := e.st.Messages(t.Context(), r.room, 0, 100)
	tail := msgs[len(msgs)-2:]
	if tail[0].Kind != store.KindAgent || tail[0].Body != "Friday is risky." || !strings.Contains(tail[1].Body, "uploads are read-only, changes refused: "+pdf) {
		t.Fatalf("messages %+v", tail)
	}
	if orig, _ := e.st.Original(t.Context(), r.room, r.pdf); string(orig) != "%PDF-1.4" {
		t.Fatal("upload changed")
	}
	if st := e.agent(t, r.room, alice.p.Token); st.Turn == nil || st.Turn.ID != tn.ID || st.Turn.Status != store.TurnDone {
		t.Fatalf("agent state turn %+v", st.Turn)
	}

	// The next turn resumes the session and quotes only what is new: not the agent's own words.
	e.do(t, "POST", "/api/rooms/"+r.room+"/messages", alice.p.Token, map[string]string{"body": "What about Monday?"})
	if code, b := e.do(t, "POST", "/api/rooms/"+r.room+"/annotations/"+strconv.FormatInt(r.ann, 10)+"/messages", alice.p.Token, map[string]string{"body": "and the red one?"}); code != http.StatusCreated {
		t.Fatalf("reply: %d %s", code, b)
	}
	tn2 := e.press(t, r, alice, alice.p.Token, false)
	job = e.job(t, r, alice, tn2.ID)
	sum := sha256.Sum256([]byte("one\n"))
	if job.Resume != sid1 || job.SessionSize != 4 || job.SessionSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("second job %+v", job)
	}
	if !strings.Contains(job.Prompt, "What about Monday?") || strings.Contains(job.Prompt, "We ship on Friday") || strings.Contains(job.Prompt, "new session") ||
		strings.Contains(job.Prompt, "Friday is risky") || !strings.Contains(job.Prompt, "- **Agent**: the brown one\n- **Alice** (new): and the red one?") {
		t.Fatalf("second prompt:\n%s", job.Prompt)
	}
	if code, b := e.do(t, "GET", r.daemonPath("sessions/"+sid1), alice.tok, nil); code != http.StatusOK || string(b) != "one\n" {
		t.Fatalf("session: %d %q", code, b)
	}
	if code, _ := e.do(t, "GET", r.daemonPath("sessions/22222222-2222-4333-8444-555555555555"), alice.tok, nil); code != http.StatusNotFound {
		t.Fatalf("other session: %d", code)
	}
	// A session upload with a gap fails the turn and keeps nothing.
	if code, b := e.result(t, r, alice, tn2.ID, turn.Result{SessionID: sid1, SessionOffset: 2, Session: []byte("x"),
		Changes: []turn.Change{{Path: "docs/synthesis.md", Content: []byte("gap")}}}); code != http.StatusConflict {
		t.Fatalf("gap: %d %s", code, b)
	}
	if revs, _ := e.st.Revisions(t.Context(), r.doc); len(revs) != 2 {
		t.Fatalf("revision kept from a refused result: %d", len(revs))
	}
	if last, _ := e.st.LastTurn(t.Context(), r.room); last.Status != store.TurnFailed || !strings.Contains(last.Error, "does not continue") {
		t.Fatalf("turn after gap %+v", last)
	}
	tn3 := e.press(t, r, alice, alice.p.Token, false)
	if code, b := e.result(t, r, alice, tn3.ID, turn.Result{SessionID: sid1, SessionOffset: 4, Session: []byte("two\n")}); code != http.StatusOK {
		t.Fatalf("continuing result: %d %s", code, b)
	}
	if data, _ := e.st.AgentSession(t.Context(), r.room, sid1); string(data) != "one\ntwo\n" {
		t.Fatalf("session %q", data)
	}
}

// 001-AC34 001-AC29: a host daemon that drops mid-turn fails it and its late result is refused; a
// daemon error is shown in the room; an oversized or malformed result fails the turn; only the
// turn's own daemon may fetch or answer it.
func TestTurnFailures(t *testing.T) {
	e := newEnvOpts(t, Options{MaxResultBytes: 4096, MaxUploadBytes: 1 << 20, MaxRoomBytes: 1 << 20})
	r := newTurnRoom(t, e)
	alice := r.alice
	bob := e.daemonFor(t, r.room, "Bob")
	e.connect(t, r.room, bob, true)

	tn := e.press(t, r, alice, alice.p.Token, false)
	for _, path := range []string{"turns/" + strconv.FormatInt(tn.ID, 10), "turns/" + strconv.FormatInt(tn.ID, 10) + "/workspace"} {
		if code, _ := e.do(t, "GET", r.daemonPath(path), bob.tok, nil); code != http.StatusConflict {
			t.Fatalf("bob reads alice's turn %s: %d", path, code)
		}
	}
	if code, _ := e.do(t, "GET", r.daemonPath("turns/999"), alice.tok, nil); code != http.StatusNotFound {
		t.Fatalf("unknown turn: %d", code)
	}
	if code, _ := e.result(t, r, bob, tn.ID, turn.Result{SessionID: sid1}); code != http.StatusConflict {
		t.Fatalf("bob answers alice's turn: %d", code)
	}
	alice.drop()
	e.waitAgent(t, r.room, bob.p.Token, func(st agentState) bool { return st.Turn != nil && st.Turn.Status == store.TurnFailed })
	st := e.waitAgent(t, r.room, bob.p.Token, hostIs(bob.p.Participant.ID, true))
	if st.Turn.Error != "the host daemon on Alice-laptop disconnected" {
		t.Fatalf("turn error %q", st.Turn.Error)
	}
	if code, _ := e.result(t, r, alice, tn.ID, turn.Result{SessionID: sid1, Changes: []turn.Change{{Path: "docs/x.md", Content: []byte("late")}}}); code != http.StatusConflict {
		t.Fatalf("late result: %d", code)
	}
	if docs, _ := e.st.Documents(t.Context(), r.room); len(docs) != 3 {
		t.Fatalf("late result kept a document: %d documents", len(docs))
	}

	// Bob hosts now; a daemon error lands in the room as is.
	tn = e.press(t, r, bob, bob.p.Token, false)
	if code, _ := e.result(t, r, bob, tn.ID, turn.Result{Error: "Claude credentials expired on bob-box"}); code != http.StatusNoContent {
		t.Fatalf("error result: %d", code)
	}
	msgs, _ := e.st.Messages(t.Context(), r.room, 0, 100)
	if last := msgs[len(msgs)-1]; last.Body != "Agent turn failed: Claude credentials expired on bob-box" {
		t.Fatalf("last message %q", last.Body)
	}
	tn = e.press(t, r, bob, bob.p.Token, false)
	if code, _ := e.result(t, r, bob, tn.ID, turn.Result{SessionID: sid1, Synthesis: strings.Repeat("x", 5000)}); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized result: %d", code)
	}
	if last, _ := e.st.LastTurn(t.Context(), r.room); last.Status != store.TurnFailed || !strings.Contains(last.Error, "too large") {
		t.Fatalf("after oversized result %+v", last)
	}
	tn = e.press(t, r, bob, bob.p.Token, false)
	if code, _ := e.result(t, r, bob, tn.ID, turn.Result{SessionID: "not-a-uuid"}); code != http.StatusBadRequest {
		t.Fatalf("bad session id: %d", code)
	}
	if last, _ := e.st.LastTurn(t.Context(), r.room); last.Status != store.TurnFailed {
		t.Fatalf("after bad session id %+v", last)
	}
	tn = e.press(t, r, bob, bob.p.Token, false)
	req, _ := http.NewRequestWithContext(t.Context(), "POST", e.ts.URL+r.daemonPath("turns/"+strconv.FormatInt(tn.ID, 10)), strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer "+bob.tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if last, _ := e.st.LastTurn(t.Context(), r.room); resp.StatusCode != http.StatusBadRequest || last.Status != store.TurnFailed || !strings.Contains(last.Error, "malformed") {
		t.Fatalf("malformed result: %d %+v", resp.StatusCode, last)
	}
	// Re-pairing Bob's daemon mid-turn fails the turn the old daemon process holds.
	e.press(t, r, bob, bob.p.Token, false)
	p := e.requestPairing(t, r.room, "bob-new")
	e.do(t, "POST", "/api/rooms/"+r.room+"/pairings/"+p.ID, bob.p.Token, map[string]bool{"approve": true})
	if last, _ := e.st.LastTurn(t.Context(), r.room); last.Status != store.TurnFailed || last.Error != "the host daemon was paired again" {
		t.Fatalf("after re-pairing %+v", last)
	}
}

// The server's backstop fails a turn whose daemon neither answers nor drops.
func TestTurnTimeoutBackstop(t *testing.T) {
	old := turnGrace
	turnGrace = 0
	t.Cleanup(func() { turnGrace = old })
	e := newEnvOpts(t, Options{TurnTimeout: 50 * time.Millisecond})
	r := newTurnRoom(t, e)
	e.press(t, r, r.alice, r.alice.p.Token, false)
	st := e.waitAgent(t, r.room, r.alice.p.Token, func(st agentState) bool { return st.Turn.Status == store.TurnFailed })
	if st.Turn.Error != "timed out after 50ms" {
		t.Fatalf("error %q", st.Turn.Error)
	}
}

// 001-AC35: a fresh session starts without --resume, says so, and finds the whole room history in
// transcript/.
func TestFreshSession(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	alice := r.alice
	tn := e.press(t, r, alice, alice.p.Token, false)
	if code, b := e.result(t, r, alice, tn.ID, turn.Result{SessionID: sid1, Session: []byte("one\n")}); code != http.StatusOK {
		t.Fatalf("result: %d %s", code, b)
	}
	e.do(t, "POST", "/api/rooms/"+r.room+"/messages", alice.p.Token, map[string]string{"body": "Start over."})
	tn = e.press(t, r, alice, alice.p.Token, true)
	job := e.job(t, r, alice, tn.ID)
	if job.Resume != "" || job.SessionSize != 0 || !strings.Contains(job.Prompt, "This is a new session") || !strings.Contains(job.Prompt, "Start over.") {
		t.Fatalf("fresh job %+v", job)
	}
	ws := e.workspace(t, r, alice, tn.ID)
	tr := ws["transcript/"+time.Now().UTC().Format(time.DateOnly)+".md"].data
	for _, want := range []string{"We ship on Friday.", "Start over."} {
		if !strings.Contains(tr, want) {
			t.Fatalf("transcript lacks %q:\n%s", want, tr)
		}
	}
	const sid2 = "22222222-2222-4333-8444-555555555555"
	if code, b := e.result(t, r, alice, tn.ID, turn.Result{SessionID: sid2, Session: []byte("fresh\n")}); code != http.StatusOK {
		t.Fatalf("fresh result: %d %s", code, b)
	}
	tn = e.press(t, r, alice, alice.p.Token, false)
	if job := e.job(t, r, alice, tn.ID); job.Resume != sid2 {
		t.Fatalf("after a fresh session, resume %q", job.Resume)
	}
}

func TestBuildPromptCapsTranscript(t *testing.T) {
	var msgs []store.Message
	for i := range 3000 {
		msgs = append(msgs, store.Message{ID: int64(i + 1), Kind: store.KindVoice, Sender: "Bob", Body: strings.Repeat("blah ", 10) + strconv.Itoa(i)})
	}
	p := buildPrompt("R", false, msgs, nil, nil, 0, nil)
	if len(p) > maxPromptTranscript+5000 || !strings.Contains(p, "earlier lines are in transcript/") || !strings.Contains(p, "blah 2999") || strings.Contains(p, "blah 0\n") {
		t.Fatalf("prompt of %d bytes", len(p))
	}
	if p := buildPrompt("R", false, nil, nil, nil, 0, nil); !strings.Contains(p, "Nothing was said or annotated") {
		t.Fatalf("empty prompt:\n%s", p)
	}
}

func TestSanitize(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"ok\x00\x1b[31m\n\tdone ", 100, "ok[31m\n\tdone"},
		{"\xff", 100, "\uFFFD"},
		{strings.Repeat("é", 10), 3, "ééé"},
	} {
		if got := sanitize(c.in, c.n); got != c.want {
			t.Errorf("sanitize(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

// 001-AC30: a host whose daemon stays connected but cannot host any more turns the LED red, with
// the daemon's reason, when no other daemon can take over.
func TestHostCannotHost(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "R").ID
	alice := e.daemonFor(t, room, "Alice")
	e.connect(t, room, alice, true)
	e.status(t, room, alice, false)
	st := e.agent(t, room, alice.p.Token)
	if st.HostID != alice.p.Participant.ID || st.Online || st.Reason != "Docker is not running" {
		t.Fatalf("state %+v", st)
	}
}

func TestDescribeAnchorAndPaths(t *testing.T) {
	for want, a := range map[string]*anchor.Anchor{
		"the whole document":               nil,
		`the text "x"`:                     {Kind: anchor.KindText, Exact: "x"},
		`a <li> element reading "Ship it"`: {Kind: anchor.KindElement, Tag: "li", Fingerprint: "Ship it"},
		"a <img> element":                  {Kind: anchor.KindElement, Tag: "img"},
	} {
		if got := describeAnchor(a); got != want {
			t.Errorf("describeAnchor = %q, want %q", got, want)
		}
	}
	for want, d := range map[string]store.Document{
		"docs/plan.md":           {ID: 1, Kind: store.DocAgent, Name: "plan.md", Format: "md"},
		"docs/doc-2.html":        {ID: 2, Kind: store.DocAgent, Name: "../x.html", Format: "html"},
		"uploads/3-a_b.md":       {ID: 3, Kind: store.DocUpload, Name: "a/b.md", Format: "md", ContentType: "text/markdown; charset=utf-8"},
		"uploads/4-deck.pptx.md": {ID: 4, Kind: store.DocUpload, Name: "deck.pptx", Format: "md", ContentType: "application/vnd.ms-powerpoint"},
	} {
		if got := textPath(d); got != want {
			t.Errorf("textPath(%s) = %q, want %q", d.Name, got, want)
		}
	}
}

const sid2 = "22222222-3333-4444-8555-666666666666"

// 001-AC39: over several turns the agent answers participant replies on its threads, opens its
// own anchored annotations on any document it sees (an upload's text, a document it wrote that
// same turn), and closes a thread; a participant reply reopens it. Quotes it cannot place are
// reported, never fatal. 001-AC16: an agent document named like an upload gets its own name.
// The synthesis carries the model the daemon reported.
func TestAgentAnnotationLoop(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	alice := r.alice
	office := fmt.Sprintf("uploads/%d-plan.docx.md", r.office)
	if _, err := e.st.AddUpload(t.Context(), &store.Upload{RoomID: r.room, Author: "Alice", Filename: "rollback.md",
		ContentType: "text/markdown", Format: "md", Data: []byte("x"), Text: "x"}, 1<<20); err != nil {
		t.Fatal(err)
	}
	tn := e.press(t, r, alice, alice.p.Token, false)
	job := e.job(t, r, alice, tn.ID)
	for _, want := range []string{`"status": "done"`, "annotations-new.json", "uploads too", "name no upload has"} {
		if !strings.Contains(job.Prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	newAnns, _ := json.Marshal([]turn.NewAnnotation{
		{Doc: office, Exact: "Converted\nplan", Body: "Who converted it?"},
		{Doc: "docs/rollback.md", Exact: "No plan", Suffix: " yet", Body: "Who writes it?"},
		{Doc: "docs/synthesis.md", Exact: "a purple fox", Body: "lost"},
		{Doc: fmt.Sprintf("uploads/%d-brief.pdf", r.pdf), Exact: "PDF", Body: "no text"},
		{Doc: "docs/nowhere.md", Exact: "x", Body: "no doc"},
	})
	code, b := e.result(t, r, alice, tn.ID, turn.Result{
		Synthesis: "Opened two questions.", Model: "claude-opus-x\x07", SessionID: sid2, Session: []byte("one\n"),
		Changes: []turn.Change{
			{Path: "docs/rollback.md", Content: []byte("# Rollback\n\nNo plan yet.\n")},
			{Path: turn.RepliesFile, Content: []byte(fmt.Sprintf(`[{"annotation": %d, "body": "the brown one"}]`, r.ann))},
			{Path: turn.NewAnnotationsFile, Content: newAnns},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("result: %d %s", code, b)
	}
	done := decodeJSON[store.Turn](t, b)
	if done.Model != "claude-opus-x" || done.Report.Annotated != 2 || done.Report.Replies != 1 ||
		!slices.Equal(done.Report.Created, []string{"rollback-agent.md"}) || len(done.Report.Ignored) != 3 {
		t.Fatalf("turn 1 %+v", done)
	}
	for i, want := range []string{"docs/synthesis.md: quote not found", "-brief.pdf: the document has no text to quote", "docs/nowhere.md: no such document"} {
		if !strings.Contains(done.Report.Ignored[i], want) {
			t.Errorf("ignored[%d] = %q, want %q", i, done.Report.Ignored[i], want)
		}
	}
	msgs, _ := e.st.Messages(t.Context(), r.room, 0, 100)
	if syn := msgs[len(msgs)-2]; syn.Kind != store.KindAgent || syn.Model != "claude-opus-x" {
		t.Fatalf("synthesis %+v", syn)
	}
	agentAnn := func(doc int64) store.Annotation {
		t.Helper()
		anns, err := e.st.Annotations(t.Context(), r.room, doc)
		if err != nil || len(anns) != 1 || anns[0].Messages[0].By != store.ByAgent || anns[0].Messages[0].Author != store.AgentName {
			t.Fatalf("agent annotation on %d: %+v, %v", doc, anns, err)
		}
		return anns[0]
	}
	onUpload := agentAnn(r.office)
	if a := onUpload.Anchor; a == nil || a.Exact != "Converted plan" || onUpload.Status != store.StatusOpen {
		t.Fatalf("annotation on the upload %+v", onUpload)
	}
	docs, _ := e.st.Documents(t.Context(), r.room)
	var rollback int64
	for _, d := range docs {
		if d.Name == "rollback-agent.md" {
			rollback = d.ID
		}
	}
	if a := agentAnn(rollback).Anchor; a == nil || a.Exact != "No plan" || !strings.HasPrefix(a.Suffix, " yet.") {
		t.Fatalf("annotation on this turn's document %+v", a)
	}

	// Round 2: Alice answers the agent's question; the agent sees it as new, answers and closes it.
	ap := "/api/rooms/" + r.room + "/annotations/" + strconv.FormatInt(onUpload.ID, 10) + "/messages"
	if code, b := e.do(t, "POST", ap, alice.p.Token, map[string]string{"body": "Bob did, with pandoc."}); code != http.StatusCreated {
		t.Fatalf("reply: %d %s", code, b)
	}
	tn2 := e.press(t, r, alice, alice.p.Token, false)
	job = e.job(t, r, alice, tn2.ID)
	if want := fmt.Sprintf("### Annotation %d on %s (open)", onUpload.ID, office); !strings.Contains(job.Prompt, want) ||
		!strings.Contains(job.Prompt, "- **Agent**: Who converted it?\n- **Alice** (new): Bob did, with pandoc.") {
		t.Fatalf("second prompt lacks the thread:\n%s", job.Prompt)
	}
	if code, b := e.result(t, r, alice, tn2.ID, turn.Result{SessionID: sid2, SessionOffset: 4, Session: []byte("two\n"),
		Changes: []turn.Change{{Path: turn.RepliesFile, Content: []byte(fmt.Sprintf(`[{"annotation": %d, "body": "Thanks, settled.", "status": "done"}]`, onUpload.ID))}},
	}); code != http.StatusOK || decodeJSON[store.Turn](t, b).Report.Closed != 1 {
		t.Fatalf("turn 2: %d %s", code, b)
	}
	if a := agentAnn(r.office); a.Status != store.StatusDone || len(a.Messages) != 3 {
		t.Fatalf("after closing %+v", a)
	}
	if code, b := e.do(t, "POST", ap, alice.p.Token, map[string]string{"body": "Not quite: the tables broke."}); code != http.StatusCreated {
		t.Fatalf("reopening reply: %d %s", code, b)
	}
	if a := agentAnn(r.office); a.Status != store.StatusReopened {
		t.Fatalf("after Alice's reply %+v", a)
	}
}
