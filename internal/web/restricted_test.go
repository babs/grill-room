package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// restrictedActor is a caller of the rules table: the admin (no device, no bearer), a browser, or a
// participant whose profile is gone, by its seat token; invite is one it made while the instance
// was open.
type restrictedActor struct {
	name   string
	d      *device
	bearer string
	invite string
}

// do sends a request as the actor; the admin goes by the admin token.
func (a restrictedActor) do(t *testing.T, e *env, method, path string, body any) (int, []byte) {
	t.Helper()
	switch {
	case a.d != nil:
		return a.d.do(t, method, path, body)
	case a.bearer != "":
		return e.do(t, method, path, a.bearer, body)
	}
	return e.do(t, method, path, e.admin, body)
}

// restrictedRoom is a room the admin made with an internal, an external room admin, an external
// member and a participant without a profile in it, each with an invite made while the instance
// was open. The mode is off when it returns; o turns it on.
func restrictedRoom(t *testing.T) (e *env, o *OIDC, room string, actors []restrictedActor) {
	t.Helper()
	e = newOIDCEnv(t, newTestIdP(t), zap.NewNop(), nil)
	o, e.srv.opts.OIDC = e.srv.opts.OIDC, nil
	e.srv.opts.StagingDir = t.TempDir()
	room = e.createRoom(t, "R").ID
	db := e.sqlDB(t)
	member := func(name string) (*device, int64) {
		d := e.device(t)
		d.join(t, room, name)
		return d, d.profile(t).Profile.ID
	}
	internal, ip := member("Ines")
	if _, err := db.ExecContext(t.Context(), `UPDATE profiles SET oidc_issuer = $1, oidc_subject = 'ines', oidc_signed_in_at = $3 WHERE id = $2`, o.cfg.Issuer, ip,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	extAdmin, ap := member("Ezra")
	if _, err := db.ExecContext(t.Context(), `INSERT INTO room_admins(room_id, profile_id, created_at) VALUES($1, $2, '2026-01-01T00:00:00Z')`, room, ap); err != nil {
		t.Fatal(err)
	}
	extMember, _ := member("Max")
	bare := e.join(t, e.createInvite(t, room).Token, "Nora")
	if _, err := db.ExecContext(t.Context(), `UPDATE participants SET profile_id = NULL WHERE id = $1`, bare.Participant.ID); err != nil {
		t.Fatal(err)
	}
	// 021-AC8: an internal room admin whose sign-in expired, last so the indexes above stay.
	expired, xp := member("Exa")
	if _, err := db.ExecContext(t.Context(), `UPDATE profiles SET oidc_issuer = $1, oidc_subject = 'exa', oidc_signed_in_at = $3 WHERE id = $2`, o.cfg.Issuer, xp,
		time.Now().Add(-o.cfg.Revalidate-time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO room_admins(room_id, profile_id, created_at) VALUES($1, $2, '2026-01-01T00:00:00Z')`, room, xp); err != nil {
		t.Fatal(err)
	}
	actors = []restrictedActor{{name: "admin"}, {name: "internal", d: internal}, {name: "external room admin", d: extAdmin},
		{name: "external member", d: extMember}, {name: "no profile", bearer: bare.Token}, {name: "expired internal room admin", d: expired}}
	for i, a := range actors {
		code, b := a.do(t, e, "POST", "/api/rooms/"+room+"/invites", nil)
		if code != http.StatusCreated {
			t.Fatalf("%s invites while open: %d %s", a.name, code, b)
		}
		actors[i].invite = decodeJSON[invite](t, b).Token
	}
	return e, o, room, actors
}

// outcome is "ok" for an answer that passed the rules, the status and error otherwise. An import
// of a body that is no archive passed them when it is refused as such.
func outcome(t *testing.T, code int, b []byte) string {
	t.Helper()
	if code < 300 || code == http.StatusUnprocessableEntity {
		return "ok"
	}
	return fmt.Sprintf("%d %s", code, errorOf(t, b))
}

const (
	cellOK      = "ok"
	cellCreate  = "403 Sign in with your organisation account to create rooms"
	cellInvite  = "403 Only members of the organisation and room admins can invite"
	cellLapsed  = "401 This invite no longer works"
	cellNoReach = ""
	cellReauth  = "401 " + msgReauth
)

// 021-AC6 021-AC7 021-AC8: every actor × create room, import, invite, shareable link, join through the
// invite it made while open; restricted mode off, every cell is allowed as before. A cell left
// empty is one the actor never reaches: no profile to create with, no shareable link for the admin.
func TestRestrictedRules(t *testing.T) {
	ops := []string{"create room", "import", "invite", "share", "join"}
	restricted := map[string][]string{
		"admin":               {cellOK, cellOK, cellOK, cellNoReach, cellOK},
		"internal":            {cellOK, cellOK, cellOK, cellOK, cellOK},
		"external room admin": {cellCreate, cellCreate, cellOK, cellOK, cellOK},
		"external member":     {cellCreate, cellCreate, cellInvite, cellInvite, cellLapsed},
		"no profile":          {cellNoReach, cellNoReach, cellInvite, cellInvite, cellLapsed},
		// 021-AC8: room admin or not, its invites lapse until it signs in again.
		"expired internal room admin": {cellReauth, cellReauth, cellReauth, cellReauth, cellLapsed},
	}
	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprintf("restricted=%v", on), func(t *testing.T) {
			e, o, room, actors := restrictedRoom(t)
			if on {
				e.srv.opts.OIDC = o
			}
			for _, a := range actors {
				for i, op := range ops {
					want := restricted[a.name][i]
					if want == cellNoReach {
						continue
					}
					if !on {
						want = cellOK
					}
					var code int
					var b []byte
					switch op {
					case "create room":
						path := "/api/profile/rooms"
						if a.d == nil {
							path = "/api/rooms"
						}
						code, b = a.do(t, e, "POST", path, map[string]string{"name": "New", "display_name": a.name})
					case "import":
						d, tok := a.d, ""
						if d == nil {
							tok = e.admin
						}
						code, b = e.send(t, d, "/api/rooms/import?display_name=Imp", tok, []byte("not an archive"), "application/gzip")
					case "invite":
						code, b = a.do(t, e, "POST", "/api/rooms/"+room+"/invites", nil)
					case "share":
						code, b = a.do(t, e, "POST", "/api/rooms/"+room+"/invites/share", nil)
					case "join":
						code, b = e.do(t, "POST", "/api/join", "", map[string]string{"invite": a.invite, "name": "Guest"})
					}
					if got := outcome(t, code, b); got != want {
						t.Errorf("%s, %s: %s, want %s", a.name, op, got, want)
					}
				}
			}
		})
	}
}

// 021-AC7: the rule is checked at join time: a room admin who steps down takes their invites with
// them, and stepping up again brings them back.
func TestRestrictedInviteOfFormerRoomAdmin(t *testing.T) {
	e, o, room, actors := restrictedRoom(t)
	e.srv.opts.OIDC = o
	ezra := actors[2]
	join := func() string {
		code, b := e.do(t, "POST", "/api/join", "", map[string]string{"invite": ezra.invite, "name": "Guest"})
		return outcome(t, code, b)
	}
	if got := join(); got != cellOK {
		t.Fatalf("room admin's invite: %s", got)
	}
	db := e.sqlDB(t)
	if _, err := db.ExecContext(t.Context(), `DELETE FROM room_admins WHERE room_id = $1 AND profile_id = $2`, room, ezra.d.profile(t).Profile.ID); err != nil {
		t.Fatal(err)
	}
	if got := join(); got != cellLapsed {
		t.Fatalf("after stepping down: %s", got)
	}
	// The join page tells a 401 as an invalid or revoked invite, whatever its text.
	if code, _ := e.do(t, "POST", "/api/join", "", map[string]string{"invite": "nope", "name": "Guest"}); code != http.StatusUnauthorized {
		t.Fatalf("unknown invite: %d", code)
	}
}

// 021-AC6: an API token, as the MCP tools use it, is held to the same rule as the browser.
func TestRestrictedRulesAPIToken(t *testing.T) {
	e, o, _, actors := restrictedRoom(t)
	ext, internal := actors[3].d.newToken(t, "full", 0).Token, actors[1].d.newToken(t, "full", 0).Token
	e.srv.opts.OIDC = o
	body := map[string]string{"name": "New", "display_name": "Bot"}
	if code, b := e.do(t, "POST", "/api/profile/rooms", ext, body); outcome(t, code, b) != cellCreate {
		t.Errorf("external token: %d %s", code, b)
	}
	if r := e.call(t, ext, "create_room", body); !r.IsError || !strings.Contains(r.text(), "organisation account") {
		t.Errorf("external MCP create_room: %+v", r)
	}
	if code, b := e.do(t, "POST", "/api/profile/rooms", internal, body); code != http.StatusCreated {
		t.Errorf("internal token: %d %s", code, b)
	}
}

// 021-AC2: an instance key that fails to load answers 500 at login and at the callback, with no
// sign-in cookie set; the callback logs reason "server".
func TestOIDCKeyLoadFails(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	e := newOIDCEnv(t, newTestIdP(t), zap.New(core), nil)
	d := e.device(t)
	state := NewToken()
	u, _ := url.Parse(e.ts.URL + "/auth")
	d.c.Jar.SetCookies(u, []*http.Cookie{{Name: oidcCookieName(state), Value: "x.y", Path: "/auth"}})
	_ = e.st.Close()
	resp := d.noFollow(t, "GET", "/auth/login")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError || logs.FilterMessage("oidc_login_failed").Len() != 1 {
		t.Errorf("login: %d, logged %d", resp.StatusCode, logs.FilterMessage("oidc_login_failed").Len())
	}
	for _, c := range resp.Cookies() {
		if strings.HasPrefix(c.Name, oidcCookiePrefix) {
			t.Errorf("login set %s", c.Name)
		}
	}
	if a, _ := d.callback(t, url.Values{"state": {state}, "code": {"c"}}); a.code != http.StatusInternalServerError {
		t.Errorf("callback: %d", a.code)
	}
	if f := logs.FilterMessage("oidc_callback_failed").All(); len(f) != 1 || f[0].ContextMap()["reason"] != "server" {
		t.Errorf("callback logs: %+v", f)
	}
}

// 021-AC4: the replaced session failing to end does not fail the sign-in: the browser still gets
// its new profile cookie, and a warning says so.
func TestOIDCReplacedSessionFails(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	idp := newTestIdP(t)
	e := newOIDCEnv(t, idp, zap.New(core), nil)
	d := e.device(t)
	d.join(t, e.createRoom(t, "R").ID, "Guest")
	var old string
	for _, c := range d.c.Jar.Cookies(&url.URL{Scheme: "http", Host: strings.TrimPrefix(e.ts.URL, "http://"), Path: "/"}) {
		if c.Name == profileCookie {
			old = c.Value
		}
	}
	if _, err := e.sqlDB(t).ExecContext(t.Context(), fmt.Sprintf(`CREATE FUNCTION keep_session() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF OLD.token_hash = '%s' THEN RAISE EXCEPTION 'kept'; END IF; RETURN OLD; END $$;
		CREATE TRIGGER keep_session BEFORE DELETE ON profile_sessions FOR EACH ROW EXECUTE FUNCTION keep_session();`, HashToken(old))); err != nil {
		t.Fatal(err)
	}
	a, _ := d.signIn(t, idp, map[string]any{"sub": "g"})
	if c := a.cookie(profileCookie); a.code != http.StatusSeeOther || c == nil || c.Value == "" || c.Value == old {
		t.Errorf("sign-in: %d %+v", a.code, c)
	}
	if logs.FilterMessage("oidc_replaced_session_failed").Len() != 1 {
		t.Errorf("no warning: %v", logs.All())
	}
}

// 021-AC5 021-AC6 021-AC7: a profile bound to another issuer than the configured one, as after a
// change of provider, is external: GET /api/profile says so, and every rule holds it as one.
func TestRestrictedFormerIssuer(t *testing.T) {
	e, o, room, actors := restrictedRoom(t)
	e.srv.opts.OIDC = o
	ines := actors[1]
	if in := ines.d.profileJSON(t)["internal"]; in != true {
		t.Fatalf("bound to the configured issuer: internal %v", in)
	}
	if _, err := e.sqlDB(t).ExecContext(t.Context(), `UPDATE profiles SET oidc_issuer = 'https://former.example' WHERE id = $1`, ines.d.profile(t).Profile.ID); err != nil {
		t.Fatal(err)
	}
	if in := ines.d.profileJSON(t)["internal"]; in != false {
		t.Errorf("bound to a former issuer: internal %v", in)
	}
	for _, c := range []struct {
		op, want string
		do       func() (int, []byte)
	}{
		{"create room", cellCreate, func() (int, []byte) {
			return ines.do(t, e, "POST", "/api/profile/rooms", map[string]string{"name": "New", "display_name": "Ines"})
		}},
		{"import", cellCreate, func() (int, []byte) {
			return e.send(t, ines.d, "/api/rooms/import?display_name=Imp", "", []byte("not an archive"), "application/gzip")
		}},
		{"invite", cellInvite, func() (int, []byte) { return ines.do(t, e, "POST", "/api/rooms/"+room+"/invites", nil) }},
		{"share", cellInvite, func() (int, []byte) { return ines.do(t, e, "POST", "/api/rooms/"+room+"/invites/share", nil) }},
		{"join", cellLapsed, func() (int, []byte) {
			return e.do(t, "POST", "/api/join", "", map[string]string{"invite": ines.invite, "name": "Guest"})
		}},
	} {
		if code, b := c.do(); outcome(t, code, b) != c.want {
			t.Errorf("%s: %s, want %s", c.op, outcome(t, code, b), c.want)
		}
	}
}

// 021-AC7, 021-AC8: an internal's invite lapses once its sign-in is past the revalidation, and works
// again after the next one.
func TestRestrictedInviteOfExpiredInternal(t *testing.T) {
	idp := newTestIdP(t)
	e := newOIDCEnv(t, idp, zap.NewNop(), nil)
	clk := e.clock()
	ines := e.device(t)
	if resp, _ := ines.signIn(t, idp, map[string]any{"sub": "ines"}); resp.code != http.StatusSeeOther {
		t.Fatalf("sign-in %d", resp.code)
	}
	room := e.createRoom(t, "R").ID
	ines.join(t, room, "Ines")
	code, b := ines.do(t, "POST", "/api/rooms/"+room+"/invites", nil)
	if code != http.StatusCreated {
		t.Fatalf("invite: %d %s", code, b)
	}
	tok := decodeJSON[invite](t, b).Token
	join := func() (int, []byte) {
		return e.do(t, "POST", "/api/join", "", map[string]string{"invite": tok, "name": "Guest"})
	}
	clk.add(DefaultOIDCRevalidate + time.Hour)
	if code, b := join(); code != http.StatusUnauthorized || errorOf(t, b) != "This invite no longer works" {
		t.Fatalf("expired creator's invite: %d %s", code, b)
	}
	if resp, _ := ines.signIn(t, idp, map[string]any{"sub": "ines"}); resp.code != http.StatusSeeOther {
		t.Fatalf("sign-in %d", resp.code)
	}
	if code, b := join(); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("after a new sign-in: %d %s", code, b)
	}
}
