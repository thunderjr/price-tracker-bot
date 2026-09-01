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

// ParseBRL turns Brazilian price text into cents. It accepts the forms both
// marketplaces emit: "R$ 4.184,06", "4.184", "4184,06", "R$4,99".
// It returns 0 when there is no parsable number.
func ParseBRL(s string) int64 {
	s = nonPrice.ReplaceAllString(s, "")
	if s == "" {
		return 0
	}

	// "." is always a thousands separator in pt-BR; "," is the decimal comma.
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
