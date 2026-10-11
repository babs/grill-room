package web

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/skill"
	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

// skillZip is a skill folder named name whose script prints out.
func skillZip(t *testing.T, name, out string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for path, body := range map[string]string{
		name + "/SKILL.md":         "---\nname: " + name + "\ndescription: Prints a stamp.\n---\nRun scripts/stamp.sh.\n",
		name + "/scripts/stamp.sh": "echo " + out + "\n",
	} {
		w, err := zw.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// raw sends body as is with the device's cookies.
func (d *device) raw(t *testing.T, method, path, ctype string, body []byte, hdr ...string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), method, d.e.ts.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", ctype)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := d.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (d *device) uploadSkill(t *testing.T, scope string, z []byte, want int) store.Skill {
	t.Helper()
	code, b := d.raw(t, "POST", "/api/profile/skills?scope="+scope, "application/zip", z)
	if code != want {
		t.Fatalf("upload: %d %s, want %d", code, b, want)
	}
	if want != http.StatusCreated {
		return store.Skill{}
	}
	return decodeJSON[store.Skill](t, b)
}

// 002-AC29 002-AC30: a profile uploads skill zips from its profile page: personal or shared,
// validated, a same-named upload is the next version, and only zips from this site are taken.
// It removes its personal skills; the admin lists and removes any.
func TestSkillUploadAPI(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "R")
	alice, bob, nobody := e.device(t), e.device(t), e.device(t)
	alice.join(t, room.ID, "Alice")
	bob.join(t, room.ID, "Bob")
	z := skillZip(t, "stamp", "STAMP-v1")
	if code, _ := nobody.raw(t, "POST", "/api/profile/skills", "application/zip", z); code != http.StatusUnauthorized {
		t.Fatalf("no profile: %d", code)
	}
	if code, _ := alice.raw(t, "POST", "/api/profile/skills", "text/plain", z); code != http.StatusUnsupportedMediaType {
		t.Fatalf("form-like type: %d", code)
	}
	if code, _ := alice.raw(t, "POST", "/api/profile/skills", "application/zip", z, "Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
		t.Fatalf("cross-site: %d", code)
	}
	if code, b := alice.raw(t, "POST", "/api/profile/skills", "application/zip", []byte("not a zip")); code != http.StatusBadRequest || !strings.Contains(string(b), "not a zip") {
		t.Fatalf("garbage: %d %s", code, b)
	}
	if code, _ := alice.raw(t, "POST", "/api/profile/skills", "application/zip", make([]byte, skill.MaxZipBytes+1)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize: %d", code)
	}
	k := alice.uploadSkill(t, "personal", z, http.StatusCreated)
	if k.Name != "stamp" || k.Version != 1 || k.Shared || !k.Mine || k.Author != "Alice" || k.Files != 2 {
		t.Fatalf("uploaded: %+v", k)
	}
	alice.uploadSkill(t, "personal", z, http.StatusConflict)
	if k = alice.uploadSkill(t, "personal", skillZip(t, "stamp", "STAMP-v2"), http.StatusCreated); k.Version != 2 {
		t.Fatalf("second version: %+v", k)
	}
	pdf := bob.uploadSkill(t, "shared", skillZip(t, "pdf", "PDF"), http.StatusCreated)
	alice.uploadSkill(t, "shared", skillZip(t, "pdf", "MINE"), http.StatusConflict)

	code, b := alice.do(t, "GET", "/api/profile/skills", nil)
	if mine := decodeJSON[[]store.Skill](t, b); code != http.StatusOK || len(mine) != 1 || mine[0].Name != "stamp" {
		t.Fatalf("Alice's skills: %d %s", code, b)
	}
	if code, _ := bob.do(t, "DELETE", "/api/profile/skills/"+strconv.FormatInt(k.ID, 10), nil); code != http.StatusNotFound {
		t.Fatalf("Bob removes Alice's skill: %d", code)
	}
	if code, _ := bob.do(t, "DELETE", "/api/profile/skills/"+strconv.FormatInt(pdf.ID, 10), nil); code != http.StatusNotFound {
		t.Fatalf("publisher removes a shared skill: %d", code)
	}
	code, b = e.do(t, "GET", "/api/skills", e.admin, nil)
	if all := decodeJSON[[]store.Skill](t, b); code != http.StatusOK || len(all) != 2 {
		t.Fatalf("admin list: %d %s", code, b)
	}
	if code, _ := e.do(t, "GET", "/api/skills", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("admin list without the token: %d", code)
	}
	if code, _ := e.do(t, "DELETE", "/api/skills/"+strconv.FormatInt(pdf.ID, 10), e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("admin removes: %d", code)
	}
	if code, _ := alice.do(t, "DELETE", "/api/profile/skills/"+strconv.FormatInt(k.ID, 10), nil); code != http.StatusNoContent {
		t.Fatalf("Alice removes hers: %d", code)
	}
	if code, b := alice.do(t, "GET", "/api/profile/skills", nil); string(b) != "[]\n" {
		t.Fatalf("after removal: %d %s", code, b)
	}
}

type roomSkillsResp struct {
	Skills     []store.Skill `json:"skills"`
	Disclaimer bool          `json:"disclaimer"`
	NoProfile  bool          `json:"no_profile"`
}

// 002-AC31 002-AC32 002-AC30: a member sees the shared library and their own personal skills;
// the first activation asks for the disclaimer, once per profile; the turn's job lists the pinned
// versions and the daemon fetches them as a plugin directory, which a library update does not
// change until the room takes it.
func TestPersonaSkillsAPI(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	alice := r.alice
	ctx := t.Context()
	ap, err := e.st.ParticipantByToken(ctx, HashToken(alice.p.Token))
	if err != nil || ap.ProfileID == 0 {
		t.Fatalf("Alice's profile: %+v %v", ap, err)
	}
	parse := func(name, out string) *skill.Skill {
		sk, err := skill.Parse(skillZip(t, name, out))
		if err != nil {
			t.Fatal(err)
		}
		return sk
	}
	stamp, err := e.st.PublishSkill(ctx, ap.ProfileID, false, parse("stamp", "STAMP-v1"))
	if err != nil {
		t.Fatal(err)
	}
	bob := e.join(t, e.createInvite(t, r.room).Token, "Bob")
	bp, _ := e.st.ParticipantByToken(ctx, HashToken(bob.Token))
	bobs, err := e.st.PublishSkill(ctx, bp.ProfileID, false, parse("bobs", "BOB"))
	if err != nil {
		t.Fatal(err)
	}

	code, b := e.do(t, "GET", r.path("/skills"), alice.p.Token, nil)
	lib := decodeJSON[roomSkillsResp](t, b)
	if code != http.StatusOK || !lib.Disclaimer || len(lib.Skills) != 1 || lib.Skills[0].ID != stamp.ID {
		t.Fatalf("Alice's room library: %d %s", code, b)
	}
	skillPath := func(id int64) string {
		return r.path("/personas/" + strconv.FormatInt(r.grill.ID, 10) + "/skills/" + strconv.FormatInt(id, 10))
	}
	if code, b := e.do(t, "PUT", skillPath(stamp.ID), alice.p.Token, map[string]bool{}); code != http.StatusPreconditionRequired {
		t.Fatalf("without the disclaimer: %d %s", code, b)
	}
	if code, _ := e.do(t, "PUT", skillPath(bobs.ID), alice.p.Token, map[string]bool{"accept_disclaimer": true}); code != http.StatusNotFound {
		t.Fatalf("Bob's personal skill from Alice: %d", code)
	}
	code, b = e.do(t, "PUT", skillPath(stamp.ID), alice.p.Token, map[string]bool{"accept_disclaimer": true})
	if p := decodeJSON[store.RoomPersona](t, b); code != http.StatusOK || len(p.Skills) != 1 || p.Skills[0].Version != 1 {
		t.Fatalf("activate: %d %s", code, b)
	}
	if code, b := e.do(t, "GET", r.path("/skills"), alice.p.Token, nil); decodeJSON[roomSkillsResp](t, b).Disclaimer {
		t.Fatalf("disclaimer asked again: %d %s", code, b)
	}
	if m := e.lastLine(t, r.room); m.Body != "Alice changed 🔥 Grill: skill stamp v1 on." {
		t.Fatalf("chat line: %+v", m)
	}

	// The library moves on; the room stays on v1 and is offered v2.
	if _, err := e.st.PublishSkill(ctx, ap.ProfileID, false, parse("stamp", "STAMP-v2")); err != nil {
		t.Fatal(err)
	}
	st := e.agent(t, r.room, alice.p.Token)
	if ks := st.Personas[0].Skills; len(ks) != 1 || ks[0].Version != 1 || ks[0].UpdateVersion != 2 {
		t.Fatalf("offered update: %+v", ks)
	}
	tn := e.press(t, r, alice, alice.p.Token, false)
	if job := e.job(t, r, alice, tn.ID); len(job.Skills) != 1 || job.Skills[0] != (turn.Skill{Name: "stamp", Version: 1}) {
		t.Fatalf("job skills: %+v", job.Skills)
	}
	plugin := e.plugin(t, r, alice, tn.ID)
	if plugin["skills/stamp/scripts/stamp.sh"].data != "echo STAMP-v1\n" || !strings.Contains(plugin[".claude-plugin/plugin.json"].data, `"name":"grill-room"`) {
		t.Fatalf("plugin: %+v", plugin)
	}
	// Taking the update during the turn applies from the next one.
	if code, b := e.do(t, "PUT", skillPath(stamp.ID), alice.p.Token, map[string]bool{}); code != http.StatusOK {
		t.Fatalf("update: %d %s", code, b)
	}
	if plugin := e.plugin(t, r, alice, tn.ID); plugin["skills/stamp/scripts/stamp.sh"].data != "echo STAMP-v1\n" {
		t.Fatalf("running turn's plugin changed: %+v", plugin)
	}
	if code, _ := e.result(t, r, alice, tn.ID, turn.Result{Error: "stop"}); code != http.StatusNoContent {
		t.Fatalf("end turn: %d", code)
	}
	tn = e.press(t, r, alice, alice.p.Token, false)
	if plugin := e.plugin(t, r, alice, tn.ID); plugin["skills/stamp/scripts/stamp.sh"].data != "echo STAMP-v2\n" {
		t.Fatalf("next turn's plugin: %+v", plugin)
	}
	if code, _ := e.do(t, "DELETE", skillPath(stamp.ID), bob.Token, nil); code != http.StatusOK {
		t.Fatalf("Bob turns it off: %d", code)
	}
	e.result(t, r, alice, tn.ID, turn.Result{Error: "stop"})
	tn = e.press(t, r, alice, alice.p.Token, false)
	if job := e.job(t, r, alice, tn.ID); len(job.Skills) != 0 {
		t.Fatalf("job after off: %+v", job.Skills)
	}
	if plugin := e.plugin(t, r, alice, tn.ID); len(plugin) != 3 {
		t.Fatalf("empty plugin: %+v", plugin)
	}
}

func (e *env) plugin(t *testing.T, r *turnRoom, d *testDaemon, id int64) map[string]tarFile {
	t.Helper()
	code, b := e.do(t, "GET", r.daemonPath("turns/"+strconv.FormatInt(id, 10)+"/skills"), d.tok, nil)
	if code != http.StatusOK {
		t.Fatalf("skills: %d %s", code, b)
	}
	files := map[string]tarFile{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		files[h.Name] = tarFile{mode: h.Mode, data: string(data)}
	}
}

// 002-AC32: a member without a profile is told so and cannot activate a skill.
func TestPersonaSkillsNeedAProfile(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	ctx := t.Context()
	carol, _, err := e.st.Join(ctx, HashToken(e.createInvite(t, r.room).Token), "Carol", HashToken("carol-token"), store.JoinProfile{})
	if err != nil || carol.ProfileID != 0 {
		t.Fatalf("join without profile: %+v %v", carol, err)
	}
	code, b := e.do(t, "GET", r.path("/skills"), "carol-token", nil)
	if lib := decodeJSON[roomSkillsResp](t, b); code != http.StatusOK || !lib.NoProfile || lib.Disclaimer {
		t.Fatalf("library: %d %s", code, b)
	}
	if code, _ := e.do(t, "PUT", r.path("/personas/"+strconv.FormatInt(r.grill.ID, 10)+"/skills/1"), "carol-token", map[string]bool{"accept_disclaimer": true}); code != http.StatusForbidden {
		t.Fatalf("activate: %d", code)
	}
}

// A skill upload takes one of the server's upload slots and gives up on a body that stops coming.
func TestSkillUploadBounded(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "R")
	alice := e.device(t)
	alice.join(t, room.ID, "Alice")
	for range maxConcurrentUploads {
		e.srv.uploads <- struct{}{}
	}
	if code, _ := alice.raw(t, "POST", "/api/profile/skills", "application/zip", skillZip(t, "stamp", "x")); code != http.StatusServiceUnavailable {
		t.Fatalf("all slots taken: %d", code)
	}
	for range maxConcurrentUploads {
		<-e.srv.uploads
	}
	old := uploadReadTimeout
	uploadReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { uploadReadTimeout = old })
	pr, pw := io.Pipe()
	defer pw.Close()
	go func() { _, _ = pw.Write(skillZip(t, "stamp", "x")[:10]) }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// The transport waits on the body it is sending: end it, or a hung server hangs the test.
	context.AfterFunc(ctx, func() { _ = pr.CloseWithError(ctx.Err()) })
	req, _ := http.NewRequestWithContext(ctx, "POST", e.ts.URL+"/api/profile/skills", pr)
	req.Header.Set("Content-Type", "application/zip")
	start := time.Now()
	resp, err := alice.c.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	if time.Since(start) >= 5*time.Second || (err == nil && resp.StatusCode == http.StatusCreated) {
		t.Fatalf("a stalled body was waited on: %v %v", resp, err)
	}
}
