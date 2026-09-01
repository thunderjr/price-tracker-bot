// Package meli scrapes Mercado Livre search results.
//
// The official API is not an option: /sites/MLB/search, /products/search and
// /items/{id} all answer 403 to unauthenticated callers, and plain HTTP against
// the site is redirected to an account-verification wall. A headful browser is
// the only route that works -- see the browser package for why headless is not.
package meli

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/thunderjr/price-tracker-bot/internal/browser"
	"github.com/thunderjr/price-tracker-bot/internal/source"
)

// Name is the source identifier stored alongside every product row.
const Name = "meli"

// extractJS reads the search grid into a JSON-friendly array. It runs as one
// round trip so a page repaint cannot interleave with our reads.
//
// Selectors verified against the live desktop grid; poly-* is Mercado Livre's
// current "polycard" search component.
const extractJS = `(() => {
  const text = (el) => (el && el.innerText ? el.innerText.trim() : "");
  const money = (root, sel) => {
    const el = root.querySelector(sel);
    if (!el) return "";
    const whole = text(el.querySelector(".andes-money-amount__fraction"));
    if (!whole) return "";
    const cents = text(el.querySelector(".andes-money-amount__cents"));
    return cents ? whole + "," + cents : whole;
  };

  return Array.from(document.querySelectorAll("ol.ui-search-layout > li")).map((li) => {
    const link = li.querySelector("a.poly-component__title") || li.querySelector("a[href*='/p/MLB'], a[href*='MLB']");
    const img = li.querySelector("img");
    const flags = [];

    const discount = text(li.querySelector(".andes-money-amount__discount"));
    if (discount) flags.push(discount);
    for (const badge of li.querySelectorAll(".poly-component__highlight, .poly-component__ad-promotions")) {
      const t = text(badge);
      if (t) flags.push(t);
    }
    if (li.querySelector(".poly-component__shipping")) flags.push("frete grátis");

    // Cross-border listings carry a CBT badge naming the origin ("China",
    // "EUA"). Its price excludes Brazilian import tax.
    const cbt = li.querySelector(".poly-component__cbt");
    if (cbt) flags.push("internacional " + text(cbt));

    return {
      title: text(li.querySelector(".poly-component__title")) || text(link),
      url: link ? link.href : "",
      image_url: img ? img.src : "",
      seller: text(li.querySelector(".poly-component__seller")),
      price: money(li, ".poly-price__current") || money(li, ".poly-component__price"),
      list_price: money(li, "s.andes-money-amount--previous"),
      rating: text(li.querySelector(".poly-reviews__rating")),
      international: !!cbt,
      site_flags: flags,
    };
  }).filter((o) => o.title && o.price);
})()`

// readyJS resolves once the grid has rendered or the anti-bot page is up, so a
// block is reported in seconds instead of waiting out the whole timeout.
const readyJS = `(() => {
  if (document.querySelectorAll("ol.ui-search-layout > li").length > 0) return "ok";
  const body = document.body ? document.body.innerText : "";
  if (/Hubo un error accediendo|Algo sali|verificar/i.test(body)) return "blocked";
  if (document.querySelector(".ui-search-rescue, .ui-empty-state")) return "empty";
  return "";
})()`

const (
	readyPollInterval = 400 * time.Millisecond
	readyPollTimeout  = 30 * time.Second
)

// waitReady polls until the grid renders or the anti-bot page shows up.
//
// This deliberately does not use chromedp.Poll: Mercado Livre hands off to a
// second navigation shortly after first paint, which destroys the JavaScript
// execution context and makes a single long-lived poll fail with "Inspected
// target navigated or closed". Re-evaluating from scratch each tick rides that
// out instead.
func waitReady(state *string) chromedp.ActionFunc {
	return func(ctx context.Context) error {
		deadline := time.Now().Add(readyPollTimeout)
		for {
			var got string
			if err := chromedp.Evaluate(readyJS, &got).Do(ctx); err == nil && got != "" {
				*state = got
				return nil
			}

			if time.Now().After(deadline) {
				return nil // caller reports the empty state
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(readyPollInterval):
			}
		}
	}
}

type rawOffer struct {
	Title         string   `json:"title"`
	URL           string   `json:"url"`
	ImageURL      string   `json:"image_url"`
	Seller        string   `json:"seller"`
	Price         string   `json:"price"`
	ListPrice     string   `json:"list_price"`
	Rating        string   `json:"rating"`
	International bool     `json:"international"`
	SiteFlags     []string `json:"site_flags"`
}

// Source searches Mercado Livre through a shared headful browser.
type Source struct {
	browser *browser.Browser
}

// New returns a Mercado Livre source driving b.
func New(b *browser.Browser) *Source { return &Source{browser: b} }

// Name identifies this source.
func (s *Source) Name() string { return Name }

// Search returns the first page of results for query.
func (s *Source) Search(ctx context.Context, query string) ([]source.Offer, error) {
	var state string
	var raws []rawOffer

	err := s.browser.Run(ctx,
		chromedp.Navigate(SearchURL(query)),
		waitReady(&state),
		chromedp.ActionFunc(func(ctx context.Context) error {
			if state != "ok" {
				return nil
			}
			return chromedp.Evaluate(extractJS, &raws).Do(ctx)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("meli: search %q: %w", query, err)
	}
	if state == "blocked" {
		return nil, source.ErrBlocked
	}
	if state == "" {
		return nil, fmt.Errorf("meli: search %q: grid never rendered", query)
	}

	offers := make([]source.Offer, 0, len(raws))
	for _, r := range raws {
		if o, ok := r.toOffer(); ok {
			offers = append(offers, o)
		}
	}
	return offers, nil
}

func (r rawOffer) toOffer() (source.Offer, bool) {
	price := source.ParseBRL(r.Price)
	if price <= 0 {
		return source.Offer{}, false
	}

	id := ProductID(r.URL)
	if id == "" {
		return source.Offer{}, false
	}

	list := source.ParseBRL(r.ListPrice)
	if list <= price {
		list = 0
	}

	return source.Offer{
		Source:         Name,
		ExternalID:     id,
		Title:          strings.Join(strings.Fields(r.Title), " "),
		URL:            CleanURL(r.URL),
		ImageURL:       r.ImageURL,
		Seller:         r.Seller,
		PriceCents:     price,
		ListPriceCents: list,
		Rating:         parseRating(r.Rating),
		International:  r.International,
		SiteFlags:      cleanFlags(r.SiteFlags),
	}, true
}

// SearchURL builds the listing URL for a free-text query. Mercado Livre's
// search lives at a path segment, not a query parameter.
func SearchURL(query string) string {
	return "https://lista.mercadolivre.com.br/" + Slug(query)
}

var slugStrip = regexp.MustCompile(`[^\p{L}\p{N}]+`)

// Slug converts a query into the hyphenated path segment the site expects.
func Slug(query string) string {
	s := slugStrip.ReplaceAllString(strings.ToLower(strings.TrimSpace(query)), "-")
	return strings.Trim(s, "-")
}

var (
	productIDRe = regexp.MustCompile(`/p/(MLB\d+)`)
	itemIDRe    = regexp.MustCompile(`\b(MLB-?\d{6,})`)
)

// ProductID extracts the stable Mercado Livre identifier from a card URL,
// preferring the catalog product id over the per-seller item id so price
// history follows the product rather than whichever seller is winning today.
func ProductID(rawURL string) string {
	if m := productIDRe.FindStringSubmatch(rawURL); m != nil {
		return m[1]
	}
	if m := itemIDRe.FindStringSubmatch(rawURL); m != nil {
		return strings.ReplaceAll(m[1], "-", "")
	}
	return ""
}

// CleanURL drops the tracking fragment Mercado Livre appends to every card
// link, which otherwise changes on every scan and defeats deduplication.
func CleanURL(rawURL string) string {
	if i := strings.IndexByte(rawURL, '#'); i >= 0 {
		rawURL = rawURL[:i]
	}
	if i := strings.IndexByte(rawURL, '?'); i >= 0 {
		rawURL = rawURL[:i]
	}
	return rawURL
}

var ratingRe = regexp.MustCompile(`\d+(?:[.,]\d+)?`)

func parseRating(s string) float64 {
	m := ratingRe.FindString(s)
	if m == "" {
		return 0
	}
	v, err := strconv.ParseFloat(strings.Replace(m, ",", ".", 1), 64)
	if err != nil || v < 0 || v > 5 {
		return 0
	}
	return v
}

func cleanFlags(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, f := range in {
		f = strings.Join(strings.Fields(f), " ")
		if f == "" || seen[f] || len(f) > 40 {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
