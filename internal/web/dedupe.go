package web

import (
	"strings"
	"sync/atomic"
	"time"
	"unicode"

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

// similar reports 1 - levenshtein(a, b) / max(len) >= dedupeSimilarity, over runes.
func similar(a, b string) bool {
	ra, rb := []rune(a), []rune(b)
	if len(ra) < len(rb) {
		ra, rb = rb, ra
	}
	if len(ra) == 0 {
		return true
	}
	// The distance is at least the length gap: most different lines stop here, before the
	// quadratic part.
	if float64(len(ra)-len(rb)) > (1-dedupeSimilarity)*float64(len(ra)) {
		return false
	}
	return 1-float64(levenshtein(ra, rb))/float64(len(ra)) >= dedupeSimilarity
}

func levenshtein(a, b []rune) int {
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

func sourceCounters() map[string]*atomic.Int64 {
	m := make(map[string]*atomic.Int64, len(VoiceSources))
	for _, s := range VoiceSources {
		m[s] = new(atomic.Int64)
	}
	return m
}
