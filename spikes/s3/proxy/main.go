// Bearer-swap reverse proxy: replaces a dummy bearer with the host's real
// Claude Code OAuth access token, read (never written) from the credentials file.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	Version        = "v0.0.0"
	CommitHash     = "0000000"
	BuildTimestamp = "1970-01-01T00:00:00"
	Builder        = "unknown"
	ProjectURL     = "grill-room/spikes/s3/proxy"
)

// Header values safe to log: feature flags and client identity, never credentials.
var loggedValues = []string{"anthropic-beta", "anthropic-version", "user-agent", "x-app"}

type config struct {
	Listen         string
	Upstream       string
	CredsFile      string
	ExpectedBearer string
	LogLevel       string
}

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func (c config) validate() error {
	if c.ExpectedBearer == "" {
		return errors.New("PROXY_EXPECTED_BEARER is required")
	}
	// Security: binding all interfaces would expose the subscription token to the LAN.
	if strings.HasPrefix(c.Listen, ":") || strings.HasPrefix(c.Listen, "0.0.0.0:") {
		return fmt.Errorf("PROXY_LISTEN %q must name a specific address", c.Listen)
	}
	if _, err := url.Parse(c.Upstream); err != nil {
		return fmt.Errorf("PROXY_UPSTREAM: %w", err)
	}
	return nil
}

type tokenSource struct {
	path      string
	mu        sync.Mutex
	mtime     time.Time
	token     string
	expiresAt time.Time
}

// get re-reads the file only when its mtime changed or after invalidate.
func (t *tokenSource) get() (string, time.Time, error) {
	st, err := os.Stat(t.path)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("stat credentials: %w", err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && st.ModTime().Equal(t.mtime) {
		return t.token, t.expiresAt, nil
	}
	raw, err := os.ReadFile(t.path)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read credentials: %w", err)
	}
	var f struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return "", time.Time{}, fmt.Errorf("parse credentials: %w", err)
	}
	if f.ClaudeAiOauth.AccessToken == "" {
		return "", time.Time{}, errors.New("credentials: claudeAiOauth.accessToken empty")
	}
	t.token, t.mtime = f.ClaudeAiOauth.AccessToken, st.ModTime()
	t.expiresAt = time.UnixMilli(f.ClaudeAiOauth.ExpiresAt)
	return t.token, t.expiresAt, nil
}

func (t *tokenSource) invalidate() {
	t.mu.Lock()
	t.token = ""
	t.mu.Unlock()
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

func newHandler(cfg config, ts *tokenSource, log *zap.Logger) (http.Handler, error) {
	up, err := url.Parse(cfg.Upstream)
	if err != nil {
		return nil, fmt.Errorf("parse upstream: %w", err)
	}
	rp := &httputil.ReverseProxy{
		FlushInterval: -1, // SSE streaming
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(up)
			pr.Out.Host = up.Host
			tok, exp, err := ts.get()
			if err != nil {
				log.Error("token_read_failed", zap.Error(err))
				pr.Out.Header.Del("Authorization")
				return
			}
			if time.Now().After(exp) {
				// ponytail: forwards anyway and lets upstream answer 401; the host's Claude Code owns refresh.
				log.Warn("token_expired", zap.Time("expires_at", exp))
			}
			pr.Out.Header.Set("Authorization", "Bearer "+tok)
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode == http.StatusUnauthorized {
				ts.invalidate()
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
	want := []byte("Bearer " + cfg.ExpectedBearer)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Security: only the sandbox holding the dummy bearer may spend the host's token.
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			log.Warn("bearer_rejected", append([]zap.Field{
				zap.String("method", r.Method), zap.String("path", r.URL.Path),
			}, headerSummary(r.Header)...)...)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		rp.ServeHTTP(w, r)
	}), nil
}

func newLogger(level string) (*zap.Logger, error) {
	lvl, err := zapcore.ParseLevel(level)
	if err != nil {
		return nil, fmt.Errorf("log level: %w", err)
	}
	zc := zap.NewProductionConfig()
	if st, err := os.Stdout.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
		zc = zap.NewDevelopmentConfig()
	}
	zc.Level = zap.NewAtomicLevelAt(lvl)
	zc.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	l, err := zc.Build()
	if err != nil {
		return nil, fmt.Errorf("build logger: %w", err)
	}
	return l, nil
}

func run() error {
	home, _ := os.UserHomeDir()
	cfg := config{
		Listen:         getEnv("PROXY_LISTEN", "127.0.0.1:8787"),
		Upstream:       getEnv("PROXY_UPSTREAM", "https://api.anthropic.com"),
		CredsFile:      getEnv("PROXY_CREDENTIALS_FILE", filepath.Join(home, ".claude", ".credentials.json")),
		ExpectedBearer: os.Getenv("PROXY_EXPECTED_BEARER"),
		LogLevel:       getEnv("LOG_LEVEL", "info"),
	}
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	log, err := newLogger(cfg.LogLevel)
	if err != nil {
		return err
	}
	defer func() { _ = log.Sync() }()

	ts := &tokenSource{path: cfg.CredsFile}
	_, exp, err := ts.get()
	if err != nil {
		return err
	}
	log.Info("token_loaded", zap.Time("expires_at", exp), zap.Duration("expires_in", time.Until(exp).Round(time.Second)))
	h, err := newHandler(cfg, ts, log)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      30 * time.Minute, // long streamed turns
		IdleTimeout:       2 * time.Minute,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("proxy_started", zap.String("listen", cfg.Listen), zap.String("upstream", cfg.Upstream), zap.String("version", Version))
	select {
	case err := <-errc:
		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
