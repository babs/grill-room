package store

import (
	"context"
	"maps"
	"slices"
	"testing"
)

// presenceTimes are the bounds of the presence tests, at now.
func presenceTimes(now int64) PresenceTimes {
	return PresenceTimes{Now: now, Grace: 20_000, Forget: 3_600_000, Lapse: 10_000}
}

// presenceOf returns room's rows live at now ("" for every room), by room and tab.
func presenceOf(t *testing.T, s *Store, room string, now int64) map[string]PresenceRow {
	t.Helper()
	rows, err := s.Presence(context.Background(), room, now)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]PresenceRow{}
	for _, r := range rows {
		out[r.Room+"/"+r.Tab] = r
	}
	return out
}

// 017-AC14: an open stream keeps its participant and tab for the grace, renewed by the same call;
// its release keeps both for the grace after; a participant back after the grace arrives again,
// while one back within it keeps their stay.
func TestPresenceOpenRelease(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	p := seed(t, s, "a")
	const t0 = int64(1_000_000)
	if err := s.PresenceOpen(ctx, "a", p.ID, "t1", presenceTimes(t0)); err != nil {
		t.Fatal(err)
	}
	if err := s.PresenceOpen(ctx, "a", p.ID, "t1", presenceTimes(t0+5_000)); err != nil {
		t.Fatal(err)
	}
	got := presenceOf(t, s, "a", t0+5_000)
	if person, tab := got["a/"], got["a/t1"]; len(got) != 2 || person.SinceMS != t0 || person.BeatMS != t0+5_000 ||
		person.ExpiresMS != t0+5_000+3_600_000 || tab.BeatMS != t0+5_000 || tab.ExpiresMS != t0+25_000 {
		t.Fatalf("after a renewal: %+v", got)
	}
	if err := s.PresenceRelease(ctx, "a", p.ID, "t1", presenceTimes(t0+6_000)); err != nil {
		t.Fatal(err)
	}
	if tab := presenceOf(t, s, "a", t0+25_999)["a/t1"]; tab.ExpiresMS != t0+26_000 {
		t.Fatalf("released tab: %+v, want it until the grace after the release", tab)
	}
	if got := presenceOf(t, s, "a", t0+26_000); len(got) != 1 || got["a/"].BeatMS != t0+6_000 {
		t.Fatalf("past the grace: %+v, want the person row alone", got)
	}
	// Back within the grace: the stay goes on.
	if err := s.PresenceOpen(ctx, "a", p.ID, "t2", presenceTimes(t0+20_000)); err != nil {
		t.Fatal(err)
	}
	if person := presenceOf(t, s, "a", t0+20_000)["a/"]; person.SinceMS != t0 {
		t.Fatalf("back within the grace: since %d, want %d", person.SinceMS, t0)
	}
	// Back once the grace ran out with no tab live: a new stay.
	const t1 = t0 + 100_000
	if err := s.PresenceOpen(ctx, "a", p.ID, "t3", presenceTimes(t1)); err != nil {
		t.Fatal(err)
	}
	got = presenceOf(t, s, "a", t1)
	if got["a/"].SinceMS != t1 || len(got) != 2 {
		t.Fatalf("back after the grace: %+v, want since %d and the new tab alone", got, t1)
	}
}

// 005-AC6, 010-AC13, 017-AC14: a beat records the tab's audio state; gone drops the tab at once and
// keeps the participant; a participant holds maxTabs tabs at most, a known tab beats on; a device
// beat holds the flag for the lapse, off drops it; Presence("") reads every room.
func TestPresenceBeatDevice(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	p := seed(t, s, "a")
	q := seed(t, s, "b")
	const t0 = int64(1_000_000)
	at := presenceTimes(t0)
	for _, tab := range []string{"t1", "t2"} {
		if ok, err := s.PresenceBeat(ctx, "a", p.ID, tab, TabState{Audio: true, Mic: true, Focused: true}, false, 2, at); err != nil || !ok {
			t.Fatalf("beat %s: %v %v", tab, ok, err)
		}
	}
	if ok, err := s.PresenceBeat(ctx, "a", p.ID, "t3", TabState{}, false, 2, at); err != nil || ok {
		t.Fatalf("a third tab past maxTabs 2: %v %v", ok, err)
	}
	if ok, err := s.PresenceDevice(ctx, "a", p.ID, "t3", true, 2, at); err != nil || ok {
		t.Fatalf("a third tab's device beat past maxTabs 2: %v %v", ok, err)
	}
	if ok, err := s.PresenceBeat(ctx, "a", p.ID, "t1", TabState{Audio: true, Deaf: true}, false, 2, presenceTimes(t0+1_000)); err != nil || !ok {
		t.Fatalf("a known tab at maxTabs: %v %v", ok, err)
	}
	if ok, err := s.PresenceDevice(ctx, "a", p.ID, "t2", true, 2, at); err != nil || !ok {
		t.Fatalf("device on: %v %v", ok, err)
	}
	got := presenceOf(t, s, "a", t0+1_000)
	if t1, t2 := got["a/t1"], got["a/t2"]; !t1.Audio || !t1.Deaf || t1.Mic || t1.Focused || t1.BeatMS != t0+1_000 ||
		!t2.Audio || !t2.Mic || !t2.Focused || t2.DeviceMS != t0 {
		t.Fatalf("tabs: %+v", got)
	}
	if ok, err := s.PresenceDevice(ctx, "a", p.ID, "t2", false, 2, at); err != nil || !ok {
		t.Fatalf("device off: %v %v", ok, err)
	}
	if d := presenceOf(t, s, "a", t0)["a/t2"].DeviceMS; d != 0 {
		t.Fatalf("device off left %d", d)
	}
	if ok, err := s.PresenceBeat(ctx, "a", p.ID, "t1", TabState{}, true, 2, at); err != nil || !ok {
		t.Fatalf("gone: %v %v", ok, err)
	}
	if _, err := s.PresenceBeat(ctx, "b", q.ID, "t1", TabState{}, false, 2, at); err != nil {
		t.Fatal(err)
	}
	all := presenceOf(t, s, "", t0)
	if keys := slices.Sorted(maps.Keys(all)); !slices.Equal(keys, []string{"a/", "a/t2", "b/", "b/t1"}) {
		t.Fatalf("every room: %v, want a's person and t2, b's person and t1", keys)
	}
}

// 017-AC14: a worker's beat reads back whole until it lapses; a new worker past
// maxWorkers is refused, a known one beats on.
func TestSTTWorkerRows(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	const now = int64(1_000_000)
	w := STTWorker{Name: "w1", Rooms: []string{"a"}, Voices: map[string][]int64{"a": {1, 2}}, Engine: "whisper", Glossary: true, ExpiresMS: now + 15_000}
	if ok, err := s.BeatSTTWorker(ctx, w, 1, now); err != nil || !ok {
		t.Fatalf("beat: %v %v", ok, err)
	}
	if ok, err := s.BeatSTTWorker(ctx, STTWorker{Name: "w2", ExpiresMS: now + 15_000}, 1, now); err != nil || ok {
		t.Fatalf("a second worker past maxWorkers 1: %v %v", ok, err)
	}
	w.ExpiresMS = now + 20_000
	if ok, err := s.BeatSTTWorker(ctx, w, 1, now); err != nil || !ok {
		t.Fatalf("a known worker at maxWorkers: %v %v", ok, err)
	}
	ws, err := s.STTWorkers(ctx, now)
	if err != nil || len(ws) != 1 || ws[0].Name != "w1" || !slices.Equal(ws[0].Rooms, w.Rooms) || !slices.Equal(ws[0].Voices["a"], w.Voices["a"]) ||
		ws[0].Engine != "whisper" || !ws[0].Glossary || ws[0].ExpiresMS != now+20_000 {
		t.Fatalf("workers: %+v %v", ws, err)
	}
	if ws, err := s.STTWorkers(ctx, now+20_000); err != nil || len(ws) != 0 {
		t.Fatalf("lapsed workers listed: %+v %v", ws, err)
	}
}
