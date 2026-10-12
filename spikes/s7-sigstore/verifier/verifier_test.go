package verifier

import (
	"os"
	"strings"
	"testing"
)

// s7-blob.sigstore.json was signed keyless by the throwaway spike/s7-sign workflow.
const spikeSAN = `^https://github\.com/babs/grill-room/\.github/workflows/s7-sign\.yml@refs/heads/spike/s7-sign$`

func open(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// 003-AC2, 003-AC3: keyless bundle checked offline against the embedded root and an identity.
func TestVerifyKeyless(t *testing.T) {
	tr, err := EmbeddedRoot()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, blob, bundle, san, wantErr string
	}{
		{"ok", "s7-blob.txt", "s7-blob.sigstore.json", spikeSAN, ""},
		{"tampered blob", "dev-blob.txt", "s7-blob.sigstore.json", spikeSAN, "invalid signature"},
		{"release identity", "s7-blob.txt", "s7-blob.sigstore.json", ReleaseSAN, "expected SAN value to match"},
		{"other repo", "s7-blob.txt", "s7-blob.sigstore.json", `^https://github\.com/evil/.*$`, "expected SAN value to match"},
		{"key bundle", "dev-blob.txt", "dev-blob.sigstore.json", spikeSAN, "transparency log"},
		{"missing bundle", "s7-blob.txt", "missing.sigstore.json", spikeSAN, "no such file"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := VerifyKeyless(tr, "testdata/"+c.bundle, open(t, c.blob), c.san)
			if c.wantErr == "" {
				if err != nil || !strings.Contains(got, "s7-sign.yml") {
					t.Fatalf("got %q, %v", got, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want error with %q, got %v", c.wantErr, err)
			}
		})
	}
}

// Dev builds: a cosign key-pair bundle, signed with no transparency log.
func TestVerifyKey(t *testing.T) {
	cases := []struct {
		name, blob, bundle, key string
		ok                      bool
	}{
		{"ok", "dev-blob.txt", "dev-blob.sigstore.json", "dev.pub", true},
		{"tampered blob", "s7-blob.txt", "dev-blob.sigstore.json", "dev.pub", false},
		{"other key", "dev-blob.txt", "dev-blob.sigstore.json", "other.pub", false},
		{"keyless bundle", "s7-blob.txt", "s7-blob.sigstore.json", "dev.pub", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pub, err := os.ReadFile("testdata/" + c.key)
			if err != nil {
				t.Fatal(err)
			}
			err = VerifyKey(pub, "testdata/"+c.bundle, open(t, c.blob))
			if (err == nil) != c.ok {
				t.Fatalf("ok=%v, err=%v", c.ok, err)
			}
		})
	}
}
