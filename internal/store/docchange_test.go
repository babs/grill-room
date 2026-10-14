package store

import (
	"errors"
	"testing"
)

// 005-AC25 005-AC26: renames keep the id and record the move; a soft delete hides the document
// from every reader but the restore list, keeps its data, frees its name; a restore over a taken
// name is refused. A plain member deletes nothing.
func TestDocumentRenameAndDelete(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	alice := joinWithProfile(t, s, "lobby", "Alice", JoinProfile{RecoveryHash: "rec", SessionHash: "sess"})
	if _, err := s.CreateProfileRoom(ctx, "r", "Room", alice.ProfileID, "Alice", "inv-r", "tok-r", "lnk-r", "lnkh-r", nil); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.Participants(ctx, "r")
	al := &ps[0]
	d, err := s.CreateAgentDocument(ctx, "r", "a.md", "md", "# A\n", "Grill")
	if err != nil {
		t.Fatal(err)
	}
	v1, _ := s.DocumentsVersion(ctx, "r")
	c, err := s.RenameDocument(ctx, "r", d.ID, "b.md", al)
	if err != nil || c.From != "a.md" || c.Document.ID != d.ID || c.Line.Body != "Alice renamed `a.md` → `b.md`" {
		t.Fatalf("rename %+v %v", c, err)
	}
	if v2, _ := s.DocumentsVersion(ctx, "r"); v2 == v1 {
		t.Fatal("the documents version did not move")
	}
	if c, err := s.RenameDocument(ctx, "r", d.ID, "b.md", al); c != nil || err != nil {
		t.Fatalf("same name: %+v %v", c, err)
	}
	rs, err := s.DocumentRenames(ctx, "r", 0)
	if err != nil || len(rs) != 1 || rs[0].From != "a.md" || rs[0].To != "b.md" {
		t.Fatalf("renames %+v %v", rs, err)
	}
	if rs, _ := s.DocumentRenames(ctx, "r", rs[0].LineID); len(rs) != 0 {
		t.Fatalf("renames past the line: %+v", rs)
	}
	bob := &Participant{ID: 999, ProfileID: 999}
	if _, err := s.SetDocumentDeleted(ctx, "r", d.ID, true, bob); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a member deletes: %v", err)
	}
	if _, err := s.SetDocumentDeleted(ctx, "r", d.ID, true, al); err != nil {
		t.Fatal(err)
	}
	if docs, _ := s.Documents(ctx, "r"); len(docs) != 0 {
		t.Fatalf("listed %+v", docs)
	}
	if _, err := s.Document(ctx, "r", d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read a deleted document: %v", err)
	}
	if _, err := s.RenameDocument(ctx, "r", d.ID, "c.md", al); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename a deleted document: %v", err)
	}
	if _, err := s.CreateAgentDocument(ctx, "r", "b.md", "md", "# New B\n", "Grill"); err != nil {
		t.Fatalf("the deleted name is free: %v", err)
	}
	if _, err := s.SetDocumentDeleted(ctx, "r", d.ID, false, al); !errors.Is(err, ErrDocNameTaken) {
		t.Fatalf("restore over a taken name: %v", err)
	}
	if del, err := s.DeletedDocuments(ctx, "r"); err != nil || len(del) != 1 || del[0].ID != d.ID || del[0].Revisions != 1 {
		t.Fatalf("deleted %+v %v", del, err)
	}
	_ = s.Close()
	for name, call := range map[string]func() error{
		"rename":  func() error { _, err := s.RenameDocument(ctx, "r", d.ID, "x.md", al); return err },
		"delete":  func() error { _, err := s.SetDocumentDeleted(ctx, "r", d.ID, true, nil); return err },
		"deleted": func() error { _, err := s.DeletedDocuments(ctx, "r"); return err },
		"renames": func() error { _, err := s.DocumentRenames(ctx, "r", 0); return err },
		"chips":   func() error { _, err := s.MessageMentions(ctx, "r", 0, 9); return err },
	} {
		if err := call(); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%s on a closed store: %v", name, err)
		}
	}
}
