package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/anchor"
)

// turnRoom is a room with Alice, her paired daemon, an upload and an agent document holding an
// annotation on "brown fox".
type turnRoom struct {
	p      *Participant
	daemon int64
	doc    int64
	ann    int64
	// grill is the room's Default persona.
	grill RoomPersona
}

func newTurnRoom(t *testing.T, s *Store, room string) turnRoom {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateRoom(ctx, room, "R"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInvite(ctx, room, "inv-"+room, 0); err != nil {
		t.Fatal(err)
	}
	p, _, err := s.Join(ctx, "inv-"+room, "Alice", "tok-"+room, JoinProfile{})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.PairDaemon(ctx, room, p.ID, "dmn-"+room)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := s.CreateAgentDocument(ctx, room, "plan.md", "md", "# Plan\n\nThe quick brown fox.\n", AgentName)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateAnnotation(ctx, room, doc.ID, 1, &anchor.Anchor{Kind: anchor.KindText, Exact: "brown fox"},
		&NewMessage{ParticipantID: p.ID, Author: "Alice", Body: "which fox?"}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	ps, err := s.RoomPersonas(ctx, room)
	if err != nil || len(ps) != 1 {
		t.Fatalf("personas: %+v, %v", ps, err)
	}
	return turnRoom{p: p, daemon: d.ID, doc: doc.ID, ann: a.ID, grill: ps[0]}
}

func (r turnRoom) start(t *testing.T, s *Store, room string) *Turn {
	t.Helper()
	// The turn reads every annotation message so far, as the room's prepareTurn does.
	_, cursor, err := s.AnnotationsSince(context.Background(), room, 0)
	if err != nil {
		t.Fatal(err)
	}
	tn := &Turn{RoomID: room, ParticipantID: r.p.ID, DaemonID: r.daemon, Prompt: "go", AnnotationCursor: cursor,
		PersonaID: r.grill.ID, PersonaName: r.grill.Name}
	if err := s.StartTurn(context.Background(), tn, false); err != nil {
		t.Fatal(err)
	}
	return tn
}

// 001-AC32: one running turn per room; a second press is refused until the first ends.
func TestStartTurnOnePerRoom(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	first := r.start(t, s, "a")
	if err := s.StartTurn(ctx, &Turn{RoomID: "a", PersonaID: r.grill.ID}, false); !errors.Is(err, ErrTurnInProgress) {
		t.Fatalf("second turn: %v, want ErrTurnInProgress", err)
	}
	// Another room is independent.
	rb := newTurnRoom(t, s, "b")
	if err := s.StartTurn(ctx, &Turn{RoomID: "b", PersonaID: rb.grill.ID}, false); err != nil {
		t.Fatalf("other room: %v", err)
	}
	m, err := s.FailTurn(ctx, "a", first.ID, "boom")
	if err != nil || m == nil || m.Body != "Grill turn failed: boom" || m.Sender != "Grill" || m.Kind != KindSystem {
		t.Fatalf("fail: %+v, %v", m, err)
	}
	if m, err := s.FailTurn(ctx, "a", first.ID, "again"); err != nil || m != nil {
		t.Fatalf("failing a failed turn: %+v, %v", m, err)
	}
	// 002-AC16: a turn runs a persona of its own room.
	for _, tn := range []*Turn{{RoomID: "a"}, {RoomID: "a", PersonaID: rb.grill.ID}, {RoomID: "gone", PersonaID: r.grill.ID}} {
		if err := s.StartTurn(ctx, tn, false); !errors.Is(err, ErrNotFound) {
			t.Fatalf("turn %+v: %v, want ErrNotFound", tn, err)
		}
	}
	if err := s.StartTurn(ctx, &Turn{RoomID: "a", PersonaID: r.grill.ID}, false); err != nil {
		t.Fatalf("turn after failure: %v", err)
	}
}

// 001-AC32 001-AC16 001-AC33: a done turn leaves revisions by the agent (annotations re-anchored),
// a new document, replies, the synthesis, a note listing the refused upload changes, and the
// session bytes.
func TestCompleteTurnApplies(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	tn := r.start(t, s, "a")
	done, err := s.CompleteTurn(ctx, "a", tn.ID, r.daemon, &TurnOutcome{
		Synthesis: "Here is where we are.",
		Documents: []AgentWrite{
			{Name: "plan.md", Format: "md", Body: "# Plan\n\nThe slow brown fox.\n"},
			{Name: "risks.html", Format: "html", Body: "<p>risk</p>"},
		},
		Replies:   []AgentReply{{AnnotationID: r.ann, Body: "the brown one"}, {AnnotationID: 999, Body: "lost"}},
		SessionID: "s1", Session: []byte("line1\n"),
		Report: TurnReport{Rejected: []string{"uploads/1-brief.pdf"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != TurnDone || done.SessionID != "s1" || done.EndedAt == nil {
		t.Fatalf("turn: %+v", done)
	}
	want := TurnReport{Revised: []string{"plan.md"}, Created: []string{"risks.html"}, Replies: 1,
		Rejected: []string{"uploads/1-brief.pdf"}, Ignored: []string{"reply to annotation 999: no such annotation"}}
	if !slices.Equal(done.Report.Revised, want.Revised) || !slices.Equal(done.Report.Created, want.Created) ||
		done.Report.Replies != 1 || !slices.Equal(done.Report.Rejected, want.Rejected) || !slices.Equal(done.Report.Ignored, want.Ignored) {
		t.Fatalf("report %+v, want %+v", done.Report, want)
	}
	stored, err := s.Turn(ctx, "a", tn.ID)
	if err != nil || stored.Status != TurnDone || len(stored.Report.Rejected) != 1 {
		t.Fatalf("stored turn: %+v, %v", stored, err)
	}
	rev, err := s.Revision(ctx, r.doc, 2)
	if err != nil || rev.Author != "Grill" || rev.Body != "# Plan\n\nThe slow brown fox.\n" {
		t.Fatalf("revision 2: %+v, %v", rev, err)
	}
	anns, err := s.Annotations(ctx, "a", r.doc)
	if err != nil || len(anns) != 1 || anns[0].Revision != 2 || anns[0].Orphaned {
		t.Fatalf("annotation not re-anchored on r2: %+v, %v", anns, err)
	}
	if msgs := anns[0].Messages; len(msgs) != 2 || msgs[1].By != ByAgent || msgs[1].Author != "Grill" || msgs[1].PersonaID != r.grill.ID || msgs[1].Badge != r.grill.Badge || msgs[1].Body != "the brown one" {
		t.Fatalf("thread: %+v", msgs)
	}
	docs, _ := s.Documents(ctx, "a")
	if len(docs) != 2 || docs[1].Name != "risks.html" || docs[1].Kind != DocAgent || docs[1].Format != "html" {
		t.Fatalf("documents: %+v", docs)
	}
	msgs, _ := s.Messages(ctx, "a", 0, 100)
	last := msgs[len(msgs)-2:]
	if last[0].Kind != KindAgent || last[0].Body != "Here is where we are." || last[0].Sender != "Grill" || last[0].PersonaID != r.grill.ID ||
		last[0].Badge != "🔥" || last[0].Colour != r.grill.Colour || last[1].Kind != KindSystem ||
		last[1].Body != "Grill turn: uploads are read-only, changes refused: uploads/1-brief.pdf. ignored: reply to annotation 999: no such annotation." {
		t.Fatalf("messages: %+v", last)
	}
	if data, err := s.AgentSession(ctx, "a", r.grill.ID, "s1"); err != nil || string(data) != "line1\n" {
		t.Fatalf("session: %q, %v", data, err)
	}
	// A result for a done turn is refused.
	if _, err := s.CompleteTurn(ctx, "a", tn.ID, r.daemon, &TurnOutcome{SessionID: "s1", SessionOffset: 6}); !errors.Is(err, ErrTurnNotRunning) {
		t.Fatalf("second result: %v", err)
	}
}

// 001-AC33: the stored session grows only by uploads that start where it ends; a fresh session
// starts its own copy at 0.
func TestCompleteTurnSessionContinuity(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	complete := func(sid string, off int64, data string) error {
		tn := r.start(t, s, "a")
		_, err := s.CompleteTurn(ctx, "a", tn.ID, r.daemon, &TurnOutcome{SessionID: sid, SessionOffset: off, Session: []byte(data)})
		if err != nil {
			if _, ferr := s.FailTurn(ctx, "a", tn.ID, "test"); ferr != nil {
				t.Fatal(ferr)
			}
		}
		return err
	}
	cases := []struct {
		sid  string
		off  int64
		data string
		err  error
		want string
	}{
		{"s1", 0, "aa\n", nil, "aa\n"},
		{"s1", 3, "bb\n", nil, "aa\nbb\n"},
		{"s1", 3, "cc\n", ErrSessionGap, "aa\nbb\n"}, // overlaps
		{"s1", 9, "cc\n", ErrSessionGap, "aa\nbb\n"}, // leaves a hole
		{"s1", 0, "cc\n", ErrSessionGap, "aa\nbb\n"}, // restarts a stored session
		{"s2", 4, "dd\n", ErrSessionGap, ""},         // new session not at 0
		{"s2", 0, "dd\n", nil, "dd\n"},
	}
	for i, c := range cases {
		if err := complete(c.sid, c.off, c.data); !errors.Is(err, c.err) {
			t.Fatalf("case %d: %v, want %v", i, err, c.err)
		}
		got, err := s.AgentSession(ctx, "a", r.grill.ID, c.sid)
		if c.want == "" {
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("case %d: session %q, %v", i, got, err)
			}
			continue
		}
		if err != nil || string(got) != c.want {
			t.Fatalf("case %d: session %q, %v, want %q", i, got, err, c.want)
		}
	}
}

// 001-AC34: a result that fails half-way keeps nothing: no revision, no reply, no message, no
// session bytes, and the turn stays running for the caller to fail.
func TestCompleteTurnAtomic(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	tn := r.start(t, s, "a")
	before, _ := s.Messages(ctx, "a", 0, 100)
	_, err := s.CompleteTurn(ctx, "a", tn.ID, r.daemon, &TurnOutcome{
		Synthesis: "partial",
		Documents: []AgentWrite{
			{Name: "plan.md", Format: "md", Body: "# Plan\n\nchanged\n"},
			{Name: "bad.pdf", Format: "pdf", Body: "x"},
		},
		Replies:   []AgentReply{{AnnotationID: r.ann, Body: "reply"}},
		SessionID: "s1", Session: []byte("x\n"),
	})
	if err == nil {
		t.Fatal("bad document accepted")
	}
	if revs, _ := s.Revisions(ctx, r.doc); len(revs) != 1 {
		t.Fatalf("partial revision kept: %d revisions", len(revs))
	}
	if anns, _ := s.Annotations(ctx, "a", r.doc); len(anns[0].Messages) != 1 {
		t.Fatal("partial reply kept")
	}
	if after, _ := s.Messages(ctx, "a", 0, 100); len(after) != len(before) {
		t.Fatal("partial message kept")
	}
	if _, err := s.AgentSession(ctx, "a", r.grill.ID, "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial session kept: %v", err)
	}
	if got, _ := s.RunningTurns(ctx, "a"); len(got) != 1 || got[0].ID != tn.ID {
		t.Fatal("turn no longer running")
	}
	// Another daemon cannot complete it, and a failed turn cannot complete either.
	if _, err := s.CompleteTurn(ctx, "a", tn.ID, r.daemon+1, &TurnOutcome{SessionID: "s1"}); !errors.Is(err, ErrTurnNotRunning) {
		t.Fatalf("other daemon: %v", err)
	}
	if _, err := s.CompleteTurn(ctx, "a", tn.ID, r.daemon, &TurnOutcome{}); err == nil {
		t.Fatal("result without session id accepted")
	}
	if _, err := s.FailTurn(ctx, "a", tn.ID, "daemon gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteTurn(ctx, "a", tn.ID, r.daemon, &TurnOutcome{SessionID: "s1"}); !errors.Is(err, ErrTurnNotRunning) {
		t.Fatalf("failed turn: %v", err)
	}
	if _, err := s.CompleteTurn(ctx, "b", tn.ID, r.daemon, &TurnOutcome{SessionID: "s1"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other room: %v", err)
	}
}

func TestFailRunningTurnsAndLookups(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ra := newTurnRoom(t, s, "a")
	rb := newTurnRoom(t, s, "b")
	newTurnRoom(t, s, "c")
	if _, err := s.LastTurn(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no turn yet: %v", err)
	}
	ta := ra.start(t, s, "a")
	if _, err := s.CompleteTurn(ctx, "a", ta.ID, ra.daemon, &TurnOutcome{SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	ta2 := ra.start(t, s, "a")
	rb.start(t, s, "b")
	rooms, err := s.FailRunningTurns(ctx, "server restarted")
	if err != nil || !slices.Equal(rooms, []string{"a", "b"}) {
		t.Fatalf("rooms %v, %v", rooms, err)
	}
	if got, err := s.RunningTurns(ctx, "a"); err != nil || len(got) != 0 {
		t.Fatalf("still running: %v %v", got, err)
	}
	last, err := s.LastTurn(ctx, "a")
	if err != nil || last.ID != ta2.ID || last.Status != TurnFailed || last.Error != "server restarted" {
		t.Fatalf("last turn: %+v, %v", last, err)
	}
	done, err := s.LastDoneTurn(ctx, "a", ra.grill.ID)
	if err != nil || done.ID != ta.ID || done.SessionID != "s" {
		t.Fatalf("last done turn: %+v, %v", done, err)
	}
	if _, err := s.LastDoneTurn(ctx, "b", rb.grill.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no done turn in b: %v", err)
	}
}

// 001-AC31: the host and the button right are per room.
func TestHostAndCanTrigger(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ra := newTurnRoom(t, s, "a")
	rb := newTurnRoom(t, s, "b")
	if err := s.SetHost(ctx, "a", ra.p.ID); err != nil {
		t.Fatal(err)
	}
	if room, _ := s.Room(ctx, "a"); room.HostParticipantID != ra.p.ID {
		t.Fatalf("host %d", room.HostParticipantID)
	}
	if err := s.SetHost(ctx, "a", rb.p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("host from another room: %v", err)
	}
	if err := s.SetCanTrigger(ctx, "a", ra.p.ID, true); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.RoomParticipant(ctx, "a", ra.p.ID); !p.CanTrigger {
		t.Fatal("right not granted")
	}
	if err := s.SetCanTrigger(ctx, "a", ra.p.ID, false); err != nil {
		t.Fatal(err)
	}
	if ps, _ := s.Participants(ctx, "a"); ps[0].CanTrigger {
		t.Fatal("right not revoked")
	}
	if err := s.SetCanTrigger(ctx, "a", rb.p.ID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("participant from another room: %v", err)
	}
	if n, _ := s.MessageCursor(ctx, "c"); n != 0 {
		t.Fatalf("cursor of an empty room: %d", n)
	}
}

// 001-AC16: an agent document never shares its name with an upload (spike S5 listed two
// "plan.md"). A new one is stored under a free "-agent" name, told in the room, and later writes
// under the upload's name revise it; an agent document that had the name first keeps it.
func TestCompleteTurnKeepsAgentNamesOffUploads(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	for _, name := range []string{"brief.md", "brief-agent.md", "plan.md"} {
		u := &Upload{RoomID: "a", Author: "Alice", Filename: name, ContentType: "text/markdown", Format: "md", Data: []byte("x"), Text: "x"}
		if _, err := s.AddUpload(ctx, u, 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	complete := func(body string) *Turn {
		t.Helper()
		tn := r.start(t, s, "a")
		done, err := s.CompleteTurn(ctx, "a", tn.ID, r.daemon, &TurnOutcome{SessionID: "s" + body,
			Documents: []AgentWrite{{Name: "brief.md", Format: "md", Body: body}, {Name: "plan.md", Format: "md", Body: body}}})
		if err != nil {
			t.Fatal(err)
		}
		return done
	}
	done := complete("v1")
	if !slices.Equal(done.Report.Created, []string{"brief-agent-agent.md"}) || !slices.Equal(done.Report.Revised, []string{"plan.md"}) ||
		!slices.Equal(done.Report.Renamed, []string{"brief.md as brief-agent-agent.md"}) {
		t.Fatalf("report %+v", done.Report)
	}
	msgs, _ := s.Messages(ctx, "a", 0, 100)
	if note := msgs[len(msgs)-1].Body; note != "Grill turn: named like an upload, so stored brief.md as brief-agent-agent.md." {
		t.Fatalf("note %q", note)
	}
	done = complete("v2")
	if len(done.Report.Created) != 0 || !slices.Equal(done.Report.Revised, []string{"brief-agent-agent.md", "plan.md"}) {
		t.Fatalf("second turn report %+v", done.Report)
	}
	docs, _ := s.Documents(ctx, "a")
	names := map[string]int{}
	for _, d := range docs {
		names[d.Name]++
	}
	for name, n := range names {
		if n > 1 && name != "plan.md" {
			t.Errorf("%d documents named %s", n, name)
		}
	}
	if len(docs) != 5 {
		t.Fatalf("documents %+v", docs)
	}
}

// 001-AC39: the agent's own annotations land on the latest revision of any document with text,
// one it wrote this turn included, authored by Agent; a reply with Done closes its thread. What
// cannot be placed is reported and the rest of the turn still applies.
func TestCompleteTurnAgentAnnotations(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	up, err := s.AddUpload(ctx, &Upload{RoomID: "a", Author: "Alice", Filename: "brief.md", ContentType: "text/markdown", Format: "md",
		Data: []byte("x"), Text: "# Brief\n\nShip on *Friday* at noon.\n"}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	pdf, err := s.AddUpload(ctx, &Upload{RoomID: "a", Author: "Alice", Filename: "a.pdf", ContentType: "application/pdf", Format: "pdf", Data: []byte("%PDF")}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	tn := r.start(t, s, "a")
	done, err := s.CompleteTurn(ctx, "a", tn.ID, r.daemon, &TurnOutcome{SessionID: "s1",
		Documents: []AgentWrite{{Name: "risks.md", Format: "md", Body: "# Risks\n\nNo rollback.\n"}},
		Replies:   []AgentReply{{AnnotationID: r.ann, Body: "the brown one", Done: true}},
		Annotations: []AgentAnnotation{
			{Path: "uploads/brief.md", DocID: up.ID, Anchor: anchor.Anchor{Exact: "Friday at noon"}, Body: "Which time zone?"},
			{Path: "docs/risks.md", AgentDoc: "risks.md", Anchor: anchor.Anchor{Exact: "No rollback", Suffix: "."}, Body: "Who owns it?"},
			{Path: "docs/plan.md", DocID: r.doc, Anchor: anchor.Anchor{Exact: "lazy dog"}, Body: "x"},
			{Path: "uploads/a.pdf", DocID: pdf.ID, Anchor: anchor.Anchor{Exact: "PDF"}, Body: "x"},
			{Path: "docs/none.md", AgentDoc: "none.md", Anchor: anchor.Anchor{Exact: "x"}, Body: "x"},
			{Path: "docs/plan.md", DocID: r.doc, Anchor: anchor.Anchor{Exact: "brown", Prefix: string(make([]rune, 65))}, Body: "x"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if done.Report.Annotated != 2 || done.Report.Closed != 1 || len(done.Report.Ignored) != 4 {
		t.Fatalf("report %+v", done.Report)
	}
	for i, want := range []string{"docs/plan.md: quote not found", "uploads/a.pdf: the document has no text to quote", "docs/none.md: no such document", "docs/plan.md: prefix and suffix"} {
		if done.Report.Ignored[i] != "annotation on "+want && !strings.HasPrefix(done.Report.Ignored[i], "annotation on "+want) {
			t.Errorf("ignored[%d] = %q, want %q", i, done.Report.Ignored[i], want)
		}
	}
	anns, _ := s.Annotations(ctx, "a", up.ID)
	if len(anns) != 1 || anns[0].Anchor.Exact != "Friday at noon" || anns[0].Revision != 1 || anns[0].Status != StatusOpen ||
		anns[0].Messages[0].By != ByAgent || anns[0].Messages[0].Body != "Which time zone?" {
		t.Fatalf("annotation on the upload %+v", anns)
	}
	docs, _ := s.Documents(ctx, "a")
	risks := docs[len(docs)-1]
	if anns, _ := s.Annotations(ctx, "a", risks.ID); risks.Name != "risks.md" || len(anns) != 1 || anns[0].Anchor.Exact != "No rollback" {
		t.Fatalf("annotation on this turn's document %+v %+v", risks, anns)
	}
	if anns, _ := s.Annotations(ctx, "a", r.doc); anns[0].Status != StatusDone {
		t.Fatalf("closed thread %+v", anns[0])
	}
	// A participant reply reopens the thread the agent closed.
	if _, err := s.AddAnnotationMessage(ctx, "a", r.ann, &NewMessage{ParticipantID: r.p.ID, Author: "Alice", Body: "not yet"}, 1<<20); err != nil {
		t.Fatal(err)
	}
	if anns, _ := s.Annotations(ctx, "a", r.doc); anns[0].Status != StatusReopened {
		t.Fatalf("after Alice's reply %+v", anns[0])
	}

	// Alice wrote again while the next turn ran: the agent's close leaves the thread open for her.
	tn2 := r.start(t, s, "a")
	if _, err := s.AddAnnotationMessage(ctx, "a", r.ann, &NewMessage{ParticipantID: r.p.ID, Author: "Alice", Body: "and the red one?"}, 1<<20); err != nil {
		t.Fatal(err)
	}
	done, err = s.CompleteTurn(ctx, "a", tn2.ID, r.daemon, &TurnOutcome{SessionID: "s1", SessionOffset: 0,
		Replies: []AgentReply{{AnnotationID: r.ann, Body: "settled", Done: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if anns, _ := s.Annotations(ctx, "a", r.doc); anns[0].Status != StatusReopened || done.Report.Closed != 0 || done.Report.Replies != 1 ||
		!slices.Equal(done.Report.Ignored, []string{fmt.Sprintf("closing annotation %d: a participant wrote in it during the turn", r.ann)}) {
		t.Fatalf("close during a participant reply: %+v, %+v", anns[0], done.Report)
	}
}

// 003-AC21: the workspace's base revisions are pinned at the first fetch. An edit of a document
// revised since conflicts, unless it writes what the document holds now; a new document never
// does.
func TestConflictOnBaseRevision(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	tn := r.start(t, s, "a")
	base, err := s.PinBaseRevisions(ctx, "a", tn.ID)
	if err != nil || len(base) != 1 || base[r.doc] != 1 {
		t.Fatalf("base %v %v", base, err)
	}
	if _, err := s.AddRevision(ctx, "a", r.doc, "# Plan\n\nA lazy dog.\n", "Secretary"); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.PinBaseRevisions(ctx, "a", tn.ID); again[r.doc] != 1 {
		t.Fatalf("the pin moved: %v", again)
	}
	edit := func(body string) error {
		_, err := s.CompleteTurn(ctx, "a", tn.ID, r.daemon, &TurnOutcome{SessionID: "s1",
			Documents: []AgentWrite{{Name: "new.md", Format: "md", Body: "# New"}, {Name: "plan.md", Format: "md", Body: body}}})
		return err
	}
	var conflict *ConflictError
	if err := edit("# Plan\n\nThe quick red fox.\n"); !errors.As(err, &conflict) || conflict.Doc != "plan.md" || err.Error() != "conflict on plan.md" {
		t.Fatalf("edit of a revised document: %v", err)
	}
	if docs, _ := s.Documents(ctx, "a"); len(docs) != 1 {
		t.Fatalf("a conflicting result kept a document: %+v", docs)
	}
	if err := edit("# Plan\n\nA lazy dog.\n"); err != nil {
		t.Fatalf("an edit matching the new version: %v", err)
	}
}

// 003-AC21: a first conflict queues the turn again ahead of the room's queue, taking the place of
// its persona's queued turn; the re-run's conflict fails it for good. Only the turn's daemon can.
func TestRejectTurnRunsAgainFirst(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	sec, _, err := s.AddPersona(ctx, "a", templateByPreset(t, s, "secretary").ID, r.p)
	if err != nil {
		t.Fatal(err)
	}
	tn := r.start(t, s, "a")
	for _, q := range []*QueuedTurn{{RoomID: "a", PersonaID: sec.ID, ParticipantID: r.p.ID}, {RoomID: "a", PersonaID: r.grill.ID, ParticipantID: r.p.ID, Fresh: true}} {
		if err := s.Enqueue(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.RejectTurn(ctx, "a", tn.ID, r.daemon+1, "plan.md"); !errors.Is(err, ErrTurnNotRunning) {
		t.Fatalf("another daemon: %v", err)
	}
	m, rerun, err := s.RejectTurn(ctx, "a", tn.ID, r.daemon, "plan.md")
	if err != nil || !rerun || !strings.Contains(m.Body, "plan.md changed while it ran") {
		t.Fatalf("first conflict: %+v %v %v", m, rerun, err)
	}
	q, _ := s.Queue(ctx, "a")
	if len(q) != 2 || q[0].PersonaID != r.grill.ID || q[0].RerunOf != tn.ID || !q[0].Fresh || q[0].Cause != "re-run after a conflict on plan.md" || q[1].PersonaID != sec.ID {
		t.Fatalf("queue %+v", q)
	}
	if c, _ := roomChain(ctx, s.db, "a"); c.Count != 0 {
		t.Fatalf("the re-run counted as a chain link: %+v", c)
	}
	head, err := s.PopQueued(ctx, "a", false)
	if err != nil || head.RerunOf != tn.ID {
		t.Fatalf("head %+v %v", head, err)
	}
	again := &Turn{RoomID: "a", ParticipantID: r.p.ID, DaemonID: r.daemon, Prompt: "go", PersonaID: r.grill.ID, PersonaName: r.grill.Name,
		RerunOf: head.RerunOf, Cause: head.Cause}
	if err := s.StartTurn(ctx, again, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Turn(ctx, "a", again.ID); got.RerunOf != tn.ID || got.Cause != head.Cause {
		t.Fatalf("re-run %+v", got)
	}
	m, rerun, err = s.RejectTurn(ctx, "a", again.ID, r.daemon, "plan.md")
	if err != nil || rerun || m.Body != "Grill turn failed: conflict on plan.md." {
		t.Fatalf("second conflict: %+v %v %v", m, rerun, err)
	}
	if q, _ := s.Queue(ctx, "a"); len(q) != 1 || q[0].PersonaID != sec.ID {
		t.Fatalf("queue after the second conflict %+v", q)
	}
	if _, _, err := s.RejectTurn(ctx, "a", again.ID, r.daemon, "plan.md"); !errors.Is(err, ErrTurnNotRunning) {
		t.Fatalf("an ended turn: %v", err)
	}
}

// 003-AC20: with parallel on, two personas run at once, never one twice; the queue hands out the
// first entry whose persona does not run.
func TestParallelStartAndPop(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	sec, _, err := s.AddPersona(ctx, "a", templateByPreset(t, s, "secretary").ID, r.p)
	if err != nil {
		t.Fatal(err)
	}
	r.start(t, s, "a")
	if err := s.StartTurn(ctx, &Turn{RoomID: "a", PersonaID: sec.ID}, false); !errors.Is(err, ErrTurnInProgress) {
		t.Fatalf("parallel off: %v", err)
	}
	if err := s.StartTurn(ctx, &Turn{RoomID: "a", PersonaID: r.grill.ID}, true); !errors.Is(err, ErrTurnInProgress) {
		t.Fatalf("the same persona twice: %v", err)
	}
	for _, id := range []int64{r.grill.ID, sec.ID} {
		if err := s.Enqueue(ctx, &QueuedTurn{RoomID: "a", PersonaID: id, ParticipantID: r.p.ID}); err != nil {
			t.Fatal(err)
		}
	}
	if q, err := s.PopQueued(ctx, "a", true); err != nil || q.PersonaID != sec.ID {
		t.Fatalf("pop %+v %v", q, err)
	}
	if q, err := s.PopQueued(ctx, "a", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the running persona's entry was popped: %+v %v", q, err)
	}
	if err := s.StartTurn(ctx, &Turn{RoomID: "a", PersonaID: sec.ID}, true); err != nil {
		t.Fatal(err)
	}
	if ts, _ := s.RunningTurns(ctx, "a"); len(ts) != 2 {
		t.Fatalf("running %+v", ts)
	}
}

// 003-AC18: the host's daemon queues a running turn and lets it go; nothing changes twice, and
// only that daemon can say so.
func TestSetHostQueued(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	tn := r.start(t, s, "a")
	for _, step := range []struct {
		queued, changed bool
	}{{true, true}, {true, false}, {false, true}, {false, false}} {
		changed, err := s.SetHostQueued(ctx, "a", tn.ID, r.daemon, step.queued)
		got, _ := s.Turn(ctx, "a", tn.ID)
		if err != nil || changed != step.changed || got.HostQueued != step.queued {
			t.Fatalf("queued %v: changed %v, turn %+v, %v", step.queued, changed, got, err)
		}
	}
	if _, err := s.SetHostQueued(ctx, "a", tn.ID, r.daemon+1, true); !errors.Is(err, ErrTurnNotRunning) {
		t.Fatalf("another daemon: %v", err)
	}
	if _, err := s.SetHostQueued(ctx, "a", tn.ID+9, r.daemon, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no such turn: %v", err)
	}
}

// 003-AC20: switching parallel mode says so once in the room; switching to the same says nothing.
func TestSetParallelTurns(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	for _, step := range []struct {
		on   bool
		says string
	}{{true, "Alice let the personas run at the same time, each in its own sandbox."}, {true, ""}, {false, "Alice let the personas run one at a time again."}} {
		m, err := s.SetParallelTurns(ctx, "a", step.on, r.p)
		room, _ := s.Room(ctx, "a")
		if err != nil || room.ParallelTurns != step.on || (m == nil) != (step.says == "") || (m != nil && m.Body != step.says) {
			t.Fatalf("on %v: %+v, room %+v, %v", step.on, m, room, err)
		}
	}
}

// 005-AC5: a stopped turn ends as "stopped" with a line in the room; its result is refused with
// nothing applied, a second stop finds it ended, and the persona's last done turn stays the one
// before, whose session the next turn resumes.
func TestStopTurn(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	first := r.start(t, s, "a")
	if _, err := s.CompleteTurn(ctx, "a", first.ID, r.daemon, &TurnOutcome{SessionID: "s1", Session: []byte("one\n")}); err != nil {
		t.Fatal(err)
	}
	tn := r.start(t, s, "a")
	m, err := s.StopTurn(ctx, "a", tn.ID, "Bob")
	if err != nil || m.Body != "Grill turn stopped by Bob." || m.Kind != KindSystem || m.Sender != "Grill" {
		t.Fatalf("stop: %+v, %v", m, err)
	}
	if _, err := s.StopTurn(ctx, "a", tn.ID, "Bob"); !errors.Is(err, ErrTurnNotRunning) {
		t.Fatalf("second stop: %v", err)
	}
	if _, err := s.StopTurn(ctx, "b", tn.ID, "Bob"); !errors.Is(err, ErrTurnNotRunning) {
		t.Fatalf("stop from another room: %v", err)
	}
	stored, err := s.Turn(ctx, "a", tn.ID)
	if err != nil || stored.Status != TurnStopped || stored.Error != "stopped by Bob" || stored.EndedAt == nil {
		t.Fatalf("stored: %+v, %v", stored, err)
	}
	out := &TurnOutcome{Synthesis: "late", SessionID: "s1", SessionOffset: 4, Session: []byte("two\n"),
		Documents: []AgentWrite{{Name: "plan.md", Format: "md", Body: "# Plan\n\nchanged\n"}}}
	if _, err := s.CompleteTurn(ctx, "a", tn.ID, r.daemon, out); !errors.Is(err, ErrTurnNotRunning) {
		t.Fatalf("result after stop: %v", err)
	}
	if _, err := s.Revision(ctx, r.doc, 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a revision was applied: %v", err)
	}
	if data, err := s.AgentSession(ctx, "a", r.grill.ID, "s1"); err != nil || string(data) != "one\n" {
		t.Fatalf("session: %q, %v", data, err)
	}
	last, err := s.LastDoneTurn(ctx, "a", r.grill.ID)
	if err != nil || last.ID != first.ID {
		t.Fatalf("last done: %+v, %v", last, err)
	}
	// The room runs again: the stopped turn holds no slot.
	r.start(t, s, "a")
}
