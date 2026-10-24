package store

import (
	"context"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// 010-AC16, 017-AC15: a voice line the other source said within the window is a repeat; the same
// source saying it again, or the other source past the window, is a new line, stamped with the
// caller's time.
func TestPostVoiceLine(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	p := seed(t, s, "a")
	const window = 5 * time.Second
	t0 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	var seen [][]string
	post := func(source string, at time.Time) bool {
		t.Helper()
		m := &Message{RoomID: "a", Kind: KindVoice, ParticipantID: p.ID, Sender: p.Name, Body: "hello there", Source: source}
		res, err := s.PostVoiceLine(ctx, m, at, window, VoiceJoin{}, func(other []string) bool {
			seen = append(seen, other)
			return slices.Contains(other, "hello there")
		})
		if err != nil {
			t.Fatal(err)
		}
		ok := res == VoiceInserted
		if ok && !m.TS.Equal(at) {
			t.Errorf("stored at %v, want %v", m.TS, at)
		}
		return ok
	}
	if !post(VoiceSourceDevice, t0) {
		t.Fatal("first line not stored")
	}
	if post(VoiceSourceServer, t0.Add(time.Second)) {
		t.Error("the other source within the window: stored, want deduped")
	}
	if !post(VoiceSourceDevice, t0.Add(2*time.Second)) {
		t.Error("the same source again: deduped, want stored")
	}
	if !post(VoiceSourceServer, t0.Add(2*time.Second+window+time.Millisecond)) {
		t.Error("the other source past the window: deduped, want stored")
	}
	if len(seen) != 1 || !slices.Equal(seen[0], []string{"hello there"}) {
		t.Errorf("dup saw %q, want the device line once", seen)
	}
}

// 018-AC4, 018-AC5: a line joins its speaker's latest voice message only while every condition
// holds; each case breaks exactly one and gets a new message.
func TestPostVoiceLineAppends(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	const first, second = "we ship", "on friday"
	exact := len(first + " " + second)
	t0 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	never := func([]string) bool { return false }
	type line struct {
		gap     *float64
		maxGap  time.Duration
		chars   int
		source  string
		speaker int // 0 Alice, 1 Bob
		body    string
	}
	gap := func(v float64) *float64 { return &v }
	ok := line{gap: gap(1), maxGap: 2500 * time.Millisecond, chars: exact, source: VoiceSourceServer, body: second}
	cases := []struct {
		name   string
		then   func(l *line)
		before func(t *testing.T, room string, ps []*Participant, first *Message) (release func())
		append bool
	}{
		{name: "all hold", append: true},
		{name: "gap absent", then: func(l *line) { l.gap = nil }},
		{name: "gap not below the paragraph gap", then: func(l *line) { l.gap = gap(2.5) }},
		{name: "paragraph gap 0", then: func(l *line) { l.gap, l.maxGap = gap(0), 0 }},
		{name: "another speaker", then: func(l *line) { l.speaker = 1 }},
		{name: "another source", then: func(l *line) { l.source = VoiceSourceDevice }},
		{name: "joined body too long", then: func(l *line) { l.chars = exact - 1 }},
		// Characters, not bytes: "été à l'été" is 11 runes and 15 bytes.
		{name: "accented body within the characters", then: func(l *line) {
			l.body = "été à l'été"
			l.chars = utf8.RuneCountInString(first + " " + l.body)
		}, append: true},
		{name: "not the latest message", before: func(t *testing.T, room string, ps []*Participant, _ *Message) func() {
			chat(t, s, room, ps[1], "ok")
			return nil
		}},
		{name: "latest message deleted", before: func(t *testing.T, room string, ps []*Participant, _ *Message) func() {
			m := chat(t, s, room, ps[1], "ok")
			if _, err := s.DeleteMessage(ctx, room, m.ID, ps[1].ID); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
		{name: "paragraph deleted", before: func(t *testing.T, room string, ps []*Participant, f *Message) func() {
			if _, err := s.DeleteMessage(ctx, room, f.ID, ps[0].ID); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
		{name: "paragraph edited", before: func(t *testing.T, room string, ps []*Participant, f *Message) func() {
			if _, _, err := s.EditMessage(ctx, room, f.ID, ps[0].ID, "we shop"); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
		{name: "read by a turn", before: func(t *testing.T, room string, _ []*Participant, f *Message) func() {
			if _, err := s.db.ExecContext(ctx, `INSERT INTO turns(room_id, host, status, fresh, resume, message_cursor, annotation_cursor, prompt, started_at)
				VALUES(?, '', 'done', 0, '', ?, 0, '', ?)`, room, f.ID, now()); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
		{name: "a turn read only the lines before", append: true, before: func(t *testing.T, room string, _ []*Participant, f *Message) func() {
			if _, err := s.db.ExecContext(ctx, `INSERT INTO turns(room_id, host, status, fresh, resume, message_cursor, annotation_cursor, prompt, started_at)
				VALUES(?, '', 'done', 0, '', ?, 0, '', ?)`, room, f.ID-1, now()); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
		{name: "turn being prepared", before: func(t *testing.T, room string, _ []*Participant, _ *Message) func() {
			release, ok, err := s.LockRoom(ctx, LockTurns, room, true)
			if err != nil || !ok {
				t.Fatalf("turn lock: %v %v", ok, err)
			}
			return release
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			room := "p" + strconv.Itoa(i)
			ps := []*Participant{seed(t, s, room)}
			if _, err := s.CreateInvite(ctx, room, "inv-bob-"+room, 0); err != nil {
				t.Fatal(err)
			}
			bob, _, err := s.Join(ctx, "inv-bob-"+room, "Bob", "tok-bob-"+room, JoinProfile{})
			if err != nil {
				t.Fatal(err)
			}
			ps = append(ps, bob)
			post := func(p *Participant, body string, l line, at time.Time) (*Message, VoiceStored) {
				t.Helper()
				m := &Message{RoomID: room, Kind: KindVoice, ParticipantID: p.ID, Sender: p.Name, Body: body, Source: l.source}
				res, err := s.PostVoiceLine(ctx, m, at, 5*time.Second, VoiceJoin{GapS: l.gap, MaxGap: l.maxGap, MaxChars: l.chars}, never)
				if err != nil {
					t.Fatal(err)
				}
				return m, res
			}
			f, res := post(ps[0], first, line{source: VoiceSourceServer}, t0)
			if res != VoiceInserted {
				t.Fatalf("first line: %v", res)
			}
			if c.before != nil {
				if release := c.before(t, room, ps, f); release != nil {
					defer release()
				}
			}
			before, _ := s.Message(ctx, room, f.ID)
			cursor, err := s.ChangeCursor(ctx, room)
			if err != nil || cursor == 0 {
				t.Fatalf("change cursor %d before the line: %v", cursor, err)
			}
			l := ok
			if c.then != nil {
				c.then(&l)
			}
			at := t0.Add(time.Second)
			m, res := post(ps[l.speaker], l.body, l, at)
			if !c.append {
				if res != VoiceInserted || m.ID == f.ID || m.Body != l.body {
					t.Fatalf("got %v, message %d %q; want a new message", res, m.ID, m.Body)
				}
				return
			}
			if res != VoiceAppended {
				t.Fatalf("got %v, want an append", res)
			}
			got, err := s.Message(ctx, room, f.ID)
			if err != nil {
				t.Fatal(err)
			}
			// 018-AC5: same id and ts, joined with one space, no edit, no version, change_seq past
			// every other of the room.
			if m.ID != f.ID || got.Body != first+" "+l.body || !got.TS.Equal(f.TS) || got.EditedAt != nil ||
				got.AppendedAt == nil || !got.AppendedAt.Equal(at) || got.ChangeSeq <= before.ChangeSeq || got.ChangeSeq <= cursor ||
				!strings.Contains(got.HTML, l.body[len(l.body)-3:]) {
				t.Fatalf("paragraph %+v, first %+v", got, f)
			}
			if !reflect.DeepEqual(*m, *got) {
				t.Errorf("returned %+v, stored %+v", *m, *got)
			}
			if vs, _ := s.MessageVersions(ctx, room, f.ID); len(vs) != 0 {
				t.Errorf("versions %+v", vs)
			}
			if seq, _ := s.ChangeCursor(ctx, room); seq != got.ChangeSeq {
				t.Errorf("change cursor %d, paragraph %d", seq, got.ChangeSeq)
			}
		})
	}
}

// 018-AC6: the repeat check reads a paragraph's last append time, not its first line's: the other
// source's copy of the latest line still arrives within the window.
func TestPostVoiceLineWindowFromLastAppend(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	p := seed(t, s, "a")
	t0 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	gap := 1.0
	join := VoiceJoin{GapS: &gap, MaxGap: 2 * time.Second, MaxChars: 1200}
	for i, b := range []string{"one", "two", "three", "four"} {
		m := &Message{RoomID: "a", Kind: KindVoice, ParticipantID: p.ID, Sender: p.Name, Body: b, Source: VoiceSourceServer}
		if _, err := s.PostVoiceLine(ctx, m, t0.Add(time.Duration(i)*3*time.Second), 5*time.Second, join, nil); err != nil {
			t.Fatal(err)
		}
	}
	var seen []string
	m := &Message{RoomID: "a", Kind: KindVoice, ParticipantID: p.ID, Sender: p.Name, Body: "four", Source: VoiceSourceDevice}
	res, err := s.PostVoiceLine(ctx, m, t0.Add(10*time.Second), 5*time.Second, VoiceJoin{}, func(o []string) bool { seen = o; return true })
	if err != nil || res != VoiceDeduped || !slices.Equal(seen, []string{"one two three four"}) {
		t.Fatalf("got %v %v, the check saw %q", res, err, seen)
	}
}

func chat(t *testing.T, s *Store, room string, p *Participant, body string) *Message {
	t.Helper()
	m := &Message{RoomID: room, Kind: KindText, ParticipantID: p.ID, Sender: p.Name, Body: body}
	if err := s.InsertMessage(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	return m
}

// 018-AC4: an imported room's resume turns keep the latest turn holding the highest cursor, so a
// line never joins a message an imported persona read, whatever order the personas come in.
func TestImportedTurnsKeepTheParagraphRead(t *testing.T) {
	ctx := t.Context()
	s := openTest(t)
	alice, grill := richRoom(t, s, "orig")
	second, _, err := s.AddPersona(ctx, "orig", templateByPreset(t, s, "secretary").ID, alice)
	if err != nil {
		t.Fatal(err)
	}
	v := &Message{RoomID: "orig", Kind: KindVoice, ParticipantID: alice.ID, Sender: alice.Name, Body: "we ship", Source: VoiceSourceServer}
	if err := s.InsertMessage(ctx, v); err != nil {
		t.Fatal(err)
	}
	// The first persona read the voice line, the second one, added later, read nothing yet.
	if _, err := s.db.ExecContext(ctx, `UPDATE turns SET message_cursor = ? WHERE persona_id = ?`, v.ID, grill.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO turns(room_id, host, status, fresh, resume, session_id, message_cursor, annotation_cursor, prompt,
		started_at, ended_at, persona_id, persona_name) VALUES('orig', '', 'done', 0, '', ?, 0, 0, '', ?, ?, ?, ?)`,
		"00000000-0000-4000-8000-000000000002", now(), now(), second.ID, second.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportRoom(ctx, export(t, s, "orig"), importOpts("copy")); err != nil {
		t.Fatal(err)
	}
	msgs, _ := s.Messages(ctx, "copy", 0, 1000)
	last := msgs[len(msgs)-1]
	if last.Body != "we ship" {
		t.Fatalf("the copy ends with %+v", last)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM turns WHERE room_id = 'copy'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("%d resume turns in the copy: %v", n, err)
	}
	gap := 1.0
	m := &Message{RoomID: "copy", Kind: KindVoice, ParticipantID: last.ParticipantID, Sender: last.Sender, Body: "on friday", Source: VoiceSourceServer}
	res, err := s.PostVoiceLine(ctx, m, time.Now(), time.Second, VoiceJoin{GapS: &gap, MaxGap: 2 * time.Second, MaxChars: 1200}, nil)
	if err != nil || res != VoiceInserted {
		t.Fatalf("got %v %v, want a new message: an imported persona read the line", res, err)
	}
}
