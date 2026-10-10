package daemon

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/creds"
	"github.com/babs/grill-room/internal/sandbox"
	"github.com/babs/grill-room/internal/turn"
	"github.com/babs/grill-room/internal/web"
)

// fakeAgent runs turns with run and records what it was given.
type fakeAgent struct {
	mu     sync.Mutex
	run    func(ctx context.Context, in sandbox.TurnInput, files map[string]string) (*sandbox.TurnOutput, error)
	inputs []sandbox.TurnInput
	closed bool
}

func (f *fakeAgent) RunTurn(ctx context.Context, in sandbox.TurnInput) (*sandbox.TurnOutput, error) {
	files := map[string]string{}
	tr := tar.NewReader(in.Workspace)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		b, _ := io.ReadAll(tr)
		files[h.Name] = string(b)
	}
	f.mu.Lock()
	f.inputs = append(f.inputs, in)
	f.mu.Unlock()
	return f.run(ctx, in, files)
}

func (f *fakeAgent) Close(context.Context) {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

// pairedHost pairs a daemon for a new participant and waits until the room sees it able to host.
func pairedHost(t *testing.T, rs *roomServer, name string, ag *fakeAgent) (participant string, cancel context.CancelFunc) {
	t.Helper()
	participant, _ = rs.join(t, name)
	d := newTestDaemon(t, dockerUp)
	d.o.Name = name + "-box"
	d.o.Model = "haiku"
	d.o.NewAgent = func(Room) Agent { return ag }
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

type agentView struct {
	HostID int64 `json:"host_participant_id"`
	Online bool  `json:"online"`
	Turn   *struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
		Error  string `json:"error"`
	} `json:"turn"`
}

func (rs *roomServer) agent(t *testing.T, tok string) agentView {
	t.Helper()
	var v agentView
	rs.call(t, "GET", "/api/rooms/"+rs.room+"/agent", tok, nil, http.StatusOK, &v)
	return v
}

func (rs *roomServer) lastMessage(t *testing.T) string {
	t.Helper()
	msgs, err := rs.st.Messages(t.Context(), rs.room, 0, 1000)
	if err != nil || len(msgs) == 0 {
		t.Fatalf("messages: %v", err)
	}
	return msgs[len(msgs)-1].Body
}

const sid = "60dc4912-36ec-4225-a44a-6a6c1ed85b1b"

// 001-AC32 001-AC33 001-AC34: a press runs the turn on the host daemon, whose result lands in the
// room; when the host drops mid-turn the turn fails, the other daemon takes over, and its next
// turn resumes the same session from the room's copy.
func TestDaemonTurnsAndTakeover(t *testing.T) {
	rs := newRoomServer(t)
	agA := &fakeAgent{}
	alice, dropA := pairedHost(t, rs, "Alice", agA)
	block := make(chan struct{})
	agA.run = func(ctx context.Context, in sandbox.TurnInput, files map[string]string) (*sandbox.TurnOutput, error) {
		if len(agA.inputs) > 1 {
			<-ctx.Done() // the second turn hangs until the daemon goes away
			close(block)
			return nil, ctx.Err()
		}
		if in.Model != "haiku" || in.Resume != "" || !strings.Contains(in.Prompt, "The code word is PAPRIKA") {
			return nil, errors.New("unexpected input")
		}
		if _, ok := files["transcript/"+time.Now().UTC().Format(time.DateOnly)+".md"]; !ok {
			return nil, errors.New("no transcript in the workspace")
		}
		return &sandbox.TurnOutput{Result: &sandbox.Result{Result: "Noted.", SessionID: sid}, Session: []byte("line1\n"),
			Changes: []turn.Change{{Path: "docs/plan.md", Content: []byte("# Plan\n")}}}, nil
	}
	if v := rs.agent(t, alice); !v.Online {
		t.Fatalf("alice's daemon is not hosting: %+v", v)
	}
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/messages", alice, map[string]string{"body": "The code word is PAPRIKA."}, http.StatusCreated, nil)
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/turns", alice, map[string]bool{}, http.StatusCreated, nil)
	waitFor(t, "turn 1 done", func() bool { v := rs.agent(t, alice); return v.Turn != nil && v.Turn.Status == "done" })
	if m := rs.lastMessage(t); m != "Noted." {
		t.Fatalf("synthesis %q", m)
	}
	if docs, _ := rs.st.Documents(t.Context(), rs.room); len(docs) != 1 || docs[0].Name != "plan.md" || docs[0].Author != "Agent" {
		t.Fatalf("documents %+v", docs)
	}

	agB := &fakeAgent{}
	bob, _ := pairedHost(t, rs, "Bob", agB)
	agB.run = func(ctx context.Context, in sandbox.TurnInput, _ map[string]string) (*sandbox.TurnOutput, error) {
		if in.Resume != sid || in.SessionSize != 6 {
			return nil, errors.New("not resuming the room's session")
		}
		data, err := in.FetchSession(ctx)
		if err != nil || string(data) != "line1\n" {
			return nil, errors.New("session copy differs")
		}
		return &sandbox.TurnOutput{Result: &sandbox.Result{Result: "PAPRIKA", SessionID: sid}, SessionOffset: 6, Session: []byte("line2\n")}, nil
	}
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/turns", alice, map[string]bool{}, http.StatusCreated, nil)
	waitFor(t, "turn 2 running on alice", func() bool { agA.mu.Lock(); defer agA.mu.Unlock(); return len(agA.inputs) == 2 })
	dropA()
	<-block
	// The room fails the turn, then elects: wait for both.
	waitFor(t, "turn 2 failed and bob hosting", func() bool { v := rs.agent(t, bob); return v.Turn.Status == "failed" && v.Online })
	if v := rs.agent(t, bob); !strings.Contains(v.Turn.Error, "Alice-box disconnected") {
		t.Fatalf("after alice dropped: %+v", v.Turn)
	}
	waitFor(t, "alice's sandbox closed", func() bool { agA.mu.Lock(); defer agA.mu.Unlock(); return agA.closed })
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/turns", bob, map[string]bool{}, http.StatusCreated, nil)
	waitFor(t, "turn 3 done", func() bool { v := rs.agent(t, bob); return v.Turn.Status == "done" })
	if m := rs.lastMessage(t); m != "PAPRIKA" {
		t.Fatalf("synthesis %q", m)
	}
	if data, _ := rs.st.AgentSession(t.Context(), rs.room, sid); string(data) != "line1\nline2\n" {
		t.Fatalf("session %q", data)
	}
	if revs, _ := rs.st.Revisions(t.Context(), 1); len(revs) != 1 {
		t.Fatalf("a revision from the failed turn was kept: %d", len(revs))
	}
}

// 001-AC29: an agent error, such as expired credentials, reaches the room as the turn's failure.
func TestDaemonTurnError(t *testing.T) {
	rs := newRoomServer(t)
	ag := &fakeAgent{run: func(context.Context, sandbox.TurnInput, map[string]string) (*sandbox.TurnOutput, error) {
		return nil, errors.New("Claude credentials expired on Alice-box")
	}}
	alice, _ := pairedHost(t, rs, "Alice", ag)
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/turns", alice, map[string]bool{}, http.StatusCreated, nil)
	waitFor(t, "turn failed", func() bool { v := rs.agent(t, alice); return v.Turn != nil && v.Turn.Status == "failed" })
	if m := rs.lastMessage(t); m != "Agent turn failed: Claude credentials expired on Alice-box" {
		t.Fatalf("message %q", m)
	}
}

// A turn past the room's timeout is cancelled and reported as timed out on this host.
func TestDaemonTurnTimeout(t *testing.T) {
	rs := newRoomServerOpts(t, web.Options{TurnTimeout: time.Second})
	ag := &fakeAgent{run: func(ctx context.Context, _ sandbox.TurnInput, _ map[string]string) (*sandbox.TurnOutput, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	alice, _ := pairedHost(t, rs, "Alice", ag)
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/turns", alice, map[string]bool{}, http.StatusCreated, nil)
	waitFor(t, "turn failed", func() bool { v := rs.agent(t, alice); return v.Turn != nil && v.Turn.Status == "failed" })
	if m := rs.lastMessage(t); m != "Agent turn failed: the turn timed out after 1s on Alice-box" {
		t.Fatalf("message %q", m)
	}
}

// A result the room refuses fails the turn there; the daemon only logs it.
func TestDaemonResultRefused(t *testing.T) {
	rs := newRoomServer(t)
	ag := &fakeAgent{run: func(context.Context, sandbox.TurnInput, map[string]string) (*sandbox.TurnOutput, error) {
		return &sandbox.TurnOutput{Result: &sandbox.Result{SessionID: sid}, SessionOffset: 5, Session: []byte("x")}, nil
	}}
	alice, _ := pairedHost(t, rs, "Alice", ag)
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/turns", alice, map[string]bool{}, http.StatusCreated, nil)
	waitFor(t, "turn failed", func() bool { v := rs.agent(t, alice); return v.Turn != nil && v.Turn.Status == "failed" })
	if v := rs.agent(t, alice); !strings.Contains(v.Turn.Error, "does not continue") {
		t.Fatalf("error %q", v.Turn.Error)
	}
}

func TestTakeTurnOnce(t *testing.T) {
	r := &roomRun{}
	for _, c := range []struct {
		id   int64
		want bool
	}{{1, true}, {1, false}, {3, true}, {2, false}, {4, true}} {
		if got := r.takeTurn(c.id); got != c.want {
			t.Fatalf("takeTurn(%d) = %v", c.id, got)
		}
	}
}

// A job the daemon cannot fetch is reported at once, so the room frees the button; a job with no
// timeout gets 15 minutes rather than none.
func TestRunTurnReportsFetchFailureAndDefaultsTimeout(t *testing.T) {
	var posted []turn.Result
	var mu sync.Mutex
	jobCode := http.StatusInternalServerError
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			var res turn.Result
			_ = json.NewDecoder(r.Body).Decode(&res)
			mu.Lock()
			posted = append(posted, res)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/workspace"):
			_ = tar.NewWriter(w).Close()
		case jobCode != http.StatusOK:
			w.WriteHeader(jobCode)
			_, _ = io.WriteString(w, `{"error":"boom"}`)
		default:
			_, _ = io.WriteString(w, `{"id":1,"prompt":"p","timeout_seconds":0}`)
		}
	}))
	defer ts.Close()
	d := newTestDaemon(t, dockerUp)
	var deadline time.Duration
	d.o.NewAgent = func(Room) Agent {
		return &fakeAgent{run: func(ctx context.Context, _ sandbox.TurnInput, _ map[string]string) (*sandbox.TurnOutput, error) {
			dl, _ := ctx.Deadline()
			deadline = time.Until(dl)
			return &sandbox.TurnOutput{Result: &sandbox.Result{SessionID: sid}}, nil
		}}
	}
	r := &roomRun{d: d, room: Room{Base: ts.URL, ID: "r"}}
	r.runTurn(context.Background(), "tok", 1)
	jobCode = http.StatusOK
	r.runTurn(context.Background(), "tok", 1)
	mu.Lock()
	defer mu.Unlock()
	if len(posted) != 2 || posted[0].Error != "could not fetch the turn on laptop: room server: boom" || posted[1].Error != "" {
		t.Fatalf("posted %+v", posted)
	}
	if deadline < 14*time.Minute {
		t.Fatalf("deadline %s", deadline)
	}
}

// 001-AC29: credentials that expire after the daemon was elected fail the turn through the real
// sandbox code with "Claude credentials expired on <host>" in the room, before Docker is touched.
func TestDaemonTurnCredentialsExpired(t *testing.T) {
	rs := newRoomServer(t)
	participant, _ := rs.join(t, "Alice")
	d := newTestDaemon(t, dockerUp)
	d.o.Name = "Alice-box"
	var dockerCalls []string
	var mu sync.Mutex
	run := func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		mu.Lock()
		dockerCalls = append(dockerCalls, strings.Join(args, " "))
		mu.Unlock()
		return nil, errors.New("docker must not run")
	}
	// The sandbox reads a token that has expired since the readiness check let the daemon host.
	dir := t.TempDir()
	body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"x","expiresAt":%d}}`, time.Now().Add(-time.Minute).UnixMilli())
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	expired := creds.NewSource(creds.Reader{ConfigDir: dir})
	d.o.NewAgent = func(Room) Agent {
		return sandbox.New(sandbox.Config{Name: "t", Host: d.o.Name, Upstream: &url.URL{}}, run, expired, d.o.Log)
	}
	d.Refresh(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	room, _ := ParseRoomURL(rs.ts.URL + "/r/" + rs.room)
	d.SetRoom(ctx, room)
	waitFor(t, "pending", func() bool { return d.Status().Pairing == "pending" })
	id := d.Status().ApproveURL[strings.Index(d.Status().ApproveURL, "=")+1:]
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/pairings/"+id, participant, map[string]bool{"approve": true}, http.StatusOK, nil)
	waitFor(t, "online", func() bool { return rs.agent(t, participant).Online })
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/turns", participant, map[string]bool{}, http.StatusCreated, nil)
	start := time.Now()
	waitFor(t, "turn failed", func() bool { v := rs.agent(t, participant); return v.Turn != nil && v.Turn.Status == "failed" })
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("took %s", took)
	}
	if m := rs.lastMessage(t); m != "Agent turn failed: Claude credentials expired on Alice-box" {
		t.Fatalf("message %q", m)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dockerCalls) != 0 {
		t.Fatalf("docker ran: %v", dockerCalls)
	}
}
