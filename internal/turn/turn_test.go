package turn

import (
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
	wantIgnored := []string{"docs/sub/x.md", "docs/notes.txt", "docs/gone.md", "docs/bin.md", "docs/big.md", RepliesFile}
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
	for p, want := range map[string]bool{"docs/a.md": true, RepliesFile: true, "uploads/1-a.pdf": false, "transcript/x.md": false, "docsx/a.md": false} {
		if NeedsContent(p) != want {
			t.Errorf("NeedsContent(%q) != %v", p, want)
		}
	}
}
