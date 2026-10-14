package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/anchor"
	"github.com/babs/grill-room/internal/store"
)

// richRoom gives alice a room she administers, holding what a starter must take and what it must
// never take: a room skill one persona has on, an agenda with a field, an agent document with an
// older revision, an upload, a message, an annotation and a second member's invite.
func (e *env) richRoom(t *testing.T, alice *device) (store.Room, *device) {
	t.Helper()
	ctx := t.Context()
	room := alice.createRoom(t, "Weekly sync", "Alice")
	base := "/api/rooms/" + room.ID
	sk := alice.uploadSkill(t, "personal", skillZip(t, "stamp", "x"), http.StatusCreated)
	code, b := alice.do(t, "POST", base+"/skills", map[string]int64{"skill_id": sk.ID})
	if code != http.StatusCreated {
		t.Fatalf("copy skill: %d %s", code, b)
	}
	roomSkill := decodeJSON[store.Skill](t, b)
	ps, _ := e.st.RoomPersonas(ctx, room.ID)
	if code, b := alice.do(t, "PUT", fmt.Sprintf("%s/personas/%d/skills/%d", base, ps[0].ID, roomSkill.ID), map[string]bool{"accept_disclaimer": true}); code != http.StatusOK {
		t.Fatalf("skill on: %d %s", code, b)
	}
	if _, err := e.st.CreateAgentDocument(ctx, room.ID, store.AgendaName, "md", "- [ ] {{topic}}: news\n", "Alice"); err != nil {
		t.Fatal(err)
	}
	notes, err := e.st.CreateAgentDocument(ctx, room.ID, "notes.md", "md", "OLD-REVISION\n", "Grill")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.AddRevision(ctx, room.ID, notes.ID, "# Notes, current\n", "Grill"); err != nil {
		t.Fatal(err)
	}
	ms, _ := e.st.Messages(ctx, room.ID, 0, 1)
	pid := ms[0].ParticipantID
	if _, err := e.st.AddUpload(ctx, &store.Upload{RoomID: room.ID, ParticipantID: pid, Author: "Alice", Filename: "plan.md", ContentType: "text/markdown",
		Format: "md", Data: []byte("# Plan\n"), Text: "# Plan\n"}, 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.CreateAnnotation(ctx, room.ID, notes.ID, 2, &anchor.Anchor{Kind: anchor.KindText, Exact: "current"},
		&store.NewMessage{ParticipantID: pid, Author: "Alice", Body: "SECRET-ANNOTATION"}, 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := e.bus.Post(ctx, &store.Message{RoomID: room.ID, Kind: store.KindText, ParticipantID: pid, Sender: "Alice", Body: "SECRET-MESSAGE"}); err != nil {
		t.Fatal(err)
	}
	inv, b := alice.do(t, "POST", base+"/invites", nil)
	if inv != http.StatusCreated {
		t.Fatalf("invite: %d %s", inv, b)
	}
	bob := e.device(t)
	bob.do(t, "POST", "/api/join", map[string]string{"invite": decodeJSON[invite](t, b).Token, "name": "Bob"})
	return room, bob
}

// 006-AC15 006-AC16 006-AC17: a room admin opens "Save as starter" on the room's personas, skills,
// agenda and documents, saves it with the documents chosen into their personal library, and a room
// created from it holds the same minus what was left out; a plain member gets a 403. An undeclared
// {{x}} in the agenda or the room name blocks the save, naming it. The starter never holds the
// room's messages, annotations, invites or older revisions.
func TestSaveRoomAsStarter(t *testing.T) {
	e := newEnv(t)
	alice := e.signedIn(t, "Alice")
	room, bob := e.richRoom(t, alice)
	base := "/api/rooms/" + room.ID
	for _, c := range []struct{ method, path string }{{"GET", "/starter-draft"}, {"POST", "/starters"}} {
		if code, b := bob.do(t, c.method, base+c.path, map[string]string{"title": "x"}); code != http.StatusForbidden {
			t.Fatalf("plain member %s %s: %d %s", c.method, c.path, code, b)
		}
	}
	code, b := alice.do(t, "GET", base+"/starter-draft", nil)
	draft := decodeJSON[starterResp](t, b).Manifest
	if code != http.StatusOK || draft.Title != "Weekly sync" || draft.Agenda != "- [ ] {{topic}}: news\n" || len(draft.Personas) != 1 ||
		strings.Join(draft.Personas[0].Skills, ",") != "stamp" || len(draft.Skills) != 1 || len(draft.Documents) != 2 || draft.Documents[0].Name != "notes.md" ||
		draft.Documents[0].Text != "# Notes, current\n" {
		t.Fatalf("draft: %d %s", code, b)
	}
	// body is a valid save, but for patch.
	body := func(patch map[string]any) map[string]any {
		out := map[string]any{"title": "Weekly", "description": "Our sync", "room_name": "Weekly: {{topic}}", "agenda": draft.Agenda,
			"personas": draft.Personas, "documents": []string{"notes.md"}, "skills": []string{"stamp"},
			"fields": []store.StarterField{{Label: "Topic", Key: "topic", Required: true}, {Label: "Unused", Key: "unused"}}}
		for k, v := range patch {
			out[k] = v
		}
		return out
	}
	for _, c := range []struct {
		patch map[string]any
		want  string
	}{
		{map[string]any{"fields": []store.StarterField{}, "room_name": "Weekly"}, "{{topic}} in the agenda is no field"},
		{map[string]any{"room_name": "Weekly: {{team}}"}, "{{team}} in the room name is no field"},
		{map[string]any{"agenda": "- [ ] {{a}} and {{b}}"}, "{{a}} in the agenda is no field"},
		{map[string]any{"skills": []string{}}, "Grill uses the skill stamp"},
		{map[string]any{"fields": []store.StarterField{{Label: "Topic", Key: "topic"}, {Label: "Topic 2", Key: "topic"}}}, "its key must be unique"},
		{map[string]any{"fields": []store.StarterField{{Label: "Topic", Key: "Topic!"}}}, "its key must be unique"},
		{map[string]any{"title": ""}, "the title must be"},
	} {
		if code, b := alice.do(t, "POST", base+"/starters", body(c.patch)); code != http.StatusBadRequest || !strings.Contains(string(b), c.want) {
			t.Errorf("want %q: %d %s", c.want, code, b)
		}
	}
	save := body(nil)
	code, b = alice.do(t, "POST", base+"/starters", save)
	st := decodeJSON[store.Starter](t, b)
	if code != http.StatusCreated || st.Library != store.LibraryPersonal || st.Author != "Alice" || st.Title != "Weekly" || st.Version != 1 {
		t.Fatalf("save: %d %s", code, b)
	}
	if _, _, err := e.st.StarterVersion(t.Context(), st.ID, 1, 0); err == nil {
		t.Fatal("profile 0 reads alice's personal starter")
	}
	prof := alice.profile(t).Profile
	_, m, err := e.st.StarterVersion(t.Context(), st.ID, 1, prof.ID)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := e.st.StarterFiles(t.Context(), st.ID, 1)
	raw, _ := json.Marshal(m)
	all := string(raw)
	for _, f := range files {
		all += string(f)
	}
	for _, never := range []string{"SECRET-MESSAGE", "SECRET-ANNOTATION", "OLD-REVISION", "plan.md", "Bob", "invite"} {
		if strings.Contains(all, never) {
			t.Errorf("the starter holds %q", never)
		}
	}
	if len(m.Documents) != 1 || len(files) != 1 || m.Chain.Budget != store.DefaultChainBudget {
		t.Fatalf("manifest %+v, %d files", m, len(files))
	}
	// A room from it holds the same, the field filled in.
	code, b = alice.createFrom(t, map[string]any{"name": "Weekly: {{topic}}", "starter": map[string]any{"id": st.ID, "version": 1}, "fields": map[string]string{"topic": "Q4"}})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, b)
	}
	copyRoom := decodeJSON[store.Room](t, b)
	again, _ := e.st.RoomDraft(t.Context(), copyRoom.ID)
	if copyRoom.Name != "Weekly: Q4" || again.Agenda != "- [ ] Q4: news" || len(again.Documents) != 1 || again.Documents[0].Text != draft.Documents[0].Text ||
		len(again.Personas) != 1 || again.Personas[0].Text != draft.Personas[0].Text {
		t.Fatalf("room from it: %+v %+v", copyRoom, again)
	}

	// Its author saves a new version from the profile page; nobody else may.
	path := "/api/profile/starters/" + strconv.FormatInt(st.ID, 10) + "/versions"
	v2 := map[string]any{"version": 1, "title": "Weekly v2", "room_name": "Weekly", "agenda": "", "personas": []store.StarterPersona{}, "documents": []string{},
		"skills": []string{}, "fields": []store.StarterField{}}
	code, b = alice.do(t, "POST", path, v2)
	if got := decodeJSON[store.Starter](t, b); code != http.StatusCreated || got.Version != 2 || got.Title != "Weekly v2" {
		t.Fatalf("v2: %d %s", code, b)
	}
	if f, _ := e.st.StarterFiles(t.Context(), st.ID, 2); len(f) != 0 {
		t.Fatalf("v2 files: %d", len(f))
	}
	// The pages read every list as one: none is [], never null.
	_, b = alice.do(t, "GET", "/api/profile/starters/"+strconv.FormatInt(st.ID, 10), nil)
	for _, want := range []string{`"documents":[]`, `"skills":[]`, `"personas":[]`, `"fields":[]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("v2 lacks %s: %s", want, b)
		}
	}
	// Saved on top of v1 while v2 exists: refused, as another tab saved since.
	if code, b := alice.do(t, "POST", path, v2); code != http.StatusConflict || !strings.Contains(string(b), "version 2 exists") {
		t.Fatalf("stale save: %d %s", code, b)
	}
	if code, _ := bob.do(t, "POST", path, v2); code != http.StatusUnauthorized && code != http.StatusNotFound {
		t.Fatalf("bob: %d", code)
	}
	carol := e.signedIn(t, "Carol")
	if code, _ := alice.do(t, "POST", "/api/profile/starters/"+strconv.FormatInt(st.ID, 10)+"/publish", nil); code != http.StatusNoContent {
		t.Fatal("publish")
	}
	if code, _ := carol.do(t, "POST", path, v2); code != http.StatusForbidden {
		t.Fatalf("carol adds a version to a shared starter: %d", code)
	}
}
