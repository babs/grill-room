package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/babs/grill-room/internal/plugin"
	"github.com/babs/grill-room/internal/sandbox"
	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

// 009-AC22: option values persist per room and plugin id in an owner-only file, read back on
// start; Keep drops the plugins the room no longer lists.
func TestPluginOptionsStore(t *testing.T) {
	dir := t.TempDir()
	o, err := NewPluginOptions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Set("https://h/r/a", 1, OptionValues{Name: "p", Values: map[string]any{"api_key": "s3cret"}, Sensitive: []string{"api_key"}}); err != nil {
		t.Fatal(err)
	}
	if err := o.Set("https://h/r/b", 1, OptionValues{Name: "p", Values: map[string]any{"region": "eu"}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "plugin-options.json")
	if info, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", err, info.Mode())
	}
	again, err := NewPluginOptions(dir)
	if err != nil {
		t.Fatal(err)
	}
	if v := again.Get("https://h/r/a", 1); v.Values["api_key"] != "s3cret" || !slices.Equal(v.Sensitive, []string{"api_key"}) ||
		again.Get("https://h/r/a", 2).Values != nil || len(again.Room("https://h/r/b")) != 1 {
		t.Fatalf("read back: %+v %+v", again.Room("https://h/r/a"), again.Room("https://h/r/b"))
	}
	if err := again.Keep("https://h/r/a", []int64{2}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "s3cret") || !strings.Contains(string(b), "eu") { //nolint:gosec // test temp dir
		t.Fatalf("after keeping none of room a: %s", b)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPluginOptions(dir); err == nil {
		t.Fatal("a broken file was read")
	}
}

// listed is a room plugin as the room server lists it for the daemon.
type listed = struct {
	ID      int64           `json:"id"`
	Name    string          `json:"name"`
	Version int             `json:"version"`
	Options []plugin.Option `json:"options"`
	Domains []string        `json:"domains"`
}

// hostRoom registers a room run whose plugins the room listed, as loadPlugins would.
func hostRoom(d *Daemon, link string, ps ...listed) *roomRun {
	r := &roomRun{d: d, room: Room{Base: "https://h", ID: strings.TrimPrefix(link, "https://h/r/")}, plugins: &roomPlugins{Plugins: ps}}
	d.mu.Lock()
	d.rooms[r.room.String()] = r
	d.mu.Unlock()
	return r
}

var optPlugin = listed{ID: 7, Name: "opt", Version: 2, Options: []plugin.Option{
	{Key: "api_key", Type: "string", Sensitive: true, Required: true},
	{Key: "region", Type: "string", Default: "eu"},
	{Key: "debug", Type: "boolean"},
	{Key: "port", Type: "number", Required: true},
	{Key: "pin", Type: "number", Sensitive: true},
}, Domains: []string{"mcp.example.com", "api.github.com"}}

// 009-AC22 009-AC24 009-AC25: the page saves only declared options, each of its type; a sensitive
// value is kept but never shown back, only that it is set; a required option with no default and
// no value makes the plugin need host setup; the domains off the allowlist are listed.
func TestSetPluginOptions(t *testing.T) {
	d := newTestDaemon(t, nil)
	d.st.Egress.Server = entriesFor([]string{".github.com"}, nil)
	const link = "https://h/r/a"
	hostRoom(d, link, optPlugin)
	ctx := context.Background()
	for _, bad := range []map[string]any{{"nope": "x"}, {"debug": "yes"}, {"port": "80"}, {"api_key": strings.Repeat("x", maxOptionLen+1)}, {"api_key": "a\x00b"}} {
		if err := d.SetPluginOptions(ctx, link, optPlugin.ID, bad, nil); err == nil {
			t.Errorf("%v saved", bad)
		} else if strings.Contains(err.Error(), "xxxx") {
			t.Errorf("the error quotes the value: %v", err)
		}
	}
	if err := d.SetPluginOptions(ctx, link, 99, map[string]any{"x": 1.0}, nil); err == nil {
		t.Error("a plugin the room does not have")
	}
	if err := d.SetPluginOptions(ctx, link, optPlugin.ID, map[string]any{"api_key": "s3cret-value", "debug": true}, nil); err != nil {
		t.Fatal(err)
	}
	hp := d.Status().Rooms[0].Plugins[0]
	if !hp.NeedsSetup || !slices.Equal(hp.Set, []string{"api_key", "debug"}) || hp.Values["debug"] != true || hp.Values["api_key"] != nil ||
		!slices.Equal(hp.OffAllowlist, []string{"mcp.example.com"}) {
		t.Fatalf("page view %+v", hp)
	}
	if v := d.o.PluginOptions.Get(link, optPlugin.ID); v.Name != "opt" || !slices.Equal(v.Sensitive, []string{"api_key", "pin"}) {
		t.Fatalf("stored %+v", v)
	}
	// An absent key keeps its value, an empty one or a cleared one forgets it.
	if err := d.SetPluginOptions(ctx, link, optPlugin.ID, map[string]any{"port": 8080.0, "debug": ""}, nil); err != nil {
		t.Fatal(err)
	}
	if v := d.o.PluginOptions.Get(link, optPlugin.ID).Values; v["api_key"] != "s3cret-value" || v["port"] != 8080.0 || v["debug"] != nil {
		t.Fatalf("values %+v", v)
	}
	if hp := d.Status().Rooms[0].Plugins[0]; hp.NeedsSetup {
		t.Fatal("still needs setup with every required option set")
	}
	if err := d.SetPluginOptions(ctx, link, optPlugin.ID, nil, []string{"api_key"}); err != nil {
		t.Fatal(err)
	}
	if hp := d.Status().Rooms[0].Plugins[0]; !hp.NeedsSetup || slices.Contains(hp.Set, "api_key") {
		t.Fatalf("after clearing: %+v", hp)
	}
}

// 009-AC22: a plugin removed then added again under its name is another plugin to the room: it
// starts with no value, and the old one's go once the room lists it no more.
func TestPluginOptionsFollowThePluginID(t *testing.T) {
	d := newTestDaemon(t, nil)
	const link = "https://h/r/a"
	r := hostRoom(d, link, optPlugin)
	if err := d.SetPluginOptions(context.Background(), link, optPlugin.ID, map[string]any{"api_key": "s3cret-value", "port": 1.0}, nil); err != nil {
		t.Fatal(err)
	}
	again := optPlugin
	again.ID = 8
	d.mu.Lock()
	r.plugins = &roomPlugins{Plugins: []listed{again}}
	d.mu.Unlock()
	if hp := d.Status().Rooms[0].Plugins[0]; !hp.NeedsSetup || len(hp.Set) != 0 {
		t.Fatalf("the new plugin inherited: %+v", hp)
	}
	load, skipped, _, _ := d.turnPluginsSetup(link, []turn.Plugin{{ID: 8, Name: "opt", Options: turnOptions(again.Options)}})
	if len(load) != 0 || len(skipped) != 1 {
		t.Fatalf("load %+v skipped %+v", load, skipped)
	}
	if err := d.o.PluginOptions.Keep(link, []int64{8}); err != nil {
		t.Fatal(err)
	}
	if len(d.o.PluginOptions.Room(link)) != 0 {
		t.Fatalf("the removed plugin's values stay: %+v", d.o.PluginOptions.Room(link))
	}
}

// 009-AC23 009-AC24: at turn time a plugin with a required option unset is left out, said why;
// the others get their values as Claude Code reads them, defaults included, sensitive ones apart.
// The redaction takes the string values of options known sensitive, also of a plugin this turn
// does not load, never a number, a boolean or a value not sensitive.
func TestTurnPluginsSetup(t *testing.T) {
	d := newTestDaemon(t, nil)
	const link = "https://h/r/a"
	opts := turnOptions(optPlugin.Options)
	set := func(id int64, name string, v map[string]any, sensitive ...string) {
		if err := d.o.PluginOptions.Set(link, id, OptionValues{Name: name, Values: v, Sensitive: sensitive}); err != nil {
			t.Fatal(err)
		}
	}
	set(7, "opt", map[string]any{"api_key": "s3cret-value", "port": 1234.0, "pin": 9876.0, "region": "us-east-1"}, "api_key", "pin")
	set(8, "unset", map[string]any{"region": "abcdef"})
	set(9, "gone", map[string]any{"token": "old-secret", "flag": true, "name": "plain-value"}, "token", "flag")
	_ = d.o.PluginOptions.Set("https://h/r/other", 7, OptionValues{Name: "opt", Values: map[string]any{"api_key": "other-room"}, Sensitive: []string{"api_key"}})
	load, skipped, po, redact := d.turnPluginsSetup(link, []turn.Plugin{{ID: 7, Name: "opt", Options: opts}, {ID: 8, Name: "unset", Options: opts}})
	if len(load) != 1 || load[0].Name != "opt" || !slices.Equal(skipped, []turn.SkippedPlugin{{Name: "unset", Reason: turn.NeedsHostSetup}}) {
		t.Fatalf("load %+v, skipped %+v", load, skipped)
	}
	if po.Secrets["opt@inline"]["api_key"] != "s3cret-value" || po.Configs["opt@inline"]["region"] != "us-east-1" || po.Configs["opt@inline"]["port"] != 1234.0 ||
		po.Secrets["opt@inline"]["pin"] != 9876.0 || po.Configs["opt@inline"]["api_key"] != nil || po.Secrets["unset@inline"] != nil || po.Configs["unset@inline"] != nil {
		t.Fatalf("options %+v", po)
	}
	if !slices.Equal(redact, []string{"old-secret", "s3cret-value"}) {
		t.Fatalf("redact %q", redact)
	}
	if load, skipped, _, _ := d.turnPluginsSetup(link, nil); len(load)+len(skipped) != 0 {
		t.Fatal("a job without plugins")
	}
}

// 009-AC22 009-AC23 009-AC24: against a real room server, a plugin whose required option the host
// has not set does not load: the turn runs without it, the room marks it "needs host setup", and
// the daemon page says so. Once the host sets the value on the page, the next turn loads it with
// the value for Claude Code and its redaction, and the room clears the mark. The room's own view
// never holds the value.
func TestDaemonPluginOptions(t *testing.T) {
	rs := newRoomServer(t)
	ag := &fakeAgent{}
	ag.run = func(_ context.Context, in sandbox.TurnInput, _ map[string]string) (*sandbox.TurnOutput, error) {
		if strings.Contains(in.Prompt, "FAIL") && len(in.Redact) > 0 {
			// A hook printed the value and claude failed on it: the error quotes it.
			return nil, fmt.Errorf("claude: error_during_execution: hook said %s", in.Redact[0])
		}
		return &sandbox.TurnOutput{Result: &sandbox.Result{Result: "ok", SessionID: sid}, SessionOffset: in.SessionSize, Session: []byte("{}\n")}, nil
	}
	fi := &fakeInspector{validate: validOutput, details: "x 1\n"}
	core, logs := observer.New(zap.DebugLevel)
	var evMu sync.Mutex
	var events []TurnEvent
	alice, d, _ := pairedDaemon(t, rs, "Alice", ag, func(d *Daemon) {
		d.o.Docker = fi.run
		d.o.PluginCacheDir = t.TempDir()
		d.o.Log = zap.New(core)
		d.o.OnTurn = func(e TurnEvent) { evMu.Lock(); events = append(events, e); evMu.Unlock() }
	})
	link := rs.ts.URL + "/r/" + rs.room
	rs.addPlugin(t, alice, "opt", snapshotOf(t, map[string]string{plugin.ManifestPath: `{"name":"opt","userConfig":{
		"api_key":{"type":"string","title":"API key","sensitive":true,"required":true},"region":{"type":"string","default":"eu"}}}`}), rs.grill(t).ID)
	waitFor(t, "approved", func() bool { return rs.version(t, "opt").State == store.PluginApproved })
	last := func() sandbox.TurnInput {
		ag.mu.Lock()
		defer ag.mu.Unlock()
		return ag.inputs[len(ag.inputs)-1]
	}
	needs := func() bool {
		ps, err := rs.st.RoomPlugins(t.Context(), rs.room)
		if err != nil || len(ps) != 1 {
			t.Fatalf("plugins %v %+v", err, ps)
		}
		return ps[0].NeedsSetup
	}
	if v := rs.turn(t, alice, "go", false); v.Turn.Status != "done" {
		t.Fatalf("turn without the value: %+v", v.Turn)
	}
	if in := last(); len(in.Plugins) != 0 || len(in.PluginOptions.Secrets) != 0 || !needs() {
		t.Fatalf("unset: plugins %+v, options %+v, needs setup %v", in.Plugins, in.PluginOptions, needs())
	}
	waitFor(t, "the page lists the plugin", func() bool {
		rooms := d.Status().Rooms
		return len(rooms) == 1 && len(rooms[0].Plugins) == 1 && rooms[0].Plugins[0].NeedsSetup
	})
	if err := d.SetPluginOptions(context.Background(), link, d.Status().Rooms[0].Plugins[0].ID, map[string]any{"api_key": "s3cret-value"}, nil); err != nil {
		t.Fatal(err)
	}
	if v := rs.turn(t, alice, "go", false); v.Turn.Status != "done" {
		t.Fatalf("turn with the value: %+v", v.Turn)
	}
	in := last()
	if len(in.Plugins) != 1 || in.PluginOptions.Secrets["opt@inline"]["api_key"] != "s3cret-value" || in.PluginOptions.Configs["opt@inline"]["region"] != "eu" ||
		!slices.Contains(in.Redact, "s3cret-value") || needs() {
		t.Fatalf("set: plugins %d, options %+v, redact %d, needs setup %v", len(in.Plugins), in.PluginOptions, len(in.Redact), needs())
	}
	// 009-AC23: an error quoting the value reaches the room, the turn event and the log redacted.
	if v := rs.turn(t, alice, "FAIL", false); v.Turn.Status != "failed" || !strings.Contains(v.Turn.Error, "hook said [redacted]") {
		t.Fatalf("failed turn: %+v", v.Turn)
	}
	evMu.Lock()
	for _, e := range events {
		if strings.Contains(e.Error, "s3cret") {
			t.Errorf("turn event %+v", e)
		}
	}
	evMu.Unlock()
	for _, e := range logs.All() {
		if b, _ := json.Marshal(e.ContextMap()); strings.Contains(e.Message+string(b), "s3cret") {
			t.Errorf("log %s %s", e.Message, b)
		}
	}
	var view struct {
		Plugins []json.RawMessage `json:"plugins"`
	}
	rs.call(t, "GET", "/api/rooms/"+rs.room+"/plugins", alice, nil, http.StatusOK, &view)
	if b, _ := json.Marshal(view); strings.Contains(string(b), "s3cret") {
		t.Fatal("the room's view holds the value")
	}
}

// 009-AC19 009-AC22: the worker answers the supervisor's plugin requests with the daemon's own
// checks.
func TestWorkerHandlesPluginRequests(t *testing.T) {
	d := newTestDaemon(t, nil)
	const link = "https://h/r/a"
	hostRoom(d, link, optPlugin)
	if r := d.handle(t.Context(), Msg{Type: "plugin_options", ID: 1, Room: link, Plugin: optPlugin.ID, Values: map[string]any{"port": 1.0}}); r.Error != "" {
		t.Fatalf("options: %+v", r)
	}
	if v := d.o.PluginOptions.Get(link, optPlugin.ID).Values; v["port"] != 1.0 {
		t.Fatalf("stored %+v", v)
	}
	if r := d.handle(t.Context(), Msg{Type: "plugin_options", ID: 2, Room: link, Plugin: 99}); r.Error != "this room has no such plugin" {
		t.Fatalf("unknown plugin: %+v", r)
	}
	if r := d.handle(t.Context(), Msg{Type: "plugin_fetch", ID: 3, Room: link, Fetch: 5, Accept: true}); r.Error != errNoRequest.Error() {
		t.Fatalf("unknown request: %+v", r)
	}
}
