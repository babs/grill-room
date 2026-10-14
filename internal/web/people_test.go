package web

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func newPeople() *people { return &people{m: map[string]map[int64]*person{}} }

func hereOf(t *testing.T, pp *people, now time.Time) map[int64]presenceOf {
	t.Helper()
	out := map[int64]presenceOf{}
	for _, p := range pp.snapshot("r", now) {
		out[p.ID] = p
	}
	return out
}

// 005-AC6: a stream that drops and comes back within the grace, as a reload or a flaky network
// does, never shows its participant gone.
func TestPresenceFlappingConnection(t *testing.T) {
	pp, t0 := newPeople(), time.Unix(1000, 0)
	release := pp.open("r", 1, "tabA", t0)
	for i := range 10 {
		at := t0.Add(time.Duration(i) * 3 * time.Second)
		release(at)
		if p := hereOf(t, pp, at.Add(time.Second))[1]; !p.Here {
			t.Fatalf("flap %d: gone during the grace", i)
		}
		release = pp.open("r", 1, "tabA", at.Add(2*time.Second))
	}
	end := t0.Add(40 * time.Second)
	release(end)
	if !hereOf(t, pp, end.Add(presenceGrace))[1].Here {
		t.Fatal("gone before the grace ran out")
	}
	p := hereOf(t, pp, end.Add(presenceGrace+time.Second))[1]
	if p.Here || p.SeenAt == nil || !p.SeenAt.Equal(end) {
		t.Fatalf("after the grace: %+v", p)
	}
}

// 005-AC6: a backgrounded phone suspends its stream and its heartbeats. Within the grace it stays;
// past it, it is gone, seen when it went quiet; its return brings it back with its audio state.
func TestPresenceBackgroundedPhone(t *testing.T) {
	pp, t0 := newPeople(), time.Unix(1000, 0)
	release := pp.open("r", 2, "phone", t0)
	pp.beat("r", 2, "phone", tab{audio: true, mic: true}, false, t0)
	if p := hereOf(t, pp, t0)[2]; !p.Here || !p.Audio || !p.Mic {
		t.Fatalf("in audio: %+v", p)
	}
	quiet := t0.Add(5 * time.Second)
	release(quiet) // the OS cut the stream; no heartbeat follows
	if p := hereOf(t, pp, quiet.Add(presenceGrace-time.Second))[2]; !p.Here || !p.Audio {
		t.Fatalf("within the grace: %+v", p)
	}
	if p := hereOf(t, pp, quiet.Add(presenceGrace+time.Second))[2]; p.Here || p.Audio || p.SeenAt == nil {
		t.Fatalf("past the grace: %+v", p)
	}
	back := quiet.Add(time.Minute)
	pp.open("r", 2, "phone", back)
	pp.beat("r", 2, "phone", tab{audio: true}, false, back)
	if p := hereOf(t, pp, back)[2]; !p.Here || !p.Audio || p.Mic || p.SeenAt != nil {
		t.Fatalf("back, muted: %+v", p)
	}
	// A stream the server still holds open keeps it here without heartbeats, as a desktop
	// browser's throttled background tab does.
	if !hereOf(t, pp, back.Add(10*time.Minute))[2].Here {
		t.Fatal("open stream, no heartbeat: gone")
	}
}

// 005-AC6: with two tabs, closing one keeps the participant here, and drops at once the audio that
// tab held.
func TestPresenceTwoTabsOneClosed(t *testing.T) {
	pp, t0 := newPeople(), time.Unix(1000, 0)
	relA := pp.open("r", 3, "tabA", t0)
	pp.open("r", 3, "tabB", t0)
	pp.beat("r", 3, "tabA", tab{audio: true, mic: true, deaf: true}, false, t0)
	pp.beat("r", 3, "tabB", tab{}, false, t0)
	if p := hereOf(t, pp, t0)[3]; !p.Audio || !p.Deaf {
		t.Fatalf("tab A in audio: %+v", p)
	}
	closed := t0.Add(time.Second)
	relA(closed)
	pp.beat("r", 3, "tabA", tab{}, true, closed)
	for _, at := range []time.Duration{0, presenceGrace + time.Second, time.Hour} {
		if p := hereOf(t, pp, closed.Add(at))[3]; !p.Here || p.Audio || p.Mic || p.Deaf {
			t.Fatalf("%s after tab A closed: %+v", at, p)
		}
	}
	// Another room's snapshot never shows them, and a deleted room forgets them.
	if got := pp.snapshot("other", closed); len(got) != 0 {
		t.Fatalf("other room: %+v", got)
	}
	pp.forget("r")
	if len(pp.m) != 0 {
		t.Fatal("deleted room kept")
	}
}

func TestPresenceForgetsAndBoundsTabs(t *testing.T) {
	pp, t0 := newPeople(), time.Unix(1000, 0)
	for i := range maxTabs {
		if !pp.beat("r", 4, "t"+string(rune('a'+i)), tab{}, false, t0) {
			t.Fatalf("tab %d refused", i)
		}
	}
	if pp.beat("r", 4, "one-too-many", tab{}, false, t0) {
		t.Fatal("tab past the bound accepted")
	}
	// A known tab still beats.
	if !pp.beat("r", 4, "ta", tab{}, false, t0) {
		t.Fatal("known tab refused")
	}
	if got := hereOf(t, pp, t0.Add(peopleForget+presenceGrace+time.Second)); len(got) != 0 || len(pp.m) != 0 {
		t.Fatalf("not forgotten: %+v %d", got, len(pp.m))
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
	pp, t0 := newPeople(), time.Unix(1000, 0)
	release := pp.open("r", 1, "tabA", t0)
	if p := hereOf(t, pp, t0)[1]; p.HereSince == nil || !p.HereSince.Equal(t0) {
		t.Fatalf("arrived: %+v", p)
	}
	reload := t0.Add(5 * time.Second)
	release(reload)
	release = pp.open("r", 1, "tabA", reload.Add(time.Second))
	if p := hereOf(t, pp, reload.Add(time.Second))[1]; !p.HereSince.Equal(t0) {
		t.Fatalf("after a reload: %+v", p.HereSince)
	}
	left := reload.Add(time.Minute)
	release(left)
	if p := hereOf(t, pp, left.Add(presenceGrace+time.Second))[1]; p.Here || p.HereSince != nil {
		t.Fatalf("away: %+v", p)
	}
	back := left.Add(time.Hour / 2)
	pp.open("r", 1, "tabA", back)
	if p := hereOf(t, pp, back)[1]; !p.HereSince.Equal(back) {
		t.Fatalf("back: %+v", p.HereSince)
	}
	pp.beat("r", 2, "tabB", tab{}, false, t0)
	pp.beat("r", 2, "tabB", tab{}, false, t0.Add(10*time.Second))
	if p := hereOf(t, pp, t0.Add(10*time.Second))[2]; !p.HereSince.Equal(t0) {
		t.Fatalf("heartbeats: %+v", p.HereSince)
	}
	// A closing page's last beat is no arrival.
	pp.beat("r", 3, "tabC", tab{}, true, t0)
	if p := hereOf(t, pp, t0.Add(presenceGrace+time.Second))[3]; p.HereSince != nil {
		t.Fatalf("gone beat: %+v", p)
	}
}
