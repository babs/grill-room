package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/store"
)

// 005-AC1 005-AC21: a participant renames themselves with the validation of a join, and may make
// it their profile's name; a room admin and the super-admin rename anyone, audited; a plain member
// renames nobody else. Each rename says so in the chat, and past lines keep the name they had.
func TestRenameParticipant(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Pricing", "Alice")
	path := "/api/rooms/" + room.ID + "/participants/%d/name"
	aliceP := alice.profile(t).Profile.Rooms[0].ParticipantID
	bob := e.device(t)
	bobP := bob.join(t, room.ID, "Bob").Participant
	if code, b := bob.do(t, "POST", "/api/rooms/"+room.ID+"/messages", map[string]string{"body": "hello"}); code != http.StatusCreated {
		t.Fatalf("post: %d %s", code, b)
	}
	rename := func(d *device, target int64, body map[string]any, want int) {
		t.Helper()
		if code, b := d.do(t, "PUT", fmt.Sprintf(path, target), body); code != want {
			t.Fatalf("rename %d to %v: %d %s, want %d", target, body, code, b, want)
		}
	}
	for _, bad := range []string{"", "  ", strings.Repeat("x", maxNameLen+1)} {
		rename(bob, bobP.ID, map[string]any{"name": bad}, http.StatusBadRequest)
	}
	rename(bob, bobP.ID, map[string]any{"name": " Robert ", "profile": true}, http.StatusNoContent)
	if l := e.lastLine(t, room.ID); l.Kind != store.KindSystem || l.Body != "Bob is now Robert" || l.ParticipantID != bobP.ID {
		t.Fatalf("self rename line: %+v", l)
	}
	if got := bob.profile(t).Profile; got.Name != "Robert" || got.Rooms[0].Name != "Robert" {
		t.Fatalf("profile after rename: %+v", got)
	}
	msgs, err := e.st.Messages(t.Context(), room.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if hello := msgs[len(msgs)-2]; hello.Body != "hello" || hello.Sender != "Bob" {
		t.Fatalf("a past line keeps its name: %+v", hello)
	}
	// The same name again writes no line.
	n := len(msgs)
	rename(bob, bobP.ID, map[string]any{"name": "Robert"}, http.StatusNoContent)
	if msgs, _ = e.st.Messages(t.Context(), room.ID, 0, 100); len(msgs) != n {
		t.Fatalf("a no-op rename wrote %d lines", len(msgs)-n)
	}

	// Bob, a plain member, renames nobody else; Alice, room admin, does.
	rename(bob, aliceP, map[string]any{"name": "Al"}, http.StatusForbidden)
	rename(alice, bobP.ID, map[string]any{"name": "Rob", "profile": true}, http.StatusNoContent)
	if l := e.lastLine(t, room.ID); l.Body != "Alice renamed Robert to Rob" {
		t.Fatalf("admin rename line: %+v", l)
	}
	// The option names only the renamer's own profile.
	if bob.profile(t).Profile.Name != "Robert" || alice.profile(t).Profile.Name != "" {
		t.Fatal("a room admin's rename changed a profile's name")
	}
	rename(alice, 999999, map[string]any{"name": "X"}, http.StatusNotFound)
	if code, _ := e.do(t, "PUT", fmt.Sprintf("/api/admin/rooms/%s/participants/%d/name", room.ID, bobP.ID), e.admin, map[string]string{"name": "Bobby"}); code != http.StatusNoContent {
		t.Fatalf("super-admin rename: %d", code)
	}
	if l := e.lastLine(t, room.ID); l.Body != "The admin renamed Rob to Bobby" {
		t.Fatalf("super-admin rename line: %+v", l)
	}
	log := e.auditLog(t, "?action="+ActionRename).Entries
	if len(log) != 2 || log[0].Actor != store.ActorSuperAdmin || !strings.HasPrefix(log[1].Actor, ActorProfile) ||
		!strings.Contains(string(log[1].Details), `"from":"Robert","participant":`+fmt.Sprint(bobP.ID)+`,"to":"Rob"`) {
		t.Fatalf("rename audit: %+v", log)
	}
	// A self-rename is no admin action, a room admin's own included.
	rename(alice, aliceP, map[string]any{"name": "Ali"}, http.StatusNoContent)
	if got := e.auditLog(t, "?action="+ActionRename).Entries; len(got) != 2 {
		t.Fatalf("self rename audited: %+v", got)
	}
	// 009-AC36: the room keeps the name before the latest rename, for the details card.
	_, b := bob.do(t, "GET", "/api/rooms/"+room.ID, nil)
	earlier := map[string]string{}
	for _, p := range decodeJSON[roomResp](t, b).Participants {
		earlier[p.Name] = p.PreviousName
	}
	if earlier["Bobby"] != "Rob" || earlier["Ali"] != "Alice" || len(earlier) != 2 {
		t.Fatalf("earlier names %v", earlier)
	}
}

// 005-AC1: the short id the presence preamble shows after a human's name comes from their profile,
// so it stays the same across a rename.
func TestRenameKeepsHumanTag(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Pricing", "Alice")
	bob := e.device(t)
	bobP := bob.join(t, room.ID, "Bob").Participant
	tag := func() (string, string) {
		ps, err := e.st.Participants(t.Context(), room.ID)
		if err != nil {
			t.Fatal(err)
		}
		tags := humanTags(ps)
		for _, p := range ps {
			if p.ID == bobP.ID {
				return tagged(p.Name, tags[p.ID]), tags[p.ID]
			}
		}
		t.Fatal("bob gone")
		return "", ""
	}
	before, id := tag()
	if code, b := bob.do(t, "PUT", fmt.Sprintf("/api/rooms/%s/participants/%d/name", room.ID, bobP.ID), map[string]string{"name": "Robert"}); code != http.StatusNoContent {
		t.Fatalf("rename: %d %s", code, b)
	}
	after, id2 := tag()
	if id == "" || strings.HasPrefix(id, "#") || id != id2 || before != "Bob ·"+id || after != "Robert ·"+id {
		t.Fatalf("tag across the rename: %q -> %q", before, after)
	}
}
