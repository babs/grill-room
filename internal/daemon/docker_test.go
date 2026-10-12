package daemon

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/babs/grill-room/internal/sandbox"
	"github.com/babs/grill-room/internal/store"
)

// sandboxHost pairs a daemon whose turns run in a real Docker sandbox built from the fake claude
// image (make sandbox-fake-image), and returns its participant token and a func that drops it.
func sandboxHost(t *testing.T, rs *roomServer, name, image string) (string, context.CancelFunc) {
	t.Helper()
	participant, _ := rs.join(t, name)
	d := newTestDaemon(t, sandbox.Docker)
	d.o.Log = zaptest.NewLogger(t, zaptest.Level(zap.WarnLevel))
	// Alice keeps a workspace cache and Bob fetches whole workspaces: both unpack in a real
	// sandbox (002-AC37).
	if name == "Alice" {
		d.cache = &wsCache{dir: t.TempDir(), max: 64 << 20}
	}
	label := fmt.Sprintf("test-%s-%d", strings.ToLower(name), time.Now().UnixNano())
	// The fake claude sends nothing upstream: the relay only needs an address.
	d.o.NewAgent = func(_ Room, _ int, _, proxy string) Agent {
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
	return participant, cancel
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
		time.Sleep(100 * time.Millisecond)
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
	// Let claude start in A's sandbox before A goes away.
	time.Sleep(3 * time.Second)
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
