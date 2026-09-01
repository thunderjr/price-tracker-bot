package amazon

import (
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

func TestParseFixture(t *testing.T) {
	offers, err := Parse(fixture(t, "ps5.html"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(offers) < 20 {
		t.Fatalf("got %d offers, want >= 20 (markup likely changed)", len(offers))
	}

	for i, o := range offers {
		switch {
		case o.Source != Name:
			t.Errorf("offer %d: source = %q", i, o.Source)
		case len(o.ExternalID) != 10:
			t.Errorf("offer %d: ASIN = %q, want 10 chars", i, o.ExternalID)
		case o.Title == "":
			t.Errorf("offer %d (%s): empty title", i, o.ExternalID)
		case o.PriceCents <= 0:
			t.Errorf("offer %d (%s): price = %d", i, o.ExternalID, o.PriceCents)
		case !strings.HasPrefix(o.URL, "https://www.amazon.com.br/dp/"):
			t.Errorf("offer %d: url = %q", i, o.URL)
		}
	}
}

// The list price must come from the price recipe. Reading it from anywhere
// else in the card picks up the installment amount, which turns a R$ 4.599
// console into a fake 92% discount.
func TestParseFixtureListPriceIsNotInstallment(t *testing.T) {
	offers, err := Parse(fixture(t, "ps5.html"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var withList int
	for _, o := range offers {
		if o.ListPriceCents == 0 {
			continue
		}
		withList++
		if o.ListPriceCents <= o.PriceCents {
			t.Errorf("%s: list %d <= price %d", o.ExternalID, o.ListPriceCents, o.PriceCents)
		}
		if d := o.Discount(); d > 90 {
			t.Errorf("%s: implausible discount %d%% (price %s, list %s)",
				o.ExternalID, d, source.FormatBRL(o.PriceCents), source.FormatBRL(o.ListPriceCents))
		}
	}
	if withList == 0 {
		t.Error("no offer carried a list price; the a-text-price selector likely broke")
	}
}

// A captcha is Amazon saying "later", not "the page changed". Only the first
// is worth retrying, so Parse has to tell them apart.
func TestParseCaptchaIsThrottled(t *testing.T) {
	page := `<html><body><form action="/errors/validateCaptcha">Digite os caracteres</form></body></html>`

	_, err := Parse(page)
	if !errors.Is(err, source.ErrThrottled) {
		t.Fatalf("Parse(captcha) error = %v, want ErrThrottled", err)
	}
	// Callers that only care "this source gave nothing" still work.
	if !errors.Is(err, source.ErrBlocked) {
		t.Error("ErrThrottled does not satisfy errors.Is(err, ErrBlocked)")
	}
}

// Amazon's throttle interstitial is a couple of kilobytes with no title.
func TestParseSmallUnrecognizedPageIsThrottled(t *testing.T) {
	for name, page := range map[string]string{
		"empty":        `<html><body></body></html>`,
		"holding page": `<html><head><title>&nbsp;</title></head><body>Sorry</body></html>`,
	} {
		if _, err := Parse(page); !errors.Is(err, source.ErrThrottled) {
			t.Errorf("%s: Parse error = %v, want ErrThrottled", name, err)
		}
	}
}

// A full-size page we can no longer parse is a redesign, and waiting will not
// help. It must not be reported as throttling, or a broken parser hides behind
// retries forever.
func TestParseLargeUnrecognizedPageIsBlocked(t *testing.T) {
	page := `<html><body><div class="s-result-item">` +
		strings.Repeat("<span>redesigned markup</span>", 5000) +
		`</div></body></html>`
	if len(page) < throttlePageBytes {
		t.Fatalf("test page is %d bytes, needs to exceed %d", len(page), throttlePageBytes)
	}

	_, err := Parse(page)
	if !errors.Is(err, source.ErrBlocked) {
		t.Fatalf("Parse error = %v, want ErrBlocked", err)
	}
	if errors.Is(err, source.ErrThrottled) {
		t.Error("a redesigned page was reported as throttling; retries would mask it")
	}
}

// A genuine "nothing matched" search is neither.
func TestParseGenuineNoResultsIsNotAnError(t *testing.T) {
	for name, page := range map[string]string{
		"pt": `<html><body><span class="s-no-results">Nenhum resultado para sua consulta.</span></body></html>`,
		"en": `<html><body><div>No results for zzqq</div></body></html>`,
	} {
		offers, err := Parse(page)
		if err != nil {
			t.Errorf("%s: Parse error = %v, want nil", name, err)
		}
		if len(offers) != 0 {
			t.Errorf("%s: got %d offers, want 0", name, len(offers))
		}
	}
}

func TestSearchURL(t *testing.T) {
	got := SearchURL("LEGO Millennium Falcon")
	want := "https://www.amazon.com.br/s?k=LEGO+Millennium+Falcon"
	if got != want {
		t.Errorf("SearchURL = %q, want %q", got, want)
	}
}
