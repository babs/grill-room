// Package proxy is the bearer-swap reverse proxy: it replaces a sandbox's dummy bearer with an
// account's real Claude token, so no sandbox or worker ever holds a usable credential. One proxy
// serves one account; each client (a worker) has its own dummy bearer, so the proxy counts its
// requests and can cut it off alone (003-AC14, 003-AC15).
//
// Derived from the grill-room S3 spike (spikes/s3/proxy/main.go) at commit 3ae0776.
package proxy

import (
	"context"
	"crypto/sha256"
	"maps"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/creds"
)

// Header values safe to log: feature flags and client identity, never credentials.
var loggedValues = []string{"anthropic-beta", "anthropic-version", "user-agent", "x-app"}

// TokenSource hands out the account's real token. Invalidate asks for a fresh read after upstream
// refused the token.
type TokenSource interface {
	Get(ctx context.Context) (creds.Token, error)
	Invalidate()
}

// allowed holds every "METHOD escaped-path" Claude Code 2.1.296 sends through the proxy (spike S16).
// Security: the sandbox can read its dummy bearer, so any other path would let its code spend the
// account's real token on any Anthropic API (connectors, OAuth, API keys).
var allowed = map[string]bool{
	"POST /v1/messages":              true,
	"POST /v1/messages/count_tokens": true,
	"HEAD /api/hello":                true,
}

// maxLogged caps a denied method or path in the log: the sandbox picks their length.
const maxLogged = 256

func capped(s string) string {
	if len(s) > maxLogged {
		return s[:maxLogged] + "..."
	}
	return s
}

// Stats is what the proxy forwarded.
type Stats struct {
	// Requests counts the forwarded requests per client.
	Requests map[string]int64
	// Denied counts per client the requests refused for their method or path.
	Denied map[string]int64
	// LastUse is when the last request was forwarded, zero before any.
	LastUse time.Time
	// Unauthorized counts upstream 401s: the account's token was refused.
	Unauthorized int64
}

// Proxy forwards the requests of its allowed clients upstream with the account's token.
type Proxy struct {
	rp  *httputil.ReverseProxy
	log *zap.Logger

	mu sync.Mutex
	// Bearers are kept as their SHA-256: the lookup's timing then says nothing about a bearer.
	clients  map[[32]byte]string
	byClient map[string][32]byte
	// live ends a client's requests in flight when it is revoked.
	live     map[string]context.Context
	stop     map[string]context.CancelFunc
	requests map[string]int64
	denied   map[string]int64
	lastUse  time.Time

	unauthorized atomic.Int64
}

// New builds a proxy to upstream that swaps in src's token. It allows no client until Allow.
func New(upstream *url.URL, src TokenSource, log *zap.Logger) *Proxy {
	p := &Proxy{log: log, clients: map[[32]byte]string{}, byClient: map[string][32]byte{}, requests: map[string]int64{},
		denied: map[string]int64{}, live: map[string]context.Context{}, stop: map[string]context.CancelFunc{}}
	p.rp = &httputil.ReverseProxy{
		FlushInterval: -1, // SSE streaming
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.Out.Host = upstream.Host
			tok, err := src.Get(pr.In.Context())
			if err != nil {
				// Forwarded without a bearer: upstream answers 401, which the turn reports.
				log.Error("token_read_failed", zap.Error(err))
				pr.Out.Header.Del("Authorization")
				return
			}
			if tok.Expired(time.Now()) {
				// ponytail: forwards anyway and lets upstream answer 401; Claude Code owns the refresh.
				log.Warn("token_expired", zap.Time("expires_at", tok.ExpiresAt))
			}
			pr.Out.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode == http.StatusUnauthorized {
				p.unauthorized.Add(1)
				src.Invalidate()
			}
			log.Info("upstream_response", append([]zap.Field{
				zap.String("method", resp.Request.Method),
				zap.String("path", resp.Request.URL.Path),
				zap.Int("status", resp.StatusCode),
			}, headerSummary(resp.Request.Header)...)...)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Error("upstream_error", zap.String("method", r.Method), zap.String("path", r.URL.Path), zap.Error(err))
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	return p
}

func hashBearer(b string) [32]byte { return sha256.Sum256([]byte("Bearer " + b)) }

// Allow lets client in with bearer, replacing the client's previous bearer.
func (p *Proxy) Allow(client, bearer string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if old, ok := p.byClient[client]; ok {
		delete(p.clients, old)
	}
	h := hashBearer(bearer)
	p.clients[h], p.byClient[client] = client, h
	if p.live[client] == nil {
		p.live[client], p.stop[client] = context.WithCancel(context.Background()) //nolint:gosec // Revoke calls it
	}
}

// Revoke shuts client out: its next request is refused.
func (p *Proxy) Revoke(client string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.byClient[client]; ok {
		delete(p.clients, h)
		delete(p.byClient, client)
	}
	if stop := p.stop[client]; stop != nil {
		stop()
		delete(p.stop, client)
		delete(p.live, client)
	}
}

// Stats returns a snapshot of the counters.
func (p *Proxy) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{Requests: maps.Clone(p.requests), Denied: maps.Clone(p.denied), LastUse: p.lastUse, Unauthorized: p.unauthorized.Load()}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := sha256.Sum256([]byte(r.Header.Get("Authorization")))
	// EscapedPath, matched exactly: dot segments, doubled or encoded slashes never match, and the
	// path forwarded is the one checked. The query string is not part of it.
	path := r.URL.EscapedPath()
	pathOK := allowed[r.Method+" "+path]
	p.mu.Lock()
	client, ok := p.clients[h]
	var live context.Context
	if ok && !pathOK {
		p.denied[client]++
	}
	if ok && pathOK {
		p.requests[client]++
		p.lastUse = time.Now()
		live = p.live[client]
	}
	p.mu.Unlock()
	// Security: only an allowed client's sandbox may spend the account's token.
	if !ok {
		p.log.Warn("bearer_rejected", append([]zap.Field{
			zap.String("method", r.Method), zap.String("path", r.URL.Path),
		}, headerSummary(r.Header)...)...)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !pathOK {
		p.log.Warn("proxy_path_denied", zap.String("method", capped(r.Method)), zap.String("path", capped(path)), zap.String("worker", client))
		http.Error(w, "forbidden: this proxy forwards only Claude Code's model calls", http.StatusForbidden)
		return
	}
	// A revoked client's streams end now, not when the model stops talking (003-AC15).
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	unhook := context.AfterFunc(live, cancel)
	defer unhook()
	p.rp.ServeHTTP(w, r.WithContext(ctx))
}

func headerSummary(h http.Header) []zap.Field {
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, strings.ToLower(k))
	}
	sort.Strings(names)
	fields := []zap.Field{zap.Strings("header_names", names)}
	for _, k := range loggedValues {
		if v := h.Values(k); len(v) > 0 {
			fields = append(fields, zap.String(k, strings.Join(v, ",")))
		}
	}
	return fields
}
