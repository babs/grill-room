package web

import (
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
