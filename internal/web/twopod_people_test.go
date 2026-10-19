package web

import (
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"
)

// voicePodOptions are testOptions with voice, the STT worker's token and device transcription.
func voicePodOptions(t *testing.T) Options {
	t.Helper()
	dir, m := modelDir(t)
	opts := testOptions
	opts.Voice = &Voice{URL: "wss://livekit.example", Key: "APIkey", Secret: lkSecret, STTTokenHash: HashToken(sttToken)}
	opts.DeviceSTTDir, opts.DeviceSTTManifest = dir, m
	return opts
}

// within waits up to bound for an event of kind on ch that ok accepts, and fails past it.
func within(t *testing.T, what string, ch <-chan sseEvent, kind string, bound time.Duration, ok func(data []byte) bool) {
	t.Helper()
	deadline := time.After(bound)
	for {
		select {
		case ev, open := <-ch:
			if !open {
				t.Fatalf("%s: stream closed", what)
			}
			if ev.event == kind && ok([]byte(ev.data)) {
				return
			}
		case <-deadline:
			t.Fatalf("%s: not within %v", what, bound)
		}
	}
}

// presenceHas decodes a presence event and reports what ok says of participant pid in it.
func presenceHas(t *testing.T, pid int64, ok func(presenceOf) bool) func([]byte) bool {
	return func(data []byte) bool {
		var got struct{ People []presenceOf }
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(got.People, func(p presenceOf) bool { return p.ID == pid })
		return i >= 0 && ok(got.People[i])
	}
}

// 017-AC14: a tab streaming from pod A and beating to pod B is online on both within 2 s, with the
// audio state it beat; an STT worker's heartbeat to A shows on a stream of B within 2 s.
func TestTwoPodsPeopleAndSTT(t *testing.T) {
	a, b := newPair(t, voicePodOptions(t))
	room := a.createRoom(t, "Two pods").ID
	alice := a.join(t, a.createInvite(t, room).Token, "Alice")
	bob := a.join(t, a.createInvite(t, room).Token, "Bob")
	al := alice.Participant.ID
	onB, _ := b.openStream(t, "/api/rooms/"+room+"/events?tab=bob-1", bob.Token, "")
	within(t, "Bob's stream on B", onB, "presence", 10*time.Second, presenceHas(t, bob.Participant.ID, func(p presenceOf) bool { return p.Here }))

	start := time.Now()
	onA, _ := a.openStream(t, "/api/rooms/"+room+"/events?tab=alice-1", alice.Token, "")
	within(t, "Alice streaming from A, on B", onB, "presence", 2*time.Second-time.Since(start), presenceHas(t, al, func(p presenceOf) bool { return p.Here }))

	start = time.Now()
	if code, out := b.do(t, "POST", "/api/rooms/"+room+"/presence", alice.Token, map[string]any{"tab": "alice-1", "audio": true, "mic": true}); code != http.StatusNoContent {
		t.Fatalf("beat to B: %d %s", code, out)
	}
	inAudio := presenceHas(t, al, func(p presenceOf) bool { return p.Here && p.Audio && p.Mic })
	within(t, "Alice's audio beat to B, on B", onB, "presence", 2*time.Second-time.Since(start), inAudio)
	within(t, "Alice's audio beat to B, on A", onA, "presence", 2*time.Second-time.Since(start), inAudio)
	if now := time.Now(); !a.srv.people.here(room, al, now) || !b.srv.people.here(room, al, now) {
		t.Fatal("Alice not here on both pods")
	}
	if l := b.srv.people.listening(time.Now()); !l[room] {
		t.Fatal("B does not see the room listening")
	}

	start = time.Now()
	if code, out := a.do(t, "POST", "/api/stt/heartbeat", sttToken, map[string]any{"worker": "w1", "rooms": []string{room},
		"voices": map[string][]int64{room: {al}}, "engine": "parakeet"}); code != http.StatusOK {
		t.Fatalf("heartbeat to A: %d %s", code, out)
	}
	within(t, "the worker's heartbeat to A, on B", onB, "stt", 2*time.Second-time.Since(start), func(data []byte) bool {
		var st sttState
		if err := json.Unmarshal(data, &st); err != nil {
			t.Fatal(err)
		}
		return st.Available && st.Listening && slices.Equal(st.Transcribed, []int64{al}) && slices.Equal(st.Engines, []string{"parakeet"})
	})
	b.srv.leader.Store(true)
	b.srv.refreshGauges()
	if n := b.srv.STTWorkers(); n != 1 {
		t.Fatalf("B counts %d workers", n)
	}
}

// 017-AC15: 010-AC16's dedupe cases across two servers, the device lines on one and the worker's on
// the other, both ways; and the same utterance posted to both at once is stored once.
func TestTwoPodsVoiceLineDedupe(t *testing.T) {
	a, b := newPair(t, voicePodOptions(t))
	clk := a.clock()
	b.srv.now = clk.now
	voiceDedupe(t, a, b, clk)
	voiceDedupe(t, b, a, clk)

	room := a.createRoom(t, "Race").ID
	alice := a.join(t, a.createInvite(t, room).Token, "Alice")
	db := a.sqlDB(t)
	for i := range 5 {
		clk.add(time.Minute)
		body := "Same words " + itoa(int64(i))
		var wg sync.WaitGroup
		codes := make([]int, 2)
		wg.Go(func() {
			codes[0], _ = a.do(t, "POST", "/api/rooms/"+room+"/voice/device-lines", alice.Token, map[string]any{"body": body})
		})
		wg.Go(func() {
			codes[1], _ = b.do(t, "POST", "/api/rooms/"+room+"/voice/lines", sttToken, map[string]any{"participant_id": alice.Participant.ID, "body": body})
		})
		wg.Wait()
		var n int
		if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM messages WHERE room_id = $1 AND body = $2`, room, body).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 || !slices.Contains(codes, http.StatusCreated) || !slices.Contains(codes, http.StatusOK) {
			t.Fatalf("round %d: %d lines stored, answers %v", i, n, codes)
		}
	}
}
