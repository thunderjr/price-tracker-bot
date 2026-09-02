package tracker

import (
	"strings"
	"testing"
	"time"

	"github.com/thunderjr/price-tracker-bot/internal/source"
	"github.com/thunderjr/price-tracker-bot/internal/store"
)

var now = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// history builds daily observations ending yesterday, oldest first.
func history(prices ...int64) []store.PricePoint {
	out := make([]store.PricePoint, len(prices))
	for i, p := range prices {
		out[i] = store.PricePoint{
			PriceCents: p,
			SeenAt:     now.AddDate(0, 0, -(len(prices) - i)),
		}
	}
	return out
}

func kinds(cs []Candidate) []Kind {
	out := make([]Kind, len(cs))
	for i, c := range cs {
		out[i] = c.Kind
	}
	return out
}

func has(cs []Candidate, k Kind) bool {
	for _, c := range cs {
		if c.Kind == k {
			return true
		}
	}
	return false
}

func get(t *testing.T, cs []Candidate, k Kind) Candidate {
	t.Helper()
	for _, c := range cs {
		if c.Kind == k {
			return c
		}
	}
	t.Fatalf("no %s candidate in %v", k, kinds(cs))
	return Candidate{}
}

func TestEvaluateNoHistory(t *testing.T) {
	r := DefaultRules(0.10, 0.01)
	got := r.Evaluate(source.Offer{PriceCents: 100000}, nil, 0, now)
	if len(got) != 0 {
		t.Errorf("first sighting produced alerts: %v", kinds(got))
	}
}

// A brand new product that is already under the target should still alert:
// the target is the user's own statement, not a claim about history.
func TestEvaluateTargetNeedsNoHistory(t *testing.T) {
	r := DefaultRules(0.10, 0.01)
	got := r.Evaluate(source.Offer{PriceCents: 340000}, nil, 350000, now)
	if !has(got, KindTarget) {
		t.Fatalf("target not fired: %v", kinds(got))
	}

	above := r.Evaluate(source.Offer{PriceCents: 360000}, nil, 350000, now)
	if has(above, KindTarget) {
		t.Error("target fired above the target price")
	}
}

func TestEvaluateNewLow(t *testing.T) {
	r := DefaultRules(0.10, 0.01)
	h := history(500000, 480000, 460000)

	got := r.Evaluate(source.Offer{PriceCents: 450000}, h, 0, now)
	c := get(t, got, KindNewLow)
	if c.RefCents != 460000 {
		t.Errorf("RefCents = %d, want the previous low 460000", c.RefCents)
	}

	if has(r.Evaluate(source.Offer{PriceCents: 470000}, h, 0, now), KindNewLow) {
		t.Error("new_low fired above the previous low")
	}
	// Matching the previous low counts: it is still the best price on record.
	if !has(r.Evaluate(source.Offer{PriceCents: 460000}, h, 0, now), KindNewLow) {
		t.Error("new_low did not fire on a tie with the previous low")
	}
}

func TestEvaluateDropVsMedian(t *testing.T) {
	r := DefaultRules(0.10, 0.01)
	h := history(500000, 500000, 500000, 500000, 500000)

	// 11% below the median clears the 10% threshold.
	if !has(r.Evaluate(source.Offer{PriceCents: 445000}, h, 0, now), KindDropVsMedian) {
		t.Error("drop_vs_median did not fire at 11% off")
	}
	// 5% off does not.
	if has(r.Evaluate(source.Offer{PriceCents: 475000}, h, 0, now), KindDropVsMedian) {
		t.Error("drop_vs_median fired at only 5% off")
	}
}

func TestEvaluateDropVsMedianNeedsEnoughHistory(t *testing.T) {
	r := DefaultRules(0.10, 0.01)
	h := history(500000, 500000, 500000, 500000) // one short of MinPoints

	got := r.Evaluate(source.Offer{PriceCents: 300000}, h, 0, now)
	if has(got, KindDropVsMedian) {
		t.Errorf("fired on %d points, want at least %d", len(h), r.MinPoints)
	}
	if !has(got, KindNewLow) {
		t.Error("new_low should still fire regardless of MinPoints")
	}
}

// One price spike must not raise the baseline and manufacture a "drop" out of
// the product's perfectly normal price. That is why the rule uses a median.
func TestEvaluateMedianResistsOutlier(t *testing.T) {
	r := DefaultRules(0.10, 0.01)
	h := history(500000, 500000, 900000, 500000, 500000)

	med, ok := r.median(h, now)
	if !ok || med != 500000 {
		t.Fatalf("median = %d (ok=%v), want 500000", med, ok)
	}

	// The mean is 580000, so a mean-based rule would call the unchanged
	// 500000 price a 14% drop.
	const mean = (500000*4 + 900000) / 5
	if float64(500000) >= float64(mean)*(1-r.DropThreshold) {
		t.Fatalf("test setup: 500000 should look like a drop against mean %d", mean)
	}
	if has(r.Evaluate(source.Offer{PriceCents: 500000}, h, 0, now), KindDropVsMedian) {
		t.Error("a single spike in the history manufactured a fake drop")
	}
}

// Points older than the window are not part of the median.
func TestEvaluateMedianWindowExcludesOldPoints(t *testing.T) {
	r := DefaultRules(0.10, 0.01)
	h := []store.PricePoint{
		{PriceCents: 900000, SeenAt: now.AddDate(0, 0, -90)},
		{PriceCents: 900000, SeenAt: now.AddDate(0, 0, -80)},
		{PriceCents: 900000, SeenAt: now.AddDate(0, 0, -70)},
		{PriceCents: 500000, SeenAt: now.AddDate(0, 0, -5)},
		{PriceCents: 500000, SeenAt: now.AddDate(0, 0, -4)},
	}
	if _, ok := r.median(h, now); ok {
		t.Error("median used points outside the 30 day window to reach MinPoints")
	}
}

func TestEvaluateSiteFlag(t *testing.T) {
	r := DefaultRules(0.10, 0.01)
	plain := []store.PricePoint{{PriceCents: 500000, SeenAt: now.AddDate(0, 0, -1)}}

	offer := source.Offer{PriceCents: 490000, ListPriceCents: 600000, SiteFlags: []string{"18% OFF"}}
	c := get(t, r.Evaluate(offer, plain, 0, now), KindSiteFlag)
	if c.Confident {
		t.Error("site_flag must be marked low confidence")
	}
	if c.RefCents != 600000 {
		t.Errorf("RefCents = %d, want the listing reference 600000", c.RefCents)
	}
}

// A promotion that was already advertised last time is not news.
func TestEvaluateSiteFlagOnlyOnTransition(t *testing.T) {
	r := DefaultRules(0.10, 0.01)
	promoted := []store.PricePoint{{
		PriceCents:     500000,
		ListPriceCents: 600000,
		SiteFlags:      []string{"18% OFF"},
		SeenAt:         now.AddDate(0, 0, -1),
	}}

	offer := source.Offer{PriceCents: 500000, ListPriceCents: 600000, SiteFlags: []string{"18% OFF"}}
	if has(r.Evaluate(offer, promoted, 0, now), KindSiteFlag) {
		t.Error("site_flag re-fired on an unchanged promotion")
	}
}

func TestShouldFire(t *testing.T) {
	r := DefaultRules(0.10, 0.01)
	c := Candidate{Kind: KindNewLow, PriceCents: 400000}

	if !r.ShouldFire(c, time.Time{}, 0, now) {
		t.Error("first ever alert was suppressed")
	}
	if !r.ShouldFire(c, now.Add(-25*time.Hour), 400000, now) {
		t.Error("alert past the cooldown was suppressed")
	}
	if r.ShouldFire(c, now.Add(-time.Hour), 400000, now) {
		t.Error("duplicate alert inside the cooldown got through")
	}

	// Inside the cooldown, a further 5% drop is worth interrupting for.
	deeper := Candidate{Kind: KindNewLow, PriceCents: 380000}
	if !r.ShouldFire(deeper, now.Add(-time.Hour), 400000, now) {
		t.Error("a 5% deeper drop was suppressed by the cooldown")
	}
	barely := Candidate{Kind: KindNewLow, PriceCents: 396000} // 1% lower
	if r.ShouldFire(barely, now.Add(-time.Hour), 400000, now) {
		t.Error("a 1% drift broke the cooldown")
	}
}

func TestEvaluateIgnoresZeroPrice(t *testing.T) {
	r := DefaultRules(0.10, 0.01)
	if got := r.Evaluate(source.Offer{PriceCents: 0}, history(500000), 100000, now); len(got) != 0 {
		t.Errorf("alerts from a zero price: %v", kinds(got))
	}
}

func TestDescribe(t *testing.T) {
	for _, c := range []Candidate{
		{Kind: KindTarget, PriceCents: 340000, RefCents: 350000},
		{Kind: KindNewLow, PriceCents: 340000, RefCents: 350000},
		{Kind: KindDropVsMedian, PriceCents: 340000, RefCents: 400000},
		{Kind: KindSiteFlag, PriceCents: 340000, RefCents: 400000},
		{Kind: KindSiteFlag, PriceCents: 340000},
	} {
		if got := Describe(c); got == "" || got == string(c.Kind) {
			t.Errorf("Describe(%s) = %q", c.Kind, got)
		}
	}
}

// The commentary a tracker exists for: say something when the cheapest offer
// moves, in either direction, without saying it on every scan.
func TestBestMove(t *testing.T) {
	r := DefaultRules(0.10, 0.01)

	for _, tc := range []struct {
		name              string
		previous, current int64
		want              Kind
		fires             bool
	}{
		{"drop past the threshold", 450000, 440000, KindBestDrop, true},
		{"rise past the threshold", 440000, 450000, KindBestRise, true},
		{"exactly at the threshold", 100000, 99000, KindBestDrop, true},
		{"below the threshold", 450000, 449900, "", false},
		{"unchanged", 450000, 450000, "", false},
		{"first ever scan", 0, 450000, "", false},
		{"everything delisted", 450000, 0, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, ok := r.BestMove(tc.previous, tc.current)
			if ok != tc.fires || kind != tc.want {
				t.Errorf("BestMove(%d, %d) = (%q, %v), want (%q, %v)",
					tc.previous, tc.current, kind, ok, tc.want, tc.fires)
			}
		})
	}
}

func TestDescribeBestMoves(t *testing.T) {
	drop := Describe(Candidate{Kind: KindBestDrop, PriceCents: 404700, RefCents: 459900})
	if !strings.Contains(drop, "caiu") || !strings.Contains(drop, "4.599") {
		t.Errorf("best-drop description = %q", drop)
	}
	rise := Describe(Candidate{Kind: KindBestRise, PriceCents: 459900, RefCents: 404700})
	if !strings.Contains(rise, "subiu") || !strings.Contains(rise, "4.047") {
		t.Errorf("best-rise description = %q", rise)
	}
}
