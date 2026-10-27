// Command e2eoidc is the browser tests' OIDC provider (spec 021): discovery and keys from go-oidc's
// oidctest, an authorization endpoint that signs in the configured user without a page, and a
// token endpoint that checks the client and PKCE. POST /control sets the user and whether the
// provider holds a session, which prompt=none needs; GET /control lists the prompts asked since.
// Plain HTTP on loopback only: never a real provider.
package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/coreos/go-oidc/v3/oidc/oidctest"
)

const keyID = "e2e"

type user struct {
	Sub    string   `json:"sub"`
	Email  string   `json:"email"`
	Name   string   `json:"name"`
	Groups []string `json:"groups"`
}

type grant struct {
	challenge, nonce, redirect string
	user                       user
}

type provider struct {
	issuer, clientID, secret string
	key                      *rsa.PrivateKey
	mu                       sync.Mutex
	user                     user
	session                  bool
	prompts                  []string
	codes                    map[string]grant
}

func main() {
	addr := flag.String("addr", "127.0.0.1:7712", "listen address, loopback")
	issuer := flag.String("issuer", "", "the issuer URL, on a loopback host; http://<addr> when empty")
	clientID := flag.String("client-id", "grill-room-e2e", "the only client")
	secret := flag.String("client-secret", "e2e-secret", "its secret")
	flag.Parse()
	// run serves forever: it only ever returns an error.
	log.Fatal(run(*addr, *issuer, *clientID, *secret))
}

func run(addr, issuer, clientID, secret string) error {
	host, _, err := net.SplitHostPort(addr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		return errors.New("e2eoidc listens on a loopback IP only")
	}
	if issuer == "" {
		issuer = "http://" + addr
	}
	if u, err := url.Parse(issuer); err != nil || u.Scheme != "http" || !loopback(u.Hostname()) {
		return errors.New("e2eoidc's issuer is an http URL on a loopback host only")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	p := &provider{issuer: issuer, clientID: clientID, secret: secret, key: key, codes: map[string]grant{},
		user: user{Sub: "e2e-user", Email: "member@example.com", Name: "Member"}}
	disc := &oidctest.Server{PublicKeys: []oidctest.PublicKey{{PublicKey: key.Public(), KeyID: keyID, Algorithm: oidc.RS256}}}
	disc.SetIssuer(p.issuer)
	mux := http.NewServeMux()
	mux.Handle("GET /.well-known/openid-configuration", disc)
	mux.Handle("GET /keys", disc)
	mux.HandleFunc("GET /auth", p.authorize)
	mux.HandleFunc("POST /token", p.token)
	mux.HandleFunc("GET /control", p.status)
	mux.HandleFunc("POST /control", p.control)
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return srv.Serve(ln)
}

// authorize signs the configured user in at once; prompt=none without a session answers
// login_required, as a provider does when it would have to show a page.
func (p *provider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if q.Get("client_id") != p.clientID || err != nil || redirect.Scheme != "http" || !loopback(redirect.Hostname()) || q.Get("code_challenge_method") != "S256" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	back := redirect.Query()
	back.Set("state", q.Get("state"))
	p.mu.Lock()
	p.prompts = append(p.prompts, q.Get("prompt"))
	if q.Get("prompt") == "none" && !p.session {
		back.Set("error", "login_required")
	} else {
		p.session = true
		code := rand.Text()
		p.codes[code] = grant{challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), redirect: q.Get("redirect_uri"), user: p.user}
		back.Set("code", code)
	}
	p.mu.Unlock()
	redirect.RawQuery = back.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound) //nolint:gosec // redirect_uri is a loopback http URL, checked above
}

func loopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || ip != nil && ip.IsLoopback()
}

func (p *provider) token(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostFormValue("client_id"), r.PostFormValue("client_secret")
	}
	if id != p.clientID || subtle.ConstantTimeCompare([]byte(secret), []byte(p.secret)) != 1 {
		tokenError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	p.mu.Lock()
	g, ok := p.codes[r.PostFormValue("code")]
	delete(p.codes, r.PostFormValue("code"))
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
	if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge || r.PostFormValue("redirect_uri") != g.redirect {
		tokenError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	now := time.Now()
	claims, err := json.Marshal(map[string]any{"iss": p.issuer, "aud": p.clientID, "sub": g.user.Sub, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		"nonce": g.nonce, "email": g.user.Email, "email_verified": g.user.Email != "", "name": g.user.Name, "groups": g.user.Groups})
	if err != nil {
		tokenError(w, http.StatusInternalServerError, "server_error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": rand.Text(), "token_type": "Bearer", "expires_in": 3600,
		"id_token": oidctest.SignIDToken(p.key, keyID, oidc.RS256, string(claims))})
}

func tokenError(w http.ResponseWriter, code int, e string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": e})
}

// control sets the user the next sign-ins get and whether a session exists, each when given, and
// clears the prompts seen.
func (p *provider) control(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User    *user `json:"user"`
		Session *bool `json:"session"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	if req.User != nil {
		p.user = *req.User
	}
	if req.Session != nil {
		p.session = *req.Session
	}
	p.prompts = nil
	p.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (p *provider) status(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"prompts": append([]string{}, p.prompts...), "session": p.session})
}
