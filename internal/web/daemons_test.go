package web

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

type pairingResp struct {
	ID         string `json:"id"`
	Secret     string `json:"secret"`
	Code       string `json:"code"`
	ApproveURL string `json:"approve_url"`
}

type pollResp struct {
	Status string `json:"status"`
	Token  string `json:"token"`
}

func (e *env) requestPairing(t *testing.T, room, name string) pairingResp {
	t.Helper()
	code, b := e.do(t, "POST", "/api/rooms/"+room+"/pairings", "", map[string]string{"name": name})
	if code != http.StatusCreated {
		t.Fatalf("create pairing: %d %s", code, b)
	}
	return decodeJSON[pairingResp](t, b)
}

func (e *env) poll(t *testing.T, p pairingResp) (int, pollResp) {
	t.Helper()
	code, b := e.do(t, "GET", "/api/pairings/"+p.ID, p.Secret, nil)
	if code != http.StatusOK {
		return code, pollResp{}
	}
	return code, decodeJSON[pollResp](t, b)
}

// pairedDaemon pairs a daemon to a new participant of room and returns its token.
func (e *env) pairedDaemon(t *testing.T, room, name string) (daemonToken string, participant joined) {
	t.Helper()
	inv := e.createInvite(t, room)
	participant = e.join(t, inv.Token, name)
	p := e.requestPairing(t, room, name+"-laptop")
	if code, b := e.do(t, "POST", "/api/rooms/"+room+"/pairings/"+p.ID, participant.Token, map[string]bool{"approve": true}); code != http.StatusOK {
		t.Fatalf("approve: %d %s", code, b)
	}
	_, r := e.poll(t, p)
	if r.Status != "approved" || r.Token == "" {
		t.Fatalf("poll after approve = %+v", r)
	}
	return r.Token, participant
}

// 001-AC22: a daemon asks to pair, a participant of the room approves in their session, and the
// daemon receives its token once, without anyone seeing or typing it.
func TestPairingFlow(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "R")
	other := e.createRoom(t, "Other")
	alice := e.join(t, e.createInvite(t, room.ID).Token, "Alice")
	mallory := e.join(t, e.createInvite(t, other.ID).Token, "Mallory")

	p := e.requestPairing(t, room.ID, "alice-laptop")
	if p.ApproveURL != baseURL+"/r/"+room.ID+"?pair="+p.ID || len(p.Code) != 7 || p.Secret == "" {
		t.Fatalf("pairing = %+v", p)
	}
	if _, r := e.poll(t, p); r.Status != "pending" {
		t.Fatalf("poll = %+v", r)
	}
	if code, _ := e.poll(t, pairingResp{ID: p.ID, Secret: "wrong"}); code != http.StatusNotFound {
		t.Fatalf("wrong secret: %d", code)
	}

	path := "/api/rooms/" + room.ID + "/pairings/" + p.ID
	if code, _ := e.do(t, "GET", path, "", nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous view: %d", code)
	}
	// A participant of another room cannot see or answer it, even through their own room's path.
	if code, _ := e.do(t, "GET", path, mallory.Token, nil); code != http.StatusUnauthorized {
		t.Fatalf("other room's participant: %d", code)
	}
	if code, _ := e.do(t, "POST", "/api/rooms/"+other.ID+"/pairings/"+p.ID, mallory.Token, map[string]bool{"approve": true}); code != http.StatusNotFound {
		t.Fatalf("answer through another room: %d", code)
	}
	code, b := e.do(t, "GET", path, alice.Token, nil)
	// 003-AC17: the machine name a daemon sends never reaches the room.
	if code != http.StatusOK || strings.Contains(string(b), "alice-laptop") || !strings.Contains(string(b), p.Code) {
		t.Fatalf("view: %d %s", code, b)
	}
	// The approval page's POST is CSRF-guarded like every participant write.
	if code, _ := e.raw(t, "POST", path, alice.Token, "application/json", map[string]string{"Sec-Fetch-Site": "cross-site"}, `{"approve":true}`); code != http.StatusForbidden {
		t.Fatalf("cross-site approve: %d", code)
	}
	if code, b := e.do(t, "POST", path, alice.Token, map[string]bool{"approve": true}); code != http.StatusOK || strings.Contains(string(b), "token") {
		t.Fatalf("approve: %d %s", code, b)
	}
	if code, _ := e.do(t, "POST", path, alice.Token, map[string]bool{"approve": true}); code != http.StatusNotFound {
		t.Fatalf("second answer: %d", code)
	}
	_, r := e.poll(t, p)
	if r.Status != "approved" || r.Token == "" {
		t.Fatalf("poll = %+v", r)
	}
	if code, _ := e.poll(t, p); code != http.StatusNotFound {
		t.Fatalf("token handed twice: %d", code)
	}
	d, err := e.st.DaemonByToken(t.Context(), HashToken(r.Token))
	if err != nil || d.ParticipantID != alice.Participant.ID {
		t.Fatalf("daemon = %+v, %v", d, err)
	}
}

func TestPairingDenyExpiryAndLimits(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "R")
	alice := e.join(t, e.createInvite(t, room.ID).Token, "Alice")

	p := e.requestPairing(t, room.ID, "x")
	if code, _ := e.do(t, "POST", "/api/rooms/"+room.ID+"/pairings/"+p.ID, alice.Token, map[string]bool{"approve": false}); code != http.StatusOK {
		t.Fatalf("deny: %d", code)
	}
	if _, r := e.poll(t, p); r.Status != "denied" || r.Token != "" {
		t.Fatalf("poll = %+v", r)
	}

	p = e.requestPairing(t, room.ID, "x")
	e.srv.pairings.mu.Lock()
	e.srv.pairings.m[p.ID].expires = time.Now().Add(-time.Second)
	e.srv.pairings.mu.Unlock()
	if code, _ := e.poll(t, p); code != http.StatusNotFound {
		t.Fatalf("expired poll: %d", code)
	}
	if code, _ := e.do(t, "POST", "/api/rooms/"+room.ID+"/pairings/"+p.ID, alice.Token, map[string]bool{"approve": true}); code != http.StatusNotFound {
		t.Fatalf("expired approve: %d", code)
	}

	for _, tc := range []struct {
		name, room, body string
		want             int
	}{
		{"unknown room", "nope", `{"name":"x"}`, http.StatusNotFound},
		{"bad json", room.ID, `{"name":`, http.StatusBadRequest},
	} {
		if code, _ := e.raw(t, "POST", "/api/rooms/"+tc.room+"/pairings", "", "application/json", nil, tc.body); code != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, code, tc.want)
		}
	}
	if code, _ := e.raw(t, "POST", "/api/rooms/"+room.ID+"/pairings", "", "text/plain", nil, `{"name":"x"}`); code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain: %d", code)
	}
	for range maxPairingsPerRoom {
		e.requestPairing(t, room.ID, "spam")
	}
	if code, _ := e.do(t, "POST", "/api/rooms/"+room.ID+"/pairings", "", map[string]string{"name": "x"}); code != http.StatusTooManyRequests {
		t.Fatalf("over the room cap: %d", code)
	}
}

// 001-AC28: the daemon's heartbeat says whether it can host and why not; the room sees it while
// the daemon's stream is open.
func TestDaemonConnection(t *testing.T) {
	old := heartbeat
	heartbeat = 50 * time.Millisecond
	t.Cleanup(func() { heartbeat = old })
	e := newEnv(t)
	room := e.createRoom(t, "R")
	tok, alice := e.pairedDaemon(t, room.ID, "Alice")
	events := "/api/rooms/" + room.ID + "/daemon/events"
	status := "/api/rooms/" + room.ID + "/daemon/status"

	for _, bad := range []string{"", alice.Token, "nope"} {
		if code, _ := e.do(t, "GET", events, bad, nil); code != http.StatusUnauthorized {
			t.Fatalf("events with %q: %d", bad, code)
		}
	}
	other := e.createRoom(t, "Other")
	if code, _ := e.do(t, "GET", "/api/rooms/"+other.ID+"/daemon/events", tok, nil); code != http.StatusUnauthorized {
		t.Fatalf("events for another room: %d", code)
	}

	ch, drop := e.openStream(t, events, tok, "")
	if ev := next(t, ch); ev.event != "hello" || !strings.Contains(ev.data, `"heartbeat_seconds"`) || strings.Contains(ev.data, "Alice-laptop") {
		t.Fatalf("hello = %+v", ev)
	}
	if code, b := e.do(t, "POST", status, tok, map[string]any{"can_host": false, "reason": "Docker is not running", "version": "v1"}); code != http.StatusNoContent {
		t.Fatalf("status: %d %s", code, b)
	}
	roomDaemons := func() []presence {
		_, b := e.do(t, "GET", "/api/rooms/"+room.ID, alice.Token, nil)
		return decodeJSON[roomResp](t, b).Daemons
	}
	ds := roomDaemons()
	if len(ds) != 1 || ds[0].CanHost || ds[0].Reason != "Docker is not running" || ds[0].ParticipantID != alice.Participant.ID {
		t.Fatalf("daemons = %+v", ds)
	}
	drop()
	deadline := time.Now().Add(5 * time.Second)
	for len(roomDaemons()) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("daemon still listed after its stream closed")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Re-pairing hands out a new token and ends the old daemon's stream.
	ch, _ = e.openStream(t, events, tok, "")
	next(t, ch)
	p := e.requestPairing(t, room.ID, "new")
	e.do(t, "POST", "/api/rooms/"+room.ID+"/pairings/"+p.ID, alice.Token, map[string]bool{"approve": true})
	expectRevokedThenClosed(t, ch)
	_, r := e.poll(t, p)

	// Revoking the participant's invite ends the new one too.
	ch, _ = e.openStream(t, events, r.Token, "")
	next(t, ch)
	if ds := roomDaemons(); len(ds) != 1 {
		t.Fatalf("after re-pairing, daemons = %+v", ds)
	}
	ap, err := e.st.ParticipantByToken(t.Context(), HashToken(alice.Token))
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := e.do(t, "DELETE", "/api/rooms/"+room.ID+"/invites/"+strconv.FormatInt(ap.InviteID, 10), e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("revoke: %d", code)
	}
	expectRevokedThenClosed(t, ch)
}
