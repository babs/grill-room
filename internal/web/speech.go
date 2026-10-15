package web

// Spike S14, behind GRILL_ROOM_PERSONA_VOICE=on: what a persona says in the audio call after its
// turn, queued in memory for the voice bot to fetch. Lost on restart, which a spoken line may be.

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/store"
)

const (
	// maxSpeech bounds the queue: a bot away for long finds only the latest lines.
	maxSpeech = 200
	// maxSpokenWords caps a line whatever the agent wrote: 60 asked, some slack before cutting.
	maxSpokenWords = 80
)

// Speech is one line a persona says in a room's call.
type Speech struct {
	Seq     int64  `json:"seq"`
	Room    string `json:"room"`
	Persona int64  `json:"persona_id"`
	Name    string `json:"persona"`
	// Slot is the persona's rank in the room, by id: the bot gives each slot its own voice.
	Slot int       `json:"slot"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`
	// Source is "spoken" for the agent's spoken.md, "summary" for the server's fallback.
	Source string `json:"source"`
}

type speechQueue struct {
	mu    sync.Mutex
	seq   int64
	items []Speech
}

func (q *speechQueue) add(s Speech) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.seq++
	s.Seq = q.seq
	q.items = append(q.items, s)
	if len(q.items) > maxSpeech {
		q.items = slices.Delete(q.items, 0, len(q.items)-maxSpeech)
	}
}

// after returns the lines past seq, and the last seq given out: below the bot's own, the server
// restarted and numbering began again.
func (q *speechQueue) after(seq int64) ([]Speech, int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	i, _ := slices.BinarySearchFunc(q.items, seq+1, func(s Speech, n int64) int { return int(s.Seq - n) })
	// Never nil: the bot reads an empty list, not null.
	return append([]Speech{}, q.items[i:]...), q.seq
}

// getSpeech hands the voice bot the lines after ?after=<seq> and the last seq; it is an STT-token
// service like the worker.
func (s *Server) getSpeech(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	items, last := s.speech.after(after)
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "last": last})
}

// queueSpeech queues what the persona of turn t says: its spoken.md, or a summary of the turn
// built from the report when it wrote none.
func (s *Server) queueSpeech(ctx context.Context, t *store.Turn, spoken, synthesis string) {
	text, source := spokenText(spoken), "spoken"
	if text == "" {
		text, source = turnSummary(t.Report, synthesis), "summary"
	}
	if text == "" {
		return
	}
	personas, err := s.bus.Store.RoomPersonas(ctx, t.RoomID)
	if err != nil {
		s.log.Warn("speech_skipped", zap.String("room", t.RoomID), zap.Error(err))
		return
	}
	slot := slices.IndexFunc(personas, func(p store.RoomPersona) bool { return p.ID == t.PersonaID })
	s.speech.add(Speech{Room: t.RoomID, Persona: t.PersonaID, Name: t.PersonaName, Slot: max(slot, 0), Text: text, At: time.Now(), Source: source})
	s.log.Info("speech_queued", zap.String("room", t.RoomID), zap.Int64("turn", t.ID), zap.String("source", source), zap.Int("words", len(strings.Fields(text))))
}

var (
	mdLink    = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	bareURL   = regexp.MustCompile(`https?://\S+|www\.\S+`)
	shortID   = regexp.MustCompile(`\s?·\s?#?[0-9a-f]+\b`)
	hashNum   = regexp.MustCompile(`#(\d+)`)
	mdMarks   = regexp.MustCompile("[*_`~>|]+|^#+\\s*|^\\s*[-+]\\s+")
	sentences = regexp.MustCompile(`[^.!?]+[.!?]*`)
)

// spokenText is spoken.md made safe to synthesise, whatever the agent did with the rules: no
// Markdown mark, link, URL or short id is read out, and the line stops at a sentence end near
// maxSpokenWords.
func spokenText(md string) string {
	var lines []string
	for _, l := range strings.Split(md, "\n") {
		l = mdLink.ReplaceAllString(l, "$1")
		l = bareURL.ReplaceAllString(l, "")
		l = shortID.ReplaceAllString(l, "")
		l = hashNum.ReplaceAllString(l, "$1")
		l = mdMarks.ReplaceAllString(l, "")
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	text := strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
	if len(strings.Fields(text)) <= maxSpokenWords {
		return text
	}
	var b strings.Builder
	words := 0
	for _, sen := range sentences.FindAllString(text, -1) {
		if n := len(strings.Fields(sen)); words+n > maxSpokenWords {
			break
		} else {
			words += n
		}
		b.WriteString(sen)
	}
	if words == 0 {
		// A first sentence over the cap, punctuation or not, is cut at the cap.
		return strings.Join(strings.Fields(text)[:maxSpokenWords], " ")
	}
	return strings.TrimSpace(b.String())
}

var frenchWords = regexp.MustCompile(`(?i)\b(le|la|les|des|une|est|et|pour|dans|sur|que|qui|pas|j'ai)\b`)

// turnSummary is the fallback when the agent wrote no spoken.md: what the turn did, in the first
// person, in the language its synthesis looks written in. Without changes, it says nothing.
func turnSummary(r store.TurnReport, synthesis string) string {
	fr := len(frenchWords.FindAllString(synthesis, -1)) > len(strings.Fields(synthesis))/10
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
		out = append(out, say("I opened %s.", "J'ai ouvert %s.", count(r.Annotated, "a note", "notes", "une note", "notes", fr)))
	}
	if r.Replies > 0 {
		out = append(out, say("I answered %s.", "J'ai répondu à %s.", count(r.Replies, "a note", "notes", "une note", "notes", fr)))
	}
	return strings.Join(out, " ")
}

func count(n int, en1, enN, fr1, frN string, fr bool) string {
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
		n = strings.TrimSuffix(n, path.Ext(n))
		said[i] = strings.Join(strings.FieldsFunc(n, func(r rune) bool { return r == '-' || r == '_' }), " ")
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
