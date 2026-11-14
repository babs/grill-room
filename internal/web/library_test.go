package web

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/voices"
)

type libraryPersonaResp struct {
	store.Template
	Versions []store.TemplateVersionInfo `json:"versions"`
	Voices   []voices.Voice              `json:"voices"`
	TTS      bool                        `json:"tts"`
}

// seated returns a device whose profile, named name, has a seat in a new room, with the room's id.
func (e *env) seated(t *testing.T, name string) (*device, string) {
	t.Helper()
	d := e.device(t)
	room := e.createRoom(t, "Room of "+name).ID
	d.join(t, room, name)
	return d, room
}

// personas returns the library personas d's profile sees, by id.
func (d *device) personas(t *testing.T) map[int64]store.Template {
	t.Helper()
	code, b := d.do(t, "GET", "/api/profile/personas", nil)
	if code != http.StatusOK {
		t.Fatalf("personas: %d %s", code, b)
	}
	out := map[int64]store.Template{}
	for _, tp := range decodeJSON[[]store.Template](t, b) {
		out[tp.ID] = tp
	}
	return out
}

// 035-AC1 035-AC2 035-AC3 035-AC4 035-AC5 035-AC6 035-AC15 035-AC19: the profile's persona library
// through the API. Alice saves a room persona, reads it with its history, edits it to v2 (her room
// copy is only offered the update; a second edit on v1 is refused; the history lists v2 first),
// duplicates it, shares it (sharing twice or unsharing a personal one says where it is); Bob
// sees it, adds it whole to his room, from his browser or with an API token, then loses it and its
// update when she unshares. Owners delete their own, the admin a shared
// one only; rooms keep their copies.
func TestProfilePersonaLibraryAPI(t *testing.T) {
	e := newEnv(t)
	if code, _ := e.device(t).do(t, "GET", "/api/profile/personas", nil); code != http.StatusUnauthorized {
		t.Fatalf("without a profile: %d", code)
	}
	alice, aroom := e.seated(t, "Alice")
	bob, broom := e.seated(t, "Bob")
	ps, err := e.st.RoomPersonas(t.Context(), aroom)
	if err != nil || len(ps) != 1 {
		t.Fatalf("room personas: %+v %v", ps, err)
	}
	grill := ps[0]
	code, b := alice.do(t, "POST", fmt.Sprintf("/api/rooms/%s/personas/%d/library", aroom, grill.ID), nil)
	if code != http.StatusCreated {
		t.Fatalf("save: %d %s", code, b)
	}
	tp := decodeJSON[store.Template](t, b)
	path := fmt.Sprintf("/api/profile/personas/%d", tp.ID)

	// 035-AC1: the list, with its sections; Bob does not see Alice's own.
	mine := alice.personas(t)
	if got := mine[tp.ID]; got.Section != store.SectionPersonal || !got.Mine || got.Author != "Alice" || got.Version != 1 || got.Title != "Grill" {
		t.Fatalf("alice's list: %+v", mine)
	}
	theirs := bob.personas(t)
	if _, ok := theirs[tp.ID]; ok || len(theirs) != 4 {
		t.Fatalf("bob's list: %+v", theirs)
	}
	var builtin int64
	for id, x := range theirs {
		if x.Section != store.SectionBuiltin || x.Mine {
			t.Fatalf("bob's list: %+v", theirs)
		}
		builtin = id
	}
	// 035-AC2: one persona, its settings and history; another profile's private one is not found.
	code, b = alice.do(t, "GET", path, nil)
	if got := decodeJSON[libraryPersonaResp](t, b); code != http.StatusOK || got.Text != grill.Text || len(got.Versions) != 1 ||
		got.Versions[0].N != 1 || got.Versions[0].Author != "Alice" || !strings.Contains(string(b), `"models":[`) {
		t.Fatalf("read: %d %s", code, b)
	}
	// 035-AC3: the editor's voices, as the room's picker lists them, and whether personas speak here.
	if got := decodeJSON[libraryPersonaResp](t, b); len(got.Voices) != len(voices.All()) || got.Voices[0].ID == "" || got.Voices[0].Name == "" ||
		got.Voices[0].Lang == "" || got.TTS {
		t.Fatalf("read, voices and tts off: %+v %v", got.Voices, got.TTS)
	}
	if code, _ := bob.do(t, "GET", path, nil); code != http.StatusNotFound {
		t.Fatalf("bob reads it: %d", code)
	}

	// 035-AC3: the author's edit, checked as a room persona change.
	edit := map[string]any{"name": "Grill", "badge": "🔥", "colour": "#123456", "text": "Edited.", "model": "opus", "effort": "high",
		"every_minutes": 5, "speaks": false, "voice_rate": 1.2, "description": "Challenges the plan.", "version": 1}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for key, val := range edit {
			m[key] = val
		}
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	for _, c := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"missing name", with("name", nil), "name must be"},
		{"missing version", with("version", nil), "version, the version the editor opened, is required"},
		{"missing model", with("model", nil), "model must be"},
		{"two triggers", with("every_n", 3), "set one of"},
		{"long description", with("description", strings.Repeat("d", 201)), "description must be"},
		{"two-line description", with("description", "a\nb"), "description must be"},
	} {
		if code, b := alice.do(t, "POST", path+"/versions", c.body); code != http.StatusBadRequest || !strings.Contains(string(b), c.want) {
			t.Errorf("%s: %d %s", c.name, code, b)
		}
	}
	if code, _ := bob.do(t, "POST", path+"/versions", edit); code != http.StatusNotFound {
		t.Fatalf("bob edits alice's private one: %d", code)
	}
	if code, b := alice.do(t, "POST", fmt.Sprintf("/api/profile/personas/%d/versions", builtin), edit); code != http.StatusForbidden || !strings.Contains(string(b), "read-only") {
		t.Fatalf("edit a built-in: %d %s", code, b)
	}
	code, b = alice.do(t, "POST", path+"/versions", edit)
	if v2 := decodeJSON[store.Template](t, b); code != http.StatusCreated || v2.Version != 2 || !v2.Mine || v2.Text != "Edited." || v2.Description != "Challenges the plan." ||
		v2.EveryMinutes != 5 || v2.SpeaksInCall() || v2.VoiceRate != 1.2 {
		t.Fatalf("edit: %d %s", code, b)
	}
	if p, err := e.st.RoomPersona(t.Context(), aroom, grill.ID); err != nil || p.Text != grill.Text || p.TemplateVersion != 1 || p.UpdateVersion != 2 {
		t.Fatalf("alice's room copy: %+v %v", p, err)
	}
	if code, b := alice.do(t, "POST", path+"/versions", edit); code != http.StatusConflict || !strings.Contains(string(b), "version 2 exists: reopen the persona to edit it") {
		t.Fatalf("a second edit on v1: %d %s", code, b)
	}
	// 035-AC2: the history lists v2 first, each version with its author.
	code, b = alice.do(t, "GET", path, nil)
	if got := decodeJSON[libraryPersonaResp](t, b); code != http.StatusOK || len(got.Versions) != 2 || got.Versions[0].N != 2 || got.Versions[1].N != 1 ||
		got.Versions[0].Author != "Alice" || got.Versions[1].Author != "Alice" || got.Versions[0].CreatedAt.Before(got.Versions[1].CreatedAt) {
		t.Fatalf("history after v2: %d %s", code, b)
	}

	// 035-AC4: duplicates.
	code, b = alice.do(t, "POST", path+"/duplicate", nil)
	cp := decodeJSON[store.Template](t, b)
	if code != http.StatusCreated || cp.Title != "Grill (copy)" || cp.Version != 1 || !cp.Mine || cp.Text != "Edited." || cp.Description != "Challenges the plan." {
		t.Fatalf("duplicate: %d %s", code, b)
	}
	if code, _ := bob.do(t, "POST", path+"/duplicate", nil); code != http.StatusNotFound {
		t.Fatalf("bob copies alice's private one: %d", code)
	}

	// 035-AC5: share, add from another profile, unshare.
	if code, _ := bob.do(t, "POST", path+"/share", nil); code != http.StatusNotFound {
		t.Fatalf("bob shares it: %d", code)
	}
	if code, _ := alice.do(t, "POST", path+"/share", nil, "Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
		t.Fatalf("cross-site share: %d", code)
	}
	if code, b := alice.do(t, "POST", path+"/unshare", nil); code != http.StatusForbidden || !strings.Contains(string(b), "Grill is not shared") {
		t.Fatalf("unshare a personal one: %d %s", code, b)
	}
	if code, b := alice.do(t, "POST", path+"/share", nil); code != http.StatusNoContent {
		t.Fatalf("share: %d %s", code, b)
	}
	if code, b := alice.do(t, "POST", path+"/share", nil); code != http.StatusForbidden || !strings.Contains(string(b), "Grill is already shared") {
		t.Fatalf("share twice: %d %s", code, b)
	}
	if got := bob.personas(t)[tp.ID]; got.Section != store.SectionInstance || got.Mine || got.Author != "Alice" {
		t.Fatalf("bob sees the shared one: %+v", got)
	}
	add := func(d *device, id int64, room string) (int, []byte) {
		return d.do(t, "POST", fmt.Sprintf("/api/profile/personas/%d/rooms", id), map[string]string{"room": room})
	}
	if code, _ := add(bob, tp.ID, aroom); code != http.StatusNotFound {
		t.Fatalf("bob adds to a room without his seat: %d", code)
	}
	code, b = add(bob, tp.ID, broom)
	added := decodeJSON[store.RoomPersona](t, b)
	if code != http.StatusCreated || added.TemplateID != tp.ID || added.TemplateVersion != 2 || added.Text != "Edited." || added.Model != "opus" ||
		added.EveryMinutes != 5 || added.Speaks || added.VoiceRate != 1.2 {
		t.Fatalf("add to a room: %d %s", code, b)
	}
	if m := e.lastLine(t, broom); m.Kind != store.KindSystem || !strings.HasPrefix(m.Body, "Bob added the persona 🔥 Grill-2") {
		t.Fatalf("add line: %+v", m)
	}
	// The same seat rules as the room routes for an API token: its profile's seat, else 403.
	tok := bob.newToken(t, "full", 30).Token
	addPath := fmt.Sprintf("/api/profile/personas/%d/rooms", builtin)
	if code, b := e.do(t, "POST", addPath, tok, map[string]string{"room": broom}); code != http.StatusCreated {
		t.Fatalf("add with a full API token: %d %s", code, b)
	}
	if code, b := e.do(t, "POST", addPath, tok, map[string]string{"room": aroom}); code != http.StatusForbidden || !strings.Contains(string(b), msgTokenNotHere) {
		t.Fatalf("add with an API token to a room without its seat: %d %s", code, b)
	}
	if code, _ := alice.do(t, "POST", path+"/versions", with("version", 2)); code != http.StatusCreated {
		t.Fatalf("edit to v3: %d", code)
	}
	if code, _ := alice.do(t, "POST", path+"/unshare", nil); code != http.StatusNoContent {
		t.Fatalf("unshare: %d", code)
	}
	if _, ok := bob.personas(t)[tp.ID]; ok {
		t.Fatal("bob still sees the unshared one")
	}
	if code, _ := bob.do(t, "POST", fmt.Sprintf("/api/rooms/%s/personas/%d/update", broom, added.ID), nil); code != http.StatusNotFound {
		t.Fatalf("bob updates from the unshared one: %d", code)
	}
	if code, _ := add(bob, tp.ID, broom); code != http.StatusNotFound {
		t.Fatalf("bob adds the unshared one: %d", code)
	}
	// 035-AC15: a room holds 12 personas.
	for i := 3; i < store.MaxRoomPersonas; i++ {
		if code, b := add(bob, builtin, broom); code != http.StatusCreated {
			t.Fatalf("add #%d: %d %s", i+1, code, b)
		}
	}
	if code, b := add(bob, builtin, broom); code != http.StatusConflict || !strings.Contains(string(b), "A room holds 12 personas at most.") {
		t.Fatalf("13th persona: %d %s", code, b)
	}

	// 035-AC6 035-AC19: deletion; rooms keep their copies.
	if code, _ := bob.do(t, "DELETE", path, nil); code != http.StatusNotFound {
		t.Fatalf("bob deletes alice's private one: %d", code)
	}
	adminPath := fmt.Sprintf("/api/admin/templates/%d", tp.ID)
	if code, _ := e.do(t, "DELETE", adminPath, e.admin, nil); code != http.StatusNotFound {
		t.Fatalf("admin removes a private one: %d", code)
	}
	if code, _ := alice.do(t, "POST", path+"/share", nil); code != http.StatusNoContent {
		t.Fatalf("share again: %d", code)
	}
	if code, _ := bob.do(t, "DELETE", path, nil); code != http.StatusForbidden {
		t.Fatalf("bob deletes the shared one: %d", code)
	}
	if code, _ := alice.do(t, "DELETE", fmt.Sprintf("/api/profile/personas/%d", builtin), nil); code != http.StatusForbidden {
		t.Fatalf("delete a built-in: %d", code)
	}
	if code, b := alice.do(t, "DELETE", path, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, b)
	}
	if p, err := e.st.RoomPersona(t.Context(), broom, added.ID); err != nil || p.TemplateID != 0 || p.TemplateVersion != 2 || p.Text != "Edited." || p.EveryMinutes != 5 {
		t.Fatalf("bob's copy after the delete: %+v %v", p, err)
	}
	code, b = e.do(t, "GET", "/api/admin/templates", e.admin, nil)
	if ls := decodeJSON[[]store.Template](t, b); code != http.StatusOK || slices.ContainsFunc(ls, func(x store.Template) bool { return x.Section == store.SectionPersonal }) {
		t.Fatalf("admin list: %d %s", code, b)
	}
	if code, _ := alice.do(t, "POST", fmt.Sprintf("/api/profile/personas/%d/share", cp.ID), nil); code != http.StatusNoContent {
		t.Fatalf("share the copy: %d", code)
	}
	if code, _ := e.do(t, "DELETE", fmt.Sprintf("/api/admin/templates/%d", cp.ID), e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("admin removes a shared one: %d", code)
	}
	var actions []string
	for _, a := range e.auditLog(t, "?target=Grill").Entries {
		actions = append(actions, a.Action+" "+strings.SplitN(a.Actor, ":", 2)[0])
	}
	want := []string{"template.remove super-admin", "template.share profile", "template.remove profile", "template.share profile", "template.unshare profile", "template.share profile"}
	if !slices.Equal(actions, want) {
		t.Fatalf("audit: %v, want %v", actions, want)
	}
}

// 035-AC3: on a server whose personas speak, a library persona's page says so, for the editor's call fields.
func TestProfilePersonaSpeech(t *testing.T) {
	e := newEnvOpts(t, ttsOptions(t))
	alice, _ := e.seated(t, "Alice")
	var id int64
	for k, x := range alice.personas(t) {
		if x.Section == store.SectionBuiltin {
			id = k
		}
	}
	code, b := alice.do(t, "GET", fmt.Sprintf("/api/profile/personas/%d", id), nil)
	if got := decodeJSON[libraryPersonaResp](t, b); code != http.StatusOK || !got.TTS || len(got.Voices) == 0 {
		t.Fatalf("read with speech: %d %s", code, b)
	}
}

// kitPersona saves Grill of a new room of d, with d's personal skill stamp and the plugin babs,
// approved by the room's AI host, on, to d's library personas, and shares it.
func (e *env) kitPersona(t *testing.T, d *device) int64 {
	t.Helper()
	ctx := t.Context()
	room := d.createRoom(t, "Kit source", "Kit").ID
	sk := d.uploadSkill(t, "personal", skillZip(t, "stamp", "STAMP"), http.StatusCreated)
	ps, err := e.st.RoomPersonas(ctx, room)
	if err != nil || len(ps) != 1 {
		t.Fatalf("room personas: %+v %v", ps, err)
	}
	if code, b := d.do(t, "PUT", fmt.Sprintf("/api/rooms/%s/personas/%d/skills/%d", room, ps[0].ID, sk.ID), map[string]bool{"accept_disclaimer": true}); code != http.StatusOK {
		t.Fatalf("skill on: %d %s", code, b)
	}
	seats, _ := e.st.Participants(ctx, room)
	tar := snapshotTar(t, "babs", "")
	sum := sha256.Sum256(tar)
	snap := store.PluginSnapshot{Commit: strings.Repeat("1", 40), SHA256: hex.EncodeToString(sum[:]), Size: 1, Files: 2, FileList: []byte(`[]`), Manifest: []byte(`{}`), Data: tar}
	if _, err := e.st.AddPlugins(ctx, room, []store.NewPlugin{{Name: "babs", URL: "https://example.com/babs.git", RefName: "main", PluginSnapshot: snap}},
		[]int64{ps[0].ID}, &seats[0], 1<<20); err != nil {
		t.Fatal(err)
	}
	r := &turnRoom{room: room, alice: e.daemonFor(t, room, "Hana")}
	e.connect(t, room, r.alice, true)
	t.Cleanup(r.alice.drop)
	nextEvent(t, r.alice.events, "inspect")
	pl, _ := e.st.RoomPlugins(ctx, room)
	if code, b := e.do(t, "POST", r.daemonPath(fmt.Sprintf("plugins/%d/report", pl[0].Versions[0].ID)), r.alice.tok, pluginReport{OK: true}); code != http.StatusOK {
		t.Fatalf("report: %d %s", code, b)
	}
	code, b := d.do(t, "POST", fmt.Sprintf("/api/rooms/%s/personas/%d/library", room, ps[0].ID), nil)
	tp := decodeJSON[store.Template](t, b)
	if code != http.StatusCreated || len(tp.FrozenSkills) != 1 || len(tp.FrozenPlugins) != 1 {
		t.Fatalf("save: %d %s", code, b)
	}
	if code, b := d.do(t, "POST", fmt.Sprintf("/api/profile/personas/%d/share", tp.ID), nil); code != http.StatusNoContent {
		t.Fatalf("share: %d %s", code, b)
	}
	return tp.ID
}

// 035-AC10: from the room and from the profile's "Add to a room", a library persona brings its
// skill in as a room skill held for the AI host, once; its plugin comes with a room admin's add
// only (009-AC5), and the answer's `waiting` names what waits.
func TestAddLibraryPersonaKit(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.device(t), e.device(t)
	tp := e.kitPersona(t, alice)
	own := alice.createRoom(t, "Second", "Alice").ID
	theirs := alice.createRoom(t, "Third", "Alice").ID
	bob.join(t, theirs, "Bob")
	for i, c := range []struct {
		d             *device
		room, path    string
		body          any
		plugins, line string
	}{
		{alice, own, "/api/rooms/" + own + "/personas", map[string]int64{"template_id": tp}, "babs",
			`Alice added the persona 🔥 Grill-2, from the library persona "Grill" v1; waiting for the AI host: skills stamp; plugins babs.`},
		{alice, own, fmt.Sprintf("/api/profile/personas/%d/rooms", tp), map[string]string{"room": own}, "babs",
			`Alice added the persona 🔥 Grill-3, from the library persona "Grill" v1; waiting for the AI host: skills stamp; plugins babs.`},
		{bob, theirs, "/api/rooms/" + theirs + "/personas", map[string]int64{"template_id": tp}, "",
			"🔥 Grill-2 comes without its plugin babs: a room admin adds plugins."},
		{bob, theirs, fmt.Sprintf("/api/profile/personas/%d/rooms", tp), map[string]string{"room": theirs}, "",
			"🔥 Grill-3 comes without its plugin babs: a room admin adds plugins."},
	} {
		code, b := c.d.do(t, "POST", c.path, c.body)
		got := decodeJSON[store.RoomPersona](t, b)
		if code != http.StatusCreated || got.Waiting == nil || strings.Join(got.Waiting.Skills, ",") != "stamp" || strings.Join(got.Waiting.Plugins, ",") != c.plugins ||
			len(got.Skills) != 1 || got.Skills[0].Name != "stamp" || got.Skills[0].Scope != store.ScopeRoom {
			t.Fatalf("#%d %s: %d %s", i, c.path, code, b)
		}
		if e.lastLine(t, c.room).Body != c.line {
			t.Fatalf("#%d line %q, want %q", i, e.lastLine(t, c.room).Body, c.line)
		}
	}
	for room, plugins := range map[string]int{own: 1, theirs: 0} {
		ks, err := e.st.RoomSkills(t.Context(), room)
		ps, _ := e.st.RoomPlugins(t.Context(), room)
		if err != nil || len(ks) != 1 || !ks[0].Pending || len(ps) != plugins {
			t.Fatalf("%s: skills %+v plugins %+v %v", room, ks, ps, err)
		}
	}
}
