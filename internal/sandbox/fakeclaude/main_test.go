package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type out struct {
	Result    string `json:"result"`
	SessionID string `json:"session_id"`
	IsError   bool   `json:"is_error"`
}

func call(t *testing.T, cwd, home, prompt string, args ...string) (out, int) {
	t.Helper()
	var buf bytes.Buffer
	code := run(append([]string{"-p", "--output-format", "json"}, args...), strings.NewReader(prompt), &buf, cwd, home)
	var o out
	if err := json.Unmarshal(buf.Bytes(), &o); err != nil {
		t.Fatalf("output %q: %v", buf.String(), err)
	}
	return o, code
}

func TestFakeClaude(t *testing.T) {
	cwd, home := t.TempDir(), t.TempDir()
	for _, d := range []string{"docs", "uploads", "transcript"} {
		if err := os.Mkdir(filepath.Join(cwd, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(cwd, "uploads", "1-a.pdf"), []byte("%PDF"), 0o444)
	_ = os.WriteFile(filepath.Join(cwd, "transcript", "2026-10-10.md"), []byte("- FAKE_MARK"), 0o444)
	_ = os.WriteFile(filepath.Join(cwd, "annotations.md"), []byte("### Annotation 7 on docs/x.md"), 0o444)

	var buf bytes.Buffer
	run([]string{"-p", "--model", "opus"}, strings.NewReader("hi"), &buf, cwd, home)
	var usage struct {
		ModelUsage map[string]struct{ OutputTokens int } `json:"modelUsage"`
	}
	if err := json.Unmarshal(buf.Bytes(), &usage); err != nil || usage.ModelUsage["claude-opus-fake"].OutputTokens == 0 {
		t.Fatalf("model usage %s: %v", buf.String(), err)
	}

	o, code := call(t, cwd, home, "The code word is PAPRIKA-42.")
	if code != 0 || o.SessionID == "" || o.Result != "Wrote docs/notes.md, turn 1." {
		t.Fatalf("first turn %+v %d", o, code)
	}
	sid := o.SessionID
	if b, _ := os.ReadFile(filepath.Join(cwd, "docs", "notes.md")); string(b) != "# Notes\n\nTurn 1.\n" {
		t.Fatalf("notes %q", b)
	}
	if o, _ := call(t, cwd, home, "FAKE_RECALL", "--resume", sid); o.Result != "The code word was PAPRIKA-42." || o.SessionID != sid {
		t.Fatalf("recall %+v", o)
	}
	if o, _ := call(t, cwd, home, "FAKE_RECALL"); o.Result != "The code word was nothing." {
		t.Fatalf("recall in a new session %+v", o)
	}
	if o, code := call(t, cwd, home, "x", "--resume", "00000000-0000-4000-8000-000000000000"); code != 1 || !o.IsError {
		t.Fatalf("unknown session %+v", o)
	}
	if o, _ := call(t, cwd, home, "FAKE_EDIT_UPLOAD"); o.Result != "I edited 1-a.pdf." {
		t.Fatalf("edit %+v", o)
	}
	if b, _ := os.ReadFile(filepath.Join(cwd, "uploads", "1-a.pdf")); string(b) != "%PDFdefaced" {
		t.Fatalf("upload %q", b)
	}
	if o, _ := call(t, cwd, home, "FAKE_REPLY"); o.Result != "Replied." {
		t.Fatalf("reply %+v", o)
	}
	if b, _ := os.ReadFile(filepath.Join(cwd, "replies.json")); string(b) != `[{"annotation": 7, "body": "fake reply"}]` {
		t.Fatalf("replies %q", b)
	}
	if o, _ := call(t, cwd, home, "FAKE_CLOSE"); o.Result != "Closed." {
		t.Fatalf("close %+v", o)
	}
	if b, _ := os.ReadFile(filepath.Join(cwd, "replies.json")); string(b) != `[{"annotation": 7, "body": "settled", "status": "done"}]` {
		t.Fatalf("closing replies %q", b)
	}
	_ = os.WriteFile(filepath.Join(cwd, "uploads", "2-plan.md"), []byte("# Plan\n\nRollback first.\n"), 0o444)
	if o, _ := call(t, cwd, home, "FAKE_ANNOTATE"); o.Result != "Annotated." {
		t.Fatalf("annotate %+v", o)
	}
	if b, _ := os.ReadFile(filepath.Join(cwd, "annotations-new.json")); string(b) != `[{"body":"fake question","doc":"uploads/2-plan.md","exact":"Rollback"}]` {
		t.Fatalf("new annotations %q", b)
	}
	if o, _ := call(t, cwd, home, "FAKE_LIST"); o.Result != "transcript: 2026-10-10.md; FAKE_MARK found: true" {
		t.Fatalf("list %+v", o)
	}
	b, _ := os.ReadFile(filepath.Join(home, ".claude", "projects", strings.ReplaceAll(cwd, "/", "-"), sid+".jsonl"))
	if n := strings.Count(string(b), "\n"); n != 4 {
		t.Fatalf("session holds %d lines", n)
	}
}
