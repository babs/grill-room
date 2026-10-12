package sandbox

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

// officeScript writes and reads back an .xlsx, a .docx, a .pptx and a PDF from matplotlib.
const officeScript = `cd /tmp && python3 - <<'PY'
import openpyxl, docx, pptx, pypdf, pandas, matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
wb = openpyxl.Workbook(); wb.active["A1"] = "grill"; wb.active["B1"] = 42; wb.save("t.xlsx")
assert openpyxl.load_workbook("t.xlsx").active["B1"].value == 42
assert pandas.read_excel("t.xlsx", header=None).iloc[0, 1] == 42
d = docx.Document(); d.add_paragraph("room notes"); d.save("t.docx")
assert docx.Document("t.docx").paragraphs[0].text == "room notes"
p = pptx.Presentation(); p.slides.add_slide(p.slide_layouts[5]).shapes.title.text = "deck"; p.save("t.pptx")
assert pptx.Presentation("t.pptx").slides[0].shapes.title.text == "deck"
plt.plot([1, 2]); plt.savefig("t.pdf")
assert len(pypdf.PdfReader("t.pdf").pages) == 1
print("office ok")
PY
pandoc t.docx -t markdown`

// 002-AC33 002-AC34 002-AC35 in a real sandbox, which needs Internet access for the allowed hosts:
// every tool runs; an .xlsx and a .docx are written and read back with no network; uv, pip, npm
// and git fetch through the egress proxy; a host off the list and every Anthropic host are refused
// there; a list change reaches the running proxy; refusals show in the denied list, and squid's
// log shows no Anthropic tunnel; the sandbox and the proxy run under their limits.
func TestEgressDocker(t *testing.T) {
	img := dockerTest(t)
	ctx := context.Background()
	name := fmt.Sprintf("egress-%d", time.Now().UnixNano())
	s := New(Config{Image: img, Name: name, Proxy: "host-gateway:1",
		Limits: Limits{CPUs: "1.5", Memory: "3g", EgressCPUs: "0.5", EgressMemory: "256m"}},
		Docker, zaptest.NewLogger(t, zaptest.Level(zap.WarnLevel)))
	t.Cleanup(func() { s.Close(context.Background()) })
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	// sh runs script as the agent in the sandbox, with extra docker exec flags.
	sh := func(t *testing.T, script string, flags ...string) (string, error) {
		t.Helper()
		ectx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		args := append(append([]string{"exec"}, flags...), s.container(), "bash", "-euo", "pipefail", "-c", script)
		out, err := exec.CommandContext(ectx, "docker", args...).CombinedOutput() //nolint:gosec // test scripts
		return string(out), err
	}
	must := func(t *testing.T, script string, flags ...string) string {
		t.Helper()
		out, err := sh(t, script, flags...)
		if err != nil {
			t.Fatalf("%s: %v\n%s", script, err, out)
		}
		return out
	}
	// connectCode is what the egress proxy answers a CONNECT to host: 200 or 403.
	connectCode := func(t *testing.T, host string) string {
		t.Helper()
		out, _ := sh(t, "curl -s -o /dev/null -m 20 -w '%{http_connect}' https://"+host+"/ || true")
		return strings.TrimSpace(out)
	}
	// waitCode polls until the proxy answers want: squid reconfigures in the background.
	waitCode := func(t *testing.T, host, want string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for got := connectCode(t, host); got != want; got = connectCode(t, host) {
			if time.Now().After(deadline) {
				t.Fatalf("CONNECT %s: %q, want %q", host, got, want)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	// Squid listens a moment after its container starts.
	deadline := time.Now().Add(20 * time.Second)
	for connectCode(t, "example.com") != "403" {
		if time.Now().After(deadline) {
			t.Fatal("the egress proxy never answered")
		}
		time.Sleep(200 * time.Millisecond)
	}

	t.Run("tools", func(t *testing.T) {
		out := must(t, "python3 --version; uv --version; node --version; npm --version; jq --version; yq --version; "+
			"rg --version; git --version; curl --version; pandoc --version; pip --version; command -v python")
		for _, want := range []string{"Python 3.", "uv 0.", "v24.", "jq-1.", "yq (https://github.com/mikefarah/yq/) version v4.",
			"ripgrep 14.", "git version 2.", "curl 8.", "pandoc 3.", "pip ", "/home/agent/.venv/bin/python"} {
			if !strings.Contains(out, want) {
				t.Errorf("tool output lacks %q:\n%s", want, out)
			}
		}
		if out := must(t, "id -u"); strings.TrimSpace(out) != "1000" {
			t.Errorf("runs as uid %s", out)
		}
	})

	t.Run("office files offline", func(t *testing.T) {
		// A dead proxy on top of no route: any network use fails the script.
		dead := "http://127.0.0.1:9"
		out := must(t, officeScript, "-e", "https_proxy="+dead, "-e", "HTTPS_PROXY="+dead, "-e", "http_proxy="+dead, "-e", "HTTP_PROXY="+dead)
		if !strings.Contains(out, "office ok") || !strings.Contains(out, "room notes") {
			t.Fatalf("office script: %s", out)
		}
	})

	t.Run("package managers and git through the proxy", func(t *testing.T) {
		must(t, "uv pip install --no-cache iniconfig==2.0.0 && python -c 'import iniconfig'")
		must(t, "pip install -q --no-cache-dir --disable-pip-version-check tomli-w==1.0.0 && python -c 'import tomli_w'")
		must(t, `cd /tmp && npm install --no-audit --no-fund --cache /tmp/npm-cache is-number@7.0.0 && node -e "require('/tmp/node_modules/is-number')"`)
		must(t, "git clone -q --depth 1 https://github.com/octocat/Hello-World /tmp/hw && test -s /tmp/hw/README")
		must(t, "curl -fsS -o /dev/null https://raw.githubusercontent.com/octocat/Hello-World/master/README")
	})

	t.Run("refused hosts", func(t *testing.T) {
		for _, host := range []string{"example.com", "api.anthropic.com", "anthropic.com", "statsig.anthropic.com", "claude.ai", "platform.claude.com"} {
			if code := connectCode(t, host); code != "403" {
				t.Errorf("CONNECT %s through the proxy: %q, want 403", host, code)
			}
		}
		// Plain HTTP to an allowed host is refused too: CONNECT to 443 only.
		if out, _ := sh(t, "curl -s -o /dev/null -m 20 -w '%{http_code}' http://pypi.org/ || true"); strings.TrimSpace(out) != "403" {
			t.Errorf("plain HTTP to pypi.org: %q", out)
		}
		// Without the proxy there is no route at all (D21).
		if _, err := sh(t, "curl -sS -m 5 --noproxy '*' https://example.com"); err == nil {
			t.Error("example.com reached without the proxy")
		}
		if err := s.Isolated(ctx); err != nil {
			t.Error(err)
		}
	})

	t.Run("allowlist change reaches the running proxy", func(t *testing.T) {
		if err := s.SetAllowlist(ctx, append(slices.Clone(DefaultAllowlist), "example.com")); err != nil {
			t.Fatal(err)
		}
		waitCode(t, "example.com", "200")
		// A raw IP whose PTR is under an allowed domain stays refused: no reverse lookup.
		if err := s.SetAllowlist(ctx, []string{".github.com"}); err != nil {
			t.Fatal(err)
		}
		waitCode(t, "api.github.com", "200")
		ips, err := net.DefaultResolver.LookupHost(ctx, "github.com")
		if err != nil || len(ips) == 0 {
			t.Fatalf("resolve github.com: %v", err)
		}
		if names, _ := net.DefaultResolver.LookupAddr(ctx, ips[0]); len(names) == 0 || !strings.HasSuffix(names[0], ".github.com.") {
			t.Logf("%s has PTR %v, not under .github.com: the IP check proves less", ips[0], names)
		}
		if code := connectCode(t, ips[0]); code != "403" {
			t.Errorf("CONNECT %s with .github.com allowed: %q, want 403", ips[0], code)
		}
		if err := s.SetAllowlist(ctx, DefaultAllowlist); err != nil {
			t.Fatal(err)
		}
		waitCode(t, "example.com", "403")
	})

	t.Run("denied list and log", func(t *testing.T) {
		denied, err := s.Denied(ctx, 50)
		if err != nil {
			t.Fatal(err)
		}
		hosts := map[string]bool{}
		for _, d := range denied {
			hosts[d.Host] = true
			if time.Since(d.Time) > 10*time.Minute || time.Until(d.Time) > time.Minute {
				t.Errorf("denied entry time %s", d.Time)
			}
		}
		for _, want := range []string{"example.com", "api.anthropic.com", "claude.ai", "platform.claude.com", "pypi.org"} {
			if !hosts[want] {
				t.Errorf("denied list lacks %s: %v", want, denied)
			}
		}
		log, err := Docker(ctx, nil, "logs", s.egress())
		if err != nil {
			t.Fatal(err)
		}
		if n := anthropicTunnels(log); n != 0 {
			t.Errorf("%d Anthropic tunnels allowed:\n%s", n, log)
		}
		t.Logf("egress log:\n%s", log)
	})

	t.Run("limits", func(t *testing.T) {
		out, err := Docker(ctx, nil, "inspect", "-f", "{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}} {{.HostConfig.MemorySwap}}", s.container(), s.egress())
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Fields(string(out)); !slices.Equal(got, []string{"1500000000", "3221225472", "3221225472", "500000000", "268435456", "268435456"}) {
			t.Fatalf("limits = %v", got)
		}
	})
}

// anthropicTunnels counts access log lines where squid let a request to an Anthropic host through.
func anthropicTunnels(log []byte) int {
	n := 0
	for l := range strings.SplitSeq(string(log), "\n") {
		f := strings.Fields(l)
		if len(f) != 5 || f[0] != logTag || strings.HasPrefix(f[2], "TCP_DENIED") {
			continue
		}
		host := strings.TrimSuffix(f[4], ":443")
		if u, err := url.Parse(f[4]); err == nil && u.Host != "" {
			host = u.Hostname()
		}
		for _, a := range anthropicDomains {
			if covers(a, host) {
				n++
			}
		}
	}
	return n
}
