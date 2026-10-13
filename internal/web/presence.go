package web

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/babs/grill-room/internal/store"
	"github.com/babs/grill-room/internal/turn"
)

// presenceSrc is the one template of the presence preamble: edit it to change what agents are told.
//
//go:embed presence.tmpl
var presenceSrc string

var presenceTmpl = template.Must(template.New("presence").
	Funcs(template.FuncMap{"join": func(s []string) string { return strings.Join(s, ", ") }}).
	Parse(presenceSrc))

const (
	// presenceLineMax caps each preamble line: a long synthesis or room must not crowd the rest.
	presenceLineMax = 100
	// speakerWindow is how recent a voice line must be for its speaker to be speaking now.
	speakerWindow = 10 * time.Second
)

// presencePersona is one persona as the preamble shows it.
type presencePersona struct {
	Name, Level, State, Trigger, Last string
	Turns                             int
}

// presenceView is what presence.tmpl renders.
type presenceView struct {
	Room string
	// Agenda is set when the room has its agenda.md, which every persona aligns on (006-AC14).
	Agenda bool
	Self   presencePersona
	// Why is the turn's cause (003-AC22).
	Why      string
	SelfCall bool
	Others   []presencePersona
	Humans   []string
	Host     string
	Speaker  string
	Chain    string
}

// oneLine folds s on one line: names and syntheses must not break the preamble's layout.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// renderPresence renders v with each line cut at presenceLineMax runes.
func renderPresence(v presenceView) (string, error) {
	var b strings.Builder
	if err := presenceTmpl.Execute(&b, v); err != nil {
		return "", fmt.Errorf("presence preamble: %w", err)
	}
	lines := strings.Split(b.String(), "\n")
	for i, l := range lines {
		if r := []rune(l); len(r) > presenceLineMax {
			lines[i] = string(r[:presenceLineMax-1]) + "…"
		}
	}
	return strings.Join(lines, "\n"), nil
}

// speakingAt reports the voice line m's author as still speaking at now.
func speakingAt(m *store.Message, now time.Time) bool { return now.Sub(m.TS) <= speakerWindow }

func levelText(l turn.Level) string {
	return "model " + orDefault(l.Model) + ", effort " + orDefault(l.Effort)
}

func orDefault(s string) string {
	if s == "" {
		return "default"
	}
	return s
}

// everyRounds is a persona's "every N rounds" trigger in words.
func everyRounds(n int) string {
	if n == 1 {
		return "every round"
	}
	return fmt.Sprintf("every %d rounds", n)
}

// everyVoice and everyMinutes are the 006-AC18 triggers in words.
func everyVoice(n int) string {
	if n == 1 {
		return "every voice line"
	}
	return fmt.Sprintf("every %d voice lines", n)
}

func everyMinutes(n int) string {
	if n == 1 {
		return "every minute"
	}
	return fmt.Sprintf("every %d minutes", n)
}

func triggerText(p *store.RoomPersona) string {
	switch {
	case p.EveryN > 0:
		return "on demand and " + everyRounds(p.EveryN)
	case p.EveryVoice > 0:
		return "on demand and " + everyVoice(p.EveryVoice)
	case p.EveryMinutes > 0:
		return "on demand and " + everyMinutes(p.EveryMinutes)
	}
	return "on demand"
}

func chainText(c store.Chain) string {
	if c.Suspended {
		return fmt.Sprintf("unlimited, the limit is suspended (%d agent turns in this chain)", c.Count)
	}
	return fmt.Sprintf("%d of %d agent turns left", max(c.Budget-c.Count, 0), c.Budget)
}

// humanTags returns, by participant id, the short id the preamble shows after each human's name,
// so two people with the same name stay apart (003-AC32): the first 4 hex digits of their
// profile's uuid, or 6 or 8 for the whole room once 4 would show two profiles alike. A participant
// with no profile gets "#" and their participant id, which no profile tag can look like.
func humanTags(ps []store.Participant) map[int64]string {
	hex := func(p store.Participant) string { return strings.ReplaceAll(p.ProfileUUID, "-", "") }
	n := 4
	for ; n < 8; n += 2 {
		seen := map[string]string{}
		clash := false
		for _, p := range ps {
			if h := hex(p); len(h) >= 8 {
				if other, ok := seen[h[:n]]; ok && other != h {
					clash = true
				}
				seen[h[:n]] = h
			}
		}
		if !clash {
			break
		}
	}
	tags := make(map[int64]string, len(ps))
	for _, p := range ps {
		if h := hex(p); len(h) >= 8 {
			tags[p.ID] = h[:n]
		} else {
			tags[p.ID] = "#" + strconv.FormatInt(p.ID, 10)
		}
	}
	return tags
}

// tagged is a human's name with its short id, "Alice ·a1b2"; a name with no id stays bare.
func tagged(name, tag string) string {
	if tag == "" {
		return oneLine(name)
	}
	return oneLine(name) + " ·" + tag
}

// presencePreamble describes the room to persona self as its turn starts: why it runs, the other
// personas and what they did, the humans present, the host and who speaks, and the chain budget
// (002-AC28, 003-AC22). Models and efforts are what each persona runs at on the host daemon, after
// its cap.
func (s *Server) presencePreamble(ctx context.Context, room *store.Room, self *store.RoomPersona, cause string) (string, error) {
	st := s.bus.Store
	personas, err := st.RoomPersonas(ctx, room.ID)
	if err != nil {
		return "", err
	}
	stats, err := st.PersonaStats(ctx, room.ID)
	if err != nil {
		return "", err
	}
	queue, err := st.Queue(ctx, room.ID)
	if err != nil {
		return "", err
	}
	running, err := st.RunningTurns(ctx, room.ID)
	if err != nil {
		return "", err
	}
	host, err := st.HostDaemon(ctx, room.ID)
	if errors.Is(err, store.ErrNotFound) {
		host = nil
	} else if err != nil {
		return "", err
	}
	level := func(p *store.RoomPersona) string {
		if host == nil {
			return levelText(p.Asks())
		}
		runs, _ := capped(host, p)
		return levelText(runs)
	}
	docs, err := st.Documents(ctx, room.ID)
	if err != nil {
		return "", err
	}
	v := presenceView{Room: oneLine(room.Name), Agenda: slices.ContainsFunc(docs, isAgenda), SelfCall: self.SelfCall, Chain: chainText(room.Chain), Why: oneLine(cause),
		Self: presencePersona{Name: self.Name, Level: level(self), Trigger: triggerText(self)}}
	for i := range personas {
		p := &personas[i]
		if p.ID == self.ID {
			continue
		}
		state := "idle"
		switch {
		case p.Pending:
			state = "waiting for the host's approval"
		case slices.ContainsFunc(running, func(t store.Turn) bool { return t.PersonaID == p.ID && !t.HostQueued }):
			state = "running"
		case slices.ContainsFunc(running, func(t store.Turn) bool { return t.PersonaID == p.ID }):
			state = "queued"
		case slices.ContainsFunc(queue, func(q store.QueuedTurn) bool { return q.PersonaID == p.ID }):
			state = "queued"
		}
		last, _, _ := strings.Cut(strings.TrimSpace(stats[p.ID].Last), "\n")
		v.Others = append(v.Others, presencePersona{Name: p.Name, Level: level(p), State: state, Turns: stats[p.ID].Turns,
			Trigger: triggerText(p), Last: oneLine(last)})
	}
	humans, err := st.Participants(ctx, room.ID)
	if err != nil {
		return "", err
	}
	tags := humanTags(humans)
	named := func(id int64, name string) string { return tagged(name, tags[id]) }
	for _, h := range humans {
		if h.ID == room.HostParticipantID {
			v.Host = named(h.ID, h.Name)
		}
		if s.people.here(room.ID, h.ID, s.now()) {
			v.Humans = append(v.Humans, named(h.ID, h.Name))
		}
	}
	if m, err := st.LastVoice(ctx, room.ID); err == nil && speakingAt(m, time.Now()) {
		v.Speaker = named(m.ParticipantID, m.Sender)
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	return renderPresence(v)
}

// isAgenda tells the room's agenda: the agent document named agenda.md (006-AC14).
func isAgenda(d store.Document) bool { return d.Kind == store.DocAgent && d.Name == store.AgendaName }
