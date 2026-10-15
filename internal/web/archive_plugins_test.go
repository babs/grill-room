package web

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/archive"
	"github.com/babs/grill-room/internal/store"
)

// archiveFiles reads a tar.gz into its entries by name.
func archiveFiles(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name], _ = io.ReadAll(tr)
	}
}

// 009-AC26 009-AC27, Phase 4 DoD: a room with an approved public plugin on Grill and a private one
// fetched by its AI host exports as format 3, its Export tab naming the private plugin. Another
// instance imports it: both plugins pinned at the same commits and hashes, on the same persona,
// and none runs: inspected by the new host, an imported version waits for its approval although
// that host approves without asking. Update on the imported public plugin reaches its repo again.
func TestPluginsAcrossInstances(t *testing.T) {
	gs := newGitServer(t)
	e := newEnvOpts(t, pluginOpts(gs))
	r := &turnRoom{room: e.createRoom(t, "Grill").ID}
	r.alice = e.daemonFor(t, r.room, "Alice")
	e.connect(t, r.room, r.alice, true)
	e.appoint(t, r.room, r.alice.p.Participant.ID)
	tok, base := r.alice.p.Token, "/api/rooms/"+r.room+"/plugins"
	grill := e.agent(t, r.room, tok).Personas[0].ID
	c1, url := marketRepo(t, gs).commit("skills.git")
	if code, b := e.do(t, "POST", base, tok, map[string]any{"url": url, "entries": []string{"babs"}, "personas": []int64{grill}}); code != http.StatusCreated {
		t.Fatalf("add: %d %s", code, b)
	}
	nextEvent(t, r.alice.events, "inspect")
	pub := e.plugins(t, r.room, tok)[0]
	if code, b := e.do(t, "POST", r.daemonPath(fmt.Sprintf("plugins/%d/report", pub.Versions[0].ID)), r.alice.tok, pluginReport{OK: true}); code != http.StatusOK {
		t.Fatalf("report: %d %s", code, b)
	}
	code, b := e.do(t, "POST", base+"/host-fetch", tok, map[string]any{"url": "git@example.com:team/priv.git"})
	if code != http.StatusAccepted {
		t.Fatalf("ask the host: %d %s", code, b)
	}
	f := decodeJSON[store.PluginFetch](t, b)
	priv := snapshotTar(t, "priv", "")
	if code, b := e.uploadFetch(t, r, r.alice, f.ID, strings.Repeat("9", 40), map[string][]byte{".": priv}); code != http.StatusCreated {
		t.Fatalf("host upload: %d %s", code, b)
	}
	src := e.plugins(t, r.room, tok)

	code, b = e.do(t, "GET", "/api/rooms/"+r.room+"/overview", tok, nil)
	if code != http.StatusOK || !strings.Contains(string(b), `"private_plugins":["priv"]`) {
		t.Fatalf("overview: %d %s", code, b)
	}
	code, ar := e.do(t, "GET", "/api/rooms/"+r.room+"/export", tok, nil)
	if code != http.StatusOK {
		t.Fatalf("export: %d", code)
	}
	files := archiveFiles(t, ar)
	for _, p := range src {
		v := p.Versions[0]
		if !bytes.Contains(files["plugins.jsonl"], []byte(`"name":"`+p.Name+`"`)) || len(files[fmt.Sprintf("plugins/%d/%s.tar", p.ID, v.SHA256)]) == 0 {
			t.Fatalf("archive lacks %s: %v", p.Name, files["plugins.jsonl"])
		}
	}
	if m, err := archive.Unpack(bytes.NewReader(ar), t.TempDir(), archive.Limits{MaxBytes: 1 << 24, MaxFiles: 100}); err != nil || m.Format != 3 {
		t.Fatalf("manifest: %+v %v", m, err)
	}

	e2 := newEnvOpts(t, pluginOpts(gs))
	code, b = e2.send(t, nil, "/api/rooms/import", e2.admin, ar, "application/gzip")
	got := decodeJSON[importResp](t, b)
	if code != http.StatusCreated || got.SameInstance || got.Counts["plugins"] != 2 || got.Counts["plugin_versions"] != 2 {
		t.Fatalf("import: %d %s", code, b)
	}
	room := got.Room.ID
	bea := e2.join(t, e2.createInvite(t, room).Token, "Bea")
	e2.appoint(t, room, bea.Participant.ID)
	host := &turnRoom{room: room, alice: e2.daemonFor(t, room, "Hana")}
	e2.connect(t, room, host.alice, true)
	copied := e2.plugins(t, room, bea.Token)
	personas := e2.agent(t, room, bea.Token).Personas
	if len(copied) != 2 || len(personas) != 1 {
		t.Fatalf("copy: %+v", copied)
	}
	for i, p := range copied {
		v, w := p.Versions[0], src[i].Versions[0]
		if p.Name != src[i].Name || p.Private != src[i].Private || p.URL != src[i].URL || v.Commit != w.Commit || v.SHA256 != w.SHA256 ||
			v.State != store.PluginInspecting || p.Running != 0 {
			t.Fatalf("copy of %s: %+v", src[i].Name, p)
		}
	}
	if copied[0].Name != "babs" || len(copied[0].Personas) != 1 || copied[0].Personas[0] != personas[0].ID || len(copied[1].Personas) != 0 {
		t.Fatalf("activations: %+v", copied)
	}
	nextEvent(t, host.alice.events, "inspect")
	code, b = e2.do(t, "POST", host.daemonPath(fmt.Sprintf("plugins/%d/report", copied[0].Versions[0].ID)), host.alice.tok, pluginReport{OK: true})
	if code != http.StatusOK || !strings.Contains(string(b), store.PluginPending) {
		t.Fatalf("imported version inspected: %d %s", code, b)
	}
	// An imported version's commit is the archive's word: no link vouches for it, and Update fetches
	// the repo's files even at the same commit; once they are the repo's, it is up to date.
	if v := copied[0].Versions[0]; !v.Imported || src[0].Versions[0].Imported {
		t.Fatalf("imported version view: %+v", v)
	}
	update := fmt.Sprintf("/api/rooms/%s/plugins/%d/update", room, copied[0].ID)
	code, b = e2.do(t, "POST", update, bea.Token, nil)
	pv := decodeJSON[pluginView](t, b)
	if code != http.StatusCreated || len(pv.Versions) != 2 || pv.Versions[1].Commit != c1 || pv.Versions[1].Imported {
		t.Fatalf("update of the imported plugin: %d %s", code, b)
	}
	if code, b := e2.do(t, "POST", update, bea.Token, nil); code != http.StatusOK || !strings.Contains(string(b), `"up_to_date":true`) {
		t.Fatalf("second update: %d %s", code, b)
	}
}

// 009-AC28: "Save as starter" lists the room's plugins at their running version, unchecked; a
// plugin left out leaves its personas too. A room created from the starter gets the kept plugin on
// the same persona, waiting for its AI host. Past the storage quota the save is refused (006-AC4).
func TestStarterPluginsAPI(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Pricing", "Alice")
	ctx := t.Context()
	ps, _ := e.st.Participants(ctx, room.ID)
	personas, _ := e.st.RoomPersonas(ctx, room.ID)
	tar := snapshotTar(t, "babs", "")
	sum := sha256.Sum256(tar)
	snap := store.PluginSnapshot{Commit: strings.Repeat("1", 40), SHA256: hex.EncodeToString(sum[:]), Size: 1, Files: 2, FileList: []byte(`[]`), Manifest: []byte(`{}`), Data: tar}
	if _, err := e.st.AddPlugins(ctx, room.ID, []store.NewPlugin{{Name: "babs", URL: "https://example.com/babs.git", RefName: "main", PluginSnapshot: snap}},
		[]int64{personas[0].ID}, &ps[0], 1<<20); err != nil {
		t.Fatal(err)
	}
	r := &turnRoom{room: room.ID, alice: e.daemonFor(t, room.ID, "Hana")}
	e.connect(t, room.ID, r.alice, true)
	nextEvent(t, r.alice.events, "inspect")
	pl, _ := e.st.RoomPlugins(ctx, room.ID)
	if code, b := e.do(t, "POST", r.daemonPath(fmt.Sprintf("plugins/%d/report", pl[0].Versions[0].ID)), r.alice.tok, pluginReport{OK: true}); code != http.StatusOK {
		t.Fatalf("report: %d %s", code, b)
	}

	code, b := alice.do(t, "GET", "/api/rooms/"+room.ID+"/starter-draft", nil)
	draft := decodeJSON[starterResp](t, b)
	if code != http.StatusOK || len(draft.Manifest.Plugins) != 1 || draft.Manifest.Plugins[0].SHA256 != snap.SHA256 ||
		strings.Join(draft.Manifest.Personas[0].Plugins, ",") != "babs" {
		t.Fatalf("draft: %d %s", code, b)
	}
	save := func(plugins []string) (int, []byte) {
		return alice.do(t, "POST", "/api/rooms/"+room.ID+"/starters", map[string]any{"title": "With plugins", "room_name": "Pricing",
			"personas": draft.Manifest.Personas, "plugins": plugins})
	}
	code, b = save([]string{})
	none := decodeJSON[store.Starter](t, b)
	if code != http.StatusCreated {
		t.Fatalf("save without plugins: %d %s", code, b)
	}
	if _, m, _ := e.st.StarterVersion(ctx, none.ID, 0, ps[0].ProfileID); len(m.Plugins) != 0 || len(m.Personas[0].Plugins) != 0 {
		t.Fatalf("a plugin left out stays: %+v", m)
	}
	used, _ := e.st.UsedBytes(ctx)
	if err := putSettings(ctx, e.st, store.InstanceSettings{RoomsPerProfile: 10, RoomCreation: store.CreationAnyone, StorageQuota: used + 512}); err != nil {
		t.Fatal(err)
	}
	if code, b := save([]string{"babs"}); code != http.StatusConflict || !strings.Contains(string(b), "storage quota") {
		t.Fatalf("past the quota: %d %s", code, b)
	}
	if err := putSettings(ctx, e.st, store.InstanceSettings{RoomsPerProfile: 10, RoomCreation: store.CreationAnyone}); err != nil {
		t.Fatal(err)
	}
	code, b = save([]string{"babs"})
	st := decodeJSON[store.Starter](t, b)
	if code != http.StatusCreated {
		t.Fatalf("save: %d %s", code, b)
	}
	code, b = alice.do(t, "POST", "/api/profile/rooms", map[string]any{"name": "Next", "display_name": "Alice",
		"starter": map[string]any{"id": st.ID, "version": st.Version}})
	if code != http.StatusCreated {
		t.Fatalf("create from the starter: %d %s", code, b)
	}
	next := decodeJSON[store.Room](t, b).ID
	got, _ := e.st.RoomPlugins(ctx, next)
	np, _ := e.st.RoomPersonas(ctx, next)
	if len(got) != 1 || got[0].Versions[0].SHA256 != snap.SHA256 || got[0].Versions[0].State != store.PluginInspecting || len(got[0].Personas) != 1 ||
		got[0].Personas[0] != np[0].ID {
		t.Fatalf("room from the starter: %+v", got)
	}
}

// 009-AC27: a github.com plugin's imported version links no commit and no compare view; one
// fetched from the repo after it links its commit, still no compare view against the import.
func TestViewPluginImported(t *testing.T) {
	c1, c2 := strings.Repeat("1", 40), strings.Repeat("2", 40)
	p := store.RoomPlugin{URL: "https://github.com/o/r", Versions: []store.PluginVersion{
		{N: 1, Commit: c1, State: store.PluginApproved, Imported: true}, {N: 2, Commit: c2, State: store.PluginPending}}}
	v := viewPlugin(p, true)
	if v.Versions[0].CommitURL != "" || v.Versions[1].CommitURL != "https://github.com/o/r/commit/"+c2 || v.Versions[1].CompareURL != "" || v.Versions[1].Change == nil {
		t.Fatalf("view %+v", v.Versions)
	}
	p.Versions[0].Imported = false
	if v := viewPlugin(p, true); v.Versions[0].CommitURL == "" || v.Versions[1].CompareURL == "" {
		t.Fatalf("not imported %+v", v.Versions)
	}
}

// putSettings stores v whole, as a PUT sending every field does.
func putSettings(ctx context.Context, st *store.Store, v store.InstanceSettings) error {
	_, err := st.UpdateInstanceSettings(ctx, func(p *store.InstanceSettings) error { *p = v; return nil })
	return err
}
