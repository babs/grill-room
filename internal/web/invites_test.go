package web

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/store"
)

type previewWire struct {
	Invites []int64       `json:"invites"`
	Lose    []previewSeat `json:"lose"`
	Keep    []previewSeat `json:"keep"`
}

type revokeWire struct {
	Revoked []int64    `json:"revoked"`
	Lost    []lostSeat `json:"lost"`
}

// joinVia joins room through invite token tok on device d, its profile cookie included.
func (d *device) joinVia(t *testing.T, tok, name string) joinResp {
	t.Helper()
	code, b := d.do(t, "POST", "/api/join", map[string]string{"invite": tok, "name": name})
	if code != http.StatusCreated {
		t.Fatalf("join %s: %d %s", name, code, b)
	}
	return decodeJSON[joinResp](t, b)
}

func fragment(url string) string { _, tok, _ := strings.Cut(url, "#"); return tok }

// 011-AC26: the share route answers the caller's one shareable link, made with the room for its
// creator and on first ask for anyone else, the same on every ask, a new one after its revoke; the
// super-admin has none, the list marks them, and neither the list nor the export carries the token.
func TestShareInviteAPI(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Shared", "Alice")
	path := "/api/rooms/" + room.ID + "/invites"
	share := func(code int, do func(string, string, any) (int, []byte)) shareResp {
		t.Helper()
		got, b := do("POST", path+"/share", nil)
		if got != code {
			t.Fatalf("share: %d %s, want %d", got, b, code)
		}
		return decodeJSON[shareResp](t, b)
	}
	aliceDo := func(m, p string, body any) (int, []byte) { return alice.do(t, m, p, body) }
	mine := share(http.StatusOK, aliceDo)
	if again := share(http.StatusOK, aliceDo); again.ID != mine.ID || again.URL != mine.URL || !strings.Contains(mine.URL, "/join#") {
		t.Fatalf("alice's link moved: %+v then %+v", mine, again)
	}
	bob := e.join(t, fragment(mine.URL), "Bob")
	bobDo := func(m, p string, body any) (int, []byte) { return e.do(t, m, p, bob.Token, body) }
	bobs := share(http.StatusCreated, bobDo)
	if again := share(http.StatusOK, bobDo); again.ID != bobs.ID || again.URL != bobs.URL {
		t.Fatalf("bob's link moved: %+v then %+v", bobs, again)
	}

	_, b := e.do(t, "GET", "/api/admin/rooms/"+room.ID+"/invites", e.admin, nil)
	if bytes.Contains(b, []byte(fragment(mine.URL))) || bytes.Contains(b, []byte(fragment(bobs.URL))) {
		t.Fatalf("the list carries a token: %s", b)
	}
	var by []int64
	for _, inv := range decodeJSON[[]store.Invite](t, b) {
		if inv.Shareable {
			by = append(by, inv.CreatedBy)
		}
	}
	if want := []int64{e.participantByName(t, room.ID, "Alice"), bob.Participant.ID}; !slices.Equal(by, want) {
		t.Fatalf("shareable invites by %v, want %v", by, want)
	}
	if _, b := bobDo("GET", path, nil); len(decodeJSON[[]store.Invite](t, b)) != 1 || !decodeJSON[[]store.Invite](t, b)[0].Shareable {
		t.Fatalf("bob's own list: %s", b)
	}
	if code, b := e.do(t, "POST", path+"/share", e.admin, nil); code != http.StatusUnauthorized {
		t.Fatalf("super-admin share: %d %s", code, b)
	}
	if code, _ := alice.do(t, "POST", path+"/share", nil, "Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
		t.Fatalf("cross-site share: %d", code)
	}

	if code, b := bobDo("DELETE", fmt.Sprintf("%s/%d", path, bobs.ID), nil); code != http.StatusOK {
		t.Fatalf("bob revokes his link: %d %s", code, b)
	}
	if next := share(http.StatusCreated, bobDo); next.ID == bobs.ID || next.URL == bobs.URL {
		t.Fatalf("no new link after the revoke: %+v", next)
	}

	code, b := alice.do(t, "GET", "/api/rooms/"+room.ID+"/export", nil)
	if code != http.StatusOK {
		t.Fatalf("export: %d", code)
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(fragment(mine.URL))) {
		t.Fatal("the export carries alice's shareable link")
	}
}

// participantByName returns the id of the valid participant called name in room.
func (e *env) participantByName(t *testing.T, room, name string) int64 {
	t.Helper()
	ps, err := e.st.Participants(t.Context(), room)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.Name == name {
			return p.ID
		}
	}
	t.Fatalf("no %s in %s", name, room)
	return 0
}

// 011-AC19 011-AC20: on a three-level chain (alice's link, Bea joins, Bea's invite, Cy joins, Cy's
// invite, Pat joins, Pat holding another seat outside the chain), every place that may revoke sees
// the same preview, marked for its caller; the DELETE cuts those invites, counts a joiner who came
// in between, and the people it names get a 401 on their next request while Pat does not.
func TestRevokePreviewAPI(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Chain", "Alice")
	path := "/api/rooms/" + room.ID
	_, b := alice.do(t, "POST", path+"/invites/share", nil)
	a := decodeJSON[shareResp](t, b)

	bea := e.device(t)
	beaJ := bea.joinVia(t, fragment(a.URL), "Bea")
	if code, b := alice.do(t, "PUT", fmt.Sprintf("%s/admins/%d", path, beaJ.Participant.ID), nil); code != http.StatusNoContent {
		t.Fatalf("appoint bea: %d %s", code, b)
	}
	_, b = bea.do(t, "POST", path+"/invites", nil)
	bInv := decodeJSON[inviteResp](t, b)
	cy := e.join(t, bInv.Token, "Cy")
	_, b = e.do(t, "POST", path+"/invites", cy.Token, nil)
	cInv := decodeJSON[inviteResp](t, b)
	pat := e.device(t)
	patOut := pat.join(t, room.ID, "Pat") // through the super-admin's invite, outside the chain
	patIn := pat.joinVia(t, cInv.Token, "Pat")

	preview := func(code int, do func(string, string, any) (int, []byte), id int64) previewWire {
		t.Helper()
		got, b := do("GET", fmt.Sprintf("%s/invites/%d/revoke-preview", path, id), nil)
		if got != code {
			t.Fatalf("preview %d: %d %s, want %d", id, got, b, code)
		}
		if code != http.StatusOK {
			return previewWire{}
		}
		return decodeJSON[previewWire](t, b)
	}
	seats := func(ps []previewSeat) string {
		var out []string
		for _, p := range ps {
			out = append(out, fmt.Sprintf("%d:%s:admin=%v:you=%v", p.ParticipantID, p.Name, p.Admin, p.You))
		}
		return strings.Join(out, ",")
	}
	aliceDo := func(m, p string, body any) (int, []byte) { return alice.do(t, m, p, body) }
	beaDo := func(m, p string, body any) (int, []byte) { return bea.do(t, m, p, body) }
	cyDo := func(m, p string, body any) (int, []byte) { return e.do(t, m, p, cy.Token, body) }
	adminDo := func(m, p string, body any) (int, []byte) {
		return e.do(t, m, strings.Replace(p, "/api/rooms/", "/api/admin/rooms/", 1), e.admin, body)
	}

	wantInvites := []int64{a.ID, bInv.ID, cInv.ID}
	lose := func(beaYou bool) string {
		return fmt.Sprintf("%d:Bea:admin=true:you=%v,%d:Cy:admin=false:you=false", beaJ.Participant.ID, beaYou, cy.Participant.ID)
	}
	keep := fmt.Sprintf("%d:Pat:admin=false:you=false", patIn.Participant.ID)
	for _, c := range []struct {
		who    string
		do     func(string, string, any) (int, []byte)
		beaYou bool
	}{{"alice", aliceDo, false}, {"bea", beaDo, true}, {"super-admin", adminDo, false}} {
		p := preview(http.StatusOK, c.do, a.ID)
		if !slices.Equal(p.Invites, wantInvites) || seats(p.Lose) != lose(c.beaYou) || seats(p.Keep) != keep {
			t.Errorf("%s's preview: %+v lose %s keep %s", c.who, p.Invites, seats(p.Lose), seats(p.Keep))
		}
	}
	// A plain member previews only what they may revoke, as the DELETE does.
	preview(http.StatusForbidden, cyDo, a.ID)
	preview(http.StatusNotFound, cyDo, 9999)
	preview(http.StatusNotFound, adminDo, 9999)
	if p := preview(http.StatusOK, cyDo, cInv.ID); !slices.Equal(p.Invites, []int64{cInv.ID}) || len(p.Lose) != 0 || seats(p.Keep) != keep {
		t.Errorf("cy's own invite: %+v lose %s keep %s", p.Invites, seats(p.Lose), seats(p.Keep))
	}
	_, b = alice.do(t, "POST", path+"/invites", nil)
	lone := decodeJSON[inviteResp](t, b)
	if p := preview(http.StatusOK, aliceDo, lone.ID); !slices.Equal(p.Invites, []int64{lone.ID}) || len(p.Lose) != 0 || len(p.Keep) != 0 {
		t.Errorf("nobody joined: %+v", p)
	}
	if code, _ := e.do(t, "GET", path, beaJ.Token, nil); code != http.StatusOK {
		t.Fatalf("the preview cut bea: %d", code)
	}

	eve := e.join(t, bInv.Token, "Eve")
	code, b := alice.do(t, "DELETE", fmt.Sprintf("%s/invites/%d?cut=1", path, a.ID), nil)
	if code != http.StatusOK {
		t.Fatalf("revoke: %d %s", code, b)
	}
	got := decodeJSON[revokeWire](t, b)
	wantLost := []lostSeat{{beaJ.Participant.ID, "Bea"}, {cy.Participant.ID, "Cy"}, {eve.Participant.ID, "Eve"}}
	if !slices.Equal(got.Revoked, wantInvites) || !slices.Equal(got.Lost, wantLost) {
		t.Fatalf("revoke answered %s", b)
	}
	for name, tok := range map[string]string{"bea": beaJ.Token, "cy": cy.Token, "eve": eve.Token, "pat's seat in the chain": patIn.Token} {
		if code, _ := e.do(t, "GET", path, tok, nil); code != http.StatusUnauthorized {
			t.Errorf("%s after the revoke: %d, want 401", name, code)
		}
	}
	if code, _ := bea.do(t, "GET", path, nil); code != http.StatusUnauthorized {
		t.Errorf("bea's browser after the revoke: %d, want 401", code)
	}
	if code, _ := e.do(t, "GET", path, patOut.Token, nil); code != http.StatusOK {
		t.Errorf("pat's other seat: %d", code)
	}
	if code, _ := pat.do(t, "GET", path, nil); code != http.StatusOK {
		t.Errorf("pat's browser: %d", code)
	}
	if code, _ := alice.do(t, "GET", path, nil); code != http.StatusOK {
		t.Errorf("alice: %d", code)
	}
	// Nothing left to cut: the same answer shape, empty.
	if code, b := alice.do(t, "DELETE", fmt.Sprintf("%s/invites/%d?cut=1", path, a.ID), nil); code != http.StatusOK || string(b) != "{\"lost\":[],\"revoked\":[]}\n" {
		t.Fatalf("second revoke: %d %s", code, b)
	}
}

// 011-AC26 (D3): the room answer marks every seat of the caller's profile as self, and nobody
// else's; no profile id leaves.
func TestRoomAnswerMarksOwnSeats(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Seats", "Alice")
	path := "/api/rooms/" + room.ID
	_, b := alice.do(t, "POST", path+"/invites/share", nil)
	second := alice.joinVia(t, fragment(decodeJSON[shareResp](t, b).URL), "Alice again")
	bob := e.join(t, e.createInvite(t, room.ID).Token, "Bob")
	selves := func(code int, b []byte) []string {
		t.Helper()
		if code != http.StatusOK || bytes.Contains(b, []byte("profile_id")) {
			t.Fatalf("room answer: %d %s", code, b)
		}
		var out []string
		for _, p := range decodeJSON[roomResp](t, b).Participants {
			if p.Self {
				out = append(out, p.Name)
			}
		}
		return out
	}
	for name, do := range map[string]func() (int, []byte){
		"alice's browser":     func() (int, []byte) { return alice.do(t, "GET", path, nil) },
		"alice's second seat": func() (int, []byte) { return e.do(t, "GET", path, second.Token, nil) },
	} {
		if got := selves(do()); !slices.Equal(got, []string{"Alice", "Alice again"}) {
			t.Errorf("%s: self on %v, want both of alice's seats", name, got)
		}
	}
	if got := selves(e.do(t, "GET", path, bob.Token, nil)); !slices.Equal(got, []string{"Bob"}) {
		t.Errorf("bob: self on %v, want only his", got)
	}
}

// 016-AC3 016-AC5: who may cut, over member, room admin and super-admin, with the box off and on.
// Off, the invite alone is revoked and its joiner stays; on, the joiner is removed, and the audit
// entry of an audited caller carries cut and the seats removed.
func TestRevokeCutRights(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Cuts", "Alice")
	path := "/api/rooms/" + room.ID
	bob := e.device(t)
	bob.join(t, room.ID, "Bob")
	callers := map[string]func(method, path string) (int, []byte){
		"member":      func(m, p string) (int, []byte) { return bob.do(t, m, p, nil) },
		"room admin":  func(m, p string) (int, []byte) { return alice.do(t, m, p, nil) },
		"super-admin": func(m, p string) (int, []byte) { return e.do(t, m, p, e.admin, nil) },
	}
	for _, c := range []struct {
		who     string
		cut     bool
		code    int
		stays   bool
		audited bool
	}{
		{"member", false, http.StatusOK, true, false},
		{"member", true, http.StatusForbidden, true, false},
		{"room admin", false, http.StatusOK, true, true},
		{"room admin", true, http.StatusOK, false, true},
		{"super-admin", false, http.StatusOK, true, true},
		{"super-admin", true, http.StatusOK, false, true},
	} {
		do := callers[c.who]
		code, b := do("POST", path+"/invites")
		if code != http.StatusCreated {
			t.Fatalf("%s invites: %d %s", c.who, code, b)
		}
		inv := decodeJSON[inviteResp](t, b)
		joiner := e.join(t, inv.Token, "Joiner")
		q := ""
		if c.cut {
			q = "?cut=1"
		}
		code, b = do("DELETE", fmt.Sprintf("%s/invites/%d%s", path, inv.ID, q))
		if code != c.code {
			t.Fatalf("%s cut=%v: %d %s", c.who, c.cut, code, b)
		}
		if code == http.StatusForbidden && !strings.Contains(string(b), "Only a room admin can remove people") {
			t.Fatalf("%s cut=%v: %s", c.who, c.cut, b)
		}
		want := http.StatusUnauthorized
		if c.stays {
			want = http.StatusOK
		}
		if code, _ := e.do(t, "GET", path, joiner.Token, nil); code != want {
			t.Fatalf("%s cut=%v: joiner gets %d, want %d", c.who, c.cut, code, want)
		}
		if !c.audited || code != http.StatusOK {
			continue
		}
		log, err := e.st.AuditLog(t.Context(), store.AuditQuery{Action: ActionInviteRevoke, Limit: 1})
		if err != nil || len(log) != 1 {
			t.Fatalf("audit: %+v %v", log, err)
		}
		removed := "[]"
		if c.cut {
			removed = fmt.Sprintf("[%d]", joiner.Participant.ID)
		}
		d := string(log[0].Details)
		if !strings.Contains(d, fmt.Sprintf(`"cut":%v`, c.cut)) || !strings.Contains(d, `"removed":`+removed) || !strings.Contains(d, fmt.Sprintf(`"invite":%d`, inv.ID)) {
			t.Fatalf("%s cut=%v: audit %s", c.who, c.cut, d)
		}
	}
}

// inviteNamed creates an invite of room with do and returns it with its token.
func inviteNamed(t *testing.T, do func(m, p string, body any) (int, []byte), room string, body any) inviteResp {
	t.Helper()
	code, b := do("POST", "/api/rooms/"+room+"/invites", body)
	if code != http.StatusCreated {
		t.Fatalf("create invite: %d %s", code, b)
	}
	return decodeJSON[inviteResp](t, b)
}

// 040-AC14: a name is trimmed, one line, at most 60 characters counted as runes; empty,
// spaces-only and null leave the invite unnamed. Create and rename share the rules; a shareable
// link takes its name when the ask makes it.
func TestInviteNameRules(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Names", "Alice")
	aliceDo := func(m, p string, body any) (int, []byte) { return alice.do(t, m, p, body) }
	renamed := inviteNamed(t, aliceDo, room.ID, nil)
	if renamed.Name != "" {
		t.Fatalf("an invite made without a body has a name: %+v", renamed.Invite)
	}
	rename := fmt.Sprintf("/api/rooms/%s/invites/%d", room.ID, renamed.ID)
	for _, c := range []struct {
		label string
		name  any
		want  string // "" with ok false: refused
		ok    bool
	}{
		{"trimmed", "  Design team  ", "Design team", true},
		{"60 runes", strings.Repeat("é", 60), strings.Repeat("é", 60), true},
		{"61 runes", strings.Repeat("a", 61), "", false},
		{"61 multibyte runes", strings.Repeat("é", 61), "", false},
		{"newline", "a\nb", "", false},
		{"carriage return", "a\rb", "", false},
		{"tab", "a\tb", "", false},
		{"other control", "a\x07b", "", false},
		{"bidi control", "a\u202eb", "", false},
		{"line separator", "a\u2028b", "", false},
		{"paragraph separator", "a\u2029b", "", false},
		{"empty", "", "", true},
		{"spaces only", "   ", "", true},
		{"null", nil, "", true},
	} {
		body := map[string]any{"name": c.name}
		code, b := alice.do(t, "POST", "/api/rooms/"+room.ID+"/invites", body)
		if c.ok && (code != http.StatusCreated || decodeJSON[inviteResp](t, b).Name != c.want) {
			t.Errorf("%s: create %d %s, want name %q", c.label, code, b, c.want)
		}
		if !c.ok && (code != http.StatusBadRequest || !strings.Contains(errorOf(t, b), "at most 60 characters")) {
			t.Errorf("%s: create %d %s, want 400", c.label, code, b)
		}
		if code, b := alice.do(t, "PATCH", rename, map[string]string{"name": "before"}); code != http.StatusOK {
			t.Fatalf("seed name: %d %s", code, b)
		}
		code, b = alice.do(t, "PATCH", rename, body)
		want := c.want
		if !c.ok {
			want = "before"
		}
		if inv, err := e.st.Invite(t.Context(), room.ID, renamed.ID); err != nil || inv.Name != want || c.ok != (code == http.StatusOK) {
			t.Errorf("%s: rename %d %s, stored %+v %v, want name %q", c.label, code, b, inv, err, want)
		}
	}

	for _, c := range []struct{ method, path string }{
		{"POST", "/api/rooms/" + room.ID + "/invites"},
		{"POST", "/api/rooms/" + room.ID + "/invites/share"},
		{"PATCH", rename},
	} {
		if code, b := alice.do(t, c.method, c.path, map[string]any{"name": 7}); code != http.StatusBadRequest || !strings.HasPrefix(errorOf(t, b), "bad json") {
			t.Errorf("%s %s with a number for a name: %d %s", c.method, c.path, code, b)
		}
	}
	if code, b := alice.do(t, "PATCH", "/api/rooms/"+room.ID+"/invites/x", map[string]string{"name": "n"}); code != http.StatusNotFound {
		t.Errorf("rename of invite x: %d %s", code, b)
	}
	// A rename names what it sets: no key, or another key, is refused, not a clear.
	if code, b := alice.do(t, "PATCH", rename, map[string]string{"name": "kept"}); code != http.StatusOK {
		t.Fatalf("seed name: %d %s", code, b)
	}
	for _, body := range []any{map[string]any{}, map[string]string{"title": "x"}} {
		if code, b := alice.do(t, "PATCH", rename, body); code != http.StatusBadRequest || errorOf(t, b) != "name is required" {
			t.Errorf("rename with %v: %d %s", body, code, b)
		}
	}
	if inv, err := e.st.Invite(t.Context(), room.ID, renamed.ID); err != nil || inv.Name != "kept" {
		t.Errorf("a refused rename changed the name: %+v %v", inv, err)
	}

	bob := e.join(t, e.createInvite(t, room.ID).Token, "Bob")
	share := func(code int, name string) shareResp {
		t.Helper()
		got, b := e.do(t, "POST", "/api/rooms/"+room.ID+"/invites/share", bob.Token, map[string]string{"name": name})
		if got != code {
			t.Fatalf("share: %d %s, want %d", got, b, code)
		}
		return decodeJSON[shareResp](t, b)
	}
	if first := share(http.StatusCreated, " Bob's link "); first.Name != "Bob's link" {
		t.Fatalf("shareable link name: %+v", first)
	}
	if again := share(http.StatusOK, "ignored"); again.Name != "Bob's link" {
		t.Fatalf("an ask after the first renamed the link: %+v", again)
	}
	if code, b := e.do(t, "POST", "/api/rooms/"+room.ID+"/invites/share", bob.Token, map[string]string{"name": "a\nb"}); code != http.StatusBadRequest {
		t.Fatalf("share with a two-line name: %d %s", code, b)
	}

	// These routes took no body before names: an empty chunked one, or one not sent as application/json, means none.
	carol := e.join(t, e.createInvite(t, room.ID).Token, "Carol")
	post := func(path, ctype, body string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), "POST", e.ts.URL+"/api/rooms/"+room.ID+path, io.NopCloser(strings.NewReader(body)))
		if err != nil {
			t.Fatal(err)
		}
		req.TransferEncoding = []string{"chunked"}
		req.Header.Set("Authorization", "Bearer "+carol.Token)
		req.Header.Set("Content-Type", ctype)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, b
	}
	for _, c := range []struct {
		path, ctype, body string
		code              int
	}{
		{"/invites", "application/json", "", http.StatusCreated},
		{"/invites", "text/plain", "hello", http.StatusCreated},
		{"/invites/share", "application/json", "", http.StatusCreated},
		{"/invites/share", "text/plain", "hello", http.StatusOK},
	} {
		if code, b := post(c.path, c.ctype, c.body); code != c.code || bytes.Contains(b, []byte(`"name"`)) {
			t.Errorf("POST %s, %s body %q: %d %s, want %d and no name", c.path, c.ctype, c.body, code, b, c.code)
		}
	}
}

// 040-AC14: who renames an invite: its creator, a stopped one included, a room admin, the admin on
// either route and a full token acting as a room admin; not another member, not an invite of
// another room, not without credentials, not with a read-only token.
func TestRenameInviteRights(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Rights", "Alice")
	other := alice.createRoom(t, "Other", "Alice")
	bob := e.join(t, e.createInvite(t, room.ID).Token, "Bob")
	carol := e.join(t, e.createInvite(t, room.ID).Token, "Carol")
	bobDo := func(m, p string, body any) (int, []byte) { return e.do(t, m, p, bob.Token, body) }
	mine := inviteNamed(t, bobDo, room.ID, nil)
	stopped := inviteNamed(t, bobDo, room.ID, nil)
	if code, b := bobDo("DELETE", fmt.Sprintf("/api/rooms/%s/invites/%d", room.ID, stopped.ID), nil); code != http.StatusOK {
		t.Fatalf("stop: %d %s", code, b)
	}
	elsewhere := e.createInvite(t, other.ID)
	read, full := alice.newToken(t, "read", 0).Token, alice.newToken(t, "full", 0).Token
	at := func(prefix, room string, id int64) string {
		return fmt.Sprintf("%s/rooms/%s/invites/%d", prefix, room, id)
	}
	asToken := func(tok string) func(string) (int, []byte) {
		return func(p string) (int, []byte) { return e.do(t, "PATCH", p, tok, map[string]string{"name": "n"}) }
	}
	for _, c := range []struct {
		label string
		do    func(string) (int, []byte)
		path  string
		code  int
	}{
		{"creator", asToken(bob.Token), at("/api", room.ID, mine.ID), http.StatusOK},
		{"creator, stopped invite", asToken(bob.Token), at("/api", room.ID, stopped.ID), http.StatusOK},
		{"room admin", func(p string) (int, []byte) { return alice.do(t, "PATCH", p, map[string]string{"name": "n"}) }, at("/api", room.ID, mine.ID), http.StatusOK},
		{"admin, admin route", asToken(e.admin), at("/api/admin", room.ID, mine.ID), http.StatusOK},
		{"admin, room route", asToken(e.admin), at("/api", room.ID, stopped.ID), http.StatusOK},
		{"full token of a room admin", asToken(full), at("/api", room.ID, mine.ID), http.StatusOK},
		{"unrelated member", asToken(carol.Token), at("/api", room.ID, mine.ID), http.StatusForbidden},
		{"room admin, invite of another room", func(p string) (int, []byte) { return alice.do(t, "PATCH", p, map[string]string{"name": "n"}) }, at("/api", room.ID, elsewhere.ID), http.StatusNotFound},
		{"member, invite of another room", asToken(bob.Token), at("/api", room.ID, elsewhere.ID), http.StatusNotFound},
		{"admin, invite of another room", asToken(e.admin), at("/api/admin", room.ID, elsewhere.ID), http.StatusNotFound},
		{"no credentials", asToken(""), at("/api", room.ID, mine.ID), http.StatusUnauthorized},
		{"read-only token", asToken(read), at("/api", room.ID, mine.ID), http.StatusForbidden},
		{"member token on the admin route", asToken(bob.Token), at("/api/admin", room.ID, mine.ID), http.StatusUnauthorized},
	} {
		if code, b := c.do(c.path); code != c.code {
			t.Errorf("%s: %d %s, want %d", c.label, code, b, c.code)
		}
	}
	// A creator who left has no seat to rename with; their invite stopped, a room admin still names it.
	dave := e.join(t, e.createInvite(t, room.ID).Token, "Dave")
	davesInv := inviteNamed(t, func(m, p string, body any) (int, []byte) { return e.do(t, m, p, dave.Token, body) }, room.ID, nil)
	if code, b := e.do(t, "DELETE", fmt.Sprintf("/api/rooms/%s/participants/%d", room.ID, dave.Participant.ID), dave.Token, nil); code != http.StatusNoContent {
		t.Fatalf("dave leaves: %d %s", code, b)
	}
	if code, b := asToken(dave.Token)(at("/api", room.ID, davesInv.ID)); code != http.StatusUnauthorized {
		t.Errorf("creator who left: %d %s, want 401", code, b)
	}
	if code, b := alice.do(t, "PATCH", at("/api", room.ID, davesInv.ID), map[string]string{"name": "Dave's"}); code != http.StatusOK {
		t.Errorf("room admin names the stopped invite of one who left: %d %s", code, b)
	}
	if inv, err := e.st.Invite(t.Context(), room.ID, davesInv.ID); err != nil || inv.RevokedAt == nil || inv.Name != "Dave's" {
		t.Errorf("dave's invite: %+v %v", inv, err)
	}
	if inv, err := e.st.Invite(t.Context(), other.ID, elsewhere.ID); err != nil || inv.Name != "" {
		t.Fatalf("the other room's invite: %+v %v", inv, err)
	}
	if code, b := e.do(t, "PATCH", at("/api", room.ID, mine.ID), carol.Token, map[string]string{"name": "n"}); code != http.StatusForbidden || errorOf(t, b) != "you can rename only the invites you created" {
		t.Fatalf("unrelated member's refusal: %d %s", code, b)
	}
}

// 040-AC15: invite.create carries the name; a room admin's and the admin's renames are
// invite.rename with the invite and its new name; a member's own creation and rename are no admin
// action. No entry holds an invite token.
func TestInviteNameAudit(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Audit", "Alice")
	aliceDo := func(m, p string, body any) (int, []byte) { return alice.do(t, m, p, body) }
	sales := inviteNamed(t, aliceDo, room.ID, map[string]string{"name": "Sales"})
	bob := e.join(t, sales.Token, "Bob")
	bobDo := func(m, p string, body any) (int, []byte) { return e.do(t, m, p, bob.Token, body) }
	own := inviteNamed(t, bobDo, room.ID, map[string]string{"name": "Bob's"})
	rename := func(do func(m, p string, body any) (int, []byte), prefix string, id int64, name string) {
		t.Helper()
		if code, b := do("PATCH", fmt.Sprintf("%s/rooms/%s/invites/%d", prefix, room.ID, id), map[string]string{"name": name}); code != http.StatusOK {
			t.Fatalf("rename: %d %s", code, b)
		}
	}
	rename(bobDo, "/api", own.ID, "Bob's friends")
	// A plain member's rename made with an API token is audited, naming the token (012-AC20).
	carol := e.device(t)
	carol.joinVia(t, sales.Token, "Carol")
	carols := inviteNamed(t, func(m, p string, body any) (int, []byte) { return carol.do(t, m, p, body) }, room.ID, nil)
	carolTok := carol.newToken(t, "full", 0)
	rename(func(m, p string, body any) (int, []byte) { return e.do(t, m, p, carolTok.Token, body) }, "/api", carols.ID, "Carol's")
	rename(aliceDo, "/api", sales.ID, "Sales EU")
	rename(func(m, p string, body any) (int, []byte) { return e.do(t, m, p, e.admin, body) }, "/api/admin", sales.ID, "")

	invID := fmt.Sprintf(`"invite":%d`, sales.ID)
	created := slices.DeleteFunc(e.auditLog(t, "?action="+ActionInviteCreate).Entries, func(a store.AuditEntry) bool { return a.TargetID != room.ID })
	if len(created) != 1 || !strings.HasPrefix(created[0].Actor, ActorProfile) || !strings.Contains(string(created[0].Details), invID) ||
		!strings.Contains(string(created[0].Details), `"name":"Sales"`) {
		t.Fatalf("invite.create entries: %+v", created)
	}
	renames := e.auditLog(t, "?action="+ActionInviteRename).Entries
	if len(renames) != 3 {
		t.Fatalf("invite.rename entries: %+v", renames)
	}
	// Newest first: the admin's clear, Alice's rename, Carol's through her token; each names the
	// name it replaced.
	for i, w := range []struct {
		actor            string
		invite           int64
		from, to, detail string
	}{
		{store.ActorSuperAdmin, sales.ID, "Sales EU", "", `"via":"token"`},
		{ActorProfile, sales.ID, "Sales", "Sales EU", `"as":"Alice"`},
		{ActorProfile, carols.ID, "", "Carol's", fmt.Sprintf(`"api_token":%d`, carolTok.ID)},
	} {
		g := renames[i]
		var got map[string]any
		if err := json.Unmarshal(g.Details, &got); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(g.Actor, w.actor) || g.TargetID != room.ID || got["invite"] != float64(w.invite) || got["from"] != w.from || got["to"] != w.to ||
			!strings.Contains(string(g.Details), w.detail) {
			t.Errorf("invite.rename %d: %+v %s, want %+v", i, g, g.Details, w)
		}
	}
	_, raw := e.do(t, "GET", "/api/admin/audit", e.admin, nil)
	for _, tok := range []string{sales.Token, own.Token, carols.Token, HashToken(sales.Token), carolTok.Token} {
		if bytes.Contains(raw, []byte(tok)) {
			t.Fatalf("the audit log holds an invite token: %s", raw)
		}
	}
}

// 040-AC10 040-AC17: the room overview gives room admins the invite each seat joined through, one
// entry per seat, two seats of one profile each with its own; /admin's participants carry it too.
// A plain member gets 403, and neither the room answer nor the join answer names an invite.
func TestSeatInvites(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Seats", "Alice")
	aliceDo := func(m, p string, body any) (int, []byte) { return alice.do(t, m, p, body) }
	partners := inviteNamed(t, aliceDo, room.ID, map[string]string{"name": "Partners"})
	plain := inviteNamed(t, aliceDo, room.ID, nil)
	bob := e.device(t)
	code, b := bob.do(t, "POST", "/api/join", map[string]string{"invite": partners.Token, "name": "Bob"})
	if code != http.StatusCreated || bytes.Contains(b, []byte("Partners")) || bytes.Contains(b, []byte("invite_id")) {
		t.Fatalf("join: %d %s", code, b)
	}
	first := decodeJSON[joinResp](t, b)
	second := bob.joinVia(t, plain.Token, "Bob again")

	code, b = alice.do(t, "GET", "/api/rooms/"+room.ID+"/overview", nil)
	if code != http.StatusOK {
		t.Fatalf("overview: %d %s", code, b)
	}
	got := map[int64]store.SeatInvite{}
	for _, si := range decodeJSON[roomOverview](t, b).Seats {
		got[si.ParticipantID] = si
	}
	if w := (store.SeatInvite{ParticipantID: first.Participant.ID, InviteID: partners.ID, InviteName: "Partners"}); got[first.Participant.ID] != w {
		t.Errorf("bob's first seat: %+v, want %+v", got[first.Participant.ID], w)
	}
	if w := (store.SeatInvite{ParticipantID: second.Participant.ID, InviteID: plain.ID}); got[second.Participant.ID] != w {
		t.Errorf("bob's second seat: %+v, want %+v", got[second.Participant.ID], w)
	}
	if len(got) != 3 {
		t.Errorf("seats %+v, want alice's and bob's two", got)
	}
	if code, b := e.do(t, "GET", "/api/rooms/"+room.ID+"/overview", first.Token, nil); code != http.StatusForbidden {
		t.Fatalf("plain member's overview: %d %s", code, b)
	}
	for _, do := range []func() (int, []byte){
		func() (int, []byte) { return alice.do(t, "GET", "/api/rooms/"+room.ID, nil) },
		func() (int, []byte) { return e.do(t, "GET", "/api/rooms/"+room.ID, second.Token, nil) },
	} {
		if code, b := do(); code != http.StatusOK || bytes.Contains(b, []byte("Partners")) || bytes.Contains(b, []byte("invite_id")) || bytes.Contains(b, []byte("invite_name")) {
			t.Fatalf("room answer names an invite: %d %s", code, b)
		}
	}
	code, b = e.do(t, "GET", "/api/admin/rooms/"+room.ID+"/participants", e.admin, nil)
	if code != http.StatusOK {
		t.Fatalf("admin participants: %d %s", code, b)
	}
	for _, p := range decodeJSON[[]adminParticipant](t, b) {
		if s := got[p.ID]; p.InviteID != s.InviteID || p.InviteName != s.InviteName {
			t.Errorf("admin participant %+v, want invite %+v", p, s)
		}
	}
}

// The invite routes, the overview and /admin's participants answer 500 when the invites table
// fails past the seat check (040-AC10, 040-AC14, 040-AC15).
func TestInviteRoutesFailWithTheStore(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Down", "Alice")
	bob := e.join(t, e.createInvite(t, room.ID).Token, "Bob")
	bobDo := func(m, p string, body any) (int, []byte) { return e.do(t, m, p, bob.Token, body) }
	own := inviteNamed(t, bobDo, room.ID, nil)
	path := "/api/rooms/" + room.ID
	rename := fmt.Sprintf("%s/invites/%d", path, own.ID)
	db := e.sqlDB(t)
	if _, err := db.ExecContext(t.Context(), `ALTER TABLE invites RENAME TO gone`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		label        string
		do           func(m, p string, body any) (int, []byte)
		method, path string
		body         any
	}{
		{"alice", func(m, p string, body any) (int, []byte) { return alice.do(t, m, p, body) }, "GET", path + "/invites", nil},
		{"alice", func(m, p string, body any) (int, []byte) { return alice.do(t, m, p, body) }, "POST", path + "/invites", map[string]string{"name": "n"}},
		{"alice", func(m, p string, body any) (int, []byte) { return alice.do(t, m, p, body) }, "POST", path + "/invites/share", nil},
		{"alice", func(m, p string, body any) (int, []byte) { return alice.do(t, m, p, body) }, "PATCH", rename, map[string]string{"name": "n"}},
		{"alice", func(m, p string, body any) (int, []byte) { return alice.do(t, m, p, body) }, "GET", path + "/overview", nil},
		{"bob", bobDo, "PATCH", rename, map[string]string{"name": "n"}},
		{"admin", func(m, p string, body any) (int, []byte) { return e.do(t, m, p, e.admin, body) }, "GET", "/api/admin/rooms/" + room.ID + "/participants", nil},
	} {
		if code, b := c.do(c.method, c.path, c.body); code != http.StatusInternalServerError {
			t.Errorf("invites gone, %s %s %s: %d %s", c.label, c.method, c.path, code, b)
		}
	}
	if _, err := db.ExecContext(t.Context(), `ALTER TABLE gone RENAME TO invites`); err != nil {
		t.Fatal(err)
	}
	// The room admin check fails before any invite is read.
	if _, err := db.ExecContext(t.Context(), `ALTER TABLE room_admins RENAME TO gone`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", path + "/invites", nil},
		{"POST", path + "/invites", nil},
		{"POST", path + "/invites/share", nil},
		{"PATCH", rename, map[string]string{"name": "n"}},
		{"GET", path + "/overview", nil},
	} {
		if code, b := bobDo(c.method, c.path, c.body); code != http.StatusInternalServerError {
			t.Errorf("room_admins gone, bob %s %s: %d %s", c.method, c.path, code, b)
		}
	}
	if _, err := db.ExecContext(t.Context(), `ALTER TABLE gone RENAME TO room_admins`); err != nil {
		t.Fatal(err)
	}
	if code, b := bobDo("PATCH", rename, nil); code != http.StatusUnsupportedMediaType {
		t.Errorf("rename without a JSON body: %d %s", code, b)
	}
}
