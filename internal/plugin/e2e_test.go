//go:build e2e

package plugin

import (
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The e2e client reaches its git server as example.com on loopback, and nothing else on
// loopback: neither a literal IP nor another name resolving there.
func TestE2EClientOnlyReachesItsHost(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	c := NewE2EClient("example.com", pool)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	get := func(u string) error {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
		resp, err := c.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusTeapot {
				return errors.New(resp.Status)
			}
		}
		return err
	}
	if err := get("https://example.com:" + port + "/"); err != nil {
		t.Fatalf("example.com: %v", err)
	}
	for _, u := range []string{"https://127.0.0.1:" + port + "/", "https://[::ffff:127.0.0.1]:" + port + "/", "https://localhost:" + port + "/"} {
		if err := get(u); !errors.Is(err, ErrBlocked) {
			t.Fatalf("%s: %v, want ErrBlocked", u, err)
		}
	}
}
