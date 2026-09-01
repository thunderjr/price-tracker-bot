package tracker

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/thunderjr/price-tracker-bot/internal/store"
)

// Notifier receives alerts a scheduled scan produced.
type Notifier interface {
	Deliver(ctx context.Context, alerts []Alert)
}

// Scheduler rescans every active watch on an interval.
type Scheduler struct {
	tracker  *Tracker
	store    *store.Store
	notifier Notifier
	interval time.Duration
	log      *slog.Logger
}

// NewScheduler wires a periodic scan loop.
func NewScheduler(t *Tracker, st *store.Store, n Notifier, interval time.Duration, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{tracker: t, store: st, notifier: n, interval: interval, log: log}
}

// Run scans on the interval until ctx is cancelled. It returns only on
// cancellation: a failing scan is logged and retried on the next tick.
func (s *Scheduler) Run(ctx context.Context) {
	s.log.Info("scheduler started", "interval", s.interval)

	// Wait a full interval before the first sweep: the bot has usually just
	// started, and anything freshly tracked was already scanned on creation.
	for {
		if !s.sleep(ctx, s.jitter(s.interval)) {
			s.log.Info("scheduler stopped")
			return
		}
		s.sweep(ctx)
	}
}

func (s *Scheduler) sweep(ctx context.Context) {
	watches, err := s.store.ActiveWatches(ctx)
	if err != nil {
		s.log.Error("scheduler: list active watches failed", "err", err)
		return
	}
	if len(watches) == 0 {
		return
	}
	s.log.Info("scheduled sweep started", "watches", len(watches))

	for i, w := range watches {
		if ctx.Err() != nil {
			return
		}
		// Space the watches out. All of them hitting the same two sites back
		// to back is exactly the shape that gets a scraper blocked.
		if i > 0 && !s.sleep(ctx, s.jitter(10*time.Second)) {
			return
		}

		res, err := s.tracker.Scan(ctx, w)
		if err != nil {
			s.log.Error("scheduled scan failed", "watch", w.ID, "query", w.Query, "err", err)
			continue
		}
		s.log.Info("scheduled scan done", "watch", w.ID, "query", w.Query,
			"found", res.Found, "tracked", res.Tracked, "filtered", res.Filtered,
			"pruned", res.Pruned, "alerts", len(res.Alerts), "skipped", len(res.Skipped))

		if len(res.Alerts) > 0 && s.notifier != nil {
			s.notifier.Deliver(ctx, res.Alerts)
		}
	}
}

// jitter spreads requests around d by +/-20%.
func (s *Scheduler) jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	spread := int64(d) / 5
	return d + time.Duration(rand.Int64N(2*spread+1)-spread)
}

// sleep waits for d, reporting false if ctx was cancelled first.
func (s *Scheduler) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
