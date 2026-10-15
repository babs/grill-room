package web

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/livekit"
)

const (
	lkSecret = "0123456789abcdef0123456789abcdef"
	sttToken = "stt-service-token"
)

// fakeLiveKit records the RoomService methods called and their bodies.
type fakeLiveKit struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeLiveKit) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, strings.TrimPrefix(r.URL.Path, "/twirp/livekit.RoomService/")+" "+string(b))
	f.mu.Unlock()
	_, _ = io.WriteString(w, "{}")
}

func (f *fakeLiveKit) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func newVoiceEnv(t *testing.T) (*env, *fakeLiveKit) {
	t.Helper()
	lk := &fakeLiveKit{}
	lts := httptest.NewServer(lk)
	t.Cleanup(lts.Close)
	opts := testOptions
	opts.Voice = &Voice{
		URL: "wss://livekit.example/sub;path", Key: "APIkey", Secret: lkSecret,
		API:          &livekit.Client{URL: lts.URL, Key: "APIkey", Secret: lkSecret, HTTP: lts.Client()},
		STTTokenHash: HashToken(sttToken),
	}
	return newEnvOpts(t, opts), lk
}

// claims decodes a JWT payload; internal/livekit tests check the signature.
func claims(t *testing.T, tok string) map[string]any {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token %q", tok)
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	return decodeJSON[map[string]any](t, b)
}

// 001-AC6: each participant gets a token for its own room's audio, under its own identity.
func TestVoiceToken(t *testing.T) {
	e, _ := newVoiceEnv(t)
	room := e.createRoom(t, "R")
	p := e.join(t, e.createInvite(t, room.ID).Token, "Alice")
	code, b := e.do(t, "GET", "/api/rooms/"+room.ID+"/voice", p.Token, nil)
	if code != http.StatusOK {
		t.Fatalf("voice: %d %s", code, b)
	}
	resp := decodeJSON[map[string]string](t, b)
	if resp["url"] != "wss://livekit.example/sub;path" {
		t.Fatalf("url %q", resp["url"])
	}
	c := claims(t, resp["token"])
	v := c["video"].(map[string]any)
	if c["sub"] != "1" || c["name"] != "Alice" || c["iss"] != "APIkey" || v["room"] != room.ID || v["roomJoin"] != true {
		t.Fatalf("claims %v", c)
	}
	// LiveKit cannot revoke a token: a short life bounds how long a kicked participant can rejoin.
	if exp := time.Unix(int64(c["exp"].(float64)), 0); time.Until(exp) > voiceTokenTTL || voiceTokenTTL > 5*time.Minute {
		t.Fatalf("token lives until %v", exp)
	}
	if v["canPublishData"] != false || v["roomAdmin"] != nil || len(v["canPublishSources"].([]any)) != 1 {
		t.Fatalf("grant wider than microphone audio: %v", v)
	}

	other := e.createRoom(t, "Other")
	if code, _ := e.do(t, "GET", "/api/rooms/"+other.ID+"/voice", p.Token, nil); code != http.StatusUnauthorized {
		t.Fatalf("other room: %d", code)
	}
	if code, _ := e.do(t, "GET", "/api/rooms/"+room.ID+"/voice", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	code, b = e.do(t, "GET", "/api/rooms/"+room.ID, p.Token, nil)
	if code != http.StatusOK || !strings.Contains(string(b), `"voice":true`) {
		t.Fatalf("room: %d %s", code, b)
	}

	off := newEnv(t)
	room = off.createRoom(t, "R")
	p = off.join(t, off.createInvite(t, room.ID).Token, "A")
	if code, _ := off.do(t, "GET", "/api/rooms/"+room.ID+"/voice", p.Token, nil); code != http.StatusNotFound {
		t.Fatalf("voice off: %d", code)
	}
	if _, b := off.do(t, "GET", "/api/rooms/"+room.ID, p.Token, nil); !strings.Contains(string(b), `"voice":false`) {
		t.Fatalf("voice off room: %s", b)
	}
}

func TestVoiceCSPAllowsLiveKit(t *testing.T) {
	e, _ := newVoiceEnv(t)
	resp, err := http.Get(e.ts.URL + "/healthz") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self' wss://livekit.example https://livekit.example") {
		t.Fatalf("csp %q", csp)
	}
	// The room page, which sets its own policy for its import map, keeps LiveKit in reach.
	resp, err = http.Get(e.ts.URL + "/r/x") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self' wss://livekit.example https://livekit.example") ||
		!strings.Contains(csp, "script-src 'self' 'sha256-") {
		t.Fatalf("room page csp %q", csp)
	}
	resp, err = http.Get(newEnv(t).ts.URL + "/healthz") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if csp := resp.Header.Get("Content-Security-Policy"); strings.Contains(csp, "connect-src") {
		t.Fatalf("voice off csp %q", csp)
	}
}

// 001-AC7: a transcribed line lands as a voice message from the speaker, for everyone.
func TestVoiceLines(t *testing.T) {
	e, _ := newVoiceEnv(t)
	room := e.createRoom(t, "R")
	inv := e.createInvite(t, room.ID)
	alice := e.join(t, inv.Token, "Alice")
	bob := e.join(t, e.createInvite(t, room.ID).Token, "Bob")
	ev := e.openEvents(t, room.ID, bob.Token, "")
	nextMessage(t, ev)
	nextMessage(t, ev)
	if got := next(t, ev); got.event != "stt" {
		t.Fatalf("got %+v, want the stt state", got)
	}
	path := "/api/rooms/" + room.ID + "/voice/lines"
	code, b := e.do(t, "POST", path, sttToken, map[string]any{"participant_id": alice.Participant.ID, "body": "  Bonjour à tous.  "})
	if code != http.StatusCreated {
		t.Fatalf("line: %d %s", code, b)
	}
	m := nextMessage(t, ev)
	if m.Kind != "voice" || m.Sender != "Alice" || m.ParticipantID != alice.Participant.ID || m.Body != "Bonjour à tous." {
		t.Fatalf("message %+v", m)
	}

	other := e.createRoom(t, "Other")
	stranger := e.join(t, e.createInvite(t, other.ID).Token, "Eve")
	cases := []struct {
		name  string
		token string
		body  any
		code  int
	}{
		{"no token", "", map[string]any{"participant_id": alice.Participant.ID, "body": "x"}, http.StatusUnauthorized},
		{"admin token", e.admin, map[string]any{"participant_id": alice.Participant.ID, "body": "x"}, http.StatusUnauthorized},
		{"participant token", alice.Token, map[string]any{"participant_id": alice.Participant.ID, "body": "x"}, http.StatusUnauthorized},
		{"participant of another room", sttToken, map[string]any{"participant_id": stranger.Participant.ID, "body": "x"}, http.StatusNotFound},
		{"unknown participant", sttToken, map[string]any{"participant_id": 999, "body": "x"}, http.StatusNotFound},
		{"empty body", sttToken, map[string]any{"participant_id": alice.Participant.ID, "body": " "}, http.StatusBadRequest},
		{"control character", sttToken, map[string]any{"participant_id": alice.Participant.ID, "body": "a\x00b"}, http.StatusBadRequest},
		{"too long", sttToken, map[string]any{"participant_id": alice.Participant.ID, "body": strings.Repeat("a", maxBodyLen+1)}, http.StatusBadRequest},
	}
	for _, c := range cases {
		if code, b := e.do(t, "POST", path, c.token, c.body); code != c.code {
			t.Errorf("%s: %d %s, want %d", c.name, code, b, c.code)
		}
	}
	// A revoked speaker's line is dropped with their access.
	if code, _ := e.do(t, "DELETE", "/api/rooms/"+room.ID+"/invites/"+itoa(inv.ID), e.admin, nil); code != http.StatusOK {
		t.Fatal("revoke")
	}
	if code, _ := e.do(t, "POST", path, sttToken, map[string]any{"participant_id": alice.Participant.ID, "body": "x"}); code != http.StatusNotFound {
		t.Fatalf("revoked speaker: %d", code)
	}
	req, _ := http.NewRequestWithContext(t.Context(), "POST", e.ts.URL+path, strings.NewReader(`{"participant_id":2,"body":"x"}`))
	req.Header.Set("Authorization", "Bearer "+sttToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("no content type: %d", resp.StatusCode)
	}

	off := newEnv(t)
	if code, _ := off.do(t, "POST", path, sttToken, map[string]any{"participant_id": 1, "body": "x"}); code != http.StatusUnauthorized {
		t.Fatalf("voice off: %d", code)
	}
}

// 007-AC13, 007-AC14: the room learns each live worker's engine, and whether one reads the glossary.
func TestSTTHeartbeatEngine(t *testing.T) {
	oldPoll := pollEvery
	pollEvery = 20 * time.Millisecond
	t.Cleanup(func() { pollEvery = oldPoll })
	e, _ := newVoiceEnv(t)
	room := e.createRoom(t, "R")
	p := e.join(t, e.createInvite(t, room.ID).Token, "A")
	ev := e.openEvents(t, room.ID, p.Token, "")
	nextMessage(t, ev)
	sttOf(t, next(t, ev))
	beat := func(body map[string]any) {
		t.Helper()
		if code, b := e.do(t, "POST", "/api/stt/heartbeat", sttToken, body); code != http.StatusNoContent {
			t.Fatalf("heartbeat %v: %d %s", body, code, b)
		}
	}
	expect := func(what string, engines []string, glossary bool) {
		t.Helper()
		st := sttOf(t, next(t, ev))
		if !slices.Equal(st.Engines, engines) || st.Glossary != glossary || st.Engines == nil {
			t.Fatalf("%s: %+v, want engines %v glossary %v", what, st, engines, glossary)
		}
	}
	// A worker from before 007 names no engine: it reads no glossary.
	beat(map[string]any{"worker": "old"})
	expect("pre-007 worker", []string{}, false)
	beat(map[string]any{"worker": "w1", "engine": " parakeet ", "glossary": false})
	expect("parakeet", []string{"parakeet"}, false)
	beat(map[string]any{"worker": "w2", "engine": "whisper", "glossary": true})
	expect("whisper with the prompt on", []string{"parakeet", "whisper"}, true)
	beat(map[string]any{"worker": "w2", "engine": "whisper", "glossary": false})
	expect("whisper with the prompt off", []string{"parakeet", "whisper"}, false)

	for name, body := range map[string]any{
		"bidi engine":     map[string]any{"worker": "w", "engine": "a\u202eb"},
		"long engine":     map[string]any{"worker": "w", "engine": strings.Repeat("e", maxEngineName+1)},
		"multiline":       map[string]any{"worker": "w", "engine": "a\nb"},
		"engine not text": map[string]any{"worker": "w", "engine": 3},
	} {
		if code, _ := e.do(t, "POST", "/api/stt/heartbeat", sttToken, body); code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, code)
		}
	}
}

// 007-AC8: an utterance the STT queue drops shows in the room as a system line naming the speaker.
func TestVoiceDropped(t *testing.T) {
	e, _ := newVoiceEnv(t)
	room := e.createRoom(t, "R")
	alice := e.join(t, e.createInvite(t, room.ID).Token, "Alice")
	ev := e.openEvents(t, room.ID, alice.Token, "")
	nextMessage(t, ev)
	sttOf(t, next(t, ev))
	path := "/api/rooms/" + room.ID + "/voice/dropped"
	if code, b := e.do(t, "POST", path, sttToken, map[string]any{"participant_id": alice.Participant.ID}); code != http.StatusCreated {
		t.Fatalf("dropped: %d %s", code, b)
	}
	m := nextMessage(t, ev)
	if m.Kind != "system" || m.Sender != "Transcription" || m.Body != "Transcription overloaded: one line lost for Alice." {
		t.Fatalf("message %+v", m)
	}
	stranger := e.join(t, e.createInvite(t, e.createRoom(t, "Other").ID).Token, "Eve")
	for name, c := range map[string]struct {
		token string
		id    int64
		code  int
	}{
		"no token":           {"", alice.Participant.ID, http.StatusUnauthorized},
		"participant token":  {alice.Token, alice.Participant.ID, http.StatusUnauthorized},
		"admin token":        {e.admin, alice.Participant.ID, http.StatusUnauthorized},
		"unknown speaker":    {sttToken, 999, http.StatusNotFound},
		"another room's one": {sttToken, stranger.Participant.ID, http.StatusNotFound},
	} {
		if code, b := e.do(t, "POST", path, c.token, map[string]any{"participant_id": c.id}); code != c.code {
			t.Errorf("%s: %d %s, want %d", name, code, b, c.code)
		}
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func sttOf(t *testing.T, ev sseEvent) sttState {
	t.Helper()
	if ev.event != "stt" {
		t.Fatalf("got %+v, want an stt event", ev)
	}
	return decodeJSON[sttState](t, []byte(ev.data))
}

// 001-AC10: the room learns when transcription goes away and when it comes back. 005-AC8: and whose
// voices the workers transcribe, counted once over the workers, from those that say so.
func TestSTTHeartbeatState(t *testing.T) {
	oldTimeout, oldPoll := sttTimeout, pollEvery
	sttTimeout, pollEvery = 300*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { sttTimeout, pollEvery = oldTimeout, oldPoll })
	e, _ := newVoiceEnv(t)
	room := e.createRoom(t, "R")
	p := e.join(t, e.createInvite(t, room.ID).Token, "A")
	ev := e.openEvents(t, room.ID, p.Token, "")
	nextMessage(t, ev)
	expect := func(what string, want sttState) {
		t.Helper()
		if want.Transcribed == nil {
			want.Transcribed = []int64{}
		}
		if want.Engines == nil {
			want.Engines = []string{}
		}
		if st := sttOf(t, next(t, ev)); !st.equal(want) || st.Transcribed == nil {
			t.Fatalf("%s: %+v, want %+v", what, st, want)
		}
	}
	expect("no worker yet", sttState{})
	beatAs := func(worker string, voices map[string][]int64, rooms ...string) {
		t.Helper()
		body := map[string]any{"worker": worker, "rooms": rooms}
		if voices != nil {
			body["voices"] = voices
		}
		if code, b := e.do(t, "POST", "/api/stt/heartbeat", sttToken, body); code != http.StatusNoContent {
			t.Fatalf("heartbeat: %d %s", code, b)
		}
	}
	beat := func(rooms ...string) { t.Helper(); beatAs("w1", nil, rooms...) }
	beat()
	expect("worker up, not in the room", sttState{Available: true})
	beat(room.ID)
	expect("worker in the room", sttState{Available: true, Listening: true})
	if n := e.srv.STTWorkers(); n != 1 {
		t.Fatalf("workers %d", n)
	}
	beatAs("w1", map[string][]int64{room.ID: {99, p.Participant.ID}, "other": {9}}, room.ID)
	expect("two voices", sttState{Available: true, Listening: true, Transcribed: []int64{p.Participant.ID, 99}})
	beatAs("w2", map[string][]int64{room.ID: {99}}, room.ID)
	beatAs("w1", map[string][]int64{room.ID: {99}}, room.ID)
	expect("A's track gone, 99 on two workers", sttState{Available: true, Listening: true, Transcribed: []int64{99}})
	// No heartbeat: the workers are gone once sttTimeout passes.
	expect("workers gone", sttState{})
	if n := e.srv.STTWorkers(); n != 0 {
		t.Fatalf("workers after timeout %d", n)
	}
	beat(room.ID)
	expect("worker back", sttState{Available: true, Listening: true})

	for name, body := range map[string]any{
		"no worker":   map[string]any{"rooms": []string{}},
		"bidi worker": map[string]any{"worker": "a\u202eb"},
		"many rooms":  map[string]any{"worker": "w", "rooms": make([]string, maxSTTRooms+1)},
		"many voices": map[string]any{"worker": "w", "voices": map[string][]int64{"a": make([]int64, maxSTTVoices), "b": {1}}},
	} {
		if code, _ := e.do(t, "POST", "/api/stt/heartbeat", sttToken, body); code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, code)
		}
	}
	for _, tok := range []string{"", e.admin, p.Token} {
		if code, _ := e.do(t, "POST", "/api/stt/heartbeat", tok, map[string]any{"worker": "w"}); code != http.StatusUnauthorized {
			t.Errorf("token %q: %d", tok, code)
		}
	}
	for i := range maxSTTWorkers - 1 {
		if code, _ := e.do(t, "POST", "/api/stt/heartbeat", sttToken, map[string]any{"worker": "w" + itoa(int64(i+2))}); code != http.StatusNoContent {
			t.Fatalf("worker %d: %d", i, code)
		}
	}
	if code, _ := e.do(t, "POST", "/api/stt/heartbeat", sttToken, map[string]any{"worker": "one too many"}); code != http.StatusTooManyRequests {
		t.Fatalf("over the bound: %d", code)
	}
	beat(room.ID) // a known worker still beats at the bound
}

// Revoking an invite drops its participants from the audio; deleting a room closes its audio.
func TestRevokeAndDeleteLeaveVoice(t *testing.T) {
	e, lk := newVoiceEnv(t)
	room := e.createRoom(t, "R")
	inv := e.createInvite(t, room.ID)
	a := e.join(t, inv.Token, "A")
	b := e.join(t, inv.Token, "B")
	e.join(t, e.createInvite(t, room.ID).Token, "C")
	if code, _ := e.do(t, "DELETE", "/api/rooms/"+room.ID+"/invites/"+itoa(inv.ID), e.admin, nil); code != http.StatusOK {
		t.Fatal("revoke")
	}
	if code, _ := e.do(t, "DELETE", "/api/rooms/"+room.ID, e.admin, nil); code != http.StatusNoContent {
		t.Fatal("delete")
	}
	want := []string{
		`RemoveParticipant {"identity":"` + itoa(a.Participant.ID) + `","room":"` + room.ID + `"}`,
		`RemoveParticipant {"identity":"` + itoa(b.Participant.ID) + `","room":"` + room.ID + `"}`,
		`DeleteRoom {"room":"` + room.ID + `"}`,
	}
	if got := lk.got(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("LiveKit calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
