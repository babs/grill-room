package store

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pluginpkg "github.com/babs/grill-room/internal/plugin"
)

// realSnapshot is a plugin named name as the server stores it, its skill saying body.
func realSnapshot(t *testing.T, name, commit, body string) PluginSnapshot {
	t.Helper()
	sn, err := pluginpkg.FromTree([]pluginpkg.Entry{{Path: pluginpkg.ManifestPath, Data: []byte(`{"name":"` + name + `"}`)},
		{Path: "skills/hi/SKILL.md", Data: []byte("---\nname: hi\ndescription: Hi.\n---\n" + body + "\n")}}, "", nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	m, err := sn.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	files, _ := json.Marshal(sn.Files)
	manifest, _ := json.Marshal(m)
	return PluginSnapshot{Commit: commit, SHA256: sn.SHA256, Size: sn.Size, Files: len(sn.Files), FileList: files, Manifest: manifest, Data: sn.Tar}
}

func commitOf(c byte) string { return strings.Repeat(string(c), 40) }

// pluginRoom is room "a" with the plugin babs on Grill at v1 (superseded, its bytes dropped), v2
// approved and v3 invalid, and the private plugin priv still waiting for inspection.
func pluginRoom(t *testing.T, s *Store) (turnRoom, map[int]PluginSnapshot) {
	t.Helper()
	ctx := context.Background()
	r := newTurnRoom(t, s, "a")
	host := hostDaemon(t, s)
	vs := map[int]PluginSnapshot{1: realSnapshot(t, "babs", commitOf('1'), "one"), 2: realSnapshot(t, "babs", commitOf('2'), "two"),
		3: realSnapshot(t, "babs", commitOf('3'), "three")}
	if _, err := s.AddPlugins(ctx, "a", []NewPlugin{{Name: "babs", URL: "https://example.com/babs.git", Ref: "main", RefName: "main", PluginSnapshot: vs[1]}},
		[]int64{r.grill.ID}, r.p, 1<<20); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.RoomPlugins(ctx, "a")
	id := ps[0].ID
	report := func(n int, ok bool) {
		t.Helper()
		p := plugin(t, s, "a", id)
		if _, _, err := s.ReportPluginVersion(ctx, "a", p.Versions[n-1].ID, host, ok, json.RawMessage(`{"ok":true}`), "bad"); err != nil {
			t.Fatal(err)
		}
	}
	report(1, true)
	for n := 2; n <= 3; n++ {
		if _, err := s.AddPluginVersion(ctx, "a", id, "main", vs[n], r.p, 1<<20); err != nil {
			t.Fatal(err)
		}
		report(n, n == 2)
	}
	if _, err := s.AddPlugins(ctx, "a", []NewPlugin{{Name: "priv", URL: "git@example.com:team/priv.git", RefName: "main", Private: true,
		PluginSnapshot: realSnapshot(t, "priv", commitOf('9'), "private")}}, nil, r.p, 1<<20); err != nil {
		t.Fatal(err)
	}
	if got := versionStates(plugin(t, s, "a", id)); got != "v1:approved v2:approved v3:invalid" || dataLen(t, s, plugin(t, s, "a", id).Versions[0].ID) != 0 {
		t.Fatalf("source states %s", got)
	}
	return r, vs
}

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var row map[string]any
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	return out
}

// 009-AC26 009-AC27: an export carries each plugin with its source and ref, every version with
// its commit and report, the snapshots still held and the persona activations. Imported, every
// plugin is pinned at the same commits and hashes and nothing runs: the versions with their
// files wait for the new host's inspection, then for its approval even though it approves
// without asking; the one whose files stayed behind is a record that never loads.
func TestExportImportPlugins(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	_, vs := pluginRoom(t, s)
	dir := export(t, s, "a")

	rows := readLines(t, filepath.Join(dir, filePlugins))
	if len(rows) != 2 || rows[0]["name"] != "babs" || rows[0]["git_ref"] != "main" || rows[0]["url"] != "https://example.com/babs.git" ||
		rows[1]["private"] != true || rows[1]["created_by"] != "Alice" {
		t.Fatalf("plugins.jsonl: %v", rows)
	}
	vrows := readLines(t, filepath.Join(dir, filePluginVersions))
	var files []string
	for _, v := range vrows {
		f, _ := v["file"].(string)
		files = append(files, f)
	}
	if len(vrows) != 4 || files[0] != "" || vrows[0]["commit"] != commitOf('1') || vrows[1]["report"] == nil || vrows[2]["state"] != PluginInvalid {
		t.Fatalf("plugin_versions.jsonl: %v", vrows)
	}
	for i, n := range []int{2, 3} {
		b, err := os.ReadFile(filepath.Join(dir, files[i+1]))
		if err != nil || string(b) != string(vs[n].Data) || !strings.HasSuffix(files[i+1], vs[n].SHA256+".tar") {
			t.Fatalf("v%d snapshot %s: %v", n, files[i+1], err)
		}
	}
	room, _ := os.ReadFile(filepath.Join(dir, fileRoom))
	if !strings.Contains(string(room), `"plugins": [`) {
		t.Fatalf("room.json lacks the activations: %s", room)
	}

	o := importOpts("copy")
	o.SameInstance = false
	rep, err := s.ImportRoom(ctx, dir, o)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Counts["plugins"] != 2 || rep.Counts["plugin_versions"] != 4 || !strings.Contains(strings.Join(rep.Warnings, " "), "Plugins: 2.") {
		t.Fatalf("report %+v", rep)
	}
	ps, err := s.RoomPlugins(ctx, "copy")
	if err != nil || len(ps) != 2 {
		t.Fatalf("copy plugins %+v %v", ps, err)
	}
	babs, priv := ps[0], ps[1]
	cps, _ := s.RoomPersonas(ctx, "copy")
	if babs.Ref != "main" || babs.RefName != "main" || babs.Private || !priv.Private || priv.URL != "git@example.com:team/priv.git" ||
		len(babs.Personas) != 1 || babs.Personas[0] != cps[0].ID || len(priv.Personas) != 0 || babs.Running() != nil {
		t.Fatalf("copy plugins %+v", ps)
	}
	// The invalid v3 keeps its state and reason, without its files: it never loads.
	if got := versionStates(&babs); got != "v1:refused v2:inspecting v3:invalid" || babs.Versions[2].Reason != "refused by Claude Code: bad" ||
		dataLen(t, s, babs.Versions[2].ID) != 0 || !babs.Versions[2].Imported || !babs.Versions[1].Imported || !babs.Versions[0].Imported {
		t.Fatalf("copy states %s", got)
	}
	for i, v := range babs.Versions {
		n := i + 1
		if v.Commit != vs[n].Commit || v.SHA256 != vs[n].SHA256 || v.DecidedBy != "" && n > 1 {
			t.Errorf("v%d: %+v", n, v)
		}
	}
	if v1 := babs.Versions[0]; v1.Reason != supersededReason || dataLen(t, s, v1.ID) != 0 || len(v1.Report) == 0 {
		t.Fatalf("v1 %+v", v1)
	}
	if babs.Versions[1].Report != nil || dataLen(t, s, babs.Versions[1].ID) != len(vs[2].Data) {
		t.Fatalf("v2 keeps the exporter's report, or lost its bytes: %+v", babs.Versions[1])
	}

	// The new host approves without asking, yet an imported version waits for it once checked.
	if _, err := s.CreateInvite(ctx, "copy", "inv-copy", 0); err != nil {
		t.Fatal(err)
	}
	hp, _, err := s.Join(ctx, "inv-copy", "Hana", "tok-copy", JoinProfile{})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.PairDaemon(ctx, "copy", hp.ID, "dmn-copy")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetHost(ctx, "copy", hp.ID); err != nil {
		t.Fatal(err)
	}
	if ids, _ := s.InspectingVersions(ctx, "copy"); len(ids) != 2 {
		t.Fatalf("waiting for inspection: %v", ids)
	}
	if _, state, err := s.ReportPluginVersion(ctx, "copy", babs.Versions[1].ID, d.ID, true, nil, ""); err != nil || state != PluginPending {
		t.Fatalf("imported v2 inspected: %s %v", state, err)
	}
	if _, err := s.DecidePluginVersion(ctx, "copy", babs.ID, 1, true, hp); !errors.Is(err, ErrPluginState) {
		t.Fatalf("approving the version left behind: %v", err)
	}
	if _, err := s.DecidePluginVersion(ctx, "copy", babs.ID, 2, true, hp); err != nil {
		t.Fatal(err)
	}
	if p := plugin(t, s, "copy", babs.ID); p.Running() == nil || p.Running().N != 2 {
		t.Fatalf("after approval: %s", versionStates(p))
	}
	// Saved as a starter, the approved import stays the archive's word in the room made from it:
	// held, and Update takes the same commit from the repo.
	prof := joinWithProfile(t, s, "lobby", "Pat", JoinProfile{RecoveryHash: "rec-p", SessionHash: "sess-p"})
	m, err := s.RoomDraft(ctx, "copy")
	if err != nil || len(m.Plugins) != 1 || !m.Plugins[0].Imported {
		t.Fatalf("draft of the copy: %+v %v", m.Plugins, err)
	}
	blobs, _ := s.RoomDraftFiles(ctx, "copy", []string{m.Plugins[0].File})
	st, err := s.SaveStarter(ctx, prof.ProfileID, 0, m, blobs)
	if err != nil {
		t.Fatal(err)
	}
	_, sm, _ := s.StarterVersion(ctx, st.ID, 0, prof.ProfileID)
	sf, _ := s.StarterFiles(ctx, st.ID, st.Version)
	if _, err := s.CreateProfileRoom(ctx, "third", "R", prof.ProfileID, "Pat", "inv-3", "tok-3", "lnk-3", "lnkh-3", &RoomSetup{From: &Provenance{Title: sm.Title, Version: 1},
		Personas: sm.Personas, Plugins: sm.Plugins, Files: sf, Chain: sm.Chain, UploadQuota: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	third, _ := s.RoomPlugins(ctx, "third")
	if len(third) != 1 || !third[0].Versions[0].Imported || third[0].Versions[0].Commit != vs[2].Commit {
		t.Fatalf("room from the starter: %+v", third)
	}
	if _, err := s.AddPluginVersion(ctx, "third", third[0].ID, "main", vs[2], prof, 1<<20); err != nil {
		t.Fatalf("update at the same commit: %v", err)
	}
	if p := plugin(t, s, "third", third[0].ID); p.Versions[1].Imported {
		t.Fatalf("the repo's version is held: %s", versionStates(p))
	}
	if _, err := s.AddPluginVersion(ctx, "third", third[0].ID, "main", vs[2], prof, 1<<20); !errors.Is(err, ErrUpToDate) {
		t.Fatalf("second update: %v", err)
	}
	// A plugin a room admin adds here is not held: the hold is the import's alone.
	if _, err := s.AddPluginVersion(ctx, "copy", priv.ID, "main", realSnapshot(t, "priv", commitOf('8'), "newer"), hp, 1<<20); err != nil {
		t.Fatal(err)
	}
	if p := plugin(t, s, "copy", priv.ID); p.Versions[1].State != PluginInspecting {
		t.Fatalf("priv v2 %s", versionStates(p))
	}
	if _, state, err := s.ReportPluginVersion(ctx, "copy", plugin(t, s, "copy", priv.ID).Versions[1].ID, d.ID, true, nil, ""); err != nil || state != PluginApproved {
		t.Fatalf("a version added here: %s %v", state, err)
	}
}

// 009-AC26: a format 2 archive has no plugin files and imports without plugins; a format 3 one
// missing them is refused.
func TestImportFormat2WithoutPlugins(t *testing.T) {
	s := openTest(t)
	pluginRoom(t, s)
	dir := export(t, s, "a")
	for _, f := range []string{filePlugins, filePluginVersions} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	// A format 2 room.json names no plugin on its personas.
	b, _ := os.ReadFile(filepath.Join(dir, fileRoom))
	var x map[string]any
	_ = json.Unmarshal(b, &x)
	for _, p := range x["personas"].([]any) {
		delete(p.(map[string]any), "plugins")
	}
	b, _ = json.Marshal(x)
	if err := os.WriteFile(filepath.Join(dir, fileRoom), b, 0o600); err != nil {
		t.Fatal(err)
	}
	o := importOpts("old")
	o.Format = 2
	if rep, err := s.ImportRoom(t.Context(), dir, o); err != nil || rep.Counts["plugins"] != 0 {
		t.Fatalf("format 2: %+v %v", rep, err)
	}
	if _, err := s.ImportRoom(t.Context(), dir, importOpts("new")); err == nil || !strings.Contains(err.Error(), filePlugins+" is missing") {
		t.Fatalf("format 3 without plugins: %v", err)
	}
}

// 009-AC27: an import re-reads every plugin it carries, as a daemon's upload is read; any one bad
// refuses the whole import.
func TestImportPluginRefusals(t *testing.T) {
	s := openTest(t)
	_, vs := pluginRoom(t, s)
	good := export(t, s, "a")
	cases := map[string]struct {
		file string
		edit func(rows []map[string]any)
		want string
	}{
		"credentials in the link": {filePlugins, func(r []map[string]any) { r[0]["url"] = "https://u:p@example.com/x.git" }, "Never put credentials"},
		"private file link":       {filePlugins, func(r []map[string]any) { r[1]["url"] = "file:///etc" }, "its link"},
		"grill-room's name":       {filePlugins, func(r []map[string]any) { r[0]["name"] = "grill-room" }, "grill-room's own"},
		"ref as an option":        {filePlugins, func(r []map[string]any) { r[0]["git_ref"] = "--upload-pack=x" }, "not a valid branch"},
		"bad commit":              {filePluginVersions, func(r []map[string]any) { r[1]["commit"] = "HEAD" }, "is not a commit"},
		"other hash":              {filePluginVersions, func(r []map[string]any) { r[1]["sha256"] = vs[3].SHA256 }, "do not match its sha256"},
		"version twice":           {filePluginVersions, func(r []map[string]any) { r[2]["n"] = 2 }, "a version twice"},
		"unknown plugin":          {filePluginVersions, func(r []map[string]any) { r[0]["plugin"] = 999 }, "version of unknown plugin"},
		"unknown state":           {filePluginVersions, func(r []map[string]any) { r[0]["state"] = "running" }, "unknown state"},
		"two plugins, one name":   {filePlugins, func(r []map[string]any) { r[1]["name"] = "babs" }, "UNIQUE"},
		"byte-less bad sha256":    {filePluginVersions, func(r []map[string]any) { r[0]["sha256"] = "x" }, "bad sha256"},
		"byte-less negative size": {filePluginVersions, func(r []map[string]any) { r[0]["size"] = -1 }, "bad sha256, size"},
		"refused, bad files":      {filePluginVersions, func(r []map[string]any) { r[2]["files"] = -3 }, "bad sha256, size"},
		"tar out of form":         {filePluginVersions, func(r []map[string]any) { r[1]["file"] = r[2]["file"] }, "do not match its sha256"},
	}
	for name, c := range cases {
		dir := t.TempDir()
		if err := os.CopyFS(dir, os.DirFS(good)); err != nil {
			t.Fatal(err)
		}
		rows := readLines(t, filepath.Join(dir, c.file))
		c.edit(rows)
		var b strings.Builder
		for _, r := range rows {
			line, _ := json.Marshal(r)
			b.Write(append(line, '\n'))
		}
		if err := os.WriteFile(filepath.Join(dir, c.file), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := s.ImportRoom(t.Context(), dir, importOpts("bad"))
		if !errors.Is(err, ErrImportRefused) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := s.Room(t.Context(), "bad"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: room left behind", name)
		}
	}
	// A persona naming a plugin the archive does not hold.
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(good)); err != nil {
		t.Fatal(err)
	}
	room, _ := os.ReadFile(filepath.Join(dir, fileRoom))
	var x map[string]any
	_ = json.Unmarshal(room, &x)
	x["personas"].([]any)[0].(map[string]any)["plugins"] = []any{999}
	room, _ = json.Marshal(x)
	if err := os.WriteFile(filepath.Join(dir, fileRoom), room, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportRoom(t.Context(), dir, importOpts("bad")); !errors.Is(err, ErrImportRefused) || !strings.Contains(err.Error(), "plugin 999 on") {
		t.Errorf("a missing plugin ref: %v", err)
	}
	// A tar that is not a snapshot, and a snapshot past this instance's cap.
	broken := func(data []byte, sha string, want string) {
		t.Helper()
		dir := t.TempDir()
		if err := os.CopyFS(dir, os.DirFS(good)); err != nil {
			t.Fatal(err)
		}
		rows := readLines(t, filepath.Join(dir, filePluginVersions))
		f, _ := rows[1]["file"].(string)
		rows[1]["sha256"] = sha
		var b strings.Builder
		for _, r := range rows {
			line, _ := json.Marshal(r)
			b.Write(append(line, '\n'))
		}
		if err := os.WriteFile(filepath.Join(dir, filePluginVersions), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ImportRoom(t.Context(), dir, importOpts("bad")); !errors.Is(err, ErrImportRefused) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
	broken(append([]byte("x"), vs[2].Data...), vs[2].SHA256, "invalid plugin")
	big, err := pluginpkg.FromTree([]pluginpkg.Entry{{Path: pluginpkg.ManifestPath, Data: []byte(`{"name":"babs"}`)},
		{Path: "blob.bin", Data: make([]byte, 3<<19)}}, "", nil, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := putSettings(t.Context(), s, InstanceSettings{RoomsPerProfile: 10, RoomCreation: CreationAnyone, PluginMaxMB: 1}); err != nil {
		t.Fatal(err)
	}
	broken(big.Tar, big.SHA256, "bigger than 1 MB")
}
