package devicestt

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func tgz(t *testing.T, members map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(members))
	for n := range members {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(members[n])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(members[n])
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeHost serves files by path and counts the requests per path. With gate set, it sends the
// first byte of /model.onnx, closes inFlight and waits for gate before the rest.
type fakeHost struct {
	mu       sync.Mutex
	files    map[string][]byte
	hits     map[string]int
	gate     chan struct{}
	inFlight chan struct{}
}

func (f *fakeHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	b, ok := f.files[r.URL.Path]
	f.hits[r.URL.Path]++
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if f.gate != nil && r.URL.Path == "/model.onnx" {
		_, _ = w.Write(b[:1])
		w.(http.Flusher).Flush()
		close(f.inFlight)
		<-f.gate
		b = b[1:]
	}
	_, _ = w.Write(b)
}

func (f *fakeHost) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.hits {
		n += c
	}
	return n
}

// fakeManifest is a manifest of two direct files and two archive members on a test server.
func fakeManifest(t *testing.T) (Manifest, *fakeHost, *httptest.Server) {
	t.Helper()
	model, vocab := []byte("model bytes"), []byte("a\nb\n")
	rt, wasm := []byte("export const ort = 1;\n"), []byte("\x00asm wasm")
	arc := tgz(t, map[string][]byte{"package/dist/rt.mjs": rt, "package/dist/rt.wasm": wasm, "package/README.md": []byte("x")})
	host := &fakeHost{files: map[string][]byte{"/model.onnx": model, "/vocab.txt": vocab, "/pkg.tgz": arc}, hits: map[string]int{}}
	ts := httptest.NewServer(host)
	t.Cleanup(ts.Close)
	ts.Client().Timeout = 10 * time.Second
	a := &Archive{URL: ts.URL + "/pkg.tgz", Size: int64(len(arc)), SHA256: sum(arc)}
	return Manifest{Files: []File{
		{Name: "model.onnx", Size: int64(len(model)), SHA256: sum(model), URL: ts.URL + "/model.onnx"},
		{Name: "vocab.txt", Size: int64(len(vocab)), SHA256: sum(vocab), URL: ts.URL + "/vocab.txt"},
		{Name: "rt.mjs", Size: int64(len(rt)), SHA256: sum(rt), Archive: a, Member: "package/dist/rt.mjs"},
		{Name: "rt.wasm", Size: int64(len(wasm)), SHA256: sum(wasm), Archive: a, Member: "package/dist/rt.wasm"},
	}}, host, ts
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		if e.Name() != ".fetch.lock" {
			out = append(out, e.Name())
		}
	}
	return out
}

// 010-AC6: every file lands under its name with its hash; a second run downloads nothing.
func TestFetchTwice(t *testing.T) {
	m, host, ts := fakeManifest(t)
	// A directory that does not exist yet, as on a first CI cache miss.
	dir := filepath.Join(t.TempDir(), "sub")
	if err := m.Fetch(t.Context(), dir, ts.Client(), zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	if err := m.Check(dir); err != nil {
		t.Fatal(err)
	}
	if got := names(t, dir); !slices.Equal(got, []string{"model.onnx", "rt.mjs", "rt.wasm", "vocab.txt"}) {
		t.Fatalf("dir holds %v", got)
	}
	if n := host.total(); n != 3 {
		t.Fatalf("first run: %d requests, want 3 (the archive once)", n)
	}
	if err := m.Fetch(t.Context(), dir, ts.Client(), zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	if n := host.total(); n != 3 {
		t.Fatalf("second run sent %d more requests, want none", n-3)
	}
}

// 010-AC6: two runs on one directory both succeed; neither sweeps the other's in-flight file.
func TestFetchConcurrent(t *testing.T) {
	m, _, ts := fakeManifest(t)
	dir := t.TempDir()
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- m.Fetch(t.Context(), dir, ts.Client(), zap.NewNop()) }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Check(dir); err != nil {
		t.Fatal(err)
	}
}

// 010-AC6: only what is missing or wrong is fetched again; a member alone refetches its archive.
func TestFetchRepairs(t *testing.T) {
	m, host, ts := fakeManifest(t)
	dir := t.TempDir()
	if err := m.Fetch(t.Context(), dir, ts.Client(), zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rt.wasm"), []byte("\x00asm wasX"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "vocab.txt")); err != nil {
		t.Fatal(err)
	}
	// A temporary file a killed run left behind.
	if err := os.WriteFile(filepath.Join(dir, ".fetch-123"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Fetch(t.Context(), dir, ts.Client(), zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	if err := m.Check(dir); err != nil {
		t.Fatal(err)
	}
	if host.hits["/pkg.tgz"] != 2 || host.hits["/vocab.txt"] != 2 || host.hits["/model.onnx"] != 1 {
		t.Fatalf("hits %v", host.hits)
	}
	if got := names(t, dir); len(got) != 4 {
		t.Fatalf("dir holds %v", got)
	}
}

// 010-AC6: a flipped byte, an oversize body, a tampered archive or a missing member fail the run
// and leave no final file and no temporary file.
func TestFetchRefuses(t *testing.T) {
	for name, tamper := range map[string]func(m *Manifest, host *fakeHost) string{
		"flipped byte": func(_ *Manifest, host *fakeHost) string {
			host.files["/model.onnx"] = []byte("model bytez")
			return "model.onnx"
		},
		"oversize": func(_ *Manifest, host *fakeHost) string {
			host.files["/vocab.txt"] = []byte("a\nb\nc\n")
			return "vocab.txt"
		},
		"short": func(_ *Manifest, host *fakeHost) string {
			host.files["/vocab.txt"] = []byte("a\n")
			return "vocab.txt"
		},
		"tampered archive": func(_ *Manifest, host *fakeHost) string {
			b := bytes.Clone(host.files["/pkg.tgz"])
			b[len(b)-1] ^= 1
			host.files["/pkg.tgz"] = b
			return "rt.mjs"
		},
		"member hash": func(m *Manifest, _ *fakeHost) string {
			m.Files[3].SHA256 = sum([]byte("other"))
			return "rt.wasm"
		},
		"member missing": func(m *Manifest, _ *fakeHost) string {
			m.Files[3].Member = "package/dist/gone.wasm"
			return "rt.wasm"
		},
		"not found": func(m *Manifest, _ *fakeHost) string {
			m.Files[0].URL += ".gone"
			return "model.onnx"
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, host, ts := fakeManifest(t)
			bad := tamper(&m, host)
			dir := t.TempDir()
			err := m.Fetch(t.Context(), dir, ts.Client(), zap.NewNop())
			if err == nil {
				t.Fatal("fetch succeeded")
			}
			for _, n := range names(t, dir) {
				if n == bad || strings.HasPrefix(n, ".fetch-") {
					t.Fatalf("%v: dir holds %v", err, names(t, dir))
				}
			}
		})
	}
}

// 010-AC6: a file already there with the wrong hash does not survive a failed download.
func TestFetchDropsWrongFile(t *testing.T) {
	m, host, ts := fakeManifest(t)
	host.files["/model.onnx"] = []byte("model bytez")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "model.onnx"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Fetch(t.Context(), dir, ts.Client(), zap.NewNop()); err == nil {
		t.Fatal("fetch succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir, "model.onnx")); !os.IsNotExist(err) {
		t.Fatalf("model.onnx still there: %v", err)
	}
}

// 010-AC5: Check names the file that is missing or wrong.
func TestCheck(t *testing.T) {
	m, _, ts := fakeManifest(t)
	dir := t.TempDir()
	if err := m.Fetch(t.Context(), dir, ts.Client(), zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vocab.txt"), []byte("a\nc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Check(dir); err == nil || !strings.Contains(err.Error(), "vocab.txt") {
		t.Fatalf("wrong hash: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "vocab.txt")); err != nil {
		t.Fatal(err)
	}
	if err := m.Check(dir); err == nil || !strings.Contains(err.Error(), "vocab.txt") {
		t.Fatalf("missing: %v", err)
	}
}

// 010-AC5: the revision follows every pin, and the offer carries base64 sha256 integrity values.
func TestRevisionAndOffer(t *testing.T) {
	rev := Pinned.Revision()
	if len(rev) != 12 {
		t.Fatalf("revision %q", rev)
	}
	for i, change := range []func(f *File){
		func(f *File) { f.SHA256 = strings.Repeat("0", 64) },
		func(f *File) { f.Size++ },
		func(f *File) { f.URL += "x" },
		func(f *File) { f.Name += "x" },
	} {
		m := Manifest{Files: slices.Clone(Pinned.Files)}
		change(&m.Files[0])
		if m.Revision() == rev {
			t.Errorf("change %d kept the revision", i)
		}
	}
	m := Manifest{Files: slices.Clone(Pinned.Files)}
	a := *m.Files[3].Archive
	a.SHA256 = strings.Repeat("0", 64)
	m.Files[3].Archive = &a
	if m.Revision() == rev {
		t.Error("an archive pin change kept the revision")
	}
	o := Pinned.Offer()
	if o.Revision != rev || len(o.Files) != 7 || o.Bytes != 383461215+18203490+93939+819537+46851+28312028+1245151 {
		t.Fatalf("offer %+v", o)
	}
	if got := o.Files[2]; got.Name != "vocab.txt" || got.Integrity != "sha256-1YVEZ56kvGrFY9H1Ret9R0vWz6Rn8KbiwdwcfTfjw10=" {
		t.Fatalf("vocab offer %+v", got)
	}
	if _, ok := Pinned.Lookup("vocab.txt"); !ok {
		t.Fatal("lookup vocab.txt")
	}
	if _, ok := Pinned.Lookup("../vocab.txt"); ok {
		t.Fatal("lookup ../vocab.txt")
	}
}
