package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/babs/grill-room/internal/monitor"
	"github.com/babs/grill-room/internal/sandbox"
)

// DefaultRetentionDays and MaxRetentionDays bound how long monitor.db keeps a turn (003-AC28).
const (
	DefaultRetentionDays = 30
	MaxRetentionDays     = 3650
)

// purgeEvery is how often the history is purged past the retention, after the purge at start.
var purgeEvery = 24 * time.Hour

// Alert thresholds (003-AC29).
const (
	contextAlertPct = 80
	queueAlertAfter = 10 * time.Minute
)

// turnStatus sorts a turn's end by its error: ok, cut off, a document conflict, out of time or
// out of login, or any other failure. Workers of other versions word errors alike.
func turnStatus(err string) string {
	switch {
	case err == "":
		return monitor.StatusOK
	case err == errCutOff.Error():
		return monitor.StatusCutOff
	case strings.Contains(err, "conflict on "):
		return monitor.StatusConflict
	case strings.HasPrefix(err, "the turn timed out"), strings.Contains(err, "login has expired"):
		return monitor.StatusExpired
	}
	return monitor.StatusFailed
}

// record stores a turn w's worker ended, with the account its room used (003-AC27).
func (s *Supervisor) record(w *worker, e *TurnEvent) {
	if s.o.Monitor == nil || e == nil || e.Phase != "end" {
		return
	}
	s.mu.Lock()
	id, account := w.inst.ID, w.inst.accountFor(e.Room)
	s.mu.Unlock()
	// Not the supervisor's context: a turn ending as it stops is still recorded.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 5*time.Second)
	defer cancel()
	err := s.o.Monitor.Record(ctx, monitor.Turn{Instance: id, Room: e.Room, TurnID: e.ID, Persona: e.Persona, Account: account,
		AskedModel: e.AskedModel, AskedEffort: e.AskedEffort, Model: e.Model, ModelNote: e.ModelNote, ModelMismatch: e.ModelMismatch,
		Effort: e.Effort, Cause: e.Cause,
		DurationMS: e.DurationMS, QueueMS: e.QueueMS, Status: turnStatus(e.Error), Error: e.Error,
		TokensIn: e.TokensIn, TokensOut: e.TokensOut, CacheRead: e.TokensCacheRead, CacheWrite: e.TokensCacheWrite, CostUSD: e.CostUSD,
		ContextTokens: e.ContextTokens, ContextWindow: e.ContextWindow, WorkspaceBytes: e.WorkspaceBytes})
	if err != nil {
		s.o.Log.Warn("monitor_record_failed", zap.String("instance", id), zap.Error(err))
	}
}

// purgeLoop purges the history past the retention now, then daily (003-AC28).
func (s *Supervisor) purgeLoop(ctx context.Context) {
	for {
		s.purge(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(purgeEvery):
		}
	}
}

func (s *Supervisor) purge(ctx context.Context) {
	n, err := s.o.Monitor.Purge(ctx, time.Duration(s.RetentionDays())*24*time.Hour)
	if err != nil {
		s.o.Log.Warn("monitor_purge_failed", zap.Error(err))
		return
	}
	if n > 0 {
		s.o.Log.Info("monitor_purged", zap.Int64("turns", n))
	}
}

// Monitor is the turn history, nil when the supervisor keeps none.
func (s *Supervisor) Monitor() *monitor.DB { return s.o.Monitor }

// RetentionDays is how long the history keeps a turn.
func (s *Supervisor) RetentionDays() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.set.retentionDays()
}

// SetRetention saves the history's retention and purges past it at once (003-AC28).
func (s *Supervisor) SetRetention(days int) error {
	if days < 1 || days > MaxRetentionDays {
		return fmt.Errorf("the retention must be 1 to %d days", MaxRetentionDays)
	}
	s.mu.Lock()
	set := s.set
	set.RetentionDays = days
	err := s.saveSettingsLocked(set)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.o.Log.Info("retention_changed", zap.Int("days", days))
	if s.o.Monitor != nil {
		s.purge(s.ctx)
	}
	return nil
}

// SetLimits saves an instance's sandbox limits; its worker restarts to apply them (003-AC26).
func (s *Supervisor) SetLimits(id string, l sandbox.Limits) error {
	if err := l.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.workers[id]
	if w == nil {
		return errors.New("no such instance")
	}
	w.inst.Limits = &l
	s.save(w)
	s.o.Log.Info("limits_changed", zap.String("instance", id), zap.String("cpus", l.CPUs), zap.String("memory", l.Memory),
		zap.String("egress_cpus", l.EgressCPUs), zap.String("egress_memory", l.EgressMemory))
	s.reload(w)
	return nil
}

// Alert is a warning at the top of the page (003-AC29).
type Alert struct {
	// Kind is "account", "unsigned", "context" or "queue".
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// Alerts gathers the page's warnings from what it shows and the history's context use.
func (s *Supervisor) Alerts(ctx context.Context, accounts []AccountView, insts []InstanceView, c ConcurrencyView) []Alert {
	var ctxs []monitor.Context
	if s.o.Monitor != nil {
		var err error
		if ctxs, err = s.o.Monitor.Contexts(ctx); err != nil {
			s.o.Log.Warn("monitor_contexts_failed", zap.Error(err))
		}
	}
	return alerts(accounts, insts, c, ctxs)
}

// alerts lists, in this order: a Claude login near or past its expiry, a worker running an
// unsigned binary, a persona session past 80% of its context in a room still served, and a turn
// waiting more than 10 minutes for a slot.
func alerts(accounts []AccountView, insts []InstanceView, c ConcurrencyView, ctxs []monitor.Context) []Alert {
	out := []Alert{}
	for _, a := range accounts {
		if a.Warning != "" {
			out = append(out, Alert{"account", "Claude account " + a.Name + ": " + a.Warning + "."})
		}
	}
	served := map[string]bool{}
	for _, in := range insts {
		if in.Unsigned {
			out = append(out, Alert{"unsigned", fmt.Sprintf("The worker for %s runs an unsigned binary (%s).", in.URL, in.Version)})
		}
		for _, r := range in.Rooms {
			served[in.ID+" "+r] = true
		}
	}
	for _, x := range ctxs {
		if x.Window <= 0 || !served[x.Instance+" "+x.Room] {
			continue
		}
		if x.Tokens*100 > contextAlertPct*x.Window {
			out = append(out, Alert{"context", fmt.Sprintf("%s in %s uses %d%% of its context: start a new session with handoff from the room.",
				x.Persona, x.Room, x.Tokens*100/x.Window)})
		}
	}
	for _, q := range c.Queued {
		if wait := time.Duration(q.SinceMS) * time.Millisecond; wait > queueAlertAfter {
			out = append(out, Alert{"queue", fmt.Sprintf("A turn of %s has waited %d minutes for a slot: raise the turns at once, or check the running turns.",
				q.Room, int(wait.Minutes()))})
		}
	}
	return out
}
