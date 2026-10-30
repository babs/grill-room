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
	"strconv"
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
	// DefaultOIDCRevalidate is how long an internal's sign-in lasts when none is set (021-AC8).
	DefaultOIDCRevalidate = 168 * time.Hour
	// DefaultOIDCName names the provider on the join page when none is set (021-AC12).
	DefaultOIDCName = "your organisation account"
	// ActionProfileOIDCUnbind is the admin making a bound profile external again (021-AC10).
	ActionProfileOIDCUnbind = "profile.oidc_unbind"
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
	// Revalidate is how long an internal's sign-in lasts, DefaultOIDCRevalidate when 0 (021-AC8).
	Revalidate time.Duration
	// AdminGroup, when set, is the group GroupsClaim holds for an admin session (021-AC11).
	AdminGroup string
	// Name is what the join page signs in with, DefaultOIDCName when empty (021-AC12).
	Name string
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
	if c.Revalidate == 0 {
		c.Revalidate = DefaultOIDCRevalidate
	}
	if c.Name == "" {
		c.Name = DefaultOIDCName
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
	// Silent is a flow started with prompt=none: the callback falls back to a normal one (021-AC8).
	Silent bool `json:"q,omitempty"`
	// Guest is the profile of Replaced when it was unbound and emailless at login: the callback
	// offers to attach it (026-AC3).
	Guest int64 `json:"g,omitempty"`
}

// Each sealed value has its own key: a state never passes as an attach offer.
const (
	oidcStateKeyUse  = "oidc-state"
	oidcAttachKeyUse = "oidc-attach"
)

func (s *Server) sealState(ctx context.Context, st oidcState) (string, error) {
	return s.seal(ctx, oidcStateKeyUse, st)
}

// seal encodes v under an HMAC of the key derived for use.
func (s *Server) seal(ctx context.Context, use string, v any) (string, error) {
	// HTML escaping would grow a "<" in next sixfold.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", fmt.Errorf("seal %s: %w", use, err)
	}
	b := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	key, err := s.keys.subKey(ctx, use)
	if err != nil {
		return "", err
	}
	sealed := base64.RawURLEncoding.EncodeToString(b)
	return sealed + "." + hmacHex(key, sealed), nil
}

// unseal decodes into v what seal sealed for use; false when it is forged or malformed.
func (s *Server) unseal(ctx context.Context, use, sealed string, v any) (bool, error) {
	enc, mac, _ := strings.Cut(sealed, ".")
	key, err := s.keys.subKey(ctx, use)
	if err != nil {
		return false, err
	}
	if !hmac.Equal([]byte(mac), []byte(hmacHex(key, enc))) {
		return false, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(enc)
	return err == nil && json.Unmarshal(b, v) == nil, nil
}

// openState returns the state v seals, false when it is forged, malformed or expired.
func (s *Server) openState(ctx context.Context, v string) (oidcState, bool, error) {
	st, ok, err := s.readState(ctx, v)
	return st, ok && st.Exp > s.now().Unix(), err
}

// readState returns the state v seals, expired or not; false and the zero state when it is forged
// or malformed, which expireOldFlows sorts as the oldest.
func (s *Server) readState(ctx context.Context, v string) (oidcState, bool, error) {
	var st oidcState
	ok, err := s.unseal(ctx, oidcStateKeyUse, v, &st)
	if !ok {
		return oidcState{}, false, err
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
// would grow sixfold in the sealed cookie. A fragment holds secrets the URL must not carry: the page
// keeps it itself (021-AC8).
func safeNext(next string) string {
	if len(next) > oidcMaxNext || !utf8.ValidString(next) || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") ||
		strings.ContainsFunc(next, func(c rune) bool { return c < 0x20 || c == 0x7f || c == '\\' || c == '#' }) {
		return "/"
	}
	return next
}

// oidcLogin starts an authorization code flow with PKCE (021-AC2), with prompt=none when asked:
// a page whose sign-in expired tries the provider silently first (021-AC8). Only this site or the
// address bar starts one: another site must not push a browser through the provider.
func (s *Server) oidcLogin(w http.ResponseWriter, r *http.Request) {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		httpError(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	q := r.URL.Query()
	st := oidcState{Next: safeNext(q.Get("next")), Silent: q.Get("prompt") == "none"}
	if c, err := r.Cookie(profileCookie); err == nil && c.Value != "" {
		st.Replaced = HashToken(c.Value)
		// Only here does the Strict cookie show which guest this browser holds (026-AC3).
		p, err := s.bus.Store.ProfileBySession(r.Context(), st.Replaced)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.serverError(w, "oidc_login_failed", err)
			return
		}
		if err == nil && attachable(p) {
			st.Guest = p.ID
		}
	}
	s.startFlow(w, r, st)
}

// attachable is a guest profile the attach offer covers: an emailed one is bound by its email
// instead (026-AC3).
func attachable(p *store.Profile) bool { return p.OIDCIssuer == "" && p.Email == "" }

// startFlow gives st a fresh state, nonce and verifier, seals it in its own cookie and sends the
// browser to the provider.
func (s *Server) startFlow(w http.ResponseWriter, r *http.Request, st oidcState) {
	st.State, st.Nonce, st.Verifier, st.Exp = NewToken(), NewToken(), oauth2.GenerateVerifier(), s.now().Add(oidcStateTTL).Unix()
	v, err := s.sealState(r.Context(), st)
	if err == nil {
		err = s.expireOldFlows(w, r)
	}
	if err != nil {
		s.serverError(w, "oidc_login_failed", err)
		return
	}
	setOIDCCookie(w, r, oidcCookieName(st.State), v, int(oidcStateTTL/time.Second))
	opts := []oauth2.AuthCodeOption{oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier)}
	if st.Silent {
		opts = append(opts, oauth2.SetAuthURLParam("prompt", "none"))
	}
	http.Redirect(w, r, s.opts.OIDC.oauth.AuthCodeURL(st.State, opts...), http.StatusFound)
}

// interactionNeeded are the errors a provider answers prompt=none with when it must show a page
// (OIDC Core 3.1.2.6).
var interactionNeeded = map[string]bool{"login_required": true, "interaction_required": true, "consent_required": true, "account_selection_required": true}

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

var oidcTmpl = template.Must(template.ParseFS(templates, "templates/oidc.html"))

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
	oidcPage(w, f.code, map[string]any{"Title": "Sign-in failed", "Text": f.text, "Retry": f.retry})
}

// oidcPage answers the sign-in page: a failure, or the attach offer (026-AC3).
func oidcPage(w http.ResponseWriter, code int, data map[string]any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = oidcTmpl.Execute(w, data)
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
	// The new flow starts here, as /auth/login refuses the cross-site chain the provider sent; it is
	// not silent, so a second refusal shows the failure page (021-AC8).
	if f == nil && st.Silent && interactionNeeded[r.URL.Query().Get("error")] {
		s.log.Info("oidc_silent_refused", zap.String("idp_error", idpErrorCode(r.URL.Query().Get("error"))))
		s.startFlow(w, r, oidcState{Next: st.Next, Replaced: st.Replaced, Guest: st.Guest})
		return
	}
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
	// Groups are only known here: the grant, under the group it was made with, stands until the
	// next sign-in (021-AC11).
	if o.cfg.AdminGroup != "" && hasGroup(claims[o.cfg.GroupsClaim], o.cfg.AdminGroup) {
		in.AdminGroup = o.cfg.AdminGroup
	}
	session := NewToken()
	in.RecoveryHash, in.SessionHash = HashToken(NewToken()), HashToken(session)
	actx := store.WithAudit(r.Context(), store.AuditEntry{At: in.Now, Action: ActionProfileOIDCBind, TargetKind: store.TargetProfile}, nil)
	// A profile bound by its email leaves its other devices: its daemons go first (021-AC4 as amended by 026).
	daemons, err := s.bus.Store.OIDCEmailDaemons(r.Context(), in)
	if err == nil {
		err = s.unhostAll(r.Context(), daemons, "the AI host's profile now signs in with the organisation account")
	}
	var p *store.Profile
	var how string
	var ended store.EndedCredentials
	if err == nil {
		p, how, ended, err = s.bus.Store.SignInOIDC(actx, in)
	}
	if err != nil {
		s.oidcFailed(w, failure(http.StatusInternalServerError, "server", oidcServerText, zap.Error(err)))
		return
	}
	s.credentialsEnded(r.Context(), daemons, ended)
	setProfileCookie(w, r, session)
	s.oidcLogins["ok_"+how].Add(1)
	s.log.Info("oidc_signed_in", zap.String("iss", idt.Issuer), zap.String("how", how), zap.Int64("profile", p.ID))
	// A guest the sign-in just bound is no longer attachable: offerAttach reads it again.
	if st.Guest != 0 && s.offerAttach(w, r, st, p.ID) {
		return
	}
	// The browser leaves the profile it held at login: that session ends, the profile stays (021-AC4).
	s.endReplaced(r.Context(), st.Replaced)
	http.Redirect(w, r, st.Next, http.StatusSeeOther)
}

func (s *Server) endReplaced(ctx context.Context, hash string) {
	if hash == "" {
		return
	}
	if err := s.bus.Store.DeleteProfileSession(ctx, hash); err != nil {
		s.log.Warn("oidc_replaced_session_failed", zap.Error(err))
	}
}

// attachOffer is what the attach page posts back, bound to both profiles and to the guest's
// session, which Not now and Attach end: a replay finds it gone (026-AC3, 026-AC5).
type attachOffer struct {
	Profile  int64  `json:"p"`
	Guest    int64  `json:"g"`
	Replaced string `json:"r"`
	Next     string `json:"next"`
	Exp      int64  `json:"exp"`
}

// guestOf is the profile of the guest session hash when it is still guest id and attachable, nil
// otherwise.
func (s *Server) guestOf(ctx context.Context, hash string, id int64) (*store.Profile, error) {
	g, err := s.bus.Store.ProfileBySession(ctx, hash)
	if errors.Is(err, store.ErrNotFound) || err == nil && (g.ID != id || !attachable(g)) {
		return nil, nil
	}
	return g, err
}

// offerAttach shows the attach page for the guest st holds, and reports whether it did; the
// browser is signed in as profile either way. A guest with nothing to move gets none (026-AC3).
func (s *Server) offerAttach(w http.ResponseWriter, r *http.Request, st oidcState, profile int64) bool {
	ctx := r.Context()
	g, err := s.guestOf(ctx, st.Replaced, st.Guest)
	var pv *store.MergePreview
	if err == nil && g != nil {
		pv, err = s.bus.Store.MergePreview(ctx, profile, g.ID, s.orgIssuer())
	}
	var offer string
	rooms, moves := attachMoves(pv)
	if err == nil && len(moves) > 0 {
		// An abandoned offer leaves no lasting session nobody holds (026-AC3).
		if err = s.bus.Store.CapProfileSession(ctx, st.Replaced, time.Unix(st.Exp, 0)); err == nil {
			offer, err = s.seal(ctx, oidcAttachKeyUse, attachOffer{Profile: profile, Guest: g.ID, Replaced: st.Replaced, Next: st.Next, Exp: st.Exp})
		}
	}
	if err != nil {
		s.log.Warn("oidc_attach_offer_failed", zap.Error(err))
	}
	if offer == "" {
		return false
	}
	as := ""
	if who := cmp.Or(pv.Keep.Name, pv.Keep.Email); who != "" {
		as = ", signed in as " + who
	}
	oidcPage(w, http.StatusOK, map[string]any{"Title": "Attach your guest profile", "Offer": offer, "Moves": moves,
		"Text": fmt.Sprintf("Attach your guest profile %s (%s) to %s%s?", cmp.Or(pv.Merge.Name, "Guest"), countOf(rooms, "room", "rooms"), s.opts.OIDC.cfg.Name, as)})
	return true
}

// attachMoves counts the guest's rooms and lists what the merge would bring, so a guest planted on
// the browser shows as someone else's (026-AC3).
func attachMoves(pv *store.MergePreview) (int, []string) {
	if pv == nil {
		return 0, nil
	}
	var names []string
	for _, r := range pv.Rooms {
		if r.Merge {
			names = append(names, r.RoomName)
		}
	}
	var moves []string
	if len(names) > 0 {
		line := "Rooms: " + strings.Join(names[:min(len(names), attachRoomNames)], ", ")
		if len(names) > attachRoomNames {
			line += " and " + strconv.Itoa(len(names)-attachRoomNames) + " more"
		}
		moves = append(moves, line)
	}
	for _, c := range []struct {
		n         int
		one, many string
	}{
		{len(pv.Skills.Merge), "personal skill", "personal skills"}, {pv.SharedSkills.Merge, "published skill", "published skills"},
		{pv.Templates.Merge, "persona template", "persona templates"}, {pv.Starters.Merge, "starter", "starters"},
	} {
		if c.n > 0 {
			moves = append(moves, countOf(c.n, c.one, c.many))
		}
	}
	return len(names), moves
}

// attachRoomNames keeps the offer short: the rest are counted.
const attachRoomNames = 5

func countOf(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// oidcAttachText is every refused attach: what changed is the log's.
const oidcAttachText = "This offer expired. Your guest profile stays as it is; the admin can merge it into yours."

// oidcAttach answers the attach page: Attach merges the guest into the signed-in profile as the
// admin's merge does, Not now leaves it; both end the guest's session (026-AC4, 026-AC5).
func (s *Server) oidcAttach(w http.ResponseWriter, r *http.Request) {
	refuse := func(code int, reason string) {
		s.log.Warn("oidc_attach_refused", zap.String("reason", reason))
		oidcPage(w, code, map[string]any{"Title": "Offer expired", "Text": oidcAttachText, "Retry": true})
	}
	if crossSite(r) {
		refuse(http.StatusForbidden, "cross_site")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var o attachOffer
	ok, err := s.unseal(r.Context(), oidcAttachKeyUse, r.PostFormValue("offer"), &o)
	if err != nil {
		s.serverError(w, "oidc_attach_failed", err)
		return
	}
	if !ok || o.Exp <= s.now().Unix() {
		refuse(http.StatusBadRequest, "offer")
		return
	}
	me, err := s.profileFromCookie(r)
	if err != nil && !errors.Is(err, errReauth) {
		s.serverError(w, "oidc_attach_failed", err)
		return
	}
	if me == nil || me.ID != o.Profile {
		refuse(http.StatusForbidden, "profile")
		return
	}
	g, err := s.guestOf(r.Context(), o.Replaced, o.Guest)
	if err != nil {
		s.serverError(w, "oidc_attach_failed", err)
		return
	}
	attach := r.PostFormValue("choice") == "attach"
	switch {
	case g == nil:
		// Gone, changed, or this offer was already answered.
		refuse(http.StatusBadRequest, "guest")
		return
	case attach:
		// ponytail: the guest is checked outside the merge's transaction; an email bound in between
		// moves with it, as the admin's merge does.
		ctx := store.WithAudit(r.Context(), store.AuditEntry{At: s.now(), Actor: store.ActorProfile + me.UUID, Action: ActionProfileMerge,
			TargetKind: store.TargetProfile, TargetID: strconv.FormatInt(me.ID, 10)}, map[string]any{"via": "oidc_attach"})
		_, moved, err := s.mergeProfilesOf(r.Context(), ctx, me.ID, g.ID)
		if errors.Is(err, store.ErrNotFound) {
			refuse(http.StatusBadRequest, "guest")
			return
		}
		if err != nil {
			s.serverError(w, "oidc_attach_failed", err)
			return
		}
		s.log.Info("oidc_guest_attached", zap.Int64("profile", me.ID), zap.Int64("guest", g.ID), zap.Any("moved", moved))
	}
	// Not now ends the guest's session here; Attach already ended every one of them (026-AC4).
	s.endReplaced(r.Context(), o.Replaced)
	http.Redirect(w, r, o.Next, http.StatusSeeOther)
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
// The room and admin cookies are path-scoped, so this request never carries them: it expires the
// ones of the profile's rooms and the admin one, or the browser would stay in its rooms.
func (s *Server) oidcLogout(w http.ResponseWriter, r *http.Request) {
	if crossSite(r) {
		httpError(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	if c, err := r.Cookie(profileCookie); err == nil && c.Value != "" {
		hash := HashToken(c.Value)
		p, err := s.bus.Store.ProfileBySession(r.Context(), hash)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.serverError(w, "oidc_logout_failed", err)
			return
		}
		if err := s.bus.Store.DeleteProfileSession(r.Context(), hash); err != nil {
			s.serverError(w, "oidc_logout_failed", err)
			return
		}
		if p != nil {
			for _, room := range p.Rooms {
				setRoomCookieAge(w, r, room.RoomID, "", -1)
			}
		}
	}
	setProfileCookieAge(w, r, "", -1)
	setAdminCookie(w, r, "", -1)
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

// errReauth is a profile, or a seat or API token of one, whose sign-in with the provider is past
// its revalidation: serverError and the seat checks answer it 401 reauth (021-AC8).
var errReauth = errors.New("organisation sign-in expired")

const msgReauth = "Your organisation sign-in expired: sign in again"

// msgDaemonReauth is what a daemon shows while its seat's owner has to sign in again (021-AC8).
const msgDaemonReauth = "The room needs its owner to sign in again with the organisation account"

// reauthDue reports an internal whose last sign-in is Revalidate old. The binding is the profile's,
// so sessions and tokens older than the binding lapse with it (021-AC8).
func (s *Server) reauthDue(b store.OIDCBinding) bool {
	o := s.opts.OIDC
	return o != nil && b.OIDCIssuer == o.cfg.Issuer && !s.now().Before(b.OIDCSignedInAt.Add(o.cfg.Revalidate))
}

// orgIssuer is the configured issuer, "" with restricted mode off.
func (s *Server) orgIssuer() string {
	if s.opts.OIDC == nil {
		return ""
	}
	return s.opts.OIDC.cfg.Issuer
}

// orgSignInError answers a sign-in or recovery link of an internal (021-AC9).
var orgSignInError = magicError{Reason: "org_account", Error: "This profile signs in with your organisation account"}

// Restricted reports restricted mode: an OIDC provider is set (021-AC1).
func (s *Server) Restricted() bool { return s.opts.OIDC != nil }

// oidcAdminLogin opens an admin session, as the token login does, for an internal whose last
// sign-in held the admin group, until the earlier of the usual expiry and its revalidation; 401
// otherwise, and the page shows the token form (021-AC11).
func (s *Server) oidcAdminLogin(w http.ResponseWriter, r *http.Request) {
	if crossSite(r) {
		httpError(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	o := s.opts.OIDC
	p, err := s.profileFromCookie(r)
	if err != nil && !errors.Is(err, errReauth) {
		s.serverError(w, "admin_login_failed", err)
		return
	}
	// A grant made under another group, or with none set now, does not count.
	if p == nil || !s.isInternal(p) || o.cfg.AdminGroup == "" || p.OIDCAdminGroup != o.cfg.AdminGroup {
		httpError(w, http.StatusUnauthorized, "no organisation admin sign-in on this browser")
		return
	}
	now, ip := s.now(), s.clientIP(r)
	expires := now.Add(store.AdminSessionTTL)
	if until := p.OIDCSignedInAt.Add(o.cfg.Revalidate); until.Before(expires) {
		expires = until
	}
	tok := NewToken()
	ctx := store.WithAudit(r.Context(), store.AuditEntry{At: now, Actor: store.ActorSuperAdmin, Action: ActionAdminLogin, TargetKind: store.TargetAdminSession},
		map[string]any{"ip": ip, "via": "oidc", "profile": p.UUID})
	sess, err := s.bus.Store.CreateAdminSession(ctx, HashToken(tok), clip(r.UserAgent(), maxUserAgent), ip, now, expires, p.ID)
	if err != nil {
		s.serverError(w, "admin_login_failed", err)
		return
	}
	setAdminCookie(w, r, tok, int(expires.Sub(now)/time.Second))
	s.log.Info("admin_signed_in", zap.Int64("session", sess.ID), zap.String("via", "oidc"), zap.Int64("profile", p.ID))
	writeJSON(w, http.StatusCreated, sess)
}

// adminGrant is what a provider admin session's profile must still hold: a binding to the
// configured issuer, the configured admin group and a sign-in within its revalidation (021-AC11).
func (s *Server) adminGrant() store.AdminGrant {
	o := s.opts.OIDC
	if o == nil {
		return store.AdminGrant{}
	}
	return store.AdminGrant{Issuer: o.cfg.Issuer, Group: o.cfg.AdminGroup, SignedInAfter: s.now().Add(-o.cfg.Revalidate)}
}

// unbindProfile makes a bound profile external again and signs the person out everywhere:
// browsers, API tokens, room seats, paired daemons and provider admin sessions. They come back as
// a guest through their recovery link or an invite (021-AC10, 021-AC11).
func (s *Server) unbindProfile(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var daemons []store.Daemon
	if err == nil {
		daemons, err = s.bus.Store.ProfileDaemons(r.Context(), id)
	}
	// The daemons go as a removal from the host's computer does; the unbind deletes their rows.
	// ProfileDaemons lists none for a profile the unbind would refuse.
	if err == nil {
		err = s.unhostAll(r.Context(), daemons, "the AI host's profile was unbound from the organisation")
	}
	var ended store.EndedCredentials
	if err == nil {
		ended.Seats, ended.DaemonRooms, err = s.bus.Store.UnbindOIDC(s.auditing(r, ActionProfileOIDCUnbind, store.TargetProfile, strconv.FormatInt(id, 10), "", nil), id)
	}
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, strconv.ErrSyntax) || errors.Is(err, strconv.ErrRange) {
		httpError(w, http.StatusNotFound, "no such bound profile")
		return
	}
	if err != nil {
		s.serverError(w, "unbind_profile_failed", err)
		return
	}
	// A daemon paired since ProfileDaemons is among those the statement returned.
	s.credentialsEnded(r.Context(), daemons, ended)
	s.log.Info("profile_oidc_unbound", zap.Int64("profile", id), zap.Int("daemons", len(daemons)))
	w.WriteHeader(http.StatusNoContent)
}
