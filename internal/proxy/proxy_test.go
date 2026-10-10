package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/babs/grill-room/internal/creds"
)

func writeCreds(t *testing.T, dir, tok string, mtime time.Time) {
	t.Helper()
	path := filepath.Join(dir, ".credentials.json")
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
	upURL, _ := url.Parse(up.URL)

	dir := t.TempDir()
	t0 := time.Unix(1_000_000, 0)
	writeCreds(t, dir, "real-1", t0)
	core, logs := observer.New(zap.DebugLevel)
	p := New(upURL, creds.NewSource(creds.Reader{ConfigDir: dir}), "dummy", zap.New(core))
	px := httptest.NewServer(p)
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
		{"real token as bearer", nil, "Bearer real-1", 403, ""},
		{"file changed reloads", func() { writeCreds(t, dir, "real-2", t0.Add(time.Second)) }, "Bearer dummy", 200, "Bearer real-2"},
		// Same mtime: only the 401 invalidation can surface the new token.
		{"401 invalidates", func() { upstreamStatus = 401 }, "Bearer dummy", 401, "Bearer real-2"},
		{"after 401 rereads same mtime", func() {
			upstreamStatus = 200
			writeCreds(t, dir, "real-3", t0.Add(time.Second))
		}, "Bearer dummy", 200, "Bearer real-3"},
		{"unreadable creds forward no bearer", func() { _ = os.Remove(filepath.Join(dir, ".credentials.json")) }, "Bearer dummy", 200, ""},
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
	if p.Unauthorized() != 1 {
		t.Fatalf("Unauthorized() = %d, want 1", p.Unauthorized())
	}
	// No log line may carry a real token, whatever field it would hide in.
	if logs.Len() == 0 {
		t.Fatal("no logs captured")
	}
	for _, e := range logs.All() {
		line := fmt.Sprint(e.Message, e.ContextMap())
		if strings.Contains(line, "real-") {
			t.Fatalf("token logged: %s", line)
		}
	}
}
