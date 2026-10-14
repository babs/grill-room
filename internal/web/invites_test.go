package web

import (
	"bytes"
	"compress/gzip"
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
	code, b := alice.do(t, "DELETE", fmt.Sprintf("%s/invites/%d", path, a.ID), nil)
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
	if code, b := alice.do(t, "DELETE", fmt.Sprintf("%s/invites/%d", path, a.ID), nil); code != http.StatusOK || string(b) != "{\"lost\":[],\"revoked\":[]}\n" {
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
