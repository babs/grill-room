package web

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coreos/go-oidc/v3/oidc"
	"go.uber.org/zap"
	"golang.org/x/oauth2"

	"github.com/babs/grill-room/internal/store"
)

const (
	// oidcCookiePrefix names a sign-in's cookie, followed by the start of its state: one cookie per
	// sign-in in flight, holding its state, nonce and PKCE verifier until the callback (021-AC2).
	oidcCookiePrefix = "grill_room_oidc_"
	// oidcCookieKey is how much of the state names the cookie: 80 bits, no collision in practice.
	oidcCookieKey = 16
	oidcStateTTL  = 10 * time.Minute
	// oidcKeepFlows bounds the older sign-in cookies a new login leaves: abandoned ones would pile up.
	oidcKeepFlows = 4
	// oidcMaxNext keeps a sealed cookie well under the 4096 bytes browsers store.
	oidcMaxNext = 512
	// oidcTimeout bounds each call to the provider: discovery, keys, code exchange.
	oidcTimeout = 10 * time.Second
	// DefaultGroupsClaim is the ID token claim read for groups when none is set (021-AC3).
	DefaultGroupsClaim = "groups"
	// ActionProfileOIDCBind is a provider sign-in, with how it found its profile (021-AC4).
	ActionProfileOIDCBind = "profile.oidc_bind"
)

// The failure pages' words (021-AC2, 021-AC3).
const (
	oidcRefusedText     = "Your account is not allowed on this instance. Ask for an invite to join a room as a guest."
	oidcRetryText       = "This sign-in expired or was started in another browser. Sign in again."
	oidcInvalidText     = "The answer of your organisation account could not be verified. Sign in again."
	oidcUnavailableText = "The sign-in provider is unavailable. Try again in a moment."
	oidcServerText      = "Something went wrong on the server. Sign in again in a moment."
)

// Results of a provider sign-in, the labels of grill_room_oidc_logins_total (021-AC13).
const (
	oidcResultRefused = "refused_group"
	oidcResultFailed  = "failed"
)

// OIDCResults are every result label.
var OIDCResults = []string{"ok_" + store.OIDCExisting, "ok_" + store.OIDCEmail, "ok_" + store.OIDCNew, oidcResultRefused, oidcResultFailed}

// OIDCConfig is the provider restricted mode signs in with (021-AC1).
type OIDCConfig struct {
	Issuer, ClientID, ClientSecret string
	// RedirectURL is the public URL of /auth/callback.
	RedirectURL string
	// Scopes are asked for on top of openid, email and profile.
	Scopes []string
	// MemberGroup, when set, is the group GroupsClaim must hold (021-AC3); GroupsClaim defaults
	// to DefaultGroupsClaim.
	MemberGroup, GroupsClaim string
}

// OIDC is a provider whose discovery document NewOIDC fetched.
type OIDC struct {
	cfg      OIDCConfig
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	client   *http.Client
}

// NewOIDC fetches c.Issuer's discovery document once (021-AC1).
func NewOIDC(ctx context.Context, c OIDCConfig) (*OIDC, error) {
	client := &http.Client{Timeout: oidcTimeout}
	p, err := oidc.NewProvider(oidc.ClientContext(ctx, client), c.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	if c.GroupsClaim == "" {
		c.GroupsClaim = DefaultGroupsClaim
	}
	return &OIDC{cfg: c, client: client, verifier: p.Verifier(&oidc.Config{ClientID: c.ClientID}), oauth: oauth2.Config{
		ClientID: c.ClientID, ClientSecret: c.ClientSecret, RedirectURL: c.RedirectURL, Endpoint: p.Endpoint(),
		Scopes: append([]string{oidc.ScopeOpenID, "email", "profile"}, c.Scopes...),
	}}, nil
}

// oidcState is what a sign-in cookie carries, under an HMAC of a key derived from the instance's.
type oidcState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"next"`
	Exp      int64  `json:"exp"`
	// Replaced is the hash of the profile session the browser held at login: the callback, reached
	// cross-site, never sees the Strict profile cookie, and ends that session (021-AC4).
	Replaced string `json:"r,omitempty"`
}

const oidcStateKeyUse = "oidc-state"

func (s *Server) sealState(ctx context.Context, st oidcState) (string, error) {
	// HTML escaping would grow a "<" in next sixfold.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(st); err != nil {
		return "", fmt.Errorf("seal oidc state: %w", err)
	}
	b := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	key, err := s.keys.subKey(ctx, oidcStateKeyUse)
	if err != nil {
		return "", err
	}
	sealed := base64.RawURLEncoding.EncodeToString(b)
	return sealed + "." + hmacHex(key, sealed), nil
}

// openState returns the state v seals, false when it is forged, malformed or expired.
func (s *Server) openState(ctx context.Context, v string) (oidcState, bool, error) {
	st, ok, err := s.readState(ctx, v)
	return st, ok && st.Exp > s.now().Unix(), err
}

// readState returns the state v seals, expired or not; false and the zero state when it is forged
// or malformed, which expireOldFlows sorts as the oldest.
func (s *Server) readState(ctx context.Context, v string) (oidcState, bool, error) {
	enc, mac, _ := strings.Cut(v, ".")
	key, err := s.keys.subKey(ctx, oidcStateKeyUse)
	if err != nil {
		return oidcState{}, false, err
	}
	if !hmac.Equal([]byte(mac), []byte(hmacHex(key, enc))) {
		return oidcState{}, false, nil
	}
	var st oidcState
	b, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil || json.Unmarshal(b, &st) != nil {
		return oidcState{}, false, nil
	}
	return st, true, nil
}

// oidcCookieName is the cookie of the sign-in whose state is state, "" for a state NewToken did
// not make: it names a cookie, so it must hold cookie-safe characters only.
func oidcCookieName(state string) string {
	if len(state) != 26 || strings.ContainsFunc(state, func(c rune) bool { return (c < 'A' || c > 'Z') && (c < '2' || c > '7') }) {
		return ""
	}
	return oidcCookiePrefix + state[:oidcCookieKey]
}

// setOIDCCookie is Lax, unlike the profile cookie: the provider sends the browser back with a
// cross-site navigation, which a Strict cookie would not ride.
func setOIDCCookie(w http.ResponseWriter, r *http.Request, name, v string, maxAge int) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure follows the request scheme, as the profile cookie's
		Name: name, Value: v, Path: "/auth", MaxAge: maxAge, HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteLaxMode,
	})
}

// safeNext keeps next when it is a path of this site, "/" otherwise: no open redirect. Browsers
// drop tabs and newlines and read \ as /, so "/\t/x" and "/\x" would leave the site; invalid UTF-8
// would grow sixfold in the sealed cookie.
func safeNext(next string) string {
	if len(next) > oidcMaxNext || !utf8.ValidString(next) || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") ||
		strings.ContainsFunc(next, func(c rune) bool { return c < 0x20 || c == 0x7f || c == '\\' }) {
		return "/"
	}
	return next
}

// oidcLogin starts an authorization code flow with PKCE (021-AC2). Only this site or the address
// bar starts one: another site must not push a browser through the provider.
func (s *Server) oidcLogin(w http.ResponseWriter, r *http.Request) {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		httpError(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	st := oidcState{State: NewToken(), Nonce: NewToken(), Verifier: oauth2.GenerateVerifier(),
		Next: safeNext(r.URL.Query().Get("next")), Exp: s.now().Add(oidcStateTTL).Unix()}
	if c, err := r.Cookie(profileCookie); err == nil && c.Value != "" {
		st.Replaced = HashToken(c.Value)
	}
	v, err := s.sealState(r.Context(), st)
	if err == nil {
		err = s.expireOldFlows(w, r)
	}
	if err != nil {
		s.serverError(w, "oidc_login_failed", err)
		return
	}
	setOIDCCookie(w, r, oidcCookieName(st.State), v, int(oidcStateTTL/time.Second))
	http.Redirect(w, r, s.opts.OIDC.oauth.AuthCodeURL(st.State, oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier)), http.StatusFound)
}

// expireOldFlows clears the request's sign-in cookies but the oidcKeepFlows newest, by their sealed
// expiry; a cookie failing its MAC counts as the oldest, so forged ones cannot evict real flows.
func (s *Server) expireOldFlows(w http.ResponseWriter, r *http.Request) error {
	type flow struct {
		name string
		exp  int64
	}
	var flows []flow
	for _, c := range r.Cookies() {
		if !strings.HasPrefix(c.Name, oidcCookiePrefix) {
			continue
		}
		st, _, err := s.readState(r.Context(), c.Value)
		if err != nil {
			return err
		}
		flows = append(flows, flow{c.Name, st.Exp})
	}
	if len(flows) <= oidcKeepFlows {
		return nil
	}
	slices.SortFunc(flows, func(a, b flow) int { return cmp.Compare(b.exp, a.exp) })
	for _, f := range flows[oidcKeepFlows:] {
		setOIDCCookie(w, r, f.name, "", -1)
	}
	return nil
}

var oidcFailTmpl = template.Must(template.ParseFS(templates, "templates/oidc.html"))

// oidcFailure is a refused callback: its status, logged reason, page words and metric result.
type oidcFailure struct {
	code         int
	reason, text string
	result       string
	fields       []zap.Field
	retry        bool
}

func failure(code int, reason, text string, fields ...zap.Field) *oidcFailure {
	return &oidcFailure{code: code, reason: reason, text: text, result: oidcResultFailed, fields: fields, retry: true}
}

// unavailable is a provider that did not answer; err, when set, is logged cut short, as it may echo
// the provider's answer.
func unavailable(err error, fields ...zap.Field) *oidcFailure {
	if err != nil {
		fields = append(fields, zap.String("error", clip(err.Error(), 256)))
	}
	return failure(http.StatusBadGateway, "provider_unavailable", oidcUnavailableText, fields...)
}

// oidcFailed answers f with a page that says why, logs the reason and counts it; fields never hold
// a code, a token or the state (021-AC2, 021-AC13).
func (s *Server) oidcFailed(w http.ResponseWriter, f *oidcFailure) {
	s.oidcLogins[f.result].Add(1)
	s.log.Warn("oidc_callback_failed", append([]zap.Field{zap.String("reason", f.reason), zap.String("iss", s.opts.OIDC.cfg.Issuer)}, f.fields...)...)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(f.code)
	_ = oidcFailTmpl.Execute(w, map[string]any{"Text": f.text, "Retry": f.retry})
}

// idpErrorCode keeps what an error code may hold, cut short: the URL is anyone's to write.
func idpErrorCode(e string) string {
	return strings.Map(func(c rune) rune {
		if c == '_' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			return c
		}
		return -1
	}, e[:min(len(e), 64)])
}

// oidcCallback ends the flow oidcLogin started: it verifies the state, the code and the ID token,
// checks the member group, and signs this browser into the profile bound to the account
// (021-AC2 to 021-AC4). Nothing is created on any failure.
func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	o := s.opts.OIDC
	st, f := s.callbackState(w, r)
	var idt *oidc.IDToken
	var claims map[string]any
	if f == nil {
		idt, claims, f = s.verifyCallback(r.Context(), r.URL.Query(), st)
	}
	if f == nil && o.cfg.MemberGroup != "" && !hasGroup(claims[o.cfg.GroupsClaim], o.cfg.MemberGroup) {
		f = &oidcFailure{code: http.StatusForbidden, reason: "group", text: oidcRefusedText, result: oidcResultRefused}
	}
	if f != nil {
		s.oidcFailed(w, f)
		return
	}
	in := signInFromClaims(idt.Issuer, idt.Subject, claims, s.now())
	session := NewToken()
	in.RecoveryHash, in.SessionHash = HashToken(NewToken()), HashToken(session)
	actx := store.WithAudit(r.Context(), store.AuditEntry{At: in.Now, Action: ActionProfileOIDCBind, TargetKind: store.TargetProfile}, nil)
	p, how, err := s.bus.Store.SignInOIDC(actx, in)
	if err != nil {
		s.oidcFailed(w, failure(http.StatusInternalServerError, "server", oidcServerText, zap.Error(err)))
		return
	}
	// The browser leaves the profile it held at login: that session ends, the profile stays (021-AC4).
	if st.Replaced != "" {
		if err := s.bus.Store.DeleteProfileSession(r.Context(), st.Replaced); err != nil {
			s.log.Warn("oidc_replaced_session_failed", zap.Error(err))
		}
	}
	setProfileCookie(w, r, session)
	s.oidcLogins["ok_"+how].Add(1)
	s.log.Info("oidc_signed_in", zap.String("iss", idt.Issuer), zap.String("how", how), zap.Int64("profile", p.ID))
	http.Redirect(w, r, st.Next, http.StatusSeeOther)
}

// callbackState opens the cookie of the sign-in the callback's state names, and clears it.
func (s *Server) callbackState(w http.ResponseWriter, r *http.Request) (oidcState, *oidcFailure) {
	q := r.URL.Query().Get("state")
	name := oidcCookieName(q)
	var st oidcState
	ok := false
	if name != "" {
		setOIDCCookie(w, r, name, "", -1)
		if c, err := r.Cookie(name); err == nil {
			if st, ok, err = s.openState(r.Context(), c.Value); err != nil {
				return st, failure(http.StatusInternalServerError, "server", oidcServerText, zap.Error(err))
			}
		}
	}
	if !ok || subtle.ConstantTimeCompare([]byte(q), []byte(st.State)) != 1 {
		return st, failure(http.StatusBadRequest, "state", oidcRetryText)
	}
	return st, nil
}

// verifyCallback turns the provider's answer into a verified ID token and its claims.
func (s *Server) verifyCallback(ctx context.Context, q url.Values, st oidcState) (*oidc.IDToken, map[string]any, *oidcFailure) {
	o := s.opts.OIDC
	if e := q.Get("error"); e != "" {
		e = idpErrorCode(e)
		return nil, nil, failure(http.StatusBadRequest, "idp_error", "Your organisation account did not sign you in ("+e+").", zap.String("idp_error", e))
	}
	ctx, cancel := context.WithTimeout(context.WithValue(ctx, oauth2.HTTPClient, o.client), oidcTimeout)
	defer cancel()
	tok, err := o.oauth.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		return nil, nil, exchangeFailure(err)
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := o.verifier.Verify(ctx, raw)
	if err != nil {
		// The key fetch error has no type: go-oidc wraps it with %v, so only its text tells it apart.
		if strings.Contains(err.Error(), "fetching keys") {
			return nil, nil, unavailable(err)
		}
		return nil, nil, failure(http.StatusBadRequest, "id_token", oidcInvalidText, zap.Error(err))
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(st.Nonce)) != 1 {
		return nil, nil, failure(http.StatusBadRequest, "nonce", oidcInvalidText)
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		return nil, nil, failure(http.StatusBadRequest, "claims", oidcInvalidText, zap.Error(err))
	}
	if idt.Subject == "" || !authorizedParty(idt.Audience, claims, o.cfg.ClientID) {
		return nil, nil, failure(http.StatusBadRequest, "claims", oidcInvalidText)
	}
	return idt, claims, nil
}

// exchangeFailure tells a refused code from a provider that did not answer: no answer, a 5xx or a
// 429.
// A RetrieveError's text holds the provider's whole answer: its code and status alone are logged.
func exchangeFailure(err error) *oidcFailure {
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) || re.Response == nil {
		return unavailable(err)
	}
	f := []zap.Field{zap.String("error", "token endpoint: "+re.ErrorCode), zap.Int("status", re.Response.StatusCode)}
	if re.Response.StatusCode >= 500 || re.Response.StatusCode == http.StatusTooManyRequests {
		return unavailable(nil, f...)
	}
	return failure(http.StatusBadRequest, "exchange", oidcInvalidText, f...)
}

// authorizedParty applies OIDC Core 3.1.3.7: a token for several audiences, or naming an azp, must
// name this client as its azp.
func authorizedParty(aud []string, claims map[string]any, clientID string) bool {
	azp, named := claims["azp"]
	if len(aud) <= 1 && !named {
		return true
	}
	v, _ := azp.(string)
	return v == clientID
}

// signInFromClaims is the sign-in a verified token asks for. An unverified address would let anyone
// take over the profile holding it (021-AC4); a name too long is cut, not dropped.
func signInFromClaims(iss, sub string, claims map[string]any, now time.Time) store.OIDCSignIn {
	in := store.OIDCSignIn{Issuer: iss, Subject: sub, Now: now}
	if claims["email_verified"] == true {
		v, _ := claims["email"].(string)
		in.Email, _ = cleanEmail(v)
	}
	name, _ := claims["name"].(string)
	name = strings.Map(func(c rune) rune {
		if unicode.IsControl(c) || unicode.Is(unicode.Bidi_Control, c) {
			return -1
		}
		return c
	}, strings.ToValidUTF8(name, ""))
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > maxNameLen {
		name = string([]rune(name)[:maxNameLen])
	}
	in.Name, _ = cleanText(name, maxNameLen, false)
	return in
}

// hasGroup reads a groups claim as a list of strings or a single string.
func hasGroup(claim any, group string) bool {
	switch v := claim.(type) {
	case string:
		return v == group
	case []any:
		return slices.Contains(v, any(group))
	}
	return false
}

// oidcLogout signs this browser out of its profile; the provider's session is left as it is.
func (s *Server) oidcLogout(w http.ResponseWriter, r *http.Request) {
	if crossSite(r) {
		httpError(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	if c, err := r.Cookie(profileCookie); err == nil && c.Value != "" {
		if err := s.bus.Store.DeleteProfileSession(r.Context(), HashToken(c.Value)); err != nil {
			s.serverError(w, "oidc_logout_failed", err)
			return
		}
	}
	setProfileCookieAge(w, r, "", -1)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func oidcCounters() map[string]*atomic.Int64 {
	m := make(map[string]*atomic.Int64, len(OIDCResults))
	for _, r := range OIDCResults {
		m[r] = new(atomic.Int64)
	}
	return m
}

// OIDCLogins counts the provider sign-ins that ended with result (021-AC13).
func (s *Server) OIDCLogins(result string) int64 { return s.oidcLogins[result].Load() }

// The refusals of externals in restricted mode (021-AC6, 021-AC7).
const (
	msgSignInToCreate = "Sign in with your organisation account to create rooms"
	msgOrgInvites     = "Only members of the organisation and room admins can invite"
)

// isInternal reports a profile bound to the configured provider; every other one, bound to a former
// provider or to none, and no profile, is external (021-AC5).
func (s *Server) isInternal(p *store.Profile) bool {
	return p != nil && s.opts.OIDC != nil && p.OIDCIssuer == s.opts.OIDC.cfg.Issuer
}

// refuseExternal answers a 403 when restricted mode keeps p from creating rooms (021-AC6).
func (s *Server) refuseExternal(w http.ResponseWriter, p *store.Profile) bool {
	if s.opts.OIDC == nil || s.isInternal(p) {
		return false
	}
	httpError(w, http.StatusForbidden, msgSignInToCreate)
	return true
}

// mayInvite reports whether sess may invite: anyone, but only internals and room admins in
// restricted mode (021-AC7). It marks a room admin on sess, whose invites are audited.
func (s *Server) mayInvite(ctx context.Context, sess *session) (bool, error) {
	admin, err := s.roomAdmin(ctx, sess)
	if err != nil || admin || s.opts.OIDC == nil {
		return err == nil, err
	}
	if sess.p.ProfileID == 0 {
		return false, nil
	}
	p, err := s.bus.Store.ProfileByID(ctx, sess.p.ProfileID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("may invite: %w", err)
	}
	return s.isInternal(p), nil
}

// Restricted reports restricted mode: an OIDC provider is set (021-AC1).
func (s *Server) Restricted() bool { return s.opts.OIDC != nil }
