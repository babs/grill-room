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
// npm package indexes, GitHub's read hosts (002-AC34) and Anthropic's docs hosts (014-AC6).
var DefaultAllowlist = []string{
	"code.claude.com", "codeload.github.com", "files.pythonhosted.org", "github.com", "objects.githubusercontent.com",
	"pypi.org", "raw.githubusercontent.com", "registry.npmjs.org", "support.claude.com",
}

// anthropicDomains are denied before any allow: reached directly with the dummy token, Claude Code
// gets a 401 and wipes its credentials (S3, D21). .claude.com holds the OAuth token host.
var anthropicDomains = []string{".anthropic.com", ".claude.ai", ".claude.com"}

// AnthropicDocs are the Anthropic hosts that serve documentation with no bearer and no account
// data: the only ones the lists may hold, by exact name (014-AC6).
var AnthropicDocs = []string{"code.claude.com", "support.claude.com"}

// linkLocal stays refused at every level: 169.254.169.254 answers cloud metadata, credentials
// included (014-AC26).
const linkLocal = "169.254.0.0/16 fe80::/10"

// privateRanges are the host's own networks, refused under "Allow every website" unless the host
// also allows the local network (014-AC2).
const privateRanges = "0.0.0.0/8 10.0.0.0/8 100.64.0.0/10 127.0.0.0/8 172.16.0.0/12 192.168.0.0/16 ::/128 ::1 fc00::/7"

// Policy is what the egress proxy lets through: the domains of Allow, normalized, or with
// AllWebsites any domain name; LocalNetwork, only with AllWebsites, adds IP literals and the
// private ranges (014-AC2, 014-AC26).
type Policy struct {
	Allow        []string
	AllWebsites  bool
	LocalNetwork bool
	// Hold asks the daemon, through the hold socket, about a public name off the list, and lets
	// the CONNECT wait for its answer (014-AC29). It needs Config.HoldDir.
	Hold bool
}

// HoldSocket is where the egress container reaches the daemon's hold server: Config.HoldDir,
// mounted, holds it as hold.sock.
const (
	holdMount  = "/run/grill-hold"
	HoldSocket = "hold.sock"
)

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
	out, err := normalize(in)
	if err == nil && len(out) > maxAllowlist {
		return nil, fmt.Errorf("%d domains: at most %d", len(out), maxAllowlist)
	}
	return out, err
}

// normalize is NormalizeAllowlist with no bound on the count: a sandbox's list merges the server's
// and its room's, each bounded where it is saved.
func normalize(in []string) ([]string, error) {
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
			if covers(a, d) && !slices.Contains(AnthropicDocs, d) {
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
	slices.Sort(out)
	return out, nil
}

// squidConf is the egress proxy's configuration for p, its list normalized. Rules match in order:
// Anthropic but its docs hosts is denied first, then anything but CONNECT to 443, then the list is
// allowed. Without "Allow every website", whatever the list lacks is denied. With it, link-local
// destinations are denied, then IP literals and private destinations unless the local network is
// allowed too, then the rest is allowed. No TLS interception: an allowed host gets the whole tunnel.
func squidConf(p Policy) string {
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
acl anthropic_docs dstdomain -n ` + strings.Join(AnthropicDocs, " ") + `
http_access deny anthropic !anthropic_docs
http_access deny !CONNECT
http_access deny !SSL_ports
`)
	if len(p.Allow) > 0 {
		b.WriteString("acl allowlist dstdomain -n " + strings.Join(p.Allow, " ") + "\nhttp_access allow allowlist\n")
	}
	if !p.AllWebsites && p.Hold {
		// Never asked: link-local, IP literals, private destinations. One helper takes every
		// question at once, so a wait never stalls other traffic.
		b.WriteString("acl link_local dst " + linkLocal + "\nhttp_access deny link_local\n")
		b.WriteString("acl ip_literal dstdom_regex -n ^[0-9.]+$ :\nhttp_access deny ip_literal\n")
		b.WriteString("acl private dst " + privateRanges + "\nhttp_access deny private\n")
		b.WriteString("external_acl_type hold concurrency=100 ttl=0 negative_ttl=0 children-max=1 children-startup=1 %DST /usr/bin/socat -t 3600 - UNIX-CONNECT:" +
			holdMount + "/" + HoldSocket + "\nacl held external hold\nhttp_access allow held\n")
	}
	if !p.AllWebsites {
		b.WriteString("http_access deny all\n")
		return b.String()
	}
	b.WriteString("acl link_local dst " + linkLocal + "\nhttp_access deny link_local\n")
	if !p.LocalNetwork {
		// A dotted or bare number is an IPv4 literal, a colon an IPv6 one: no domain name has either.
		b.WriteString("acl ip_literal dstdom_regex -n ^[0-9.]+$ :\nhttp_access deny ip_literal\n")
		b.WriteString("acl private dst " + privateRanges + "\nhttp_access deny private\n")
	}
	b.WriteString("http_access allow all\n")
	return b.String()
}

// Denied is one egress request the proxy refused.
type Denied struct {
	Host string    `json:"host"`
	Time time.Time `json:"time"`
}

// Egress is what the proxy's recent log says: the last refusals, newest first, and the requests it
// let through, by host (003b-AC16), and by room link where the daemon sums several sandboxes
// (014-AC3).
type Egress struct {
	Denied   []Denied                  `json:"denied"`
	Hits     map[string]int            `json:"hits"`
	RoomHits map[string]map[string]int `json:"room_hits,omitempty"`
}

// EgressRequest is one request the egress proxy let through as a tunnel, or refused.
type EgressRequest struct {
	// Host is lowercase, without the port when it is 443; a plain HTTP request's URL host.
	Host    string
	Method  string
	Allowed bool
	Time    time.Time
}

// parseRequests reads squid's access log lines; squid's own messages and requests neither
// tunnelled nor refused are skipped.
func parseRequests(log []byte) []EgressRequest {
	var out []EgressRequest
	sc := bufio.NewScanner(bytes.NewReader(log))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 5 || f[0] != logTag {
			continue
		}
		host := strings.ToLower(f[4])
		if f[3] == "CONNECT" {
			host = strings.TrimSuffix(host, ":443")
		} else if u, err := url.Parse(host); err == nil && u.Host != "" {
			host = u.Host
		}
		// Only an HTTPS tunnel gets through: squid denies everything else.
		allowed := strings.HasPrefix(f[2], "TCP_TUNNEL")
		if !allowed && !strings.HasPrefix(f[2], "TCP_DENIED") {
			continue
		}
		sec, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		out = append(out, EgressRequest{Host: host, Method: f[3], Allowed: allowed, Time: time.Unix(sec, 0).UTC()})
	}
	return out
}

// parseEgress reads squid's access log: the last n refusals, newest first, and every request let
// through, counted by host.
func parseEgress(log []byte, n int) Egress {
	all := []Denied{}
	hits := map[string]int{}
	for _, q := range parseRequests(log) {
		if q.Allowed {
			hits[q.Host]++
		} else {
			all = append(all, Denied{Host: q.Host, Time: q.Time})
		}
	}
	slices.Reverse(all)
	return Egress{Denied: all[:min(n, len(all))], Hits: hits}
}

// SetPolicy sets what the sandbox may reach, its list normalized; a running proxy takes it at once.
// The list is checked before it reaches squid, so a bad one never stops the proxy.
func (s *Sandbox) SetPolicy(ctx context.Context, p Policy) error {
	list, err := normalize(p.Allow)
	if err != nil {
		return err
	}
	p.Allow = list
	p.LocalNetwork = p.LocalNetwork && p.AllWebsites
	p.Hold = p.Hold && s.cfg.HoldDir != ""
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policy = p
	if !s.started {
		return nil
	}
	_, err = s.run(ctx, strings.NewReader(squidConf(p)), "exec", "-i", s.egress(), "sh", "-c",
		"cat > /tmp/squid.conf.new && squid -k parse -f /tmp/squid.conf.new 2>/dev/null && mv /tmp/squid.conf.new /tmp/squid.conf && squid -k reconfigure -f /tmp/squid.conf")
	if err != nil {
		return fmt.Errorf("apply the allowlist: %w", err)
	}
	return nil
}

// Requests returns the requests the egress proxy logged since since, oldest first; none before the
// sandbox starts. A turn reads those of its own run (014-AC3).
func (s *Sandbox) Requests(ctx context.Context, since time.Time) ([]EgressRequest, error) {
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if !started {
		return nil, nil
	}
	out, err := s.run(ctx, nil, "logs", "--since", since.UTC().Format(time.RFC3339Nano), s.egress())
	if err != nil {
		return nil, fmt.Errorf("egress log: %w", err)
	}
	return parseRequests(out), nil
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

// holdMount is the egress proxy's mount of the hold socket's folder, none without one.
func (s *Sandbox) holdMount() []string {
	if s.cfg.HoldDir == "" {
		return nil
	}
	return []string{"-v", s.cfg.HoldDir + ":" + holdMount}
}
