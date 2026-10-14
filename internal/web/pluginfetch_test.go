package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/plugin"
	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

type fetchList struct {
	Plugins []pluginView        `json:"plugins"`
	Fetches []store.PluginFetch `json:"fetches"`
}

func (e *env) fetches(t *testing.T, room, tok string) []store.PluginFetch {
	t.Helper()
	code, b := e.do(t, "GET", "/api/rooms/"+room+"/plugins", tok, nil)
	if code != http.StatusOK {
		t.Fatalf("list: %d %s", code, b)
	}
	return decodeJSON[fetchList](t, b).Fetches
}

// snapshotTar is a plugin named name, as a daemon's snapshot.
func snapshotTar(t *testing.T, name, manifest string) []byte {
	t.Helper()
	if manifest == "" {
		manifest = `{"name":"` + name + `"}`
	}
	s, err := plugin.FromTree([]plugin.Entry{{Path: plugin.ManifestPath, Data: []byte(manifest)},
		{Path: "skills/hi/SKILL.md", Data: []byte("---\nname: hi\ndescription: Hi.\n---\nSay hi.\n")}}, "", nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return s.Tar
}

// uploadFetch posts what a daemon fetched: commit, ref_name and one snapshot per entry ("." for the repo).
func (e *env) uploadFetch(t *testing.T, r *turnRoom, d *testDaemon, id int64, commit string, snaps map[string][]byte) (int, []byte) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("commit", commit)
	_ = mw.WriteField("ref_name", "main")
	for entry, tar := range snaps {
		w, _ := mw.CreateFormFile("snapshot", entry)
		_, _ = w.Write(tar)
	}
	_ = mw.Close()
	return e.raw(t, "POST", r.daemonPath(fmt.Sprintf("plugin-fetches/%d", id)), d.tok, mw.FormDataContentType(), nil, body.String())
}

// 009-AC19 009-AC20: a room admin asks the AI host to fetch an ssh repo; the request waits in the
// room, the host's daemon hears of it and lists it, and nothing reaches the server's own fetch.
// The daemon's upload is checked again: a tar not in normalised form or other plugins than asked
// fail the request. A good one adds the plugin private, on the personas asked, waiting for
// inspection. A member gets 403; a link with credentials or another transport is refused.
func TestHostFetchAdd(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	e.appoint(t, r.room, r.alice.p.Participant.ID)
	tok, base := r.alice.p.Token, "/api/rooms/"+r.room+"/plugins"
	bob := e.join(t, e.createInvite(t, r.room).Token, "Bob")
	for _, u := range []string{"https://user:pw@example.com/o/r.git", "file:///etc", "ext::sh -c id", "-oProxyCommand=x@h:p", "http://example.com/o/r",
		"ssh://git:pw@example.com/o/r", "git@example.com:-x"} {
		if code, b := e.do(t, "POST", base+"/host-fetch", tok, map[string]any{"url": u}); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", u, code, b)
		}
	}
	if code, _ := e.do(t, "POST", base+"/host-fetch", tok, map[string]any{"url": "git@example.com:o/r.git", "ref": "--upload-pack=x"}); code != http.StatusBadRequest {
		t.Errorf("ref as an option: %d", code)
	}
	if code, _ := e.do(t, "POST", base+"/host-fetch", bob.Token, map[string]any{"url": "git@example.com:o/r.git"}); code != http.StatusForbidden {
		t.Fatalf("member: %d", code)
	}
	nextEvent(t, r.alice.events, "plugins")
	code, b := e.do(t, "POST", base+"/host-fetch", tok, map[string]any{"url": "git@example.com:o/r.git", "personas": []int64{r.grill.ID}})
	if code != http.StatusAccepted {
		t.Fatalf("ask: %d %s", code, b)
	}
	f := decodeJSON[store.PluginFetch](t, b)
	if line := e.lastLine(t, r.room).Body; !strings.Contains(line, "asked the AI host to fetch a plugin from git@example.com:o/r.git") {
		t.Fatalf("chat line %q", line)
	}
	nextEvent(t, r.alice.events, "plugins")
	code, b = e.do(t, "GET", r.daemonPath("plugins"), r.alice.tok, nil)
	if code != http.StatusOK || !strings.Contains(string(b), `"url":"git@example.com:o/r.git"`) || !strings.Contains(string(b), `"max_bytes":20971520`) {
		t.Fatalf("daemon list: %d %s", code, b)
	}
	if fs := e.fetches(t, r.room, bob.Token); len(fs) != 1 || fs[0].State != store.FetchWaiting {
		t.Fatalf("member's list: %+v", fs)
	}
	bd := e.daemonFor(t, r.room, "Bobby")
	if code, _ := e.do(t, "GET", r.daemonPath("plugins"), bd.tok, nil); code != http.StatusForbidden {
		t.Fatalf("non-host list: %d", code)
	}
	commit := strings.Repeat("c", 40)
	tar := snapshotTar(t, "r", "")
	if code, _ := e.uploadFetch(t, r, bd, f.ID, commit, map[string][]byte{".": tar}); code != http.StatusForbidden {
		t.Fatalf("non-host upload: %d", code)
	}
	// Two plugins where the room asked for the repo's one: refused, and the request fails.
	if code, b := e.uploadFetch(t, r, r.alice, f.ID, commit, map[string][]byte{".": tar, "x": snapshotTar(t, "x", "")}); code != http.StatusUnprocessableEntity {
		t.Fatalf("more than asked: %d %s", code, b)
	}
	if fs := e.fetches(t, r.room, tok); len(fs) != 1 || fs[0].State != store.FetchFailed || fs[0].Reason != "the AI host sent other plugins than the room asked for" {
		t.Fatalf("after a refused upload: %+v", fs)
	}
	if code, _ := e.do(t, "DELETE", fmt.Sprintf("/api/rooms/%s/plugin-fetches/%d", r.room, f.ID), bob.Token, nil); code != http.StatusForbidden {
		t.Fatalf("member dismiss: %d", code)
	}
	if code, _ := e.do(t, "DELETE", fmt.Sprintf("/api/rooms/%s/plugin-fetches/%d", r.room, f.ID), tok, nil); code != http.StatusNoContent {
		t.Fatalf("dismiss: %d", code)
	}

	code, b = e.do(t, "POST", base+"/host-fetch", tok, map[string]any{"url": "git@example.com:o/r.git", "personas": []int64{r.grill.ID}})
	f = decodeJSON[store.PluginFetch](t, b)
	if code != http.StatusAccepted {
		t.Fatalf("ask again: %d %s", code, b)
	}
	// A tar out of normalised form: refused as the AC9 rules say.
	if code, b := e.uploadFetch(t, r, r.alice, f.ID, commit, map[string][]byte{".": append(slices.Clone(tar), 0)}); code != http.StatusUnprocessableEntity ||
		!strings.Contains(string(b), "invalid plugin") {
		t.Fatalf("bad tar: %d %s", code, b)
	}
	e.do(t, "DELETE", fmt.Sprintf("/api/rooms/%s/plugin-fetches/%d", r.room, f.ID), tok, nil)
	_, b = e.do(t, "POST", base+"/host-fetch", tok, map[string]any{"url": "git@example.com:o/r.git", "personas": []int64{r.grill.ID}})
	f = decodeJSON[store.PluginFetch](t, b)
	if code, b := e.uploadFetch(t, r, r.alice, f.ID, commit, map[string][]byte{".": tar}); code != http.StatusCreated {
		t.Fatalf("upload: %d %s", code, b)
	}
	nextEvent(t, r.alice.events, "inspect")
	ps := e.plugins(t, r.room, tok)
	if len(ps) != 1 || ps[0].Name != "r" || !ps[0].Private || ps[0].URL != "git@example.com:o/r.git" || len(ps[0].Personas) != 1 ||
		ps[0].Versions[0].State != store.PluginInspecting || ps[0].Versions[0].Commit != commit || ps[0].RefName != "main" {
		t.Fatalf("plugins %+v", ps)
	}
	if fs := e.fetches(t, r.room, tok); len(fs) != 0 {
		t.Fatalf("the done request stays: %+v", fs)
	}
	if code, _ := e.uploadFetch(t, r, r.alice, f.ID, commit, map[string][]byte{".": tar}); code != http.StatusNotFound {
		t.Fatalf("upload twice: %d", code)
	}
}

// 009-AC21: Update on a private plugin asks the AI host; a host that cannot reach the repo fails
// the request with that sentence and the approved version keeps running; a declined one says so.
func TestHostFetchUpdate(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	e.appoint(t, r.room, r.alice.p.Participant.ID)
	tok, base := r.alice.p.Token, "/api/rooms/"+r.room+"/plugins"
	_, b := e.do(t, "POST", base+"/host-fetch", tok, map[string]any{"url": "ssh://git@example.com/o/r.git"})
	f := decodeJSON[store.PluginFetch](t, b)
	if code, b := e.uploadFetch(t, r, r.alice, f.ID, strings.Repeat("a", 40), map[string][]byte{".": snapshotTar(t, "r", "")}); code != http.StatusCreated {
		t.Fatalf("upload: %d %s", code, b)
	}
	p := e.plugins(t, r.room, tok)[0]
	if code, b := e.do(t, "POST", r.daemonPath(fmt.Sprintf("plugins/%d/report", p.Versions[0].ID)), r.alice.tok, pluginReport{OK: true}); code != http.StatusOK {
		t.Fatalf("report: %d %s", code, b)
	}
	update := fmt.Sprintf("%s/%d/update", base, p.ID)
	code, b := e.do(t, "POST", update, tok, nil)
	if code != http.StatusAccepted || !strings.Contains(string(b), `"host_fetch"`) {
		t.Fatalf("update: %d %s", code, b)
	}
	if code, _ := e.do(t, "POST", update, tok, nil); code != http.StatusConflict {
		t.Fatalf("second update while one waits: %d", code)
	}
	f = e.fetches(t, r.room, tok)[0]
	if f.PluginName != "r" {
		t.Fatalf("request %+v", f)
	}
	if code, b := e.do(t, "POST", r.daemonPath(fmt.Sprintf("plugin-fetches/%d", f.ID)), r.alice.tok, map[string]string{"error": "This AI host cannot reach the repo"}); code != http.StatusNoContent {
		t.Fatalf("failure: %d %s", code, b)
	}
	if line := e.lastLine(t, r.room).Body; line != "The AI host could not fetch the update of r: This AI host cannot reach the repo." {
		t.Fatalf("chat line %q", line)
	}
	if p := e.plugins(t, r.room, tok)[0]; p.Running != 1 || len(p.Versions) != 1 {
		t.Fatalf("after the failure: %+v", p)
	}
	e.do(t, "DELETE", fmt.Sprintf("/api/rooms/%s/plugin-fetches/%d", r.room, f.ID), tok, nil)
	e.do(t, "POST", update, tok, nil)
	f = e.fetches(t, r.room, tok)[0]
	if code, _ := e.do(t, "POST", r.daemonPath(fmt.Sprintf("plugin-fetches/%d", f.ID)), r.alice.tok, map[string]bool{"declined": true}); code != http.StatusNoContent {
		t.Fatalf("decline: %d", code)
	}
	if fs := e.fetches(t, r.room, tok); fs[0].Reason != "the AI host declined it" {
		t.Fatalf("declined: %+v", fs)
	}
	e.do(t, "DELETE", fmt.Sprintf("/api/rooms/%s/plugin-fetches/%d", r.room, f.ID), tok, nil)
	// A new commit adds a version waiting for inspection; another name is refused.
	e.do(t, "POST", update, tok, nil)
	f = e.fetches(t, r.room, tok)[0]
	if code, b := e.uploadFetch(t, r, r.alice, f.ID, strings.Repeat("b", 40), map[string][]byte{".": snapshotTar(t, "other", "")}); code != http.StatusUnprocessableEntity ||
		!strings.Contains(string(b), "named other now") {
		t.Fatalf("renamed: %d %s", code, b)
	}
	e.do(t, "DELETE", fmt.Sprintf("/api/rooms/%s/plugin-fetches/%d", r.room, f.ID), tok, nil)
	e.do(t, "POST", update, tok, nil)
	f = e.fetches(t, r.room, tok)[0]
	if code, b := e.uploadFetch(t, r, r.alice, f.ID, strings.Repeat("b", 40), map[string][]byte{".": snapshotTar(t, "r", `{"name":"r","version":"2"}`)}); code != http.StatusCreated {
		t.Fatalf("new version: %d %s", code, b)
	}
	if p := e.plugins(t, r.room, tok)[0]; p.Running != 1 || len(p.Versions) != 2 || p.Versions[1].State != store.PluginInspecting {
		t.Fatalf("after the update: %+v", p)
	}
}

// 009-AC24: the job names each plugin's options, never a value; a result skipping a plugin for
// host setup marks it, and the room says why it does not load; a later turn that loads it clears it.
func TestNeedsHostSetup(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	e.appoint(t, r.room, r.alice.p.Participant.ID)
	tok := r.alice.p.Token
	m, _ := json.Marshal(plugin.Manifest{Name: "opt", Options: []plugin.Option{{Key: "api_key", Sensitive: true, Required: true}, {Key: "region", Default: "eu"}}})
	if _, err := e.st.AddPlugins(t.Context(), r.room, []store.NewPlugin{{Name: "opt", URL: "https://example.com/opt", RefName: "main",
		PluginSnapshot: store.PluginSnapshot{Commit: strings.Repeat("a", 40), SHA256: "s", FileList: []byte("[]"), Manifest: m, Data: []byte("t")}}},
		[]int64{r.grill.ID}, &r.alice.p.Participant, 1<<20); err != nil {
		t.Fatal(err)
	}
	p := e.plugins(t, r.room, tok)[0]
	if code, b := e.do(t, "POST", r.daemonPath(fmt.Sprintf("plugins/%d/report", p.Versions[0].ID)), r.alice.tok, pluginReport{OK: true}); code != http.StatusOK {
		t.Fatalf("report: %d %s", code, b)
	}
	tn := e.press(t, r, r.alice, tok, false)
	j := e.job(t, r, r.alice, tn.ID)
	want := []turn.PluginOption{{Key: "api_key", Sensitive: true, Required: true}, {Key: "region", Default: "eu"}}
	if len(j.Plugins) != 1 || fmt.Sprint(j.Plugins[0].Options) != fmt.Sprint(want) {
		t.Fatalf("job plugins %+v", j.Plugins)
	}
	if code, b := e.result(t, r, r.alice, tn.ID, turn.Result{SessionID: sid1, Session: []byte("one\n"),
		SkippedPlugins: []turn.SkippedPlugin{{Name: "opt", Reason: turn.NeedsHostSetup}, {Name: "not-pinned", Reason: turn.NeedsHostSetup}}}); code != http.StatusOK {
		t.Fatalf("result: %d %s", code, b)
	}
	if p := e.plugins(t, r.room, tok)[0]; !p.NeedsSetup || p.Why != needsSetupText {
		t.Fatalf("after a skip: %+v", p)
	}
	tn = e.press(t, r, r.alice, tok, false)
	if code, b := e.result(t, r, r.alice, tn.ID, turn.Result{SessionID: sid1, SessionOffset: 4, Session: []byte("two\n")}); code != http.StatusOK {
		t.Fatalf("result: %d %s", code, b)
	}
	if p := e.plugins(t, r.room, tok)[0]; p.NeedsSetup || p.Why != "" {
		t.Fatalf("after a load: %+v", p)
	}
}

// 009-AC25: the report keeps the domains off the host's allowlist, bounded.
func TestReportAllowlistMissing(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	tok := r.alice.p.Token
	if _, err := e.st.AddPlugins(t.Context(), r.room, []store.NewPlugin{{Name: "x", URL: "https://example.com/x", RefName: "main",
		PluginSnapshot: store.PluginSnapshot{Commit: strings.Repeat("a", 40), SHA256: "s", FileList: []byte("[]"), Manifest: []byte(`{"name":"x","domains":["mcp.example.com"]}`), Data: []byte("t")}}},
		nil, &r.alice.p.Participant, 1<<20); err != nil {
		t.Fatal(err)
	}
	many := make([]string, 150)
	for i := range many {
		many[i] = fmt.Sprintf("d%d.example.com", i)
	}
	many[0] = "mcp.example.com"
	v := e.plugins(t, r.room, tok)[0].Versions[0]
	if code, b := e.do(t, "POST", r.daemonPath(fmt.Sprintf("plugins/%d/report", v.ID)), r.alice.tok, pluginReport{OK: true, AllowlistMissing: many}); code != http.StatusOK {
		t.Fatalf("report: %d %s", code, b)
	}
	var rep pluginReport
	if err := json.Unmarshal(e.plugins(t, r.room, tok)[0].Versions[0].Report, &rep); err != nil || len(rep.AllowlistMissing) != maxReportDomains || rep.AllowlistMissing[0] != "mcp.example.com" {
		t.Fatalf("stored report: %v %d", err, len(rep.AllowlistMissing))
	}
}

// 009-AC19 009-AC20: what the routes refuse. The request: too many or badly named plugins, a
// persona of another room, an unknown request to dismiss. The daemon's answer: a failure with no
// words, a request no longer waiting, an upload that is not a form, names no commit, or a plugin
// whose name the room has already, which fails the request with the reason.
func TestHostFetchEdges(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	e.appoint(t, r.room, r.alice.p.Participant.ID)
	tok, base := r.alice.p.Token, "/api/rooms/"+r.room+"/plugins"
	const url = "git@example.com:o/r.git"
	many := make([]string, store.MaxRoomPlugins+1)
	for i := range many {
		many[i] = fmt.Sprintf("p%d", i)
	}
	other := newTurnRoom(t, e)
	for _, c := range []struct {
		body map[string]any
		want int
	}{
		{map[string]any{"url": url, "entries": many}, http.StatusBadRequest},
		{map[string]any{"url": url, "entries": []string{"../x"}}, http.StatusBadRequest},
		{map[string]any{"url": url, "personas": []int64{other.grill.ID}}, http.StatusNotFound},
	} {
		if code, b := e.do(t, "POST", base+"/host-fetch", tok, c.body); code != c.want {
			t.Errorf("%v: %d %s", c.body, code, b)
		}
	}
	if code, _ := e.do(t, "DELETE", fmt.Sprintf("/api/rooms/%s/plugin-fetches/%d", r.room, 999), tok, nil); code != http.StatusNotFound {
		t.Fatalf("dismiss unknown: %d", code)
	}
	ask := func() store.PluginFetch {
		t.Helper()
		code, b := e.do(t, "POST", base+"/host-fetch", tok, map[string]any{"url": url})
		if code != http.StatusAccepted {
			t.Fatalf("ask: %d %s", code, b)
		}
		return decodeJSON[store.PluginFetch](t, b)
	}
	f := ask()
	post := func(ct, body string) (int, []byte) {
		return e.raw(t, "POST", r.daemonPath(fmt.Sprintf("plugin-fetches/%d", f.ID)), r.alice.tok, ct, nil, body)
	}
	if code, b := post("text/plain", "x"); code != http.StatusUnprocessableEntity || !strings.Contains(string(b), "not multipart") {
		t.Fatalf("not a form: %d %s", code, b)
	}
	f = ask()
	if code, b := e.uploadFetch(t, r, r.alice, f.ID, "not-a-commit", map[string][]byte{".": snapshotTar(t, "r", "")}); code != http.StatusUnprocessableEntity ||
		!strings.Contains(string(b), "names no commit") {
		t.Fatalf("no commit: %d %s", code, b)
	}
	f = ask()
	if code, _ := post("application/json", `{"error":"  "}`); code != http.StatusNoContent {
		t.Fatalf("failure: %d", code)
	}
	if fs := e.fetches(t, r.room, tok); fs[len(fs)-1].Reason != "the fetch failed" {
		t.Fatalf("a failure with no words: %+v", fs[len(fs)-1])
	}
	if code, _ := post("application/json", `{"error":"again"}`); code != http.StatusConflict {
		t.Fatalf("a failed request answered again: %d", code)
	}
	if code, _ := e.uploadFetch(t, r, r.alice, f.ID, strings.Repeat("a", 40), map[string][]byte{".": snapshotTar(t, "r", "")}); code != http.StatusConflict {
		t.Fatalf("an upload for a failed request: %d", code)
	}
	for _, x := range e.fetches(t, r.room, tok) {
		e.do(t, "DELETE", fmt.Sprintf("/api/rooms/%s/plugin-fetches/%d", r.room, x.ID), tok, nil)
	}
	// A plugin of that name in the room already: the request fails with the store's reason.
	f = ask()
	if code, _ := e.uploadFetch(t, r, r.alice, f.ID, strings.Repeat("a", 40), map[string][]byte{".": snapshotTar(t, "r", "")}); code != http.StatusCreated {
		t.Fatalf("first: %d", code)
	}
	f = ask()
	if code, b := e.uploadFetch(t, r, r.alice, f.ID, strings.Repeat("b", 40), map[string][]byte{".": snapshotTar(t, "r", "")}); code != http.StatusConflict ||
		!strings.Contains(string(b), store.ErrPluginName.Error()) {
		t.Fatalf("name taken: %d %s", code, b)
	}
	if fs := e.fetches(t, r.room, tok); len(fs) != 1 || fs[0].State != store.FetchFailed {
		t.Fatalf("after the clash: %+v", fs)
	}
}

// 009-AC22 009-AC25: the host's daemon reads each plugin with the options and domains of its
// running version, else of its latest one; never a value.
func TestDaemonPluginsList(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	add := func(name, manifest string) int64 {
		t.Helper()
		if _, err := e.st.AddPlugins(t.Context(), r.room, []store.NewPlugin{{Name: name, URL: "https://example.com/" + name, RefName: "main",
			PluginSnapshot: store.PluginSnapshot{Commit: strings.Repeat("a", 40), SHA256: "s-" + name, FileList: []byte("[]"), Manifest: []byte(manifest), Data: []byte("t")}}},
			nil, &r.alice.p.Participant, 1<<20); err != nil {
			t.Fatal(err)
		}
		ps, _ := e.st.RoomPlugins(t.Context(), r.room)
		for _, p := range ps {
			if p.Name == name {
				return p.Versions[0].ID
			}
		}
		return 0
	}
	v := add("run", `{"name":"run","options":[{"key":"api_key","sensitive":true,"required":true}],"domains":["mcp.example.com"]}`)
	if code, b := e.do(t, "POST", r.daemonPath(fmt.Sprintf("plugins/%d/report", v)), r.alice.tok, pluginReport{OK: true}); code != http.StatusOK {
		t.Fatalf("report: %d %s", code, b)
	}
	add("new", `{"name":"new","options":[{"key":"region","default":"eu"}]}`)
	code, b := e.do(t, "GET", r.daemonPath("plugins"), r.alice.tok, nil)
	var got struct {
		Plugins []daemonPlugin `json:"plugins"`
	}
	if err := json.Unmarshal(b, &got); code != http.StatusOK || err != nil || len(got.Plugins) != 2 {
		t.Fatalf("list: %d %s", code, b)
	}
	byName := map[string]daemonPlugin{}
	for _, p := range got.Plugins {
		byName[p.Name] = p
	}
	if p := byName["run"]; p.Version != 1 || len(p.Options) != 1 || !p.Options[0].Sensitive || len(p.Domains) != 1 || p.ID == 0 {
		t.Fatalf("running plugin %+v", p)
	}
	if p := byName["new"]; p.Version != 1 || len(p.Options) != 1 || p.Options[0].Default != "eu" {
		t.Fatalf("latest-only plugin %+v", p)
	}
}
