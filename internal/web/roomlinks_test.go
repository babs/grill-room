package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/babs/grill-room/internal/store"
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
