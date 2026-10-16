package store

import (
	"context"
	"errors"
	"testing"
)

// twoSpeakers seeds room "r" with Alice and Bob, and returns them with a text line and a voice
// line by Alice, and a system line.
func twoSpeakers(t *testing.T, s *Store) (alice, bob *Participant, text, voice, system *Message) {
	t.Helper()
	ctx := context.Background()
	alice = seed(t, s, "r")
	if _, err := s.CreateInvite(ctx, "r", "inv-bob", 0); err != nil {
		t.Fatal(err)
	}
	bob, system, err := s.Join(ctx, "inv-bob", "Bob", "tok-bob", JoinProfile{})
	if err != nil {
		t.Fatal(err)
	}
	text = &Message{RoomID: "r", Kind: KindText, ParticipantID: alice.ID, Sender: "Alice", Body: "first"}
	voice = &Message{RoomID: "r", Kind: KindVoice, Source: VoiceSourceServer, ParticipantID: alice.ID, Sender: "Alice", Body: "wreck a nice beach"}
	for _, m := range []*Message{text, voice} {
		if err := s.InsertMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	return alice, bob, text, voice, system
}

// 002-AC9 002-AC10 002-AC11
func TestEditAndDeleteRules(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	alice, bob, text, voice, system := twoSpeakers(t, s)

	for name, tc := range map[string]struct {
		id, by int64
		want   error
	}{
		"someone else's text":  {text.ID, bob.ID, ErrForbidden},
		"someone else's voice": {voice.ID, bob.ID, ErrForbidden},
		"a system line":        {system.ID, bob.ID, ErrForbidden},
		"another room's id":    {text.ID + 1000, alice.ID, ErrNotFound},
	} {
		if _, _, err := s.EditMessage(ctx, "r", tc.id, tc.by, "x"); !errors.Is(err, tc.want) {
			t.Errorf("edit %s: got %v, want %v", name, err, tc.want)
		}
		if _, err := s.DeleteMessage(ctx, "r", tc.id, tc.by); !errors.Is(err, tc.want) {
			t.Errorf("delete %s: got %v, want %v", name, err, tc.want)
		}
	}
	if _, _, err := s.EditMessage(ctx, "other", text.ID, alice.ID, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("edit through another room: got %v", err)
	}

	if voice.HTML != "<p>wreck a nice beach</p>\n" || system.HTML != "" {
		t.Fatalf("rendered on write, system lines excepted: %q %q", voice.HTML, system.HTML)
	}
	m, _, err := s.EditMessage(ctx, "r", voice.ID, alice.ID, "recognize *speech*")
	if err != nil || m.Body != "recognize *speech*" || m.EditedAt == nil || m.ChangeSeq == 0 {
		t.Fatalf("voice correction: %+v %v", m, err)
	}
	if got, err := s.Message(ctx, "r", voice.ID); err != nil || got.HTML != "<p>recognize <em>speech</em></p>\n" {
		t.Fatalf("edit re-renders: %+v %v", got, err)
	}
	if again, _, err := s.EditMessage(ctx, "r", voice.ID, alice.ID, "recognize *speech*"); err != nil || again.ChangeSeq != m.ChangeSeq {
		t.Fatalf("same body must change nothing: %+v %v", again, err)
	}
	if _, _, err := s.EditMessage(ctx, "r", voice.ID, alice.ID, "recognise speech"); err != nil {
		t.Fatal(err)
	}
	vs, err := s.MessageVersions(ctx, "r", voice.ID)
	if err != nil || len(vs) != 2 || vs[0].Body != "wreck a nice beach" || vs[1].Body != "recognize *speech*" {
		t.Fatalf("versions: %+v %v", vs, err)
	}
	if !vs[0].WrittenAt.Equal(voice.TS) || !vs[1].WrittenAt.Equal(*m.EditedAt) {
		t.Errorf("versions carry when each body was written: %+v", vs)
	}

	d, err := s.DeleteMessage(ctx, "r", text.ID, alice.ID)
	if err != nil || d.DeletedAt == nil || d.Body != "first" {
		t.Fatalf("delete: %+v %v", d, err)
	}
	if again, err := s.DeleteMessage(ctx, "r", text.ID, alice.ID); err != nil || again.ChangeSeq != d.ChangeSeq {
		t.Fatalf("second delete must change nothing: %+v %v", again, err)
	}
	if _, _, err := s.EditMessage(ctx, "r", text.ID, alice.ID, "x"); !errors.Is(err, ErrMessageDeleted) {
		t.Fatalf("edit after delete: %v", err)
	}
	// The body stays in the store.
	got, err := s.Message(ctx, "r", text.ID)
	if err != nil || got.Body != "first" || got.DeletedAt == nil {
		t.Fatalf("deleted message: %+v %v", got, err)
	}
}

// 002-AC9: the change feed the event stream replays.
func TestChangedMessagesAndCursor(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	alice, _, text, voice, _ := twoSpeakers(t, s)
	base, err := s.ChangeCursor(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EditMessage(ctx, "r", voice.ID, alice.ID, "v2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteMessage(ctx, "r", text.ID, alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EditMessage(ctx, "r", voice.ID, alice.ID, "v3"); err != nil {
		t.Fatal(err)
	}
	cur, err := s.ChangeCursor(ctx, "r")
	if err != nil || cur != base+3 {
		t.Fatalf("cursor %d %v, want %d", cur, err, base+3)
	}
	ms, err := s.ChangedMessages(ctx, "r", base, 10)
	if err != nil || len(ms) != 2 || ms[0].ID != text.ID || ms[1].ID != voice.ID || ms[1].Body != "v3" {
		t.Fatalf("changed: %+v %v", ms, err)
	}
	if ms, err := s.ChangedMessages(ctx, "r", cur, 10); err != nil || len(ms) != 0 {
		t.Fatalf("past the cursor: %+v %v", ms, err)
	}
}

// 002-AC12: what a turn saw, and what changed after.
func TestMessageChanges(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	alice, _, text, voice, _ := twoSpeakers(t, s)
	edit := func(id int64, body string) {
		t.Helper()
		if _, _, err := s.EditMessage(ctx, "r", id, alice.ID, body); err != nil {
			t.Fatal(err)
		}
	}
	edit(voice.ID, "seen by the turn")
	seenAt, err := s.ChangeCursor(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	later := &Message{RoomID: "r", Kind: KindText, ParticipantID: alice.ID, Sender: "Alice", Body: "unseen"}
	if err := s.InsertMessage(ctx, later); err != nil {
		t.Fatal(err)
	}
	edit(voice.ID, "after 1")
	edit(voice.ID, "after 2")
	edit(later.ID, "unseen edited")
	if _, err := s.DeleteMessage(ctx, "r", text.ID, alice.ID); err != nil {
		t.Fatal(err)
	}
	upTo, err := s.ChangeCursor(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	edit(voice.ID, "after the next turn's cursor")

	cs, err := s.MessageChanges(ctx, "r", voice.ID, seenAt, upTo)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].ID != text.ID || cs[0].Before != "first" || cs[0].DeletedAt == nil {
		t.Fatalf("a change past upTo waits for the next turn; the later message was never seen: %+v", cs)
	}
	cs, err = s.MessageChanges(ctx, "r", voice.ID, seenAt, upTo+1)
	if err != nil || len(cs) != 2 || cs[1].ID != voice.ID || cs[1].Before != "seen by the turn" || cs[1].Body != "after the next turn's cursor" {
		t.Fatalf("before is the body the turn saw: %+v %v", cs, err)
	}
}
