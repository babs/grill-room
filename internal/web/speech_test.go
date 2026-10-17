package web

import (
	"strings"
	"testing"

	"github.com/babs/grill-room/internal/store"
)

// 013-AC3: every mark the agent might leave is gone, in two shapes each, and whitespace folds.
func TestSpokenTextSanitises(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"**J'ai** changé la _spec_.", "J'ai changé la spec."},
		{"# Résumé\n- I changed ~~it~~ the plan.\n> quoted", "Résumé I changed it the plan. quoted"},
		{"See [the spec](https://x.example/a) now.", "See the spec now."},
		{"Voir ![schéma](img.png) ici.", "Voir schéma ici."},
		{"Check https://example.com/a?b=1 and www.example.org today.", "Check and today."},
		{"I ran `go test ./...` then ```\nmake lint\n``` again.", "I ran then again."},
		{"About Alice ·a1b2's note, I replied.", "About Alice's note, I replied."},
		{"Bob · #9f3c asked.", "Bob asked."},
		{"Issue #14 and #7 are fixed.", "Issue 14 and 7 are fixed."},
		{"Done 🎉 and shipped ✅️ 👍🏽.", "Done and shipped ."},
		{"1. First.\n2) Second.", "First. Second."},
		{"  lots\t of \n\n space  ", "lots of space"},
		{"🎉 **`code`** https://x.example", ""},
	} {
		if got := spokenText(c.in, 60); got != c.want {
			t.Errorf("spokenText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func words(n int, w string) string { return strings.TrimSpace(strings.Repeat(w+" ", n)) }

// 013-AC3: whole sentences up to the cap; a first sentence over it is cut at its last comma within
// the cap, else at the cap, and closed with a full stop. Versions and decimals are not sentence ends.
func TestSpokenTextCap(t *testing.T) {
	s20 := words(19, "mot") + " fin."
	if got := spokenText(s20+" "+s20+" "+s20+" "+s20, 60); got != s20+" "+s20+" "+s20 {
		t.Errorf("four 20-word sentences, cap 60: %d words", len(strings.Fields(got)))
	}
	if got := spokenText(s20+" "+s20, 40); got != s20+" "+s20 {
		t.Error("exactly at the cap is kept whole")
	}
	if got := spokenText(s20+" "+s20+" "+s20, 45); got != s20+" "+s20 {
		t.Errorf("cap 45: %q", got)
	}
	long := words(600, "word")
	if got := spokenText(long, 60); len(strings.Fields(got)) != 60 || !strings.HasSuffix(got, "word.") {
		t.Errorf("600 words with no punctuation: %d words, %q", len(strings.Fields(got)), got[len(got)-10:])
	}
	comma := words(30, "un") + ", " + words(35, "deux") + " fin."
	if got := spokenText(comma, 60); got != words(30, "un")+"." {
		t.Errorf("a 66-word first sentence with a comma: %q", got)
	}
	twoCommas := words(10, "a") + ", " + words(40, "b") + ", " + words(20, "c") + "."
	if got := spokenText(twoCommas, 60); got != words(10, "a")+", "+words(40, "b")+"." {
		t.Errorf("the last comma within the cap: %q", got)
	}
	noComma := words(65, "trois") + "."
	if got := spokenText(noComma, 60); got != words(60, "trois")+"." {
		t.Errorf("a 66-word first sentence without a comma: %q", got)
	}
	ver := "I moved us to version 2.3.1 today. " + words(55, "x") + " end. Then 99.9 percent!"
	if got := spokenText(ver, 60); got != "I moved us to version 2.3.1 today." {
		t.Errorf("a version is not a sentence end: %q", got)
	}
	q := "Is it done? " + words(56, "y") + " end! Bob, which one?"
	if got := spokenText(q, 60); got != "Is it done? "+words(56, "y")+" end!" {
		t.Errorf("? and ! end sentences: %q", got)
	}
}

// 013-AC5 (server side): the function-word count, in both directions and on a tie.
func TestTextLang(t *testing.T) {
	for _, c := range []struct{ text, want string }{
		{"J'ai changé la spec et je pense que c'est mieux.", "fr"},
		{"Nous avons fini le plan pour la démo.", "fr"},
		{"I changed the spec and it is better now.", "en"},
		{"We have finished the plan for the demo.", "en"},
		{"le the", "en"},
		{"Kubernetes GitHub", "fallback"},
	} {
		if got := textLang("fallback", c.text); got != c.want {
			t.Errorf("textLang(%q) = %s, want %s", c.text, got, c.want)
		}
	}
	if textLang("en", "le plan est prêt", "the plan") != "fr" {
		t.Error("several texts count together")
	}
}

// 013-AC4: the actions in the first person, in the synthesis's language; nothing without actions.
func TestTurnSummary(t *testing.T) {
	r := store.TurnReport{Revised: []string{"spec-paiement.md"}, Created: []string{"notes_v2.md", "plan.html"}, Annotated: 2, Replies: 1}
	if got := turnSummary(r, "J'ai revu la spec et les notes de la réunion."); got !=
		"J'ai modifié spec paiement. J'ai écrit notes v2 et plan. J'ai ouvert 2 notes. J'ai répondu à une discussion." {
		t.Errorf("French: %q", got)
	}
	if got := turnSummary(r, "I reviewed the spec and the meeting notes."); got !=
		"I changed spec paiement. I wrote notes v2 and plan. I opened 2 notes. I answered a thread." {
		t.Errorf("English: %q", got)
	}
	if got := turnSummary(store.TurnReport{Annotated: 1, Replies: 3}, "Done."); got != "I opened a note. I answered 3 threads." {
		t.Errorf("counts: %q", got)
	}
	if got := turnSummary(store.TurnReport{Rejected: []string{"uploads/a.md"}, Closed: 2}, "J'ai lu la spec."); got != "" {
		t.Errorf("no action to speak of: %q", got)
	}
}
