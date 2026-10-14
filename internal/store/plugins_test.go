package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func snap(commit, data string) PluginSnapshot {
	return PluginSnapshot{Commit: commit, SHA256: "sha-" + commit, Size: int64(len(data)), Files: 1,
		FileList: []byte(`[{"path":"a","sha256":"x","size":1}]`), Manifest: []byte(`{"name":"x"}`), Data: []byte(data)}
}

func newPlugin(name, commit string) NewPlugin {
	v := snap(commit, "tar of "+name+"@"+commit)
	v.SHA256 = "sha-" + name + "-" + commit
	return NewPlugin{Name: name, URL: "https://example.com/" + name, Ref: "", RefName: "main", PluginSnapshot: v}
}

func addPlugin(t *testing.T, s *Store, room string, by *Participant, name, commit string, personas ...int64) *RoomPlugin {
	t.Helper()
	if _, err := s.AddPlugins(context.Background(), room, []NewPlugin{newPlugin(name, commit)}, personas, by, 1<<20); err != nil {
		t.Fatal(err)
	}
	ps, err := s.RoomPlugins(context.Background(), room)
	if err != nil {
		t.Fatal(err)
	}
	for i := range ps {
		if ps[i].Name == name {
			return &ps[i]
		}
	}
	t.Fatalf("plugin %s not listed", name)
	return nil
}

func plugin(t *testing.T, s *Store, room string, id int64) *RoomPlugin {
	t.Helper()
	p, err := s.RoomPlugin(context.Background(), room, id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func versionStates(p *RoomPlugin) string {
	var out []string
	for _, v := range p.Versions {
		out = append(out, fmt.Sprintf("v%d:%s", v.N, v.State))
	}
	return strings.Join(out, " ")
}

// 009-AC10: at most 10 plugins per room and 5 per persona; a name taken in the room or active on
// the persona is refused; a persona of another room refuses the whole add.
func TestPluginLimits(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	other := newTurnRoom(t, s, "b")
	if _, err := s.AddPlugins(ctx, "a", []NewPlugin{newPlugin("p0", "c0")}, []int64{other.grill.ID}, r.p, 1<<20); !errors.Is(err, ErrNotFound) {
		t.Fatalf("persona of another room: %v", err)
	}
	if ps, _ := s.RoomPlugins(ctx, "a"); len(ps) != 0 {
		t.Fatalf("a refused add left %d plugins", len(ps))
	}
	for i := range 5 {
		addPlugin(t, s, "a", r.p, fmt.Sprintf("p%d", i), "c", r.grill.ID)
	}
	if _, err := s.AddPlugins(ctx, "a", []NewPlugin{newPlugin("p5", "c")}, []int64{r.grill.ID}, r.p, 1<<20); !errors.Is(err, ErrLimit) {
		t.Fatalf("6th plugin on the persona: %v", err)
	}
	if _, err := s.AddPlugins(ctx, "a", []NewPlugin{newPlugin("p0", "c")}, nil, r.p, 1<<20); !errors.Is(err, ErrPluginName) {
		t.Fatalf("same name twice: %v", err)
	}
	var six []NewPlugin
	for i := 5; i < 11; i++ {
		six = append(six, newPlugin(fmt.Sprintf("p%d", i), "c"))
	}
	if _, err := s.AddPlugins(ctx, "a", six, nil, r.p, 1<<20); !errors.Is(err, ErrLimit) {
		t.Fatalf("11th plugin in the room: %v", err)
	}
	p5 := addPlugin(t, s, "a", r.p, "p5", "c")
	if err := s.SetPersonaPlugin(ctx, "a", r.grill.ID, p5.ID, true); !errors.Is(err, ErrLimit) {
		t.Fatalf("6th activation: %v", err)
	}
	if err := s.SetPersonaPlugin(ctx, "a", other.grill.ID, p5.ID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("persona of another room: %v", err)
	}
	ps, _ := s.RoomPlugins(ctx, "a")
	if err := s.SetPersonaPlugin(ctx, "a", r.grill.ID, ps[0].ID, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPersonaPlugin(ctx, "a", r.grill.ID, p5.ID, true); err != nil {
		t.Fatalf("5th activation after one off: %v", err)
	}
	// Room names are unique, so only a plugin removed with its activation kept could clash.
	if _, err := s.db.ExecContext(ctx, `UPDATE room_plugins SET removed_at = ? WHERE id = ?`, now(), p5.ID); err != nil {
		t.Fatal(err)
	}
	again := addPlugin(t, s, "a", r.p, "p5", "c2")
	if err := s.SetPersonaPlugin(ctx, "a", r.grill.ID, again.ID, true); !errors.Is(err, ErrPersonaPluginName) {
		t.Fatalf("same name active twice on a persona: %v", err)
	}
}

// 009-AC11 009-AC13 009-AC14: a version waits for inspection, then is approved at once or waits
// for the host under "approve everything"; the host approves or refuses; a failed validation is
// invalid; an update to the same commit is up to date; the approved version keeps running.
func TestPluginVersionStates(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	if err := s.SetHost(ctx, "a", r.p.ID); err != nil {
		t.Fatal(err)
	}
	p := addPlugin(t, s, "a", r.p, "babs", "c1", r.grill.ID)
	if ids, err := s.InspectingVersions(ctx, "a"); err != nil || len(ids) != 1 || ids[0] != p.Versions[0].ID {
		t.Fatalf("inspecting: %v %v", ids, err)
	}
	if p.Running() != nil || p.Versions[0].AddedBy != "Alice" {
		t.Fatalf("v1: %+v", p.Versions[0])
	}
	m, state, err := s.ReportPluginVersion(ctx, "a", p.Versions[0].ID, hostDaemon(t, s), true, []byte(`{"ok":true}`), "")
	if err != nil || state != PluginApproved || !strings.Contains(m.Body, "approved automatically") {
		t.Fatalf("report: %v %s %v", m, state, err)
	}
	if _, _, err := s.ReportPluginVersion(ctx, "a", p.Versions[0].ID, hostDaemon(t, s), true, nil, ""); !errors.Is(err, ErrPluginState) {
		t.Fatalf("second report: %v", err)
	}
	if _, err := s.AddPluginVersion(ctx, "a", p.ID, "main", snap("c1", "x"), r.p, 1<<20); !errors.Is(err, ErrUpToDate) {
		t.Fatalf("same commit: %v", err)
	}
	m, err = s.AddPluginVersion(ctx, "a", p.ID, "main", snap("c2", "tar2"), r.p, 1<<20)
	if err != nil || m.Body != "Alice updated the plugin babs to v2, main @ c2." {
		t.Fatalf("update: %+v %v", m, err)
	}
	dm, err := s.HostDaemon(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDaemonSettings(ctx, dm.ID, DaemonSettings{ApproveAll: true}); err != nil {
		t.Fatal(err)
	}
	p = plugin(t, s, "a", p.ID)
	if _, state, err = s.ReportPluginVersion(ctx, "a", p.Versions[1].ID, hostDaemon(t, s), true, nil, ""); err != nil || state != PluginPending {
		t.Fatalf("report under approve everything: %s %v", state, err)
	}
	if p = plugin(t, s, "a", p.ID); p.Running().N != 1 || versionStates(p) != "v1:approved v2:pending" {
		t.Fatalf("pending: running %d, %s", p.Running().N, versionStates(p))
	}
	bob, _, err := s.Join(ctx, "inv-a", "Bob", "tok-bob", JoinProfile{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecidePluginVersion(ctx, "a", p.ID, 2, true, bob); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member decides: %v", err)
	}
	if m, err = s.DecidePluginVersion(ctx, "a", p.ID, 2, true, r.p); err != nil || m.Body != "Alice approved the plugin babs v2." {
		t.Fatalf("approve: %+v %v", m, err)
	}
	if _, err := s.DecidePluginVersion(ctx, "a", p.ID, 2, false, r.p); !errors.Is(err, ErrPluginState) {
		t.Fatalf("decided twice: %v", err)
	}
	if _, err := s.AddPluginVersion(ctx, "a", p.ID, "main", snap("c3", "tar3"), r.p, 1<<20); err != nil {
		t.Fatal(err)
	}
	p = plugin(t, s, "a", p.ID)
	m, state, err = s.ReportPluginVersion(ctx, "a", p.Versions[2].ID, hostDaemon(t, s), false, nil, "skills/x: bad frontmatter")
	if err != nil || state != PluginInvalid || m.Body != "The plugin babs v3 was refused by Claude Code: skills/x: bad frontmatter." {
		t.Fatalf("invalid: %+v %s %v", m, state, err)
	}
	if _, err := s.DecidePluginVersion(ctx, "a", p.ID, 3, true, r.p); !errors.Is(err, ErrPluginState) {
		t.Fatalf("approve an invalid version: %v", err)
	}
	if _, err := s.AddPluginVersion(ctx, "a", p.ID, "main", snap("c4", "tar4"), r.p, 1<<20); err != nil {
		t.Fatal(err)
	}
	p = plugin(t, s, "a", p.ID)
	if _, _, err := s.ReportPluginVersion(ctx, "a", p.Versions[3].ID, hostDaemon(t, s), true, nil, ""); err != nil {
		t.Fatal(err)
	}
	if m, err = s.DecidePluginVersion(ctx, "a", p.ID, 4, false, r.p); err != nil || !strings.Contains(m.Body, "refused the plugin babs v4") {
		t.Fatalf("refuse: %+v %v", m, err)
	}
	p = plugin(t, s, "a", p.ID)
	if p.Running().N != 2 || versionStates(p) != "v1:approved v2:approved v3:invalid v4:refused" || p.Versions[2].Reason != "refused by Claude Code: skills/x: bad frontmatter" {
		t.Fatalf("final: running %d, %s, %q", p.Running().N, versionStates(p), p.Versions[2].Reason)
	}
	if ids, _ := s.InspectingVersions(ctx, "a"); len(ids) != 0 {
		t.Fatalf("still inspecting: %v", ids)
	}
}

// 009-AC17 009-AC18: a turn copies the approved running version of each plugin its persona has on;
// a plugin with no approved version, or off, is not loaded; its tar is served by sha256 only for
// a version the turn lists.
func TestTurnPlugins(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	on := addPlugin(t, s, "a", r.p, "on", "c1", r.grill.ID)
	pending := addPlugin(t, s, "a", r.p, "pending", "c1", r.grill.ID)
	off := addPlugin(t, s, "a", r.p, "off", "c1")
	for _, p := range []*RoomPlugin{on, off} {
		if _, _, err := s.ReportPluginVersion(ctx, "a", p.Versions[0].ID, hostDaemon(t, s), true, nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	// A newer version waiting does not replace the approved one.
	if _, err := s.AddPluginVersion(ctx, "a", on.ID, "main", snap("c2", "tar2"), r.p, 1<<20); err != nil {
		t.Fatal(err)
	}
	tn := r.start(t, s, "a")
	got, err := s.TurnPlugins(ctx, "a", tn.ID)
	if err != nil || len(got) != 1 || got[0].Name != "on" || got[0].N != 1 || got[0].SHA256 != "sha-on-c1" || got[0].VersionID != on.Versions[0].ID {
		t.Fatalf("turn plugins: %+v %v", got, err)
	}
	if data, err := s.TurnPluginData(ctx, "a", tn.ID, "sha-on-c1"); err != nil || string(data) != "tar of on@c1" {
		t.Fatalf("tar: %q %v", data, err)
	}
	for _, sha := range []string{"sha-c2", pending.Versions[0].SHA256, ""} {
		if _, err := s.TurnPluginData(ctx, "a", tn.ID, sha); !errors.Is(err, ErrNotFound) {
			t.Fatalf("tar %q not on the turn: %v", sha, err)
		}
	}
	if _, err := s.TurnPluginData(ctx, "b", tn.ID, "sha-on-c1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another room's turn: %v", err)
	}
}

func pluginRows(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), `SELECT count(*) FROM room_plugins`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 009-AC12: removing a plugin takes it off every persona at once; its snapshots go at once, or
// when the running turn that loads it ends, or at the next start.
func TestRemovePlugin(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	idle := addPlugin(t, s, "a", r.p, "idle", "c1", r.grill.ID)
	used := addPlugin(t, s, "a", r.p, "used", "c1", r.grill.ID)
	if _, _, err := s.ReportPluginVersion(ctx, "a", used.Versions[0].ID, hostDaemon(t, s), true, nil, ""); err != nil {
		t.Fatal(err)
	}
	tn := r.start(t, s, "a")
	if m, err := s.RemovePlugin(ctx, "a", idle.ID, r.p); err != nil || m.Body != "Alice removed the plugin idle." {
		t.Fatalf("remove: %+v %v", m, err)
	}
	if n := pluginRows(t, s); n != 1 {
		t.Fatalf("an unused removed plugin stays: %d rows", n)
	}
	if _, err := s.RemovePlugin(ctx, "a", used.ID, r.p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemovePlugin(ctx, "a", used.ID, r.p); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed twice: %v", err)
	}
	if ps, _ := s.RoomPlugins(ctx, "a"); len(ps) != 0 || pluginRows(t, s) != 1 {
		t.Fatalf("listed %d, rows %d", len(ps), pluginRows(t, s))
	}
	if _, err := s.TurnPluginData(ctx, "a", tn.ID, "sha-used-c1"); err != nil {
		t.Fatalf("the running turn lost its tar: %v", err)
	}
	// The name is free again for a new plugin while the removed one waits.
	addPlugin(t, s, "a", r.p, "used", "c9")
	if _, err := s.FailTurn(ctx, "a", tn.ID, "boom"); err != nil {
		t.Fatal(err)
	}
	if n := pluginRows(t, s); n != 1 {
		t.Fatalf("the removed plugin outlived its turn: %d rows", n)
	}
	var acts int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM room_persona_plugins`).Scan(&acts); err != nil || acts != 0 {
		t.Fatalf("activations left: %d %v", acts, err)
	}
}

// 009-AC12: a removed plugin left by a server stopped mid-turn goes at the next start.
func TestOpenPurgesRemovedPlugins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r := newTurnRoom(t, s, "a")
	p := addPlugin(t, s, "a", r.p, "x", "c1")
	if _, err := s.db.ExecContext(context.Background(), `UPDATE room_plugins SET removed_at = ? WHERE id = ?`, now(), p.ID); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if n := pluginRows(t, s); n != 0 {
		t.Fatalf("rows after start: %d", n)
	}
}

// 009-AC9: snapshots count in the room's volume and against its quota.
func TestPluginVolumeAndQuota(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	addPlugin(t, s, "a", r.p, "x", "c1")
	o, err := s.RoomOverview(ctx, "a", 1<<20)
	if err != nil || o.Volume.Plugins != int64(len("tar of x@c1")) || o.Volume.Counted() != o.Volume.Plugins {
		t.Fatalf("volume: %+v %v", o.Volume, err)
	}
	if used, _, err := s.RoomUsage(ctx, "a", 1<<20); err != nil || used != o.Volume.Plugins {
		t.Fatalf("usage: %d %v", used, err)
	}
	big := newPlugin("big", "c1")
	big.Data = make([]byte, 100)
	if _, err := s.AddPlugins(ctx, "a", []NewPlugin{big}, nil, r.p, 100); !errors.Is(err, ErrRoomFull) {
		t.Fatalf("over quota: %v", err)
	}
	if _, err := s.AddPluginVersion(ctx, "a", plugin(t, s, "a", addPlugin(t, s, "a", r.p, "y", "c1").ID).ID, "main", snap("c2", string(make([]byte, 100))), r.p, 100); !errors.Is(err, ErrRoomFull) {
		t.Fatalf("update over quota: %v", err)
	}
}

// 009-AC13: the host's daemon reads a version's snapshot by id, in its own room only.
func TestPluginVersionData(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	newTurnRoom(t, s, "b")
	p := addPlugin(t, s, "a", r.p, "x", "c1")
	if p.Latest().N != 1 {
		t.Fatalf("latest: %+v", p.Latest())
	}
	if data, err := s.PluginVersionData(ctx, "a", p.Versions[0].ID); err != nil || string(data) != "tar of x@c1" {
		t.Fatalf("data: %q %v", data, err)
	}
	if _, err := s.PluginVersionData(ctx, "b", p.Versions[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another room: %v", err)
	}
	if _, err := s.RoomPlugin(ctx, "b", p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("plugin of another room: %v", err)
	}
	if (&RoomPlugin{}).Latest() != nil {
		t.Fatal("latest of none")
	}
}

// hostDaemon makes room a's first paired daemon its host, unless the room has one, and returns its id.
func hostDaemon(t *testing.T, s *Store) int64 {
	t.Helper()
	ctx := context.Background()
	if d, err := s.HostDaemon(ctx, "a"); err == nil {
		return d.ID
	}
	var id, pid int64
	if err := s.db.QueryRowContext(ctx, `SELECT id, participant_id FROM daemons WHERE room_id = 'a' ORDER BY id LIMIT 1`).Scan(&id, &pid); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHost(ctx, "a", pid); err != nil {
		t.Fatal(err)
	}
	return id
}

func dataLen(t *testing.T, s *Store, id int64) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), `SELECT COALESCE((SELECT length(data) FROM plugin_snapshots WHERE version_id = ?), 0)`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 009-AC9 009-AC12: once a newer version is approved, an older one's snapshot is emptied, at once
// or when the running turn that loads it ends; its row stays, and the room's quota drops.
func TestSupersededSnapshotsEmptied(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	p := addPlugin(t, s, "a", r.p, "x", "c1", r.grill.ID)
	if _, _, err := s.ReportPluginVersion(ctx, "a", p.Versions[0].ID, hostDaemon(t, s), true, nil, ""); err != nil {
		t.Fatal(err)
	}
	tn := r.start(t, s, "a")
	for _, c := range []string{"c2", "c3"} {
		if _, err := s.AddPluginVersion(ctx, "a", p.ID, "main", snap(c, "tar-"+c), r.p, 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	p = plugin(t, s, "a", p.ID)
	before, _, _ := s.RoomUsage(ctx, "a", 1<<20)
	// v3 approved while v2 still waits: v2 is superseded too.
	if _, _, err := s.ReportPluginVersion(ctx, "a", p.Versions[2].ID, hostDaemon(t, s), true, nil, ""); err != nil {
		t.Fatal(err)
	}
	if v1, v2, v3 := dataLen(t, s, p.Versions[0].ID), dataLen(t, s, p.Versions[1].ID), dataLen(t, s, p.Versions[2].ID); v1 == 0 || v2 != len("tar-c2") || v3 == 0 {
		t.Fatalf("while the turn loads v1 and v2 waits for inspection: %d %d %d", v1, v2, v3)
	}
	if _, _, err := s.ReportPluginVersion(ctx, "a", p.Versions[1].ID, hostDaemon(t, s), false, nil, "bad"); err != nil {
		t.Fatal(err)
	}
	if v2 := dataLen(t, s, p.Versions[1].ID); v2 != 0 {
		t.Fatalf("an invalid version older than the running one keeps %d bytes", v2)
	}
	if _, err := s.FailTurn(ctx, "a", tn.ID, "boom"); err != nil {
		t.Fatal(err)
	}
	after, _, _ := s.RoomUsage(ctx, "a", 1<<20)
	if v1 := dataLen(t, s, p.Versions[0].ID); v1 != 0 || after != before-int64(len("tar of x@c1"))-int64(len("tar-c2")) {
		t.Fatalf("after the turn: v1 %d bytes, usage %d -> %d", v1, before, after)
	}
	if p = plugin(t, s, "a", p.ID); len(p.Versions) != 3 || p.Running().N != 3 {
		t.Fatalf("rows: %s", versionStates(p))
	}
}

// 009-AC12: the trigger runs the very statements the store runs at start and on removal.
func TestPurgeTriggerMatchesTheStatements(t *testing.T) {
	s := openTest(t)
	var sql string
	if err := s.db.QueryRowContext(context.Background(), `SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = 'turns_purge_plugins'`).Scan(&sql); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{purgePlugins, dropSuperseded} {
		if !strings.Contains(sql, q+";") {
			t.Fatalf("the trigger lacks:\n%s\nit reads:\n%s", q, sql)
		}
	}
}

// 009-AC13: only the room's host daemon reports, checked in the report's transaction; a removed
// plugin's snapshot is gone from the inspection route.
func TestReportNeedsTheHostDaemon(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	host := hostDaemon(t, s)
	bob, _, err := s.Join(ctx, "inv-a", "Bob", "tok-bob", JoinProfile{})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.PairDaemon(ctx, "a", bob.ID, "dmn-bob")
	if err != nil {
		t.Fatal(err)
	}
	p := addPlugin(t, s, "a", r.p, "x", "c1")
	if _, _, err := s.ReportPluginVersion(ctx, "a", p.Versions[0].ID, other.ID, true, nil, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-host report: %v", err)
	}
	if _, _, err := s.ReportPluginVersion(ctx, "a", 999, host, true, nil, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown version: %v", err)
	}
	if _, err := s.RemovePlugin(ctx, "a", p.ID, r.p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PluginVersionData(ctx, "a", p.Versions[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed plugin's snapshot: %v", err)
	}
	if _, err := s.DecidePluginVersion(ctx, "a", p.ID, 1, true, r.p); !errors.Is(err, ErrNotFound) {
		t.Fatalf("decide on a removed plugin: %v", err)
	}
	if _, err := s.AddPluginVersion(ctx, "a", p.ID, "main", snap("c2", "x"), r.p, 1<<20); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update a removed plugin: %v", err)
	}
	if err := s.SetPersonaPlugin(ctx, "a", r.grill.ID, p.ID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("activate a removed plugin: %v", err)
	}
	if _, err := s.TurnPlugins(ctx, "a", 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown turn: %v", err)
	}
}

// 009-AC9: a stored cap below 1 MB reads as the default.
func TestPluginMaxMBDefault(t *testing.T) {
	s := openTest(t)
	if err := s.SetSetting(context.Background(), settingPluginMaxMB, "0"); err != nil {
		t.Fatal(err)
	}
	if v, err := s.InstanceSettings(context.Background()); err != nil || v.PluginMaxMB != DefaultPluginMaxMB {
		t.Fatalf("%+v %v", v, err)
	}
}

// A failing database surfaces as an error from every plugin call, never as a refusal or a success.
func TestPluginCallsFailWithTheDatabase(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	p := addPlugin(t, s, "a", r.p, "x", "c1", r.grill.ID)
	_ = s.Close()
	for name, err := range map[string]error{
		"add": func() error {
			_, err := s.AddPlugins(ctx, "a", []NewPlugin{newPlugin("y", "c")}, nil, r.p, 1<<20)
			return err
		}(),
		"version": func() error {
			_, err := s.AddPluginVersion(ctx, "a", p.ID, "main", snap("c2", "x"), r.p, 1<<20)
			return err
		}(),
		"remove": func() error { _, err := s.RemovePlugin(ctx, "a", p.ID, r.p); return err }(),
		"toggle": s.SetPersonaPlugin(ctx, "a", r.grill.ID, p.ID, false),
		"decide": func() error { _, err := s.DecidePluginVersion(ctx, "a", p.ID, 1, true, r.p); return err }(),
		"report": func() error {
			_, _, err := s.ReportPluginVersion(ctx, "a", p.Versions[0].ID, 1, true, nil, "")
			return err
		}(),
		"inspect":   func() error { _, err := s.InspectingVersions(ctx, "a"); return err }(),
		"data":      func() error { _, err := s.PluginVersionData(ctx, "a", p.Versions[0].ID); return err }(),
		"turn":      func() error { _, err := s.TurnPlugins(ctx, "a", 1); return err }(),
		"turn data": func() error { _, err := s.TurnPluginData(ctx, "a", 1, "x"); return err }(),
		"list":      func() error { _, err := s.RoomPlugins(ctx, "a"); return err }(),
		"one":       func() error { _, err := s.RoomPlugin(ctx, "a", p.ID); return err }(),
	} {
		if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrPluginState) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestShortCommit(t *testing.T) {
	if shortCommit("abc") != "abc" || shortCommit(strings.Repeat("a", 40)) != "aaaaaaa" {
		t.Fatal("short commit")
	}
}

// 009-AC11 009-AC14: a pending version older than the running one, its files dropped, can no
// longer be approved or refused.
func TestDecideSupersededVersion(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	host := hostDaemon(t, s)
	d, err := s.HostDaemon(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDaemonSettings(ctx, d.ID, DaemonSettings{ApproveAll: true}); err != nil {
		t.Fatal(err)
	}
	p := addPlugin(t, s, "a", r.p, "x", "c1")
	if _, err := s.AddPluginVersion(ctx, "a", p.ID, "main", snap("c2", "tar2"), r.p, 1<<20); err != nil {
		t.Fatal(err)
	}
	p = plugin(t, s, "a", p.ID)
	for _, v := range p.Versions {
		if _, _, err := s.ReportPluginVersion(ctx, "a", v.ID, host, true, nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DecidePluginVersion(ctx, "a", p.ID, 2, true, r.p); err != nil {
		t.Fatal(err)
	}
	if n := dataLen(t, s, p.Versions[0].ID); n != 0 {
		t.Fatalf("v1 keeps %d bytes", n)
	}
	for _, approve := range []bool{true, false} {
		if _, err := s.DecidePluginVersion(ctx, "a", p.ID, 1, approve, r.p); !errors.Is(err, ErrPluginState) {
			t.Fatalf("decide v1 (approve %v): %v", approve, err)
		}
	}
	if p = plugin(t, s, "a", p.ID); versionStates(p) != "v1:pending v2:approved" || p.Running().N != 2 {
		t.Fatalf("states: %s", versionStates(p))
	}
}

// 009-AC5 009-AC16: the room stream's plugins version moves on an add, a report, a persona's
// activation and a removal, and only for that room.
func TestPluginsVersion(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	rb := newTurnRoom(t, s, "b")
	ver := func(room string) string {
		t.Helper()
		v, err := s.PluginsVersion(ctx, room)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	addPlugin(t, s, "b", rb.p, "other", "c1")
	other := ver("b")
	seen := map[string]bool{ver("a"): true}
	step := func(what string) {
		t.Helper()
		v := ver("a")
		if seen[v] {
			t.Fatalf("%s: version %q seen before", what, v)
		}
		seen[v] = true
	}
	p := addPlugin(t, s, "a", r.p, "babs", "c1")
	step("add")
	if _, _, err := s.ReportPluginVersion(ctx, "a", p.Versions[0].ID, hostDaemon(t, s), true, nil, ""); err != nil {
		t.Fatal(err)
	}
	step("report")
	if err := s.SetPersonaPlugin(ctx, "a", r.grill.ID, p.ID, true); err != nil {
		t.Fatal(err)
	}
	step("on")
	if _, err := s.RemovePlugin(ctx, "a", p.ID, r.p); err != nil {
		t.Fatal(err)
	}
	if v := ver("a"); v != "" || ver("b") != other || other == "" {
		t.Fatalf("after the removal: %q, other room %q then %q", v, other, ver("b"))
	}
}

// The room stream runs PluginsVersion on every change in the room: it reads plugin_versions
// through its covering index only, never the table where the rows sit.
func TestPluginsVersionUsesTheCoveringIndex(t *testing.T) {
	s := openTest(t)
	rows, err := s.db.QueryContext(context.Background(), `EXPLAIN QUERY PLAN `+pluginsVersionSQL, "a", "a")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	all := strings.Join(plan, "\n")
	if !strings.Contains(all, "SEARCH v USING COVERING INDEX plugin_versions_state") || strings.Contains(all, "SCAN v") {
		t.Fatalf("plan:\n%s", all)
	}
}

// Snapshot bytes live outside plugin_versions, so a scan of its rows never reads a tar.
func TestPluginVersionsHoldNoSnapshot(t *testing.T) {
	s := openTest(t)
	var n int
	if err := s.db.QueryRowContext(context.Background(), `SELECT count(*) FROM pragma_table_info('plugin_versions') WHERE type = 'BLOB'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("blob columns in plugin_versions: %d %v", n, err)
	}
}
