package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func bindTo(t *testing.T, s *Store, id int64, sub string) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(), `UPDATE profiles SET oidc_issuer = 'https://idp.example', oidc_subject = NULLIF(?, ''), oidc_signed_in_at = ?
		WHERE id = ?`, sub, formatTS(time.Unix(1000, 0)), id); err != nil {
		t.Fatal(err)
	}
	if sub == "" {
		if _, err := s.db.ExecContext(t.Context(), `UPDATE profiles SET oidc_issuer = NULL, oidc_signed_in_at = NULL WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}
}

func subjectOf(t *testing.T, s *Store, id int64) string {
	t.Helper()
	var sub string
	if err := s.db.QueryRowContext(t.Context(), `SELECT COALESCE(oidc_subject, '') FROM profiles WHERE id = ?`, id).Scan(&sub); err != nil {
		t.Fatal(err)
	}
	return sub
}

// 021-AC4: a merge keeps A's provider binding, or moves B's to A; the pair stays unique.
func TestMergeOIDCBinding(t *testing.T) {
	for _, c := range []struct{ name, a, b, want string }{
		{"A only", "sub-a", "", "sub-a"},
		{"B only", "", "sub-b", "sub-b"},
		{"both", "sub-a", "sub-b", "sub-a"},
		{"neither", "", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := openTest(t)
			a, b := newProfile(t, s, "a"), newProfile(t, s, "b")
			bindTo(t, s, a, c.a)
			bindTo(t, s, b, c.b)
			ctx := WithAudit(t.Context(), AuditEntry{At: time.Now(), Actor: ActorSuperAdmin, Action: "profile.merge", TargetKind: TargetProfile}, nil)
			if _, _, err := s.MergeProfiles(ctx, a, b); err != nil {
				t.Fatal(err)
			}
			// Both bound: B's binding goes, said without its subject.
			var details string
			if err := s.db.QueryRowContext(t.Context(), `SELECT details FROM audit_log WHERE action = 'profile.merge'`).Scan(&details); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(details, "dropped_oidc_subject") != (c.name == "both") || strings.Contains(details, "sub-b") {
				t.Errorf("audit %s", details)
			}
			p, err := s.profile(t.Context(), `id = ?`, a)
			if err != nil || subjectOf(t, s, a) != c.want || (c.want == "") != (p.OIDCIssuer == "") || (c.want == "") != p.OIDCSignedInAt.IsZero() {
				t.Fatalf("A bound to %q (%+v %v), want %q", subjectOf(t, s, a), p, err, c.want)
			}
		})
	}
}

// 021-AC4: the pair (issuer, subject) is unique; unbound profiles are many.
func TestOIDCPairUnique(t *testing.T) {
	s := openTest(t)
	a, b := newProfile(t, s, "a"), newProfile(t, s, "b")
	bindTo(t, s, a, "x")
	if _, err := s.db.ExecContext(t.Context(), `UPDATE profiles SET oidc_issuer = 'https://idp.example', oidc_subject = 'x' WHERE id = ?`, b); !isUnique(err) {
		t.Fatalf("second binding of the pair: %v", err)
	}
	if _, err := s.db.ExecContext(t.Context(), `UPDATE profiles SET oidc_issuer = 'https://other.example', oidc_subject = 'x' WHERE id = ?`, b); err != nil {
		t.Fatalf("same subject at another issuer: %v", err)
	}
}

// 021-AC4: SignInOIDC's three ways, and a store failure leaves no profile.
func TestSignInOIDC(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	in := func(sub, email, sess string) OIDCSignIn {
		return OIDCSignIn{Issuer: "https://idp.example", Subject: sub, Email: email, Name: "N", RecoveryHash: "rec-" + sess, SessionHash: sess, Now: time.Unix(5000, 0)}
	}
	p, how, err := s.SignInOIDC(ctx, in("s1", "a@example.com", "h1"))
	if err != nil || how != OIDCNew || p.Email != "a@example.com" || p.Name != "N" || p.OIDCIssuer != "https://idp.example" || !p.OIDCSignedInAt.Equal(time.Unix(5000, 0)) {
		t.Fatalf("new: %+v %s %v", p, how, err)
	}
	if q, err := s.ProfileBySession(ctx, "h1"); err != nil || q.ID != p.ID {
		t.Fatalf("session: %+v %v", q, err)
	}
	// A returning sign-in refreshes its time (021-AC4; phase 3 revalidates from it).
	later := in("s1", "", "h2")
	later.Now = time.Unix(9000, 0)
	if q, how, err := s.SignInOIDC(ctx, later); err != nil || how != OIDCExisting || q.ID != p.ID || !q.OIDCSignedInAt.Equal(later.Now) {
		t.Fatalf("existing: %+v %s %v", q, how, err)
	}
	other := newProfile(t, s, "o")
	if _, err := s.db.ExecContext(ctx, `UPDATE profiles SET email = 'o@example.com' WHERE id = ?`, other); err != nil {
		t.Fatal(err)
	}
	if q, how, err := s.SignInOIDC(ctx, in("s2", "o@example.com", "h3")); err != nil || how != OIDCEmail || q.ID != other || subjectOf(t, s, other) != "s2" {
		t.Fatalf("email: %+v %s %v", q, how, err)
	}
	// The address of a bound profile: a new one, without it.
	if q, how, err := s.SignInOIDC(ctx, in("s3", "o@example.com", "h4")); err != nil || how != OIDCNew || q.ID == other || q.Email != "" {
		t.Fatalf("taken email: %+v %s %v", q, how, err)
	}
	n := count(t, s, `SELECT count(*) FROM profiles`)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := s.SignInOIDC(cctx, in("s4", "", "h5")); err == nil {
		t.Fatal("a cancelled sign-in succeeded")
	}
	if count(t, s, `SELECT count(*) FROM profiles`) != n {
		t.Fatal("a failed sign-in left a profile")
	}
	if err := s.DeleteProfileSession(ctx, "h1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProfileBySession(ctx, "h1"); err == nil {
		t.Fatal("deleted session still signs in")
	}
}

// 021 Phase 1: migration 54 binds nothing on a database at version 53 and keeps its profiles.
func TestMigrateOIDCColumns(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	dsn := testDSN(t)
	s := openAt(t, dsn)
	id := newProfile(t, s, "keep")
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE profiles DROP CONSTRAINT profiles_oidc_unique;
ALTER TABLE profiles DROP COLUMN oidc_issuer, DROP COLUMN oidc_subject, DROP COLUMN oidc_signed_in_at;
UPDATE schema_version SET version = 53`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s = openAt(t, dsn)
	p, err := s.profile(ctx, `id = ?`, id)
	if err != nil || p.OIDCIssuer != "" || !p.OIDCSignedInAt.IsZero() {
		t.Fatalf("profile after the migration: %+v %v", p, err)
	}
	var v int
	if err := s.db.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&v); err != nil || v != baselineVersion+len(migrations) {
		t.Fatalf("version %d: %v", v, err)
	}
	b := newProfile(t, s, "b")
	bindTo(t, s, id, "x")
	if _, err := s.db.ExecContext(ctx, `UPDATE profiles SET oidc_issuer = 'https://idp.example', oidc_subject = 'x' WHERE id = ?`, b); !isUnique(err) {
		t.Fatalf("pair not unique after the migration: %v", err)
	}
}

// 021-AC7: a restricted join refuses an invite whose creator is an external (bound to no issuer,
// or another one) who is not a room admin, in the join's transaction; the admin's (NULL creator), an internal's and a room admin's
// work, and so does every invite with the rule off.
func TestJoinRestricted(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := seed(t, s, "a")
	if _, err := s.CreateInvite(ctx, "a", "by-alice", alice.ID); err != nil {
		t.Fatal(err)
	}
	join := func(name, issuer string) error {
		_, _, err := s.Join(ctx, "by-alice", name, "tok-"+name, JoinProfile{Issuer: issuer})
		return err
	}
	if err := join("off", ""); err != nil {
		t.Fatalf("rule off: %v", err)
	}
	if err := join("ext", "https://idp.example"); !errors.Is(err, ErrInviteLapsed) {
		t.Fatalf("external creator: %v", err)
	}
	if _, _, err := s.Join(ctx, "inv-a", "Admin's", "tok-adm", JoinProfile{Issuer: "https://idp.example"}); err != nil {
		t.Fatalf("admin's invite: %v", err)
	}
	bindTo(t, s, alice.ProfileID, "alice")
	if err := join("int", "https://idp.example"); err != nil {
		t.Fatalf("internal creator: %v", err)
	}
	if err := join("former", "https://other.example"); !errors.Is(err, ErrInviteLapsed) {
		t.Fatalf("creator bound to a former issuer: %v", err)
	}
	bindTo(t, s, alice.ProfileID, "")
	if _, err := s.db.ExecContext(ctx, `INSERT INTO room_admins(room_id, profile_id, created_at) VALUES('a', ?, ?)`, alice.ProfileID, now()); err != nil {
		t.Fatal(err)
	}
	if err := join("radm", "https://idp.example"); err != nil {
		t.Fatalf("room admin creator: %v", err)
	}
}

// 021-AC9: a profile bound to the configured issuer refuses its sign-in and recovery links, which
// stay unused, and is not found by email for a sign-in link; another issuer, or none set, does not.
func TestOrgSignInRefused(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	const iss = "https://idp.example"
	a := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	if _, err := s.db.ExecContext(ctx, `UPDATE profiles SET email = 'a@example.com', oidc_issuer = ?, oidc_subject = 'a' WHERE id = ?`, iss, a.ProfileID); err != nil {
		t.Fatal(err)
	}
	link(s, t, a.ProfileID, "a@example.com", MagicSignIn, "magic-a", time.Hour)
	if _, err := s.CreateRecoveryLink(ctx, a.ProfileID, "link-a", time.Now()); err != nil {
		t.Fatal(err)
	}
	calls := map[string]func(org string) error{
		"recovery token": func(org string) error { _, err := s.SignInByRecovery(ctx, "rec-a", "x", org); return err },
		"recovery link":  func(org string) error { _, err := s.UseRecoveryLink(ctx, "link-a", "y", time.Now(), org); return err },
		"sign-in link": func(org string) error {
			_, err := s.UseMagicLink(ctx, MagicUse{TokenHash: "magic-a", Purpose: MagicSignIn, SessionHash: "z", Now: time.Now(), OrgIssuer: org})
			return err
		},
		"by email": func(org string) error { _, err := s.ProfileIDByEmail(ctx, "a@example.com", org); return err },
	}
	for name, call := range calls {
		if err := call(iss); !errors.Is(err, ErrOrgSignIn) {
			t.Errorf("%s: %v, want ErrOrgSignIn", name, err)
		}
	}
	if n := count(t, s, `SELECT (SELECT count(*) FROM magic_links WHERE used_at IS NULL) + (SELECT count(*) FROM recovery_links WHERE used_at IS NULL)`); n != 2 {
		t.Errorf("%d links unused, want 2", n)
	}
	if n := count(t, s, `SELECT count(*) FROM profile_sessions`); n != 1 {
		t.Errorf("%d sessions, want the join's", n)
	}
	for _, org := range []string{"https://former.example", ""} {
		if err := calls["by email"](org); err != nil {
			t.Errorf("by email, org %q: %v", org, err)
		}
	}
	for _, name := range []string{"recovery token", "recovery link", "sign-in link"} {
		if err := calls[name]("https://former.example"); err != nil {
			t.Errorf("%s, another issuer: %v", name, err)
		}
	}
}

// 021-AC8: the seat, session and token lookups carry their profile's binding.
func TestLookupsCarryBinding(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	at := time.Unix(5000, 0).UTC()
	a := joinWithProfile(t, s, "a", "Alice", JoinProfile{RecoveryHash: "rec-a", SessionHash: "sess-a"})
	if _, err := s.db.ExecContext(ctx, `UPDATE profiles SET oidc_issuer = 'https://idp.example', oidc_subject = 'a', oidc_signed_in_at = ? WHERE id = ?`,
		formatTS(at), a.ProfileID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAPIToken(ctx, a.ProfileID, "ci", "api-a", ScopeFull, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	seat, err1 := s.ParticipantByToken(ctx, "tok-a")
	viaSession, err2 := s.ProfileParticipant(ctx, "sess-a", "a")
	viaToken, err3 := s.APITokenParticipant(ctx, "api-a", "a", time.Now())
	tok, err4 := s.APITokenByHash(ctx, "api-a", time.Now())
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]OIDCBinding{"seat": seat.OIDCBinding, "session": viaSession.OIDCBinding, "token seat": viaToken.OIDCBinding, "token": tok.OIDCBinding} {
		if got.OIDCIssuer != "https://idp.example" || !got.OIDCSignedInAt.Equal(at) {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

// 021-AC11, 021-AC10: each sign-in records the admin group it held, a merge moves B's binding to A
// without B's grant, and UnbindOIDC clears the whole binding once, audited, ErrNotFound after.
func TestOIDCAdminGroupAndUnbind(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	sign := func(sub, sess, group string) *Profile {
		t.Helper()
		p, _, err := s.SignInOIDC(ctx, OIDCSignIn{Issuer: "https://idp.example", Subject: sub, RecoveryHash: "rec-" + sess, SessionHash: sess, Now: time.Unix(5000, 0), AdminGroup: group})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if p := sign("s1", "h1", "ops"); p.OIDCAdminGroup != "ops" {
		t.Fatalf("admin sign-in recorded %q", p.OIDCAdminGroup)
	}
	if p := sign("s1", "h2", ""); p.OIDCAdminGroup != "" {
		t.Fatal("a sign-in without the group kept the grant")
	}
	b := sign("s2", "h3", "ops")
	a := newProfile(t, s, "a")
	mctx := WithAudit(ctx, AuditEntry{At: time.Now(), Actor: ActorSuperAdmin, Action: "profile.merge", TargetKind: TargetProfile}, nil)
	if _, _, err := s.MergeProfiles(mctx, a, b.ID); err != nil {
		t.Fatal(err)
	}
	if p, err := s.profile(ctx, `id = ?`, a); err != nil || p.OIDCAdminGroup != "" || subjectOf(t, s, a) != "s2" {
		t.Fatalf("merged: %+v %v", p, err)
	}
	uctx := func() context.Context {
		return WithAudit(ctx, AuditEntry{At: time.Now(), Actor: ActorSuperAdmin, Action: "profile.oidc_unbind", TargetKind: TargetProfile}, nil)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO api_tokens(profile_id, name, token_hash, scope, created_at) VALUES(?, 't', 'tok-a', 'full', ?)`, a, formatTS(time.Now())); err != nil {
		t.Fatal(err)
	}
	if count(t, s, `SELECT count(*) FROM profile_sessions WHERE profile_id = ?`, a) == 0 {
		t.Fatal("no session to end")
	}
	if _, _, err := s.UnbindOIDC(uctx(), a); err != nil {
		t.Fatal(err)
	}
	// 021-AC10: browser sessions and API tokens go, so an unbind revives no expired access.
	if n := count(t, s, `SELECT (SELECT count(*) FROM profile_sessions WHERE profile_id = ?) + (SELECT count(*) FROM api_tokens WHERE profile_id = ?)`, a, a); n != 0 {
		t.Fatalf("unbind left %d sessions and tokens", n)
	}
	if p, err := s.profile(ctx, `id = ?`, a); err != nil || p.OIDCAdminGroup != "" || p.OIDCIssuer != "" || !p.OIDCSignedInAt.IsZero() || subjectOf(t, s, a) != "" {
		t.Fatalf("unbound: %+v %v", p, err)
	}
	if _, _, err := s.UnbindOIDC(uctx(), a); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second unbind: %v", err)
	}
	if n := count(t, s, `SELECT count(*) FROM audit_log WHERE action = 'profile.oidc_unbind'`); n != 1 {
		t.Fatalf("%d unbind entries", n)
	}
	// Unbound, the subject signs in to a new profile, as any first sign-in.
	if p := sign("s2", "h4", ""); p.ID == a {
		t.Fatal("unbound profile found by its former subject")
	}
}

// 021-AC11: an admin session opened through the provider holds only while its profile is bound to
// the issuer, holds the configured admin group and is within its revalidation; failing any, it
// ends. Losing the group at a sign-in, an unbind or the profile's deletion end it too; a token
// session is never touched.
func TestProviderAdminSessions(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	at := time.Unix(5000, 0)
	sign := func(sub, sess, group string) *Profile {
		t.Helper()
		p, _, err := s.SignInOIDC(ctx, OIDCSignIn{Issuer: "https://idp.example", Subject: sub, Name: "Olga", RecoveryHash: "rec-" + sess, SessionHash: sess, Now: at, AdminGroup: group})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	open := func(hash string, profile int64) {
		t.Helper()
		if _, err := s.CreateAdminSession(ctx, hash, "ua", "ip", at, at.Add(AdminSessionTTL), profile); err != nil {
			t.Fatal(err)
		}
	}
	grant := AdminGrant{Issuer: "https://idp.example", Group: "ops", SignedInAfter: at.Add(-time.Second)}
	touch := func(hash string, g AdminGrant) error {
		_, err := s.TouchAdminSession(ctx, hash, at.Add(time.Minute), g)
		return err
	}
	p := sign("ops", "p1", "ops")
	open("token", 0)
	// A server without restricted mode refuses a provider session but keeps it for its return.
	open("off", p.ID)
	if err := touch("off", AdminGrant{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mode off: %v", err)
	}
	if err := touch("off", grant); err != nil {
		t.Fatalf("mode back on: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE token_hash = 'off'`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		g    AdminGrant
	}{
		{"another admin group", AdminGrant{Issuer: grant.Issuer, Group: "narrower", SignedInAfter: grant.SignedInAfter}},
		{"no admin group", AdminGrant{Issuer: grant.Issuer, SignedInAfter: grant.SignedInAfter}},
		{"another issuer", AdminGrant{Issuer: "https://new.example", Group: "ops", SignedInAfter: grant.SignedInAfter}},
		{"past the revalidation", AdminGrant{Issuer: grant.Issuer, Group: "ops", SignedInAfter: at}},
	} {
		open("h-"+c.name, p.ID)
		if err := touch("h-"+c.name, grant); err != nil {
			t.Fatalf("%s: valid grant refused: %v", c.name, err)
		}
		if err := touch("h-"+c.name, c.g); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", c.name, err)
		}
		// Ended, not only refused: the right grant no longer opens it.
		if err := touch("h-"+c.name, grant); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: still open: %v", c.name, err)
		}
	}
	if err := touch("token", AdminGrant{}); err != nil {
		t.Fatalf("token session: %v", err)
	}
	open("listed", p.ID)
	list, err := s.AdminSessions(ctx, at)
	if err != nil || len(list) != 2 || list[0].ProfileUUID != p.UUID || list[0].ProfileName != "Olga" || list[1].ProfileUUID != "" {
		t.Fatalf("sessions %+v %v", list, err)
	}
	sessions := func() int { return count(t, s, `SELECT count(*) FROM admin_sessions WHERE profile_id IS NOT NULL`) }
	sign("ops", "p2", "")
	if n := sessions(); n != 0 {
		t.Errorf("a sign-in without the group left %d sessions", n)
	}
	p = sign("ops", "p3", "ops")
	open("unbind", p.ID)
	if _, _, err := s.UnbindOIDC(WithAudit(ctx, AuditEntry{At: at, Actor: ActorSuperAdmin, Action: "profile.oidc_unbind", TargetKind: TargetProfile}, nil), p.ID); err != nil {
		t.Fatal(err)
	}
	if n := sessions(); n != 0 {
		t.Errorf("an unbind left %d sessions", n)
	}
	p = sign("ops", "p4", "ops")
	open("delete", p.ID)
	if err := s.DeleteProfile(WithAudit(ctx, AuditEntry{At: at, Actor: ActorSuperAdmin, Action: "profile.delete", TargetKind: TargetProfile}, nil), p.ID); err != nil {
		t.Fatal(err)
	}
	if n := sessions(); n != 0 || count(t, s, `SELECT count(*) FROM admin_sessions`) != 1 {
		t.Errorf("a profile delete left %d provider sessions", n)
	}
}

// 021-AC10, 021-AC4: the audit entry is written in the change's transaction; when it cannot be,
// neither the unbind nor the sign-in happens.
func TestOIDCAuditFailureRollsBack(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	bad := func(action string) context.Context {
		return WithAudit(ctx, AuditEntry{At: time.Now(), Actor: ActorSuperAdmin, Action: action, TargetKind: TargetProfile}, map[string]any{"x": make(chan int)})
	}
	id := newProfile(t, s, "a")
	bindTo(t, s, id, "s1")
	if _, _, err := s.UnbindOIDC(bad("profile.oidc_unbind"), id); err == nil {
		t.Fatal("unbind with a failing audit succeeded")
	}
	if subjectOf(t, s, id) != "s1" {
		t.Fatal("a failed unbind cleared the binding")
	}
	n := count(t, s, `SELECT count(*) FROM profiles`)
	in := OIDCSignIn{Issuer: "https://idp.example", Subject: "s9", RecoveryHash: "rec-x", SessionHash: "hx", Now: time.Unix(5000, 0)}
	if _, _, err := s.SignInOIDC(bad("profile.oidc_bind"), in); err == nil {
		t.Fatal("sign-in with a failing audit succeeded")
	}
	if count(t, s, `SELECT count(*) FROM profiles`) != n {
		t.Fatal("a failed sign-in left a profile")
	}
}

// 021-AC11: an emailless admin with no room activity keeps its profile and its provider admin
// session through the purge of empty profiles.
func TestPurgeKeepsProviderAdmin(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	at := time.Now().Add(-25 * time.Hour)
	p, _, err := s.SignInOIDC(ctx, OIDCSignIn{Issuer: "https://idp.example", Subject: "ops", RecoveryHash: "rec-o", SessionHash: "ho", Now: at, AdminGroup: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE profiles SET created_at = ? WHERE id = ?`, formatTS(at), p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE profile_sessions SET created_at = ? WHERE profile_id = ?`, formatTS(at), p.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := s.CreateAdminSession(ctx, "adm", "ua", "ip", now, now.Add(AdminSessionTTL), p.ID); err != nil {
		t.Fatal(err)
	}
	if purged, err := s.PurgeEmptyProfiles(ctx, now, 24*time.Hour); err != nil || len(purged) != 0 {
		t.Fatalf("purged %v %v", purged, err)
	}
	if _, err := s.TouchAdminSession(ctx, "adm", now, AdminGrant{Issuer: "https://idp.example", Group: "ops", SignedInAfter: at.Add(-time.Hour)}); err != nil {
		t.Fatalf("admin session after the purge: %v", err)
	}
}

// 021-AC10: an unbind ends its seats' own tokens, their paired daemons and the pairings they
// approved, leaving the seats; a sign-in on the profile, by its recovery token, reaches them again.
func TestUnbindEndsSeatsAndDaemons(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	seat := joinWithProfile(t, s, "r1", "Ines", JoinProfile{RecoveryHash: "rec-i", SessionHash: "sess-i"})
	d, err := s.PairDaemon(ctx, "r1", seat.ID, "daemon-i")
	if err != nil {
		t.Fatal(err)
	}
	// Unbound, the profile's daemons are none an unbind would remove.
	if ds, err := s.ProfileDaemons(ctx, seat.ProfileID); err != nil || len(ds) != 0 {
		t.Fatalf("unbound profile's daemons: %+v %v", ds, err)
	}
	bindTo(t, s, seat.ProfileID, "ines")
	if ds, err := s.ProfileDaemons(ctx, seat.ProfileID); err != nil || len(ds) != 1 || ds[0].ID != d.ID || ds[0].RoomID != "r1" {
		t.Fatalf("profile daemons: %+v %v", ds, err)
	}
	// An approval the daemon has not collected yet would pair it after the unbind.
	nowMS := time.Now().UnixMilli()
	if ok, err := s.CreatePairing(ctx, &Pairing{ID: "pr1", RoomID: "r1", Code: "123456", SecretHash: "sec", ExpiresAt: time.Now().Add(10 * time.Minute)}, 5, 50, nowMS); err != nil || !ok {
		t.Fatalf("pairing: %v %v", ok, err)
	}
	if err := s.ApprovePairing(ctx, "r1", "pr1", seat.ID, nowMS); err != nil {
		t.Fatal(err)
	}
	seats, rooms, err := s.UnbindOIDC(WithAudit(ctx, AuditEntry{At: time.Now(), Actor: ActorSuperAdmin, Action: "profile.oidc_unbind", TargetKind: TargetProfile}, nil), seat.ProfileID)
	if err != nil || len(seats) != 1 || seats[0].ID != seat.ID || seats[0].RoomID != "r1" || !slices.Equal(rooms, []string{"r1"}) {
		t.Fatalf("unbind: %+v %v %v", seats, rooms, err)
	}
	if _, err := s.ParticipantByToken(ctx, "tok-r1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("seat token after unbind: %v", err)
	}
	if _, err := s.DaemonByToken(ctx, "daemon-i"); !errors.Is(err, ErrNotFound) {
		t.Errorf("daemon after unbind: %v", err)
	}
	if ds, err := s.ProfileDaemons(ctx, seat.ProfileID); err != nil || len(ds) != 0 {
		t.Errorf("profile daemons after unbind: %+v %v", ds, err)
	}
	if _, _, err := s.PollPairing(ctx, "pr1", "sec", "daemon-late", nowMS); !errors.Is(err, ErrNotFound) {
		t.Errorf("approved pairing polled after unbind: %v", err)
	}
	if n := count(t, s, `SELECT count(*) FROM participants WHERE id = ? AND removed_at IS NULL`, seat.ID); n != 1 {
		t.Fatal("unbind removed the seat")
	}
	if _, err := s.SignInByRecovery(ctx, "rec-i", "sess-back", ""); err != nil {
		t.Fatal(err)
	}
	if p, err := s.ProfileParticipant(ctx, "sess-back", "r1"); err != nil || p.ID != seat.ID {
		t.Fatalf("seat after the profile's sign-in: %+v %v", p, err)
	}
}
