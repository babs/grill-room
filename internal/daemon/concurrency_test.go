package daemon

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/sandbox"
	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

// heldTurn is a turn the fake agent runs: it waits for the test to finish it.
type heldTurn struct {
	room, persona string
	slot          int
	files         map[string]string
	in            sandbox.TurnInput
	out           chan *sandbox.TurnOutput
}

// turnDesk hands the test every turn a fake agent starts, and lets it finish them one by one.
type turnDesk struct {
	started chan *heldTurn
	mu      sync.Mutex
	// sandboxes records each sandbox built, as "room/slot".
	sandboxes []string
}

var (
	roomInPrompt    = regexp.MustCompile(`an agent of the grill session "([^"]+)"`)
	personaInPrompt = regexp.MustCompile(`(?m)^You are (\S+), an agent`)
)

func newTurnDesk() *turnDesk { return &turnDesk{started: make(chan *heldTurn, 32)} }

// newAgent builds the fake sandbox slot of r: its turns go to the desk.
func (k *turnDesk) newAgent(r Room, slot int, _, _ string) Agent {
	k.mu.Lock()
	k.sandboxes = append(k.sandboxes, fmt.Sprintf("%s/%d", r.ID, slot))
	k.mu.Unlock()
	return &fakeAgent{run: func(ctx context.Context, in sandbox.TurnInput, files map[string]string) (*sandbox.TurnOutput, error) {
		h := &heldTurn{slot: slot, files: files, in: in, out: make(chan *sandbox.TurnOutput, 1)}
		if m := roomInPrompt.FindStringSubmatch(in.Prompt); m != nil {
			h.room = m[1]
		}
		if m := personaInPrompt.FindStringSubmatch(in.Prompt); m != nil {
			h.persona = m[1]
		}
		k.started <- h
		select {
		case out := <-h.out:
			return out, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
}

// next is the next turn to start; none within a few seconds fails the test.
func (k *turnDesk) next(t *testing.T) *heldTurn {
	t.Helper()
	select {
	case h := <-k.started:
		return h
	case <-time.After(10 * time.Second):
		t.Fatal("no turn started")
		return nil
	}
}

// idle checks that no turn starts for a while.
func (k *turnDesk) idle(t *testing.T) {
	t.Helper()
	select {
	case h := <-k.started:
		t.Fatalf("a turn of %s/%s started past the cap", h.room, h.persona)
	case <-time.After(300 * time.Millisecond):
	}
}

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// finish ends h with synthesis and changes, each "path=content".
func (h *heldTurn) finish(synthesis string, changes ...string) {
	out := &sandbox.TurnOutput{Result: &sandbox.Result{Result: synthesis, SessionID: newSessionID()}, Session: []byte("{}\n")}
	for _, c := range changes {
		path, content, _ := strings.Cut(c, "=")
		out.Changes = append(out.Changes, turn.Change{Path: path, Content: []byte(content)})
	}
	h.out <- out
}

// hostedRoom is one room of the room server, hosted by host.
type hostedRoom struct {
	id, name, host string
}

// runningView is the room's running turns as the room page reads them.
type runningView struct {
	Running []struct {
		ID         int64  `json:"id"`
		PersonaID  int64  `json:"persona_id"`
		HostQueued bool   `json:"host_queued"`
		RerunOf    int64  `json:"rerun_of"`
		Cause      string `json:"cause"`
		Status     string `json:"status"`
	} `json:"running"`
	Turn *struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
		Error  string `json:"error"`
	} `json:"turn"`
	Chain store.Chain `json:"chain"`
}

func (rs *roomServer) running(t *testing.T, r hostedRoom) runningView {
	t.Helper()
	var v runningView
	rs.call(t, "GET", "/api/rooms/"+r.id+"/agent", r.host, nil, http.StatusOK, &v)
	return v
}

// hostQueued reports whether the room shows a turn "queued (host busy)" (003-AC18).
func (v runningView) hostQueued() bool {
	return len(v.Running) == 1 && v.Running[0].HostQueued
}

// hostRoom creates a room named name on rs, joins Alice to it, and pairs d with it for her.
func (rs *roomServer) hostRoom(t *testing.T, d *Daemon, ctx context.Context, name string) hostedRoom {
	t.Helper()
	var room store.Room
	rs.call(t, "POST", "/api/rooms", rs.admin, map[string]string{"name": name}, http.StatusCreated, &room)
	var inv struct {
		Token string `json:"token"`
	}
	rs.call(t, "POST", "/api/rooms/"+room.ID+"/invites", rs.admin, nil, http.StatusCreated, &inv)
	var j struct {
		Token string `json:"token"`
	}
	rs.call(t, "POST", "/api/join", "", map[string]string{"invite": inv.Token, "name": "Alice"}, http.StatusCreated, &j)
	r, _ := ParseRoomURL(rs.ts.URL + "/r/" + room.ID)
	grant(d, r.String())
	d.SetRoom(ctx, r)
	waitFor(t, "pending", func() bool { s := d.Status(); return s.Room == r.String() && s.Pairing == "pending" })
	approve := d.Status().ApproveURL
	rs.call(t, "POST", "/api/rooms/"+room.ID+"/pairings/"+approve[strings.Index(approve, "=")+1:], j.Token, map[string]bool{"approve": true}, http.StatusOK, nil)
	h := hostedRoom{id: room.ID, name: name, host: j.Token}
	waitFor(t, "a host online", func() bool {
		var v agentView
		rs.call(t, "GET", "/api/rooms/"+room.ID+"/agent", h.host, nil, http.StatusOK, &v)
		return v.Online
	})
	return h
}

// slotsThrough is a worker's slot client talking to g over the protocol, as through a
// supervisor that serves instance.
func slotsThrough(t *testing.T, g *gate, instance string) *slots {
	t.Helper()
	upR, upW := io.Pipe()
	downR, downW := io.Pipe()
	t.Cleanup(func() { _ = upW.Close(); _ = downW.Close() })
	worker, sup := NewConn(downR, upW), NewConn(upR, downW)
	sl := &slots{conn: worker, waiting: map[int64]chan bool{}}
	go func() {
		for {
			m, err := sup.Recv()
			if err != nil {
				return
			}
			switch m.Type {
			case "acquire":
				g.acquire(slotKey{instance, m.Slot}, m.Turn.Room, m.Turn.ID, sup.Send)
			case "release":
				g.release(slotKey{instance, m.Slot})
			}
		}
	}()
	go func() {
		for {
			m, err := worker.Recv()
			if err != nil {
				return
			}
			sl.answer(m)
		}
	}()
	return sl
}

// cappedHost is a daemon, as one instance's worker, whose turns ask g for a slot.
func cappedHost(t *testing.T, desk *turnDesk, g *gate, instance string) (*Daemon, context.Context) {
	t.Helper()
	d := newTestDaemon(t, dockerUp)
	d.o.Model = "haiku"
	d.o.NewAgent = desk.newAgent
	d.acquire = slotsThrough(t, g, instance).acquire
	d.Refresh(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); d.Wait() })
	return d, ctx
}

func (rs *roomServer) press(t *testing.T, r hostedRoom, persona int64) store.Turn {
	t.Helper()
	var started store.Turn
	rs.call(t, "POST", "/api/rooms/"+r.id+"/turns", r.host, map[string]int64{"persona_id": persona}, http.StatusCreated, &started)
	return started
}

func (rs *roomServer) personaOf(t *testing.T, r hostedRoom) int64 {
	t.Helper()
	ps, err := rs.st.RoomPersonas(t.Context(), r.id)
	if err != nil || len(ps) == 0 {
		t.Fatalf("personas: %v", err)
	}
	return ps[0].ID
}

// waitDone waits until the room's last turn ended, and returns it.
func (rs *roomServer) waitDone(t *testing.T, r hostedRoom, id int64) *store.Turn {
	t.Helper()
	var got *store.Turn
	waitFor(t, "turn "+fmt.Sprint(id)+" ended", func() bool {
		tn, err := rs.st.Turn(t.Context(), r.id, id)
		got = tn
		return err == nil && tn.Status != store.TurnRunning
	})
	return got
}

// 003-AC18 003-AC19: with a global cap of one, three rooms of two instances take their turns one
// at a time, in the order they were pressed; the rooms waiting show "queued (host busy)". With a
// cap of two, two rooms run at once and the third waits.
func TestGlobalCapAndFIFO(t *testing.T) {
	for _, n := range []int{1, 2} {
		t.Run(fmt.Sprintf("cap %d", n), func(t *testing.T) {
			rs := newRoomServer(t)
			desk := newTurnDesk()
			g := newGate(n)
			// Two instances share the supervisor's queue: rooms 1 and 2 on one, room 3 on the other.
			d1, ctx1 := cappedHost(t, desk, g, "instance-a")
			d2, ctx2 := cappedHost(t, desk, g, "instance-b")
			rooms := []hostedRoom{rs.hostRoom(t, d1, ctx1, "R1"), rs.hostRoom(t, d1, ctx1, "R2"), rs.hostRoom(t, d2, ctx2, "R3")}
			if s := d1.Status(); len(s.Rooms) != 2 {
				t.Fatalf("one worker serves both rooms of its instance: %+v", s.Rooms)
			}
			var turns []store.Turn
			var held []*heldTurn
			for i, r := range rooms {
				turns = append(turns, rs.press(t, r, rs.personaOf(t, r)))
				if i < n {
					// Under the cap: it runs at once, beside the ones before it (003-AC19).
					held = append(held, desk.next(t))
					if held[i].room != r.name {
						t.Fatalf("turn %d ran %s, want %s", i, held[i].room, r.name)
					}
					continue
				}
				waitFor(t, r.name+" queued (host busy)", func() bool { return rs.running(t, r).hostQueued() })
			}
			desk.idle(t)
			for i := range rooms[:n] {
				if v := rs.running(t, rooms[i]); len(v.Running) != 1 || v.Running[0].HostQueued {
					t.Fatalf("%s runs: %+v", rooms[i].name, v)
				}
			}
			// Each finished turn lets the next waiting one run, first pressed first.
			for i := range rooms {
				held[i].finish("done " + rooms[i].name)
				if tn := rs.waitDone(t, rooms[i], turns[i].ID); tn.Status != store.TurnDone {
					t.Fatalf("%s: %+v", rooms[i].name, tn)
				}
				if next := i + n; next < len(rooms) {
					h := desk.next(t)
					if h.room != rooms[next].name {
						t.Fatalf("after %s, %s ran; want %s first", rooms[i].name, h.room, rooms[next].name)
					}
					held = append(held, h)
					if v := rs.running(t, rooms[next]); len(v.Running) != 1 || v.Running[0].HostQueued {
						t.Fatalf("%s left the host's queue but shows %+v", rooms[next].name, v)
					}
				}
			}
			v := g.view()
			if v.Granted != 3 || v.Waited != int64(3-n) || len(v.Running) != 0 || len(v.Queued) != 0 {
				t.Fatalf("gate after the turns: %+v", v)
			}
		})
	}
}

// addSecretary adds the Secretary persona to the room and turns parallel mode on or off.
func (rs *roomServer) addSecretary(t *testing.T, r hostedRoom, parallel bool) (grill, sec int64) {
	t.Helper()
	var templates []store.Template
	rs.call(t, "GET", "/api/rooms/"+r.id+"/templates", r.host, nil, http.StatusOK, &templates)
	var p store.RoomPersona
	rs.call(t, "POST", "/api/rooms/"+r.id+"/personas", r.host, map[string]int64{"template_id": templates[1].ID}, http.StatusCreated, &p)
	rs.call(t, "PATCH", "/api/rooms/"+r.id+"/agent", r.host, map[string]bool{"parallel_turns": parallel}, http.StatusNoContent, nil)
	return rs.personaOf(t, r), p.ID
}

// 003-AC20: with parallel on, two personas of a room run at once, each in its own sandbox. With
// it off, the second waits in the room's queue and the room keeps one sandbox.
func TestParallelPersonas(t *testing.T) {
	rs := newRoomServer(t)
	desk := newTurnDesk()
	d, ctx := cappedHost(t, desk, newGate(4), "instance")
	r := rs.hostRoom(t, d, ctx, "R")
	grill, sec := rs.addSecretary(t, r, false)

	t1 := rs.press(t, r, grill)
	h1 := desk.next(t)
	rs.call(t, "POST", "/api/rooms/"+r.id+"/turns", r.host, map[string]int64{"persona_id": sec}, http.StatusAccepted, nil)
	desk.idle(t)
	h1.finish("one")
	rs.waitDone(t, r, t1.ID)
	h2 := desk.next(t)
	h2.finish("two")
	waitFor(t, "the queued turn done", func() bool { return len(rs.running(t, r).Running) == 0 })
	desk.mu.Lock()
	if got := strings.Join(desk.sandboxes, " "); got != r.id+"/0" {
		t.Fatalf("parallel off built sandboxes %s", got)
	}
	desk.mu.Unlock()

	rs.call(t, "PATCH", "/api/rooms/"+r.id+"/agent", r.host, map[string]bool{"parallel_turns": true}, http.StatusNoContent, nil)
	tg := rs.press(t, r, grill)
	ts := rs.press(t, r, sec)
	a, b := desk.next(t), desk.next(t)
	if a.slot == b.slot || a.persona == b.persona {
		t.Fatalf("parallel turns share a sandbox: %s in %d, %s in %d", a.persona, a.slot, b.persona, b.slot)
	}
	if v := rs.running(t, r); len(v.Running) != 2 {
		t.Fatalf("running %+v", v.Running)
	}
	a.finish("a")
	b.finish("b")
	for _, id := range []int64{tg.ID, ts.ID} {
		if tn := rs.waitDone(t, r, id); tn.Status != store.TurnDone {
			t.Fatalf("turn %+v", tn)
		}
	}
	desk.mu.Lock()
	defer desk.mu.Unlock()
	if got := strings.Join(desk.sandboxes, " "); got != r.id+"/0 "+r.id+"/1" {
		t.Fatalf("sandboxes %s", got)
	}
}

// lastIn is the last message of room r.
func (rs *roomServer) lastIn(t *testing.T, r hostedRoom) store.Message {
	t.Helper()
	msgs, err := rs.st.Messages(t.Context(), r.id, 0, 1000)
	if err != nil || len(msgs) == 0 {
		t.Fatalf("messages: %v", err)
	}
	return msgs[len(msgs)-1]
}

// byPersona orders two turns started together: Grill's first.
func byPersona(a, b *heldTurn) (*heldTurn, *heldTurn) {
	if a.persona == "Grill" {
		return a, b
	}
	return b, a
}

// rerunning is the room's running re-run, or fails the test.
func (rs *roomServer) rerunning(t *testing.T, r hostedRoom) int64 {
	t.Helper()
	for _, tn := range rs.running(t, r).Running {
		if tn.RerunOf != 0 {
			return tn.ID
		}
	}
	t.Fatal("no re-run is running")
	return 0
}

// 003-AC21: in parallel, a turn whose edited document another turn revised since its workspace
// was built has its result set aside, and runs once more, first in the room's queue, on the new
// version. The re-run is no chain link. A re-run that conflicts again fails with "conflict on
// <doc>", and runs no more.
func TestConflictRerunThenFail(t *testing.T) {
	rs := newRoomServer(t)
	desk := newTurnDesk()
	d, ctx := cappedHost(t, desk, newGate(4), "instance")
	r := rs.hostRoom(t, d, ctx, "R")
	grill, sec := rs.addSecretary(t, r, true)
	t0 := rs.press(t, r, grill)
	desk.next(t).finish("first draft", "docs/plan.md=v1")
	rs.waitDone(t, r, t0.ID)

	tg, ts := rs.press(t, r, grill), rs.press(t, r, sec)
	g, s := byPersona(desk.next(t), desk.next(t))
	if g.files["docs/plan.md"] != "v1" || s.files["docs/plan.md"] != "v1" {
		t.Fatalf("workspaces %q, %q", g.files["docs/plan.md"], s.files["docs/plan.md"])
	}
	g.finish("grill edits", "docs/plan.md=v2 grill")
	rs.waitDone(t, r, tg.ID)
	s.finish("secretary edits", "docs/plan.md=v2 secretary")
	if tn := rs.waitDone(t, r, ts.ID); tn.Status != store.TurnFailed || tn.Error != "conflict on plan.md" {
		t.Fatalf("conflicting turn %+v", tn)
	}
	rerun := desk.next(t)
	if rerun.persona != "Secretary" || rerun.files["docs/plan.md"] != "v2 grill" {
		t.Fatalf("re-run of %s on %q", rerun.persona, rerun.files["docs/plan.md"])
	}
	v := rs.running(t, r)
	if len(v.Running) != 1 || v.Running[0].RerunOf != ts.ID || v.Running[0].Cause != "re-run after a conflict on plan.md" {
		t.Fatalf("re-run %+v", v.Running)
	}
	if v.Chain.Count != 0 {
		t.Fatalf("the re-run counted as a chain link: %+v", v.Chain)
	}
	rerunID := v.Running[0].ID
	rerun.finish("on top of grill", "docs/plan.md=v3 secretary on grill")
	if tn := rs.waitDone(t, r, rerunID); tn.Status != store.TurnDone {
		t.Fatalf("re-run %+v", tn)
	}
	docs, _ := rs.st.Documents(t.Context(), r.id)
	if revs, _ := rs.st.Revisions(t.Context(), docs[0].ID); len(revs) != 3 {
		t.Fatalf("revisions %+v", revs)
	}

	// A second conflict: the re-run loses to Grill again.
	tg, ts = rs.press(t, r, grill), rs.press(t, r, sec)
	g, s = byPersona(desk.next(t), desk.next(t))
	g.finish("grill again", "docs/plan.md=v4 grill")
	rs.waitDone(t, r, tg.ID)
	s.finish("secretary again", "docs/plan.md=v4 secretary")
	rs.waitDone(t, r, ts.ID)
	rerun = desk.next(t)
	rerunID = rs.rerunning(t, r)
	tg = rs.press(t, r, grill)
	desk.next(t).finish("grill once more", "docs/plan.md=v5 grill")
	rs.waitDone(t, r, tg.ID)
	rerun.finish("secretary once more", "docs/plan.md=v5 secretary")
	if tn := rs.waitDone(t, r, rerunID); tn.Status != store.TurnFailed || tn.Error != "conflict on plan.md" {
		t.Fatalf("second conflict %+v", tn)
	}
	desk.idle(t)
	if m := rs.lastIn(t, r); m.Body != "Secretary turn failed: conflict on plan.md." {
		t.Fatalf("last message %q", m.Body)
	}
	if q, _ := rs.st.Queue(t.Context(), r.id); len(q) != 0 {
		t.Fatalf("queue %+v", q)
	}
}

// 003-AC21: thread replies, new annotations, syntheses and other documents never conflict: a
// parallel turn that revised the annotated document first leaves them all in.
func TestRepliesAndAnnotationsNeverConflict(t *testing.T) {
	rs := newRoomServer(t)
	desk := newTurnDesk()
	d, ctx := cappedHost(t, desk, newGate(4), "instance")
	r := rs.hostRoom(t, d, ctx, "R")
	grill, sec := rs.addSecretary(t, r, true)
	t0 := rs.press(t, r, grill)
	desk.next(t).finish("draft", "docs/plan.md=The rollout needs a rollback step.")
	rs.waitDone(t, r, t0.ID)
	docs, _ := rs.st.Documents(t.Context(), r.id)
	var ann store.Annotation
	rs.call(t, "POST", fmt.Sprintf("/api/rooms/%s/documents/%d/annotations", r.id, docs[0].ID), r.host,
		map[string]any{"revision": 1, "anchor": map[string]string{"kind": "text", "exact": "rollback", "prefix": "needs a ", "suffix": " step"}, "body": "How long?"},
		http.StatusCreated, &ann)

	tg, ts := rs.press(t, r, grill), rs.press(t, r, sec)
	g, s := byPersona(desk.next(t), desk.next(t))
	g.finish("revised", "docs/plan.md=The rollout needs a rollback step, tested weekly.")
	rs.waitDone(t, r, tg.ID)
	s.finish("noted",
		fmt.Sprintf(`replies.json=[{"annotation": %d, "body": "Ten minutes."}]`, ann.ID),
		`annotations-new.json=[{"doc": "docs/plan.md", "exact": "rollout", "prefix": "The ", "suffix": " needs", "body": "Which one?"}]`,
		"docs/notes.md=# Notes")
	tn := rs.waitDone(t, r, ts.ID)
	if tn.Status != store.TurnDone || tn.Report.Replies != 1 || tn.Report.Annotated != 1 || len(tn.Report.Created) != 1 {
		t.Fatalf("turn %+v", tn)
	}
	if m := rs.lastIn(t, r); m.Body != "noted" {
		t.Fatalf("synthesis %q", m.Body)
	}
}

// A turn brings its sandbox up before it fetches the workspace, whose tar holds the worker's
// cache lock: a sandbox that does not start fails the turn without fetching it.
func TestSandboxStartsBeforeWorkspace(t *testing.T) {
	rs := newRoomServer(t)
	ag := &fakeAgent{startErr: errors.New("docker run failed")}
	alice, _ := pairedHost(t, rs, "Alice", ag)
	var started store.Turn
	rs.call(t, "POST", "/api/rooms/"+rs.room+"/turns", alice, map[string]bool{}, http.StatusCreated, &started)
	waitFor(t, "turn failed", func() bool { v := rs.agent(t, alice); return v.Turn.Status == "failed" })
	if v := rs.agent(t, alice); v.Turn.Error != "docker run failed" {
		t.Fatalf("turn %+v", v.Turn)
	}
	if tn, _ := rs.st.Turn(t.Context(), rs.room, started.ID); tn.BaseRevisions != nil {
		t.Fatalf("the workspace was fetched: %+v", tn.BaseRevisions)
	}
}
