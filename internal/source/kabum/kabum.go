// Package kabum scrapes KaBuM! search results over plain HTTP.
//
// Like Amazon BR and unlike Mercado Livre, KaBuM! serves its search results to
// an ordinary HTTP client with a desktop User-Agent, so this source needs no
// browser. What it serves is a Next.js page whose __NEXT_DATA__ script carries
// the whole result set as JSON, so the offers are read from that rather than
// from the rendered markup -- structured fields do not rot the way CSS
// selectors do.
//
// There is also a public catalogue API at
// servicespub.prod.api.aws.grupokabum.com.br/catalog/v2/products, and it is
// deliberately not used: it only answers for queries that resolve to a
// category. "playstation 5" works there, while "lego millennium falcon" comes
// back CATALOG_NOT_MATCH even though the site search finds real LEGO sets for
// it. This bot tracks arbitrary free-text queries, so the search page is the
// only endpoint that can serve them.
package kabum

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/thunderjr/price-tracker-bot/internal/source"
)

// Name is the source identifier stored alongside every product row.
const Name = "kabum"

const maxBodyBytes = 8 << 20

// throttlePageBytes is the size below which an unrecognized page is a block or
// challenge interstitial rather than a redesigned search page. A real search
// page runs to several hundred kilobytes.
const throttlePageBytes = 50 << 10

// retryBackoff is how long to wait before each retry of a throttled request.
var retryBackoff = []time.Duration{5 * time.Second, 20 * time.Second}

// userAgents rotates across a few current desktop builds.
var userAgents = []string{
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36",
}

// challengeMarkers identify a bot-check interstitial served in place of the
// search page.
var challengeMarkers = []string{
	"Request unsuccessful. Incapsula",
	"Attention Required! | Cloudflare",
	"/cdn-cgi/challenge-platform",
	"Checking your browser before accessing",
}

// Source searches KaBuM!.
type Source struct {
	client *http.Client
}

// New returns a KaBuM! source. A nil client gets a sensible default.
//
// That default carries its own transport, because KaBuM! is slow in a specific
// place: measured live, the TCP connect takes 14ms and the TLS handshake 8.1
// seconds. net/http allows 10 seconds for a handshake by default, so the same
// request succeeds from a shell and fails inside the container, where the
// little extra overhead tips it over -- "net/http: TLS handshake timeout"
// after a suspiciously round 10.05s. A scan runs every few hours and can
// afford to wait.
func New(client *http.Client) *Source {
	if client == nil {
		client = &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				ForceAttemptHTTP2:     true,
				TLSHandshakeTimeout:   30 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				MaxIdleConns:          10,
				IdleConnTimeout:       90 * time.Second,
			},
		}
	}
	return &Source{client: client}
}

// Name identifies this source.
func (s *Source) Name() string { return Name }

// Search returns the first page of results for query.
//
// A throttled request is waited out -- one backoff per entry in retryBackoff,
// each with a different User-Agent -- before giving up, on the same reasoning
// as the Amazon source: a scan runs every few hours, so half a minute spent
// riding out a block is much cheaper than losing the source for the cycle.
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
		return nil, fmt.Errorf("kabum: request: %w", err)
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
		return nil, fmt.Errorf("kabum: get: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("kabum: http %d: %w", resp.StatusCode, source.ErrThrottled)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kabum: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("kabum: read body: %w", err)
	}

	offers, err := Parse(string(body))
	if errors.Is(err, source.ErrBlocked) {
		// Say enough to tell a challenge apart from a layout change without
		// having to reproduce it.
		return nil, fmt.Errorf("kabum: %d byte page, title %q: %w", len(body), pageTitle(body), err)
	}
	return offers, err
}

// SearchURL builds the search URL for a free-text query. KaBuM! puts the term
// in a path segment, not a query parameter.
func SearchURL(query string) string {
	return "https://www.kabum.com.br/busca/" + Slug(query)
}

var slugStrip = regexp.MustCompile(`[^\p{L}\p{N}]+`)

// Slug converts a query into the hyphenated path segment the site expects.
func Slug(query string) string {
	return strings.Trim(slugStrip.ReplaceAllString(strings.ToLower(strings.TrimSpace(query)), "-"), "-")
}

var nextDataRe = regexp.MustCompile(`(?s)<script id="__NEXT_DATA__" type="application/json"[^>]*>(.*?)</script>`)

// Parse extracts offers from a search results page. It is exported so the
// parser can be tested against saved fixtures without touching the network --
// which is how you find out whether a scan went empty because KaBuM! changed
// its markup or because the query genuinely matched nothing.
func Parse(html string) ([]source.Offer, error) {
	if containsAny(html, challengeMarkers) {
		return nil, source.ErrThrottled
	}

	m := nextDataRe.FindStringSubmatch(html)
	if m == nil {
		// No result payload at all. KaBuM! pads a search that matches nothing
		// with unrelated recommendations rather than returning an empty set
		// (a nonsense query still claims 14400 items), so there is no such
		// thing here as a legitimately empty search page: a missing payload is
		// a block or a redesign, never "no results".
		if len(html) < throttlePageBytes {
			return nil, source.ErrThrottled
		}
		return nil, source.ErrBlocked
	}

	var data nextData
	if err := json.Unmarshal(sanitizeJSON(m[1]), &data); err != nil {
		return nil, fmt.Errorf("kabum: decode __NEXT_DATA__: %w", err)
	}

	products := data.Props.PageProps.Data.CatalogServer.Data
	offers := make([]source.Offer, 0, len(products))
	for _, p := range products {
		if o, ok := p.offer(); ok {
			offers = append(offers, o)
		}
	}
	if len(offers) == 0 {
		return nil, nil
	}
	return offers, nil
}

// nextData is the slice of the Next.js payload that holds the result set.
type nextData struct {
	Props struct {
		PageProps struct {
			Data struct {
				CatalogServer struct {
					Data []product `json:"data"`
				} `json:"catalogServer"`
			} `json:"data"`
		} `json:"pageProps"`
	} `json:"props"`
}

// product is one entry of the catalogue payload.
//
// Every price is json.Number rather than float64 on purpose: the literal text
// is what centsFromNumber needs, and decoding into a float64 first is the one
// step that loses a cent. The prime* fields are ignored -- those are prices
// only a KaBuM! Prime subscriber can actually pay, so ranking on them would
// quote a number the reader cannot get.
type product struct {
	Code              int64       `json:"code"`
	Name              string      `json:"name"`
	SellerName        string      `json:"sellerName"`
	Image             string      `json:"image"`
	Price             json.Number `json:"price"`
	PriceWithDiscount json.Number `json:"priceWithDiscount"`
	OldPrice          json.Number `json:"oldPrice"`
	MaxInstallment    string      `json:"maxInstallment"`
	Rating            float64     `json:"rating"`
	Available         bool        `json:"available"`
	Flags             struct {
		IsOffer        bool `json:"isOffer"`
		IsFlash        bool `json:"isFlash"`
		IsOpenbox      bool `json:"isOpenbox"`
		IsPreOrder     bool `json:"isPreOrder"`
		IsFreeShipping bool `json:"isFreeShipping"`
		HasGift        bool `json:"hasGift"`
	} `json:"flags"`
}

// offer normalizes one catalogue entry.
//
// KaBuM! publishes the two figures Amazon states in prose: priceWithDiscount is
// the cash price for Pix or boleto, and price is the card price, which its
// instalment plan totals to the cent. So price is this listing's financing
// total, not a former price -- reading it as one would post a discount on
// nearly every card that never expires. oldPrice is the only genuine former
// price, and only when it sits above both.
func (p product) offer() (source.Offer, bool) {
	if p.Code == 0 || p.Name == "" || !p.Available {
		return source.Offer{}, false
	}

	// The cash price leads, falling back to the card price on a listing with no
	// Pix discount to quote.
	priceCents := centsFromNumber(p.PriceWithDiscount)
	cardCents := centsFromNumber(p.Price)
	if priceCents <= 0 {
		priceCents = cardCents
	}
	if priceCents <= 0 {
		return source.Offer{}, false
	}

	plan := source.ParseInstallments(p.MaxInstallment)

	// maxInstallment goes stale when a marketplace seller re-prices. Seen live:
	// this listing moved from R$ 4.209,47 to R$ 6.509,90 on the card while its
	// plan still read "10x de R$ 420,94". Financing is never cheaper than the
	// card price, so a plan totalling less than it is stale rather than a
	// bargain -- and publishing it would be worse than saying nothing, because
	// in "parcelado" mode the watch ranks on the plan total and this listing
	// would lead the list at a phantom R$ 4.209,40 while actually being the
	// dearest console on the page.
	if total := plan.TotalCents(); total > 0 && total < cardCents && !plan.IsInstallmentTotal(cardCents) {
		plan = source.InstallmentPlan{}
	}
	plan = plan.ResolveInterestAgainst(cardCents)

	// oldPrice is a former price only when it sits above the card price. Equal
	// to it -- which is the usual case -- it is the card price restated, and
	// the gap down to the cash price is the Pix discount, not a markdown. This
	// cannot be left to the plan-total check the other sources use, because
	// that check reads the very field that goes stale above.
	listCents := centsFromNumber(p.OldPrice)
	if listCents <= cardCents || listCents <= priceCents {
		listCents = 0
	}

	var flags []string
	for _, f := range []struct {
		on    bool
		label string
	}{
		{p.Flags.IsFlash, "Oferta Flash"},
		{p.Flags.IsOffer, "Oferta"},
		// Open Box is an opened or returned unit. It is cheaper than new for a
		// reason, so it is surfaced rather than left to look like a bargain.
		{p.Flags.IsOpenbox, "Open Box"},
		{p.Flags.IsPreOrder, "Pré-venda"},
		{p.Flags.HasGift, "Brinde"},
		{p.Flags.IsFreeShipping, "Frete grátis"},
	} {
		if f.on {
			flags = append(flags, f.label)
		}
	}

	rating := p.Rating
	if rating < 0 || rating > 5 {
		rating = 0
	}

	return source.Offer{
		Source:     Name,
		ExternalID: strconv.FormatInt(p.Code, 10),
		Title:      strings.Join(strings.Fields(p.Name), " "),
		// The bare product path redirects to the slugged one, so the code alone
		// is a stable canonical link that cannot go stale when a name changes.
		URL:            "https://www.kabum.com.br/produto/" + strconv.FormatInt(p.Code, 10),
		ImageURL:       p.Image,
		Seller:         strings.TrimSpace(p.SellerName),
		PriceCents:     priceCents,
		ListPriceCents: listCents,
		Rating:         rating,
		Installments:   plan,
		SiteFlags:      flags,
	}, true
}

// centsFromNumber converts a JSON number literal to integer cents without a
// float ever touching the value.
//
// KaBuM! serializes its prices from 32-bit floats, so one page carries both
// "127.96" and "127.95999908447266" for the same figure, and R$ 1.943,33
// arrives as "1943.3299560546875". Rounding the decimal text is exact, while
// parsing to float64 and multiplying by 100 is where the cent goes.
//
// source.ParseBRL cannot do this job: it reads "." as a pt-BR thousands
// separator as soon as there are more than two decimals, which would turn that
// first figure into R$ 127.959.999.084.472,66.
func centsFromNumber(n json.Number) int64 {
	s := strings.TrimSpace(n.String())
	s = strings.TrimPrefix(s, "+")
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	if s == "" {
		return 0
	}

	// Exponent notation never appears in this catalogue, and guessing at what
	// it meant would be worse than declining the value.
	if strings.ContainsAny(s, "eE") {
		return 0
	}

	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	units, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || units > 1<<40 {
		return 0
	}
	if !digitsOnly(frac) {
		return 0
	}

	// Two decimals, rounded half-up on the third -- the digit that separates
	// 127.95999908447266 from 127.95.
	for len(frac) < 3 {
		frac += "0"
	}
	cents := int64(frac[0]-'0')*10 + int64(frac[1]-'0')
	if frac[2] >= '5' {
		cents++ // may reach 100, which the multiply below carries correctly
	}

	total := units*100 + cents
	if neg {
		return -total
	}
	return total
}

func digitsOnly(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// sanitizeJSON blanks the raw control characters KaBuM! leaves in the payload.
//
// Product descriptions are HTML pasted in with their newlines intact, and a
// literal newline inside a JSON string is invalid -- encoding/json rejects the
// whole document with "invalid character '\n' in string", which would drop
// every offer on the page over a field this source does not even read. A byte
// below 0x20 is never legal there, so replacing it with a space cannot corrupt
// a value that was already well formed.
func sanitizeJSON(s string) []byte {
	b := []byte(s)
	for i, c := range b {
		if c < 0x20 {
			b[i] = ' '
		}
	}
	return b
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
