package web

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// handQueue is the participant ids of ps's raised hands, by rank.
func handQueue(ps []presenceOf) []int64 {
	n := 0
	for _, p := range ps {
		n = max(n, p.Hand)
	}
	out := make([]int64, n)
	for _, p := range ps {
		if p.Hand > 0 {
			out[p.Hand-1] = p.ID
		}
	}
	return out
}

// 031-AC1: the caller raises and lowers their own hand, both idempotent; a room admin lowers
// anyone's, a member only their own; a non-participant gets what the room's other participant
// routes answer; a removed participant loses their hand.
func TestHandRoutes(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Hands", "Alice")
	path := "/api/rooms/" + room.ID
	bob := e.join(t, e.createInvite(t, room.ID).Token, "Bob")
	carol := e.join(t, e.createInvite(t, room.ID).Token, "Carol")
	b, c := bob.Participant.ID, carol.Participant.ID
	for _, tok := range []string{bob.Token, carol.Token} {
		if code, out := e.do(t, "POST", path+"/presence", tok, map[string]any{"tab": "t1"}); code != http.StatusNoContent {
			t.Fatalf("beat: %d %s", code, out)
		}
	}
	queue := func() []int64 { return handQueue(e.srv.people.snapshot(room.ID, e.srv.lease())) }
	for _, s := range []struct {
		name, method, path, token string
		code                      int
	}{
		{"Bob raises", "PUT", "/hand", bob.Token, http.StatusNoContent},
		{"Carol raises", "PUT", "/hand", carol.Token, http.StatusNoContent},
		{"Bob raises again", "PUT", "/hand", bob.Token, http.StatusNoContent},
		{"Bob lowers Carol's", "DELETE", fmt.Sprintf("/hands/%d", c), bob.Token, http.StatusForbidden},
		{"Bob lowers his own by id", "DELETE", fmt.Sprintf("/hands/%d", b), bob.Token, http.StatusNoContent},
		{"Bob lowers again", "DELETE", "/hand", bob.Token, http.StatusNoContent},
		{"a bad id", "DELETE", "/hands/x", bob.Token, http.StatusNotFound},
	} {
		if code, out := e.do(t, s.method, path+s.path, s.token, nil); code != s.code {
			t.Fatalf("%s: %d %s, want %d", s.name, code, out, s.code)
		}
	}
	if got := queue(); !slices.Equal(got, []int64{c}) {
		t.Fatalf("queue %v, want Carol's alone", got)
	}
	e.do(t, "PUT", path+"/hand", bob.Token, nil)
	if got := queue(); !slices.Equal(got, []int64{c, b}) {
		t.Fatalf("queue %v, want Carol then Bob", got)
	}
	if code, out := alice.do(t, "DELETE", fmt.Sprintf("%s/hands/%d", path, c), nil); code != http.StatusNoContent {
		t.Fatalf("the room admin lowers Carol's: %d %s", code, out)
	}
	if got := queue(); !slices.Equal(got, []int64{b}) {
		t.Fatalf("queue %v, want Bob's alone", got)
	}
	// Bob is removed with his hand up: it goes.
	if code, out := alice.do(t, "DELETE", fmt.Sprintf("%s/participants/%d", path, b), nil); code != http.StatusNoContent {
		t.Fatalf("remove Bob: %d %s", code, out)
	}
	if got := queue(); len(got) != 0 {
		t.Fatalf("queue %v after Bob's removal", got)
	}
	// Non-participants: no token, another room's, a removed seat's, as GET messages answers them.
	other := e.createRoom(t, "Other").ID
	dave := e.join(t, e.createInvite(t, other).Token, "Dave")
	for _, tok := range []string{"", dave.Token, bob.Token} {
		want, _ := e.do(t, "GET", path+"/messages", tok, nil)
		if want < 400 {
			t.Fatalf("GET messages with %q: %d", tok, want)
		}
		for _, r := range [][2]string{{"PUT", "/hand"}, {"DELETE", "/hand"}, {"DELETE", fmt.Sprintf("/hands/%d", c)}} {
			if code, _ := e.do(t, r[0], path+r[1], tok, nil); code != want {
				t.Fatalf("%s %s with %q: %d, want %d", r[0], r[1], tok, code, want)
			}
		}
	}
}

// 031-AC4 031-AC6: the presence event ranks the hands by raise time; a hand stays through a reload
// within the grace and lapses once its participant has had no live tab past it, for good.
func TestHandsRankAndLapse(t *testing.T) {
	e, r, ids := peopleRoom(t, 2)
	pp := e.srv.people
	a, b := ids[0], ids[1]
	t0 := time.Now()
	relA := pp.open(r, a, "tabA", t0)
	relB := pp.open(r, b, "tabB", t0)
	t.Cleanup(func() { relB(t0) })
	for _, h := range []struct {
		pid int64
		at  time.Duration
	}{{b, time.Second}, {a, 2 * time.Second}} {
		if err := pp.hand(r, h.pid, true, t0.Add(h.at)); err != nil {
			t.Fatal(err)
		}
	}
	// Bob beats at each look, so he stays while Alice comes and goes.
	ranks := func(at time.Time) [2]int {
		if _, err := pp.beat(r, b, "tabB", tab{}, false, at); err != nil {
			t.Fatal(err)
		}
		got := hereOf(t, pp, r, at)
		return [2]int{got[a].Hand, got[b].Hand}
	}
	if got := ranks(t0.Add(3 * time.Second)); got != [2]int{2, 1} {
		t.Fatalf("ranks %v, want Alice 2nd, Bob 1st", got)
	}
	// A reload: back within the grace.
	reload := t0.Add(4 * time.Second)
	relA(reload)
	if got := ranks(reload.Add(presenceGrace - time.Second)); got != [2]int{2, 1} {
		t.Fatalf("within the grace: %v", got)
	}
	relA = pp.open(r, a, "tabA", reload.Add(presenceGrace-time.Second))
	left := reload.Add(presenceGrace)
	relA(left)
	ranks(left.Add(presenceGrace / 2))
	if got := ranks(left.Add(presenceGrace + time.Second)); got != [2]int{0, 1} {
		t.Fatalf("past the grace: %v, want Alice's lowered", got)
	}
	back := left.Add(presenceGrace + 2*time.Second)
	relA = pp.open(r, a, "tabA", back)
	t.Cleanup(func() { relA(back) })
	if got := ranks(back); got != [2]int{0, 1} {
		t.Fatalf("Alice back: %v, want her hand still lowered", got)
	}
}

// 031-AC7: a raise and a lower write no message and no audit entry, and the personas' preamble
// says nothing of hands. The export's columns are checked in store's TestExportCoversEveryColumn.
func TestHandsLeaveNoTrace(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Quiet", "Alice")
	path := "/api/rooms/" + room.ID
	bob := e.join(t, e.createInvite(t, room.ID).Token, "Bob")
	e.do(t, "POST", path+"/presence", bob.Token, map[string]any{"tab": "t1"})
	last, audits := e.lastLine(t, room.ID).ID, len(e.auditLog(t, "").Entries)
	e.do(t, "PUT", path+"/hand", bob.Token, nil)
	if got := handQueue(e.srv.people.snapshot(room.ID, e.srv.lease())); len(got) != 1 {
		t.Fatalf("queue %v", got)
	}
	r, err := e.st.Room(t.Context(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	ps, err := e.st.RoomPersonas(t.Context(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	pre, err := e.srv.presencePreamble(t.Context(), r, &ps[0], "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(pre), "hand") || strings.Contains(pre, "✋") {
		t.Fatalf("the preamble mentions hands:\n%s", pre)
	}
	if code, _ := alice.do(t, "DELETE", fmt.Sprintf("%s/hands/%d", path, bob.Participant.ID), nil); code != http.StatusNoContent {
		t.Fatalf("lower: %d", code)
	}
	if got := e.lastLine(t, room.ID).ID; got != last {
		t.Fatalf("a hand wrote message %d", got)
	}
	if got := len(e.auditLog(t, "").Entries); got != audits {
		t.Fatalf("%d audit entries, want %d", got, audits)
	}
}

// 031-AC2: a hand raised through pod A shows in a stream of pod B within 2 s.
func TestTwoPodsHands(t *testing.T) {
	a, b := newPair(t, testOptions)
	room := a.createRoom(t, "Two pods").ID
	alice := a.join(t, a.createInvite(t, room).Token, "Alice")
	bob := a.join(t, a.createInvite(t, room).Token, "Bob")
	al := alice.Participant.ID
	a.openStream(t, "/api/rooms/"+room+"/events?tab=alice-1", alice.Token, "")
	onB, _ := b.openStream(t, "/api/rooms/"+room+"/events?tab=bob-1", bob.Token, "")
	within(t, "Alice here on B", onB, "presence", 10*time.Second, presenceHas(t, al, func(p presenceOf) bool { return p.Here }))
	start := time.Now()
	if code, out := a.do(t, "PUT", "/api/rooms/"+room+"/hand", alice.Token, nil); code != http.StatusNoContent {
		t.Fatalf("raise on A: %d %s", code, out)
	}
	within(t, "Alice's hand on B", onB, "presence", 2*time.Second-time.Since(start), presenceHas(t, al, func(p presenceOf) bool { return p.Hand == 1 }))
}
