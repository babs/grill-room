package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

func (e *env) docNames(t *testing.T, r *turnRoom, tok string) []string {
	t.Helper()
	var names []string
	for _, d := range e.documents(t, r.room, tok) {
		names = append(names, d.Name)
	}
	return names
}

// 005-AC25: any member renames a document, which keeps its id, revisions and annotations. The name
// keeps its extension, passes an upload's checks and is the room's only one. The chat says it, the
// workspace moves the file, and a resumed persona's next turn hears the old and new paths. An
// agent document is not renamed while a turn runs.
func TestRenameDocument(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	tok := r.alice.p.Token
	e.finish(t, r, r.alice, e.press(t, r, r.alice, tok, false), "first")
	rename := func(doc int64, name string, want int) {
		t.Helper()
		if code, b := e.do(t, "PATCH", fmt.Sprintf("/api/rooms/%s/documents/%d", r.room, doc), tok, map[string]string{"name": name}); code != want {
			t.Fatalf("rename %d to %q: %d %s, want %d", doc, name, code, b, want)
		}
	}
	if _, err := e.st.CreateAgentDocument(t.Context(), r.room, "other.md", "md", "# Other\n", store.AgentName); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]int{
		"roadmap.md": http.StatusBadRequest, "a/b.docx": http.StatusBadRequest, " ": http.StatusBadRequest,
		strings.Repeat("x", maxFileName) + ".docx": http.StatusBadRequest,
	} {
		rename(r.office, name, want)
	}
	rename(r.doc, "other.md", http.StatusConflict)
	rename(r.doc, ".hidden.md", http.StatusBadRequest)
	rename(999999, "x.md", http.StatusNotFound)
	rename(r.office, "Roadmap.DOCX", http.StatusNoContent)
	if l := e.lastLine(t, r.room); l.Body != "Alice renamed `plan.docx` → `Roadmap.DOCX`" || l.Kind != store.KindSystem {
		t.Fatalf("line %+v", l)
	}
	rename(r.doc, "summary.md", http.StatusNoContent)
	if code, b := e.do(t, "GET", fmt.Sprintf("/api/rooms/%s/documents/%d/annotations", r.room, r.doc), tok, nil); code != http.StatusOK || !strings.Contains(string(b), "which fox?") {
		t.Fatalf("annotations after the rename: %d %s", code, b)
	}

	tn := e.press(t, r, r.alice, tok, false)
	prompt := e.job(t, r, r.alice, tn.ID).Prompt
	for _, want := range []string{"## Documents renamed since your last turn",
		fmt.Sprintf("- `uploads/%d-plan.docx.md` → `uploads/%d-Roadmap.DOCX.md`\n", r.office, r.office),
		"- `docs/synthesis.md` → `docs/summary.md`\n"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	files := e.workspace(t, r, r.alice, tn.ID)
	if _, ok := files["docs/summary.md"]; !ok {
		t.Error("the renamed document is not in the workspace")
	}
	if _, ok := files["docs/synthesis.md"]; ok {
		t.Error("the old path stayed in the workspace")
	}
	// An agent document stays put while a turn runs; an upload is read-only to turns.
	rename(r.doc, "late.md", http.StatusConflict)
	rename(r.pdf, "brief-v2.pdf", http.StatusNoContent)
	e.finish(t, r, r.alice, tn, "second")

	// A new session reads the workspace afresh: no list of moves.
	rename(r.doc, "final.md", http.StatusNoContent)
	tn = e.press(t, r, r.alice, tok, true)
	if p := e.job(t, r, r.alice, tn.ID).Prompt; strings.Contains(p, "Documents renamed") {
		t.Errorf("a new session got the moves:\n%s", p)
	}
}

// 005-AC26: a room admin deletes a document, softly: it leaves the list, the workspace and the
// threads the agents read, the chat says so, and its data still counts in the volume. A room
// admin restores it, unless another document took its name. A plain member does neither, and an
// agent document is not deleted while a turn runs. Both actions are audited.
func TestDeleteDocument(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	tok := r.alice.p.Token
	bob := e.join(t, e.createInvite(t, r.room).Token, "Bob")
	path := func(doc int64, suffix string) string {
		return fmt.Sprintf("/api/rooms/%s/documents/%d%s", r.room, doc, suffix)
	}
	if code, _ := e.do(t, "DELETE", path(r.doc, ""), bob.Token, nil); code != http.StatusForbidden {
		t.Fatalf("a member deletes: %d", code)
	}
	if code, _ := e.do(t, "GET", "/api/rooms/"+r.room+"/documents/deleted", bob.Token, nil); code != http.StatusForbidden {
		t.Fatalf("a member lists deleted: %d", code)
	}
	if code, _ := e.do(t, "PUT", fmt.Sprintf("/api/rooms/%s/admins/%d", r.room, r.alice.p.Participant.ID), e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("appoint: %d", code)
	}
	volume := func() int64 {
		o, err := e.st.RoomOverview(t.Context(), r.room, 1<<30)
		if err != nil {
			t.Fatal(err)
		}
		// Turns add sessions and transcript lines meanwhile: the documents' own data only.
		return o.Volume.Uploads + o.Volume.Revisions
	}
	before := volume()

	tn := e.press(t, r, r.alice, tok, false)
	if code, _ := e.do(t, "DELETE", path(r.doc, ""), tok, nil); code != http.StatusConflict {
		t.Fatalf("an agent document deleted mid-turn: %d", code)
	}
	if code, _ := e.do(t, "DELETE", path(r.pdf, ""), tok, nil); code != http.StatusNoContent {
		t.Fatalf("an upload deleted mid-turn: %d", code)
	}
	e.finish(t, r, r.alice, tn, "done")
	if code, _ := e.do(t, "DELETE", path(r.doc, ""), tok, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if l := e.lastLine(t, r.room); l.Body != "Alice deleted the document `synthesis.md`" {
		t.Fatalf("line %+v", l)
	}
	if names := e.docNames(t, r, tok); strings.Contains(strings.Join(names, ","), "synthesis.md") || strings.Contains(strings.Join(names, ","), "brief.pdf") {
		t.Fatalf("listed %v", names)
	}
	if code, _ := e.do(t, "GET", path(r.doc, ""), tok, nil); code != http.StatusNotFound {
		t.Fatalf("a deleted document reads: %d", code)
	}
	if v := volume(); v != before {
		t.Fatalf("volume %d, was %d", v, before)
	}
	tn = e.press(t, r, r.alice, tok, false)
	files := e.workspace(t, r, r.alice, tn.ID)
	for name := range files {
		if strings.Contains(name, "synthesis") || strings.Contains(name, "brief") {
			t.Errorf("workspace holds %s", name)
		}
	}
	if strings.Contains(files["annotations.md"].data, "which fox?") {
		t.Error("the deleted document's thread reached the agent")
	}
	// The agent writes the name again: a new document, the deleted one untouched.
	res := turn.Result{Synthesis: "rewrote it", SessionID: sid1, SessionOffset: 2, Session: []byte("y\n"),
		Changes: []turn.Change{{Path: "docs/synthesis.md", Content: []byte("# New\n")}}}
	if code, b := e.result(t, r, r.alice, tn.ID, res); code != http.StatusOK {
		t.Fatalf("result: %d %s", code, b)
	}
	code, b := e.do(t, "GET", "/api/rooms/"+r.room+"/documents/deleted", tok, nil)
	deleted := decodeJSON[[]store.Document](t, b)
	if code != http.StatusOK || len(deleted) != 2 || deleted[0].ID != r.pdf || deleted[1].ID != r.doc {
		t.Fatalf("deleted: %d %s", code, b)
	}
	if code, _ := e.do(t, "POST", path(r.doc, "/restore"), tok, nil); code != http.StatusConflict {
		t.Fatalf("restore over a taken name: %d", code)
	}
	if code, _ := e.do(t, "POST", path(r.pdf, "/restore"), tok, nil); code != http.StatusNoContent {
		t.Fatalf("restore: %d", code)
	}
	if l := e.lastLine(t, r.room); l.Body != "Alice restored the document `brief.pdf`" {
		t.Fatalf("line %+v", l)
	}
	if code, _ := e.do(t, "POST", path(r.pdf, "/restore"), tok, nil); code != http.StatusNotFound {
		t.Fatalf("restore a live document: %d", code)
	}
	if got := e.auditLog(t, "?target="+r.room).Entries; len(got) < 3 || got[0].Action != ActionDocumentRestore || got[1].Action != ActionDocumentDelete {
		t.Fatalf("audit %+v", got)
	}
}
