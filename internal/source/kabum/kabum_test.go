package kabum

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/thunderjr/price-tracker-bot/internal/source"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

func find(offers []source.Offer, id string) (source.Offer, bool) {
	for _, o := range offers {
		if o.ExternalID == id {
			return o, true
		}
	}
	return source.Offer{}, false
}

func TestParseFixture(t *testing.T) {
	offers, err := Parse(fixture(t, "ps5.html"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(offers) < 20 {
		t.Fatalf("got %d offers, want >= 20 (payload likely changed)", len(offers))
	}

	for i, o := range offers {
		switch {
		case o.Source != Name:
			t.Errorf("offer %d: source = %q", i, o.Source)
		case o.ExternalID == "":
			t.Errorf("offer %d: empty code", i)
		case o.Title == "":
			t.Errorf("offer %d (%s): empty title", i, o.ExternalID)
		case o.PriceCents <= 0:
			t.Errorf("offer %d (%s): price = %d", i, o.ExternalID, o.PriceCents)
		case !strings.HasPrefix(o.URL, "https://www.kabum.com.br/produto/"):
			t.Errorf("offer %d: url = %q", i, o.URL)
		case o.ListPriceCents != 0 && o.ListPriceCents <= o.PriceCents:
			t.Errorf("offer %d (%s): list %d not above price %d", i, o.ExternalID, o.ListPriceCents, o.PriceCents)
		}
	}
}

// The card price is the plan total, so it must never be recorded as a former
// price. This is the listing the whole mapping was derived from: R$ 4.184,07
// cash, R$ 4.499,00 on the card, 10x R$ 449,90 -- and Amazon sells the same
// console at the same three numbers.
func TestParseCardPriceIsNotADiscount(t *testing.T) {
	offers, err := Parse(fixture(t, "ps5.html"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	o, ok := find(offers, "989702")
	if !ok {
		t.Fatal("listing 989702 missing from the fixture")
	}
	if o.PriceCents != 418407 {
		t.Errorf("price = %d, want 418407 (the Pix cash price)", o.PriceCents)
	}
	if o.ListPriceCents != 0 {
		t.Errorf("list price = %d, want 0: R$ 4.499 is the plan total, not a former price", o.ListPriceCents)
	}
	if o.Discount() != 0 {
		t.Errorf("discount = %d%%, want 0", o.Discount())
	}
	if o.Installments.Count != 10 || o.Installments.Each != 44990 {
		t.Errorf("plan = %dx %d, want 10x 44990", o.Installments.Count, o.Installments.Each)
	}
	if o.Installments.TotalCents() != 449900 {
		t.Errorf("plan total = %d, want 449900", o.Installments.TotalCents())
	}
	// The plan totals the card price exactly, so it is interest-free -- the
	// gap down to the cash price is a Pix discount. Labelling this "com juros"
	// would contradict Amazon, which prints "sem juros" on the same offer.
	if o.Installments.Interest != source.InterestFree {
		t.Errorf("interest = %q, want %q", o.Installments.Interest, source.InterestFree)
	}
}

// oldPrice above the card price is the one genuine former price KaBuM! gives.
func TestParseKeepsGenuineFormerPrice(t *testing.T) {
	offers, err := Parse(fixture(t, "ps5.html"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	o, ok := find(offers, "1065184")
	if !ok {
		t.Fatal("listing 1065184 missing from the fixture")
	}
	if o.PriceCents != 474207 {
		t.Errorf("price = %d, want 474207", o.PriceCents)
	}
	if o.ListPriceCents != 548280 {
		t.Errorf("list price = %d, want 548280 (oldPrice, above the R$ 5.099 plan total)", o.ListPriceCents)
	}
	if got := o.Discount(); got != 13 {
		t.Errorf("discount = %d%%, want 13%%", got)
	}
}

// Every listing on the fixture page must end up with a plan whose total is the
// card price, because that is the invariant the interest wording rests on.
func TestParseEveryPlanTotalsTheCardPrice(t *testing.T) {
	offers, err := Parse(fixture(t, "ps5.html"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var free, charged, unknown int
	for _, o := range offers {
		switch o.Installments.Interest {
		case source.InterestFree:
			free++
		case source.InterestCharged:
			charged++
		default:
			unknown++
			t.Errorf("%s: plan %dx %d left unresolved", o.ExternalID, o.Installments.Count, o.Installments.Each)
		}
	}
	if free == 0 {
		t.Error("no interest-free plan resolved, so the card-price comparison is not running")
	}
	t.Logf("interest: %d free, %d charged, %d unknown", free, charged, unknown)
}

// Money is integer cents, and KaBuM! serializes prices from 32-bit floats, so
// the same figure arrives with a tail of noise on some cards. Getting this
// wrong is not a rounding nit: routed through source.ParseBRL, the first case
// below reads as R$ 127.959.999.084.472,66.
func TestCentsFromNumber(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"4499", 449900},
		{"4184.07", 418407},
		{"449.9", 44990},
		{"0", 0},
		// float32 artifacts seen live on one page, alongside the clean form.
		{"127.95999908447266", 12796},
		{"127.96", 12796},
		{"170.97000122070312", 17097},
		{"1943.3299560546875", 194333},
		{"11167.42", 1116742},
		// half-up on the third decimal, including the carry into reais.
		{"1.995", 200},
		{"1.994", 199},
		{"1.999", 200},
		{"0.005", 1},
		// nothing usable rather than a wrong number.
		{"", 0},
		{"1e3", 0},
		{"abc", 0},
		{"12.3x", 0},
	}
	for _, c := range cases {
		if got := centsFromNumber(json.Number(c.in)); got != c.want {
			t.Errorf("centsFromNumber(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// The float32 artifacts have to survive the mapping, not merely
// centsFromNumber's own table: what reaches PriceCents is the number the bot
// ranks offers by and prints in the chat. Routed through source.ParseBRL these
// same literals come back as R$ 127.959.999.084.472,66 -- and large enough to
// overflow the tolerance arithmetic that would otherwise catch them, so
// nothing downstream notices.
func TestParseArtifactPricesReachCentsIntact(t *testing.T) {
	page := `<html><script id="__NEXT_DATA__" type="application/json">` +
		`{"props":{"pageProps":{"data":{"catalogServer":{"data":[` +
		`{"code":1,"name":"Artefato A","available":true,"price":127.95999908447266,"priceWithDiscount":127.95999908447266},` +
		`{"code":2,"name":"Artefato B","available":true,"price":1943.3299560546875,"priceWithDiscount":1943.3299560546875},` +
		`{"code":3,"name":"Artefato C","available":true,"price":170.97000122070312,"priceWithDiscount":170.97000122070312},` +
		`{"code":4,"name":"Artefato D","available":true,"price":4209.47021484375,"priceWithDiscount":4209.47021484375}` +
		`]}}}}}</script></html>`

	offers, err := Parse(page)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	want := map[string]int64{"1": 12796, "2": 194333, "3": 17097, "4": 420947}
	if len(offers) != len(want) {
		t.Fatalf("got %d offers, want %d", len(offers), len(want))
	}
	for _, o := range offers {
		if got := want[o.ExternalID]; o.PriceCents != got {
			t.Errorf("%s: price = %d, want %d", o.ExternalID, o.PriceCents, got)
		}
	}
}

// A description with its newlines left in is invalid JSON, and rejecting the
// document would drop every offer on the page over a field this source never
// reads.
func TestParseSurvivesRawControlCharacters(t *testing.T) {
	page := `<html><head><title>KaBuM!</title></head><body>` +
		`<script id="__NEXT_DATA__" type="application/json">` +
		`{"props":{"pageProps":{"data":{"catalogServer":{"data":[{` +
		`"code":123,"name":"Console Teste","available":true,` +
		`"description":"<p>linha um` + "\n" + `linha dois</p>",` +
		`"price":1000,"priceWithDiscount":900,"oldPrice":1000,` +
		`"maxInstallment":"10x de R$ 100,00"}]}}}}}` +
		`</script></body></html>`

	offers, err := Parse(page)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(offers) != 1 {
		t.Fatalf("got %d offers, want 1", len(offers))
	}
	if offers[0].PriceCents != 90000 {
		t.Errorf("price = %d, want 90000", offers[0].PriceCents)
	}
}

func TestParseSkipsUnavailableAndUnpriced(t *testing.T) {
	page := `<html><script id="__NEXT_DATA__" type="application/json">` +
		`{"props":{"pageProps":{"data":{"catalogServer":{"data":[` +
		`{"code":1,"name":"Fora de estoque","available":false,"price":100,"priceWithDiscount":90},` +
		`{"code":2,"name":"Sem preço","available":true,"price":0,"priceWithDiscount":0},` +
		`{"code":3,"name":"Sem nome","available":true,"price":100,"priceWithDiscount":90},` +
		`{"code":4,"name":"Válido","available":true,"price":100,"priceWithDiscount":90}` +
		`]}}}}}</script></html>`

	offers, err := Parse(page)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(offers) != 2 {
		t.Fatalf("got %d offers, want 2 (codes 3 and 4)", len(offers))
	}
}

// A listing with no Pix discount quotes one price for both, and must not be
// dropped for having priceWithDiscount at zero.
func TestParseFallsBackToTheCardPrice(t *testing.T) {
	page := `<html><script id="__NEXT_DATA__" type="application/json">` +
		`{"props":{"pageProps":{"data":{"catalogServer":{"data":[` +
		`{"code":7,"name":"Sem desconto","available":true,"price":7099,"priceWithDiscount":0,` +
		`"oldPrice":7099,"maxInstallment":"10x de R$ 709,90"}` +
		`]}}}}}</script></html>`

	offers, err := Parse(page)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(offers) != 1 {
		t.Fatalf("got %d offers, want 1", len(offers))
	}
	if offers[0].PriceCents != 709900 {
		t.Errorf("price = %d, want 709900", offers[0].PriceCents)
	}
	if offers[0].ListPriceCents != 0 {
		t.Errorf("list price = %d, want 0", offers[0].ListPriceCents)
	}
}

// A marketplace seller re-pricing leaves maxInstallment behind. Seen live on
// two listings at once: the card price moved to R$ 6.509,90 while the plan
// still read "10x de R$ 420,94", a total of R$ 4.209,40.
//
// Both halves of that payload are traps. The plan would lead a "parcelado"
// watch at a phantom R$ 4.209,40 total while the listing is really the dearest
// console on the page, and oldPrice repeats the card price, which reads as a
// 5% markdown that is only the Pix discount wearing a strikethrough.
func TestParseRejectsAStaleInstallmentPlan(t *testing.T) {
	page := `<html><script id="__NEXT_DATA__" type="application/json">` +
		`{"props":{"pageProps":{"data":{"catalogServer":{"data":[` +
		`{"code":1004851,"name":"Console PS5 Slim Digital","available":true,` +
		`"price":6509.9,"priceWithDiscount":6184.41,"oldPrice":6509.9,` +
		`"maxInstallment":"10x de R$ 420,94"}` +
		`]}}}}}</script></html>`

	offers, err := Parse(page)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(offers) != 1 {
		t.Fatalf("got %d offers, want 1", len(offers))
	}
	o := offers[0]

	if o.PriceCents != 618441 {
		t.Errorf("price = %d, want 618441", o.PriceCents)
	}
	if o.Installments.Count != 0 || o.Installments.Each != 0 {
		t.Errorf("plan = %dx %d, want none: a plan totalling R$ 4.209,40 cannot belong to a R$ 6.509,90 card price",
			o.Installments.Count, o.Installments.Each)
	}
	if o.Installments.TotalCents() != 0 {
		t.Errorf("plan total = %d, want 0", o.Installments.TotalCents())
	}
	if o.ListPriceCents != 0 {
		t.Errorf("list price = %d, want 0: oldPrice only restates the card price", o.ListPriceCents)
	}
	if o.Discount() != 0 {
		t.Errorf("discount = %d%%, want 0", o.Discount())
	}
}

// A plan totalling more than the card price is real interest, not stale data --
// financing costs more, it never costs less -- so that one is kept and named.
func TestParseKeepsAPlanAboveTheCardPrice(t *testing.T) {
	page := `<html><script id="__NEXT_DATA__" type="application/json">` +
		`{"props":{"pageProps":{"data":{"catalogServer":{"data":[` +
		`{"code":5,"name":"Console com juros","available":true,` +
		`"price":4499,"priceWithDiscount":4184.07,"oldPrice":4499,` +
		`"maxInstallment":"12x de R$ 419,90"}` +
		`]}}}}}</script></html>`

	offers, err := Parse(page)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	o := offers[0]
	if o.Installments.Count != 12 || o.Installments.Each != 41990 {
		t.Fatalf("plan = %dx %d, want 12x 41990", o.Installments.Count, o.Installments.Each)
	}
	if o.Installments.Interest != source.InterestCharged {
		t.Errorf("interest = %q, want %q", o.Installments.Interest, source.InterestCharged)
	}
}

// oldPrice equal to the card price is never a markdown, whatever the plan says.
func TestParseOldPriceEqualToTheCardPriceIsNotADiscount(t *testing.T) {
	page := `<html><script id="__NEXT_DATA__" type="application/json">` +
		`{"props":{"pageProps":{"data":{"catalogServer":{"data":[` +
		`{"code":9,"name":"Console","available":true,` +
		`"price":4499,"priceWithDiscount":4184.07,"oldPrice":4499,` +
		`"maxInstallment":"10x de R$ 449,90"}` +
		`]}}}}}</script></html>`

	offers, err := Parse(page)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := offers[0].ListPriceCents; got != 0 {
		t.Errorf("list price = %d, want 0", got)
	}
}

func TestParseChallengeIsThrottled(t *testing.T) {
	for _, marker := range challengeMarkers {
		page := "<html><body>" + marker + "</body></html>"
		if _, err := Parse(page); !errors.Is(err, source.ErrThrottled) {
			t.Errorf("marker %q: err = %v, want ErrThrottled", marker, err)
		}
	}
}

func TestParseSmallUnrecognizedPageIsThrottled(t *testing.T) {
	if _, err := Parse("<html><body>nope</body></html>"); !errors.Is(err, source.ErrThrottled) {
		t.Errorf("err = %v, want ErrThrottled", err)
	}
}

// A big page with no payload is a redesign, not a rate limit -- and never "no
// results", because KaBuM! pads a search that matches nothing with unrelated
// recommendations instead of returning an empty set.
func TestParseLargeUnrecognizedPageIsBlocked(t *testing.T) {
	page := "<html><body>" + strings.Repeat("x", throttlePageBytes+1) + "</body></html>"
	_, err := Parse(page)
	if !errors.Is(err, source.ErrBlocked) {
		t.Errorf("err = %v, want ErrBlocked", err)
	}
	if errors.Is(err, source.ErrThrottled) {
		t.Error("a redesigned page must not be reported as throttling")
	}
}

func TestSearchURL(t *testing.T) {
	cases := map[string]string{
		"playstation 5":           "https://www.kabum.com.br/busca/playstation-5",
		"  LEGO Millennium  ":     "https://www.kabum.com.br/busca/lego-millennium",
		"LEGO Olivia Rodrigo 431": "https://www.kabum.com.br/busca/lego-olivia-rodrigo-431",
	}
	for in, want := range cases {
		if got := SearchURL(in); got != want {
			t.Errorf("SearchURL(%q) = %q, want %q", in, got, want)
		}
	}
}
