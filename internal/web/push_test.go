package web

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/webpush"
)

// pushBrowser stands in for a browser and its push service: a TLS endpoint that keeps what it
// gets, and the subscription keys to decrypt it with.
type pushBrowser struct {
	srv    *httptest.Server
	priv   *ecdh.PrivateKey
	auth   []byte
	status int
	reply  string
	mu     sync.Mutex
	got    []pushPayload
	auths  []string
	// clock is the server's time; read, the last line its push loop read; lastLine, the newest.
	clock    time.Time
	read     int64
	lastLine func() int64
	// sent counts the batches the push loop sent; seen is the clock of its last tick.
	sent int
	seen time.Time
}

func (b *pushBrowser) ticked(now time.Time, sent int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent += sent
	b.seen = now
}

// flushed says whether a tick ran at the clock's current time, and how many batches went in all.
func (b *pushBrowser) flushed() (bool, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.seen.Before(b.clock), b.sent
}

func (b *pushBrowser) now() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.clock
}

func (b *pushBrowser) advance(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clock = b.clock.Add(d)
}

func (b *pushBrowser) noteRead(last int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.read = max(b.read, last)
}

func (b *pushBrowser) readUpTo() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.read
}

func newPushBrowser(t *testing.T) *pushBrowser {
	t.Helper()
	priv, _ := ecdh.P256().GenerateKey(rand.Reader)
	b := &pushBrowser{priv: priv, auth: make([]byte, 16), status: http.StatusCreated}
	_, _ = rand.Read(b.auth)
	b.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		plain, err := webpush.Decrypt(b.priv, b.auth, body)
		if err != nil {
			t.Errorf("push body: %v", err)
		}
		var p pushPayload
		if err := json.Unmarshal(plain, &p); err != nil {
			t.Errorf("payload %s: %v", plain, err)
		}
		// The payload holds nothing but what pushPayload declares.
		var raw map[string]any
		_ = json.Unmarshal(plain, &raw)
		if len(raw) != 4 {
			t.Errorf("payload fields %v", raw)
		}
		b.mu.Lock()
		b.got = append(b.got, p)
		b.auths = append(b.auths, r.Header.Get("Authorization"))
		status, reply := b.status, b.reply
		b.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *pushBrowser) subscription(path string) map[string]any {
	enc := base64.RawURLEncoding.EncodeToString
	return map[string]any{"endpoint": b.srv.URL + path, "keys": map[string]string{"p256dh": enc(b.priv.PublicKey().Bytes()), "auth": enc(b.auth)}}
}

func (b *pushBrowser) pushes() []pushPayload {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]pushPayload(nil), b.got...)
}

// pushEnv is a server that sends to b, its push loop running.
func pushEnv(t *testing.T, b *pushBrowser) *env {
	t.Helper()
	opts := testOptions
	opts.PushHosts, opts.PushClient = []string{"127.0.0.1"}, b.srv.Client()
	e := newEnvOpts(t, opts)
	oldWindow, oldTick := pushWindow, pushTick
	pushWindow, pushTick = 150*time.Millisecond, 20*time.Millisecond
	// The batches run on a clock the test moves: what is said before a move is one batch, however
	// slow the machine (a 150 ms window once split two mentions on a Windows runner).
	b.clock = time.Now()
	e.srv.now = b.now
	e.srv.pushRead = b.noteRead
	e.srv.pushTicked = b.ticked
	b.lastLine = func() int64 {
		id, err := e.st.LastMessageID(context.Background())
		if err != nil {
			t.Error(err)
		}
		return id
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.srv.PushLoop(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		pushWindow, pushTick = oldWindow, oldTick
	})
	return e
}

// waitPushes waits until b got n pushes, then a little longer to see no more come.
func waitPushes(t *testing.T, b *pushBrowser, n int) []pushPayload {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	// Polls observable state, bounded by the deadline: no wall-clock assertion.
	wait := func(what string, done func() bool) {
		for !done() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(pushTick) // polls, bounded by the deadline
		}
	}
	// Every line said so far is read; the clock moves past the window; a tick runs at that time and
	// sends every batch due; the browser gets each one sent.
	wait("the push loop to read every line", func() bool { return b.readUpTo() >= b.lastLine() })
	b.advance(pushWindow)
	wait("a tick past the window", func() bool { ok, _ := b.flushed(); return ok })
	_, sent := b.flushed()
	wait("the pushes sent", func() bool { return len(b.pushes()) >= sent })
	got := b.pushes()
	if len(got) != n {
		t.Fatalf("%d pushes, want %d: %+v", len(got), n, got)
	}
	return got
}

// 005-AC24: the subscription API checks the subscription and its push service; the VAPID key is
// generated once and kept in the instance data.
func TestPushSubscribeAPI(t *testing.T) {
	b := newPushBrowser(t)
	e := pushEnv(t, b)
	room := e.createRoom(t, "Pricing")
	alice := e.join(t, e.createInvite(t, room.ID).Token, "Alice")
	path := "/api/rooms/" + room.ID + "/push"
	code, body := e.do(t, "GET", path, alice.Token, nil)
	key := decodeJSON[map[string]string](t, body)["key"]
	if raw, err := base64.RawURLEncoding.DecodeString(key); code != http.StatusOK || err != nil || len(raw) != 65 || raw[0] != 4 {
		t.Fatalf("key: %d %s", code, body)
	}
	stored, _ := e.st.Setting(t.Context(), vapidSetting)
	e2 := New(e.bus, "", baseURL, testOptions, e.srv.log)
	if v, err := e2.vapidKey(t.Context()); err != nil || v.PublicKey() != key || stored == "" {
		t.Fatalf("a second server instance has another key: %v", err)
	}
	bad := func(edit func(map[string]any), want string) {
		t.Helper()
		sub := b.subscription("/p")
		edit(sub)
		code, body := e.do(t, "PUT", path, alice.Token, map[string]any{"subscription": sub})
		if code != http.StatusBadRequest || !strings.Contains(string(body), want) {
			t.Errorf("%v: %d %s, want 400 %q", sub, code, body, want)
		}
	}
	bad(func(s map[string]any) { s["endpoint"] = "https://169.254.169.254/latest" }, "not one the instance sends to")
	bad(func(s map[string]any) { s["endpoint"] = "https://fcm.googleapis.com.evil.example/x" }, "not one the instance sends to")
	bad(func(s map[string]any) { s["endpoint"] = "http://127.0.0.1/x" }, "https URL")
	bad(func(s map[string]any) { s["keys"] = map[string]string{"p256dh": "AAAA", "auth": "AAAA"} }, "P-256")
	if code, body := e.do(t, "PUT", path, alice.Token, map[string]any{"subscription": b.subscription("/p"), "all_messages": true}); code != http.StatusNoContent {
		t.Fatalf("subscribe: %d %s", code, body)
	}
	subs, _ := e.st.PushSubscriptions(t.Context(), room.ID)
	if len(subs) != 1 || !subs[0].AllMessages || subs[0].ParticipantID != alice.Participant.ID {
		t.Fatalf("stored %+v", subs)
	}
	if code, _ := e.do(t, "DELETE", path, alice.Token, map[string]any{"subscription": map[string]string{"endpoint": b.srv.URL + "/p"}}); code != http.StatusNoContent {
		t.Fatalf("unsubscribe: %d", code)
	}
	if subs, _ := e.st.PushSubscriptions(t.Context(), room.ID); len(subs) != 0 {
		t.Fatalf("left %+v", subs)
	}
	if code, _ := e.do(t, "GET", path, "", nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous key: %d", code)
	}
}

func TestPushHostAllowed(t *testing.T) {
	for host, want := range map[string]bool{
		"https://fcm.googleapis.com/fcm/send/x":          true,
		"https://web.push.apple.com/abc":                 true,
		"https://FCM.googleapis.com/x":                   true,
		"https://wns2-par02p.notify.windows.com/w/?t=1":  true,
		"https://evilfcm.googleapis.com.example/x":       false,
		"https://notfcm.googleapis.com/x":                false,
		"https://fcm.googleapis.com@127.0.0.1/x":         false,
		"https://updates.push.services.mozilla.com:8443": true,
		"::bad": false,
	} {
		if got := pushHostAllowed(host, DefaultPushHosts); got != want {
			t.Errorf("%s: %v", host, got)
		}
	}
}

// 005-AC24: pushes go out on the default triggers (a mention, a synthesis, the end of a turn one
// started) and on any line once opted in; never while one of the participant's tabs is focused,
// never on their own lines. Events of one room gather into one push, which names the room and the
// kind of event and links to it, never the text. A subscription the push service says is gone or
// made with another VAPID key is dropped; one refused for the server's own VAPID setup stays.
func TestPushDispatch(t *testing.T) {
	b := newPushBrowser(t)
	e := pushEnv(t, b)
	room := e.createRoom(t, "Pricing")
	alice := e.join(t, e.createInvite(t, room.ID).Token, "Ana María")
	bob := e.join(t, e.createInvite(t, room.ID).Token, "Bob")
	path := "/api/rooms/" + room.ID
	if code, _ := e.do(t, "PUT", path+"/push", alice.Token, map[string]any{"subscription": b.subscription("/alice")}); code != http.StatusNoContent {
		t.Fatalf("subscribe: %d", code)
	}
	say := func(tok, body string) {
		t.Helper()
		if code, b := e.do(t, "POST", path+"/messages", tok, map[string]string{"body": body}); code != http.StatusCreated {
			t.Fatalf("post: %d %s", code, b)
		}
	}
	say(bob.Token, "plain line")
	say(alice.Token, "@Bob mine")
	say(bob.Token, "`@Ana_María` quoted as code")
	waitPushes(t, b, 0)

	say(bob.Token, "@Ana_María the secret plan")
	say(bob.Token, "@ana_maría again")
	got := waitPushes(t, b, 1)
	last := e.lastLine(t, room.ID)
	want := pushPayload{Room: "Pricing", Kind: pushMention, Count: 2, URL: fmt.Sprintf("/r/%s#m%d", room.ID, last.ID)}
	if last.Body != "@ana_maría again" || got[0] != want || !strings.HasPrefix(b.auths[0], "vapid t=") {
		t.Fatalf("push %+v %q, want %+v", got[0], b.auths[0], want)
	}

	// A synthesis reaches her; her focused tab holds everything back.
	if err := e.bus.Post(t.Context(), &store.Message{RoomID: room.ID, Kind: store.KindAgent, Sender: "Grill", Body: "synthesis"}); err != nil {
		t.Fatal(err)
	}
	got = waitPushes(t, b, 2)
	if got[1].Kind != pushSynthesis || got[1].Count != 1 {
		t.Fatalf("synthesis push %+v", got[1])
	}
	beat := func(focused bool) {
		t.Helper()
		if code, _ := e.do(t, "POST", path+"/presence", alice.Token, map[string]any{"tab": "t1", "focused": focused}); code != http.StatusNoContent {
			t.Fatalf("beat: %d", code)
		}
	}
	beat(true)
	say(bob.Token, "@Ana_María while you look")
	waitPushes(t, b, 2)
	beat(false)
	// Back in the room before the batch goes: she has seen it.
	say(bob.Token, "@Ana_María just before you look")
	beat(true)
	waitPushes(t, b, 2)
	beat(false)

	// Opted in to every line.
	if code, _ := e.do(t, "PUT", path+"/push", alice.Token, map[string]any{"subscription": b.subscription("/alice"), "all_messages": true}); code != http.StatusNoContent {
		t.Fatal("resubscribe")
	}
	say(bob.Token, "plain line again")
	if got = waitPushes(t, b, 3); got[2].Kind != pushMessage {
		t.Fatalf("opted-in push %+v", got[2])
	}

	refuse := func(status int, reply string) {
		b.mu.Lock()
		b.status, b.reply = status, reply
		b.mu.Unlock()
	}
	// A 401 that blames the server's own VAPID setup keeps the subscription: the next push goes.
	refuse(http.StatusUnauthorized, `{"errno": 108, "message": "Invalid JWT"}`)
	say(bob.Token, "@Ana_María one more")
	waitPushes(t, b, 4)
	// A 401 for another VAPID key, as after a key rotation: the next push drops the subscription.
	refuse(http.StatusUnauthorized, `{"code": 401, "errno": 109, "message": "VAPID public key mismatch"}`)
	say(bob.Token, "@Ana_María after a rotation")
	waitPushes(t, b, 5)
	// The send drops it once the 401 is back, after the browser counted the push.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(pushTick) { // polls, bounded by the deadline
		subs, err := e.st.PushSubscriptions(t.Context(), room.ID)
		if err == nil && len(subs) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a rejected subscription stayed: %+v %v", subs, err)
		}
	}
}

// 005-AC24: a turn that ends without a synthesis, failed or stopped, notifies whoever started it,
// and nobody else.
func TestPushKind(t *testing.T) {
	sub := store.PushSubscription{ParticipantID: 7}
	allSub := store.PushSubscription{ParticipantID: 7, AllMessages: true}
	gone := time.Now()
	for _, c := range []struct {
		name      string
		m         store.Message
		sub       store.PushSubscription
		starter   int64
		mentioned bool
		want      string
	}{
		{"failed turn, starter", store.Message{Kind: store.KindSystem, TurnID: 3}, sub, 7, false, pushTurn},
		{"failed turn, other", store.Message{Kind: store.KindSystem, TurnID: 3}, sub, 8, false, ""},
		{"join line", store.Message{Kind: store.KindSystem, ParticipantID: 8}, allSub, 0, false, ""},
		{"own line", store.Message{Kind: store.KindText, ParticipantID: 7}, allSub, 0, true, ""},
		{"deleted", store.Message{Kind: store.KindText, ParticipantID: 8, DeletedAt: &gone}, allSub, 0, true, ""},
		{"voice, opted in", store.Message{Kind: store.KindVoice, ParticipantID: 8}, allSub, 0, false, pushMessage},
		{"voice", store.Message{Kind: store.KindVoice, ParticipantID: 8}, sub, 0, false, ""},
		{"synthesis", store.Message{Kind: store.KindAgent}, sub, 0, false, pushSynthesis},
	} {
		if got := pushKind(c.m, c.sub, c.starter, c.mentioned); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if got := pushTopic("ab-c_D.e/" + strings.Repeat("x", 40)); got != "ab-c_De"+strings.Repeat("x", 25) {
		t.Errorf("topic %q", got)
	}
}

// A push service answering with a redirect is not followed: the post would go to a host the
// allowlist never saw.
func TestPushClientFollowsNoRedirect(t *testing.T) {
	followed := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed = true }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("x"))
	resp, err := newPushClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if followed || resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("followed %v, status %d", followed, resp.StatusCode)
	}
}

// A stored VAPID key that does not parse is an error, not a new key: the browsers subscribed with
// the old one would stop getting pushes.
func TestPushCorruptKey(t *testing.T) {
	b := newPushBrowser(t)
	e := pushEnv(t, b)
	room := e.createRoom(t, "Pricing")
	alice := e.join(t, e.createInvite(t, room.ID).Token, "Alice")
	if err := e.st.SetSetting(t.Context(), vapidSetting, "not base64!"); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.do(t, "GET", "/api/rooms/"+room.ID+"/push", alice.Token, nil); code != http.StatusInternalServerError {
		t.Fatalf("corrupt key: %d", code)
	}
	if v, _ := e.st.Setting(t.Context(), vapidSetting); v != "not base64!" {
		t.Fatalf("the stored key was replaced: %q", v)
	}
}
