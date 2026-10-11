// Command s7verify verifies a cosign bundle for a blob: keyless against a GitHub Actions identity,
// or against a cosign public key.
//
//	s7verify -blob f -bundle f.sigstore.json [-san REGEX] [-root trusted_root.json]
//	s7verify -blob f -bundle f.sigstore.json -key cosign.pub
//	s7verify -fetch-root out.json -tuf-cache dir
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/babs/grill-room/spikes/s7-sigstore/verifier"
	"github.com/sigstore/sigstore-go/pkg/root"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "REFUSED:", err)
		os.Exit(1)
	}
}

func run() error {
	blob := flag.String("blob", "", "signed file")
	bundlePath := flag.String("bundle", "", "cosign bundle (new format)")
	san := flag.String("san", verifier.ReleaseSAN, "expected certificate SAN, as a regexp")
	rootPath := flag.String("root", "", "trusted_root.json to use instead of the embedded one")
	key := flag.String("key", "", "cosign public key: verify a key-pair signature instead of keyless")
	fetch := flag.String("fetch-root", "", "refresh the trusted root through TUF and write it here")
	tufCache := flag.String("tuf-cache", "", "TUF metadata cache dir for -fetch-root")
	flag.Parse()

	if *fetch != "" {
		b, err := verifier.FetchRoot(*tufCache)
		if err != nil {
			return err
		}
		fmt.Printf("fetched trusted root through TUF: %d bytes\n", len(b))
		return os.WriteFile(*fetch, b, 0o600)
	}
	if *blob == "" || *bundlePath == "" {
		flag.Usage()
		os.Exit(2)
	}
	f, err := os.Open(*blob)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }() // read-only: a close error loses nothing

	if *key != "" {
		pub, err := os.ReadFile(*key)
		if err != nil {
			return err
		}
		if err := verifier.VerifyKey(pub, *bundlePath, f); err != nil {
			return err
		}
		fmt.Println("OK key", *key)
		return nil
	}

	tr, err := loadRoot(*rootPath)
	if err != nil {
		return err
	}
	got, err := verifier.VerifyKeyless(tr, *bundlePath, f, *san)
	if err != nil {
		return err
	}
	fmt.Println("OK keyless", got)
	return nil
}

func loadRoot(path string) (*root.TrustedRoot, error) {
	if path == "" {
		return verifier.EmbeddedRoot()
	}
	return root.NewTrustedRootFromPath(path)
}
