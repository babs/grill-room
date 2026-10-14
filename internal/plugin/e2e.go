//go:build e2e

package plugin

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
)

// NewE2EClient is NewClient for the e2e build only: it trusts rootCAs and dials host, and only
// host, on 127.0.0.1, so the browser tests fetch plugins from a local git server. A literal IP
// stays refused by CheckURL, and another name resolving to 127.0.0.1 is refused here. The release
// build has no such function: its guard cannot be loosened.
func NewE2EClient(host string, rootCAs *x509.CertPool) *http.Client {
	lo := netip.MustParseAddr("127.0.0.1")
	return newClient(options{rootCAs: rootCAs, allow: []netip.Addr{lo},
		lookup: func(ctx context.Context, h string) ([]netip.Addr, error) {
			if h == host {
				return []netip.Addr{lo}, nil
			}
			addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", h)
			if err == nil && slices.ContainsFunc(addrs, func(a netip.Addr) bool { return a.Unmap() == lo }) {
				return nil, fmt.Errorf("%w (%s resolves to %s)", ErrBlocked, h, lo)
			}
			return addrs, err
		}})
}
