package plugin

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var loopback = netip.MustParseAddr("127.0.0.1")

// testClient is the guarded client with names mapped by hosts, trusting srv's certificate and
// exempting the loopback test server only.
func testClient(srv *httptest.Server, lookup func(host string) []netip.Addr) *http.Client {
	o := options{
		lookup: func(_ context.Context, host string) ([]netip.Addr, error) {
			if a := lookup(host); a != nil {
				return a, nil
			}
			return nil, fmt.Errorf("no such host %s", host)
		},
		allow: []netip.Addr{loopback},
	}
	if srv != nil {
		o.rootCAs = srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	}
	c := newClient(o)
	c.Transport.(checked).RoundTripper.(*http.Transport).DisableKeepAlives = true
	return c
}

func get(t *testing.T, c *http.Client, u string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	require.NoError(t, err)
	resp, err := c.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

// 009-AC7, 009-AC8: every case is refused before a byte leaves for the address.
func TestGuardRefuses(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://internal.test/x", http.StatusFound)
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	names := map[string]string{"private.test": "192.168.1.10", "internal.test": "10.1.2.3", "example.com": "127.0.0.1"}
	c := testClient(srv, func(h string) []netip.Addr {
		if a, ok := names[h]; ok {
			return []netip.Addr{netip.MustParseAddr(a)}
		}
		return nil
	})
	cases := []struct {
		name, url string
		want      error
	}{
		{"http", "http://example.com/", ErrNotHTTPS},
		{"userinfo", "https://user:token@example.com/", ErrCredentials},
		{"userinfo name only", "https://user@example.com/", ErrCredentials},
		{"loopback", "https://127.0.0.2/", ErrBlocked},
		{"rfc1918", "https://10.0.0.1/", ErrBlocked},
		{"metadata", "https://169.254.169.254/latest/meta-data/", ErrBlocked},
		{"ipv6 loopback", "https://[::1]/", ErrBlocked},
		{"ipv6 unique-local", "https://[fd00::1]/", ErrBlocked},
		{"cgnat", "https://100.64.0.1/", ErrBlocked},
		{"ipv4-mapped", "https://[::ffff:10.0.0.1]/", ErrBlocked},
		{"multicast", "https://224.0.0.1/", ErrBlocked},
		{"unspecified", "https://0.0.0.0/", ErrBlocked},
		{"broadcast", "https://255.255.255.255/", ErrBlocked},
		{"nat64", "https://[64:ff9b::a00:1]/", ErrBlocked},
		{"nat64 local", "https://[64:ff9b:1::a00:1]/", ErrBlocked},
		{"6to4", "https://[2002:a00:1::1]/", ErrBlocked},
		{"teredo", "https://[2001:0:4136:e378:8000:63bf:f5ff:fffe]/", ErrBlocked},
		{"ipv4-compatible", "https://[::10.0.0.1]/", ErrBlocked},
		{"siit", "https://[::ffff:0:a00:1]/", ErrBlocked},
		{"site-local", "https://[fec0::1]/", ErrBlocked},
		{"discard-only", "https://[100::1]/", ErrBlocked},
		{"ietf protocol", "https://192.0.0.8/", ErrBlocked},
		{"benchmarking", "https://198.18.0.1/", ErrBlocked},
		{"name resolving private", "https://private.test/", ErrBlocked},
		{"redirect to private", "https://example.com:" + port + "/", ErrBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorIs(t, get(t, c, tc.url), tc.want)
		})
	}
}

// 009-AC8: the check runs on the dialed address, so a name that answers public then private is
// refused on the second connection.
func TestGuardRebinding(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	var n atomic.Int32
	c := testClient(srv, func(string) []netip.Addr {
		if n.Add(1) == 1 {
			return []netip.Addr{loopback} // the exempt test server stands for a public address
		}
		return []netip.Addr{netip.MustParseAddr("10.0.0.7")}
	})
	u := "https://example.com:" + port + "/"
	require.NoError(t, get(t, c, u))
	require.ErrorIs(t, get(t, c, u), ErrBlocked)
	require.Equal(t, int32(2), n.Load())
}

func TestCheckURL(t *testing.T) {
	for raw, want := range map[string]error{
		"https://github.com/babs/skills":       nil,
		"https://github.com/babs/skills.git":   nil,
		"git@github.com:babs/skills.git":       ErrNotHTTPS,
		"ssh://git@github.com/babs/skills":     ErrNotHTTPS,
		"file:///etc":                          ErrNotHTTPS,
		"ext::sh -c id":                        ErrNotHTTPS,
		"https://x:y@github.com/babs/skills":   ErrCredentials,
		"https://[fe80::1]/r":                  ErrBlocked,
		"https://93.184.216.34/r":              nil,
		"https://[2a00:1450:4007:80f::200e]/r": nil,
		"https://[2606:4700:4700::1111]/repo":  nil,
	} {
		err := CheckURL(raw)
		if want == nil {
			require.NoError(t, err, raw)
		} else {
			require.ErrorIs(t, err, want, raw)
		}
	}
	require.Error(t, CheckURL("https:///nohost"))
	require.Error(t, CheckURL("https://exa mple.com/%zz"))
	require.NotContains(t, CheckURL("https://u:s3cret@h/").Error(), "s3cret")
}
