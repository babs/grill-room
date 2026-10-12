package web

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
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

// viewers counts each participant's open room streams: those with one are present.
type viewers struct {
	mu sync.Mutex
	m  map[int64]int // by participant id
}

func (v *viewers) add(pid int64, n int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.m[pid] += n; v.m[pid] <= 0 {
		delete(v.m, pid)
	}
}

func (v *viewers) present(pid int64) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.m[pid] > 0
}

// presencePersona is one persona as the preamble shows it.
type presencePersona struct {
	Name, Level, State, Trigger, Last string
	Turns                             int
}

// presenceView is what presence.tmpl renders.
type presenceView struct {
	Room     string
	Self     presencePersona
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

func triggerText(p *store.RoomPersona) string {
	if p.EveryN > 0 {
		return fmt.Sprintf("on demand and every %d rounds", p.EveryN)
	}
	return "on demand"
}

func chainText(c store.Chain) string {
	if c.Suspended {
		return fmt.Sprintf("unlimited, the limit is suspended (%d agent turns in this chain)", c.Count)
	}
	return fmt.Sprintf("%d of %d agent turns left", max(c.Budget-c.Count, 0), c.Budget)
}

// presencePreamble describes the room to persona self as its turn starts: the other personas and
// what they did, the humans present, the host and who speaks, and the chain budget (002-AC28).
// Models and efforts are what each persona runs at on the host daemon, after its cap.
func (s *Server) presencePreamble(ctx context.Context, room *store.Room, self *store.RoomPersona) (string, error) {
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
	v := presenceView{Room: oneLine(room.Name), SelfCall: self.SelfCall, Chain: chainText(room.Chain),
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
	for _, h := range humans {
		if h.ID == room.HostParticipantID {
			v.Host = oneLine(h.Name)
		}
		if s.viewers.present(h.ID) {
			v.Humans = append(v.Humans, oneLine(h.Name))
		}
	}
	if m, err := st.LastVoice(ctx, room.ID); err == nil && speakingAt(m, time.Now()) {
		v.Speaker = oneLine(m.Sender)
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	return renderPresence(v)
}
