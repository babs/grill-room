package web

// What a persona says in the call after its turn (spec 013): spoken.md made safe to synthesise,
// or a summary of the turn's actions when it wrote none.

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/babs/grill-room/internal/store"
)

var (
	mdLink   = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	codeSpan = regexp.MustCompile("```[\\s\\S]*?```|`[^`]*`")
	bareURL  = regexp.MustCompile(`(?i)\b(?:https?://|www\.)\S+`)
	// A short id follows a name after a middle dot, with or without a # (009 mentions).
	shortID  = regexp.MustCompile(`\s?·\s?#?[0-9a-f]{2,}\b`)
	hashNum  = regexp.MustCompile(`#(\d+)`)
	lineMark = regexp.MustCompile(`(?m)^\s*(?:#{1,6}\s+|>\s*|[-*+]\s+|\d+[.)]\s+)`)
	inlineMk = regexp.MustCompile(`[*_~|<>]+`)
	// A sentence ends at . ! ? or … followed by a space or the end: "2.3.1" and "99.9" stay whole.
	sentenceEnd = regexp.MustCompile(`[.!?…]+(?:\s+|$)`)
)

// spokenText is spoken.md made safe to synthesise, whatever the agent did with the rules: no
// Markdown mark, code, link target, URL, emoji, short id or mention's @ is read out, and the line
// keeps whole sentences up to words words (013-AC3). Empty means nothing to say.
func spokenText(md string, words int) string {
	t := codeSpan.ReplaceAllString(md, " ")
	t = mdLink.ReplaceAllString(t, "$1")
	t = bareURL.ReplaceAllString(t, " ")
	t = shortID.ReplaceAllString(t, "")
	t = hashNum.ReplaceAllString(t, "$1")
	// Before inlineMk drops "_": "@Ana_María" is said "Ana María", never "at AnaMaría".
	t = mentionRe.ReplaceAllStringFunc(t, func(m string) string {
		i := strings.LastIndexByte(m, '@')
		return m[:i] + strings.ReplaceAll(m[i+1:], "_", " ")
	})
	t = lineMark.ReplaceAllString(t, "")
	t = inlineMk.ReplaceAllString(t, "")
	t = strings.Map(func(r rune) rune {
		// Emoji and pictographs, their joiners and variation selectors: a synthesiser spells them out.
		if unicode.Is(unicode.So, r) || r == '‍' || r >= '︀' && r <= '️' || r >= 0x1f3fb && r <= 0x1f3ff {
			return -1
		}
		return r
	}, t)
	t = strings.Join(strings.Fields(t), " ")
	if t == "" || !hasWord(t) {
		return ""
	}
	return capWords(t, words)
}

func hasWord(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) })
}

// capWords keeps the whole sentences of t that fit in max words. A first sentence over max is cut
// at its last comma within max words, else at max words, and closed with a full stop.
func capWords(t string, max int) string {
	if len(strings.Fields(t)) <= max {
		return t
	}
	var out []string
	n := 0
	rest := t
	for rest != "" {
		end := len(rest)
		if loc := sentenceEnd.FindStringIndex(rest); loc != nil {
			end = loc[1]
		}
		sen := strings.TrimSpace(rest[:end])
		rest = rest[end:]
		w := len(strings.Fields(sen))
		if n+w > max {
			break
		}
		out, n = append(out, sen), n+w
	}
	if len(out) > 0 {
		return strings.Join(out, " ")
	}
	first := strings.Fields(t)[:max]
	for i := len(first) - 1; i > 0; i-- {
		if strings.HasSuffix(first[i], ",") {
			first = first[:i+1]
			break
		}
	}
	return strings.TrimRight(strings.Join(first, " "), ",;:") + "."
}

// Function words of each language: their count tells a French text from an English one (013-AC5).
var (
	frWords = wordSet("le la les un une des du de au aux et ou mais donc est sont suis était a ai as ont avons " +
		"je tu il elle on nous vous ils elles ce cette ces qui que quoi dans sur pour par avec sans pas ne plus " +
		"j'ai c'est l'on qu'il d'un d'une très aussi comme leur leurs son sa ses mon ma mes notre votre y en")
	enWords = wordSet("the a an and or but so is are was were be been am i you he she it we they this that " +
		"these those who which what in on at for by with without not no to of from as it's i'm we'll i've " +
		"have has had do does did will would can could should my your our their its there here")
	wordRE = regexp.MustCompile(`[\p{L}']+`)
)

func wordSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

// frenchCount is how many French and English function words text holds.
func frenchCount(text string) (fr, en int) {
	for _, w := range wordRE.FindAllString(strings.ToLower(strings.ReplaceAll(text, "’", "'")), -1) {
		if frWords[w] {
			fr++
		}
		if enWords[w] {
			en++
		}
	}
	return fr, en
}

// textLang is "fr" when texts hold more French function words than English ones, "en" on a tie
// or more English, and fallback when they hold none of either.
func textLang(fallback string, texts ...string) string {
	fr, en := 0, 0
	for _, t := range texts {
		f, e := frenchCount(t)
		fr, en = fr+f, en+e
	}
	switch {
	case fr == 0 && en == 0:
		return fallback
	case fr > en:
		return "fr"
	}
	return "en"
}

// turnSummary is what a persona that wrote no spoken.md says: its actions in the first person, in
// the language of its synthesis; nothing when it changed nothing (013-AC4).
func turnSummary(r store.TurnReport, synthesis string) string {
	fr := textLang("en", synthesis) == "fr"
	say := func(en, frs string, a ...any) string {
		if fr {
			return fmt.Sprintf(frs, a...)
		}
		return fmt.Sprintf(en, a...)
	}
	var out []string
	if len(r.Revised) > 0 {
		out = append(out, say("I changed %s.", "J'ai modifié %s.", docNames(r.Revised, fr)))
	}
	if len(r.Created) > 0 {
		out = append(out, say("I wrote %s.", "J'ai écrit %s.", docNames(r.Created, fr)))
	}
	if r.Annotated > 0 {
		out = append(out, say("I opened %s.", "J'ai ouvert %s.", count(r.Annotated, fr, "a note", "notes", "une note", "notes")))
	}
	if r.Replies > 0 {
		out = append(out, say("I answered %s.", "J'ai répondu à %s.", count(r.Replies, fr, "a thread", "threads", "une discussion", "discussions")))
	}
	return strings.Join(out, " ")
}

func count(n int, fr bool, en1, enN, fr1, frN string) string {
	switch {
	case n == 1 && fr:
		return fr1
	case n == 1:
		return en1
	case fr:
		return strconv.Itoa(n) + " " + frN
	}
	return strconv.Itoa(n) + " " + enN
}

// docNames says document names as words: "spec-paiement.md" is "spec paiement".
func docNames(names []string, fr bool) string {
	said := make([]string, len(names))
	for i, n := range names {
		n = strings.TrimSuffix(path.Base(n), path.Ext(n))
		said[i] = strings.Join(strings.FieldsFunc(n, func(r rune) bool { return r == '-' || r == '_' || r == '.' }), " ")
	}
	and := " and "
	if fr {
		and = " et "
	}
	if len(said) == 1 {
		return said[0]
	}
	return strings.Join(said[:len(said)-1], ", ") + and + said[len(said)-1]
}
