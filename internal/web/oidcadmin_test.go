package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/livekit"
	"github.com/babs/grill-room/internal/store"
)

const adminGroup = "grill-admins"

// oidcAdminLogin asks /admin for a session on d's profile cookie, and returns the status and the
// admin cookie set.
func (d *device) oidcAdminLogin(t *testing.T, hdr ...string) (int, *http.Cookie) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), "POST", d.e.ts.URL+"/api/admin/login/oidc", nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := d.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, cookieOf(resp, adminCookie)
}

// 021-AC11: a sign-in holding the admin group opens an admin session, audited admin.login via oidc
// with the profile's uuid, capped at the profile's revalidation; one without it, an external, an
// expired sign-in or a grant under another group than the configured one get 401; the admin token
// keeps working.
func TestOIDCAdminGroup(t *testing.T) {
	idp := newTestIdP(t)
	e := newOIDCEnv(t, idp, zap.NewNop(), func(c *OIDCConfig) { c.AdminGroup = adminGroup })
	clk := e.clock()
	ana := e.device(t)
	if resp, _ := ana.signIn(t, idp, map[string]any{"sub": "ana", "groups": []any{"staff", adminGroup}}); resp.code != http.StatusSeeOther {
		t.Fatalf("sign-in %d", resp.code)
	}
	if v := e.device(t).profileJSON(t)["admin_sign_in"]; v != true {
		t.Errorf("admin_sign_in %v", v)
	}
	code, c := ana.oidcAdminLogin(t)
	if code != http.StatusCreated || c == nil || c.MaxAge != int(store.AdminSessionTTL/time.Second) || c.Path != adminCookiePath || !c.HttpOnly {
		t.Fatalf("admin group: %d %+v", code, c)
	}
	if code, _ := ana.sessions(t); code != http.StatusOK {
		t.Fatalf("sessions with the oidc session: %d", code)
	}
	if code, _ := ana.oidcAdminLogin(t, "Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
		t.Errorf("cross-site: %d", code)
	}
	var uuid string
	if err := e.sqlDB(t).QueryRowContext(t.Context(), `SELECT uuid FROM profiles WHERE oidc_subject = 'ana'`).Scan(&uuid); err != nil {
		t.Fatal(err)
	}
	entries := e.auditLog(t, "?action="+ActionAdminLogin).Entries
	if len(entries) != 1 {
		t.Fatalf("admin.login entries %+v", entries)
	}
	var d map[string]any
	_ = json.Unmarshal(entries[0].Details, &d)
	if d["via"] != "oidc" || d["profile"] != uuid || entries[0].Actor != store.ActorSuperAdmin {
		t.Errorf("audit %+v %s", entries[0], entries[0].Details)
	}

	// An hour of revalidation left: the session ends with it, not 12 h later.
	clk.add(DefaultOIDCRevalidate - time.Hour)
	if code, c := ana.oidcAdminLogin(t); code != http.StatusCreated || c == nil || c.MaxAge != 3600 {
		t.Fatalf("capped: %d %+v", code, c)
	}
	clk.add(time.Hour)
	if code, _ := ana.sessions(t); code != http.StatusUnauthorized {
		t.Errorf("past the revalidation: sessions %d", code)
	}
	if code, c := ana.oidcAdminLogin(t); code != http.StatusUnauthorized || c != nil {
		t.Errorf("expired sign-in: %d %+v", code, c)
	}

	// The group removed at the provider: no admin after the next sign-in.
	if resp, _ := ana.signIn(t, idp, map[string]any{"sub": "ana", "groups": []any{"staff"}}); resp.code != http.StatusSeeOther {
		t.Fatalf("sign-in %d", resp.code)
	}
	if code, c := ana.oidcAdminLogin(t); code != http.StatusUnauthorized || c != nil {
		t.Errorf("without the group: %d %+v", code, c)
	}
	// The group as a single string counts as the claim does for members (021-AC3).
	if resp, _ := ana.signIn(t, idp, map[string]any{"sub": "ana", "groups": adminGroup}); resp.code != http.StatusSeeOther {
		t.Fatalf("sign-in %d", resp.code)
	}
	if code, _ := ana.oidcAdminLogin(t); code != http.StatusCreated {
		t.Errorf("group as a string: %d", code)
	}
	// A grant made under a former setting does not outlive the setting.
	for _, g := range []string{"", "a-narrower-group"} {
		e.srv.opts.OIDC.cfg.AdminGroup = g
		if code, _ := ana.oidcAdminLogin(t); code != http.StatusUnauthorized {
			t.Errorf("admin group %q: %d", g, code)
		}
	}
	e.srv.opts.OIDC.cfg.AdminGroup = adminGroup

	guest := e.device(t)
	guest.join(t, e.createRoom(t, "R").ID, "Gus")
	if code, _ := guest.oidcAdminLogin(t); code != http.StatusUnauthorized {
		t.Errorf("external: %d", code)
	}
	if code, _ := e.device(t).oidcAdminLogin(t); code != http.StatusUnauthorized {
		t.Errorf("no profile: %d", code)
	}
	// A former provider's binding is external, flag or not (021-AC5).
	if _, err := e.sqlDB(t).ExecContext(t.Context(), `UPDATE profiles SET oidc_issuer = 'https://former.example' WHERE oidc_subject = 'ana'`); err != nil {
		t.Fatal(err)
	}
	if code, _ := ana.oidcAdminLogin(t); code != http.StatusUnauthorized {
		t.Errorf("former issuer: %d", code)
	}
	if code, c := e.device(t).login(t, e.admin, "203.0.113.9"); code != http.StatusCreated || c == nil {
		t.Errorf("admin token: %d", code)
	}
}

// 021-AC11: without an admin group, a member's sign-in never opens /admin and /admin offers no
// provider sign-in; without restricted mode the route does not exist.
func TestOIDCAdminGroupUnset(t *testing.T) {
	idp := newTestIdP(t)
	e := newOIDCEnv(t, idp, zap.NewNop(), nil)
	ana := e.device(t)
	if resp, _ := ana.signIn(t, idp, map[string]any{"sub": "ana", "groups": []any{adminGroup}}); resp.code != http.StatusSeeOther {
		t.Fatalf("sign-in %d", resp.code)
	}
	if code, _ := ana.oidcAdminLogin(t); code != http.StatusUnauthorized {
		t.Errorf("no admin group set: %d", code)
	}
	// /admin offers no provider sign-in without an admin group.
	if _, ok := ana.profileJSON(t)["admin_sign_in"]; ok {
		t.Error("admin_sign_in without an admin group")
	}
	if code, _ := newEnv(t).device(t).oidcAdminLogin(t); code != http.StatusNotFound {
		t.Errorf("restricted mode off: %d", code)
	}
}

type adminProfileRow struct {
	ID            int64      `json:"id"`
	Internal      *bool      `json:"internal"`
	SignedInAt    *time.Time `json:"signed_in_at"`
	SignInExpired bool       `json:"sign_in_expired"`
}

func (e *env) adminProfiles(t *testing.T) map[int64]adminProfileRow {
	t.Helper()
	code, b := e.do(t, "GET", "/api/admin/profiles", e.admin, nil)
	if code != http.StatusOK {
		t.Fatalf("profiles: %d %s", code, b)
	}
	m := map[int64]adminProfileRow{}
	for _, r := range decodeJSON[[]adminProfileRow](t, b) {
		m[r.ID] = r
	}
	return m
}

// 021-AC10: /admin lists whether each profile is internal, its last sign-in and an expired one;
// Unbind makes it external again, audited, and signs it out everywhere: browsers, API tokens, its
// seats' tokens and paired daemons; its recovery link brings it back as a guest, seats and all.
func TestOIDCUnbind(t *testing.T) {
	idp := newTestIdP(t)
	e := newOIDCEnv(t, idp, zap.NewNop(), nil)
	lk := &fakeLiveKit{}
	lts := httptest.NewServer(lk)
	t.Cleanup(lts.Close)
	e.srv.opts.Voice = &Voice{URL: "wss://livekit.example", Key: "APIkey", Secret: lkSecret,
		API: &livekit.Client{URL: lts.URL, Key: "APIkey", Secret: lkSecret, HTTP: lts.Client()}}
	clk := e.clock()
	ines := e.device(t)
	if resp, _ := ines.signIn(t, idp, map[string]any{"sub": "ines"}); resp.code != http.StatusSeeOther {
		t.Fatalf("sign-in %d", resp.code)
	}
	id := profileID(t, ines.profileJSON(t))
	room := e.createRoom(t, "Seats").ID
	seat := ines.join(t, room, "Ines")
	api := ines.newToken(t, store.ScopeFull, 0)
	pairing := e.requestPairing(t, room, "ines-laptop")
	if code, b := e.do(t, "POST", "/api/rooms/"+room+"/pairings/"+pairing.ID, seat.Token, map[string]bool{"approve": true}); code != http.StatusOK {
		t.Fatalf("approve: %d %s", code, b)
	}
	_, paired := e.poll(t, pairing)
	// A second room without a daemon: nothing but the unbind wakes its stream.
	other := e.createRoom(t, "Other").ID
	otherSeat := ines.join(t, other, "Ines")
	guest := e.device(t)
	guest.join(t, e.createRoom(t, "R").ID, "Gus")
	gid := profileID(t, guest.profileJSON(t))

	rows := e.adminProfiles(t)
	if r := rows[id]; r.Internal == nil || !*r.Internal || r.SignedInAt == nil || !r.SignedInAt.Equal(clk.now()) || r.SignInExpired {
		t.Errorf("internal row %+v", r)
	}
	if r := rows[gid]; r.Internal == nil || *r.Internal || r.SignedInAt != nil || r.SignInExpired {
		t.Errorf("external row %+v", r)
	}
	clk.add(DefaultOIDCRevalidate)
	if r := e.adminProfiles(t)[id]; !r.SignInExpired {
		t.Errorf("past the revalidation %+v", r)
	}

	path := "/api/admin/profiles/" + strconv.FormatInt(id, 10) + "/oidc"
	// Read within the window, as the unbind of a member whose page is open.
	clk.add(-DefaultOIDCRevalidate)
	stream := e.openEvents(t, other, otherSeat.Token, "")
	clk.add(DefaultOIDCRevalidate)
	if code, b := e.do(t, "DELETE", path, e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("unbind: %d %s", code, b)
	}
	// Well before the heartbeat (20 s): the unbind wakes the seat's room.
	revoked := make(chan struct{})
	go func() {
		for ev := range stream {
			if ev.event == "revoked" {
				close(revoked)
				return
			}
		}
	}()
	select {
	case <-revoked:
	case <-time.After(3 * time.Second):
		t.Error("the unbound seat's stream was not revoked promptly")
	}
	if r := e.adminProfiles(t)[id]; r.Internal == nil || *r.Internal || r.SignedInAt != nil || r.SignInExpired {
		t.Errorf("unbound row %+v", r)
	}
	// Signed out everywhere, so unbinding an expired leaver revives nothing.
	if code, b := ines.do(t, "GET", "/api/profile/tokens", nil); code != http.StatusUnauthorized {
		t.Errorf("unbound profile cookie: %d %s", code, b)
	}
	if code, b := e.do(t, "GET", "/api/profile", api.Token, nil); code != http.StatusUnauthorized {
		t.Errorf("unbound API token: %d %s", code, b)
	}
	if code, b := e.do(t, "GET", "/api/rooms/"+room, seat.Token, nil); code != http.StatusUnauthorized {
		t.Errorf("unbound seat token: %d %s", code, b)
	}
	if code, b := e.do(t, "POST", "/api/rooms/"+room+"/daemon/status", paired.Token, map[string]any{"can_host": false, "reason": "idle"}); code != http.StatusUnauthorized {
		t.Errorf("unbound daemon: %d %s", code, b)
	}
	// Its seat leaves the audio, off the request.
	kick := `RemoveParticipant {"identity":"` + itoa(seat.Participant.ID) + `","room":"` + room + `"}`
	for deadline := time.Now().Add(5 * time.Second); !slices.Contains(lk.got(), kick) && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if !slices.Contains(lk.got(), kick) {
		t.Errorf("seat kept in the audio: %v", lk.got())
	}
	// Back as a guest through a recovery link: the seat comes with the profile, its messages kept.
	code, b := e.do(t, "POST", "/api/admin/profiles/"+strconv.FormatInt(id, 10)+"/recovery-link", e.admin, nil)
	if code != http.StatusCreated {
		t.Fatalf("recovery link: %d %s", code, b)
	}
	back := e.device(t)
	if code, b := back.do(t, "POST", "/api/profile/recover", map[string]string{"token": strings.TrimPrefix(decodeJSON[recoveryLinkResp](t, b).URL, baseURL+"/profile#recover=")}); code != http.StatusOK {
		t.Fatalf("recover: %d %s", code, b)
	}
	if code, b := back.do(t, "GET", "/api/rooms/"+room, nil); code != http.StatusOK {
		t.Errorf("seat after recovery: %d %s", code, b)
	}
	if p := back.profileJSON(t); profileID(t, p) != id || p["internal"] != false {
		t.Errorf("recovered profile %v", p)
	}
	var iss, sub, at, admin *string
	if err := e.sqlDB(t).QueryRowContext(t.Context(), `SELECT oidc_issuer, oidc_subject, oidc_signed_in_at, oidc_admin_group FROM profiles WHERE id = $1`, id).
		Scan(&iss, &sub, &at, &admin); err != nil || iss != nil || sub != nil || at != nil || admin != nil {
		t.Errorf("binding left: %v %v %v %v %v", iss, sub, at, admin, err)
	}
	entries := e.auditLog(t, "?action="+ActionProfileOIDCUnbind).Entries
	if len(entries) != 1 || entries[0].TargetKind != store.TargetProfile || entries[0].TargetID != strconv.FormatInt(id, 10) || entries[0].Actor != store.ActorSuperAdmin {
		t.Errorf("audit %+v", entries)
	}
	for _, p := range []string{path, "/api/admin/profiles/" + strconv.FormatInt(gid, 10) + "/oidc", "/api/admin/profiles/999999/oidc", "/api/admin/profiles/x/oidc"} {
		if code, _ := e.do(t, "DELETE", p, e.admin, nil); code != http.StatusNotFound {
			t.Errorf("%s: %d", p, code)
		}
	}
	if code, _ := ines.do(t, "DELETE", path, nil); code != http.StatusUnauthorized {
		t.Errorf("unbind without the admin: %d", code)
	}
	if n := len(e.auditLog(t, "?action="+ActionProfileOIDCUnbind).Entries); n != 1 {
		t.Errorf("refused unbinds audited: %d entries", n)
	}
}

// 021-AC10, 021-AC12: with restricted mode off the profiles list and /api/profile are as before.
func TestOIDCAdminViewsOff(t *testing.T) {
	e := newEnv(t)
	d := e.device(t)
	d.join(t, e.createRoom(t, "R").ID, "Gus")
	_, b := e.do(t, "GET", "/api/admin/profiles", e.admin, nil)
	for _, r := range decodeJSON[[]map[string]any](t, b) {
		for _, k := range []string{"internal", "signed_in_at", "sign_in_expired"} {
			if _, ok := r[k]; ok {
				t.Errorf("profiles list has %s: %s", k, b)
			}
		}
	}
	for _, k := range []string{"provider_name", "admin_sign_in"} {
		if _, ok := d.profileJSON(t)[k]; ok {
			t.Errorf("%s without restricted mode", k)
		}
	}
	if code, _ := e.do(t, "DELETE", "/api/admin/profiles/1/oidc", e.admin, nil); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Errorf("unbind route without restricted mode: %d", code)
	}
}

// 021-AC12: /api/profile names the provider for the join page's button, by default or as set.
func TestOIDCProviderName(t *testing.T) {
	idp := newTestIdP(t)
	if got := newOIDCEnv(t, idp, zap.NewNop(), nil).device(t).profileJSON(t)["provider_name"]; got != DefaultOIDCName {
		t.Errorf("default: %v", got)
	}
	e := newOIDCEnv(t, idp, zap.NewNop(), func(c *OIDCConfig) { c.Name = "Acme SSO" })
	if got := e.device(t).profileJSON(t)["provider_name"]; got != "Acme SSO" {
		t.Errorf("set: %v", got)
	}
}

// 021-AC11: a provider admin session belongs to its profile. Listed with it, it ends when the
// admin group setting changes, the sign-in passes its revalidation, a sign-in no longer holds the
// group, or the profile is unbound or deleted; a token session lives on.
func TestOIDCAdminSessionOwned(t *testing.T) {
	idp := newTestIdP(t)
	e := newOIDCEnv(t, idp, zap.NewNop(), func(c *OIDCConfig) { c.AdminGroup = adminGroup })
	clk := e.clock()
	admin := map[string]any{"sub": "olga", "name": "Olga", "groups": []any{adminGroup}}
	olga := e.device(t)
	open := func(t *testing.T) {
		t.Helper()
		if resp, _ := olga.signIn(t, idp, admin); resp.code != http.StatusSeeOther {
			t.Fatalf("sign-in %d", resp.code)
		}
		if code, _ := olga.oidcAdminLogin(t); code != http.StatusCreated {
			t.Fatalf("admin session: %d", code)
		}
	}
	gone := func(t *testing.T, why string) {
		t.Helper()
		if code, _ := olga.sessions(t); code != http.StatusUnauthorized {
			t.Errorf("%s: sessions %d", why, code)
		}
	}
	keeper := e.device(t)
	if code, _ := keeper.login(t, e.admin, "203.0.113.9"); code != http.StatusCreated {
		t.Fatalf("token login %d", code)
	}

	open(t)
	_, list := keeper.sessions(t)
	var uuid string
	_ = e.sqlDB(t).QueryRowContext(t.Context(), `SELECT uuid FROM profiles WHERE oidc_subject = 'olga'`).Scan(&uuid)
	if len(list) != 2 || list[0].ProfileUUID != uuid || list[0].ProfileName != "Olga" || list[1].ProfileUUID != "" {
		t.Fatalf("sessions list %+v", list)
	}
	e.srv.opts.OIDC.cfg.AdminGroup = "a-narrower-group"
	gone(t, "admin group narrowed")
	if code, _ := keeper.sessions(t); code != http.StatusOK {
		t.Errorf("token session under the new setting: %d", code)
	}
	e.srv.opts.OIDC.cfg.AdminGroup = adminGroup
	if code, _ := olga.sessions(t); code != http.StatusUnauthorized {
		t.Error("an ended session came back with the setting")
	}

	open(t)
	// A session opened mid-window is capped there; the check holds even before its own expiry.
	clk.add(DefaultOIDCRevalidate)
	gone(t, "past the revalidation")

	open(t)
	if resp, _ := e.device(t).signIn(t, idp, map[string]any{"sub": "olga", "groups": []any{"staff"}}); resp.code != http.StatusSeeOther {
		t.Fatalf("sign-in %d", resp.code)
	}
	gone(t, "a sign-in without the group")

	open(t)
	id := profileID(t, olga.profileJSON(t))
	if code, b := e.do(t, "DELETE", "/api/admin/profiles/"+strconv.FormatInt(id, 10)+"/oidc", e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("unbind: %d %s", code, b)
	}
	gone(t, "unbound")

	if resp, _ := olga.signIn(t, idp, admin); resp.code != http.StatusSeeOther {
		t.Fatalf("sign-in %d", resp.code)
	}
	open(t)
	// The token session made at the start ended with the clock: a fresh one.
	if code, _ := keeper.login(t, e.admin, "203.0.113.9"); code != http.StatusCreated {
		t.Fatalf("token login %d", code)
	}
	id = profileID(t, olga.profileJSON(t))
	if code, b := e.do(t, "DELETE", "/api/admin/profiles/"+strconv.FormatInt(id, 10), e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, b)
	}
	gone(t, "profile deleted")
	if code, list := keeper.sessions(t); code != http.StatusOK || len(list) != 1 {
		t.Errorf("token session: %d %+v", code, list)
	}
}

// 021-AC10: a refused unbind, of an external, leaves its daemon connected; a bound profile's unbind
// fails the turn its daemon runs, as the host removing the room does.
func TestOIDCUnbindDaemons(t *testing.T) {
	e := newOIDCEnv(t, newTestIdP(t), zap.NewNop(), nil)
	r := newTurnRoom(t, e)
	var pid int64
	if err := e.sqlDB(t).QueryRowContext(t.Context(), `SELECT profile_id FROM participants WHERE id = $1`, r.alice.p.Participant.ID).Scan(&pid); err != nil || pid == 0 {
		t.Fatalf("alice's profile: %d %v", pid, err)
	}
	unbind := func() int {
		code, _ := e.do(t, "DELETE", "/api/admin/profiles/"+strconv.FormatInt(pid, 10)+"/oidc", e.admin, nil)
		return code
	}
	conns := func() int {
		var n int
		if err := e.sqlDB(t).QueryRowContext(t.Context(), `SELECT count(*) FROM daemon_connections c JOIN daemons d ON d.id = c.daemon_id
			WHERE d.participant_id = $1`, r.alice.p.Participant.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if conns() == 0 {
		t.Fatal("no daemon connection to keep")
	}
	if code := unbind(); code != http.StatusNotFound || conns() == 0 {
		t.Fatalf("external: %d, %d connections", code, conns())
	}
	if code, b := e.do(t, "POST", r.daemonPath("status"), r.alice.tok, map[string]any{"can_host": true}); code != http.StatusNoContent {
		t.Fatalf("daemon after a refused unbind: %d %s", code, b)
	}
	if _, err := e.sqlDB(t).ExecContext(t.Context(), `UPDATE profiles SET oidc_issuer = $1, oidc_subject = 'alice', oidc_signed_in_at = $2 WHERE id = $3`,
		e.srv.opts.OIDC.cfg.Issuer, time.Now().UTC().Format(time.RFC3339Nano), pid); err != nil {
		t.Fatal(err)
	}
	tn := e.press(t, r, r.alice, r.alice.p.Token, false)
	if code := unbind(); code != http.StatusNoContent {
		t.Fatalf("bound: %d", code)
	}
	running, err := e.st.RunningTurns(t.Context(), r.room)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range running {
		if x.ID == tn.ID {
			t.Errorf("turn %d still running after the unbind", tn.ID)
		}
	}
}
