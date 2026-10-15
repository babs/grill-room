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

// recentLine is a stored voice line as the repeat check compares it.
type recentLine struct {
	source, norm string
	at           time.Time
}

// recentLines holds, by participant id, their voice lines of the last dedupeWindow. In memory: the
// room server runs one replica, and a restart forgets at most dedupeWindow of lines.
type recentLines map[int64][]recentLine

// sweep drops the lines older than dedupeWindow, and the participants left with none.
func (rl recentLines) sweep(now time.Time) {
	for pid, lines := range rl {
		kept := lines[:0]
		for _, l := range lines {
			if now.Sub(l.at) <= dedupeWindow {
				kept = append(kept, l)
			}
		}
		if len(kept) == 0 {
			delete(rl, pid)
		} else {
			rl[pid] = kept
		}
	}
}

// dup reports a line of pid from source, normalised to n, that repeats one of pid's lines from the
// other source in the window. Two lines from one source never repeat: a person may say a thing twice.
func (rl recentLines) dup(pid int64, source, n string, now time.Time) bool {
	rl.sweep(now)
	for _, l := range rl[pid] {
		if l.source != source && similar(l.norm, n) {
			return true
		}
	}
	return false
}

func (rl recentLines) add(pid int64, source, n string, now time.Time) {
	rl[pid] = append(rl[pid], recentLine{source: source, norm: n, at: now})
}

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
