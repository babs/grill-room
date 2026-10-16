package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// 014-AC8: one line per domain; while it is undecided a new refusal of the same domain counts on
// it, in place; once decided the next refusal posts a new line. Each room has its own lines.
func TestRecordBlocked(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seed(t, s, "a")
	seed(t, s, "b")
	first, err := s.RecordBlocked(ctx, "a", "Grill", []string{"example.com", "docs.rs"})
	if err != nil || len(first) != 2 || first[0].Body != "Grill tried to reach example.com (blocked)" || first[0].Blocked == nil ||
		*first[0].Blocked != (Blocked{Domain: "example.com", Persona: "Grill", Count: 1}) || first[0].Kind != KindSystem {
		t.Fatalf("first = %+v %v", first, err)
	}
	again, err := s.RecordBlocked(ctx, "a", "Scribe", []string{"example.com"})
	if err != nil || len(again) != 1 || again[0].ID != first[0].ID || again[0].Blocked.Count != 2 ||
		again[0].Body != "Grill tried to reach example.com (blocked, 2 times)" || again[0].ChangeSeq <= first[0].ChangeSeq {
		t.Fatalf("again = %+v %v", again, err)
	}
	if other, err := s.RecordBlocked(ctx, "b", "Grill", []string{"example.com"}); err != nil || other[0].ID == first[0].ID {
		t.Fatalf("other room = %+v %v", other, err)
	}
	alice := participantNamed(t, s, "a", "Alice")
	if _, err := s.RequestAllow(ctx, "a", first[0].ID, EgressRoom, alice); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmAllow(ctx, "a", first[0].ID, ""); err != nil {
		t.Fatal(err)
	}
	next, err := s.RecordBlocked(ctx, "a", "Grill", []string{"example.com"})
	if err != nil || next[0].ID == first[0].ID || next[0].Blocked.Count != 1 {
		t.Fatalf("after a decision = %+v %v", next, err)
	}
	// The stream sends lines with their blocked part.
	msgs, err := s.Messages(ctx, "a", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range msgs {
		if m.Blocked != nil {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("%d blocked lines in the replay", n)
	}
}

func participantNamed(t *testing.T, s *Store, room, name string) *Participant {
	t.Helper()
	ps, err := s.Participants(context.Background(), room)
	if err != nil {
		t.Fatal(err)
	}
	for i := range ps {
		if ps[i].Name == name {
			return &ps[i]
		}
	}
	t.Fatalf("no %s in %s", name, room)
	return nil
}

// lastAudit returns the newest audit entry.
func lastAudit(t *testing.T, s *Store) AuditEntry {
	t.Helper()
	es, err := s.AuditLog(context.Background(), AuditQuery{Limit: 1})
	if err != nil || len(es) != 1 {
		t.Fatalf("audit log: %v %v", es, err)
	}
	return es[0]
}

// 014-AC10 014-AC11 014-AC14: an approval waits for the AI host's app; once it confirms, the line
// becomes the approval and its audit entry, with domain, scope and persona, commits with it. A
// refusal by the app drops the approval and says why on the line; a second approval of a line
// decided or waiting is refused.
func TestAllowFlow(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seed(t, s, "a")
	alice := participantNamed(t, s, "a", "Alice")
	lines, err := s.RecordBlocked(ctx, "a", "Grill", []string{"example.com", "docs.rs"})
	if err != nil {
		t.Fatal(err)
	}
	id := lines[0].ID
	m, err := s.RequestAllow(ctx, "a", id, EgressServer, alice)
	if err != nil || m.Blocked.Pending != EgressServer || m.Blocked.Decision != "" {
		t.Fatalf("request = %+v %v", m, err)
	}
	if _, err := s.RequestAllow(ctx, "a", id, EgressRoom, alice); !errors.Is(err, ErrDecided) {
		t.Fatalf("second request: %v", err)
	}
	if p, err := s.PendingAllows(ctx, "a"); err != nil || len(p) != 1 || p[0] != (PendingAllow{ID: id, Domain: "example.com", Scope: EgressServer, By: "Alice"}) {
		t.Fatalf("pending = %+v %v", p, err)
	}
	m, err = s.ConfirmAllow(ctx, "a", id, "")
	if err != nil || m.Body != "Alice allowed example.com for every room on this AI host" || m.Blocked.Decision != EgressServer || m.Blocked.Pending != "" {
		t.Fatalf("confirm = %+v %v", m, err)
	}
	e := lastAudit(t, s)
	var d map[string]any
	_ = json.Unmarshal(e.Details, &d)
	if e.Action != ActionEgressAllow || e.TargetID != "a" || !strings.HasPrefix(e.Actor, ActorProfile) || d["domain"] != "example.com" || d["scope"] != EgressServer || d["persona"] != "Grill" {
		t.Fatalf("audit = %+v", e)
	}
	if _, err := s.ConfirmAllow(ctx, "a", id, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second confirm: %v", err)
	}
	if p, _ := s.PendingAllows(ctx, "a"); len(p) != 0 {
		t.Fatalf("pending after confirm = %+v", p)
	}
	// The app refuses: the line stays undecided, with why, and can be approved again.
	other := lines[1].ID
	if _, err := s.RequestAllow(ctx, "a", other, EgressRoom, alice); err != nil {
		t.Fatal(err)
	}
	m, err = s.ConfirmAllow(ctx, "a", other, "this room's list is full")
	if err != nil || m.Blocked.Error != "this room's list is full" || m.Blocked.Pending != "" || m.Blocked.Decision != "" || !strings.Contains(m.Body, "(blocked)") {
		t.Fatalf("refused = %+v %v", m, err)
	}
	if e := lastAudit(t, s); e.Action == ActionEgressAllow && e.TargetID == "a" && strings.Contains(string(e.Details), "docs.rs") {
		t.Fatal("a refused approval was audited")
	}
	if m, err = s.RequestAllow(ctx, "a", other, EgressRoom, alice); err != nil || m.Blocked.Error != "" {
		t.Fatalf("again = %+v %v", m, err)
	}
	if m, err = s.ConfirmAllow(ctx, "a", other, ""); err != nil || m.Body != "Alice allowed docs.rs for this room" {
		t.Fatalf("room confirm = %+v %v", m, err)
	}
	if _, err := s.BlockedLine(ctx, "a", lines[0].ID-1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a plain line as a blocked one: %v", err)
	}
}

// 014-AC12: the host switches the delegation, with a line and an audit entry; a no-op writes
// neither; a new host, elected or handed the role, starts with it off, the same host keeps it.
func TestEgressDelegation(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seed(t, s, "a")
	alice := participantNamed(t, s, "a", "Alice")
	m, err := s.SetEgressDelegation(ctx, "a", true, alice)
	if err != nil || m == nil || m.Body != "Alice let the room admins allow websites for this room." {
		t.Fatalf("on = %+v %v", m, err)
	}
	if e := lastAudit(t, s); e.Action != ActionEgressDelegate || !strings.Contains(string(e.Details), `"on":true`) {
		t.Fatalf("audit = %+v", e)
	}
	if m, err := s.SetEgressDelegation(ctx, "a", true, alice); err != nil || m != nil {
		t.Fatalf("no-op = %+v %v", m, err)
	}
	on := func() bool {
		r, err := s.Room(ctx, "a")
		if err != nil {
			t.Fatal(err)
		}
		return r.EgressAdmins
	}
	if err := s.SetHost(ctx, "a", alice.ID); err != nil || !on() {
		t.Fatalf("same host: %v %v", err, on())
	}
	bob, _, err := s.Join(ctx, "inv-a", "Bob", "tok-bob", JoinProfile{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetHost(ctx, "a", bob.ID); err != nil || on() {
		t.Fatalf("elected host: %v %v", err, on())
	}
	if _, err := s.SetEgressDelegation(ctx, "a", true, bob); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HandOver(ctx, "a", alice.ID, bob); err != nil || on() {
		t.Fatalf("handed over: %v %v", err, on())
	}
	// An approval the previous host's app had not applied is dropped, with why.
	lines, err := s.RecordBlocked(ctx, "a", "Grill", []string{"pending.example"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestAllow(ctx, "a", lines[0].ID, EgressServer, alice); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHost(ctx, "a", bob.ID); err != nil {
		t.Fatal(err)
	}
	m, err = s.Message(ctx, "a", lines[0].ID)
	if err != nil || m.Blocked.Pending != "" || m.Blocked.Error != errHostChanged || m.ChangeSeq <= lines[0].ChangeSeq {
		t.Fatalf("pending after a host change: %+v %v", m.Blocked, err)
	}
	if p, _ := s.PendingAllows(ctx, "a"); len(p) != 0 {
		t.Fatalf("still pending: %+v", p)
	}
}

// 014-AC15: a revocation from the host's app is the host's line and audit entry.
func TestEgressRevoked(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seed(t, s, "a")
	m, err := s.EgressRevoked(ctx, "a", "example.com", EgressRoom)
	if err != nil || m.Body != "Alice removed example.com from this room's websites" || m.Sender != "Alice" {
		t.Fatalf("room = %+v %v", m, err)
	}
	if e := lastAudit(t, s); e.Action != ActionEgressRevoke || !strings.Contains(string(e.Details), `"scope":"room"`) {
		t.Fatalf("audit = %+v", e)
	}
	if m, err := s.EgressRevoked(ctx, "a", "example.com", EgressServer); err != nil || m.Body != "Alice removed example.com from every room" {
		t.Fatalf("server = %+v %v", m, err)
	}
	if _, err := s.CreateRoom(ctx, "nohost", "No host"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EgressRevoked(ctx, "nohost", "example.com", EgressRoom); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no host: %v", err)
	}
}
