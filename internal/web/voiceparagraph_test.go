package web

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/store"
)

// paragraphOptions are voicePodOptions with voice paragraphs on, at the default gap.
func paragraphOptions(t *testing.T) Options {
	t.Helper()
	opts := voicePodOptions(t)
	opts.VoiceParagraphGap = DefaultVoiceParagraphGap
	return opts
}

// 018-AC4, 018-AC5, 018-AC9: two lines 1 s apart make one message, redrawn on the stream as an
// edit; another speaker's line closes it, and the first speaker's next line starts a new one.
func TestVoiceParagraph(t *testing.T) {
	e := newEnvOpts(t, paragraphOptions(t))
	room := e.createRoom(t, "R").ID
	alice := e.join(t, e.createInvite(t, room).Token, "Alice")
	bob := e.join(t, e.createInvite(t, room).Token, "Bob")
	ch := e.chatEvents(t, room, bob.Token, "")
	line := func(pid int64, body string, gap any) store.Message {
		t.Helper()
		code, b := e.do(t, "POST", "/api/rooms/"+room+"/voice/lines", sttToken, map[string]any{"participant_id": pid, "body": body, "gap_s": gap})
		if code != http.StatusCreated {
			t.Fatalf("%q: %d %s", body, code, b)
		}
		return decodeJSON[store.Message](t, b)
	}
	for m := nextMessage(t, ch); m.Kind != store.KindSystem || m.Sender != "Bob"; m = nextMessage(t, ch) {
	}

	first := line(alice.Participant.ID, "We ship", nil)
	if m := nextMessage(t, ch); m.ID != first.ID {
		t.Fatalf("stream: %+v", m)
	}
	joined := line(alice.Participant.ID, "on Friday.", 1)
	if joined.ID != first.ID || joined.Body != "We ship on Friday." || joined.EditedAt != nil || joined.AppendedAt == nil {
		t.Fatalf("append answered %+v", joined)
	}
	if m := nextEdit(t, ch); m.ID != first.ID || m.Body != "We ship on Friday." || m.EditedAt != nil || m.ChangeSeq != joined.ChangeSeq {
		t.Fatalf("stream edit: %+v", m)
	}
	closer := line(bob.Participant.ID, "Fine by me.", 0.5)
	if m := nextMessage(t, ch); m.ID != closer.ID || closer.ID == first.ID {
		t.Fatalf("another speaker: %+v, stream %+v", closer, m)
	}
	again := line(alice.Participant.ID, "And the docs.", 0.5)
	if m := nextMessage(t, ch); m.ID != again.ID || again.ID == first.ID || again.Body != "And the docs." {
		t.Fatalf("after another speaker: %+v, stream %+v", again, m)
	}
	if n := e.srv.VoiceLinesAppended(store.VoiceSourceServer); n != 1 {
		t.Errorf("appended{server} %d, want 1", n)
	}
	if n := e.srv.VoiceLinesAppended(store.VoiceSourceDevice); n != 0 {
		t.Errorf("appended{device} %d, want 0", n)
	}
	if n := e.srv.VoiceLines(store.VoiceSourceServer); n != 4 {
		t.Errorf("lines{server} %d, want 4", n)
	}
	var n int
	if err := e.sqlDB(t).QueryRowContext(t.Context(), `SELECT count(*) FROM messages WHERE room_id = $1 AND kind = 'voice'`, room).Scan(&n); err != nil || n != 3 {
		t.Fatalf("%d voice messages, want 3: %v", n, err)
	}
}

// 018-AC6: the other source's copy of a paragraph's last or middle line is still a repeat,
// whichever source grew the paragraph, and a device line already stored stops the server's
// paragraph from growing by the same utterance.
func TestVoiceParagraphDedupe(t *testing.T) {
	e := newEnvOpts(t, paragraphOptions(t))
	paragraphDedupe(t, e, e, e.clock())
}

// 017-AC15, 018-AC6: the same, the device lines on one pod and the worker's on the other.
func TestTwoPodsVoiceParagraphDedupe(t *testing.T) {
	a, b := newPair(t, paragraphOptions(t))
	clk := a.clock()
	b.srv.now = clk.now
	paragraphDedupe(t, a, b, clk)
}

func paragraphDedupe(t *testing.T, dev, srv *env, clk *clock) {
	t.Helper()
	room := dev.createRoom(t, "R").ID
	alice := dev.join(t, dev.createInvite(t, room).Token, "Alice")
	device := func(body string, gap any) (int, []byte) {
		return dev.do(t, "POST", "/api/rooms/"+room+"/voice/device-lines", alice.Token, map[string]any{"body": body, "gap_s": gap})
	}
	server := func(body string, gap any) (int, []byte) {
		return srv.do(t, "POST", "/api/rooms/"+room+"/voice/lines", sttToken, map[string]any{"participant_id": alice.Participant.ID, "body": body, "gap_s": gap})
	}
	type post func(body string, gap any) (int, []byte)
	type step struct {
		post post
		body string
		want int
	}
	const a, b, c = "The rollout starts on Monday morning", "then we watch the error budget", "and we close the ticket on Friday"
	created, deduped := http.StatusCreated, http.StatusOK
	cases := []struct {
		name  string
		steps []step
	}{
		{"server paragraph, then the device copy of its last line", []step{{server, a, created}, {server, b, created}, {device, b, deduped}}},
		{"device paragraph, then the server copy of its last line", []step{{device, a, created}, {device, b, created}, {server, b, deduped}}},
		{"server paragraph, then the device copy of a middle line", []step{{server, a, created}, {server, b, created}, {server, c, created}, {device, b, deduped}}},
		{"device paragraph, then the server copy of a middle line", []step{{device, a, created}, {device, b, created}, {device, c, created}, {server, b, deduped}}},
		// The device's copy lands first, as its own message: the worker's must not join the paragraph.
		{"device copy first", []step{{server, a, created}, {device, b, created}, {server, b, deduped}}},
	}
	for _, cs := range cases {
		clk.add(time.Minute)
		for i, st := range cs.steps {
			clk.add(time.Second)
			if code, body := st.post(st.body, 1); code != st.want {
				t.Fatalf("%s, line %d: %d %s", cs.name, i, code, body)
			}
		}
	}
	rows, err := dev.sqlDB(t).QueryContext(t.Context(), `SELECT body FROM messages WHERE room_id = $1 AND kind = 'voice' ORDER BY id`, room)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		got = append(got, b)
	}
	want := []string{a + " " + b, a + " " + b, a + " " + b + " " + c, a + " " + b + " " + c, a, b}
	if len(got) != len(want) {
		t.Fatalf("voice messages %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("voice messages %q, want %q", got, want)
		}
	}
}

// 018-AC7: "every 2 voice lines" counts closed paragraphs: not one still open, even past the gap;
// one closed by the close delay fires on the tick, once, even with a voice line checking at the same time;
// one closed by another speaker's line fires on that line; one closed by a text message fires on
// the next tick. A turn starts the count again.
func TestVoiceParagraphTrigger(t *testing.T) {
	e := newEnvOpts(t, paragraphOptions(t))
	c := e.clock()
	c.set(time.Now())
	r := newTurnRoom(t, e)
	alice := r.alice
	tok := alice.p.Token
	bob := e.join(t, e.createInvite(t, r.room).Token, "Bob")
	e.openEvents(t, r.room, tok, "")
	e.waitPresent(t, alice.p.Participant.ID)
	sec := e.secretary(t, r, tok, map[string]any{"every_voice": 2})
	line := func(pid int64, body string, gap any) store.Message {
		t.Helper()
		code, b := e.do(t, "POST", "/api/rooms/"+r.room+"/voice/lines", sttToken, map[string]any{"participant_id": pid, "body": body, "gap_s": gap})
		if code != http.StatusCreated {
			t.Fatalf("%q: %d %s", body, code, b)
		}
		return decodeJSON[store.Message](t, b)
	}
	busy := func() int {
		st := e.agent(t, r.room, tok)
		return len(st.Queue) + len(st.Running)
	}
	fires := func(what string) {
		t.Helper()
		if n := busy(); n != 1 {
			t.Fatalf("%s: %d turns queued or running, want 1", what, n)
		}
		tn := e.nextTurn(t, r, alice)
		if tn.PersonaID != sec.ID || tn.Cause != "every 2 voice lines" {
			t.Fatalf("%s: turn %+v", what, tn)
		}
		e.finish(t, r, alice, tn, "Noted.")
		e.srv.voiceTick(t.Context())
		if n := busy(); n != 0 {
			t.Fatalf("%s: fired again after its turn: %d", what, n)
		}
	}
	quiet := func(what string) {
		t.Helper()
		if n := busy(); n != 0 {
			t.Fatalf("%s: fired on an open paragraph: %d", what, n)
		}
	}
	a := alice.p.Participant.ID

	line(a, "zero", nil)
	c.add(time.Second)
	p := line(a, "one", nil)
	c.add(time.Second)
	if q := line(a, "two", 1); q.ID != p.ID {
		t.Fatalf("not appended: %+v", q)
	}
	quiet("2 lines in one paragraph")
	c.add(DefaultVoiceParagraphGap + time.Second)
	e.srv.voiceTick(t.Context())
	quiet("tick past the gap, a monologue's next line may still join")
	c.add(DefaultVoiceParagraphClose - DefaultVoiceParagraphGap - time.Second)
	e.srv.voiceTick(t.Context())
	if busy() != 1 {
		t.Fatal("close delay passed: the tick did not fire")
	}
	e.srv.voiceTriggers(t.Context(), r.room)
	e.srv.voiceTick(t.Context())
	fires("close delay passed")

	line(a, "three", nil)
	c.add(time.Second)
	line(a, "four", 1)
	c.add(500 * time.Millisecond)
	line(bob.Participant.ID, "five", nil)
	quiet("another speaker's first line")
	c.add(500 * time.Millisecond)
	line(bob.Participant.ID, "six", 0.5)
	quiet("another speaker's paragraph")
	c.add(500 * time.Millisecond)
	line(a, "seven", nil)
	fires("another speaker's line")

	line(a, "eight", nil)
	c.add(time.Second)
	line(a, "nine", nil)
	c.add(time.Second)
	line(a, "ten", 1)
	e.postBody(t, r, tok, "a text line")
	quiet("text message, before the tick")
	e.srv.voiceTick(t.Context())
	fires("text message, on the tick")
}

// 018-AC7, 006-AC18: with paragraphs off every voice line is its own message and counts as soon as
// it is stored, as before 018; the tick adds nothing.
func TestVoiceTriggerParagraphsOff(t *testing.T) {
	e := newEnvOpts(t, voicePodOptions(t))
	c := e.clock()
	c.set(time.Now())
	r := newTurnRoom(t, e)
	alice := r.alice
	tok := alice.p.Token
	e.openEvents(t, r.room, tok, "")
	e.waitPresent(t, alice.p.Participant.ID)
	sec := e.secretary(t, r, tok, map[string]any{"every_voice": 2})
	busy := func() int {
		st := e.agent(t, r.room, tok)
		return len(st.Queue) + len(st.Running)
	}
	for i, body := range []string{"one", "two"} {
		if i > 0 && busy() != 0 {
			t.Fatal("fired after one line")
		}
		code, b := e.do(t, "POST", "/api/rooms/"+r.room+"/voice/lines", sttToken, map[string]any{"participant_id": alice.p.Participant.ID, "body": body, "gap_s": 1})
		if code != http.StatusCreated {
			t.Fatalf("%q: %d %s", body, code, b)
		}
	}
	e.srv.voiceTick(t.Context())
	if n := busy(); n != 1 {
		t.Fatalf("%d turns queued or running, want 1", n)
	}
	if tn := e.nextTurn(t, r, alice); tn.PersonaID != sec.ID {
		t.Fatalf("turn %+v", tn)
	}
}

// 018-AC7: every voice tick re-checks each room where someone is here, so a paragraph closed by
// the close delay fires on the next tick whenever it comes: 49 s after the previous one (a leader
// change, a slow tick), as a new leader's first tick, or once someone is back after an absence.
// Nobody here, it does not fire.
func TestVoiceTickRechecksPresentRooms(t *testing.T) {
	old := presenceGrace
	presenceGrace = 200 * time.Millisecond
	t.Cleanup(func() { presenceGrace = old })
	type step struct {
		at   time.Duration // the tick's clock, after now
		here bool
		want bool // fired by this tick
	}
	for _, c := range []struct {
		name    string
		lineAgo time.Duration
		steps   []step
	}{
		{"ticks 49 s apart", 10 * time.Second, []step{{0, true, false}, {49 * time.Second, true, true}}},
		{"a new leader's first tick", 5 * time.Minute, []step{{0, true, true}}},
		{"absent while due, back later", 0, []step{{0, true, false}, {25 * time.Second, false, false},
			{115 * time.Second, false, false}, {130 * time.Second, true, true}}},
		{"nobody here", 0, []step{{25 * time.Second, false, false}, {5 * time.Minute, false, false}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnvOpts(t, paragraphOptions(t))
			clk := e.clock()
			now := time.Now()
			clk.set(now.Add(-c.lineAgo))
			r := newTurnRoom(t, e)
			tok := r.alice.p.Token
			pid := r.alice.p.Participant.ID
			sec := e.secretary(t, r, tok, map[string]any{"every_voice": 1})
			if code, b := e.do(t, "POST", "/api/rooms/"+r.room+"/voice/lines", sttToken, map[string]any{"participant_id": pid, "body": "one", "gap_s": 1}); code != http.StatusCreated {
				t.Fatalf("voice line: %d %s", code, b)
			}
			busy := func() int {
				st := e.agent(t, r.room, tok)
				return len(st.Queue) + len(st.Running)
			}
			var leave context.CancelFunc
			fired := false
			for i, st := range c.steps {
				switch {
				case st.here && leave == nil:
					_, leave = e.openStream(t, "/api/rooms/"+r.room+"/events", tok, "")
					e.waitPresent(t, pid)
				case !st.here && leave != nil:
					leave()
					leave = nil
				}
				for n := 0; !st.here && e.srv.people.anyoneHere(r.room, e.srv.lease()); n++ {
					if n > 250 {
						t.Fatal("still here")
					}
					time.Sleep(20 * time.Millisecond) // polls past the 200 ms grace, bounded by 250 rounds
				}
				clk.set(now.Add(st.at))
				e.srv.voiceTick(t.Context())
				if got := busy() == 1; got != st.want {
					t.Fatalf("tick %d at +%s, here %v: fired %v, want %v", i, st.at, st.here, got, st.want)
				}
				fired = fired || st.want
			}
			if !fired {
				return
			}
			if tn := e.nextTurn(t, r, r.alice); tn.PersonaID != sec.ID {
				t.Fatalf("turn %+v", tn)
			}
		})
	}
}
