package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

func newToken(t *testing.T, s *Store, ctx context.Context, profile int64, hash, scope string, expires *time.Time, now time.Time) *APIToken {
	t.Helper()
	tok, err := s.CreateAPIToken(ctx, profile, "t-"+hash, hash, scope, expires, now)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// 012-AC1, 012-AC7, 012-AC11: the cap counts unexpired tokens only; expiry turns lookups into
// ErrTokenExpired.
func TestAPITokenCapAndExpiry(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	p := newProfile(t, s, "p")
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	first := newToken(t, s, ctx, p, "h0", ScopeRead, ptr(now.Add(time.Hour)), now)
	for i := 1; i < MaxAPITokens; i++ {
		newToken(t, s, ctx, p, fmt.Sprintf("h%d", i), ScopeFull, nil, now)
	}
	if _, err := s.CreateAPIToken(ctx, p, "over", "h-over", ScopeFull, nil, now); !errors.Is(err, ErrLimit) {
		t.Fatalf("101st: %v", err)
	}
	later := now.Add(time.Hour)
	got, err := s.APITokenByHash(ctx, "h0", later)
	if !errors.Is(err, ErrTokenExpired) || got.ID != first.ID {
		t.Fatalf("expired lookup: %+v %v", got, err)
	}
	if _, err := s.CreateAPIToken(ctx, p, "again", "h-again", ScopeFull, nil, later); err != nil {
		t.Fatalf("once one expired: %v", err)
	}
	if _, err := s.APITokenByHash(ctx, "nope", later); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	ls, err := s.APITokens(ctx, p)
	if err != nil || len(ls) != MaxAPITokens+1 {
		t.Fatalf("list: %d %v", len(ls), err)
	}
	b, _ := json.Marshal(ls[0])
	var keys map[string]any
	_ = json.Unmarshal(b, &keys)
	for _, k := range []string{"token", "token_hash", "hash", "profile_id"} {
		if _, ok := keys[k]; ok {
			t.Fatalf("list JSON has %q: %s", k, b)
		}
	}
}

// 012-AC9: a use is written at most once per window; the purge clears the IP, keeps the time,
// and deletes a token 30 days past its expiry (012-AC7).
func TestAPITokenTouchAndPurge(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	p := newProfile(t, s, "p")
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	tok := newToken(t, s, ctx, p, "h", ScopeFull, ptr(now.Add(time.Hour)), now)
	for _, c := range []struct {
		at    time.Duration
		wrote bool
	}{{0, true}, {10 * time.Second, false}, {61 * time.Second, true}} {
		wrote, err := s.TouchAPIToken(ctx, tok.ID, "192.0.2.1", now.Add(c.at), time.Minute)
		if err != nil || wrote != c.wrote {
			t.Fatalf("touch at %s: %v %v", c.at, wrote, err)
		}
	}
	expiry := now.Add(time.Hour)
	if n, err := s.PurgeAPITokens(ctx, expiry.Add(29*24*time.Hour), now); err != nil || n != 0 {
		t.Fatalf("29 days past expiry: %d %v", n, err)
	}
	ls, _ := s.APITokens(ctx, p)
	if len(ls) != 1 || ls[0].LastUsedIP != "192.0.2.1" || !ls[0].LastUsedAt.Equal(now.Add(61*time.Second)) {
		t.Fatalf("before the IP retention: %+v", ls)
	}
	if _, err := s.PurgeAPITokens(ctx, expiry.Add(29*24*time.Hour), now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	ls, _ = s.APITokens(ctx, p)
	if ls[0].LastUsedIP != "" || ls[0].LastUsedAt == nil {
		t.Fatalf("past the IP retention: %+v", ls[0])
	}
	if n, err := s.PurgeAPITokens(ctx, expiry.Add(31*24*time.Hour), now); err != nil || n != 1 {
		t.Fatalf("31 days past expiry: %d %v", n, err)
	}
}

// 012-AC3: a token acts as its profile's newest participant with a valid invite.
func TestAPITokenParticipant(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	pa := joinWithProfile(t, s, "r", "Ann", JoinProfile{RecoveryHash: "rec", SessionHash: "sess"})
	other := joinWithProfile(t, s, "o", "Bob", JoinProfile{RecoveryHash: "rec-o", SessionHash: "sess-o"})
	now := time.Now()
	newToken(t, s, ctx, pa.ProfileID, "h", ScopeFull, ptr(now.Add(time.Hour)), now)
	got, err := s.APITokenParticipant(ctx, "h", "r", now)
	if err != nil || got.ID != pa.ID {
		t.Fatalf("in room: %+v %v", got, err)
	}
	if _, err := s.APITokenParticipant(ctx, "h", other.RoomID, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other room: %v", err)
	}
	if _, err := s.APITokenParticipant(ctx, "h", "r", now.Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired: %v", err)
	}
	if _, err := s.RevokeInvite(ctx, "r", pa.InviteID, true, "Admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.APITokenParticipant(ctx, "h", "r", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked invite: %v", err)
	}
}

// 012-AC10: create and revoke write their entry in their transaction, with the token's details;
// an audited change made with a token carries its id.
func TestAPITokenAudit(t *testing.T) {
	s := openTest(t)
	p := newProfile(t, s, "p")
	now := time.Now()
	audited := func(action string) context.Context {
		return WithAudit(t.Context(), AuditEntry{At: now, Actor: "profile:x", Action: action, TargetKind: TargetProfile, TargetID: fmt.Sprint(p)}, nil)
	}
	tok, err := s.CreateAPIToken(audited("api_token.create"), p, "ci", "h", ScopeRead, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeAPIToken(WithAPIToken(audited("api_token.revoke"), 42), p, tok.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeAPIToken(audited("api_token.revoke"), p, tok.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second revoke: %v", err)
	}
	log, err := s.AuditLog(t.Context(), AuditQuery{Limit: 5})
	if err != nil || len(log) != 2 {
		t.Fatalf("audit: %+v %v", log, err)
	}
	var revoke, create map[string]any
	_ = json.Unmarshal(log[0].Details, &revoke)
	_ = json.Unmarshal(log[1].Details, &create)
	if log[1].Action != "api_token.create" || create["token_id"] != float64(tok.ID) || create["name"] != "ci" || create["scope"] != "read" || create["expires_at"] != nil {
		t.Fatalf("create entry: %+v %s", log[1], log[1].Details)
	}
	if revoke["api_token"] != float64(42) || revoke["token_id"] != float64(tok.ID) {
		t.Fatalf("revoke entry: %s", log[0].Details)
	}
	if _, ok := create["api_token"]; ok {
		t.Fatal("an entry made without a token names one")
	}
}

// 012-AC12: a merge deletes B's tokens and counts them; deleting a profile deletes its tokens.
func TestAPITokensMergeAndDelete(t *testing.T) {
	f := newMergeFixture(t)
	s, ctx := f.s, t.Context()
	now := time.Now()
	newToken(t, s, ctx, f.b.ProfileID, "hb1", ScopeFull, nil, now)
	newToken(t, s, ctx, f.b.ProfileID, "hb2", ScopeRead, nil, now)
	newToken(t, s, ctx, f.a.ProfileID, "ha", ScopeRead, nil, now)
	pv, err := s.MergePreview(ctx, f.a.ProfileID, f.b.ProfileID, "")
	if err != nil || pv.APITokens != 2 {
		t.Fatalf("preview: %+v %v", pv, err)
	}
	_, moved, _, err := s.MergeProfiles(ctx, f.a.ProfileID, f.b.ProfileID, "")
	if err != nil || moved.APITokensRevoked != 2 {
		t.Fatalf("merge: %+v %v", moved, err)
	}
	for _, h := range []string{"hb1", "hb2"} {
		if _, err := s.APITokenByHash(ctx, h, now); !errors.Is(err, ErrNotFound) {
			t.Fatalf("B's token %s survived the merge: %v", h, err)
		}
	}
	if err := s.DeleteProfile(ctx, f.a.ProfileID); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM api_tokens`); n != 0 {
		t.Fatalf("%d tokens outlived their profile", n)
	}
}

// 012-AC12: an unexpired token keeps an otherwise empty profile from the purge; an expired one
// does not, and goes with it.
func TestPurgeEmptyProfilesAPIToken(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	p := newProfile(t, s, "p")
	now := time.Now()
	newToken(t, s, ctx, p, "h", ScopeRead, ptr(now.Add(time.Hour)), now)
	ageProfile(t, s, p, 1000*time.Hour)
	if got, err := s.PurgeEmptyProfiles(purgeAudit(now), now, EmptyProfileIdle); err != nil || len(got) != 0 {
		t.Fatalf("with a live token: %v %v", got, err)
	}
	later := now.Add(2 * time.Hour)
	if got, err := s.PurgeEmptyProfiles(purgeAudit(later), later, EmptyProfileIdle); err != nil || len(got) != 1 {
		t.Fatalf("with an expired token: %v %v", got, err)
	}
	if n := count(t, s, `SELECT count(*) FROM api_tokens`); n != 0 {
		t.Fatalf("%d tokens outlived the purged profile", n)
	}
}
