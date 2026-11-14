package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

// 023-AC2 to 023-AC6 023-AC21: the link routes' rights, cap, duplicate answer, audit and lines.
func TestRoomLinkRoutes(t *testing.T) {
	e := newEnv(t)
	ana := e.device(t)
	pricing := ana.createRoom(t, "Pricing", "Ana")
	legal := ana.createRoom(t, "Legal", "Ana")
	bob := e.device(t)
	bob.join(t, pricing.ID, "Bob")
	bob.join(t, legal.ID, "Bob")
	carl := e.device(t)
	carl.join(t, e.createRoom(t, "Elsewhere").ID, "Carl")
	full, read := ana.newToken(t, "full", 30), ana.newToken(t, "read", 0)
	links := func(room string) string { return "/api/rooms/" + room + "/links" }
	both := map[string]any{"room": legal.ID, "reads": true, "read_by": true}

	for _, c := range []struct {
		name         string
		dev          *device
		token        string
		method, path string
		body         any
		want         int
		msg          string
	}{
		{"member not admin", bob, "", "POST", links(pricing.ID), both, http.StatusForbidden, msgLinkBothAdmin},
		{"read-only token", nil, read.Token, "POST", links(pricing.ID), both, http.StatusForbidden, msgTokenReadOnly},
		{"no direction", ana, "", "POST", links(pricing.ID), map[string]any{"room": legal.ID}, http.StatusBadRequest, ""},
		{"self", ana, "", "POST", links(pricing.ID), map[string]any{"room": pricing.ID, "reads": true}, http.StatusBadRequest, msgLinkSelf},
		{"self, not admin", bob, "", "POST", links(pricing.ID), map[string]any{"room": pricing.ID, "reads": true}, http.StatusBadRequest, msgLinkSelf},
		{"non-member list", carl, "", "GET", links(pricing.ID), nil, http.StatusUnauthorized, ""},
	} {
		var code int
		var b []byte
		if c.dev != nil {
			code, b = c.dev.do(t, c.method, c.path, c.body)
		} else {
			code, b = e.do(t, c.method, c.path, c.token, c.body)
		}
		if code != c.want || c.msg != "" && errorOf(t, b) != c.msg {
			t.Errorf("%s: %d %s, want %d %q", c.name, code, b, c.want, c.msg)
		}
	}

	// 023-AC2: Ana links both ways with her token; 023-AC3: again answers 200 with the same links.
	code, b := e.do(t, "POST", links(pricing.ID), full.Token, both)
	made := decodeJSON[[]store.RoomLink](t, b)
	if code != http.StatusCreated || len(made) != 2 || made[0].ReaderRoom != pricing.ID || made[1].ReaderRoom != legal.ID || !made[0].Member || !made[1].CanRemove {
		t.Fatalf("create: %d %s", code, b)
	}
	code, b = ana.do(t, "POST", links(pricing.ID), both)
	if again := decodeJSON[[]store.RoomLink](t, b); code != http.StatusOK || len(again) != 2 || again[0].ID != made[0].ID {
		t.Fatalf("duplicate: %d %s", code, b)
	}
	// 023-AC6: a line in each room, one entry per link with both rooms and the token.
	if got := e.lastLine(t, legal.ID).Body; got != "Ana linked this room: it now reads Pricing" {
		t.Fatalf("line in Legal: %q", got)
	}
	if got := e.lastLine(t, pricing.ID).Body; got != "Ana linked Legal: it now reads this room" {
		t.Fatalf("line in Pricing: %q", got)
	}
	es := e.auditLog(t, "?action="+ActionRoomLinkCreate).Entries
	var d map[string]any
	if len(es) != 2 || json.Unmarshal(es[1].Details, &d) != nil || d["api_token"] != float64(full.ID) ||
		d["reader_room"] != pricing.ID || d["read_name"] != "Legal" || d["read_room"] != legal.ID || d["reader_name"] != "Pricing" {
		t.Fatalf("audit: %+v", es)
	}

	// 023-AC1: a plain member lists both rows, removing none.
	code, b = bob.do(t, "GET", links(legal.ID), nil)
	if got := decodeJSON[[]store.RoomLink](t, b); code != http.StatusOK || len(got) != 2 || got[0].CanRemove || !got[0].Member {
		t.Fatalf("member list: %d %s", code, b)
	}
	if code, _ := bob.do(t, "DELETE", fmt.Sprintf("%s/%d", links(legal.ID), made[0].ID), nil); code != http.StatusForbidden {
		t.Fatalf("member removes: %d", code)
	}
	if code, b := e.do(t, "DELETE", fmt.Sprintf("%s/%d", links(legal.ID), made[0].ID), read.Token, nil); code != http.StatusForbidden || errorOf(t, b) != msgTokenReadOnly {
		t.Fatalf("read-only token removes: %d %s", code, b)
	}

	// 023-AC5: an admin of Legal only removes "Pricing reads Legal" from Legal's side.
	if _, err := e.sqlDB(t).ExecContext(t.Context(), `DELETE FROM room_admins WHERE room_id = $1`, pricing.ID); err != nil {
		t.Fatal(err)
	}
	if code, b := ana.do(t, "DELETE", fmt.Sprintf("%s/%d", links(legal.ID), made[0].ID), nil); code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", code, b)
	}
	if got := e.lastLine(t, pricing.ID).Body; got != "Ana removed the link: this room no longer reads Legal" {
		t.Fatalf("line in Pricing: %q", got)
	}
	if es := e.auditLog(t, "?action="+ActionRoomLinkDelete).Entries; len(es) != 1 {
		t.Fatalf("delete audit: %+v", es)
	}
	if code, _ := ana.do(t, "DELETE", fmt.Sprintf("%s/%d", links(legal.ID), made[0].ID), nil); code != http.StatusNotFound {
		t.Fatalf("second remove: %d", code)
	}

	// 023-AC5: a link id is scoped to its rooms: one between Legal and Other is not Pricing's to
	// remove, even for an admin of all three.
	other := ana.createRoom(t, "Other", "Ana")
	code, b = ana.do(t, "POST", links(legal.ID), map[string]any{"room": other.ID, "reads": true})
	if code != http.StatusCreated {
		t.Fatalf("link Legal and Other: %d %s", code, b)
	}
	foreign := decodeJSON[[]store.RoomLink](t, b)[0]
	if code, _ := ana.do(t, "DELETE", fmt.Sprintf("%s/%d", links(pricing.ID), foreign.ID), nil); code != http.StatusNotFound {
		t.Fatalf("remove through a room the link is not of: %d", code)
	}
	if got, err := e.st.RoomLink(t.Context(), legal.ID, foreign.ID); err != nil || got.ReadRoom != other.ID {
		t.Fatalf("link after the refused remove: %+v %v", got, err)
	}
	for _, c := range []struct {
		name, method, path string
		body               any
		want               int
	}{
		{"link id not a number", "DELETE", links(legal.ID) + "/abc", nil, http.StatusNotFound},
		{"unknown other room", "POST", links(legal.ID), map[string]any{"room": "nope", "reads": true}, http.StatusForbidden},
		{"no room", "POST", links(legal.ID), map[string]any{"reads": true}, http.StatusBadRequest},
	} {
		if code, b := ana.do(t, c.method, c.path, c.body); code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, code, b, c.want)
		}
	}
	if code, b := ana.do(t, "POST", links(legal.ID), "not an object"); code != http.StatusBadRequest {
		t.Errorf("bad json: %d %s", code, b)
	}
	// 023-AC5: an admin of the reader room removes it too.
	if code, b := ana.do(t, "DELETE", fmt.Sprintf("%s/%d", links(legal.ID), foreign.ID), nil); code != http.StatusNoContent {
		t.Fatalf("reader-side remove: %d %s", code, b)
	}
}

// The link routes answer 500 when the store fails past the seat check, and log why.
func TestRoomLinkRoutesFailWithTheStore(t *testing.T) {
	e := newEnv(t)
	ana := e.device(t)
	a, b := ana.createRoom(t, "A", "Ana"), ana.createRoom(t, "B", "Ana")
	code, body := ana.do(t, "POST", "/api/rooms/"+a.ID+"/links", map[string]any{"room": b.ID, "reads": true})
	if code != http.StatusCreated {
		t.Fatalf("link: %d %s", code, body)
	}
	id := decodeJSON[[]store.RoomLink](t, body)[0].ID
	del := fmt.Sprintf("/api/rooms/%s/links/%d", b.ID, id)
	post := map[string]any{"room": b.ID, "read_by": true}
	db := e.sqlDB(t)
	for _, tc := range []struct {
		table string
		calls [][3]any
	}{
		{"room_admins", [][3]any{{"POST", "/api/rooms/" + a.ID + "/links", post}, {"DELETE", del, nil}}},
		{"room_links", [][3]any{{"GET", "/api/rooms/" + a.ID + "/links", nil}, {"POST", "/api/rooms/" + a.ID + "/links", post}, {"DELETE", del, nil}}},
	} {
		if _, err := db.ExecContext(t.Context(), `ALTER TABLE `+tc.table+` RENAME TO gone`); err != nil {
			t.Fatal(err)
		}
		for _, c := range tc.calls {
			if code, body := ana.do(t, c[0].(string), c[1].(string), c[2]); code != http.StatusInternalServerError {
				t.Errorf("%s gone, %s %s: %d %s", tc.table, c[0], c[1], code, body)
			}
		}
		if _, err := db.ExecContext(t.Context(), `ALTER TABLE gone RENAME TO `+tc.table); err != nil {
			t.Fatal(err)
		}
	}
}

// 023-AC4: the 11th read room answers 409 with the spec's sentence.
func TestRoomLinkCapRoute(t *testing.T) {
	e := newEnv(t)
	ana := e.device(t)
	main := ana.createRoom(t, "Main", "Ana")
	if _, err := e.sqlDB(t).ExecContext(t.Context(), `UPDATE profiles SET max_rooms = 20`); err != nil {
		t.Fatal(err)
	}
	for i := range store.MaxRoomReads + 1 {
		other := ana.createRoom(t, fmt.Sprintf("R%d", i), "Ana")
		code, b := ana.do(t, "POST", "/api/rooms/"+main.ID+"/links", map[string]any{"room": other.ID, "reads": true})
		if i < store.MaxRoomReads && code != http.StatusCreated {
			t.Fatalf("link %d: %d %s", i, code, b)
		}
		if i == store.MaxRoomReads && (code != http.StatusConflict || errorOf(t, b) != "This room already reads 10 rooms. Remove one first.") {
			t.Fatalf("11th: %d %s", code, b)
		}
	}
}

// linkedFixture is turn room r reading two rooms named alike, Legal and legal!, linked by Ana, an
// admin of all three.
type linkedFixture struct {
	ana           *device
	legal, legal2 store.Room
	anaLegal      int64
}

func newLinkedFixture(t *testing.T, e *env, r *turnRoom) linkedFixture {
	t.Helper()
	f := linkedFixture{ana: e.device(t)}
	f.legal = f.ana.createRoom(t, "Legal", "Ana")
	f.legal2 = f.ana.createRoom(t, "legal!", "Ana")
	pid := f.ana.join(t, r.room, "Ana").Participant.ID
	if code, b := e.do(t, "PUT", fmt.Sprintf("/api/admin/rooms/%s/admins/%d", r.room, pid), e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("admin: %d %s", code, b)
	}
	f.link(t, r, f.legal.ID)
	f.link(t, r, f.legal2.ID)
	f.anaLegal = f.say(t, e, f.legal.ID, "Contracts are signed by legal.")
	return f
}

func (f linkedFixture) link(t *testing.T, r *turnRoom, other string) store.RoomLink {
	t.Helper()
	code, b := f.ana.do(t, "POST", "/api/rooms/"+r.room+"/links", map[string]any{"room": other, "reads": true})
	if code != http.StatusCreated {
		t.Fatalf("link: %d %s", code, b)
	}
	return decodeJSON[[]store.RoomLink](t, b)[0]
}

// say posts body in room as Ana and returns its id.
func (f linkedFixture) say(t *testing.T, e *env, room, body string) int64 {
	t.Helper()
	if code, b := f.ana.do(t, "POST", "/api/rooms/"+room+"/messages", map[string]string{"body": body}); code != http.StatusCreated {
		t.Fatalf("say: %d %s", code, b)
	}
	return e.lastLine(t, room).ID
}

// 023-AC9 023-AC10 023-AC11 023-AC13 023-AC14: a turn reads its linked rooms under linked/<slug>/,
// slugs apart; their transcript stops at the turn's start, leaves deleted lines out and shows
// edits untagged; their documents are pinned at the first fetch; a room linked later, or unlinked
// during the turn, is refused like any path outside it.
func TestLinkedRoomWorkspace(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	if _, err := e.sqlDB(t).ExecContext(t.Context(), `UPDATE profiles SET max_rooms = 20`); err != nil {
		t.Fatal(err)
	}
	f := newLinkedFixture(t, e, r)
	ctx := t.Context()
	legal := f.legal.ID
	gone := f.say(t, e, legal, "Secret pricing floor.")
	edited := f.say(t, e, legal, "Draft one.")
	who := e.lastLine(t, legal).ParticipantID
	if _, _, err := e.st.EditMessage(ctx, legal, edited, who, "Draft two."); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.DeleteMessage(ctx, legal, gone, who); err != nil {
		t.Fatal(err)
	}
	doc, err := e.st.CreateAgentDocument(ctx, legal, "terms.md", "md", "# Terms v1\n", "Secretary")
	if err != nil {
		t.Fatal(err)
	}
	pdf, err := e.st.AddUpload(ctx, &store.Upload{RoomID: legal, ParticipantID: who, Author: "Ana", Filename: "nda.pdf", ContentType: "application/pdf", Format: "pdf", Data: []byte("%PDF-nda")}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	// 023-AC10: a file sent with a message deleted since stays out; one with a live message stays in.
	chat := func(name string) (msg, doc int64) {
		m := &store.Message{RoomID: legal, Kind: store.KindText, ParticipantID: who, Sender: "Ana", Body: "see " + name}
		if err := e.st.InsertMessageUploads(ctx, m, []store.Upload{{RoomID: legal, ParticipantID: who, Author: "Ana", Filename: name,
			ContentType: "text/markdown; charset=utf-8", Format: "md", Data: []byte("# " + name + "\n"), Text: "# " + name + "\n"}}, 1<<20); err != nil {
			t.Fatal(err)
		}
		return m.ID, m.Documents[0].ID
	}
	goneMsg, goneFile := chat("hidden.md")
	keptMsg, keptFile := chat("kept.md")
	if _, err := e.st.DeleteMessage(ctx, legal, goneMsg, who); err != nil {
		t.Fatal(err)
	}
	// Two transcript chunks in Legal: a partial tar must not share one chunk's buffer with the next.
	for i := range turn.TranscriptChunk {
		if err := e.st.InsertMessage(ctx, &store.Message{RoomID: legal, Kind: store.KindSystem, Sender: "Ana", Body: fmt.Sprintf("Filler %d.", i)}); err != nil {
			t.Fatal(err)
		}
	}
	hiddenPath, keptPath := fmt.Sprintf("linked/legal/uploads/chat/%d-hidden.md", goneFile), fmt.Sprintf("linked/legal/uploads/chat/%d-kept.md", keptFile)
	tn := e.press(t, r, r.alice, r.alice.p.Token, false)
	f.say(t, e, legal, "Said after the turn started.")
	annex := f.ana.createRoom(t, "Annex", "Ana")
	f.link(t, r, annex.ID)

	// 023-AC11: the prompt names each folder, its room and documents.
	prompt := e.job(t, r, r.alice, tn.ID).Prompt
	for _, want := range []string{"## Linked rooms", `- linked/legal/: "Legal", documents: docs/terms.md, ` + fmt.Sprintf("uploads/%d-nda.pdf", pdf.ID), `- linked/legal-2/: "legal!"`, "read-only"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "linked/annex") || strings.Contains(prompt, "hidden.md") {
		t.Fatal("a room linked after the turn's start, or a deleted message's file, is in its prompt")
	}

	ws := e.workspace(t, r, r.alice, tn.ID)
	tr := ""
	for name, file := range ws {
		if strings.HasPrefix(name, "linked/legal/transcript/") {
			tr += file.data
		}
	}
	for _, c := range []struct {
		text string
		in   bool
	}{
		{"Contracts are signed by legal.", true}, {"Draft two.", true}, {"(edited)", false}, {"Draft one.", false},
		{"Secret pricing floor.", false}, {"(deleted)", false}, {"Said after the turn started.", false}, {"see hidden.md", false}, {"see kept.md", true},
	} {
		if strings.Contains(tr, c.text) != c.in {
			t.Errorf("linked transcript has %q: %v, want %v\n%s", c.text, !c.in, c.in, tr)
		}
	}
	if ws["linked/legal/docs/terms.md"].data != "# Terms v1\n" || ws["linked/legal/docs/terms.md"].mode != 0o444 ||
		ws[fmt.Sprintf("linked/legal/uploads/%d-nda.pdf", pdf.ID)].data != "%PDF-nda" {
		t.Fatalf("linked files: %+v", ws)
	}
	if ws[hiddenPath] != (tarFile{}) || ws[keptPath].data != "# kept.md\n" {
		t.Fatalf("chat files: hidden %+v, kept %+v", ws[hiddenPath], ws[keptPath])
	}
	if !slices.ContainsFunc(slices.Collect(maps.Keys(ws)), func(n string) bool { return strings.HasPrefix(n, "linked/legal-2/transcript/") }) {
		t.Fatal("no linked/legal-2/ for the second room named legal")
	}
	// 023-AC10: Legal's own transcript keeps the deleted line, tagged.
	own := ""
	if err := e.srv.writeTranscript(ctx, legal, e.lastLine(t, legal).ID, "", false, nil, func(_ string, _ int64, _ blobKey, data func() ([]byte, error)) error {
		b, _ := data()
		own += string(b)
		return nil
	}); err != nil || !strings.Contains(own, "(deleted): Secret pricing floor.") || !strings.Contains(own, "(edited): Draft two.") {
		t.Fatalf("Legal's own transcript: %v\n%s", err, own)
	}

	// 023-AC13: documents stay at their revision of the first fetch.
	if _, err := e.st.AddRevision(ctx, legal, doc.ID, "# Terms v2\n", "Secretary"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.CreateAgentDocument(ctx, legal, "later.md", "md", "# Later\n", "Secretary"); err != nil {
		t.Fatal(err)
	}
	late, err := e.st.AddUpload(ctx, &store.Upload{RoomID: legal, ParticipantID: who, Author: "Ana", Filename: "late.pdf", ContentType: "application/pdf", Format: "pdf", Data: []byte("%PDF-late")}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	latePath := fmt.Sprintf("linked/legal/uploads/%d-late.pdf", late.ID)
	ws = e.workspace(t, r, r.alice, tn.ID)
	if ws["linked/legal/docs/terms.md"].data != "# Terms v1\n" || ws["linked/legal/docs/later.md"] != (tarFile{}) || ws[latePath] != (tarFile{}) {
		t.Fatalf("the linked snapshot moved: %v", slices.Sorted(maps.Keys(ws)))
	}

	ids := strconv.FormatInt(tn.ID, 10)
	fetch := func(p string) int {
		code, _ := e.do(t, "POST", r.daemonPath("turns/"+ids+"/workspace"), r.alice.tok, turn.PathsRequest{Paths: []string{p}})
		return code
	}
	manifest := func() []string {
		code, b := e.do(t, "GET", r.daemonPath("turns/"+ids+"/manifest"), r.alice.tok, nil)
		if code != http.StatusOK {
			t.Fatalf("manifest: %d %s", code, b)
		}
		var out []string
		for _, f := range decodeJSON[turn.Manifest](t, b).Files {
			out = append(out, f.Path)
		}
		return out
	}
	if got := manifest(); slices.Contains(got, latePath) || slices.Contains(got, hiddenPath) || slices.Contains(got, "linked/legal/docs/later.md") || !slices.Contains(got, keptPath) {
		t.Fatalf("manifest: %v", got)
	}
	for _, p := range []string{latePath, hiddenPath, "linked/legal/docs/later.md"} {
		if code := fetch(p); code != http.StatusBadRequest {
			t.Fatalf("%s: %d", p, code)
		}
	}
	// A partial tar of every generated file matches the full one byte for byte.
	var gen []string
	for name := range ws {
		if strings.Contains(name, "transcript/") && !strings.HasSuffix(name, "/") || name == turn.AnnotationsFile {
			gen = append(gen, name)
		}
	}
	code, b := e.do(t, "POST", r.daemonPath("turns/"+ids+"/workspace"), r.alice.tok, turn.PathsRequest{Paths: gen})
	part := untar(t, b)
	if code != http.StatusOK || len(gen) < 5 || len(part) != len(gen) {
		t.Fatalf("partial: %d %v", code, gen)
	}
	for _, name := range gen {
		if part[name] != ws[name] {
			t.Fatalf("%s differs in the partial tar", name)
		}
	}
	// 023-AC10 023-AC13: privacy wins over the snapshot: a message deleted or a document renamed in
	// Legal after the first fetch shows on the next; the path gone answers 400.
	if _, err := e.st.DeleteMessage(ctx, legal, keptMsg, who); err != nil {
		t.Fatal(err)
	}
	ndaPath := fmt.Sprintf("linked/legal/uploads/%d-nda.pdf", pdf.ID)
	renamed := fmt.Sprintf("linked/legal/uploads/%d-contract.pdf", pdf.ID)
	if _, err := e.st.RenameDocument(ctx, legal, pdf.ID, "contract.pdf", &store.Participant{ID: who, RoomID: legal, Name: "Ana"}); err != nil {
		t.Fatal(err)
	}
	ws = e.workspace(t, r, r.alice, tn.ID)
	tr = ""
	for name, file := range ws {
		if strings.HasPrefix(name, "linked/legal/transcript/") {
			tr += file.data
		}
	}
	if ws[keptPath] != (tarFile{}) || strings.Contains(tr, "see kept.md") || ws[ndaPath] != (tarFile{}) || ws[renamed].data != "%PDF-nda" {
		t.Fatalf("after a delete and a rename in Legal: %v\n%s", slices.Sorted(maps.Keys(ws)), tr)
	}
	for p, want := range map[string]int{keptPath: http.StatusBadRequest, ndaPath: http.StatusBadRequest, renamed: http.StatusOK} {
		if code := fetch(p); code != want {
			t.Fatalf("%s: %d, want %d", p, code, want)
		}
	}
	// 023-AC14: the room linked after the turn started serves nothing to it.
	if code := fetch("linked/annex/transcript/x/001.md"); code != http.StatusBadRequest {
		t.Fatalf("annex path: %d", code)
	}
	if code := fetch("linked/legal/docs/terms.md"); code != http.StatusOK {
		t.Fatalf("listed linked path: %d", code)
	}
	// 023-AC13: Legal unlinked mid-turn: its files are refused, the manifest and full tar leave it out.
	ls, err := e.st.RoomLinks(ctx, r.room, 0)
	if err != nil {
		t.Fatal(err)
	}
	if code, b := f.ana.do(t, "DELETE", fmt.Sprintf("/api/rooms/%s/links/%d", r.room, ls[0].ID), nil); code != http.StatusNoContent || ls[0].ReadRoom != legal {
		t.Fatalf("unlink: %d %s %+v", code, b, ls[0])
	}
	if code := fetch("linked/legal/docs/terms.md"); code != http.StatusBadRequest {
		t.Fatalf("unlinked path: %d", code)
	}
	// Linked again during the turn: a new link, not this turn's.
	f.link(t, r, legal)
	if code := fetch("linked/legal/docs/terms.md"); code != http.StatusBadRequest {
		t.Fatalf("relinked path: %d", code)
	}
	code, b = e.do(t, "GET", r.daemonPath("turns/"+ids+"/manifest"), r.alice.tok, nil)
	m := decodeJSON[turn.Manifest](t, b)
	if code != http.StatusOK || slices.Contains(m.Dirs, "linked/legal") || !slices.Contains(m.Dirs, "linked/legal-2/docs") ||
		slices.ContainsFunc(m.Files, func(f turn.File) bool { return strings.HasPrefix(f.Path, "linked/legal/") }) {
		t.Fatalf("manifest after unlink: %d %+v", code, m)
	}
	for name := range e.workspace(t, r, r.alice, tn.ID) {
		if strings.HasPrefix(name, "linked/legal/") {
			t.Fatalf("full tar after unlink holds %s", name)
		}
	}

	// 023-AC12: writes under linked/ are dropped and listed; the result is still accepted.
	code, b = e.result(t, r, r.alice, tn.ID, turn.Result{SessionID: sid1, Synthesis: "Read Legal.", Changes: []turn.Change{
		{Path: "linked/legal/docs/terms.md"}, {Path: "linked/legal-2/notes.md"}, {Path: "linked/legal/transcript/x.md", Deleted: true}}})
	done := decodeJSON[store.Turn](t, b)
	if code != http.StatusOK || done.Status != store.TurnDone || len(done.Report.Ignored) != 3 || done.Report.Ignored[1] != "linked/legal-2/notes.md: linked rooms are read-only" {
		t.Fatalf("result: %d %s", code, b)
	}
	if got := e.lastLine(t, r.room).Body; !strings.Contains(got, "linked/legal/docs/terms.md: linked rooms are read-only") {
		t.Fatalf("system line %q", got)
	}
	if n, _ := e.st.Documents(ctx, legal); len(n) != 6 {
		t.Fatalf("Legal's documents changed: %+v", n)
	}

	// 023-AC13: the next turn reads the rooms linked since, Legal again under its new link, last:
	// "legal!" takes the slug legal.
	next := e.press(t, r, r.alice, r.alice.p.Token, false)
	ws = e.workspace(t, r, r.alice, next.ID)
	in := func(dir string) (n int, text string) {
		for name, file := range ws {
			if strings.HasPrefix(name, dir+"/transcript/") && !strings.HasSuffix(name, "/") {
				n, text = n+1, text+file.data
			}
		}
		return n, text
	}
	annexN, _ := in("linked/annex")
	legalN, legalText := in("linked/legal")
	_, legal2Text := in("linked/legal-2")
	if annexN == 0 || legalN == 0 || strings.Contains(legalText, "Contracts") || !strings.Contains(legal2Text, "Contracts") || ws["linked/legal-2/docs/later.md"].data != "# Later\n" {
		t.Fatalf("next turn's workspace: %v", slices.Sorted(maps.Keys(ws)))
	}
}

// 023-AC9: a room's name becomes a path-safe slug.
func TestSlugOf(t *testing.T) {
	for _, c := range [][2]string{
		{"Legal", "legal"}, {"Pricing & Legal (EU)", "pricing-legal-eu"}, {"../..", "room"}, {"", "room"},
		{"Été 2026", "été-2026"}, {"a/b\\c", "a-b-c"}, {strings.Repeat("ab ", 40), strings.Trim(strings.Repeat("ab-", 20), "-")},
	} {
		if got := slugOf(c[0]); got != c[1] || !turn.ValidPath("linked/"+got) {
			t.Errorf("slugOf(%q) = %q, want %q", c[0], got, c[1])
		}
	}
	ls := []store.LinkedRoom{{Name: "Legal"}, {Name: "legal"}, {Name: "LEGAL!"}, {Name: "legal-2"}}
	linkSlugs(ls)
	if got := []string{ls[0].Slug, ls[1].Slug, ls[2].Slug, ls[3].Slug}; !slices.Equal(got, []string{"legal", "legal-2", "legal-3", "legal-2-2"}) {
		t.Fatalf("slugs %v", got)
	}
}

// 023-AC11: the prompt section shows only with a linked room.
func TestLinkedPromptSection(t *testing.T) {
	if p := buildPrompt(promptInput{Persona: "Grill", Room: "A"}); strings.Contains(p, "linked/") || strings.Contains(p, "Linked rooms") {
		t.Fatalf("no link, yet:\n%s", p)
	}
	p := buildPrompt(promptInput{Persona: "Grill", Room: "A", Linked: []linkedPrompt{{Slug: "legal", Name: "Legal"}}})
	if !strings.Contains(p, "## Linked rooms") || !strings.Contains(p, "- linked/legal/: \"Legal\"\n") {
		t.Fatalf("prompt:\n%s", p)
	}
}

// 023-AC16: every prompt tells the persona how to propose a related room, and that a member decides.
func TestPromptOffersRoomNew(t *testing.T) {
	p := buildPrompt(promptInput{Persona: "Grill", Room: "A"})
	for _, want := range []string{`writing room-new.json: {"name": `, `"brief": `, "at most 80 characters", "at most 64 KB", "One proposal per turn", "a member decides", "creates nothing"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
}

// 023-AC15: ?participant= marks the reading rooms where that person sits, among the caller's.
func TestRoomLinksPersonMember(t *testing.T) {
	e := newEnv(t)
	ana := e.device(t)
	pricing := ana.createRoom(t, "Pricing", "Ana")
	legal := ana.createRoom(t, "Legal", "Ana")
	hidden := e.createRoom(t, "Hidden")
	bob := e.device(t)
	bobID := bob.join(t, legal.ID, "Bob").Participant.ID
	bob.join(t, pricing.ID, "Bob")
	bob.join(t, hidden.ID, "Bob")
	if code, b := ana.do(t, "POST", "/api/rooms/"+legal.ID+"/links", map[string]any{"room": pricing.ID, "reads": true, "read_by": true}); code != http.StatusCreated {
		t.Fatalf("link: %d %s", code, b)
	}
	if _, err := e.sqlDB(t).ExecContext(t.Context(), `INSERT INTO room_links(reader_room_id, read_room_id, created_at) VALUES($1, $2, '2026-01-01T00:00:00Z')`, hidden.ID, legal.ID); err != nil {
		t.Fatal(err)
	}
	code, b := ana.do(t, "GET", fmt.Sprintf("/api/rooms/%s/links?participant=%d", legal.ID, bobID), nil)
	got := map[string]bool{}
	for _, l := range decodeJSON[[]store.RoomLink](t, b) {
		got[l.ReaderRoom+">"+l.ReadRoom] = l.PersonMember
	}
	// Bob sits in Pricing and Hidden; Ana only in Pricing; Legal reading Pricing is not about Legal.
	if want := map[string]bool{pricing.ID + ">" + legal.ID: true, legal.ID + ">" + pricing.ID: false, hidden.ID + ">" + legal.ID: false}; code != http.StatusOK || !maps.Equal(got, want) {
		t.Fatalf("links: %d %v, want %v", code, got, want)
	}
	if code, _ := ana.do(t, "GET", "/api/rooms/"+legal.ID+"/links?participant=x", nil); code != http.StatusBadRequest {
		t.Fatalf("bad participant: %d", code)
	}
	// Only who may remove someone asks where they sit: a plain member lists the links, no more.
	anaID := ana.join(t, hidden.ID, "Ana").Participant.ID
	if code, b := bob.do(t, "GET", fmt.Sprintf("/api/rooms/%s/links?participant=%d", legal.ID, anaID), nil); code != http.StatusForbidden {
		t.Fatalf("member asks: %d %s", code, b)
	}
	if code, _ := bob.do(t, "GET", "/api/rooms/"+legal.ID+"/links", nil); code != http.StatusOK {
		t.Fatalf("member lists: %d", code)
	}
}

// 023-AC9: a stored blob is hashed once; a generated file each time; the memo stays bounded.
func TestBlobSums(t *testing.T) {
	var c blobSums
	loads := 0
	data := func() ([]byte, error) { loads++; return []byte("abc"), nil }
	for range 3 {
		got, err := c.sum(blobKey{7, 1}, data)
		if err != nil || got.size != 3 || got.sha256 != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
			t.Fatalf("sum %+v %v", got, err)
		}
	}
	if loads != 1 {
		t.Fatalf("stored blob loaded %d times", loads)
	}
	for range 2 {
		if _, err := c.sum(blobKey{}, data); err != nil {
			t.Fatal(err)
		}
	}
	if loads != 3 {
		t.Fatalf("generated file loaded %d times, want each time", loads-1)
	}
	if _, err := c.sum(blobKey{8, 0}, func() ([]byte, error) { return nil, errors.New("gone") }); err == nil || len(c.m) != 1 {
		t.Fatalf("a failed load is kept: %v %v", err, c.m)
	}
	for i := range maxBlobSums + 1 {
		_, _ = c.sum(blobKey{int64(i + 100), 1}, data)
	}
	if len(c.m) > maxBlobSums {
		t.Fatalf("memo holds %d", len(c.m))
	}
}
