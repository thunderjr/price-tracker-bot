// Package amazon scrapes Amazon Brazil search results over plain HTTP.
//
// Unlike Mercado Livre, Amazon BR serves complete server-rendered search
// results to an ordinary HTTP client with a desktop User-Agent, so this source
// needs no browser. It does occasionally answer with a captcha interstitial;
// that is reported as source.ErrBlocked rather than parsed.
package amazon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"github.com/thunderjr/price-tracker-bot/internal/source"
)

// Name is the source identifier stored alongside every product row.
const Name = "amazon"

const maxBodyBytes = 8 << 20

// throttlePageBytes is the size below which an unrecognized page is Amazon's
// throttle interstitial rather than a redesigned search page. The real one
// runs well over a megabyte; the throttle page is about 2 KB.
const throttlePageBytes = 50 << 10

// retryBackoff is how long to wait before each retry of a throttled request.
// A scan runs every few hours, so spending half a minute to ride out a captcha
// is much cheaper than losing the source for the whole cycle.
var retryBackoff = []time.Duration{5 * time.Second, 20 * time.Second, 45 * time.Second}

// userAgents rotates across a few current desktop builds. Amazon tolerates
// plain clients but does notice a single frozen fingerprint over time.
var userAgents = []string{
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36",
}

// captchaMarkers identify Amazon's "are you a robot" interstitial.
var captchaMarkers = []string{
	"/errors/validateCaptcha",
	"Digite os caracteres",
	"Type the characters you see in this image",
}

// noResultsMarkers are how Amazon says a search genuinely matched nothing.
// Telling that apart from an unrecognized block page matters: a block that
// parses to zero offers would look like "this product has no offers" forever.
var noResultsMarkers = []string{
	"s-no-results",
	"Nenhum resultado para",
	"No results for",
}

// Source searches Amazon Brazil.
type Source struct {
	client *http.Client
}

// New returns an Amazon BR source. A nil client gets a sensible default.
func New(client *http.Client) *Source {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Source{client: client}
}

// Name identifies this source.
func (s *Source) Name() string { return Name }

// Search returns the first page of results for query.
//
// Amazon throttles bursts with a captcha that clears on its own, so throttling
// is waited out -- one backoff per entry in retryBackoff, each with a different
// User-Agent -- before giving up.
func (s *Source) Search(ctx context.Context, query string) ([]source.Offer, error) {
	offers, err := s.search(ctx, query)
	for _, wait := range retryBackoff {
		// Only throttling is worth waiting out. An unparseable page will look
		// exactly the same in twenty seconds.
		if !errors.Is(err, source.ErrThrottled) {
			return offers, err
		}
		// Jitter so repeated scans do not retry on the same rhythm.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait + rand.N(wait/2)):
		}
		offers, err = s.search(ctx, query)
	}
	return offers, err
}

func (s *Source) search(ctx context.Context, query string) ([]source.Offer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, SearchURL(query), nil)
	if err != nil {
		return nil, fmt.Errorf("amazon: request: %w", err)
	}
	req.Header.Set("User-Agent", userAgents[rand.IntN(len(userAgents))])
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "pt-BR,pt;q=0.9,en;q=0.8")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("amazon: get: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("amazon: http %d: %w", resp.StatusCode, source.ErrThrottled)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("amazon: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("amazon: read body: %w", err)
	}

	offers, err := Parse(string(body))
	if errors.Is(err, source.ErrBlocked) {
		// Say enough to tell a captcha apart from a layout change without
		// having to reproduce it.
		return nil, fmt.Errorf("amazon: %d byte page, title %q: %w", len(body), pageTitle(body), err)
	}
	return offers, err
}

// SearchURL builds the search URL for a free-text query.
func SearchURL(query string) string {
	return "https://www.amazon.com.br/s?" + url.Values{"k": {query}}.Encode()
}

// Parse extracts offers from a search results page. It is exported so the
// parser can be tested against saved fixtures without touching the network --
// which is how you find out whether a scan went empty because Amazon changed
// its markup or because the query genuinely has no results.
func Parse(html string) ([]source.Offer, error) {
	if containsAny(html, captchaMarkers) {
		return nil, source.ErrThrottled
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, fmt.Errorf("amazon: parse html: %w", err)
	}

	var (
		offers []source.Offer
		cards  int
	)
	doc.Find(`div[data-component-type="s-search-result"]`).Each(func(_ int, sel *goquery.Selection) {
		cards++
		if o, ok := parseResult(sel); ok {
			offers = append(offers, o)
		}
	})

	if len(offers) > 0 {
		return offers, nil
	}

	// A page full of result cards none of which carry a price is a real
	// answer: everything matching is "Ver opções de compra" with no offer of
	// its own. Calling that blocked would report "Sem resposta de: Amazon" and
	// send the scan looking for a fault that is not there.
	if cards > 0 || containsAny(html, noResultsMarkers) {
		return nil, nil
	}

	// No cards and no "nothing matched" notice, so this is not an empty result
	// set. Amazon's throttle page is a couple of kilobytes and carries no
	// title; anything substantial is a page we should be able to parse and no
	// longer can, which is a parser problem rather than a rate limit.
	if len(html) < throttlePageBytes {
		return nil, source.ErrThrottled
	}
	return nil, source.ErrBlocked
}

var titleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

func pageTitle(body []byte) string {
	m := titleRe.FindSubmatch(body)
	if m == nil {
		return ""
	}
	return strings.Join(strings.Fields(string(m[1])), " ")
}

func containsAny(s string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

func parseResult(sel *goquery.Selection) (source.Offer, bool) {
	asin, _ := sel.Attr("data-asin")
	if asin == "" {
		return source.Offer{}, false
	}

	// Every price lives inside the price recipe. Reading prices from the whole
	// card instead picks up the installment amount ("10x R$ 383,32") and
	// records it as the list price.
	price := sel.Find(`[data-cy="price-recipe"] .a-price[data-a-color="base"] > .a-offscreen`).First()
	if price.Length() == 0 {
		price = sel.Find(`[data-cy="price-recipe"] .a-price > .a-offscreen`).First()
	}
	priceCents := source.ParseBRL(price.Text())
	if priceCents <= 0 {
		return source.Offer{}, false // sponsored shells and out-of-stock cards
	}

	// Only a struck-through figure is a candidate former price. The other
	// .a-text-price in the recipe is the per-instalment amount ("em até 10x
	// de R$ 449,90"), which is far below the price and never a reference.
	listCents := source.ParseBRL(
		sel.Find(`[data-cy="price-recipe"] .a-text-price[data-a-strike="true"] > .a-offscreen`).First().Text())

	plan := source.ParseInstallments(
		text(sel.Find(`[data-cy="price-recipe"]`).First())).ResolveInterest(priceCents)
	if plan.IsInstallmentTotal(listCents) {
		// Not a former price: the same item paid in instalments. Keeping it
		// would post a discount that never expires.
		listCents = 0
	}
	if listCents <= priceCents {
		listCents = 0
	}

	title := text(sel.Find(`[data-cy="title-recipe"] h2`).First())
	if title == "" {
		title = text(sel.Find("h2").First())
	}
	if title == "" {
		return source.Offer{}, false
	}

	var flags []string
	if c := text(sel.Find(".s-coupon-highlight-color").First()); c != "" {
		flags = append(flags, c)
	}
	sel.Find(".a-badge-text").Each(func(_ int, b *goquery.Selection) {
		if t := text(b); t != "" && len(t) <= 40 {
			flags = append(flags, t)
		}
	})

	return source.Offer{
		Source:         Name,
		ExternalID:     asin,
		International:  looksInternational(title),
		Title:          title,
		URL:            "https://www.amazon.com.br/dp/" + asin,
		ImageURL:       attr(sel.Find("img.s-image").First(), "src"),
		Seller:         "",
		PriceCents:     priceCents,
		ListPriceCents: listCents,
		Rating:         parseRating(text(sel.Find(".a-icon-alt").First())),
		Installments:   plan,
		SiteFlags:      dedupe(flags),
	}, true
}

// internationalRe matches how Amazon BR labels a cross-border listing. There
// is no structural marker in the search results, only the title.
var internationalRe = regexp.MustCompile(`(?i)\b(international version|global version|versão internacional|produto importado|importado do exterior)\b`)

func looksInternational(title string) bool {
	return internationalRe.MatchString(title)
}

func text(sel *goquery.Selection) string {
	return strings.Join(strings.Fields(sel.Text()), " ")
}

func attr(sel *goquery.Selection, name string) string {
	v, _ := sel.Attr(name)
	return v
}

func parseRating(s string) float64 {
	// "4,8 de 5 estrelas"
	f, _, _ := strings.Cut(s, " ")
	v, err := strconv.ParseFloat(strings.Replace(f, ",", ".", 1), 64)
	if err != nil || v < 0 || v > 5 {
		return 0
	}
	return v
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
