package store

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

// 004-AC1: a session lives 12 h from its login, can be revoked, and expired ones are swept at the
// next login.
func TestAdminSessionLifecycle(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	a, err := s.CreateAdminSession(ctx, "hash-a", "ua", "203.0.113.1", t0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateAdminSession(ctx, "hash-b", "ua", "203.0.113.2", t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAdminSession(ctx, "hash-b", "ua", "ip", t0); err == nil {
		t.Fatal("a token hash used twice")
	}
	if id, err := s.TouchAdminSession(ctx, "hash-a", t0.Add(12*time.Hour-time.Second)); err != nil || id != a.ID {
		t.Fatalf("touch before expiry: %d %v", id, err)
	}
	if _, err := s.TouchAdminSession(ctx, "hash-a", t0.Add(12*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("touch at expiry: %v", err)
	}
	list, err := s.AdminSessions(ctx, t0.Add(12*time.Hour))
	if err != nil || len(list) != 1 || list[0].ID != b.ID || list[0].IP != "203.0.113.2" {
		t.Fatalf("listed at a's expiry: %+v %v", list, err)
	}
	list, _ = s.AdminSessions(ctx, t0)
	if len(list) != 2 || !list[1].LastUsedAt.Equal(t0.Add(12*time.Hour-time.Second)) {
		t.Fatalf("last use: %+v", list)
	}
	if err := s.RevokeAdminSession(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TouchAdminSession(ctx, "hash-b", t0.Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked: %v", err)
	}
	if err := s.RevokeAdminSession(ctx, b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke twice: %v", err)
	}
	if _, err := s.CreateAdminSession(ctx, "hash-c", "ua", "ip", t0.Add(13*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM admin_sessions WHERE token_hash = 'hash-a'`); n != 0 {
		t.Fatalf("expired session kept: %d", n)
	}
}

func audit(t *testing.T, s *Store, at time.Time, action, kind, id, name string) {
	t.Helper()
	if err := s.Audit(t.Context(), AuditEntry{At: at, Actor: ActorSuperAdmin, Action: action, TargetKind: kind, TargetID: id, TargetName: name}); err != nil {
		t.Fatal(err)
	}
}

// 004-AC6: entries outlive the room and the profile they name, with the names they had; the purge
// removes only what is older than its cutoff.
func TestAuditOutlivesTargetsUntilPurged(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	p := seed(t, s, "a")
	name, err := s.AuditTargetName(ctx, TargetRoom, "a")
	if err != nil || name != "Room a" {
		t.Fatalf("room name: %q %v", name, err)
	}
	pid := strconv.FormatInt(p.ProfileID, 10)
	if name, err := s.AuditTargetName(ctx, TargetProfile, pid); err != nil || name != "Alice" {
		t.Fatalf("profile name: %q %v", name, err)
	}
	audit(t, s, t0.Add(-400*24*time.Hour), "room.create", TargetRoom, "a", "Room a")
	audit(t, s, t0, "room.delete", TargetRoom, "a", "Room a")
	audit(t, s, t0, "profile.delete", TargetProfile, pid, "Alice")
	if err := s.DeleteRoom(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProfile(ctx, p.ProfileID); err != nil {
		t.Fatal(err)
	}
	got, err := s.AuditLog(ctx, AuditQuery{Limit: 10})
	if err != nil || len(got) != 3 || got[2].TargetName != "Room a" || got[0].TargetName != "Alice" || string(got[0].Details) != "{}" {
		t.Fatalf("after deletes: %+v %v", got, err)
	}
	for kind, id := range map[string]string{TargetRoom: "a", TargetProfile: pid, TargetSkill: "1", TargetTemplate: "999"} {
		if name, err := s.AuditTargetName(ctx, kind, id); err != nil || name != "" {
			t.Errorf("gone %s: %q %v", kind, name, err)
		}
	}
	if _, err := s.AuditTargetName(ctx, "planet", "x"); err == nil {
		t.Error("unknown kind accepted")
	}
	n, err := s.PurgeAudit(ctx, t0.Add(-365*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if got, _ := s.AuditLog(ctx, AuditQuery{Limit: 10}); len(got) != 2 || got[1].Action != "room.delete" {
		t.Fatalf("after purge: %+v", got)
	}
}

// 004-AC6: details are JSON, capped in size; the log filters by action and target and pages.
func TestAuditDetailsAndQuery(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	big, _ := json.Marshal(map[string]string{"x": strings.Repeat("a", MaxAuditDetails)})
	for i, d := range []json.RawMessage{nil, json.RawMessage(`{"invite":3}`), big} {
		if err := s.Audit(ctx, AuditEntry{At: t0, Action: "invite.create", TargetKind: TargetRoom, TargetID: "r" + strconv.Itoa(i), TargetName: "100%_Room", Details: d}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Audit(ctx, AuditEntry{At: t0, Action: "x", Details: json.RawMessage(`{`)}); err == nil {
		t.Fatal("invalid details accepted")
	}
	// Matched by an unescaped "0%_R", "_" or "%".
	audit(t, s, t0, "room.create", TargetRoom, "r9", "100xxRoom")
	all, _ := s.AuditLog(ctx, AuditQuery{Limit: 10})
	if len(all) != 4 || string(all[1].Details) != `{"truncated":true}` || string(all[2].Details) != `{"invite":3}` || !all[0].At.Equal(t0) {
		t.Fatalf("entries: %+v", all)
	}
	for q, want := range map[AuditQuery]int{
		{Action: "invite.create", Limit: 10}:             3,
		{Target: "r1", Limit: 10}:                        1,
		{Target: "100%_room", Limit: 10}:                 3,
		{Target: "0%_R", Limit: 10}:                      3,
		{Target: "_", Limit: 10}:                         3,
		{Target: "%", Limit: 10}:                         3,
		{Target: "xx", Action: "x", Limit: 10}:           0,
		{Before: all[1].ID, Limit: 10}:                   2,
		{Before: all[1].ID, Limit: 1}:                    1,
		{Action: "room.create", Target: "r9", Limit: 10}: 1,
	} {
		if got, err := s.AuditLog(ctx, q); err != nil || len(got) != want {
			t.Errorf("%+v: %d entries, want %d (%v)", q, len(got), want, err)
		}
	}
}

// Every admin and audit call reports a failed database instead of answering empty.
func TestAdminCallsFailWithTheStore(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	_ = s.Close()
	calls := map[string]func() error{
		"create":      func() error { _, err := s.CreateAdminSession(ctx, "h", "ua", "ip", t0); return err },
		"touch":       func() error { _, err := s.TouchAdminSession(ctx, "h", t0); return err },
		"list":        func() error { _, err := s.AdminSessions(ctx, t0); return err },
		"purge":       func() error { _, err := s.PurgeAdminSessions(ctx, t0); return err },
		"revoke":      func() error { return s.RevokeAdminSession(ctx, 1) },
		"audit":       func() error { return s.Audit(ctx, AuditEntry{At: t0, Action: "x"}) },
		"log":         func() error { _, err := s.AuditLog(ctx, AuditQuery{Limit: 1}); return err },
		"purge audit": func() error { _, err := s.PurgeAudit(ctx, t0); return err },
		"target name": func() error { _, err := s.AuditTargetName(ctx, TargetRoom, "a"); return err },
	}
	for name, call := range calls {
		if err := call(); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
