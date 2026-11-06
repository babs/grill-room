package store

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
)

func newRoom(t *testing.T, s *Store, id string) {
	t.Helper()
	if _, err := s.CreateRoom(context.Background(), id, id); err != nil {
		t.Fatal(err)
	}
}

func upload(room string, size int) *Upload {
	return &Upload{RoomID: room, Author: "Alice", Filename: "a.png", ContentType: "image/png", Format: "image", Data: bytes.Repeat([]byte{1}, size)}
}

// 001-AC14: the room limit counts every original in the room, and only that room.
func TestAddUploadRoomLimit(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	newRoom(t, s, "a")
	newRoom(t, s, "b")
	if _, err := s.AddUpload(ctx, upload("a", 60), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddUpload(ctx, upload("a", 40), 100); err != nil {
		t.Fatalf("exactly at the limit: %v", err)
	}
	if _, err := s.AddUpload(ctx, upload("a", 1), 100); !errors.Is(err, ErrRoomFull) {
		t.Fatalf("one byte over: got %v, want ErrRoomFull", err)
	}
	if _, err := s.AddUpload(ctx, upload("b", 100), 100); err != nil {
		t.Fatalf("other room: %v", err)
	}
	if _, err := s.AddUpload(ctx, upload("gone", 1), 100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown room: got %v, want ErrNotFound", err)
	}
	docs, err := s.Documents(ctx, "a")
	if err != nil || len(docs) != 2 {
		t.Fatalf("documents: %v, %v", docs, err)
	}
}

// 001-AC14: concurrent uploads cannot overshoot the limit between check and insert.
func TestAddUploadRoomLimitConcurrent(t *testing.T) {
	s := openTest(t)
	newRoom(t, s, "a")
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for range 20 {
		wg.Go(func() {
			if _, err := s.AddUpload(context.Background(), upload("a", 10), 55); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			} else if !errors.Is(err, ErrRoomFull) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if ok != 5 {
		t.Fatalf("%d uploads stored, want 5", ok)
	}
}

// 001-AC12 001-AC13: the original is kept byte for byte; a text format also gets revision 1.
func TestUploadKeepsOriginalAndText(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	newRoom(t, s, "a")
	orig := []byte("PK\x03\x04 docx bytes")
	d, err := s.AddUpload(ctx, &Upload{RoomID: "a", Author: "Alice", Filename: "plan.docx",
		ContentType: "application/x-docx", Format: "md", Data: orig, Text: "# Plan"}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != DocUpload || d.Revisions != 1 || d.Size != int64(len(orig)) {
		t.Fatalf("document: %+v", d)
	}
	got, err := s.Original(ctx, "a", d.ID)
	if err != nil || !bytes.Equal(got, orig) {
		t.Fatalf("original: %q, %v", got, err)
	}
	r, err := s.Revision(ctx, d.ID, 1)
	if err != nil || r.Body != "# Plan" {
		t.Fatalf("revision 1: %+v, %v", r, err)
	}
	if _, err := s.Original(ctx, "b", d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("original through another room: %v", err)
	}
	img, err := s.AddUpload(ctx, upload("a", 3), 1<<20)
	if err != nil || img.Revisions != 0 {
		t.Fatalf("image: %+v, %v", img, err)
	}
	if _, err := s.Revision(ctx, img.ID, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("image has no text revision: %v", err)
	}
	fetched, err := s.Document(ctx, "a", d.ID)
	if err != nil || fetched.ContentType != "application/x-docx" || fetched.Name != "plan.docx" {
		t.Fatalf("document: %+v, %v", fetched, err)
	}
	if _, err := s.Document(ctx, "b", d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("document through another room: %v", err)
	}
}

// 001-AC17: agent documents take numbered revisions; uploads refuse them (D30).
func TestAddRevision(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	newRoom(t, s, "a")
	newRoom(t, s, "b")
	v0, err := s.DocumentsVersion(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateAgentDocument(ctx, "a", "synthesis.md", "md", "one", "agent")
	if err != nil {
		t.Fatal(err)
	}
	v1, _ := s.DocumentsVersion(ctx, "a")
	for i, body := range []string{"two", "three"} {
		r, err := s.AddRevision(ctx, "a", d.ID, body, "agent")
		if err != nil || r.N != i+2 {
			t.Fatalf("revision %d: %+v, %v", i+2, r, err)
		}
	}
	v2, _ := s.DocumentsVersion(ctx, "a")
	if v0 == v1 || v1 == v2 {
		t.Fatalf("version did not move: %s %s %s", v0, v1, v2)
	}
	if vb, _ := s.DocumentsVersion(ctx, "b"); vb != v0 {
		t.Fatalf("other room's version moved: %s", vb)
	}
	revs, err := s.Revisions(ctx, d.ID)
	if err != nil || len(revs) != 3 || revs[2].N != 3 {
		t.Fatalf("revisions: %+v, %v", revs, err)
	}
	if r, err := s.Revision(ctx, d.ID, 2); err != nil || r.Body != "two" {
		t.Fatalf("revision 2: %+v, %v", r, err)
	}
	if _, err := s.AddRevision(ctx, "b", d.ID, "x", "agent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revision through another room: %v", err)
	}
	if _, err := s.AddRevision(ctx, "a", 999, "x", "agent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown document: %v", err)
	}
	up, err := s.AddUpload(ctx, &Upload{RoomID: "a", Author: "Alice", Filename: "u.md", ContentType: "text/markdown", Format: "md", Data: []byte("x"), Text: "x"}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddRevision(ctx, "a", up.ID, "edited", "agent"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("revision of an upload: got %v, want ErrReadOnly", err)
	}
	if r, _ := s.Revisions(ctx, up.ID); len(r) != 1 {
		t.Fatalf("upload revisions: %+v", r)
	}
	if _, err := s.CreateAgentDocument(ctx, "a", "x.pdf", "pdf", "", "agent"); err == nil {
		t.Fatal("agent pdf accepted")
	}
	if _, err := s.CreateAgentDocument(ctx, "gone", "x.md", "md", "", "agent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown room: %v", err)
	}
	if _, err := s.Original(ctx, "a", d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("agent document has no original: %v", err)
	}
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

var errAny = errors.New("any error")

// 029-AC5: a message and its uploads are stored together, in the chat folder, or not at all: a
// failure on the second of two files leaves no message, upload, document or link.
func TestInsertMessageUploads(t *testing.T) {
	two := func(second *Upload) []Upload {
		first := upload("a", 10)
		first.Filename = "first.png"
		return []Upload{*first, *second}
	}
	bad := upload("a", 10)
	bad.Format = "exe"
	cases := []struct {
		name    string
		second  *Upload
		wantErr error
	}{
		{"both stored", upload("a", 10), nil},
		{"second over the room quota", upload("a", 91), ErrRoomFull},
		{"second fails in the database", bad, errAny},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTest(t)
			ctx := context.Background()
			newRoom(t, s, "a")
			m := &Message{RoomID: "a", Kind: KindText, Sender: "Alice", Body: "see"}
			err := s.InsertMessageUploads(ctx, m, two(tc.second), 100)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatal(err)
				}
				got, err := s.Message(ctx, "a", m.ID)
				if err != nil || len(got.Documents) != 2 || got.Documents[0].Name != "first.png" || got.Documents[1].Name != "a.png" ||
					got.Documents[0].Size != 10 || got.Documents[0].Format != "image" || got.Documents[0].Deleted {
					t.Fatalf("message documents: %+v, %v", got, err)
				}
				docs, err := s.Documents(ctx, "a")
				if err != nil || len(docs) != 2 || docs[0].Folder != FolderChat || docs[1].Folder != FolderChat {
					t.Fatalf("documents: %+v, %v", docs, err)
				}
				return
			}
			var ue *UploadError
			if !errors.As(err, &ue) || ue.Index != 1 || (tc.wantErr != errAny && !errors.Is(err, tc.wantErr)) {
				t.Fatalf("got %v, want an UploadError on index 1 wrapping %v", err, tc.wantErr)
			}
			for _, tb := range []string{"messages", "uploads", "documents", "revisions", "message_documents"} {
				if n := countRows(t, s, tb); n != 0 {
					t.Errorf("%d rows in %s after the rollback", n, tb)
				}
			}
		})
	}
}

// 029-AC8 029-AC10: a message shows its documents' current names, marks a deleted one, and keeps
// them through its own edit and delete; the room's deletion takes the links.
func TestMessageDocumentsFollowChanges(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	p := seed(t, s, "a")
	m := &Message{RoomID: "a", Kind: KindText, ParticipantID: p.ID, Sender: p.Name, Body: "two files"}
	first, second := upload("a", 5), upload("a", 5)
	second.Filename = "b.png"
	if err := s.InsertMessageUploads(ctx, m, []Upload{*first, *second}, 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenameDocument(ctx, "a", m.Documents[0].ID, "renamed.png", p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDocumentDeleted(ctx, "a", m.Documents[1].ID, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EditMessage(ctx, "a", m.ID, p.ID, "edited"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteMessage(ctx, "a", m.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.Messages(ctx, "a", m.ID-1, 1)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("messages: %v, %v", msgs, err)
	}
	want := []MessageDocument{
		{ID: m.Documents[0].ID, Name: "renamed.png", Format: "image", Size: 5},
		{ID: m.Documents[1].ID, Name: "b.png", Format: "image", Size: 5, Deleted: true},
	}
	if got := msgs[0].Documents; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("documents %+v, want %+v", got, want)
	}
	if docs, err := s.Documents(ctx, "a"); err != nil || !slices.ContainsFunc(docs, func(d Document) bool { return d.ID == want[0].ID }) {
		t.Fatalf("the renamed document left the room: %+v, %v", docs, err)
	}
	if err := s.DeleteRoom(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, "message_documents"); n != 0 {
		t.Fatalf("%d links after the room's deletion", n)
	}
}
