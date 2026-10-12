// Package trusttest signs blobs with a throwaway dev key, as `cosign sign-blob --key` does, for
// tests only: it keeps sigstore-go's signing code out of the daemon binary.
package trusttest

import (
	"testing"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/sign"
)

// Key is a dev signing key.
type Key struct {
	kp *sign.EphemeralKeypair
	// PEM is the public key, as a host would add it to the trusted dev keys.
	PEM []byte
}

// NewKey makes a P-256 key.
func NewKey(t testing.TB) *Key {
	t.Helper()
	kp, err := sign.NewEphemeralKeypair(nil)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := kp.GetPublicKeyPem()
	if err != nil {
		t.Fatal(err)
	}
	return &Key{kp: kp, PEM: []byte(pub)}
}

// Sign returns blob's sigstore bundle, with no certificate and no transparency log entry.
func (k *Key) Sign(t testing.TB, blob []byte) []byte {
	t.Helper()
	pb, err := sign.Bundle(&sign.PlainData{Data: blob}, k.kp, sign.BundleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := (&bundle.Bundle{Bundle: pb}).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return b
}
