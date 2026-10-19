package web

import (
	"bytes"
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

// podBeats posts one pod heartbeat: with its stream open, the pod is up and lines keep their room's
// queue maximum (013-AC34).
func (e *env) podBeats(t *testing.T) {
	t.Helper()
	if code, b := e.do(t, "POST", "/api/tts/heartbeat", ttsToken, map[string]string{"pod": "p"}); code != http.StatusNoContent {
		t.Fatalf("heartbeat: %d %s", code, b)
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
	e.podBeats(t)
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

// queueLine queues one line of room at e's clock, as a turn would.
func (e *env) queueLine(t *testing.T, room string, queueS int) int64 {
	t.Helper()
	l := ttsLine{Room: room, Text: "a", ttsTimeouts: defaultTTSTimeouts, QueuedAt: e.srv.now()}
	l.QueueS = queueS
	seq, err := e.srv.addTTSLine(t.Context(), l)
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

// takeLine is what the pod's stream reads after seq at e's clock.
func (e *env) takeLine(t *testing.T, after int64) []store.TTSLine {
	t.Helper()
	ls, err := e.st.TTSLinesAfter(t.Context(), after, e.srv.now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	return ls
}

// 013-AC14, 017-AC10: a stream resumes after Last-Event-ID; one from the future starts over; a pod
// with none gets the lines no pod took, never one already said.
func TestTTSStreamResume(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	e := newEnvLog(t, ttsOptions(t), zap.New(core))
	room := e.createRoom(t, "R").ID
	first := e.queueLine(t, room, defaultTTSTimeouts.QueueS)
	seq := e.queueLine(t, room, defaultTTSTimeouts.QueueS)
	ch, stop1 := e.openStream(t, "/api/tts/events", ttsToken, strconv.FormatInt(first, 10))
	if ev := nextEvent(t, ch, "line"); ev.id != strconv.FormatInt(seq, 10) {
		t.Errorf("resumed at %s, want %d", ev.id, seq)
	}
	ch, stop2 := e.openStream(t, "/api/tts/events", ttsToken, strconv.FormatInt(seq+1_000_000_000_000, 10))
	if ev := nextEvent(t, ch, "line"); ev.id != strconv.FormatInt(first, 10) {
		t.Errorf("an id from the future starts over: %s", ev.id)
	}
	stop1()
	stop2()
	// A stream still running would take the next line.
	waitFor(t, func() bool { return logs.FilterMessage("tts_disconnected").Len() == 2 })

	// Both lines were said; the third was not.
	third := e.queueLine(t, room, defaultTTSTimeouts.QueueS)
	ch, _ = e.openStream(t, "/api/tts/events", ttsToken, "")
	if ev := nextEvent(t, ch, "line"); ev.id != strconv.FormatInt(third, 10) {
		t.Errorf("a pod with no Last-Event-ID got line %s, want only %d", ev.id, third)
	}
}

// 013-AC34: a line queued while no pod is up (streaming and heartbeating) waits up to coldQueueS
// for one scaling from zero, and the pod gets that budget in queue_s; with a pod connected, the
// room's maximum stands. A line never taken counts as no_service once past its budget.
func TestTTSColdStartQueue(t *testing.T) {
	e := newEnvOpts(t, ttsOptions(t))
	room := e.createRoom(t, "R").ID
	now := time.Unix(1_800_000_000, 0)
	e.srv.now = func() time.Time { return now }
	e.srv.lease = e.srv.now
	queueS := func(seq int64) int {
		t.Helper()
		ls := e.takeLine(t, seq-1)
		if len(ls) == 0 || ls[0].Seq != seq {
			t.Fatalf("line %d not handed out: %+v", seq, ls)
		}
		return ls[0].QueueS
	}
	// Queued first, so no read after a later seq takes it.
	unsent := e.queueLine(t, room, defaultTTSTimeouts.QueueS)
	seq := e.queueLine(t, room, defaultTTSTimeouts.QueueS)
	late := now.Add(time.Duration(coldQueueS-1) * time.Second)
	now = late
	if got := queueS(seq); got != coldQueueS {
		t.Errorf("a cold line at %ds: queue_s %d", coldQueueS-1, got)
	}
	e.srv.sweepShared(t.Context())
	if st, _ := e.srv.TTSStats(); st.NoService != 0 {
		t.Errorf("no_service %d before the cold budget", st.NoService)
	}
	now = now.Add(2 * time.Second)
	e.srv.sweepShared(t.Context())
	if st, _ := e.srv.TTSStats(); st.NoService != 1 {
		t.Errorf("no_service %d past the cold budget, want 1 (line %d)", st.NoService, unsent)
	}
	if got := queueS(e.queueLine(t, room, 300)); got != 300 {
		t.Errorf("a room maximum over the cold budget: %d", got)
	}
	// A heartbeat but no stream (the pod reconnecting): cold.
	e.podBeats(t)
	if got := queueS(e.queueLine(t, room, defaultTTSTimeouts.QueueS)); got != coldQueueS {
		t.Errorf("a heartbeat with no stream: %d", got)
	}
	// A pod up: the room's maximum, waiting behind other speech included.
	if err := e.st.RenewTTSStream(t.Context(), now.Add(ttsLease).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	e.srv.tts.forget()
	if got := queueS(e.queueLine(t, room, defaultTTSTimeouts.QueueS)); got != defaultTTSTimeouts.QueueS {
		t.Errorf("a warm line: %d", got)
	}
	// A stream left by a pod gone with its node, no heartbeat within ttsLapse: still cold.
	now = now.Add(ttsLapse + time.Second)
	if err := e.st.RenewTTSStream(t.Context(), now.Add(ttsLease).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	e.srv.tts.forget()
	if got := queueS(e.queueLine(t, room, defaultTTSTimeouts.QueueS)); got != coldQueueS {
		t.Errorf("a stream with no heartbeat: %d", got)
	}
}

// 013-AC14: the heartbeat's states are checked, and the pod counts as up for ttsLapse.
func TestTTSHeartbeat(t *testing.T) {
	e := newEnvOpts(t, ttsOptions(t))
	now := time.Now()
	e.srv.now = func() time.Time { return now }
	e.srv.lease = e.srv.now
	e.srv.leader.Store(true)
	stats := func() TTSStats {
		e.srv.refreshGauges()
		st, _ := e.srv.TTSStats()
		return st
	}
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
	if stats().PodUp {
		t.Error("up before any heartbeat")
	}
	ok := map[string]any{"pod": "tts-1", "rooms": []string{"r"}, "bots": []string{"persona-1"},
		"states": []ttsState{{Room: "r", PersonaID: 1, State: "speaking"}, {Room: "r", PersonaID: 2, State: "dropped", Reason: "stale"}}}
	if code, b := e.do(t, "POST", "/api/tts/heartbeat", ttsToken, ok); code != http.StatusNoContent {
		t.Fatalf("heartbeat: %d %s", code, b)
	}
	if !stats().PodUp {
		t.Error("down after a heartbeat")
	}
	now = now.Add(ttsLapse + time.Second)
	if stats().PodUp {
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
	e.srv.withVoices(ctx, r.room, onlyGrill, "en")
	e.srv.withVoices(ctx, r.room, onlySec, "en")
	g, _ := e.st.RoomPersona(ctx, r.room, r.grill.ID)
	s, _ := e.st.RoomPersona(ctx, r.room, sec.ID)
	if g.Voice == "" || s.Voice == "" || g.Voice == s.Voice {
		t.Errorf("Grill %q, Secretary %q: two defaults must differ", g.Voice, s.Voice)
	}
	// A stored voice never moves (013-AC16).
	e.srv.withVoices(ctx, r.room, stale, "en")
	if again, _ := e.st.RoomPersona(ctx, r.room, r.grill.ID); again.Voice != g.Voice {
		t.Errorf("Grill's voice moved from %q to %q", g.Voice, again.Voice)
	}
}

// waitFor waits until cond holds, for at most 5 s: an observable state, never a fixed sleep.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatal("condition not reached within 10 s")
		}
	}
}

// 013-AC25: room admins set the call settings within their ranges; a plain member gets 403; a
// change says so in the chat and reaches the next line.
func TestTTSSpeechSettings(t *testing.T) {
	e := newEnvOpts(t, ttsOptions(t))
	alice := e.device(t)
	room := alice.createRoom(t, "Call", "Alice")
	path := "/api/rooms/" + room.ID + "/speech"
	bob := e.device(t)
	bob.join(t, room.ID, "Bob")
	if code, _ := bob.do(t, "PATCH", path, map[string]int{"quiet_ms": 2000}); code != http.StatusForbidden {
		t.Errorf("a member sets the call settings: %d", code)
	}
	for _, bad := range []map[string]int{{"quiet_ms": 499}, {"quiet_ms": 5001}, {"resume_s": -1}, {"resume_s": 61}, {"queue_s": 9}, {"queue_s": 301},
		{"words": 39}, {"words": 61}} {
		if code, _ := alice.do(t, "PATCH", path, bad); code != http.StatusBadRequest {
			t.Errorf("%v: %d", bad, code)
		}
	}
	code, b := alice.do(t, "PATCH", path, map[string]int{"quiet_ms": 500, "resume_s": 60, "queue_s": 300, "words": 40})
	if got := decodeJSON[store.Room](t, b).Speech; code != http.StatusOK || got != (store.Speech{QuietMS: 500, ResumeS: 60, QueueS: 300, Words: 40}) {
		t.Fatalf("set: %d %s", code, b)
	}
	if m := e.lastLine(t, room.ID); m.Kind != store.KindSystem || !strings.Contains(m.Body, "quiet before speaking 1000 ms → 500 ms") ||
		!strings.Contains(m.Body, "spoken words 60 → 40") {
		t.Errorf("chat line %q", m.Body)
	}
	// Admin token: allowed too.
	if code, _ := e.do(t, "PATCH", path, e.admin, map[string]int{"words": 50}); code != http.StatusOK {
		t.Errorf("the admin token: %d", code)
	}
	// Both changes are audited, the room admin's and the token's (004-AC6).
	got, err := e.st.AuditLog(t.Context(), store.AuditQuery{Action: ActionRoomSpeech, Limit: 10})
	if err != nil || len(got) != 2 || got[1].TargetID != room.ID || got[0].Actor != store.ActorSuperAdmin {
		t.Errorf("audit %+v %v", got, err)
	}
}

// 013-AC25, 013-AC1: the line carries the room's settings, and the prompt the room's word cap.
func TestTTSLineCarriesRoomSettings(t *testing.T) {
	e := newEnvOpts(t, ttsOptions(t))
	r := newTurnRoom(t, e)
	if _, err := e.st.SetSpeech(t.Context(), r.room, store.Speech{QuietMS: 1500, ResumeS: 10, QueueS: 100, Words: 45}, nil); err != nil {
		t.Fatal(err)
	}
	e.inAudio(t, r.room, r.alice.p, false)
	events, _ := e.openStream(t, "/api/tts/events", ttsToken, "")
	e.podBeats(t)
	long := strings.Repeat("word ", 50) + "end."
	prompt := ttsTurn(t, e, r, turn.Result{Synthesis: "ok", Changes: []turn.Change{{Path: turn.SpokenFile, Content: []byte(long)}}})
	if !strings.Contains(prompt, "45 words at most") {
		t.Error("the prompt does not carry the room's cap")
	}
	l := decodeJSON[ttsLine](t, []byte(nextEvent(t, events, "line").data))
	if l.ttsTimeouts != (ttsTimeouts{QuietMS: 1500, ResumeS: 10, QueueS: 100}) || len(strings.Fields(l.Text)) != 45 {
		t.Errorf("line %+v, %d words", l.ttsTimeouts, len(strings.Fields(l.Text)))
	}
}

// 013-AC27, 013-AC11, 013-AC16: the pod hands over samples for catalogue voices only; the picker
// lists them with the room's language and who holds which voice, and a room with no line takes the
// browser's language for its defaults.
func TestTTSVoicesAndSamples(t *testing.T) {
	e := newEnvOpts(t, ttsOptions(t))
	room := e.createRoom(t, "Empty")
	p := e.join(t, e.createInvite(t, room.ID).Token, "Alice")
	put := func(voice, lang string, body []byte) int {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), "PUT", e.ts.URL+"/api/tts/samples/"+voice+"/"+lang, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+ttsToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	for _, c := range []struct {
		voice, lang string
		body        []byte
		code        int
	}{{"estelle", "fr", []byte("OggS-estelle"), 204}, {"marius", "fr", []byte("x"), 400}, {"estelle", "de", []byte("x"), 400},
		{"alba", "en", nil, 400}, {"alba", "en", make([]byte, maxSampleBytes+1), 413}} {
		if got := put(c.voice, c.lang, c.body); got != c.code {
			t.Errorf("PUT %s/%s: %d, want %d", c.voice, c.lang, got, c.code)
		}
	}
	_, b := e.do(t, "GET", "/api/tts/samples", ttsToken, nil)
	if h := decodeJSON[map[string]map[string]string](t, b)["samples"]; len(h) != 1 || len(h["estelle/fr"]) != 64 {
		t.Errorf("hashes %v", h)
	}
	code, b := e.do(t, "GET", "/api/rooms/"+room.ID+"/voices/estelle/fr", p.Token, nil)
	if code != http.StatusOK || string(b) != "OggS-estelle" {
		t.Errorf("sample: %d %q", code, b)
	}
	if code, _ := e.do(t, "GET", "/api/rooms/"+room.ID+"/voices/alba/en", p.Token, nil); code != http.StatusNotFound {
		t.Errorf("a sample never handed over: %d", code)
	}
	_, b = e.do(t, "GET", "/api/rooms/"+room.ID+"/voices?lang=fr", p.Token, nil)
	got := decodeJSON[struct {
		Voices []voiceChoice    `json:"voices"`
		Lang   string           `json:"lang"`
		Used   map[string]int64 `json:"used"`
	}](t, b)
	if got.Lang != "fr" || len(got.Voices) < 20 || got.Voices[0].ID != "estelle" || !slices.Equal(got.Voices[0].Samples, []string{"fr"}) || got.Voices[0].Credit == "" {
		t.Errorf("voices %+v", got)
	}
	// The room has no line: Grill took the first French voice from the browser's language.
	if len(got.Used) != 1 || got.Used["estelle"] == 0 {
		t.Errorf("used %v", got.Used)
	}
	off := newEnv(t)
	offRoom := off.createRoom(t, "R")
	q := off.join(t, off.createInvite(t, offRoom.ID).Token, "A")
	if code, _ := off.do(t, "GET", "/api/rooms/"+offRoom.ID+"/voices", q.Token, nil); code != http.StatusNotFound {
		t.Errorf("voices with TTS off: %d", code)
	}
}

// 013-AC29: the room's tts event carries each persona's state from the pod's heartbeat, and clears
// once the pod is silent past ttsLapse.
func TestTTSRoomEvent(t *testing.T) {
	e := newEnvOpts(t, ttsOptions(t))
	now := time.Now()
	e.srv.now = func() time.Time { return now }
	e.srv.lease = e.srv.now
	if code, _ := e.do(t, "POST", "/api/tts/heartbeat", ttsToken, map[string]any{"pod": "p", "states": []ttsState{
		{Room: "r1", PersonaID: 3, State: "speaking"}, {Room: "r1", PersonaID: 4, State: "waiting"}, {Room: "r1", PersonaID: 5, State: "idle"},
		{Room: "r2", PersonaID: 6, State: "speaking"}}}); code != http.StatusNoContent {
		t.Fatal(code)
	}
	ev := e.srv.ttsRoomEvent("r1")
	if !strings.Contains(ev, `"pod":true`) || !strings.Contains(ev, `"3":{`) || !strings.Contains(ev, `"state":"waiting"`) ||
		strings.Contains(ev, `"5":`) || strings.Contains(ev, `"6":`) {
		t.Errorf("event %s", ev)
	}
	now = now.Add(ttsLapse + time.Second)
	if ev := e.srv.ttsRoomEvent("r1"); ev != "event: tts\ndata: {\"personas\":{},\"pod\":false}\n\n" {
		t.Errorf("after the lapse: %q", ev)
	}
}

// 013-AC16: a room from Spec grill gives its two speaking personas, Grill and Rodin, two different
// default voices, and none to the silent Secretary.
func TestSpecGrillVoicesDiffer(t *testing.T) {
	e := newEnvOpts(t, ttsOptions(t))
	alice := e.signedIn(t, "Alice")
	code, b := alice.createFrom(t, map[string]any{"name": "G", "starter": map[string]any{"id": alice.builtin(t, "Spec grill"), "version": 1},
		"fields": map[string]string{"topic": "Billing"}})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, b)
	}
	room := decodeJSON[store.Room](t, b).ID
	ctx := t.Context()
	ps, _ := e.st.RoomPersonas(ctx, room)
	e.srv.withVoices(ctx, room, ps, "en")
	ps, _ = e.st.RoomPersonas(ctx, room)
	voice := map[string]string{}
	for _, p := range ps {
		voice[p.Name] = p.Voice
	}
	if voice["Grill"] == "" || voice["Rodin"] == "" || voice["Grill"] == voice["Rodin"] || voice["Secretary"] != "" {
		t.Errorf("voices %v: Grill and Rodin need two different ones, Secretary none", voice)
	}
}

// A /metrics scrape reads memory only: with the database gone the gauges answer what the last
// refresh computed, at once.
func TestGaugesReadMemory(t *testing.T) {
	e := newEnvOpts(t, ttsOptions(t))
	r := newTurnRoom(t, e)
	_, closeRoom := e.openStream(t, "/api/rooms/"+r.room+"/events?tab=alice-1", r.alice.p.Token, "")
	defer closeRoom()
	e.podBeats(t)
	e.srv.leader.Store(true)
	e.srv.refreshGauges()
	if err := e.st.Close(); err != nil {
		t.Fatal(err)
	}
	st, _ := e.srv.TTSStats()
	if n := e.srv.TTSDemand(); n != 1 || !st.PodUp {
		t.Fatalf("demand %d, pod up %v; want the last refresh's 1, true", n, st.PodUp)
	}
}

// 013-AC34: a stream that ends releases its lease, so the next line gets the cold budget at once;
// a lease another stream renewed since stays.
func TestTTSStreamReleasesLease(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	e := newEnvLog(t, ttsOptions(t), zap.New(core))
	room := e.createRoom(t, "R").ID
	e.podBeats(t)
	queued := func() int {
		t.Helper()
		seq := e.queueLine(t, room, defaultTTSTimeouts.QueueS)
		ls := e.takeLine(t, seq-1)
		if len(ls) == 0 || ls[0].Seq != seq {
			t.Fatalf("line %d not handed out: %+v", seq, ls)
		}
		return ls[0].QueueS
	}
	ch, stop := e.openStream(t, "/api/tts/events", ttsToken, "")
	nextEvent(t, ch, "rooms")
	if got := queued(); got != defaultTTSTimeouts.QueueS {
		t.Fatalf("with a stream: queue_s %d", got)
	}
	stop()
	// The release runs as the stream returns, before it logs.
	waitFor(t, func() bool { return logs.FilterMessage("tts_disconnected").Len() == 1 })
	e.srv.tts.forget()
	if got := queued(); got != coldQueueS {
		t.Fatalf("after the stream ended: queue_s %d, want %d", got, coldQueueS)
	}
	// Another stream's later renewal is not this one's to release.
	if err := e.st.RenewTTSStream(t.Context(), time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := e.st.ReleaseTTSStream(t.Context(), time.Now().Add(ttsLease).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if p, err := e.st.TTSPodState(t.Context()); err != nil || p.StreamUntilMS == 0 {
		t.Fatalf("a later lease released: %+v %v", p, err)
	}
}
