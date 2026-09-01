package tracker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/thunderjr/price-tracker-bot/internal/relevance"
	"github.com/thunderjr/price-tracker-bot/internal/source"
	"github.com/thunderjr/price-tracker-bot/internal/store"
)

// heartbeat is how long an unchanged price goes unrecorded before we write a
// point anyway, so "still R$ 4.199" stays distinguishable from "not checked".
const heartbeat = 24 * time.Hour

// maxTrackedPerWatch bounds how many listings one watch follows.
const maxTrackedPerWatch = 2000

// Alert is a fired alert, ready to be delivered.
type Alert struct {
	Candidate
	Watch   store.Watch
	Product store.Product
}

// Result summarizes one watch scan.
type Result struct {
	// Found is how many listings this scan matched, after filtering. It is
	// not the size of the watch: a source that fails contributes nothing
	// here, while everything it had contributed before stays tracked.
	Found int
	// Tracked is how many listings the watch follows once the scan is done.
	Tracked int
	// Filtered is how many the relevance filter removed as accessories,
	// look-alikes, duplicates or out-of-range.
	Filtered int
	Alerts   []Alert
	Skipped  map[string]error // source name -> why it contributed nothing
	// Pruned is how many already-tracked listings were dropped because they
	// no longer pass the watch's filters.
	Pruned int
	// Suggestion, when set, is a price floor worth offering the user because
	// the results fell into two distant groups.
	Suggestion int64
}

// Tracker scans watches and records what it finds.
type Tracker struct {
	store   *store.Store
	sources []source.Source
	rules   Rules
	log     *slog.Logger

	// pace is the pause between source queries. Scraping politely is what
	// keeps these sources working at all.
	pace time.Duration
}

// New builds a Tracker over the given sources.
func New(st *store.Store, rules Rules, log *slog.Logger, sources ...source.Source) *Tracker {
	if log == nil {
		log = slog.Default()
	}
	return &Tracker{store: st, sources: sources, rules: rules, log: log, pace: 5 * time.Second}
}

// Scan searches every source for the watch's query, records prices and returns
// the alerts worth sending.
//
// A source that is blocked or failing does not fail the scan: partial results
// from the other source are far more useful than nothing.
func (t *Tracker) Scan(ctx context.Context, w store.Watch) (*Result, error) {
	now := time.Now()
	res := &Result{Skipped: map[string]error{}}
	var allKept []source.Offer

	// Filters change, and a watch created before a filter existed keeps
	// following whatever it picked up back then. Re-check what is already
	// tracked against the current rules first, or the accessories the user
	// asked to be rid of stay in every report forever.
	pruned, err := t.prune(ctx, w, nil)
	if err != nil {
		return res, err
	}
	res.Pruned = pruned

	rejected := map[string]bool{}

	for i, src := range t.sources {
		if i > 0 {
			if err := t.wait(ctx); err != nil {
				return res, err
			}
		}

		offers, err := src.Search(ctx, w.Query)
		if err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			level := slog.LevelError
			if errors.Is(err, source.ErrBlocked) {
				level = slog.LevelWarn
			}
			t.log.Log(ctx, level, "source failed", "source", src.Name(), "query", w.Query, "err", err)
			res.Skipped[src.Name()] = err
			continue
		}

		kept, report := relevance.Filter(w.Query, offers, filterOptions(w))
		if n := report.Dropped(); n > 0 {
			t.log.Debug("filtered listings", "source", src.Name(), "query", w.Query,
				"kept", len(kept), "dropped", n)
		}
		res.Filtered += report.Dropped()
		allKept = append(allKept, kept...)
		for _, d := range report.Drops {
			rejected[d.Source+"/"+d.ExternalID] = true
		}

		res.Found += len(kept)
		for _, o := range kept {
			alerts, err := t.record(ctx, w, o, now)
			if err != nil {
				return res, err
			}
			res.Alerts = append(res.Alerts, alerts...)
		}
	}

	if len(res.Skipped) == len(t.sources) {
		return res, fmt.Errorf("tracker: every source failed for %q", w.Query)
	}
	// Prune again now that this scan has spoken. The first pass could only
	// judge listings by what the database already knew about them, and a
	// listing the marketplace has just flagged -- as an import, say -- is
	// filtered out before it is ever written back, so its stored row never
	// learns. Unlinking whatever this scan rejected closes that gap.
	late, err := t.prune(ctx, w, rejected)
	if err != nil {
		return res, err
	}
	res.Pruned += late

	// Only worth suggesting while the user has not already set a floor.
	if w.MinCents == 0 {
		if floor, ok := relevance.SuggestFloor(allKept); ok {
			res.Suggestion = floor
		}
	}

	tracked, err := t.store.WatchOffers(ctx, w.ID, t.rules.MedianWindow, maxTrackedPerWatch)
	if err != nil {
		return res, err
	}
	res.Tracked = len(tracked)

	if err := t.store.MarkWatchScanned(ctx, w.ID, now); err != nil {
		return res, err
	}
	return res, nil
}

// filterOptions maps a watch's settings onto the relevance filter.
func filterOptions(w store.Watch) relevance.Options {
	return relevance.Options{
		Exclude:            w.Exclude,
		Require:            w.Require,
		MinCents:           w.MinCents,
		MaxCents:           w.MaxCents,
		AllowInternational: w.AllowInternational,
	}
}

// prune unlinks tracked listings the watch should no longer follow: those its
// current filters reject on stored attributes, plus any key in rejected, which
// carries what this scan's own filtering threw away.
func (t *Tracker) prune(ctx context.Context, w store.Watch, rejected map[string]bool) (int, error) {
	tracked, err := t.store.WatchOffers(ctx, w.ID, t.rules.MedianWindow, maxTrackedPerWatch)
	if err != nil {
		return 0, err
	}
	if len(tracked) == 0 {
		return 0, nil
	}

	offers := make([]source.Offer, len(tracked))
	for i, o := range tracked {
		offers[i] = source.Offer{
			Source:        o.Source,
			ExternalID:    o.ExternalID,
			Title:         o.Title,
			PriceCents:    o.PriceCents,
			International: o.International,
		}
	}

	keep := make(map[string]bool, len(offers))
	kept, _ := relevance.Filter(w.Query, offers, filterOptions(w))
	for _, o := range kept {
		keep[o.Source+"/"+o.ExternalID] = true
	}

	n := 0
	for _, o := range tracked {
		key := o.Source + "/" + o.ExternalID
		if keep[key] && !rejected[key] {
			continue
		}
		if err := t.store.UnlinkWatchProduct(ctx, w.ID, o.ID); err != nil {
			return n, err
		}
		t.log.Debug("untracked listing", "watch", w.ID, "source", o.Source,
			"id", o.ExternalID, "title", o.Title)
		n++
	}
	return n, nil
}

// record persists one offer and returns any alerts it triggers.
func (t *Tracker) record(ctx context.Context, w store.Watch, o source.Offer, now time.Time) ([]Alert, error) {
	product := store.Product{
		Source:        o.Source,
		ExternalID:    o.ExternalID,
		Title:         o.Title,
		URL:           o.URL,
		ImageURL:      o.ImageURL,
		Seller:        o.Seller,
		International: o.International,
	}

	id, err := t.store.UpsertProduct(ctx, product)
	if err != nil {
		return nil, err
	}
	product.ID = id

	if err := t.store.LinkWatchProduct(ctx, w.ID, id, now); err != nil {
		return nil, err
	}

	// Alerts compare against everything recorded before this observation, so
	// read the history before writing the new point.
	history, err := t.store.PriceHistory(ctx, id, time.Time{})
	if err != nil {
		return nil, err
	}

	if shouldRecord(history, o, now) {
		point := store.PricePoint{
			ProductID:      id,
			PriceCents:     o.PriceCents,
			ListPriceCents: o.ListPriceCents,
			SiteFlags:      o.SiteFlags,
			SeenAt:         now,
		}
		if err := t.store.AddPricePoint(ctx, point); err != nil {
			return nil, err
		}
	}

	var out []Alert
	for _, c := range t.rules.Evaluate(o, history, w.TargetCents, now) {
		lastAt, lastPrice, err := t.store.LastAlert(ctx, id, string(c.Kind))
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		if !t.rules.ShouldFire(c, lastAt, lastPrice, now) {
			continue
		}
		if err := t.store.RecordAlert(ctx, w.ID, id, string(c.Kind), c.PriceCents, now); err != nil {
			return nil, err
		}
		out = append(out, Alert{Candidate: c, Watch: w, Product: product})
	}
	return out, nil
}

// shouldRecord keeps the history readable: a point per change, plus a daily
// heartbeat so a long flat stretch is still visible as "we kept looking".
func shouldRecord(history []store.PricePoint, o source.Offer, now time.Time) bool {
	if len(history) == 0 {
		return true
	}
	last := history[len(history)-1]
	return last.PriceCents != o.PriceCents ||
		last.ListPriceCents != o.ListPriceCents ||
		now.Sub(last.SeenAt) >= heartbeat
}

// wait pauses between source queries, jittered so our traffic does not arrive
// on a metronome.
func (t *Tracker) wait(ctx context.Context) error {
	if t.pace <= 0 {
		return nil
	}
	d := t.pace + rand.N(t.pace)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
