package web

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func hereOf(t *testing.T, pp *people, room string, now time.Time) map[int64]presenceOf {
	t.Helper()
	out := map[int64]presenceOf{}
	for _, p := range pp.snapshot(room, now) {
		out[p.ID] = p
	}
	return out
}

// peopleRoom is a server's presence, a room of its and n participants of the room.
func peopleRoom(t *testing.T, n int) (e *env, room string, ids []int64) {
	t.Helper()
	e = newEnv(t)
	room = e.createRoom(t, "R").ID
	inv := e.createInvite(t, room).Token
	for i := range n {
		ids = append(ids, e.join(t, inv, "P"+itoa(int64(i))).Participant.ID)
	}
	return e, room, ids
}

// 005-AC6: a stream that drops and comes back within the grace, as a reload or a flaky network
// does, never shows its participant gone.
func TestPresenceFlappingConnection(t *testing.T) {
	e, r, ids := peopleRoom(t, 1)
	pp, t0, a := e.srv.people, time.Unix(1000, 0), ids[0]
	release := pp.open(r, a, "tabA", t0)
	for i := range 10 {
		at := t0.Add(time.Duration(i) * 3 * time.Second)
		release(at)
		if p := hereOf(t, pp, r, at.Add(time.Second))[a]; !p.Here {
			t.Fatalf("flap %d: gone during the grace", i)
		}
		release = pp.open(r, a, "tabA", at.Add(2*time.Second))
	}
	end := t0.Add(40 * time.Second)
	release(end)
	if !hereOf(t, pp, r, end.Add(presenceGrace))[a].Here {
		t.Fatal("gone before the grace ran out")
	}
	p := hereOf(t, pp, r, end.Add(presenceGrace+time.Second))[a]
	if p.Here || p.SeenAt == nil || !p.SeenAt.Equal(end) {
		t.Fatalf("after the grace: %+v", p)
	}
}

// 005-AC6: a backgrounded phone suspends its stream and its heartbeats. Within the grace it stays;
// past it, it is gone, seen when it went quiet; its return brings it back with its audio state.
func TestPresenceBackgroundedPhone(t *testing.T) {
	e, r, ids := peopleRoom(t, 1)
	pp, t0, a := e.srv.people, time.Unix(1000, 0), ids[0]
	release := pp.open(r, a, "phone", t0)
	pp.beat(r, a, "phone", tab{audio: true, mic: true}, false, t0)
	if p := hereOf(t, pp, r, t0)[a]; !p.Here || !p.Audio || !p.Mic {
		t.Fatalf("in audio: %+v", p)
	}
	quiet := t0.Add(5 * time.Second)
	release(quiet) // the OS cut the stream; no heartbeat follows
	if p := hereOf(t, pp, r, quiet.Add(presenceGrace-time.Second))[a]; !p.Here || !p.Audio {
		t.Fatalf("within the grace: %+v", p)
	}
	if p := hereOf(t, pp, r, quiet.Add(presenceGrace+time.Second))[a]; p.Here || p.Audio || p.SeenAt == nil {
		t.Fatalf("past the grace: %+v", p)
	}
	back := quiet.Add(time.Minute)
	release = pp.open(r, a, "phone", back)
	t.Cleanup(func() { release(back) })
	pp.beat(r, a, "phone", tab{audio: true}, false, back)
	if p := hereOf(t, pp, r, back)[a]; !p.Here || !p.Audio || p.Mic || p.SeenAt != nil {
		t.Fatalf("back, muted: %+v", p)
	}
}

// 005-AC6, 017-AC14: a stream the server holds open keeps its participant here without
// heartbeats, as a desktop browser's throttled background tab does: it renews its tab every half
// grace, on the clock, until it closes.
func TestPresenceStreamRenews(t *testing.T) {
	old := presenceGrace
	presenceGrace = 200 * time.Millisecond
	t.Cleanup(func() { presenceGrace = old })
	e, r, ids := peopleRoom(t, 1)
	pp, a := e.srv.people, ids[0]
	opened := time.Now()
	release := pp.open(r, a, "", opened)
	// One grace of wall time with the stream as the only sign of life.
	beat := func() time.Time {
		p := pp.read(r, time.Now())[r][a]
		if p == nil || len(p.tabs) == 0 {
			return time.Time{}
		}
		return p.tabs[0].beat
	}
	waitFor(t, func() bool { return beat().After(opened.Add(presenceGrace)) })
	if now := time.Now(); !pp.here(r, a, now) {
		t.Fatal("open stream, no heartbeat: gone")
	}
	closed := time.Now()
	release(closed)
	if b := beat(); !b.Equal(closed.Truncate(time.Millisecond)) {
		t.Fatalf("tab beat %v after the release at %v", b, closed)
	}
	if pp.here(r, a, closed.Add(presenceGrace+time.Millisecond)) {
		t.Fatal("released stream: still here past the grace")
	}
}

// 005-AC6: with two tabs, closing one keeps the participant here, and drops at once the audio that
// tab held.
func TestPresenceTwoTabsOneClosed(t *testing.T) {
	e, r, ids := peopleRoom(t, 1)
	pp, t0, a := e.srv.people, time.Unix(1000, 0), ids[0]
	relA := pp.open(r, a, "tabA", t0)
	relB := pp.open(r, a, "tabB", t0)
	t.Cleanup(func() { relB(t0) })
	pp.beat(r, a, "tabA", tab{audio: true, mic: true, deaf: true}, false, t0)
	pp.beat(r, a, "tabB", tab{}, false, t0)
	if p := hereOf(t, pp, r, t0)[a]; !p.Audio || !p.Deaf {
		t.Fatalf("tab A in audio: %+v", p)
	}
	closed := t0.Add(time.Second)
	relA(closed)
	pp.beat(r, a, "tabA", tab{}, true, closed)
	for _, at := range []time.Duration{0, presenceGrace + time.Second, time.Hour} {
		pp.beat(r, a, "tabB", tab{}, false, closed.Add(at)) // tab B lives on
		if p := hereOf(t, pp, r, closed.Add(at))[a]; !p.Here || p.Audio || p.Mic || p.Deaf {
			t.Fatalf("%s after tab A closed: %+v", at, p)
		}
	}
	// Another room's snapshot never shows them, and a deleted room takes their rows.
	if got := pp.snapshot("other", closed); len(got) != 0 {
		t.Fatalf("other room: %+v", got)
	}
	if code, _ := e.do(t, "DELETE", "/api/rooms/"+r, e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if got := pp.snapshot(r, closed.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("deleted room kept: %+v", got)
	}
}

func TestPresenceForgetsAndBoundsTabs(t *testing.T) {
	e, r, ids := peopleRoom(t, 1)
	pp, t0, a := e.srv.people, time.Unix(1000, 0), ids[0]
	beat := func(id string, at time.Time) bool {
		t.Helper()
		ok, err := pp.beat(r, a, id, tab{}, false, at)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	for i := range maxTabs {
		if !beat("t"+string(rune('a'+i)), t0) {
			t.Fatalf("tab %d refused", i)
		}
	}
	if beat("one-too-many", t0) {
		t.Fatal("tab past the bound accepted")
	}
	// A known tab still beats; the lapsed ones free their place.
	if !beat("ta", t0) {
		t.Fatal("known tab refused")
	}
	if !beat("one-more", t0.Add(presenceGrace+time.Second)) {
		t.Fatal("tab refused once the others lapsed")
	}
	if got := hereOf(t, pp, r, t0.Add(peopleForget+presenceGrace+time.Second)); len(got) != 0 {
		t.Fatalf("not forgotten: %+v", got)
	}
}

// 005-AC6: the stream replays the room's presence at open, then sends it again as it changes; the
// heartbeat carries the audio state and refuses a bad tab id.
func TestPresenceEvents(t *testing.T) {
	old := pollEvery
	pollEvery = 20 * time.Millisecond
	t.Cleanup(func() { pollEvery = old })
	e := newEnv(t)
	room := e.createRoom(t, "R")
	alice := e.join(t, e.createInvite(t, room.ID).Token, "Alice")
	bob := e.join(t, e.createInvite(t, room.ID).Token, "Bob")
	ch, _ := e.openStream(t, "/api/rooms/"+room.ID+"/events?tab=alice-1", alice.Token, "")
	next := func(ok func(map[int64]presenceOf) bool) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case ev := <-ch:
				if ev.event != "presence" {
					continue
				}
				var got struct{ People []presenceOf }
				if err := json.Unmarshal([]byte(ev.data), &got); err != nil {
					t.Fatal(err)
				}
				m := map[int64]presenceOf{}
				for _, p := range got.People {
					m[p.ID] = p
				}
				if ok(m) {
					return
				}
			case <-deadline:
				t.Fatal("no matching presence event")
			}
		}
	}
	a, b := alice.Participant.ID, bob.Participant.ID
	next(func(m map[int64]presenceOf) bool { return m[a].Here && !m[b].Here })
	path := "/api/rooms/" + room.ID + "/presence"
	if code, _ := e.do(t, "POST", path, bob.Token, map[string]any{"tab": "bob-1", "audio": true, "mic": true}); code != http.StatusNoContent {
		t.Fatalf("heartbeat: %d", code)
	}
	next(func(m map[int64]presenceOf) bool {
		return m[b].Here && m[b].Audio && m[b].Mic && !m[b].Muted && !m[b].Deaf
	})
	e.do(t, "POST", path, bob.Token, map[string]any{"tab": "bob-1", "audio": true, "muted": true, "deaf": true})
	next(func(m map[int64]presenceOf) bool { return m[b].Audio && !m[b].Mic && m[b].Muted && m[b].Deaf })
	// A mic or deafen outside the audio means nothing.
	e.do(t, "POST", path, bob.Token, map[string]any{"tab": "bob-1", "mic": true, "muted": true, "deaf": true})
	next(func(m map[int64]presenceOf) bool {
		return m[b].Here && !m[b].Audio && !m[b].Mic && !m[b].Muted && !m[b].Deaf
	})
	for _, bad := range []string{"", "a b", "../x", string(make([]byte, 65))} {
		if code, _ := e.do(t, "POST", path, bob.Token, map[string]any{"tab": bad}); code != http.StatusBadRequest {
			t.Fatalf("tab %q: %d", bad, code)
		}
	}
	if code, _ := e.do(t, "POST", path, "", map[string]any{"tab": "x"}); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
}

// 009-AC36: here since is when the current stay began: a reload within the grace keeps it, a
// return after it starts a new one, a heartbeat alone starts one too, and none shows while away.
func TestPresenceHereSince(t *testing.T) {
	e, r, ids := peopleRoom(t, 3)
	pp, t0 := e.srv.people, time.Unix(1000, 0)
	a, b, c := ids[0], ids[1], ids[2]
	release := pp.open(r, a, "tabA", t0)
	if p := hereOf(t, pp, r, t0)[a]; p.HereSince == nil || !p.HereSince.Equal(t0) {
		t.Fatalf("arrived: %+v", p)
	}
	reload := t0.Add(5 * time.Second)
	release(reload)
	release = pp.open(r, a, "tabA", reload.Add(time.Second))
	if p := hereOf(t, pp, r, reload.Add(time.Second))[a]; !p.HereSince.Equal(t0) {
		t.Fatalf("after a reload: %+v", p.HereSince)
	}
	left := reload.Add(time.Minute)
	release(left)
	if p := hereOf(t, pp, r, left.Add(presenceGrace+time.Second))[a]; p.Here || p.HereSince != nil {
		t.Fatalf("away: %+v", p)
	}
	back := left.Add(time.Hour / 2)
	release = pp.open(r, a, "tabA", back)
	t.Cleanup(func() { release(back) })
	if p := hereOf(t, pp, r, back)[a]; !p.HereSince.Equal(back) {
		t.Fatalf("back: %+v", p.HereSince)
	}
	pp.beat(r, b, "tabB", tab{}, false, t0)
	pp.beat(r, b, "tabB", tab{}, false, t0.Add(10*time.Second))
	if p := hereOf(t, pp, r, t0.Add(10*time.Second))[b]; !p.HereSince.Equal(t0) {
		t.Fatalf("heartbeats: %+v", p.HereSince)
	}
	// A closing page's last beat is no arrival.
	pp.beat(r, c, "tabC", tab{}, true, t0)
	if p := hereOf(t, pp, r, t0.Add(presenceGrace+time.Second))[c]; p.HereSince != nil {
		t.Fatalf("gone beat: %+v", p)
	}
}

// A presence or STT worker read that fails logs, as the caller goes on without it.
func TestPresenceReadFailureLogs(t *testing.T) {
	e := newEnv(t)
	core, logs := observer.New(zap.WarnLevel)
	pp, stt := newPeople(e.st, zap.New(core)), newSTTWorkers(e.st, zap.New(core))
	_ = e.st.Close()
	now := time.Now()
	pp.snapshot("r", now)
	pp.read("r", now)
	stt.live(now)
	if n, m := logs.FilterMessage("presence_read_failed").Len(), logs.FilterMessage("stt_workers_read_failed").Len(); n != 2 || m != 1 {
		t.Fatalf("presence_read_failed %d, stt_workers_read_failed %d; want 2, 1", n, m)
	}
}

// A failed presence read keeps the pod's last copy: a database hiccup does not show everyone gone.
func TestPresenceReadKeepsCopy(t *testing.T) {
	oldPoll := pollEvery
	pollEvery = 0
	t.Cleanup(func() { pollEvery = oldPoll })
	e, r, ids := peopleRoom(t, 1)
	pp, now := e.srv.people, time.Now()
	if ok, err := pp.beat(r, ids[0], "tab1", tab{}, false, now); err != nil || !ok {
		t.Fatalf("beat: %v %v", ok, err)
	}
	if !pp.here(r, ids[0], now) || !pp.present(now)[r] {
		t.Fatal("not here after a beat")
	}
	_ = e.st.Close()
	if !pp.here(r, ids[0], now) || !pp.present(now)[r] {
		t.Fatal("a failed read showed the participant gone")
	}
}
