package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestWatchLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	w, err := s.CreateWatch(ctx, 42, WatchSpec{Query: "  ps5  ", TargetCents: 350000})
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	if w.Query != "ps5" {
		t.Errorf("Query = %q, want normalized %q", w.Query, "ps5")
	}
	if w.TargetCents != 350000 || !w.Active {
		t.Errorf("target = %d, active = %v", w.TargetCents, w.Active)
	}

	// Re-tracking the same query updates in place instead of duplicating.
	again, err := s.CreateWatch(ctx, 42, WatchSpec{Query: "ps5", TargetCents: 0})
	if err != nil {
		t.Fatalf("CreateWatch again: %v", err)
	}
	if again.ID != w.ID {
		t.Errorf("second CreateWatch made a new row: %d vs %d", again.ID, w.ID)
	}
	if again.TargetCents != 0 {
		t.Errorf("target = %d, want cleared", again.TargetCents)
	}

	if err := s.SetWatchActive(ctx, w.ID, false); err != nil {
		t.Fatalf("SetWatchActive: %v", err)
	}
	active, err := s.ActiveWatches(ctx)
	if err != nil {
		t.Fatalf("ActiveWatches: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("paused watch still active: %+v", active)
	}

	all, err := s.Watches(ctx, 42)
	if err != nil {
		t.Fatalf("Watches: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("Watches returned %d, want 1 (paused ones must stay listed)", len(all))
	}

	if err := s.DeleteWatch(ctx, w.ID); err != nil {
		t.Fatalf("DeleteWatch: %v", err)
	}
	if _, err := s.Watch(ctx, w.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Watch after delete: %v, want ErrNotFound", err)
	}
}

func TestWatchesAreScopedPerChat(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if _, err := s.CreateWatch(ctx, 1, WatchSpec{Query: "ps5", TargetCents: 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateWatch(ctx, 2, WatchSpec{Query: "ps5", TargetCents: 0}); err != nil {
		t.Fatal(err)
	}

	for _, chat := range []int64{1, 2} {
		got, err := s.Watches(ctx, chat)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Errorf("chat %d sees %d watches, want 1", chat, len(got))
		}
	}
}

func TestPriceHistory(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	pid, err := s.UpsertProduct(ctx, Product{Source: "meli", ExternalID: "MLB1", Title: "PS5", URL: "u"})
	if err != nil {
		t.Fatalf("UpsertProduct: %v", err)
	}

	base := time.Now().Add(-40 * 24 * time.Hour)
	for i, cents := range []int64{500000, 480000, 460000} {
		p := PricePoint{
			ProductID:  pid,
			PriceCents: cents,
			SeenAt:     base.Add(time.Duration(i) * 15 * 24 * time.Hour),
			SiteFlags:  []string{"oferta"},
		}
		if err := s.AddPricePoint(ctx, p); err != nil {
			t.Fatalf("AddPricePoint: %v", err)
		}
	}

	all, err := s.PriceHistory(ctx, pid, time.Time{})
	if err != nil {
		t.Fatalf("PriceHistory: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("full history has %d points, want 3", len(all))
	}
	if all[0].PriceCents != 500000 || all[2].PriceCents != 460000 {
		t.Errorf("history not in chronological order: %v", all)
	}
	if len(all[0].SiteFlags) != 1 || all[0].SiteFlags[0] != "oferta" {
		t.Errorf("SiteFlags round trip = %v", all[0].SiteFlags)
	}

	recent, err := s.PriceHistory(ctx, pid, time.Now().Add(-20*24*time.Hour))
	if err != nil {
		t.Fatalf("PriceHistory windowed: %v", err)
	}
	if len(recent) != 1 {
		t.Errorf("20d window has %d points, want 1", len(recent))
	}
}

// Points written inside the same second must still come back in order: the
// stored timestamp is compared as a string, and a layout that trims trailing
// zeros from the fraction sorts "12:00:00Z" after "12:00:00.5Z".
func TestPriceHistoryOrdersWithinASecond(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	pid, err := s.UpsertProduct(ctx, Product{Source: "meli", ExternalID: "MLB1", Title: "PS5", URL: "u"})
	if err != nil {
		t.Fatalf("UpsertProduct: %v", err)
	}

	// A whole second, which RFC3339Nano writes without any fraction at all,
	// then half a second later.
	whole := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for i, cents := range []int64{500000, 480000} {
		p := PricePoint{
			ProductID:  pid,
			PriceCents: cents,
			SeenAt:     whole.Add(time.Duration(i) * 500 * time.Millisecond),
		}
		if err := s.AddPricePoint(ctx, p); err != nil {
			t.Fatalf("AddPricePoint: %v", err)
		}
	}

	after, err := s.PriceHistory(ctx, pid, whole.Add(250*time.Millisecond))
	if err != nil {
		t.Fatalf("PriceHistory: %v", err)
	}
	if len(after) != 1 || after[0].PriceCents != 480000 {
		t.Errorf("window from mid-second returned %v, want only the 480000 point", after)
	}
}

func TestUpsertProductIsStableByExternalID(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	first, err := s.UpsertProduct(ctx, Product{Source: "amazon", ExternalID: "B01", Title: "old", URL: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.UpsertProduct(ctx, Product{Source: "amazon", ExternalID: "B01", Title: "new", URL: "u2"})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("upsert made a new row (%d then %d); price history would fork", first, second)
	}

	// Same external id on a different source must not collide.
	other, err := s.UpsertProduct(ctx, Product{Source: "meli", ExternalID: "B01", Title: "x", URL: "u3"})
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Error("products from different sources collapsed into one row")
	}
}

func TestWatchOffersAndStats(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	now := time.Now()

	w, err := s.CreateWatch(ctx, 7, WatchSpec{Query: "ps5", TargetCents: 0})
	if err != nil {
		t.Fatal(err)
	}

	for i, spec := range []struct {
		ext    string
		prices []int64
	}{
		{"MLB1", []int64{500000, 420000}}, // dropped, and holds the window low
		{"MLB2", []int64{410000}},         // cheapest right now
	} {
		pid, err := s.UpsertProduct(ctx, Product{Source: "meli", ExternalID: spec.ext, Title: spec.ext, URL: "u"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.LinkWatchProduct(ctx, w.ID, pid, now); err != nil {
			t.Fatal(err)
		}
		for j, c := range spec.prices {
			at := now.Add(-time.Duration(len(spec.prices)-j) * time.Hour)
			if err := s.AddPricePoint(ctx, PricePoint{ProductID: pid, PriceCents: c, SeenAt: at}); err != nil {
				t.Fatal(err)
			}
		}
		_ = i
	}

	offers, err := s.WatchOffers(ctx, w.ID, 30*24*time.Hour, 10)
	if err != nil {
		t.Fatalf("WatchOffers: %v", err)
	}
	if len(offers) != 2 {
		t.Fatalf("got %d offers, want 2", len(offers))
	}
	if offers[0].ExternalID != "MLB2" {
		t.Errorf("offers not sorted cheapest first: %s then %s", offers[0].ExternalID, offers[1].ExternalID)
	}
	if offers[0].PriceCents != 410000 {
		t.Errorf("latest price = %d, want the most recent point 410000", offers[0].PriceCents)
	}
	if offers[1].LowCents != 420000 {
		t.Errorf("MLB1 window low = %d, want 420000", offers[1].LowCents)
	}

	st, err := s.Stats(ctx, w.ID, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Products != 2 || st.BestCents != 410000 || st.LowCents != 410000 {
		t.Errorf("Stats = %+v", st)
	}
}

func TestAlertDedupeBookkeeping(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	now := time.Now()

	w, err := s.CreateWatch(ctx, 1, WatchSpec{Query: "ps5", TargetCents: 0})
	if err != nil {
		t.Fatal(err)
	}
	pid, err := s.UpsertProduct(ctx, Product{Source: "meli", ExternalID: "MLB1", Title: "x", URL: "u"})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.LastAlert(ctx, w.ID, pid, "new_low"); !errors.Is(err, ErrNotFound) {
		t.Errorf("LastAlert on fresh product = %v, want ErrNotFound", err)
	}

	if err := s.RecordAlert(ctx, w.ID, pid, "new_low", 400000, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAlert(ctx, w.ID, pid, "new_low", 380000, now); err != nil {
		t.Fatal(err)
	}

	at, cents, err := s.LastAlert(ctx, w.ID, pid, "new_low")
	if err != nil {
		t.Fatalf("LastAlert: %v", err)
	}
	if cents != 380000 {
		t.Errorf("LastAlert cents = %d, want the most recent 380000", cents)
	}
	if now.Sub(at) > time.Minute {
		t.Errorf("LastAlert time = %v, want ~now", at)
	}

	// A different kind is tracked independently.
	if _, _, err := s.LastAlert(ctx, w.ID, pid, "drop_vs_median"); !errors.Is(err, ErrNotFound) {
		t.Errorf("kinds are not independent: %v", err)
	}

	// So is another watch following the same listing. Sharing the cooldown
	// silences one of them for a day and the notification is simply lost.
	other, err := s.CreateWatch(ctx, 1, WatchSpec{Query: "playstation 5"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LastAlert(ctx, other.ID, pid, "new_low"); !errors.Is(err, ErrNotFound) {
		t.Errorf("another watch's alert satisfied this watch's cooldown: %v", err)
	}
}

// Deleting a watch must not take the price history with it.
func TestDeleteWatchKeepsHistory(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	now := time.Now()

	w, err := s.CreateWatch(ctx, 1, WatchSpec{Query: "ps5", TargetCents: 0})
	if err != nil {
		t.Fatal(err)
	}
	pid, err := s.UpsertProduct(ctx, Product{Source: "meli", ExternalID: "MLB1", Title: "x", URL: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LinkWatchProduct(ctx, w.ID, pid, now); err != nil {
		t.Fatal(err)
	}
	if err := s.AddPricePoint(ctx, PricePoint{ProductID: pid, PriceCents: 100, SeenAt: now}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteWatch(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	hist, err := s.PriceHistory(ctx, pid, time.Time{})
	if err != nil {
		t.Fatalf("PriceHistory: %v", err)
	}
	if len(hist) != 1 {
		t.Errorf("history lost on watch delete: %d points", len(hist))
	}
}
