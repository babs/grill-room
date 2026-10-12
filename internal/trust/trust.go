// Package trust decides whether a daemon binary may run: its cosign bundle must verify with
// sigstore-go against the project's release identity, or against a dev key the host trusts
// (spec 003 AC2 to AC4).
//
// Ported from spikes/s7-sigstore/verifier in this repository (commit 8b65e60), with digests in
// place of blob readers and the trusted root refreshed through TUF with fallbacks.
package trust

import (
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/theupdateframework/go-tuf/v2/metadata/fetcher"
	"go.uber.org/zap"
)

// Issuer is the OIDC issuer of GitHub Actions workflow identities.
const Issuer = "https://token.actions.githubusercontent.com"

// ReleaseSAN is the only keyless identity a binary may carry: the release workflow on a semver tag.
const ReleaseSAN = `^https://github\.com/babs/grill-room/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$`

// releaseIdentity is the SAN of the release of version.
func releaseIdentity(version string) string {
	return "https://github.com/babs/grill-room/.github/workflows/release.yml@refs/tags/" + version
}

// Snapshot of the public-good trusted root, so verification works with no network.
//
//go:embed trusted_root.json
var embeddedRoot []byte

// Signer says who signed a binary.
type Signer struct {
	// Kind is "project" for the release workflow, "dev-key" for a key the host trusts.
	Kind string `json:"kind"`
	// Identity is the certificate's SAN, or the dev key's name.
	Identity string `json:"identity"`
}

// DevKey is a cosign public key the host trusts for dev builds.
type DevKey struct {
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
	pub         crypto.PublicKey
}

// ParseDevKey reads a PEM public key, as `cosign generate-key-pair` writes it.
func ParseDevKey(name string, pemBytes []byte) (DevKey, error) {
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		return DevKey{}, errors.New("not a PEM public key")
	}
	pub, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return DevKey{}, fmt.Errorf("public key: %w", err)
	}
	if _, err := signature.LoadVerifier(pub, crypto.SHA256); err != nil {
		return DevKey{}, fmt.Errorf("public key: %w", err)
	}
	sum := sha256.Sum256(blk.Bytes)
	return DevKey{Name: name, Fingerprint: hex.EncodeToString(sum[:8]), pub: pub}, nil
}

// Verify checks a bundle for a blob whose sha256 is digest. It accepts the release of version
// signed keyless by the project's release workflow, else a signature by one of keys.
func Verify(tm root.TrustedMaterial, bundleJSON, digest []byte, version string, keys []DevKey) (Signer, error) {
	var b bundle.Bundle
	if err := b.UnmarshalJSON(bundleJSON); err != nil {
		return Signer{}, fmt.Errorf("bundle: %w", err)
	}
	san, err := verifyKeyless(tm, &b, digest)
	if err == nil {
		if want := releaseIdentity(version); san != want {
			return Signer{}, fmt.Errorf("signed by %s, but served as %s", san, version)
		}
		return Signer{Kind: "project", Identity: san}, nil
	}
	for _, k := range keys {
		if verifyKey(k, &b, digest) == nil {
			return Signer{Kind: "dev-key", Identity: k.Name}, nil
		}
	}
	if len(keys) > 0 {
		return Signer{}, fmt.Errorf("%w; no trusted dev key matches either", err)
	}
	return Signer{}, err
}

// verifyKeyless checks the Fulcio chain, the Rekor inclusion and the identity, and returns the SAN.
func verifyKeyless(tm root.TrustedMaterial, b *bundle.Bundle, digest []byte) (string, error) {
	v, err := verify.NewVerifier(tm, verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		return "", fmt.Errorf("verifier: %w", err)
	}
	id, err := verify.NewShortCertificateIdentity(Issuer, "", "", ReleaseSAN)
	if err != nil {
		return "", fmt.Errorf("identity: %w", err)
	}
	res, err := v.Verify(b, verify.NewPolicy(verify.WithArtifactDigest("sha256", digest), verify.WithCertificateIdentity(id)))
	if err != nil {
		return "", err
	}
	if res.Signature == nil || res.Signature.Certificate == nil {
		return "", errors.New("no certificate in the verification result")
	}
	return res.Signature.Certificate.SubjectAlternativeName, nil
}

// verifyKey checks a cosign key-pair bundle. Dev signing skips Rekor, so the signature is checked
// at the current time with no transparency log.
func verifyKey(k DevKey, b *bundle.Bundle, digest []byte) error {
	sv, err := signature.LoadVerifier(k.pub, crypto.SHA256)
	if err != nil {
		return err
	}
	tm := root.NewTrustedPublicKeyMaterial(func(string) (root.TimeConstrainedVerifier, error) {
		return root.NewExpiringKey(sv, time.Time{}, time.Time{}), nil
	})
	v, err := verify.NewVerifier(tm, verify.WithCurrentTime())
	if err != nil {
		return err
	}
	_, err = v.Verify(b, verify.NewPolicy(verify.WithArtifactDigest("sha256", digest), verify.WithKey()))
	return err
}

// Roots hands out the trusted root: refreshed through TUF when online, else the last good copy on
// disk, else the one embedded in the binary (003-AC3).
type Roots struct {
	// Dir holds the TUF cache and the last good trusted_root.json.
	Dir string
	// Transport reaches the TUF repository; nil is http.DefaultTransport.
	Transport http.RoundTripper
	Log       *zap.Logger

	mu   sync.Mutex
	tm   *root.TrustedRoot
	info RootInfo
	next time.Time
}

// RootInfo says where the trusted root in use came from.
type RootInfo struct {
	// Source is "tuf", "disk" (the last good copy) or "embedded".
	Source string    `json:"source"`
	At     time.Time `json:"at"`
	Error  string    `json:"error,omitempty"`
}

// How long a root is used before the next TUF attempt: a fallback is retried sooner.
const (
	refreshEvery  = 24 * time.Hour
	fallbackEvery = time.Hour
)

const rootFile = "trusted_root.json"

// Info says where the root in use came from, without refreshing it.
func (r *Roots) Info() RootInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.info
}

// Get returns the trusted root, refreshing it when it is due.
func (r *Roots) Get() (*root.TrustedRoot, RootInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tm != nil && time.Now().Before(r.next) {
		return r.tm, r.info
	}
	b, err := r.fetch()
	if err == nil {
		if tm, perr := root.NewTrustedRootFromJSON(b); perr == nil {
			if werr := writeAtomic(filepath.Join(r.Dir, rootFile), b); werr != nil {
				r.Log.Warn("trusted_root_not_saved", zap.Error(werr))
			}
			r.tm, r.info, r.next = tm, RootInfo{Source: "tuf", At: time.Now()}, time.Now().Add(refreshEvery)
			return r.tm, r.info
		} else {
			err = perr
		}
	}
	r.Log.Warn("trusted_root_refresh_failed", zap.Error(err))
	r.next = time.Now().Add(fallbackEvery)
	path := filepath.Join(r.Dir, rootFile)
	if b, rerr := os.ReadFile(path); rerr == nil { //nolint:gosec // our own state file
		if tm, perr := root.NewTrustedRootFromJSON(b); perr == nil {
			fi, _ := os.Stat(path)
			r.tm, r.info = tm, RootInfo{Source: "disk", At: fi.ModTime(), Error: err.Error()}
			return r.tm, r.info
		}
	}
	tm, eerr := root.NewTrustedRootFromJSON(embeddedRoot)
	if eerr != nil {
		// The embedded root is checked by the tests: this is a broken build.
		panic(fmt.Sprintf("embedded trusted root: %v", eerr))
	}
	r.tm, r.info = tm, RootInfo{Source: "embedded", Error: err.Error()}
	return r.tm, r.info
}

// fetch gets trusted_root.json through TUF, with the TUF cache under Dir.
func (r *Roots) fetch() ([]byte, error) {
	rt := r.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	// Bounded: the TUF client sends its requests with no context.
	f := fetcher.NewDefaultFetcher()
	// SetHTTPClient keeps the single attempt; NewFetcherWithHTTPClient would retry for 15 minutes.
	f.SetHTTPClient(&http.Client{Timeout: 20 * time.Second, Transport: rt})
	c, err := tuf.New(tuf.DefaultOptions().WithCachePath(filepath.Join(r.Dir, "tuf")).WithFetcher(f))
	if err != nil {
		return nil, fmt.Errorf("tuf: %w", err)
	}
	b, err := c.GetTarget(rootFile)
	if err != nil {
		return nil, fmt.Errorf("tuf: %w", err)
	}
	return b, nil
}

func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
