package web

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/store"
)

// newDeviceEnv is a voice server that offers device transcription.
func newDeviceEnv(t *testing.T) *env {
	t.Helper()
	dir, m := modelDir(t)
	e, _ := newModelEnv(t, dir, m)
	return e
}

// sttBeat sends a worker heartbeat for rooms and returns the device map it answers.
func (e *env) sttBeat(t *testing.T, rooms ...string) map[string][]int64 {
	t.Helper()
	code, b := e.do(t, "POST", "/api/stt/heartbeat", sttToken, map[string]any{"worker": "w1", "rooms": rooms})
	if code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", code, b)
	}
	var resp struct {
		Device map[string][]int64 `json:"device"`
	}
	if err := json.Unmarshal(b, &resp); err != nil || resp.Device == nil {
		t.Fatalf("heartbeat answer %s: %v", b, err)
	}
	return resp.Device
}

// presence posts body as tok's presence beat in room and fails on anything but 204.
func (e *env) presence(t *testing.T, room, tok string, body map[string]any) {
	t.Helper()
	if code, b := e.do(t, "POST", "/api/rooms/"+room+"/presence", tok, body); code != http.StatusNoContent {
		t.Fatalf("presence %v: %d %s", body, code, b)
	}
}

// 010-AC13, 010-AC15: a device beat flags its participant; the flag holds for deviceLapse after the
// last one, not a moment more; an announced stop (device_stt false, or the tab gone) drops it at
// once, while another tab still beating keeps it. The worker's heartbeat answer carries the flags of
// its own rooms only.
func TestDeviceFlagHandover(t *testing.T) {
	e := newDeviceEnv(t)
	clk := e.clock()
	room := e.createRoom(t, "R").ID
	other := e.createRoom(t, "Other").ID
	alice := e.join(t, e.createInvite(t, room).Token, "Alice")
	bob := e.join(t, e.createInvite(t, room).Token, "Bob")
	eve := e.join(t, e.createInvite(t, other).Token, "Eve")
	a, b := alice.Participant.ID, bob.Participant.ID
	expect := func(what string, want map[string][]int64, rooms ...string) {
		t.Helper()
		if got := e.sttBeat(t, rooms...); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: %v, want %v", what, got, want)
		}
	}
	expect("nobody flagged", map[string][]int64{}, room, other)

	on := map[string]any{"tab": "a1", "device_stt": true}
	e.presence(t, room, alice.Token, on)
	e.presence(t, other, eve.Token, map[string]any{"tab": "e1", "device_stt": true})
	expect("worker in room only", map[string][]int64{room: {a}}, room)
	expect("worker in both rooms", map[string][]int64{room: {a}, other: {eve.Participant.ID}}, room, other)
	expect("worker in no room", map[string][]int64{}, "nowhere")

	// Silent stop: no more beats.
	clk.add(deviceLapse - 100*time.Millisecond)
	expect("2.9 s after the last beat", map[string][]int64{room: {a}}, room)
	clk.add(100 * time.Millisecond)
	expect("3 s after the last beat", map[string][]int64{}, room)

	// Announced stop by the device worker.
	e.presence(t, room, alice.Token, on)
	e.presence(t, room, bob.Token, map[string]any{"tab": "b1", "device_stt": true})
	expect("two flagged", map[string][]int64{room: {a, b}}, room)
	e.presence(t, room, alice.Token, map[string]any{"tab": "a1", "device_stt": false})
	expect("Alice stopped", map[string][]int64{room: {b}}, room)

	// Announced stop by the page closing.
	e.presence(t, room, bob.Token, map[string]any{"tab": "b1", "gone": true})
	expect("Bob's tab gone", map[string][]int64{}, room)

	// Two tabs: stopping one leaves the other's flag.
	e.presence(t, room, alice.Token, on)
	e.presence(t, room, alice.Token, map[string]any{"tab": "a2", "device_stt": true})
	e.presence(t, room, alice.Token, map[string]any{"tab": "a2", "gone": true})
	expect("one of two tabs gone", map[string][]int64{room: {a}}, room)
	e.presence(t, other, eve.Token, map[string]any{"tab": "e1", "device_stt": true})
	if n := e.srv.DeviceSTTParticipants(); n != 2 {
		t.Fatalf("flagged participants %d, want Alice and Eve", n)
	}
	clk.add(deviceLapse)
	if n := e.srv.DeviceSTTParticipants(); n != 0 {
		t.Fatalf("flagged participants after the lapse %d", n)
	}

	// A device beat is refused like a regular one past maxTabs, and needs a valid tab and session.
	for i := range maxTabs {
		e.presence(t, room, bob.Token, map[string]any{"tab": "t" + itoa(int64(i)), "device_stt": true})
	}
	if code, _ := e.do(t, "POST", "/api/rooms/"+room+"/presence", bob.Token, map[string]any{"tab": "one-more", "device_stt": true}); code != http.StatusTooManyRequests {
		t.Fatalf("past maxTabs: %d", code)
	}
	e.presence(t, room, bob.Token, map[string]any{"tab": "t0", "device_stt": true}) // a known tab still beats
	clk.add(deviceLapse)
	e.presence(t, room, bob.Token, map[string]any{"tab": "one-more", "device_stt": true}) // the lapsed ones free their place
	if code, _ := e.do(t, "POST", "/api/rooms/"+room+"/presence", bob.Token, map[string]any{"tab": "a b", "device_stt": true}); code != http.StatusBadRequest {
		t.Fatalf("bad tab: %d", code)
	}
	if code, _ := e.do(t, "POST", "/api/rooms/"+room+"/presence", eve.Token, on); code != http.StatusUnauthorized {
		t.Fatalf("another room's participant: %d", code)
	}

	// Without the offer a device beat is refused and flags nobody, on or off.
	off, _ := newVoiceEnv(t)
	offRoom := off.createRoom(t, "R").ID
	p := off.join(t, off.createInvite(t, offRoom).Token, "A")
	for _, v := range []bool{true, false} {
		if code, _ := off.do(t, "POST", "/api/rooms/"+offRoom+"/presence", p.Token, map[string]any{"tab": "a1", "device_stt": v}); code != http.StatusNotFound {
			t.Fatalf("not offered, device_stt %v: %d", v, code)
		}
	}
	if got := off.sttBeat(t, offRoom); len(got) != 0 {
		t.Fatalf("not offered: heartbeat answer %v", got)
	}
	if n := off.srv.DeviceSTTParticipants(); n != 0 {
		t.Fatalf("not offered: %d flagged", n)
	}
}

// 010-AC13: a device beat leaves the tab's audio state as it was, needs no event stream, and the
// presence event carries the flag.
func TestDeviceFlagInPresence(t *testing.T) {
	old := pollEvery
	pollEvery = 20 * time.Millisecond
	t.Cleanup(func() { pollEvery = old })
	e := newDeviceEnv(t)
	clk := e.clock()
	room := e.createRoom(t, "R").ID
	alice := e.join(t, e.createInvite(t, room).Token, "Alice")
	bob := e.join(t, e.createInvite(t, room).Token, "Bob")
	a, b := alice.Participant.ID, bob.Participant.ID
	ch, _ := e.openStream(t, "/api/rooms/"+room+"/events?tab=alice-1", alice.Token, "")
	wait := func(what string, ok func(map[int64]presenceOf) bool) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case ev := <-ch:
				if ev.event != "presence" {
					continue
				}
				var got struct{ People []presenceOf }
				if err := json.Unmarshal([]byte(ev.data), &got); err != nil {
					t.Fatal(err)
				}
				m := map[int64]presenceOf{}
				for _, p := range got.People {
					m[p.ID] = p
				}
				if ok(m) {
					return
				}
			case <-deadline:
				t.Fatalf("%s: no matching presence event", what)
			}
		}
	}
	wait("Alice here", func(m map[int64]presenceOf) bool { return m[a].Here && !m[a].DeviceSTT })
	e.presence(t, room, alice.Token, map[string]any{"tab": "alice-1", "audio": true, "mic": true})
	e.presence(t, room, alice.Token, map[string]any{"tab": "alice-1", "device_stt": true})
	wait("Alice on her device, still in the audio", func(m map[int64]presenceOf) bool {
		return m[a].DeviceSTT && m[a].Audio && m[a].Mic
	})
	// Bob's page has no stream open here: his worker's beat still flags him.
	e.presence(t, room, bob.Token, map[string]any{"tab": "bob-1", "device_stt": true})
	wait("Bob on his device", func(m map[int64]presenceOf) bool { return m[b].DeviceSTT && m[a].DeviceSTT })
	e.presence(t, room, alice.Token, map[string]any{"tab": "alice-1", "device_stt": false})
	wait("Alice back on the server, still in the audio", func(m map[int64]presenceOf) bool {
		return !m[a].DeviceSTT && m[a].Audio && m[a].Mic && m[b].DeviceSTT
	})
	clk.add(deviceLapse)
	wait("Bob lapsed", func(m map[int64]presenceOf) bool { return !m[b].DeviceSTT })
}

// 010-AC10: a participant posts their device's lines under their own name, whatever the body says;
// the line is a voice line from the device that counts for voice triggers. 404 without the offer.
func TestDeviceLines(t *testing.T) {
	e := newDeviceEnv(t)
	r := newTurnRoom(t, e)
	alice := r.alice
	tok := alice.p.Token
	sec := e.secretary(t, r, tok, map[string]any{"every_voice": 1})
	bob := e.join(t, e.createInvite(t, r.room).Token, "Bob")
	path := "/api/rooms/" + r.room + "/voice/device-lines"
	code, b := e.do(t, "POST", path, bob.Token, map[string]any{"body": "  Bonjour à tous.  ", "participant_id": alice.p.Participant.ID, "sender": "Alice"})
	if code != http.StatusCreated {
		t.Fatalf("line: %d %s", code, b)
	}
	m := decodeJSON[store.Message](t, b)
	if m.Kind != store.KindVoice || m.Source != store.VoiceSourceDevice || m.ParticipantID != bob.Participant.ID || m.Sender != "Bob" || m.Body != "Bonjour à tous." {
		t.Fatalf("message %+v", m)
	}
	if tn := e.nextTurn(t, r, alice); tn.PersonaID != sec.ID || !strings.Contains(tn.Cause, "voice line") {
		t.Fatalf("voice trigger turn %+v", tn)
	}
	code, b = e.do(t, "POST", "/api/rooms/"+r.room+"/voice/lines", sttToken, map[string]any{"participant_id": bob.Participant.ID, "body": "Something else entirely."})
	if code != http.StatusCreated || decodeJSON[store.Message](t, b).Source != store.VoiceSourceServer {
		t.Fatalf("worker line: %d %s", code, b)
	}
	if e.srv.VoiceLines(store.VoiceSourceDevice) != 1 || e.srv.VoiceLines(store.VoiceSourceServer) != 1 {
		t.Fatalf("lines counted: device %d server %d", e.srv.VoiceLines(store.VoiceSourceDevice), e.srv.VoiceLines(store.VoiceSourceServer))
	}

	other := e.createRoom(t, "Other")
	stranger := e.join(t, e.createInvite(t, other.ID).Token, "Eve")
	for name, c := range map[string]struct {
		token string
		body  any
		code  int
	}{
		"no session":                  {"", map[string]any{"body": "x"}, http.StatusUnauthorized},
		"STT token":                   {sttToken, map[string]any{"body": "x"}, http.StatusUnauthorized},
		"participant of another room": {stranger.Token, map[string]any{"body": "x"}, http.StatusUnauthorized},
		"empty body":                  {bob.Token, map[string]any{"body": " "}, http.StatusBadRequest},
		"control character":           {bob.Token, map[string]any{"body": "a\x00b"}, http.StatusBadRequest},
		"too long":                    {bob.Token, map[string]any{"body": strings.Repeat("a", maxBodyLen+1)}, http.StatusBadRequest},
		"body not text":               {bob.Token, map[string]any{"body": 3}, http.StatusBadRequest},
		"no body":                     {bob.Token, map[string]any{}, http.StatusBadRequest},
	} {
		if code, b := e.do(t, "POST", path, c.token, c.body); code != c.code {
			t.Errorf("%s: %d %s, want %d", name, code, b, c.code)
		}
	}
	// A revoked participant's device lines are refused with their access.
	inv := e.createInvite(t, r.room)
	carol := e.join(t, inv.Token, "Carol")
	if code, _ := e.do(t, "DELETE", "/api/rooms/"+r.room+"/invites/"+itoa(inv.ID)+"?cut=1", e.admin, nil); code != http.StatusOK {
		t.Fatal("revoke")
	}
	if code, _ := e.do(t, "POST", path, carol.Token, map[string]any{"body": "still here?"}); code != http.StatusUnauthorized {
		t.Fatalf("revoked participant: %d", code)
	}

	off, _ := newVoiceEnv(t)
	room := off.createRoom(t, "R")
	p := off.join(t, off.createInvite(t, room.ID).Token, "A")
	if code, _ := off.do(t, "POST", "/api/rooms/"+room.ID+"/voice/device-lines", p.Token, map[string]any{"body": "x"}); code != http.StatusNotFound {
		t.Fatalf("not offered: %d", code)
	}
}

// 010-AC16: a voice line repeating, within 5 s, a line of the same participant from the other
// source is dropped and counted; the same source twice, another sentence, another speaker or a
// later line is kept.
func TestVoiceLineDedupe(t *testing.T) {
	e := newDeviceEnv(t)
	clk := e.clock()
	room := e.createRoom(t, "R").ID
	alice := e.join(t, e.createInvite(t, room).Token, "Alice")
	bob := e.join(t, e.createInvite(t, room).Token, "Bob")
	device := func(tok, body string) (int, []byte) {
		return e.do(t, "POST", "/api/rooms/"+room+"/voice/device-lines", tok, map[string]any{"body": body})
	}
	server := func(pid int64, body string) (int, []byte) {
		return e.do(t, "POST", "/api/rooms/"+room+"/voice/lines", sttToken, map[string]any{"participant_id": pid, "body": body})
	}
	stored := func(code int, b []byte) bool {
		t.Helper()
		switch {
		case code == http.StatusCreated:
			return true
		case code == http.StatusOK && string(b) == `{"deduped":true}`+"\n":
			return false
		}
		t.Fatalf("line: %d %s", code, b)
		return false
	}
	a := alice.Participant.ID
	cases := []struct {
		name         string
		first, then  func() (int, []byte)
		gap          time.Duration
		keep         bool
		droppedLabel string
	}{
		{"case change", func() (int, []byte) { return server(a, "We ship on Friday") }, func() (int, []byte) { return device(alice.Token, "we ship ON friday") }, time.Second, false, store.VoiceSourceDevice},
		{"accent change", func() (int, []byte) { return device(alice.Token, "Réélu été") }, func() (int, []byte) { return server(a, "Reelu ete") }, time.Second, false, store.VoiceSourceServer},
		{"trailing punctuation", func() (int, []byte) { return server(a, "Is the rollback ready") }, func() (int, []byte) { return device(alice.Token, "Is the rollback ready?!") }, time.Second, false, store.VoiceSourceDevice},
		{"one word off", func() (int, []byte) { return server(a, "The release goes out on Friday morning") }, func() (int, []byte) { return device(alice.Token, "The release goes out on Friday mornings") }, time.Second, false, store.VoiceSourceDevice},
		{"4.9 s gap", func() (int, []byte) { return server(a, "Who owns the runbook") }, func() (int, []byte) { return device(alice.Token, "who owns the runbook.") }, 4900 * time.Millisecond, false, store.VoiceSourceDevice},
		{"5.1 s gap", func() (int, []byte) { return server(a, "Who signs the change") }, func() (int, []byte) { return device(alice.Token, "who signs the change.") }, 5100 * time.Millisecond, true, ""},
		{"6 s gap", func() (int, []byte) { return device(alice.Token, "Check the alerts first") }, func() (int, []byte) { return server(a, "check the alerts first") }, 6 * time.Second, true, ""},
		{"same source, device", func() (int, []byte) { return device(alice.Token, "Yes yes") }, func() (int, []byte) { return device(alice.Token, "Yes yes") }, time.Second, true, ""},
		{"same source, server", func() (int, []byte) { return server(a, "No") }, func() (int, []byte) { return server(a, "No") }, time.Second, true, ""},
		{"different sentence", func() (int, []byte) { return server(a, "We ship on Friday") }, func() (int, []byte) { return device(alice.Token, "We wait for Monday") }, time.Second, true, ""},
		{"another speaker", func() (int, []byte) { return server(a, "Ready when you are") }, func() (int, []byte) { return device(bob.Token, "Ready when you are") }, time.Second, true, ""},
	}
	for _, c := range cases {
		clk.add(time.Minute) // nothing of the previous case is in the window
		before := map[string]int64{}
		for _, s := range VoiceSources {
			before[s] = e.srv.VoiceLinesDeduped(s)
		}
		if !stored(c.first()) {
			t.Fatalf("%s: the first line was dropped", c.name)
		}
		clk.add(c.gap)
		if kept := stored(c.then()); kept != c.keep {
			t.Errorf("%s: kept %v, want %v", c.name, kept, c.keep)
		}
		for _, s := range VoiceSources {
			want := before[s]
			if s == c.droppedLabel {
				want++
			}
			if got := e.srv.VoiceLinesDeduped(s); got != want {
				t.Errorf("%s: deduped{%s} %d, want %d", c.name, s, got, want)
			}
		}
	}
}

func TestNormLineAndSimilar(t *testing.T) {
	for in, want := range map[string]string{
		"  Été, à l'Œil!  ": "ete a l œil",
		"ÇA va ?":           "ca va",
		"x--2  y":           "x 2 y",
		"...":               "",
		"Straße 42":         "straße 42",
	} {
		if got := normLine(in); got != want {
			t.Errorf("normLine(%q) = %q, want %q", in, got, want)
		}
	}
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"abcde", "abcde", true},
		{"abcde", "abcdx", true},  // 0.8
		{"abcde", "abcxx", false}, // 0.6
		{"abcd", "abcde", true},   // 0.8
		{"abc", "abcde", false},   // the length gap alone
		{"", "", true},            // two punctuation-only lines
		{"", "a", false},
		{"été", "ete", false}, // similar compares what normLine gives it
	} {
		if got := similar(c.a, c.b); got != c.want {
			t.Errorf("similar(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
