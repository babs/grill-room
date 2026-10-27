package store

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"
)

// 018-AC7, 020-AC2: "every 2 voice lines" counts a closed line and a paragraph of two appended
// lines as 2 only once the paragraph is closed: by a message other than a typed one after it,
// deleted or not, or by the close delay passing since its last line, not the paragraph gap. With
// gap 0 every voice message counts, as before 018 (006-AC18).
func TestDueVoicePersonasCountsClosedParagraphs(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	const gap, closeAfter = 2500 * time.Millisecond, 20 * time.Second
	t0 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	last := t0.Add(time.Second)
	cases := []struct {
		name  string
		after func(t *testing.T, room string, alice, bob *Participant)
		now   time.Time
		gap   time.Duration
		due   bool
	}{
		// A monologue's next line may still join it.
		{name: "open, past the gap", now: last.Add(3 * time.Second), gap: gap},
		{name: "open, just before the close delay", now: last.Add(closeAfter - time.Millisecond), gap: gap},
		{name: "close delay passed", now: last.Add(closeAfter + time.Second), gap: gap, due: true},
		{name: "another speaker's line", now: last.Add(time.Second), gap: gap, due: true,
			after: func(t *testing.T, room string, _, bob *Participant) {
				voiceLine(t, s, room, bob, "fine by me", nil, last.Add(time.Second))
			}},
		// 020-AC2: typed messages, deleted or not, close nothing; the close delay still does.
		{name: "a text message", now: last.Add(time.Second), gap: gap,
			after: func(t *testing.T, room string, alice, _ *Participant) { chat(t, s, room, alice, "ok") }},
		{name: "two text messages", now: last.Add(time.Second), gap: gap,
			after: func(t *testing.T, room string, alice, bob *Participant) {
				chat(t, s, room, alice, "ok")
				chat(t, s, room, bob, "sure")
			}},
		{name: "a deleted text message", now: last.Add(time.Second), gap: gap,
			after: func(t *testing.T, room string, _, bob *Participant) {
				m := chat(t, s, room, bob, "oops")
				if _, err := s.DeleteMessage(ctx, room, m.ID, bob.ID); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "a text message, then the close delay", now: last.Add(closeAfter + time.Second), gap: gap, due: true,
			after: func(t *testing.T, room string, alice, _ *Participant) { chat(t, s, room, alice, "ok") }},
		{name: "a room event", now: last.Add(time.Second), gap: gap, due: true,
			after: func(t *testing.T, room string, _, _ *Participant) {
				if err := s.InsertMessage(ctx, &Message{RoomID: room, Kind: KindSystem, Sender: "admin", Body: "renamed"}); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "a deleted voice line", now: last.Add(time.Second), gap: gap, due: true,
			after: func(t *testing.T, room string, _, bob *Participant) {
				m := voiceLine(t, s, room, bob, "oops", nil, last.Add(time.Second))
				if _, err := s.DeleteMessage(ctx, room, m.ID, bob.ID); err != nil {
					t.Fatal(err)
				}
			}},
		// Another pod's clock may run behind the one that stamped the line.
		{name: "gap 0, clock behind the line", now: t0, due: true},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			room := "v" + strconv.Itoa(i)
			alice := seed(t, s, room)
			if _, err := s.CreateInvite(ctx, room, "inv-bob-"+room, 0); err != nil {
				t.Fatal(err)
			}
			bob, _, err := s.Join(ctx, "inv-bob-"+room, "Bob", "tok-bob-"+room, JoinProfile{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE room_personas SET every_voice = 2,
				mark_msg_id = (SELECT MAX(id) FROM messages) WHERE room_id = ?`, room); err != nil {
				t.Fatal(err)
			}
			one := 1.0
			voiceLine(t, s, room, alice, "zero", nil, t0.Add(-time.Minute))
			p := voiceLine(t, s, room, alice, "one", nil, t0)
			if q := voiceLine(t, s, room, alice, "two", &one, last); q.ID != p.ID {
				t.Fatalf("not appended: %+v", q)
			}
			if c.after != nil {
				c.after(t, room, alice, bob)
			}
			vc := VoiceCount{Gap: c.gap, Close: closeAfter}
			due, err := s.DueVoicePersonas(ctx, c.now, vc, []string{room})
			if err != nil {
				t.Fatal(err)
			}
			if (len(due) == 1) != c.due {
				t.Fatalf("due %+v, want %v", due, c.due)
			}
		})
	}
}

// 018-AC7: the voice tick asks for the rooms where someone is here; the count reads those rooms
// only, each due persona once.
func TestDueVoicePersonasOnlyTheGivenRooms(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	t0 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	for _, room := range []string{"a", "b", "c"} {
		alice := seed(t, s, room)
		if _, err := s.db.ExecContext(ctx, `UPDATE room_personas SET every_voice = 1,
			mark_msg_id = (SELECT MAX(id) FROM messages) WHERE room_id = ?`, room); err != nil {
			t.Fatal(err)
		}
		voiceLine(t, s, room, alice, "one", nil, t0)
	}
	vc := VoiceCount{Gap: 2500 * time.Millisecond, Close: 20 * time.Second}
	for _, c := range []struct {
		rooms, want []string
	}{
		{[]string{"a", "c"}, []string{"a", "c"}},
		{[]string{"b"}, []string{"b"}},
		{[]string{"b", "nowhere"}, []string{"b"}},
		{nil, nil},
	} {
		due, err := s.DueVoicePersonas(ctx, t0.Add(time.Minute), vc, c.rooms)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, p := range due {
			got = append(got, p.RoomID)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("rooms %q: due in %q, want %q", c.rooms, got, c.want)
		}
	}
}

// voiceLine posts p's line at at, gap seconds after their previous one, paragraphs on.
func voiceLine(t *testing.T, s *Store, room string, p *Participant, body string, gap *float64, at time.Time) *Message {
	t.Helper()
	m := &Message{RoomID: room, Kind: KindVoice, ParticipantID: p.ID, Sender: p.Name, Body: body, Source: VoiceSourceServer}
	join := VoiceJoin{GapS: gap, MaxGap: 2500 * time.Millisecond, MaxChars: 1200}
	if _, err := s.PostVoiceLine(context.Background(), m, at, 5*time.Second, join, func([]string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	return m
}
