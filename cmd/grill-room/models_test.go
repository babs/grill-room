package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/devicestt"
)

// vocabHost serves body at /vocab.txt and a 500 anywhere else.
func vocabHost(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vocab.txt" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(ts.Close)
	ts.Client().Timeout = 10 * time.Second
	return ts
}

func vocabManifest(ts *httptest.Server, body []byte, path string) devicestt.Manifest {
	sum := sha256.Sum256(body)
	return devicestt.Manifest{Files: []devicestt.File{{Name: "vocab.txt", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:]), URL: ts.URL + path}}}
}

// 010-AC6: `models fetch --dir` fills the directory; a wrong command or a missing --dir fails.
func TestModelsCommand(t *testing.T) {
	body := []byte("vocab")
	ts := vocabHost(t, body)
	m := vocabManifest(ts, body, "/vocab.txt")
	t.Setenv("LOG_FORMAT", "json")
	dir := t.TempDir()
	if err := command([]string{"models", "fetch", "--dir", dir}, m, ts.Client()); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "vocab.txt")); err != nil || string(b) != "vocab" {
		t.Fatalf("vocab.txt %q %v", b, err)
	}
	for _, args := range [][]string{{"models"}, {"serve"}, {"models", "fetch"}, {"models", "fetch", "--best-effort"},
		{"models", "fetch", "--dir", dir, "extra"}, {"models", "fetch", "--bogus"}} {
		if err := command(args, m, ts.Client()); err == nil {
			t.Errorf("%q succeeded", args)
		}
	}
}

// 010-AC6: a flipped byte or an HTTP 500 fails the fetch, or with --best-effort succeeds; either
// way no file lands under the final name.
func TestModelsBestEffort(t *testing.T) {
	body := []byte("vocab")
	ts := vocabHost(t, body)
	t.Setenv("LOG_FORMAT", "json")
	for name, m := range map[string]devicestt.Manifest{
		"flipped byte": vocabManifest(ts, []byte("vocaB"), "/vocab.txt"),
		"http 500":     vocabManifest(ts, body, "/gone"),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := command([]string{"models", "fetch", "--dir", dir}, m, ts.Client()); err == nil {
				t.Fatal("fetch succeeded")
			}
			if err := command([]string{"models", "fetch", "--best-effort", "--dir", dir}, m, ts.Client()); err != nil {
				t.Fatalf("best effort: %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "vocab.txt")); !os.IsNotExist(err) {
				t.Fatalf("vocab.txt: %v", err)
			}
		})
	}
}
