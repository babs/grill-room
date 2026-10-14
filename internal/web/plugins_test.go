package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/babs/grill-room/internal/plugin"
	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

// gitEnv isolates the git CLI from the user's and the system's config, as internal/plugin's
// tests do.
func gitEnv(home string) []string {
	return []string{"HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(home, "gitconfig"),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z"}
}

type gitServer struct {
	srv    *httptest.Server
	root   string
	home   string
	client *http.Client
}

// newGitServer serves every bare repo under its root with git http-backend over TLS, reached as
// example.com, the name in httptest's certificate. Its client dials the test server whatever the
// name, so the server's own address guard is not in the way.
func newGitServer(t *testing.T) *gitServer {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	out, err := exec.CommandContext(t.Context(), "git", "--exec-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	backend := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if runtime.GOOS == "windows" {
		backend += ".exe"
	}
	gs := &gitServer{root: t.TempDir(), home: t.TempDir()}
	gs.srv = httptest.NewTLSServer(&cgi.Handler{
		Path:       backend,
		Env:        append([]string{"GIT_PROJECT_ROOT=" + gs.root, "GIT_HTTP_EXPORT_ALL=1"}, gitEnv(gs.home)...),
		InheritEnv: []string{"PATH", "SYSTEMROOT", "TEMP", "TMP"},
	})
	t.Cleanup(gs.srv.Close)
	tr := gs.srv.Client().Transport.(*http.Transport).Clone()
	addr := gs.srv.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	gs.client = &http.Client{Transport: tr, Timeout: plugin.Timeout}
	return gs
}

type gitRepo struct {
	t   *testing.T
	gs  *gitServer
	dir string
}

func (gs *gitServer) newRepo(t *testing.T) *gitRepo {
	r := &gitRepo{t: t, gs: gs, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")
	return r
}

func (r *gitRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.CommandContext(r.t.Context(), "git", append([]string{"-c", "core.autocrlf=false"}, args...)...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), gitEnv(r.gs.home)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *gitRepo) write(name, body string) {
	r.t.Helper()
	p := filepath.Join(r.dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// commit commits everything and publishes the repo as name; it returns the commit and the URL.
func (r *gitRepo) commit(name string) (commit, url string) {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", "c")
	dst := filepath.Join(r.gs.root, name)
	_ = os.RemoveAll(dst)
	r.git("clone", "-q", "--bare", r.dir, dst)
	_, port, _ := net.SplitHostPort(r.gs.srv.Listener.Addr().String())
	return r.git("rev-parse", "HEAD"), "https://example.com:" + port + "/" + name
}

// marketRepo is a marketplace holding the plugin babs at its root, and an entry from elsewhere.
func marketRepo(t *testing.T, gs *gitServer) *gitRepo {
	r := gs.newRepo(t)
	r.write(".claude-plugin/marketplace.json", `{"name": "babs-skills", "plugins": [
		{"name": "babs", "source": "./", "description": "Babs's skills"},
		{"name": "remote", "source": {"source": "github", "repo": "o/r"}}]}`)
	r.write(".claude-plugin/plugin.json", `{"name": "babs", "version": "1.0.0"}`)
	r.write("skills/spec-feature/SKILL.md", "---\nname: spec-feature\ndescription: Specs.\n---\nWrite a spec.\n")
	return r
}

func pluginOpts(gs *gitServer) Options {
	o := testOptions
	o.PluginClient = gs.client
	return o
}

// appoint makes participant id a room admin, as the super-admin.
func (e *env) appoint(t *testing.T, room string, id int64) {
	t.Helper()
	if code, b := e.do(t, "PUT", fmt.Sprintf("/api/admin/rooms/%s/admins/%d", room, id), e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("appoint: %d %s", code, b)
	}
}

type pluginList struct {
	Plugins []pluginView `json:"plugins"`
}

func (e *env) plugins(t *testing.T, room, tok string) []pluginView {
	t.Helper()
	code, b := e.do(t, "GET", "/api/rooms/"+room+"/plugins", tok, nil)
	if code != http.StatusOK {
		t.Fatalf("list plugins: %d %s", code, b)
	}
	return decodeJSON[pluginList](t, b).Plugins
}

type checkResp struct {
	Commit      string      `json:"commit"`
	RefName     string      `json:"ref_name"`
	Marketplace string      `json:"marketplace"`
	Plugins     []candidate `json:"plugins"`
}

// endTurn stops turn id so that the next press starts at once.
func (e *env) endTurn(t *testing.T, r *turnRoom, tok string, id int64) {
	t.Helper()
	if code := e.stop(t, r, tok, id); code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("stop: %d", code)
	}
}

func jobPlugins(j turn.Job) string {
	var out []string
	for _, p := range j.Plugins {
		out = append(out, fmt.Sprintf("%s v%d %s", p.Name, p.Version, p.SHA256[:8]))
	}
	return strings.Join(out, ", ")
}

// 009-AC5 009-AC6 009-AC10 009-AC11 009-AC13 009-AC14 009-AC17 009-AC18: a room admin checks a
// marketplace repo and adds its plugin for a persona; the host's daemon gets it to inspect, and
// its report approves it; turns load only the approved version, as a tar by sha256; an update to
// the same commit is up to date, a new commit a new version that waits for the host while the
// approved one keeps running.
func TestPluginFlow(t *testing.T) {
	gs := newGitServer(t)
	e := newEnvOpts(t, pluginOpts(gs))
	r := newTurnRoom(t, e)
	e.appoint(t, r.room, r.alice.p.Participant.ID)
	tok, base := r.alice.p.Token, "/api/rooms/"+r.room+"/plugins"
	repo := marketRepo(t, gs)
	c1, url := repo.commit("skills.git")

	code, b := e.do(t, "POST", base+"/check", tok, map[string]string{"url": url})
	ck := decodeJSON[checkResp](t, b)
	if code != http.StatusOK || ck.Commit != c1 || ck.RefName != "main" || ck.Marketplace != "babs-skills" || len(ck.Plugins) != 2 {
		t.Fatalf("check: %d %s", code, b)
	}
	if p := ck.Plugins[0]; !p.Pickable || p.Name != "babs" || p.Manifest == nil || strings.Join(p.Manifest.Skills, ",") != "spec-feature" {
		t.Fatalf("babs: %+v", p)
	}
	if p := ck.Plugins[1]; p.Pickable || p.Reason != plugin.ReasonNotInRepo {
		t.Fatalf("remote: %+v", p)
	}
	if code, b := e.do(t, "POST", base, tok, map[string]any{"url": url, "entries": []string{"remote"}}); code != http.StatusUnprocessableEntity {
		t.Fatalf("add a disabled entry: %d %s", code, b)
	}
	if code, b := e.do(t, "POST", base, tok, map[string]any{"url": url}); code != http.StatusBadRequest {
		t.Fatalf("add with no entry picked: %d %s", code, b)
	}
	if code, b = e.do(t, "POST", base, tok, map[string]any{"url": url, "entries": []string{"babs"}, "personas": []int64{r.grill.ID}}); code != http.StatusCreated {
		t.Fatalf("add: %d %s", code, b)
	}
	ps := decodeJSON[pluginList](t, b).Plugins
	if len(ps) != 1 || ps[0].RefName != "main" || len(ps[0].Personas) != 1 || ps[0].Personas[0] != r.grill.ID || ps[0].Versions[0].State != store.PluginInspecting ||
		ps[0].Why != "the AI host is checking it" || ps[0].Versions[0].ShortCommit != c1[:7] {
		t.Fatalf("added: %s", b)
	}
	v1 := ps[0].Versions[0]
	if ev := nextEvent(t, r.alice.events, "inspect"); ev.data != fmt.Sprintf(`{"id":%d}`, v1.ID) {
		t.Fatalf("inspect event %+v, want version %d", ev, v1.ID)
	}
	code, b = e.do(t, "GET", r.daemonPath(fmt.Sprintf("plugins/%d/snapshot", v1.ID)), r.alice.tok, nil)
	sn, err := plugin.ValidateTar(bytes.NewReader(b), 1<<20)
	if code != http.StatusOK || err != nil || sn.SHA256 != v1.SHA256 {
		t.Fatalf("snapshot: %d %v", code, err)
	}

	// 009-AC18: inspecting, the plugin does not load.
	tn := e.press(t, r, r.alice, tok, false)
	if j := e.job(t, r, r.alice, tn.ID); len(j.Plugins) != 0 {
		t.Fatalf("job of an inspecting plugin: %+v", j.Plugins)
	}
	e.endTurn(t, r, tok, tn.ID)

	report := func(id int64, rep pluginReport, want int) string {
		t.Helper()
		code, b := e.do(t, "POST", r.daemonPath(fmt.Sprintf("plugins/%d/report", id)), r.alice.tok, rep)
		if code != want {
			t.Fatalf("report: %d %s, want %d", code, b, want)
		}
		return string(b)
	}
	if got := report(v1.ID, pluginReport{OK: true, Validate: json.RawMessage(`{"warnings":["CLAUDE.md"]}`), Details: &plugin.Details{AlwaysOnTokens: 120, CostKnown: true}}, http.StatusOK); !strings.Contains(got, "approved") {
		t.Fatalf("report: %s", got)
	}
	report(v1.ID, pluginReport{OK: true}, http.StatusConflict)
	ps = e.plugins(t, r.room, tok)
	if ps[0].Running != 1 || ps[0].Why != "" || !strings.Contains(string(ps[0].Versions[0].Report), `"always_on_tokens":120`) {
		t.Fatalf("approved: %+v", ps[0])
	}

	tn = e.press(t, r, r.alice, tok, false)
	j := e.job(t, r, r.alice, tn.ID)
	if jobPlugins(j) != "babs v1 "+v1.SHA256[:8] {
		t.Fatalf("job plugins: %q", jobPlugins(j))
	}
	code, b = e.do(t, "GET", r.daemonPath(fmt.Sprintf("turns/%d/plugins/%s", tn.ID, v1.SHA256)), r.alice.tok, nil)
	if code != http.StatusOK || !bytes.Equal(b, sn.Tar) {
		t.Fatalf("turn plugin tar: %d", code)
	}
	if code, _ := e.do(t, "GET", r.daemonPath(fmt.Sprintf("turns/%d/plugins/%s", tn.ID, strings.Repeat("0", 64))), r.alice.tok, nil); code != http.StatusNotFound {
		t.Fatalf("a tar the turn does not list: %d", code)
	}
	e.endTurn(t, r, tok, tn.ID)

	id := ps[0].ID
	update := fmt.Sprintf("%s/%d/update", base, id)
	if code, b := e.do(t, "POST", update, tok, nil); code != http.StatusOK || !strings.Contains(string(b), `"up_to_date":true`) {
		t.Fatalf("update to the same commit: %d %s", code, b)
	}
	repo.write("skills/review/SKILL.md", "---\nname: review\ndescription: Reviews.\n---\nReview.\n")
	c2, _ := repo.commit("skills.git")
	e.settings(t, r.room, r.alice, store.DaemonSettings{ApproveAll: true})
	code, b = e.do(t, "POST", update, tok, nil)
	pv := decodeJSON[pluginView](t, b)
	if code != http.StatusCreated || len(pv.Versions) != 2 || pv.Versions[1].Commit != c2 || pv.Running != 1 || pv.Versions[1].Change == nil ||
		strings.Join(pv.Versions[1].Change.ComponentsAdded, ",") != "skill:review" || strings.Join(pv.Versions[1].Change.FilesAdded, ",") != "skills/review/SKILL.md" {
		t.Fatalf("update: %d %s", code, b)
	}
	v2 := pv.Versions[1]
	nextEvent(t, r.alice.events, "inspect")
	if got := report(v2.ID, pluginReport{OK: true}, http.StatusOK); !strings.Contains(got, store.PluginPending) {
		t.Fatalf("report under approve everything: %s", got)
	}
	tn = e.press(t, r, r.alice, tok, false)
	if got := jobPlugins(e.job(t, r, r.alice, tn.ID)); got != "babs v1 "+v1.SHA256[:8] {
		t.Fatalf("job while v2 waits: %q", got)
	}
	e.endTurn(t, r, tok, tn.ID)
	bob := e.join(t, e.createInvite(t, r.room).Token, "Bob")
	decision := fmt.Sprintf("%s/%d/versions/2/decision", base, id)
	if code, _ := e.do(t, "POST", decision, bob.Token, map[string]bool{"approve": true}); code != http.StatusForbidden {
		t.Fatalf("a member decides: %d", code)
	}
	if code, b := e.do(t, "POST", decision, tok, map[string]bool{"approve": true}); code != http.StatusOK {
		t.Fatalf("host approves: %d %s", code, b)
	}
	if code, _ := e.do(t, "POST", decision, tok, map[string]bool{"approve": true}); code != http.StatusConflict {
		t.Fatalf("approved twice: %d", code)
	}
	tn = e.press(t, r, r.alice, tok, false)
	if got := jobPlugins(e.job(t, r, r.alice, tn.ID)); got != "babs v2 "+v2.SHA256[:8] {
		t.Fatalf("job after approval: %q", got)
	}
	e.endTurn(t, r, tok, tn.ID)
	if code, _ := e.do(t, "GET", r.daemonPath(fmt.Sprintf("turns/%d/plugins/%s", tn.ID, v2.SHA256)), r.alice.tok, nil); code != http.StatusConflict {
		t.Fatalf("tar of an ended turn: %d", code)
	}
	tn = e.press(t, r, r.alice, tok, false)
	// 009-AC12: removed while a turn loads it, the plugin's tar stays until that turn ends.
	if code, _ := e.do(t, "DELETE", fmt.Sprintf("%s/%d", base, id), tok, nil); code != http.StatusNoContent {
		t.Fatalf("remove: %d", code)
	}
	if code, _ := e.do(t, "GET", r.daemonPath(fmt.Sprintf("turns/%d/plugins/%s", tn.ID, v2.SHA256)), r.alice.tok, nil); code != http.StatusOK {
		t.Fatalf("tar of the running turn after removal: %d", code)
	}
	if ps := e.plugins(t, r.room, tok); len(ps) != 0 {
		t.Fatalf("listed after removal: %+v", ps)
	}
	var lines []string
	msgs, err := e.st.Messages(t.Context(), r.room, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if strings.Contains(m.Body, "plugin") {
			lines = append(lines, m.Body)
		}
	}
	want := []string{"Alice added the plugin babs, main @ " + c1[:7] + ".", "The AI host checked the plugin babs v1: approved automatically.",
		"Alice updated the plugin babs to v2, main @ " + c2[:7] + ".", "The AI host checked the plugin babs v2. It waits for the host's approval.",
		"Alice approved the plugin babs v2.", "Alice removed the plugin babs."}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("chat lines:\n%s", strings.Join(lines, "\n"))
	}
}

// 009-AC6: a repo without a marketplace and without plugin.json is one plugin named after the
// repo; a repo the server cannot read anonymously, or an ssh link, offers the host's fetch.
func TestPluginPlainRepoAndHostFetch(t *testing.T) {
	gs := newGitServer(t)
	e := newEnvOpts(t, pluginOpts(gs))
	r := newTurnRoom(t, e)
	e.appoint(t, r.room, r.alice.p.Participant.ID)
	tok, base := r.alice.p.Token, "/api/rooms/"+r.room+"/plugins"
	repo := gs.newRepo(t)
	repo.write("skills/hello/SKILL.md", "---\nname: hello\ndescription: Hi.\n---\nSay hi.\n")
	_, url := repo.commit("hello-tools.git")
	code, b := e.do(t, "POST", base, tok, map[string]any{"url": url})
	if ps := decodeJSON[pluginList](t, b).Plugins; code != http.StatusCreated || len(ps) != 1 || ps[0].Name != "hello-tools" {
		t.Fatalf("add: %d %s", code, b)
	}
	code, b = e.do(t, "POST", base+"/check", tok, map[string]any{"url": url})
	if ck := decodeJSON[checkResp](t, b); code != http.StatusOK || ck.Plugins[0].Pickable || ck.Plugins[0].Reason != store.ErrPluginName.Error() {
		t.Fatalf("check of a plugin the room has: %d %s", code, b)
	}
	for _, u := range []string{strings.Replace(url, "hello-tools", "missing", 1), "git@github.com:babs/private.git", "ssh://git@github.com/babs/private.git"} {
		code, b := e.do(t, "POST", base+"/check", tok, map[string]any{"url": u})
		if code != http.StatusUnprocessableEntity || !strings.Contains(string(b), `"host_fetch":true`) {
			t.Fatalf("%s: %d %s", u, code, b)
		}
	}
	code, b = e.do(t, "POST", base+"/check", tok, map[string]any{"url": "http://example.com/x"})
	if code != http.StatusBadRequest || strings.Contains(string(b), "host_fetch") {
		t.Fatalf("http link: %d %s", code, b)
	}
}

// 009-AC5: a plain member reads the plugins and gets a 403 on every write; deciding is the host's.
func TestPluginWritesNeedARoomAdmin(t *testing.T) {
	gs := newGitServer(t)
	e := newEnvOpts(t, pluginOpts(gs))
	r := newTurnRoom(t, e)
	e.appoint(t, r.room, r.alice.p.Participant.ID)
	_, url := marketRepo(t, gs).commit("skills.git")
	base := "/api/rooms/" + r.room + "/plugins"
	if code, b := e.do(t, "POST", base, r.alice.p.Token, map[string]any{"url": url, "entries": []string{"babs"}}); code != http.StatusCreated {
		t.Fatalf("add: %d %s", code, b)
	}
	id := e.plugins(t, r.room, r.alice.p.Token)[0].ID
	bob := e.join(t, e.createInvite(t, r.room).Token, "Bob")
	for _, c := range []struct{ method, path string }{
		{"POST", base + "/check"}, {"POST", base}, {"POST", fmt.Sprintf("%s/%d/update", base, id)}, {"DELETE", fmt.Sprintf("%s/%d", base, id)},
		{"PUT", fmt.Sprintf("/api/rooms/%s/personas/%d/plugins/%d", r.room, r.grill.ID, id)},
		{"DELETE", fmt.Sprintf("/api/rooms/%s/personas/%d/plugins/%d", r.room, r.grill.ID, id)},
		{"POST", fmt.Sprintf("%s/%d/versions/1/decision", base, id)},
	} {
		if code, b := e.do(t, c.method, c.path, bob.Token, map[string]any{"url": url, "entries": []string{"babs"}, "approve": true}); code != http.StatusForbidden {
			t.Errorf("%s %s: %d %s", c.method, c.path, code, b)
		}
	}
	if ps := e.plugins(t, r.room, bob.Token); len(ps) != 1 || ps[0].Name != "babs" {
		t.Fatalf("member's list: %+v", ps)
	}
	// The room admin toggles; a 6th plugin is past the persona's limit (009-AC10).
	toggle := fmt.Sprintf("/api/rooms/%s/personas/%d/plugins/%d", r.room, r.grill.ID, id)
	for _, m := range []string{"PUT", "PUT", "DELETE"} {
		if code, b := e.do(t, m, toggle, r.alice.p.Token, nil); code != http.StatusNoContent {
			t.Fatalf("%s toggle: %d %s", m, code, b)
		}
	}
	// Bob's daemon is no host: it neither reads a snapshot nor reports.
	bd := e.daemonFor(t, r.room, "Bobby")
	v := e.plugins(t, r.room, bob.Token)[0].Versions[0].ID
	if code, _ := e.do(t, "GET", r.daemonPath(fmt.Sprintf("plugins/%d/snapshot", v)), bd.tok, nil); code != http.StatusForbidden {
		t.Fatalf("non-host snapshot: %d", code)
	}
	if code, _ := e.do(t, "POST", r.daemonPath(fmt.Sprintf("plugins/%d/report", v)), bd.tok, pluginReport{OK: true}); code != http.StatusForbidden {
		t.Fatalf("non-host report: %d", code)
	}
}

// 009-AC13: only the host's daemon is told to inspect a version.
func TestInspectEventGoesToTheHostOnly(t *testing.T) {
	gs := newGitServer(t)
	e := newEnvOpts(t, pluginOpts(gs))
	r := newTurnRoom(t, e)
	e.appoint(t, r.room, r.alice.p.Participant.ID)
	bd := e.daemonFor(t, r.room, "Bobby")
	e.connect(t, r.room, bd, true)
	_, url := marketRepo(t, gs).commit("skills.git")
	if code, b := e.do(t, "POST", "/api/rooms/"+r.room+"/plugins", r.alice.p.Token, map[string]any{"url": url, "entries": []string{"babs"}}); code != http.StatusCreated {
		t.Fatalf("add: %d %s", code, b)
	}
	nextEvent(t, r.alice.events, "inspect")
	// Each switch of parallel turns sends both daemons a "room" event after anything the
	// preceding changes sent: two of them bound what Bob's stream got for the add.
	for _, on := range []bool{true, false} {
		if code, b := e.do(t, "PATCH", "/api/rooms/"+r.room+"/agent", r.alice.p.Token, map[string]bool{"parallel_turns": on}); code != http.StatusNoContent {
			t.Fatalf("parallel: %d %s", code, b)
		}
		for ev := next(t, bd.events); ev.event != "room"; ev = next(t, bd.events) {
			if ev.event == "inspect" {
				t.Fatalf("a non-host daemon got %+v", ev)
			}
		}
	}
}

// 009-AC7: a link with credentials is refused with the sentence the room shows, and neither
// logged nor stored.
func TestPluginCredentialsNeverLogged(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	e := newEnvLog(t, testOptions, zap.New(core))
	alice := e.device(t)
	room := alice.createRoom(t, "Own", "Alice")
	for _, u := range []string{"https://babs:s3cr3t-tok@example.com/skills.git", "http://ghp_s3cr3t-tok@example.com/x"} {
		for _, path := range []string{"/plugins/check", "/plugins"} {
			code, b := alice.do(t, "POST", "/api/rooms/"+room.ID+path, map[string]any{"url": u, "entries": []string{"x"}})
			if code != http.StatusBadRequest || decodeJSON[map[string]string](t, b)["error"] != plugin.ErrCredentials.Error() {
				t.Fatalf("%s: %d %s", path, code, b)
			}
		}
	}
	var all strings.Builder
	for _, l := range logs.All() {
		b, _ := json.Marshal(l.ContextMap())
		all.WriteString(l.Message + string(b) + "\n")
	}
	if !strings.Contains(all.String(), "plugin_url_refused") || strings.Contains(all.String(), "s3cr3t") || strings.Contains(all.String(), "example.com") {
		t.Fatalf("logs:\n%s", all.String())
	}
	for _, f := range []string{e.dbPath, e.dbPath + "-wal"} {
		if b, err := os.ReadFile(f); err == nil && bytes.Contains(b, []byte("s3cr3t")) {
			t.Fatalf("%s holds the secret", f)
		}
	}
}

// 009-AC9: the super-admin sets the snapshot cap, 1 to 200 MB, 20 by default; a fetch past it
// fails naming it.
func TestPluginMaxMB(t *testing.T) {
	gs := newGitServer(t)
	e := newEnvOpts(t, pluginOpts(gs))
	put := func(body map[string]any) (int, store.InstanceSettings) {
		code, b := e.do(t, "PUT", "/api/admin/settings", e.admin, body)
		if code != http.StatusOK {
			return code, store.InstanceSettings{}
		}
		return code, decodeJSON[store.InstanceSettings](t, b)
	}
	base := map[string]any{"rooms_per_profile": 10, "storage_quota_bytes": 0, "room_creation": "anyone"}
	if code, v := put(base); code != http.StatusOK || v.PluginMaxMB != store.DefaultPluginMaxMB {
		t.Fatalf("a page without the field keeps the default: %d %+v", code, v)
	}
	for _, bad := range []int{0, 201, -1} {
		base["plugin_max_mb"] = bad
		if code, _ := put(base); code != http.StatusBadRequest {
			t.Fatalf("plugin_max_mb %d: %d", bad, code)
		}
	}
	base["plugin_max_mb"] = 1
	if code, v := put(base); code != http.StatusOK || v.PluginMaxMB != 1 {
		t.Fatalf("1 MB: %d %+v", code, v)
	}
	r := newTurnRoom(t, e)
	e.appoint(t, r.room, r.alice.p.Participant.ID)
	repo := gs.newRepo(t)
	blob := make([]byte, 3<<19)
	_, _ = rand.Read(blob)
	repo.write("bin/blob", string(blob))
	_, url := repo.commit("big.git")
	code, b := e.do(t, "POST", "/api/rooms/"+r.room+"/plugins/check", r.alice.p.Token, map[string]any{"url": url})
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(b), "1 MB") {
		t.Fatalf("check past the cap: %d %s", code, b)
	}
}

// 009-AC8: one fetch per room, two over the server; the next gets a retry answer.
func TestPluginFetchSlots(t *testing.T) {
	e := newEnv(t)
	take := func(room string, want int) func() {
		t.Helper()
		w := httptest.NewRecorder()
		release, ok := e.srv.fetchSlot(w, room)
		if ok != (want == 0) || (want != 0 && w.Code != want) {
			t.Fatalf("%s: ok %v code %d, want %d", room, ok, w.Code, want)
		}
		return release
	}
	ra := take("a", 0)
	take("a", http.StatusConflict)
	rb := take("b", 0)
	take("c", http.StatusServiceUnavailable)
	ra()
	take("c", 0)()
	rb()
	take("a", 0)()
}

// 009-AC7: an error net/http built with the link loses it before a log or an answer.
func TestFetchErrorDropsTheLink(t *testing.T) {
	err := fmt.Errorf("git: %w", &neturl.Error{Op: "Get", URL: "https://example.com/x", Err: errors.New("connection refused")})
	if got := fetchError(err); strings.Contains(got, "example.com") || !strings.Contains(got, "connection refused") {
		t.Fatalf("fetchError: %q", got)
	}
	if got := fetchError(io.ErrUnexpectedEOF); got != io.ErrUnexpectedEOF.Error() {
		t.Fatalf("plain error: %q", got)
	}
}

// 009-AC10 009-AC11 009-AC13: the refusals of the plugin routes, each with its status.
func TestPluginRefusals(t *testing.T) {
	gs := newGitServer(t)
	e := newEnvOpts(t, pluginOpts(gs))
	r := newTurnRoom(t, e)
	e.appoint(t, r.room, r.alice.p.Participant.ID)
	tok, base := r.alice.p.Token, "/api/rooms/"+r.room+"/plugins"
	repo := marketRepo(t, gs)
	_, url := repo.commit("skills.git")
	other := newTurnRoom(t, e)
	for _, c := range []struct {
		method, path string
		body         any
		want         int
	}{
		{"POST", base, map[string]any{"url": url, "entries": []string{"babs"}, "personas": []int64{other.grill.ID}}, http.StatusNotFound},
		{"POST", base, map[string]any{"url": url, "entries": []string{"nope"}}, http.StatusUnprocessableEntity},
		{"POST", base, map[string]any{"url": url, "entries": strings.Split("a b c d e f g h i j k", " ")}, http.StatusConflict},
		{"POST", base + "/99/update", nil, http.StatusNotFound},
		{"DELETE", base + "/99", nil, http.StatusNotFound},
		{"PUT", fmt.Sprintf("/api/rooms/%s/personas/%d/plugins/99", r.room, r.grill.ID), nil, http.StatusNotFound},
		{"POST", base + "/99/versions/1/decision", map[string]bool{"approve": true}, http.StatusNotFound},
		{"GET", r.daemonPath("plugins/99/snapshot"), nil, http.StatusNotFound},
		{"POST", r.daemonPath("plugins/99/report"), pluginReport{OK: true}, http.StatusNotFound},
		{"POST", r.daemonPath("plugins/99/report"), map[string]any{"validate": strings.Repeat(" ", maxValidateJSON) + "{}"}, http.StatusRequestEntityTooLarge},
	} {
		auth := tok
		if strings.Contains(c.path, "/daemon/") {
			auth = r.alice.tok
		}
		if code, b := e.do(t, c.method, c.path, auth, c.body); code != c.want {
			t.Errorf("%s %s: %d %s, want %d", c.method, c.path, code, b, c.want)
		}
	}
	if code, b := e.do(t, "POST", base, tok, map[string]any{"url": url, "entries": []string{"babs"}}); code != http.StatusCreated {
		t.Fatalf("add: %d %s", code, b)
	}
	p := e.plugins(t, r.room, tok)[0]
	// A failed validation names its first error; one without says so.
	code, b := e.do(t, "POST", r.daemonPath(fmt.Sprintf("plugins/%d/report", p.Versions[0].ID)), r.alice.tok, pluginReport{})
	if code != http.StatusOK || !strings.Contains(string(b), store.PluginInvalid) {
		t.Fatalf("invalid: %d %s", code, b)
	}
	if got := e.plugins(t, r.room, tok)[0]; got.Why != "refused by Claude Code: the validation failed" {
		t.Fatalf("why: %q", got.Why)
	}
	update := fmt.Sprintf("%s/%d/update", base, p.ID)
	// A broken plugin.json, a snapshot past the room's quota, a broken marketplace: no new version.
	repo.write(".claude-plugin/plugin.json", "{")
	repo.commit("skills.git")
	if code, b := e.do(t, "POST", update, tok, nil); code != http.StatusUnprocessableEntity || !strings.Contains(string(b), "cannot be added") {
		t.Fatalf("broken plugin.json: %d %s", code, b)
	}
	repo.write(".claude-plugin/plugin.json", `{"name": "babs", "version": "2"}`)
	repo.commit("skills.git")
	if code, b := e.do(t, "PUT", "/api/admin/rooms/"+r.room+"/quota", e.admin, map[string]int64{"quota_bytes": 100}); code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("quota: %d %s", code, b)
	}
	if code, b := e.do(t, "POST", update, tok, nil); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("past the quota: %d %s", code, b)
	}
	repo.write(".claude-plugin/marketplace.json", "{")
	repo.commit("skills.git")
	for _, path := range []string{update, base + "/check"} {
		if code, b := e.do(t, "POST", path, tok, map[string]any{"url": url}); code != http.StatusUnprocessableEntity {
			t.Fatalf("broken marketplace on %s: %d %s", path, code, b)
		}
	}
	repo.write(".claude-plugin/marketplace.json", `{"name": "babs-skills", "plugins": [{"name": "babs", "source": "./"}]}`)
	// The plugin renamed upstream, then gone from the marketplace: no new version.
	repo.write(".claude-plugin/plugin.json", `{"name": "babs2"}`)
	repo.commit("skills.git")
	if code, b := e.do(t, "POST", update, tok, nil); code != http.StatusUnprocessableEntity || !strings.Contains(string(b), "named babs2") {
		t.Fatalf("renamed: %d %s", code, b)
	}
	repo.write(".claude-plugin/marketplace.json", `{"name": "m", "plugins": []}`)
	repo.commit("skills.git")
	if code, b := e.do(t, "POST", update, tok, nil); code != http.StatusUnprocessableEntity || !strings.Contains(string(b), "no longer lists") {
		t.Fatalf("gone: %d %s", code, b)
	}
	if code, b := e.do(t, "POST", base+"/check", tok, map[string]any{"url": url}); code != http.StatusOK || !strings.Contains(string(b), `"plugins":[]`) {
		t.Fatalf("empty marketplace: %d %s", code, b)
	}
}

// 009-AC13: a version waiting when the host role moves goes to the new host's daemon; the former
// host's daemon and another room's daemon can no longer read or report it. A reason over several
// lines lands on one.
func TestInspectFollowsTheHost(t *testing.T) {
	gs := newGitServer(t)
	e := newEnvOpts(t, pluginOpts(gs))
	r := newTurnRoom(t, e)
	e.appoint(t, r.room, r.alice.p.Participant.ID)
	bd := e.daemonFor(t, r.room, "Bobby")
	e.connect(t, r.room, bd, true)
	_, url := marketRepo(t, gs).commit("skills.git")
	if code, b := e.do(t, "POST", "/api/rooms/"+r.room+"/plugins", r.alice.p.Token, map[string]any{"url": url, "entries": []string{"babs"}}); code != http.StatusCreated {
		t.Fatalf("add: %d %s", code, b)
	}
	v := nextEvent(t, r.alice.events, "inspect").data
	if code, b := e.do(t, "PUT", "/api/rooms/"+r.room+"/host", r.alice.p.Token, map[string]int64{"participant_id": bd.p.Participant.ID}); code != http.StatusNoContent {
		t.Fatalf("hand over: %d %s", code, b)
	}
	if ev := nextEvent(t, bd.events, "inspect"); ev.data != v {
		t.Fatalf("new host got %+v, want %s", ev, v)
	}
	id := decodeJSON[struct{ ID int64 }](t, []byte(v)).ID
	report := r.daemonPath(fmt.Sprintf("plugins/%d/report", id))
	if code, _ := e.do(t, "POST", report, r.alice.tok, pluginReport{OK: true}); code != http.StatusForbidden {
		t.Fatalf("former host's report: %d", code)
	}
	o := newTurnRoom(t, e)
	if code, _ := e.do(t, "GET", r.daemonPath(fmt.Sprintf("plugins/%d/snapshot", id)), o.alice.tok, nil); code != http.StatusUnauthorized {
		t.Fatalf("another room's daemon reads: %d", code)
	}
	if code, _ := e.do(t, "POST", report, o.alice.tok, pluginReport{OK: true}); code != http.StatusUnauthorized {
		t.Fatalf("another room's daemon reports: %d", code)
	}
	if code, b := e.do(t, "POST", report, bd.tok, pluginReport{Error: "skills/x/SKILL.md: bad frontmatter\n\tline 2: name missing"}); code != http.StatusOK {
		t.Fatalf("new host reports: %d %s", code, b)
	}
	if why := e.plugins(t, r.room, r.alice.p.Token)[0].Why; why != "refused by Claude Code: skills/x/SKILL.md: bad frontmatter line 2: name missing" {
		t.Fatalf("why: %q", why)
	}
}

// 009-AC9 009-AC10 009-AC13: with no host connected a version waits "for the AI host to check
// it"; a second add of a plugin the room has is refused; a snapshot past the room's quota too.
func TestPluginWaitsAndRoomLimits(t *testing.T) {
	gs := newGitServer(t)
	e := newEnvOpts(t, pluginOpts(gs))
	alice := e.device(t)
	room := alice.createRoom(t, "Own", "Alice")
	base := "/api/rooms/" + room.ID + "/plugins"
	_, url := marketRepo(t, gs).commit("skills.git")
	add := map[string]any{"url": url, "entries": []string{"babs"}}
	if code, b := alice.do(t, "POST", base, add); code != http.StatusCreated {
		t.Fatalf("add: %d %s", code, b)
	}
	code, b := alice.do(t, "GET", base, nil)
	ps := decodeJSON[pluginList](t, b).Plugins
	if code != http.StatusOK || ps[0].Why != "waiting for the AI host to check it" || ps[0].Versions[0].Reason != ps[0].Why {
		t.Fatalf("no host: %d %s", code, b)
	}
	if code, b := alice.do(t, "POST", base, add); code != http.StatusConflict || !strings.Contains(string(b), store.ErrPluginName.Error()) {
		t.Fatalf("added twice: %d %s", code, b)
	}
	if code, b := alice.do(t, "DELETE", fmt.Sprintf("%s/%d", base, ps[0].ID), nil); code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", code, b)
	}
	if code, b := e.do(t, "PUT", "/api/admin/rooms/"+room.ID+"/quota", e.admin, map[string]int64{"quota_bytes": 100}); code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("quota: %d %s", code, b)
	}
	if code, b := alice.do(t, "POST", base, add); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("past the quota: %d %s", code, b)
	}
}

func marketTree(n int) []plugin.Entry {
	var ps []string
	for i := range n {
		ps = append(ps, fmt.Sprintf(`{"name": "p%d", "source": "./p%d"}`, i, i))
	}
	tree := []plugin.Entry{{Path: plugin.MarketplacePath, Data: []byte(`{"name": "m", "plugins": [` + strings.Join(ps, ",") + `]}`)}}
	for i := range n {
		tree = append(tree, plugin.Entry{Path: fmt.Sprintf("p%d/skills/s/SKILL.md", i), Data: []byte("---\nname: s\ndescription: d\n---\n")})
	}
	return tree
}

// 009-AC6: a marketplace lists its first 50 plugins as pickable and says why not for the rest; a
// broken marketplace, or a request gone, stops the listing.
func TestCandidates(t *testing.T) {
	keepAll := func(string) bool { return true }
	market, cs, err := candidates(t.Context(), &fetched{tree: marketTree(maxCandidates + 1), maxBytes: 1 << 20}, "https://example.com/r", keepAll)
	if err != nil || market != "m" || len(cs) != maxCandidates+1 || !cs[maxCandidates-1].Pickable || cs[maxCandidates-1].snap == nil ||
		cs[maxCandidates].Pickable || !strings.Contains(cs[maxCandidates].Reason, "first 50") {
		t.Fatalf("%s %d %v", market, len(cs), err)
	}
	if _, _, err := candidates(t.Context(), &fetched{tree: []plugin.Entry{{Path: plugin.MarketplacePath, Data: []byte("{")}}}, "", keepAll); err == nil {
		t.Fatal("a broken marketplace listed")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := candidates(ctx, &fetched{tree: marketTree(1), maxBytes: 1 << 20}, "", keepAll); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

// 009-AC18: why a plugin with no running version does not load, by its latest version's state;
// a stored manifest that does not parse reads as none.
func TestViewPluginWhy(t *testing.T) {
	for state, why := range map[string]string{store.PluginPending: "waiting for the host's approval", store.PluginRefused: "refused by the host",
		store.PluginInspecting: "waiting for the AI host to check it"} {
		p := store.RoomPlugin{Versions: []store.PluginVersion{{N: 1, Commit: "abc", State: state}}}
		if got := viewPlugin(p, false).Why; got != why {
			t.Errorf("%s: %q", state, got)
		}
	}
	if manifestOf(json.RawMessage("{")) != nil || repoName("::") != "" {
		t.Fatal("bad input read")
	}
}

// The fetch log names a class of failure, never the error's text.
func TestErrorClass(t *testing.T) {
	for err, want := range map[error]string{plugin.ErrNoAccess: "no_access", fmt.Errorf("x: %w", plugin.ErrNoV2): "no_v2",
		plugin.ErrTooBig: "too_big", plugin.ErrBlocked: "blocked", plugin.ErrInvalid: "invalid", context.DeadlineExceeded: "timeout",
		io.EOF: "other"} {
		if got := errorClass(err); got != want {
			t.Errorf("%v: %s, want %s", err, got, want)
		}
	}
}
