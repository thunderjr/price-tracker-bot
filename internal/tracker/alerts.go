// Package tracker turns scan results into price history and alerts.
package tracker

import (
	"fmt"
	"slices"
	"time"

	"github.com/thunderjr/price-tracker-bot/internal/source"
	"github.com/thunderjr/price-tracker-bot/internal/store"
)

// Kind names an alert rule. The value is persisted, so don't rename these.
type Kind string

const (
	// KindDropVsMedian fires when a price falls well below its own recent
	// median. The median is the honest baseline: it ignores the listing's
	// advertised "de/por" price, which is routinely inflated.
	KindDropVsMedian Kind = "drop_vs_median"

	// KindNewLow fires on the cheapest price we have ever recorded.
	KindNewLow Kind = "new_low"

	// KindSiteFlag fires when the marketplace itself starts advertising a
	// promotion. Low confidence by design -- see Candidate.Confident.
	KindSiteFlag Kind = "site_flag"

	// KindTarget fires when the price crosses the target the user set.
	KindTarget Kind = "target"

	// KindBestDrop and KindBestRise report that the cheapest offer a watch
	// can find has moved. Unlike the rules above they say nothing about
	// whether the price is good -- they are the running commentary a tracker
	// is for, so you hear about movement without having to ask.
	KindBestDrop Kind = "best_drop"
	KindBestRise Kind = "best_rise"
)

// Candidate is an alert the rules think is worth sending.
type Candidate struct {
	Kind       Kind
	PriceCents int64
	// RefCents is what the price is being compared against: the median, the
	// previous low, the listing's reference price, or the user's target.
	RefCents int64
	// Confident is false for signals the marketplace supplied about itself.
	Confident bool
}

// Discount is how far below the reference the price sits, in percent.
func (c Candidate) Discount() int {
	if c.RefCents <= c.PriceCents || c.PriceCents <= 0 {
		return 0
	}
	return int((c.RefCents - c.PriceCents) * 100 / c.RefCents)
}

// Rules configures alerting.
type Rules struct {
	// DropThreshold is how far below the median a price must fall, 0.10 = 10%.
	DropThreshold float64
	// MedianWindow is how far back the median looks.
	MedianWindow time.Duration
	// MinPoints is the history needed before the median is trustworthy.
	MinPoints int
	// MinHistoryAge is how long a product must have been tracked before any
	// claim about its history is worth making. Without it, the second scan of
	// a newly tracked product declares half the catalogue a record low.
	MinHistoryAge time.Duration
	// MinPointsNewLow is the history needed before "lowest ever" means
	// anything.
	MinPointsNewLow int
	// Cooldown suppresses a repeat of the same alert for the same product.
	Cooldown time.Duration
	// ReFireDrop lets a still-falling price break the cooldown, 0.05 = 5%.
	ReFireDrop float64
	// BestMoveThreshold is how far a watch's best price must move to be worth
	// a message, 0.01 = 1%. Below it, ordinary cent-level wobble would
	// notify on every scan.
	BestMoveThreshold float64
}

// DefaultRules returns sensible settings; the thresholds usually come from
// config.
func DefaultRules(dropThreshold, bestMoveThreshold float64) Rules {
	return Rules{
		DropThreshold:     dropThreshold,
		MedianWindow:      30 * 24 * time.Hour,
		MinPoints:         5,
		MinHistoryAge:     12 * time.Hour,
		MinPointsNewLow:   3,
		Cooldown:          24 * time.Hour,
		ReFireDrop:        0.05,
		BestMoveThreshold: bestMoveThreshold,
	}
}

// Evaluate decides which alerts a fresh observation deserves.
//
// history holds the product's earlier observations, oldest first, and must not
// include offer itself. target is the watch's target price, or 0 for none.
func (r Rules) Evaluate(offer source.Offer, history []store.PricePoint, target int64, now time.Time) []Candidate {
	price := offer.PriceCents
	if price <= 0 {
		return nil
	}

	var out []Candidate

	if target > 0 && price <= target {
		out = append(out, Candidate{Kind: KindTarget, PriceCents: price, RefCents: target, Confident: true})
	}

	// Everything below compares against our own record, so a product we have
	// barely met cannot produce one. Twenty minutes of history is not grounds
	// for telling someone they are looking at a record low.
	if len(history) == 0 || historyAge(history, now) < r.MinHistoryAge {
		return out
	}

	if len(history) >= r.MinPointsNewLow {
		if low := minPrice(history); price <= low {
			out = append(out, Candidate{Kind: KindNewLow, PriceCents: price, RefCents: low, Confident: true})
		}
	}

	if med, ok := r.median(history, now); ok {
		if float64(price) < float64(med)*(1-r.DropThreshold) {
			out = append(out, Candidate{Kind: KindDropVsMedian, PriceCents: price, RefCents: med, Confident: true})
		}
	}

	// The marketplace started advertising a promotion that was not there on the
	// previous look.
	prev := history[len(history)-1]
	if promoting(offer.ListPriceCents, price, offer.SiteFlags) &&
		!promoting(prev.ListPriceCents, prev.PriceCents, prev.SiteFlags) {
		out = append(out, Candidate{
			Kind:       KindSiteFlag,
			PriceCents: price,
			RefCents:   offer.ListPriceCents,
			Confident:  false,
		})
	}

	return out
}

// BestMove decides whether a watch's new best price is worth reporting,
// given the best price the user was last told about.
//
// Both directions are reported: a rise matters as much as a drop when you are
// deciding whether to buy now or wait.
func (r Rules) BestMove(previous, current int64) (Kind, bool) {
	// Nothing to compare against on a watch's first scan, and nothing to say
	// when every listing has gone.
	if previous <= 0 || current <= 0 {
		return "", false
	}

	diff := current - previous
	if diff < 0 {
		diff = -diff
	}
	if float64(diff) < float64(previous)*r.BestMoveThreshold {
		return "", false
	}

	if current < previous {
		return KindBestDrop, true
	}
	return KindBestRise, true
}

// ShouldFire applies the cooldown. lastFired is the zero time when this kind
// has never fired for this product.
func (r Rules) ShouldFire(c Candidate, lastFired time.Time, lastPrice int64, now time.Time) bool {
	if lastFired.IsZero() || now.Sub(lastFired) >= r.Cooldown {
		return true
	}
	// Still inside the cooldown: only a meaningfully deeper drop gets through,
	// otherwise every tick of a slow slide becomes its own notification.
	return lastPrice > 0 && float64(c.PriceCents) <= float64(lastPrice)*(1-r.ReFireDrop)
}

// median returns the median price over the window, and whether there was
// enough history to trust it.
func (r Rules) median(history []store.PricePoint, now time.Time) (int64, bool) {
	cutoff := now.Add(-r.MedianWindow)

	prices := make([]int64, 0, len(history))
	for _, p := range history {
		if !p.SeenAt.Before(cutoff) {
			prices = append(prices, p.PriceCents)
		}
	}
	if len(prices) < r.MinPoints {
		return 0, false
	}

	slices.Sort(prices)
	mid := len(prices) / 2
	if len(prices)%2 == 1 {
		return prices[mid], true
	}
	return (prices[mid-1] + prices[mid]) / 2, true
}

// historyAge is how long the product has been observed.
func historyAge(history []store.PricePoint, now time.Time) time.Duration {
	oldest := history[0].SeenAt
	for _, p := range history[1:] {
		if p.SeenAt.Before(oldest) {
			oldest = p.SeenAt
		}
	}
	if oldest.IsZero() {
		return 0
	}
	return now.Sub(oldest)
}

func minPrice(history []store.PricePoint) int64 {
	low := history[0].PriceCents
	for _, p := range history[1:] {
		if p.PriceCents < low {
			low = p.PriceCents
		}
	}
	return low
}

// promoting reports whether a listing is advertising a promotion of its own.
func promoting(listCents, priceCents int64, flags []string) bool {
	return listCents > priceCents || len(flags) > 0
}

// Describe renders a one-line reason for a candidate, in pt-BR.
func Describe(c Candidate) string {
	switch c.Kind {
	case KindTarget:
		return fmt.Sprintf("atingiu seu alvo de %s", source.FormatBRL(c.RefCents))
	case KindNewLow:
		return fmt.Sprintf("menor preço já registrado (antes %s)", source.FormatBRL(c.RefCents))
	case KindDropVsMedian:
		return fmt.Sprintf("%d%% abaixo da mediana de 30 dias (%s)", c.Discount(), source.FormatBRL(c.RefCents))
	case KindBestDrop:
		return fmt.Sprintf("melhor preço caiu %d%%: era %s",
			c.Discount(), source.FormatBRL(c.RefCents))
	case KindBestRise:
		return fmt.Sprintf("melhor preço subiu: era %s", source.FormatBRL(c.RefCents))
	case KindSiteFlag:
		if c.RefCents > c.PriceCents {
			return fmt.Sprintf("anúncio marcou promoção, preço de referência %s", source.FormatBRL(c.RefCents))
		}
		return "anúncio marcou promoção"
	default:
		return string(c.Kind)
	}
}
