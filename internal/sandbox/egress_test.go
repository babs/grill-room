package sandbox

import (
	"context"
	"net/url"
	"slices"
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

// 002-AC35: squid denies Anthropic before any allow, then anything but CONNECT to 443, then
// anything off the list; an empty list allows nothing.
func TestSquidConf(t *testing.T) {
	conf := squidConf([]string{"github.com", "pypi.org"})
	var access []string
	for l := range strings.SplitSeq(conf, "\n") {
		if strings.HasPrefix(l, "http_access ") {
			access = append(access, l)
		}
	}
	want := []string{"http_access deny anthropic", "http_access deny !CONNECT", "http_access deny !SSL_ports", "http_access allow allowlist", "http_access deny all"}
	if !slices.Equal(access, want) {
		t.Fatalf("http_access rules = %q", access)
	}
	// -n on both: a reverse lookup would let a raw IP whose PTR names an allowed domain through.
	for _, line := range []string{"acl anthropic dstdomain -n .anthropic.com .claude.ai platform.claude.com", "acl allowlist dstdomain -n github.com pypi.org", "acl SSL_ports port 443"} {
		if !strings.Contains(conf, line+"\n") {
			t.Errorf("conf lacks %q", line)
		}
	}
	if strings.Contains(conf, "ssl_bump") || strings.Contains(conf, "include") {
		t.Error("conf intercepts TLS or includes other files")
	}
	empty := squidConf(nil)
	if strings.Contains(empty, "allowlist") || !strings.HasSuffix(empty, "http_access deny all\n") {
		t.Fatalf("empty list conf:\n%s", empty)
	}
}

// 002-AC36: the denied list comes from squid's access log lines, newest first, the last n only;
// allowed requests and squid's own messages are skipped.
func TestParseDenied(t *testing.T) {
	log := strings.Join([]string{
		"2026/10/11 03:51:29| Accepting HTTP Socket connections",
		"grill-egress 1791690691 TCP_TUNNEL/200 CONNECT pypi.org:443",
		"grill-egress 1791690692 TCP_DENIED/403 CONNECT api.anthropic.com:443",
		"grill-egress 1791690693 TCP_DENIED/403 GET http://example.com/path",
		"grill-egress 1791690694 TCP_DENIED/403 CONNECT example.org:8443",
		"grill-egress 1791690694 TCP_DENIED_ABORTED/403 CONNECT aborted.example:443",
		"grill-egress notanumber TCP_DENIED/403 CONNECT x.com:443",
		"grill-egress 1791690695 TCP_DENIED/403 CONNECT",
		"other 1791690696 TCP_DENIED/403 CONNECT y.com:443",
	}, "\n")
	got := parseDenied([]byte(log), 10)
	want := []Denied{
		{"aborted.example", time.Unix(1791690694, 0).UTC()},
		{"example.org:8443", time.Unix(1791690694, 0).UTC()},
		{"example.com", time.Unix(1791690693, 0).UTC()},
		{"api.anthropic.com", time.Unix(1791690692, 0).UTC()},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v", got)
	}
	if got := parseDenied([]byte(log), 1); len(got) != 1 || got[0].Host != "aborted.example" {
		t.Fatalf("last 1 = %+v", got)
	}
	if got := parseDenied(nil, 5); got == nil || len(got) != 0 {
		t.Fatalf("empty log = %#v", got)
	}
}

// 002-AC34 002-AC36: an allowlist set before the start is the one squid starts with; once
// started, a new list reaches the running squid, parsed before it replaces the old one; a bad
// list never reaches docker.
func TestSetAllowlist(t *testing.T) {
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
	up, _ := url.Parse("https://x")
	s := New(Config{Image: "img", Name: "t", Upstream: up}, f.run, testCreds(t, "x", time.Now().Add(time.Hour)), zap.NewNop())
	ctx := context.Background()
	if d, err := s.Denied(ctx, 5); err != nil || len(d) != 0 {
		t.Fatalf("denied before start = %v, %v", d, err)
	}
	if err := s.SetAllowlist(ctx, []string{"Example.com"}); err != nil {
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
	if !slices.Contains(run, "GRILL_SQUID_CONF="+squidConf([]string{"example.com"})) {
		t.Fatalf("egress run = %v", run)
	}
	if err := s.SetAllowlist(ctx, []string{"*.bad"}); err == nil {
		t.Fatal("bad list accepted")
	}
	if c := f.find("exec", "-i", "grill-room-t-egress"); c != nil {
		t.Fatalf("bad list reached docker: %v", c)
	}
	if err := s.SetAllowlist(ctx, []string{"pypi.org"}); err != nil {
		t.Fatal(err)
	}
	c := f.find("exec", "-i", "grill-room-t-egress")
	script := c[len(c)-1]
	if !strings.Contains(script, "squid -k parse -f /tmp/squid.conf.new") || !strings.Contains(script, "&& squid -k reconfigure") {
		t.Fatalf("reconfigure script = %q", script)
	}
	if last := f.stdin[len(f.stdin)-1]; last != squidConf([]string{"pypi.org"}) {
		t.Fatalf("conf sent = %q", last)
	}
	d, err := s.Denied(ctx, 5)
	if err != nil || len(d) != 1 || d[0].Host != "example.com" {
		t.Fatalf("denied = %v, %v", d, err)
	}
}
