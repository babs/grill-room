package web

import (
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/livekit"
	"github.com/babs/grill-room/internal/store"
)

var offerRe = regexp.MustCompile(`name="offer" value="([^"]+)"`)

// sessionOf is the profile cookie d holds.
func sessionOf(t *testing.T, d *device) string {
	t.Helper()
	u, _ := url.Parse(d.e.ts.URL + "/")
	for _, c := range d.c.Jar.Cookies(u) {
		if c.Name == profileCookie {
			return c.Value
		}
	}
	return ""
}

// guestDevice is a browser that joined a room by invite as name, with no email: a guest.
func guestDevice(t *testing.T, e *env, name string) (*device, int64, string) {
	t.Helper()
	d := e.device(t)
	room := e.createRoom(t, name+"'s room")
	d.join(t, room.ID, name)
	return d, profileID(t, d.profileJSON(t)), room.ID
}

// offerOf signs d in as sub and returns the callback's answer, page and attach offer, "" for none.
func offerOf(t *testing.T, d *device, idp *testIdP, sub string) (answer, string, string) {
	t.Helper()
	a, body := d.signIn(t, idp, map[string]any{"sub": sub})
	offer := ""
	if m := offerRe.FindStringSubmatch(body); m != nil {
		offer = html.UnescapeString(m[1])
	}
	return a, body, offer
}

// postAttach posts the attach form as d, with extra headers, without following the redirect.
func postAttach(t *testing.T, d *device, offer, choice string, hdr ...string) (int, string, string) {
	t.Helper()
	c := *d.c
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(t.Context(), "POST", d.e.ts.URL+"/auth/attach", strings.NewReader(url.Values{"offer": {offer}, "choice": {choice}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(b), resp.Header.Get("Location")
}

func (e *env) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.sqlDB(t).QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *env) sessions(t *testing.T, tok string) int {
	t.Helper()
	return e.count(t, `SELECT count(*) FROM profile_sessions WHERE token_hash = $1`, HashToken(tok))
}

func (e *env) merges(t *testing.T) int {
	t.Helper()
	return e.count(t, `SELECT count(*) FROM audit_log WHERE action = $1`, ActionProfileMerge)
}

// seatlessGuest is a browser holding a guest profile without a seat, so without a name; with
// things, it has a personal and a published skill, a persona template and a starter.
func seatlessGuest(t *testing.T, e *env, things bool) (*device, int64) {
	t.Helper()
	db := e.sqlDB(t)
	id := insertProfile(t, e, "", "")
	tok := NewToken()
	queries := []string{`INSERT INTO profile_sessions(profile_id, token_hash, created_at, expires_at_s) VALUES($1, $2, '2026-01-01T00:00:00Z', $3)`}
	if things {
		queries = append(queries, `INSERT INTO skills(name, profile_id, created_at) VALUES('notes', $1, '2026-01-01T00:00:00Z')`,
			`INSERT INTO skills(name, publisher_id, created_at) VALUES('shared-notes', $1, '2026-01-01T00:00:00Z')`,
			`INSERT INTO persona_templates(profile_id, title, created_at) VALUES($1, 'Critic', '2026-01-01T00:00:00Z')`,
			`INSERT INTO starters(profile_id, library, created_at) VALUES($1, 'personal', '2026-01-01T00:00:00Z')`)
	}
	for i, q := range queries {
		args := []any{id}
		if i == 0 {
			args = append(args, HashToken(tok), time.Now().Add(time.Hour).Unix())
		}
		if _, err := db.ExecContext(t.Context(), q, args...); err != nil {
			t.Fatal(err)
		}
	}
	return cookieDevice(e, tok), id
}

// cookieDevice is a browser holding the profile session tok.
func cookieDevice(e *env, tok string) *device {
	d := &device{e: e, c: &http.Client{Jar: newJar()}}
	u, _ := url.Parse(e.ts.URL + "/")
	d.c.Jar.SetCookies(u, []*http.Cookie{{Name: profileCookie, Value: tok, Path: "/"}})
	return d
}

func newJar() http.CookieJar { j, _ := cookiejar.New(nil); return j }

// 026-AC3: the offer shows only for an unbound, emailless guest the browser held at login, still
// one at the callback, with something to move; it names both sides and lists what moves. The
// browser is signed in either way.
func TestOIDCAttachOffer(t *testing.T) {
	idp := newTestIdP(t)
	e := newOIDCEnv(t, idp, zap.NewNop(), nil)
	setBound := func(t *testing.T, id int64) {
		if _, err := e.sqlDB(t).ExecContext(t.Context(), `UPDATE profiles SET oidc_issuer = 'https://other.example', oidc_subject = 'o' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	gwen := func(t *testing.T) (*device, int64) { d, id, _ := guestDevice(t, e, "Gwen"); return d, id }
	for _, tc := range []struct {
		name   string
		device func(t *testing.T) (*device, int64)
		// guest runs before the login, before after it, ahead of the callback.
		guest, before func(t *testing.T, id int64)
		claims        map[string]any
		// want is the offer's question, "" for no offer; moves the list it shows.
		want  string
		moves []string
	}{
		{name: "emailless guest", device: gwen, want: "Attach your guest profile Gwen (1 room) to your organisation account?", moves: []string{"Rooms: Gwen's room"}},
		{name: "named account", device: gwen, claims: map[string]any{"name": "Hana Org", "email": "hana@example.com", "email_verified": true},
			want: "Attach your guest profile Gwen (1 room) to your organisation account, signed in as Hana Org?", moves: []string{"Rooms: Gwen's room"}},
		{name: "account with an email only", device: gwen, claims: map[string]any{"email": "ivo@example.com", "email_verified": true},
			want: "Attach your guest profile Gwen (1 room) to your organisation account, signed in as ivo@example.com?"},
		{name: "nameless guest with things", device: func(t *testing.T) (*device, int64) { return seatlessGuest(t, e, true) },
			want: "Attach your guest profile Guest (0 rooms) to your organisation account?", moves: []string{"1 personal skill", "1 published skill", "1 persona template", "1 starter"}},
		{name: "guest with nothing to move", device: func(t *testing.T) (*device, int64) { return seatlessGuest(t, e, false) }},
		{name: "guest with an email", device: gwen, guest: func(t *testing.T, id int64) { setEmail(t, e, id, "had@example.com") }},
		{name: "emailed at login, not at the callback", device: gwen, guest: func(t *testing.T, id int64) { setEmail(t, e, id, "was@example.com") },
			before: func(t *testing.T, id int64) { setEmail(t, e, id, "") }},
		{name: "got an email before the callback", device: gwen, before: func(t *testing.T, id int64) { setEmail(t, e, id, "late@example.com") }},
		{name: "bound before the callback", device: gwen, before: setBound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, guest := tc.device(t)
			old := sessionOf(t, d)
			if tc.guest != nil {
				tc.guest(t, guest)
			}
			auth, _, _ := d.startLogin(t, "/r/room-1")
			if tc.before != nil {
				tc.before(t, guest)
			}
			claims := map[string]any{"sub": "u-" + strings.ReplaceAll(tc.name, " ", "-")}
			maps.Copy(claims, tc.claims)
			a, body := d.callback(t, url.Values{"state": {auth.Get("state")}, "code": {idp.issue(t, auth, claims, nil)}})
			body = html.UnescapeString(body)
			if c := a.cookie(profileCookie); c == nil || c.Value == old {
				t.Errorf("not signed in: %+v", c)
			}
			if tc.want == "" {
				if a.code != http.StatusSeeOther || offerRe.MatchString(body) || e.sessions(t, old) != 0 {
					t.Errorf("no offer expected: %d, guest session %d", a.code, e.sessions(t, old))
				}
				return
			}
			if a.code != http.StatusOK || !strings.Contains(body, "<p>"+tc.want+"</p>") || !offerRe.MatchString(body) || a.header.Get("Cache-Control") != "no-store" {
				t.Errorf("offer: %d %q", a.code, body)
			}
			if !strings.Contains(body, "<p>Your guest profile's other devices are signed out and its paired AI hosts removed.</p>") {
				t.Errorf("offer lacks the sign-out note: %q", body)
			}
			for _, m := range tc.moves {
				if !strings.Contains(body, "<li>"+m+"</li>") {
					t.Errorf("offer lacks %q: %q", m, body)
				}
			}
			// 026-AC3: the guest session stays until the answer, no longer than the offer.
			if left := e.count(t, `SELECT count(*) FROM profile_sessions WHERE token_hash = $1 AND expires_at_s <= $2`, HashToken(old),
				time.Now().Add(oidcStateTTL).Unix()+1); left != 1 {
				t.Error("guest session ended before the answer, or outlives the offer")
			}
		})
	}
	// A silent login refused by the provider starts a normal flow that keeps the offer.
	g, _, _ := guestDevice(t, e, "Gwen")
	resp := g.noFollow(t, "GET", "/auth/login?prompt=none&next=/r/x")
	_ = resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	a, _ := g.callback(t, url.Values{"state": {loc.Query().Get("state")}, "error": {"login_required"}})
	loc, _ = url.Parse(a.header.Get("Location"))
	normal := loc.Query()
	if a, body := g.callback(t, url.Values{"state": {normal.Get("state")}, "code": {idp.issue(t, normal, map[string]any{"sub": "silent"}, nil)}}); a.code != http.StatusOK || !offerRe.MatchString(body) {
		t.Errorf("silent then normal: %d", a.code)
	}
	// No profile on the browser, then a browser already in the account: no offer.
	d := e.device(t)
	for range 2 {
		if a, _, offer := offerOf(t, d, idp, "plain"); a.code != http.StatusSeeOther || offer != "" {
			t.Errorf("offer without a guest: %d", a.code)
		}
	}
}

// 026-AC4: Attach merges the guest into the signed-in profile, audited as the profile with via
// oidc_attach, and ends the guest session; Not now keeps both apart and ends it too.
func TestOIDCAttach(t *testing.T) {
	idp := newTestIdP(t)
	e := newOIDCEnv(t, idp, zap.NewNop(), nil)
	d, guest, room := guestDevice(t, e, "Gwen")
	old := sessionOf(t, d)
	_, _, offer := offerOf(t, d, idp, "gwen")
	me := profileID(t, d.profileJSON(t))
	if code, _, loc := postAttach(t, d, offer, "attach"); code != http.StatusSeeOther || loc != "/r/room-1" {
		t.Fatalf("attach: %d %s", code, loc)
	}
	p := d.profile(t).Profile
	if p.ID != me || len(p.Rooms) != 1 || p.Rooms[0].RoomID != room {
		t.Errorf("signed-in profile after attach %+v", p)
	}
	if e.count(t, `SELECT count(*) FROM profiles WHERE id = $1`, guest) != 0 || e.sessions(t, old) != 0 {
		t.Error("guest profile or session left")
	}
	var actor, target, details, uuid string
	if err := e.sqlDB(t).QueryRowContext(t.Context(), `SELECT a.actor, a.target_id, a.details, p.uuid FROM audit_log a JOIN profiles p ON p.id = $2
		WHERE a.action = $1`, ActionProfileMerge, me).Scan(&actor, &target, &details, &uuid); err != nil {
		t.Fatal(err)
	}
	var dm map[string]any
	_ = json.Unmarshal([]byte(details), &dm)
	if actor != store.ActorProfile+uuid || target != strconv.FormatInt(me, 10) || dm["via"] != "oidc_attach" || dm["merge_name"] != "Gwen" || dm["keep_uuid"] != uuid {
		t.Errorf("audit %s %s %s", actor, target, details)
	}

	d2, guest2, room2 := guestDevice(t, e, "Hal")
	old2 := sessionOf(t, d2)
	_, _, offer2 := offerOf(t, d2, idp, "hal")
	if code, _, loc := postAttach(t, d2, offer2, "later"); code != http.StatusSeeOther || loc != "/r/room-1" {
		t.Fatalf("not now: %d %s", code, loc)
	}
	if p := d2.profile(t).Profile; len(p.Rooms) != 0 || p.ID == guest2 {
		t.Errorf("signed-in profile after not now %+v", p)
	}
	if e.count(t, `SELECT count(*) FROM participants WHERE profile_id = $1 AND room_id = $2`, guest2, room2) != 1 || e.sessions(t, old2) != 0 || e.merges(t) != 1 {
		t.Error("not now merged, or kept the guest session")
	}
}

// 026-AC5: every refused attach answers the expired page and merges nothing.
func TestOIDCAttachRefused(t *testing.T) {
	idp := newTestIdP(t)
	e := newOIDCEnv(t, idp, zap.NewNop(), nil)
	for _, tc := range []struct {
		name string
		code int
		// post answers the offer as the refused case does; gone is a guest it removes.
		post func(t *testing.T, d *device, guest int64, offer string) (int, string)
		gone bool
	}{
		{"expired", http.StatusBadRequest, func(t *testing.T, d *device, _ int64, offer string) (int, string) {
			prev := e.srv.now
			e.srv.now = func() time.Time { return prev().Add(oidcStateTTL + time.Second) }
			defer func() { e.srv.now = prev }()
			code, body, _ := postAttach(t, d, offer, "attach")
			return code, body
		}, false},
		{"altered", http.StatusBadRequest, func(t *testing.T, d *device, _ int64, offer string) (int, string) {
			enc, mac, _ := strings.Cut(offer, ".")
			b, _ := base64.RawURLEncoding.DecodeString(enc)
			b = []byte(strings.Replace(string(b), `"next":"/r/room-1"`, `"next":"/r/room-2"`, 1))
			code, body, _ := postAttach(t, d, base64.RawURLEncoding.EncodeToString(b)+"."+mac, "attach")
			return code, body
		}, false},
		{"a state sealed as an offer", http.StatusBadRequest, func(t *testing.T, d *device, _ int64, offer string) (int, string) {
			var o attachOffer
			if ok, err := e.srv.unseal(t.Context(), oidcAttachKeyUse, offer, &o); !ok || err != nil {
				t.Fatalf("offer %v %v", ok, err)
			}
			forged, err := e.srv.seal(t.Context(), oidcStateKeyUse, o)
			if err != nil {
				t.Fatal(err)
			}
			code, body, _ := postAttach(t, d, forged, "attach")
			return code, body
		}, false},
		{"empty", http.StatusBadRequest, func(t *testing.T, d *device, _ int64, _ string) (int, string) {
			code, body, _ := postAttach(t, d, "", "attach")
			return code, body
		}, false},
		{"replayed after attach", http.StatusBadRequest, func(t *testing.T, d *device, _ int64, offer string) (int, string) {
			if code, _, _ := postAttach(t, d, offer, "attach"); code != http.StatusSeeOther {
				t.Fatalf("first attach %d", code)
			}
			code, body, _ := postAttach(t, d, offer, "attach")
			return code, body
		}, true},
		{"replayed after not now", http.StatusBadRequest, func(t *testing.T, d *device, _ int64, offer string) (int, string) {
			if code, _, _ := postAttach(t, d, offer, "later"); code != http.StatusSeeOther {
				t.Fatalf("not now %d", code)
			}
			code, body, _ := postAttach(t, d, offer, "attach")
			return code, body
		}, false},
		{"cross-site", http.StatusForbidden, func(t *testing.T, d *device, _ int64, offer string) (int, string) {
			code, body, _ := postAttach(t, d, offer, "attach", "Sec-Fetch-Site", "cross-site")
			return code, body
		}, false},
		{"same-site", http.StatusForbidden, func(t *testing.T, d *device, _ int64, offer string) (int, string) {
			code, body, _ := postAttach(t, d, offer, "attach", "Sec-Fetch-Site", "same-site")
			return code, body
		}, false},
		{"no profile cookie", http.StatusForbidden, func(t *testing.T, _ *device, _ int64, offer string) (int, string) {
			code, body, _ := postAttach(t, e.device(t), offer, "attach")
			return code, body
		}, false},
		{"another profile's cookie", http.StatusForbidden, func(t *testing.T, _ *device, _ int64, offer string) (int, string) {
			other := e.device(t)
			if a, _ := other.signIn(t, idp, map[string]any{"sub": "other"}); a.code != http.StatusSeeOther {
				t.Fatalf("other sign-in %d", a.code)
			}
			code, body, _ := postAttach(t, other, offer, "attach")
			return code, body
		}, false},
		{"guest got an email", http.StatusBadRequest, func(t *testing.T, d *device, guest int64, offer string) (int, string) {
			setEmail(t, e, guest, "now@example.com")
			code, body, _ := postAttach(t, d, offer, "attach")
			return code, body
		}, false},
		{"guest got bound", http.StatusBadRequest, func(t *testing.T, d *device, guest int64, offer string) (int, string) {
			if _, err := e.sqlDB(t).ExecContext(t.Context(), `UPDATE profiles SET oidc_issuer = 'https://other.example', oidc_subject = 'b' WHERE id = $1`, guest); err != nil {
				t.Fatal(err)
			}
			code, body, _ := postAttach(t, d, offer, "attach")
			return code, body
		}, false},
		{"guest merged into another guest", http.StatusBadRequest, func(t *testing.T, d *device, guest int64, offer string) (int, string) {
			_, other, _ := guestDevice(t, e, "Ivo")
			if _, _, _, err := e.srv.bus.Store.MergeProfiles(t.Context(), other, guest, ""); err != nil {
				t.Fatal(err)
			}
			code, body, _ := postAttach(t, d, offer, "attach")
			return code, body
		}, true},
		{"guest deleted", http.StatusBadRequest, func(t *testing.T, d *device, guest int64, offer string) (int, string) {
			if _, err := e.sqlDB(t).ExecContext(t.Context(), `DELETE FROM profiles WHERE id = $1`, guest); err != nil {
				t.Fatal(err)
			}
			code, body, _ := postAttach(t, d, offer, "attach")
			return code, body
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, guest, room := guestDevice(t, e, "Gwen")
			a, _, offer := offerOf(t, d, idp, "r-"+strings.ReplaceAll(tc.name, " ", "-"))
			if a.code != http.StatusOK || offer == "" {
				t.Fatalf("no offer: %d", a.code)
			}
			me := profileID(t, d.profileJSON(t))
			// The first answer of a replay merges once.
			replayed := map[bool]int{true: 1}[tc.name == "replayed after attach"]
			before := e.merges(t)
			code, body := tc.post(t, d, guest, offer)
			if code != tc.code || !strings.Contains(body, oidcAttachText) {
				t.Errorf("answer %d %q", code, body)
			}
			if e.merges(t) != before+replayed || e.count(t, `SELECT count(*) FROM participants WHERE profile_id = $1`, me) != replayed {
				t.Error("merged")
			}
			if !tc.gone && e.count(t, `SELECT count(*) FROM participants WHERE profile_id = $1 AND room_id = $2`, guest, room) != 1 {
				t.Error("guest lost its seat")
			}
		})
	}
}

// 026-AC4: off, the route does not exist; an API token never reaches it.
func TestOIDCAttachRoute(t *testing.T) {
	if code, _ := newEnv(t).device(t).do(t, "POST", "/auth/attach", nil); code != http.StatusNotFound {
		t.Errorf("mode off: %d", code)
	}
	e := newOIDCEnv(t, newTestIdP(t), zap.NewNop(), nil)
	alice := e.device(t)
	alice.join(t, e.createRoom(t, "R").ID, "Alice")
	if code, b := e.do(t, "POST", "/auth/attach", alice.newToken(t, "full", 0).Token, nil); code != http.StatusForbidden || errorOf(t, b) != msgTokenCookie {
		t.Errorf("token: %d %s", code, b)
	}
}

// 026-AC4: Attach ends every session and seat token of the guest: a phone that recovered the
// kiosk's guest does not end up in the signed-in internal profile.
func TestOIDCAttachSignsOutTheGuestEverywhere(t *testing.T) {
	idp := newTestIdP(t)
	e := newOIDCEnv(t, idp, zap.NewNop(), nil)
	room := e.createRoom(t, "Kiosk room")
	kiosk := e.device(t)
	seat := kiosk.join(t, room.ID, "Mallory")
	phone := e.device(t)
	if code, _ := phone.do(t, "POST", "/api/profile/recover", map[string]string{"token": strings.TrimPrefix(seat.RecoveryURL, baseURL+"/profile#recover=")}); code != http.StatusOK {
		t.Fatal("recover failed")
	}
	if code, _ := e.do(t, "GET", "/api/rooms/"+room.ID, seat.Token, nil); code != http.StatusOK {
		t.Fatalf("seat token before: %d", code)
	}
	_, _, offer := offerOf(t, kiosk, idp, "victim")
	if code, _, _ := postAttach(t, kiosk, offer, "attach"); code != http.StatusSeeOther {
		t.Fatalf("attach %d", code)
	}
	if p := phone.profileJSON(t); p["profile"] != nil || p["internal"] != false {
		t.Errorf("phone after attach: %v", p)
	}
	if code, _ := e.do(t, "GET", "/api/rooms/"+room.ID, seat.Token, nil); code != http.StatusUnauthorized {
		t.Errorf("seat token after: %d", code)
	}
	// The kiosk, now the account, still reaches the room through its profile.
	if p := kiosk.profile(t).Profile; len(p.Rooms) != 1 || p.Rooms[0].RoomID != room.ID {
		t.Errorf("kiosk after attach %+v", p)
	}
}

// 026-AC3: what the offer lists: rooms by name, five at most, then counts.
func TestAttachMoves(t *testing.T) {
	rooms := func(n int) []store.MergeRoom {
		var out []store.MergeRoom
		for i := range n {
			out = append(out, store.MergeRoom{RoomName: "r" + strconv.Itoa(i), Merge: true})
		}
		return append(out, store.MergeRoom{RoomName: "theirs", Keep: true})
	}
	for _, tc := range []struct {
		pv    *store.MergePreview
		rooms int
		moves string
	}{
		{nil, 0, ""},
		{&store.MergePreview{Rooms: rooms(0)}, 0, ""},
		{&store.MergePreview{Rooms: rooms(2), Skills: store.MergeSkills{Merge: []string{"a", "b"}}, SharedSkills: store.MergeCount{Merge: 1}},
			2, "Rooms: r0, r1|2 personal skills|1 published skill"},
		{&store.MergePreview{Rooms: rooms(7), Templates: store.MergeCount{Merge: 3}, Starters: store.MergeCount{Merge: 2}},
			7, "Rooms: r0, r1, r2, r3, r4 and 2 more|3 persona templates|2 starters"},
	} {
		n, moves := attachMoves(tc.pv)
		if n != tc.rooms || strings.Join(moves, "|") != tc.moves {
			t.Errorf("%d %q, want %d %q", n, moves, tc.rooms, tc.moves)
		}
	}
}

// withVoice gives e a fake LiveKit and returns it, for the kicks to land in.
func withVoice(t *testing.T, e *env) *fakeLiveKit {
	t.Helper()
	lk := &fakeLiveKit{}
	lts := httptest.NewServer(lk)
	t.Cleanup(lts.Close)
	e.srv.opts.Voice = &Voice{URL: "wss://livekit.example", Key: "APIkey", Secret: lkSecret,
		API: &livekit.Client{URL: lts.URL, Key: "APIkey", Secret: lkSecret, HTTP: lts.Client()}}
	return lk
}

// guestTurnRoom is a room whose guest, Alice, hosts a daemon running a turn and approved a pairing
// no daemon polled yet; it returns the room, her profile, a browser holding her session, the turn
// and the pairing.
func guestTurnRoom(t *testing.T, e *env) (*turnRoom, int64, *device, store.Turn, pairingResp) {
	t.Helper()
	r := newTurnRoom(t, e)
	var id int64
	if err := e.sqlDB(t).QueryRowContext(t.Context(), `SELECT profile_id FROM participants WHERE id = $1`, r.alice.p.Participant.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	tok := NewToken()
	if _, err := e.sqlDB(t).ExecContext(t.Context(), `INSERT INTO profile_sessions(profile_id, token_hash, created_at, expires_at_s) VALUES($1, $2, '2026-01-01T00:00:00Z', $3)`,
		id, HashToken(tok), time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	pending := e.requestPairing(t, r.room, "alice-desktop")
	if code, b := e.do(t, "POST", "/api/rooms/"+r.room+"/pairings/"+pending.ID, r.alice.p.Token, map[string]bool{"approve": true}); code != http.StatusOK {
		t.Fatalf("approve: %d %s", code, b)
	}
	tn := e.press(t, r, r.alice, r.alice.p.Token, false)
	return r, id, cookieDevice(e, tok), tn, pending
}

// running reports turn id still running in room.
func (e *env) running(t *testing.T, room string, id int64) bool {
	t.Helper()
	turns, err := e.st.RunningTurns(t.Context(), room)
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(turns, func(x store.Turn) bool { return x.ID == id })
}

// 026-AC4: a merge whose result is bound, by Attach or by the admin, in either direction, signs the
// guest's side out everywhere: its browser, its daemons (their running turn failed), the pairings
// its seats approved, and its seats, whose open pages are told at once and which leave the audio.
func TestMergeIntoBoundRemovesDaemons(t *testing.T) {
	idp := newTestIdP(t)
	e := newOIDCEnv(t, idp, zap.NewNop(), nil)
	lk := withVoice(t, e)
	internal := func(t *testing.T, sub string) int64 {
		d := e.device(t)
		if a, _ := d.signIn(t, idp, map[string]any{"sub": sub}); a.code != http.StatusSeeOther {
			t.Fatalf("sign-in %d", a.code)
		}
		return profileID(t, d.profileJSON(t))
	}
	adminMerge := func(t *testing.T, keep, merge int64) {
		if code, b := e.do(t, "POST", "/api/admin/profiles/merge", e.admin, map[string]int64{"keep": keep, "merge": merge}); code != http.StatusOK {
			t.Fatalf("merge %d %s", code, b)
		}
	}
	for _, tc := range []struct {
		name  string
		merge func(t *testing.T, guest *device, id int64)
	}{
		{"attach", func(t *testing.T, guest *device, _ int64) {
			_, _, offer := offerOf(t, guest, idp, "daemon-attach")
			if code, _, _ := postAttach(t, guest, offer, "attach"); code != http.StatusSeeOther {
				t.Fatalf("attach %d", code)
			}
		}},
		{"admin merge into an internal", func(t *testing.T, _ *device, id int64) { adminMerge(t, internal(t, "daemon-admin"), id) }},
		{"admin merge of an internal into the guest", func(t *testing.T, _ *device, id int64) { adminMerge(t, id, internal(t, "daemon-mirror")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, id, guest, tn, pending := guestTurnRoom(t, e)
			// A room without the daemon: only the merge's own notice wakes its stream.
			other := e.createRoom(t, "Other").ID
			stream := e.openEvents(t, other, guest.join(t, other, "Alice").Token, "")
			tc.merge(t, guest, id)
			if e.running(t, r.room, tn.ID) {
				t.Errorf("turn %d still running after the merge", tn.ID)
			}
			if code, b := e.do(t, "POST", r.daemonPath("status"), r.alice.tok, map[string]any{"can_host": true}); code != http.StatusUnauthorized {
				t.Errorf("merged daemon: %d %s", code, b)
			}
			if code, p := e.poll(t, pending); code == http.StatusOK && p.Token != "" {
				t.Errorf("approved pairing polled: %+v", p)
			}
			if code, _ := e.do(t, "GET", "/api/rooms/"+r.room, r.alice.p.Token, nil); code != http.StatusUnauthorized {
				t.Errorf("guest seat token: %d", code)
			}
			if tc.name != "attach" {
				if p := guest.profileJSON(t); p["profile"] != nil {
					t.Errorf("guest browser still signed in: %v", p)
				}
			}
			// Well before the heartbeat (20 s): the merge wakes the seat's room.
			revoked := time.After(3 * time.Second)
		wait:
			for {
				select {
				case ev := <-stream:
					if ev.event == "revoked" {
						break wait
					}
				case <-revoked:
					t.Error("the guest seat's stream was not revoked promptly")
					break wait
				}
			}
			kick := `RemoveParticipant {"identity":"` + itoa(r.alice.p.Participant.ID) + `","room":"` + r.room + `"}`
			for deadline := time.Now().Add(5 * time.Second); !slices.Contains(lk.got(), kick) && time.Now().Before(deadline); {
				time.Sleep(20 * time.Millisecond)
			}
			if !slices.Contains(lk.got(), kick) {
				t.Errorf("guest seat kept in the audio: %v", lk.got())
			}
		})
	}
}

// 026-AC4: a refused self-merge of a bound profile unhosts nothing; the preview says when the
// result is bound to the configured provider.
func TestMergeSelfAndResultBound(t *testing.T) {
	idp := newTestIdP(t)
	e := newOIDCEnv(t, idp, zap.NewNop(), nil)
	r, id, _, tn, _ := guestTurnRoom(t, e)
	if _, err := e.sqlDB(t).ExecContext(t.Context(), `UPDATE profiles SET oidc_issuer = $1, oidc_subject = 'self', oidc_signed_in_at = $2 WHERE id = $3`,
		idp.ts.URL, time.Now().UTC().Format(time.RFC3339Nano), id); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.do(t, "POST", "/api/admin/profiles/merge", e.admin, map[string]int64{"keep": id, "merge": id}); code != http.StatusBadRequest {
		t.Errorf("self-merge: %d", code)
	}
	if !e.running(t, r.room, tn.ID) {
		t.Error("a refused self-merge failed the turn")
	}
	if code, b := e.do(t, "POST", r.daemonPath("status"), r.alice.tok, map[string]any{"can_host": true}); code != http.StatusNoContent {
		t.Errorf("daemon after a refused self-merge: %d %s", code, b)
	}
	_, guest, _ := guestDevice(t, e, "Gwen")
	_, other, _ := guestDevice(t, e, "Hal")
	former := insertProfile(t, e, "", "former")
	off := newEnv(t)
	offBound := insertProfile(t, off, "", "x")
	offGuest := insertProfile(t, off, "", "")
	for _, tc := range []struct {
		e           *env
		keep, merge int64
		want        bool
	}{
		{e, id, guest, true}, {e, guest, id, true}, {e, guest, other, false},
		// Bound to another issuer than the configured one, or with the mode off: as 011.
		{e, former, guest, false}, {e, guest, former, false}, {off, offBound, offGuest, false},
	} {
		code, b := tc.e.do(t, "GET", "/api/admin/profiles/merge-preview?keep="+strconv.FormatInt(tc.keep, 10)+"&merge="+strconv.FormatInt(tc.merge, 10), tc.e.admin, nil)
		if code != http.StatusOK || strings.Contains(string(b), `"result_bound"`) != tc.want {
			t.Errorf("preview %d into %d: %d %s, want result_bound %v", tc.merge, tc.keep, code, b, tc.want)
		}
	}
}

// 021-AC4 as amended by 026: a profile bound by its email signs its other devices out first, as it is reached through
// the provider only now: browser sessions, seat tokens and their push subscriptions, daemons (their
// running turn failed) and the pairings its seats approved.
func TestOIDCEmailBindingSignsOutOtherDevices(t *testing.T) {
	idp := newTestIdP(t)
	e := newOIDCEnv(t, idp, zap.NewNop(), nil)
	lk := withVoice(t, e)
	r, id, other, tn, pending := guestTurnRoom(t, e)
	setEmail(t, e, id, "olga@example.com")
	if _, err := e.sqlDB(t).ExecContext(t.Context(), `INSERT INTO push_subscriptions(participant_id, endpoint, p256dh, auth, all_messages, created_at)
		VALUES($1, 'https://push.example/1', 'k', 'a', 1, '2026-01-01T00:00:00Z')`, r.alice.p.Participant.ID); err != nil {
		t.Fatal(err)
	}
	stream := e.openEvents(t, r.room, r.alice.p.Token, "")
	d := e.device(t)
	if a, _ := d.signIn(t, idp, map[string]any{"sub": "olga", "email": "olga@example.com", "email_verified": true}); a.code != http.StatusSeeOther {
		t.Fatalf("sign-in %d", a.code)
	}
	if profileID(t, d.profileJSON(t)) != id || bound(t, e, id).Subject != "olga" {
		t.Fatal("not bound by email")
	}
	if p := other.profileJSON(t); p["profile"] != nil {
		t.Errorf("other device still signed in: %v", p)
	}
	if code, _ := e.do(t, "GET", "/api/rooms/"+r.room, r.alice.p.Token, nil); code != http.StatusUnauthorized {
		t.Errorf("seat token: %d", code)
	}
	if e.running(t, r.room, tn.ID) {
		t.Errorf("turn %d still running", tn.ID)
	}
	if code, b := e.do(t, "POST", r.daemonPath("status"), r.alice.tok, map[string]any{"can_host": true}); code != http.StatusUnauthorized {
		t.Errorf("daemon: %d %s", code, b)
	}
	if code, p := e.poll(t, pending); code == http.StatusOK && p.Token != "" {
		t.Errorf("approved pairing polled: %+v", p)
	}
	if n := e.count(t, `SELECT count(*) FROM push_subscriptions`); n != 0 {
		t.Errorf("%d push subscriptions left", n)
	}
	revoked := time.After(3 * time.Second)
wait:
	for {
		select {
		case ev := <-stream:
			if ev.event == "revoked" {
				break wait
			}
		case <-revoked:
			t.Error("the seat's stream was not revoked promptly")
			break wait
		}
	}
	kick := `RemoveParticipant {"identity":"` + itoa(r.alice.p.Participant.ID) + `","room":"` + r.room + `"}`
	for deadline := time.Now().Add(5 * time.Second); !slices.Contains(lk.got(), kick) && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if !slices.Contains(lk.got(), kick) {
		t.Errorf("seat kept in the audio: %v", lk.got())
	}
	// The browser it signed in is the profile's only way in now.
	if p := d.profile(t).Profile; p.ID != id || len(p.Rooms) != 1 {
		t.Errorf("signed-in browser %+v", p)
	}
}
