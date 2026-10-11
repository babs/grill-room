// Package verifier checks a cosign bundle for a blob with sigstore-go, the way the supervisor
// would check a worker binary before running it (spec 003 AC2, AC3).
package verifier

import (
	"crypto"
	"crypto/x509"
	_ "embed"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/sigstore/sigstore/pkg/signature"
)

// Issuer is the OIDC issuer of GitHub Actions workflow identities.
const Issuer = "https://token.actions.githubusercontent.com"

// ReleaseSAN is the only identity a release binary may carry: the release workflow on a semver tag.
const ReleaseSAN = `^https://github\.com/babs/grill-room/\.github/workflows/release\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+$`

// Snapshot of the public-good trusted root, so verification works with no network.
//
//go:embed trusted_root.json
var embeddedRoot []byte

// EmbeddedRoot returns the trusted root compiled into the binary.
func EmbeddedRoot() (*root.TrustedRoot, error) {
	return root.NewTrustedRootFromJSON(embeddedRoot)
}

// FetchRoot refreshes the trusted root through TUF, caching metadata under cacheDir.
func FetchRoot(cacheDir string) ([]byte, error) {
	c, err := tuf.New(tuf.DefaultOptions().WithCachePath(cacheDir))
	if err != nil {
		return nil, fmt.Errorf("tuf client: %w", err)
	}
	b, err := c.GetTarget("trusted_root.json")
	if err != nil {
		return nil, fmt.Errorf("tuf target: %w", err)
	}
	if _, err := root.NewTrustedRootFromJSON(b); err != nil {
		return nil, fmt.Errorf("tuf trusted root: %w", err)
	}
	return b, nil
}

// VerifyKeyless checks a keyless bundle: Fulcio chain, Rekor inclusion, and a certificate whose
// SAN matches sanRegex and whose issuer is GitHub Actions. It returns the signer's SAN.
func VerifyKeyless(tm root.TrustedMaterial, bundlePath string, blob io.Reader, sanRegex string) (string, error) {
	b, err := bundle.LoadJSONFromPath(bundlePath)
	if err != nil {
		return "", fmt.Errorf("load bundle: %w", err)
	}
	v, err := verify.NewVerifier(tm, verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		return "", fmt.Errorf("verifier: %w", err)
	}
	id, err := verify.NewShortCertificateIdentity(Issuer, "", "", sanRegex)
	if err != nil {
		return "", fmt.Errorf("identity: %w", err)
	}
	res, err := v.Verify(b, verify.NewPolicy(verify.WithArtifact(blob), verify.WithCertificateIdentity(id)))
	if err != nil {
		return "", err
	}
	if res.Signature == nil || res.Signature.Certificate == nil {
		return "", errors.New("no certificate in verification result")
	}
	return res.Signature.Certificate.SubjectAlternativeName, nil
}

// VerifyKey checks a bundle signed with a cosign key pair (dev builds). Dev signing skips Rekor,
// so the signature is checked at the current time with no transparency log.
func VerifyKey(pubPEM []byte, bundlePath string, blob io.Reader) error {
	blk, _ := pem.Decode(pubPEM)
	if blk == nil {
		return errors.New("public key: no PEM block")
	}
	pub, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return fmt.Errorf("public key: %w", err)
	}
	sv, err := signature.LoadVerifier(pub, crypto.SHA256)
	if err != nil {
		return fmt.Errorf("public key verifier: %w", err)
	}
	tm := root.NewTrustedPublicKeyMaterial(func(string) (root.TimeConstrainedVerifier, error) {
		return root.NewExpiringKey(sv, time.Time{}, time.Time{}), nil
	})
	b, err := bundle.LoadJSONFromPath(bundlePath)
	if err != nil {
		return fmt.Errorf("load bundle: %w", err)
	}
	v, err := verify.NewVerifier(tm, verify.WithCurrentTime())
	if err != nil {
		return fmt.Errorf("verifier: %w", err)
	}
	_, err = v.Verify(b, verify.NewPolicy(verify.WithArtifact(blob), verify.WithKey()))
	return err
}
