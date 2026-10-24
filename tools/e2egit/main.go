// Command e2egit serves the bare git repos under -root over smart HTTP and TLS, with git
// http-backend, for the browser tests' plugins (e2e/plugins.spec.js). Its certificate is
// httptest's, valid for example.com; it writes the PEM to -ca for the e2e build of the server.
package main

import (
	"context"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http/cgi" //nolint:gosec // httpoxy: fixed in Go 1.6.3; a test-only server on loopback
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7692", "listen address")
	root := flag.String("root", "test-results/git", "folder of the bare repos to serve")
	ca := flag.String("ca", "test-results/git-ca.pem", "where to write the server's certificate")
	flag.Parse()
	// run serves forever: it only ever returns an error.
	log.Fatal(run(*addr, *root, *ca))
}

func run(addr, root, ca string) error {
	out, err := exec.Command("git", "--exec-path").Output() //nolint:noctx // runs once at start
	if err != nil {
		return fmt.Errorf("git: %w", err)
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	backend := &cgi.Handler{
		Path:       filepath.Join(strings.TrimSpace(string(out)), "git-http-backend"),
		Env:        []string{"GIT_PROJECT_ROOT=" + abs, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1"},
		InheritEnv: []string{"PATH", "HOME"},
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		return err
	}
	srv := httptest.NewUnstartedServer(backend)
	_ = srv.Listener.Close() // replaced by the fixed address
	srv.Listener = ln
	srv.StartTLS()
	// Written aside then renamed: the server reads it at its first fetch, never half written.
	tmp := ca + ".tmp"
	if err := os.WriteFile(tmp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, ca); err != nil {
		return err
	}
	log.Printf("serving %s on https://example.com:%s", abs, strings.TrimPrefix(addr[strings.LastIndex(addr, ":"):], ":"))
	select {}
}
