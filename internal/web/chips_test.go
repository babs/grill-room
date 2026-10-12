package web

import (
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/store"
)

// 005-AC27: the page draws only the mentions the server names, and finds their words with
// mentionRe's own boundaries: room.js must hold the two character classes mentionRe holds.
func TestChipsFollowMentionRe(t *testing.T) {
	js, err := os.ReadFile("ui/room.js")
	if err != nil {
		t.Fatal(err)
	}
	// What may not come before the @, and what a name is made of.
	for _, class := range []string{`\p{L}\p{N}_@`, `\p{L}\p{N}_-`} {
		if !strings.Contains(mentionRe.String(), "["+class+"]") && !strings.Contains(mentionRe.String(), "[^"+class+"]") {
			t.Fatalf("mentionRe %s lacks %s: update room.js and this test together", mentionRe, class)
		}
		if !strings.Contains(string(js), "/["+class+"]/u") {
			t.Errorf("room.js does not test [%s]", class)
		}
	}
}

// 005-AC27: the server names a line's personas and members by mentionRe, case-insensitively, once
// each as written; unknown names, code and addresses name nobody; a persona wins over a person of
// the same name.
func TestNamed(t *testing.T) {
	personas := []store.RoomPersona{{ID: 1, PersonaFields: store.PersonaFields{Name: "Grill"}}, {ID: 2, PersonaFields: store.PersonaFields{Name: "Bob"}}}
	people := []store.Participant{{ID: 10, Name: "Ana María"}, {ID: 11, Name: "Bob"}}
	got := named("@grill and @GRILL, @Ana_María, @nobody, `@Grill`, bob@grill.com and @Bob", personas, people)
	want := []store.Named{{At: "@grill", PersonaID: 1}, {At: "@GRILL", PersonaID: 1}, {At: "@Ana_María", ParticipantID: 10}, {At: "@Bob", PersonaID: 2}}
	if !slices.Equal(got, want) {
		t.Fatalf("named %+v, want %+v", got, want)
	}
	if named("no mention here", personas, people) != nil {
		t.Fatal("named a line with no @")
	}
}

// 005-AC27: the event stream sends each line with what it names, and an edit's answer too.
func TestStreamNamesMentions(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	bob := e.join(t, e.createInvite(t, r.room).Token, "Bob Smith")
	e.say(t, r, bob.Token, "@grill ask @Bob_Smith, not @nobody")
	line := e.lastLine(t, r.room)
	ch := e.chatEvents(t, r.room, bob.Token, "")
	for {
		m := decodeJSON[store.Message](t, []byte(next(t, ch).data))
		if m.ID != line.ID {
			continue
		}
		want := []store.Named{{At: "@grill", PersonaID: r.grill.ID}, {At: "@Bob_Smith", ParticipantID: bob.Participant.ID}}
		if !slices.Equal(m.Named, want) {
			t.Fatalf("named %+v, want %+v", m.Named, want)
		}
		break
	}
	code, b := e.do(t, "PATCH", fmt.Sprintf("/api/rooms/%s/messages/%d", r.room, line.ID), bob.Token, map[string]string{"body": "@Grill only"})
	if m := decodeJSON[store.Message](t, b); code != http.StatusOK || len(m.Named) != 1 || m.Named[0].At != "@Grill" {
		t.Fatalf("edit: %d %s", code, b)
	}
}
