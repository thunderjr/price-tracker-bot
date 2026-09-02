package tracker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/thunderjr/price-tracker-bot/internal/source"
	"github.com/thunderjr/price-tracker-bot/internal/store"
)

// fakeSource replays canned results, one batch per Search call.
type fakeSource struct {
	name    string
	batches [][]source.Offer
	err     error
	calls   int
}

func (f *fakeSource) Name() string { return f.name }

func (f *fakeSource) Search(context.Context, string) ([]source.Offer, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if len(f.batches) == 0 {
		return nil, nil
	}
	b := f.batches[0]
	if len(f.batches) > 1 {
		f.batches = f.batches[1:]
	}
	return b, nil
}

func newTracker(t *testing.T, sources ...source.Source) (*Tracker, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tr := New(st, DefaultRules(0.10, 0.01), log, sources...)
	tr.pace = 0 // tests must not sleep
	return tr, st
}

func offer(src, id string, cents int64) source.Offer {
	return source.Offer{Source: src, ExternalID: id, Title: id, URL: "https://x/" + id, PriceCents: cents}
}

func TestScanRecordsOffers(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{name: "meli", batches: [][]source.Offer{{
		offer("meli", "MLB1", 500000),
		offer("meli", "MLB2", 400000),
	}}}
	tr, st := newTracker(t, fake)

	w, err := st.CreateWatch(ctx, 1, store.WatchSpec{Query: "ps5", TargetCents: 0})
	if err != nil {
		t.Fatal(err)
	}

	res, err := tr.Scan(ctx, *w)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if res.Found != 2 {
		t.Errorf("Found = %d, want 2", res.Found)
	}
	if len(res.Alerts) != 0 {
		t.Errorf("first scan alerted: %v", res.Alerts)
	}

	offers, err := st.WatchOffers(ctx, w.ID, 30*24*time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(offers) != 2 {
		t.Fatalf("stored %d offers, want 2", len(offers))
	}

	got, err := st.Watch(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastScanAt.IsZero() {
		t.Error("last_scan_at not updated")
	}
}

// An unchanged price must not append a point on every tick.
func TestScanDeduplicatesUnchangedPrices(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{name: "meli", batches: [][]source.Offer{{offer("meli", "MLB1", 500000)}}}
	tr, st := newTracker(t, fake)

	w, _ := st.CreateWatch(ctx, 1, store.WatchSpec{Query: "ps5", TargetCents: 0})
	for range 3 {
		if _, err := tr.Scan(ctx, *w); err != nil {
			t.Fatal(err)
		}
	}

	pid, err := st.UpsertProduct(ctx, store.Product{Source: "meli", ExternalID: "MLB1", Title: "x", URL: "u"})
	if err != nil {
		t.Fatal(err)
	}
	hist, err := st.PriceHistory(ctx, pid, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 {
		t.Errorf("recorded %d points for an unchanged price, want 1", len(hist))
	}
}

func TestScanAlertsOnDrop(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{name: "meli", batches: [][]source.Offer{
		{offer("meli", "MLB1", 400000)},
	}}
	tr, st := newTracker(t, fake)

	w, _ := st.CreateWatch(ctx, 1, store.WatchSpec{Query: "ps5"})

	// Give the product a real history first. A "lowest ever" claim needs one:
	// see TestNoAlertsWithoutRealHistory.
	pid, err := st.UpsertProduct(ctx, store.Product{Source: "meli", ExternalID: "MLB1", Title: "MLB1", URL: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.LinkWatchProduct(ctx, w.ID, pid, time.Now()); err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		at := time.Now().Add(-time.Duration(4-i) * 24 * time.Hour)
		if err := st.AddPricePoint(ctx, store.PricePoint{ProductID: pid, PriceCents: 500000, SeenAt: at}); err != nil {
			t.Fatal(err)
		}
	}

	res, err := tr.Scan(ctx, *w)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Alerts) == 0 || res.Alerts[0].Kind != KindNewLow {
		t.Fatalf("alerts = %v, want a new_low", res.Alerts)
	}
	if res.Alerts[0].Product.ExternalID != "MLB1" || res.Alerts[0].Watch.ID != w.ID {
		t.Errorf("alert not attributed correctly: %+v", res.Alerts[0])
	}

	// The cooldown must stop the very next scan repeating the same kind. A
	// different kind firing once is not a repeat: the price fell far enough
	// below the median to be its own finding.
	fake.batches = [][]source.Offer{{offer("meli", "MLB1", 399000)}}
	res, err = tr.Scan(ctx, *w)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range res.Alerts {
		if a.Kind == KindNewLow {
			t.Errorf("new_low repeated inside the cooldown: %v", a)
		}
	}
}

// The bug that spammed a real chat with 56 messages: a watch's second scan,
// twenty minutes after its first, declared most of the catalogue a record low.
// Nothing that compares against history may fire until there is history.
func TestNoAlertsWithoutRealHistory(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{name: "meli", batches: [][]source.Offer{
		{offer("meli", "MLB1", 500000), offer("meli", "MLB2", 500000)},
		{offer("meli", "MLB1", 499000), offer("meli", "MLB2", 499000)},
	}}
	tr, st := newTracker(t, fake)

	w, _ := st.CreateWatch(ctx, 1, store.WatchSpec{Query: "ps5"})
	if _, err := tr.Scan(ctx, *w); err != nil {
		t.Fatal(err)
	}

	res, err := tr.Scan(ctx, *w)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Alerts) != 0 {
		t.Errorf("a rescan minutes later produced %d alerts: %v", len(res.Alerts), res.Alerts)
	}
}

// One dead source must not cost us the other source's results.
func TestScanSurvivesOneBlockedSource(t *testing.T) {
	ctx := context.Background()
	blocked := &fakeSource{name: "meli", err: source.ErrBlocked}
	ok := &fakeSource{name: "amazon", batches: [][]source.Offer{{offer("amazon", "B01", 100000)}}}
	tr, st := newTracker(t, blocked, ok)

	w, _ := st.CreateWatch(ctx, 1, store.WatchSpec{Query: "ps5", TargetCents: 0})
	res, err := tr.Scan(ctx, *w)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if res.Found != 1 {
		t.Errorf("Found = %d, want the surviving source's 1", res.Found)
	}
	if !errors.Is(res.Skipped["meli"], source.ErrBlocked) {
		t.Errorf("Skipped[meli] = %v, want ErrBlocked", res.Skipped["meli"])
	}
}

func TestScanFailsWhenEverySourceFails(t *testing.T) {
	ctx := context.Background()
	a := &fakeSource{name: "meli", err: source.ErrBlocked}
	b := &fakeSource{name: "amazon", err: errors.New("boom")}
	tr, st := newTracker(t, a, b)

	w, _ := st.CreateWatch(ctx, 1, store.WatchSpec{Query: "ps5", TargetCents: 0})
	if _, err := tr.Scan(ctx, *w); err == nil {
		t.Fatal("Scan succeeded with no working source")
	}

	got, err := st.Watch(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastScanAt.IsZero() {
		t.Error("a fully failed scan was recorded as a successful one")
	}
}

func TestScanTargetAlert(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{name: "meli", batches: [][]source.Offer{{offer("meli", "MLB1", 340000)}}}
	tr, st := newTracker(t, fake)

	w, _ := st.CreateWatch(ctx, 1, store.WatchSpec{Query: "ps5", TargetCents: 350000})
	res, err := tr.Scan(ctx, *w)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Alerts) != 1 || res.Alerts[0].Kind != KindTarget {
		t.Fatalf("alerts = %v, want one target alert on the first scan", res.Alerts)
	}
}

func TestShouldRecordHeartbeat(t *testing.T) {
	n := time.Now()
	hist := []store.PricePoint{{PriceCents: 100, SeenAt: n.Add(-25 * time.Hour)}}
	if !shouldRecord(hist, source.Offer{PriceCents: 100}, n) {
		t.Error("no heartbeat point after 25h of an unchanged price")
	}

	fresh := []store.PricePoint{{PriceCents: 100, SeenAt: n.Add(-time.Hour)}}
	if shouldRecord(fresh, source.Offer{PriceCents: 100}, n) {
		t.Error("recorded a redundant point an hour after the last one")
	}
	if !shouldRecord(fresh, source.Offer{PriceCents: 99}, n) {
		t.Error("a price change was not recorded")
	}
	// A promotion appearing at the same price is still a change worth keeping.
	if !shouldRecord(fresh, source.Offer{PriceCents: 100, ListPriceCents: 200}, n) {
		t.Error("a new reference price was not recorded")
	}
}

// A watch created before a filter existed keeps following what it picked up
// back then. Scanning must re-check the existing set, or the accessories the
// user asked to be rid of stay in every report forever.
func TestScanPrunesNowFilteredListings(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{name: "meli", batches: [][]source.Offer{{
		{Source: "meli", ExternalID: "MLB1", Title: "Console PlayStation 5 Slim", URL: "u", PriceCents: 450000},
		{Source: "meli", ExternalID: "MLB2", Title: "Suporte Vertical para PS5 Slim", URL: "u", PriceCents: 5000},
	}}}
	tr, st := newTracker(t, fake)

	// Link both by hand, as a scan from before the filter existed would have.
	w, _ := st.CreateWatch(ctx, 1, store.WatchSpec{Query: "ps5 slim"})
	now := time.Now()
	for _, spec := range []struct {
		ext, title string
		cents      int64
	}{
		{"MLB1", "Console PlayStation 5 Slim", 450000},
		{"MLB2", "Suporte Vertical para PS5 Slim", 5000},
	} {
		pid, err := st.UpsertProduct(ctx, store.Product{
			Source: "meli", ExternalID: spec.ext, Title: spec.title, URL: "u",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.LinkWatchProduct(ctx, w.ID, pid, now); err != nil {
			t.Fatal(err)
		}
		if err := st.AddPricePoint(ctx, store.PricePoint{
			ProductID: pid, PriceCents: spec.cents, SeenAt: now.Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}

	before, err := st.WatchOffers(ctx, w.ID, 30*24*time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 2 {
		t.Fatalf("setup tracked %d listings, want 2", len(before))
	}

	res, err := tr.Scan(ctx, *w)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if res.Pruned != 1 {
		t.Errorf("Pruned = %d, want 1", res.Pruned)
	}

	after, err := st.WatchOffers(ctx, w.ID, 30*24*time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].ExternalID != "MLB1" {
		t.Fatalf("after prune tracking %d listings %v, want only the console", len(after), after)
	}
}

// Pruning must follow the watch's own bounds too, so setting a min price
// cleans up what the watch already collected.
func TestScanPrunesByPriceBound(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{name: "meli", batches: [][]source.Offer{{
		offer("meli", "MLB1", 450000),
	}}}
	tr, st := newTracker(t, fake)

	w, _ := st.CreateWatch(ctx, 1, store.WatchSpec{Query: "ps5"})
	if _, err := tr.Scan(ctx, *w); err != nil {
		t.Fatal(err)
	}

	cheap := source.Offer{Source: "meli", ExternalID: "MLB9", Title: "Jogo qualquer", URL: "u", PriceCents: 19900}
	pid, err := st.UpsertProduct(ctx, store.Product{
		Source: cheap.Source, ExternalID: cheap.ExternalID, Title: cheap.Title, URL: cheap.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := st.LinkWatchProduct(ctx, w.ID, pid, now); err != nil {
		t.Fatal(err)
	}
	if err := st.AddPricePoint(ctx, store.PricePoint{ProductID: pid, PriceCents: cheap.PriceCents, SeenAt: now}); err != nil {
		t.Fatal(err)
	}

	if err := st.SetWatchBounds(ctx, w.ID, 300000, 0); err != nil {
		t.Fatal(err)
	}
	updated, err := st.Watch(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}

	res, err := tr.Scan(ctx, *updated)
	if err != nil {
		t.Fatal(err)
	}
	if res.Pruned != 1 {
		t.Errorf("Pruned = %d, want the cheap listing gone", res.Pruned)
	}
}

// A listing the marketplace has only just flagged -- as an import, say -- is
// filtered out before it is written back, so its stored row never learns and
// the first prune pass, which can only judge stored attributes, keeps it.
// Pruning again with what this scan rejected is what actually removes it.
func TestScanPrunesWhatThisScanRejected(t *testing.T) {
	ctx := context.Background()

	// First scan: the marketplace has not flagged it yet, so it gets tracked.
	plain := source.Offer{
		Source: "meli", ExternalID: "MLB1", Title: "Console PlayStation 5 Slim",
		URL: "u", PriceCents: 369300,
	}
	flagged := plain
	flagged.International = true

	fake := &fakeSource{name: "meli", batches: [][]source.Offer{{plain}, {flagged}}}
	tr, st := newTracker(t, fake)

	w, _ := st.CreateWatch(ctx, 1, store.WatchSpec{Query: "ps5 slim"})
	if _, err := tr.Scan(ctx, *w); err != nil {
		t.Fatal(err)
	}
	before, err := st.WatchOffers(ctx, w.ID, 30*24*time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatalf("first scan tracked %d listings, want 1", len(before))
	}

	// Second scan: now flagged international. It is filtered before upsert,
	// so nothing updates the stored row -- it must still be untracked.
	res, err := tr.Scan(ctx, *w)
	if err != nil {
		t.Fatal(err)
	}
	if res.Pruned != 1 {
		t.Errorf("Pruned = %d, want the newly flagged import removed", res.Pruned)
	}

	after, err := st.WatchOffers(ctx, w.ID, 30*24*time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Errorf("still tracking %d listings after the import was flagged: %v", len(after), after)
	}
}

// Found counts what this scan matched; Tracked is the size of the watch. They
// diverge whenever a source fails -- its listings stay tracked from before --
// and reporting one as the other reads as data loss.
func TestScanFoundIsNotTracked(t *testing.T) {
	ctx := context.Background()
	meli := &fakeSource{name: "meli", batches: [][]source.Offer{
		{offer("meli", "MLB1", 400000), offer("meli", "MLB2", 410000)},
		{offer("meli", "MLB1", 400000), offer("meli", "MLB2", 410000)},
	}}
	amazon := &fakeSource{name: "amazon", batches: [][]source.Offer{
		{offer("amazon", "B01", 420000)},
	}}
	tr, st := newTracker(t, meli, amazon)

	w, _ := st.CreateWatch(ctx, 1, store.WatchSpec{Query: "ps5"})
	res, err := tr.Scan(ctx, *w)
	if err != nil {
		t.Fatal(err)
	}
	if res.Found != 3 || res.Tracked != 3 {
		t.Fatalf("first scan: Found=%d Tracked=%d, want 3 and 3", res.Found, res.Tracked)
	}

	// Amazon goes down. Its listing must stay tracked, and Found must not
	// claim the watch shrank.
	amazon.err = source.ErrBlocked
	res, err = tr.Scan(ctx, *w)
	if err != nil {
		t.Fatal(err)
	}
	if res.Found != 2 {
		t.Errorf("Found = %d, want the 2 that answered", res.Found)
	}
	if res.Tracked != 3 {
		t.Errorf("Tracked = %d, want 3 -- a blocked source must not untrack its listings", res.Tracked)
	}
	if res.Skipped["amazon"] == nil {
		t.Error("blocked source not reported in Skipped")
	}
}

// The gap that meant no notifications ever arrived: scheduled scans only spoke
// up for the four judgement rules, none of which fire on ordinary movement, so
// a watch could shift hundreds of reais in silence.
func TestScanNotifiesOnBestPriceMove(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{name: "meli", batches: [][]source.Offer{
		{offer("meli", "MLB1", 450000)}, // first sighting: nothing to compare
		{offer("meli", "MLB1", 430000)}, // -4.4%: worth saying
		{offer("meli", "MLB1", 429900)}, // -0.02%: not worth saying
		{offer("meli", "MLB1", 460000)}, // +7%: worth saying
	}}
	tr, st := newTracker(t, fake)

	w, _ := st.CreateWatch(ctx, 1, store.WatchSpec{Query: "ps5"})

	reload := func() store.Watch {
		t.Helper()
		got, err := st.Watch(ctx, w.ID)
		if err != nil {
			t.Fatal(err)
		}
		return *got
	}

	res, err := tr.Scan(ctx, reload())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Alerts) != 0 {
		t.Errorf("first scan announced a move: %v", res.Alerts)
	}
	if res.BestCents != 450000 {
		t.Errorf("BestCents = %d, want 450000", res.BestCents)
	}
	if got := reload().NotifiedBestCents; got != 450000 {
		t.Fatalf("first best price not remembered: %d", got)
	}

	res, err = tr.Scan(ctx, reload())
	if err != nil {
		t.Fatal(err)
	}
	drop := findKind(res.Alerts, KindBestDrop)
	if drop == nil {
		t.Fatalf("no best_drop after a 4%% fall: %v", res.Alerts)
	}
	if drop.PriceCents != 430000 || drop.RefCents != 450000 {
		t.Errorf("best_drop = %d from %d, want 430000 from 450000", drop.PriceCents, drop.RefCents)
	}

	res, err = tr.Scan(ctx, reload())
	if err != nil {
		t.Fatal(err)
	}
	if findKind(res.Alerts, KindBestDrop) != nil || findKind(res.Alerts, KindBestRise) != nil {
		t.Errorf("a 0.02%% wobble produced a message: %v", res.Alerts)
	}

	res, err = tr.Scan(ctx, reload())
	if err != nil {
		t.Fatal(err)
	}
	rise := findKind(res.Alerts, KindBestRise)
	if rise == nil {
		t.Fatalf("no best_rise after a 7%% climb: %v", res.Alerts)
	}
	// Measured against what was last reported, not the wobble in between.
	if rise.RefCents != 430000 {
		t.Errorf("best_rise compared against %d, want the last reported 430000", rise.RefCents)
	}
}

// A move is announced once, not on every scan that still sees the new price.
func TestScanAnnouncesEachMoveOnce(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{name: "meli", batches: [][]source.Offer{
		{offer("meli", "MLB1", 450000)},
		{offer("meli", "MLB1", 400000)},
	}}
	tr, st := newTracker(t, fake)

	w, _ := st.CreateWatch(ctx, 1, store.WatchSpec{Query: "ps5"})
	for range 4 {
		got, err := st.Watch(ctx, w.ID)
		if err != nil {
			t.Fatal(err)
		}
		res, err := tr.Scan(ctx, *got)
		if err != nil {
			t.Fatal(err)
		}
		if n := countKind(res.Alerts, KindBestDrop); n > 1 {
			t.Fatalf("%d best_drop alerts in one scan", n)
		}
	}

	// The price settled at 400000 and stayed there; only the move itself
	// should have been reported.
	final, err := st.Watch(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.NotifiedBestCents != 400000 {
		t.Errorf("NotifiedBestCents = %d, want 400000", final.NotifiedBestCents)
	}
}

func findKind(alerts []Alert, k Kind) *Alert {
	for i := range alerts {
		if alerts[i].Kind == k {
			return &alerts[i]
		}
	}
	return nil
}

func countKind(alerts []Alert, k Kind) int {
	n := 0
	for _, a := range alerts {
		if a.Kind == k {
			n++
		}
	}
	return n
}
