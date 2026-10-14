package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/turn"
)

// emulator answers the docker commands RunTurn sends with an in-memory /workspace and home, and a
// claude that runs Go code on them.
type emulator struct {
	mu     sync.Mutex
	ws     map[string][]byte
	home   map[string][]byte
	claude func(e *emulator, prompt string, args []string) (string, error)
	calls  []string
}

func newEmulator() *emulator {
	return &emulator{ws: map[string][]byte{}, home: map[string][]byte{}}
}

func untar(t io.Reader, into map[string][]byte) error {
	tr := tar.NewReader(t)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(tr)
			into[h.Name] = b
		}
	}
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (e *emulator) run(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if args[0] == "info" {
		return []byte("28|Docker Desktop"), nil
	}
	if args[0] != "exec" {
		return nil, nil
	}
	cmd := args[2:]
	if args[1] == "-i" {
		cmd = args[3:]
	}
	e.calls = append(e.calls, strings.Join(cmd, " "))
	switch {
	case cmd[0] == "sh" && strings.Contains(cmd[2], "-mindepth 1 -delete"):
		e.ws = map[string][]byte{}
	case cmd[0] == "sh" && strings.Contains(cmd[2], "sha256sum -z"):
		var b strings.Builder
		for p, d := range e.ws {
			fmt.Fprintf(&b, "%s  ./%s\x00", sum(d), p)
		}
		return []byte(b.String()), nil
	case cmd[0] == "sh" && strings.Contains(cmd[2], "stat -c %s"):
		d, ok := e.home[strings.TrimPrefix(cmd[4], home+"/")]
		if !ok {
			return nil, errors.New("stat: no such file")
		}
		return fmt.Appendf(nil, "%d\n%s  -\n", len(d), sum(d)), nil
	case cmd[0] == "tar" && cmd[1] == "-x":
		if cmd[3] == Workspace {
			return nil, untar(stdin, e.ws)
		}
		// A directory under the home keeps its path, as the plugin directory does.
		sub := map[string][]byte{}
		err := untar(stdin, sub)
		prefix := strings.TrimPrefix(strings.TrimPrefix(cmd[3], home), "/")
		for p, d := range sub {
			if prefix != "" {
				p = prefix + "/" + p
			}
			e.home[p] = d
		}
		return nil, err
	case cmd[0] == "rm" && cmd[1] == "-rf":
		for _, dir := range cmd[2:] {
			prefix := strings.TrimPrefix(dir, home+"/") + "/"
			for p := range e.home {
				if strings.HasPrefix(p, prefix) {
					delete(e.home, p)
				}
			}
		}
	case cmd[0] == "tar" && cmd[1] == "-c":
		list, _ := io.ReadAll(stdin)
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, p := range strings.Split(strings.TrimSuffix(string(list), "\x00"), "\x00") {
			_ = tw.WriteHeader(&tar.Header{Name: p, Mode: 0o644, Size: int64(len(e.ws[p]))})
			_, _ = tw.Write(e.ws[p])
		}
		_ = tw.Close()
		return buf.Bytes(), nil
	case cmd[0] == "claude":
		prompt, _ := io.ReadAll(stdin)
		return func() ([]byte, error) {
			out, err := e.claude(e, string(prompt), cmd)
			return []byte(out), err
		}()
	case cmd[0] == "tail":
		d, ok := e.home[strings.TrimPrefix(cmd[3], home+"/")]
		if !ok {
			return nil, errors.New("tail: no such file")
		}
		var off int
		_, _ = fmt.Sscanf(cmd[2], "+%d", &off)
		return d[off-1:], nil
	}
	return nil, nil
}

func workspaceTar(t *testing.T, files map[string]string) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(data))
	}
	_ = tw.Close()
	return &buf
}

const sid = "60dc4912-36ec-4225-a44a-6a6c1ed85b1b"

func newTurnSandbox(t *testing.T, e *emulator) *Sandbox {
	t.Helper()
	s := New(Config{Image: "img", Name: "t", Proxy: testProxy}, e.run, zap.NewNop())
	t.Cleanup(func() { s.Close(context.Background()) })
	return s
}

// 001-AC32 001-AC33 001-AC16: a turn starts from the room's workspace only, reports every changed
// path with the contents of documents and replies only, and returns the session bytes it added.
func TestRunTurn(t *testing.T) {
	e := newEmulator()
	e.ws["docs/stale.md"] = []byte("from an earlier turn")
	e.claude = func(e *emulator, prompt string, args []string) (string, error) {
		if prompt != "the prompt" || !slices.Contains(args, "haiku") || slices.Contains(args, "--resume") {
			return "", fmt.Errorf("claude got %q %v", prompt, args)
		}
		e.ws["docs/plan.md"] = []byte("# Plan v2")
		e.ws["docs/new.md"] = []byte("# New")
		e.ws["uploads/1-brief.pdf"] = []byte("defaced")
		delete(e.ws, "uploads/2-a.png")
		e.ws["replies.json"] = []byte(`[]`)
		e.ws["scratch.txt"] = []byte("x")
		e.home[".claude/projects/-workspace/"+sid+".jsonl"] = []byte("l1\nl2\n")
		return `{"result":"done","session_id":"` + sid + `"}`, nil
	}
	s := newTurnSandbox(t, e)
	out, err := s.RunTurn(context.Background(), TurnInput{Bearer: testBearer,
		Workspace: workspaceTar(t, map[string]string{"docs/plan.md": "# Plan", "uploads/1-brief.pdf": "%PDF", "uploads/2-a.png": "PNG", "transcript/d.md": "t"}),
		Prompt:    "the prompt", Model: "haiku",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Result.Result != "done" || out.SessionOffset != 0 || string(out.Session) != "l1\nl2\n" {
		t.Fatalf("out %+v", out)
	}
	want := []turn.Change{
		{Path: "docs/new.md", Content: []byte("# New")},
		{Path: "docs/plan.md", Content: []byte("# Plan v2")},
		{Path: "replies.json", Content: []byte("[]")},
		{Path: "scratch.txt"},
		{Path: "uploads/1-brief.pdf"},
		{Path: "uploads/2-a.png", Deleted: true},
	}
	if len(out.Changes) != len(want) {
		t.Fatalf("changes %+v", out.Changes)
	}
	for i, c := range out.Changes {
		if c.Path != want[i].Path || c.Deleted != want[i].Deleted || !bytes.Equal(c.Content, want[i].Content) {
			t.Fatalf("change %d = %+v, want %+v", i, c, want[i])
		}
	}
}

// 002-AC31: a turn's skills land in the home's plugin directory and reach claude through
// --plugin-dir; the next turn starts from none.
func TestRunTurnLoadsSkills(t *testing.T) {
	e := newEmulator()
	var seen []string
	e.claude = func(e *emulator, _ string, args []string) (string, error) {
		i := slices.Index(args, "--plugin-dir")
		got := "none"
		if i >= 0 {
			got = args[i+1] + " " + string(e.home[".grill-room/plugin/skills/stamp/SKILL.md"])
		} else if len(e.home[".grill-room/plugin/skills/stamp/SKILL.md"]) > 0 {
			got = "stale plugin"
		}
		seen = append(seen, got)
		e.home[".claude/projects/-workspace/"+sid+".jsonl"] = []byte("l\n")
		return `{"result":"done","session_id":"` + sid + `"}`, nil
	}
	s := newTurnSandbox(t, e)
	for _, plugin := range []io.Reader{workspaceTar(t, map[string]string{"skills/stamp/SKILL.md": "stamp v1"}), nil} {
		if _, err := s.RunTurn(context.Background(), TurnInput{Workspace: workspaceTar(t, nil), Prompt: "p", Plugin: plugin, Bearer: testBearer}); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(seen, []string{pluginDir + " stamp v1", "none"}) {
		t.Fatalf("claude saw %q", seen)
	}
}

// 009-AC17: each room plugin lands in its own folder under the home's plugins directory and
// reaches claude through its own --plugin-dir, after the skills' one and in the turn's order. A
// later turn keeps none of an earlier one's plugins, and with none passes no flag. A name that
// would leave the folder, or a second plugin of one name, fails the turn before claude runs.
func TestRunTurnLoadsPlugins(t *testing.T) {
	e := newEmulator()
	var seen [][]string
	e.claude = func(e *emulator, _ string, args []string) (string, error) {
		var got []string
		for i, a := range args {
			if a == "--plugin-dir" {
				got = append(got, args[i+1])
			}
		}
		var files []string
		for p, d := range e.home {
			if strings.HasPrefix(p, ".grill-room/plugin") {
				files = append(files, p+"="+string(d))
			}
		}
		slices.Sort(files)
		seen = append(seen, append(got, files...))
		e.home[".claude/projects/-workspace/"+sid+".jsonl"] = []byte("l\n")
		return `{"result":"done","session_id":"` + sid + `"}`, nil
	}
	s := newTurnSandbox(t, e)
	tarOf := func(files map[string]string) io.Reader { return workspaceTar(t, files) }
	for _, in := range []TurnInput{
		{Plugin: tarOf(map[string]string{"skills/stamp/SKILL.md": "s"}), Plugins: []PluginTar{
			{Name: "zeta", Tar: tarOf(map[string]string{".claude-plugin/plugin.json": "z"})},
			{Name: "alpha", Tar: tarOf(map[string]string{"skills/a/SKILL.md": "a"})}}},
		{Plugins: []PluginTar{{Name: "alpha", Tar: tarOf(map[string]string{"skills/a/SKILL.md": "a2"})}}},
		{},
	} {
		in.Workspace, in.Prompt, in.Bearer = workspaceTar(t, nil), "p", testBearer
		if _, err := s.RunTurn(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
	want := [][]string{
		{pluginDir, pluginsDir + "/zeta", pluginsDir + "/alpha",
			".grill-room/plugin/skills/stamp/SKILL.md=s", ".grill-room/plugins/alpha/skills/a/SKILL.md=a", ".grill-room/plugins/zeta/.claude-plugin/plugin.json=z"},
		{pluginsDir + "/alpha", ".grill-room/plugins/alpha/skills/a/SKILL.md=a2"},
		nil,
	}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Fatalf("claude saw\n%q\nwant\n%q", seen, want)
	}
	for _, bad := range [][]PluginTar{
		{{Name: "../x", Tar: tarOf(nil)}},
		{{Name: "grill-room", Tar: tarOf(nil)}},
		{{Name: "a", Tar: tarOf(nil)}, {Name: "a", Tar: tarOf(nil)}},
	} {
		_, err := s.RunTurn(context.Background(), TurnInput{Workspace: workspaceTar(t, nil), Prompt: "p", Bearer: testBearer, Plugins: bad})
		if err == nil || len(seen) != 3 {
			t.Fatalf("plugins %+v: err %v, claude ran %d times", bad, err, len(seen))
		}
	}
}

// 001-AC34: resuming fetches the room's session when the local copy is missing or differs, keeps
// a matching one, and uploads only the bytes past the room's copy.
func TestRunTurnSyncsSession(t *testing.T) {
	room := []byte("a\nb\n")
	path := ".claude/projects/-workspace/" + sid + ".jsonl"
	for _, tc := range []struct {
		name  string
		local []byte
		fetch bool
	}{
		{"missing", nil, true},
		{"diverged after a failed turn", []byte("a\nb\nlost\n"), true},
		{"same size, other bytes", []byte("a\nX\n"), true},
		{"same", room, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEmulator()
			if tc.local != nil {
				e.home[path] = tc.local
			}
			e.claude = func(e *emulator, _ string, args []string) (string, error) {
				if !slices.Contains(args, sid) || !bytes.Equal(e.home[path], room) {
					return "", fmt.Errorf("resumed %v on %q", args, e.home[path])
				}
				e.home[path] = append(e.home[path], "c\n"...)
				return `{"result":"ok","session_id":"` + sid + `"}`, nil
			}
			fetched := false
			s := newTurnSandbox(t, e)
			out, err := s.RunTurn(context.Background(), TurnInput{Bearer: testBearer,
				Workspace: workspaceTar(t, nil), Prompt: "p", Resume: sid, SessionSize: int64(len(room)), SessionSHA256: sum(room),
				FetchSession: func(context.Context) ([]byte, error) { fetched = true; return room, nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			if fetched != tc.fetch || out.SessionOffset != 4 || string(out.Session) != "c\n" {
				t.Fatalf("fetched %v, out %+v", fetched, out)
			}
		})
	}
}

func TestRunTurnFailures(t *testing.T) {
	ok := func(*emulator, string, []string) (string, error) {
		return `{"result":"ok","session_id":"` + sid + `"}`, nil
	}
	for _, tc := range []struct {
		name   string
		in     TurnInput
		claude func(*emulator, string, []string) (string, error)
		want   string
	}{
		{"bad session id", TurnInput{Resume: "../x"}, ok, "is not a UUID"},
		{"fetch fails", TurnInput{Resume: sid, FetchSession: func(context.Context) ([]byte, error) { return nil, errors.New("gone") }}, ok, "fetch session"},
		{"fetched copy does not match", TurnInput{Resume: sid, SessionSize: 3, SessionSHA256: "00",
			FetchSession: func(context.Context) ([]byte, error) { return []byte("abc"), nil }}, ok, "does not match"},
		{"claude fails", TurnInput{}, func(*emulator, string, []string) (string, error) { return "", errors.New("exit 1") }, "claude"},
		{"documents too large", TurnInput{}, func(e *emulator, _ string, _ []string) (string, error) {
			e.ws["docs/big.md"] = make([]byte, MaxChangedBytes+1)
			return ok(e, "", nil)
		}, "over 24 MB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEmulator()
			e.claude = tc.claude
			tc.in.Workspace = workspaceTar(t, nil)
			tc.in.Bearer = testBearer
			_, err := newTurnSandbox(t, e).RunTurn(context.Background(), tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
		})
	}
	// 003-AC14: with no bearer for the account proxy, nothing runs in the sandbox.
	e := newEmulator()
	s := New(Config{Name: "t", Proxy: testProxy}, e.run, zap.NewNop())
	if _, err := s.RunTurn(context.Background(), TurnInput{}); err == nil || len(e.calls) != 0 {
		t.Fatalf("err %v, calls %v", err, e.calls)
	}
}

func TestParseHashes(t *testing.T) {
	h := strings.Repeat("a", 64)
	m, err := parseHashes([]byte(h + "  ./docs/new\nline.md\x00" + h + "  ./a b\x00"))
	if err != nil || m["docs/new\nline.md"] != h || m["a b"] != h || len(m) != 2 {
		t.Fatalf("%v %v", m, err)
	}
	for _, bad := range []string{"nonsense\x00", h + " ./x\x00", h + "  x\x00", "abc  ./x\x00"} {
		if _, err := parseHashes([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
