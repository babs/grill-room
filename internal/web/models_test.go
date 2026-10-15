package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/babs/grill-room/internal/devicestt"
)

// modelFiles is a tiny stand-in for the pinned manifest, one file per served type.
var modelFiles = map[string]string{
	"enc.onnx": "onnx bytes", "rt.wasm": "\x00asm", "rt.mjs": "export {};\n", "vocab.txt": "a\nb\n",
}

// modelDir writes modelFiles to a directory and returns it with their manifest.
func modelDir(t *testing.T) (string, *devicestt.Manifest) {
	t.Helper()
	dir := t.TempDir()
	m := &devicestt.Manifest{}
	for _, name := range []string{"enc.onnx", "rt.wasm", "rt.mjs", "vocab.txt"} {
		b := []byte(modelFiles[name])
		sum := sha256.Sum256(b)
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
		m.Files = append(m.Files, devicestt.File{Name: name, Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:]), URL: "https://example.com/" + name})
	}
	return dir, m
}

// newModelEnv is a voice server with dir as its model directory, logging to the returned observer.
func newModelEnv(t *testing.T, dir string, m *devicestt.Manifest) (*env, *observer.ObservedLogs) {
	t.Helper()
	opts := testOptions
	opts.Voice = &Voice{URL: "wss://livekit.example", Key: "APIkey", Secret: lkSecret, STTTokenHash: HashToken(sttToken)}
	opts.DeviceSTTDir, opts.DeviceSTTManifest = dir, m
	core, logs := observer.New(zap.InfoLevel)
	return newEnvLog(t, opts, zap.New(core)), logs
}

// rawGet sends path as is: a dotted or encoded path reaches the server unchanged.
func rawGet(t *testing.T, e *env, method, path string, hdr map[string]string) (int, http.Header, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, e.ts.URL+path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultTransport.RoundTrip(req) // no redirect following
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header, b
}

// sttDevice reads the room's first stt event and returns its raw device field.
func sttDevice(t *testing.T, e *env) json.RawMessage {
	t.Helper()
	room := e.createRoom(t, "R")
	p := e.join(t, e.createInvite(t, room.ID).Token, "Alice")
	ev := nextEvent(t, e.openEvents(t, room.ID, p.Token, ""), "stt")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(ev.data), &raw); err != nil {
		t.Fatal(err)
	}
	return raw["device"]
}

// 010-AC5: a complete directory is offered in the stt event and its files served with immutable
// caching, same-origin CORP, the worker CSP and their own content type.
func TestModelsServed(t *testing.T) {
	dir, m := modelDir(t)
	e, logs := newModelEnv(t, dir, m)
	if logs.FilterMessage("device_stt_offered").Len() != 1 {
		t.Fatalf("logs %v", logs.All())
	}
	var offer devicestt.Offer
	if err := json.Unmarshal(sttDevice(t, e), &offer); err != nil {
		t.Fatal(err)
	}
	rev := m.Revision()
	if offer.Revision != rev || offer.Bytes != 10+4+11+4 || len(offer.Files) != 4 ||
		offer.Files[3] != (devicestt.OfferFile{Name: "vocab.txt", Size: 4, Integrity: m.Files[3].Integrity()}) ||
		!strings.HasPrefix(offer.Files[3].Integrity, "sha256-") {
		t.Fatalf("offer %+v", offer)
	}
	for name, ctype := range map[string]string{
		"enc.onnx": "application/octet-stream", "rt.wasm": "application/wasm",
		"rt.mjs": "text/javascript; charset=utf-8", "vocab.txt": "text/plain; charset=utf-8",
	} {
		code, hdr, b := rawGet(t, e, "GET", "/models/"+rev+"/"+name, nil)
		want := map[string]string{
			"Content-Type":                 ctype,
			"Cache-Control":                "public, max-age=31536000, immutable",
			"Cross-Origin-Resource-Policy": "same-origin",
			"Cross-Origin-Embedder-Policy": "credentialless",
			"Content-Security-Policy":      "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; worker-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'",
		}
		if code != http.StatusOK || string(b) != modelFiles[name] {
			t.Fatalf("%s: %d %q", name, code, b)
		}
		for k, v := range want {
			if got := hdr.Get(k); got != v {
				t.Errorf("%s %s: %q, want %q", name, k, got, v)
			}
		}
	}
	code, _, b := rawGet(t, e, "GET", "/models/"+rev+"/enc.onnx", map[string]string{"Range": "bytes=0-3"})
	if code != http.StatusPartialContent || string(b) != "onnx" {
		t.Fatalf("range: %d %q", code, b)
	}
	code, hdr, b := rawGet(t, e, "HEAD", "/models/"+rev+"/enc.onnx", nil)
	if code != http.StatusOK || len(b) != 0 || hdr.Get("Content-Length") != "10" {
		t.Fatalf("head: %d %v", code, hdr)
	}
	if code, _, _ := rawGet(t, e, "POST", "/models/"+rev+"/enc.onnx", nil); code != http.StatusMethodNotAllowed {
		t.Fatalf("post: %d", code)
	}
}

// 010-AC5: anything but a manifest file of the current revision is a 404, never a redirect.
func TestModelsNotFound(t *testing.T) {
	dir, m := modelDir(t)
	// A file in the directory but not in the manifest, and one beside it.
	if err := os.WriteFile(filepath.Join(dir, "extra.onnx"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "secret.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, _ := newModelEnv(t, dir, m)
	rev := m.Revision()
	for _, path := range []string{
		"/models/000000000000/vocab.txt",
		"/models/" + rev + "/extra.onnx",
		"/models/" + rev + "/sub",
		"/models/" + rev + "/",
		"/models/" + rev,
		"/models/",
		"/models/" + rev + "/../secret.txt",
		"/models/" + rev + "/../" + rev + "/vocab.txt",
		"/models/" + rev + "/%2e%2e/secret.txt",
		"/models/" + rev + "/..%2fsecret.txt",
		"/models/" + rev + "/%2e%2e%2fsecret.txt",
		"/models/" + rev + "/sub/../vocab.txt",
		"/models/" + rev + "/vocab.txt/",
		"/models/" + rev + "/VOCAB.TXT",
		"/models//" + rev + "/vocab.txt",
	} {
		if code, _, b := rawGet(t, e, "GET", path, nil); code != http.StatusNotFound {
			t.Errorf("%s: %d %q", path, code, b)
		}
	}
}

// 010-AC5: a missing file, a wrong hash or no directory offers nothing: no device in the stt
// event, /models/ a 404, and one log line naming the file.
func TestModelsNotOffered(t *testing.T) {
	for name, c := range map[string]struct {
		breakDir func(t *testing.T, dir string)
		unset    bool
		file     string
	}{
		"missing file": {breakDir: func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "rt.wasm")); err != nil {
				t.Fatal(err)
			}
		}, file: "rt.wasm"},
		"wrong hash": {breakDir: func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "vocab.txt"), []byte("a\nc\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, file: "vocab.txt"},
		"a directory in its place": {breakDir: func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "rt.mjs")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(dir, "rt.mjs"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, file: "rt.mjs"},
		"unset": {unset: true},
	} {
		t.Run(name, func(t *testing.T) {
			dir, m := modelDir(t)
			if c.unset {
				dir = ""
			} else {
				c.breakDir(t, dir)
			}
			e, logs := newModelEnv(t, dir, m)
			if got := sttDevice(t, e); got != nil {
				t.Fatalf("device %s", got)
			}
			if code, _, _ := rawGet(t, e, "GET", "/models/"+m.Revision()+"/enc.onnx", nil); code != http.StatusNotFound {
				t.Fatalf("model: %d", code)
			}
			lines := logs.FilterMessage("device_stt_unavailable").All()
			if c.unset {
				if len(lines) != 0 {
					t.Fatalf("logs %v", lines)
				}
				return
			}
			if len(lines) != 1 || !strings.Contains(lines[0].ContextMap()["error"].(string), c.file) {
				t.Fatalf("logs %v", logs.All())
			}
		})
	}
}

// 010-AC4: the room page and its docs window are isolated and may compile WebAssembly in workers;
// what they frame embeds under COEP; the transcription workers get their own policy.
func TestIsolationHeaders(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/r/some-room", "/r/some-room/docs", "/r/some-room/docs/1"} {
		code, hdr, _ := rawGet(t, e, "GET", path, nil)
		csp := hdr.Get("Content-Security-Policy")
		if code != http.StatusOK || hdr.Get("Cross-Origin-Opener-Policy") != "same-origin" ||
			hdr.Get("Cross-Origin-Embedder-Policy") != "credentialless" ||
			!strings.Contains(csp, "' 'wasm-unsafe-eval'; ") || !strings.HasSuffix(csp, "; worker-src 'self'") {
			t.Errorf("%s: %d %v", path, code, hdr)
		}
	}
	room := e.createRoom(t, "R")
	p := e.join(t, e.createInvite(t, room.ID).Token, "Alice")
	doc := e.uploadDoc(t, room.ID, p.Token, "notes.md", []byte("# Notes\n"))
	pdf := e.uploadDoc(t, room.ID, p.Token, "a.pdf", []byte("%PDF-1.4\n%%EOF\n"))
	docs := "/api/rooms/" + room.ID + "/documents/"
	for _, path := range []string{docs + itoa(doc.ID) + "/view", docs + itoa(pdf.ID) + "/original", docs + "999/view"} {
		code, hdr := e.getHeaders(t, path, p.Token)
		if hdr.Get("Cross-Origin-Embedder-Policy") != "credentialless" {
			t.Errorf("%s: %d %v", path, code, hdr)
		}
	}
	for path, want := range map[string]string{
		"/static/stt/engine.js": workerCSP,
		"/static/room.js":       "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'",
	} {
		code, hdr, _ := rawGet(t, e, "GET", path, nil)
		if got := hdr.Get("Content-Security-Policy"); got != want || hdr.Get("Cross-Origin-Embedder-Policy") != "credentialless" {
			t.Errorf("%s: %d csp %q", path, code, got)
		}
	}
}

// getHeaders sends an authenticated GET and returns the status and headers.
func (e *env) getHeaders(t *testing.T, path, token string) (int, http.Header) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.ts.URL+path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header
}
