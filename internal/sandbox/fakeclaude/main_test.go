package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
	if runtime.GOOS == "windows" {
		t.Skip("the fake claude runs in the Linux sandbox only; it names projects from POSIX paths")
	}
	cwd, home := t.TempDir(), t.TempDir()
	for _, d := range []string{"docs", "uploads", "transcript"} {
		if err := os.Mkdir(filepath.Join(cwd, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(cwd, "uploads", "1-a.pdf"), []byte("%PDF"), 0o444)
	_ = os.Mkdir(filepath.Join(cwd, "transcript", "2026-10-10"), 0o755)
	_ = os.WriteFile(filepath.Join(cwd, "transcript", "2026-10-10", "001.md"), []byte("- FAKE_MARK"), 0o444)
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
	if o, _ := call(t, cwd, home, "FAKE_LIST"); o.Result != "transcript: 2026-10-10/001.md; FAKE_MARK found: true" {
		t.Fatalf("list %+v", o)
	}
	// 002-AC18: the persona's text and effort, as the daemon passes them.
	sys := filepath.Join(home, "persona.md")
	_ = os.WriteFile(sys, []byte("You are Secretary, the note taker.\nMore.\n"), 0o600)
	if o, _ := call(t, cwd, home, "FAKE_WHOAMI", "--append-system-prompt-file", sys, "--effort", "low"); o.Result != "You are Secretary, the note taker. (effort low)" {
		t.Fatalf("whoami %+v", o)
	}
	if o, code := call(t, cwd, home, "FAKE_WHOAMI"); code != 1 || !o.IsError {
		t.Fatalf("whoami without a system prompt %+v", o)
	}
	// 003-AC24: --autocompact as passed, and a context use that grows with the session.
	if o, _ := call(t, cwd, home, "FAKE_AUTOCOMPACT", "--autocompact", "200000"); o.Result != "autocompact 200000" {
		t.Fatalf("autocompact %+v", o)
	}
	if o, _ := call(t, cwd, home, "FAKE_AUTOCOMPACT"); o.Result != "autocompact auto" {
		t.Fatalf("no autocompact %+v", o)
	}
	contextOf := func(prompt string, args ...string) (tokens, window int64) {
		var buf bytes.Buffer
		run(append([]string{"-p"}, args...), strings.NewReader(prompt), &buf, cwd, home)
		var r struct {
			Usage      map[string]int64 `json:"usage"`
			ModelUsage map[string]struct {
				ContextWindow int64 `json:"contextWindow"`
			} `json:"modelUsage"`
		}
		if err := json.Unmarshal(buf.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		return r.Usage["input_tokens"] + r.Usage["cache_read_input_tokens"] + r.Usage["cache_creation_input_tokens"], r.ModelUsage["claude-default-fake"].ContextWindow
	}
	small, w := contextOf("hi", "--resume", sid)
	if larger, _ := contextOf(strings.Repeat("x", 4000), "--resume", sid); w != window || larger <= small {
		t.Fatalf("context %d then %d, window %d", small, larger, w)
	}
	if full, _ := contextOf("FAKE_CONTEXT_FULL"); full != window*85/100 {
		t.Fatalf("full context %d", full)
	}
	// 003-AC25: the room's handoff instructions: the last turn writes the note with the latest code
	// word, the new session reads it back.
	if o, _ := call(t, cwd, home, "## Handoff\n\nWrite docs/_handoff-Grill.md for it: the decisions.", "--resume", sid); o.Result != "Handoff note written." {
		t.Fatalf("handoff write %+v", o)
	}
	if b, _ := os.ReadFile(filepath.Join(cwd, "docs", "_handoff-Grill.md")); string(b) != "# Handoff\n\nThe code word was PAPRIKA-42.\n" {
		t.Fatalf("note %q", b)
	}
	if o, _ := call(t, cwd, home, "Your previous session wrote docs/_handoff-Grill.md for you: read it first"); o.Result != "Read docs/_handoff-Grill.md: The code word was PAPRIKA-42." {
		t.Fatalf("handoff read %+v", o)
	}
	if o, code := call(t, cwd, home, "Your previous session wrote docs/_handoff-Nobody.md for you"); code != 1 || !o.IsError {
		t.Fatalf("missing note %+v", o)
	}
	// 002-AC31: the skills of --plugin-dir run from their own folder; without it, none.
	plugin := filepath.Join(home, "plugin")
	_ = os.MkdirAll(filepath.Join(plugin, "skills", "stamp", "scripts"), 0o755)
	_ = os.WriteFile(filepath.Join(plugin, "skills", "stamp", "SKILL.md"), []byte("---\nname: stamp\n---\n"), 0o600)
	_ = os.WriteFile(filepath.Join(plugin, "skills", "stamp", "scripts", "stamp.sh"), []byte("cat SKILL.md >/dev/null && echo STAMP-v1\n"), 0o600)
	if o, _ := call(t, cwd, home, "FAKE_SKILLS", "--plugin-dir", plugin); o.Result != "Skills used: stamp: STAMP-v1." {
		t.Fatalf("skills %+v", o)
	}
	if b, _ := os.ReadFile(filepath.Join(cwd, "docs", "skills.md")); string(b) != "# Skills\n\n- stamp: STAMP-v1\n" {
		t.Fatalf("skills.md %q", b)
	}
	if o, _ := call(t, cwd, home, "FAKE_SKILLS"); o.Result != "No skills." {
		t.Fatalf("no skills %+v", o)
	}
	// 002-AC24: the ping-pong of an agent chain, each persona calling the other.
	for prompt, want := range map[string]string{
		"## Who is here\nYou: @Grill\n\nYou are Grill, an agent.\n- FAKE_PINGPONG grill Secretary": "FAKE_PINGPONG grill Secretary: over to @Secretary.",
		"You are Secretary, an agent.\n- FAKE_PINGPONG grill Secretary: over to @Secretary.":       "FAKE_PINGPONG grill Secretary: over to @grill.",
	} {
		if o, _ := call(t, cwd, home, prompt); o.Result != want {
			t.Fatalf("ping-pong %q: %+v", prompt, o)
		}
	}
	if o, code := call(t, cwd, home, "FAKE_PINGPONG alone"); code != 1 || !o.IsError {
		t.Fatalf("ping-pong without names %+v", o)
	}
	slow = time.Millisecond
	if o, _ := call(t, cwd, home, "FAKE_SLOW"); o.Result != "Slow turn done." {
		t.Fatalf("slow %+v", o)
	}
	b, _ := os.ReadFile(filepath.Join(home, ".claude", "projects", strings.ReplaceAll(cwd, "/", "-"), sid+".jsonl"))
	// Two lines per turn of the session: the first prompt, the recall, two context reads, the note.
	if n := strings.Count(string(b), "\n"); n != 10 {
		t.Fatalf("session holds %d lines", n)
	}
}

// 014-AC8: FAKE_FETCH fetches the prompt's last domain through HTTPS_PROXY and says how it went: a
// proxy refusing the CONNECT shows as an error, a page answering as its status.
func TestFakeFetch(t *testing.T) {
	cwd, home := t.TempDir(), t.TempDir()
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "second.example:443" {
			t.Errorf("proxy got %s %s", r.Method, r.Host)
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer refuse.Close()
	t.Setenv("HTTPS_PROXY", refuse.URL)
	o, code := call(t, cwd, home, "FAKE_FETCH first.example\nlater FAKE_FETCH second.example")
	if code != 0 || !strings.HasPrefix(o.Result, "Could not fetch second.example: ") {
		t.Fatalf("refused: %d %q", code, o.Result)
	}
	if o, code := call(t, cwd, home, "FAKE_FETCH"); code != 1 || !o.IsError {
		t.Fatalf("no domain: %d %+v", code, o)
	}
}
