// Package proxy is the bearer-swap reverse proxy: it replaces the sandbox's dummy bearer with the
// host's real Claude Code OAuth access token, so the sandbox never holds a usable credential.
//
// Derived from the grill-room S3 spike (spikes/s3/proxy/main.go) at commit 3ae0776.
package proxy

import (
	"crypto/subtle"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/creds"
)

// Header values safe to log: feature flags and client identity, never credentials.
var loggedValues = []string{"anthropic-beta", "anthropic-version", "user-agent", "x-app"}

// Proxy forwards authorised sandbox requests upstream with the real token.
type Proxy struct {
	rp   *httputil.ReverseProxy
	want []byte
	log  *zap.Logger
	// unauthorized counts upstream 401s: the caller tells an expired host token from other failures.
	unauthorized atomic.Int64
}

// New builds a proxy to upstream. dummy is the bearer the sandbox holds; any other is refused.
func New(upstream *url.URL, src *creds.Source, dummy string, log *zap.Logger) *Proxy {
	p := &Proxy{want: []byte("Bearer " + dummy), log: log}
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
				// ponytail: forwards anyway and lets upstream answer 401; the host's Claude Code owns refresh.
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

// Unauthorized returns how many upstream 401s the proxy has seen.
func (p *Proxy) Unauthorized() int64 { return p.unauthorized.Load() }

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Security: only the sandbox holding the dummy bearer may spend the host's token.
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), p.want) != 1 {
		p.log.Warn("bearer_rejected", append([]zap.Field{
			zap.String("method", r.Method), zap.String("path", r.URL.Path),
		}, headerSummary(r.Header)...)...)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	p.rp.ServeHTTP(w, r)
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
