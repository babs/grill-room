package daemon

import (
	"archive/tar"
	"archive/zip"
	"bytes"
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
	"github.com/babs/grill-room/internal/skill"
	"github.com/babs/grill-room/internal/store"
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
	participant, _, cancel = pairedDaemon(t, rs, name, ag)
	return participant, cancel
}

// pairedDaemon is pairedHost, returning the daemon too.
func pairedDaemon(t *testing.T, rs *roomServer, name string, ag *fakeAgent) (participant string, d *Daemon, cancel context.CancelFunc) {
	t.Helper()
	participant, _ = rs.join(t, name)
	d = newTestDaemon(t, dockerUp)
	d.o.Name = name + "-box"
	d.o.Model = "haiku"
	d.o.NewAgent = func(Room, string) Agent { return ag }
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
	// The room elects its host just after it lists the daemon as able to host.
	waitFor(t, "a host online", func() bool { return rs.agent(t, participant).Online })
	return participant, d, cancel
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
	return rs.last(t).Body
}

func (rs *roomServer) last(t *testing.T) store.Message {
	t.Helper()
	msgs, err := rs.st.Messages(t.Context(), rs.room, 0, 1000)
	if err != nil || len(msgs) == 0 {
		t.Fatalf("messages: %v", err)
	}
	return msgs[len(msgs)-1]
}

const sid = "60dc4912-36ec-4225-a44a-6a6c1ed85b1b"

// grill is the room's Default persona.
func (rs *roomServer) grill(t *testing.T) store.RoomPersona {
	t.Helper()
	ps, err := rs.st.RoomPersonas(t.Context(), rs.room)
	if err != nil || len(ps) == 0 {
		t.Fatalf("personas: %+v %v", ps, err)
	}
	return ps[0]
}

// 001-AC32 001-AC33 001-AC34: a press runs the turn on the host daemon, whose result lands in the
// room; when the host drops mid-turn the turn fails, the other daemon takes over, and its next
// turn resumes the same session from the room's copy. Each synthesis names the model it ran on:
// the one claude reported, else the daemon's setting.
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
	if m := rs.last(t); m.Body != "Noted." || m.Model != "haiku" {
		t.Fatalf("synthesis %+v", m)
	}
	if docs, _ := rs.st.Documents(t.Context(), rs.room); len(docs) != 1 || docs[0].Name != "plan.md" || docs[0].Author != "Grill" {
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
		res := &sandbox.Result{Result: "PAPRIKA", SessionID: sid}
		if err := json.Unmarshal([]byte(`{"modelUsage": {"claude-side-x": {"outputTokens": 9}, "claude-opus-x": {"outputTokens": 900}}}`), res); err != nil {
			return nil, err
		}
		return &sandbox.TurnOutput{Result: res, SessionOffset: 6, Session: []byte("line2\n")}, nil
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
	if m := rs.last(t); m.Body != "PAPRIKA" || m.Model != "claude-opus-x" {
		t.Fatalf("synthesis %+v", m)
	}
	if done, _ := rs.st.LastDoneTurn(t.Context(), rs.room, rs.grill(t).ID); done.Model != "claude-opus-x" {
		t.Fatalf("turn model %q", done.Model)
	}
	if data, _ := rs.st.AgentSession(t.Context(), rs.room, rs.grill(t).ID, sid); string(data) != "line1\nline2\n" {
		t.Fatalf("session %q", data)
	}
	if revs, _ := rs.st.Revisions(t.Context(), 1); len(revs) != 1 {
		t.Fatalf("a revision from the failed turn was kept: %d", len(revs))
	}
}

// 002-AC16 002-AC18: two personas in one room each keep their own session; after a takeover the
// new host resumes each one's session, never the other's. Each turn gets its persona's text, the
// model and effort it asks for (an empty model falls back to the daemon's), and its synthesis
// carries them.
func TestDaemonPersonasKeepTheirSessions(t *testing.T) {
	rs := newRoomServer(t)
	run := func(name string) func(ctx context.Context, in sandbox.TurnInput, _ map[string]string) (*sandbox.TurnOutput, error) {
		return func(ctx context.Context, in sandbox.TurnInput, _ map[string]string) (*sandbox.TurnOutput, error) {
			sess := "22222222-2222-4222-8222-222222222222"
			if strings.HasPrefix(in.System, "You are Grill,") {
				sess = "11111111-1111-4111-8111-111111111111"
			}
			if in.Resume == "" {
				return &sandbox.TurnOutput{Result: &sandbox.Result{Result: name + " new " + in.Model + "/" + in.Effort, SessionID: sess}, Session: []byte(sess + "\n")}, nil
			}
			data, err := in.FetchSession(ctx)
			if err != nil {
				return nil, err
			}
			return &sandbox.TurnOutput{Result: &sandbox.Result{Result: name + " resumed " + strings.TrimSpace(string(data)), SessionID: in.Resume},
				SessionOffset: in.SessionSize, Session: []byte("more\n")}, nil
		}
	}
	agA := &fakeAgent{}
	agA.run = run("A")
	alice, dropA := pairedHost(t, rs, "Alice", agA)
	var templates []store.Template
	rs.call(t, "GET", "/api/rooms/"+rs.room+"/templates", alice, nil, http.StatusOK, &templates)
	var sec store.RoomPersona
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/personas", alice, map[string]int64{"template_id": templates[1].ID}, http.StatusCreated, &sec)
	rs.call(t, "PATCH", "/api/rooms/"+rs.room+"/personas/"+fmt.Sprint(sec.ID), alice, map[string]string{"model": "sonnet", "effort": "low"}, http.StatusOK, nil)
	grill := rs.grill(t)
	press := func(tok string, persona int64) store.Message {
		t.Helper()
		var started store.Turn
		rs.call(t, "POST", "/api/rooms/"+rs.room+"/turns", tok, map[string]int64{"persona_id": persona}, http.StatusCreated, &started)
		waitFor(t, "turn done", func() bool { v := rs.agent(t, tok); return v.Turn.ID == started.ID && v.Turn.Status != "running" })
		if v := rs.agent(t, tok); v.Turn.Status != "done" {
			t.Fatalf("turn %+v, last message %q", v.Turn, rs.lastMessage(t))
		}
		return rs.last(t)
	}
	if m := press(alice, grill.ID); m.Body != "A new haiku/" || m.Sender != "Grill" || m.PersonaID != grill.ID || m.Model != "haiku" || m.Effort != "" {
		t.Fatalf("grill on A: %+v", m)
	}
	if m := press(alice, sec.ID); m.Body != "A new sonnet/low" || m.Sender != "Secretary" || m.Model != "sonnet" || m.Effort != "low" || m.Badge != "📝" {
		t.Fatalf("secretary on A: %+v", m)
	}
	if in := agA.inputs[1]; !strings.HasPrefix(in.System, "You are Secretary,") || in.Prompt == "" || !strings.Contains(in.Prompt, "You are Secretary, an agent") {
		t.Fatalf("secretary's input: %+v", in)
	}

	agB := &fakeAgent{}
	agB.run = run("B")
	bob, _ := pairedHost(t, rs, "Bob", agB)
	dropA()
	waitFor(t, "bob is the host", func() bool {
		var st struct {
			Daemons []struct {
				Name string `json:"name"`
			} `json:"daemons"`
		}
		rs.call(t, "GET", "/api/rooms/"+rs.room+"/agent", bob, nil, http.StatusOK, &st)
		return len(st.Daemons) == 1 && st.Daemons[0].Name == "Bob-box" && rs.agent(t, bob).Online
	})
	if m := press(bob, sec.ID); m.Body != "B resumed 22222222-2222-4222-8222-222222222222" {
		t.Fatalf("secretary after takeover: %+v", m)
	}
	if m := press(bob, grill.ID); m.Body != "B resumed 11111111-1111-4111-8111-111111111111" {
		t.Fatalf("grill after takeover: %+v", m)
	}
	for _, p := range []struct {
		id  int64
		sid string
	}{{grill.ID, "11111111-1111-4111-8111-111111111111"}, {sec.ID, "22222222-2222-4222-8222-222222222222"}} {
		if data, err := rs.st.AgentSession(t.Context(), rs.room, p.id, p.sid); err != nil || string(data) != p.sid+"\nmore\n" {
			t.Fatalf("persona %d session %q %v", p.id, data, err)
		}
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
	if m := rs.lastMessage(t); m != "Grill turn failed: Claude credentials expired on Alice-box" {
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
	if m := rs.lastMessage(t); m != "Grill turn failed: the turn timed out after 1s on Alice-box" {
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
	d.o.NewAgent = func(Room, string) Agent {
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
	d.o.NewAgent = func(Room, string) Agent {
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
	if m := rs.lastMessage(t); m != "Grill turn failed: Claude credentials expired on Alice-box" {
		t.Fatalf("message %q", m)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dockerCalls) != 0 {
		t.Fatalf("docker ran: %v", dockerCalls)
	}
}

// The model and effort a room asks for reach claude's argv only when claude knows them; anything
// else falls back to the daemon's setting. 002-AC19 002-AC20: the cap clamps them, raised to what
// the host approved, and an approval the room sent is sanitised like a request.
func TestRunSettings(t *testing.T) {
	d := &Daemon{o: Options{Model: "haiku"}, st: Status{Settings: Settings{MaxModel: "sonnet", MaxEffort: "high"}}}
	for _, c := range []struct {
		model, effort      string
		approved           turn.Level
		wantModel, wantEff string
		capped             bool
	}{
		{"", "", turn.Level{}, "haiku", "", false},
		{"sonnet", "medium", turn.Level{}, "sonnet", "medium", false},
		{"opus", "max", turn.Level{}, "sonnet", "high", true},
		{"opus", "max", turn.Level{Model: "opus", Effort: "max"}, "opus", "max", false},
		{"fable", "max", turn.Level{Model: "opus", Effort: "xhigh"}, "opus", "xhigh", true},
		{"opus", "max", turn.Level{Model: "--verbose", Effort: "max;"}, "sonnet", "high", true},
		{"--dangerously-skip-permissions", "--verbose", turn.Level{}, "haiku", "", false},
		{"opus x", "high;", turn.Level{}, "haiku", "", false},
	} {
		run, capped := d.runSettings(turn.Job{Model: c.model, Effort: c.effort, Approved: c.approved})
		if run.Model != c.wantModel || run.Effort != c.wantEff || capped != c.capped {
			t.Errorf("%q/%q approved %v: got %v %v, want %q/%q %v", c.model, c.effort, c.approved, run, capped, c.wantModel, c.wantEff, c.capped)
		}
	}
}

// 002-AC19 002-AC20 002-AC36: the cap set on the daemon page reaches the room through the
// heartbeat; a persona asking above it runs clamped and its synthesis says so; once the host
// approves in the room, the next turn runs as asked, and the daemon's cap is unchanged.
func TestDaemonHostCap(t *testing.T) {
	rs := newRoomServer(t)
	ag := &fakeAgent{}
	ag.run = func(_ context.Context, in sandbox.TurnInput, _ map[string]string) (*sandbox.TurnOutput, error) {
		sess := "33333333-3333-4333-8333-333333333333"
		return &sandbox.TurnOutput{Result: &sandbox.Result{Result: in.Model + "/" + in.Effort, SessionID: sess}, Session: []byte("x\n"),
			SessionOffset: in.SessionSize}, nil
	}
	alice, d, _ := pairedDaemon(t, rs, "Alice", ag)
	if err := d.SetSettings(Settings{MaxModel: "sonnet", MaxEffort: "high"}); err != nil {
		t.Fatal(err)
	}
	type capView struct {
		Cap *struct {
			MaxModel string `json:"max_model"`
		} `json:"cap"`
		CapRequests []json.RawMessage `json:"cap_requests"`
	}
	var v capView
	waitFor(t, "the room has the cap", func() bool {
		rs.call(t, "GET", "/api/rooms/"+rs.room+"/agent", alice, nil, http.StatusOK, &v)
		return v.Cap != nil && v.Cap.MaxModel == "sonnet"
	})
	grill := rs.grill(t)
	rs.call(t, "PATCH", "/api/rooms/"+rs.room+"/personas/"+fmt.Sprint(grill.ID), alice, map[string]string{"model": "opus", "effort": "max"}, http.StatusOK, nil)
	press := func() store.Message {
		t.Helper()
		var started store.Turn
		rs.call(t, "POST", "/api/rooms/"+rs.room+"/turns", alice, map[string]int64{"persona_id": grill.ID}, http.StatusCreated, &started)
		waitFor(t, "turn done", func() bool { v := rs.agent(t, alice); return v.Turn.ID == started.ID && v.Turn.Status != "running" })
		return rs.last(t)
	}
	if m := press(); m.Body != "sonnet/high" || m.ReqModel != "opus" || m.ReqEffort != "max" || m.Model != "sonnet" || m.Effort != "high" {
		t.Fatalf("capped turn: %+v", m)
	}
	rs.call(t, "GET", "/api/rooms/"+rs.room+"/agent", alice, nil, http.StatusOK, &v)
	if len(v.CapRequests) != 1 {
		t.Fatalf("banner: %+v", v)
	}
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/personas/"+fmt.Sprint(grill.ID)+"/cap", alice, map[string]any{"approve": true, "model": "opus", "effort": "max"}, http.StatusOK, nil)
	if m := press(); m.Body != "opus/max" || m.ReqModel != "" {
		t.Fatalf("approved turn: %+v", m)
	}
	if s := d.Status().Settings; s.MaxModel != "sonnet" || s.MaxEffort != "high" {
		t.Fatalf("the approval changed the daemon's cap: %+v", s)
	}
}

// publishSkill stores, as the profile of participant tok, a skill whose script prints out, and
// turns it on for the room's Grill: the disclaimer accepted, its latest version pinned.
func (rs *roomServer) publishSkill(t *testing.T, tok, name, out string, activate bool) *store.Skill {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for path, body := range map[string]string{
		"SKILL.md":         "---\nname: " + name + "\ndescription: Prints a stamp.\n---\nRun `sh scripts/stamp.sh` from this folder.\n",
		"scripts/stamp.sh": "echo " + out + "\n",
	} {
		w, _ := zw.Create(path)
		_, _ = io.WriteString(w, body)
	}
	_ = zw.Close()
	sk, err := skill.Parse(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	p, err := rs.st.ParticipantByToken(t.Context(), web.HashToken(tok))
	if err != nil {
		t.Fatal(err)
	}
	k, err := rs.st.PublishSkill(t.Context(), p.ProfileID, false, sk)
	if err != nil {
		t.Fatal(err)
	}
	if activate {
		rs.call(t, "PUT", fmt.Sprintf("/api/rooms/%s/personas/%d/skills/%d", rs.room, rs.grill(t).ID, k.ID), tok,
			map[string]bool{"accept_disclaimer": true}, http.StatusOK, nil)
	}
	return k
}

// 002-AC31: the daemon hands the sandbox the persona's skills as the plugin directory the room
// built for the turn, and nothing for a persona without skills.
func TestDaemonLoadsPersonaSkills(t *testing.T) {
	rs := newRoomServer(t)
	ag := &fakeAgent{}
	ag.run = func(_ context.Context, in sandbox.TurnInput, _ map[string]string) (*sandbox.TurnOutput, error) {
		names := "none"
		if in.Plugin != nil {
			var got []string
			tr := tar.NewReader(in.Plugin)
			for h, err := tr.Next(); err == nil; h, err = tr.Next() {
				if h.Typeflag == tar.TypeReg {
					b, _ := io.ReadAll(tr)
					got = append(got, h.Name+"="+strings.TrimSpace(string(b)))
				}
			}
			names = strings.Join(got[1:], ",")
		}
		return &sandbox.TurnOutput{Result: &sandbox.Result{Result: names, SessionID: sid}, SessionOffset: in.SessionSize, Session: []byte("{}\n")}, nil
	}
	alice, _ := pairedHost(t, rs, "Alice", ag)
	if v := rs.turn(t, alice, "go", false); v.Turn.Status != "done" || rs.lastMessage(t) != "none" {
		t.Fatalf("no skill: %+v %q", v.Turn, rs.lastMessage(t))
	}
	rs.publishSkill(t, alice, "stamp", "STAMP-v1", true)
	if v := rs.turn(t, alice, "go", false); v.Turn.Status != "done" ||
		rs.lastMessage(t) != "skills/stamp/SKILL.md=---\nname: stamp\ndescription: Prints a stamp.\n---\nRun `sh scripts/stamp.sh` from this folder.,skills/stamp/scripts/stamp.sh=echo STAMP-v1" {
		t.Fatalf("with a skill: %+v %q", v.Turn, rs.lastMessage(t))
	}
}
