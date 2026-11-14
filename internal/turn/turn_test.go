package turn

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// 001-AC16: every shape of upload change is refused; documents, replies and scratch files are
// sorted by the contract.
func TestClassify(t *testing.T) {
	o := Classify([]Change{
		{Path: "uploads/1-brief.pdf"},
		{Path: "uploads/2-plan.docx.md", Deleted: true},
		{Path: "uploads/new.md", Content: []byte("x")},
		{Path: "docs/../uploads/3-a.png"},
		{Path: "./docs/plan.md", Content: []byte("# Plan")},
		{Path: "docs/page.HTML", Content: []byte("<p>x</p>")},
		{Path: "docs/sub/x.md", Content: []byte("x")},
		{Path: "docs/notes.txt", Content: []byte("x")},
		{Path: "docs/gone.md", Deleted: true},
		{Path: "docs/bin.md", Content: []byte{0xff, 0xfe}},
		{Path: "docs/big.md", Content: make([]byte, MaxDocBytes+1)},
		{Path: "docs/nul.md", Content: []byte("# a\x00b")},
		{Path: "docs/nul.html", Content: []byte("<p>\x00</p>")},
		{Path: "transcript/2026-10-10.md", Content: []byte("edited")},
		{Path: "scratch.txt"},
		{Path: RepliesFile, Content: []byte(`[{"annotation": 4, "body": " ok "}, {"annotation": 0, "body": "x"}, {"annotation": 5, "body": ""}]`)},
	})
	if want := []string{"uploads/1-brief.pdf", "uploads/2-plan.docx.md", "uploads/new.md", "uploads/3-a.png"}; !slices.Equal(o.Rejected, want) {
		t.Fatalf("rejected %v, want %v", o.Rejected, want)
	}
	if len(o.Docs) != 2 || o.Docs[0] != (Doc{Name: "plan.md", Format: "md", Body: "# Plan"}) || o.Docs[1].Format != "html" {
		t.Fatalf("docs %+v", o.Docs)
	}
	if len(o.Replies) != 1 || o.Replies[0] != (Reply{Annotation: 4, Body: "ok"}) {
		t.Fatalf("replies %+v", o.Replies)
	}
	wantIgnored := []string{"docs/sub/x.md", "docs/notes.txt", "docs/gone.md", "docs/bin.md", "docs/big.md", "docs/nul.md", "docs/nul.html", RepliesFile}
	if len(o.Ignored) != len(wantIgnored) {
		t.Fatalf("ignored %q", o.Ignored)
	}
	for i, p := range wantIgnored {
		if !strings.HasPrefix(o.Ignored[i], p+": ") {
			t.Fatalf("ignored[%d] = %q, want %s", i, o.Ignored[i], p)
		}
	}
}

func TestClassifyBadReplies(t *testing.T) {
	for _, body := range []string{`{"annotation": 1}`, `not json`, `[{"annotation": "x"}]`} {
		o := Classify([]Change{{Path: RepliesFile, Content: []byte(body)}})
		if len(o.Replies) != 0 || len(o.Ignored) != 1 {
			t.Fatalf("%s: %+v", body, o)
		}
	}
	long := strings.Repeat("é", MaxReplyLen+10)
	o := Classify([]Change{{Path: RepliesFile, Content: []byte(`[{"annotation": 1, "body": "` + long + `"}]`)}})
	if len([]rune(o.Replies[0].Body)) != MaxReplyLen {
		t.Fatalf("reply not cut: %d runes", len([]rune(o.Replies[0].Body)))
	}
	// A deleted replies file is scratch.
	if o := Classify([]Change{{Path: RepliesFile, Deleted: true}}); len(o.Ignored)+len(o.Replies) != 0 {
		t.Fatalf("deleted replies: %+v", o)
	}
}

func TestValidDocName(t *testing.T) {
	for name, want := range map[string]bool{
		"plan.md": true, "Synthèse v2.html": true, "a.markdown": true, "x.htm": true,
		"plan": false, ".md": false, ".hidden.md": false, "a/b.md": false, `a\b.md`: false, " a.md": false,
		"a\x00.md": false, "a\u202e.md": false, strings.Repeat("a", MaxDocName) + ".md": false, "a.pdf": false,
	} {
		if got := ValidDocName(name); got != want {
			t.Errorf("ValidDocName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestNeedsContent(t *testing.T) {
	for p, want := range map[string]bool{"docs/a.md": true, RepliesFile: true, DriftFile: true, RoomNewFile: true, "uploads/1-a.pdf": false, "transcript/x.md": false, "docsx/a.md": false} {
		if NeedsContent(p) != want {
			t.Errorf("NeedsContent(%q) != %v", p, want)
		}
	}
}

// 001-AC39: replies may close their thread, and annotations-new.json opens the agent's own
// annotations; malformed entries are reported, never fatal.
func TestClassifyAgentAnnotations(t *testing.T) {
	long := strings.Repeat("a", 70)
	o := Classify([]Change{
		{Path: RepliesFile, Content: []byte(`[{"annotation": 4, "body": "settled", "status": "done"}, {"annotation": 5, "body": "x", "status": "open"}]`)},
		{Path: NewAnnotationsFile, Content: []byte(`[
			{"doc": "uploads/3-plan.md", "exact": " the second\n  style sheet ", "prefix": "` + long + `b", "suffix": "c` + long + `", "body": " Who owns it? "},
			{"doc": "./docs/plan-revised.md", "exact": "flag", "body": "On by default?"},
			{"doc": "", "exact": "x", "body": "y"},
			{"doc": "docs/a.md", "exact": "  ", "body": "y"},
			{"doc": "docs/a.md", "exact": "x", "body": ""}
		]`)},
	})
	if len(o.Replies) != 1 || o.Replies[0] != (Reply{Annotation: 4, Body: "settled", Status: "done"}) {
		t.Fatalf("replies %+v", o.Replies)
	}
	want := []NewAnnotation{
		{Doc: "uploads/3-plan.md", Exact: "the second style sheet", Prefix: long[7:] + "b", Suffix: "c" + long[:63], Body: "Who owns it?"},
		{Doc: "docs/plan-revised.md", Exact: "flag", Body: "On by default?"},
	}
	if !slices.Equal(o.Annotations, want) {
		t.Fatalf("annotations %+v,\nwant %+v", o.Annotations, want)
	}
	if len(o.Ignored) != 2 || !strings.Contains(o.Ignored[0], `status other than "done"`) || !strings.Contains(o.Ignored[1], "3 annotations without") {
		t.Fatalf("ignored %q", o.Ignored)
	}
	if !NeedsContent(NewAnnotationsFile) {
		t.Fatal("annotations-new.json needs its content")
	}
	for _, body := range []string{`{}`, `nope`} {
		if o := Classify([]Change{{Path: NewAnnotationsFile, Content: []byte(body)}}); len(o.Annotations) != 0 || len(o.Ignored) != 1 {
			t.Fatalf("%s: %+v", body, o)
		}
	}
	var many strings.Builder
	many.WriteString("[")
	for i := range MaxNewAnnotations + 2 {
		if i > 0 {
			many.WriteString(",")
		}
		many.WriteString(`{"doc": "docs/a.md", "exact": "x", "body": "y"}`)
	}
	many.WriteString("]")
	if o := Classify([]Change{{Path: NewAnnotationsFile, Content: []byte(many.String())}}); len(o.Annotations) != MaxNewAnnotations ||
		len(o.Ignored) != 1 || !strings.Contains(o.Ignored[0], "2 annotations past") {
		t.Fatalf("over the cap: %d annotations, %q", len(o.Annotations), o.Ignored)
	}
}

// 002-AC37: a workspace path stays inside the workspace.
func TestValidPath(t *testing.T) {
	for p, want := range map[string]bool{
		"docs/plan.md": true, "uploads/3-a b.pdf": true, "annotations.md": true, `uploads/1-a\b`: true,
		"": false, ".": false, "..": false, "../x": false, "docs/../../x": false, "/etc/passwd": false,
		"docs/": false, "./docs/x": false, "docs//x": false, "docs/./x": false,
	} {
		if ValidPath(p) != want {
			t.Errorf("ValidPath(%q) = %v", p, !want)
		}
	}
}

// 013-AC2: spoken.md travels with its content and comes out of Classify as text; an oversized or
// non-UTF-8 one is ignored with a reason, a deleted one is nothing.
func TestSpokenFile(t *testing.T) {
	if !NeedsContent(SpokenFile) || NeedsContent("spoken.txt") || NeedsContent("docs-spoken.md") {
		t.Fatal("only spoken.md travels with its content")
	}
	if o := Classify([]Change{{Path: SpokenFile, Content: []byte("J'ai relu la spec.")}}); o.Spoken != "J'ai relu la spec." || len(o.Ignored) != 0 {
		t.Errorf("spoken.md: %+v", o)
	}
	if o := Classify([]Change{{Path: "./" + SpokenFile, Content: make([]byte, MaxSpokenBytes)}}); len(o.Spoken) != MaxSpokenBytes {
		t.Errorf("a spoken.md of exactly the cap is kept: %d", len(o.Spoken))
	}
	for _, c := range []Change{
		{Path: SpokenFile, Content: make([]byte, MaxSpokenBytes+1)},
		{Path: SpokenFile, Content: []byte{0xff, 0xfe}},
	} {
		if o := Classify([]Change{c}); o.Spoken != "" || len(o.Ignored) != 1 {
			t.Errorf("refused spoken.md: %+v", o)
		}
	}
	if o := Classify([]Change{{Path: SpokenFile, Deleted: true}}); o.Spoken != "" || len(o.Ignored) != 0 {
		t.Errorf("deleted spoken.md: %+v", o)
	}
}

// 032-AC13: drift.json reaches the server raw, never listed by Classify; ParseDrift takes a known
// level with a reason of one plain line and refuses every other shape.
func TestDrift(t *testing.T) {
	if o := Classify([]Change{{Path: DriftFile, Content: []byte(`{"level": "high"}`)}}); string(o.Drift) != `{"level": "high"}` || len(o.Ignored) != 0 {
		t.Fatalf("classify: %+v", o)
	}
	// An old daemon's change, and a deleted file, carry none.
	if o := Classify([]Change{{Path: DriftFile}, {Path: "./" + DriftFile, Deleted: true}}); o.Drift != nil || len(o.Ignored) != 0 {
		t.Fatalf("no bytes: %+v", o)
	}
	if d, err := ParseDrift([]byte(`{"level": "high", "reason": " Bob has waited four minutes. "}`)); err != nil || d != (Drift{Level: "high", Reason: "Bob has waited four minutes."}) {
		t.Fatalf("valid: %+v %v", d, err)
	}
	if _, err := ParseDrift([]byte(`{"level": "low", "reason": "` + strings.Repeat("é", MaxDriftReason) + `"}`)); err != nil {
		t.Fatalf("200 characters: %v", err)
	}
	for name, b := range map[string]string{
		"bad JSON":       `{"level": "high",`,
		"an array":       `[{"level": "high", "reason": "x"}]`,
		"unknown level":  `{"level": "critical", "reason": "x"}`,
		"upper case":     `{"level": "High", "reason": "x"}`,
		"no reason":      `{"level": "high"}`,
		"blank reason":   `{"level": "high", "reason": "  "}`,
		"long reason":    `{"level": "high", "reason": "` + strings.Repeat("a", MaxDriftReason+1) + `"}`,
		"two lines":      `{"level": "high", "reason": "Bob waits.\nAlice talks."}`,
		"carriage":       `{"level": "high", "reason": "Bob waits.\rAlice talks."}`,
		"bidi":           `{"level": "high", "reason": "Bob \u202ewaits"}`,
		"line separator": `{"level": "high", "reason": "Bob waits.\u2028Alice talks."}`,
		"para separator": `{"level": "high", "reason": "Bob waits.\u2029Alice talks."}`,
		"not UTF-8":      "{\"level\": \"high\", \"reason\": \"\xff\"}",
		"over the limit": `{"level": "high", "reason": "x", "pad": "` + strings.Repeat(" ", MaxDriftBytes) + `"}`,
	} {
		if d, err := ParseDrift([]byte(b)); err == nil {
			t.Errorf("%s: accepted %+v", name, d)
		}
	}
}

// 023-AC12: any change under linked/ is ignored and listed, whatever its kind.
func TestClassifyLinked(t *testing.T) {
	o := Classify([]Change{{Path: "linked/legal/docs/a.md", Content: []byte("x")}, {Path: "linked/b/new.txt"}, {Path: "linked", Deleted: true}, {Path: "linkedin.md"}})
	want := []string{"linked/legal/docs/a.md: linked rooms are read-only", "linked/b/new.txt: linked rooms are read-only", "linked: linked rooms are read-only"}
	if !slices.Equal(o.Ignored, want) || len(o.Docs) != 0 || len(o.Rejected) != 0 {
		t.Fatalf("%+v", o)
	}
}

// 023-AC16: the daemon takes room-new.json out of the changes and posts it as room_new, absent when
// the turn wrote none; ParseRoomNew takes one object with a one-line name and a brief within 64 KB.
func TestRoomNew(t *testing.T) {
	changes, text := TakeRoomNew([]Change{{Path: "docs/a.md", Content: []byte("a")}, {Path: "./" + RoomNewFile, Content: []byte(`{"name": "Legal"}`)}})
	if text != `{"name": "Legal"}` || len(changes) != 1 || changes[0].Path != "docs/a.md" {
		t.Fatalf("take: %q %+v", text, changes)
	}
	if _, text := TakeRoomNew([]Change{{Path: RoomNewFile, Deleted: true}}); text != "" {
		t.Fatalf("a deleted file: %q", text)
	}
	for res, want := range map[string]bool{`{"name": "Legal"}`: true, "": false} {
		b, _ := json.Marshal(Result{SessionID: "s", RoomNew: res})
		if strings.Contains(string(b), `"room_new"`) != want {
			t.Errorf("result %s", b)
		}
	}
	long := strings.Repeat("é", MaxRoomNewName)
	if r, err := ParseRoomNew(`{"name": " ` + long + ` ", "brief": "# Why\n\n` + strings.Repeat("a", MaxRoomNewBrief-7) + `"}`); err != nil || r.Name != long || len(r.Brief) != MaxRoomNewBrief {
		t.Fatalf("at the limits: %v %d", err, len(r.Brief))
	}
	for name, s := range map[string]string{
		"bad json":       `{"name": "Legal"`,
		"two proposals":  ` [{"name": "Legal"}, {"name": "Tax"}]`,
		"empty name":     `{"name": " ", "brief": "x"}`,
		"no name":        `{"brief": "x"}`,
		"two lines":      `{"name": "Legal\nTax"}`,
		"line separator": `{"name": "Legal\u2028Tax"}`,
		"bidi":           `{"name": "Legal\u202eTax"}`,
		"81 characters":  `{"name": "` + long + `x"}`,
		"brief too long": `{"name": "Legal", "brief": "` + strings.Repeat("a", MaxRoomNewBrief+1) + `"}`,
		"nul":            `{"name": "Legal", "brief": "a\u0000b"}`,
		"not utf-8":      "{\"name\": \"\xff\"}",
		"over the file":  `{"name": "Legal", "pad": "` + strings.Repeat(" ", MaxRoomNewBytes) + `"}`,
	} {
		if r, err := ParseRoomNew(s); err == nil {
			t.Errorf("%s: accepted %+v", name, r)
		}
	}
	for name, s := range map[string]string{
		"an array":         `[{"name": "Legal"}]`,
		"two objects":      `{"name": "Legal"} {"name": "Tax"}`,
		"two objects, nl":  "{\"name\": \"Legal\"}\n{\"name\": \"Tax\"}\n",
		"object, garbage":  `{"name": "Legal"} x`,
		"object, a number": `{"name": "Legal"}1`,
	} {
		if _, err := ParseRoomNew(s); err == nil || !strings.HasPrefix(err.Error(), "one proposal per turn") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if r, err := ParseRoomNew("\n{\"name\": \"Legal\"}\n\t "); err != nil || r.Name != "Legal" {
		t.Errorf("trailing whitespace: %+v %v", r, err)
	}
}
