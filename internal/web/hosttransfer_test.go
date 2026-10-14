package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

// 009-AC33 009-AC34: a room admin who does not host moves the role to a participant whose daemon
// can host, with a chat line naming them, an audit entry and the name the previous host's page
// shows, or takes it for their own app; a plain member, a former room admin and another room's
// member are refused, a participant without a daemon gets the reason; the super-admin does the
// same from /admin; the host's own hand-over keeps its 003 line.
func TestHostTransfer(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "R").ID
	bob, carol := e.daemonFor(t, room, "Bob"), e.daemonFor(t, room, "Carol")
	aliceD := e.daemonFor(t, room, "Alice")
	alice := aliceD.p
	dave := e.join(t, e.createInvite(t, room).Token, "Dave")
	if code, b := e.do(t, "PUT", fmt.Sprintf("/api/admin/rooms/%s/admins/%d", room, alice.Participant.ID), e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("appoint alice: %d %s", code, b)
	}
	e.connect(t, room, bob, true)
	e.waitAgent(t, room, alice.Token, hostIs(bob.p.Participant.ID, true))
	e.connect(t, room, carol, true)
	bobID, carolID := bob.p.Participant.ID, carol.p.Participant.ID
	path := "/api/rooms/" + room + "/host"
	to := func(id int64) map[string]int64 { return map[string]int64{"participant_id": id} }

	if code, b := e.do(t, "PUT", path, dave.Token, to(carolID)); code != http.StatusForbidden {
		t.Fatalf("plain member: %d %s", code, b)
	}
	other := e.createRoom(t, "Other").ID
	if code, b := e.do(t, "PUT", path, e.join(t, e.createInvite(t, other).Token, "Eve").Token, to(carolID)); code != http.StatusUnauthorized {
		t.Fatalf("another room's member: %d %s", code, b)
	}
	if code, b := e.do(t, "PUT", path, alice.Token, to(dave.Participant.ID)); code != http.StatusConflict || !strings.Contains(string(b), "no connected daemon able to host") {
		t.Fatalf("no daemon: %d %s", code, b)
	}
	e.status(t, room, carol, false)
	if code, b := e.do(t, "PUT", path, alice.Token, to(carolID)); code != http.StatusConflict {
		t.Fatalf("daemon unable to host: %d %s", code, b)
	}
	e.status(t, room, carol, true)
	if code, b := e.do(t, "PUT", path, alice.Token, to(carolID)); code != http.StatusNoContent {
		t.Fatalf("room admin: %d %s", code, b)
	}
	if st := e.agent(t, room, bob.p.Token); !hostIs(carolID, true)(st) || st.HostBy != "Alice made Carol the AI host" {
		t.Fatalf("after the room admin: %+v", st)
	}
	if m := e.lastLine(t, room); m.Kind != store.KindSystem || m.Body != "Alice made Carol the AI host" || m.ParticipantID != alice.Participant.ID {
		t.Fatalf("line %+v", m)
	}

	// The super-admin, from /admin.
	if code, b := e.do(t, "PUT", "/api/admin/rooms/"+room+"/host", e.admin, to(bobID)); code != http.StatusNoContent {
		t.Fatalf("super-admin: %d %s", code, b)
	}
	if st := e.agent(t, room, bob.p.Token); !hostIs(bobID, true)(st) || st.HostBy != "The admin made Bob the AI host" {
		t.Fatalf("after the super-admin: %+v", st)
	}
	if m := e.lastLine(t, room); m.Body != "The admin made Bob the AI host" {
		t.Fatalf("line %q", m.Body)
	}
	if code, _ := e.do(t, "PUT", "/api/admin/rooms/nope/host", e.admin, to(bobID)); code != http.StatusNotFound {
		t.Fatalf("unknown room: %d", code)
	}
	if code, _ := e.do(t, "PUT", "/api/admin/rooms/"+room+"/host", alice.Token, to(carolID)); code != http.StatusUnauthorized {
		t.Fatalf("admin route as a member: %d", code)
	}

	// /admin's room view says who hosts and who can.
	_, b := e.do(t, "GET", "/api/admin/rooms/"+room+"/participants", e.admin, nil)
	got := map[string]string{}
	for _, p := range decodeJSON[[]adminParticipant](t, b) {
		got[p.Name] = fmt.Sprint(p.Host, p.CanHost)
	}
	if want := map[string]string{"Alice": "false false", "Bob": "true true", "Carol": "false true", "Dave": "false false"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("participants %v", got)
	}

	// The host's own hand-over: the 003 line, nothing to tell, no admin action.
	if code, b := e.do(t, "PUT", path, bob.p.Token, to(carolID)); code != http.StatusNoContent {
		t.Fatalf("host hands over: %d %s", code, b)
	}
	if st := e.agent(t, room, bob.p.Token); !hostIs(carolID, true)(st) || st.HostBy != "" {
		t.Fatalf("after the host: %+v", st)
	}
	if m := e.lastLine(t, room); m.Body != "Carol holds the agent(s)." {
		t.Fatalf("line %q", m.Body)
	}

	// A room admin takes the role for their own app; removed from the admins, they cannot any more.
	e.connect(t, room, aliceD, true)
	if code, b := e.do(t, "PUT", path, alice.Token, to(alice.Participant.ID)); code != http.StatusNoContent {
		t.Fatalf("room admin takes the role: %d %s", code, b)
	}
	if m := e.lastLine(t, room); m.Body != "Alice took the AI host role" {
		t.Fatalf("line %q", m.Body)
	}
	if st := e.agent(t, room, bob.p.Token); !hostIs(alice.Participant.ID, true)(st) || st.HostBy != "Alice took the AI host role" {
		t.Fatalf("after taking the role: %+v", st)
	}
	if code, b := e.do(t, "PUT", path, alice.Token, to(carolID)); code != http.StatusNoContent {
		t.Fatalf("hand back: %d %s", code, b)
	}
	if code, b := e.do(t, "DELETE", fmt.Sprintf("/api/admin/rooms/%s/admins/%d", room, alice.Participant.ID), e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("remove alice: %d %s", code, b)
	}
	if code, b := e.do(t, "PUT", path, alice.Token, to(bobID)); code != http.StatusForbidden {
		t.Fatalf("former room admin: %d %s", code, b)
	}
	aliceD.drop()

	entries := e.auditLog(t, "?action="+ActionHostTransfer).Entries
	// Newest first: Alice handing back and taking the role, as a room admin, then the two above.
	if len(entries) != 4 {
		t.Fatalf("audit %+v", entries)
	}
	super, admin := entries[2], entries[3]
	for _, c := range []struct {
		en       store.AuditEntry
		actor    string
		from, to int64
	}{{super, store.ActorSuperAdmin, carolID, bobID}, {admin, ActorProfile, bobID, carolID}} {
		d := string(c.en.Details)
		if !strings.HasPrefix(c.en.Actor, c.actor) || c.en.TargetID != room || !strings.Contains(d, fmt.Sprintf(`"from":%d`, c.from)) ||
			!strings.Contains(d, fmt.Sprintf(`"to":%d`, c.to)) {
			t.Errorf("entry %+v %s", c.en, d)
		}
	}
	if !strings.Contains(string(admin.Details), `"as":"Alice"`) || !strings.Contains(string(admin.Details), `"from_name":"Bob"`) ||
		!strings.Contains(string(admin.Details), `"to_name":"Carol"`) {
		t.Errorf("room admin entry %s", admin.Details)
	}

	// An election hands the role on when the host's daemon goes: no one to name then.
	carol.drop()
	if st := e.waitAgent(t, room, bob.p.Token, hostIs(bobID, true)); st.HostBy != "" {
		t.Fatalf("after the election: %+v", st)
	}
}

// 009-AC34: a turn running on the previous host's daemon finishes there, its result accepted
// from that daemon, while the queued turn starts on the new host's once it does.
func TestHostTransferRunningTurn(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	alice := r.alice
	carol := e.daemonFor(t, r.room, "Carol")
	e.connect(t, r.room, carol, true)
	tn := e.press(t, r, alice, alice.p.Token, false)
	code, b := e.do(t, "POST", "/api/rooms/"+r.room+"/turns", alice.p.Token, map[string]bool{})
	if code != http.StatusAccepted {
		t.Fatalf("queued press: %d %s", code, b)
	}
	if code, b := e.do(t, "PUT", "/api/admin/rooms/"+r.room+"/host", e.admin, map[string]int64{"participant_id": carol.p.Participant.ID}); code != http.StatusNoContent {
		t.Fatalf("transfer: %d %s", code, b)
	}
	if st := e.agent(t, r.room, alice.p.Token); len(st.Running) != 1 || st.Running[0].ID != tn.ID || len(st.Queue) != 1 {
		t.Fatalf("right after the transfer: running %+v queue %+v", st.Running, st.Queue)
	}
	// The new host's daemon may not answer the old one's turn.
	if code, _ := e.result(t, r, carol, tn.ID, turn.Result{SessionID: sid1}); code != http.StatusConflict {
		t.Fatalf("new host answers the old turn: %d", code)
	}
	if code, b := e.result(t, r, alice, tn.ID, turn.Result{SessionID: sid1, SessionOffset: 0, Session: []byte("one\n"), Synthesis: "Done."}); code != http.StatusOK {
		t.Fatalf("old host's result: %d %s", code, b)
	}
	if done, err := e.st.Turn(t.Context(), r.room, tn.ID); err != nil || done.Status != store.TurnDone {
		t.Fatalf("old turn %+v %v", done, err)
	}
	ev := nextEvent(t, carol.events, "turn")
	next := decodeJSON[struct{ ID int64 }](t, []byte(ev.data))
	nt, err := e.st.Turn(t.Context(), r.room, next.ID)
	if err != nil || nt.Status != store.TurnRunning || nt.DaemonID == tn.DaemonID {
		t.Fatalf("queued turn %+v %v, first ran on daemon %d", nt, err, tn.DaemonID)
	}
}
