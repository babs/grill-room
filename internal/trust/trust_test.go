package trust

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"
	"go.uber.org/zap"
)

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // test data
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func digestOf(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// releaseDigest is the sha256 of v0.0.1's linux-amd64 binary, as its release publishes it: the
// bundle is verified against the digest, so the 8 MB binary stays out of the repository.
func releaseDigest(t *testing.T) []byte {
	t.Helper()
	sum, _, _ := strings.Cut(string(read(t, "v0.0.1-linux-amd64.sha256")), " ")
	d, err := hex.DecodeString(sum)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func embedded(t *testing.T) *root.TrustedRoot {
	t.Helper()
	tm, err := root.NewTrustedRootFromJSON(embeddedRoot)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func key(t *testing.T, name string) DevKey {
	t.Helper()
	k, err := ParseDevKey(strings.TrimSuffix(name, ".pub"), read(t, name))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// 003-AC2, 003-AC3: the real v0.0.1 release bundle verifies offline against the embedded root as
// a project release; a tampered binary, another version, another workflow's identity, a dev-key
// bundle with no trusted key and a missing bundle are refused, each with its reason.
func TestVerifyRelease(t *testing.T) {
	tm := embedded(t)
	rel := read(t, "v0.0.1-linux-amd64.sigstore.json")
	digest := releaseDigest(t)
	tampered := append([]byte{}, digest...)
	tampered[0] ^= 1
	for _, c := range []struct {
		name    string
		bundle  []byte
		digest  []byte
		version string
		want    string
	}{
		{"release", rel, digest, "v0.0.1", ""},
		{"tampered binary", rel, tampered, "v0.0.1", "does not match digest"},
		{"served as another version", rel, digest, "v0.0.2", "served as v0.0.2"},
		{"wrong identity", read(t, "s7-blob.sigstore.json"), digestOf(read(t, "s7-blob.txt")), "v0.0.1", "expected SAN value to match"},
		{"dev bundle, no dev key", read(t, "dev-blob.sigstore.json"), digestOf(read(t, "dev-blob.txt")), "v0.0.1", "transparency log"},
		{"missing bundle", nil, digest, "v0.0.1", "bundle"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, err := Verify(tm, c.bundle, c.digest, c.version, nil)
			if c.want == "" {
				if err != nil || s.Kind != "project" || !strings.HasSuffix(s.Identity, "release.yml@refs/tags/v0.0.1") {
					t.Fatalf("got %+v, %v", s, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error with %q, got %+v, %v", c.want, s, err)
			}
		})
	}
}

// 003-AC4: a cosign key-pair bundle runs only with its own key among the trusted dev keys.
func TestVerifyDevKey(t *testing.T) {
	tm := embedded(t)
	bundle, blob := read(t, "dev-blob.sigstore.json"), read(t, "dev-blob.txt")
	dev, other := key(t, "dev.pub"), key(t, "other.pub")
	for _, c := range []struct {
		name   string
		bundle []byte
		blob   []byte
		keys   []DevKey
		want   string
	}{
		{"its key", bundle, blob, []DevKey{other, dev}, "dev"},
		{"other key only", bundle, blob, []DevKey{other}, ""},
		{"tampered blob", bundle, append(blob, 'x'), []DevKey{dev}, ""},
		{"keyless bundle of another workflow", read(t, "s7-blob.sigstore.json"), read(t, "s7-blob.txt"), []DevKey{dev}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, err := Verify(tm, c.bundle, digestOf(c.blob), "v0.0.1", c.keys)
			if c.want != "" {
				if err != nil || s.Kind != "dev-key" || s.Identity != c.want {
					t.Fatalf("got %+v, %v", s, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "no trusted dev key matches") {
				t.Fatalf("want a refusal, got %+v, %v", s, err)
			}
		})
	}
}

func TestParseDevKey(t *testing.T) {
	if _, err := ParseDevKey("x", []byte("not a key")); err == nil {
		t.Fatal("garbage accepted")
	}
	k := key(t, "dev.pub")
	if len(k.Fingerprint) != 16 || k.Name != "dev" {
		t.Fatalf("key %+v", k)
	}
}

type failing struct{}

func (failing) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network down")
}

// 003-AC3: with TUF unreachable, the last good root on disk is used, else the embedded one; both
// still verify a release offline.
func TestRootsFallBack(t *testing.T) {
	dir := t.TempDir()
	r := &Roots{Dir: dir, Transport: failing{}, Log: zap.NewNop()}
	tm, info := r.Get()
	if info.Source != "embedded" || !strings.Contains(info.Error, "network down") {
		t.Fatalf("no disk copy: %+v", info)
	}
	if _, err := Verify(tm, read(t, "v0.0.1-linux-amd64.sigstore.json"), releaseDigest(t), "v0.0.1", nil); err != nil {
		t.Fatalf("embedded root: %v", err)
	}
	if err := writeAtomic(filepath.Join(dir, rootFile), embeddedRoot); err != nil {
		t.Fatal(err)
	}
	r = &Roots{Dir: dir, Transport: failing{}, Log: zap.NewNop()}
	if _, info := r.Get(); info.Source != "disk" || info.At.IsZero() {
		t.Fatalf("disk copy: %+v", info)
	}
	// A corrupt disk copy is skipped.
	if err := os.WriteFile(filepath.Join(dir, rootFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = &Roots{Dir: dir, Transport: failing{}, Log: zap.NewNop()}
	if _, info := r.Get(); info.Source != "embedded" {
		t.Fatalf("corrupt disk copy: %+v", info)
	}
	// Within the retry delay, the fallback is reused without a new attempt.
	if _, again := r.Get(); again != info {
		t.Fatalf("second Get: %+v", again)
	}
}

// 003-AC3: online, the root comes from TUF and is kept on disk as the last good copy. It needs the
// public TUF repository: set GRILL_ROOM_TEST_NETWORK=1.
func TestRootsTUF(t *testing.T) {
	if os.Getenv("GRILL_ROOM_TEST_NETWORK") != "1" {
		t.Skip("set GRILL_ROOM_TEST_NETWORK=1 to reach the public TUF repository")
	}
	dir := t.TempDir()
	tm, info := (&Roots{Dir: dir, Log: zap.NewNop()}).Get()
	if info.Source != "tuf" {
		t.Fatalf("source %+v", info)
	}
	if _, err := os.Stat(filepath.Join(dir, rootFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(tm, read(t, "v0.0.1-linux-amd64.sigstore.json"), releaseDigest(t), "v0.0.1", nil); err != nil {
		t.Fatal(err)
	}
}

// Offline proof in a container with no network (make test-offline): the embedded root verifies
// the release, and TUF fails.
func TestOffline(t *testing.T) {
	if os.Getenv("GRILL_ROOM_TEST_OFFLINE") != "1" {
		t.Skip("run by make test-offline in a container with --network none")
	}
	tm, info := (&Roots{Dir: t.TempDir(), Log: zap.NewNop()}).Get()
	if info.Source != "embedded" || info.Error == "" {
		t.Fatalf("source %+v", info)
	}
	if _, err := Verify(tm, read(t, "v0.0.1-linux-amd64.sigstore.json"), releaseDigest(t), "v0.0.1", nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("offline: %s root (%s), v0.0.1 verified", info.Source, info.Error)
}
