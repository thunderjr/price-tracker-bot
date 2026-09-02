// Package source defines the common shape of a price source and the helpers
// every source needs to normalize Brazilian marketplace data.
package source

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ErrBlocked means a source returned something other than results. The caller
// should skip that source for the current scan rather than fail the whole
// thing.
//
// ErrThrottled narrows it to "we asked too often, it will pass". The
// distinction matters because the two demand opposite responses: throttling
// means back off and try later, while a bare ErrBlocked on a page we cannot
// recognize usually means the site was redesigned and the parser needs work.
var (
	ErrBlocked   = errors.New("source: blocked by anti-bot protection")
	ErrThrottled = fmt.Errorf("source: rate limited: %w", ErrBlocked)
)

// Source searches one marketplace for a free-text query.
type Source interface {
	Name() string
	Search(ctx context.Context, query string) ([]Offer, error)
}

// Offer is one normalized listing. Money is always integer cents (BRL);
// floats never touch a price in this codebase.
type Offer struct {
	Source         string   `json:"source"`
	ExternalID     string   `json:"external_id"`
	Title          string   `json:"title"`
	URL            string   `json:"url"`
	ImageURL       string   `json:"image_url,omitempty"`
	Seller         string   `json:"seller,omitempty"`
	PriceCents     int64    `json:"price_cents"`
	ListPriceCents int64    `json:"list_price_cents,omitempty"`
	Rating         float64  `json:"rating,omitempty"`
	SiteFlags      []string `json:"site_flags,omitempty"`
	// Installments is the listing's financing offer, kept so a reference price
	// can be checked against it.
	Installments InstallmentPlan `json:"installments,omitzero"`
	// International marks a cross-border listing. The price shown for these
	// excludes Brazilian import tax, so it is not comparable with a domestic
	// price and would win every "cheapest" comparison dishonestly.
	International bool `json:"international,omitempty"`
}

// Discount reports the percentage off the listing's own reference price.
// It returns 0 when there is no credible reference price.
func (o Offer) Discount() int {
	if o.ListPriceCents <= o.PriceCents || o.PriceCents <= 0 {
		return 0
	}
	return int((o.ListPriceCents - o.PriceCents) * 100 / o.ListPriceCents)
}

var nonPrice = regexp.MustCompile(`[^\d,\.]`)

// dotDecimal matches the shapes where "." cannot be a thousands separator:
// pt-BR groups thousands in threes, so "4.184" is four thousand reais, while
// "3499.90" is somebody typing a target with a decimal point. Read as
// thousands that becomes R$ 349.990,00 and the target then fires on
// everything.
var dotDecimal = regexp.MustCompile(`^\d+\.\d{1,2}$`)

// ParseBRL turns Brazilian price text into cents. It accepts the forms both
// marketplaces emit: "R$ 4.184,06", "4.184", "4184,06", "R$4,99", plus the
// "3499.90" a person types when a target price is asked for.
// It returns 0 when there is no parsable number.
func ParseBRL(s string) int64 {
	s = nonPrice.ReplaceAllString(s, "")
	if s == "" {
		return 0
	}

	// "." is a thousands separator in pt-BR and "," the decimal comma, except
	// where the digits after the "." cannot be a thousands group.
	if dotDecimal.MatchString(s) {
		s = strings.Replace(s, ".", ",", 1)
	}

	whole, frac, hasFrac := strings.Cut(s, ",")
	whole = strings.ReplaceAll(whole, ".", "")
	if whole == "" {
		whole = "0"
	}

	cents, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0
	}
	cents *= 100
	if !hasFrac {
		return cents
	}

	frac = strings.ReplaceAll(frac, ".", "")
	switch {
	case len(frac) == 1:
		frac += "0"
	case len(frac) > 2:
		frac = frac[:2]
	case frac == "":
		frac = "00"
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return cents
	}
	return cents + f
}

// FormatBRL renders cents as "R$ 4.184,06".
func FormatBRL(cents int64) string {
	neg := cents < 0
	if neg {
		cents = -cents
	}
	whole := strconv.FormatInt(cents/100, 10)

	var b strings.Builder
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(r)
	}

	sign := ""
	if neg {
		sign = "-"
	}
	return sign + "R$ " + b.String() + "," + pad2(cents%100)
}

func pad2(n int64) string {
	if n < 10 {
		return "0" + strconv.FormatInt(n, 10)
	}
	return strconv.FormatInt(n, 10)
}

// Installment plan tolerance, 1.5% as a ratio of integers. A displayed
// installment total is rounded (ten instalments of R$ 449,90 are advertised as
// "R$ 4.499"), so the arithmetic never matches to the cent -- and a price is
// integer cents, which no comparison here is allowed to turn into a float.
const (
	installmentToleranceNum = 15
	installmentToleranceDen = 1000
)

// Interest says what a listing claims about the cost of financing. Both
// marketplaces state it in words next to the plan, and the difference is real
// money: "em até 12x de R$ 11,08 com juros" on a R$ 118,72 item comes to
// R$ 132,96.
type Interest string

const (
	InterestUnknown Interest = ""
	InterestFree    Interest = "sem juros"
	InterestCharged Interest = "com juros"
)

// InstallmentPlan is a listing's financing offer, parsed from free text.
//
// A listing publishes at most one plan -- the longest it will grant, which is
// what "em até 12x" means -- so this is a single plan rather than a list.
type InstallmentPlan struct {
	Count    int      // number of instalments, 0 when unstated
	Each     int64    // cents per instalment
	Total    int64    // cents for the whole plan, when stated outright
	Interest Interest // what the listing says about interest, if anything
	// OtherMeansCents is what Mercado Livre quotes for paying another way
	// ("ou R$ 5.499 em outros meios"). Higher than the headline price, and a
	// frequent source of fake "was" prices.
	OtherMeansCents int64
}

// TotalCents is what the plan costs in total.
func (p InstallmentPlan) TotalCents() int64 {
	if p.Total > 0 {
		return p.Total
	}
	if p.Count > 1 && p.Each > 0 {
		return int64(p.Count) * p.Each
	}
	return 0
}

// IsInstallmentTotal reports whether cents is this listing's financing total
// rather than a price it once sold for.
//
// Both marketplaces show a struck-through figure beside the cash price, and on
// most listings that figure is simply the same item paid in instalments:
// Amazon strikes "R$ 4.499,00" next to "R$ 4.184,06 à vista no Pix ou NuPay
// ou em até 10x de R$ 449,90", and ten times 449,90 is exactly 4.499,00.
// Reading it as a former price invents a discount that never expires, on
// nearly every listing, and buries the real ones.
func (p InstallmentPlan) IsInstallmentTotal(cents int64) bool {
	return within(cents, p.TotalCents()) || within(cents, p.OtherMeansCents)
}

// ResolveInterest fills in the wording a listing left out, using its own
// arithmetic. Mercado Livre prints "sem juros" only when the plan really is
// free and writes nothing at all otherwise, so instalments adding up to more
// than the cash price are charging interest whatever the card says. A stated
// wording is never overridden, and the tolerance keeps cent-rounding
// ("10x R$ 459,99" against a R$ 4.599,00 price) from being read as interest.
//
// The inference only ever moves a plan from unknown to "com juros", so the
// worst it can do is overstate what a plan costs -- never hide it.
func (p InstallmentPlan) ResolveInterest(priceCents int64) InstallmentPlan {
	if p.Count <= 1 || p.Interest != InterestUnknown || priceCents <= 0 {
		return p
	}
	if total := p.TotalCents(); total > priceCents && !within(total, priceCents) {
		p.Interest = InterestCharged
	}
	return p
}

// ResolveInterestAgainst fills in the interest wording from a reference price
// the listing publishes as a number, for a source that never states the
// wording in words at all.
//
// Kabum is that source: "juros" appears nowhere on a search page, but every
// card carries both figures -- a card price and a lower cash price for Pix --
// and its plans total the card price to the cent. So the plan itself is free
// and the gap down to the cash price is a Pix discount, not financing cost.
//
// Comparing against the cash price, which is what ResolveInterest does for the
// sources that do print the wording, would label every Kabum plan "com juros"
// and contradict Amazon on the very same offer: R$ 4.184 cash, 10x R$ 449,90,
// and Amazon says "sem juros" outright.
//
// A stated wording is never overridden.
func (p InstallmentPlan) ResolveInterestAgainst(referenceCents int64) InstallmentPlan {
	if p.Count <= 1 || p.Interest != InterestUnknown || referenceCents <= 0 {
		return p
	}

	total := p.TotalCents()
	switch {
	case total <= 0:
		return p
	case within(total, referenceCents):
		p.Interest = InterestFree
	case total > referenceCents:
		p.Interest = InterestCharged
	}
	return p
}

// within reports whether cents is the same figure as total, allowing for the
// rounding both sites apply when they display a total.
func within(cents, total int64) bool {
	if total <= 0 || cents <= 0 {
		return false
	}

	diff := cents - total
	if diff < 0 {
		diff = -diff
	}
	return diff*installmentToleranceDen <= total*installmentToleranceNum
}

// Installment clauses, matched explicitly rather than by scanning for numbers.
// A price recipe contains several figures -- the cash price, the struck total,
// the per-instalment amount -- and picking them out positionally reads the
// wrong one.
var (
	// "em até 10x de R$ 449,90", optionally prefixed by the plan total as
	// Mercado Livre writes it: "ou R$ 4.599,90 em 10x R$ 459,99".
	installmentRe = regexp.MustCompile(
		`(?i)(?:ou\s+R\$\s*([\d.]+(?:,\d{1,2})?)\s+)?em\s+(?:at[eé]\s+)?(\d{1,2})\s*x\s*(?:de\s+)?R\$\s*([\d.]+(?:,\d{1,2})?)`)
	// Mercado Livre's bare form: "12x R$ 459,99 sem juros", with or without
	// the "de" it sometimes writes ("12x de R$ 459,99").
	bareInstallmentRe = regexp.MustCompile(
		`(?i)(?:^|\s)(\d{1,2})\s*x\s*(?:de\s+)?R\$\s*([\d.]+(?:,\d{1,2})?)`)
	// "ou R$ 5.499 em outros meios" states a total outright.
	otherMeansRe = regexp.MustCompile(
		`(?i)ou\s+R\$\s*([\d.]+(?:,\d{1,2})?)\s*(?:em\s+)?outros\s+meios`)
	// The juros wording trails the plan it describes.
	interestRe = regexp.MustCompile(`(?i)\b(sem|com)\s+juros\b`)
)

// ParseInstallments reads a marketplace's financing line. It handles the forms
// both sites emit:
//
//	"em até 10x de R$ 449,90 sem juros"
//	"12x R$ 459,99 sem juros"
//	"ou R$ 4.599,90 em 10x R$ 459,99 sem juros"
//	"ou R$ 5.499 em outros meios"
func ParseInstallments(text string) InstallmentPlan {
	var plan InstallmentPlan
	if text == "" {
		return plan
	}
	text = strings.Join(strings.Fields(text), " ")

	// Mercado Livre's other-payment figure sits in the same line as the plan
	// on some cards and alone on others, so read it first and keep going.
	if m := otherMeansRe.FindStringSubmatch(text); m != nil {
		plan.OtherMeansCents = ParseBRL(m[1])
	}

	if m := installmentRe.FindStringSubmatch(text); m != nil {
		if n, err := strconv.Atoi(m[2]); err == nil && n > 1 {
			plan.Count = n
		}
		plan.Each = ParseBRL(m[3])
		plan.Total = ParseBRL(m[1]) // empty group parses to 0
	} else if m := bareInstallmentRe.FindStringSubmatch(text); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 1 {
			plan.Count = n
		}
		plan.Each = ParseBRL(m[2])
	}

	if plan.Count > 0 {
		plan.Interest = parseInterest(text)
	}
	return plan
}

// parseInterest reads the juros wording. Absent wording stays unknown rather
// than being assumed interest-free: presenting a financed total as if it cost
// nothing extra is the more expensive mistake.
func parseInterest(text string) Interest {
	m := interestRe.FindStringSubmatch(text)
	if m == nil {
		return InterestUnknown
	}
	if strings.EqualFold(m[1], "sem") {
		return InterestFree
	}
	return InterestCharged
}
