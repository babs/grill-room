// Package plugin fetches Claude Code plugins from public git repositories, snapshots them into a
// deterministic tar, and reads what they contain (spec 009, AC5 to AC15). The server image has no
// git binary, so the git client here is pure Go.
package plugin

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Timeout bounds one request to a git server (009-AC8).
const Timeout = 60 * time.Second

var (
	// ErrCredentials refuses a URL holding userinfo; the text is what the user reads (009-AC7).
	ErrCredentials = errors.New("Never put credentials in a link. For a private repo, the AI host fetches it.") //nolint:revive,staticcheck // user-facing sentence
	// ErrNotHTTPS refuses any scheme but https, ssh and scp-like forms included.
	ErrNotHTTPS = errors.New("only https links can be fetched by the server")
	// ErrBlocked refuses a connection to a non-public address (009-AC8).
	ErrBlocked = errors.New("refused: not a public address")
)

// CheckURL validates a repo URL before anything is fetched. It never echoes the URL back: it may
// hold a secret.
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		// url.Parse rejects scp-like "git@host:path"; still say why when the shape is that one.
		if !strings.Contains(raw, "://") {
			return ErrNotHTTPS
		}
		return errors.New("not a valid link")
	}
	// ssh's "git@" is how private repos are named (009-AC19), not a credential.
	if u.User != nil && (u.Scheme == "https" || u.Scheme == "http") {
		return ErrCredentials
	}
	if u.Scheme != "https" {
		return ErrNotHTTPS
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("the link has no host")
	}
	if a, err := netip.ParseAddr(host); err == nil && blocked(a) {
		return ErrBlocked
	}
	return nil
}

// special are ranges no public git server lives in, including the IPv6 forms that embed an
// IPv4 address (NAT64, 6to4, Teredo, IPv4-compatible, SIIT) and so could reach a private one.
var special = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{"100.64.0.0/10", "0.0.0.0/8", "240.0.0.0/4", "192.0.0.0/24", "198.18.0.0/15",
		"64:ff9b::/96", "64:ff9b:1::/48", "2002::/16", "2001::/32", "::/96", "::ffff:0:0:0/96", "fec0::/10", "100::/64"} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// blocked reports whether a may not be dialed.
func blocked(a netip.Addr) bool {
	a = a.Unmap()
	return !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() ||
		a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() ||
		slices.ContainsFunc(special, func(p netip.Prefix) bool { return p.Contains(a) })
}

// options are test hooks, unexported so no caller outside this package can loosen the guard.
type options struct {
	lookup  func(ctx context.Context, host string) ([]netip.Addr, error)
	rootCAs *x509.CertPool
	// allow exempts exact addresses from the address check: the loopback test server.
	allow []netip.Addr
}

// NewClient returns an http.Client that fetches public https URLs only. The address check runs
// in the dialer on the address actually dialed, after resolution, for every connection: a
// redirect or a DNS answer that changes between lookups cannot reach a private address.
func NewClient() *http.Client { return newClient(options{}) }

func newClient(o options) *http.Client {
	lookup := o.lookup
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	d := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrBlocked, err)
		}
		if blocked(ap.Addr()) && !slices.Contains(o.allow, ap.Addr()) {
			return fmt.Errorf("%w (%s)", ErrBlocked, ap.Addr())
		}
		return nil
	}}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		var addrs []netip.Addr
		if a, err := netip.ParseAddr(host); err == nil {
			addrs = []netip.Addr{a}
		} else if addrs, err = lookup(ctx, host); err != nil {
			return nil, err
		}
		err = fmt.Errorf("%s: no address", host)
		for _, a := range addrs {
			var c net.Conn
			if c, err = d.DialContext(ctx, network, net.JoinHostPort(a.Unmap().String(), port)); err == nil {
				return c, nil
			}
		}
		return nil, err
	}
	return &http.Client{
		Timeout: Timeout,
		Transport: checked{&http.Transport{
			// Security: no proxy from the environment, the proxy would dial the target unchecked.
			Proxy:                 nil,
			DialContext:           dial,
			TLSClientConfig:       &tls.Config{RootCAs: o.rootCAs, MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       30 * time.Second,
			ForceAttemptHTTP2:     true,
		}},
	}
}

// checked runs CheckURL on every request, the first and each redirect.
type checked struct{ http.RoundTripper }

func (c checked) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := CheckURL(req.URL.String()); err != nil {
		return nil, err
	}
	return c.RoundTripper.RoundTrip(req)
}
