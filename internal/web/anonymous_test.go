package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/babs/grill-room/internal/store"
)

type anonRoomResp struct {
	store.Room
	RecoveryURL string `json:"recovery_url"`
}

// anonSettings let anonymous rooms be created: anyone creates rooms, under a storage quota far off
// (026-AC1).
var anonSettings = store.InstanceSettings{RoomsPerProfile: 5, RoomCreation: store.CreationAnyone, StorageQuota: 1 << 40}

// newAnonEnv is an instance with anonymous rooms on, perHour of them per client IP, under
// anonSettings.
func newAnonEnv(t *testing.T, perHour int) *env {
	t.Helper()
	opts := testOptions
	opts.AnonymousRooms, opts.AnonymousRoomsPerHour = true, perHour
	e := newEnvOpts(t, opts)
	e.setSettings(t, anonSettings)
	return e
}

// anonEvents counts the anonymous route's limiter slots spent.
func (e *env) anonEvents(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.sqlDB(t).QueryRowContext(t.Context(), `SELECT count(*) FROM rate_events WHERE scope = 'anon-room'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// anonRoom posts body as JSON to the anonymous route as the device, from the client ip behind the
// proxy, with extra headers hdr.
func (d *device) anonRoom(t *testing.T, ip string, body any, hdr ...string) (int, http.Header, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(t.Context(), "POST", d.e.ts.URL+"/api/rooms/anonymous", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", ip)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := d.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, out
}

func (e *env) profileCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.sqlDB(t).QueryRowContext(t.Context(), `SELECT count(*) FROM profiles`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

var anonBody = map[string]string{"name": "Open room", "display_name": "Ann"}

// 019-AC1 019-AC10: off, the route answers as an unknown one and an emailless profile imports.
func TestAnonymousRoomsOff(t *testing.T) {
	e := newEnv(t)
	d := e.device(t)
	code, _, b := d.anonRoom(t, "203.0.113.1", anonBody)
	wantCode, wantBody := d.do(t, "POST", "/api/no-such-route", anonBody)
	if code != http.StatusNotFound || code != wantCode || string(b) != string(wantBody) {
		t.Fatalf("flag off: %d %s, want %d %s", code, b, wantCode, wantBody)
	}
	room := d.createRoom(t, "Mine", "Alice")
	if p := d.profile(t).Profile; p.Email != "" {
		t.Fatalf("profile has an email: %+v", p)
	}
	_, ar := d.do(t, "GET", "/api/rooms/"+room.ID+"/export", nil)
	if code, b := e.send(t, d, "/api/rooms/import?display_name=Alice", "", ar, "application/gzip"); code != http.StatusCreated {
		t.Fatalf("emailless import, flag off: %d %s", code, b)
	}
}

// 019-AC8: the home page learns the flag from GET /api/profile; off, the key is absent.
func TestAnonymousRoomsProfileFlag(t *testing.T) {
	for _, on := range []bool{false, true} {
		opts := testOptions
		opts.AnonymousRooms = on
		d := newEnvOpts(t, opts).device(t)
		code, b := d.do(t, "GET", "/api/profile", nil)
		if code != http.StatusOK || strings.Contains(string(b), `"anonymous_rooms"`) != on ||
			decodeJSON[profileResp](t, b).AnonymousRooms != on {
			t.Fatalf("flag %v: %d %s", on, code, b)
		}
	}
}

// 019-AC2 019-AC7: a browser without a profile gets one and the room, as its admin, with both
// cookies, the recovery link and its shareable invite; audited and logged as anonymous.
func TestAnonymousRoomCreates(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	opts := testOptions
	opts.AnonymousRooms = true
	e := newEnvLog(t, opts, zap.New(core))
	e.setSettings(t, anonSettings)
	d := e.device(t)
	code, _, b := d.anonRoom(t, "203.0.113.2", anonBody)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, b)
	}
	got := decodeJSON[anonRoomResp](t, b)
	if got.ID == "" || got.Name != "Open room" || !strings.HasPrefix(got.RecoveryURL, baseURL+"/profile#recover=") {
		t.Fatalf("answer: %s", b)
	}
	u, _ := url.Parse(e.ts.URL + "/api/rooms/" + got.ID)
	var roomTok string
	var profileSet bool
	for _, c := range d.c.Jar.Cookies(u) {
		switch c.Name {
		case cookieName:
			roomTok = c.Value
		case profileCookie:
			profileSet = true
		}
	}
	if roomTok == "" || !profileSet {
		t.Fatalf("cookies: room %q, profile %v", roomTok, profileSet)
	}
	// The room cookie alone opens the room as its admin.
	req, _ := http.NewRequestWithContext(t.Context(), "GET", u.String(), nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: roomTok})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if rr := decodeJSON[roomResp](t, rb); resp.StatusCode != http.StatusOK || rr.Me == nil || !rr.Me.Admin || rr.Me.Name != "Ann" || len(rr.Participants) != 1 {
		t.Fatalf("room cookie: %d %s", resp.StatusCode, rb)
	}
	prof := d.profile(t).Profile
	if prof == nil || len(prof.Rooms) != 1 || prof.Rooms[0].RoomID != got.ID || !prof.Rooms[0].Admin {
		t.Fatalf("profile: %+v", prof)
	}
	_, ib := d.do(t, "GET", "/api/rooms/"+got.ID+"/invites", nil)
	if invs := decodeJSON[[]store.Invite](t, ib); len(invs) != 2 || !invs[1].Shareable {
		t.Fatalf("invites: %s", ib)
	}
	uuid, err := e.st.RoomAdmin(t.Context(), got.ID, prof.ID)
	if err != nil {
		t.Fatal(err)
	}
	created := e.auditLog(t, "?action="+ActionRoomCreate+"&target="+got.ID).Entries
	if len(created) != 1 || created[0].Actor != ActorProfile+uuid || created[0].TargetName != "Open room" ||
		!strings.Contains(string(created[0].Details), `"anonymous":true`) || !strings.Contains(string(created[0].Details), `"as":"Ann"`) {
		t.Fatalf("audit: %+v", created)
	}
	lines := logs.FilterMessage("room_created").AllUntimed()
	if len(lines) != 1 || lines[0].ContextMap()["anonymous"] != true || lines[0].ContextMap()["room"] != got.ID {
		t.Fatalf("log: %+v", lines)
	}
}

// 019-AC3: a browser with a profile is sent to its profile page; nothing is made, no slot spent.
func TestAnonymousRoomWithProfile(t *testing.T) {
	e := newAnonEnv(t, 1)
	alice := e.device(t)
	alice.createRoom(t, "Mine", "Alice")
	profiles, rooms := e.profileCount(t), len(alice.profile(t).Profile.Rooms)
	code, _, b := alice.anonRoom(t, "203.0.113.3", anonBody)
	if code != http.StatusConflict || errorOf(t, b) != "You already have a profile: create the room from your profile page" {
		t.Fatalf("with a profile: %d %s", code, b)
	}
	if e.profileCount(t) != profiles || len(alice.profile(t).Profile.Rooms) != rooms {
		t.Fatal("something was created")
	}
	if code, _, b := e.device(t).anonRoom(t, "203.0.113.3", anonBody); code != http.StatusCreated {
		t.Fatalf("the 409 spent the slot: %d %s", code, b)
	}
}

// 019-AC4: per client IP, its /64 for IPv6, N rooms an hour; past it 429 with Retry-After. A 400 or
// a 403 spends nothing.
func TestAnonymousRoomRateLimit(t *testing.T) {
	e := newAnonEnv(t, 2)
	for _, c := range []struct{ name, first, second, third, other string }{
		{"ipv4", "198.51.100.7", "198.51.100.7", "198.51.100.7", "198.51.100.8"},
		{"ipv6 /64", "2001:db8::1", "2001:db8::2", "2001:db8::ffff:1", "2001:db8:0:1::1"},
	} {
		for _, ip := range []string{c.first, c.second} {
			if code, _, b := e.device(t).anonRoom(t, ip, anonBody); code != http.StatusCreated {
				t.Fatalf("%s %s: %d %s", c.name, ip, code, b)
			}
		}
		profiles := e.profileCount(t)
		code, hdr, b := e.device(t).anonRoom(t, c.third, anonBody)
		var got struct {
			Reason string `json:"reason"`
			Retry  int64  `json:"retry_after_s"`
		}
		_ = json.Unmarshal(b, &got)
		if code != http.StatusTooManyRequests || got.Reason != "rate_limited" || got.Retry < 1 || hdr.Get("Retry-After") != strconv.FormatInt(got.Retry, 10) {
			t.Fatalf("%s: past the limit: %d %v %s", c.name, code, hdr, b)
		}
		if e.profileCount(t) != profiles {
			t.Fatalf("%s: the 429 made a profile", c.name)
		}
		if code, _, b := e.device(t).anonRoom(t, c.other, anonBody); code != http.StatusCreated {
			t.Fatalf("%s: another client: %d %s", c.name, code, b)
		}
	}

	const ip = "192.0.2.9"
	for range 3 {
		if code, _, _ := e.device(t).anonRoom(t, ip, map[string]string{"name": "", "display_name": "Ann"}); code != http.StatusBadRequest {
			t.Fatalf("bad name: %d", code)
		}
	}
	e.setSettings(t, store.InstanceSettings{RoomsPerProfile: 5, RoomCreation: store.CreationSuperAdmin})
	for range 3 {
		if code, _, _ := e.device(t).anonRoom(t, ip, anonBody); code != http.StatusForbidden {
			t.Fatalf("super-admin only: %d", code)
		}
	}
	e.setSettings(t, anonSettings)
	for i := range 2 {
		if code, _, b := e.device(t).anonRoom(t, ip, anonBody); code != http.StatusCreated {
			t.Fatalf("room %d after refusals: %d %s", i, code, b)
		}
	}
	if code, _, _ := e.device(t).anonRoom(t, ip, anonBody); code != http.StatusTooManyRequests {
		t.Fatalf("third after refusals: %d", code)
	}
}

// 019-AC4: the sweep keeps an anonymous creation for its whole hour, then the window frees it.
func TestAnonymousRoomSurvivesSweep(t *testing.T) {
	e := newAnonEnv(t, 1)
	clk := e.clock()
	const ip = "203.0.113.4"
	if code, _, b := e.device(t).anonRoom(t, ip, anonBody); code != http.StatusCreated {
		t.Fatalf("first: %d %s", code, b)
	}
	clk.add(59 * time.Minute)
	e.srv.sweepShared(t.Context())
	if code, _, _ := e.device(t).anonRoom(t, ip, anonBody); code != http.StatusTooManyRequests {
		t.Fatalf("after a sweep within the hour: %d", code)
	}
	clk.add(2 * time.Minute)
	if code, _, b := e.device(t).anonRoom(t, ip, anonBody); code != http.StatusCreated {
		t.Fatalf("past the hour: %d %s", code, b)
	}
}

// 019-AC5 019-AC6: the guardrails and the validation answer as POST /api/profile/rooms does, and
// leave no profile.
func TestAnonymousRoomRefusals(t *testing.T) {
	e := newAnonEnv(t, 1000)
	alice := e.device(t)
	alice.createRoom(t, "Mine", "Alice")
	same := func(name string, body map[string]string, want int) {
		t.Helper()
		profiles := e.profileCount(t)
		code, _, b := e.device(t).anonRoom(t, "203.0.113.5", body)
		pcode, pb := alice.do(t, "POST", "/api/profile/rooms", body)
		if code != want || pcode != want || errorOf(t, b) != errorOf(t, pb) {
			t.Errorf("%s: %d %s, profile route %d %s, want %d", name, code, b, pcode, pb, want)
		}
		if n := e.profileCount(t); n != profiles {
			t.Errorf("%s: %d profiles, want %d", name, n, profiles)
		}
	}
	long := strings.Repeat("x", maxRoomName+1)
	for name, body := range map[string]map[string]string{
		"empty name":         {"name": " ", "display_name": "Ann"},
		"long name":          {"name": long, "display_name": "Ann"},
		"empty display name": {"name": "R", "display_name": ""},
		"long display name":  {"name": "R", "display_name": strings.Repeat("x", maxNameLen+1)},
	} {
		same(name, body, http.StatusBadRequest)
	}
	e.setSettings(t, store.InstanceSettings{RoomsPerProfile: 5, RoomCreation: store.CreationSuperAdmin})
	same("super-admin only", anonBody, http.StatusForbidden)
	e.setSettings(t, store.InstanceSettings{RoomsPerProfile: 5, RoomCreation: store.CreationAnyone, StorageQuota: 1})
	same("storage quota", anonBody, http.StatusForbidden)
}

// 019-AC9: with the flag on, an emailless profile cannot import, preview included, before its body
// is read; with an email it can, and the admin always can.
func TestAnonymousRoomsImportNeedsEmail(t *testing.T) {
	e := newAnonEnv(t, 5)
	d := e.device(t)
	code, _, b := d.anonRoom(t, "203.0.113.6", anonBody)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, b)
	}
	room := decodeJSON[anonRoomResp](t, b)
	_, ar := d.do(t, "GET", "/api/rooms/"+room.ID+"/export", nil)
	// Not an archive: reading it would answer 422.
	for _, path := range []string{"/api/rooms/import?display_name=Ann", "/api/rooms/import?preview=1&display_name=Ann"} {
		if code, b := e.send(t, d, path, "", []byte("not an archive"), "application/gzip"); code != http.StatusForbidden || errorOf(t, b) != "Add an email to your profile to import a room" {
			t.Fatalf("%s, no email: %d %s", path, code, b)
		}
	}
	if _, err := e.sqlDB(t).ExecContext(t.Context(), `UPDATE profiles SET email = $1 WHERE id = $2`, "ann@example.com", d.profile(t).Profile.ID); err != nil {
		t.Fatal(err)
	}
	if code, b := e.send(t, d, "/api/rooms/import?display_name=Ann", "", ar, "application/gzip"); code != http.StatusCreated {
		t.Fatalf("with an email: %d %s", code, b)
	}
	if code, b := e.send(t, nil, "/api/rooms/import", e.admin, ar, "application/gzip"); code != http.StatusCreated {
		t.Fatalf("admin: %d %s", code, b)
	}
}

// 019-AC4 019-AC5: a refusal the instance settings decide is answered before the limiter and the
// write: past the limit it is still a 403, and no profile was ever inserted, not even rolled back.
func TestAnonymousRoomRefusedBeforeTheWrite(t *testing.T) {
	e := newAnonEnv(t, 2)
	const ip = "203.0.113.20"
	d := e.device(t)
	if code, _, b := d.anonRoom(t, ip, anonBody); code != http.StatusCreated {
		t.Fatalf("first: %d %s", code, b)
	}
	first := d.profile(t).Profile.ID
	e.setSettings(t, store.InstanceSettings{RoomsPerProfile: 5, RoomCreation: store.CreationSuperAdmin})
	for i := range 5 {
		if code, _, b := e.device(t).anonRoom(t, ip, anonBody); code != http.StatusForbidden || errorOf(t, b) != "room creation refused: only the super-admin creates rooms on this instance" {
			t.Fatalf("refusal %d: %d %s", i, code, b)
		}
	}
	e.setSettings(t, anonSettings)
	d = e.device(t)
	if code, _, b := d.anonRoom(t, ip, anonBody); code != http.StatusCreated {
		t.Fatalf("after the refusals: %d %s", code, b)
	}
	// A rolled back insert still takes an id from the sequence.
	if next := d.profile(t).Profile.ID; next != first+1 {
		t.Fatalf("profile id %d after %d: a refusal inserted a profile", next, first)
	}
}

// 019-AC2 019-AC4: the error branches leave no profile, and spend no slot: a cross-site request, a
// body that is not JSON, a store failure during the creation, which gives its slot back, and the
// store failing the limiter or the profile lookup.
func TestAnonymousRoomErrors(t *testing.T) {
	e := newAnonEnv(t, 1)
	db := e.sqlDB(t)
	exec := func(q string) {
		t.Helper()
		if _, err := db.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name, ip, breaks, repairs string
		hdr                       []string
		want                      int
	}{
		{name: "cross-site (019-AC4)", ip: "198.51.100.30", hdr: []string{"Sec-Fetch-Site", "cross-site"}, want: http.StatusForbidden},
		{name: "not json (019-AC4)", ip: "198.51.100.31", hdr: []string{"Content-Type", "text/plain"}, want: http.StatusUnsupportedMediaType},
		{name: "creation fails (019-AC4)", ip: "198.51.100.32", want: http.StatusInternalServerError,
			breaks: `ALTER TABLE rooms ADD CONSTRAINT no_rooms CHECK (false) NOT VALID`, repairs: `ALTER TABLE rooms DROP CONSTRAINT no_rooms`},
		{name: "limiter fails (019-AC4)", ip: "198.51.100.33", want: http.StatusInternalServerError,
			breaks: `ALTER TABLE rate_events ADD CONSTRAINT no_events CHECK (false) NOT VALID`, repairs: `ALTER TABLE rate_events DROP CONSTRAINT no_events`},
	} {
		if c.breaks != "" {
			exec(c.breaks)
		}
		profiles := e.profileCount(t)
		if code, _, b := e.device(t).anonRoom(t, c.ip, anonBody, c.hdr...); code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, code, b, c.want)
		}
		if n := e.profileCount(t); n != profiles {
			t.Errorf("%s: %d profiles, want %d", c.name, n, profiles)
		}
		if c.repairs != "" {
			exec(c.repairs)
		}
		// At a limit of 1, the next request from the same client proves the slot is still free.
		if code, _, b := e.device(t).anonRoom(t, c.ip, anonBody); code != http.StatusCreated {
			t.Errorf("%s: the next request: %d %s", c.name, code, b)
		}
	}

	// 019-AC3: a profile cookie the store cannot look up is a 500, not a second profile.
	d := e.device(t)
	if code, _, b := d.anonRoom(t, "198.51.100.34", anonBody); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, b)
	}
	_ = e.st.Close()
	if code, _, b := d.anonRoom(t, "198.51.100.35", anonBody); code != http.StatusInternalServerError {
		t.Fatalf("profile lookup fails: %d %s", code, b)
	}
	// 019-AC5: the guardrails' read failing is a 500 too.
	if code, _, b := e.device(t).anonRoom(t, "198.51.100.36", anonBody); code != http.StatusInternalServerError {
		t.Fatalf("guardrails read fails: %d %s", code, b)
	}
}

// 019-AC4 019-AC5: the creation mode is checked before the limiter, the storage quota after it: an
// IP over its limit gets 403 while only the super-admin creates rooms, and 429 at the quota; a
// quota refusal gives its slot back.
func TestAnonymousRoomGuardrailsAndTheLimiter(t *testing.T) {
	e := newAnonEnv(t, 1)
	events := func() int { return e.anonEvents(t) }
	const over, fresh = "203.0.113.40", "203.0.113.41"
	if code, _, b := e.device(t).anonRoom(t, over, anonBody); code != http.StatusCreated {
		t.Fatalf("first: %d %s", code, b)
	}
	e.setSettings(t, store.InstanceSettings{RoomsPerProfile: 5, RoomCreation: store.CreationSuperAdmin})
	for i := range 3 {
		if code, _, b := e.device(t).anonRoom(t, over, anonBody); code != http.StatusForbidden {
			t.Fatalf("super-admin only, over the limit, %d: %d %s", i, code, b)
		}
	}
	if n := events(); n != 1 {
		t.Fatalf("%d limiter events after the mode refusals, want 1", n)
	}
	e.setSettings(t, store.InstanceSettings{RoomsPerProfile: 5, RoomCreation: store.CreationAnyone, StorageQuota: 1})
	if code, _, b := e.device(t).anonRoom(t, over, anonBody); code != http.StatusTooManyRequests {
		t.Fatalf("at the quota, over the limit: %d %s", code, b)
	}
	if code, _, b := e.device(t).anonRoom(t, fresh, anonBody); code != http.StatusForbidden || !strings.Contains(errorOf(t, b), "storage quota") {
		t.Fatalf("at the quota: %d %s", code, b)
	}
	if n := events(); n != 1 {
		t.Fatalf("%d limiter events after the quota refusal, want 1", n)
	}
	e.setSettings(t, anonSettings)
	if code, _, b := e.device(t).anonRoom(t, fresh, anonBody); code != http.StatusCreated {
		t.Fatalf("after the quota refusal: %d %s", code, b)
	}
}

// 026-AC1: with anonymous rooms on and no storage quota, the route refuses before the limiter, an
// IP over its limit included, and creates nothing; with a quota it creates.
func TestAnonymousRoomNeedsAQuota(t *testing.T) {
	e := newAnonEnv(t, 1)
	const over, fresh = "203.0.113.50", "203.0.113.51"
	if code, _, b := e.device(t).anonRoom(t, over, anonBody); code != http.StatusCreated {
		t.Fatalf("with a quota: %d %s", code, b)
	}
	noQuota := anonSettings
	noQuota.StorageQuota = 0
	e.setSettings(t, noQuota)
	profiles, events := e.profileCount(t), e.anonEvents(t)
	for _, ip := range []string{over, fresh, fresh} {
		code, _, b := e.device(t).anonRoom(t, ip, anonBody)
		if code != http.StatusForbidden || errorOf(t, b) != "Room creation without an invite needs a storage quota: set one in /admin › Server" {
			t.Fatalf("%s, no quota: %d %s", ip, code, b)
		}
	}
	if e.profileCount(t) != profiles || e.anonEvents(t) != events {
		t.Fatal("a refusal without a quota made a profile or spent a slot")
	}
	e.setSettings(t, anonSettings)
	if code, _, b := e.device(t).anonRoom(t, fresh, anonBody); code != http.StatusCreated {
		t.Fatalf("quota set again: %d %s", code, b)
	}
}
