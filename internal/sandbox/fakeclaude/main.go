// Command fakeclaude stands in for the claude CLI in the sandbox image for tests, so turns run end
// to end with no subscription use. It keeps a session jsonl the way claude -p does, and acts on
// the first marker of this list found in the prompt (a failed turn's lines come back in the next
// prompt, so a FAKE_SLEEP left there must not shadow the others):
//
//	FAKE_EDIT_UPLOAD  append to the first upload (must be refused)
//	FAKE_REPLY        answer the first annotation in annotations.md
//	FAKE_LIST         answer with the transcript files and whether they hold FAKE_MARK
//	FAKE_RECALL       answer with the code word an earlier turn of the session was told
//	FAKE_SLEEP        hang until killed (a turn in progress)
//
// Any other prompt writes docs/notes.md. A prompt holding "code word is X" is remembered.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func main() {
	cwd, _ := os.Getwd()
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, cwd, os.Getenv("HOME")))
}

var (
	codeWord   = regexp.MustCompile(`code word is ([A-Z0-9-]+)`)
	annotation = regexp.MustCompile(`### Annotation (\d+)`)
)

func result(out io.Writer, sid, text string, isErr bool) int {
	_ = json.NewEncoder(out).Encode(map[string]any{"type": "result", "subtype": "success", "is_error": isErr, "result": text, "session_id": sid})
	if isErr {
		return 1
	}
	return 0
}

func uuid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func run(args []string, stdin io.Reader, out io.Writer, cwd, home string) int {
	var resume string
	for i, a := range args {
		if a == "--resume" && i+1 < len(args) {
			resume = args[i+1]
		}
	}
	prompt, err := io.ReadAll(stdin)
	if err != nil {
		return result(out, "", err.Error(), true)
	}
	dir := filepath.Join(home, ".claude", "projects", strings.ReplaceAll(cwd, "/", "-"))
	sid := resume
	if sid == "" {
		sid = uuid()
	}
	path := filepath.Join(dir, sid+".jsonl")
	history, err := os.ReadFile(path) //nolint:gosec // a test double reading its own session file
	if resume != "" && err != nil {
		return result(out, "", "No conversation found with session ID: "+resume, true)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // see above
		return result(out, sid, err.Error(), true)
	}
	answer, err := act(string(prompt), string(history), cwd)
	if err != nil {
		return result(out, sid, err.Error(), true)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // see above
	if err != nil {
		return result(out, sid, err.Error(), true)
	}
	enc := json.NewEncoder(f)
	_ = enc.Encode(map[string]string{"type": "user", "prompt": string(prompt)})
	_ = enc.Encode(map[string]string{"type": "assistant", "text": answer})
	if err := f.Close(); err != nil {
		return result(out, sid, err.Error(), true)
	}
	return result(out, sid, answer, false)
}

func act(prompt, history, cwd string) (string, error) {
	ws := func(p string) string { return filepath.Join(cwd, p) }
	switch {
	case strings.Contains(prompt, "FAKE_EDIT_UPLOAD"):
		ups, _ := filepath.Glob(ws("uploads/*"))
		if len(ups) == 0 {
			return "", fmt.Errorf("no upload to edit")
		}
		if err := os.Chmod(ups[0], 0o600); err != nil {
			return "", err
		}
		f, err := os.OpenFile(ups[0], os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // see above
		if err != nil {
			return "", err
		}
		_, _ = f.WriteString("defaced")
		return "I edited " + filepath.Base(ups[0]) + ".", f.Close()
	case strings.Contains(prompt, "FAKE_REPLY"):
		b, _ := os.ReadFile(ws("annotations.md"))
		m := annotation.FindSubmatch(b)
		if m == nil {
			return "", fmt.Errorf("no annotation to answer")
		}
		return "Replied.", os.WriteFile(ws("replies.json"), fmt.Appendf(nil, `[{"annotation": %s, "body": "fake reply"}]`, m[1]), 0o600) //nolint:gosec // see above
	case strings.Contains(prompt, "FAKE_LIST"):
		files, _ := filepath.Glob(ws("transcript/*.md"))
		marked := false
		var names []string
		for _, f := range files {
			names = append(names, filepath.Base(f))
			b, _ := os.ReadFile(f) //nolint:gosec // see above
			marked = marked || strings.Contains(string(b), "FAKE_MARK")
		}
		return fmt.Sprintf("transcript: %s; FAKE_MARK found: %v", strings.Join(names, ","), marked), nil
	case strings.Contains(prompt, "FAKE_RECALL"):
		sc := bufio.NewScanner(strings.NewReader(history))
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		word := "nothing"
		for sc.Scan() {
			if m := codeWord.FindStringSubmatch(sc.Text()); m != nil {
				word = m[1]
			}
		}
		return "The code word was " + word + ".", nil
	case strings.Contains(prompt, "FAKE_SLEEP"):
		time.Sleep(time.Hour)
		return "", nil
	}
	turns := strings.Count(history, `"type":"user"`) + 1
	if err := os.WriteFile(ws("docs/notes.md"), fmt.Appendf(nil, "# Notes\n\nTurn %d.\n", turns), 0o600); err != nil { //nolint:gosec // see above
		return "", err
	}
	return fmt.Sprintf("Wrote docs/notes.md, turn %d.", turns), nil
}
