package web

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

// 032-AC1: a room admin turns the time keeper on and sets its wait; a wait outside the choices is a
// 400, and a member who is no room admin is refused both while their other changes still apply.
func TestTimeKeeperOption(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Keep", "Alice")
	bob := e.join(t, e.createInvite(t, room.ID).Token, "Bob")
	ps, err := e.st.RoomPersonas(t.Context(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/rooms/%s/personas/%d", room.ID, ps[0].ID)
	for _, c := range []struct {
		name string
		by   func(body any) (int, []byte)
		body map[string]any
		code int
		msg  string
	}{
		{"admin on", func(b any) (int, []byte) { return alice.do(t, "PATCH", path, b) }, map[string]any{"time_keeper": true, "cut_in_s": 20}, http.StatusOK, ""},
		{"bad wait", func(b any) (int, []byte) { return alice.do(t, "PATCH", path, b) }, map[string]any{"cut_in_s": 15}, http.StatusBadRequest, "cut_in_s must be one of [0 10 20 30 60]"},
		{"negative wait", func(b any) (int, []byte) { return alice.do(t, "PATCH", path, b) }, map[string]any{"cut_in_s": -10}, http.StatusBadRequest, "cut_in_s must be one of [0 10 20 30 60]"},
		{"member role", func(b any) (int, []byte) { return e.do(t, "PATCH", path, bob.Token, b) }, map[string]any{"time_keeper": false}, http.StatusForbidden, "only a room admin can do this"},
		{"member wait", func(b any) (int, []byte) { return e.do(t, "PATCH", path, bob.Token, b) }, map[string]any{"cut_in_s": 10}, http.StatusForbidden, "only a room admin can do this"},
		{"member badge", func(b any) (int, []byte) { return e.do(t, "PATCH", path, bob.Token, b) }, map[string]any{"badge": "🧯"}, http.StatusOK, ""},
	} {
		code, b := c.by(c.body)
		if code != c.code || (c.msg != "" && errorOf(t, b) != c.msg) {
			t.Errorf("%s: %d %s, want %d %q", c.name, code, b, c.code, c.msg)
		}
	}
	p, _ := e.st.RoomPersona(t.Context(), room.ID, ps[0].ID)
	if !p.TimeKeeper || p.CutInS != 20 || p.Badge != "🧯" {
		t.Fatalf("persona %+v", p)
	}
	if m := e.lastLine(t, room.ID); !strings.Contains(e.lineBefore(t, room.ID, m.ID).Body, "changed 🔥 Grill: now keeps time, speak over after off → 20 s.") {
		t.Errorf("no line for the role")
	}
}

// lineBefore is the room's message just before id.
func (e *env) lineBefore(t *testing.T, room string, id int64) store.Message {
	t.Helper()
	ms, err := e.st.Messages(t.Context(), room, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(ms) - 1; i > 0; i-- {
		if ms[i].ID == id {
			return ms[i-1]
		}
	}
	t.Fatalf("no line before %d", id)
	return store.Message{}
}

// 032-AC2: talk time counts from the first line of the speaker's run, which another speaker's line
// or a gap over 30 s ends; a paragraph ends at its last append.
func TestRunStart(t *testing.T) {
	at := func(s int) time.Time { return time.Unix(1_000_000+int64(s), 0) }
	line := func(pid int64, ts int, appended ...int) store.Message {
		m := store.Message{ParticipantID: pid, TS: at(ts)}
		if len(appended) > 0 {
			a := at(appended[0])
			m.AppendedAt = &a
		}
		return m
	}
	for _, c := range []struct {
		name  string
		lines []store.Message
		want  int
	}{
		{"one line", []store.Message{line(1, 100)}, 100},
		{"own lines", []store.Message{line(1, 100), line(1, 80), line(1, 60)}, 60},
		{"another speaker between", []store.Message{line(1, 100), line(2, 90), line(1, 80)}, 100},
		{"another speaker before", []store.Message{line(1, 100), line(1, 90), line(2, 85)}, 90},
		{"a 31 s gap", []store.Message{line(1, 100), line(1, 40, 69)}, 100},
		{"a 30 s gap", []store.Message{line(1, 100), line(1, 40, 70)}, 40},
		{"a 31 s gap after a plain line", []store.Message{line(1, 100), line(1, 69)}, 100},
		{"a paragraph begun long ago", []store.Message{line(1, 300, 400)}, 300},
	} {
		if got := runStart(c.lines); !got.Equal(at(c.want)) {
			t.Errorf("%s: %v, want %v", c.name, got.Sub(at(0)), c.want)
		}
	}
}

// 032-AC2: one compact duration format.
func TestSpan(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second: "0 s", 0: "0 s", 40 * time.Second: "40 s", 59*time.Second + 900*time.Millisecond: "59 s",
		time.Minute: "1 min", 3 * time.Minute: "3 min", 4*time.Minute + 10*time.Second: "4 min 10 s", 75 * time.Minute: "75 min",
	} {
		if got := span(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}

// 032-AC2: the preamble's lines for a time keeper, and for any other persona, golden.
func TestRenderKeeper(t *testing.T) {
	base := presenceView{Room: "R", Self: presencePersona{Name: "Timekeeper", Level: "model sonnet, effort low", Trigger: "on demand and every 5 minutes"},
		Chain: "25 of 25 agent turns left", Humans: []string{"@Alice ·a1b2", "@Bob ·c3d4"}, Host: "Alice ·a1b2", Speaker: "Alice ·a1b2"}
	other := base
	keeper := base
	keeper.Keeper, keeper.SpeakerFor = true, "4 min 10 s"
	keeper.Hands = []string{"1. Bob ·c3d4, for 3 min", "2. Carol ·e5f6, for 40 s", "3. Dan ·0a0b, for 30 s", "4. Eve ·1c1d, for 20 s", "5. Fay ·2e2f, for 10 s"}
	keeper.HandsMore = 1
	none := base
	none.Keeper, none.Speaker = true, ""
	var out []string
	for _, v := range []presenceView{other, keeper, none} {
		got, err := renderPresence(v)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, got)
	}
	golden(t, "testdata/presence-keeper.golden", strings.Join(out, "\n\n----\n\n"))
	if strings.Contains(strings.ToLower(out[0]), "hand") {
		t.Errorf("another persona's preamble mentions hands:\n%s", out[0])
	}
}

// 032-AC2: in a room, the time keeper sees six raised hands as five and "and 1 more", in queue
// order, and the speaker with their talk time; Grill's preamble has neither.
func TestKeeperPreamble(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Floor", "Alice")
	path := "/api/rooms/" + room.ID
	var first store.Participant
	for i, name := range []string{"Bob", "Carol", "Dan", "Eve", "Fay", "Gus"} {
		j := e.join(t, e.createInvite(t, room.ID).Token, name)
		e.do(t, "POST", path+"/presence", j.Token, map[string]any{"tab": "t1"})
		if code, b := e.do(t, "PUT", path+"/hand", j.Token, nil); code != http.StatusNoContent {
			t.Fatalf("raise %s: %d %s", name, code, b)
		}
		if i == 0 {
			first = j.Participant
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err := e.bus.Post(t.Context(), &store.Message{RoomID: room.ID, Kind: store.KindVoice, Source: store.VoiceSourceServer,
		ParticipantID: first.ID, Sender: "Bob", Body: "and another thing"}); err != nil {
		t.Fatal(err)
	}
	r, err := e.st.Room(t.Context(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	ps, err := e.st.RoomPersonas(t.Context(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	grill := ps[0]
	keeper := grill
	keeper.TimeKeeper = true
	clean := func(s string) string {
		s = regexp.MustCompile(`·[0-9a-f]{4}\b|·#\d+`).ReplaceAllString(s, "·<id>")
		return regexp.MustCompile(`for \d+ s\b`).ReplaceAllString(s, "for <t>")
	}
	got, err := e.srv.presencePreamble(t.Context(), r, &keeper, "every 5 minutes")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"\nHands raised:\n1. Bob ·<id>, for <t>\n2. Carol ·<id>, for <t>\n3. Dan ·<id>, for <t>\n4. Eve ·<id>, for <t>\n5. Fay ·<id>, for <t>\nand 1 more\nHost: ",
		"\nBob ·<id> speaks now, for <t>\n",
	} {
		if !strings.Contains(clean(got), want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
	plain, err := e.srv.presencePreamble(t.Context(), r, &grill, "every 5 minutes")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(plain), "hand") || !strings.Contains(clean(plain), "\nSpeaking now: Bob ·<id>\n") {
		t.Errorf("another persona's preamble:\n%s", plain)
	}
}

// 032-AC3: a time keeper's turn with no answer posts nothing and says nothing, neither the summary
// of what it changed nor a spoken.md it wrote; the room's state says it had nothing to say. Another
// persona's empty turn keeps today's behaviour: no message, and the summary in the call.
func TestSilentKeeperSpeech(t *testing.T) {
	e := newEnvOpts(t, ttsOptions(t))
	r := newTurnRoom(t, e)
	e.inAudio(t, r.room, r.alice.p, false)
	events, _ := e.openStream(t, "/api/tts/events", ttsToken, "")
	e.podBeats(t)
	nextEvent(t, events, "rooms")
	alice := r.alice.p.Participant
	if _, _, _, err := e.st.EditPersona(t.Context(), r.room, r.grill.ID, store.PersonaPatch{TimeKeeper: ptrTo(true)}, &alice); err != nil {
		t.Fatal(err)
	}
	agentLines := func() int {
		ms, _ := e.st.Messages(t.Context(), r.room, 0, 1000)
		n := 0
		for _, m := range ms {
			if m.Kind == store.KindAgent {
				n++
			}
		}
		return n
	}
	before := agentLines()
	ttsTurn(t, e, r, turn.Result{Synthesis: " \n", Changes: []turn.Change{{Path: "docs/synthesis.md", Content: []byte("# Synthesis\n\nKept quiet.\n")}}})
	st := e.agent(t, r.room, r.alice.p.Token)
	if agentLines() != before || len(st.Personas) != 1 || !st.Personas[0].Silent {
		t.Fatalf("silent turn: %d agent lines (was %d), personas %+v", agentLines(), before, st.Personas)
	}
	ttsTurn(t, e, r, turn.Result{Changes: []turn.Change{{Path: turn.SpokenFile, Content: []byte("Alice, Bob has waited four minutes.")}}})
	if st := e.agent(t, r.room, r.alice.p.Token); agentLines() != before || !st.Personas[0].Silent {
		t.Fatalf("spoken but no answer: %d agent lines (was %d), personas %+v", agentLines(), before, st.Personas)
	}
	// The silence is the turn's: a line handed to the call for the time keeper, as the e2e hook
	// does with no synthesis, is said. It is the stream's next line, so neither turn above queued one.
	e.srv.queueSpeech(t.Context(), &store.Turn{RoomID: r.room, PersonaID: r.grill.ID}, "Hello from the hook.", "")
	if l := decodeJSON[ttsLine](t, []byte(nextEvent(t, events, "line").data)); l.Text != "Hello from the hook." {
		t.Fatalf("hook line %q", l.Text)
	}
	if _, _, _, err := e.st.EditPersona(t.Context(), r.room, r.grill.ID, store.PersonaPatch{TimeKeeper: ptrTo(false)}, &alice); err != nil {
		t.Fatal(err)
	}
	ttsTurn(t, e, r, turn.Result{Changes: []turn.Change{{Path: "docs/notes.md", Content: []byte("# Notes\n")}}})
	// The next line on the stream is this turn's summary.
	if l := decodeJSON[ttsLine](t, []byte(nextEvent(t, events, "line").data)); l.Text != "I wrote notes." {
		t.Errorf("line %q", l.Text)
	}
	if agentLines() != before {
		t.Errorf("an empty turn posted a message")
	}
	if p, _ := e.st.RoomPersona(t.Context(), r.room, r.grill.ID); p.Silent {
		t.Error("a persona without the role is silent")
	}
}

// 032-AC6: a member's copy of the Timekeeper gets the timer and the voice, not the role nor the wait,
// which stay with the persona that had them; a room admin's copy takes both.
func TestTimekeeperAddByRole(t *testing.T) {
	e := newEnv(t)
	alice := e.device(t)
	room := alice.createRoom(t, "Adds", "Alice")
	bob := e.join(t, e.createInvite(t, room.ID).Token, "Bob")
	path := "/api/rooms/" + room.ID
	ps, err := e.st.RoomPersonas(t.Context(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, b := alice.do(t, "PATCH", fmt.Sprintf("%s/personas/%d", path, ps[0].ID), map[string]any{"time_keeper": true}); code != http.StatusOK {
		t.Fatalf("role: %d %s", code, b)
	}
	var tpl int64
	ts, _ := e.st.Templates(t.Context(), 0)
	for _, x := range ts {
		if x.Preset == "timekeeper" {
			tpl = x.ID
		}
	}
	add := func(do func(body any) (int, []byte)) store.RoomPersona {
		t.Helper()
		code, b := do(map[string]int64{"template_id": tpl})
		if code != http.StatusCreated {
			t.Fatalf("add: %d %s", code, b)
		}
		return decodeJSON[store.RoomPersona](t, b)
	}
	byBob := add(func(b any) (int, []byte) { return e.do(t, "POST", path+"/personas", bob.Token, b) })
	if byBob.TimeKeeper || byBob.CutInS != 0 || byBob.EveryMinutes != 5 || !byBob.Speaks {
		t.Fatalf("member's copy %+v", byBob)
	}
	if p, _ := e.st.RoomPersona(t.Context(), room.ID, ps[0].ID); !p.TimeKeeper {
		t.Fatal("a member's add took the role")
	}
	byAlice := add(func(b any) (int, []byte) { return alice.do(t, "POST", path+"/personas", b) })
	if !byAlice.TimeKeeper || byAlice.CutInS != 20 || byAlice.EveryMinutes != 5 || !byAlice.Speaks {
		t.Fatalf("admin's copy %+v", byAlice)
	}
	if p, _ := e.st.RoomPersona(t.Context(), room.ID, ps[0].ID); p.TimeKeeper {
		t.Fatal("an admin's add left the role with Grill")
	}
}

func ptrTo[T any](v T) *T { return &v }
