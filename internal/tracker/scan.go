package tracker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
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

// maxMisses is how many consecutive scans may fail to see a tracked listing
// before the watch lets it go. The tail of a search page shifts between
// requests, so a single absence proves nothing.
const maxMisses = 3

// ErrScanInProgress reports that this watch is already being scanned.
//
// The scheduler, the manager's buttons and the CLI all reach Scan, and two of
// them working one watch at once would record its price twice, fire its alerts
// twice, and send two messages about a single watch.
//
// The guard is per process, which is every path inside the running bot. A
// separate `ptb scan` against the same database is not fenced off from it.
var ErrScanInProgress = errors.New("tracker: watch is already being scanned")

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
	// Pruned is how many already-tracked listings were dropped: either the
	// watch's filters no longer accept them, or they have gone missing from a
	// source that answered.
	Pruned int
	// BestCents is the cheapest price the watch can currently find.
	BestCents int64
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

	// scanning is the set of watches being scanned right now. The lock lives
	// here rather than in the callers because every entry point has to share
	// one, and the front end's own guard could not see the scheduler.
	scanningMu sync.Mutex
	scanning   map[int64]bool
}

// New builds a Tracker over the given sources.
func New(st *store.Store, rules Rules, log *slog.Logger, sources ...source.Source) *Tracker {
	if log == nil {
		log = slog.Default()
	}
	return &Tracker{
		store:    st,
		sources:  sources,
		rules:    rules,
		log:      log,
		pace:     5 * time.Second,
		scanning: map[int64]bool{},
	}
}

// Scan searches every source for the watch's query, records prices and returns
// the alerts worth sending.
//
// A source that is blocked or failing does not fail the scan: partial results
// from the other source are far more useful than nothing.
//
// One watch is scanned by one caller at a time; a second gets
// ErrScanInProgress rather than a duplicate of everything.
func (t *Tracker) Scan(ctx context.Context, w store.Watch) (*Result, error) {
	if !t.beginScan(w.ID) {
		return nil, ErrScanInProgress
	}
	defer t.endScan(w.ID)
	return t.scan(ctx, w)
}

// ScanInProgress reports whether this watch is being scanned, so a caller can
// say so up front instead of starting work it would only lose the race for.
func (t *Tracker) ScanInProgress(watchID int64) bool {
	t.scanningMu.Lock()
	defer t.scanningMu.Unlock()
	return t.scanning[watchID]
}

func (t *Tracker) beginScan(watchID int64) bool {
	t.scanningMu.Lock()
	defer t.scanningMu.Unlock()
	if t.scanning[watchID] {
		return false
	}
	t.scanning[watchID] = true
	return true
}

func (t *Tracker) endScan(watchID int64) {
	t.scanningMu.Lock()
	defer t.scanningMu.Unlock()
	delete(t.scanning, watchID)
}

func (t *Tracker) scan(ctx context.Context, w store.Watch) (*Result, error) {
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

	// seen collects the keys this scan kept, dropped every key its filtering
	// threw away. They are reconciled after the loop rather than inside it,
	// because the same key can appear on both sides.
	seen := map[string]bool{}
	var dropped []string

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
		for _, o := range kept {
			seen[o.Source+"/"+o.ExternalID] = true
		}
		for _, d := range report.Drops {
			dropped = append(dropped, d.Source+"/"+d.ExternalID)
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

	// A duplicate result card is dropped while the copy that was kept carries
	// the same key, so a key on both sides was not rejected at all: unlinking
	// it would undo the product this scan has just recorded.
	rejected := make(map[string]bool, len(dropped))
	for _, key := range dropped {
		if !seen[key] {
			rejected[key] = true
		}
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

	gone, err := t.dropDelisted(ctx, w, seen, res.Skipped)
	if err != nil {
		return res, err
	}
	res.Pruned += gone

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

	// WatchOffers returns the cheapest first, so this is the watch's best
	// price right now.
	if len(tracked) > 0 {
		res.BestCents = tracked[0].PriceCents
		move, err := t.reportBestMove(ctx, w, tracked[0], now)
		if err != nil {
			return res, err
		}
		if move != nil {
			res.Alerts = append(res.Alerts, *move)
		}
	}

	if err := t.store.MarkWatchScanned(ctx, w.ID, now); err != nil {
		return res, err
	}
	return res, nil
}

// reportBestMove notifies when the watch's cheapest offer has moved since the
// last time the user heard about it, and remembers the new figure.
//
// The comparison is against what was last *reported*, not against the previous
// scan, so a slow slide is announced once it adds up rather than never, and a
// price that keeps flapping between two values does not notify twice.
func (t *Tracker) reportBestMove(ctx context.Context, w store.Watch, best store.WatchOffer, now time.Time) (*Alert, error) {
	previous := w.NotifiedBestCents

	kind, ok := t.rules.BestMove(previous, best.PriceCents)
	if !ok {
		// Still worth remembering the first best price, so the next move has
		// something to be measured against.
		if previous == 0 && best.PriceCents > 0 {
			return nil, t.store.SetNotifiedBest(ctx, w.ID, best.PriceCents)
		}
		return nil, nil
	}

	if err := t.store.SetNotifiedBest(ctx, w.ID, best.PriceCents); err != nil {
		return nil, err
	}
	if err := t.store.RecordAlert(ctx, w.ID, best.ID, string(kind), best.PriceCents, now); err != nil {
		return nil, err
	}

	return &Alert{
		Candidate: Candidate{
			Kind:       kind,
			PriceCents: best.PriceCents,
			RefCents:   previous,
			Confident:  true,
		},
		Watch:   w,
		Product: best.Product,
	}, nil
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

// dropDelisted unlinks listings that have gone missing from a source that
// answered. seen holds the keys this scan kept.
//
// Absence is only evidence when the source that would have carried the listing
// actually replied, and even then not on the first scan: a search page's tail
// shifts between requests. Left linked, a delisted offer's last price goes on
// driving the watch's best price, its best-move alerts, its digest and its
// stats -- months after the listing stopped existing.
func (t *Tracker) dropDelisted(ctx context.Context, w store.Watch, seen map[string]bool, skipped map[string]error) (int, error) {
	answered := make(map[string]bool, len(t.sources))
	for _, src := range t.sources {
		if _, down := skipped[src.Name()]; !down {
			answered[src.Name()] = true
		}
	}

	tracked, err := t.store.WatchOffers(ctx, w.ID, t.rules.MedianWindow, maxTrackedPerWatch)
	if err != nil {
		return 0, err
	}

	n := 0
	for _, o := range tracked {
		if !answered[o.Source] || seen[o.Source+"/"+o.ExternalID] {
			continue
		}
		misses, err := t.store.MissWatchProduct(ctx, w.ID, o.ID)
		if errors.Is(err, store.ErrNotFound) {
			// The link went away underneath us -- the watch was deleted
			// mid-scan. There is nothing left to unlink.
			continue
		}
		if err != nil {
			return n, err
		}
		if misses < maxMisses {
			continue
		}
		if err := t.store.UnlinkWatchProduct(ctx, w.ID, o.ID); err != nil {
			return n, err
		}
		t.log.Debug("untracked delisted listing", "watch", w.ID, "source", o.Source,
			"id", o.ExternalID, "misses", misses, "title", o.Title)
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
			ProductID:            id,
			PriceCents:           o.PriceCents,
			ListPriceCents:       o.ListPriceCents,
			SiteFlags:            o.SiteFlags,
			InstallmentCount:     o.Installments.Count,
			InstallmentEachCents: o.Installments.Each,
			SeenAt:               now,
		}
		if err := t.store.AddPricePoint(ctx, point); err != nil {
			return nil, err
		}
	}

	var out []Alert
	for _, c := range t.rules.Evaluate(o, history, w.TargetCents, now) {
		lastAt, lastPrice, err := t.store.LastAlert(ctx, w.ID, id, string(c.Kind))
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
