package uikit

import (
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func readTokens(t *testing.T) string {
	t.Helper()
	b, err := files.ReadFile("kit/tokens.css")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// 005-AC28: both binaries serve the one file.
func TestHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, Prefix+"tokens.css", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "--primary:") || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("%d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

var decl = regexp.MustCompile(`(--[a-z0-9-]+):\s*([^;]+);`)

var (
	lightBlock = regexp.MustCompile(`(?s)^.*?:root \{(.*?)\}`)
	darkBlock  = regexp.MustCompile(`(?s)@media \(prefers-color-scheme: dark\) \{ :root \{(.*?)\}`)
)

// themes returns the light tokens, and the dark ones laid over them.
func themes(t *testing.T, css string) (light, dark map[string]string) {
	t.Helper()
	l, d := lightBlock.FindStringSubmatch(css), darkBlock.FindStringSubmatch(css)
	if l == nil || d == nil {
		t.Fatal("no light or no dark block")
	}
	light, dark = map[string]string{}, map[string]string{}
	for _, m := range decl.FindAllStringSubmatch(l[1], -1) {
		light[m[1]], dark[m[1]] = m[2], m[2]
	}
	for _, m := range decl.FindAllStringSubmatch(d[1], -1) {
		dark[m[1]] = m[2]
	}
	return light, dark
}

type rgba struct{ r, g, b, a float64 }

var (
	hexRe = regexp.MustCompile(`^#([0-9a-f]{6})$`)
	rgbRe = regexp.MustCompile(`^rgb\((\d+) (\d+) (\d+)(?: / (\.\d+|[01]))?\)$`)
)

func parse(t *testing.T, v string) rgba {
	t.Helper()
	if m := hexRe.FindStringSubmatch(v); m != nil {
		n, _ := strconv.ParseUint(m[1], 16, 32)
		return rgba{float64(n >> 16), float64(n >> 8 & 0xff), float64(n & 0xff), 1}
	}
	if m := rgbRe.FindStringSubmatch(v); m != nil {
		c := rgba{a: 1}
		c.r, _ = strconv.ParseFloat(m[1], 64)
		c.g, _ = strconv.ParseFloat(m[2], 64)
		c.b, _ = strconv.ParseFloat(m[3], 64)
		if m[4] != "" {
			c.a, _ = strconv.ParseFloat(m[4], 64)
		}
		return c
	}
	t.Fatalf("not a colour: %q", v)
	return rgba{}
}

// over lays a translucent colour on an opaque one, as a tint shows on its surface.
func over(c, base rgba) rgba {
	return rgba{c.r*c.a + base.r*(1-c.a), c.g*c.a + base.g*(1-c.a), c.b*c.a + base.b*(1-c.a), 1}
}

func luminance(c rgba) float64 {
	ch := func(v float64) float64 {
		v /= 255
		if v <= 0.03928 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(c.r) + 0.7152*ch(c.g) + 0.0722*ch(c.b)
}

func ratio(a, b rgba) float64 {
	x, y := luminance(a), luminance(b)
	if x < y {
		x, y = y, x
	}
	return (x + 0.05) / (y + 0.05)
}

// 005-AC29: every text pair the UIs draw reaches 4.5:1 and every non-text pair 3:1, light and
// dark, computed from the tokens file itself.
func TestTokenContrast(t *testing.T) {
	text := [][2]string{
		{"--text", "--bg"}, {"--text", "--surface"}, {"--text-muted", "--bg"}, {"--text-muted", "--surface"},
		{"--on-primary", "--primary"}, {"--primary-text", "--bg"}, {"--primary-text", "--surface"}, {"--primary-text", "--primary-bg"},
		{"--warning", "--bg"}, {"--warning", "--warning-bg"}, {"--danger", "--surface"}, {"--danger", "--bg"}, {"--danger", "--danger-bg"},
		{"--on-danger", "--danger"}, {"--success", "--surface"}, {"--chrome-text", "--chrome"}, {"--chrome-muted", "--chrome"},
		{"--on-accent", "--accent"}, {"--on-flame-soft", "--flame-soft"}, {"--text", "--primary-bg"}, {"--text", "--warning-bg"},
		{"--text", "--danger-bg"}, {"--text", "--info-bg"}, {"--text", "--highlight"}, {"--text", "--border"},
	}
	nonText := [][2]string{
		{"--border-strong", "--bg"}, {"--border-strong", "--surface"}, {"--brand", "--surface"}, {"--brand", "--chrome"},
		{"--dot-ring", "--surface"}, {"--dot-ring", "--bg"}, {"--primary", "--surface"},
	}
	light, dark := themes(t, readTokens(t))
	for name, th := range map[string]map[string]string{"light": light, "dark": dark} {
		surface := parse(t, th["--surface"])
		col := func(k string) rgba {
			v, ok := th[k]
			if !ok {
				t.Fatalf("%s: no token %s", name, k)
			}
			return over(parse(t, strings.TrimSpace(v)), surface)
		}
		for _, set := range []struct {
			pairs [][2]string
			min   float64
		}{{text, 4.5}, {nonText, 3}} {
			for _, p := range set.pairs {
				if r := ratio(col(p[0]), col(p[1])); r < set.min {
					t.Errorf("%s: %s on %s is %.2f:1, under %.1f", name, p[0], p[1], r, set.min)
				}
			}
		}
	}
}

// The ratios the design doc quotes, so that the computation itself is checked.
func TestRatioMatchesKnownPairs(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want float64
	}{{"#ffffff", "#159f8c", 3.30}, {"#ffffff", "#0f7d6e", 5.03}, {"#0f7d6e", "#f7f6f2", 4.65}} {
		if r := ratio(parse(t, c.a), parse(t, c.b)); math.Abs(r-c.want) > 0.02 {
			t.Errorf("%s on %s: %.2f, want %.2f", c.a, c.b, r, c.want)
		}
	}
}

// colourLiteral finds a colour written out: hex, rgb(), hsl() or a named colour other than the
// keywords that are no colour.
var colourLiteral = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|\b(?:rgba?|hsla?|hwb|lab|lch|oklab|oklch|color)\(|` +
	`(?:^|[\s:,(])(?:white|black|red|green|blue|grey|gray|orange|yellow|teal|navy|purple|silver|crimson|maroon|olive|lime|aqua|fuchsia|pink|brown|gold)(?:$|[\s;,)!])`)

// 005-AC28: page stylesheets lay out only; every colour comes from a token.
func TestPageCSSHasNoColourLiterals(t *testing.T) {
	var pages []string
	for _, glob := range []string{"../web/ui/*.css", "../web/ui/admin/*.css", "../daemon/ui/*.css"} {
		m, err := filepath.Glob(glob)
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, m...)
	}
	if len(pages) < 3 {
		t.Fatalf("found only %v", pages)
	}
	for _, p := range pages {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if colourLiteral.MatchString(line) {
				t.Errorf("%s:%d: colour literal: %s", p, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// The guard itself, on the shapes the stylesheets held before 005-AC28.
func TestColourLiteralShapes(t *testing.T) {
	for _, s := range []string{"color: #b3261e;", "background:#fff;", "outline: 2px solid black;", "box-shadow: 0 0 0 2px rgba(52, 199, 89, 0.3);",
		"background: color-mix(in srgb, var(--persona) 15%, white);", "color: white;", "border: 1px solid #CFD2DC",
		"color: oklch(70% 0.1 180);", "background: teal;", "border-color: crimson !important;"} {
		if !colourLiteral.MatchString(s) {
			t.Errorf("missed %q", s)
		}
	}
	for _, s := range []string{"color: var(--danger);", "background: transparent;", "content: attr(data-badge);", "#manage h2 { font-size: 1rem; }", "white-space: nowrap;",
		"background: color-mix(in srgb, var(--persona) 15%, var(--surface));"} {
		if colourLiteral.MatchString(s) {
			t.Errorf("flagged %q", s)
		}
	}
}
