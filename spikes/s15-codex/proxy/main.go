// Bearer-swap proxy for a Codex ChatGPT login (spike S15): swaps the sandbox's dummy bearer and
// account id for the host's real ones, read (never written) from <CODEX_HOME>/auth.json on every
// request, so a refresh done by the user's own codex is picked up at once. CONNECT and
// absolute-URI requests are logged and refused: that is how the spike sees which other hosts
// codex tries (auth.openai.com refresh, telemetry).
package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Header values safe to log: client identity and feature flags, never credentials.
var loggedValues = []string{"user-agent", "originator", "version", "openai-beta", "upgrade", "x-codex-turn-metadata"}

type authFile struct {
	Tokens struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	} `json:"tokens"`
}

func readAuth(path string) (tok, account string, err error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the operator's own codex home
	if err != nil {
		return "", "", fmt.Errorf("read %s: %w", path, err)
	}
	var f authFile
	if err := json.Unmarshal(raw, &f); err != nil {
		// The decoder's message can quote the input, which holds the token.
		return "", "", errors.New("auth.json is not valid JSON")
	}
	if f.Tokens.AccessToken == "" {
		return "", "", errors.New("auth.json holds no tokens.access_token")
	}
	return f.Tokens.AccessToken, f.Tokens.AccountID, nil
}

// codex checks that the account id of its auth.json is in this list (workspace routing): the
// sandbox only knows a placeholder id, so the real one is swapped for it on the way back.
const accountsCheck = "/backend-api/wham/accounts/check"

func maskAccount(resp *http.Response, authPath, dummy string) error {
	_, acc, err := readAuth(authPath)
	if err != nil || acc == "" {
		return err
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return fmt.Errorf("read accounts check: %w", err)
	}
	body = bytes.ReplaceAll(body, []byte(acc), []byte(dummy))
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}

func headerFields(h http.Header) []zap.Field {
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, strings.ToLower(k))
	}
	sort.Strings(names)
	f := []zap.Field{zap.Strings("header_names", names)}
	for _, k := range loggedValues {
		if v := h.Values(k); len(v) > 0 {
			s := strings.Join(v, ",")
			if len(s) > 200 {
				s = s[:200] + "..."
			}
			f = append(f, zap.String(k, s))
		}
	}
	return f
}

func main() {
	listen := flag.String("listen", "127.0.0.1:18787", "listen address")
	upstream := flag.String("upstream", "https://chatgpt.com", "upstream origin")
	home, _ := os.UserHomeDir()
	authPath := flag.String("auth", filepath.Join(home, ".codex", "auth.json"), "real auth.json, read only")
	flag.Parse()
	dummy := os.Getenv("S15_DUMMY_BEARER")
	dummyAcc := os.Getenv("S15_DUMMY_ACCOUNT")
	if dummy == "" {
		fmt.Fprintln(os.Stderr, "S15_DUMMY_BEARER is required")
		os.Exit(2)
	}
	up, err := url.Parse(*upstream)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad upstream:", err)
		os.Exit(2)
	}
	log, _ := zap.NewProduction()
	defer func() { _ = log.Sync() }()

	rp := &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(up)
			pr.Out.Host = up.Host
			tok, acc, err := readAuth(*authPath)
			if err != nil {
				log.Error("auth_read_failed", zap.Error(err))
				pr.Out.Header.Del("Authorization")
				return
			}
			pr.Out.Header.Set("Authorization", "Bearer "+tok)
			// The ChatGPT backend binds the token to a workspace through this header; a codex
			// configured as a plain provider (custom-provider mode) does not send it.
			if acc != "" {
				pr.Out.Header.Set("ChatGPT-Account-Id", acc)
			}
			if pr.In.URL.Path == accountsCheck {
				// The body is rewritten below: it must come back uncompressed.
				pr.Out.Header.Del("Accept-Encoding")
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.Request.URL.Path == accountsCheck && dummyAcc != "" {
				if err := maskAccount(resp, *authPath, dummyAcc); err != nil {
					return err
				}
			}
			log.Info("upstream_response", append([]zap.Field{
				zap.String("method", resp.Request.Method), zap.String("path", resp.Request.URL.Path),
				zap.Int("status", resp.StatusCode),
			}, headerFields(resp.Request.Header)...)...)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Error("upstream_error", zap.String("path", r.URL.Path), zap.Error(err))
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	want := []byte("Bearer " + dummy)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Forward-proxy traffic: anything codex sends outside its base URLs.
		if r.Method == http.MethodConnect || r.URL.IsAbs() {
			log.Warn("egress_refused", zap.String("method", r.Method), zap.String("host", r.Host), zap.String("url_path", r.URL.Path))
			http.Error(w, "refused by s15 proxy", http.StatusForbidden)
			return
		}
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			log.Warn("bearer_rejected", append([]zap.Field{zap.String("method", r.Method), zap.String("path", r.URL.Path),
				zap.Bool("has_authorization", len(got) > 0)}, headerFields(r.Header)...)...)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		rp.ServeHTTP(w, r)
	})
	srv := &http.Server{Addr: *listen, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	log.Info("listening", zap.String("addr", *listen), zap.String("upstream", up.String()))
	network, addr := "tcp", *listen
	if a, ok := strings.CutPrefix(*listen, "unix:"); ok {
		// A unix socket reaches a --network none container through a bind mount.
		network, addr = "unix", a
		_ = os.Remove(addr)
	}
	ln, err := net.Listen(network, addr)
	if err != nil {
		log.Fatal("listen", zap.Error(err))
	}
	if err := srv.Serve(ln); err != nil {
		log.Fatal("serve", zap.Error(err))
	}
}
