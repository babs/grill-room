package web

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/store"
)

type formFile struct {
	name string
	data []byte
}

// postFiles posts a chat message as multipart/form-data: body, then each file as a "file" part.
func (e *env) postFiles(t *testing.T, room, token, body string, files ...formFile) (int, []byte) {
	t.Helper()
	return e.postForm(t, room, token, func(mw *multipart.Writer) {
		_ = mw.WriteField("body", body)
		for _, f := range files {
			fw, err := mw.CreateFormFile("file", f.name)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = fw.Write(f.data)
		}
	})
}

// postForm posts a chat message as the multipart/form-data fill writes.
func (e *env) postForm(t *testing.T, room, token string, fill func(*multipart.Writer)) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fill(mw)
	_ = mw.Close()
	req, err := http.NewRequestWithContext(t.Context(), "POST", e.ts.URL+"/api/rooms/"+room+"/messages", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// stored counts the room's text messages, uploads and documents.
func (e *env) stored(t *testing.T, room string) string {
	t.Helper()
	var msgs, ups, docs int
	err := e.sqlDB(t).QueryRowContext(t.Context(), `SELECT
		(SELECT count(*) FROM messages WHERE room_id = $1 AND kind = 'text'),
		(SELECT count(*) FROM uploads WHERE room_id = $1),
		(SELECT count(*) FROM documents WHERE room_id = $1)`, room).Scan(&msgs, &ups, &docs)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%d messages, %d uploads, %d documents", msgs, ups, docs)
}

// 029-AC4 029-AC7: text and files in one multipart request; the message carries its documents in
// the order sent, in the answer, the list and the live event, and they land in the chat folder.
func TestPostMessageWithFiles(t *testing.T) {
	e := newEnv(t)
	room, alice := e.member(t)
	ch := e.chatEvents(t, room.ID, alice.Token, "")
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	code, b := e.postFiles(t, room.ID, alice.Token, "look", formFile{"plan.md", []byte("# Plan")}, formFile{"shot.png", png})
	if code != http.StatusCreated {
		t.Fatalf("post: %d %s", code, b)
	}
	m := decodeJSON[store.Message](t, b)
	want := []store.MessageDocument{{Name: "plan.md", Format: "md", Size: 6}, {Name: "shot.png", Format: "image", Size: int64(len(png))}}
	check := func(where string, got []store.MessageDocument) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: documents %+v", where, got)
		}
		for i := range want {
			if got[i].ID == 0 || got[i].Name != want[i].Name || got[i].Format != want[i].Format || got[i].Size != want[i].Size || got[i].Deleted {
				t.Fatalf("%s: document %d is %+v, want %+v", where, i, got[i], want[i])
			}
		}
	}
	if m.Body != "look" {
		t.Fatalf("body %q", m.Body)
	}
	check("answer", m.Documents)
	for ev := nextMessage(t, ch); ; ev = nextMessage(t, ch) {
		if ev.ID == m.ID {
			check("live event", ev.Documents)
			break
		}
	}
	_, lb := e.get(t, "/api/rooms/"+room.ID+"/messages", alice.Token)
	list := decodeJSON[struct{ Messages []store.Message }](t, lb)
	check("list", list.Messages[len(list.Messages)-1].Documents)
	for _, d := range e.documents(t, room.ID, alice.Token) {
		if d.Folder != store.FolderChat {
			t.Fatalf("document %+v is not in the chat folder", d)
		}
	}

	// Files and no text are a message; neither is a 400 as with JSON.
	code, b = e.postFiles(t, room.ID, alice.Token, "  ", formFile{"only.md", []byte("# Only")})
	if m := decodeJSON[store.Message](t, b); code != http.StatusCreated || m.Body != "" || len(m.Documents) != 1 {
		t.Fatalf("files without text: %d %s", code, b)
	}
	if code, b := e.postFiles(t, room.ID, alice.Token, ""); code != http.StatusBadRequest || !strings.Contains(string(b), "message must be 1 to") {
		t.Fatalf("neither text nor file: %d %s", code, b)
	}
	if code, b := e.postFiles(t, room.ID, alice.Token, "text only"); code != http.StatusCreated || strings.Contains(string(b), `"documents"`) {
		t.Fatalf("multipart text only: %d %s", code, b)
	}

	// The JSON body works as before, without documents, and adds none.
	before := len(e.documents(t, room.ID, alice.Token))
	code, b = e.do(t, "POST", "/api/rooms/"+room.ID+"/messages", alice.Token, map[string]string{"body": "plain"})
	if code != http.StatusCreated || strings.Contains(string(b), `"documents"`) || decodeJSON[store.Message](t, b).Body != "plain" {
		t.Fatalf("json post: %d %s", code, b)
	}
	if code, _ := e.do(t, "POST", "/api/rooms/"+room.ID+"/messages", alice.Token, map[string]string{"body": " "}); code != http.StatusBadRequest {
		t.Fatalf("json empty body: %d, want 400", code)
	}
	if n := len(e.documents(t, room.ID, alice.Token)); n != before {
		t.Fatalf("a JSON message added documents: %d, was %d", n, before)
	}
	// Uploads from the Documents pane stay at the root.
	if code, b := e.upload(t, room.ID, alice.Token, "root.md", []byte("# Root"), nil); code != http.StatusCreated || decodeJSON[store.Document](t, b).Folder != "" {
		t.Fatalf("pane upload: %d %s", code, b)
	}
}

// 029-AC5: any refused file refuses the whole message, naming the file, and stores nothing.
func TestPostMessageFileRefusals(t *testing.T) {
	docx := fixture(t, "fixture.docx")
	ok := formFile{"ok.md", []byte("# ok")}
	cases := []struct {
		name     string
		prefill  int
		files    []formFile
		want     int
		contains string
	}{
		{"format", 0, []formFile{ok, {"notes.txt", []byte("hello")}}, http.StatusUnsupportedMediaType, "notes.txt: unsupported file type"},
		{"file size", 0, []formFile{ok, {"big.md", md(20001)}}, http.StatusRequestEntityTooLarge, "big.md: file is over the 20000 bytes limit"},
		{"room quota", 20000, []formFile{{"a.md", md(20000)}, {"b.md", md(10001)}}, http.StatusRequestEntityTooLarge,
			"b.md: this upload would take the room over its 50000 bytes limit"},
		{"conversion", 0, []formFile{ok, {"x.docx", append(docx, "pandoc-fails"...)}}, http.StatusUnprocessableEntity, "could not convert x.docx to Markdown"},
		{"pandoc missing", 0, []formFile{ok, {"x.docx", append(docx, "pandoc-missing"...)}}, http.StatusServiceUnavailable, "x.docx: document conversion is unavailable"},
		{"five files", 0, []formFile{ok, ok, ok, ok, ok}, http.StatusBadRequest, "at most 4 files per message"},
	}
	e := newEnv(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			room, alice := e.member(t)
			if tc.prefill > 0 {
				if code, b := e.upload(t, room.ID, alice.Token, "fill.md", md(tc.prefill), nil); code != http.StatusCreated {
					t.Fatalf("prefill: %d %s", code, b)
				}
			}
			before := e.stored(t, room.ID)
			code, b := e.postFiles(t, room.ID, alice.Token, "with files", tc.files...)
			if code != tc.want || !strings.Contains(string(b), tc.contains) {
				t.Fatalf("got %d %s, want %d containing %q", code, b, tc.want, tc.contains)
			}
			if after := e.stored(t, room.ID); after != before {
				t.Fatalf("stored %s after the refusal, had %s", after, before)
			}
		})
	}
}

// 029-AC5: a failure inside the transaction on the second of two files leaves no message, no
// upload and no document; the room quota is re-checked there, naming the file.
func TestPostMessageFilesRollback(t *testing.T) {
	e := newEnv(t)
	room, alice := e.member(t)
	db := e.sqlDB(t)
	if _, err := db.ExecContext(t.Context(), `CREATE FUNCTION fail_second() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.name = 'second.md' THEN RAISE EXCEPTION 'injected'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER fail_second BEFORE INSERT ON documents FOR EACH ROW EXECUTE FUNCTION fail_second()`); err != nil {
		t.Fatal(err)
	}
	code, b := e.postFiles(t, room.ID, alice.Token, "two", formFile{"first.md", []byte("# 1")}, formFile{"second.md", []byte("# 2")})
	if code != http.StatusInternalServerError {
		t.Fatalf("got %d %s, want 500", code, b)
	}
	if got := e.stored(t, room.ID); got != "0 messages, 0 uploads, 0 documents" {
		t.Fatalf("after the rollback: %s", got)
	}
	if _, err := db.ExecContext(t.Context(), `DROP TRIGGER fail_second ON documents; DROP FUNCTION fail_second()`); err != nil {
		t.Fatal(err)
	}

	// The early check passes, then the room fills before the transaction: its own check refuses.
	if _, err := db.ExecContext(t.Context(), `CREATE FUNCTION fill_room() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.filename = 'first.md' THEN UPDATE rooms SET quota_bytes = 5 WHERE id = NEW.room_id; END IF; RETURN NEW; END $$;
		CREATE TRIGGER fill_room BEFORE INSERT ON uploads FOR EACH ROW EXECUTE FUNCTION fill_room()`); err != nil {
		t.Fatal(err)
	}
	code, b = e.postFiles(t, room.ID, alice.Token, "two", formFile{"first.md", []byte("# 1")}, formFile{"second.md", []byte("# 2")})
	if code != http.StatusRequestEntityTooLarge || !strings.Contains(string(b), "second.md: this upload would take the room over its") {
		t.Fatalf("got %d %s, want 413 naming second.md", code, b)
	}
	if got := e.stored(t, room.ID); got != "0 messages, 0 uploads, 0 documents" {
		t.Fatalf("after the in-transaction quota refusal: %s", got)
	}
}

// 029-AC8 029-AC10: the message shows a renamed document's new name and marks a deleted one;
// editing or deleting the message keeps its documents in the room.
func TestMessageDocumentsRenameDeleteEdit(t *testing.T) {
	e := newEnv(t)
	room, alice := e.member(t)
	code, b := e.postFiles(t, room.ID, alice.Token, "two", formFile{"a.md", []byte("# a")}, formFile{"b.md", []byte("# b")})
	if code != http.StatusCreated {
		t.Fatalf("post: %d %s", code, b)
	}
	m := decodeJSON[store.Message](t, b)
	ctx := t.Context()
	if _, err := e.st.RenameDocument(ctx, room.ID, m.Documents[0].ID, "renamed.md", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.SetDocumentDeleted(ctx, room.ID, m.Documents[1].ID, true, nil); err != nil {
		t.Fatal(err)
	}
	msgPath := "/api/rooms/" + room.ID + "/messages/" + strconv.FormatInt(m.ID, 10)
	code, b = e.do(t, "PATCH", msgPath, alice.Token, map[string]string{"body": "edited"})
	if code != http.StatusOK {
		t.Fatalf("edit: %d %s", code, b)
	}
	got := decodeJSON[store.Message](t, b).Documents
	if len(got) != 2 || got[0].Name != "renamed.md" || got[0].Deleted || got[1].Name != "b.md" || !got[1].Deleted {
		t.Fatalf("edited message documents: %+v", got)
	}
	if code, b := e.do(t, "DELETE", msgPath, alice.Token, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, b)
	}
	_, lb := e.get(t, "/api/rooms/"+room.ID+"/messages", alice.Token)
	for _, raw := range decodeJSON[struct{ Messages []map[string]any }](t, lb).Messages {
		if raw["id"] == float64(m.ID) {
			if _, ok := raw["documents"]; ok || raw["deleted_at"] == nil {
				t.Fatalf("deleted message in the list: %v", raw)
			}
		}
	}
	docs := e.documents(t, room.ID, alice.Token)
	if len(docs) != 1 || docs[0].ID != m.Documents[0].ID || docs[0].Name != "renamed.md" {
		t.Fatalf("documents after the message's deletion: %+v", docs)
	}
	var links int
	if err := e.sqlDB(t).QueryRowContext(ctx, `SELECT count(*) FROM message_documents WHERE message_id = $1`, m.ID).Scan(&links); err != nil || links != 2 {
		t.Fatalf("%d links after the message's deletion: %v", links, err)
	}
}

// 029-AC4: each file of a message takes an upload slot, as on the documents route; a text-only
// message takes none, a missing slot stores nothing, and every slot comes back on every path.
func TestPostMessageFilesSlots(t *testing.T) {
	e := newEnv(t)
	room, alice := e.member(t)
	files := func(n int) []formFile {
		out := make([]formFile, n)
		for i := range out {
			out[i] = formFile{fmt.Sprintf("f%d.md", i), []byte("# f")}
		}
		return out
	}
	cases := []struct {
		name  string
		free  int
		files []formFile
		want  int
	}{
		{"no free slot, one file", 0, files(1), http.StatusServiceUnavailable},
		{"no free slot, text only", 0, nil, http.StatusCreated},
		{"one free slot, two files", 1, files(2), http.StatusServiceUnavailable},
		{"four free slots, four files", 4, files(4), http.StatusCreated},
		{"refused file", 4, []formFile{{"notes.txt", []byte("hello")}}, http.StatusUnsupportedMediaType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			taken := cap(e.srv.uploads) - tc.free
			for range taken {
				e.srv.uploads <- struct{}{}
			}
			// Empty whatever is held, so a failed case cannot block the next one's fill.
			t.Cleanup(func() {
				for len(e.srv.uploads) > 0 {
					<-e.srv.uploads
				}
			})
			before := e.stored(t, room.ID)
			code, b := e.postFiles(t, room.ID, alice.Token, "x", tc.files...)
			if code != tc.want {
				t.Fatalf("got %d %s, want %d", code, b, tc.want)
			}
			if code == http.StatusServiceUnavailable && (!strings.Contains(string(b), "too many uploads in progress") || e.stored(t, room.ID) != before) {
				t.Fatalf("503 %s, stored %s, had %s", b, e.stored(t, room.ID), before)
			}
			if n := len(e.srv.uploads); n != taken {
				t.Fatalf("%d slots held after the request, the test holds %d", n, taken)
			}
		})
	}
}

// 029-AC4: the whole body has a size limit named in the 413, which a body field tripping it
// answers too; one body field per message.
func TestPostMessageFormLimits(t *testing.T) {
	e := newEnv(t)
	room, alice := e.member(t)
	limit := maxMessageFiles*(e.srv.opts.MaxUploadBytes+multipartSlack) + 4*maxBodyLen + multipartSlack
	tooLarge := "message is over the " + sizeText(limit) + " limit"
	pad := func(n int64) func(*multipart.Writer) {
		return func(mw *multipart.Writer) { _ = mw.WriteField("pad", string(md(int(n)))) }
	}
	cases := []struct {
		name     string
		fill     func(*multipart.Writer)
		want     int
		contains string
	}{
		{"skipped field over the limit", pad(limit + 1), http.StatusRequestEntityTooLarge, tooLarge},
		{"body field trips the limit", func(mw *multipart.Writer) {
			pad(limit - 1000)(mw)
			_ = mw.WriteField("body", string(md(4000)))
		}, http.StatusRequestEntityTooLarge, tooLarge},
		{"body over the byte cap is refused, not truncated", func(mw *multipart.Writer) {
			_ = mw.WriteField("body", " "+strings.Repeat("😀", maxBodyLen)+" and this tail is lost")
		}, http.StatusBadRequest, bodyLenText},
		{"two body fields", func(mw *multipart.Writer) {
			_ = mw.WriteField("body", "a")
			_ = mw.WriteField("body", "b")
		}, http.StatusBadRequest, "one body field per message"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, b := e.postForm(t, room.ID, alice.Token, tc.fill); code != tc.want || !strings.Contains(string(b), tc.contains) {
				t.Fatalf("got %d %s, want %d containing %q", code, b, tc.want, tc.contains)
			}
		})
	}
	if got := e.stored(t, room.ID); got != "0 messages, 0 uploads, 0 documents" {
		t.Fatalf("stored %s", got)
	}
}

// 029-AC4: each file part restarts the read deadline, so a message whose files each arrive in
// time is read whole, though the request takes longer than one deadline.
func TestPostMessageFilesDeadline(t *testing.T) {
	e := newEnv(t)
	room, alice := e.member(t)
	old := uploadReadTimeout
	uploadReadTimeout = 2 * time.Second
	t.Cleanup(func() { uploadReadTimeout = old })
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("body", "slow")
	for _, n := range []string{"a.md", "b.md"} {
		fw, _ := mw.CreateFormFile("file", n)
		_, _ = fw.Write([]byte("# " + n))
	}
	_ = mw.Close()
	all := buf.Bytes()
	// Cut before b.md's part, then after its headers: the server sees the part start at the second write.
	b := bytes.Index(all, []byte(`filename="b.md"`))
	if b < 0 {
		t.Fatal("no b.md part")
	}
	cut1 := bytes.LastIndex(all[:b], []byte("\r\n--"+mw.Boundary()))
	cut2 := cut1 + bytes.Index(all[cut1:], []byte("\r\n\r\n")) + 4
	pr, pw := io.Pipe()
	go func() {
		// Each pause stays 0.6 s inside one deadline; both together pass it by 0.8 s.
		_, _ = pw.Write(all[:cut1])
		time.Sleep(1400 * time.Millisecond)
		_, _ = pw.Write(all[cut1:cut2])
		time.Sleep(1400 * time.Millisecond)
		_, _ = pw.Write(all[cut2:])
		_ = pw.Close()
	}()
	req, _ := http.NewRequestWithContext(t.Context(), "POST", e.ts.URL+"/api/rooms/"+room.ID+"/messages", pr)
	req.Header.Set("Authorization", "Bearer "+alice.Token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if b, _ := io.ReadAll(resp.Body); resp.StatusCode != http.StatusCreated {
		t.Fatalf("slow message: %d %s, want 201", resp.StatusCode, b)
	}
}
