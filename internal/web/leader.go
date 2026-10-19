package web

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// leaderEvery is how often a follower tries the leader lock and the leader checks it still holds
// it, and turnSweepEvery how often the leader looks for turns to fail. Vars for tests.
var (
	leaderEvery    = 5 * time.Second
	turnSweepEvery = 10 * time.Second
)

// Lead runs the singletons while this pod holds the cluster's leader lock, until ctx ends: they
// start when it takes the lock and stop as soon as it loses it (017-AC17).
func (s *Server) Lead(ctx context.Context) {
	var stop func()
	s.bus.Store.Leader(ctx, s.bus.Pod(), leaderEvery, func(leader bool) {
		if !leader {
			// ponytail: a killed session shows at the next ping, up to leaderEvery after another pod
			// took the lock: both run the loops meanwhile, each idempotent or deduplicated, though a
			// push may go twice. Fence the loops with the lock if that ever matters.
			s.leader.Store(false)
			stop()
			s.log.Info("leader_lost", zap.String("pod", s.bus.Pod()))
			return
		}
		loops, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		for _, loop := range s.singletons {
			wg.Go(func() { loop(loops) })
		}
		stop = func() { cancel(); wg.Wait() }
		s.leader.Store(true)
		s.log.Info("leader_elected", zap.String("pod", s.bus.Pod()))
	}, func(err error) { s.log.Warn("leader_failed", zap.Error(err)) })
}

// Leader reports whether this pod runs the singletons now.
func (s *Server) Leader() bool { return s.leader.Load() }

// SweepTurns runs sweepTurns every turnSweepEvery until ctx ends.
func (s *Server) SweepTurns(ctx context.Context) {
	t := time.NewTicker(turnSweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepTurns(ctx)
		}
	}
}

// sweepTurns fails the running turns whose daemon holds no live connection on any pod, as when it
// vanished with its pod, and those past the turn timeout plus the grace, counted from when they
// left the host's queue, for a daemon that neither answers nor drops (003-AC18, 017-AC18). Failing
// a turn is conditional on it running, so a pod's own disconnect path racing this posts one line.
func (s *Server) sweepTurns(ctx context.Context) {
	ts, err := s.bus.Store.AllRunningTurns(ctx)
	if err != nil || len(ts) == 0 {
		if err != nil && ctx.Err() == nil {
			s.log.Warn("turn_sweep_failed", zap.Error(err))
		}
		return
	}
	// Read after the turns: each listed turn started on a daemon live then, so one missing here left.
	live, err := s.bus.Store.LiveDaemons(ctx, "", s.lease().UnixMilli())
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("turn_sweep_failed", zap.Error(err))
		}
		return
	}
	ids := map[int64]bool{}
	for _, d := range live {
		ids[d.ID] = true
	}
	now := s.now()
	for _, t := range ts {
		switch {
		case !ids[t.DaemonID]:
			s.failTurn(ctx, t.RoomID, t.ID, "the host's daemon disconnected")
		case !t.HostQueued && !now.Before(t.StartedAt.Add(s.opts.TurnTimeout+turnGrace)):
			s.failTurn(ctx, t.RoomID, t.ID, "timed out after "+s.opts.TurnTimeout.String())
		}
	}
}
