package store

import (
	"context"
	"errors"
	"testing"

	"github.com/babs/grill-room/internal/anchor"
)

func text(exact, prefix, suffix string) *anchor.Anchor {
	return &anchor.Anchor{Kind: anchor.KindText, Exact: exact, Prefix: prefix, Suffix: suffix}
}

func say(p *Participant, body string, images ...string) *NewMessage {
	m := &NewMessage{ParticipantID: p.ID, Author: p.Name, Body: body}
	for _, img := range images {
		m.Attachments = append(m.Attachments, Attachment{ContentType: "image/png", Data: []byte(img)})
	}
	return m
}

func agentSays(body string) *NewMessage { return &NewMessage{Author: "agent", Body: body} }

// annotated sets up room "a" with an agent document and returns the participant and document id.
func annotated(t *testing.T, s *Store, body string) (*Participant, int64) {
	t.Helper()
	p := seed(t, s, "a")
	d, err := s.CreateAgentDocument(context.Background(), "a", "plan.md", "md", body, "agent")
	if err != nil {
		t.Fatal(err)
	}
	return p, d.ID
}

func TestCreateAnnotation(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p, doc := annotated(t, s, "The rollout needs a rollback step.")
	seed(t, s, "b")

	a, err := s.CreateAnnotation(ctx, "a", doc, 1, text("rollback", "needs a ", " step"), say(p, "why?", "img"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != StatusOpen || a.Revision != 1 || len(a.Messages) != 1 || a.Messages[0].By != ByParticipant || len(a.Messages[0].Attachments) != 1 {
		t.Fatalf("created %+v", a)
	}
	whole, err := s.CreateAnnotation(ctx, "a", doc, 0, nil, agentSays("overall: fine"), 1<<20)
	if err != nil || whole.Anchor != nil || whole.Messages[0].By != ByAgent || whole.Messages[0].ParticipantID != 0 {
		t.Fatalf("whole-document comment: %+v, %v", whole, err)
	}

	cases := map[string]struct {
		room string
		doc  int64
		rev  int
		a    *anchor.Anchor
		want error
	}{
		"other room's document": {"b", doc, 1, nil, ErrNotFound},
		"unknown document":      {"a", 9999, 1, nil, ErrNotFound},
		"stale revision":        {"a", doc, 0, text("x", "", ""), ErrStaleRevision},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := s.CreateAnnotation(ctx, tc.room, tc.doc, tc.rev, tc.a, say(p, "x"), 1<<20); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	img, err := s.AddUpload(ctx, &Upload{RoomID: "a", Author: "x", Filename: "i.png", ContentType: "image/png", Format: "image", Data: []byte("png")}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []*anchor.Anchor{text("x", "", ""), {Kind: anchor.KindElement, Path: "body", Tag: "body"}} {
		if _, err := s.CreateAnnotation(ctx, "a", img.ID, 0, a, say(p, "x"), 1<<20); !errors.Is(err, ErrNotAnchorable) {
			t.Fatalf("%s anchor on an image: got %v, want ErrNotAnchorable", a.Kind, err)
		}
	}
	if _, err := s.CreateAnnotation(ctx, "a", img.ID, 0, nil, say(p, "nice"), 1<<20); err != nil {
		t.Fatalf("whole-document comment on an image: %v", err)
	}
	if _, err := s.CreateAnnotation(ctx, "a", doc, 1, &anchor.Anchor{Kind: "range"}, say(p, "x"), 1<<20); err == nil {
		t.Fatal("invalid anchor stored")
	}

	list, err := s.Annotations(ctx, "a", doc)
	if err != nil || len(list) != 2 || list[0].Anchor.Exact != "rollback" || list[1].Anchor != nil {
		t.Fatalf("list: %+v, %v", list, err)
	}
	if at := list[0].Messages[0].Attachments[0]; at.Data != nil || at.Size != 3 || at.ContentType != "image/png" {
		t.Fatalf("listed attachment: %+v", at)
	}
	if other, _ := s.Annotations(ctx, "b", doc); len(other) != 0 {
		t.Fatalf("another room reads %d annotations of this one", len(other))
	}
}

// 001-AC18: threads take messages from participants and the agent; a participant message on a
// done annotation reopens it, an agent one does not.
func TestThreadAndStatus(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p, doc := annotated(t, s, "Ship it.")
	a, err := s.CreateAnnotation(ctx, "a", doc, 1, text("Ship", "", " it."), say(p, "when?"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	status := func() string {
		t.Helper()
		list, err := s.Annotations(ctx, "a", doc)
		if err != nil {
			t.Fatal(err)
		}
		return list[0].Status
	}
	v0, _ := s.AnnotationsVersion(ctx, "a")
	if _, err := s.AddAnnotationMessage(ctx, "a", a.ID, agentSays("Monday"), 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAnnotationStatus(ctx, "a", a.ID, StatusDone); err != nil || status() != StatusDone {
		t.Fatalf("done: %v, %s", err, status())
	}
	if _, err := s.AddAnnotationMessage(ctx, "a", a.ID, agentSays("noted"), 1<<20); err != nil || status() != StatusDone {
		t.Fatalf("agent message changed the status: %v, %s", err, status())
	}
	if _, err := s.AddAnnotationMessage(ctx, "a", a.ID, say(p, "not done"), 1<<20); err != nil || status() != StatusReopened {
		t.Fatalf("participant message: %v, %s", err, status())
	}
	if v, _ := s.AnnotationsVersion(ctx, "a"); v == v0 {
		t.Fatal("version did not move")
	}
	list, _ := s.Annotations(ctx, "a", doc)
	var by []string
	for _, m := range list[0].Messages {
		by = append(by, m.By+":"+m.Body)
	}
	if len(by) != 4 || by[1] != "agent:Monday" || by[3] != "participant:not done" {
		t.Fatalf("thread: %v", by)
	}

	seed(t, s, "b")
	if _, err := s.AddAnnotationMessage(ctx, "b", a.ID, agentSays("x"), 1<<20); !errors.Is(err, ErrNotFound) {
		t.Fatalf("message through another room: %v", err)
	}
	if err := s.SetAnnotationStatus(ctx, "b", a.ID, StatusDone); !errors.Is(err, ErrNotFound) {
		t.Fatalf("status through another room: %v", err)
	}
	if err := s.SetAnnotationStatus(ctx, "a", a.ID, "closed"); err == nil {
		t.Fatal("unknown status stored")
	}
}

// 001-AC20: attachments count against the room limit, with uploads.
func TestAttachmentsCountAgainstTheRoom(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p, doc := annotated(t, s, "x")
	used, quota, err := s.RoomUsage(ctx, "a", 1<<20)
	if err != nil || quota != 1<<20 {
		t.Fatal(quota, err)
	}
	limit := used + 10
	a, err := s.CreateAnnotation(ctx, "a", doc, 0, nil, say(p, "pic", "123456"), limit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddAnnotationMessage(ctx, "a", a.ID, say(p, "two more", "123", "45"), limit); !errors.Is(err, ErrRoomFull) {
		t.Fatalf("over the limit: %v", err)
	}
	if _, err := s.AddAnnotationMessage(ctx, "a", a.ID, say(p, "fits", "1234"), limit); err != nil {
		t.Fatalf("exactly at the limit: %v", err)
	}
	if got, _, _ := s.RoomUsage(ctx, "a", limit); got != limit {
		t.Fatalf("room bytes %d, want %d", got, limit)
	}
	if _, err := s.AddUpload(ctx, &Upload{RoomID: "a", Author: "x", Filename: "f.md", ContentType: "text/markdown", Format: "md", Data: []byte("y"), Text: "y"}, limit); !errors.Is(err, ErrRoomFull) {
		t.Fatalf("upload past attachments: %v", err)
	}
	list, _ := s.Annotations(ctx, "a", doc)
	at, err := s.Attachment(ctx, "a", list[0].Messages[0].Attachments[0].ID)
	if err != nil || string(at.Data) != "123456" {
		t.Fatalf("attachment: %+v, %v", at, err)
	}
	seed(t, s, "b")
	if _, err := s.Attachment(ctx, "b", at.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("attachment through another room: %v", err)
	}
}

// 001-AC21: a new revision re-anchors reworded text on its context, flags removed text orphaned
// while keeping it listed, and un-flags it when a later revision brings the text back.
func TestAddRevisionReanchors(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p, doc := annotated(t, s, "# Plan\n\nThe rollout needs a rollback step before the freeze.\n\nKeep the team informed.\n\nOwners sign off.\n")
	reworded, err := s.CreateAnnotation(ctx, "a", doc, 1, text("rollback step", "Plan The rollout needs a ", " before the freeze. Keep the team"), say(p, "how?"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := s.CreateAnnotation(ctx, "a", doc, 1, text("Owners sign off.", "Keep the team informed. ", ""), say(p, "who?"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	whole, err := s.CreateAnnotation(ctx, "a", doc, 1, nil, say(p, "overall"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	v0, _ := s.AnnotationsVersion(ctx, "a")
	if _, err := s.AddRevision(ctx, "a", doc, "# Plan\n\nThe rollout needs a tested way back before the freeze.\n\nKeep the team informed.\n", "agent"); err != nil {
		t.Fatal(err)
	}
	byID := func() map[int64]Annotation {
		t.Helper()
		list, err := s.Annotations(ctx, "a", doc)
		if err != nil {
			t.Fatal(err)
		}
		out := map[int64]Annotation{}
		for _, a := range list {
			out[a.ID] = a
		}
		return out
	}
	got := byID()
	if len(got) != 3 {
		t.Fatalf("an annotation went missing: %d left", len(got))
	}
	if a := got[reworded.ID]; a.Orphaned || a.Revision != 2 || a.Anchor.Exact != "tested way back" {
		t.Fatalf("reworded: %+v", a)
	}
	if a := got[removed.ID]; !a.Orphaned || a.Revision != 1 || a.Anchor.Exact != "Owners sign off." {
		t.Fatalf("removed: %+v %+v", a, a.Anchor)
	}
	if a := got[whole.ID]; a.Orphaned || a.Anchor != nil {
		t.Fatalf("whole document: %+v", a)
	}
	if v, _ := s.AnnotationsVersion(ctx, "a"); v == v0 {
		t.Fatal("re-anchoring did not move the version: live views would not redraw")
	}

	if _, err := s.AddRevision(ctx, "a", doc, "# Plan\n\nThe rollout needs a tested way back before the freeze.\n\nKeep the team informed. Owners sign off.\n", "agent"); err != nil {
		t.Fatal(err)
	}
	if a := byID()[removed.ID]; a.Orphaned || a.Revision != 3 {
		t.Fatalf("text back in r3: %+v", a)
	}
}

func TestAddRevisionReanchorsHTMLElements(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p := seed(t, s, "a")
	d, err := s.CreateAgentDocument(ctx, "a", "page.html", "html", "<ul><li>Freeze</li><li>Ship</li></ul>", "agent")
	if err != nil {
		t.Fatal(err)
	}
	li := &anchor.Anchor{Kind: anchor.KindElement, Path: "body > ul:nth-of-type(1) > li:nth-of-type(2)", Tag: "li", Fingerprint: "Ship"}
	if _, err := s.CreateAnnotation(ctx, "a", d.ID, 1, li, say(p, "flag?"), 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddRevision(ctx, "a", d.ID, "<ul><li>Back up</li><li>Freeze</li><li>Ship</li></ul>", "agent"); err != nil {
		t.Fatal(err)
	}
	list, _ := s.Annotations(ctx, "a", d.ID)
	if a := list[0]; a.Orphaned || a.Anchor.Path != "body > ul:nth-of-type(1) > li:nth-of-type(3)" {
		t.Fatalf("element after re-render: %+v %+v", a, a.Anchor)
	}
}

// The agent's read: annotations with a message past the cursor, whole threads, image bytes.
func TestAnnotationsSince(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p, doc := annotated(t, s, "Ship it.")
	first, cursor, err := s.AnnotationsSince(ctx, "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	// seed's own annotation, with its image.
	if len(first) != 1 || string(first[0].Messages[0].Attachments[0].Data) != "png" || cursor == 0 {
		t.Fatalf("from 0: %+v, cursor %d", first, cursor)
	}
	if again, c, _ := s.AnnotationsSince(ctx, "a", cursor); len(again) != 0 || c != cursor {
		t.Fatalf("nothing new: %d annotations, cursor %d -> %d", len(again), cursor, c)
	}

	a, err := s.CreateAnnotation(ctx, "a", doc, 1, text("Ship", "", " it."), say(p, "when?", "img1"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	mid, c2, err := s.AnnotationsSince(ctx, "a", cursor)
	if err != nil || len(mid) != 1 || mid[0].ID != a.ID || c2 <= cursor {
		t.Fatalf("after one: %+v, %d, %v", mid, c2, err)
	}
	if _, err := s.AddAnnotationMessage(ctx, "a", a.ID, say(p, "and see this", "img2"), 1<<20); err != nil {
		t.Fatal(err)
	}
	got, c3, err := s.AnnotationsSince(ctx, "a", c2)
	if err != nil || len(got) != 1 || c3 <= c2 {
		t.Fatalf("after reply: %+v, %d, %v", got, c3, err)
	}
	msgs := got[0].Messages
	if len(msgs) != 2 || msgs[0].ID > c2 || msgs[1].ID <= c2 {
		t.Fatalf("whole thread, the reply past the cursor: %+v", msgs)
	}
	if string(msgs[0].Attachments[0].Data) != "img1" || string(msgs[1].Attachments[0].Data) != "img2" {
		t.Fatal("attachment data missing")
	}

	seed(t, s, "b")
	if other, _, _ := s.AnnotationsSince(ctx, "b", 0); len(other) != 1 || other[0].RoomID != "b" {
		t.Fatalf("room b reads: %+v", other)
	}
}
