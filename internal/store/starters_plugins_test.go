package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	pluginpkg "github.com/babs/grill-room/internal/plugin"
)

// 009-AC28: "Save as starter" lists the room's plugins at their running version, with the
// personas that have them on; a plugin with no running version has nothing to carry. A room
// created from the starter gets them waiting for its AI host's inspection, then the host's mode
// decides, as for starter skills (006-AC13). Snapshots count against the storage quota (006-AC4)
// and the new room's quota.
func TestStarterPlugins(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	_, vs := pluginRoom(t, s)
	alice := joinWithProfile(t, s, "lobby", "Alice", JoinProfile{RecoveryHash: "rec", SessionHash: "sess"})
	m, err := s.RoomDraft(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Plugins) != 1 || m.Plugins[0].Name != "babs" || m.Plugins[0].Commit != vs[2].Commit || m.Plugins[0].SHA256 != vs[2].SHA256 ||
		len(m.Personas) != 1 || strings.Join(m.Personas[0].Plugins, ",") != "babs" {
		t.Fatalf("draft plugins %+v, personas %+v", m.Plugins, m.Personas)
	}
	files, err := s.RoomDraftFiles(ctx, "a", []string{m.Plugins[0].File})
	if err != nil || string(files[m.Plugins[0].File]) != string(vs[2].Data) {
		t.Fatalf("draft plugin file: %v", err)
	}
	if _, err := s.RoomDraftFiles(ctx, "lobby", []string{m.Plugins[0].File}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another room's plugin: %v", err)
	}

	used, _ := s.UsedBytes(ctx)
	if err := s.SetInstanceSettings(ctx, InstanceSettings{RoomsPerProfile: 10, RoomCreation: CreationAnyone, StorageQuota: used + 1024}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveStarter(ctx, alice.ProfileID, 0, m, files); !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), "storage quota") {
		t.Fatalf("a starter past the quota with its plugin: %v", err)
	}
	if err := s.SetInstanceSettings(ctx, InstanceSettings{RoomsPerProfile: 10, RoomCreation: CreationAnyone}); err != nil {
		t.Fatal(err)
	}
	st, err := s.SaveStarter(ctx, alice.ProfileID, 0, m, files)
	if err != nil || st.Size < int64(len(vs[2].Data)) {
		t.Fatalf("save: %+v %v", st, err)
	}
	_, sm, _ := s.StarterVersion(ctx, st.ID, 0, alice.ProfileID)
	sf, _ := s.StarterFiles(ctx, st.ID, st.Version)
	setup := func(quota int64) *RoomSetup {
		return &RoomSetup{From: &Provenance{Title: sm.Title, Version: 1}, Personas: sm.Personas, Plugins: sm.Plugins, Files: sf, SkillsPending: true,
			Chain: sm.Chain, UploadQuota: quota}
	}
	// A plugin the new room cannot take is left out, with a line, and off its personas: the room
	// is created all the same (006-AC13).
	left := func(room string, set *RoomSetup, want string, kept int) {
		t.Helper()
		if _, err := s.CreateProfileRoom(ctx, room, "R", alice.ProfileID, "Ali", "inv-"+room, "tok-"+room, "lnk-"+room, "lnkh-"+room, set); err != nil {
			t.Fatalf("%s: %v", room, err)
		}
		ps, _ := s.RoomPlugins(ctx, room)
		ms, _ := s.Messages(ctx, room, 0, 50)
		if len(ps) != kept || ms[len(ms)-1].Body != want {
			t.Fatalf("%s: %d plugins, last line %q", room, len(ps), ms[len(ms)-1].Body)
		}
	}
	// A plugin left out does not count against the storage quota either.
	used, _ = s.UsedBytes(ctx)
	if err := s.SetInstanceSettings(ctx, InstanceSettings{RoomsPerProfile: 10, RoomCreation: CreationAnyone, StorageQuota: used + int64(len(vs[2].Data))/2}); err != nil {
		t.Fatal(err)
	}
	left("small", setup(16), "The room starts without the starter's plugin babs: it would take the room past its quota.", 0)
	if _, err := s.CreateProfileRoom(ctx, "over", "R", alice.ProfileID, "Ali", "inv-over", "tok-over", "lnk-over", "lnkh-over", setup(1<<20)); !errors.Is(err, ErrCreationRefused) ||
		!strings.Contains(err.Error(), "storage quota") {
		t.Fatalf("a kept plugin past the storage quota: %v", err)
	}
	if err := s.SetInstanceSettings(ctx, InstanceSettings{RoomsPerProfile: 10, RoomCreation: CreationAnyone}); err != nil {
		t.Fatal(err)
	}
	many := setup(1 << 20)
	many.Plugins = nil
	for i := range MaxRoomPlugins + 1 {
		p := sm.Plugins[0]
		p.Name = fmt.Sprintf("p%d", i)
		many.Plugins = append(many.Plugins, p)
	}
	left("many", many, fmt.Sprintf("The room starts without the starter's plugin p10: a room holds at most %d plugins.", MaxRoomPlugins), MaxRoomPlugins)
	big, err := pluginpkg.FromTree([]pluginpkg.Entry{{Path: pluginpkg.ManifestPath, Data: []byte(`{"name":"babs"}`)},
		{Path: "blob.bin", Data: make([]byte, 3<<19)}}, "", nil, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	capped := setup(4 << 20)
	capped.Files = map[string][]byte{sm.Plugins[0].File: big.Tar}
	if err := s.SetInstanceSettings(ctx, InstanceSettings{RoomsPerProfile: 10, RoomCreation: CreationAnyone, PluginMaxMB: 1}); err != nil {
		t.Fatal(err)
	}
	left("capped", capped, "The room starts without the starter's plugin babs: too big: the plugin is bigger than 1 MB.", 0)
	if err := s.SetInstanceSettings(ctx, InstanceSettings{RoomsPerProfile: 10, RoomCreation: CreationAnyone}); err != nil {
		t.Fatal(err)
	}
	for _, room := range []string{"held", "free"} {
		if _, err := s.CreateProfileRoom(ctx, room, "R", alice.ProfileID, "Ali", "inv-"+room, "tok-"+room, "lnk-"+room, "lnkh-"+room, setup(1<<20)); err != nil {
			t.Fatal(err)
		}
		ps, _ := s.RoomPlugins(ctx, room)
		cps, _ := s.RoomPersonas(ctx, room)
		if len(ps) != 1 || ps[0].Name != "babs" || ps[0].Ref != "main" || len(ps[0].Versions) != 1 || ps[0].Versions[0].Commit != vs[2].Commit ||
			ps[0].Versions[0].SHA256 != vs[2].SHA256 || ps[0].Versions[0].State != PluginInspecting || len(ps[0].Personas) != 1 || ps[0].Personas[0] != cps[0].ID {
			t.Fatalf("%s: plugins %+v", room, ps)
		}
		host, _ := s.ProfileParticipant(ctx, "sess", room)
		d, err := s.PairDaemon(ctx, room, host.ID, "dmn-"+room)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetDaemonSettings(ctx, d.ID, DaemonSettings{ApproveAll: room == "held"}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetHost(ctx, room, host.ID); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"held": PluginPending, "free": PluginApproved}[room]
		if _, state, err := s.ReportPluginVersion(ctx, room, ps[0].Versions[0].ID, d.ID, true, nil, ""); err != nil || state != want {
			t.Fatalf("%s: inspected %s %v, want %s", room, state, err, want)
		}
	}
	// 006-AC7: the built-ins carry no plugin.
	for _, b := range builtinStarters {
		if len(b.m.Plugins) != 0 {
			t.Fatalf("built-in %s carries plugins", b.key)
		}
	}
}
