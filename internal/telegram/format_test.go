package telegram

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/thunderjr/price-tracker-bot/internal/store"
	"github.com/thunderjr/price-tracker-bot/internal/tracker"
)

// MarkdownV2 rejects a message with an unescaped reserved character, so
// Telegram drops it entirely. Product titles are full of them.
var markdownReserved = []string{"\\", "_", "*", "[", "]", "(", ")", "~", "`", ">", "#", "+", "-", "=", "|", "{", "}", ".", "!"}

// unescapedOutsideLinks reports reserved characters that are neither escaped
// nor part of a link target, which is the only place they may appear raw.
func unescapedOutsideLinks(t *testing.T, s string) []string {
	t.Helper()

	// Strip the two places MarkdownV2 allows raw punctuation: "(...)" link
	// targets and `code spans`.
	var b strings.Builder
	depth, code := 0, false
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\':
			b.WriteString(s[i : i+1])
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			}
			continue
		case s[i] == '`':
			code = !code
			continue
		case code:
			continue
		case s[i] == '(' && i > 0 && s[i-1] == ']':
			depth++
			continue
		case s[i] == ')' && depth > 0:
			depth--
			continue
		}
		if depth == 0 {
			b.WriteByte(s[i])
		}
	}
	if code {
		t.Errorf("unbalanced code span in:\n%s", s)
	}

	var bad []string
	stripped := b.String()
	for i := 0; i < len(stripped); i++ {
		if stripped[i] == '\\' {
			i++
			continue
		}
		for _, r := range markdownReserved {
			if stripped[i] == r[0] && !isMarkup(stripped, i) {
				bad = append(bad, r)
			}
		}
	}
	return bad
}

// isMarkup allows the emphasis characters we use deliberately.
func isMarkup(s string, i int) bool {
	return s[i] == '*' || s[i] == '_' || s[i] == '`' || s[i] == '[' || s[i] == ']'
}

func digestAlert(kind tracker.Kind, title string, price, ref int64) tracker.Alert {
	return tracker.Alert{
		Candidate: tracker.Candidate{Kind: kind, PriceCents: price, RefCents: ref, Confident: kind != tracker.KindSiteFlag},
		Watch:     store.Watch{ID: 1, ChatID: 1, Query: "ps5 slim"},
		Product:   store.Product{Source: "meli", Title: title, URL: "https://ml/p/MLB1"},
	}
}

func digestOffer(src, title string, price, list, low int64) store.WatchOffer {
	return store.WatchOffer{
		Product:        store.Product{Source: src, Title: title, URL: "https://" + src + "/x"},
		PriceCents:     price,
		ListPriceCents: list,
		LowCents:       low,
		SeenAt:         time.Now(),
	}
}

func TestFormatDigestEscapesTitles(t *testing.T) {
	alerts := []tracker.Alert{digestAlert(
		tracker.KindNewLow,
		// Every character MarkdownV2 cares about, in a plausible title.
		"Console PlayStation®5 Slim [Digital] - 825GB (Astro Bot) 10% + 2 jogos_extra!",
		404700, 459900,
	)}
	offers := []store.WatchOffer{
		digestOffer("meli", "Console PlayStation®5 Slim [Digital] - 825GB (Astro Bot)", 404700, 459900, 389900),
		digestOffer("amazon", "PlayStation 5 Slim Disk 1TB - 10% off!", 418406, 0, 0),
	}

	got := formatDigest(store.Watch{Query: "ps5 (slim)"}, alerts, offers, len(offers), nil)
	if bad := unescapedOutsideLinks(t, got); len(bad) > 0 {
		t.Errorf("unescaped MarkdownV2 characters %v in:\n%s", bad, got)
	}
	if !strings.Contains(got, "4\\.047,00") {
		t.Errorf("price not rendered/escaped:\n%s", got)
	}
}

// The whole point: one message, at most three links, however many findings.
func TestFormatDigestCapsAtThreeOffers(t *testing.T) {
	var alerts []tracker.Alert
	for i := range 56 {
		alerts = append(alerts, digestAlert(tracker.KindNewLow, fmt.Sprintf("Console %d", i), 400000, 500000))
	}
	// Only the top rows are fetched for display; the total is passed
	// separately, because "+N outras" must describe the watch, not the query
	// limit that produced these rows.
	var offers []store.WatchOffer
	for i := range digestOffers {
		offers = append(offers, digestOffer("meli", fmt.Sprintf("Console %d", i), int64(400000+i), 0, 0))
	}

	got := formatDigest(store.Watch{Query: "ps5"}, alerts, offers, 91, nil)
	// The headline carries a link of its own, so count the offer rows only.
	if n := strings.Count(got, "https://meli/x"); n != 3 {
		t.Errorf("digest lists %d offers, want 3:\n%s", n, got)
	}
	if !strings.Contains(got, "e mais 55 alertas") {
		t.Errorf("digest did not report the remaining findings:\n%s", got)
	}
	if !strings.Contains(got, "+88 outras ofertas") {
		t.Errorf("digest miscounted the remaining offers (should be 91-3):\n%s", got)
	}
}

// The headline goes to the finding the user most wants to see.
func TestFormatDigestHeadlinePicksStrongestKind(t *testing.T) {
	alerts := []tracker.Alert{
		digestAlert(tracker.KindSiteFlag, "flagged", 400000, 500000),
		digestAlert(tracker.KindTarget, "hit the target", 340000, 350000),
		digestAlert(tracker.KindNewLow, "record low", 390000, 400000),
	}
	got := formatDigest(store.Watch{Query: "ps5"}, alerts, nil, 0, nil)
	if !strings.Contains(got, "🎯") || !strings.Contains(got, "hit the target") {
		t.Errorf("target did not take the headline:\n%s", got)
	}
}

func TestFormatDigestMarksLowConfidence(t *testing.T) {
	alerts := []tracker.Alert{digestAlert(tracker.KindSiteFlag, "x", 100000, 200000)}
	if !strings.Contains(formatDigest(store.Watch{Query: "ps5"}, alerts, nil, 0, nil), "referência") {
		t.Error("site_flag digest did not disclose that the reference price is the seller's own")
	}

	confident := []tracker.Alert{digestAlert(tracker.KindNewLow, "x", 100000, 200000)}
	if strings.Contains(formatDigest(store.Watch{Query: "ps5"}, confident, nil, 0, nil), "referência informado") {
		t.Error("a confident digest carried the low-confidence disclaimer")
	}
}

// The scan report and a price alert are the same message shape now, so the
// escaping and the skipped-source note have to hold for both.
func TestFormatDigestReportsSkippedSources(t *testing.T) {
	w := store.Watch{Query: "lego 10.300 (delorean)", TargetCents: 350000}
	offers := []store.WatchOffer{
		digestOffer("meli", "LEGO® Icons 10300 - Back to the Future (1.872 peças)", 404700, 459900, 389900),
	}

	got := formatDigest(w, nil, offers, len(offers), map[string]error{"amazon": errTest})
	if bad := unescapedOutsideLinks(t, got); len(bad) > 0 {
		t.Errorf("unescaped MarkdownV2 characters %v in:\n%s", bad, got)
	}
	if !strings.Contains(got, "Amazon") {
		t.Errorf("skipped source not reported:\n%s", got)
	}
	if !strings.Contains(got, "mín 30d") {
		t.Errorf("window low not shown:\n%s", got)
	}
	if !strings.Contains(got, "Alvo") {
		t.Errorf("target not shown:\n%s", got)
	}
}

func TestFormatDigestEmpty(t *testing.T) {
	got := formatDigest(store.Watch{Query: "nada"}, nil, nil, 0, nil)
	if !strings.Contains(got, "Nenhuma oferta") {
		t.Errorf("empty result not explained:\n%s", got)
	}
	if bad := unescapedOutsideLinks(t, got); len(bad) > 0 {
		t.Errorf("unescaped MarkdownV2 characters %v in:\n%s", bad, got)
	}
}

func TestFormatDetailAndList(t *testing.T) {
	rows := []watchRow{
		{
			Watch: store.Watch{ID: 1, Query: "ps5 (slim)", TargetCents: 350000, Active: true, LastScanAt: time.Now().Add(-2 * time.Hour)},
			Stats: store.WatchStats{Products: 14, BestCents: 404700, LowCents: 389900},
		},
		{
			Watch: store.Watch{ID: 2, Query: "steam deck", Active: false},
			Stats: store.WatchStats{},
		},
	}

	for _, r := range rows {
		if bad := unescapedOutsideLinks(t, formatDetail(r)); len(bad) > 0 {
			t.Errorf("formatDetail(%q) has unescaped %v", r.Watch.Query, bad)
		}
	}
	if bad := unescapedOutsideLinks(t, formatList(rows)); len(bad) > 0 {
		t.Errorf("formatList has unescaped %v", bad)
	}

	if !strings.Contains(formatDetail(rows[1]), "pausada") {
		t.Error("paused state not shown in the detail view")
	}
	if !strings.Contains(formatDetail(rows[1]), "nunca escaneada") {
		t.Error("never-scanned state not shown")
	}
}

func TestFormatListEmpty(t *testing.T) {
	if bad := unescapedOutsideLinks(t, formatList(nil)); len(bad) > 0 {
		t.Errorf("empty list has unescaped %v", bad)
	}
}

func TestHelpTextIsValidMarkdown(t *testing.T) {
	// A fractional threshold puts a "." in the text, which MarkdownV2 counts
	// as markup and would make Telegram drop the whole message.
	for _, threshold := range []float64{0.01, 0.005} {
		text := helpText(threshold)
		if bad := unescapedOutsideLinks(t, text); len(bad) > 0 {
			t.Errorf("helpText(%v) has unescaped %v:\n%s", threshold, bad, text)
		}
	}

	// The quoted figure follows the configuration instead of being written
	// into the text, where it went stale the moment the default changed.
	if got := helpText(0.03); !strings.Contains(got, "mais de 3%") {
		t.Errorf("helpText(0.03) does not quote the configured threshold:\n%s", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("curto", 20); got != "curto" {
		t.Errorf("truncate short = %q", got)
	}
	got := truncate("Console PlayStation 5 Slim Digital Edition", 20)
	if len([]rune(got)) != 20 {
		t.Errorf("truncate = %q, %d runes, want 20", got, len([]rune(got)))
	}
	// Multi-byte characters must be counted as runes, not bytes.
	if got := truncate("ção ção ção ção", 8); len([]rune(got)) != 8 {
		t.Errorf("truncate on multibyte = %q, %d runes", got, len([]rune(got)))
	}
}

var errTest = errTestType{}

type errTestType struct{}

func (errTestType) Error() string { return "test" }

// The financing line, including the total, because the instalments usually add
// up to more than the cash price shown above them.
func TestFormatDigestShowsInstallments(t *testing.T) {
	offer := digestOffer("amazon", "PlayStation 5 Slim Digital", 418406, 0, 0)
	offer.InstallmentCount = 10
	offer.InstallmentEachCents = 44990
	offer.InstallmentInterest = "sem juros"

	got := formatDigest(store.Watch{Query: "ps5"}, nil, []store.WatchOffer{offer}, 1, nil)
	if !strings.Contains(got, "ou 10x R$ 449,90 sem juros") {
		t.Errorf("installment line missing:\n%s", got)
	}
	if !strings.Contains(got, `total R$ 4\.499,00`) {
		t.Errorf("installment total missing:\n%s", got)
	}
	if bad := unescapedOutsideLinks(t, got); len(bad) > 0 {
		t.Errorf("unescaped MarkdownV2 characters %v in:\n%s", bad, got)
	}
}

// No plan, or a single "instalment", means nothing to show.
func TestInstallmentLinesOmittedWhenAbsent(t *testing.T) {
	for name, o := range map[string]store.WatchOffer{
		"no plan":     {PriceCents: 418406},
		"one payment": {PriceCents: 418406, InstallmentCount: 1, InstallmentEachCents: 418406},
		"zero amount": {PriceCents: 418406, InstallmentCount: 10},
	} {
		if got := installmentLines(store.ModeCash, o); len(got) != 0 {
			t.Errorf("%s: installmentLines = %v, want none", name, got)
		}
	}
}

// When the instalments add up to no more than the cash price there is no total
// worth spelling out.
func TestInstallmentLinesOmitRedundantTotal(t *testing.T) {
	o := store.WatchOffer{PriceCents: 449900, InstallmentCount: 10, InstallmentEachCents: 44990}
	lines := installmentLines(store.ModeCash, o)
	if len(lines) != 1 || !strings.Contains(lines[0], "ou 10x R$ 449,90") {
		t.Fatalf("installmentLines = %v", lines)
	}
	if strings.Contains(lines[0], "total") {
		t.Errorf("installmentLines = %v, want no total when it equals the price", lines)
	}
}

// Both wordings are passed through as the site states them. A plan marked
// "com juros" costs real money and must not be shown as if it were free.
func TestInstallmentLinesCarryTheInterestWording(t *testing.T) {
	for _, tc := range []struct {
		interest string
		want     string
	}{
		{"sem juros", "ou 10x R$ 449,90 sem juros"},
		{"com juros", "ou 10x R$ 449,90 com juros"},
		{"", "ou 10x R$ 449,90 \\(total"}, // unknown: stated as neither
	} {
		o := store.WatchOffer{
			PriceCents:           418406,
			InstallmentCount:     10,
			InstallmentEachCents: 44990,
			InstallmentInterest:  tc.interest,
		}
		lines := installmentLines(store.ModeCash, o)
		if len(lines) != 1 || !strings.Contains(lines[0], tc.want) {
			t.Errorf("interest %q: installmentLines = %v, want one containing %q", tc.interest, lines, tc.want)
		}
		if tc.interest == "" && strings.Contains(lines[0], "juros") {
			t.Errorf("unknown interest claimed a wording: %v", lines)
		}
	}
}

// Mercado Livre's other-payment figure is a separate option and is listed too.
func TestInstallmentLinesShowOtherMeans(t *testing.T) {
	o := store.WatchOffer{
		PriceCents:           519900,
		InstallmentCount:     12,
		InstallmentEachCents: 45999,
		InstallmentInterest:  "sem juros",
		OtherMeansCents:      549900,
	}

	lines := installmentLines(store.ModeCash, o)
	if len(lines) != 2 {
		t.Fatalf("installmentLines = %v, want the plan and the other-payment total", lines)
	}
	if !strings.Contains(lines[1], `R$ 5\.499,00 em outros meios`) {
		t.Errorf("other-payment line = %q", lines[1])
	}

	// Not worth a line when it is not actually more than the price.
	o.OtherMeansCents = 519900
	if got := installmentLines(store.ModeCash, o); len(got) != 1 {
		t.Errorf("installmentLines = %v, want the other-payment line dropped", got)
	}
}

// The detail view has to answer "what would this cost me" without a second tap.
func TestFormatDetailShowsPaymentOptions(t *testing.T) {
	row := watchRow{
		Watch: store.Watch{ID: 1, Query: "ps5 slim", Active: true},
		Stats: store.WatchStats{Products: 14, BestCents: 418406},
		Best: store.WatchOffer{
			PriceCents:           418406,
			InstallmentCount:     10,
			InstallmentEachCents: 44990,
			InstallmentInterest:  "sem juros",
		},
	}

	got := formatDetail(row)
	if !strings.Contains(got, "ou 10x R$ 449,90 sem juros") {
		t.Errorf("detail view omits the payment options:\n%s", got)
	}
	if bad := unescapedOutsideLinks(t, got); len(bad) > 0 {
		t.Errorf("unescaped MarkdownV2 characters %v in:\n%s", bad, got)
	}
}

// A backslash in a title is not escaped by go-telegram's EscapeMarkdown, so it
// reached Telegram raw and escaped the character after it -- breaking a link,
// or costing the whole message.
func TestFormatDigestEscapesBackslash(t *testing.T) {
	alerts := []tracker.Alert{digestAlert(tracker.KindNewLow, `LEGO 10\/1 Falcon`, 400000, 500000)}
	offers := []store.WatchOffer{digestOffer("meli", `LEGO 10\/1 Falcon`, 400000, 0, 0)}

	got := formatDigest(store.Watch{Query: "lego"}, alerts, offers, 1, nil)
	if strings.Contains(got, `10\/1`) {
		t.Errorf("the title's backslash reached Telegram unescaped:\n%s", got)
	}
	if !strings.Contains(got, `10\\`) {
		t.Errorf("the title's backslash was not doubled:\n%s", got)
	}
	if bad := unescapedOutsideLinks(t, got); len(bad) > 0 {
		t.Errorf("digest has unescaped %v:\n%s", bad, got)
	}
}

// The alerted listing is often not one of the three cheapest offers, and the
// message that announced its price then offered no way to reach it.
func TestFormatDigestLinksTheHeadlineListing(t *testing.T) {
	alerts := []tracker.Alert{digestAlert(tracker.KindTarget, "Console PlayStation 5 Slim", 340000, 350000)}
	// Three cheaper offers, none of them the listing that alerted.
	var offers []store.WatchOffer
	for i := range digestOffers {
		offers = append(offers, digestOffer("amazon", fmt.Sprintf("Jogo %d", i), int64(20000+i), 0, 0))
	}

	got := formatDigest(store.Watch{Query: "ps5"}, alerts, offers, 4, nil)
	if !strings.Contains(got, "https://ml/p/MLB1") {
		t.Errorf("the alerted listing has no link anywhere in the digest:\n%s", got)
	}
}

// In "parcelado" mode the financed total is what the watch ranked on, so it is
// what the message has to lead with -- and the cash price becomes the
// alternative worth naming.
func TestFormatDigestLeadsWithTheFinancedTotal(t *testing.T) {
	o := digestOffer("amazon", "PlayStation 5 Slim", 410000, 0, 0)
	o.InstallmentCount = 12
	o.InstallmentEachCents = 41990
	o.InstallmentInterest = "com juros"
	o.EffectiveCents = 503880

	got := formatDigest(
		store.Watch{Query: "ps5", PriceMode: store.ModeInstallment},
		nil, []store.WatchOffer{o}, 1, nil)

	for _, want := range []string{
		`*R$ 5\.038,80*`,          // the headline is the financed total
		"12x R$ 419,90 com juros", // broken down, and honest about the interest
		`ou R$ 4\.100,00 à vista`, // the other way to pay is still offered
		"💳 _por total parcelado_",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("digest missing %q:\n%s", want, got)
		}
	}
	// The total is the headline, so repeating it in brackets is noise.
	if strings.Contains(got, "total R$ 5") {
		t.Errorf("digest repeated the total it already leads with:\n%s", got)
	}
	if bad := unescapedOutsideLinks(t, got); len(bad) > 0 {
		t.Errorf("unescaped MarkdownV2 characters %v in:\n%s", bad, got)
	}
}

// Cash mode is unchanged: the cash price leads and the plan is the alternative.
func TestFormatDigestCashModeLeadsWithCash(t *testing.T) {
	o := digestOffer("amazon", "PlayStation 5 Slim", 410000, 0, 0)
	o.InstallmentCount = 12
	o.InstallmentEachCents = 41990
	o.EffectiveCents = 410000

	got := formatDigest(store.Watch{Query: "ps5"}, nil, []store.WatchOffer{o}, 1, nil)
	if !strings.Contains(got, `*R$ 4\.100,00*`) {
		t.Errorf("cash mode did not lead with the cash price:\n%s", got)
	}
	if !strings.Contains(got, `ou 12x R$ 419,90 \(total R$ 5\.038,80\)`) {
		t.Errorf("cash mode did not offer the plan with its total:\n%s", got)
	}
	if strings.Contains(got, "à vista") {
		t.Errorf("cash mode restated the price it already leads with:\n%s", got)
	}
	if strings.Contains(got, "por total parcelado") {
		t.Errorf("cash mode claimed the parcelado ordering:\n%s", got)
	}
}

// The detail view names the figure it is showing, so a switched watch does not
// look like a price change.
func TestFormatDetailNamesThePriceMode(t *testing.T) {
	row := watchRow{
		Watch: store.Watch{ID: 1, Query: "ps5", Active: true, PriceMode: store.ModeInstallment},
		Stats: store.WatchStats{Products: 3, BestCents: 503880},
		Best: store.WatchOffer{
			PriceCents:           410000,
			InstallmentCount:     12,
			InstallmentEachCents: 41990,
			EffectiveCents:       503880,
		},
	}

	got := formatDetail(row)
	if !strings.Contains(got, `Melhor parcelado: *R$ 5\.038,80*`) {
		t.Errorf("detail view did not name the parcelado figure:\n%s", got)
	}
	if !strings.Contains(got, `ou R$ 4\.100,00 à vista`) {
		t.Errorf("detail view omitted the cash option:\n%s", got)
	}

	row.Watch.PriceMode = store.ModeCash
	row.Stats.BestCents = 410000
	row.Best.EffectiveCents = 410000
	if cash := formatDetail(row); !strings.Contains(cash, "Melhor agora") {
		t.Errorf("cash mode detail view:\n%s", cash)
	}
}

// The button says what tapping it will do, not what the watch currently is.
func TestDetailKeyboardModeButtonNamesTheSwitch(t *testing.T) {
	for mode, want := range map[store.PriceMode]string{
		store.ModeCash:        "💳 Parcelado",
		store.ModeInstallment: "💵 À vista",
	} {
		kb := detailKeyboard(watchRow{Watch: store.Watch{ID: 1, PriceMode: mode, Active: true}}, 0)
		var found bool
		for _, row := range kb.InlineKeyboard {
			for _, b := range row {
				if b.Text == want {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("mode %q: no button labelled %q", mode, want)
		}
	}
}
