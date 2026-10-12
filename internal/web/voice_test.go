package web

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	if code, _ := e.do(t, "DELETE", "/api/rooms/"+room.ID+"/invites/"+itoa(inv.ID), e.admin, nil); code != http.StatusNoContent {
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

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func sttOf(t *testing.T, ev sseEvent) sttState {
	t.Helper()
	if ev.event != "stt" {
		t.Fatalf("got %+v, want an stt event", ev)
	}
	return decodeJSON[sttState](t, []byte(ev.data))
}

// 001-AC10: the room learns when transcription goes away and when it comes back.
func TestSTTHeartbeatState(t *testing.T) {
	oldTimeout, oldPoll := sttTimeout, sttPoll
	sttTimeout, sttPoll = 300*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { sttTimeout, sttPoll = oldTimeout, oldPoll })
	e, _ := newVoiceEnv(t)
	room := e.createRoom(t, "R")
	p := e.join(t, e.createInvite(t, room.ID).Token, "A")
	ev := e.openEvents(t, room.ID, p.Token, "")
	nextMessage(t, ev)
	if st := sttOf(t, next(t, ev)); st != (sttState{}) {
		t.Fatalf("no worker yet: %+v", st)
	}
	beat := func(rooms ...string) {
		t.Helper()
		if code, b := e.do(t, "POST", "/api/stt/heartbeat", sttToken, map[string]any{"worker": "w1", "rooms": rooms}); code != http.StatusNoContent {
			t.Fatalf("heartbeat: %d %s", code, b)
		}
	}
	beat()
	if st := sttOf(t, next(t, ev)); st != (sttState{Available: true}) {
		t.Fatalf("worker up, not in the room: %+v", st)
	}
	beat(room.ID)
	if st := sttOf(t, next(t, ev)); st != (sttState{Available: true, Listening: true}) {
		t.Fatalf("worker in the room: %+v", st)
	}
	if n := e.srv.STTWorkers(); n != 1 {
		t.Fatalf("workers %d", n)
	}
	// No heartbeat: the worker is gone once sttTimeout passes.
	if st := sttOf(t, next(t, ev)); st != (sttState{}) {
		t.Fatalf("worker gone: %+v", st)
	}
	if n := e.srv.STTWorkers(); n != 0 {
		t.Fatalf("workers after timeout %d", n)
	}
	beat(room.ID)
	if st := sttOf(t, next(t, ev)); st != (sttState{Available: true, Listening: true}) {
		t.Fatalf("worker back: %+v", st)
	}

	for name, body := range map[string]any{
		"no worker":   map[string]any{"rooms": []string{}},
		"bidi worker": map[string]any{"worker": "a\u202eb"},
		"many rooms":  map[string]any{"worker": "w", "rooms": make([]string, maxSTTRooms+1)},
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
	if code, _ := e.do(t, "DELETE", "/api/rooms/"+room.ID+"/invites/"+itoa(inv.ID), e.admin, nil); code != http.StatusNoContent {
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
