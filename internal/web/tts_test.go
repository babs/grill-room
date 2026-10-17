package web

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/babs/grill-room/internal/livekit"
	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

const ttsToken = "tts-service-token"

func ttsOptions(t *testing.T) Options {
	t.Helper()
	opts := testOptions
	opts.Voice = &Voice{URL: "wss://livekit.example", Key: "APIkey", Secret: lkSecret, STTTokenHash: HashToken(sttToken),
		API: &livekit.Client{URL: "http://127.0.0.1:1", Key: "APIkey", Secret: lkSecret}}
	opts.TTSTokenHash = HashToken(ttsToken)
	return opts
}

// inAudio reports p's tab in the room's call, deafened or not.
func (e *env) inAudio(t *testing.T, room string, p joined, deaf bool) {
	t.Helper()
	if code, b := e.do(t, "POST", "/api/rooms/"+room+"/presence", p.Token, map[string]any{"tab": "tab1", "audio": true, "deaf": deaf}); code != http.StatusNoContent {
		t.Fatalf("presence: %d %s", code, b)
	}
}

// 013-AC13: without the token, or without LiveKit, every /api/tts route is 404; a wrong token 401.
func TestTTSRoutes(t *testing.T) {
	off := testOptions
	off.TTSTokenHash = HashToken(ttsToken)
	noLK := newEnvOpts(t, off)
	voiceOnly, _ := newVoiceEnv(t)
	for _, e := range []*env{newEnv(t), noLK, voiceOnly} {
		for _, path := range []string{"/api/tts/events", "/api/tts/heartbeat"} {
			for _, m := range []string{"GET", "POST"} {
				if code, _ := e.do(t, m, path, ttsToken, nil); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
					t.Errorf("%s %s with voice off: %d", m, path, code)
				}
			}
		}
	}
	on := newEnvOpts(t, ttsOptions(t))
	for _, tok := range []string{"", "wrong", sttToken, on.admin} {
		if code, _ := on.do(t, "GET", "/api/tts/events", tok, nil); code != http.StatusUnauthorized {
			t.Errorf("events with token %q: %d", tok, code)
		}
		if code, _ := on.do(t, "POST", "/api/tts/heartbeat", tok, map[string]string{"pod": "p"}); code != http.StatusUnauthorized {
			t.Errorf("heartbeat with token %q: %d", tok, code)
		}
	}
}

// ttsTurn runs one turn of Grill in a room where Alice is in the call, and returns its prompt;
// result is what the daemon posts.
func ttsTurn(t *testing.T, e *env, r *turnRoom, result turn.Result) string {
	t.Helper()
	tn := e.press(t, r, r.alice, r.alice.p.Token, false)
	prompt := e.job(t, r, r.alice, tn.ID).Prompt
	result.SessionID, result.Session = "11111111-1111-4111-8111-"+strconv.FormatInt(100000000000+tn.ID, 10), []byte("x\n")
	if code, b := e.result(t, r, r.alice, tn.ID, result); code != http.StatusOK {
		t.Fatalf("result: %d %s", code, b)
	}
	return prompt
}

// 013-AC1, 013-AC2, 013-AC14, 013-AC16: with a listener in the call the prompt asks for spoken.md;
// the line reaches the pod's stream sanitised, with the default voice stored; the rooms event lists
// the speaking persona. No log line carries the words.
func TestTTSLine(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	e := newEnvLog(t, ttsOptions(t), zap.New(core))
	r := newTurnRoom(t, e)
	e.inAudio(t, r.room, r.alice.p, false)
	events, _ := e.openStream(t, "/api/tts/events", ttsToken, "")
	if ev := nextEvent(t, events, "rooms"); !strings.Contains(ev.data, `"room":"`+r.room+`"`) || !strings.Contains(ev.data, `"name":"Grill","voice":"alba"`) {
		t.Fatalf("rooms: %s", ev.data)
	}
	prompt := ttsTurn(t, e, r, turn.Result{Synthesis: "I read the plan.", Changes: []turn.Change{
		{Path: turn.SpokenFile, Content: []byte("**I read** the [plan](docs/plan.md). Alice ·a1b2, which date?")}}})
	if !strings.Contains(prompt, "Write spoken.md") || !strings.Contains(prompt, "60 words at most") {
		t.Fatal("the prompt does not ask for spoken.md")
	}
	ev := nextEvent(t, events, "line")
	l := decodeJSON[ttsLine](t, []byte(ev.data))
	if ev.id != strconv.FormatInt(l.Seq, 10) || l.Room != r.room || l.PersonaID != r.grill.ID || l.Persona != "Grill" || l.Voice != "alba" ||
		l.Rate != 1 || l.Text != "I read the plan. Alice, which date?" || l.ttsTimeouts != defaultTTSTimeouts {
		t.Fatalf("line %+v", l)
	}
	if p, _ := e.st.RoomPersona(t.Context(), r.room, r.grill.ID); p.Voice != "alba" {
		t.Errorf("default voice not stored: %q", p.Voice)
	}
	for _, entry := range logs.All() {
		b, _ := json.Marshal(entry.ContextMap())
		if strings.Contains(entry.Message+string(b), "which date") {
			t.Errorf("a log line carries the spoken words: %s %s", entry.Message, b)
		}
	}

	// 013-AC4: no spoken.md, the summary of the turn's actions.
	ttsTurn(t, e, r, turn.Result{Synthesis: "I rewrote the synthesis.", Changes: []turn.Change{
		{Path: "docs/synthesis.md", Content: []byte("# Synthesis\n\nThe slow brown fox.\n")}}})
	if l := decodeJSON[ttsLine](t, []byte(nextEvent(t, events, "line").data)); l.Text != "I changed synthesis." {
		t.Errorf("summary line %q", l.Text)
	}
}

// 013-AC1: the prompt is byte-identical to the prompt of a server without voice when nobody hears
// the call (absent or deafened) or the persona does not speak. Both servers run the same steps.
func TestTTSPromptConditions(t *testing.T) {
	type step struct {
		name string
		prep func(e *env, r *turnRoom)
	}
	steps := []step{
		{"nobody in the call", func(*env, *turnRoom) {}},
		{"a deafened listener", func(e *env, r *turnRoom) { e.inAudio(t, r.room, r.alice.p, true) }},
		{"Speaks in the call off", func(e *env, r *turnRoom) {
			e.inAudio(t, r.room, r.alice.p, false)
			if code, b := e.do(t, "PATCH", "/api/rooms/"+r.room+"/personas/"+strconv.FormatInt(r.grill.ID, 10), r.alice.p.Token, map[string]bool{"speaks": false}); code != http.StatusOK {
				t.Fatalf("speaks off: %d %s", code, b)
			}
		}},
	}
	run := func(e *env) (*turnRoom, []string) {
		r := newTurnRoom(t, e)
		var out []string
		for _, st := range steps {
			st.prep(e, r)
			out = append(out, ttsTurn(t, e, r, turn.Result{Synthesis: "ok", Changes: []turn.Change{{Path: turn.SpokenFile, Content: []byte("Hello.")}}}))
		}
		return r, out
	}
	br, want := run(newEnv(t))
	e := newEnvOpts(t, ttsOptions(t))
	r, got := run(e)
	// Short ids and clock times differ between two servers; nothing else may.
	norm := regexp.MustCompile(`·[0-9a-f]{4}|\d{4}-\d\d-\d\d[ T]\d\d:\d\d:\d\d`)
	for i, st := range steps {
		if norm.ReplaceAllString(strings.ReplaceAll(want[i], br.room, r.room), "") != norm.ReplaceAllString(got[i], "") {
			t.Errorf("%s: the prompt changed\n%s\n---\n%s", st.name, strings.ReplaceAll(want[i], br.room, r.room), got[i])
		}
	}
	if st, _ := e.srv.TTSStats(); st.Queued != 0 {
		t.Errorf("queued %d lines: nobody listened, or the persona did not speak", st.Queued)
	}
}

// 013-AC11, 013-AC37: the persona API refuses a voice outside the catalogue and a rate out of range.
func TestTTSPersonaFields(t *testing.T) {
	e := newEnvOpts(t, ttsOptions(t))
	r := newTurnRoom(t, e)
	path := "/api/rooms/" + r.room + "/personas/" + strconv.FormatInt(r.grill.ID, 10)
	for _, body := range []map[string]any{{"voice": "marius"}, {"voice": "cosette"}, {"voice_rate": 0.85}, {"voice_rate": 1.35}, {"voice_rate": 2}} {
		if code, _ := e.do(t, "PATCH", path, r.alice.p.Token, body); code != http.StatusBadRequest {
			t.Errorf("%v: %d", body, code)
		}
	}
	code, b := e.do(t, "PATCH", path, r.alice.p.Token, map[string]any{"voice": "cml-2114", "voice_rate": 1.234})
	if p := decodeJSON[store.RoomPersona](t, b); code != http.StatusOK || p.Voice != "cml-2114" || p.VoiceRate != 1.23 || !p.Speaks {
		t.Fatalf("set voice and rate: %d %s", code, b)
	}
}

// 013-AC14: a line no pod took expires after its room's queue maximum as no_service; numbering
// grows across a restart, and a stream resumes after Last-Event-ID.
func TestTTSHub(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	h := newTTSHub(now)
	l := ttsLine{Room: "r", Text: "a", ttsTimeouts: defaultTTSTimeouts, QueuedAt: now}
	first := h.add(l, now)
	second := h.add(l, now)
	if got, _ := h.next(first, now); len(got) != 1 || got[0].Seq != second {
		t.Fatalf("after %d: %+v", first, got)
	}
	// The second was sent; the first was not, so it expires as no_service.
	if n := h.noServiceDropped(now.Add(41 * time.Second)); n != 1 {
		t.Errorf("no_service %d, want 1", n)
	}
	if got, _ := h.next(0, now.Add(41*time.Second)); len(got) != 0 {
		t.Errorf("expired lines handed out: %+v", got)
	}
	if again := newTTSHub(now.Add(time.Second)); again.add(l, now) <= second {
		t.Error("a restarted server numbers its lines below the old ones")
	}
	for range maxTTSLines + 3 {
		h.add(l, now)
	}
	if len(h.lines) != maxTTSLines || h.noService.Load() != 1+3 {
		t.Errorf("kept %d lines, no_service %d", len(h.lines), h.noService.Load())
	}

	e := newEnvOpts(t, ttsOptions(t))
	e.srv.tts.add(l, time.Now())
	seq := e.srv.tts.add(ttsLine{Room: "r", Text: "b", ttsTimeouts: defaultTTSTimeouts, QueuedAt: time.Now()}, time.Now())
	ch, stop1 := e.openStream(t, "/api/tts/events", ttsToken, strconv.FormatInt(seq-1, 10))
	if ev := nextEvent(t, ch, "line"); ev.id != strconv.FormatInt(seq, 10) {
		t.Errorf("resumed at %s, want %d", ev.id, seq)
	}
	ch, stop2 := e.openStream(t, "/api/tts/events", ttsToken, strconv.FormatInt(seq+1_000_000, 10))
	if ev := nextEvent(t, ch, "line"); ev.id != strconv.FormatInt(seq-1, 10) {
		t.Errorf("an id from the future starts over: %s", ev.id)
	}
	stop1()
	stop2()
	waitFor(t, func() bool { e.srv.tts.mu.Lock(); defer e.srv.tts.mu.Unlock(); return e.srv.tts.streams == 0 })
	// A restarted pod sends no Last-Event-ID: it gets the lines no pod took, never one already said.
	third := e.srv.tts.add(ttsLine{Room: "r", Text: "c", ttsTimeouts: defaultTTSTimeouts, QueuedAt: time.Now()}, time.Now())
	e.srv.tts.mu.Lock()
	for _, l := range e.srv.tts.lines {
		l.sent = l.Seq < third
	}
	e.srv.tts.mu.Unlock()
	ch, _ = e.openStream(t, "/api/tts/events", ttsToken, "")
	if ev := nextEvent(t, ch, "line"); ev.id != strconv.FormatInt(third, 10) {
		t.Errorf("a pod with no Last-Event-ID got line %s, want only %d", ev.id, third)
	}
	if h := newTTSHub(time.Now()); h.resumePoint() != h.seq {
		t.Error("an empty hub resumes from its own counter")
	}
}

// 013-AC14: the heartbeat's states are checked, and the pod counts as up for ttsLapse.
func TestTTSHeartbeat(t *testing.T) {
	e := newEnvOpts(t, ttsOptions(t))
	now := time.Now()
	e.srv.now = func() time.Time { return now }
	for _, body := range []map[string]any{
		{},
		{"pod": strings.Repeat("p", maxTTSPodName+1)},
		{"pod": "p", "states": []ttsState{{Room: "r", State: "shouting"}}},
		{"pod": "p", "states": []ttsState{{Room: "r", State: "dropped"}}},
		{"pod": "p", "states": []ttsState{{Room: "r", State: "dropped", Reason: "bored"}}},
		{"pod": "p", "states": []ttsState{{Room: "r", State: "speaking", Reason: "stale"}}},
	} {
		if code, _ := e.do(t, "POST", "/api/tts/heartbeat", ttsToken, body); code != http.StatusBadRequest {
			t.Errorf("%v: %d", body, code)
		}
	}
	if st, _ := e.srv.TTSStats(); st.PodUp {
		t.Error("up before any heartbeat")
	}
	ok := map[string]any{"pod": "tts-1", "rooms": []string{"r"}, "bots": []string{"persona-1"},
		"states": []ttsState{{Room: "r", PersonaID: 1, State: "speaking"}, {Room: "r", PersonaID: 2, State: "dropped", Reason: "stale"}}}
	if code, b := e.do(t, "POST", "/api/tts/heartbeat", ttsToken, ok); code != http.StatusNoContent {
		t.Fatalf("heartbeat: %d %s", code, b)
	}
	if st, _ := e.srv.TTSStats(); !st.PodUp {
		t.Error("down after a heartbeat")
	}
	now = now.Add(ttsLapse + time.Second)
	if st, _ := e.srv.TTSStats(); st.PodUp {
		t.Error("still up past the lapse")
	}
}

// 013-AC16: defaults are picked from the store, not a caller's snapshot: a snapshot taken before
// another call stored a voice must not hand that voice out again.
func TestTTSDefaultVoicesNeverRepeat(t *testing.T) {
	e := newEnvOpts(t, ttsOptions(t))
	r := newTurnRoom(t, e)
	sec := e.addPersona(t, r, r.alice.p.Token, e.templates(t, r, r.alice.p.Token)[1].ID)
	ctx := t.Context()
	ps, err := e.st.RoomPersonas(ctx, r.room)
	if err != nil || len(ps) != 2 {
		t.Fatalf("personas %v %v", ps, err)
	}
	stale := slices.Clone(ps)
	// The first call sees only Grill speaking, the second only Secretary, from the same old read.
	onlyGrill, onlySec := slices.Clone(ps), stale
	onlyGrill[1].Speaks, onlySec[0].Speaks = false, false
	e.srv.withVoices(ctx, r.room, onlyGrill)
	e.srv.withVoices(ctx, r.room, onlySec)
	g, _ := e.st.RoomPersona(ctx, r.room, r.grill.ID)
	s, _ := e.st.RoomPersona(ctx, r.room, sec.ID)
	if g.Voice == "" || s.Voice == "" || g.Voice == s.Voice {
		t.Errorf("Grill %q, Secretary %q: two defaults must differ", g.Voice, s.Voice)
	}
	// A stored voice never moves (013-AC16).
	e.srv.withVoices(ctx, r.room, stale)
	if again, _ := e.st.RoomPersona(ctx, r.room, r.grill.ID); again.Voice != g.Voice {
		t.Errorf("Grill's voice moved from %q to %q", g.Voice, again.Voice)
	}
}

// waitFor waits until cond holds, for at most 5 s: an observable state, never a fixed sleep.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached within 5 s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
