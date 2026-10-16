package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultAllowlist is where the sandbox may connect until the host edits the list: the Python and
// npm package indexes and GitHub's read hosts (002-AC34).
var DefaultAllowlist = []string{
	"codeload.github.com", "files.pythonhosted.org", "github.com", "objects.githubusercontent.com",
	"pypi.org", "raw.githubusercontent.com", "registry.npmjs.org",
}

// anthropicDomains are denied before any allow: reached directly with the dummy token, Claude Code
// gets a 401 and wipes its credentials (S3, D21).
var anthropicDomains = []string{".anthropic.com", ".claude.ai", "platform.claude.com"}

const (
	egressPort = "3128"
	// egressURL is the proxy the sandbox's tools use; "egress" is the squid container's alias.
	egressURL = "http://egress:" + egressPort
	// maxAllowlist bounds the list the page can save.
	maxAllowlist = 100
	// logTag starts every access log line, telling them apart from squid's own messages.
	logTag = "grill-egress"
)

// domainRe is a lowercase DNS name of two labels or more, optionally with a leading dot (the domain
// and its subdomains). The last label starts with a letter, so an IP address never matches.
var domainRe = regexp.MustCompile(`^\.?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

// covers reports whether entry, a squid dstdomain value, matches host or a subdomain of host.
func covers(entry, host string) bool {
	host = strings.TrimPrefix(host, ".")
	if !strings.HasPrefix(entry, ".") {
		return entry == host
	}
	return host == entry[1:] || strings.HasSuffix(host, entry)
}

// Allowed reports whether the allowlist list lets the sandbox reach host (009-AC25).
func Allowed(list []string, host string) bool {
	host = strings.ToLower(host)
	return slices.ContainsFunc(list, func(e string) bool { return covers(e, host) })
}

// NormalizeAllowlist validates the host's allowlist and returns it lowercased, sorted and without
// duplicates. An entry a leading-dot entry already covers is dropped: squid refuses to start on it.
func NormalizeAllowlist(in []string) ([]string, error) {
	seen := map[string]bool{}
	for _, raw := range in {
		d := strings.ToLower(strings.TrimSpace(raw))
		switch {
		case d == "":
			continue
		case strings.Contains(d, "*"):
			return nil, fmt.Errorf("%q: no wildcards; a leading dot, as in .example.com, allows a domain and its subdomains", raw)
		case len(d) > 253 || !domainRe.MatchString(d):
			return nil, fmt.Errorf("%q is not a domain name", raw)
		}
		for _, a := range anthropicDomains {
			if covers(a, d) {
				return nil, fmt.Errorf("%q is an Anthropic host: the sandbox never reaches those directly", raw)
			}
		}
		seen[d] = true
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		covered := false
		for p := range seen {
			if p != d && strings.HasPrefix(p, ".") && covers(p, d) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, d)
		}
	}
	if len(out) > maxAllowlist {
		return nil, fmt.Errorf("%d domains: at most %d", len(out), maxAllowlist)
	}
	slices.Sort(out)
	return out, nil
}

// squidConf is the egress proxy's configuration for allow, a normalized list. Rules match in order:
// Anthropic is denied first, then anything but CONNECT to 443, then whatever the list lacks. No TLS
// interception, so an allowed host gets the whole tunnel.
func squidConf(allow []string) string {
	var b strings.Builder
	b.WriteString(`http_port ` + egressPort + `
pid_filename /tmp/squid.pid
cache_log stdio:/dev/stderr
logformat grill ` + logTag + ` %ts %Ss/%03>Hs %rm %ru
access_log stdio:/dev/stdout logformat=grill
cache deny all
cache_mem 8 MB
memory_cache_shared off
# Squid sizes its tables on the fd limit, which Docker sets in the millions.
max_filedescriptors 1024
pinger_enable off
netdb_filename none
shutdown_lifetime 1 seconds
httpd_suppress_version_string on
visible_hostname grill-room-egress
acl CONNECT method CONNECT
acl SSL_ports port 443
# -n: no reverse lookup, or a raw IP whose PTR names an allowed domain gets through.
acl anthropic dstdomain -n ` + strings.Join(anthropicDomains, " ") + `
http_access deny anthropic
http_access deny !CONNECT
http_access deny !SSL_ports
`)
	if len(allow) > 0 {
		b.WriteString("acl allowlist dstdomain -n " + strings.Join(allow, " ") + "\nhttp_access allow allowlist\n")
	}
	b.WriteString("http_access deny all\n")
	return b.String()
}

// Denied is one egress request the proxy refused.
type Denied struct {
	Host string    `json:"host"`
	Time time.Time `json:"time"`
}

// Egress is what the proxy's recent log says: the last refusals, newest first, and the requests it
// let through, by host (003b-AC16).
type Egress struct {
	Denied []Denied       `json:"denied"`
	Hits   map[string]int `json:"hits"`
}

// parseEgress reads squid's access log: the last n refusals, newest first, and every request let
// through, counted by host.
func parseEgress(log []byte, n int) Egress {
	all := []Denied{}
	hits := map[string]int{}
	sc := bufio.NewScanner(bytes.NewReader(log))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 5 || f[0] != logTag {
			continue
		}
		host := f[4]
		if f[3] == "CONNECT" {
			host = strings.TrimSuffix(host, ":443")
		} else if u, err := url.Parse(host); err == nil && u.Host != "" {
			host = u.Host
		}
		if !strings.HasPrefix(f[2], "TCP_DENIED") {
			// Only an HTTPS tunnel gets through: squid denies everything else.
			if strings.HasPrefix(f[2], "TCP_TUNNEL") {
				hits[host]++
			}
			continue
		}
		sec, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		all = append(all, Denied{Host: host, Time: time.Unix(sec, 0).UTC()})
	}
	slices.Reverse(all)
	return Egress{Denied: all[:min(n, len(all))], Hits: hits}
}

// SetAllowlist sets the domains the sandbox may reach, normalized; a running proxy takes them at
// once. The list is checked before it reaches squid, so a bad one never stops the proxy.
func (s *Sandbox) SetAllowlist(ctx context.Context, list []string) error {
	list, err := NormalizeAllowlist(list)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allow = list
	if !s.started {
		return nil
	}
	_, err = s.run(ctx, strings.NewReader(squidConf(list)), "exec", "-i", s.egress(), "sh", "-c",
		"cat > /tmp/squid.conf.new && squid -k parse -f /tmp/squid.conf.new 2>/dev/null && mv /tmp/squid.conf.new /tmp/squid.conf && squid -k reconfigure -f /tmp/squid.conf")
	if err != nil {
		return fmt.Errorf("apply the allowlist: %w", err)
	}
	return nil
}

// Egress returns the last n egress requests the proxy refused, newest first, and the requests it
// let through by host; none before the sandbox starts.
func (s *Sandbox) Egress(ctx context.Context, n int) (Egress, error) {
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if !started {
		return Egress{Denied: []Denied{}, Hits: map[string]int{}}, nil
	}
	out, err := s.run(ctx, nil, "logs", "--tail", "2000", s.egress())
	if err != nil {
		return Egress{}, fmt.Errorf("egress log: %w", err)
	}
	return parseEgress(out, n), nil
}
