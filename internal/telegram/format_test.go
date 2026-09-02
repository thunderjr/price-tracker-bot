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
var markdownReserved = []string{"_", "*", "[", "]", "(", ")", "~", "`", ">", "#", "+", "-", "=", "|", "{", "}", ".", "!"}

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
	if n := strings.Count(got, "https://"); n != 3 {
		t.Errorf("digest carries %d links, want 3:\n%s", n, got)
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
	if bad := unescapedOutsideLinks(t, helpText); len(bad) > 0 {
		t.Errorf("helpText has unescaped %v:\n%s", bad, helpText)
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

	got := formatDigest(store.Watch{Query: "ps5"}, nil, []store.WatchOffer{offer}, 1, nil)
	if !strings.Contains(got, "ou 10x R$ 449,90") {
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
func TestInstallmentLineOmittedWhenAbsent(t *testing.T) {
	for name, o := range map[string]store.WatchOffer{
		"no plan":     {PriceCents: 418406},
		"one payment": {PriceCents: 418406, InstallmentCount: 1, InstallmentEachCents: 418406},
		"zero amount": {PriceCents: 418406, InstallmentCount: 10},
	} {
		if got := installmentLine(o); got != "" {
			t.Errorf("%s: installmentLine = %q, want empty", name, got)
		}
	}
}

// When the instalments add up to no more than the cash price there is no total
// worth spelling out.
func TestInstallmentLineOmitsRedundantTotal(t *testing.T) {
	o := store.WatchOffer{PriceCents: 449900, InstallmentCount: 10, InstallmentEachCents: 44990}
	got := installmentLine(o)
	if !strings.Contains(got, "ou 10x R$ 449,90") {
		t.Errorf("installmentLine = %q", got)
	}
	if strings.Contains(got, "total") {
		t.Errorf("installmentLine = %q, want no total when it equals the price", got)
	}
}
