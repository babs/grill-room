package web

import (
	"cmp"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/babs/grill-room/internal/livekit"
	"github.com/babs/grill-room/internal/store"
)

// reauthCase is a credential of an internal and the request it makes; expired is the status it
// gets past the window, 401 when 0.
type reauthCase struct {
	name    string
	do      func(t *testing.T) (int, []byte)
	expired int
}

// checkAll asserts every case answers 2xx, or its expired status with reason reauth when expired.
func checkAll(t *testing.T, when string, cases []reauthCase, expired bool) {
	t.Helper()
	for _, c := range cases {
		code, b := c.do(t)
		want := cmp.Or(c.expired, http.StatusUnauthorized)
		reauth := code == want && strings.Contains(string(b), `"reason":"reauth"`)
		if expired && !reauth || !expired && code/100 != 2 {
			t.Errorf("%s, %s: %d %s", when, c.name, code, b)
		}
	}
}

// 021-AC8: one second past the window, the profile cookie, a seat, its paired daemon and an API
// token of an internal answer 401 reauth on every route, and its open streams close with reauth,
// its voice dropped; nothing is deleted, a new sign-in restores them, an external is never
// affected, and with the mode off nothing changes.
func TestReauthAfterWindow(t *testing.T) {
	idp := newTestIdP(t)
	e := newOIDCEnv(t, idp, zap.NewNop(), nil)
	lk := &fakeLiveKit{}
	lts := httptest.NewServer(lk)
	t.Cleanup(lts.Close)
	e.srv.opts.Voice = &Voice{URL: "wss://livekit.example", Key: "APIkey", Secret: lkSecret,
		API: &livekit.Client{URL: lts.URL, Key: "APIkey", Secret: lkSecret, HTTP: lts.Client()}}
	clk := e.clock()
	room := e.createRoom(t, "R").ID
	ines := e.device(t)
	if resp, _ := ines.signIn(t, idp, map[string]any{"sub": "ines"}); resp.code != http.StatusSeeOther {
		t.Fatalf("sign-in %d", resp.code)
	}
	seat := ines.join(t, room, "Ines")
	api := ines.newToken(t, store.ScopeFull, 0)
	pairing := e.requestPairing(t, room, "ines-laptop")
	if code, b := e.do(t, "POST", "/api/rooms/"+room+"/pairings/"+pairing.ID, seat.Token, map[string]bool{"approve": true}); code != http.StatusOK {
		t.Fatalf("approve: %d %s", code, b)
	}
	_, paired := e.poll(t, pairing)
	// Another browser of hers, by its profile cookie alone.
	laptop := e.device(t)
	if resp, _ := laptop.signIn(t, idp, map[string]any{"sub": "ines"}); resp.code != http.StatusSeeOther {
		t.Fatalf("laptop sign-in %d", resp.code)
	}
	ext := e.device(t)
	ext.join(t, room, "Ezra")
	roomPath := "/api/rooms/" + room
	cases := []reauthCase{
		{"profile cookie on /api/profile", func(t *testing.T) (int, []byte) { return laptop.do(t, "GET", "/api/profile", nil) }, 0},
		{"profile cookie on a profile route", func(t *testing.T) (int, []byte) { return laptop.do(t, "GET", "/api/profile/tokens", nil) }, 0},
		{"profile cookie on the room", func(t *testing.T) (int, []byte) { return laptop.do(t, "GET", roomPath, nil) }, 0},
		{"seat token", func(t *testing.T) (int, []byte) { return e.do(t, "GET", roomPath, seat.Token, nil) }, 0},
		{"seat and profile cookies", func(t *testing.T) (int, []byte) { return ines.do(t, "GET", roomPath, nil) }, 0},
		{"API token on /api/profile", func(t *testing.T) (int, []byte) { return e.do(t, "GET", "/api/profile", api.Token, nil) }, 0},
		{"API token on the room", func(t *testing.T) (int, []byte) { return e.do(t, "GET", roomPath, api.Token, nil) }, 0},
		// 403: a daemon wipes its pairing on a 401.
		{"paired daemon", func(t *testing.T) (int, []byte) {
			return e.do(t, "POST", roomPath+"/daemon/status", paired.Token, map[string]any{"can_host": false, "reason": "idle"})
		}, http.StatusForbidden},
	}
	external := []reauthCase{
		{"external profile", func(t *testing.T) (int, []byte) { return ext.do(t, "GET", "/api/profile", nil) }, 0},
		{"external seat", func(t *testing.T) (int, []byte) { return ext.do(t, "GET", roomPath, nil) }, 0},
	}
	daemonEvents, _ := e.openStream(t, roomPath+"/daemon/events", paired.Token, "")
	streams := map[string]<-chan sseEvent{"seat": e.openEvents(t, room, seat.Token, ""), "API token": e.openEvents(t, room, api.Token, "")}

	clk.add(DefaultOIDCRevalidate - time.Second)
	checkAll(t, "within the window", append(cases, external...), false)
	clk.add(2 * time.Second)
	checkAll(t, "past the window", cases, true)
	checkAll(t, "past the window", external, false)
	// The heartbeat's re-check, as any wake-up, closes the stream.
	e.bus.Notify(room)
	for name, ch := range streams {
		if ev := nextEvent(t, ch, "revoked"); ev.data != `{"reason":"reauth"}` {
			t.Errorf("%s stream: %+v", name, ev)
		}
	}
	// The daemon's stream closes without "revoked", which would wipe its pairing.
	for closed := false; !closed; {
		select {
		case ev, ok := <-daemonEvents:
			if ev.event == "revoked" {
				t.Errorf("daemon stream: %+v", ev)
			}
			closed = !ok
		case <-time.After(5 * time.Second):
			t.Fatal("daemon stream still open")
		}
	}
	if kick := `RemoveParticipant {"identity":"` + itoa(seat.Participant.ID) + `","room":"` + room + `"}`; !slices.Contains(lk.got(), kick) {
		t.Errorf("voice kept: %v", lk.got())
	}
	var sessions, tokens, seats int
	err := e.sqlDB(t).QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM profile_sessions s JOIN profiles p ON p.id = s.profile_id WHERE p.oidc_subject = 'ines'),
		(SELECT count(*) FROM api_tokens), (SELECT count(*) FROM participants WHERE removed_at IS NULL)`).Scan(&sessions, &tokens, &seats)
	if err != nil || sessions != 2 || tokens != 1 || seats != 2 {
		t.Errorf("after expiry: %d sessions, %d tokens, %d seats, %v", sessions, tokens, seats, err)
	}

	o := e.srv.opts.OIDC
	e.srv.opts.OIDC = nil
	checkAll(t, "mode off", cases, false)
	e.srv.opts.OIDC = o

	if resp, _ := laptop.signIn(t, idp, map[string]any{"sub": "ines"}); resp.code != http.StatusSeeOther {
		t.Fatalf("new sign-in %d", resp.code)
	}
	checkAll(t, "after a new sign-in", cases, false)

	// Past the window again, the daemon may still remove itself: that only lowers access.
	clk.add(DefaultOIDCRevalidate + time.Second)
	if code, b := e.do(t, "DELETE", roomPath+"/daemon", paired.Token, nil); code != http.StatusNoContent {
		t.Errorf("daemon removes itself: %d %s", code, b)
	}
}

// 021-AC8: a browser that signed in as another account past the window is not sent to sign in
// again for the former account's room cookie: that cookie is dropped as a bad seat's, which ends
// the loop a silent sign-in into the new account would run.
func TestReauthOtherAccount(t *testing.T) {
	idp := newTestIdP(t)
	e := newOIDCEnv(t, idp, zap.NewNop(), nil)
	clk := e.clock()
	room := e.createRoom(t, "R").ID
	d := e.device(t)
	if resp, _ := d.signIn(t, idp, map[string]any{"sub": "ines"}); resp.code != http.StatusSeeOther {
		t.Fatalf("sign-in %d", resp.code)
	}
	d.join(t, room, "Ines")
	clk.add(DefaultOIDCRevalidate + time.Second)
	if code, b := d.do(t, "GET", "/api/rooms/"+room, nil); code != http.StatusUnauthorized || !strings.Contains(string(b), `"reauth"`) {
		t.Fatalf("expired: %d %s", code, b)
	}
	if resp, _ := d.signIn(t, idp, map[string]any{"sub": "bob", "name": "Bob"}); resp.code != http.StatusSeeOther {
		t.Fatalf("sign-in as bob %d", resp.code)
	}
	resp := d.noFollow(t, "GET", "/api/rooms/"+room)
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if c := cookieOf(resp, cookieName); resp.StatusCode != http.StatusUnauthorized || strings.Contains(string(b), "reauth") ||
		!strings.Contains(string(b), "invalid or revoked token") || c == nil || c.MaxAge >= 0 || c.Path != "/api/rooms/"+room {
		t.Fatalf("other account: %d %s %+v", resp.StatusCode, b, c)
	}
	if p := d.profileJSON(t); p["internal"] != true || p["profile"].(map[string]any)["name"] != "Bob" {
		t.Errorf("profile %v", p)
	}
}

// 021-AC8: a profile bound by email keeps the sessions and seats it held before: they lapse with its
// sign-in, as the window is the profile's, not the credential's.
func TestReauthCoversCredentialsOlderThanTheBinding(t *testing.T) {
	idp := newTestIdP(t)
	e := newOIDCEnv(t, idp, zap.NewNop(), nil)
	clk := e.clock()
	room := e.createRoom(t, "R").ID
	old := e.device(t)
	seat := old.join(t, room, "Olga")
	id := old.profile(t).Profile.ID
	if _, err := e.sqlDB(t).ExecContext(t.Context(), `UPDATE profiles SET email = 'olga@example.com' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if resp, _ := e.device(t).signIn(t, idp, map[string]any{"sub": "olga", "email": "olga@example.com", "email_verified": true}); resp.code != http.StatusSeeOther {
		t.Fatalf("sign-in %d", resp.code)
	}
	if bound(t, e, id).Subject != "olga" {
		t.Fatal("not bound by email")
	}
	clk.add(DefaultOIDCRevalidate + time.Second)
	checkAll(t, "past the window", []reauthCase{
		{"session from before", func(t *testing.T) (int, []byte) { return old.do(t, "GET", "/api/profile", nil) }, 0},
		{"seat from before", func(t *testing.T) (int, []byte) { return e.do(t, "GET", "/api/rooms/"+room, seat.Token, nil) }, 0},
	}, true)
}

// 021-AC8: prompt=none goes to the provider; its login_required starts a normal flow from the
// callback, to the same next, and a second refusal shows the failure page instead of looping.
func TestReauthSilentFlow(t *testing.T) {
	idp := newTestIdP(t)
	e := newOIDCEnv(t, idp, zap.NewNop(), nil)
	d := e.device(t)
	login := func(query string) url.Values {
		t.Helper()
		resp := d.noFollow(t, "GET", "/auth/login?"+query)
		_ = resp.Body.Close()
		loc, err := url.Parse(resp.Header.Get("Location"))
		if resp.StatusCode != http.StatusFound || err != nil {
			t.Fatalf("login %s: %d %v", query, resp.StatusCode, err)
		}
		return loc.Query()
	}
	for _, q := range []string{"next=/r/x", "prompt=login&next=/r/x", "prompt=consent"} {
		if auth := login(q); auth.Has("prompt") {
			t.Errorf("%s: prompt %q", q, auth.Get("prompt"))
		}
	}
	for _, refusal := range []string{"login_required", "interaction_required", "consent_required", "account_selection_required"} {
		t.Run(refusal, func(t *testing.T) {
			silent := login("prompt=none&next=" + url.QueryEscape("/r/x?y=1"))
			if silent.Get("prompt") != "none" {
				t.Fatalf("silent login: %v", silent)
			}
			resp, body := d.callback(t, url.Values{"state": {silent.Get("state")}, "error": {refusal}})
			loc, _ := url.Parse(resp.header.Get("Location"))
			normal := loc.Query()
			if resp.code != http.StatusFound || !strings.HasPrefix(loc.String(), idp.ts.URL) || normal.Has("prompt") ||
				normal.Get("state") == silent.Get("state") || resp.cookie(oidcCookieName(normal.Get("state"))) == nil {
				t.Fatalf("fallback: %d %s %s", resp.code, loc, body)
			}
			if again, body := d.callback(t, url.Values{"state": {normal.Get("state")}, "error": {refusal}}); again.code != http.StatusBadRequest ||
				!strings.Contains(body, "did not sign you in") {
				t.Fatalf("second refusal: %d %s", again.code, body)
			}
		})
	}
	// The normal flow it starts ends where the page was; the page keeps its fragment itself.
	silent := login("prompt=none&next=" + url.QueryEscape("/r/x?y=1"))
	resp, _ := d.callback(t, url.Values{"state": {silent.Get("state")}, "error": {"login_required"}})
	loc, _ := url.Parse(resp.header.Get("Location"))
	normal := loc.Query()
	done, _ := d.callback(t, url.Values{"state": {normal.Get("state")}, "code": {idp.issue(t, normal, map[string]any{"sub": "ines"}, nil)}})
	if done.code != http.StatusSeeOther || done.header.Get("Location") != "/r/x?y=1" {
		t.Fatalf("sign-in: %d %s", done.code, done.header.Get("Location"))
	}
	// Another error of a silent flow is a failure, as one of a normal flow.
	silent = login("prompt=none")
	if resp, _ := d.callback(t, url.Values{"state": {silent.Get("state")}, "error": {"access_denied"}}); resp.code != http.StatusBadRequest {
		t.Errorf("access_denied: %d", resp.code)
	}
}

// 021-AC9: an internal's sign-in and recovery links, the admin's one-time ones included, are
// refused and left unused, and no sign-in link is mailed to it; an external's still work.
func TestOrgProfileRefusesLinks(t *testing.T) {
	idp := newTestIdP(t)
	core, logs := observer.New(zap.InfoLevel)
	e := newOIDCEnv(t, idp, zap.New(core), nil)
	box := &mailbox{ch: make(chan [3]string, 16)}
	e.srv.opts.SendMail, e.srv.opts.MagicLinkTTL = box.send, DefaultMagicLinkTTL
	room := e.createRoom(t, "R").ID
	db := e.sqlDB(t)
	// Each joins, keeping the permanent recovery link, and holds an email; the internal is bound by it.
	member := func(name, email string) (int64, string) {
		d := e.device(t)
		j := d.join(t, room, name)
		id := d.profile(t).Profile.ID
		if _, err := db.ExecContext(t.Context(), `UPDATE profiles SET email = $1 WHERE id = $2`, email, id); err != nil {
			t.Fatal(err)
		}
		return id, strings.TrimPrefix(j.RecoveryURL, baseURL+"/profile#recover=")
	}
	internal, internalRec := member("Ines", "ines@example.com")
	if resp, _ := e.device(t).signIn(t, idp, map[string]any{"sub": "ines", "email": "ines@example.com", "email_verified": true}); resp.code != http.StatusSeeOther ||
		bound(t, e, internal).Subject != "ines" {
		t.Fatalf("bind %d", resp.code)
	}
	external, externalRec := member("Ezra", "ezra@example.com")

	signinLink := func(id int64, email string) string {
		t.Helper()
		tok := NewToken()
		if err := e.st.CreateMagicLink(t.Context(), store.MagicLink{ProfileID: id, Email: email, Purpose: store.MagicSignIn, TokenHash: HashToken(tok),
			ExpiresAt: time.Now().Add(time.Hour)}, time.Now()); err != nil {
			t.Fatal(err)
		}
		return tok
	}
	adminLink := func(id int64) string {
		t.Helper()
		code, b := e.do(t, "POST", "/api/admin/profiles/"+strconv.FormatInt(id, 10)+"/recovery-link", e.admin, nil)
		if code != http.StatusCreated {
			t.Fatalf("recovery link: %d %s", code, b)
		}
		return strings.TrimPrefix(decodeJSON[recoveryLinkResp](t, b).URL, baseURL+"/profile#recover=")
	}
	use := func(kind, tok string) (int, []byte) {
		if kind == "recover" {
			return e.device(t).do(t, "POST", "/api/profile/recover", map[string]string{"token": tok})
		}
		return e.device(t).do(t, "POST", "/api/profile/magic", map[string]string{"token": tok, "purpose": store.MagicSignIn})
	}
	refused := func(name, kind, tok string) {
		t.Helper()
		code, b := use(kind, tok)
		if r := decodeJSON[magicError](t, b); code != http.StatusForbidden || r.Reason != "org_account" || r.Error != "This profile signs in with your organisation account" {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
	magic, admin := signinLink(internal, "ines@example.com"), adminLink(internal)
	refused("internal sign-in link", "magic", magic)
	refused("internal admin recovery link", "recover", admin)
	refused("internal recovery token", "recover", internalRec)
	var unused int
	if err := db.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM magic_links WHERE profile_id = $1 AND used_at IS NULL)
		+ (SELECT count(*) FROM recovery_links WHERE profile_id = $1 AND used_at IS NULL)`, internal).Scan(&unused); err != nil || unused != 2 {
		t.Errorf("%d links left unused, %v", unused, err)
	}
	var sessions int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM profile_sessions WHERE profile_id = $1`, internal).Scan(&sessions); err != nil || sessions != 2 {
		t.Errorf("internal has %d sessions, want its join's and its sign-in's: %v", sessions, err)
	}
	// Asking for a sign-in link answers as for any address, and mails nothing.
	ask := func(email string) {
		t.Helper()
		if code, b := e.device(t).do(t, "POST", "/api/profile/signin", map[string]string{"email": email}); code != http.StatusAccepted {
			t.Fatalf("ask %s: %d %s", email, code, b)
		}
	}
	ask("ines@example.com")
	ask("nobody@example.com")
	box.none(t)
	if n := logs.FilterMessage("email_signin_failed").Len(); n != 0 {
		t.Errorf("%d email_signin_failed logged", n)
	}

	ask("ezra@example.com")
	if to, tok := box.next(t); to != "ezra@example.com" {
		t.Errorf("mailed %s", to)
	} else if code, b := use("magic", tok); code != http.StatusOK {
		t.Errorf("external sign-in link: %d %s", code, b)
	}
	for name, tok := range map[string]string{"external admin recovery link": adminLink(external), "external recovery token": externalRec} {
		if code, b := use("recover", tok); code != http.StatusOK {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}

	// With the mode off, the internal's links work as before.
	e.srv.opts.OIDC = nil
	for name, c := range map[string][2]string{"sign-in link": {"magic", magic}, "admin recovery link": {"recover", admin}, "recovery token": {"recover", internalRec}} {
		if code, b := use(c[0], c[1]); code != http.StatusOK {
			t.Errorf("mode off, %s: %d %s", name, code, b)
		}
	}
}
