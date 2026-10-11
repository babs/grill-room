package web

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

var update = flag.Bool("update", false, "rewrite the golden files from the current output")

// chatEvents opens the room stream at query and keeps messages and message_edited events only.
func (e *env) chatEvents(t *testing.T, room, token, query string) <-chan sseEvent {
	t.Helper()
	ch, _ := e.openStream(t, "/api/rooms/"+room+"/events"+query, token, "")
	out := make(chan sseEvent, 64)
	go func() {
		defer close(out)
		for ev := range ch {
			if ev.event == "" || ev.event == "message_edited" {
				out <- ev
			}
		}
	}()
	return out
}

func nextEdit(t *testing.T, ch <-chan sseEvent) store.Message {
	t.Helper()
	ev := next(t, ch)
	if ev.event != "message_edited" {
		t.Fatalf("got %+v, want message_edited", ev)
	}
	return decodeJSON[store.Message](t, []byte(ev.data))
}

// 002-AC8 002-AC9 002-AC10 002-AC11: authors edit and delete their own lines, voice included,
// everyone sees it live, and nobody else can.
func TestEditAndDeleteMessages(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "R").ID
	alice := e.join(t, e.createInvite(t, room).Token, "Alice")
	bob := e.join(t, e.createInvite(t, room).Token, "Bob")
	path := "/api/rooms/" + room + "/messages"
	msgPath := func(id int64) string { return path + "/" + strconv.FormatInt(id, 10) }

	code, b := e.do(t, "POST", path, alice.Token, map[string]string{"body": "**hi** <script>x</script>"})
	if code != http.StatusCreated {
		t.Fatalf("post: %d %s", code, b)
	}
	text := decodeJSON[store.Message](t, b)
	if text.Body != "**hi** <script>x</script>" || text.HTML != "<p><strong>hi</strong> &lt;script&gt;x&lt;/script&gt;</p>\n" {
		t.Fatalf("a message carries its source and its render: %+v", text)
	}
	voice := &store.Message{RoomID: room, Kind: store.KindVoice, ParticipantID: alice.Participant.ID, Sender: "Alice", Body: "wreck a nice beach"}
	if err := e.bus.Post(t.Context(), voice); err != nil {
		t.Fatal(err)
	}
	ch := e.chatEvents(t, room, bob.Token, "")
	for m := nextMessage(t, ch); m.ID != voice.ID; m = nextMessage(t, ch) {
	}

	for _, id := range []int64{text.ID, voice.ID} {
		if code, b := e.do(t, "PATCH", msgPath(id), bob.Token, map[string]string{"body": "mine now"}); code != http.StatusForbidden {
			t.Fatalf("someone else's edit: %d %s", code, b)
		}
		if code, b := e.do(t, "DELETE", msgPath(id), bob.Token, nil); code != http.StatusForbidden {
			t.Fatalf("someone else's delete: %d %s", code, b)
		}
	}
	for name, tc := range map[string]struct {
		method, path string
		body         any
		want         int
	}{
		"bad id":       {"PATCH", path + "/x", map[string]string{"body": "y"}, http.StatusNotFound},
		"unknown id":   {"PATCH", msgPath(voice.ID + 100), map[string]string{"body": "y"}, http.StatusNotFound},
		"empty body":   {"PATCH", msgPath(text.ID), map[string]string{"body": " "}, http.StatusBadRequest},
		"bad json":     {"PATCH", msgPath(text.ID), "x", http.StatusBadRequest},
		"delete bad":   {"DELETE", path + "/x", nil, http.StatusNotFound},
		"versions bad": {"GET", path + "/x/versions", nil, http.StatusNotFound},
		"no token":     {"PATCH", msgPath(text.ID), map[string]string{"body": "y"}, http.StatusUnauthorized},
	} {
		tok := alice.Token
		if name == "no token" {
			tok = ""
		}
		if code, b := e.do(t, tc.method, tc.path, tok, tc.body); code != tc.want {
			t.Errorf("%s: %d %s, want %d", name, code, b, tc.want)
		}
	}

	if code, b := e.do(t, "PATCH", msgPath(voice.ID), alice.Token, map[string]string{"body": "recognize speech"}); code != http.StatusOK {
		t.Fatalf("voice correction: %d %s", code, b)
	}
	if m := nextEdit(t, ch); m.ID != voice.ID || m.Body != "recognize speech" || m.EditedAt == nil || m.HTML != "<p>recognize speech</p>\n" {
		t.Fatalf("live correction: %+v", m)
	}
	if code, b := e.do(t, "PATCH", msgPath(text.ID), alice.Token, map[string]string{"body": "_hello_"}); code != http.StatusOK {
		t.Fatalf("edit: %d %s", code, b)
	}
	if m := nextEdit(t, ch); m.ID != text.ID || m.HTML != "<p><em>hello</em></p>\n" {
		t.Fatalf("live edit: %+v", m)
	}
	code, b = e.do(t, "GET", msgPath(text.ID)+"/versions", bob.Token, nil)
	if vs := decodeJSON[[]store.MessageVersion](t, b); code != http.StatusOK || len(vs) != 1 || vs[0].Body != "**hi** <script>x</script>" {
		t.Fatalf("anyone reads earlier versions: %d %s", code, b)
	}

	if code, b := e.do(t, "DELETE", msgPath(text.ID), alice.Token, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, b)
	}
	if m := nextEdit(t, ch); m.ID != text.ID || m.DeletedAt == nil || m.Body != "" || m.HTML != "" {
		t.Fatalf("a deleted message reaches clients without its body: %+v", m)
	}
	if code, _ := e.do(t, "GET", msgPath(text.ID)+"/versions", bob.Token, nil); code != http.StatusNotFound {
		t.Fatalf("versions of a deleted message: %d", code)
	}
	if code, _ := e.do(t, "PATCH", msgPath(text.ID), alice.Token, map[string]string{"body": "back"}); code != http.StatusConflict {
		t.Fatalf("edit after delete: %d", code)
	}
	if code, _ := e.do(t, "DELETE", msgPath(text.ID), alice.Token, nil); code != http.StatusNoContent {
		t.Fatalf("second delete: %d", code)
	}
	if m, err := e.st.Message(t.Context(), room, text.ID); err != nil || m.Body != "_hello_" {
		t.Fatalf("the store keeps a deleted body: %+v %v", m, err)
	}
}

// 002-AC9 002-AC40: a stream resumed with ?since= catches up on the edits it missed; ?edits= skips
// the ones it had; a fresh stream gets every message as it stands, with no edit replay.
func TestEventsReplayEdits(t *testing.T) {
	e := newEnv(t)
	room := e.createRoom(t, "R").ID
	alice := e.join(t, e.createInvite(t, room).Token, "Alice")
	path := "/api/rooms/" + room + "/messages"
	post := func(body string) store.Message {
		t.Helper()
		code, b := e.do(t, "POST", path, alice.Token, map[string]string{"body": body})
		if code != http.StatusCreated {
			t.Fatalf("post: %d %s", code, b)
		}
		return decodeJSON[store.Message](t, b)
	}
	first, second := post("one"), post("two")
	if code, b := e.do(t, "PATCH", path+"/"+strconv.FormatInt(first.ID, 10), alice.Token, map[string]string{"body": "one!"}); code != http.StatusOK {
		t.Fatalf("edit: %d %s", code, b)
	}
	if code, b := e.do(t, "DELETE", path+"/"+strconv.FormatInt(second.ID, 10), alice.Token, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, b)
	}

	since := fmt.Sprintf("?since=%d", second.ID)
	ch := e.chatEvents(t, room, alice.Token, since)
	if m := nextEdit(t, ch); m.ID != first.ID || m.Body != "one!" {
		t.Fatalf("missed edit: %+v", m)
	}
	last := nextEdit(t, ch)
	if last.ID != second.ID || last.DeletedAt == nil {
		t.Fatalf("missed delete: %+v", last)
	}

	for _, query := range []string{since + "&edits=" + strconv.FormatInt(last.ChangeSeq, 10), ""} {
		ch := e.chatEvents(t, room, alice.Token, query)
		var got []string
		marker := post("marker " + query)
		for ev := next(t, ch); ; ev = next(t, ch) {
			if ev.event != "" {
				t.Fatalf("%q: unexpected %+v", query, ev)
			}
			m := decodeJSON[store.Message](t, []byte(ev.data))
			got = append(got, m.Body)
			if m.ID == marker.ID {
				break
			}
		}
		if query == "" && (len(got) < 3 || got[1] != "one!" || got[2] != "") {
			t.Fatalf("fresh stream: %q", got)
		}
	}
}

func golden(t *testing.T, path string, got string) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/web -run %s -update)", err, t.Name())
	}
	if got != string(want) {
		t.Errorf("%s differs from the output; review with -update and git diff\ngot:\n%s", path, got)
	}
}

// Lines stored before rendering on write have no HTML: they render on read; deleted ones never do.
func TestViewMessage(t *testing.T) {
	ts := time.Now()
	for _, tc := range []struct {
		in   store.Message
		want string
	}{
		{store.Message{Kind: store.KindText, Body: "*old*"}, "<p><em>old</em></p>\n"},
		{store.Message{Kind: store.KindText, Body: "*x*", HTML: "stored"}, "stored"},
		{store.Message{Kind: store.KindSystem, Body: "*joined*"}, ""},
		{store.Message{Kind: store.KindVoice, Body: "*x*", DeletedAt: &ts}, ""},
	} {
		if got := viewMessage(tc.in); got.HTML != tc.want || (tc.in.DeletedAt != nil && got.Body != "") {
			t.Errorf("viewMessage(%+v) = %+v, want html %q", tc.in, got, tc.want)
		}
	}
}

var promptTS = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}`)

// 002-AC12: lines the agent saw and their author edited or deleted after its turn are flagged in
// the next prompt; lines it never saw just show as they stand.
func TestPromptFlagsChangedMessages(t *testing.T) {
	e := newEnv(t)
	r := newTurnRoom(t, e)
	alice := r.alice
	path := "/api/rooms/" + r.room + "/messages"
	post := func(body string) int64 {
		t.Helper()
		code, b := e.do(t, "POST", path, alice.p.Token, map[string]string{"body": body})
		if code != http.StatusCreated {
			t.Fatalf("post: %d %s", code, b)
		}
		return decodeJSON[store.Message](t, b).ID
	}
	change := func(method string, id int64, body any) {
		t.Helper()
		if code, b := e.do(t, method, path+"/"+strconv.FormatInt(id, 10), alice.p.Token, body); code >= 300 {
			t.Fatalf("%s %d: %d %s", method, id, code, b)
		}
	}
	budget := post("Budget is 10k.")
	same := post("Unchanged in the end.")
	voice := &store.Message{RoomID: r.room, Kind: store.KindVoice, ParticipantID: alice.p.Participant.ID, Sender: "Alice", Body: "we need a role back"}
	if err := e.bus.Post(t.Context(), voice); err != nil {
		t.Fatal(err)
	}
	ship, err := e.st.MessageCursor(t.Context(), r.room)
	if err != nil {
		t.Fatal(err)
	}
	ship -= 3 // the turn room's own "We ship on Friday." line

	tn := e.press(t, r, alice, alice.p.Token, false)
	if code, b := e.result(t, r, alice, tn.ID, turn.Result{SessionID: sid1, Session: []byte("one\n")}); code != http.StatusOK {
		t.Fatalf("result: %d %s", code, b)
	}
	change("PATCH", ship, map[string]string{"body": "We ship on Monday."})
	change("PATCH", ship, map[string]string{"body": "We ship on Tuesday.\nWith a rollback plan."})
	change("DELETE", budget, nil)
	change("PATCH", voice.ID, map[string]string{"body": "we need a rollback"})
	change("PATCH", same, map[string]string{"body": "Changed for a while."})
	change("PATCH", same, map[string]string{"body": "Unchanged in the end."})
	unseen := post("Draft.")
	change("PATCH", unseen, map[string]string{"body": "Final."})
	gone := post("Oops, wrong room.")
	change("DELETE", gone, nil)

	tn = e.press(t, r, alice, alice.p.Token, false)
	got := promptTS.ReplaceAllString(e.job(t, r, alice, tn.ID).Prompt, "<ts>")
	// The preamble names who speaks now, from the clock: TestPresencePreamble holds its own golden.
	if i := strings.Index(got, "\nYou are Grill"); i < 0 || !strings.HasPrefix(got, "## Who is here\n") {
		t.Fatalf("no presence preamble before the prompt:\n%s", got)
	} else {
		got = got[i+1:]
	}
	golden(t, "testdata/prompt-changes.golden", got)

	day := time.Now().UTC().Format(time.DateOnly)
	tr := e.workspace(t, r, alice, tn.ID)["transcript/"+day+".md"].data
	for _, want := range []string{"**Alice** (edited): We ship on Tuesday.\n  With a rollback plan.\n", "**Alice** (deleted): Budget is 10k.\n",
		"**Alice** (voice, edited): we need a rollback\n", "**Alice** (edited): Final.\n"} {
		if !strings.Contains(tr, want) {
			t.Errorf("transcript lacks %q:\n%s", want, tr)
		}
	}

	// Flagged once: the turn after sees nothing changed.
	if code, b := e.result(t, r, alice, tn.ID, turn.Result{SessionID: sid1, SessionOffset: 4, Session: []byte("two\n")}); code != http.StatusOK {
		t.Fatalf("result: %d %s", code, b)
	}
	tn = e.press(t, r, alice, alice.p.Token, false)
	if p := e.job(t, r, alice, tn.ID).Prompt; strings.Contains(p, "Changed since your last turn") {
		t.Fatalf("changes flagged twice:\n%s", p)
	}
}
