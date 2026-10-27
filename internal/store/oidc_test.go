package store

import (
	"context"
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
