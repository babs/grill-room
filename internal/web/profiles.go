package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	smtpmail "github.com/babs/grill-room/internal/mail"
	"github.com/babs/grill-room/internal/store"
)

const (
	profileCookie = "grill_room_profile"
	// DefaultMagicLinkTTL is how long an emailed sign-in link works.
	DefaultMagicLinkTTL = 15 * time.Minute
	maxEmailLen         = 254
	// Magic link requests allowed within magicWindow: per client IP, per address from one IP, and
	// per address from every IP, which caps the mail one address gets.
	magicPerIP        = 10
	magicPerAddressIP = 3
	magicPerAddress   = 10
	magicWindow       = 15 * time.Minute
	sendMailTimeout   = 30 * time.Second
	// emailNudgeSnooze is how long "Later" hides the add-an-email banner (011-AC8).
	emailNudgeSnooze = 14 * 24 * time.Hour
	// mailFailHold is how long a failed bind send makes sign-in answer "mail failing": past it,
	// sign-in is offered again, as only a bind send can clear the state (011-AC28).
	mailFailHold       = 5 * time.Minute
	mailFailedText     = "The mail could not be sent. Try again in a few minutes; if it keeps failing, tell the admin."
	addressRejectedTxt = "The mail server refused this address. Check it, or use another one."
)

var (
	// errMailFailed is a relay that refused the session or did not answer.
	errMailFailed = errors.New("mail send failed")
	// errAddressRejected is a relay that works but permanently refused this address or mail.
	errAddressRejected = errors.New("mail address rejected")
)

// reasonError answers a failed request with a reason code the page maps to its next step.
func reasonError(w http.ResponseWriter, code int, reason, msg string) {
	writeJSON(w, code, map[string]string{"reason": reason, "error": msg})
}

// mailFailingFor returns how long sign-in still answers "mail failing": a bind send failed at the
// relay within mailFailHold and none succeeded since. 0 when mail works.
func (s *Server) mailFailingFor() time.Duration {
	at := s.mailFailedAt.Load()
	if at == 0 {
		return 0
	}
	return max(mailFailHold-s.now().Sub(time.UnixMilli(at)), 0)
}

// retryAfter sets the Retry-After header for d and returns d in whole seconds, rounded up, at least 1.
func retryAfter(w http.ResponseWriter, d time.Duration) int64 {
	n := max(int64((d+time.Second-1)/time.Second), 1)
	w.Header().Set("Retry-After", strconv.FormatInt(n, 10))
	return n
}

func (s *Server) ttlSeconds() int64 { return int64(s.opts.MagicLinkTTL / time.Second) }

func secure(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// setProfileCookie scopes the profile session to the whole site, unlike a room's participant
// cookie: the profile follows its person from room to room.
func setProfileCookie(w http.ResponseWriter, r *http.Request, tok string) {
	http.SetCookie(w, &http.Cookie{
		Name:     profileCookie,
		Value:    tok,
		Path:     "/",
		MaxAge:   int(store.ProfileSessionMaxAge / time.Second),
		HttpOnly: true,
		Secure:   secure(r),
		SameSite: http.SameSiteStrictMode,
	})
}

// profileFromCookie returns the profile of the request's profile cookie, nil when it has none or
// a stale one.
func (s *Server) profileFromCookie(r *http.Request) (*store.Profile, error) {
	c, err := r.Cookie(profileCookie)
	if err != nil || c.Value == "" {
		return nil, nil
	}
	p, err := s.bus.Store.ProfileBySession(r.Context(), HashToken(c.Value))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	return p, err
}

type profileResp struct {
	// EmailEnabled tells whether the server can send magic links: SMTP is configured.
	EmailEnabled bool `json:"email_enabled"`
	// MagicLinkTTLS is how long an emailed link works, in seconds, for the page to say so (011-AC22).
	MagicLinkTTLS int64          `json:"magic_link_ttl_s,omitempty"`
	Profile       *store.Profile `json:"profile"`
}

func (s *Server) profileResp(p *store.Profile) profileResp {
	r := profileResp{EmailEnabled: s.opts.SendMail != nil, Profile: p}
	if r.EmailEnabled {
		r.MagicLinkTTLS = int64(s.opts.MagicLinkTTL / time.Second)
	}
	return r
}

func (s *Server) getProfile(w http.ResponseWriter, r *http.Request) {
	p, err := s.profileFromCookie(r)
	if err != nil {
		s.serverError(w, "get_profile_failed", err)
		return
	}
	if p != nil {
		pb, err := s.bus.Store.PendingEmail(r.Context(), p.ID, s.now())
		if err != nil {
			s.serverError(w, "get_profile_failed", err)
			return
		}
		if pb.Email != "" {
			now := s.now()
			// Relative to the server clock, so a wrong client clock does not matter.
			ttl, age := int64((pb.ExpiresAt.Sub(now)+time.Second-1)/time.Second), int64(max(now.Sub(pb.SentAt), 0)/time.Second)
			p.PendingEmail, p.PendingTTLS, p.PendingAgeS = pb.Email, &ttl, &age
		}
	}
	writeJSON(w, http.StatusOK, s.profileResp(p))
}

// snoozeNudge hides the add-an-email banner for emailNudgeSnooze on every device of the profile.
func (s *Server) snoozeNudge(w http.ResponseWriter, r *http.Request) {
	if crossSite(r) {
		httpError(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	p, err := s.profileFromCookie(r)
	if err != nil {
		s.serverError(w, "snooze_nudge_failed", err)
		return
	}
	if p == nil {
		httpError(w, http.StatusUnauthorized, "no profile on this browser")
		return
	}
	if err := s.bus.Store.SnoozeEmailNudge(r.Context(), p.ID, s.now().Add(emailNudgeSnooze)); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.serverError(w, "snooze_nudge_failed", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// signedIn sets a fresh profile session cookie for p's device and answers with the profile.
func (s *Server) signedIn(w http.ResponseWriter, r *http.Request, p *store.Profile, tok, how string) {
	setProfileCookie(w, r, tok)
	s.log.Info("profile_signed_in", zap.Int64("profile", p.ID), zap.String("how", how))
	writeJSON(w, http.StatusOK, s.profileResp(p))
}

func (s *Server) recoverProfile(w http.ResponseWriter, r *http.Request) {
	if crossSite(r) {
		httpError(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &req) {
		return
	}
	tok, hash := NewToken(), HashToken(req.Token)
	how := "recovery"
	p, err := s.bus.Store.SignInByRecovery(r.Context(), hash, HashToken(tok))
	if errors.Is(err, store.ErrNotFound) {
		how = "recovery_link"
		ctx := store.WithAudit(r.Context(), store.AuditEntry{At: s.now(), Actor: store.ActorSystem, Action: ActionProfileRecoveryUsed,
			TargetKind: store.TargetProfile}, nil)
		p, err = s.bus.Store.UseRecoveryLink(ctx, hash, HashToken(tok), s.now())
	}
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusUnauthorized, "this recovery link is invalid")
		return
	}
	if err != nil {
		s.serverError(w, "recover_profile_failed", err)
		return
	}
	s.signedIn(w, r, p, tok, how)
}

type recoveryLinkResp struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// createRecoveryLink makes a one-time recovery link for the admin to hand over; the token is shown
// here once and only its hash is kept (011-AC1).
func (s *Server) createRecoveryLink(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpError(w, http.StatusNotFound, "no such profile")
		return
	}
	tok, now := NewToken(), s.now()
	ctx := s.auditing(r, ActionProfileRecoveryLink, store.TargetProfile, strconv.FormatInt(id, 10), "",
		map[string]any{"expires_at": now.Add(store.RecoveryLinkTTL).UTC()})
	expires, err := s.bus.Store.CreateRecoveryLink(ctx, id, HashToken(tok), now)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no such profile")
		return
	}
	if err != nil {
		s.serverError(w, "create_recovery_link_failed", err)
		return
	}
	s.log.Info("recovery_link_created", zap.Int64("profile", id))
	writeJSON(w, http.StatusCreated, recoveryLinkResp{URL: s.baseURL + "/profile#recover=" + tok, ExpiresAt: expires.UTC()})
}

// cleanEmail lowercases a bare address and accepts it if mail parses it back unchanged.
func cleanEmail(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || len(s) > maxEmailLen {
		return "", false
	}
	return s, true
}

// emailRequest checks a magic link request: SMTP on, same site, a valid address, within the rate
// limits. It answers and returns false when the request must stop.
// gate, when set, runs once the address is valid and before the rate limit; it answers and returns
// false to stop.
func (s *Server) emailRequest(w http.ResponseWriter, r *http.Request, gate func() bool) (string, []string, bool) {
	if s.opts.SendMail == nil {
		httpError(w, http.StatusNotFound, "email sign-in is not configured")
		return "", nil, false
	}
	if crossSite(r) {
		httpError(w, http.StatusForbidden, "cross-site request refused")
		return "", nil, false
	}
	var req struct {
		Email string `json:"email"`
	}
	if !decode(w, r, &req) {
		return "", nil, false
	}
	email, ok := cleanEmail(req.Email)
	if !ok {
		reasonError(w, http.StatusBadRequest, "invalid_email", "not an email address")
		return "", nil, false
	}
	if gate != nil && !gate() {
		return "", nil, false
	}
	now, ip := s.now(), s.clientIP(r)
	var hits []string
	for _, k := range []struct {
		key string
		max int
	}{{"ip:" + ip, magicPerIP}, {"to:" + email + " ip:" + ip, magicPerAddressIP}, {"to:" + email, magicPerAddress}} {
		if !s.magicLimit.allow(k.key, k.max, now) {
			wait := retryAfter(w, s.magicLimit.retryIn(k.key, now))
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"reason": "rate_limited", "retry_after_s": wait,
				"error": "too many links asked for, retry in " + strconv.FormatInt((wait+59)/60, 10) + " min"})
			return "", nil, false
		}
		hits = append(hits, k.key)
	}
	return email, hits, true
}

func (s *Server) bindEmail(w http.ResponseWriter, r *http.Request) {
	if s.opts.SendMail == nil {
		httpError(w, http.StatusNotFound, "email sign-in is not configured")
		return
	}
	p, err := s.profileFromCookie(r)
	if err != nil {
		s.serverError(w, "bind_email_failed", err)
		return
	}
	if p == nil {
		httpError(w, http.StatusUnauthorized, "no profile on this browser")
		return
	}
	email, hits, ok := s.emailRequest(w, r, nil)
	if !ok {
		return
	}
	// The bind mail goes out before the answer, so a relay failure shows on the page; the address
	// is the requester's own (011-AC28).
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), sendMailTimeout)
	defer cancel()
	err = s.mailLink(ctx, p.ID, email, store.MagicBind)
	if errors.Is(err, errMailFailed) || errors.Is(err, errAddressRejected) {
		// A mail that did not go out spends no allowance: retrying is the page's next step.
		for _, k := range hits {
			s.magicLimit.unhit(k)
		}
	}
	switch {
	case errors.Is(err, errMailFailed):
		reasonError(w, http.StatusBadGateway, "mail_failed", mailFailedText)
	case errors.Is(err, errAddressRejected):
		reasonError(w, http.StatusBadGateway, "address_rejected", addressRejectedTxt)
	case err != nil:
		s.serverError(w, "bind_email_failed", err)
	default:
		writeJSON(w, http.StatusAccepted, map[string]any{"email": email, "ttl_s": s.ttlSeconds()})
	}
}

// emailSignIn mails a sign-in link when a profile holds the address. The lookup and the mail come
// after the answer, which then neither says nor times whether the address is known. "Mail failing"
// comes from bind sends only: a sign-in send happens for known addresses alone, so letting it move
// the state would tell them apart. It is checked before the rate limit, as nothing gets sent
// (011-AC28).
func (s *Server) emailSignIn(w http.ResponseWriter, r *http.Request) {
	email, _, ok := s.emailRequest(w, r, func() bool {
		d := s.mailFailingFor()
		if d == 0 {
			return true
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"reason": "mail_failed", "error": mailFailedText, "retry_after_s": retryAfter(w, d)})
		return false
	})
	if !ok {
		return
	}
	s.afterAnswer(r, func(ctx context.Context) {
		id, err := s.bus.Store.ProfileIDByEmail(ctx, email)
		if errors.Is(err, store.ErrNotFound) {
			return
		}
		if err != nil {
			s.log.Error("email_signin_failed", zap.Error(err))
			return
		}
		if err := s.mailLink(ctx, id, email, store.MagicSignIn); err != nil && !errors.Is(err, errMailFailed) && !errors.Is(err, errAddressRejected) {
			s.log.Error("email_signin_failed", zap.Error(err))
		}
	})
	writeJSON(w, http.StatusAccepted, map[string]any{"ttl_s": s.ttlSeconds()})
}

// afterAnswer runs work in the background, past the request, within sendMailTimeout. WaitMail
// waits for it.
func (s *Server) afterAnswer(r *http.Request, work func(ctx context.Context)) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), sendMailTimeout)
	s.mailWG.Go(func() {
		defer cancel()
		work(ctx)
	})
}

// WaitMail waits for the mail sends still running past their answers, each bounded by
// sendMailTimeout: run it at shutdown, before the store closes.
func (s *Server) WaitMail() { s.mailWG.Wait() }

// mailLink stores a link for profileID and mails it. A bind link goes to #bind=, which the page
// spends at once; a sign-in link to #magic=, which waits for a click. A failed send, logged here,
// drops the link, so it never shows as pending, and returns errAddressRejected when the relay
// refused this address, errMailFailed otherwise. Only a bind send sets or clears the server's mail
// failing state.
func (s *Server) mailLink(ctx context.Context, profileID int64, email, purpose string) error {
	tok := NewToken()
	now := s.now()
	err := s.bus.Store.CreateMagicLink(ctx, store.MagicLink{
		ProfileID: profileID, Email: email, Purpose: purpose, TokenHash: HashToken(tok), ExpiresAt: now.Add(s.opts.MagicLinkTTL),
	}, now)
	if err != nil {
		return fmt.Errorf("magic link for profile %d: %w", profileID, err)
	}
	// 011-AC24: the page and the toasts use these same words.
	subject, intro, anchor := "Sign in to grill-room", "This link signs you in to your grill-room profile:", "magic"
	if purpose == store.MagicBind {
		subject, intro, anchor = "Add this email to your grill-room profile", "This link adds "+email+" to a grill-room profile:", "bind"
	}
	body := fmt.Sprintf("%s\n\n%s/profile#%s=%s\n\nIt works once, within %s. If you did not ask for it, ignore this mail.\n",
		intro, s.baseURL, anchor, tok, durationText(s.opts.MagicLinkTTL))
	if err := s.opts.SendMail(ctx, email, subject, body); err != nil {
		// Relays echo the recipient in their errors; the log must not hold it.
		s.log.Error("magic_link_send_failed", zap.Int64("profile", profileID),
			zap.String("error", strings.ReplaceAll(err.Error(), email, "<recipient>")))
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if derr := s.bus.Store.DeleteMagicLink(dctx, HashToken(tok)); derr != nil {
			s.log.Error("magic_link_drop_failed", zap.Int64("profile", profileID), zap.Error(derr))
		}
		if errors.Is(err, smtpmail.ErrRecipient) {
			return errAddressRejected
		}
		if purpose == store.MagicBind {
			s.mailFailedAt.Store(s.now().UnixMilli())
		}
		return errMailFailed
	}
	if purpose == store.MagicBind {
		s.mailFailedAt.Store(0)
	}
	s.log.Info("magic_link_sent", zap.Int64("profile", profileID), zap.String("purpose", purpose))
	return nil
}

func durationText(d time.Duration) string {
	switch {
	case d == time.Minute:
		return "1 minute"
	case d%time.Minute == 0:
		return strconv.Itoa(int(d/time.Minute)) + " minutes"
	}
	return d.String()
}

// magicError is an emailed link's failure: the page shows the outcome Reason names (011-AC23).
type magicError struct {
	Error       string `json:"error"`
	Reason      string `json:"reason"`
	ProfileName string `json:"profile_name,omitempty"`
	Email       string `json:"email,omitempty"`
}

// useMagicLink spends an emailed link. A sign-in link signs this browser in; a bind link binds the
// address to the profile this browser is in, or, once confirmed, to the profile that asked for it,
// leaving this browser as it was.
func (s *Server) useMagicLink(w http.ResponseWriter, r *http.Request) {
	if s.opts.SendMail == nil {
		httpError(w, http.StatusNotFound, "email sign-in is not configured")
		return
	}
	if crossSite(r) {
		httpError(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	var req struct {
		Token string `json:"token"`
		// Purpose is the link kind the page expects from the URL it came from: the page asks before
		// a sign-in and not before a bind, so the server must not take one for the other.
		Purpose string `json:"purpose"`
		Confirm bool   `json:"confirm"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Purpose != store.MagicBind && req.Purpose != store.MagicSignIn {
		httpError(w, http.StatusBadRequest, "purpose must be bind or signin")
		return
	}
	current, err := s.profileFromCookie(r)
	if err != nil {
		s.serverError(w, "use_magic_link_failed", err)
		return
	}
	var currentID int64
	if current != nil {
		currentID = current.ID
	}
	tok := NewToken()
	p, err := s.bus.Store.UseMagicLink(r.Context(), store.MagicUse{TokenHash: HashToken(req.Token), Purpose: req.Purpose,
		SessionHash: HashToken(tok), Current: currentID, Confirm: req.Confirm, Now: s.now()})
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusUnauthorized, magicError{Reason: "invalid", Error: "this link is invalid"})
	case errors.Is(err, store.ErrLinkExpired):
		writeJSON(w, http.StatusGone, magicError{Reason: "expired", Error: "this link has expired"})
	case errors.Is(err, store.ErrLinkUsed):
		writeJSON(w, http.StatusGone, magicError{Reason: "used", Error: "this link was already used"})
	case errors.Is(err, store.ErrWrongProfile):
		writeJSON(w, http.StatusConflict, magicError{Reason: "confirm", Error: "this link adds the address to another profile",
			ProfileName: p.Name, Email: p.Email})
	case errors.Is(err, store.ErrEmailTaken):
		writeJSON(w, http.StatusConflict, magicError{Reason: "taken",
			Error: "this address is bound to another profile: sign in with it, or ask the admin to merge the two profiles"})
	case err != nil:
		s.serverError(w, "use_magic_link_failed", err)
	case req.Purpose == store.MagicBind && p.ID != currentID:
		s.log.Info("profile_email_bound", zap.Int64("profile", p.ID), zap.Bool("elsewhere", true))
		writeJSON(w, http.StatusOK, map[string]any{"bound": true, "email": p.Email, "profile_name": p.Name})
	case req.Purpose == store.MagicBind:
		s.log.Info("profile_email_bound", zap.Int64("profile", p.ID))
		writeJSON(w, http.StatusOK, s.profileResp(p))
	default:
		s.signedIn(w, r, p, tok, "magic_link")
	}
}

func (s *Server) listProfiles(w http.ResponseWriter, r *http.Request) {
	ps, err := s.bus.Store.Profiles(r.Context())
	if err != nil {
		s.serverError(w, "list_profiles_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, ps)
}

func (s *Server) deleteProfile(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		err = s.bus.Store.DeleteProfile(s.auditing(r, ActionProfileDelete, store.TargetProfile, strconv.FormatInt(id, 10), "", nil), id)
	}
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, strconv.ErrSyntax) || errors.Is(err, strconv.ErrRange) {
		httpError(w, http.StatusNotFound, "no such profile")
		return
	}
	if err != nil {
		s.serverError(w, "delete_profile_failed", err)
		return
	}
	s.checkpoint(context.WithoutCancel(r.Context()))
	s.log.Info("profile_deleted", zap.Int64("profile", id))
	w.WriteHeader(http.StatusNoContent)
}

// limiter counts events per key in a sliding window. In memory: the room server runs one replica.
type limiter struct {
	mu     sync.Mutex
	window time.Duration
	hits   map[string][]time.Time
}

// allow records an event for key and reports whether it is within max per window.
func (l *limiter) allow(key string, maxHits int, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[string][]time.Time{}
	}
	// Sweep idle keys once the map grows, so a flood of addresses cannot grow it without bound.
	if len(l.hits) > 4096 {
		for k, ts := range l.hits {
			if now.Sub(ts[len(ts)-1]) >= l.window {
				delete(l.hits, k)
			}
		}
	}
	ts := l.hits[key]
	i := 0
	for i < len(ts) && now.Sub(ts[i]) >= l.window {
		i++
	}
	ts = ts[i:]
	if len(ts) >= maxHits {
		l.hits[key] = ts
		return false
	}
	l.hits[key] = append(ts, now)
	return true
}

// retryIn returns how long until key's oldest event leaves the window, 0 if key has none.
func (l *limiter) retryIn(key string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ts := l.hits[key]; len(ts) > 0 {
		return max(l.window-now.Sub(ts[0]), 0)
	}
	return 0
}

// unhit forgets key's newest event.
func (l *limiter) unhit(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ts := l.hits[key]; len(ts) > 0 {
		l.hits[key] = ts[:len(ts)-1]
	}
}

// reset forgets key's events.
func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}
