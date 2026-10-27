package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/coreos/go-oidc/v3/oidc/oidctest"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const (
	idpClientID = "grill-room"
	idpSecret   = "client-secret-value"
	idpKeyID    = "k1"
)

// testIdP is an in-process OIDC provider: go-oidc's oidctest serves discovery and keys, and its
// token endpoint hands out the ID token issue registered for a code, after checking PKCE.
type testIdP struct {
	ts    *httptest.Server
	key   *rsa.PrivateKey
	mu    sync.Mutex
	codes map[string]idpGrant
	// issued are every code, ID token and access token handed out, which no log may hold.
	issued []string
	// tokenStatus, when set, is the token endpoint's answer; keysDown fails the keys endpoint.
	tokenStatus int
	keysDown    bool
}

type idpGrant struct {
	challenge string
	idToken   string
}

func newTestIdP(t *testing.T) *testIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &testIdP{key: key, codes: map[string]idpGrant{}}
	disc := &oidctest.Server{PublicKeys: []oidctest.PublicKey{{PublicKey: key.Public(), KeyID: idpKeyID, Algorithm: oidc.RS256}}}
	mux := http.NewServeMux()
	mux.Handle("/", disc)
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		idp.mu.Lock()
		down := idp.keysDown
		idp.mu.Unlock()
		if down {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, strings.Repeat("x", 1000))
			return
		}
		disc.ServeHTTP(w, r)
	})
	mux.HandleFunc("POST /token", idp.token)
	idp.ts = httptest.NewServer(mux)
	t.Cleanup(idp.ts.Close)
	disc.SetIssuer(idp.ts.URL)
	return idp
}

func (idp *testIdP) token(w http.ResponseWriter, r *http.Request) {
	if user, pass, _ := r.BasicAuth(); user != idpClientID || pass != idpSecret {
		if r.PostFormValue("client_id") != idpClientID || r.PostFormValue("client_secret") != idpSecret {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid_client"}`)
			return
		}
	}
	idp.mu.Lock()
	status := idp.tokenStatus
	idp.mu.Unlock()
	if status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":"temporarily_unavailable"}`)
		return
	}
	idp.mu.Lock()
	g, ok := idp.codes[r.PostFormValue("code")]
	delete(idp.codes, r.PostFormValue("code"))
	idp.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
	if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"code `+r.PostFormValue("code")+` refused"}`)
		return
	}
	at := NewToken()
	idp.mu.Lock()
	idp.issued = append(idp.issued, at)
	idp.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": at, "token_type": "Bearer", "expires_in": 3600, "id_token": g.idToken})
}

// issue registers a code for the authorization request auth, whose ID token holds the standard
// claims, then edit's changes; key, when set, signs it instead of the provider's key.
func (idp *testIdP) issue(t *testing.T, auth url.Values, edit map[string]any, key *rsa.PrivateKey) string {
	t.Helper()
	now := time.Now()
	claims := map[string]any{"iss": idp.ts.URL, "aud": idpClientID, "sub": "user-1", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		"nonce": auth.Get("nonce")}
	maps.Copy(claims, edit)
	for k, v := range claims {
		if v == nil {
			delete(claims, k)
		}
	}
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	if key == nil {
		key = idp.key
	}
	tok := oidctest.SignIDToken(key, idpKeyID, oidc.RS256, string(raw))
	code := NewToken()
	idp.mu.Lock()
	idp.codes[code] = idpGrant{challenge: auth.Get("code_challenge"), idToken: tok}
	idp.issued = append(idp.issued, code, tok)
	idp.mu.Unlock()
	return code
}

// newOIDCEnv is a restricted instance signing in with idp; edit changes the provider settings.
func newOIDCEnv(t *testing.T, idp *testIdP, log *zap.Logger, edit func(*OIDCConfig)) *env {
	t.Helper()
	c := OIDCConfig{Issuer: idp.ts.URL, ClientID: idpClientID, ClientSecret: idpSecret, RedirectURL: baseURL + "/auth/callback", Scopes: []string{"groups", "offline"}}
	if edit != nil {
		edit(&c)
	}
	o, err := NewOIDC(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	opts := testOptions
	opts.OIDC = o
	return newEnvLog(t, opts, log)
}

// noFollow sends req with the device's cookies, without following a redirect.
func (d *device) noFollow(t *testing.T, method, path string) *http.Response {
	t.Helper()
	c := *d.c
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(t.Context(), method, d.e.ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// startLogin opens /auth/login?next= and returns the authorization request it redirects to, its
// URL and the state cookie it sets.
func (d *device) startLogin(t *testing.T, next string) (url.Values, *url.URL, *http.Cookie) {
	t.Helper()
	resp := d.noFollow(t, "GET", "/auth/login?next="+url.QueryEscape(next))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return loc.Query(), loc, cookieOf(resp, oidcCookieName(loc.Query().Get("state")))
}

// answer is a response read whole.
type answer struct {
	code   int
	header http.Header
}

func (a answer) cookie(name string) *http.Cookie {
	return cookieOf(&http.Response{Header: a.header}, name)
}

// callback opens /auth/callback with query q and returns the answer and its body.
// The provider sends the browser back cross-site: as browsers do, and Go's jar does not, the
// callback and the redirect it answers go without the SameSite=Strict profile cookie.
func (d *device) callback(t *testing.T, q url.Values) (answer, string) {
	t.Helper()
	c := *d.c
	c.Transport = strictTransport{}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(t.Context(), "GET", d.e.ts.URL+"/auth/callback?"+q.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return answer{resp.StatusCode, resp.Header}, string(b)
}

// strictTransport drops the profile cookie from what a cross-site navigation sends.
type strictTransport struct{}

func (strictTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	cookies := r.Cookies()
	r = r.Clone(r.Context())
	r.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name != profileCookie {
			r.AddCookie(c)
		}
	}
	return http.DefaultTransport.RoundTrip(r)
}

// signIn runs a whole sign-in whose ID token gets claims, and returns the callback's answer.
func (d *device) signIn(t *testing.T, idp *testIdP, claims map[string]any) (answer, string) {
	t.Helper()
	auth, _, _ := d.startLogin(t, "/r/room-1")
	return d.callback(t, url.Values{"state": {auth.Get("state")}, "code": {idp.issue(t, auth, claims, nil)}})
}

func (d *device) profileJSON(t *testing.T) map[string]any {
	t.Helper()
	code, b := d.do(t, "GET", "/api/profile", nil)
	if code != http.StatusOK {
		t.Fatalf("profile: %d %s", code, b)
	}
	return decodeJSON[map[string]any](t, b)
}

func profileID(t *testing.T, resp map[string]any) int64 {
	t.Helper()
	p, _ := resp["profile"].(map[string]any)
	if p == nil {
		return 0
	}
	return int64(p["id"].(float64))
}

func cookieOf(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// 021-AC2: the login redirects to the provider with PKCE S256, state, nonce and every scope, and
// keeps state, nonce and verifier in a short-lived Lax HttpOnly cookie, not in the URL.
func TestOIDCLogin(t *testing.T) {
	idp := newTestIdP(t)
	e := newOIDCEnv(t, idp, zap.NewNop(), nil)
	auth, loc, c := e.device(t).startLogin(t, "/r/x")
	if loc.Scheme+"://"+loc.Host != idp.ts.URL || loc.Path != "/auth" {
		t.Errorf("redirects to %s", loc)
	}
	if auth.Get("response_type") != "code" || auth.Get("client_id") != idpClientID || auth.Get("redirect_uri") != baseURL+"/auth/callback" ||
		auth.Get("scope") != "openid email profile groups offline" || auth.Get("code_challenge_method") != "S256" ||
		len(auth.Get("code_challenge")) < 43 || auth.Get("state") == "" || auth.Get("nonce") == "" || auth.Get("state") == auth.Get("nonce") {
		t.Errorf("authorization request %v", auth)
	}
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/auth" || c.MaxAge != 600 {
		t.Fatalf("state cookie %+v", c)
	}
	if strings.Contains(loc.String(), "code_verifier") {
		t.Error("the verifier is in the URL")
	}
	// Two logins never share a state.
	again, _, _ := e.device(t).startLogin(t, "/")
	if again.Get("state") == auth.Get("state") || again.Get("nonce") == auth.Get("nonce") {
		t.Error("state or nonce reused")
	}
}

// 021-AC2: next is kept only as a path of this site.
func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/r/abc?x=1#y": "/r/abc?x=1#y", "/": "/", "": "/", "//evil.example": "/", "/\\evil.example": "/", "https://evil.example": "/",
		"evil.example": "/", "/\t/evil.example": "/", "/a\\b": "/", "/\n": "/", "/" + strings.Repeat("a", 512): "/", "/" + strings.Repeat("a", 511): "/" + strings.Repeat("a", 511), "/\xff": "/", "javascript:alert(1)": "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

// 021-AC2, 021-AC13: every broken callback is refused with a page that says why, creates and binds
// nothing, clears the state cookie, sets no profile cookie, logs its reason and counts as failed.
func TestOIDCCallbackRefusals(t *testing.T) {
	idp := newTestIdP(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.InfoLevel)
	e := newOIDCEnv(t, idp, zap.New(core), nil)
	cases := []struct {
		name, reason, text string
		// query builds the callback's query from the authorization request.
		query func(t *testing.T, d *device, auth url.Values) url.Values
	}{
		// 021-AC2: a state NewToken did not make names no cookie: refused, nothing cleared.
		{"lowercase state", "state", "Sign in again", func(t *testing.T, d *device, auth url.Values) url.Values {
			return url.Values{"state": {strings.ToLower(auth.Get("state"))}, "code": {idp.issue(t, auth, nil, nil)}}
		}},
		{"cookie syntax in state", "state", "Sign in again", func(t *testing.T, d *device, auth url.Values) url.Values {
			return url.Values{"state": {"AAAAAAAAAAAAAAAA;x=1AAAAAA"}, "code": {idp.issue(t, auth, nil, nil)}}
		}},
		{"wrong state", "state", "Sign in again", func(t *testing.T, d *device, auth url.Values) url.Values {
			return url.Values{"state": {NewToken()}, "code": {idp.issue(t, auth, nil, nil)}}
		}},
		{"no state cookie", "state", "Sign in again", func(t *testing.T, d *device, auth url.Values) url.Values {
			d.c.Jar, _ = cookiejar.New(nil)
			return url.Values{"state": {auth.Get("state")}, "code": {idp.issue(t, auth, nil, nil)}}
		}},
		{"forged state cookie", "state", "Sign in again", func(t *testing.T, d *device, auth url.Values) url.Values {
			u, _ := url.Parse(d.e.ts.URL + "/auth")
			c := d.c.Jar.Cookies(u)[0]
			enc, _, _ := strings.Cut(c.Value, ".")
			b, _ := base64.RawURLEncoding.DecodeString(enc)
			var st oidcState
			_ = json.Unmarshal(b, &st)
			st.Next = "//evil.example"
			b, _ = json.Marshal(st)
			d.c.Jar.SetCookies(u, []*http.Cookie{{Name: c.Name, Value: base64.RawURLEncoding.EncodeToString(b) + "." + strings.Repeat("0", 64), Path: "/auth"}})
			return url.Values{"state": {auth.Get("state")}, "code": {idp.issue(t, auth, nil, nil)}}
		}},
		{"expired state cookie", "state", "Sign in again", func(t *testing.T, d *device, auth url.Values) url.Values {
			d.e.srv.now = func() time.Time { return time.Now().Add(oidcStateTTL + time.Second) }
			return url.Values{"state": {auth.Get("state")}, "code": {idp.issue(t, auth, nil, nil)}}
		}},
		{"provider error", "idp_error", "did not sign you in (access_denied)", func(t *testing.T, d *device, auth url.Values) url.Values {
			return url.Values{"state": {auth.Get("state")}, "error": {"access_denied"}}
		}},
		{"unknown code", "exchange", "could not be verified", func(t *testing.T, d *device, auth url.Values) url.Values {
			return url.Values{"state": {auth.Get("state")}, "code": {"made-up"}}
		}},
		{"wrong PKCE verifier", "exchange", "could not be verified", func(t *testing.T, d *device, auth url.Values) url.Values {
			auth.Set("code_challenge", "not-the-challenge")
			return url.Values{"state": {auth.Get("state")}, "code": {idp.issue(t, auth, nil, nil)}}
		}},
		{"wrong nonce", "nonce", "could not be verified", func(t *testing.T, d *device, auth url.Values) url.Values {
			return url.Values{"state": {auth.Get("state")}, "code": {idp.issue(t, auth, map[string]any{"nonce": "other"}, nil)}}
		}},
		{"no nonce", "nonce", "could not be verified", func(t *testing.T, d *device, auth url.Values) url.Values {
			return url.Values{"state": {auth.Get("state")}, "code": {idp.issue(t, auth, map[string]any{"nonce": nil}, nil)}}
		}},
		{"wrong aud", "id_token", "could not be verified", func(t *testing.T, d *device, auth url.Values) url.Values {
			return url.Values{"state": {auth.Get("state")}, "code": {idp.issue(t, auth, map[string]any{"aud": "another-client"}, nil)}}
		}},
		{"wrong iss", "id_token", "could not be verified", func(t *testing.T, d *device, auth url.Values) url.Values {
			return url.Values{"state": {auth.Get("state")}, "code": {idp.issue(t, auth, map[string]any{"iss": "https://other.example"}, nil)}}
		}},
		{"expired token", "id_token", "could not be verified", func(t *testing.T, d *device, auth url.Values) url.Values {
			return url.Values{"state": {auth.Get("state")}, "code": {idp.issue(t, auth, map[string]any{"exp": time.Now().Add(-time.Minute).Unix()}, nil)}}
		}},
		{"bad signature", "id_token", "could not be verified", func(t *testing.T, d *device, auth url.Values) url.Values {
			return url.Values{"state": {auth.Get("state")}, "code": {idp.issue(t, auth, nil, other)}}
		}},
		// OIDC Core 3.1.3.7: a subject, and this client as azp of a token for several audiences.
		{"no subject", "claims", "could not be verified", func(t *testing.T, d *device, auth url.Values) url.Values {
			return url.Values{"state": {auth.Get("state")}, "code": {idp.issue(t, auth, map[string]any{"sub": ""}, nil)}}
		}},
		{"several audiences, no azp", "claims", "could not be verified", func(t *testing.T, d *device, auth url.Values) url.Values {
			return url.Values{"state": {auth.Get("state")}, "code": {idp.issue(t, auth, map[string]any{"aud": []string{idpClientID, "other"}}, nil)}}
		}},
		{"several audiences, another azp", "claims", "could not be verified", func(t *testing.T, d *device, auth url.Values) url.Values {
			return url.Values{"state": {auth.Get("state")}, "code": {idp.issue(t, auth, map[string]any{"aud": []string{idpClientID, "other"}, "azp": "other"}, nil)}}
		}},
		{"another azp", "claims", "could not be verified", func(t *testing.T, d *device, auth url.Values) url.Values {
			return url.Values{"state": {auth.Get("state")}, "code": {idp.issue(t, auth, map[string]any{"azp": "other"}, nil)}}
		}},
	}
	var states []string
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e.srv.now = time.Now
			d := e.device(t)
			auth, _, _ := d.startLogin(t, "/")
			states = append(states, auth.Get("state"))
			before := e.srv.OIDCLogins("failed")
			logs.TakeAll()
			q := tc.query(t, d, auth)
			resp, body := d.callback(t, q)
			if resp.code != http.StatusBadRequest || !strings.Contains(body, tc.text) || !strings.HasPrefix(resp.header.Get("Content-Type"), "text/html") {
				t.Errorf("answer %d %q", resp.code, body)
			}
			if tc.name == "lowercase state" || tc.name == "cookie syntax in state" {
				if sc := resp.header.Values("Set-Cookie"); len(sc) != 0 {
					t.Errorf("a malformed state set cookies %v", sc)
				}
			} else if c := resp.cookie(oidcCookieName(q.Get("state"))); c == nil || c.MaxAge >= 0 || c.Path != "/auth" {
				t.Errorf("state cookie not cleared: %+v", c)
			}
			if resp.cookie(profileCookie) != nil {
				t.Error("profile cookie set")
			}
			if n := e.profileCount(t); n != 0 {
				t.Errorf("%d profiles", n)
			}
			if got := e.srv.OIDCLogins("failed"); got != before+1 {
				t.Errorf("failed count %d, want %d", got, before+1)
			}
			l := logs.FilterMessage("oidc_callback_failed").All()
			if len(l) != 1 || l[0].ContextMap()["reason"] != tc.reason || l[0].ContextMap()["iss"] != idp.ts.URL {
				t.Errorf("logs %+v", l)
			}
		})
	}
	e.srv.now = time.Now
	// 021-AC13: no log line holds a code, an ID or access token, the state or the client secret.
	idp.mu.Lock()
	secrets := append(append([]string{idpSecret}, idp.issued...), states...)
	idp.mu.Unlock()
	for _, l := range logs.All() {
		line, _ := json.Marshal(l.ContextMap())
		for _, s := range secrets {
			if strings.Contains(string(line)+l.Message, s) {
				t.Errorf("log %s %s holds a secret", l.Message, line)
			}
		}
	}
}

// 021-AC3: a sign-in without the member group is refused with the AC's words and leaves nothing;
// the group as a list or as one string is let in.
func TestOIDCMemberGroup(t *testing.T) {
	idp := newTestIdP(t)
	e := newOIDCEnv(t, idp, zap.NewNop(), func(c *OIDCConfig) { c.MemberGroup, c.GroupsClaim = "staff", "roles" })
	for i, tc := range []struct {
		groups any
		ok     bool
	}{{nil, false}, {[]string{"guests"}, false}, {"guests", false}, {"staff-old", false}, {[]string{"staff-old"}, false}, {[]string{"guests", "staff"}, true}, {"staff", true}} {
		d := e.device(t)
		resp, body := d.signIn(t, idp, map[string]any{"sub": "u" + strconv.Itoa(i), "roles": tc.groups, "groups": "staff"})
		if !tc.ok {
			if resp.code != http.StatusForbidden || !strings.Contains(body, oidcRefusedText) || strings.Contains(body, "/auth/login") {
				t.Errorf("groups %v: %d %q", tc.groups, resp.code, body)
			}
			if n := e.profileCount(t); n != 0 {
				t.Errorf("groups %v: %d profiles", tc.groups, n)
			}
			continue
		}
		if resp.code != http.StatusSeeOther || profileID(t, d.profileJSON(t)) == 0 {
			t.Errorf("groups %v: %d %q", tc.groups, resp.code, body)
		}
	}
	if e.srv.OIDCLogins("refused_group") != 5 || e.srv.OIDCLogins("ok_new") != 2 || e.srv.OIDCLogins("failed") != 0 {
		t.Errorf("counts %d %d", e.srv.OIDCLogins("refused_group"), e.srv.OIDCLogins("ok_new"))
	}
}

// insertProfile adds a profile holding email ("" for none), bound to sub when set.
func insertProfile(t *testing.T, e *env, email, sub string) int64 {
	t.Helper()
	var id int64
	var iss any
	if sub != "" {
		iss = "https://idp.example"
	}
	err := e.sqlDB(t).QueryRowContext(t.Context(), `INSERT INTO profiles(recovery_hash, created_at, uuid, email, oidc_issuer, oidc_subject)
		VALUES($1, '2026-01-01T00:00:00Z', gen_random_uuid()::text, NULLIF($2, ''), $3, NULLIF($4, '')) RETURNING id`, NewToken(), email, iss, sub).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

type boundRow struct {
	Email, Issuer, Subject, Name string
	SignedIn                     bool
}

func bound(t *testing.T, e *env, id int64) boundRow {
	t.Helper()
	var r boundRow
	err := e.sqlDB(t).QueryRowContext(t.Context(), `SELECT COALESCE(email, ''), COALESCE(oidc_issuer, ''), COALESCE(oidc_subject, ''), COALESCE(name, ''),
		oidc_signed_in_at IS NOT NULL FROM profiles WHERE id = $1`, id).Scan(&r.Email, &r.Issuer, &r.Subject, &r.Name, &r.SignedIn)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// 021-AC4: the binding rules, each audited with how it found the profile.
func TestOIDCBinding(t *testing.T) {
	idp := newTestIdP(t)
	core, logs := observer.New(zap.InfoLevel)
	e := newOIDCEnv(t, idp, zap.New(core), nil)
	signIn := func(d *device, claims map[string]any) int64 {
		t.Helper()
		resp, body := d.signIn(t, idp, claims)
		if resp.code != http.StatusSeeOther || resp.header.Get("Location") != "/r/room-1" {
			t.Fatalf("sign-in: %d %s %q", resp.code, resp.header.Get("Location"), body)
		}
		if c := resp.cookie(profileCookie); c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
			t.Fatalf("profile cookie %+v", c)
		}
		return profileID(t, d.profileJSON(t))
	}

	// New: bound, with the verified email and the name of the claims.
	alice := signIn(e.device(t), map[string]any{"sub": "alice", "email": "Alice@Example.com", "email_verified": true, "name": "  Alice  "})
	if r := bound(t, e, alice); r != (boundRow{"alice@example.com", idp.ts.URL, "alice", "Alice", true}) {
		t.Errorf("new profile %+v", r)
	}
	// Existing: another browser, another email in the claims, lands in the same profile.
	if id := signIn(e.device(t), map[string]any{"sub": "alice", "email": "new@example.com", "email_verified": true}); id != alice {
		t.Errorf("existing: profile %d, want %d", id, alice)
	}
	// Email: an unbound profile holding the verified address is bound.
	bob := insertProfile(t, e, "bob@example.com", "")
	if id := signIn(e.device(t), map[string]any{"sub": "bob", "email": "bob@example.com", "email_verified": true}); id != bob {
		t.Errorf("email: profile %d, want %d", id, bob)
	}
	if r := bound(t, e, bob); r.Issuer != idp.ts.URL || r.Subject != "bob" {
		t.Errorf("bob %+v", r)
	}
	// An unverified address binds nothing, and the new profile does not take it.
	carol := insertProfile(t, e, "carol@example.com", "")
	for i, v := range []any{false, "true", nil} {
		id := signIn(e.device(t), map[string]any{"sub": "carol-" + strconv.Itoa(i), "email": "carol@example.com", "email_verified": v})
		if id == carol || bound(t, e, id).Email != "" {
			t.Errorf("email_verified %v bound profile %d %+v", v, id, bound(t, e, id))
		}
	}
	if r := bound(t, e, carol); r.Issuer != "" || r.Email != "carol@example.com" {
		t.Errorf("carol %+v", r)
	}
	// A second account never takes a bound profile: its verified address belongs to alice.
	mallory := signIn(e.device(t), map[string]any{"sub": "mallory", "email": "alice@example.com", "email_verified": true})
	if mallory == alice || bound(t, e, mallory).Email != "" || bound(t, e, alice).Subject != "alice" {
		t.Errorf("second sub: profile %d, alice %+v", mallory, bound(t, e, alice))
	}
	// This browser's unbound profile with another email is left alone: a new profile is bound.
	room := e.createRoom(t, "R")
	d := e.device(t)
	d.join(t, room.ID, "Dave")
	old := profileID(t, d.profileJSON(t))
	dave := signIn(d, map[string]any{"sub": "dave", "email": "dave@example.com", "email_verified": true})
	if dave == old || bound(t, e, old).Issuer != "" || bound(t, e, dave).Email != "dave@example.com" {
		t.Errorf("browser profile %d bound, new %d", old, dave)
	}
	// A browser in a bound profile signing in as another account switches to it.
	if id := signIn(d, map[string]any{"sub": "alice"}); id != alice {
		t.Errorf("switch: profile %d, want %d", id, alice)
	}

	rows, err := e.sqlDB(t).QueryContext(t.Context(), `SELECT target_id, actor, details FROM audit_log WHERE action = $1 ORDER BY id`, ActionProfileOIDCBind)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var target, actor, details string
		if err := rows.Scan(&target, &actor, &details); err != nil {
			t.Fatal(err)
		}
		var dm map[string]string
		_ = json.Unmarshal([]byte(details), &dm)
		if dm["iss"] != idp.ts.URL || !strings.HasPrefix(actor, ActorProfile) {
			t.Errorf("audit %s %s", actor, details)
		}
		got = append(got, target+":"+dm["how"])
	}
	id := func(n int64, how string) string { return strconv.FormatInt(n, 10) + ":" + how }
	want := []string{id(alice, "new"), id(alice, "existing"), id(bob, "email")}
	if len(got) != 9 || strings.Join(got[:3], " ") != strings.Join(want, " ") || got[6] != id(mallory, "new") || got[7] != id(dave, "new") || got[8] != id(alice, "existing") {
		t.Errorf("audit %v", got)
	}
	// 021-AC13: each sign-in logs how it went, and counts by it.
	signed := logs.FilterMessage("oidc_signed_in").All()
	if len(signed) != 9 || signed[2].ContextMap()["how"] != "email" || signed[2].ContextMap()["profile"] != bob || signed[2].ContextMap()["iss"] != idp.ts.URL {
		t.Errorf("logs %+v", signed)
	}
	if e.srv.OIDCLogins("ok_new") != 6 || e.srv.OIDCLogins("ok_existing") != 2 || e.srv.OIDCLogins("ok_email") != 1 {
		t.Errorf("counts new %d existing %d email %d", e.srv.OIDCLogins("ok_new"), e.srv.OIDCLogins("ok_existing"), e.srv.OIDCLogins("ok_email"))
	}
}

// 021-AC5: in restricted mode GET /api/profile says so, where to sign in, and whether the profile
// is internal; sign-out ends the session.
func TestOIDCProfileAndLogout(t *testing.T) {
	idp := newTestIdP(t)
	e := newOIDCEnv(t, idp, zap.NewNop(), nil)
	d := e.device(t)
	check := func(want bool) {
		t.Helper()
		p := d.profileJSON(t)
		if p["restricted"] != true || p["sign_in_url"] != "/auth/login" || p["internal"] != want {
			t.Errorf("profile %v, want internal %v", p, want)
		}
	}
	check(false)
	d.join(t, e.createRoom(t, "R").ID, "Erin")
	check(false)
	if resp, _ := d.signIn(t, idp, map[string]any{"sub": "erin"}); resp.code != http.StatusSeeOther {
		t.Fatalf("sign-in %d", resp.code)
	}
	check(true)

	if code, _ := d.do(t, "POST", "/auth/logout", nil, "Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
		t.Errorf("cross-site sign-out %d", code)
	}
	resp := d.noFollow(t, "POST", "/auth/logout")
	_ = resp.Body.Close()
	if c := cookieOf(resp, profileCookie); resp.StatusCode != http.StatusSeeOther || c == nil || c.MaxAge >= 0 {
		t.Errorf("sign-out %d %+v", resp.StatusCode, c)
	}
	if p := d.profileJSON(t); p["profile"] != nil || p["internal"] != false {
		t.Errorf("after sign-out %v", p)
	}
	var n int
	if err := e.sqlDB(t).QueryRowContext(t.Context(), `SELECT count(*) FROM profile_sessions s JOIN profiles p ON p.id = s.profile_id WHERE p.oidc_subject = 'erin'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d sessions left: %v", n, err)
	}
}

// 021-AC1: without an issuer the /auth routes do not exist and GET /api/profile is as before.
func TestOIDCOffChangesNothing(t *testing.T) {
	e := newEnv(t)
	d := e.device(t)
	for _, r := range []string{"GET /auth/login", "GET /auth/callback", "POST /auth/logout"} {
		method, path, _ := strings.Cut(r, " ")
		if code, _ := d.do(t, method, path, nil); code != http.StatusNotFound {
			t.Errorf("%s: %d", r, code)
		}
	}
	d.join(t, e.createRoom(t, "R").ID, "Finn")
	_, b := d.do(t, "GET", "/api/profile", nil)
	for _, k := range []string{"restricted", "sign_in_url", "internal"} {
		if strings.Contains(string(b), `"`+k+`"`) {
			t.Errorf("%s in %s", k, b)
		}
	}
	if e.srv.Restricted() {
		t.Error("restricted without an issuer")
	}
}

// 021-AC1: a provider whose discovery fails stops NewOIDC.
func TestNewOIDCDiscoveryFails(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(ts.Close)
	if _, err := NewOIDC(t.Context(), OIDCConfig{Issuer: ts.URL, ClientID: "c", ClientSecret: "s"}); err == nil {
		t.Fatal("discovery of a 404 accepted")
	}
	idp := newTestIdP(t)
	if _, err := NewOIDC(t.Context(), OIDCConfig{Issuer: idp.ts.URL + "/other", ClientID: "c", ClientSecret: "s"}); err == nil {
		t.Fatal("an issuer that is not the document's accepted")
	}
}

func mustSubKey(t *testing.T, secret []byte) []byte {
	t.Helper()
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("oidc-state"))
	return m.Sum(nil)
}

// openState refuses a malformed cookie.
func TestOpenStateMalformed(t *testing.T) {
	e := newOIDCEnv(t, newTestIdP(t), zap.NewNop(), nil)
	v, err := e.srv.sealState(t.Context(), oidcState{State: "s", Exp: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if st, ok, err := e.srv.openState(t.Context(), v); !ok || err != nil || st.State != "s" {
		t.Fatalf("sealed state: %+v %v %v", st, ok, err)
	}
	enc, mac, _ := strings.Cut(v, ".")
	// 021-AC2: the MAC is under a key derived for this use, not a limiter key hash of the payload.
	secret, err := e.srv.keys.secret(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if mac != hmacHex(mustSubKey(t, secret), enc) {
		t.Error("state not sealed under the derived key")
	}
	limiterHash, _ := e.srv.keys.hash(t.Context(), "oidc-state:"+enc)
	for _, bad := range []string{enc + "." + limiterHash, "", "nodot", enc + ".", enc + "x." + mac, "!!!." + mac, enc[1:] + "." + mac} {
		if _, ok, _ := e.srv.openState(t.Context(), bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

// 021-AC2: two sign-ins in flight on one browser each keep their own cookie, and both complete in
// either order; each callback clears its own cookie only.
func TestOIDCTwoSignInsInFlight(t *testing.T) {
	idp := newTestIdP(t)
	e := newOIDCEnv(t, idp, zap.NewNop(), nil)
	d := e.device(t)
	first, _, c1 := d.startLogin(t, "/r/one")
	second, _, c2 := d.startLogin(t, "/r/two")
	if c1 == nil || c2 == nil || c1.Name == c2.Name || !strings.HasPrefix(c1.Name, oidcCookiePrefix) {
		t.Fatalf("cookies %+v %+v", c1, c2)
	}
	for _, f := range []struct {
		auth url.Values
		next string
		sub  string
	}{{second, "/r/two", "a"}, {first, "/r/one", "b"}} {
		resp, body := d.callback(t, url.Values{"state": {f.auth.Get("state")}, "code": {idp.issue(t, f.auth, map[string]any{"sub": f.sub}, nil)}})
		if resp.code != http.StatusSeeOther || resp.header.Get("Location") != f.next {
			t.Fatalf("%s: %d %s %q", f.next, resp.code, resp.header.Get("Location"), body)
		}
		var cleared []string
		for _, c := range (&http.Response{Header: resp.header}).Cookies() {
			if strings.HasPrefix(c.Name, oidcCookiePrefix) {
				cleared = append(cleared, c.Name)
			}
		}
		if len(cleared) != 1 || cleared[0] != oidcCookieName(f.auth.Get("state")) {
			t.Errorf("%s cleared %v", f.next, cleared)
		}
	}
}

// 021-AC2: a sign-in another site starts is refused; this site and the address bar start one.
func TestOIDCLoginCrossSite(t *testing.T) {
	e := newOIDCEnv(t, newTestIdP(t), zap.NewNop(), nil)
	for site, want := range map[string]int{"cross-site": http.StatusForbidden, "same-site": http.StatusForbidden, "same-origin": http.StatusFound, "none": http.StatusFound, "": http.StatusFound} {
		c := *e.device(t).c
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		req, _ := http.NewRequestWithContext(t.Context(), "GET", e.ts.URL+"/auth/login", nil)
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want || (want == http.StatusForbidden && len(resp.Cookies()) != 0) {
			t.Errorf("Sec-Fetch-Site %q: %d %v", site, resp.StatusCode, resp.Cookies())
		}
	}
}

// 021-AC2: a full sign-in asked to come back to another site lands on /.
func TestOIDCNextOffSite(t *testing.T) {
	idp := newTestIdP(t)
	e := newOIDCEnv(t, idp, zap.NewNop(), nil)
	for i, next := range []string{"//evil.example", "https://evil.example", "/\\evil.example"} {
		d := e.device(t)
		auth, _, _ := d.startLogin(t, next)
		resp, _ := d.callback(t, url.Values{"state": {auth.Get("state")}, "code": {idp.issue(t, auth, map[string]any{"sub": "n" + strconv.Itoa(i)}, nil)}})
		if resp.code != http.StatusSeeOther || resp.header.Get("Location") != "/" {
			t.Errorf("next %q: %d %q", next, resp.code, resp.header.Get("Location"))
		}
	}
}

// 021-AC2, 021-AC13: a provider that does not answer gets a 502 page, logged
// provider_unavailable with the status, counted as failed, and nothing is created.
func TestOIDCProviderUnavailable(t *testing.T) {
	idp := newTestIdP(t)
	core, logs := observer.New(zap.InfoLevel)
	e := newOIDCEnv(t, idp, zap.New(core), nil)
	for _, tc := range []struct {
		name   string
		set    func(down bool)
		status int64
	}{
		{"token endpoint 503", func(down bool) { idp.tokenStatus = map[bool]int{true: http.StatusServiceUnavailable}[down] }, 503},
		{"token endpoint 429", func(down bool) { idp.tokenStatus = map[bool]int{true: http.StatusTooManyRequests}[down] }, 429},
		{"token endpoint 400", func(down bool) { idp.tokenStatus = map[bool]int{true: http.StatusBadRequest}[down] }, 0},
		{"keys down", func(down bool) { idp.keysDown = down }, 0},
	} {
		d := e.device(t)
		auth, _, _ := d.startLogin(t, "/")
		idp.mu.Lock()
		tc.set(true)
		idp.mu.Unlock()
		logs.TakeAll()
		before := e.srv.OIDCLogins("failed")
		resp, body := d.callback(t, url.Values{"state": {auth.Get("state")}, "code": {idp.issue(t, auth, nil, nil)}})
		idp.mu.Lock()
		tc.set(false)
		idp.mu.Unlock()
		l := logs.FilterMessage("oidc_callback_failed").All()
		if tc.name == "token endpoint 400" {
			// A refusal the provider gave is not an outage.
			if resp.code != http.StatusBadRequest || len(l) != 1 || l[0].ContextMap()["reason"] != "exchange" || l[0].ContextMap()["status"] != int64(400) {
				t.Errorf("%s: %d %+v", tc.name, resp.code, l)
			}
			continue
		}
		if resp.code != http.StatusBadGateway || !strings.Contains(body, oidcUnavailableText) || len(l) != 1 || l[0].ContextMap()["reason"] != "provider_unavailable" {
			t.Errorf("%s: %d %q %+v", tc.name, resp.code, body, l)
		}
		if msg, _ := l[0].ContextMap()["error"].(string); len(msg) > 256 {
			t.Errorf("%s: logged %d bytes of error", tc.name, len(msg))
		}
		if tc.status != 0 && l[0].ContextMap()["status"] != tc.status {
			t.Errorf("%s: logged %+v", tc.name, l[0].ContextMap())
		}
		if e.srv.OIDCLogins("failed") != before+1 || e.profileCount(t) != 0 {
			t.Errorf("%s: count %d, %d profiles", tc.name, e.srv.OIDCLogins("failed"), e.profileCount(t))
		}
	}
	// No answer at all: the token endpoint is gone.
	d := e.device(t)
	auth, _, _ := d.startLogin(t, "/")
	idp.ts.CloseClientConnections()
	idp.ts.Close()
	logs.TakeAll()
	if resp, _ := d.callback(t, url.Values{"state": {auth.Get("state")}, "code": {"c"}}); resp.code != http.StatusBadGateway ||
		logs.FilterMessage("oidc_callback_failed").All()[0].ContextMap()["reason"] != "provider_unavailable" {
		t.Errorf("provider gone: %d", resp.code)
	}
}

// 021-AC2: the provider's error code reaches the page and the log cut to 64 characters of
// [a-z0-9_]: the URL is anyone's to write.
func TestIdPErrorCode(t *testing.T) {
	for in, want := range map[string]string{
		"access_denied": "access_denied",
		"<script>alert(1)</script>" + strings.Repeat("a", 100): "scriptalert1script" + strings.Repeat("a", 64-len("<script>alert(1)</script>")),
		"Login Required\n": "oginequired",
		"é_x":              "_x",
	} {
		if got := idpErrorCode(in); got != want {
			t.Errorf("idpErrorCode(%q) = %q, want %q", in, got, want)
		}
	}
	idp := newTestIdP(t)
	e := newOIDCEnv(t, idp, zap.NewNop(), nil)
	d := e.device(t)
	auth, _, _ := d.startLogin(t, "/")
	_, body := d.callback(t, url.Values{"state": {auth.Get("state")}, "error": {"<script>x</script>" + strings.Repeat("b", 100)}})
	if strings.Contains(body, "<script>x") || !strings.Contains(body, "(scriptxscript"+strings.Repeat("b", 64-len("<script>x</script>"))+")") {
		t.Errorf("page %q", body)
	}
}

// 021-AC4: the sign-in a verified token asks for: email only when verified, the name cleaned and
// cut to 40 characters rather than dropped.
func TestSignInFromClaims(t *testing.T) {
	now := time.Unix(1, 0)
	long := strings.Repeat("é", 30) + strings.Repeat("b", 18)
	for _, tc := range []struct {
		claims      map[string]any
		email, name string
	}{
		{map[string]any{"email": "A@Example.com", "email_verified": true, "name": " Ann "}, "a@example.com", "Ann"},
		{map[string]any{"email": "a@example.com", "email_verified": "true"}, "", ""},
		{map[string]any{"email": "a@example.com"}, "", ""},
		{map[string]any{"email": "not an address", "email_verified": true}, "", ""},
		{map[string]any{"name": long}, "", strings.Repeat("é", 30) + strings.Repeat("b", 10)},
		{map[string]any{"name": "Bo\u202eb\x07\n"}, "", "Bob"},
		{map[string]any{"name": 42}, "", ""},
		{map[string]any{"name": "\u202e"}, "", ""},
	} {
		in := signInFromClaims("iss", "sub", tc.claims, now)
		if in.Issuer != "iss" || in.Subject != "sub" || !in.Now.Equal(now) || in.Email != tc.email || in.Name != tc.name {
			t.Errorf("%v: %+v", tc.claims, in)
		}
	}
	if utf8.RuneCountInString(long) != 48 {
		t.Fatal("the long name is not 48 runes")
	}
}

// 021-AC4: the audit actor is the profile itself, for each way it was found; the binding stays
// out of GET /api/profile's profile object.
func TestOIDCAuditActorAndHiddenBinding(t *testing.T) {
	idp := newTestIdP(t)
	e := newOIDCEnv(t, idp, zap.NewNop(), nil)
	insertProfile(t, e, "x@example.com", "")
	for _, claims := range []map[string]any{{"sub": "x"}, {"sub": "x"}, {"sub": "y", "email": "x@example.com", "email_verified": true}} {
		d := e.device(t)
		if resp, _ := d.signIn(t, idp, claims); resp.code != http.StatusSeeOther {
			t.Fatalf("sign-in %d", resp.code)
		}
		_, b := d.do(t, "GET", "/api/profile", nil)
		var raw struct {
			Profile map[string]any `json:"profile"`
		}
		_ = json.Unmarshal(b, &raw)
		for k := range raw.Profile {
			if strings.HasPrefix(k, "oidc") {
				t.Errorf("profile shows %s: %s", k, b)
			}
		}
	}
	rows, err := e.sqlDB(t).QueryContext(t.Context(), `SELECT a.actor, p.uuid, a.details FROM audit_log a JOIN profiles p ON p.id::text = a.target_id WHERE a.action = $1 ORDER BY a.id`, ActionProfileOIDCBind)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var hows []string
	for rows.Next() {
		var actor, uuid, details string
		if err := rows.Scan(&actor, &uuid, &details); err != nil {
			t.Fatal(err)
		}
		if actor != ActorProfile+uuid {
			t.Errorf("actor %s, profile uuid %s", actor, uuid)
		}
		var dm map[string]string
		_ = json.Unmarshal([]byte(details), &dm)
		hows = append(hows, dm["how"])
	}
	if strings.Join(hows, " ") != "new existing email" {
		t.Errorf("hows %v", hows)
	}
}

// 012-AC6, 021-AC2: an API token never reaches the sign-in routes.
func TestOIDCRoutesCookieOnly(t *testing.T) {
	e := newOIDCEnv(t, newTestIdP(t), zap.NewNop(), nil)
	alice := e.device(t)
	alice.join(t, e.createRoom(t, "R").ID, "Alice")
	tok := alice.newToken(t, "full", 0).Token
	for _, r := range []string{"GET /auth/login", "GET /auth/callback", "POST /auth/logout"} {
		method, path, _ := strings.Cut(r, " ")
		if code, b := e.do(t, method, path, tok, nil); code != http.StatusForbidden || errorOf(t, b) != msgTokenCookie {
			t.Errorf("%s: %d %s", r, code, b)
		}
	}
}

// 021-AC2: sign-out without a profile cookie still answers and clears.
func TestOIDCLogoutWithoutProfile(t *testing.T) {
	e := newOIDCEnv(t, newTestIdP(t), zap.NewNop(), nil)
	resp := e.device(t).noFollow(t, "POST", "/auth/logout")
	_ = resp.Body.Close()
	if c := cookieOf(resp, profileCookie); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" || c == nil || c.MaxAge >= 0 {
		t.Errorf("sign-out %d %+v", resp.StatusCode, c)
	}
}

// 021-AC2: the largest next kept seals into a cookie browsers store whole.
func TestOIDCCookieSize(t *testing.T) {
	e := newOIDCEnv(t, newTestIdP(t), zap.NewNop(), nil)
	room := e.createRoom(t, "R").ID
	for _, c := range []string{"<", "\"", "\u2028"} {
		next := "/" + strings.Repeat(c, (oidcMaxNext-1)/len(c))
		if safeNext(next) != next {
			t.Fatalf("next of %q not kept", c)
		}
		// A browser in a profile seals its session hash too.
		d := e.device(t)
		d.join(t, room, "P")
		resp := d.noFollow(t, "GET", "/auth/login?next="+url.QueryEscape(next))
		_ = resp.Body.Close()
		sc := resp.Header.Values("Set-Cookie")
		if len(sc) != 1 || len(sc[0]) > 4096 || !strings.Contains(sc[0], "grill_room_oidc_") {
			t.Errorf("next of %q: %d cookies, %d bytes", c, len(sc), len(sc[0]))
		}
	}
}

// 021-AC2: a new sign-in keeps the 4 newest sign-ins in flight and clears the others.
func TestOIDCOldFlowsExpire(t *testing.T) {
	e := newOIDCEnv(t, newTestIdP(t), zap.NewNop(), nil)
	d := e.device(t)
	u, _ := url.Parse(e.ts.URL + "/auth")
	base := time.Now().Add(time.Minute)
	var names []string
	for i := range 10 {
		st := oidcState{State: NewToken(), Exp: base.Add(time.Duration(i) * time.Second).Unix()}
		v, err := e.srv.sealState(t.Context(), st)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, oidcCookieName(st.State))
		d.c.Jar.SetCookies(u, []*http.Cookie{{Name: names[i], Value: v, Path: "/auth"}})
	}
	d.c.Jar.SetCookies(u, []*http.Cookie{{Name: oidcCookiePrefix + "junk", Value: "x", Path: "/auth"}})
	d.startLogin(t, "/")
	left := map[string]bool{}
	for _, c := range d.c.Jar.Cookies(u) {
		if strings.HasPrefix(c.Name, oidcCookiePrefix) {
			left[c.Name] = true
		}
	}
	if len(left) != oidcKeepFlows+1 {
		t.Errorf("%d sign-in cookies left: %v", len(left), left)
	}
	for _, n := range names[10-oidcKeepFlows:] {
		if !left[n] {
			t.Errorf("newest flow %s cleared", n)
		}
	}
}

// 021-AC4: a guest profile this browser holds is not bound: the browser moves to a new bound
// profile, the old session ends and the old profile stays as it was.
func TestOIDCReplacedSessionEnds(t *testing.T) {
	idp := newTestIdP(t)
	e := newOIDCEnv(t, idp, zap.NewNop(), nil)
	d := e.device(t)
	d.join(t, e.createRoom(t, "R").ID, "Guest")
	guest := profileID(t, d.profileJSON(t))
	var old string
	for _, c := range d.c.Jar.Cookies(&url.URL{Scheme: "http", Host: strings.TrimPrefix(e.ts.URL, "http://"), Path: "/"}) {
		if c.Name == profileCookie {
			old = c.Value
		}
	}
	if old == "" {
		t.Fatal("no profile cookie")
	}
	if resp, _ := d.signIn(t, idp, map[string]any{"sub": "g", "email": "g@example.com", "email_verified": true}); resp.code != http.StatusSeeOther {
		t.Fatalf("sign-in %d", resp.code)
	}
	if id := profileID(t, d.profileJSON(t)); id == guest || id == 0 {
		t.Errorf("browser in profile %d, guest %d", id, guest)
	}
	var n int
	if err := e.sqlDB(t).QueryRowContext(t.Context(), `SELECT count(*) FROM profile_sessions WHERE token_hash = $1`, HashToken(old)).Scan(&n); err != nil || n != 0 {
		t.Errorf("replaced session kept: %d %v", n, err)
	}
	if r := bound(t, e, guest); r.Issuer != "" || r.Email != "" {
		t.Errorf("guest changed: %+v", r)
	}
}

// 021-AC2: cookies failing their MAC count as the oldest: forged far-future flows cannot evict a
// real one.
func TestOIDCForgedFlowsEvictedFirst(t *testing.T) {
	e := newOIDCEnv(t, newTestIdP(t), zap.NewNop(), nil)
	d := e.device(t)
	u, _ := url.Parse(e.ts.URL + "/auth")
	real := oidcState{State: NewToken(), Exp: time.Now().Add(time.Minute).Unix()}
	v, err := e.srv.sealState(t.Context(), real)
	if err != nil {
		t.Fatal(err)
	}
	d.c.Jar.SetCookies(u, []*http.Cookie{{Name: oidcCookieName(real.State), Value: v, Path: "/auth"}})
	var forged []string
	for range oidcKeepFlows {
		st := oidcState{State: NewToken(), Exp: time.Now().Add(1000 * time.Hour).Unix()}
		b, _ := json.Marshal(st)
		forged = append(forged, oidcCookieName(st.State))
		d.c.Jar.SetCookies(u, []*http.Cookie{{Name: forged[len(forged)-1], Value: base64.RawURLEncoding.EncodeToString(b) + "." + strings.Repeat("0", 64), Path: "/auth"}})
	}
	d.startLogin(t, "/")
	left := map[string]bool{}
	for _, c := range d.c.Jar.Cookies(u) {
		left[c.Name] = true
	}
	if !left[oidcCookieName(real.State)] {
		t.Errorf("the real flow was evicted: %v", left)
	}
}
