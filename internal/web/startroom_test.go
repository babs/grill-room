package web

import (
	"archive/tar"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/store"
)

// builtin returns the id of the built-in starter titled title.
func (d *device) builtin(t *testing.T, title string) int64 {
	t.Helper()
	for _, st := range d.starters(t) {
		if st.Title == title && st.Library == store.LibraryBuiltin {
			return st.ID
		}
	}
	t.Fatalf("no built-in %q", title)
	return 0
}

// createFrom posts "Create room" from the editor as d and returns the status and body.
func (d *device) createFrom(t *testing.T, body map[string]any) (int, []byte) {
	t.Helper()
	if body["display_name"] == nil {
		body["display_name"] = "Alice"
	}
	return d.do(t, "POST", "/api/profile/rooms", body)
}

func (e *env) roomOf(t *testing.T, id string) *store.Room {
	t.Helper()
	r, err := e.st.Room(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// 006-AC9 006-AC10 006-AC11 006-AC12 006-AC14: a room from a built-in holds its personas and
// agenda, the fields put in the name and the agenda, its creator as room admin; it records where
// it came from, in the chat, Room settings and the export; a required field left empty refuses it.
func TestCreateRoomFromBuiltin(t *testing.T) {
	e := newEnv(t)
	alice := e.signedIn(t, "Alice")
	grill := alice.builtin(t, "Spec grill")
	starter := map[string]any{"id": grill, "version": 1}
	if code, b := alice.createFrom(t, map[string]any{"name": "Grill: {{topic}}", "starter": starter}); code != http.StatusBadRequest || !strings.Contains(string(b), "fill in Topic") {
		t.Fatalf("no topic: %d %s", code, b)
	}
	if code, b := alice.createFrom(t, map[string]any{"name": "Grill: {{topic}}", "starter": starter, "fields": map[string]string{"topic": "two\nlines"}}); code != http.StatusBadRequest {
		t.Fatalf("two-line field: %d %s", code, b)
	}
	if code, _ := alice.createFrom(t, map[string]any{"name": "X", "starter": map[string]any{"id": 9999, "version": 1}}); code != http.StatusNotFound {
		t.Fatalf("unknown starter: %d", code)
	}
	code, b := alice.createFrom(t, map[string]any{"name": "Grill: {{topic}} {{nope}}", "starter": starter, "fields": map[string]string{"topic": " Billing "}})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, b)
	}
	room := decodeJSON[store.Room](t, b)
	if room.Name != "Grill: Billing {{nope}}" || room.CreatedFrom == nil || *room.CreatedFrom != (store.Provenance{Title: "Spec grill", Version: 1}) {
		t.Fatalf("room: %+v", room)
	}
	ctx := t.Context()
	ps, _ := e.st.RoomPersonas(ctx, room.ID)
	if len(ps) != 3 || ps[0].Name != "Grill" || ps[0].Effort != "high" || ps[0].EveryVoice != 0 || ps[1].Name != "Secretary" || ps[1].EveryVoice != 20 ||
		ps[1].NextVoice != 20 || ps[0].TemplateID != 0 || ps[0].Pending ||
		ps[2].Name != "Rodin" || ps[2].Effort != "high" || ps[2].EveryN != 0 || ps[2].EveryVoice != 0 || ps[2].EveryMinutes != 0 {
		t.Fatalf("personas: %+v", ps)
	}
	docs, _ := e.st.Documents(ctx, room.ID)
	if len(docs) != 1 || docs[0].Name != store.AgendaName || docs[0].Kind != store.DocAgent {
		t.Fatalf("documents: %+v", docs)
	}
	rev, _ := e.st.Revision(ctx, docs[0].ID, 1)
	if !strings.Contains(rev.Body, "# Agenda: grill of Billing\n") || strings.Contains(rev.Body, "{{") {
		t.Fatalf("agenda: %q", rev.Body)
	}
	ms, _ := e.st.Messages(ctx, room.ID, 0, 10)
	if len(ms) != 2 || ms[1].Body != `Alice created the room from the starter "Spec grill" v1.` {
		t.Fatalf("messages: %+v", ms)
	}
	code, b = alice.do(t, "GET", "/api/rooms/"+room.ID, nil)
	if got := decodeJSON[roomResp](t, b); code != http.StatusOK || !got.Me.Admin {
		t.Fatalf("creator: %d %s", code, b)
	}
	code, b = alice.do(t, "GET", "/api/rooms/"+room.ID+"/overview", nil)
	if code != http.StatusOK || !strings.Contains(string(b), `"created_from":{"title":"Spec grill","version":1}`) {
		t.Fatalf("overview: %d %s", code, b)
	}
	code, b = alice.do(t, "GET", "/api/rooms/"+room.ID+"/export", nil)
	if code != http.StatusOK {
		t.Fatalf("export: %d", code)
	}
	var x struct {
		CreatedFrom *store.Provenance `json:"created_from"`
	}
	retar(t, b, func(h *tar.Header, data []byte) []byte {
		if strings.HasSuffix(h.Name, "room.json") {
			_ = json.Unmarshal(data, &x)
		}
		return data
	})
	if x.CreatedFrom == nil || x.CreatedFrom.Title != "Spec grill" {
		t.Fatalf("room.json created_from: %+v", x.CreatedFrom)
	}
	// An empty optional field becomes an empty string.
	code, b = alice.createFrom(t, map[string]any{"name": "Retro: {{team}}", "starter": map[string]any{"id": alice.builtin(t, "Retro"), "version": 1},
		"fields": map[string]string{"team": "Ops"}})
	if code != http.StatusCreated {
		t.Fatalf("retro: %d %s", code, b)
	}
	docs, _ = e.st.Documents(ctx, decodeJSON[store.Room](t, b).ID)
	rev, _ = e.st.Revision(ctx, docs[0].ID, 1)
	if !strings.HasPrefix(rev.Body, "# Agenda: retrospective of Ops \n") {
		t.Fatalf("retro agenda: %q", rev.Body)
	}
	// The guardrails hold (004-AC7).
	e.setSettings(t, store.InstanceSettings{RoomsPerProfile: 2, RoomCreation: store.CreationAnyone})
	if code, b := alice.createFrom(t, map[string]any{"name": "Third", "starter": starter, "fields": map[string]string{"topic": "x"}}); code != http.StatusForbidden {
		t.Fatalf("past the limit: %d %s", code, b)
	}
}

// 006-AC9 006-AC10: the editor's changes go to the room only: Blank's persona, renamed personas,
// an emptied agenda; two personas of one name and too many personas are refused.
func TestCreateRoomFromEditor(t *testing.T) {
	e := newEnv(t)
	alice := e.signedIn(t, "Alice")
	code, b := alice.do(t, "GET", "/api/profile/starters/0", nil)
	blank := decodeJSON[starterResp](t, b)
	if code != http.StatusOK || blank.Title != "Blank" || len(blank.Manifest.Personas) != 1 || blank.Manifest.Personas[0].Name != "Grill" {
		t.Fatalf("blank: %d %s", code, b)
	}
	code, b = alice.createFrom(t, map[string]any{"name": "Plain", "personas": blank.Manifest.Personas})
	if code != http.StatusCreated {
		t.Fatalf("blank room: %d %s", code, b)
	}
	room := decodeJSON[store.Room](t, b)
	ps, _ := e.st.RoomPersonas(t.Context(), room.ID)
	docs, _ := e.st.Documents(t.Context(), room.ID)
	if room.CreatedFrom != nil || len(ps) != 1 || ps[0].Name != "Grill" || len(docs) != 0 {
		t.Fatalf("blank room: %+v %+v %+v", room, ps, docs)
	}
	grill := alice.builtin(t, "Spec grill")
	critic := store.StarterPersona{PersonaFields: store.PersonaFields{Name: "Critic", Badge: "🧐", Colour: "#112233", Text: "Find the flaw.", Model: "haiku", Effort: "low"}, EveryMinutes: 5}
	code, b = alice.createFrom(t, map[string]any{"name": "Edited", "starter": map[string]any{"id": grill, "version": 1}, "fields": map[string]string{"topic": "x"},
		"agenda": "", "personas": []store.StarterPersona{critic}})
	if code != http.StatusCreated {
		t.Fatalf("edited: %d %s", code, b)
	}
	room = decodeJSON[store.Room](t, b)
	ps, _ = e.st.RoomPersonas(t.Context(), room.ID)
	docs, _ = e.st.Documents(t.Context(), room.ID)
	if len(ps) != 1 || ps[0].Name != "Critic" || ps[0].EveryMinutes != 5 || ps[0].NextAt == nil || len(docs) != 0 {
		t.Fatalf("edited room: %+v %+v", ps, docs)
	}
	if _, m, _ := e.st.StarterVersion(t.Context(), grill, 1, 0); len(m.Personas) != 3 {
		t.Fatal("the editor changed the starter")
	}
	many := make([]store.StarterPersona, store.MaxRoomPersonas+1)
	for i := range many {
		many[i] = critic
		many[i].Name = "P" + strconv.Itoa(i)
	}
	twice := critic
	twice.Name = "critic"
	bad := critic
	bad.EveryN = 2
	for name, personas := range map[string][]store.StarterPersona{"too many": many, "same name": {critic, twice}, "two triggers": {bad}} {
		if code, b := alice.createFrom(t, map[string]any{"name": "R", "personas": personas}); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
}

// personalStarter saves, for d's profile, a starter whose persona uses a skill and which holds an
// upload and an agent document.
func (e *env) personalStarter(t *testing.T, d *device) *store.Starter {
	t.Helper()
	m := &store.StarterManifest{Title: "Weekly", RoomName: "Weekly", Agenda: "- [ ] news",
		Personas: []store.StarterPersona{{PersonaFields: store.PersonaFields{Name: "Grill", Badge: "🔥", Colour: "#112233", Text: "t", Model: "sonnet", Effort: "medium"},
			Skills: []string{"stamp"}}},
		Skills: []store.StarterSkill{{Name: "stamp", Version: 3, Author: "Alice", Description: "Stamps.", Files: 1, Size: 4, SHA256: "x", File: "skill/0"}},
		Documents: []store.StarterDocument{{Name: "brief.pdf", Kind: store.DocUpload, Format: "pdf", ContentType: "application/pdf", File: "doc/0", Size: 8},
			{Name: "notes.md", Kind: store.DocAgent, Format: "md", Text: "# Notes\n"}},
		Fields: []store.StarterField{}, Chain: store.StarterChain{Budget: 7, Parallel: true}, Glossary: "Kubernetes"}
	st, err := e.st.SaveStarter(t.Context(), d.profile(t).Profile.ID, 0, m, map[string][]byte{"skill/0": []byte("tar!"), "doc/0": []byte("%PDF-1.4")})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// 006-AC11 006-AC13: a room from a personal starter holds copies: a later version or the
// starter's deletion changes nothing in it. Its skills wait for the first host; with "approve
// everything" on they wait for that host's answer, otherwise they turn on when it connects. A
// pending skill never loads in a turn. The editor leaves out a document; a persona cannot keep a
// skill left out.
func TestRoomFromPersonalStarter(t *testing.T) {
	e := newEnv(t)
	alice := e.signedIn(t, "Alice")
	st := e.personalStarter(t, alice)
	starter := map[string]any{"id": st.ID, "version": 1}
	if code, b := alice.createFrom(t, map[string]any{"name": "W", "starter": starter, "skills": []string{}}); code != http.StatusBadRequest ||
		!strings.Contains(string(b), "Grill uses the skill stamp") {
		t.Fatalf("persona keeps a left-out skill: %d %s", code, b)
	}
	if code, b := alice.createFrom(t, map[string]any{"name": "W", "starter": starter, "documents": []string{"nope.md"}}); code != http.StatusBadRequest {
		t.Fatalf("unknown document: %d %s", code, b)
	}
	if code, b := alice.createFrom(t, map[string]any{"name": "W", "starter": starter, "skills": []string{"stamp", "stamp"}}); code != http.StatusBadRequest ||
		!strings.Contains(string(b), "named twice") {
		t.Fatalf("a skill twice: %d %s", code, b)
	}
	if code, _ := e.signedIn(t, "Bob").createFrom(t, map[string]any{"name": "W", "starter": starter}); code != http.StatusNotFound {
		t.Fatalf("bob uses alice's personal starter: %d", code)
	}
	code, b := alice.createFrom(t, map[string]any{"name": "Weekly", "starter": starter, "documents": []string{"brief.pdf"}})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, b)
	}
	room := decodeJSON[store.Room](t, b)
	ctx := t.Context()
	check := func(when string) {
		t.Helper()
		r := e.roomOf(t, room.ID)
		ps, _ := e.st.RoomPersonas(ctx, room.ID)
		docs, _ := e.st.Documents(ctx, room.ID)
		ks, _ := e.st.RoomSkills(ctx, room.ID)
		if r.Chain.Budget != 7 || !r.ParallelTurns || *r.CreatedFrom != (store.Provenance{Title: "Weekly", Version: 1}) ||
			len(ps) != 1 || len(ps[0].Skills) != 1 || ps[0].Skills[0].Name != "stamp" || ps[0].Skills[0].Version != 3 ||
			len(docs) != 2 || docs[0].Name != store.AgendaName || docs[1].Name != "brief.pdf" || docs[1].Size != 8 ||
			len(ks) != 1 || !ks[0].Pending || !ks[0].FromStarter || ks[0].Version != 3 {
			t.Fatalf("%s: %+v %+v %+v %+v", when, r, ps, docs, ks)
		}
	}
	check("created")
	if g, _ := e.st.STTGlossary(ctx, room.ID); g != "Kubernetes" {
		t.Fatalf("glossary %q", g)
	}
	m := &store.StarterManifest{Title: "Weekly v2", Personas: []store.StarterPersona{}, Skills: []store.StarterSkill{}, Documents: []store.StarterDocument{},
		Fields: []store.StarterField{}, Chain: store.StarterChain{Budget: 1}}
	if _, err := e.st.SaveStarter(ctx, alice.profile(t).Profile.ID, st.ID, m, nil); err != nil {
		t.Fatal(err)
	}
	check("after v2")
	if code, _ := alice.do(t, "DELETE", "/api/profile/starters/"+strconv.FormatInt(st.ID, 10), nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	check("after the starter's deletion")

	// A host with "approve everything" on: the skill still waits, and a turn loads none.
	d := e.daemonFor(t, room.ID, "Hal")
	d.events, d.drop = e.openStream(t, "/api/rooms/"+room.ID+"/daemon/events", d.tok, "")
	if ev := next(t, d.events); ev.event != "hello" {
		t.Fatalf("got %+v", ev)
	}
	e.settings(t, room.ID, d, store.DaemonSettings{ApproveAll: true})
	if ks, _ := e.st.RoomSkills(ctx, room.ID); !ks[0].Pending {
		t.Fatal("released under approve everything")
	}
	r := &turnRoom{room: room.ID, alice: d}
	tn := e.press(t, r, d, d.p.Token, false)
	if job := e.job(t, r, d, tn.ID); len(job.Skills) != 0 {
		t.Fatalf("a pending skill loads: %+v", job.Skills)
	}
	e.settings(t, room.ID, d, store.DaemonSettings{})
	if ks, _ := e.st.RoomSkills(ctx, room.ID); ks[0].Pending || ks[0].FromStarter {
		t.Fatalf("not released: %+v", ks)
	}
	ms, _ := e.st.Messages(ctx, room.ID, 0, 50)
	if last := ms[len(ms)-1]; !strings.Contains(last.Body, "The starter's skills are on") || !strings.HasSuffix(last.Body, ": stamp.") {
		t.Fatalf("line: %+v", last)
	}
}

// 013-AC30: the built-in Spec grill keeps Secretary out of the call and Grill and Rodin in it; Retro and Doc
// review keep every persona speaking. A starter saved from a room carries each persona's speaks,
// voice and rate and the room's call settings; an older starter reads as speaking and defaults.
func TestStartersCarryVoices(t *testing.T) {
	e := newEnv(t)
	alice := e.signedIn(t, "Alice")
	speaking := func(room string) map[string]bool {
		ps, _ := e.st.RoomPersonas(t.Context(), room)
		out := map[string]bool{}
		for _, p := range ps {
			out[p.Name] = p.Speaks
		}
		return out
	}
	create := func(title string, fields map[string]string) store.Room {
		t.Helper()
		id := alice.builtin(t, title)
		var latest int
		for _, st := range alice.starters(t) {
			if st.ID == id {
				latest = st.Version
			}
		}
		code, b := alice.createFrom(t, map[string]any{"name": title, "starter": map[string]any{"id": id, "version": latest}, "fields": fields})
		if code != http.StatusCreated {
			t.Fatalf("%s: %d %s", title, code, b)
		}
		return decodeJSON[store.Room](t, b)
	}
	grill := create("Spec grill", map[string]string{"topic": "Billing"})
	if got := speaking(grill.ID); !got["Grill"] || got["Secretary"] || !got["Rodin"] {
		t.Errorf("Spec grill: %v", got)
	}
	for title, fields := range map[string]map[string]string{"Retro": {"team": "Core"}, "Doc review": {"document": "RFC"}} {
		for name, on := range speaking(create(title, fields).ID) {
			if !on {
				t.Errorf("%s: %s is silent", title, name)
			}
		}
	}

	ctx := t.Context()
	ps, _ := e.st.RoomPersonas(ctx, grill.ID)
	if _, err := e.st.DefaultVoice(ctx, grill.ID, ps[0].ID, "cml-3267"); err != nil {
		t.Fatal(err)
	}
	sp := store.Speech{QuietMS: 3000, ResumeS: 5, QueueS: 60, Words: 50}
	if _, err := e.st.SetSpeech(ctx, grill.ID, sp, nil); err != nil {
		t.Fatal(err)
	}
	draft, err := e.st.RoomDraft(ctx, grill.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Through "Save as starter", as the editor posts it.
	code0, b0 := alice.do(t, "POST", "/api/rooms/"+grill.ID+"/starters", map[string]any{"title": "Grill with voices", "room_name": draft.RoomName,
		"agenda": draft.Agenda, "fields": draft.Fields, "personas": draft.Personas, "documents": []string{}, "skills": []string{}, "plugins": []string{}})
	if code0 != http.StatusCreated && code0 != http.StatusOK {
		t.Fatalf("save as starter: %d %s", code0, b0)
	}
	st := decodeJSON[store.Starter](t, b0)
	code, b := alice.createFrom(t, map[string]any{"name": "Copy", "starter": map[string]any{"id": st.ID, "version": st.Version}})
	if code != http.StatusCreated {
		t.Fatalf("from the saved starter: %d %s", code, b)
	}
	copied := decodeJSON[store.Room](t, b)
	got, _ := e.st.RoomPersonas(ctx, copied.ID)
	if got[0].Voice != "cml-3267" || !got[0].Speaks || got[1].Speaks || e.roomOf(t, copied.ID).Speech != sp {
		t.Errorf("copied %+v / %+v, speech %+v", got[0], got[1], e.roomOf(t, copied.ID).Speech)
	}

	old := e.personalStarter(t, alice)
	code, b = alice.createFrom(t, map[string]any{"name": "Old", "starter": map[string]any{"id": old.ID, "version": old.Version}})
	if code != http.StatusCreated {
		t.Fatalf("older starter: %d %s", code, b)
	}
	oldRoom := decodeJSON[store.Room](t, b)
	if q, _ := e.st.RoomPersonas(ctx, oldRoom.ID); !q[0].Speaks || q[0].Voice != "" || q[0].VoiceRate != 1 || e.roomOf(t, oldRoom.ID).Speech != store.DefaultSpeech {
		t.Errorf("older starter: %+v", q[0])
	}
}
