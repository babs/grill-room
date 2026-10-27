package store

import (
	"context"
	"database/sql"
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
	a, err := s.CreateAdminSession(ctx, "hash-a", "ua", "203.0.113.1", t0, t0.Add(AdminSessionTTL), 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateAdminSession(ctx, "hash-b", "ua", "203.0.113.2", t0.Add(time.Hour), t0.Add(time.Hour).Add(AdminSessionTTL), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAdminSession(ctx, "hash-b", "ua", "ip", t0, t0.Add(AdminSessionTTL), 0); err == nil {
		t.Fatal("a token hash used twice")
	}
	if id, err := s.TouchAdminSession(ctx, "hash-a", t0.Add(12*time.Hour-time.Second), AdminGrant{}); err != nil || id != a.ID {
		t.Fatalf("touch before expiry: %d %v", id, err)
	}
	if _, err := s.TouchAdminSession(ctx, "hash-a", t0.Add(12*time.Hour), AdminGrant{}); !errors.Is(err, ErrNotFound) {
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
	if _, err := s.TouchAdminSession(ctx, "hash-b", t0.Add(time.Hour), AdminGrant{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked: %v", err)
	}
	if err := s.RevokeAdminSession(ctx, b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke twice: %v", err)
	}
	if _, err := s.CreateAdminSession(ctx, "hash-c", "ua", "ip", t0.Add(13*time.Hour), t0.Add(13*time.Hour).Add(AdminSessionTTL), 0); err != nil {
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
	pid := strconv.FormatInt(p.ProfileID, 10)
	audit(t, s, t0.Add(-400*24*time.Hour), "room.create", TargetRoom, "a", "Room a")
	// The entries read the names the targets have in the delete's own transaction.
	entry := func(action, kind, id string) context.Context {
		return WithAudit(ctx, AuditEntry{At: t0, Actor: ActorSuperAdmin, Action: action, TargetKind: kind, TargetID: id}, nil)
	}
	if err := s.DeleteProfile(entry("profile.delete", TargetProfile, pid), p.ProfileID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRoom(entry("room.delete", TargetRoom, "a"), "a"); err != nil {
		t.Fatal(err)
	}
	got, err := s.AuditLog(ctx, AuditQuery{Limit: 10})
	if err != nil || len(got) != 3 || got[0].TargetName != "Room a" || got[1].TargetName != "Alice" || string(got[0].Details) != "{}" {
		t.Fatalf("after deletes: %+v %v", got, err)
	}
	// A target gone already: the delete fails, and no entry is written.
	if err := s.DeleteRoom(entry("room.delete", TargetRoom, "a"), "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if again, _ := s.AuditLog(ctx, AuditQuery{Limit: 10}); len(again) != 3 {
		t.Fatalf("a failed delete wrote an entry: %+v", again)
	}
	n, err := s.PurgeAudit(ctx, t0.Add(-365*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if got, _ := s.AuditLog(ctx, AuditQuery{Limit: 10}); len(got) != 2 || got[0].Action != "room.delete" {
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
		"create": func() error {
			_, err := s.CreateAdminSession(ctx, "h", "ua", "ip", t0, t0.Add(AdminSessionTTL), 0)
			return err
		},
		"touch":       func() error { _, err := s.TouchAdminSession(ctx, "h", t0, AdminGrant{}); return err },
		"list":        func() error { _, err := s.AdminSessions(ctx, t0); return err },
		"purge":       func() error { _, err := s.PurgeAdminSessions(ctx, t0); return err },
		"revoke":      func() error { return s.RevokeAdminSession(ctx, 1) },
		"audit":       func() error { return s.Audit(ctx, AuditEntry{At: t0, Action: "x"}) },
		"log":         func() error { _, err := s.AuditLog(ctx, AuditQuery{Limit: 1}); return err },
		"purge audit": func() error { _, err := s.PurgeAudit(ctx, t0); return err },
	}
	for name, call := range calls {
		if err := call(); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// 004-AC6: an audited change and its entry share one transaction. The entry goes first to fail:
// a failed audit write rolls the change back (the reverse, a failed change, writes no entry, as
// TestAuditOutlivesTargetsUntilPurged shows).
func TestAuditWriteFailureRollsBackTheChange(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	p := seed(t, s, "a")
	tpl, err := s.PublishTemplate(ctx, p.ProfileID, "Alice", "Critic", PersonaFields{Name: "Critic", Badge: "🧐", Colour: "#112233", Text: "v1", Model: "sonnet", Effort: "medium"})
	if err != nil {
		t.Fatal(err)
	}
	k := publish(t, s, p.ProfileID, true, "stamp", "v1")
	sess, err := s.CreateAdminSession(ctx, "h1", "ua", "ip", t0, t0.Add(AdminSessionTTL), 0)
	if err != nil {
		t.Fatal(err)
	}
	invs, err := s.Invites(ctx, "a", 0)
	if err != nil || len(invs) != 1 {
		t.Fatalf("invites %+v %v", invs, err)
	}
	failInserts(t, s, "audit_log", "audit down")
	audited := func(kind, id string) context.Context {
		return WithAudit(ctx, AuditEntry{At: t0, Actor: ActorSuperAdmin, Action: "x", TargetKind: kind, TargetID: id}, nil)
	}
	one := func(q string, args ...any) string {
		var v sql.NullString
		if err := s.db.QueryRowContext(ctx, q, args...).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v.String
	}
	for _, c := range []struct {
		name   string
		change func() error
		state  string
		args   []any
		want   string
	}{
		{"create room", func() error { _, err := s.CreateRoom(audited(TargetRoom, "n"), "n", "N"); return err }, `SELECT count(*) FROM rooms WHERE id = 'n'`, nil, "0"},
		{"delete room", func() error { return s.DeleteRoom(audited(TargetRoom, "a"), "a") }, `SELECT count(*) FROM rooms WHERE id = 'a'`, nil, "1"},
		{"rename room", func() error { _, err := s.RenameRoom(audited(TargetRoom, "a"), "a", "Z", nil); return err }, `SELECT name FROM rooms WHERE id = 'a'`, nil, "Room a"},
		{"room quota", func() error { return s.SetRoomQuota(audited(TargetRoom, "a"), "a", 5) }, `SELECT quota_bytes FROM rooms WHERE id = 'a'`, nil, ""},
		{"create invite", func() error { _, err := s.CreateInvite(audited(TargetRoom, "a"), "a", "ix", 0); return err }, `SELECT count(*) FROM invites WHERE token_hash = 'ix'`, nil, "0"},
		{"revoke invite", func() error {
			_, err := s.RevokeInvite(audited(TargetRoom, "a"), "a", invs[0].ID, true, "Admin")
			return err
		}, `SELECT revoked_at FROM invites WHERE id = ?`, []any{invs[0].ID}, ""},
		{"delete profile", func() error { return s.DeleteProfile(audited(TargetProfile, "p"), p.ProfileID) }, `SELECT count(*) FROM profiles WHERE id = ?`, []any{p.ProfileID}, "1"},
		{"remove template", func() error { return s.RemoveTemplate(audited(TargetTemplate, "t"), tpl.ID) }, `SELECT count(*) FROM persona_templates WHERE id = ?`, []any{tpl.ID}, "1"},
		{"remove skill", func() error { return s.RemoveSkill(audited(TargetSkill, "k"), k.ID, 0) }, `SELECT count(*) FROM skills WHERE id = ?`, []any{k.ID}, "1"},
		{"admin login", func() error {
			_, err := s.CreateAdminSession(audited(TargetAdminSession, ""), "h2", "ua", "ip", t0, t0.Add(AdminSessionTTL), 0)
			return err
		}, `SELECT count(*) FROM admin_sessions`, nil, "1"},
		{"admin revoke", func() error { return s.RevokeAdminSession(audited(TargetAdminSession, "s"), sess.ID) }, `SELECT count(*) FROM admin_sessions`, nil, "1"},
	} {
		if err := c.change(); err == nil || !strings.Contains(err.Error(), "audit down") {
			t.Errorf("%s: %v, want the audit failure", c.name, err)
		}
		if got := one(c.state, c.args...); got != c.want {
			t.Errorf("%s: state %q, want %q: the change outlived its failed audit", c.name, got, c.want)
		}
	}
	// The same changes without an entry to write go through.
	if err := s.SetRoomQuota(ctx, "a", 5); err != nil {
		t.Fatal(err)
	}
}
