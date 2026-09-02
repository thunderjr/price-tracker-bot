//go:build integration

// Live end-to-end test: real marketplaces, real browser, real database.
// Run it inside the container, where Chromium has its Xvfb display:
//
//	make integration
//
// It is excluded from `go test ./...` because it depends on two websites being
// up and unblocked, which is exactly what `make smoke` is for.
package tracker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thunderjr/price-tracker-bot/internal/browser"
	"github.com/thunderjr/price-tracker-bot/internal/source"
	"github.com/thunderjr/price-tracker-bot/internal/source/amazon"
	"github.com/thunderjr/price-tracker-bot/internal/source/kabum"
	"github.com/thunderjr/price-tracker-bot/internal/source/meli"
	"github.com/thunderjr/price-tracker-bot/internal/store"
)

const liveQuery = "ps5"

func TestLiveScanEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	chrome := browser.New(browser.Options{
		ExecPath:   os.Getenv("CHROME_PATH"),
		ProfileDir: filepath.Join(t.TempDir(), "chrome"),
		Logger:     slog.Default(),
	})
	defer chrome.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "live.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	// Two trackers over one store. The first phase exercises all three
	// sources; the repeat-scan phases use Mercado Livre alone, because four
	// Amazon searches inside a minute is a request pattern Amazon answers
	// with a captcha -- and rightly so. The real bot scans every 3h.
	both := New(st, DefaultRules(0.10, 0.01), slog.Default(),
		meli.New(chrome), amazon.New(nil), kabum.New(nil))
	both.pace = 3 * time.Second

	tr := New(st, DefaultRules(0.10, 0.01), slog.Default(), meli.New(chrome))
	tr.pace = time.Second

	w, err := st.CreateWatch(ctx, 1, store.WatchSpec{Query: liveQuery, TargetCents: 0})
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}

	// 1. A real scan reaches both marketplaces and records what it found.
	res, err := both.Scan(ctx, *w)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	// Throttling is the environment saying "later" -- usually because this
	// very suite has been hammering the site -- so it is reported and skipped.
	// Anything else is a real failure of the source.
	for name, err := range res.Skipped {
		if errors.Is(err, source.ErrThrottled) {
			t.Logf("source %s is rate limiting us; skipping its assertions: %v", name, err)
			continue
		}
		t.Errorf("source %s did not answer: %v", name, err)
	}
	if res.Skipped[meli.Name] != nil {
		t.Fatalf("Mercado Livre did not answer, which is what this suite exists to catch: %v",
			res.Skipped[meli.Name])
	}
	if res.Found < 20 {
		t.Fatalf("got %d offers, want >= 20", res.Found)
	}
	if len(res.Alerts) != 0 {
		t.Errorf("first ever scan produced alerts: %v", res.Alerts)
	}

	offers, err := st.WatchOffers(ctx, w.ID, store.ModeCash, 30*24*time.Hour, 500)
	if err != nil {
		t.Fatalf("WatchOffers: %v", err)
	}
	if len(offers) != res.Found {
		t.Errorf("stored %d offers, scanned %d", len(offers), res.Found)
	}

	var bySource = map[string]int{}
	for _, o := range offers {
		bySource[o.Source]++
		if o.PriceCents <= 0 {
			t.Errorf("%s/%s stored with no price", o.Source, o.ExternalID)
		}
		if o.Title == "" || o.URL == "" {
			t.Errorf("%s/%s stored without a title or url", o.Source, o.ExternalID)
		}
	}
	for _, want := range []string{meli.Name, amazon.Name, kabum.Name} {
		if bySource[want] == 0 && res.Skipped[want] == nil {
			t.Errorf("no offers stored from %s", want)
		}
	}
	t.Logf("scanned %q: %v", liveQuery, bySource)

	// 2. Rescanning immediately must not double up the history.
	if _, err := tr.Scan(ctx, *w); err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	// The later phases rescan Mercado Livre only, so track one of its products.
	var subject store.WatchOffer
	for _, o := range offers {
		if o.Source == meli.Name {
			subject = o
			break
		}
	}
	if subject.ID == 0 {
		t.Fatal("no Mercado Livre product to follow")
	}

	hist, err := st.PriceHistory(ctx, subject.ID, time.Time{})
	if err != nil {
		t.Fatalf("PriceHistory: %v", err)
	}
	if len(hist) > 1 {
		t.Errorf("an unchanged price recorded %d points across two scans", len(hist))
	}

	// 3. Backdate an inflated history for every Mercado Livre product, then
	//    confirm the next real scan reads today's genuine prices as drops.
	//
	//    Inflating all of them rather than one chosen product is what makes
	//    this deterministic: Mercado Livre reorders its results between
	//    searches, so any single product may simply not come back.
	now := time.Now()
	inflated := map[int64]bool{}
	for _, o := range offers {
		if o.Source != meli.Name {
			continue
		}
		for i := range 6 {
			p := store.PricePoint{
				ProductID:  o.ID,
				PriceCents: o.PriceCents * 2,
				SeenAt:     now.AddDate(0, 0, -(10 - i)),
			}
			if err := st.AddPricePoint(ctx, p); err != nil {
				t.Fatalf("AddPricePoint: %v", err)
			}
		}
		inflated[o.ID] = true
	}
	if len(inflated) == 0 {
		t.Fatal("no Mercado Livre products to inflate")
	}

	res, err = tr.Scan(ctx, *w)
	if err != nil {
		t.Fatalf("third Scan: %v", err)
	}

	var dropped []int64
	for _, a := range res.Alerts {
		if a.Kind == KindDropVsMedian && inflated[a.Product.ID] {
			dropped = append(dropped, a.Product.ID)
		}
	}
	if len(dropped) == 0 {
		t.Fatalf("no drop_vs_median across %d products at half their recorded median", len(inflated))
	}
	t.Logf("drop_vs_median fired for %d of %d products", len(dropped), len(inflated))

	// 4. And the cooldown stops the very next scan repeating any of them.
	res, err = tr.Scan(ctx, *w)
	if err != nil {
		t.Fatalf("fourth Scan: %v", err)
	}
	for _, a := range res.Alerts {
		if a.Kind == KindDropVsMedian && inflated[a.Product.ID] {
			t.Errorf("drop_vs_median repeated inside the cooldown for product %d", a.Product.ID)
		}
	}
}

var (
	_ source.Source = (*amazon.Source)(nil)
	_ source.Source = (*kabum.Source)(nil)
)
