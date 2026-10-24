package web

import (
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	// dedupeWindow and dedupeSimilarity tell a voice line that repeats another source's: during a
	// handover both the device and the worker may transcribe the same utterance (010-AC16).
	dedupeWindow     = 5 * time.Second
	dedupeSimilarity = 0.8
)

// normLine is a line as the repeat check compares it: lowercase, accents dropped, letters and
// digits only, one space between words.
func normLine(s string) string {
	var b strings.Builder
	space := false
	for _, r := range norm.NFD.String(s) {
		switch {
		case unicode.Is(unicode.Mn, r):
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(unicode.ToLower(r))
		default:
			space = true
		}
	}
	return b.String()
}

// normCap bounds the runes repeats compares: NFD can triple a body (Hangul syllables), and the
// check runs under the write lock.
const normCap = maxBodyLen

// capRunes keeps the first n runes of s, or its last n when tail is set.
func capRunes(s string, n int, tail bool) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	if tail {
		return string(r[len(r)-n:])
	}
	return string(r[:n])
}

// repeats reports whether norm, a normalised line, repeats other, a normalised voice message of the
// other source: the whole of it, or for a line of 2 words or more, a stretch of it as many words
// long, give or take one, as a paragraph grown by appends holds its lines (018-AC6).
// ponytail: peak measured ~94 ms, 26.4M block steps, under the write lock (4000-rune paragraph, 500-word line of its words; sweep 120-1000 words, French and Hangul); lower normCap to go lower.
func repeats(other, norm string) bool {
	// A paragraph grows at its end, where the other source's copy lands: keep other's tail.
	other, norm = capRunes(other, normCap, true), capRunes(norm, normCap, false)
	if similar(other, norm) {
		return true
	}
	nw := len(strings.Fields(norm))
	if nw < 2 {
		return false
	}
	ws := splitWords(other)
	l := newLine([]rune(norm))
	// The trigram bound rules out most stretches; from each first word, one distance run reads the
	// three widths, which are prefixes of each other.
	widest := make([]int, len(ws.starts))
	ws.stretches(norm, nw-1, nw+1, func(i, w, common int) bool {
		if enoughTrigrams(common, ws.ends[i+w-1]-ws.starts[i], l.n) {
			widest[i] = w
		}
		return true
	})
	for i, w := range widest {
		if w == 0 {
			continue
		}
		a, v := ws.starts[i], max(nw-1, 1)
		found := false
		l.scores(ws.r[a:ws.ends[i+w-1]], func(j, d int) bool {
			for ; v <= w && ws.ends[i+v-1]-a < j; v++ {
			}
			if v <= w && ws.ends[i+v-1]-a == j {
				n := max(j, l.n)
				found = 1-float64(d)/float64(n) >= dedupeSimilarity
			}
			return !found
		})
		if found {
			return true
		}
	}
	return false
}

// wordSpans is a normalised string, its words split by single spaces, as rune offsets.
type wordSpans struct {
	r            []rune
	starts, ends []int
}

func splitWords(s string) wordSpans {
	ws := wordSpans{r: []rune(s)}
	for i, c := range ws.r {
		if c != ' ' && (i == 0 || ws.r[i-1] == ' ') {
			ws.starts = append(ws.starts, i)
		}
		if c != ' ' && (i == len(ws.r)-1 || ws.r[i+1] == ' ') {
			ws.ends = append(ws.ends, i+1)
		}
	}
	return ws
}

// stretches calls yield with each stretch of minW to maxW words, as its first word i and width w,
// and the trigrams it shares with norm, until yield returns false. The counts slide with the
// stretch: each rune enters and leaves once per width.
func (ws wordSpans) stretches(norm string, minW, maxW int, yield func(i, w, common int) bool) {
	r := ws.r
	nr := []rune(norm)
	need := map[uint64]int{}
	for p := 0; p+3 <= len(nr); p++ {
		need[gram(nr, p)]++
	}
	for n := max(minW, 1); n <= maxW && n <= len(ws.starts); n++ {
		have := map[uint64]int{}
		common, lo, hi := 0, 0, 0
		for i := 0; i+n <= len(ws.starts); i++ {
			a, b := ws.starts[i], ws.ends[i+n-1]
			for ; lo < a; lo++ {
				if lo < hi {
					g := gram(r, lo)
					have[g]--
					if have[g] < need[g] {
						common--
					}
				}
			}
			hi = max(hi, lo)
			for ; hi < b-2; hi++ {
				g := gram(r, hi)
				if have[g] < need[g] {
					common++
				}
				have[g]++
			}
			if !yield(i, n, common) {
				return
			}
		}
	}
}

// enoughTrigrams is false when strings of la and lb runes sharing common trigrams cannot be similar:
// an edit removes at most 3 of the longer one's, so a distance of k leaves max(la, lb)-2-3k in
// common. It keeps the quadratic similar off most stretches: the check runs under the write lock.
func enoughTrigrams(common, la, lb int) bool {
	longest := max(la, lb)
	return longest == 0 || common >= longest-2-3*maxDistance(longest)
}

// gram packs the 3 runes of r at p, 21 bits each.
func gram(r []rune, p int) uint64 {
	//nolint:gosec // r comes from a string: every rune is in 0..0x10FFFF
	return uint64(r[p])<<42 | uint64(r[p+1])<<21 | uint64(r[p+2])
}

// similar reports 1 - levenshtein(a, b) / max(len) >= dedupeSimilarity, over runes.
func similar(a, b string) bool { return newLine([]rune(b)).similar([]rune(a)) }

// maxDistance is the largest levenshtein distance similar accepts between strings whose longer
// one has n > 0 runes: 1 - d/n >= 0.8 is d <= n/5.
func maxDistance(n int) int { return n / 5 }

// distanceOps lets a test count the block steps distance runs: a cost bound that wall time under
// -race and a loaded CI runner cannot give.
var distanceOps func(n int)

// line is a string prepared for Myers' bit-parallel levenshtein (Hyyrö's blocks of 64 runes): a
// distance costs len(text) * ceil(len(line)/64) word operations, not their product. repeats
// compares one line with hundreds of stretches under the write lock, on text a participant chooses.
type line struct {
	n int
	// eq holds, per rune, the bits of the line's positions that hold it.
	eq   map[rune][]uint64
	none []uint64
}

func newLine(r []rune) *line {
	blocks := (len(r) + 63) / 64
	l := &line{n: len(r), eq: map[rune][]uint64{}, none: make([]uint64, blocks)}
	for i, c := range r {
		if l.eq[c] == nil {
			l.eq[c] = make([]uint64, blocks)
		}
		l.eq[c][i/64] |= 1 << (i % 64)
	}
	return l
}

// similar is similar(string(text), the line).
func (l *line) similar(text []rune) bool {
	n := max(len(text), l.n)
	if n == 0 {
		return true
	}
	// The distance is at least the length gap: most different lines stop here.
	gap := len(text) - l.n
	if max(gap, -gap) > maxDistance(n) {
		return false
	}
	return 1-float64(l.distance(text))/float64(n) >= dedupeSimilarity
}

// distance is levenshtein(text, the line).
func (l *line) distance(text []rune) int {
	d := l.n
	l.scores(text, func(_, s int) bool { d = s; return true })
	return d
}

// scores calls yield with levenshtein(text[:j], the line) for j from 1, until it returns false.
// Each column of the DP is a vertical delta vector, +1 or -1 bits in pv and mv, and the score
// follows its bottom cell.
func (l *line) scores(text []rune, yield func(j, d int) bool) {
	if l.n == 0 {
		for j := range text {
			if !yield(j+1, j+1) {
				return
			}
		}
		return
	}
	blocks := len(l.none)
	if distanceOps != nil {
		distanceOps(len(text) * blocks)
	}
	pv, mv := make([]uint64, blocks), make([]uint64, blocks)
	for b := range pv {
		pv[b] = ^uint64(0)
	}
	last := uint((l.n - 1) % 64) //nolint:gosec // l.n > 0 here: 0..63
	score := l.n
	for j, c := range text {
		eqs := l.eq[c]
		if eqs == nil {
			eqs = l.none
		}
		hin := 1 // the top row is 0, 1, 2...: every column adds 1 there
		for b := range blocks {
			eq, p, m := eqs[b], pv[b], mv[b]
			xv := eq | m
			if hin < 0 {
				eq |= 1
			}
			xh := (((eq & p) + p) ^ p) | eq
			ph := m | ^(xh | p)
			mh := p & xh
			bit := uint(63)
			if b == blocks-1 {
				bit = last
			}
			hout := int(ph>>bit&1) - int(mh>>bit&1)
			ph, mh = ph<<1, mh<<1
			if hin < 0 {
				mh |= 1
			} else if hin > 0 {
				ph |= 1
			}
			pv[b], mv[b] = mh|^(xv|ph), ph&xv
			hin = hout
		}
		score += hin
		if !yield(j+1, score) {
			return
		}
	}
}

func sourceCounters() map[string]*atomic.Int64 {
	m := make(map[string]*atomic.Int64, len(VoiceSources))
	for _, s := range VoiceSources {
		m[s] = new(atomic.Int64)
	}
	return m
}
