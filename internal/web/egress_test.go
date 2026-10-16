package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

// egressStatus reports d ready to host, from a daemon that takes website approvals unless old.
func (e *env) egressStatus(t *testing.T, room string, d *testDaemon, old bool) {
	t.Helper()
	if code, b := e.do(t, "POST", "/api/rooms/"+room+"/daemon/status", d.tok, map[string]any{"can_host": true, "egress": !old}); code != http.StatusNoContent {
		t.Fatalf("status: %d %s", code, b)
	}
}

// egressRoom is a room hosted by Hank, whose daemon takes approvals, with Ada a room admin, Mia a
// plain member and Oscar, a member who administers another room only.
type egressRoom struct {
	room            string
	hank            *testDaemon
	ada, mia, oscar joined
}

func newEgressRoom(t *testing.T, e *env) *egressRoom {
	t.Helper()
	r := &egressRoom{room: e.createRoom(t, "R").ID}
	r.hank = e.daemonFor(t, r.room, "Hank")
	e.connect(t, r.room, r.hank, true)
	e.egressStatus(t, r.room, r.hank, false)
	e.waitAgent(t, r.room, r.hank.p.Token, hostIs(r.hank.p.Participant.ID, true))
	inv := e.createInvite(t, r.room).Token
	r.ada, r.mia, r.oscar = e.join(t, inv, "Ada"), e.join(t, inv, "Mia"), e.join(t, inv, "Oscar")
	other := e.createRoom(t, "Other").ID
	oscarThere := e.join(t, e.createInvite(t, other).Token, "Oscar")
	for _, a := range []struct {
		room string
		id   int64
	}{{r.room, r.ada.Participant.ID}, {other, oscarThere.Participant.ID}} {
		if code, b := e.do(t, "PUT", fmt.Sprintf("/api/admin/rooms/%s/admins/%d", a.room, a.id), e.admin, nil); code != http.StatusNoContent {
			t.Fatalf("appoint: %d %s", code, b)
		}
	}
	return r
}

// blocked posts a fresh blocked line for domain, checked or not, and returns its id.
func (e *env) blocked(t *testing.T, room, domain string) int64 {
	t.Helper()
	ms, err := e.st.RecordBlocked(t.Context(), room, "Grill", []string{domain})
	if err != nil {
		t.Fatal(err)
	}
	return ms[0].ID
}

func (e *env) allow(t *testing.T, room string, id int64, tok, scope string) (int, string) {
	t.Helper()
	code, b := e.do(t, "POST", fmt.Sprintf("/api/rooms/%s/blocked/%d/allow", room, id), tok, map[string]string{"scope": scope})
	return code, string(b)
}

// 014-AC9 014-AC12 014-AC13: who may approve, over actor, scope, delegation and room; what may be
// approved; and when the AI host's app is there to take it.
func TestAllowBlockedRights(t *testing.T) {
	e := newEnv(t)
	r := newEgressRoom(t, e)
	hank := r.hank.p.Token
	delegate := func(tok string, on bool) int {
		code, _ := e.do(t, "PUT", "/api/rooms/"+r.room+"/egress-delegation", tok, map[string]bool{"on": on})
		return code
	}
	n := 0
	for _, c := range []struct {
		name      string
		tok       string
		scope     string
		delegated bool
		want      int
	}{
		{"host, room", hank, store.EgressRoom, false, http.StatusAccepted},
		{"host, everywhere", hank, store.EgressServer, false, http.StatusAccepted},
		{"undelegated admin", r.ada.Token, store.EgressRoom, false, http.StatusForbidden},
		{"delegated admin, room", r.ada.Token, store.EgressRoom, true, http.StatusAccepted},
		{"delegated admin, everywhere", r.ada.Token, store.EgressServer, true, http.StatusForbidden},
		{"admin of another room", r.oscar.Token, store.EgressRoom, true, http.StatusForbidden},
		{"member", r.mia.Token, store.EgressRoom, true, http.StatusForbidden},
		{"member, everywhere", r.mia.Token, store.EgressServer, false, http.StatusForbidden},
		{"bad scope", hank, "planet", false, http.StatusBadRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			if code := delegate(hank, c.delegated); code != http.StatusNoContent {
				t.Fatalf("delegation: %d", code)
			}
			n++
			id := e.blocked(t, r.room, "site"+strconv.Itoa(n)+".example")
			if code, b := e.allow(t, r.room, id, c.tok, c.scope); code != c.want {
				t.Fatalf("%d %s, want %d", code, b, c.want)
			}
		})
	}
	// Only the host switches the delegation; it goes when the role moves.
	if code := delegate(r.ada.Token, false); code != http.StatusForbidden {
		t.Fatalf("admin switches the delegation: %d", code)
	}
	if code, _ := e.do(t, "PUT", "/api/rooms/"+r.room+"/egress-delegation", hank, map[string]string{}); code != http.StatusBadRequest {
		t.Fatalf("delegation without on: %d", code)
	}
	if code, _ := e.do(t, "POST", fmt.Sprintf("/api/rooms/%s/daemon/blocked/x", r.room), r.hank.tok, map[string]string{}); code != http.StatusNotFound {
		t.Fatalf("confirm a bad id: %d", code)
	}
	if code, _ := e.do(t, "POST", fmt.Sprintf("/api/rooms/%s/daemon/blocked/%d", r.room, e.blocked(t, r.room, "none.example")), r.hank.tok, map[string]string{"error": "\x00"}); code != http.StatusNotFound {
		t.Fatalf("confirm with none waiting: %d", code)
	}
	if code := delegate(hank, true); code != http.StatusNoContent {
		t.Fatal(code)
	}
	if err := e.st.SetHost(t.Context(), r.room, r.ada.Participant.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetHost(t.Context(), r.room, r.hank.p.Participant.ID); err != nil {
		t.Fatal(err)
	}
	if code, b := e.allow(t, r.room, e.blocked(t, r.room, "after.example"), r.ada.Token, store.EgressRoom); code != http.StatusForbidden || !strings.Contains(b, "has not let") {
		t.Fatalf("delegation after a host change: %d %s", code, b)
	}
	// What the allowlist refuses is refused here with its message.
	for domain, want := range map[string]string{"api.anthropic.com": "Anthropic host", "10.0.0.1": "not a domain name", "*.x": "no wildcards"} {
		if code, b := e.allow(t, r.room, e.blocked(t, r.room, domain), hank, store.EgressRoom); code != http.StatusBadRequest || !strings.Contains(b, want) {
			t.Errorf("%s: %d %s", domain, code, b)
		}
	}
	// The banner lists what waits; the host, not a member, dismisses one; a refusal raises it again.
	banner := e.blocked(t, r.room, "banner.example")
	if st := e.agent(t, r.room, r.mia.Token); !slices.ContainsFunc(st.BlockedRequests, func(b store.BlockedRequest) bool { return b.MessageID == banner }) {
		t.Fatalf("banner %+v", st.BlockedRequests)
	}
	if code, _ := e.do(t, "POST", fmt.Sprintf("/api/rooms/%s/blocked/%d/dismiss", r.room, banner), r.mia.Token, nil); code != http.StatusForbidden {
		t.Fatalf("member dismisses: %d", code)
	}
	if code, b := e.do(t, "POST", fmt.Sprintf("/api/rooms/%s/blocked/%d/dismiss", r.room, banner), hank, nil); code != http.StatusOK {
		t.Fatalf("host dismisses: %d %s", code, b)
	}
	if code, _ := e.do(t, "POST", fmt.Sprintf("/api/rooms/%s/blocked/%d/dismiss", r.room, banner), hank, nil); code != http.StatusConflict {
		t.Fatalf("dismissed twice: %d", code)
	}
	if st := e.agent(t, r.room, hank); slices.ContainsFunc(st.BlockedRequests, func(b store.BlockedRequest) bool { return b.MessageID == banner }) {
		t.Fatal("a dismissed website stayed on the banner")
	}
	// A decided line, a line that is no blocked one.
	id := e.blocked(t, r.room, "twice.example")
	if code, _ := e.allow(t, r.room, id, hank, store.EgressRoom); code != http.StatusAccepted {
		t.Fatal(code)
	}
	if code, _ := e.allow(t, r.room, id, hank, store.EgressServer); code != http.StatusConflict {
		t.Fatalf("waiting line: %d", code)
	}
	plain := e.lastLine(t, r.room).ID - 1000
	if code, _ := e.allow(t, r.room, plain, hank, store.EgressRoom); code != http.StatusNotFound {
		t.Fatalf("no such line: %d", code)
	}
	// 014-AC9: an app from before 014, then none.
	e.egressStatus(t, r.room, r.hank, true)
	if st := e.agent(t, r.room, hank); st.EgressHost != msgHostAppOld {
		t.Fatalf("old app: %+v", st)
	}
	if code, b := e.allow(t, r.room, e.blocked(t, r.room, "old.example"), hank, store.EgressRoom); code != http.StatusConflict || !strings.Contains(b, msgHostAppOld) {
		t.Fatalf("old app: %d %s", code, b)
	}
	r.hank.drop()
	e.waitAgent(t, r.room, r.mia.Token, func(st agentState) bool { return st.EgressHost == msgHostAppOffline })
	if code, b := e.allow(t, r.room, e.blocked(t, r.room, "offline.example"), hank, store.EgressRoom); code != http.StatusConflict || !strings.Contains(b, msgHostAppOffline) {
		t.Fatalf("offline app: %d %s", code, b)
	}
}

// 014-AC8 014-AC10 014-AC14: a turn's refused domains become lines, the bad ones dropped; an
// approval reaches the host's daemon as an event, and its confirmation turns the line into the
// approval, audited; another daemon cannot confirm; a refusal says why on the line.
func TestAllowBlockedFlow(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	e.egressStatus(t, r.room, r.alice, false)
	tn := e.press(t, r, r.alice, r.alice.p.Token, false)
	if code, b := e.result(t, r, r.alice, tn.ID, turn.Result{Synthesis: "ok", SessionID: sid1,
		Blocked: []string{"example.com", "api.anthropic.com", "10.0.0.1", "example.com", ".dotted.example", "docs.rs"}}); code != http.StatusOK {
		t.Fatalf("result: %d %s", code, b)
	}
	msgs, err := e.st.Messages(t.Context(), r.room, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var lines []store.Message
	for _, m := range msgs {
		if m.Blocked != nil {
			lines = append(lines, m)
		}
	}
	if len(lines) != 2 || lines[0].Body != "Grill tried to reach example.com (blocked)" || lines[1].Blocked.Domain != "docs.rs" {
		t.Fatalf("blocked lines %+v", lines)
	}
	id := lines[0].ID
	if code, b := e.allow(t, r.room, id, r.alice.p.Token, store.EgressServer); code != http.StatusAccepted {
		t.Fatalf("allow: %d %s", code, b)
	}
	ev := nextEvent(t, r.alice.events, "egress_allow")
	var p store.PendingAllow
	if err := json.Unmarshal([]byte(ev.data), &p); err != nil || p != (store.PendingAllow{ID: id, Domain: "example.com", Scope: store.EgressServer, By: "Alice"}) {
		t.Fatalf("event %+v %v", ev, err)
	}
	bob := e.daemonFor(t, r.room, "Bob")
	if code, _ := e.do(t, "POST", r.daemonPath(fmt.Sprintf("blocked/%d", id)), bob.tok, map[string]string{}); code != http.StatusForbidden {
		t.Fatalf("another daemon confirms: %d", code)
	}
	if code, b := e.do(t, "POST", r.daemonPath(fmt.Sprintf("blocked/%d", id)), r.alice.tok, map[string]string{}); code != http.StatusNoContent {
		t.Fatalf("confirm: %d %s", code, b)
	}
	if m, _ := e.st.Message(t.Context(), r.room, id); m.Body != "Alice allowed example.com for every room on this AI host" || m.Blocked.Decision != store.EgressServer {
		t.Fatalf("line %+v", m)
	}
	entries := e.auditLog(t, "?action="+store.ActionEgressAllow).Entries
	if len(entries) != 1 || entries[0].TargetID != r.room || !strings.Contains(string(entries[0].Details), `"persona":"Grill"`) {
		t.Fatalf("audit %+v", entries)
	}
	if code, _ := e.do(t, "POST", r.daemonPath(fmt.Sprintf("blocked/%d", id)), r.alice.tok, map[string]string{}); code != http.StatusNotFound {
		t.Fatalf("second confirm: %d", code)
	}
	other := lines[1].ID
	if code, _ := e.allow(t, r.room, other, r.alice.p.Token, store.EgressRoom); code != http.StatusAccepted {
		t.Fatal(code)
	}
	nextEvent(t, r.alice.events, "egress_allow")
	if code, _ := e.do(t, "POST", r.daemonPath(fmt.Sprintf("blocked/%d", other)), r.alice.tok, map[string]string{"error": "this room's websites: 101 domains: at most 100"}); code != http.StatusNoContent {
		t.Fatal(code)
	}
	if m, _ := e.st.Message(t.Context(), r.room, other); m.Blocked.Error != "this room's websites: 101 domains: at most 100" || m.Blocked.Decision != "" {
		t.Fatalf("refused line %+v", m.Blocked)
	}
	// 014-AC15: the host's app tells of a removal.
	if code, _ := e.do(t, "POST", r.daemonPath("egress-revoked"), r.alice.tok, map[string]string{"domain": "example.com", "scope": "server"}); code != http.StatusNoContent {
		t.Fatal(code)
	}
	if m := e.lastLine(t, r.room); m.Body != "Alice removed example.com from every room" {
		t.Fatalf("revoked line %q", m.Body)
	}
	for _, bad := range []any{map[string]string{"domain": "10.0.0.1", "scope": "room"}, map[string]string{"domain": "example.com", "scope": "planet"}, "nope"} {
		if code, _ := e.do(t, "POST", r.daemonPath("egress-revoked"), r.alice.tok, bad); code != http.StatusBadRequest {
			t.Fatalf("revoked %v: %d", bad, code)
		}
	}
	if code, _ := e.do(t, "POST", r.daemonPath("egress-revoked"), bob.tok, map[string]string{"domain": "example.com", "scope": "room"}); code != http.StatusForbidden {
		t.Fatalf("revoked by another daemon: %d", code)
	}
	if code, _ := e.do(t, "POST", fmt.Sprintf("/api/rooms/%s/blocked/%d/allow", r.room, other), r.alice.p.Token, "nope"); code != http.StatusBadRequest {
		t.Fatalf("allow with bad JSON: %d", code)
	}
	if code, _ := e.do(t, "POST", fmt.Sprintf("/api/rooms/%s/blocked/x/allow", r.room), r.alice.p.Token, map[string]string{"scope": "room"}); code != http.StatusNotFound {
		t.Fatalf("allow a bad id: %d", code)
	}
}

// 014-AC19: members see the connectors the host's app says are on; each change the host makes is
// the host's line and audit entry; only the host's daemon tells it.
func TestConnectorEvents(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	if code, b := e.do(t, "POST", r.daemonPath("status"), r.alice.tok, map[string]any{"can_host": true, "egress": true,
		"connectors": []map[string]any{{"name": "Google Calendar", "actions": true}, {"name": "\x00"}}}); code != http.StatusNoContent {
		t.Fatalf("status: %d %s", code, b)
	}
	mia := e.join(t, e.createInvite(t, r.room).Token, "Mia")
	st := e.waitAgent(t, r.room, mia.Token, func(st agentState) bool { return len(st.Connectors) > 0 })
	if !reflect.DeepEqual(st.Connectors, []roomConnector{{Name: "Google Calendar", Actions: true}}) {
		t.Fatalf("connectors %+v", st.Connectors)
	}
	if code, b := e.do(t, "POST", r.daemonPath("connector-event"), r.alice.tok, map[string]any{"name": "Google Calendar", "kind": "enable"}); code != http.StatusNoContent {
		t.Fatalf("event: %d %s", code, b)
	}
	if m := e.lastLine(t, r.room); m.Body != "Alice turned on the connector Google Calendar for this room, read only" {
		t.Fatalf("line %q", m.Body)
	}
	if es := e.auditLog(t, "?action="+store.ActionConnectorEnable).Entries; len(es) != 1 || es[0].TargetID != r.room {
		t.Fatalf("audit %+v", es)
	}
	for _, bad := range []map[string]any{{"name": "X", "kind": "explode"}, {"name": "", "kind": "enable"}} {
		if code, _ := e.do(t, "POST", r.daemonPath("connector-event"), r.alice.tok, bad); code != http.StatusBadRequest {
			t.Errorf("%v: %d", bad, code)
		}
	}
	bob := e.daemonFor(t, r.room, "Bob")
	if code, _ := e.do(t, "POST", r.daemonPath("connector-event"), bob.tok, map[string]any{"name": "X", "kind": "enable"}); code != http.StatusForbidden {
		t.Fatalf("another daemon: %d", code)
	}
}
