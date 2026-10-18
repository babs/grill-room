package web

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// gaugeEvery is how often SweepShared computes the gauges /metrics reads: as often as the streams
// poll, so a gauge trails the shared state by about one poll, not a scrape interval.
const gaugeEvery = time.Second

// sharedSweepEvery is how often SweepShared drops the expired shared state. A var for tests.
var sharedSweepEvery = time.Minute

// SweepShared deletes the shared state whose lease ended, the rate events past every limiter
// window and the TTS lines the TTS service never took, every minute, and refreshes the gauges,
// until ctx ends. Every pod runs it: the sweep is idempotent.
func (s *Server) SweepShared(ctx context.Context) {
	sweep := time.NewTicker(sharedSweepEvery)
	defer sweep.Stop()
	gauges := time.NewTicker(gaugeEvery)
	defer gauges.Stop()
	s.refreshGauges()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			s.sweepShared(ctx)
		case <-gauges.C:
			s.refreshGauges()
		}
	}
}

func (s *Server) sweepShared(ctx context.Context) {
	// ponytail: one cutoff, the longest limiter window, for every rate scope; per-scope cutoffs if
	// rate_events grows large.
	gone, err := s.bus.Store.SweepExpired(ctx, s.lease().UnixMilli(), s.now().Add(-max(magicWindow, loginWindow)).UnixMilli())
	if err != nil && ctx.Err() == nil {
		s.log.Warn("shared_sweep_failed", zap.Error(err))
	}
	// Their pod died with their streams: nobody else ends their turns.
	for _, d := range gone {
		s.log.Info("daemon_lease_lapsed", zap.String("room", d.RoomID), zap.Int64("daemon", d.ID))
		s.daemonGone(ctx, &d)
	}
	if s.tts == nil {
		return
	}
	n, err := s.bus.Store.SweepTTSLines(ctx, s.now().UnixMilli())
	if err != nil && ctx.Err() == nil {
		s.log.Warn("tts_sweep_failed", zap.Error(err))
	}
	s.tts.noService.Add(n)
}

// refreshGauges computes the gauges from the store.
func (s *Server) refreshGauges() {
	now := s.lease()
	s.gauges.sttWorkers.Store(int64(len(s.stt.live(now))))
	s.gauges.deviceSTT.Store(int64(s.people.deviceCount(now)))
	if s.tts == nil {
		return
	}
	s.gauges.ttsDemand.Store(int64(s.ttsDemand()))
	p, _ := s.ttsPod()
	s.gauges.ttsPodUp.Store(beating(p, now))
}
