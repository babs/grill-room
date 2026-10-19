package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/babs/grill-room/internal/bus"
	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/store/storetest"
)

// device is a browser: a cookie jar against the test server.
type device struct {
	e *env
	c *http.Client
}

func (e *env) device(t *testing.T) *device {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &device{e: e, c: &http.Client{Jar: jar}}
}

// do sends a JSON request with the device's cookies and extra headers, and returns status and body.
func (d *device) do(t *testing.T, method, path string, body any, hdr ...string) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(t.Context(), method, d.e.ts.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := d.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (d *device) join(t *testing.T, room, name string) joinResp {
	t.Helper()
	code, b := d.do(t, "POST", "/api/join", map[string]string{"invite": d.e.createInvite(t, room).Token, "name": name})
	if code != http.StatusCreated {
		t.Fatalf("join: %d %s", code, b)
	}
	return decodeJSON[joinResp](t, b)
}

func (d *device) profile(t *testing.T) profileResp {
	t.Helper()
	code, b := d.do(t, "GET", "/api/profile", nil)
	if code != http.StatusOK {
		t.Fatalf("profile: %d %s", code, b)
	}
	return decodeJSON[profileResp](t, b)
}

// 002-AC1 002-AC2 002-AC3: the first join makes a profile and shows its recovery link once, a later
// join attaches to it, and the link signs another device into it, rooms included.
func TestProfileFollowsItsPersonAcrossRoomsAndDevices(t *testing.T) {
	e := newEnv(t)
	a, b, c := e.createRoom(t, "A"), e.createRoom(t, "B"), e.createRoom(t, "C")
	laptop := e.device(t)
	if got := laptop.profile(t); got.Profile != nil || got.EmailEnabled {
		t.Fatalf("before any join: %+v", got)
	}
	first := laptop.join(t, a.ID, "Alice")
	recovery, ok := strings.CutPrefix(first.RecoveryURL, baseURL+"/profile#recover=")
	if !ok || recovery == "" {
		t.Fatalf("recovery url: %q", first.RecoveryURL)
	}
	if second := laptop.join(t, b.ID, "Ali"); second.RecoveryURL != "" {
		t.Fatal("recovery link shown again on the second join")
	}
	prof := laptop.profile(t).Profile
	if prof == nil || len(prof.Rooms) != 2 || prof.Rooms[0].RoomID != b.ID || prof.Rooms[1].RoomID != a.ID || prof.Rooms[1].Name != "Alice" {
		t.Fatalf("profile rooms: %+v", prof)
	}
	if strings.Contains(string(mustJSON(t, first.Participant)), "profile") {
		t.Fatal("participant JSON exposes its profile")
	}

	phone := e.device(t)
	if code, _ := phone.do(t, "GET", "/api/rooms/"+a.ID, nil); code != http.StatusUnauthorized {
		t.Fatalf("phone before recovery: %d", code)
	}
	if code, _ := phone.do(t, "POST", "/api/profile/recover", map[string]string{"token": "nope"}); code != http.StatusUnauthorized {
		t.Fatalf("bad recovery: %d", code)
	}
	if code, _ := phone.do(t, "POST", "/api/profile/recover", map[string]string{"token": recovery}, "Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
		t.Fatalf("cross-site recovery: %d", code)
	}
	code, body := phone.do(t, "POST", "/api/profile/recover", map[string]string{"token": recovery})
	if code != http.StatusOK || decodeJSON[profileResp](t, body).Profile.ID != prof.ID {
		t.Fatalf("recover: %d %s", code, body)
	}
	// The phone now opens both rooms as the newest participant the profile has in each.
	for room, name := range map[string]string{a.ID: "Alice", b.ID: "Ali"} {
		code, body := phone.do(t, "GET", "/api/rooms/"+room, nil)
		if code != http.StatusOK || decodeJSON[roomResp](t, body).Me.Name != name {
			t.Fatalf("phone in %s: %d %s", room, code, body)
		}
	}
	if code, _ := phone.do(t, "POST", "/api/rooms/"+a.ID+"/messages", map[string]string{"body": "from the phone"}); code != http.StatusCreated {
		t.Fatalf("phone posts: %d", code)
	}
	if code, _ := phone.do(t, "POST", "/api/rooms/"+a.ID+"/messages", map[string]string{"body": "x"}, "Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
		t.Fatalf("cross-site post through the profile cookie: %d", code)
	}
	if code, _ := phone.do(t, "GET", "/api/rooms/"+c.ID, nil); code != http.StatusUnauthorized {
		t.Fatalf("a room the profile is not in: %d", code)
	}
	// The phone's event stream ends when the room drops the profile's participant.
	ch := phone.events(t, a.ID)
	_, invs := e.do(t, "GET", "/api/rooms/"+a.ID+"/invites", e.admin, nil)
	if code, _ := e.do(t, "DELETE", "/api/rooms/"+a.ID+"/invites/"+strconv.FormatInt(decodeJSON[[]store.Invite](t, invs)[0].ID, 10)+"?cut=1", e.admin, nil); code != http.StatusOK {
		t.Fatal("revoke failed")
	}
	waitRevoked(t, ch)
	if code, _ := phone.do(t, "GET", "/api/rooms/"+a.ID, nil); code != http.StatusUnauthorized {
		t.Fatalf("after the revoke: %d", code)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// events opens the room's event stream with the device's cookies; the channel carries raw lines.
func (d *device) events(t *testing.T, room string) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", d.e.ts.URL+"/api/rooms/"+room+"/events", nil)
	resp, err := d.c.Do(req) //nolint:bodyclose // the reader goroutine closes it
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("events: %v %v", resp, err)
	}
	ch := make(chan string, 256)
	go func() {
		defer resp.Body.Close()
		defer close(ch)
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				ch <- string(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
}

func waitRevoked(t *testing.T, ch <-chan string) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case s, ok := <-ch:
			if !ok {
				t.Fatal("stream closed without a revoked event")
			}
			if strings.Contains(s, "event: revoked") {
				return
			}
		case <-timeout:
			t.Fatal("no revoked event")
		}
	}
}

// mailbox collects what the server mails.
type mailbox struct{ ch chan [3]string }

func (m *mailbox) send(_ context.Context, to, subject, body string) error {
	m.ch <- [3]string{to, subject, body}
	return nil
}

var magicLink = regexp.MustCompile(regexp.QuoteMeta(baseURL) + `/profile#(?:magic|bind)=([A-Z0-9]+)`)

// next returns the next mail's recipient and link token.
func (m *mailbox) next(t *testing.T) (string, string) {
	t.Helper()
	select {
	case got := <-m.ch:
		tok := magicLink.FindStringSubmatch(got[2])
		if tok == nil {
			t.Fatalf("no link in %q", got[2])
		}
		return got[0], tok[1]
	case <-time.After(5 * time.Second):
		t.Fatal("no mail")
		return "", ""
	}
}

func (m *mailbox) none(t *testing.T) {
	t.Helper()
	select {
	case got := <-m.ch:
		t.Fatalf("unexpected mail to %s", got[0])
	case <-time.After(200 * time.Millisecond):
	}
}

func newMailEnv(t *testing.T, ttl time.Duration) (*env, *mailbox) {
	t.Helper()
	box := &mailbox{ch: make(chan [3]string, 16)}
	opts := testOptions
	opts.SendMail, opts.MagicLinkTTL = box.send, ttl
	return newEnvOpts(t, opts), box
}

// 002-AC4: without SMTP the option is off and every email route answers 404.
func TestEmailSignInOffWithoutSMTP(t *testing.T) {
	e := newEnv(t)
	d := e.device(t)
	d.join(t, e.createRoom(t, "A").ID, "Alice")
	if d.profile(t).EmailEnabled {
		t.Fatal("email offered without SMTP")
	}
	for _, path := range []string{"/api/profile/email", "/api/profile/signin", "/api/profile/magic"} {
		if code, _ := d.do(t, "POST", path, map[string]string{"email": "a@example.com", "token": "x"}); code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, code)
		}
	}
}

// 005-AC48: the profile sets its own name, checked as at join; its seats keep theirs.
func TestSetProfileName(t *testing.T) {
	e := newEnv(t)
	d := e.device(t)
	d.join(t, e.createRoom(t, "A").ID, "Alice")
	if code, _ := e.device(t).do(t, "PUT", "/api/profile/name", map[string]string{"name": "Bob"}); code != http.StatusUnauthorized {
		t.Fatalf("without a profile: %d", code)
	}
	if code, _ := d.do(t, "PUT", "/api/profile/name", map[string]string{"name": "Bob"}, "Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
		t.Fatalf("cross-site: %d", code)
	}
	for _, bad := range []string{"", "   ", strings.Repeat("x", maxNameLen+1), "a\nb", "a\u202eb"} {
		if code, _ := d.do(t, "PUT", "/api/profile/name", map[string]string{"name": bad}); code != http.StatusBadRequest {
			t.Errorf("name %q: %d, want 400", bad, code)
		}
	}
	if p := d.profile(t).Profile; p.Name != "" {
		t.Fatalf("a refused name was stored: %q", p.Name)
	}
	code, b := d.do(t, "PUT", "/api/profile/name", map[string]string{"name": "  " + strings.Repeat("é", maxNameLen) + " "})
	if want := strings.Repeat("é", maxNameLen); code != http.StatusOK || decodeJSON[map[string]string](t, b)["name"] != want {
		t.Fatalf("set: %d %s", code, b)
	}
	if code, _ := d.do(t, "PUT", "/api/profile/name", map[string]string{"name": "Bob"}); code != http.StatusOK {
		t.Fatalf("set: %d", code)
	}
	p := d.profile(t).Profile
	if p.Name != "Bob" || p.Rooms[0].Name != "Alice" {
		t.Fatalf("profile %q, seat %q: want Bob, Alice", p.Name, p.Rooms[0].Name)
	}
}

// 002-AC4: bind an email, sign in elsewhere with it, each link once, and the expiry.
func TestMagicLinks(t *testing.T) {
	e, box := newMailEnv(t, time.Minute)
	room := e.createRoom(t, "A")
	laptop := e.device(t)
	laptop.join(t, room.ID, "Alice")
	if !laptop.profile(t).EmailEnabled {
		t.Fatal("email not offered with SMTP")
	}
	if code, _ := e.device(t).do(t, "POST", "/api/profile/email", map[string]string{"email": "alice@example.com"}); code != http.StatusUnauthorized {
		t.Fatalf("bind without a profile: %d", code)
	}
	for _, bad := range []string{"", "alice", "Alice <alice@example.com>", "a@b@example.com", strings.Repeat("a", 250) + "@example.com"} {
		if code, _ := laptop.do(t, "POST", "/api/profile/email", map[string]string{"email": bad}); code != http.StatusBadRequest {
			t.Errorf("bind %q: %d, want 400", bad, code)
		}
	}
	if code, _ := laptop.do(t, "POST", "/api/profile/email", map[string]string{"email": " Alice@Example.com "}); code != http.StatusAccepted {
		t.Fatalf("bind: %d", code)
	}
	to, tok := box.next(t)
	if to != "alice@example.com" {
		t.Fatalf("mailed %s", to)
	}
	// A bind link only binds; elsewhere it asks for a confirm and stays usable meanwhile (011-AC23),
	// so it never signs anyone in.
	tablet := e.device(t)
	if code, _ := tablet.do(t, "POST", "/api/profile/magic", map[string]string{"token": tok, "purpose": "bind"}); code != http.StatusConflict {
		t.Fatalf("bind link in a browser without the profile: %d", code)
	}
	other := e.device(t)
	other.join(t, room.ID, "Mallory")
	if code, _ := other.do(t, "POST", "/api/profile/magic", map[string]string{"token": tok, "purpose": "bind"}); code != http.StatusConflict {
		t.Fatalf("bind link in another profile: %d", code)
	}
	if tablet.profile(t).Profile != nil || other.profile(t).Profile.Email != "" {
		t.Fatal("a refused bind link signed in or bound")
	}
	code, body := laptop.do(t, "POST", "/api/profile/magic", map[string]string{"token": tok, "purpose": "bind"})
	if code != http.StatusOK || decodeJSON[profileResp](t, body).Profile.Email != "alice@example.com" {
		t.Fatalf("bind link: %d %s", code, body)
	}
	if code, _ := laptop.do(t, "POST", "/api/profile/magic", map[string]string{"token": tok, "purpose": "bind"}); code != http.StatusGone {
		t.Fatalf("second use: %d", code)
	}

	phone := e.device(t)
	if code, _ := phone.do(t, "POST", "/api/profile/signin", map[string]string{"email": "alice@example.com"}); code != http.StatusAccepted {
		t.Fatalf("sign in: %d", code)
	}
	_, tok = box.next(t)
	// A sign-in link offered as a bind link, which the page spends without asking, is refused and
	// stays usable: a link someone else asked for cannot sign a browser in unasked.
	for _, purpose := range []string{"bind", "", "other"} {
		want := map[string]int{"bind": http.StatusUnauthorized, "": http.StatusBadRequest, "other": http.StatusBadRequest}[purpose]
		if code, _ := phone.do(t, "POST", "/api/profile/magic", map[string]string{"token": tok, "purpose": purpose}); code != want {
			t.Fatalf("sign-in link as %q: %d, want %d", purpose, code, want)
		}
	}
	if phone.profile(t).Profile != nil {
		t.Fatal("a sign-in link offered as a bind signed the phone in")
	}
	if code, _ := phone.do(t, "POST", "/api/profile/magic", map[string]string{"token": tok, "purpose": "signin"}, "Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
		t.Fatalf("cross-site link: %d", code)
	}
	if code, _ := phone.do(t, "POST", "/api/profile/magic", map[string]string{"token": tok, "purpose": "signin"}); code != http.StatusOK {
		t.Fatalf("sign-in link: %d", code)
	}
	if code, _ := phone.do(t, "GET", "/api/rooms/"+room.ID, nil); code != http.StatusOK {
		t.Fatalf("phone in the room: %d", code)
	}

	// An unknown address gets the same answer and no mail.
	if code, _ := phone.do(t, "POST", "/api/profile/signin", map[string]string{"email": "nobody@example.com"}); code != http.StatusAccepted {
		t.Fatalf("unknown address: %d", code)
	}
	box.none(t)

	// Another profile asking for Alice's address gets a link, which then refuses to steal it.
	bob := e.device(t)
	bob.join(t, room.ID, "Bob")
	if code, _ := bob.do(t, "POST", "/api/profile/email", map[string]string{"email": "alice@example.com"}); code != http.StatusAccepted {
		t.Fatalf("bob bind: %d", code)
	}
	_, tok = box.next(t)
	if code, body := bob.do(t, "POST", "/api/profile/magic", map[string]string{"token": tok, "purpose": "bind"}); code != http.StatusConflict || bytes.Contains(body, []byte("alice@")) {
		t.Fatalf("taken: %d %s", code, body)
	}
}

// fixedClock pins e's server clock to t0 plus what the returned setter sets.
func fixedClock(e *env) (t0 time.Time, set func(time.Duration)) {
	var skew atomic.Int64
	t0 = time.Now().UTC().Truncate(time.Second)
	e.srv.now = func() time.Time { return t0.Add(time.Duration(skew.Load())) }
	return t0, func(d time.Duration) { skew.Store(int64(d)) }
}

func TestMagicLinkExpires(t *testing.T) {
	e, box := newMailEnv(t, time.Minute)
	_, skew := fixedClock(e)
	d := e.device(t)
	d.join(t, e.createRoom(t, "A").ID, "Alice")
	if code, _ := d.do(t, "POST", "/api/profile/email", map[string]string{"email": "alice@example.com"}); code != http.StatusAccepted {
		t.Fatalf("bind: %d", code)
	}
	_, tok := box.next(t)
	skew(time.Minute)
	if code, _ := d.do(t, "POST", "/api/profile/magic", map[string]string{"token": tok, "purpose": "bind"}); code != http.StatusGone {
		t.Fatalf("expired link: %d", code)
	}
	if d.profile(t).Profile.Email != "" {
		t.Fatal("expired link bound the email")
	}
}

// 002-AC4: per client IP, per address from one IP, and per address overall; past a limit the
// answer is 429. The test client is on loopback, so it stands for a proxy: X-Forwarded-For names
// the client.
func TestMagicLinkRateLimits(t *testing.T) {
	e, box := newMailEnv(t, time.Minute)
	d := e.device(t)
	ask := func(email, ip string) int {
		code, _ := d.do(t, "POST", "/api/profile/signin", map[string]string{"email": email}, "X-Forwarded-For", ip)
		return code
	}
	for i := range magicPerAddressIP {
		if code := ask("same@example.com", "198.51.100.1"); code != http.StatusAccepted {
			t.Fatalf("request %d: %d", i, code)
		}
	}
	if code := ask("SAME@example.com", "198.51.100.1"); code != http.StatusTooManyRequests {
		t.Fatalf("past the address limit from one IP: %d", code)
	}
	// Someone else hammering an address does not lock its owner out at once.
	for i := magicPerAddressIP; i < magicPerAddress; i++ {
		if code := ask("same@example.com", "198.51.100."+strconv.Itoa(10+i)); code != http.StatusAccepted {
			t.Fatalf("address request %d from a new IP: %d", i, code)
		}
	}
	// Past the overall cap the address gets no more mail, from any IP.
	if code := ask("same@example.com", "198.51.100.99"); code != http.StatusTooManyRequests {
		t.Fatalf("past the address cap: %d", code)
	}
	for i := range magicPerIP {
		if code := ask("other"+strconv.Itoa(i)+"@example.com", "203.0.113.7"); code != http.StatusAccepted {
			t.Fatalf("ip request %d: %d", i, code)
		}
	}
	if code := ask("fresh@example.com", "203.0.113.7"); code != http.StatusTooManyRequests {
		t.Fatalf("past the ip limit: %d", code)
	}
	if code := ask("fresh@example.com", "203.0.113.8"); code != http.StatusAccepted {
		t.Fatalf("another ip: %d", code)
	}
	box.none(t) // no profile holds any of these addresses
}

// 002-AC5 004-AC3: a bound email reaches its owner and the admin's profiles view, never another
// participant, nor any other admin view, even once its owner posted, published and was audited.
func TestEmailStaysPrivate(t *testing.T) {
	e, box := newMailEnv(t, time.Minute)
	room := e.createRoom(t, "A")
	alice, bob := e.device(t), e.device(t)
	alice.join(t, room.ID, "Alice")
	bobJoin := bob.join(t, room.ID, "Bob")
	const email = "alice.private@example.com"
	alice.do(t, "POST", "/api/profile/email", map[string]string{"email": email})
	_, tok := box.next(t)
	if code, _ := alice.do(t, "POST", "/api/profile/magic", map[string]string{"token": tok, "purpose": "bind"}); code != http.StatusOK {
		t.Fatal("bind failed")
	}
	if alice.profile(t).Profile.Email != email {
		t.Fatal("owner does not see their email")
	}
	if code, _ := alice.do(t, "POST", "/api/rooms/"+room.ID+"/messages", map[string]string{"body": "ping"}); code != http.StatusCreated {
		t.Fatal("post")
	}
	critic := map[string]any{"name": "Critic", "badge": "🧐", "colour": "#112233", "text": "Find the flaw.", "model": "sonnet", "effort": "medium", "title": "Critic"}
	if code, b := alice.do(t, "POST", "/api/rooms/"+room.ID+"/templates", critic); code != http.StatusCreated {
		t.Fatalf("publish: %d %s", code, b)
	}
	other := e.createRoom(t, "B")
	carol := e.device(t)
	carol.join(t, other.ID, "Carol")
	carolID := strconv.FormatInt(carol.profile(t).Profile.ID, 10)
	if code, _ := e.do(t, "DELETE", "/api/profiles/"+carolID, e.admin, nil); code != http.StatusNoContent {
		t.Fatal("delete profile")
	}
	var seen []byte
	for _, path := range []string{"/api/rooms/" + room.ID, "/api/profile", "/api/rooms/" + room.ID + "/agent", "/api/rooms/" + room.ID + "/invites",
		"/api/rooms/" + room.ID + "/templates", "/api/rooms/" + room.ID + "/skills", "/api/rooms/" + room.ID + "/documents"} {
		_, b := bob.do(t, "GET", path, nil)
		seen = append(seen, b...)
	}
	for _, ev := range collect(e.openEvents(t, room.ID, bobJoin.Token, ""), 200*time.Millisecond) {
		seen = append(seen, ev.data...)
	}
	if !bytes.Contains(seen, []byte("ping")) || !bytes.Contains(seen, []byte("Critic")) {
		t.Fatalf("the participant views miss what Alice did: %s", seen)
	}
	if bytes.Contains(seen, []byte("alice.private")) {
		t.Fatalf("a participant sees another profile's email: %s", seen)
	}
	seen = nil
	for _, path := range []string{"/api/rooms", "/api/templates", "/api/skills", "/api/admin/rooms", "/api/admin/templates", "/api/admin/skills",
		"/api/admin/instance", "/api/admin/audit", "/api/admin/sessions", "/api/admin/rooms/" + room.ID + "/invites"} {
		code, b := e.do(t, "GET", path, e.admin, nil)
		if code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, code, b)
		}
		seen = append(seen, b...)
	}
	if !bytes.Contains(seen, []byte("profile.delete")) || bytes.Contains(seen, []byte("alice.private")) {
		t.Fatalf("an admin view other than profiles shows the email: %s", seen)
	}
	for _, path := range []string{"/api/profiles", "/api/admin/profiles"} {
		if _, b := e.do(t, "GET", path, e.admin, nil); !bytes.Contains(b, []byte(email)) {
			t.Fatalf("the profiles view %s lacks it: %s", path, b)
		}
	}
}

func collect(ch <-chan sseEvent, quiet time.Duration) []sseEvent {
	var out []sseEvent
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-time.After(quiet):
			return out
		}
	}
}

// Deleting a profile signs its devices out and leaves its participants in their rooms.
func TestAdminDeletesProfile(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "A")
	laptop := e.device(t)
	j := laptop.join(t, room.ID, "Alice")
	recovery := strings.TrimPrefix(j.RecoveryURL, baseURL+"/profile#recover=")
	id := laptop.profile(t).Profile.ID
	for _, bad := range []string{"abc", "99999999999999999999", "42"} {
		if code, _ := e.do(t, "DELETE", "/api/profiles/"+bad, e.admin, nil); code != http.StatusNotFound {
			t.Errorf("delete %s: %d", bad, code)
		}
	}
	if code, _ := e.do(t, "DELETE", "/api/profiles/"+strconv.FormatInt(id, 10), e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if laptop.profile(t).Profile != nil {
		t.Fatal("deleted profile still signed in")
	}
	if code, body := laptop.do(t, "GET", "/api/rooms/"+room.ID, nil); code != http.StatusOK || decodeJSON[roomResp](t, body).Me.Name != "Alice" {
		t.Fatalf("participant lost the room: %d %s", code, body)
	}
	if code, _ := e.device(t).do(t, "POST", "/api/profile/recover", map[string]string{"token": recovery}); code != http.StatusUnauthorized {
		t.Fatalf("recovery after delete: %d", code)
	}
	// The next join makes a new profile.
	if again := laptop.join(t, room.ID, "Alice"); again.RecoveryURL == "" {
		t.Fatal("no new profile after the delete")
	}
}

// createRoom creates a room from the device's profile page, as name, joining a lobby first when
// the device holds no profile yet (004-AC7).
func (d *device) createRoom(t *testing.T, room, name string) store.Room {
	t.Helper()
	if d.profile(t).Profile == nil {
		d.join(t, d.e.createRoom(t, "Lobby").ID, name)
	}
	code, b := d.do(t, "POST", "/api/profile/rooms", map[string]string{"name": room, "display_name": name})
	if code != http.StatusCreated {
		t.Fatalf("create room: %d %s", code, b)
	}
	return decodeJSON[store.Room](t, b)
}

// 002-AC6 004-AC8: a room admin renames, the super-admin too, and the room hears who renamed it
// from what to what; a plain member cannot any more.
func TestRenameRoom(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Pricing", "Alice")
	bob := e.join(t, e.createInvite(t, room.ID).Token, "Bob")
	stranger := e.join(t, e.createInvite(t, e.createRoom(t, "Other").ID).Token, "Mallory")
	ch := e.openEvents(t, room.ID, bob.Token, "")
	nextMessage(t, ch) // Alice joined
	nextMessage(t, ch) // Bob joined
	path := "/api/rooms/" + room.ID
	for _, bad := range []string{"", "  ", strings.Repeat("x", maxRoomName+1), "a\u202eb", "a\nb"} {
		if code, _ := alice.do(t, "PATCH", path, map[string]string{"name": bad}); code != http.StatusBadRequest {
			t.Errorf("rename to %q: %d, want 400", bad, code)
		}
	}
	if code, _ := e.do(t, "PATCH", path, stranger.Token, map[string]string{"name": "x"}); code != http.StatusUnauthorized {
		t.Fatalf("another room's member: %d", code)
	}
	if code, _ := e.do(t, "PATCH", path, bob.Token, map[string]string{"name": "x"}); code != http.StatusForbidden {
		t.Fatalf("plain member: %d", code)
	}
	const xss = `<img src=x onerror=alert(1)>`
	code, b := alice.do(t, "PATCH", path, map[string]string{"name": xss})
	if code != http.StatusOK || decodeJSON[store.Room](t, b).Name != xss {
		t.Fatalf("rename: %d %s", code, b)
	}
	if m := nextMessage(t, ch); m.Kind != store.KindSystem || m.Body != `Alice renamed the room from "Pricing" to "`+xss+`"` {
		t.Fatalf("system line: %+v", m)
	}
	if code, _ := e.do(t, "PATCH", path, e.admin, map[string]string{"name": "Pricing v2"}); code != http.StatusOK {
		t.Fatalf("admin rename: %d", code)
	}
	if m := nextMessage(t, ch); m.Body != `The admin renamed the room from "`+xss+`" to "Pricing v2"` {
		t.Fatalf("admin line: %+v", m)
	}
	if code, _ := e.do(t, "PATCH", "/api/rooms/nope", e.admin, map[string]string{"name": "x"}); code != http.StatusNotFound {
		t.Fatalf("admin, unknown room: %d", code)
	}
}

// 002-AC7: any member invites and revokes their own invites; the admin revokes any.
func TestMemberInvites(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "R")
	aliceInv, bobInv := e.createInvite(t, room.ID), e.createInvite(t, room.ID)
	alice, bob := e.join(t, aliceInv.Token, "Alice"), e.join(t, bobInv.Token, "Bob")
	path := "/api/rooms/" + room.ID + "/invites"
	code, b := e.do(t, "POST", path, alice.Token, nil)
	if code != http.StatusCreated {
		t.Fatalf("member invite: %d %s", code, b)
	}
	inv := decodeJSON[struct {
		invite
		CreatedBy int64 `json:"created_by"`
	}](t, b)
	if inv.CreatedBy != alice.Participant.ID || !strings.HasPrefix(inv.URL, baseURL+"/join#") {
		t.Fatalf("invite: %+v", inv)
	}
	carol := e.join(t, inv.Token, "Carol")
	if code, b := e.do(t, "GET", path, alice.Token, nil); code != http.StatusOK || len(decodeJSON[[]store.Invite](t, b)) != 1 {
		t.Fatalf("alice's invites: %d %s", code, b)
	}
	if code, b := e.do(t, "GET", path, bob.Token, nil); code != http.StatusOK || len(decodeJSON[[]store.Invite](t, b)) != 0 {
		t.Fatalf("bob's invites: %d %s", code, b)
	}
	if code, b := e.do(t, "GET", path, e.admin, nil); code != http.StatusOK || len(decodeJSON[[]store.Invite](t, b)) != 3 {
		t.Fatalf("admin's invites: %d %s", code, b)
	}
	one := path + "/" + strconv.FormatInt(inv.ID, 10)
	if code, _ := e.do(t, "DELETE", one, bob.Token, nil); code != http.StatusForbidden {
		t.Fatalf("bob revokes alice's invite: %d", code)
	}
	if code, _ := e.do(t, "DELETE", path+"/9999", alice.Token, nil); code != http.StatusNotFound {
		t.Fatalf("unknown invite: %d", code)
	}
	if code, _ := e.do(t, "DELETE", path+"/"+strconv.FormatInt(aliceInv.ID, 10), alice.Token, nil); code != http.StatusForbidden {
		t.Fatalf("alice revokes the admin's invite: %d", code)
	}
	// 016-AC5: removing people is a room admin's act.
	if code, b := e.do(t, "DELETE", one+"?cut=1", alice.Token, nil); code != http.StatusForbidden || !strings.Contains(string(b), "Only a room admin can remove people") {
		t.Fatalf("alice cuts with hers: %d %s", code, b)
	}
	if code, _ := e.do(t, "DELETE", one, alice.Token, nil); code != http.StatusOK {
		t.Fatalf("alice revokes hers: %d", code)
	}
	// 016-AC3: a revoke stops new joins; who joined through it stays.
	if code, _ := e.do(t, "GET", "/api/rooms/"+room.ID, carol.Token, nil); code != http.StatusOK {
		t.Fatalf("carol after the revoke: %d", code)
	}
	if code, _ := e.do(t, "DELETE", path+"/"+strconv.FormatInt(bobInv.ID, 10), e.admin, nil); code != http.StatusOK {
		t.Fatalf("admin revokes: %d", code)
	}
	other := e.join(t, e.createInvite(t, e.createRoom(t, "Other").ID).Token, "Mallory")
	if code, _ := e.do(t, "POST", path, other.Token, nil); code != http.StatusUnauthorized {
		t.Fatalf("another room's member invites: %d", code)
	}
}

// A relay's error names the recipient: the log line must not.
func TestSendFailureLogHidesTheAddress(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	opts := testOptions
	done := make(chan struct{})
	opts.SendMail = func(_ context.Context, to, _, _ string) error {
		defer close(done)
		return errors.New("rcpt to: 550 5.1.1 <" + to + ">: unknown user")
	}
	st, err := store.Open(storetest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(bus.New(st, NewToken()), "", baseURL, opts, zap.New(core))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	e := &env{ts: ts, st: st, srv: srv}
	d := e.device(t)
	room, _ := st.CreateRoom(t.Context(), "r", "R")
	if _, err := st.CreateInvite(t.Context(), room.ID, HashToken("inv"), 0); err != nil {
		t.Fatal(err)
	}
	if code, _ := d.do(t, "POST", "/api/join", map[string]string{"invite": "inv", "name": "Alice"}); code != http.StatusCreated {
		t.Fatal("join failed")
	}
	// 011-AC28: the bind mail goes out before the answer, so the failure is logged by then.
	if code, b := d.do(t, "POST", "/api/profile/email", map[string]string{"email": "secret@example.com"}); code != http.StatusBadGateway || bytes.Contains(b, []byte("secret@")) {
		t.Fatalf("bind: %d %s", code, b)
	}
	<-done
	line := logs.FilterMessage("magic_link_send_failed").All()
	if len(line) != 1 {
		t.Fatal("no failure logged")
	}
	if got := line[0].ContextMap()["error"]; strings.Contains(fmt.Sprint(got), "secret@") || !strings.Contains(fmt.Sprint(got), "<recipient>") {
		t.Fatalf("logged %v", got)
	}
}

// A browser whose room cookie lost its participant still enters through its profile, when the
// profile has another participant in the room; a bearer token gets no such fallback.
func TestStaleRoomCookieFallsBackToTheProfile(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "R")
	laptop := e.device(t)
	first := laptop.join(t, room.ID, "Alice")
	recovery := strings.TrimPrefix(first.RecoveryURL, baseURL+"/profile#recover=")
	phone := e.device(t)
	if code, _ := phone.do(t, "POST", "/api/profile/recover", map[string]string{"token": recovery}); code != http.StatusOK {
		t.Fatal("recover failed")
	}
	again := phone.join(t, room.ID, "Alice on the phone")
	_, invs := e.do(t, "GET", "/api/rooms/"+room.ID+"/invites", e.admin, nil)
	if code, _ := e.do(t, "DELETE", "/api/rooms/"+room.ID+"/invites/"+strconv.FormatInt(decodeJSON[[]store.Invite](t, invs)[0].ID, 10)+"?cut=1", e.admin, nil); code != http.StatusOK {
		t.Fatal("revoke failed")
	}
	if code, body := laptop.do(t, "GET", "/api/rooms/"+room.ID, nil); code != http.StatusOK || decodeJSON[roomResp](t, body).Me.ID != again.Participant.ID {
		t.Fatalf("laptop through its profile: %d %s", code, body)
	}
	if code, _ := e.do(t, "GET", "/api/rooms/"+room.ID, first.Token, nil); code != http.StatusUnauthorized {
		t.Fatalf("revoked bearer token: %d", code)
	}
}

// The event stream re-checks access the way the request did: room cookie, then profile.
func TestStreamSurvivesARevokedRoomCookieWithAValidProfile(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "R")
	laptop := e.device(t)
	first := laptop.join(t, room.ID, "Alice")
	phone := e.device(t)
	if code, _ := phone.do(t, "POST", "/api/profile/recover", map[string]string{"token": strings.TrimPrefix(first.RecoveryURL, baseURL+"/profile#recover=")}); code != http.StatusOK {
		t.Fatal("recover failed")
	}
	phone.join(t, room.ID, "Alice on the phone")
	ch := laptop.events(t, room.ID)
	_, invs := e.do(t, "GET", "/api/rooms/"+room.ID+"/invites", e.admin, nil)
	if code, _ := e.do(t, "DELETE", "/api/rooms/"+room.ID+"/invites/"+strconv.FormatInt(decodeJSON[[]store.Invite](t, invs)[0].ID, 10)+"?cut=1", e.admin, nil); code != http.StatusOK {
		t.Fatal("revoke failed")
	}
	if code, _ := phone.do(t, "POST", "/api/rooms/"+room.ID+"/messages", map[string]string{"body": "still here"}); code != http.StatusCreated {
		t.Fatal("post failed")
	}
	timeout := time.After(5 * time.Second)
	for {
		select {
		case s, ok := <-ch:
			if !ok || strings.Contains(s, "event: revoked") {
				t.Fatalf("stream ended: %q", s)
			}
			if strings.Contains(s, "still here") {
				return
			}
		case <-timeout:
			t.Fatal("message never arrived")
		}
	}
}

// 002-AC7: a member cut off by a revoke cannot come back through an invite they created.
func TestRevokedMemberCannotRejoinThroughTheirOwnInvite(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "R")
	inv := e.createInvite(t, room.ID)
	alice := e.join(t, inv.Token, "Alice")
	code, b := e.do(t, "POST", "/api/rooms/"+room.ID+"/invites", alice.Token, nil)
	if code != http.StatusCreated {
		t.Fatal("member invite failed")
	}
	spare := decodeJSON[invite](t, b)
	if code, _ := e.do(t, "DELETE", "/api/rooms/"+room.ID+"/invites/"+strconv.FormatInt(inv.ID, 10)+"?cut=1", e.admin, nil); code != http.StatusOK {
		t.Fatal("revoke failed")
	}
	if code, _ := e.do(t, "POST", "/api/join", "", map[string]string{"invite": spare.Token, "name": "Alice"}); code != http.StatusUnauthorized {
		t.Fatalf("rejoin through her own invite: %d", code)
	}
}

// An unreadable "mail failing" flag is a hint lost, not a failed sign-in: the request goes on.
func TestMailFlagUnreadable(t *testing.T) {
	a, _, _ := newRelayPair(t)
	if err := a.st.SetSetting(t.Context(), mailFailedAtKey, "not-a-time"); err != nil {
		t.Fatal(err)
	}
	if code, _, m := post(t, a, "/api/profile/signin", `{"email":"nobody@example.com"}`); code != http.StatusAccepted {
		t.Fatalf("sign-in: %d %v", code, m)
	}
}
