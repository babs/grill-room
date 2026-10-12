package daemon

import (
	"bytes"
	"context"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/trust"
	"github.com/babs/grill-room/internal/trust/trusttest"
)

// offlineRoots never reaches TUF: verification runs on the embedded root.
type offline struct{}

func (offline) RoundTrip(*http.Request) (*http.Response, error) { return nil, errors.New("offline") }

// ownExe is this test binary: an executable for the platform the tests run on.
func ownExe(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// otherPlatform is the header of an executable for another platform than this one.
func otherPlatform(t *testing.T) []byte {
	t.Helper()
	if runtime.GOOS+"/"+runtime.GOARCH == "linux/arm64" {
		return headers(t)["linux/amd64"]
	}
	return headers(t)["linux/arm64"]
}

// release is what a fake instance serves for this platform.
type release struct {
	version string
	bin     []byte
	// sum is the published sha256; empty publishes bin's.
	sum    string
	bundle []byte
}

// fakeInstance serves rel; hits counts the binary downloads.
func fakeInstance(t *testing.T, rel *atomic.Pointer[release], hits *atomic.Int32) *httptest.Server {
	t.Helper()
	file := releaseFile(runtime.GOOS, runtime.GOARCH)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := rel.Load()
		switch r.URL.Path {
		case "/api/daemon/version":
			sum := cur.sum
			if sum == "" {
				sum = hexSHA(cur.bin)
			}
			bundles := map[string]string{}
			if cur.bundle != nil {
				bundles[target] = "/download/" + file + bundleExt
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"version": cur.version, "sha256": map[string]string{target: sum}, "bundles": bundles})
		case "/download/" + file:
			if hits != nil {
				hits.Add(1)
			}
			_, _ = w.Write(cur.bin)
		case "/download/" + file + bundleExt:
			if cur.bundle == nil {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(cur.bundle)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func serve(t *testing.T, r release, hits *atomic.Int32) (*httptest.Server, *atomic.Pointer[release]) {
	t.Helper()
	var p atomic.Pointer[release]
	p.Store(&r)
	return fakeInstance(t, &p, hits), &p
}

// testBins is a binary store in a temp dir trusting keys, as a supervisor at v-self.
func testBins(t *testing.T, keys ...*trusttest.Key) *Binaries {
	t.Helper()
	dir := t.TempDir()
	b := &Binaries{Dir: filepath.Join(dir, "bin"), Self: "/self/grill-room-daemon", Version: "v-self", HTTP: &http.Client{},
		Roots: &trust.Roots{Dir: filepath.Join(dir, "trust"), Transport: offline{}, Log: zap.NewNop()},
		Keys:  &DevKeys{Dir: filepath.Join(dir, "dev-keys"), Log: zap.NewNop()}, Log: zap.NewNop()}
	for i, k := range keys {
		if err := b.Keys.Add("k"+string(rune('0'+i)), k.PEM); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

func instanceAt(t *testing.T, url string) Instance {
	t.Helper()
	id, err := InstanceID(url)
	if err != nil {
		t.Fatal(err)
	}
	return Instance{ID: id, URL: url}
}

// 003-AC2: a binary is refused, with its reason, when its bundle is missing, when it was changed
// after signing, when its signer is not trusted, when it is built for another platform, or when
// the download differs from the published digest. Nothing is stored.
func TestResolveRefuses(t *testing.T) {
	trusted, stranger := trusttest.NewKey(t), trusttest.NewKey(t)
	exe := ownExe(t)
	tampered := append(append([]byte{}, exe...), "tampered"...)
	wrongArch := otherPlatform(t)
	s7, err := os.ReadFile("../trust/testdata/s7-blob.txt")
	if err != nil {
		t.Fatal(err)
	}
	s7Bundle, err := os.ReadFile("../trust/testdata/s7-blob.sigstore.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		rel   release
		relax bool
		want  string
	}{
		{"missing bundle", release{version: "v1.2.3", bin: exe}, false, "no signature bundle"},
		{"tampered after signing", release{version: "v1.2.3", bin: tampered, bundle: trusted.Sign(t, exe)}, false, "signature refused"},
		{"untrusted dev key", release{version: "v1.2.3", bin: exe, bundle: stranger.Sign(t, exe)}, false, "no trusted dev key matches"},
		{"another workflow's identity", release{version: "v1.2.3", bin: s7, bundle: s7Bundle}, false, "expected SAN value to match"},
		{"wrong arch", release{version: "v1.2.3", bin: wrongArch, bundle: trusted.Sign(t, wrongArch)}, false, "refused: "},
		{"wrong arch, relaxed", release{version: "v1.2.3", bin: wrongArch}, true, "refused: "},
		{"not a binary at all, relaxed", release{version: "v1.2.3", bin: []byte("#!/bin/sh\necho hi\n")}, true, "executable"},
		{"download differs from the published digest", release{version: "v1.2.3", bin: exe, sum: strings.Repeat("0", 64), bundle: trusted.Sign(t, exe)}, false, "does not match the sha256"},
		{"older than worker mode", release{version: "v0.0.1", bin: exe, bundle: trusted.Sign(t, exe)}, false, "older than v0.0.2"},
		{"not a semver", release{version: "latest", bin: exe}, false, "not a semver"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := testBins(t, trusted)
			srv, _ := serve(t, tc.rel, nil)
			in := instanceAt(t, srv.URL)
			in.Relax = tc.relax
			_, err := b.Resolve(context.Background(), in)
			var refused errRefused
			if !errors.As(err, &refused) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a refusal with %q, got %v", tc.want, err)
			}
			if left, _ := filepath.Glob(filepath.Join(b.Dir, "*", "*")); len(left) != 0 {
				t.Fatalf("stored: %v", left)
			}
			if left, _ := filepath.Glob(filepath.Join(b.Dir, ".dl-*")); len(left) != 0 {
				t.Fatalf("downloads left: %v", left)
			}
		})
	}
}

// 003-AC4, 003-AC7: a binary signed with a trusted dev key is stored once under its version, with
// its bundle, and verified again from disk on later starts, offline included.
func TestResolveDevKeyStoresOnce(t *testing.T) {
	key := trusttest.NewKey(t)
	exe := ownExe(t)
	var hits atomic.Int32
	srv, rel := serve(t, release{version: "v1.2.3", bin: exe, bundle: key.Sign(t, exe)}, &hits)
	b := testBins(t, key)
	in := instanceAt(t, srv.URL)
	r, err := b.Resolve(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(b.Dir, "v1.2.3", releaseFile(runtime.GOOS, runtime.GOARCH))
	if r.Path != want || r.Version != "v1.2.3" || r.Served != "v1.2.3" || r.DevKey != "k0" || r.Relaxed || !strings.Contains(r.Trust, "dev key k0") {
		t.Fatalf("resolved %+v", r)
	}
	if _, err := os.Stat(want + bundleExt); err != nil {
		t.Fatal("bundle not kept:", err)
	}
	if got, _ := os.ReadFile(want); !bytes.Equal(got, exe) { //nolint:gosec // test file
		t.Fatal("stored binary differs")
	}
	if r, err = b.Resolve(context.Background(), in); err != nil || r.Path != want || hits.Load() != 1 {
		t.Fatalf("second resolve: %+v %v, %d downloads", r, err, hits.Load())
	}
	// A rebuild published under the same version replaces the stored copy.
	rebuilt := append(append([]byte{}, exe...), 0)
	rel.Store(&release{version: "v1.2.3", bin: rebuilt, bundle: key.Sign(t, rebuilt)})
	if r, err = b.Resolve(context.Background(), in); err != nil || hits.Load() != 2 {
		t.Fatalf("rebuild: %+v %v, %d downloads", r, err, hits.Load())
	}
	// Offline, the copy that ran last is verified again from disk.
	srv.Close()
	in.Version = "v1.2.3"
	if r, err = b.Resolve(context.Background(), in); err != nil || r.Path != want || r.Served != "" {
		t.Fatalf("offline: %+v %v", r, err)
	}
	// Once its key is gone, the stored copy no longer runs.
	if err := b.Keys.Remove("k0"); err != nil {
		t.Fatal(err)
	}
	if r, err = b.Resolve(context.Background(), in); err == nil && r.Path == want {
		t.Fatalf("offline copy ran with its key removed: %+v", r)
	}
}

// 003-AC7: an instance serving the supervisor's own version, or no release, runs the supervisor's
// binary, and nothing is stored.
func TestResolveOwnVersion(t *testing.T) {
	for _, v := range []string{"v-self", ""} {
		b := testBins(t)
		srv, _ := serve(t, release{version: v, bin: []byte("unused")}, nil)
		r, err := b.Resolve(context.Background(), instanceAt(t, srv.URL))
		if err != nil || r.Path != b.Self || r.Version != "v-self" {
			t.Fatalf("version %q: %+v %v", v, r, err)
		}
		if _, err := os.Stat(b.Dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("version %q: bin dir created", v)
		}
	}
}

// 003-AC4: with the relax toggle on, an unsigned or badly signed binary runs, marked as such.
func TestResolveRelaxed(t *testing.T) {
	exe := ownExe(t)
	for _, rel := range []release{{version: "v1.2.3", bin: exe}, {version: "v1.2.3", bin: exe, bundle: trusttest.NewKey(t).Sign(t, exe)}} {
		b := testBins(t)
		srv, _ := serve(t, rel, nil)
		in := instanceAt(t, srv.URL)
		in.Relax = true
		r, err := b.Resolve(context.Background(), in)
		if err != nil || !r.Relaxed || !strings.Contains(r.Trust, "accepts unsigned") {
			t.Fatalf("relaxed: %+v %v", r, err)
		}
	}
}

// 003-AC7: a version no instance uses is removed, at start and when an instance changes version;
// one still used by another instance stays. A copy of the supervisor's own version is not kept:
// the supervisor's binary serves it.
func TestBinariesGC(t *testing.T) {
	b := testBins(t)
	for _, v := range []string{"v1.0.0", "v2.0.0", "v3.0.0", "v-self"} {
		if err := os.MkdirAll(filepath.Join(b.Dir, v), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(b.Dir, ".dl-123"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	present := func() []string {
		var out []string
		entries, _ := os.ReadDir(b.Dir)
		for _, e := range entries {
			out = append(out, e.Name())
		}
		return out
	}
	b.Init([]Instance{{ID: "a", Version: "v1.0.0"}, {ID: "b", Version: "v2.0.0"}, {ID: "c", Version: "v-self"}})
	if got := strings.Join(present(), ","); got != "v1.0.0,v2.0.0" {
		t.Fatalf("after start: %s", got)
	}
	b.use("b", "v1.0.0")
	if got := strings.Join(present(), ","); got != "v1.0.0" {
		t.Fatalf("after b moved to v1.0.0: %s", got)
	}
	b.own("a", "v-self")
	if got := strings.Join(present(), ","); got != "v1.0.0" {
		t.Fatalf("v1.0.0 still used by b: %s", got)
	}
	b.own("b", "v-self")
	if got := present(); len(got) != 0 {
		t.Fatalf("no version used: %v", got)
	}
}

// 003-AC4: dev keys are PEM public keys under a plain name; garbage and duplicates are refused.
func TestDevKeys(t *testing.T) {
	k := &DevKeys{Dir: t.TempDir(), Log: zap.NewNop()}
	key := trusttest.NewKey(t)
	for _, bad := range []struct{ name, pem string }{
		{"../escape", string(key.PEM)}, {"", string(key.PEM)}, {".hidden", string(key.PEM)}, {"ok", "not a key"},
	} {
		if err := k.Add(bad.name, []byte(bad.pem)); err == nil {
			t.Fatalf("accepted %q", bad.name)
		}
	}
	if err := k.Add("laptop", key.PEM); err != nil {
		t.Fatal(err)
	}
	if err := k.Add("laptop", key.PEM); err == nil {
		t.Fatal("duplicate accepted")
	}
	// A key dropped in the directory by hand counts too; a broken file is skipped.
	if err := os.WriteFile(filepath.Join(k.Dir, "hand.pub"), trusttest.NewKey(t).PEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(k.Dir, "broken.pub"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range k.List() {
		names = append(names, d.Name)
	}
	if strings.Join(names, ",") != "hand,laptop" {
		t.Fatalf("keys %v", names)
	}
	if err := k.Remove("laptop"); err != nil || k.Remove("laptop") == nil || k.Remove("../x") == nil {
		t.Fatal("remove")
	}
}

// The terminal never receives a worker's control characters, C1 codes or bidi overrides, and a
// worker cannot pose as another instance (L2 from phase 2).
func TestSafeLogLine(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry any
		bad   string
	}{
		{"escape in a JSON string", json.RawMessage(`{"msg":"\u001b[2J"}`), "\x1b"},
		{"raw C1 CSI", json.RawMessage("{\"msg\":\"\u009b31m\"}"), "\u009b"},
		{"bidi override in a key", json.RawMessage("{\"\u202e\":\"x\"}"), "\u202e"},
		{"not JSON", json.RawMessage("plain \u2066text"), "\u2066"},
		{"stderr line with ESC", map[string]any{"stream": "stderr", "msg": "\x1b]0;title\x07"}, "\x1b"},
		{"tag character outside the BMP", map[string]any{"msg": "a\U000E0041b"}, "\U000E0041"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := safeLogLine("inst", tc.entry)
			if strings.Contains(string(out), tc.bad) || bytes.ContainsAny(out, "\n\r") {
				t.Fatalf("unsafe output %q", out)
			}
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil || m["instance"] != "inst" {
				t.Fatalf("output %q: %v", out, err)
			}
		})
	}
	if out := safeLogLine("real", json.RawMessage(`{"instance":"other","msg":"x"}`)); !strings.Contains(string(out), `"instance":"real"`) {
		t.Fatalf("instance spoofed: %s", out)
	}
	if out := safeLogLine("i", json.RawMessage(`{"msg":"`+strings.Repeat("a", maxLogLine)+`"}`)); !strings.Contains(string(out), "worker_log_dropped") {
		t.Fatalf("huge line: %d bytes", len(out))
	}
}

// headers returns minimal executable headers, enough for debug/elf, debug/macho and debug/pe.
func headers(t *testing.T) map[string][]byte {
	t.Helper()
	w := func(v any) []byte {
		var buf bytes.Buffer
		if err := binary.Write(&buf, binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	elfFor := func(m elf.Machine) []byte {
		h := elf.Header64{Type: uint16(elf.ET_EXEC), Machine: uint16(m), Version: 1, Ehsize: 64}
		copy(h.Ident[:], []byte{0x7f, 'E', 'L', 'F', byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)})
		return w(h)
	}
	machoFor := func(c macho.Cpu) []byte {
		return append(w(macho.FileHeader{Magic: macho.Magic64, Cpu: c, Type: macho.TypeExec}), 0, 0, 0, 0)
	}
	peFor := func(m uint16) []byte {
		dos := make([]byte, 0x40)
		copy(dos, "MZ")
		binary.LittleEndian.PutUint32(dos[0x3c:], 0x40)
		return append(append(append(dos, "PE\x00\x00"...), w(pe.FileHeader{Machine: m})...), make([]byte, 8)...)
	}
	return map[string][]byte{
		"linux/amd64": elfFor(elf.EM_X86_64), "linux/arm64": elfFor(elf.EM_AARCH64),
		"darwin/amd64": machoFor(macho.CpuAmd64), "darwin/arm64": machoFor(macho.CpuArm64),
		"windows/amd64": peFor(pe.IMAGE_FILE_MACHINE_AMD64), "windows/arm64": peFor(pe.IMAGE_FILE_MACHINE_ARM64),
	}
}

// 003-AC2: a binary runs only on the OS and architecture it was built for; a signature covers its
// bytes, not the platform it was published for.
func TestCheckPlatform(t *testing.T) {
	dir := t.TempDir()
	hs := headers(t)
	for built, b := range hs {
		path := filepath.Join(dir, strings.ReplaceAll(built, "/", "-"))
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		for want := range hs {
			goos, goarch, _ := strings.Cut(want, "/")
			if err := checkPlatform(path, goos, goarch); (err == nil) != (built == want) {
				t.Errorf("built for %s, running on %s: %v", built, want, err)
			}
		}
	}
	if err := checkPlatform(filepath.Join(dir, "missing"), "linux", "amd64"); err == nil {
		t.Error("missing file accepted")
	}
	if err := checkPlatform(filepath.Join(dir, "linux-amd64"), "plan9", "amd64"); err == nil {
		t.Error("unknown OS accepted")
	}
}

func TestLineWriter(t *testing.T) {
	var lines []string
	w := &lineWriter{emit: func(b []byte) { lines = append(lines, string(b)) }}
	_, _ = w.Write([]byte("a\nb"))
	_, _ = w.Write([]byte("c\n"))
	_, _ = w.Write(bytes.Repeat([]byte("x"), maxLogLine+10))
	if len(lines) != 3 || lines[0] != "a" || lines[1] != "bc" || len(lines[2]) != maxLogLine {
		t.Fatalf("lines %q", lines)
	}
}
