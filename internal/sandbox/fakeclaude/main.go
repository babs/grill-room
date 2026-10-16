// Command fakeclaude stands in for the claude CLI in the sandbox image for tests, so turns run end
// to end with no subscription use. It keeps a session jsonl the way claude -p does, and acts on
// the first marker of this list found in the prompt (a failed turn's lines come back in the next
// prompt, so a FAKE_SLEEP left there must not shadow the others):
//
//	FAKE_EDIT_UPLOAD  append to the first upload (must be refused)
//	FAKE_REPLY        answer the first annotation in annotations.md
//	FAKE_CLOSE        answer the last annotation in annotations.md and mark it done
//	FAKE_ANNOTATE     open an annotation on the first word after the first upload's heading
//	FAKE_LIST         answer with the transcript files and whether they hold FAKE_MARK
//	FAKE_RECALL       answer with the latest code word an earlier turn of the session was told
//	FAKE_WHOAMI       answer with the first line of --append-system-prompt-file, and --effort
//	FAKE_AUTOCOMPACT  answer with --autocompact, "auto" without it
//	FAKE_CONTEXT_FULL report a session context at 85% of the window
//	FAKE_SKILLS       run every scripts/*.sh of each skill in the first --plugin-dir and write
//	                  what they print to docs/skills.md, as an agent using its skills would
//	FAKE_PLUGINS      answer with each --plugin-dir, in order, and its files with their sha256
//	FAKE_HOOKS        answer with what the plugins' SessionStart hooks printed
//	FAKE_PINGPONG A B as persona A call @B, as any other call @A, and repeat the marker so the
//	                  called persona's prompt holds it too (an agent chain)
//	FAKE_SLOW         take a few seconds (a turn others queue behind)
//	FAKE_SLEEP        write docs/notes.md and the prompt to the session, as a turn under way
//	                  does, then hang until killed
//	FAKE_FETCH D      GET https://D/ through the egress proxy, the prompt's last D, and answer
//	                  with the status or the error
//
// Every turn first runs the --plugin-dir plugins' SessionStart command hooks with their option
// values, as Claude Code does, and keeps what they print in the session.
//
// `claude plugin validate --json <dir>` and `claude --plugin-dir <dir> plugin details <name>`
// answer from the folder's contents, in Claude Code 2.1.296's shapes.
//
// Before them, the room's own handoff instructions: a prompt asking to write docs/_handoff-<name>.md
// writes it with the latest code word, and a new session told to read it answers with what it says.
// Any other prompt writes docs/notes.md. A prompt holding "code word is X" is remembered.
//
// The result reports a context use that grows with the session: the prompt and history sizes over
// four, in a 200k window, and a fixed cost.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func main() {
	cwd, _ := os.Getwd()
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, cwd, os.Getenv("HOME")))
}

// slow is how long FAKE_SLOW takes; a var for tests.
var slow = 8 * time.Second

var (
	codeWord   = regexp.MustCompile(`code word is ([A-Z0-9-]+)`)
	pingPong   = regexp.MustCompile(`FAKE_PINGPONG ([\p{L}\p{N}_-]+) ([\p{L}\p{N}_-]+)`)
	persona    = regexp.MustCompile(`(?m)^You are (\S+), an agent`)
	annotation = regexp.MustCompile(`### Annotation (\d+)`)
	writeNote  = regexp.MustCompile(`Write (docs/_handoff-[\p{L}\p{N}_-]+\.md) for it`)
	readNote   = regexp.MustCompile(`Your previous session wrote (docs/_handoff-[\p{L}\p{N}_-]+\.md) for you`)
)

// window is the context window the fake reports.
const window = 200_000

// cost is the total_cost_usd the fake reports for every turn.
const cost = 0.0125

// result reports model, what --model asked for, beside a small side model, as claude does, and
// used as the last request's context: input, cache read and cache creation tokens. The turn's
// tokens are that request's plus the side model's, at a fixed cost.
func result(out io.Writer, model, sid, text string, isErr bool, used int64) int {
	read, create := used/2, max(used-used/2-3, 0)
	usage := map[string]any{
		"claude-" + model + "-fake": map[string]int64{"inputTokens": 3, "outputTokens": 120, "cacheReadInputTokens": read,
			"cacheCreationInputTokens": create, "contextWindow": window},
		"claude-side-fake": map[string]int64{"inputTokens": 50, "outputTokens": 3, "contextWindow": window}}
	last := map[string]int64{"input_tokens": 3, "cache_read_input_tokens": read, "cache_creation_input_tokens": create}
	_ = json.NewEncoder(out).Encode(map[string]any{"type": "result", "subtype": "success", "is_error": isErr, "result": text, "session_id": sid,
		"modelUsage": usage, "usage": last, "total_cost_usd": cost})
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
	if code, ok := pluginCommand(args, out); ok {
		return code
	}
	resume, model, system, effort, autocompact := "", "default", "", "default", "auto"
	var dirs []string
	for i, a := range args {
		if i+1 == len(args) {
			break
		}
		switch a {
		case "--resume":
			resume = args[i+1]
		case "--model":
			model = args[i+1]
		case "--append-system-prompt-file":
			system = args[i+1]
		case "--effort":
			effort = args[i+1]
		case "--plugin-dir":
			dirs = append(dirs, args[i+1])
		case "--autocompact":
			autocompact = args[i+1]
		}
	}
	prompt, err := io.ReadAll(stdin)
	if err != nil {
		return result(out, model, "", err.Error(), true, 0)
	}
	dir := filepath.Join(home, ".claude", "projects", strings.ReplaceAll(cwd, "/", "-"))
	sid := resume
	if sid == "" {
		sid = uuid()
	}
	path := filepath.Join(dir, sid+".jsonl")
	history, err := os.ReadFile(path) //nolint:gosec // a test double reading its own session file
	if resume != "" && err != nil {
		return result(out, model, "", "No conversation found with session ID: "+resume, true, 0)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // see above
		return result(out, model, sid, err.Error(), true, 0)
	}
	hooks := sessionStartHooks(dirs, home, cwd)
	var answer string
	switch {
	case strings.Contains(string(prompt), "FAKE_HOOKS"):
		answer, err = "Hooks: none.", nil
		if len(hooks) > 0 {
			answer = "Hooks: " + strings.Join(hooks, " | ") + "."
		}
	case strings.Contains(string(prompt), "FAKE_SKILLS"):
		plugin := ""
		if len(dirs) > 0 {
			plugin = dirs[0]
		}
		answer, err = useSkills(plugin, cwd)
	case strings.Contains(string(prompt), "FAKE_PLUGINS"):
		answer, err = listPlugins(dirs)
	default:
		answer, err = act(string(prompt), string(history), cwd, system, effort, autocompact, path)
	}
	if err != nil {
		return result(out, model, sid, err.Error(), true, 0)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // see above
	if err != nil {
		return result(out, model, sid, err.Error(), true, 0)
	}
	enc := json.NewEncoder(f)
	for _, h := range hooks {
		_ = enc.Encode(map[string]string{"type": "hook", "event": "SessionStart", "output": h})
	}
	_ = enc.Encode(map[string]string{"type": "user", "prompt": string(prompt)})
	_ = enc.Encode(map[string]string{"type": "assistant", "text": answer})
	if err := f.Close(); err != nil {
		return result(out, model, sid, err.Error(), true, 0)
	}
	used := max(int64(len(prompt)+len(history))/4, 10)
	if strings.Contains(string(prompt), "FAKE_CONTEXT_FULL") {
		used = window * 85 / 100
	}
	return result(out, model, sid, answer, false, used)
}

func act(prompt, history, cwd, system, effort, autocompact, session string) (string, error) {
	ws := func(p string) string { return filepath.Join(cwd, p) }
	if m := writeNote.FindStringSubmatch(prompt); m != nil {
		word := "nothing"
		if ms := codeWord.FindAllStringSubmatch(history+prompt, -1); ms != nil {
			word = ms[len(ms)-1][1]
		}
		return "Handoff note written.", os.WriteFile(ws(m[1]), []byte("# Handoff\n\nThe code word was "+word+".\n"), 0o600) //nolint:gosec // see above
	}
	if m := readNote.FindStringSubmatch(prompt); m != nil {
		b, err := os.ReadFile(ws(m[1])) //nolint:gosec // see above
		if err != nil {
			return "", fmt.Errorf("no handoff note: %w", err)
		}
		_, note, _ := strings.Cut(string(b), "\n\n")
		return "Read " + m[1] + ": " + strings.TrimSpace(note), nil
	}
	switch {
	case strings.Contains(prompt, "FAKE_AUTOCOMPACT"):
		return "autocompact " + autocompact, nil
	case strings.Contains(prompt, "FAKE_FETCH"):
		ms := fetchMark.FindAllStringSubmatch(prompt, -1)
		if ms == nil {
			return "", fmt.Errorf("FAKE_FETCH wants a domain")
		}
		return fetch(ms[len(ms)-1][1]), nil
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
	case strings.Contains(prompt, "FAKE_CLOSE"):
		b, _ := os.ReadFile(ws("annotations.md"))
		ms := annotation.FindAllSubmatch(b, -1)
		if ms == nil {
			return "", fmt.Errorf("no annotation to close")
		}
		return "Closed.", os.WriteFile(ws("replies.json"), fmt.Appendf(nil, `[{"annotation": %s, "body": "settled", "status": "done"}]`, ms[len(ms)-1][1]), 0o600) //nolint:gosec // see above
	case strings.Contains(prompt, "FAKE_ANNOTATE"):
		ups, _ := filepath.Glob(ws("uploads/*.md"))
		if len(ups) == 0 {
			return "", fmt.Errorf("no Markdown upload to annotate")
		}
		b, _ := os.ReadFile(ups[0]) //nolint:gosec // see above
		_, body, _ := strings.Cut(string(b), "\n\n")
		word, _, _ := strings.Cut(strings.TrimSpace(body), " ")
		anns, _ := json.Marshal([]map[string]string{{"doc": "uploads/" + filepath.Base(ups[0]), "exact": word, "body": "fake question"}})
		return "Annotated.", os.WriteFile(ws("annotations-new.json"), anns, 0o600) //nolint:gosec // see above
	case strings.Contains(prompt, "FAKE_LIST"):
		files, _ := filepath.Glob(ws("transcript/*/*.md"))
		marked := false
		var names []string
		for _, f := range files {
			names = append(names, filepath.Base(filepath.Dir(f))+"/"+filepath.Base(f))
			b, _ := os.ReadFile(f) //nolint:gosec // see above
			marked = marked || strings.Contains(string(b), "FAKE_MARK")
		}
		return fmt.Sprintf("transcript: %s; FAKE_MARK found: %v", strings.Join(names, ","), marked), nil
	case strings.Contains(prompt, "FAKE_RECALL"):
		sc := bufio.NewScanner(strings.NewReader(history))
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		word := "nothing"
		for sc.Scan() {
			// The latest told wins: a persona's first prompt quotes the whole room.
			if ms := codeWord.FindAllStringSubmatch(sc.Text(), -1); ms != nil {
				word = ms[len(ms)-1][1]
			}
		}
		return "The code word was " + word + ".", nil
	case strings.Contains(prompt, "FAKE_PINGPONG"):
		return pingPongCall(prompt)
	case strings.Contains(prompt, "FAKE_WHOAMI"):
		return whoami(system, effort)
	case strings.Contains(prompt, "FAKE_SLOW"):
		time.Sleep(slow)
		return "Slow turn done.", nil
	case strings.Contains(prompt, "FAKE_SLEEP"):
		// What a killed turn leaves behind must never reach the room.
		_ = os.WriteFile(ws("docs/notes.md"), []byte("# Notes\n\nWritten by a turn that never ended.\n"), 0o600) //nolint:gosec // see above
		if f, err := os.OpenFile(session, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {              //nolint:gosec // see above
			_ = json.NewEncoder(f).Encode(map[string]string{"type": "user", "prompt": prompt})
			_ = f.Close()
		}
		time.Sleep(time.Hour)
		return "", nil
	}
	turns := strings.Count(history, `"type":"user"`) + 1
	if err := os.WriteFile(ws("docs/notes.md"), fmt.Appendf(nil, "# Notes\n\nTurn %d.\n", turns), 0o600); err != nil { //nolint:gosec // see above
		return "", err
	}
	return fmt.Sprintf("Wrote docs/notes.md, turn %d.", turns), nil
}

func whoami(system, effort string) (string, error) {
	b, err := os.ReadFile(system) //nolint:gosec // see above
	if err != nil {
		return "", fmt.Errorf("no system prompt file: %w", err)
	}
	first, _, _ := strings.Cut(string(b), "\n")
	return fmt.Sprintf("%s (effort %s)", first, effort), nil
}

func pingPongCall(prompt string) (string, error) {
	m, self := pingPong.FindStringSubmatch(prompt), persona.FindStringSubmatch(prompt)
	if m == nil || self == nil {
		return "", fmt.Errorf("FAKE_PINGPONG wants two persona names and a prompt naming its persona")
	}
	other := m[1]
	if strings.EqualFold(self[1], m[1]) {
		other = m[2]
	}
	return fmt.Sprintf("FAKE_PINGPONG %s %s: over to @%s.", m[1], m[2], other), nil
}

// useSkills runs each skill's scripts the way the agent would, from the skill's folder.
func useSkills(plugin, cwd string) (string, error) {
	if plugin == "" {
		return "No skills.", nil
	}
	mds, _ := filepath.Glob(filepath.Join(plugin, "skills", "*", "SKILL.md"))
	var lines []string
	for _, md := range mds {
		dir := filepath.Dir(md)
		scripts, _ := filepath.Glob(filepath.Join(dir, "scripts", "*.sh"))
		var outs []string
		for _, sc := range scripts {
			cmd := exec.Command("sh", sc) //nolint:gosec,noctx // a test double running the skill it was handed
			cmd.Dir = dir
			b, err := cmd.Output()
			if err != nil {
				return "", fmt.Errorf("%s: %w", sc, err)
			}
			outs = append(outs, strings.TrimSpace(string(b)))
		}
		lines = append(lines, filepath.Base(dir)+": "+strings.Join(outs, " "))
	}
	body := "# Skills\n\n- " + strings.Join(lines, "\n- ") + "\n"
	if err := os.WriteFile(filepath.Join(cwd, "docs", "skills.md"), []byte(body), 0o600); err != nil { //nolint:gosec // see above
		return "", err
	}
	return "Skills used: " + strings.Join(lines, "; ") + ".", nil
}

var fetchMark = regexp.MustCompile(`FAKE_FETCH ([a-z0-9][a-z0-9.-]*[a-z0-9])`)

// fetch GETs https://domain/ through the sandbox's HTTPS_PROXY, the egress proxy, as WebFetch does.
func fetch(domain string) string {
	// Past the daemon's longest hold: an approval in the window lets the fetch go on.
	c := &http.Client{Timeout: 90 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
	resp, err := c.Get("https://" + domain + "/") //nolint:noctx,gosec // a test fake: fetching what the prompt names is its job; the timeout bounds it
	if err != nil {
		return "Could not fetch " + domain + ": " + err.Error()
	}
	_ = resp.Body.Close()
	return "Fetched " + domain + ": " + resp.Status
}
