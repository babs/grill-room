package web

import (
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
)

// 018-AC6: a line repeats another source's message when it matches the whole message or a
// word-aligned stretch of it; a short line that only ends the same way, or one word, does not.
func TestRepeats(t *testing.T) {
	long := strings.Repeat("nous parlerons du budget de la semaine prochaine ", 80)
	for _, c := range []struct {
		name, other, line string
		want              bool
	}{
		{"whole line", "We ship on Friday", "we ship ON friday", true},
		{"whole line, one word", "Oui", "oui", true},
		{"whole line, one word off", "The release goes out on Friday morning", "The release goes out on Friday mornings", true},
		{"end of a paragraph", "On termine ici pour aujourd'hui. On se revoit demain", "on se revoit demain", true},
		{"middle of a paragraph", "The rollout starts on Monday morning then we watch the error budget and we close the ticket on Friday",
			"then we watch the error budget", true},
		{"word off inside a paragraph", "First item done. Then we watch the errors budget and we close it", "then we watch the error budget", true},
		{"one word more inside a paragraph", "First item done. Then we watch the whole error budget and we close it", "then we watch the error budget", true},
		{"last words differ", "Merci à tous, on termine avec vous demain", "à demain", false},
		{"one word at the end", "Je ne sais pas, je crois que oui", "oui", false},
		{"different sentence", "We ship on Friday", "We wait for Monday", false},
		{"no stretch matches", long, "the release goes out on friday morning", false},
		{"stretch at the end of a long message", long + "the release goes out on friday morning", "the release goes out on friday morning", true},
	} {
		if got := repeats(normLine(c.other), normLine(c.line)); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// The trigram filter never turns away a pair similar would accept: it is a bound, not a guess.
func TestEnoughTrigramsIsABound(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	word := func() string { return []string{"la", "le", "on", "se", "voit", "demain", "budget", "ok"}[rng.IntN(8)] }
	for range 20000 {
		var b []string
		for range 1 + rng.IntN(12) {
			b = append(b, word())
		}
		r := []rune(strings.Join(b, " "))
		for range rng.IntN(len(r)/3 + 1) {
			i := rng.IntN(len(r))
			switch rng.IntN(3) {
			case 0:
				r = append(r[:i], r[i+1:]...)
			case 1:
				r[i] = rune('a' + rng.IntN(26))
			default:
				r = append(r[:i], append([]rune{rune('a' + rng.IntN(26))}, r[i:]...)...)
			}
		}
		a, bs := string(r), strings.Join(b, " ")
		if similar(a, bs) && !enoughTrigrams(sharedTrigrams(a, bs), len([]rune(a)), len([]rune(bs))) {
			t.Fatalf("filtered out a similar pair: %q, %q", a, bs)
		}
	}
}

// 018-AC6: the check runs under the write lock; a 4000-character message and a long line that
// matches nowhere stay cheap.
func BenchmarkRepeatsNoMatch(b *testing.B) {
	other := normLine(strings.Repeat("nous parlerons du budget de la semaine prochaine avec toute l'équipe ", 60)[:4000])
	line := normLine(strings.Repeat("the release goes out on friday morning after the checks ", 5))
	for b.Loop() {
		repeats(other, line)
	}
}

// sharedTrigrams counts the trigrams a and b have in common, repeats included.
func sharedTrigrams(a, b string) int {
	count := func(s string) map[string]int {
		r, m := []rune(s), map[string]int{}
		for i := 0; i+3 <= len(r); i++ {
			m[string(r[i:i+3])]++
		}
		return m
	}
	ca, cb, n := count(a), count(b), 0
	for g, k := range ca {
		n += min(k, cb[g])
	}
	return n
}

// The sliding trigram counts filter nothing similar would accept: repeats matches a plain scan of
// every stretch.
func TestRepeatsMatchesAPlainScan(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	vocab := []string{"on", "se", "revoit", "demain", "le", "budget", "oui", "à", "vous", "ok"}
	words := func(n int) []string {
		var out []string
		for range n {
			out = append(out, vocab[rng.IntN(len(vocab))])
		}
		return out
	}
	plain := func(other, norm string) bool {
		if similar(other, norm) {
			return true
		}
		ow, nw := strings.Fields(other), len(strings.Fields(norm))
		if nw < 2 {
			return false
		}
		for n := nw - 1; n <= nw+1; n++ {
			for i := 0; i+n <= len(ow); i++ {
				if similar(strings.Join(ow[i:i+n], " "), norm) {
					return true
				}
			}
		}
		return false
	}
	// An edited copy of the line inside the message: near the similarity threshold, where the
	// filter's bound is tightest.
	edit := func(s string) string {
		r := []rune(s)
		for range rng.IntN(len(r)/4 + 1) {
			i := rng.IntN(len(r))
			if rng.IntN(2) == 0 {
				r = append(r[:i], r[i+1:]...)
			} else {
				r[i] = rune('a' + rng.IntN(26))
			}
			if len(r) == 0 {
				break
			}
		}
		return string(r)
	}
	hits := 0
	for range 20000 {
		norm := normLine(strings.Join(words(1+rng.IntN(12)), " "))
		other := normLine(strings.Join(words(rng.IntN(10)), " ") + " " + edit(norm) + " " + strings.Join(words(rng.IntN(10)), " "))
		want := plain(other, norm)
		if want {
			hits++
		}
		if got := repeats(other, norm); got != want {
			t.Fatalf("repeats(%q, %q) = %v, a plain scan says %v", other, norm, got, want)
		}
	}
	if hits < 100 {
		t.Fatalf("only %d matches: the corpus does not exercise the filter", hits)
	}
}

// stretches counts each stretch's shared trigrams as a fresh count of that stretch would.
func TestStretchesCountTrigrams(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	vocab := []string{"la", "lala", "on", "se", "revoit", "demain", "aaa", "é"}
	for range 2000 {
		var o, l []string
		for range rng.IntN(15) {
			o = append(o, vocab[rng.IntN(len(vocab))])
		}
		for range 1 + rng.IntN(5) {
			l = append(l, vocab[rng.IntN(len(vocab))])
		}
		other, norm := strings.Join(o, " "), strings.Join(l, " ")
		seen := 0
		ws := splitWords(other)
		ws.stretches(norm, 1, 4, func(i, w, common int) bool {
			seen++
			if s := string(ws.r[ws.starts[i]:ws.ends[i+w-1]]); common != sharedTrigrams(s, norm) {
				t.Fatalf("stretch %q of %q against %q: %d trigrams shared, want %d", s, other, norm, common, sharedTrigrams(s, norm))
			}
			return true
		})
		if want := max(0, len(o)) + max(0, len(o)-1) + max(0, len(o)-2) + max(0, len(o)-3); seen != want {
			t.Fatalf("%q: %d stretches, want %d", other, seen, want)
		}
	}
}

// shuffledVocabulary is a repeat check near the costliest a participant can ask for: a paragraph
// of size characters and a line of n of its own words, shuffled, which shares enough trigrams with
// every stretch to reach the distance.
func shuffledVocabulary(size, n int) (other, line string) {
	rng := rand.New(rand.NewPCG(7, 8))
	vocab := strings.Fields("nous allons regarder le budget de la semaine prochaine avec toute l'équipe puis on termine")
	var p []string
	for len(strings.Join(p, " ")) < size {
		p = append(p, vocab[rng.IntN(len(vocab))])
	}
	other = normLine(strings.Join(p, " ")[:size])
	w := strings.Fields(other)
	l := make([]string, n)
	for i := range l {
		l[i] = w[rng.IntN(len(w))]
	}
	return other, strings.Join(l, " ")
}

func BenchmarkRepeatsShuffledVocabulary(b *testing.B) {
	for _, c := range []struct{ size, words int }{{1200, 120}, {4000, 120}, {4000, 400}} {
		other, line := shuffledVocabulary(c.size, c.words)
		b.Run(strconv.Itoa(c.size)+"/"+strconv.Itoa(c.words), func(b *testing.B) {
			for b.Loop() {
				repeats(other, line)
			}
		})
	}
}

// 018-AC6: against a 4000-character paragraph, the repeat check of a line of any width stays under
// 30 million block steps, about 90 ms: the plain DP took close to a billion cells.
func TestRepeatsShuffledVocabularyCost(t *testing.T) {
	ops := 0
	distanceOps = func(n int) { ops += n }
	defer func() { distanceOps = nil }()
	for _, words := range []int{120, 200, 400, 600} {
		ops = 0
		other, line := shuffledVocabulary(4000, words)
		repeats(other, line)
		t.Logf("%d words: %d block steps", words, ops)
		if ops == 0 || ops > 30_000_000 {
			t.Errorf("%d words: %d block steps, want 1 to 30M", words, ops)
		}
	}
}

// similar's formula accepts maxDistance(n) and refuses one more, for every length a line can have.
func TestMaxDistanceIsSimilarsThreshold(t *testing.T) {
	for n := 1; n <= maxBodyLen; n++ {
		ok := func(d int) bool { return 1-float64(d)/float64(n) >= dedupeSimilarity }
		if k := maxDistance(n); !ok(k) || ok(k+1) {
			t.Fatalf("n %d: maxDistance %d, the formula accepts %d: %v, %d: %v", n, k, k, ok(k), k+1, ok(k+1))
		}
	}
}

// fullLevenshtein is the plain DP over runes: what line.distance must equal.
func fullLevenshtein(a, b []rune) int {
	prev, cur := make([]int, len(b)+1), make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := range a {
		cur[0] = i + 1
		for j := range b {
			cost := 1
			if a[i] == b[j] {
				cost = 0
			}
			cur[j+1] = min(prev[j+1]+1, cur[j]+1, prev[j]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// The bit-parallel distance equals the plain DP, across block edges (63, 64, 65, 128 runes), on
// random strings and on shuffled vocabularies; similar gives the plain formula's answers.
func TestLineDistanceIsLevenshtein(t *testing.T) {
	rng := rand.New(rand.NewPCG(9, 10))
	vocab := strings.Fields("on se revoit demain le budget oui à vous été")
	gen := func(n int) []rune {
		if rng.IntN(2) == 0 {
			r := make([]rune, n)
			for i := range r {
				r[i] = []rune("abcé ")[rng.IntN(5)]
			}
			return r
		}
		var w []string
		for len([]rune(strings.Join(w, " "))) < n {
			w = append(w, vocab[rng.IntN(len(vocab))])
		}
		return []rune(strings.Join(w, " "))[:n]
	}
	sizes := []int{0, 1, 2, 5, 63, 64, 65, 127, 128, 129, 200}
	for range 3000 {
		a, b := gen(sizes[rng.IntN(len(sizes))]+rng.IntN(3)), gen(sizes[rng.IntN(len(sizes))]+rng.IntN(3))
		if rng.IntN(3) == 0 {
			b = []rune(edited(rng, string(a)))
		}
		want := fullLevenshtein(a, b)
		if got := newLine(b).distance(a); got != want {
			t.Fatalf("distance(%q, %q) = %d, want %d", string(a), string(b), got, want)
		}
		n := max(len(a), len(b))
		plain := n == 0 || 1-float64(want)/float64(n) >= dedupeSimilarity
		if got := similar(string(a), string(b)); got != plain {
			t.Fatalf("similar(%q, %q) = %v, the formula says %v (distance %d)", string(a), string(b), got, plain, want)
		}
	}
}

// edited is s with up to a quarter of its runes deleted, replaced or inserted.
func edited(rng *rand.Rand, s string) string {
	r := []rune(s)
	for range rng.IntN(len(r)/4 + 1) {
		if len(r) == 0 {
			break
		}
		i := rng.IntN(len(r))
		switch rng.IntN(3) {
		case 0:
			r = append(r[:i], r[i+1:]...)
		case 1:
			r[i] = rune('a' + rng.IntN(26))
		default:
			r = append(r[:i], append([]rune{rune('a' + rng.IntN(26))}, r[i:]...)...)
		}
	}
	return string(r)
}
