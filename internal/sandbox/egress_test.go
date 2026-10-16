package sandbox

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// 002-AC36: the page's allowlist is checked: domains only, no wildcard but a leading dot, never an
// Anthropic host; it comes back lowercased, sorted, deduplicated, without entries a leading-dot
// entry covers (squid refuses to start on those).
func TestNormalizeAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want []string
		err  string
	}{
		{"default", DefaultAllowlist, DefaultAllowlist, ""},
		{"empty", nil, []string{}, ""},
		{"blank entries dropped", []string{" ", ""}, []string{}, ""},
		{"case, spaces, dups, order", []string{" PyPI.org", "github.com", "pypi.org "}, []string{"github.com", "pypi.org"}, ""},
		{"leading dot covers", []string{"github.com", "api.github.com", ".github.com", "x.githubusercontent.com"}, []string{".github.com", "x.githubusercontent.com"}, ""},
		{"nested leading dots", []string{".a.example.com", ".example.com"}, []string{".example.com"}, ""},
		{"suffix is not a subdomain", []string{".hub.com", "github.com"}, []string{".hub.com", "github.com"}, ""},
		{"star", []string{"*.example.com"}, nil, "no wildcards"},
		{"star inside", []string{"ex*ample.com"}, nil, "no wildcards"},
		{"single label", []string{"localhost"}, nil, "not a domain name"},
		{"ip", []string{"10.0.0.1"}, nil, "not a domain name"},
		{"port", []string{"example.com:443"}, nil, "not a domain name"},
		{"scheme", []string{"https://example.com"}, nil, "not a domain name"},
		{"path", []string{"example.com/x"}, nil, "not a domain name"},
		{"trailing dot", []string{"example.com."}, nil, "not a domain name"},
		{"double dot", []string{"example..com"}, nil, "not a domain name"},
		{"hyphen edge", []string{"-x.example.com"}, nil, "not a domain name"},
		{"squid option", []string{"-i"}, nil, "not a domain name"},
		{"newline", []string{"a.com\nhttp_access allow all"}, nil, "not a domain name"},
		{"anthropic api", []string{"api.anthropic.com"}, nil, "Anthropic host"},
		{"anthropic apex", []string{"anthropic.com"}, nil, "Anthropic host"},
		{"anthropic dot", []string{".anthropic.com"}, nil, "Anthropic host"},
		{"claude.ai", []string{"CLAUDE.AI"}, nil, "Anthropic host"},
		{"platform", []string{"platform.claude.com"}, nil, "Anthropic host"},
		// 014-AC6: the docs hosts by exact name only; every other claude.com host stays out.
		{"docs hosts", []string{"support.claude.com", "Code.Claude.com"}, []string{"code.claude.com", "support.claude.com"}, ""},
		{"docs host dotted", []string{".code.claude.com"}, nil, "Anthropic host"},
		{"docs subdomain", []string{"x.code.claude.com"}, nil, "Anthropic host"},
		{"claude.com apex", []string{"claude.com"}, nil, "Anthropic host"},
		{"claude.com dot", []string{".claude.com"}, nil, "Anthropic host"},
		{"docs redirect", []string{"docs.claude.com"}, nil, "Anthropic host"},
		{"hosted connector", []string{"slack.mcp.claude.com"}, nil, "Anthropic host"},
		{"connector relay", []string{"mcp-proxy.anthropic.com"}, nil, "Anthropic host"},
		{"docs anthropic", []string{"docs.anthropic.com"}, nil, "Anthropic host"},
		{"too long", []string{strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + "." + strings.Repeat("d", 60) + "." + strings.Repeat("e", 10) + ".com"}, nil, "not a domain name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeAllowlist(tc.in)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	many := make([]string, maxAllowlist+1)
	for i := range many {
		many[i] = "h" + strings.Repeat("x", i%50) + string(rune('a'+i%26)) + ".example.com"
	}
	if _, err := NormalizeAllowlist(many); err == nil {
		t.Fatal("over the limit accepted")
	}
}

// squidHead is every configuration's start, as v0.0.29 wrote it but for the Anthropic lines
// (014-AC6).
const squidHead = `http_port 3128
pid_filename /tmp/squid.pid
cache_log stdio:/dev/stderr
logformat grill grill-egress %ts %Ss/%03>Hs %rm %ru
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
acl anthropic dstdomain -n .anthropic.com .claude.ai .claude.com
acl anthropic_docs dstdomain -n code.claude.com support.claude.com
http_access deny anthropic !anthropic_docs
http_access deny !CONNECT
http_access deny !SSL_ports
`

// 002-AC35 014-AC2 014-AC6 014-AC26: squid denies Anthropic but its docs hosts before any allow,
// then anything but CONNECT to 443, then allows the list. Switch off, the rest is denied, as
// before 014. Every website: link-local, IP literals and private destinations denied, the rest
// allowed. Local network too: only link-local stays denied. An empty list allows nothing switched
// off. The whole text is compared: a reordered rule is a different policy.
func TestSquidConf(t *testing.T) {
	const list = "acl allowlist dstdomain -n github.com pypi.org\nhttp_access allow allowlist\n"
	const linkLocalDeny = "acl link_local dst 169.254.0.0/16 fe80::/10\nhttp_access deny link_local\n"
	const privateDeny = "acl ip_literal dstdom_regex -n ^[0-9.]+$ :\nhttp_access deny ip_literal\n" +
		"acl private dst 0.0.0.0/8 10.0.0.0/8 100.64.0.0/10 127.0.0.0/8 172.16.0.0/12 192.168.0.0/16 ::/128 ::1 fc00::/7\nhttp_access deny private\n"
	allow := []string{"github.com", "pypi.org"}
	for _, tc := range []struct {
		name string
		p    Policy
		want string
	}{
		{"list only", Policy{Allow: allow}, squidHead + list + "http_access deny all\n"},
		{"empty list", Policy{}, squidHead + "http_access deny all\n"},
		{"local network without every website", Policy{Allow: allow, LocalNetwork: true}, squidHead + list + "http_access deny all\n"},
		{"every website", Policy{Allow: allow, AllWebsites: true}, squidHead + list + linkLocalDeny + privateDeny + "http_access allow all\n"},
		{"every website, empty list", Policy{AllWebsites: true}, squidHead + linkLocalDeny + privateDeny + "http_access allow all\n"},
		{"local network", Policy{Allow: allow, AllWebsites: true, LocalNetwork: true}, squidHead + list + linkLocalDeny + "http_access allow all\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := squidConf(tc.p); got != tc.want {
				t.Fatalf("got:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
	// 014-AC26: the metadata range is denied at both levels, from the one constant.
	for _, p := range []Policy{{AllWebsites: true}, {AllWebsites: true, LocalNetwork: true}} {
		if !strings.Contains(squidConf(p), "acl link_local dst "+linkLocal+"\n") || !strings.Contains(linkLocal, "169.254.0.0/16") {
			t.Fatalf("%+v: link-local not denied", p)
		}
	}
	for _, p := range []Policy{{}, {AllWebsites: true}, {AllWebsites: true, LocalNetwork: true}} {
		if c := squidConf(p); strings.Contains(c, "ssl_bump") || strings.Contains(c, "include") {
			t.Errorf("%+v: conf intercepts TLS or includes other files", p)
		}
	}
}

// 002-AC36 003b-AC16: the denied list comes from squid's access log lines, newest first, the last n
// only; squid's own messages are skipped; tunnels let through count as hits by host.
func TestParseEgress(t *testing.T) {
	log := strings.Join([]string{
		"2026/10/11 03:51:29| Accepting HTTP Socket connections",
		"grill-egress 1791690691 TCP_TUNNEL/200 CONNECT pypi.org:443",
		"grill-egress 1791690691 TCP_TUNNEL/200 CONNECT PyPI.org:443",
		"grill-egress 1791690691 TCP_TUNNEL_ABORTED/200 CONNECT files.pythonhosted.org:443",
		"grill-egress 1791690691 NONE_NONE/000 CONNECT never.example:443",
		"grill-egress 1791690692 TCP_DENIED/403 CONNECT api.anthropic.com:443",
		"grill-egress 1791690693 TCP_DENIED/403 GET http://example.com/path",
		"grill-egress 1791690694 TCP_DENIED/403 CONNECT example.org:8443",
		"grill-egress 1791690694 TCP_DENIED_ABORTED/403 CONNECT aborted.example:443",
		"grill-egress notanumber TCP_DENIED/403 CONNECT x.com:443",
		"grill-egress 1791690695 TCP_DENIED/403 CONNECT",
		"other 1791690696 TCP_DENIED/403 CONNECT y.com:443",
	}, "\n")
	eg := parseEgress([]byte(log), 10)
	got := eg.Denied
	if want := map[string]int{"pypi.org": 2, "files.pythonhosted.org": 1}; !maps.Equal(eg.Hits, want) {
		t.Fatalf("hits = %v, want %v", eg.Hits, want)
	}
	want := []Denied{
		{"aborted.example", time.Unix(1791690694, 0).UTC()},
		{"example.org:8443", time.Unix(1791690694, 0).UTC()},
		{"example.com", time.Unix(1791690693, 0).UTC()},
		{"api.anthropic.com", time.Unix(1791690692, 0).UTC()},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v", got)
	}
	if got := parseEgress([]byte(log), 1).Denied; len(got) != 1 || got[0].Host != "aborted.example" {
		t.Fatalf("last 1 = %+v", got)
	}
	if got := parseEgress(nil, 5); got.Denied == nil || len(got.Denied) != 0 || got.Hits == nil {
		t.Fatalf("empty log = %#v", got)
	}
}

// 002-AC34 002-AC36 014-AC1: a policy set before the start is the one squid starts with; once
// started, a new one reaches the running squid, parsed before it replaces the old one; a bad
// list never reaches docker; the local network never comes without every website.
func TestSetPolicy(t *testing.T) {
	f := &fakeDocker{reply: func(args []string) ([]byte, error) {
		switch {
		case args[0] == "info":
			return []byte("29|Linux"), nil
		case args[0] == "network" && args[1] == "inspect":
			return []byte("127.0.0.1"), nil
		case args[0] == "logs":
			return []byte("grill-egress 1791690692 TCP_DENIED/403 CONNECT example.com:443\n"), nil
		}
		return nil, nil
	}}
	s := New(Config{Image: "img", Name: "t", Proxy: testProxy}, f.run, zap.NewNop())
	ctx := context.Background()
	if d, err := s.Egress(ctx, 5); err != nil || len(d.Denied) != 0 {
		t.Fatalf("denied before start = %v, %v", d, err)
	}
	if err := s.SetPolicy(ctx, Policy{Allow: []string{"Example.com"}, AllWebsites: true}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("docker called before the start: %v", f.calls)
	}
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx)
	run := f.find("run", "-d", "--name", "grill-room-t-egress")
	if !slices.Contains(run, "GRILL_SQUID_CONF="+squidConf(Policy{Allow: []string{"example.com"}, AllWebsites: true})) {
		t.Fatalf("egress run = %v", run)
	}
	if err := s.SetPolicy(ctx, Policy{Allow: []string{"*.bad"}}); err == nil {
		t.Fatal("bad list accepted")
	}
	if c := f.find("exec", "-i", "grill-room-t-egress"); c != nil {
		t.Fatalf("bad list reached docker: %v", c)
	}
	// 014-AC16: a room's list adds to the server's past one list's bound.
	merged := make([]string, maxAllowlist+5)
	for i := range merged {
		merged[i] = "h" + strconv.Itoa(i) + ".example.com"
	}
	if err := s.SetPolicy(ctx, Policy{Allow: merged}); err != nil {
		t.Fatalf("merged lists refused: %v", err)
	}
	if err := s.SetPolicy(ctx, Policy{Allow: []string{"pypi.org"}, LocalNetwork: true}); err != nil {
		t.Fatal(err)
	}
	c := f.find("exec", "-i", "grill-room-t-egress")
	script := c[len(c)-1]
	if !strings.Contains(script, "squid -k parse -f /tmp/squid.conf.new") || !strings.Contains(script, "&& squid -k reconfigure") {
		t.Fatalf("reconfigure script = %q", script)
	}
	if last := f.stdin[len(f.stdin)-1]; last != squidConf(Policy{Allow: []string{"pypi.org"}}) {
		t.Fatalf("conf sent = %q", last)
	}
	eg, err := s.Egress(ctx, 5)
	d := eg.Denied
	if err != nil || len(d) != 1 || d[0].Host != "example.com" {
		t.Fatalf("denied = %v, %v", d, err)
	}
}
