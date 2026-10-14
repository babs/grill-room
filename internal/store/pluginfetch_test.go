package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 009-AC19, 009-AC20: a request waits for the AI host and says so in the chat; once fetched, its
// plugins land private on the personas still in the room, added as the participant who asked, and
// the request is gone. A second completion finds nothing waiting.
func TestPluginFetchAdd(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	other := newTurnRoom(t, s, "b")
	if _, _, err := s.AddPluginFetch(ctx, "a", NewPluginFetch{URL: "git@example.com:o/r.git", Personas: []int64{other.grill.ID}}, r.p); !errors.Is(err, ErrNotFound) {
		t.Fatalf("persona of another room: %v", err)
	}
	f, m, err := s.AddPluginFetch(ctx, "a", NewPluginFetch{URL: "git@example.com:o/r.git", Ref: "v1", Entries: []string{"one"}, Personas: []int64{r.grill.ID}}, r.p)
	if err != nil {
		t.Fatal(err)
	}
	if f.State != FetchWaiting || f.RequestedBy != "Alice" || f.Ref != "v1" || len(f.Entries) != 1 || !strings.Contains(m.Body, "asked the AI host to fetch") {
		t.Fatalf("request %+v, line %q", f, m.Body)
	}
	if fs, _ := s.PluginFetches(ctx, "b", false); len(fs) != 0 {
		t.Fatalf("another room lists %d requests", len(fs))
	}
	if got, err := s.PluginFetch(ctx, "a", f.ID); err != nil || got.URL != f.URL || got.Personas[0] != r.grill.ID {
		t.Fatalf("get: %v %+v", err, got)
	}
	if _, err := s.PluginFetch(ctx, "b", f.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another room's request: %v", err)
	}
	p := newPlugin("one", "c1")
	p.URL, p.Entry = f.URL, "one"
	msgs, upToDate, err := s.CompletePluginFetch(ctx, "a", f.ID, []NewPlugin{p}, 1<<20)
	if err != nil || upToDate || len(msgs) != 1 || !strings.HasPrefix(msgs[0].Body, "Alice added the plugin one") {
		t.Fatalf("complete: %v %v %+v", err, upToDate, msgs)
	}
	ps, _ := s.RoomPlugins(ctx, "a")
	if len(ps) != 1 || !ps[0].Private || len(ps[0].Personas) != 1 || ps[0].Personas[0] != r.grill.ID || ps[0].Versions[0].State != PluginInspecting {
		t.Fatalf("plugins %+v", ps)
	}
	if fs, _ := s.PluginFetches(ctx, "a", false); len(fs) != 0 {
		t.Fatalf("the done request stays: %+v", fs)
	}
	if _, _, err := s.CompletePluginFetch(ctx, "a", f.ID, []NewPlugin{p}, 1<<20); !errors.Is(err, ErrNotFound) {
		t.Fatalf("twice: %v", err)
	}
}

// 009-AC21: an update of a private plugin waits for the AI host, one at a time; a failure keeps the
// running version, says why in the chat and stays listed until dismissed; the same commit says up
// to date; a new one makes a version waiting for inspection.
func TestPluginFetchUpdate(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	p := addPlugin(t, s, "a", r.p, "priv", "c1")
	ask := func() *PluginFetch {
		t.Helper()
		f, m, err := s.AddPluginFetch(ctx, "a", NewPluginFetch{PluginID: p.ID, URL: p.URL}, r.p)
		if err != nil || !strings.Contains(m.Body, "an update of the plugin priv") || f.PluginName != "priv" {
			t.Fatalf("ask: %v %+v", err, f)
		}
		return f
	}
	f := ask()
	if _, _, err := s.AddPluginFetch(ctx, "a", NewPluginFetch{PluginID: p.ID, URL: p.URL}, r.p); !errors.Is(err, ErrFetchWaiting) {
		t.Fatalf("second update: %v", err)
	}
	if _, err := s.FailPluginFetch(ctx, "a", f.ID, "This AI host cannot reach the repo"); err != nil {
		t.Fatal(err)
	}
	if b := lastBody(t, s, "a"); b != "The AI host could not fetch the update of priv: This AI host cannot reach the repo." {
		t.Fatalf("line %q", b)
	}
	if _, err := s.FailPluginFetch(ctx, "a", f.ID, "again"); !errors.Is(err, ErrPluginState) {
		t.Fatalf("fail a failed one: %v", err)
	}
	if fs, _ := s.PluginFetches(ctx, "a", true); len(fs) != 0 {
		t.Fatalf("waiting only lists the failed one: %+v", fs)
	}
	if fs, _ := s.PluginFetches(ctx, "a", false); len(fs) != 1 || fs[0].Reason != "This AI host cannot reach the repo" {
		t.Fatalf("failed one: %+v", fs)
	}
	if err := s.DeletePluginFetch(ctx, "a", f.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePluginFetch(ctx, "a", f.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dismiss twice: %v", err)
	}
	f = ask()
	same := newPlugin("priv", "c1")
	if _, upToDate, err := s.CompletePluginFetch(ctx, "a", f.ID, []NewPlugin{same}, 1<<20); err != nil || !upToDate {
		t.Fatalf("same commit: %v %v", err, upToDate)
	}
	if b := lastBody(t, s, "a"); b != "The AI host fetched the plugin priv: up to date." {
		t.Fatalf("line %q", b)
	}
	f = ask()
	if _, upToDate, err := s.CompletePluginFetch(ctx, "a", f.ID, []NewPlugin{newPlugin("priv", "c2")}, 1<<20); err != nil || upToDate {
		t.Fatalf("new commit: %v %v", err, upToDate)
	}
	if got := versionStates(plugin(t, s, "a", p.ID)); got != "v1:inspecting v2:inspecting" {
		t.Fatalf("versions %s", got)
	}
	// Removing the plugin drops its update waiting for the AI host.
	ask()
	if _, err := s.RemovePlugin(ctx, "a", p.ID, r.p); err != nil {
		t.Fatal(err)
	}
	if fs, _ := s.PluginFetches(ctx, "a", false); len(fs) != 0 {
		t.Fatalf("requests left after the removal: %+v", fs)
	}
}

// 009-AC19: a room keeps at most MaxRoomFetches requests.
func TestPluginFetchLimit(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	for range MaxRoomFetches {
		if _, _, err := s.AddPluginFetch(ctx, "a", NewPluginFetch{URL: "git@example.com:o/r.git"}, r.p); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.AddPluginFetch(ctx, "a", NewPluginFetch{URL: "git@example.com:o/r.git"}, r.p); !errors.Is(err, ErrLimit) {
		t.Fatalf("one past the limit: %v", err)
	}
}

// 009-AC24: a turn's result marks the plugins it pinned and skipped as needing host setup and
// clears the others it pinned; a plugin it did not pin keeps its mark. The plugins version moves.
func TestSetPluginsSetup(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	a := addPlugin(t, s, "a", r.p, "pa", "c1")
	b := addPlugin(t, s, "a", r.p, "pb", "c1")
	before, _ := s.PluginsVersion(ctx, "a")
	if changed, err := s.SetPluginsSetup(ctx, "a", []string{"pa", "pb"}, []string{"pa", "pb"}); err != nil || !changed {
		t.Fatalf("mark: %v %v", err, changed)
	}
	if v, _ := s.PluginsVersion(ctx, "a"); v == before {
		t.Fatal("the plugins version did not move")
	}
	if changed, _ := s.SetPluginsSetup(ctx, "a", []string{"pa"}, nil); !changed {
		t.Fatal("clearing pa changed nothing")
	}
	if !plugin(t, s, "a", b.ID).NeedsSetup || plugin(t, s, "a", a.ID).NeedsSetup {
		t.Fatalf("pa %v pb %v", plugin(t, s, "a", a.ID).NeedsSetup, plugin(t, s, "a", b.ID).NeedsSetup)
	}
	if changed, _ := s.SetPluginsSetup(ctx, "a", []string{"pa"}, nil); changed {
		t.Fatal("no change reported a change")
	}
}
