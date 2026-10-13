//go:build unix

package main

import (
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/daemon"
	"github.com/babs/grill-room/internal/sandbox"
	"github.com/babs/grill-room/internal/store"
)

// docker runs a docker command for the test, failing it on error.
func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput() //nolint:gosec,noctx // fixed test arguments
	if err != nil {
		t.Fatalf("docker %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// 003-AC20 003-AC8 in real Docker with the fake claude: in parallel mode two personas of a room run
// at once, each in its own sandbox. A supervisor killed with its worker leaves both behind; the
// next supervisor of the same state dir removes them at start, by their labels, and leaves alone
// what another state dir or another project labelled.
func TestDockerOrphansRemovedAtStart(t *testing.T) {
	if os.Getenv("GRILL_ROOM_TEST_DOCKER") != "1" {
		t.Skip("set GRILL_ROOM_TEST_DOCKER=1 to run the Docker sandbox tests")
	}
	image := os.Getenv("GRILL_ROOM_SANDBOX_FAKE_IMAGE")
	if image == "" {
		image = "grill-room-sandbox:fake"
	}
	rs := newRoomServer(t)
	alice := rs.join(t, "Alice")
	env := append(daemonEnv(t), "GRILL_ROOM_SANDBOX_IMAGE="+image)
	stateDir := strings.TrimPrefix(env[0], "GRILL_ROOM_DAEMON_STATE_DIR=")
	room, _ := daemon.ParseRoomURL(rs.url())
	first, second := daemon.SlotSandboxName(stateDir, room, 0), daemon.SlotSandboxName(stateDir, room, 1)
	sup := startProc(t, env, rs.url())
	pg := sup.open(t)
	rs.pair(t, pg, alice)
	rs.online(t, alice)

	var templates []store.Template
	rs.call(t, "GET", "/api/rooms/"+rs.room+"/templates", alice, nil, http.StatusOK, &templates)
	var sec store.RoomPersona
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/personas", alice, map[string]int64{"template_id": templates[1].ID}, http.StatusCreated, &sec)
	rs.call(t, "PATCH", "/api/rooms/"+rs.room+"/agent", alice, map[string]bool{"parallel_turns": true}, http.StatusNoContent, nil)
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/messages", alice, map[string]string{"body": "FAKE_SLOW"}, http.StatusCreated, nil)
	var running struct {
		Running []struct {
			ID         int64 `json:"id"`
			HostQueued bool  `json:"host_queued"`
		} `json:"running"`
	}
	agent := func() {
		rs.call(t, "GET", "/api/rooms/"+rs.room+"/agent", alice, nil, http.StatusOK, &running)
	}
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/turns", alice, map[string]bool{}, http.StatusCreated, nil)
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/turns", alice, map[string]int64{"persona_id": sec.ID}, http.StatusCreated, nil)
	waitFor(t, "two turns running at once", func() bool {
		agent()
		return len(running.Running) == 2 && !running.Running[0].HostQueued && !running.Running[1].HostQueued &&
			sandboxObjects(t, first) > 0 && sandboxObjects(t, second) > 0
	})
	ids := []int64{running.Running[0].ID, running.Running[1].ID}
	waitFor(t, "both turns done", func() bool { agent(); return len(running.Running) == 0 })
	for _, id := range ids {
		if tn, err := rs.st.Turn(t.Context(), rs.room, id); err != nil || tn.Status != store.TurnDone {
			t.Fatalf("turn %+v %v", tn, err)
		}
	}

	// Decoys: another state dir's sandbox volume, and a volume of another project.
	other := "grill-room-test-other-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	docker(t, "volume", "create", "--label", "grill-room-root=000000000000", "--label", sandbox.Label+"="+other, other)
	foreign := other + "-foreign"
	docker(t, "volume", "create", "--label", "com.example.project=other", foreign)
	t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", "-f", other, foreign).Run() }) //nolint:gosec,noctx // fixed test names

	// Frozen first, the worker cannot clean up when its supervisor dies: as when both are killed.
	worker, _ := os.FindProcess(onlyInstance(t, pg.status(t)).PID)
	if err := worker.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	if err := sup.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	sup.exit(t)
	if err := worker.Kill(); err != nil {
		t.Fatal(err)
	}
	if sandboxObjects(t, first) == 0 || sandboxObjects(t, second) == 0 {
		t.Fatal("the killed supervisor left no sandbox: nothing to test")
	}

	next := startProc(t, env)
	waitFor(t, "the orphans removed", func() bool { return strings.Contains(next.out.String(), "sandboxes_removed") })
	if n := sandboxObjects(t, first) + sandboxObjects(t, second); n != 0 {
		t.Fatalf("%d orphaned sandbox objects left", n)
	}
	if got := docker(t, "volume", "ls", "-q", "--filter", "name="+other); !strings.Contains(got, other) || !strings.Contains(got, foreign) {
		t.Fatalf("the cleanup removed another's volumes: %q", got)
	}
	if pg := next.open(t); pg.post(t, "/api/stop", nil) != http.StatusNoContent {
		t.Fatal("stop refused")
	}
	next.exit(t)
}
