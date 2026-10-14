package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

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
