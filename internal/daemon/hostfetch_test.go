//go:build !windows

package daemon

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/plugin"
	"github.com/babs/grill-room/internal/store"
)

// fakeSSH stands for the host's ssh: GIT_SSH_COMMAND runs a script that logs each call to the
// file it returns, then runs the command git asks the remote end for (git-upload-pack '<path>')
// on this machine. The ssh transport runs end to end with no server and no key. The host's git
// config is replaced by gitconfig, empty unless the test writes it.
func fakeSSH(t *testing.T) (calls, gitconfig string) {
	t.Helper()
	return fakeSSHRunning(t, "for a; do last=$a; done\nexec sh -c \"$last\"\n")
}

// fakeSSHRunning is fakeSSH with the script's body after the logging line.
func fakeSSHRunning(t *testing.T, body string) (calls, gitconfig string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	dir := t.TempDir()
	calls, gitconfig = filepath.Join(dir, "calls"), filepath.Join(dir, "gitconfig")
	script := filepath.Join(dir, "ssh.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" >> \""+calls+"\"\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gitconfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSH_COMMAND", "sh "+script)
	t.Setenv("GIT_SSH_VARIANT", "simple")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", gitconfig)
	t.Setenv("HOME", dir)
	return calls, gitconfig
}

func gitCalls(t *testing.T, calls string) int {
	t.Helper()
	b, err := os.ReadFile(calls) //nolint:gosec // the test's own file
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

// bareRepo commits files to a new repo and returns the path of its bare clone and the commit.
func bareRepo(t *testing.T, files map[string]string) (path, commit string) {
	t.Helper()
	work, bare := t.TempDir(), filepath.Join(t.TempDir(), "repo.git")
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(work, "init", "-q", "-b", "main")
	for p, c := range files {
		full := filepath.Join(work, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run(work, "add", "-A")
	run(work, "commit", "-q", "-m", "c")
	run(work, "tag", "v1")
	run(work, "clone", "-q", "--bare", work, bare)
	return bare, run(work, "rev-parse", "HEAD")
}

var pluginFiles = map[string]string{
	".claude-plugin/plugin.json": `{"name":"priv","userConfig":{"api_key":{"type":"string","sensitive":true,"required":true}}}`,
	"skills/hi/SKILL.md":         "---\nname: hi\ndescription: Hi.\n---\nSay hi.\n",
}

// 009-AC20: the host's git fetches a repo over ssh (the transport end to end, through
// GIT_SSH_COMMAND), at the default branch or a tag, and the daemon builds the snapshot from the
// commit's files: the same bytes as the server's own fetch would store. Its folder is gone after.
func TestHostFetchOverSSH(t *testing.T) {
	calls, _ := fakeSSH(t)
	bare, commit := bareRepo(t, pluginFiles)
	dir := t.TempDir()
	for _, ref := range []string{"", "v1", "main", commit} {
		got, refName, snaps, err := hostFetchRepo(t.Context(), dir, HostFetch{URL: "ssh://git@example.invalid" + bare, Ref: ref}, plugin.DefaultMaxBytes)
		if err != nil {
			t.Fatalf("ref %q: %v", ref, err)
		}
		wantName := ref
		if ref == "" {
			wantName = "main"
		}
		if got != commit || refName != wantName || len(snaps) != 1 || snaps[0].entry != "" {
			t.Fatalf("ref %q: commit %s, ref name %q, %d snapshots", ref, got, refName, len(snaps))
		}
		var es []plugin.Entry
		for p, c := range pluginFiles {
			es = append(es, plugin.Entry{Path: p, Data: []byte(c)})
		}
		want, _ := plugin.FromTree(es, "", nil, plugin.DefaultMaxBytes)
		if snaps[0].snap.SHA256 != want.SHA256 {
			t.Fatalf("ref %q: snapshot %s, the server's would be %s", ref, snaps[0].snap.SHA256, want.SHA256)
		}
	}
	if gitCalls(t, calls) == 0 {
		t.Fatal("the fetch did not go through ssh")
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("fetch folders left: %v", left)
	}
	// The scp-like form reaches the same repo.
	if _, _, _, err := hostFetchRepo(t.Context(), dir, HostFetch{URL: "git@example.invalid:" + bare}, plugin.DefaultMaxBytes); err != nil {
		t.Fatalf("scp-like: %v", err)
	}
}

// 009-AC20: file:// and ext:: never run git; a link rewritten to file:// by the host's own git
// config (url.<base>.insteadOf) is refused by git itself, as the protocols are limited to https
// and ssh; an unknown repo is a git failure, which the room reads as "This AI host cannot reach
// the repo". A repo past the size cap is too big while it is fetched.
func TestHostFetchRefusals(t *testing.T) {
	calls, gitconfig := fakeSSH(t)
	bare, _ := bareRepo(t, pluginFiles)
	dir := t.TempDir()
	for _, u := range []string{"file://" + bare, "ext::sh -c touch% /tmp/pwned", "-oProxyCommand=x@h:p", "git@h:--upload-pack=x"} {
		if _, _, _, err := hostFetchRepo(t.Context(), dir, HostFetch{URL: u}, plugin.DefaultMaxBytes); !errors.Is(err, plugin.ErrHostURL) {
			t.Errorf("%s: %v", u, err)
		}
	}
	if _, _, _, err := hostFetchRepo(t.Context(), dir, HostFetch{URL: "ssh://git@example.invalid" + bare, Ref: "--upload-pack=x"}, plugin.DefaultMaxBytes); err == nil {
		t.Error("a ref as an option was taken")
	}
	if n := gitCalls(t, calls); n != 0 {
		t.Fatalf("%d git calls for refused links", n)
	}
	if err := os.WriteFile(gitconfig, []byte("[url \"file://"+bare+"\"]\n\tinsteadOf = https://rewritten.invalid/r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := hostFetchRepo(t.Context(), dir, HostFetch{URL: "https://rewritten.invalid/r"}, plugin.DefaultMaxBytes)
	var ge *gitError
	if !errors.As(err, &ge) || !strings.Contains(ge.stderr, "transport 'file' not allowed") {
		t.Fatalf("rewritten to file://: %v", err)
	}
	_, _, _, err = hostFetchRepo(t.Context(), dir, HostFetch{URL: "ssh://git@example.invalid/no/such/repo.git"}, plugin.DefaultMaxBytes)
	if !errors.As(err, &ge) {
		t.Fatalf("unknown repo: %v", err)
	}
	noise := make([]byte, 256<<10)
	_, _ = rand.Read(noise)
	big, _ := bareRepo(t, map[string]string{".claude-plugin/plugin.json": `{"name":"big"}`, "blob.bin": string(noise)})
	if _, _, _, err := hostFetchRepo(t.Context(), dir, HostFetch{URL: "ssh://git@example.invalid" + big}, 16<<10); !errors.Is(err, plugin.ErrTooBig) {
		t.Fatalf("past the cap: %v", err)
	}
}

// 009-AC6 009-AC20: a marketplace repo gives the entries the room named, and without names says
// which it holds.
func TestHostFetchMarketplace(t *testing.T) {
	fakeSSH(t)
	bare, _ := bareRepo(t, map[string]string{
		".claude-plugin/marketplace.json": `{"name":"m","plugins":[{"name":"a","source":"./a"},{"name":"b","source":"./b"},{"name":"r","source":{"source":"github","repo":"o/r"}}]}`,
		"a/.claude-plugin/plugin.json":    `{"name":"a"}`,
		"b/skills/x/SKILL.md":             "x",
	})
	url := "ssh://git@example.invalid" + bare
	_, _, snaps, err := hostFetchRepo(t.Context(), t.TempDir(), HostFetch{URL: url, Entries: []string{"a", "b"}}, plugin.DefaultMaxBytes)
	if err != nil || len(snaps) != 2 || snaps[0].entry != "a" || snaps[1].entry != "b" {
		t.Fatalf("entries: %v %+v", err, snaps)
	}
	if m, _ := snaps[1].snap.Manifest(); m == nil || m.Name != "b" {
		t.Fatalf("b's manifest from its marketplace entry: %+v", m)
	}
	if _, _, _, err := hostFetchRepo(t.Context(), t.TempDir(), HostFetch{URL: url}, plugin.DefaultMaxBytes); err == nil ||
		err.Error() != "the repo is the marketplace m: ask again naming the plugins to add, among a, b" {
		t.Fatalf("no names: %v", err)
	}
	// Only the marketplace and the picked plugin's files are read.
	dir := t.TempDir()
	if _, _, err := fetchCommit(t.Context(), dir, url, "", 1<<20); err != nil {
		t.Fatal(err)
	}
	tree, err := readTree(t.Context(), dir, []string{"a"}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range tree {
		if read := e.Data != nil; read != (e.Path != "b/skills/x/SKILL.md") {
			t.Errorf("%s read %v", e.Path, read)
		}
	}
	if _, _, _, err := hostFetchRepo(t.Context(), t.TempDir(), HostFetch{URL: url, Entries: []string{"r"}}, plugin.DefaultMaxBytes); err == nil ||
		!strings.Contains(err.Error(), plugin.ReasonNotInRepo) {
		t.Fatalf("an entry stored elsewhere: %v", err)
	}
}

// 009-AC19 009-AC20 009-AC21: a room's request reaches the host's page, and no git runs until the
// host presses Fetch. Fetch uploads the plugin, which the room adds private; a repo the host's
// git cannot reach fails the request with "This AI host cannot reach the repo"; Decline fails it
// with the host's word, running nothing.
func TestHostFetchFromTheRoom(t *testing.T) {
	calls, _ := fakeSSH(t)
	bare, commit := bareRepo(t, pluginFiles)
	rs := newRoomServer(t)
	alice, d, _ := pairedDaemon(t, rs, "Alice", &fakeAgent{}, func(d *Daemon) { d.o.FetchDir = t.TempDir() })
	link := rs.ts.URL + "/r/" + rs.room
	ps, err := rs.st.Participants(t.Context(), rs.room)
	if err != nil {
		t.Fatal(err)
	}
	ask := func(url string) int64 {
		t.Helper()
		f, _, err := rs.st.AddPluginFetch(t.Context(), rs.room, store.NewPluginFetch{URL: url}, &ps[0])
		if err != nil {
			t.Fatal(err)
		}
		// A chat line wakes the room's streams, as the route's notify does.
		rs.call(t, "POST", "/api/rooms/"+rs.room+"/messages", alice, map[string]string{"body": "fetch"}, http.StatusCreated, nil)
		waitFor(t, "the request on the page", func() bool {
			for _, r := range d.Status().Rooms {
				for _, f2 := range r.Fetches {
					if f2.ID == f.ID {
						return true
					}
				}
			}
			return false
		})
		return f.ID
	}
	state := func(id int64) *store.PluginFetch {
		f, err := rs.st.PluginFetch(t.Context(), rs.room, id)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	id := ask("ssh://git@example.invalid" + bare)
	if n := gitCalls(t, calls); n != 0 {
		t.Fatalf("git ran %d times before Fetch", n)
	}
	if err := d.PluginFetch(context.Background(), link, id, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the plugin added", func() bool { return state(id) == nil })
	got, err := rs.st.RoomPlugins(t.Context(), rs.room)
	if err != nil || len(got) != 1 || got[0].Name != "priv" || !got[0].Private || got[0].Versions[0].Commit != commit {
		t.Fatalf("plugins %v %+v", err, got)
	}
	if gitCalls(t, calls) == 0 {
		t.Fatal("Fetch ran no git")
	}

	id = ask("ssh://git@example.invalid/no/such/repo.git")
	if err := d.PluginFetch(context.Background(), link, id, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the request failed", func() bool { f := state(id); return f != nil && f.State == store.FetchFailed })
	if f := state(id); f.Reason != "This AI host cannot reach the repo" {
		t.Fatalf("reason %q", f.Reason)
	}

	id = ask("ssh://git@example.invalid" + bare)
	before := gitCalls(t, calls)
	if err := d.PluginFetch(context.Background(), link, id, false); err != nil {
		t.Fatal(err)
	}
	if f := state(id); f.State != store.FetchFailed || f.Reason != "the AI host declined it" || gitCalls(t, calls) != before {
		t.Fatalf("declined: %+v, git calls %d then %d", f, before, gitCalls(t, calls))
	}
	if err := d.PluginFetch(context.Background(), link, 999, true); !errors.Is(err, errNoRequest) {
		t.Fatalf("unknown request: %v", err)
	}
}

// 009-AC20: past the deadline, the fetch ends with git and everything it started: an ssh that
// stalls, holding git's pipes, is killed with it instead of holding the fetch.
func TestHostFetchStallKillsTheGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	fakeSSHRunning(t, "echo $$ > \""+pidFile+"\"\nexec sleep 60\n")
	defer func(old time.Duration) { hostFetchTimeout = old }(hostFetchTimeout)
	hostFetchTimeout = 500 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		_, _, _, err := hostFetchRepo(context.Background(), t.TempDir(), HostFetch{URL: "ssh://git@example.invalid/o/r.git"}, plugin.DefaultMaxBytes)
		done <- err
	}()
	select {
	case err := <-done:
		var ge *gitError
		if !errors.As(err, &ge) {
			t.Fatalf("stalled fetch: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the fetch waits on the stalled ssh")
	}
	b, err := os.ReadFile(pidFile) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	waitFor(t, "the stalled ssh gone", func() bool { return syscall.Kill(pid, 0) != nil })
}

// 009-AC20: git never runs an askpass program, whichever the host set: GIT_ASKPASS, SSH_ASKPASS
// or core.askPass. A repo asking for a login fails as unreachable instead.
func TestHostFetchNeverAsksForAPassword(t *testing.T) {
	_, gitconfig := fakeSSH(t)
	dir := t.TempDir()
	logged := filepath.Join(dir, "asked")
	askpass := filepath.Join(dir, "askpass.sh")
	if err := os.WriteFile(askpass, []byte("#!/bin/sh\necho \"$@\" >> \""+logged+"\"\necho guess\n"), 0o700); err != nil { //nolint:gosec // an executable test script
		t.Fatal(err)
	}
	if err := os.WriteFile(gitconfig, []byte("[core]\n\taskPass = "+askpass+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_ASKPASS", askpass)
	t.Setenv("SSH_ASKPASS", askpass)
	t.Setenv("GIT_SSL_NO_VERIFY", "1")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, _, _, err := hostFetchRepo(t.Context(), t.TempDir(), HostFetch{URL: srv.URL + "/o/r.git"}, plugin.DefaultMaxBytes)
	var ge *gitError
	if !errors.As(err, &ge) || roomReason(err) != errUnreachable.Error() {
		t.Fatalf("login asked: %v", err)
	}
	if _, err := os.Stat(logged); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("git ran an askpass program")
	}
}

// 009-AC21: the room reads git's failures as the host's access, what the repo or the request holds
// as it is, and nothing of any other failure on the host's machine.
func TestRoomReason(t *testing.T) {
	for err, want := range map[error]string{
		&gitError{cmd: "fetch", err: errors.New("exit status 128"), stderr: "fatal: repository not found"}: errUnreachable.Error(),
		fmt.Errorf("%w: the repo is over 1 MB", plugin.ErrTooBig):                                          "too big: the repo is over 1 MB",
		requestErrorf("the repo has no marketplace"):                                                       "the repo has no marketplace",
		fmt.Errorf("the plugin a: %w", plugin.ErrInvalid):                                                  "the plugin a: invalid plugin",
		plugin.ErrHostURL: plugin.ErrHostURL.Error(),
		fmt.Errorf("fetch folder: %w", os.ErrPermission): "the AI host's computer failed to fetch it: the reason is in its log",
	} {
		if got := roomReason(err); got != want {
			t.Errorf("%v: %q, want %q", err, got, want)
		}
	}
}

// 009-AC19 009-AC21: the page's presses on what cannot run say why: a request the room no longer
// lists, one fetched already, a room this daemon holds no token for; a room refusing the decline
// gets its error back. Plain helpers: the room link a page sends, a marketplace root's prefix.
func TestPluginFetchPressEdges(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusInternalServerError)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(`{"error":"room says no"}`))
	}))
	defer srv.Close()
	d := newTestDaemon(t, nil)
	room := Room{Base: srv.URL, ID: "r1"}
	r := &roomRun{d: d, room: room, ctx: t.Context(), plugins: &roomPlugins{Fetches: []HostFetch{{ID: 1, URL: "git@h:o/r"}}}}
	d.rooms[room.String()] = r
	if err := d.PluginFetch(t.Context(), room.String(), 2, false); !errors.Is(err, errNoRequest) {
		t.Fatalf("unknown: %v", err)
	}
	if err := d.PluginFetch(t.Context(), room.String(), 1, false); err == nil || !strings.Contains(err.Error(), "not paired") {
		t.Fatalf("no token: %v", err)
	}
	if err := d.o.Tokens.Set(room, "tok"); err != nil {
		t.Fatal(err)
	}
	if err := d.PluginFetch(t.Context(), room.String(), 1, false); err == nil || !strings.Contains(err.Error(), "room says no") {
		t.Fatalf("refused decline: %v", err)
	}
	d.mu.Lock()
	r.fetching = map[int64]bool{1: true}
	d.mu.Unlock()
	if err := d.PluginFetch(t.Context(), room.String(), 1, true); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("twice: %v", err)
	}
	if got := roomKey("not a link"); got != "not a link" {
		t.Fatalf("roomKey kept %q", got)
	}
	if got := roomKey(room.String() + "?x=1#y"); got != room.String() {
		t.Fatalf("roomKey %q", got)
	}
	for root, want := range map[string]string{"": "", "./": "", ".": "", "./plugins/a": "plugins/a/", "b/": "b/"} {
		if got := rootPrefix(root); got != want {
			t.Errorf("rootPrefix(%q) = %q, want %q", root, got, want)
		}
	}
	ge := &gitError{cmd: "fetch", err: errors.New("exit status 128"), stderr: "fatal: nope"}
	if ge.Error() != "git fetch: exit status 128: fatal: nope" || errors.Unwrap(ge) == nil || requestErrorf("bad %s", "ref").Error() != "bad ref" {
		t.Fatalf("error texts: %v", ge)
	}
}
