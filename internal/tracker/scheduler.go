package tracker

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/thunderjr/price-tracker-bot/internal/store"
)

// Notifier receives alerts a scheduled scan produced. It reports a delivery
// that did not happen, because by then the alerts are already recorded as
// fired and the cooldown will not offer them again.
type Notifier interface {
	Deliver(ctx context.Context, alerts []Alert) error
}

// watchTimeout bounds one watch's scan. Generous, because Mercado Livre goes
// through a real browser and Amazon backs off for up to a minute when it is
// rate limiting.
const defaultWatchTimeout = 6 * time.Minute

// Scheduler rescans every active watch on an interval.
type Scheduler struct {
	tracker      *Tracker
	store        *store.Store
	notifier     Notifier
	interval     time.Duration
	watchTimeout time.Duration
	log          *slog.Logger
}

// NewScheduler wires a periodic scan loop.
func NewScheduler(t *Tracker, st *store.Store, n Notifier, interval time.Duration, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{
		tracker:      t,
		store:        st,
		notifier:     n,
		interval:     interval,
		watchTimeout: defaultWatchTimeout,
		log:          log,
	}
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

		// Re-read the watch. A sweep spaces its watches minutes apart, so the
		// row taken at the start is stale by now: the user may have paused it
		// or changed its filters, and a scan of their own may have moved
		// NotifiedBestCents, which decides what counts as a move worth
		// mentioning.
		fresh, err := s.store.Watch(ctx, w.ID)
		if err != nil {
			s.log.Error("scheduler: reload watch failed", "watch", w.ID, "err", err)
			continue
		}
		if !fresh.Active {
			continue
		}

		// Bound each watch on its own. Without this a single wedged scan holds
		// the sweep open indefinitely, and every later watch in the list goes
		// unscanned for the rest of the interval.
		scanCtx, cancel := context.WithTimeout(ctx, s.watchTimeout)
		res, err := s.tracker.Scan(scanCtx, *fresh)
		cancel()

		if errors.Is(err, ErrScanInProgress) {
			s.log.Info("scheduled scan skipped, watch already being scanned",
				"watch", fresh.ID, "query", fresh.Query)
			continue
		}
		if err != nil {
			s.log.Error("scheduled scan failed", "watch", fresh.ID, "query", fresh.Query, "err", err)
			continue
		}
		s.log.Info("scheduled scan done", "watch", fresh.ID, "query", fresh.Query,
			"found", res.Found, "tracked", res.Tracked, "filtered", res.Filtered,
			"pruned", res.Pruned, "alerts", len(res.Alerts), "skipped", len(res.Skipped))

		if len(res.Alerts) > 0 && s.notifier != nil {
			if err := s.notifier.Deliver(ctx, res.Alerts); err != nil {
				s.log.Error("scheduled alerts not delivered", "watch", fresh.ID, "err", err)
			}
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
