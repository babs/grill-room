package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/creds"
	"github.com/babs/grill-room/internal/daemon"
	"github.com/babs/grill-room/internal/sandbox"
)

// Distinctive host-only values: the privacy grep must not hit them by chance.
const (
	cfgAccount = "acct-cfg-zq7"
	tokAccount = "acct-tok-zq7"
)

// procMemCount counts secret in the readable memory of process pid. A test process may read its
// descendants' memory under Yama ptrace_scope 1.
func procMemCount(t *testing.T, pid int, secret []byte) int {
	t.Helper()
	maps, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		t.Fatal(err)
	}
	mem, err := os.Open(fmt.Sprintf("/proc/%d/mem", pid))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	n, regions := 0, 0
	sc := bufio.NewScanner(bytes.NewReader(maps))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || f[1][0] != 'r' {
			continue
		}
		lo, hi, _ := strings.Cut(f[0], "-")
		start, err1 := strconv.ParseUint(lo, 16, 64)
		end, err2 := strconv.ParseUint(hi, 16, 64)
		if err1 != nil || err2 != nil || end-start > 1<<30 {
			continue
		}
		buf := make([]byte, end-start)
		if _, err := mem.ReadAt(buf, int64(start)); err != nil && err != io.EOF { //nolint:gosec // addresses from maps
			continue
		}
		regions++
		n += bytes.Count(buf, secret)
	}
	if regions == 0 {
		t.Fatalf("no memory of %d read: the scan proves nothing", pid)
	}
	return n
}

// treeCount counts the files under root holding secret.
func treeCount(t *testing.T, root string, secret []byte) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			b, err := os.ReadFile(p) //nolint:gosec // the test's own state dir
			if err != nil {
				return err
			}
			if bytes.Contains(b, secret) {
				n++
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// events reads the room's event stream replay: every message the room shows.
func (rs *roomServer) events(t *testing.T, tok string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, rs.ts.URL+"/api/rooms/"+rs.room+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body) // ends at the deadline
	if !bytes.Contains(b, []byte("event:")) {
		t.Fatalf("no events: %q", b)
	}
	return b
}

// liveTurn makes the room's persona bare, asks for a one-word answer, presses the button and
// returns the finished turn.
func (rs *roomServer) liveTurn(t *testing.T, tok string) {
	t.Helper()
	var st struct {
		Online   bool
		Personas []struct{ ID int64 }
	}
	waitFor(t, "the room's host online", func() bool {
		rs.call(t, "GET", "/api/rooms/"+rs.room+"/agent", tok, nil, http.StatusOK, &st)
		return st.Online
	})
	// A bare persona: the Default text asks for a synthesis document, which costs requests.
	rs.call(t, "PATCH", fmt.Sprintf("/api/rooms/%s/personas/%d", rs.room, st.Personas[0].ID), tok,
		map[string]string{"text": "You are Grill. Answer in one word. Use no tool."}, http.StatusOK, nil)
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/messages", tok, map[string]string{"body": "Reply with the single word: ok."}, http.StatusCreated, nil)
	var started struct{ ID int64 }
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/turns", tok, map[string]bool{}, http.StatusCreated, &started)
	deadline := time.Now().Add(4 * time.Minute)
	for {
		tn, err := rs.st.Turn(t.Context(), rs.room, started.ID)
		if err != nil {
			t.Fatal(err)
		}
		if tn.Status != "running" {
			if tn.Status != "done" {
				t.Fatalf("turn %s: %s", tn.Status, tn.Error)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the turn did not end")
		}
		time.Sleep(time.Second)
	}
}

// 003-AC12 003-AC13 003-AC14 003-AC17 with real Claude (haiku), on this machine: one supervisor
// serves two rooms on two room servers, one on a Claude Code config dir account, the other on a
// setup token account. Each runs a real turn through its own account's proxy in the supervisor.
// Then no worker environment, worker memory, worker state dir or sandbox holds either real token,
// and no room response or stored prompt holds an account name, the config dir or the hostname.
// Only counts are printed. Run by make test-live-accounts; spends two haiku requests.
func TestLiveAccounts(t *testing.T) {
	tokenFile := os.Getenv("GRILL_ROOM_TEST_SETUP_TOKEN_FILE")
	image := os.Getenv("GRILL_ROOM_TEST_SANDBOX_IMAGE")
	if tokenFile == "" || image == "" {
		t.Skip("set GRILL_ROOM_TEST_SETUP_TOKEN_FILE and GRILL_ROOM_TEST_SANDBOX_IMAGE to spend two haiku requests")
	}
	raw, err := os.ReadFile(tokenFile) //nolint:gosec // the tester's token file
	if err != nil {
		t.Fatal(err)
	}
	setupTok := bytes.TrimSpace(raw)
	cfgDir := creds.ForConfigDir(os.Getenv("GRILL_ROOM_TEST_CONFIG_DIR")).ConfigDir
	cfgTok, err := creds.ForConfigDir(cfgDir).Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()

	rs1, rs2 := newRoomServer(t), newRoomServer(t)
	alice, bob := rs1.join(t, "Alice"), rs2.join(t, "Bob")
	state := filepath.Join(t.TempDir(), "state")
	env := []string{"GRILL_ROOM_DAEMON_STATE_DIR=" + state, "GRILL_ROOM_SANDBOX_IMAGE=" + image, "GRILL_ROOM_AGENT_MODEL=haiku",
		// No keyring: the setup token lands in the supervisor's owner-only file, which this test can see.
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/nonexistent",
		// A token in the supervisor's environment must not reach its workers.
		"CLAUDE_CODE_OAUTH_TOKEN=" + string(setupTok)}
	sup := startProc(t, env, rs1.url())
	pg := sup.open(t)
	rs1.pair(t, pg, alice)
	for _, body := range []map[string]string{
		{"name": cfgAccount, "kind": "config_dir", "config_dir": cfgDir},
		{"name": tokAccount, "kind": "setup_token", "token": string(setupTok)},
	} {
		if code, b := do(t, "POST", pg.base+"/api/accounts", pg.token, pg.base, body); code != http.StatusNoContent {
			t.Fatalf("add account %s: %d %s", body["name"], code, b)
		}
	}
	if code := startProc(t, env, rs2.url()).exit(t); code != 0 {
		t.Fatalf("handoff exit %d", code)
	}
	rs2.pair(t, pg, bob)
	id1, _ := daemon.InstanceID(rs1.ts.URL)
	id2, _ := daemon.InstanceID(rs2.ts.URL)
	for _, m := range []map[string]string{
		{"instance": id1, "account": cfgAccount},
		{"instance": id2, "account": cfgAccount},
		{"instance": id2, "room": rs2.url(), "account": tokAccount},
	} {
		if code, b := do(t, "POST", pg.base+"/api/instance-account", pg.token, pg.base, m); code != http.StatusNoContent {
			t.Fatalf("map %v: %d %s", m, code, b)
		}
	}
	rs1.liveTurn(t, alice)
	rs2.liveTurn(t, bob)
	used := map[string]int{}
	for _, line := range strings.Split(sup.out.String(), "\n") {
		var e struct{ Msg, Account, Path string }
		if json.Unmarshal([]byte(line), &e) == nil && e.Msg == "upstream_response" && e.Path == "/v1/messages" {
			used[e.Account]++
		}
	}
	t.Logf("real Claude requests per account: %v", used)
	if used[cfgAccount] == 0 || used[tokAccount] == 0 || len(used) != 2 {
		t.Fatalf("each room did not go through its own account's proxy: %v", used)
	}

	// 003-AC14: the scan. Positive controls first: the scanners find what the supervisor holds.
	secrets := map[string][]byte{"config-dir token": []byte(cfgTok.AccessToken), "setup token": setupTok}
	if procMemCount(t, sup.cmd.Process.Pid, setupTok) == 0 || treeCount(t, filepath.Join(state, "accounts"), setupTok) != 1 {
		t.Fatal("positive control: the supervisor's memory or token file shows no setup token, the scanners are blind")
	}
	if b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/environ", sup.cmd.Process.Pid)); !bytes.Contains(b, setupTok) {
		t.Fatal("positive control: the supervisor's environment shows no token")
	}
	var st pageStatus
	waitFor(t, "two running workers", func() bool { st = pg.status(t); return len(st.Instances) == 2 })
	total := 0
	for _, in := range st.Instances {
		environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", in.PID))
		if err != nil {
			t.Fatal(err)
		}
		r, _ := daemon.ParseRoomURL(in.Rooms[0])
		sb := sandbox.New(sandbox.Config{Name: daemon.SandboxName(state, r)}, sandbox.Docker, zap.NewNop())
		for what, secret := range secrets {
			counts, err := sb.Scan(t.Context(), secret)
			if err != nil {
				t.Fatal(err)
			}
			counts["worker environ"] = bytes.Count(environ, secret)
			counts["worker memory"] = procMemCount(t, in.PID, secret)
			counts["worker state dirs"] = treeCount(t, filepath.Join(state, "instances", in.ID), secret)
			t.Logf("instance %s, %s matches: %v", in.ID, what, counts)
			for _, n := range counts {
				total += n
			}
		}
	}
	if total != 0 {
		t.Errorf("a worker or a sandbox holds a real token: %d matches", total)
	}

	// 003-AC17: the privacy grep over what each room shows and each stored prompt.
	private := map[string]string{"config-dir account": cfgAccount, "setup-token account": tokAccount, "config dir": cfgDir, "hostname": host}
	hits := 0
	for _, r := range []struct {
		rs  *roomServer
		tok string
	}{{rs1, alice}, {rs2, bob}} {
		var seen bytes.Buffer
		for _, path := range []string{"", "/agent", "/documents"} {
			_, b := do(t, "GET", r.rs.ts.URL+"/api/rooms/"+r.rs.room+path, r.tok, "", nil)
			seen.Write(b)
		}
		seen.Write(r.rs.events(t, r.tok))
		tn, err := r.rs.st.LastTurn(t.Context(), r.rs.room)
		if err != nil || tn.Prompt == "" {
			t.Fatalf("stored prompt: %v", err)
		}
		seen.WriteString(tn.Prompt)
		if !bytes.Contains(seen.Bytes(), []byte(" holds the agent(s).")) {
			t.Error(`the room does not say "<name> holds the agent(s)"`)
		}
		for what, v := range private {
			n := bytes.Count(seen.Bytes(), []byte(v))
			t.Logf("room %s: %s matches: %d", r.rs.room, what, n)
			hits += n
		}
	}
	if hits != 0 {
		t.Errorf("host-only values reached the rooms: %d matches", hits)
	}
	if strings.Contains(sup.out.String(), string(setupTok)) || strings.Contains(sup.out.String(), cfgTok.AccessToken) {
		t.Error("a token reached the supervisor's logs")
	}
	if pg.post(t, "/api/stop", nil) != http.StatusNoContent || sup.exit(t) != 0 {
		t.Fatal("stop")
	}
}
