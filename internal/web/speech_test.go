package web

import (
	"net/http"
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

// Spike S14: spoken.md never reaches the synthesizer with a mark, link, URL or short id in it.
func TestSpokenText(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"J'ai relu la spec.", "J'ai relu la spec."},
		{"## Point\n\n- J'ai ajouté **l'idempotence** à `docs/spec.md`.", "Point J'ai ajouté l'idempotence à docs/spec.md."},
		{"Voir [la doc](https://docs.stripe.com/x) et https://example.com/a?b=1 ici.", "Voir la doc et ici."},
		{"Alice ·a1b2 et Bob ·#12, regardez la note #14.", "Alice et Bob, regardez la note 14."},
		{"> I closed _two_ notes.\n+ Raj owns it.", "I closed two notes. Raj owns it."},
		{"   \n\n", ""},
	} {
		if got := spokenText(c.in); got != c.want {
			t.Errorf("spokenText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if n := len(strings.Fields(spokenText(strings.Repeat("mot ", 600)))); n != maxSpokenWords {
		t.Errorf("an unpunctuated text is cut at %d words, got %d", maxSpokenWords, n)
	}
	if n := len(strings.Fields(spokenText(strings.Repeat("mot ", 100) + ". Fin."))); n != maxSpokenWords {
		t.Errorf("a first sentence over the cap is cut at %d words, got %d", maxSpokenWords, n)
	}
	long := strings.Repeat("Une phrase de cinq mots. ", 30)
	got := spokenText(long)
	if n := len(strings.Fields(got)); n > maxSpokenWords || n == 0 || !strings.HasSuffix(got, ".") {
		t.Errorf("a long text is cut at a sentence end within %d words: %d words, %q", maxSpokenWords, n, got[len(got)-20:])
	}
}

func TestTurnSummary(t *testing.T) {
	r := store.TurnReport{Revised: []string{"spec-paiement.md"}, Created: []string{"risques.md", "plan_b.md"}, Annotated: 1, Replies: 2}
	if got, want := turnSummary(r, "J'ai ajouté une section sur la page et les risques pour la prod."), "J'ai modifié spec paiement. J'ai écrit risques et plan b. J'ai ouvert une note. J'ai répondu à 2 notes."; got != want {
		t.Errorf("fr: %q, want %q", got, want)
	}
	if got, want := turnSummary(r, "I added a section on retries to the spec."), "I changed spec paiement. I wrote risques and plan b. I opened a note. I answered 2 notes."; got != want {
		t.Errorf("en: %q, want %q", got, want)
	}
	if got := turnSummary(store.TurnReport{}, "Nothing to add."); got != "" {
		t.Errorf("a turn that changed nothing says nothing, got %q", got)
	}
}

func TestSpeechQueue(t *testing.T) {
	var q speechQueue
	for i := range maxSpeech + 5 {
		q.add(Speech{Text: strings.Repeat("x", i+1)})
	}
	all, last := q.after(0)
	if len(all) != maxSpeech || all[0].Seq != 6 || all[len(all)-1].Seq != maxSpeech+5 || last != maxSpeech+5 {
		t.Fatalf("the queue keeps the latest %d lines: %d, first %d", maxSpeech, len(all), all[0].Seq)
	}
	if got, _ := q.after(maxSpeech + 3); len(got) != 2 || got[0].Seq != maxSpeech+4 {
		t.Errorf("after a seq: %+v", got)
	}
	if got, _ := q.after(maxSpeech + 5); got == nil || len(got) != 0 {
		t.Errorf("nothing after the last seq, got %d", len(got))
	}
}

func TestPromptAsksForSpokenOnlyWithTheFlag(t *testing.T) {
	if strings.Contains(buildPrompt(promptInput{Persona: "Grill", Room: "r"}), "spoken.md") {
		t.Error("the prompt asks for spoken.md with the flag off")
	}
	if p := buildPrompt(promptInput{Persona: "Grill", Room: "r", Spoken: true}); !strings.Contains(p, "Write spoken.md") || strings.Contains(p, "`") {
		t.Error("with the flag on, the prompt asks for spoken.md")
	}
}

// Spike S14 end to end on the server: a turn's spoken.md is queued, sanitised, for the voice bot;
// a turn without it gets the summary; the route needs the STT token and exists only with the flag.
func TestSpeechFromTurns(t *testing.T) {
	ve, _ := newVoiceEnv(t)
	opts := ve.srv.opts
	opts.PersonaVoice = true
	e := newEnvOpts(t, opts)
	r := newTurnRoom(t, e)
	tn := e.press(t, r, r.alice, r.alice.p.Token, false)
	if job := e.job(t, r, r.alice, tn.ID); !strings.Contains(job.Prompt, "Write spoken.md") {
		t.Fatal("the prompt does not ask for spoken.md")
	}
	if code, b := e.result(t, r, r.alice, tn.ID, turn.Result{Synthesis: "Friday is risky.", SessionID: sid1, Session: []byte("one\n"),
		Changes: []turn.Change{{Path: "spoken.md", Content: []byte("I read the **plan**. Alice ·a1b2, is Friday fixed?\n")}}}); code != http.StatusOK {
		t.Fatalf("result: %d %s", code, b)
	}
	tn2 := e.press(t, r, r.alice, r.alice.p.Token, false)
	if code, b := e.result(t, r, r.alice, tn2.ID, turn.Result{Synthesis: "Updated the synthesis.", SessionID: sid1, SessionOffset: 4, Session: []byte("two\n"),
		Changes: []turn.Change{{Path: "docs/synthesis.md", Content: []byte("# Synthesis\n\nNew.\n")}}}); code != http.StatusOK {
		t.Fatalf("result: %d %s", code, b)
	}
	if code, _ := e.do(t, "GET", "/api/stt/speech", r.alice.p.Token, nil); code != http.StatusUnauthorized {
		t.Errorf("a participant token reads the speech queue: %d", code)
	}
	code, b := e.do(t, "GET", "/api/stt/speech?after=0", sttToken, nil)
	resp := decodeJSON[struct {
		Items []Speech
		Last  int64
	}](t, b)
	got := resp.Items
	if code != http.StatusOK || len(got) != 2 || resp.Last != 2 {
		t.Fatalf("speech: %d %s", code, b)
	}
	if s := got[0]; s.Text != "I read the plan. Alice, is Friday fixed?" || s.Source != "spoken" || s.Room != r.room || s.Name == "" {
		t.Errorf("spoken.md line: %+v", s)
	}
	if s := got[1]; s.Text != "I changed synthesis." || s.Source != "summary" {
		t.Errorf("summary line: %+v", s)
	}
	if code, _ := ve.do(t, "GET", "/api/stt/speech", sttToken, nil); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Errorf("without the flag the route exists: %d", code)
	}
}
