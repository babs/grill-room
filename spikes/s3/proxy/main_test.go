package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
)

func writeCreds(t *testing.T, path, tok string, mtime time.Time) {
	t.Helper()
	body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":%d}}`, tok, time.Now().Add(time.Hour).UnixMilli())
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestProxy(t *testing.T) {
	var gotAuth string
	upstreamStatus := http.StatusOK
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(upstreamStatus)
	}))
	defer up.Close()

	creds := filepath.Join(t.TempDir(), "creds.json")
	t0 := time.Unix(1_000_000, 0)
	writeCreds(t, creds, "real-1", t0)

	ts := &tokenSource{path: creds}
	h, err := newHandler(config{Upstream: up.URL, ExpectedBearer: "dummy"}, ts, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	px := httptest.NewServer(h)
	defer px.Close()

	tests := []struct {
		name       string
		setup      func()
		auth       string
		wantStatus int
		wantUp     string // Authorization seen upstream, "" = request must not reach upstream
	}{
		{"swap", nil, "Bearer dummy", 200, "Bearer real-1"},
		{"no bearer", nil, "", 403, ""},
		{"wrong bearer", nil, "Bearer other", 403, ""},
		{"api key shape", nil, "real-1", 403, ""},
		{"file changed reloads", func() { writeCreds(t, creds, "real-2", t0.Add(time.Second)) }, "Bearer dummy", 200, "Bearer real-2"},
		// Same mtime: only the 401 invalidation can surface the new token.
		{"401 invalidates", func() { upstreamStatus = 401 }, "Bearer dummy", 401, "Bearer real-2"},
		{"after 401 rereads same mtime", func() {
			upstreamStatus = 200
			writeCreds(t, creds, "real-3", t0.Add(time.Second))
		}, "Bearer dummy", 200, "Bearer real-3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup()
			}
			gotAuth = ""
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, px.URL+"/v1/messages", http.NoBody)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tc.wantStatus || gotAuth != tc.wantUp {
				t.Fatalf("status=%d upstreamAuth=%q, want %d %q", resp.StatusCode, gotAuth, tc.wantStatus, tc.wantUp)
			}
		})
	}
}

func TestValidateListen(t *testing.T) {
	for _, l := range []string{":8787", "0.0.0.0:8787"} {
		if err := (config{Listen: l, ExpectedBearer: "x"}).validate(); err == nil {
			t.Errorf("listen %q accepted", l)
		}
	}
}
