package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
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
	d.o.Name = name + "-box"
	d.o.Log = zaptest.NewLogger(t, zaptest.Level(zap.WarnLevel))
	label := fmt.Sprintf("test-%s-%d", strings.ToLower(name), time.Now().UnixNano())
	up, _ := url.Parse("http://127.0.0.1:1")
	d.o.NewAgent = func(Room) Agent {
		return sandbox.New(sandbox.Config{Image: image, Name: label, Host: d.o.Name, Upstream: up}, sandbox.Docker, d.o.Creds, d.o.Log)
	}
	t.Cleanup(func() {
		// A dropped daemon closes its sandbox; this catches a test that failed before.
		_ = exec.Command("docker", "rm", "-f", "-v", "grill-room-"+label, "grill-room-"+label+"-relay").Run()                 //nolint:gosec,noctx // fixed test names
		_ = exec.Command("docker", "network", "rm", "grill-room-"+label).Run()                                                //nolint:gosec,noctx // fixed test names
		_ = exec.Command("docker", "volume", "rm", "-f", "grill-room-"+label+"-home", "grill-room-"+label+"-workspace").Run() //nolint:gosec,noctx // fixed test names
	})
	d.Refresh(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	room, _ := ParseRoomURL(rs.ts.URL + "/r/" + rs.room)
	d.SetRoom(ctx, room)
	waitFor(t, "pending", func() bool { return d.Status().Pairing == "pending" })
	id := d.Status().ApproveURL[strings.Index(d.Status().ApproveURL, "=")+1:]
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/pairings/"+id, participant, map[string]bool{"approve": true}, http.StatusOK, nil)
	waitFor(t, "can host", func() bool {
		for _, rd := range rs.daemons(t, participant) {
			if rd.Name == d.o.Name && rd.CanHost {
				return true
			}
		}
		return false
	})
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
	if notes.Name != "notes.md" || notes.Author != store.AgentName {
		t.Fatalf("documents %+v", docs)
	}

	v = rs.turn(t, alice, "FAKE_EDIT_UPLOAD", false)
	if want := fmt.Sprintf("uploads are read-only, changes refused: uploads/%d-brief.pdf", pdf.ID); v.Turn.Status != "done" || !strings.Contains(rs.lastMessage(t), want) {
		t.Fatalf("turn 2: %+v %q", v.Turn, rs.lastMessage(t))
	}
	if orig, _ := rs.st.Original(t.Context(), rs.room, pdf.ID); string(orig) != "%PDF-1.4 brief" {
		t.Fatalf("upload changed: %q", orig)
	}
	stored, _ := rs.st.LastDoneTurn(t.Context(), rs.room)
	sid := stored.SessionID

	bob, _ := sandboxHost(t, rs, "Bob", image)
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/messages", alice, map[string]string{"body": "FAKE_SLEEP"}, http.StatusCreated, nil)
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/turns", alice, map[string]bool{}, http.StatusCreated, nil)
	// Let claude start in A's sandbox before A goes away.
	time.Sleep(3 * time.Second)
	dropA()
	waitFor(t, "bob hosts after the failed turn", func() bool {
		v := rs.agent(t, bob)
		return v.Turn.Status == "failed" && v.Online && strings.Contains(v.Turn.Error, "Alice-box disconnected")
	})
	if revs, _ := rs.st.Revisions(t.Context(), notes.ID); len(revs) != 1 {
		t.Fatalf("partial revision kept: %d revisions", len(revs))
	}

	v = rs.turn(t, bob, "FAKE_RECALL", false)
	if v.Turn.Status != "done" || rs.lastMessage(t) != "The code word was PAPRIKA-42." {
		t.Fatalf("takeover: %+v %q", v.Turn, rs.lastMessage(t))
	}
	if last, _ := rs.st.LastDoneTurn(t.Context(), rs.room); last.SessionID != sid {
		t.Fatalf("session %s after takeover, want %s", last.SessionID, sid)
	}

	v = rs.turn(t, bob, "FAKE_LIST", true)
	day := time.Now().UTC().Format(time.DateOnly)
	if v.Turn.Status != "done" || rs.lastMessage(t) != "transcript: "+day+".md; FAKE_MARK found: true" {
		t.Fatalf("fresh: %+v %q", v.Turn, rs.lastMessage(t))
	}
	if last, _ := rs.st.LastDoneTurn(t.Context(), rs.room); last.SessionID == sid || !last.Fresh {
		t.Fatalf("fresh turn kept session %s", last.SessionID)
	}
}
