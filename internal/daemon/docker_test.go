package daemon

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"

	"github.com/babs/grill-room/internal/pgtest"
	"github.com/babs/grill-room/internal/plugin"
	"github.com/babs/grill-room/internal/sandbox"
	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/web"
)

// sandboxHost pairs a daemon whose turns run in a real Docker sandbox built from the fake claude
// image (make sandbox-fake-image), and returns its participant token and a func that drops it.
func sandboxHost(t *testing.T, rs *roomServer, name, image string) (string, context.CancelFunc) {
	t.Helper()
	participant, _, cancel := sandboxDaemon(t, rs, name, image)
	return participant, cancel
}

// sandboxDaemon is sandboxHost that also returns the daemon.
func sandboxDaemon(t *testing.T, rs *roomServer, name, image string) (string, *Daemon, context.CancelFunc) {
	t.Helper()
	participant, _ := rs.join(t, name)
	d := newTestDaemon(t, sandbox.Docker)
	d.o.Log = zaptest.NewLogger(t, zaptest.Level(zap.WarnLevel))
	// Alice keeps a workspace cache and Bob fetches whole workspaces: both unpack in a real
	// sandbox (002-AC37).
	if name == "Alice" {
		d.cache = &wsCache{dir: t.TempDir(), max: 64 << 20}
	}
	// Plugin inspections run from the image turns run in (009-AC13).
	d.st.Image.Ref, d.o.PluginCacheDir = image, t.TempDir()
	label := fmt.Sprintf("test-%s-%d", strings.ToLower(name), time.Now().UnixNano())
	// The fake claude sends nothing upstream: the relay only needs an address.
	d.o.NewAgent = func(_ Room, _ int, _, proxy, _ string) Agent {
		return sandbox.New(sandbox.Config{Image: image, Name: label, Proxy: proxy}, sandbox.Docker, d.o.Log)
	}
	t.Cleanup(func() {
		// A dropped daemon closes its sandbox; this catches a test that failed before.
		_ = exec.Command("docker", "rm", "-f", "-v", "grill-room-"+label, "grill-room-"+label+"-relay", "grill-room-"+label+"-egress").Run() //nolint:gosec,noctx // fixed test names
		_ = exec.Command("docker", "network", "rm", "grill-room-"+label, "grill-room-"+label+"-egress").Run()                                //nolint:gosec,noctx // fixed test names
		_ = exec.Command("docker", "volume", "rm", "-f", "grill-room-"+label+"-home", "grill-room-"+label+"-workspace").Run()                //nolint:gosec,noctx // fixed test names
	})
	d.Refresh(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	room, _ := ParseRoomURL(rs.ts.URL + "/r/" + rs.room)
	grant(d, room.String())
	d.SetRoom(ctx, room)
	waitFor(t, "pending", func() bool { return d.Status().Pairing == "pending" })
	id := d.Status().ApproveURL[strings.Index(d.Status().ApproveURL, "=")+1:]
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/pairings/"+id, participant, map[string]bool{"approve": true}, http.StatusOK, nil)
	waitFor(t, "can host", func() bool { return rs.hostsFor(t, participant) })
	return participant, d, cancel
}

// turn posts body, presses the button as tok and waits for the turn to end.
func (rs *roomServer) turn(t *testing.T, tok, body string, fresh bool) agentView {
	t.Helper()
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/messages", tok, map[string]string{"body": body}, http.StatusCreated, nil)
	var started struct {
		ID int64 `json:"id"`
	}
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/turns", tok, map[string]bool{"fresh": fresh}, http.StatusCreated, &started)
	var v agentView
	deadline := time.Now().Add(90 * time.Second)
	for v = rs.agent(t, tok); v.Turn.ID != started.ID || v.Turn.Status == "running"; v = rs.agent(t, tok) {
		if time.Now().After(deadline) {
			t.Fatalf("turn %d still running", started.ID)
		}
		time.Sleep(100 * time.Millisecond) // polls the turn, bounded by the deadline
	}
	return v
}

// 001-AC32 001-AC33 001-AC34 001-AC35 001-AC16 001-AC12 in real Docker sandboxes with a fake
// claude: a turn on A edits a document; A's edit to an upload is refused; A dies mid-turn and
// nothing is kept; B takes over and its claude resumes the session the room stored; a fresh
// session finds the room history in transcript/.
func TestDockerTurnsAndTakeover(t *testing.T) {
	if os.Getenv("GRILL_ROOM_TEST_DOCKER") != "1" {
		t.Skip("set GRILL_ROOM_TEST_DOCKER=1 to run the Docker sandbox tests")
	}
	image := os.Getenv("GRILL_ROOM_SANDBOX_FAKE_IMAGE")
	if image == "" {
		image = "grill-room-sandbox:fake"
	}
	rs := newRoomServer(t)
	alice, dropA := sandboxHost(t, rs, "Alice", image)
	pdf, err := rs.st.AddUpload(t.Context(), &store.Upload{RoomID: rs.room, Author: "Alice", Filename: "brief.pdf",
		ContentType: "application/pdf", Format: "pdf", Data: []byte("%PDF-1.4 brief")}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}

	v := rs.turn(t, alice, "The code word is PAPRIKA-42. FAKE_MARK", false)
	if v.Turn.Status != "done" || rs.lastMessage(t) != "Wrote docs/notes.md, turn 1." {
		t.Fatalf("turn 1: %+v %q", v.Turn, rs.lastMessage(t))
	}
	docs, _ := rs.st.Documents(t.Context(), rs.room)
	notes := docs[len(docs)-1]
	if notes.Name != "notes.md" || notes.Author != "Grill" {
		t.Fatalf("documents %+v", docs)
	}

	v = rs.turn(t, alice, "FAKE_EDIT_UPLOAD", false)
	if want := fmt.Sprintf("uploads are read-only, changes refused: uploads/%d-brief.pdf", pdf.ID); v.Turn.Status != "done" || !strings.Contains(rs.lastMessage(t), want) {
		t.Fatalf("turn 2: %+v %q", v.Turn, rs.lastMessage(t))
	}
	if orig, _ := rs.st.Original(t.Context(), rs.room, pdf.ID); string(orig) != "%PDF-1.4 brief" {
		t.Fatalf("upload changed: %q", orig)
	}
	stored, _ := rs.st.LastDoneTurn(t.Context(), rs.room, rs.grill(t).ID)
	sid := stored.SessionID

	bob, _ := sandboxHost(t, rs, "Bob", image)
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/messages", alice, map[string]string{"body": "FAKE_SLEEP"}, http.StatusCreated, nil)
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/turns", alice, map[string]bool{}, http.StatusCreated, nil)
	// A goes away mid-turn: once its turn runs, FAKE_SLEEP keeps it running.
	waitFor(t, "the turn running on A", func() bool { return rs.agent(t, alice).Turn.Status == "running" })
	dropA()
	waitFor(t, "bob hosts after the failed turn", func() bool {
		v := rs.agent(t, bob)
		return v.Turn.Status == "failed" && v.Online && strings.Contains(v.Turn.Error, "the host's daemon disconnected")
	})
	if revs, _ := rs.st.Revisions(t.Context(), notes.ID); len(revs) != 1 {
		t.Fatalf("partial revision kept: %d revisions", len(revs))
	}

	v = rs.turn(t, bob, "FAKE_RECALL", false)
	if v.Turn.Status != "done" || rs.lastMessage(t) != "The code word was PAPRIKA-42." {
		t.Fatalf("takeover: %+v %q", v.Turn, rs.lastMessage(t))
	}
	if last, _ := rs.st.LastDoneTurn(t.Context(), rs.room, rs.grill(t).ID); last.SessionID != sid {
		t.Fatalf("session %s after takeover, want %s", last.SessionID, sid)
	}

	v = rs.turn(t, bob, "FAKE_LIST", true)
	day := time.Now().UTC().Format(time.DateOnly)
	if v.Turn.Status != "done" || rs.lastMessage(t) != "transcript: "+day+"/001.md; FAKE_MARK found: true" {
		t.Fatalf("fresh: %+v %q", v.Turn, rs.lastMessage(t))
	}
	if last, _ := rs.st.LastDoneTurn(t.Context(), rs.room, rs.grill(t).ID); last.SessionID == sid || !last.Fresh {
		t.Fatalf("fresh turn kept session %s", last.SessionID)
	}
}

// 002-AC16 002-AC18 in real Docker sandboxes with a fake claude: Grill and Secretary run in one
// room, each in its own session; the Secretary gets its text and effort; after A drops, B resumes
// each persona's own session, and each recalls its own code word.
func TestDockerPersonasTakeover(t *testing.T) {
	if os.Getenv("GRILL_ROOM_TEST_DOCKER") != "1" {
		t.Skip("set GRILL_ROOM_TEST_DOCKER=1 to run the Docker sandbox tests")
	}
	image := os.Getenv("GRILL_ROOM_SANDBOX_FAKE_IMAGE")
	if image == "" {
		image = "grill-room-sandbox:fake"
	}
	rs := newRoomServer(t)
	alice, dropA := sandboxHost(t, rs, "Alice", image)
	var templates []store.Template
	rs.call(t, "GET", "/api/rooms/"+rs.room+"/templates", alice, nil, http.StatusOK, &templates)
	var sec store.RoomPersona
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/personas", alice, map[string]int64{"template_id": templates[1].ID}, http.StatusCreated, &sec)
	rs.call(t, "PATCH", "/api/rooms/"+rs.room+"/personas/"+fmt.Sprint(sec.ID), alice, map[string]string{"effort": "low"}, http.StatusOK, nil)
	grill := rs.grill(t)
	say := func(tok, body string, persona int64) string {
		t.Helper()
		rs.call(t, "POST", "/api/rooms/"+rs.room+"/messages", tok, map[string]string{"body": body}, http.StatusCreated, nil)
		var started store.Turn
		rs.call(t, "POST", "/api/rooms/"+rs.room+"/turns", tok, map[string]int64{"persona_id": persona}, http.StatusCreated, &started)
		waitFor(t, "turn ended", func() bool { v := rs.agent(t, tok); return v.Turn.ID == started.ID && v.Turn.Status != "running" })
		if v := rs.agent(t, tok); v.Turn.Status != "done" {
			t.Fatalf("turn %+v: %q", v.Turn, rs.lastMessage(t))
		}
		return rs.lastMessage(t)
	}
	say(alice, "The code word is GRILL-1.", grill.ID)
	say(alice, "The code word is SECRETARY-2.", sec.ID)
	if got := say(alice, "FAKE_WHOAMI", sec.ID); got != "You are Secretary, the note taker of this session. You record; you never challenge, judge or suggest. (effort low)" {
		t.Fatalf("secretary's persona: %q", got)
	}
	bob, _ := sandboxHost(t, rs, "Bob", image)
	dropA()
	waitFor(t, "bob hosts", func() bool {
		return rs.hostsFor(t, bob) && len(rs.daemons(t, bob)) == 1 && rs.agent(t, bob).Online
	})
	if got := say(bob, "FAKE_RECALL", grill.ID); got != "The code word was GRILL-1." {
		t.Fatalf("grill after takeover: %q", got)
	}
	if got := say(bob, "FAKE_RECALL", sec.ID); got != "The code word was SECRETARY-2." {
		t.Fatalf("secretary after takeover: %q", got)
	}
}

// 002-AC31 in a real Docker sandbox with a fake claude: the persona's skill reaches claude through
// --plugin-dir, its script runs in the sandbox and its output lands in a document; the pin holds
// across a library update until the room takes it; a skill turned off is gone from the next turn.
func TestDockerSkills(t *testing.T) {
	if os.Getenv("GRILL_ROOM_TEST_DOCKER") != "1" {
		t.Skip("set GRILL_ROOM_TEST_DOCKER=1 to run the Docker sandbox tests")
	}
	image := os.Getenv("GRILL_ROOM_SANDBOX_FAKE_IMAGE")
	if image == "" {
		image = "grill-room-sandbox:fake"
	}
	rs := newRoomServer(t)
	alice, _ := sandboxHost(t, rs, "Alice", image)
	k := rs.publishSkill(t, alice, "stamp", "STAMP-v1", true)
	doc := func() string {
		t.Helper()
		docs, _ := rs.st.Documents(t.Context(), rs.room)
		for _, d := range docs {
			if d.Name == "skills.md" {
				rev, _ := rs.st.Revision(t.Context(), d.ID, d.Revisions)
				return rev.Body
			}
		}
		return ""
	}
	for i, want := range []string{"STAMP-v1", "STAMP-v1", "STAMP-v2"} {
		switch i {
		case 1:
			rs.publishSkill(t, alice, "stamp", "STAMP-v2", false)
		case 2:
			rs.call(t, "PUT", fmt.Sprintf("/api/rooms/%s/personas/%d/skills/%d", rs.room, rs.grill(t).ID, k.ID), alice, map[string]bool{}, http.StatusOK, nil)
		}
		v := rs.turn(t, alice, fmt.Sprintf("FAKE_SKILLS %d", i), false)
		if v.Turn.Status != "done" || rs.lastMessage(t) != "Skills used: stamp: "+want+"." || doc() != "# Skills\n\n- stamp: "+want+"\n" {
			t.Fatalf("turn %d: %+v %q %q", i, v.Turn, rs.lastMessage(t), doc())
		}
	}
	rs.call(t, "DELETE", fmt.Sprintf("/api/rooms/%s/personas/%d/skills/%d", rs.room, rs.grill(t).ID, k.ID), alice, nil, http.StatusOK, nil)
	if v := rs.turn(t, alice, "FAKE_SKILLS off", false); v.Turn.Status != "done" || rs.lastMessage(t) != "No skills." {
		t.Fatalf("off: %+v %q", v.Turn, rs.lastMessage(t))
	}
}

// fixtureSnapshot is the 009 fixture plugin (github.com/babs/skills at 53195d8): from the local
// checkout in .scratchpad/fixture/skills, else fetched under GRILL_ROOM_TEST_LIVE_GIT=1, else a
// small synthetic plugin with the same skill.
func fixtureSnapshot(t *testing.T) *plugin.Snapshot {
	t.Helper()
	if _, err := os.Stat("../../.scratchpad/fixture/skills/.claude-plugin/plugin.json"); err == nil {
		s, err := plugin.FromDir("../../.scratchpad/fixture/skills", plugin.DefaultMaxBytes)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if os.Getenv("GRILL_ROOM_TEST_LIVE_GIT") == "1" {
		const repo, pin = "https://github.com/babs/skills", "53195d86d96aa2917ab23a9729c51d1b0118dc82"
		ctx, cancel := context.WithTimeout(t.Context(), plugin.Timeout)
		defer cancel()
		es, err := plugin.Fetch(ctx, plugin.NewClient(), repo, pin, plugin.DefaultMaxBytes)
		if err != nil {
			t.Fatal(err)
		}
		s, err := plugin.FromTree(es, "./", nil, plugin.DefaultMaxBytes)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	return snapshotOf(t, map[string]string{plugin.ManifestPath: `{"name":"babs","version":"1.0.0","description":"fixture"}`,
		"skills/spec-feature/SKILL.md": "---\nname: spec-feature\ndescription: Write a spec.\n---\nWrite specs/NNN-slug.md.\n"})
}

// 009-AC13 009-AC17 in a real Docker sandbox with the fake claude: the host's daemon inspects the
// fixture plugin in a throwaway container, which approves it; Grill's turn then gets two
// --plugin-dir, grill-room's then the plugin's, which holds plugin.json and the spec-feature skill
// at the snapshot's hashes. The Secretary, without the plugin, gets grill-room's only.
func TestDockerPluginTurn(t *testing.T) {
	if os.Getenv("GRILL_ROOM_TEST_DOCKER") != "1" {
		t.Skip("set GRILL_ROOM_TEST_DOCKER=1 to run the Docker sandbox tests")
	}
	image := os.Getenv("GRILL_ROOM_SANDBOX_FAKE_IMAGE")
	if image == "" {
		image = "grill-room-sandbox:fake"
	}
	rs := newRoomServer(t)
	alice, _ := sandboxHost(t, rs, "Alice", image)
	waitFor(t, "alice hosts", func() bool { return rs.agent(t, alice).Online })
	var templates []store.Template
	rs.call(t, "GET", "/api/rooms/"+rs.room+"/templates", alice, nil, http.StatusOK, &templates)
	var sec store.RoomPersona
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/personas", alice, map[string]int64{"template_id": templates[1].ID}, http.StatusCreated, &sec)
	k := rs.publishSkill(t, alice, "stamp", "STAMP", true)
	rs.call(t, "PUT", fmt.Sprintf("/api/rooms/%s/personas/%d/skills/%d", rs.room, sec.ID, k.ID), alice,
		map[string]bool{"accept_disclaimer": true}, http.StatusOK, nil)
	grill := rs.grill(t)
	snap := fixtureSnapshot(t)
	m, err := snap.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	rs.addPlugin(t, alice, m.Name, snap, grill.ID)
	deadline := time.Now().Add(90 * time.Second)
	for rs.version(t, m.Name).State == store.PluginInspecting {
		if time.Now().After(deadline) {
			t.Fatal("the plugin is still waiting for inspection")
		}
		time.Sleep(100 * time.Millisecond) // polls the version, bounded by the deadline
	}
	if v := rs.version(t, m.Name); v.State != store.PluginApproved {
		t.Fatalf("inspection: %s %q %s", v.State, v.Reason, v.Report)
	}
	hash := map[string]string{}
	for _, f := range snap.Files {
		hash[f.Path] = f.SHA256
	}
	ask := func(persona int64) string {
		t.Helper()
		rs.call(t, "POST", "/api/rooms/"+rs.room+"/messages", alice, map[string]string{"body": "FAKE_PLUGINS"}, http.StatusCreated, nil)
		var started store.Turn
		rs.call(t, "POST", "/api/rooms/"+rs.room+"/turns", alice, map[string]int64{"persona_id": persona}, http.StatusCreated, &started)
		deadline := time.Now().Add(90 * time.Second)
		for v := rs.agent(t, alice); v.Turn.ID != started.ID || v.Turn.Status == "running"; v = rs.agent(t, alice) {
			if time.Now().After(deadline) {
				t.Fatalf("turn %d still running", started.ID)
			}
			time.Sleep(100 * time.Millisecond) // polls the turn, bounded by the deadline
		}
		return rs.lastMessage(t)
	}
	got := ask(grill.ID)
	dirs := strings.Split(strings.TrimSuffix(strings.TrimPrefix(got, "Plugin dirs: "), "."), " | ")
	if len(dirs) != 2 || !strings.HasPrefix(dirs[0], "/home/agent/.grill-room/plugin [") || !strings.HasPrefix(dirs[1], "/home/agent/.grill-room/plugins/"+m.Name+" [") {
		t.Fatalf("grill's plugin dirs: %q", got)
	}
	for _, p := range []string{plugin.ManifestPath, "skills/spec-feature/SKILL.md"} {
		if hash[p] == "" || !strings.Contains(dirs[1], p+"="+hash[p]) {
			t.Fatalf("%s at %s missing from %q", p, hash[p], dirs[1])
		}
	}
	if got := ask(sec.ID); strings.Count(got, "/home/agent/") != 1 || !strings.HasPrefix(got, "Plugin dirs: /home/agent/.grill-room/plugin [") {
		t.Fatalf("secretary's plugin dirs: %q", got)
	}
}

// 009-AC13, Phase 2 DoD: `claude plugin validate` passes on the stored fixture snapshot in the
// real sandbox image (warnings allowed), through the daemon's own inspection; `plugin details`
// lists its spec-feature skill and a token cost. Neither command calls the API.
func TestDockerPluginValidateRealImage(t *testing.T) {
	if os.Getenv("GRILL_ROOM_TEST_DOCKER") != "1" {
		t.Skip("set GRILL_ROOM_TEST_DOCKER=1 to run the Docker sandbox tests")
	}
	image := os.Getenv("GRILL_ROOM_SANDBOX_IMAGE")
	if image == "" {
		image = "grill-room-sandbox:local"
	}
	rs := newRoomServer(t)
	alice, _, _ := pairedDaemon(t, rs, "Alice", &fakeAgent{}, func(d *Daemon) {
		d.o.Docker, d.st.Image.Ref = sandbox.Docker, image
		d.o.Log = zaptest.NewLogger(t, zaptest.Level(zap.WarnLevel))
	})
	snap := fixtureSnapshot(t)
	m, err := snap.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	rs.addPlugin(t, alice, m.Name, snap)
	deadline := time.Now().Add(90 * time.Second)
	for rs.version(t, m.Name).State == store.PluginInspecting {
		if time.Now().After(deadline) {
			t.Fatal("the plugin is still waiting for inspection")
		}
		time.Sleep(100 * time.Millisecond) // polls the version, bounded by the deadline
	}
	v := rs.version(t, m.Name)
	var rep pluginReport
	if err := json.Unmarshal(v.Report, &rep); err != nil {
		t.Fatal(err)
	}
	var skills []string
	if rep.Details != nil {
		for _, c := range rep.Details.Components {
			if c.Kind == "Skills" {
				skills = c.Names
			}
		}
	}
	if v.State != store.PluginApproved || !rep.OK || !rep.Details.CostKnown || !slices.Contains(skills, "spec-feature") {
		t.Fatalf("inspection: %s %q %s", v.State, v.Reason, v.Report)
	}
	t.Logf("validate: %s; always-on %d tokens", rep.Validate, rep.Details.AlwaysOnTokens)
}

// 009-AC13: inspecting a plugin runs none of its code. A SessionStart hook and a stdio MCP server
// that would each leave a file in the inspection's home leave nothing after `claude plugin
// validate` and `claude plugin details`, run by the daemon in the real sandbox image; `claude -p`
// in the same container, its API unreachable, does run the hook (the control).
func TestDockerPluginInspectionRunsNoHook(t *testing.T) {
	if os.Getenv("GRILL_ROOM_TEST_DOCKER") != "1" {
		t.Skip("set GRILL_ROOM_TEST_DOCKER=1 to run the Docker sandbox tests")
	}
	image := os.Getenv("GRILL_ROOM_SANDBOX_IMAGE")
	if image == "" {
		image = "grill-room-sandbox:local"
	}
	var mu sync.Mutex
	var home, control string
	details := false
	run := func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		mu.Lock()
		look := args[0] == "rm" && details && home == ""
		mu.Unlock()
		if look {
			// Before the container goes: what its home holds once both commands ran.
			out, err := sandbox.Docker(ctx, nil, "exec", args[len(args)-1], "ls", "-A", "/tmp")
			// Control, in the same container: claude -p runs the hook before its API call fails.
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			cout, cerr := sandbox.Docker(cctx, nil, "exec", "-e", "ANTHROPIC_API_KEY=dummy", "-e", "ANTHROPIC_BASE_URL=http://127.0.0.1:9",
				args[len(args)-1], "sh", "-c", "timeout 20 claude -p --plugin-dir /tmp/p hi >/dev/null 2>&1; ls -A /tmp")
			cancel()
			mu.Lock()
			home, control = string(out)+fmt.Sprint(err), string(cout)+fmt.Sprint(cerr)
			mu.Unlock()
		}
		out, err := sandbox.Docker(ctx, stdin, args...)
		if slices.Contains(args, "details") {
			mu.Lock()
			details = true
			mu.Unlock()
		}
		return out, err
	}
	rs := newRoomServer(t)
	alice, _, _ := pairedDaemon(t, rs, "Alice", &fakeAgent{}, func(d *Daemon) {
		d.o.Docker, d.st.Image.Ref = run, image
		d.o.Log = zaptest.NewLogger(t, zaptest.Level(zap.WarnLevel))
	})
	rs.addPlugin(t, alice, "hooked", snapshotOf(t, map[string]string{
		plugin.ManifestPath:  `{"name":"hooked","version":"1.0.0","description":"d","author":{"name":"a"}}`,
		"hooks/hooks.json":   `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"touch $HOME/hook.txt"}]}]}}`,
		".mcp.json":          `{"mcpServers":{"local":{"command":"sh","args":["-c","touch $HOME/mcp.txt"]}}}`,
		"skills/x/SKILL.md":  "---\nname: x\ndescription: d\n---\nx\n",
		"bin/tool":           "#!/bin/sh\ntouch $HOME/bin.txt\n",
		"agents/helper.md":   "---\nname: helper\ndescription: d\n---\nh\n",
		"commands/run.md":    "run\n",
		"skills/x/script.sh": "touch $HOME/script.txt\n",
	}))
	deadline := time.Now().Add(90 * time.Second)
	for rs.version(t, "hooked").State == store.PluginInspecting {
		if time.Now().After(deadline) {
			t.Fatal("the plugin is still waiting for inspection")
		}
		time.Sleep(100 * time.Millisecond) // polls the version, bounded by the deadline
	}
	v := rs.version(t, "hooked")
	var rep pluginReport
	_ = json.Unmarshal(v.Report, &rep)
	if v.State != store.PluginApproved || rep.Details == nil || len(rep.Details.Components) == 0 {
		t.Fatalf("inspection: %s %q %s", v.State, v.Reason, v.Report)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, f := range []string{"hook.txt", "mcp.txt", "bin.txt", "script.txt"} {
		if !strings.Contains(home, "p\n") || strings.Contains(home, f) {
			t.Fatalf("the inspection's home after both commands: %q", home)
		}
	}
	if !strings.Contains(control, "hook.txt") || !strings.Contains(control, "mcp.txt") {
		t.Fatalf("control: claude -p left no hook.txt or mcp.txt, the check proves nothing: %q", control)
	}
	t.Logf("home: %q; control: %q; details: %+v", home, control, rep.Details.Components)
}

// 009-AC22 009-AC23 009-AC24, Phase 3 DoD, in a real sandbox with the fake claude: a plugin with a
// required sensitive option and a SessionStart hook printing the option's length and value.
// Unset, the turn runs without the plugin and the room says it needs host setup. Set on the
// daemon, the hook gets CLAUDE_PLUGIN_OPTION_API_KEY from the files the daemon wrote, and the
// value it printed reaches the room as [redacted]: no trace of it in the server's database, the
// room's export or the server's log. The next turn resumes the session without fetching it.
func TestDockerPluginOptions(t *testing.T) {
	if os.Getenv("GRILL_ROOM_TEST_DOCKER") != "1" {
		t.Skip("set GRILL_ROOM_TEST_DOCKER=1 to run the Docker sandbox tests")
	}
	image := os.Getenv("GRILL_ROOM_SANDBOX_FAKE_IMAGE")
	if image == "" {
		image = "grill-room-sandbox:fake"
	}
	const secret = "s3cret-Value-42"
	core, logs := observer.New(zap.DebugLevel)
	var sessionFetches atomic.Int64
	rs := newRoomServerLog(t, web.Options{MaxUploadBytes: 1 << 20, MaxRoomBytes: 1 << 20, MaxAttachmentBytes: 1 << 20}, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/daemon/sessions/") {
				sessionFetches.Add(1)
			}
			h.ServeHTTP(w, r)
		})
	}, zap.New(core))
	alice, d, _ := sandboxDaemon(t, rs, "Alice", image)
	waitFor(t, "alice hosts", func() bool { return rs.agent(t, alice).Online })
	hook := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command",` +
		`"command":"mkdir -p docs; printf 'key %s\\n' \"$CLAUDE_PLUGIN_OPTION_API_KEY\" > docs/hook.md; echo len=${#CLAUDE_PLUGIN_OPTION_API_KEY} key=$CLAUDE_PLUGIN_OPTION_API_KEY"}]}]}}`
	snap := snapshotOf(t, map[string]string{
		plugin.ManifestPath: `{"name":"opt","userConfig":{"api_key":{"type":"string","title":"API key","sensitive":true,"required":true}}}`,
		"hooks/hooks.json":  hook,
	})
	rs.addPlugin(t, alice, "opt", snap, rs.grill(t).ID)
	deadline := time.Now().Add(90 * time.Second)
	for rs.version(t, "opt").State == store.PluginInspecting {
		if time.Now().After(deadline) {
			t.Fatal("the plugin is still waiting for inspection")
		}
		time.Sleep(100 * time.Millisecond) // polls the version, bounded by the deadline
	}
	if v := rs.version(t, "opt"); v.State != store.PluginApproved {
		t.Fatalf("inspection: %s %q", v.State, v.Reason)
	}
	needs := func() bool {
		ps, _ := rs.st.RoomPlugins(t.Context(), rs.room)
		return len(ps) == 1 && ps[0].NeedsSetup
	}
	if v := rs.turn(t, alice, "FAKE_HOOKS", false); v.Turn.Status != "done" || rs.lastMessage(t) != "Hooks: none." || !needs() {
		t.Fatalf("unset: %+v %q, needs setup %v", v.Turn, rs.lastMessage(t), needs())
	}
	waitFor(t, "the page lists the plugin", func() bool { rooms := d.Status().Rooms; return len(rooms) == 1 && len(rooms[0].Plugins) == 1 })
	if err := d.SetPluginOptions(t.Context(), rs.ts.URL+"/r/"+rs.room, d.Status().Rooms[0].Plugins[0].ID, map[string]any{"api_key": secret}, nil); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("Hooks: len=%d key=[redacted].", len(secret))
	if v := rs.turn(t, alice, "FAKE_HOOKS", false); v.Turn.Status != "done" || rs.lastMessage(t) != want || needs() {
		t.Fatalf("set: %+v %q, needs setup %v", v.Turn, rs.lastMessage(t), needs())
	}
	if v := rs.turn(t, alice, "FAKE_HOOKS", false); v.Turn.Status != "done" || rs.lastMessage(t) != want {
		t.Fatalf("next turn: %+v %q", v.Turn, rs.lastMessage(t))
	}
	if n := sessionFetches.Load(); n != 0 {
		t.Fatalf("the daemon fetched the session %d times", n)
	}
	var docs []store.Document
	rs.call(t, "GET", "/api/rooms/"+rs.room+"/documents", alice, nil, http.StatusOK, &docs)
	if !slices.ContainsFunc(docs, func(d store.Document) bool { return d.Name == "hook.md" }) {
		t.Fatalf("the hook's document is missing: %+v", docs)
	}

	req, _ := http.NewRequestWithContext(t.Context(), "GET", rs.ts.URL+"/api/admin/rooms/"+rs.room+"/export", nil)
	req.Header.Set("Authorization", "Bearer "+rs.admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("export: %d %v", resp.StatusCode, err)
	}
	export, err := io.ReadAll(gz)
	if err != nil || !strings.Contains(string(export), "[redacted]") {
		t.Fatalf("export: %v, %d bytes", err, len(export))
	}
	if hits, _ := pgtest.Find(t, rs.dsn, secret); len(hits) != 0 {
		t.Errorf("the database holds the option's value in %v", hits)
	}
	var logged strings.Builder
	for _, e := range logs.All() {
		b, _ := json.Marshal(e.ContextMap())
		logged.WriteString(e.Message + string(b) + "\n")
	}
	for what, b := range map[string]string{"export": string(export), "server log": logged.String()} {
		if strings.Contains(b, secret) {
			t.Errorf("the %s holds the option's value", what)
		}
	}
}
