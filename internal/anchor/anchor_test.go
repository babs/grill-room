package anchor

import (
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/render"
)

func parse(t *testing.T, page string) *Doc {
	t.Helper()
	d, err := Parse([]byte(page))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func md(t *testing.T, src string) *Doc {
	t.Helper()
	page, err := render.MdToHTML([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return parse(t, string(page))
}

func TestValidate(t *testing.T) {
	long := func(n int) string { return strings.Repeat("é", n) }
	cases := map[string]struct {
		a  Anchor
		ok bool
	}{
		"text":                     {Anchor{Kind: KindText, Exact: "rollback", Prefix: "needs a ", Suffix: " step"}, true},
		"text at the limits":       {Anchor{Kind: KindText, Exact: long(MaxExact), Prefix: long(MaxContext), Suffix: long(MaxContext)}, true},
		"exact over the limit":     {Anchor{Kind: KindText, Exact: long(MaxExact + 1)}, false},
		"prefix over the limit":    {Anchor{Kind: KindText, Exact: "x", Prefix: long(MaxContext + 1)}, false},
		"blank exact":              {Anchor{Kind: KindText, Exact: "  "}, false},
		"invalid utf-8":            {Anchor{Kind: KindText, Exact: "\xff"}, false},
		"text with a path":         {Anchor{Kind: KindText, Exact: "x", Path: "body"}, false},
		"element":                  {Anchor{Kind: KindElement, Path: "body > div:nth-of-type(2) > p:nth-of-type(1)", Tag: "p", Fingerprint: "Hi"}, true},
		"body element":             {Anchor{Kind: KindElement, Path: "body", Tag: "body"}, true},
		"custom element":           {Anchor{Kind: KindElement, Path: "body > my-card:nth-of-type(1)", Tag: "my-card"}, true},
		"tag is not the last step": {Anchor{Kind: KindElement, Path: "body > div:nth-of-type(1)", Tag: "p"}, false},
		"selector injection":       {Anchor{Kind: KindElement, Path: "body > p:nth-of-type(1), script", Tag: "p"}, false},
		"zero index":               {Anchor{Kind: KindElement, Path: "body > p:nth-of-type(0)", Tag: "p"}, false},
		"path not from body":       {Anchor{Kind: KindElement, Path: "html > body", Tag: "body"}, false},
		"path too deep":            {Anchor{Kind: KindElement, Path: "body" + strings.Repeat(" > div:nth-of-type(1)", 65), Tag: "div"}, false},
		"fingerprint too long":     {Anchor{Kind: KindElement, Path: "body", Tag: "body", Fingerprint: long(MaxFingerprint + 1)}, false},
		"element with a quote":     {Anchor{Kind: KindElement, Path: "body", Tag: "body", Exact: "x"}, false},
		"unknown kind":             {Anchor{Kind: "range", Exact: "x"}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := tc.a.Validate(); (err == nil) != tc.ok {
				t.Fatalf("Validate() = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

// The quoted text is what the browser's model holds: no diagram, formula, script, button or
// hidden text, whitespace folded.
func TestTextModelSkipsWhatIsNotTheDocument(t *testing.T) {
	d := parse(t, `<html><head><title>T</title></head><body>
		<p>Keep   this
		line</p><script>var no;</script><button>Copy</button><div class="mermaid">graph</div>
		<span class="math inline">x^2</span><p hidden>secret</p><del class="gg-del">old</del><del>struck</del>
		<style>p{}</style><noscript>enable js</noscript><template>tpl</template><p>end</p></body></html>`)
	if want := "Keep this line struck end"; d.text != want {
		t.Fatalf("text = %q, want %q", d.text, want)
	}
}

const plan = `# Rollout plan

The rollout needs a **rollback** step before the freeze.

Owners sign off on the plan.
`

// 001-AC21: a quote follows its text across revisions, re-anchors on context when reworded, and
// is orphaned, unchanged, when the text is gone.
func TestReanchorText(t *testing.T) {
	a := Anchor{Kind: KindText, Exact: "rollback step", Prefix: "Rollout plan The rollout needs a ", Suffix: " before the freeze. Owners sign"}
	cases := map[string]struct {
		next      string
		ok        bool
		wantExact string
	}{
		"unchanged":       {plan, true, "rollback step"},
		"moved down":      {"# Intro\n\nNew paragraph first.\n\n" + plan, true, "rollback step"},
		"re-wrapped":      {"# Rollout plan\n\nThe rollout needs\na **rollback**\nstep before the freeze.\n", true, "rollback step"},
		"reworded":        {"# Rollout plan\n\nThe rollout needs a tested way back before the freeze.\n\nOwners sign off on the plan.\n", true, "tested way back"},
		"reworded longer": {"# Rollout plan\n\nThe rollout needs a documented, rehearsed and timed rollback procedure before the freeze.\n\nOwners sign off on the plan.\n", false, ""},
		"removed":         {"# Rollout plan\n\nOwners sign off on the plan.\n", false, ""},
		"context changed": {"# Plan\n\nWe will see.\n", false, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok := md(t, tc.next).Reanchor(a)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (got %+v)", ok, tc.ok, got)
			}
			if !ok {
				if got != a {
					t.Fatalf("an orphan must keep its anchor: got %+v", got)
				}
				return
			}
			if got.Exact != tc.wantExact {
				t.Fatalf("exact = %q, want %q", got.Exact, tc.wantExact)
			}
			if got.Prefix != "Rollout plan The rollout needs a " && !strings.HasSuffix(got.Prefix, "The rollout needs a ") {
				t.Fatalf("prefix not refreshed: %q", got.Prefix)
			}
			if !strings.HasPrefix(got.Suffix, " before the freeze.") {
				t.Fatalf("suffix not refreshed: %q", got.Suffix)
			}
		})
	}
}

// Of two identical quotes, the context picks the one annotated; re-anchoring keeps it there.
func TestReanchorTextPicksByContext(t *testing.T) {
	d := md(t, "Ship it on Monday.\n\nTest it on Monday.\n")
	got, ok := d.Reanchor(Anchor{Kind: KindText, Exact: "on Monday", Prefix: "Test it ", Suffix: "."})
	if !ok || got.Prefix != "Ship it on Monday. Test it " {
		t.Fatalf("got %+v, %v: want the second Monday", got, ok)
	}
}

// A quote at the very start or end of the document has an empty prefix or suffix; rewording it
// still re-anchors.
func TestReanchorTextAtDocumentEdges(t *testing.T) {
	d := md(t, "# Launch checklist\n\nBody text here.\n")
	got, ok := d.Reanchor(Anchor{Kind: KindText, Exact: "Rollout plan", Suffix: " Body text here."})
	if !ok || got.Exact != "Launch checklist" || got.Prefix != "" {
		t.Fatalf("start: got %+v, %v", got, ok)
	}
	got, ok = d.Reanchor(Anchor{Kind: KindText, Exact: "Body copy.", Prefix: "Launch checklist "})
	if !ok || got.Exact != "Body text here." || strings.TrimSpace(got.Suffix) != "" {
		t.Fatalf("end: got %+v, %v", got, ok)
	}
}

const page = `<!doctype html><html><head><title>Plan</title></head><body>
<header><h1>Plan</h1></header>
<main>
  <p>Intro paragraph.</p>
  <ul><li>Freeze the schema</li><li>Ship behind a flag</li></ul>
  <img src="chart.png" alt="Risk chart">
  <div></div>
</main>
</body></html>`

// 001-AC19: an element anchor survives a re-render that moves the element, and is orphaned when
// the element is gone.
func TestReanchorElement(t *testing.T) {
	d := parse(t, page)
	li := Anchor{Kind: KindElement, Path: "body > main:nth-of-type(1) > ul:nth-of-type(1) > li:nth-of-type(2)", Tag: "li", Fingerprint: "Ship behind a flag"}
	img := Anchor{Kind: KindElement, Path: "body > main:nth-of-type(1) > img:nth-of-type(1)", Tag: "img", Fingerprint: "@Risk chart"}
	empty := Anchor{Kind: KindElement, Path: "body > main:nth-of-type(1) > div:nth-of-type(1)", Tag: "div"}
	for _, a := range []Anchor{li, img, empty} {
		if got, ok := d.Reanchor(a); !ok || got != a {
			t.Fatalf("same page: got %+v, %v, want %+v", got, ok, a)
		}
	}

	cases := map[string]struct {
		page     string
		a        Anchor
		ok       bool
		wantPath string
	}{
		"sibling inserted before": {
			strings.Replace(page, "<li>Freeze", "<li>Back up</li><li>Freeze", 1), li, true,
			"body > main:nth-of-type(1) > ul:nth-of-type(1) > li:nth-of-type(3)",
		},
		"wrapped in a new section": {
			strings.Replace(strings.Replace(page, "<ul>", "<section><ul>", 1), "</ul>", "</ul></section>", 1), li, true,
			"body > main:nth-of-type(1) > section:nth-of-type(1) > ul:nth-of-type(1) > li:nth-of-type(2)",
		},
		"image moved to the header": {
			strings.Replace(strings.Replace(page, `<img src="chart.png" alt="Risk chart">`, "", 1), "<h1>Plan</h1>", `<h1>Plan</h1><img src="chart.png" alt="Risk chart">`, 1), img, true,
			"body > header:nth-of-type(1) > img:nth-of-type(1)",
		},
		"element removed":          {strings.Replace(page, "<li>Ship behind a flag</li>", "", 1), li, false, ""},
		"text changed":             {strings.Replace(page, "Ship behind a flag", "Ship dark", 1), li, false, ""},
		"empty element moved away": {strings.Replace(page, "<div></div>", "", 1), empty, false, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok := parse(t, tc.page).Reanchor(tc.a)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (got %+v)", ok, tc.ok, got)
			}
			if !ok {
				if got != tc.a {
					t.Fatalf("an orphan must keep its anchor: got %+v", got)
				}
				return
			}
			if got.Path != tc.wantPath || got.Tag != tc.a.Tag || got.Fingerprint != tc.a.Fingerprint {
				t.Fatalf("got %+v, want path %q", got, tc.wantPath)
			}
			if err := got.Validate(); err != nil {
				t.Fatalf("re-anchored element does not validate: %v", err)
			}
		})
	}
}

func TestFingerprintCutsAt80Runes(t *testing.T) {
	d := parse(t, "<p>"+strings.Repeat("é", 100)+"</p>")
	got, ok := d.Reanchor(Anchor{Kind: KindElement, Path: "body > p:nth-of-type(1)", Tag: "p", Fingerprint: strings.Repeat("é", 80)})
	if !ok || got.Fingerprint != strings.Repeat("é", 80) {
		t.Fatalf("got %+v, %v", got, ok)
	}
}

func TestReanchorUnknownKind(t *testing.T) {
	if _, ok := parse(t, page).Reanchor(Anchor{Kind: "range"}); ok {
		t.Fatal("unknown kind anchored")
	}
}

func TestFramesetPageHoldsNothing(t *testing.T) {
	d := parse(t, `<html><frameset><frame src="a.html"></frameset></html>`)
	if _, ok := d.Reanchor(Anchor{Kind: KindText, Exact: "a"}); ok {
		t.Fatal("text found in a frameset")
	}
	if _, ok := d.Reanchor(Anchor{Kind: KindElement, Path: "body", Tag: "body"}); !ok {
		t.Fatal("the empty body itself should resolve")
	}
}
