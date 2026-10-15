package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	smtpmail "github.com/babs/grill-room/internal/mail"
	"github.com/babs/grill-room/internal/store"
)

// 011-AC1 011-AC2 011-AC3: the admin's recovery link signs a browser in once within 24 h, through
// the same route as the permanent one; a newer link voids the older; both are audited, never with
// the token.
func TestAdminRecoveryLink(t *testing.T) {
	e := newEnv(t)
	t0, skew := fixedClock(e)
	laptop := e.device(t)
	j := laptop.join(t, e.createRoom(t, "A").ID, "Alice")
	permanent := strings.TrimPrefix(j.RecoveryURL, baseURL+"/profile#recover=")
	id := strconv.FormatInt(laptop.profile(t).Profile.ID, 10)
	path := "/api/admin/profiles/" + id + "/recovery-link"

	generate := func() (string, time.Time) {
		t.Helper()
		code, b := e.do(t, "POST", path, e.admin, nil)
		if code != http.StatusCreated {
			t.Fatalf("generate: %d %s", code, b)
		}
		got := decodeJSON[recoveryLinkResp](t, b)
		tok, ok := strings.CutPrefix(got.URL, baseURL+"/profile#recover=")
		if !ok || tok == "" {
			t.Fatalf("url: %q", got.URL)
		}
		return tok, got.ExpiresAt
	}
	recoverWith := func(tok string) (int, []byte) {
		return e.device(t).do(t, "POST", "/api/profile/recover", map[string]string{"token": tok})
	}

	if code, _ := e.do(t, "POST", path, "", nil); code != http.StatusUnauthorized {
		t.Fatalf("without the admin: %d", code)
	}
	for _, bad := range []string{"abc", "999999"} {
		if code, _ := e.do(t, "POST", "/api/admin/profiles/"+bad+"/recovery-link", e.admin, nil); code != http.StatusNotFound {
			t.Errorf("profile %s: %d", bad, code)
		}
	}
	voided, _ := generate()
	tok, expires := generate()
	if !expires.Equal(t0.Add(store.RecoveryLinkTTL)) {
		t.Fatalf("expires at %v, want %v", expires, t0.Add(store.RecoveryLinkTTL))
	}
	code, b := recoverWith(tok)
	if code != http.StatusOK || strconv.FormatInt(decodeJSON[profileResp](t, b).Profile.ID, 10) != id {
		t.Fatalf("one-time link: %d %s", code, b)
	}
	expired, _ := generate()
	for name, bad := range map[string]string{"second use": tok, "voided": voided, "unknown": "nope"} {
		if code, b := recoverWith(bad); code != http.StatusUnauthorized || !bytes.Contains(b, []byte("this recovery link is invalid")) {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
	skew(store.RecoveryLinkTTL)
	if code, _ := recoverWith(expired); code != http.StatusUnauthorized {
		t.Fatalf("after 24 h: %d", code)
	}
	if code, _ := recoverWith(permanent); code != http.StatusOK {
		t.Fatalf("the permanent link still works: %d", code)
	}

	log := e.auditLog(t, "")
	actions := map[string]int{}
	for _, en := range log.Entries {
		actions[en.Action]++
		if bytes.Contains(en.Details, []byte(tok)) || bytes.Contains(en.Details, []byte(HashToken(tok))) || bytes.Contains(en.Details, []byte(voided)) {
			t.Fatalf("audit entry holds a token: %s", en.Details)
		}
		if en.Action == ActionProfileRecoveryLink || en.Action == ActionProfileRecoveryUsed {
			var d map[string]any
			_ = json.Unmarshal(en.Details, &d)
			if en.TargetID != id || en.TargetName != "Alice" || d["profile_uuid"] == "" || d["profile_uuid"] == nil {
				t.Fatalf("entry: %+v %s", en, en.Details)
			}
			if en.Action == ActionProfileRecoveryLink && (en.Actor != store.ActorSuperAdmin || d["expires_at"] == nil) {
				t.Fatalf("generation entry: %+v %s", en, en.Details)
			}
		}
	}
	if actions[ActionProfileRecoveryLink] != 3 || actions[ActionProfileRecoveryUsed] != 1 {
		t.Fatalf("audit actions: %v", actions)
	}
}

// 011-AC21: the pending address shows to its profile only: never to another profile, a room view or
// the admin's profiles list.
func TestPendingEmailOwnerOnly(t *testing.T) {
	e, box := newMailEnv(t, time.Minute)
	room := e.createRoom(t, "A")
	alice, bob := e.device(t), e.device(t)
	alice.join(t, room.ID, "Alice")
	bob.join(t, room.ID, "Bob")
	const pending = "alice.pending@example.com"
	if code, _ := alice.do(t, "POST", "/api/profile/email", map[string]string{"email": pending}); code != http.StatusAccepted {
		t.Fatal("bind")
	}
	box.next(t)
	if got := alice.profile(t).Profile.PendingEmail; got != pending {
		t.Fatalf("owner's pending: %q", got)
	}
	var seen []byte
	for _, path := range []string{"/api/profile", "/api/rooms/" + room.ID, "/api/rooms/" + room.ID + "/invites"} {
		_, b := bob.do(t, "GET", path, nil)
		seen = append(seen, b...)
	}
	for _, path := range []string{"/api/profiles", "/api/admin/profiles", "/api/admin/rooms", "/api/admin/audit"} {
		_, b := e.do(t, "GET", path, e.admin, nil)
		seen = append(seen, b...)
	}
	if bytes.Contains(seen, []byte("alice.pending")) || bytes.Contains(seen, []byte("pending_email")) {
		t.Fatalf("the pending address leaks: %s", seen)
	}
}

// 011-AC23: each outcome of an emailed link, with the reason the page shows.
func TestMagicLinkOutcomes(t *testing.T) {
	e, box := newMailEnv(t, time.Minute)
	_, skew := fixedClock(e)
	room := e.createRoom(t, "A")
	alice := e.device(t)
	alice.join(t, room.ID, "Alice")
	ask := func(d *device, email string) string {
		t.Helper()
		if code, b := d.do(t, "POST", "/api/profile/email", map[string]string{"email": email}); code != http.StatusAccepted {
			t.Fatalf("bind: %d %s", code, b)
		}
		_, tok := box.next(t)
		return tok
	}
	open := func(d *device, tok string, confirm bool) (int, magicError) {
		t.Helper()
		code, b := d.do(t, "POST", "/api/profile/magic", map[string]any{"token": tok, "purpose": "bind", "confirm": confirm})
		var m magicError
		_ = json.Unmarshal(b, &m)
		return code, m
	}

	expired := ask(alice, "late@example.com")
	skew(time.Minute)
	if code, m := open(alice, expired, false); code != http.StatusGone || m.Reason != "expired" {
		t.Fatalf("expired: %d %+v", code, m)
	}
	skew(0)
	if code, m := open(alice, "nope", false); code != http.StatusUnauthorized || m.Reason != "invalid" {
		t.Fatalf("invalid: %d %+v", code, m)
	}

	// Opened in another browser, the link names the profile and waits for the confirm.
	tok := ask(alice, "alice@example.com")
	other := e.device(t)
	other.join(t, room.ID, "Mallory")
	code, m := open(other, tok, false)
	if code != http.StatusConflict || m.Reason != "confirm" || m.ProfileName != "Alice" || m.Email != "alice@example.com" {
		t.Fatalf("unconfirmed: %d %+v", code, m)
	}
	if alice.profile(t).Profile.Email != "" {
		t.Fatal("bound without a confirm")
	}
	code, b := other.do(t, "POST", "/api/profile/magic", map[string]any{"token": tok, "purpose": "bind", "confirm": true})
	var bound struct {
		Bound       bool            `json:"bound"`
		Email       string          `json:"email"`
		ProfileName string          `json:"profile_name"`
		Profile     json.RawMessage `json:"profile"`
	}
	_ = json.Unmarshal(b, &bound)
	if code != http.StatusOK || !bound.Bound || bound.Email != "alice@example.com" || bound.ProfileName != "Alice" || bound.Profile != nil {
		t.Fatalf("confirmed: %d %s", code, b)
	}
	if got := other.profile(t).Profile; got.Email != "" || got.Name == "Alice" {
		t.Fatalf("the confirming browser changed: %+v", got)
	}
	if got := alice.profile(t).Profile; got.Email != "alice@example.com" || got.PendingEmail != "" {
		t.Fatalf("alice after the confirm: %+v", got)
	}
	if code, m := open(alice, tok, false); code != http.StatusGone || m.Reason != "used" {
		t.Fatalf("used: %d %+v", code, m)
	}

	// Another profile asking for Alice's address: taken, confirmed or not, and naming nobody.
	bob := e.device(t)
	bob.join(t, room.ID, "Bob")
	tok = ask(bob, "alice@example.com")
	for _, d := range []*device{bob, alice} {
		code, b := d.do(t, "POST", "/api/profile/magic", map[string]any{"token": tok, "purpose": "bind", "confirm": true})
		if code != http.StatusConflict || !bytes.Contains(b, []byte(`"reason":"taken"`)) || bytes.Contains(b, []byte("Alice")) || bytes.Contains(b, []byte("alice@")) {
			t.Fatalf("taken: %d %s", code, b)
		}
	}
}

// 011-AC24: each mail says what its link does and how long it works.
func TestMagicLinkMailWording(t *testing.T) {
	e, box := newMailEnv(t, 15*time.Minute)
	d := e.device(t)
	d.join(t, e.createRoom(t, "A").ID, "Alice")
	read := func() [3]string {
		t.Helper()
		select {
		case m := <-box.ch:
			return m
		case <-time.After(5 * time.Second):
			t.Fatal("no mail")
			return [3]string{}
		}
	}
	d.do(t, "POST", "/api/profile/email", map[string]string{"email": "alice@example.com"})
	bind := read()
	tok := magicLink.FindStringSubmatch(bind[2])
	if bind[1] != "Add this email to your grill-room profile" || !strings.Contains(bind[2], "This link adds alice@example.com to a grill-room profile") ||
		!strings.Contains(bind[2], "If you did not ask for it, ignore this mail.") ||
		!strings.Contains(bind[2], "It works once, within 15 minutes.") || !strings.Contains(bind[2], "/profile#bind=") {
		t.Fatalf("bind mail: %q", bind)
	}
	d.do(t, "POST", "/api/profile/magic", map[string]string{"token": tok[1], "purpose": "bind"})
	e.device(t).do(t, "POST", "/api/profile/signin", map[string]string{"email": "alice@example.com"})
	signin := read()
	if signin[1] != "Sign in to grill-room" || !strings.Contains(signin[2], "This link signs you in to your grill-room profile") ||
		!strings.Contains(signin[2], "It works once, within 15 minutes.") || !strings.Contains(signin[2], "/profile#magic=") ||
		!strings.Contains(signin[2], "If you did not ask for it, ignore this mail.") {
		t.Fatalf("sign-in mail: %q", signin)
	}
}

// 011-AC8: "Later" snoozes the banner 14 days on the profile, so every device reads it.
func TestSnoozeEmailNudge(t *testing.T) {
	e := newEnv(t)
	t0, _ := fixedClock(e)
	laptop := e.device(t)
	j := laptop.join(t, e.createRoom(t, "A").ID, "Alice")
	if code, _ := e.device(t).do(t, "POST", "/api/profile/nudge", nil); code != http.StatusUnauthorized {
		t.Fatalf("without a profile: %d", code)
	}
	if code, _ := laptop.do(t, "POST", "/api/profile/nudge", nil, "Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
		t.Fatalf("cross-site: %d", code)
	}
	if laptop.profile(t).Profile.EmailNudgeUntil != nil {
		t.Fatal("snoozed before Later")
	}
	if code, _ := laptop.do(t, "POST", "/api/profile/nudge", nil); code != http.StatusNoContent {
		t.Fatalf("snooze: %d", code)
	}
	phone := e.device(t)
	if code, _ := phone.do(t, "POST", "/api/profile/recover", map[string]string{"token": strings.TrimPrefix(j.RecoveryURL, baseURL+"/profile#recover=")}); code != http.StatusOK {
		t.Fatal("recover")
	}
	until := phone.profile(t).Profile.EmailNudgeUntil
	if until == nil || !until.Equal(t0.Add(emailNudgeSnooze)) {
		t.Fatalf("snoozed until %v on the other device", until)
	}
}

// 011-AC22: every GET /api/profile answer names the emailed links' lifetime, signed in or out, and
// only when email is on.
func TestProfileNamesMagicLinkTTL(t *testing.T) {
	e, _ := newMailEnv(t, 15*time.Minute)
	in, out := e.device(t), e.device(t)
	in.join(t, e.createRoom(t, "A").ID, "Alice")
	for name, d := range map[string]*device{"signed in": in, "signed out": out} {
		if got := d.profile(t).MagicLinkTTLS; got != 900 {
			t.Errorf("%s: %d, want 900", name, got)
		}
	}
	plain := newEnv(t)
	if _, b := plain.device(t).do(t, "GET", "/api/profile", nil); bytes.Contains(b, []byte("magic_link_ttl_s")) {
		t.Fatalf("without SMTP: %s", b)
	}
}

// relay is a fake SMTP relay: down while fail is set, refusing every recipient while reject is.
type relay struct {
	fail, reject atomic.Bool
	box          mailbox
}

func newRelayEnv(t *testing.T) (*env, *relay) {
	t.Helper()
	rl := &relay{box: mailbox{ch: make(chan [3]string, 16)}}
	opts := testOptions
	opts.MagicLinkTTL = 15 * time.Minute
	opts.SendMail = func(ctx context.Context, to, subject, body string) error {
		switch {
		case rl.fail.Load():
			return errors.New("dial: connection refused")
		case rl.reject.Load():
			return fmt.Errorf("rcpt to: %w: 550 no such user <%s>", smtpmail.ErrRecipient, to)
		}
		return rl.box.send(ctx, to, subject, body)
	}
	return newEnvOpts(t, opts), rl
}

// post sends a JSON body without cookies and returns the status, the Retry-After header and the body.
func post(t *testing.T, e *env, path, body string) (int, string, map[string]any) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), "POST", e.ts.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, resp.Header.Get("Retry-After"), m
}

// relayEnvWithAlice is a relay env on a fixed clock whose profile Alice holds alice@example.com.
func relayEnvWithAlice(t *testing.T) (*env, *relay, *device, time.Time, func(time.Duration)) {
	t.Helper()
	e, rl := newRelayEnv(t)
	t0, skew := fixedClock(e)
	alice := e.device(t)
	alice.join(t, e.createRoom(t, "A").ID, "Alice")
	code, b := alice.do(t, "POST", "/api/profile/email", map[string]string{"email": "alice@example.com"})
	if code != http.StatusAccepted || string(b) != `{"email":"alice@example.com","ttl_s":900}`+"\n" {
		t.Fatalf("bind: %d %s", code, b)
	}
	_, tok := rl.box.next(t)
	if code, _ := alice.do(t, "POST", "/api/profile/magic", map[string]string{"token": tok, "purpose": "bind"}); code != http.StatusOK {
		t.Fatal("bind link")
	}
	return e, rl, alice, t0, skew
}

// 011-AC28: sign-in answers alike for known and unknown addresses; a failed bind send makes every
// sign-in answer "mail failed" alike with when to retry, before the rate limit, and drops the
// unsent link; a successful bind send clears it.
func TestEmailRequestFeedback(t *testing.T) {
	e, rl, alice, _, skew := relayEnvWithAlice(t)
	n := 0
	known := func() (int, string, map[string]any) {
		return post(t, e, "/api/profile/signin", `{"email":"alice@example.com"}`)
	}
	// A fresh unknown address each time keeps clear of the per-address limit.
	unknown := func() (int, string, map[string]any) {
		n++
		return post(t, e, "/api/profile/signin", fmt.Sprintf(`{"email":"nobody%d@example.com"}`, n))
	}
	kc, _, kb := known()
	uc, _, ub := unknown()
	if kc != http.StatusAccepted || kc != uc || fmt.Sprint(kb) != fmt.Sprint(ub) || kb["ttl_s"] != float64(900) {
		t.Fatalf("known %d %v, unknown %d %v", kc, kb, uc, ub)
	}
	e.srv.WaitMail()
	rl.box.next(t)

	if code, b := alice.do(t, "POST", "/api/profile/email", map[string]string{"email": "nope"}); code != http.StatusBadRequest || !bytes.Contains(b, []byte(`"reason":"invalid_email"`)) {
		t.Fatalf("invalid: %d %s", code, b)
	}
	rl.fail.Store(true)
	code, b := alice.do(t, "POST", "/api/profile/email", map[string]string{"email": "alice.new@example.com"})
	var m map[string]string
	_ = json.Unmarshal(b, &m)
	if code != http.StatusBadGateway || m["reason"] != "mail_failed" || m["error"] != mailFailedText {
		t.Fatalf("bind on a failing relay: %d %s", code, b)
	}
	if p := alice.profile(t).Profile; p.PendingEmail != "" || p.PendingTTLS != nil || p.PendingAgeS != nil {
		t.Fatalf("the unsent link shows as pending: %+v", p)
	}
	skew(time.Minute)
	kc, kr, kb := known()
	uc, ur, ub := unknown()
	if kc != http.StatusServiceUnavailable || kc != uc || kr != ur || fmt.Sprint(kb) != fmt.Sprint(ub) || kb["reason"] != "mail_failed" ||
		kb["retry_after_s"] != float64(240) || kr != "240" {
		t.Fatalf("sign-in while failing: known %d %q %v, unknown %d %q %v", kc, kr, kb, uc, ur, ub)
	}
	// Nothing is sent while failing, so nothing counts against the rate limit.
	for i := range magicPerAddressIP + 2 {
		if code, _, _ := known(); code != http.StatusServiceUnavailable {
			t.Fatalf("sign-in %d while failing: %d", i, code)
		}
	}
	// Past the hold, sign-in is offered again.
	skew(mailFailHold)
	if code, _, _ := unknown(); code != http.StatusAccepted {
		t.Fatalf("past the hold: %d", code)
	}
	skew(0)
	rl.fail.Store(false)
	if code, _ := alice.do(t, "POST", "/api/profile/email", map[string]string{"email": "alice.new@example.com"}); code != http.StatusAccepted {
		t.Fatalf("bind once the relay works: %d", code)
	}
	rl.box.next(t)
	// Seconds on the server clock, whatever the client's.
	for _, c := range []struct{ at, ttl, age time.Duration }{{0, 900, 0}, {90 * time.Second, 810, 90}} {
		skew(c.at)
		if p := alice.profile(t).Profile; p.PendingEmail != "alice.new@example.com" || p.PendingTTLS == nil || *p.PendingTTLS != int64(c.ttl) ||
			p.PendingAgeS == nil || *p.PendingAgeS != int64(c.age) {
			t.Fatalf("pending at %v: %+v", c.at, p)
		}
	}
	// profile.js reads these wire names; decoding through the struct would not notice a rename.
	if _, b := alice.do(t, "GET", "/api/profile", nil); !bytes.Contains(b, []byte(`"pending_ttl_s":`)) || !bytes.Contains(b, []byte(`"pending_age_s":`)) {
		t.Fatalf("pending wire names: %s", b)
	}
	if code, _, _ := known(); code != http.StatusAccepted {
		t.Fatalf("after a successful send: %d", code)
	}
	e.srv.WaitMail()
	rl.box.next(t)
}

// 011-AC28: a sign-in send happens for a known address only, so it never moves the mail failing
// state: the next sign-in answers the same whichever address came before.
func TestSignInSendLeavesMailStateAlone(t *testing.T) {
	e, rl, _, _, _ := relayEnvWithAlice(t)
	rl.fail.Store(true)
	for i, order := range [][]string{{"alice@example.com", "nobody1@example.com"}, {"nobody2@example.com", "alice@example.com"}} {
		for _, email := range order {
			code, _, b := post(t, e, "/api/profile/signin", `{"email":"`+email+`"}`)
			if code != http.StatusAccepted {
				t.Fatalf("order %d, %s: %d %v", i, email, code, b)
			}
			if email == "alice@example.com" {
				e.srv.WaitMail() // the failed send is over before the next request
			}
		}
	}
}

// 011-AC28: a relay refusing one address answers that binder "address rejected" and leaves sign-in
// to everyone else.
func TestRejectedAddressDoesNotBlockSignIn(t *testing.T) {
	e, rl, alice, _, _ := relayEnvWithAlice(t)
	rl.reject.Store(true)
	code, b := alice.do(t, "POST", "/api/profile/email", map[string]string{"email": "typo@example.con"})
	var m map[string]string
	_ = json.Unmarshal(b, &m)
	if code != http.StatusBadGateway || m["reason"] != "address_rejected" || m["error"] != addressRejectedTxt || strings.Contains(string(b), "typo@") {
		t.Fatalf("rejected bind: %d %s", code, b)
	}
	if alice.profile(t).Profile.PendingEmail != "" {
		t.Fatal("the rejected link shows as pending")
	}
	if code, _, _ := post(t, e, "/api/profile/signin", `{"email":"nobody@example.com"}`); code != http.StatusAccepted {
		t.Fatalf("sign-in after a rejected address: %d", code)
	}
}

// 011-AC28: a rate-limited request says when to retry, in its body and its Retry-After header.
func TestEmailRequestRetryAfter(t *testing.T) {
	e, _ := newRelayEnv(t)
	_, skew := fixedClock(e)
	ask := func() (int, string, map[string]any) {
		return post(t, e, "/api/profile/signin", `{"email":"same@example.com"}`)
	}
	for range magicPerAddressIP {
		if code, _, _ := ask(); code != http.StatusAccepted {
			t.Fatalf("within the limit: %d", code)
		}
	}
	for _, c := range []struct {
		after time.Duration
		want  string
	}{{0, "900"}, {5*time.Minute + 500*time.Millisecond, "600"}} {
		skew(c.after)
		code, header, m := ask()
		if code != http.StatusTooManyRequests || m["reason"] != "rate_limited" || fmt.Sprint(m["retry_after_s"]) != c.want || header != c.want {
			t.Fatalf("after %v: %d %v %q", c.after, code, m, header)
		}
	}
}

// 011-AC28: a bind whose mail did not go out gives its rate-limit allowance back.
func TestFailedBindSpendsNoAllowance(t *testing.T) {
	_, rl, alice, _, _ := relayEnvWithAlice(t)
	for _, c := range []struct {
		name   string
		toggle *atomic.Bool
	}{{"mail_failed", &rl.fail}, {"address_rejected", &rl.reject}} {
		c.toggle.Store(true)
		for i := range magicPerAddressIP + 1 {
			code, b := alice.do(t, "POST", "/api/profile/email", map[string]string{"email": c.name + "@example.com"})
			if code != http.StatusBadGateway || !bytes.Contains(b, []byte(c.name)) {
				t.Fatalf("%s, bind %d: %d %s", c.name, i, code, b)
			}
		}
		c.toggle.Store(false)
	}
}
