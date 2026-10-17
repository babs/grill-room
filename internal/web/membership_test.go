package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/store"
)

// lastBody is the body of room's newest message.
func (e *env) lastBody(t *testing.T, room string) string {
	t.Helper()
	return e.lastLine(t, room).Body
}

// 016-AC7 016-AC12 016-AC13 016-AC14, Q2: who may remove whom, leave, block and unblock, through
// the room's routes and the super-admin's.
func TestRemoveLeaveBlockAPI(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Seats", "Alice")
	path := "/api/rooms/" + room.ID
	bob := e.device(t)
	bobP := bob.join(t, room.ID, "Bob").Participant
	carol := e.join(t, e.createInvite(t, room.ID).Token, "Carol")
	dan := e.device(t)
	danP := dan.join(t, room.ID, "Dan").Participant
	if code, b := alice.do(t, "PUT", fmt.Sprintf("%s/admins/%d", path, danP.ID), nil); code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("appoint dan: %d %s", code, b)
	}
	_, b := alice.do(t, "GET", path, nil)
	aliceID := decodeJSON[roomResp](t, b).Me.ID
	remove := func(d *device, id int64, block bool) (int, string) {
		t.Helper()
		code, b := d.do(t, "DELETE", fmt.Sprintf("%s/participants/%d", path, id), map[string]bool{"block": block})
		if code == http.StatusNoContent {
			return code, ""
		}
		return code, errorOf(t, b)
	}
	for _, c := range []struct {
		name string
		by   *device
		id   int64
		code int
		msg  string
	}{
		{"member removes member", bob, carol.Participant.ID, http.StatusForbidden, "only a room admin can do this"},
		{"admin removes admin", alice, danP.ID, http.StatusConflict, "Remove their room admin role first"},
		{"admin leaves", dan, danP.ID, http.StatusConflict, "Step down as room admin first"},
		{"last admin leaves", alice, aliceID, http.StatusConflict, "Step down as room admin first"},
		{"unknown seat", alice, 9999, http.StatusNotFound, "no such participant in this room"},
		{"admin removes member", alice, carol.Participant.ID, http.StatusNoContent, ""},
		{"member leaves", bob, bobP.ID, http.StatusNoContent, ""},
	} {
		if code, msg := remove(c.by, c.id, false); code != c.code || msg != c.msg {
			t.Fatalf("%s: %d %q, want %d %q", c.name, code, msg, c.code, c.msg)
		}
	}
	if code, _ := e.do(t, "GET", path, carol.Token, nil); code != http.StatusUnauthorized {
		t.Fatalf("carol after removal: %d", code)
	}
	if code, _ := bob.do(t, "GET", path, nil); code != http.StatusUnauthorized {
		t.Fatalf("bob after leaving: %d", code)
	}
	if got := e.lastBody(t, room.ID); got != "Bob left the room" {
		t.Fatalf("leave line %q", got)
	}

	// Block, refused rejoin, the list, unblock, rejoin.
	erin := e.device(t)
	erinP := erin.join(t, room.ID, "Erin").Participant
	if code, msg := remove(alice, erinP.ID, true); code != http.StatusNoContent {
		t.Fatalf("block erin: %d %s", code, msg)
	}
	if got := e.lastBody(t, room.ID); got != "Alice removed Erin from the room and blocked them from rejoining" {
		t.Fatalf("removal line %q", got)
	}
	code, b := erin.do(t, "POST", "/api/join", map[string]string{"invite": e.createInvite(t, room.ID).Token, "name": "Erin"})
	if code != http.StatusForbidden || errorOf(t, b) != "A room admin blocked you from rejoining this room." {
		t.Fatalf("blocked rejoin: %d %s", code, b)
	}
	if code, _ := dan.do(t, "GET", path+"/blocks", nil); code != http.StatusOK {
		t.Fatalf("dan lists blocks: %d", code)
	}
	code, b = alice.do(t, "GET", path+"/blocks", nil)
	blocks := decodeJSON[[]store.Block](t, b)
	if code != http.StatusOK || len(blocks) != 1 || blocks[0].Name != "Erin" || blocks[0].By != "Alice" {
		t.Fatalf("blocks: %d %s", code, b)
	}
	if code, _ := e.do(t, "GET", "/api/admin/rooms/"+room.ID+"/blocks", e.admin, nil); code != http.StatusOK {
		t.Fatalf("admin lists blocks: %d", code)
	}
	unblock := fmt.Sprintf("%s/blocks/%d", path, blocks[0].ProfileID)
	if code, _ := e.do(t, "DELETE", unblock, e.join(t, e.createInvite(t, room.ID).Token, "Mallory").Token, nil); code != http.StatusForbidden {
		t.Fatalf("member unblocks: %d", code)
	}
	if code, b := alice.do(t, "DELETE", unblock, nil); code != http.StatusNoContent {
		t.Fatalf("unblock: %d %s", code, b)
	}
	if got := e.lastBody(t, room.ID); got != "Alice unblocked Erin" {
		t.Fatalf("unblock line %q", got)
	}
	if again := erin.join(t, room.ID, "Erin"); again.Participant.ID == erinP.ID {
		t.Fatal("rejoined on the old seat")
	}
	log, err := e.st.AuditLog(t.Context(), store.AuditQuery{Action: ActionParticipantUnblock, Limit: 1})
	if err != nil || len(log) != 1 {
		t.Fatalf("unblock audit %+v %v", log, err)
	}

	// Q2: the super-admin's mirror.
	frank := e.join(t, e.createInvite(t, room.ID).Token, "Frank")
	if code, b := e.do(t, "DELETE", fmt.Sprintf("/api/admin/rooms/%s/participants/%d", room.ID, frank.Participant.ID), e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("admin removes frank: %d %s", code, b)
	}
	if got := e.lastBody(t, room.ID); got != "The admin removed Frank from the room" {
		t.Fatalf("admin line %q", got)
	}
	log, err = e.st.AuditLog(t.Context(), store.AuditQuery{Action: ActionParticipantRemove, Limit: 10})
	if err != nil || len(log) != 3 {
		t.Fatalf("remove audit: %d entries %v", len(log), err)
	}
}

// 016-AC17: a removed person's personal token keeps their other rooms, gets 403 here, and the room
// leaves list_rooms; rejoining gives it back. A room admin's full token removes, audited with the
// token; a read token is refused.
func TestRemovalAndAPITokens(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Here", "Alice")
	other := e.createRoom(t, "Elsewhere")
	bob := e.device(t)
	bobP := bob.join(t, room.ID, "Bob").Participant
	bob.join(t, other.ID, "Bob")
	tok := bob.newToken(t, store.ScopeFull, 0).Token
	full := alice.newToken(t, store.ScopeFull, 0).Token
	read := alice.newToken(t, store.ScopeRead, 0).Token
	target := fmt.Sprintf("/api/rooms/%s/participants/%d", room.ID, bobP.ID)
	if code, _ := e.do(t, "DELETE", target, read, nil); code != http.StatusForbidden {
		t.Fatalf("read token removes: %d", code)
	}
	if code, b := e.do(t, "DELETE", target, full, nil); code != http.StatusNoContent {
		t.Fatalf("full token removes: %d %s", code, b)
	}
	log, err := e.st.AuditLog(t.Context(), store.AuditQuery{Action: ActionParticipantRemove, Limit: 1})
	if err != nil || len(log) != 1 || !strings.Contains(string(log[0].Details), `"api_token":`) {
		t.Fatalf("audit %+v %v", log, err)
	}
	code, b := e.do(t, "GET", "/api/rooms/"+room.ID, tok, nil)
	if code != http.StatusForbidden || errorOf(t, b) != msgTokenNotHere {
		t.Fatalf("bob's token here: %d %s", code, b)
	}
	if code, _ := e.do(t, "GET", "/api/rooms/"+other.ID, tok, nil); code != http.StatusOK {
		t.Fatalf("bob's token elsewhere: %d", code)
	}
	_, b = bob.do(t, "GET", "/api/profile", nil)
	if strings.Contains(string(b), room.ID) || !strings.Contains(string(b), other.ID) {
		t.Fatalf("bob's rooms: %s", b)
	}
	bob.join(t, room.ID, "Bob")
	if code, _ := e.do(t, "GET", "/api/rooms/"+room.ID, tok, nil); code != http.StatusOK {
		t.Fatalf("bob's token after rejoining: %d", code)
	}
}

// 016-AC8 016-AC9: removing the AI host fails its running turn with "the AI host left the room",
// revokes its app's stream, drops the turns it queued, and elects the first other eligible app,
// which says so; with none, the room has no host.
func TestRemovedHostFailsOver(t *testing.T) {
	for _, standby := range []bool{true, false} {
		t.Run(fmt.Sprintf("standby=%v", standby), func(t *testing.T) {
			e := newEnv(t)
			r := newTurnRoom(t, e)
			alice := r.alice
			tn := e.press(t, r, alice, alice.p.Token, false)
			if code, b := e.do(t, "POST", "/api/rooms/"+r.room+"/turns", alice.p.Token, map[string]bool{}); code != http.StatusAccepted {
				t.Fatalf("queued press: %d %s", code, b)
			}
			var bob *testDaemon
			if standby {
				bob = e.daemonFor(t, r.room, "Bob")
				e.connect(t, r.room, bob, true)
			}
			watcher := e.join(t, e.createInvite(t, r.room).Token, "Watcher")
			chat := e.chatEvents(t, r.room, watcher.Token, "")
			if code, b := e.do(t, "DELETE", fmt.Sprintf("/api/admin/rooms/%s/participants/%d", r.room, alice.p.Participant.ID), e.admin, nil); code != http.StatusNoContent {
				t.Fatalf("remove alice: %d %s", code, b)
			}
			if ev := nextEvent(t, alice.events, "revoked"); ev.event != "revoked" {
				t.Fatalf("alice's app got %+v", ev)
			}
			got, err := e.st.Turn(t.Context(), r.room, tn.ID)
			if err != nil || got.Status != store.TurnFailed || got.Error != whyHostLeft {
				t.Fatalf("turn %+v %v", got, err)
			}
			if q, err := e.st.Queue(t.Context(), r.room); err != nil || len(q) != 0 {
				t.Fatalf("queue %+v %v", q, err)
			}
			if !standby {
				st := e.waitAgent(t, r.room, watcher.Token, func(st agentState) bool { return st.HostID == 0 })
				if st.Online {
					t.Fatalf("no host, online: %+v", st)
				}
				return
			}
			e.waitAgent(t, r.room, watcher.Token, hostIs(bob.p.Participant.ID, true))
			for m := nextMessage(t, chat); m.Body != "Bob holds the agent(s)."; m = nextMessage(t, chat) {
			}
		})
	}
}
