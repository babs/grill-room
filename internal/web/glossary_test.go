package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// 005-AC23: the prompt is one sentence, the glossary last so an overlong prompt loses the least
// wanted terms first; duplicates count once, across sources and case.
func TestBuildSTTPrompt(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		glossary, names, docs []string
		want                  string
		used, total           int
	}{
		{"empty", nil, nil, nil, "", 0, 0},
		{"glossary only", []string{"Kubernetes"}, nil, nil, "On parle de Kubernetes.", 1, 1},
		{"names only", nil, []string{"Alice", "Bob"}, nil, "Avec Alice et Bob.", 2, 2},
		{"all three", []string{"Argo CD", "Helm", "p95"}, []string{"Alice", "Grill"}, []string{"plan"},
			"Autour de plan, avec Alice et Grill, on parle de Argo CD, Helm et p95.", 6, 6},
		{"duplicates", []string{"Helm", "helm"}, []string{"HELM", "Alice"}, []string{"alice"},
			"Avec Alice, on parle de Helm.", 2, 2},
		{"multiline name", nil, []string{"Ada\nLovelace"}, nil, "Avec Ada Lovelace.", 1, 1},
	} {
		got := buildSTTPrompt(tc.glossary, tc.names, tc.docs)
		if got.Prompt != tc.want || got.Used != tc.used || got.Total != tc.total {
			t.Errorf("%s: %+v, want %q %d/%d", tc.name, got, tc.want, tc.used, tc.total)
		}
	}
}

// 005-AC23: past the budget, terms are taken glossary first, then names, then documents, and the
// prompt stays within it.
func TestBuildSTTPromptCap(t *testing.T) {
	var glossary, names, docs []string
	for i := range 60 {
		glossary = append(glossary, fmt.Sprintf("term%02d", i))
		names = append(names, fmt.Sprintf("name%02d", i))
		docs = append(docs, fmt.Sprintf("doc%02d", i))
	}
	got := buildSTTPrompt(glossary, names, docs)
	if got.Total != 180 || got.Used >= got.Total || got.Used < 60 {
		t.Fatalf("used %d of %d", got.Used, got.Total)
	}
	if !strings.Contains(got.Prompt, "term59") || strings.Contains(got.Prompt, "doc00") {
		t.Fatalf("glossary not first, or documents not last: %s", got.Prompt)
	}
	// Every glossary term made it, then names in order until the budget ran out: no name after a
	// missing one.
	missing := false
	for _, n := range names {
		in := strings.Contains(got.Prompt, n)
		if in && missing {
			t.Fatalf("%s kept after an earlier name was dropped", n)
		}
		missing = missing || !in
	}
	cost := promptFrame
	for _, w := range strings.FieldsFunc(got.Prompt, func(r rune) bool { return r == ',' || r == ' ' || r == '.' }) {
		if strings.HasPrefix(w, "term") || strings.HasPrefix(w, "name") {
			cost += estTokens(w)
		}
	}
	if cost > promptBudget {
		t.Fatalf("estimated %d tokens, over %d", cost, promptBudget)
	}
	// The glossary sits at the end: Whisper cuts an overlong prompt from its start.
	if !strings.HasSuffix(got.Prompt, "term59.") || !strings.HasPrefix(got.Prompt, "Avec name00") {
		t.Fatalf("order: %s", got.Prompt)
	}
	// A glossary of usual terms holds about the capacity Manage announces.
	var usual []string
	for i := range 100 {
		usual = append(usual, fmt.Sprintf("Kube%02d", i))
	}
	if n := buildSTTPrompt(usual, nil, nil).Used; n < glossaryCapacity-10 || n > glossaryCapacity+10 {
		t.Fatalf("%d usual terms fit, Manage says ~%d", n, glossaryCapacity)
	}
}

func TestSplitTerms(t *testing.T) {
	if got := strings.Join(splitTerms(" a, b\n\n c ,,\nd e "), "|"); got != "a|b|c|d e" {
		t.Fatal(got)
	}
}

// 005-AC23: any member sets the glossary, with a line in the room; the worker reads the prompt
// with its service token only, and it follows the room's names and documents.
func TestGlossaryRoutes(t *testing.T) {
	e, _ := newVoiceEnv(t)
	room := e.createRoom(t, "R")
	alice := e.join(t, e.createInvite(t, room.ID).Token, "Alice")
	path := "/api/rooms/" + room.ID + "/glossary"
	code, b := e.do(t, "PUT", path, alice.Token, map[string]string{"glossary": "Kubernetes\nArgo CD, Helm"})
	if code != http.StatusOK {
		t.Fatalf("put: %d %s", code, b)
	}
	got := decodeJSON[glossaryResp](t, b)
	if got.Glossary != "Kubernetes\nArgo CD, Helm" || got.Used != 5 || got.Total != 5 || got.Capacity != glossaryCapacity ||
		got.Prompt != "Avec Alice et Grill, on parle de Kubernetes, Argo CD et Helm." {
		t.Fatalf("glossary: %+v", got)
	}
	msgs, err := e.st.Messages(t.Context(), room.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if last := msgs[len(msgs)-1]; last.Body != "Alice changed the transcription glossary" || last.ParticipantID != alice.Participant.ID {
		t.Fatalf("journal line: %+v", last)
	}
	// The same glossary again writes no line.
	e.do(t, "PUT", path, alice.Token, map[string]string{"glossary": " Kubernetes\nArgo CD, Helm "})
	if again, _ := e.st.Messages(t.Context(), room.ID, 0, 100); len(again) != len(msgs) {
		t.Fatal("unchanged glossary wrote a line")
	}
	for _, bad := range []string{strings.Repeat("x", 61), strings.Repeat("ab,", 700), "a\x00b"} {
		if code, _ := e.do(t, "PUT", path, alice.Token, map[string]string{"glossary": bad}); code != http.StatusBadRequest {
			t.Fatalf("%.20q: %d", bad, code)
		}
	}
	prompt := "/api/stt/rooms/" + room.ID + "/prompt"
	for _, tok := range []string{"", alice.Token, e.admin} {
		if code, _ := e.do(t, "GET", prompt, tok, nil); code != http.StatusUnauthorized {
			t.Fatalf("prompt with a non-STT token: %d", code)
		}
	}
	code, b = e.do(t, "GET", prompt, sttToken, nil)
	if p := decodeJSON[map[string]string](t, b); code != http.StatusOK || p["prompt"] != got.Prompt {
		t.Fatalf("worker prompt: %d %s", code, b)
	}
	// A new participant joins the prompt at once; clearing the glossary leaves the names, the room's
	// Grill persona included.
	e.join(t, e.createInvite(t, room.ID).Token, "Bob")
	e.do(t, "PUT", path, alice.Token, map[string]string{"glossary": ""})
	_, b = e.do(t, "GET", prompt, sttToken, nil)
	if p := decodeJSON[map[string]string](t, b); p["prompt"] != "Avec Alice, Bob et Grill." {
		t.Fatalf("after clear: %s", b)
	}
	if code, _ := e.do(t, "GET", "/api/stt/rooms/nope/prompt", sttToken, nil); code != http.StatusNotFound {
		t.Fatalf("unknown room: %d", code)
	}
}
