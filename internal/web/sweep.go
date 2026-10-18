package web

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// sharedSweepEvery is how often SweepShared drops the expired shared state.
const sharedSweepEvery = time.Minute

// SweepShared deletes the shared state whose lease ended, and the rate events past every limiter
// window, every minute until ctx ends. Every pod runs it: the sweep is idempotent.
func (s *Server) SweepShared(ctx context.Context) {
	tick := time.NewTicker(sharedSweepEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now := s.now()
		if err := s.bus.Store.SweepExpired(ctx, now.UnixMilli(), now.Add(-max(magicWindow, loginWindow)).UnixMilli()); err != nil && ctx.Err() == nil {
			s.log.Warn("shared_sweep_failed", zap.Error(err))
		}
	}
}
