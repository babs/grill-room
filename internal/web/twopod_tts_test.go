package web

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/voices"
)

// 017-AC10: a TTS line queued on A while the TTS pod streams from B reaches the pod within 1 s,
// p95 over 20 lines; the pod reconnecting to A with Last-Event-ID gets the lines queued meanwhile,
// none again, none skipped.
func TestTwoPodsTTSLine(t *testing.T) {
	a, b := newPair(t, ttsOptions(t))
	room := a.createRoom(t, "Two pods").ID
	events, stop := b.openStream(t, "/api/tts/events", ttsToken, "")
	nextEvent(t, events, "lexicon")
	var took []time.Duration
	var last string
	for range 20 {
		start := time.Now()
		seq := a.queueLine(t, room, defaultTTSTimeouts.QueueS)
		ev := nextEvent(t, events, "line")
		took = append(took, time.Since(start))
		if last = strconv.FormatInt(seq, 10); ev.id != last {
			t.Fatalf("got line %s, want %s", ev.id, last)
		}
	}
	slices.Sort(took)
	if p95 := took[18]; p95 > time.Second {
		t.Errorf("p95 %v from A to B's stream, want at most 1 s (%v)", p95, took)
	}
	stop()
	var gap []string
	for range 3 {
		gap = append(gap, strconv.FormatInt(a.queueLine(t, room, defaultTTSTimeouts.QueueS), 10))
	}
	events, _ = a.openStream(t, "/api/tts/events", ttsToken, last)
	for _, want := range gap {
		if ev := nextEvent(t, events, "line"); ev.id != want {
			t.Fatalf("after Last-Event-ID %s: line %s, want %s", last, ev.id, want)
		}
	}
	next := strconv.FormatInt(a.queueLine(t, room, defaultTTSTimeouts.QueueS), 10)
	if ev := nextEvent(t, events, "line"); ev.id != next {
		t.Fatalf("line %s, want the new %s", ev.id, next)
	}
}

// 017-AC14: the TTS pod's heartbeat to B shows in a room stream on A within 2 s; a lexicon edit on A
// reaches the pod's stream on B.
func TestTwoPodsTTSPodAndLexicon(t *testing.T) {
	a, b := newPair(t, ttsOptions(t))
	room := a.createRoom(t, "Two pods")
	alice := a.join(t, a.createInvite(t, room.ID).Token, "Alice")
	roomEvents, _ := a.openStream(t, "/api/rooms/"+room.ID+"/events", alice.Token, "")
	if ev := nextEvent(t, roomEvents, "tts"); !strings.Contains(ev.data, `"pod":false`) {
		t.Fatalf("tts before any heartbeat: %s", ev.data)
	}
	start := time.Now()
	if code, body := b.do(t, "POST", "/api/tts/heartbeat", ttsToken, map[string]any{"pod": "tts-1",
		"states": []ttsState{{Room: room.ID, PersonaID: 7, State: "speaking"}}}); code != http.StatusNoContent {
		t.Fatalf("heartbeat: %d %s", code, body)
	}
	ev := nextEvent(t, roomEvents, "tts")
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the heartbeat to B took %v to show on A", d)
	}
	if !strings.Contains(ev.data, `"pod":true`) || !strings.Contains(ev.data, `"7":{`) {
		t.Errorf("tts on A: %s", ev.data)
	}

	pod, _ := b.openStream(t, "/api/tts/events", ttsToken, "")
	nextEvent(t, pod, "lexicon")
	term := voices.Term{Term: "Twopodterm", Style: "en", En: "two-pod-term"}
	if code, body := a.do(t, "POST", "/api/admin/pronunciations", a.admin, term); code != http.StatusCreated {
		t.Fatalf("add: %d %s", code, body)
	}
	if ev := nextEvent(t, pod, "lexicon"); !strings.Contains(ev.data, fmt.Sprintf(`"term":%q`, term.Term)) {
		t.Errorf("the edit on A did not reach B's stream: %.200s", ev.data)
	}
}
