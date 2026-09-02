package source

import "testing"

func TestParseBRL(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"R$ 4.184,06", 418406},
		{"R$4.184,06", 418406},
		{"4.184", 418400},
		{"4184,06", 418406},
		{"R$ 349,91", 34991},
		{"R$ 4,99", 499},
		{"R$ 1.234.567,89", 123456789},
		{"99", 9900},
		{"R$ 10,5", 1050},
		{"", 0},
		{"R$", 0},
		{"grátis", 0},
	} {
		if got := ParseBRL(tc.in); got != tc.want {
			t.Errorf("ParseBRL(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestFormatBRL(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{418406, "R$ 4.184,06"},
		{34991, "R$ 349,91"},
		{123456789, "R$ 1.234.567,89"},
		{500, "R$ 5,00"},
		{5, "R$ 0,05"},
		{0, "R$ 0,00"},
	} {
		if got := FormatBRL(tc.in); got != tc.want {
			t.Errorf("FormatBRL(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatBRLRoundTrip(t *testing.T) {
	for _, cents := range []int64{1, 99, 100, 418406, 123456789} {
		if got := ParseBRL(FormatBRL(cents)); got != cents {
			t.Errorf("round trip %d -> %q -> %d", cents, FormatBRL(cents), got)
		}
	}
}

func TestDiscount(t *testing.T) {
	for _, tc := range []struct {
		price, list int64
		want        int
	}{
		{418406, 449900, 7},
		{404700, 459900, 12},
		{100000, 0, 0},     // no reference price
		{100000, 90000, 0}, // reference below price: not a discount
		{0, 449900, 0},     // no price
	} {
		o := Offer{PriceCents: tc.price, ListPriceCents: tc.list}
		if got := o.Discount(); got != tc.want {
			t.Errorf("Discount(%d, %d) = %d, want %d", tc.price, tc.list, got, tc.want)
		}
	}
}

func TestParseInstallments(t *testing.T) {
	for _, tc := range []struct {
		in    string
		count int
		each  int64
		total int64
	}{
		// Amazon
		{"em até 10x de R$ 449,90 sem juros", 10, 44990, 0},
		{"em até 12x de R$ 383,32 sem juros", 12, 38332, 0},
		// Mercado Livre
		{"12x R$ 459,99 sem juros", 12, 45999, 0},
		{"ou R$ 4.599,90 em 10x R$ 459,99 sem juros", 10, 45999, 459990},
		{"ou R$ 5.499 em outros meios", 0, 0, 549900},
		{"ou R$ 749 em outros meios", 0, 0, 74900},
		// Nothing to read
		{"", 0, 0, 0},
		{"frete grátis", 0, 0, 0},
	} {
		got := ParseInstallments(tc.in)
		if got.Count != tc.count || got.Each != tc.each || got.Total != tc.total {
			t.Errorf("ParseInstallments(%q) = {count:%d each:%d total:%d}, want {count:%d each:%d total:%d}",
				tc.in, got.Count, got.Each, got.Total, tc.count, tc.each, tc.total)
		}
	}
}

func TestInstallmentPlanTotalCents(t *testing.T) {
	for _, tc := range []struct {
		plan InstallmentPlan
		want int64
	}{
		{InstallmentPlan{Count: 10, Each: 44990}, 449900},
		{InstallmentPlan{Total: 549900}, 549900},
		{InstallmentPlan{Count: 10, Each: 44990, Total: 459990}, 459990}, // stated wins
		{InstallmentPlan{Count: 1, Each: 44990}, 0},                      // one "instalment" is just the price
		{InstallmentPlan{}, 0},
	} {
		if got := tc.plan.TotalCents(); got != tc.want {
			t.Errorf("TotalCents(%+v) = %d, want %d", tc.plan, got, tc.want)
		}
	}
}

// The check that stops a payment-method gap being reported as a discount.
func TestIsInstallmentTotal(t *testing.T) {
	// Amazon: R$ 4.184,06 cash, "R$ 4.499,00" struck, "em até 10x de R$ 449,90".
	amazon := ParseInstallments("em até 10x de R$ 449,90 sem juros")
	if !amazon.IsInstallmentTotal(449900) {
		t.Error("R$ 4.499,00 not recognized as 10 x R$ 449,90")
	}

	// Mercado Livre displays the total without cents, so the arithmetic is
	// 90 centavos out and still has to match.
	meli := ParseInstallments("ou R$ 4.599 em 10x R$ 459,99 sem juros")
	if !meli.IsInstallmentTotal(459900) {
		t.Error("R$ 4.599 not recognized as 10 x R$ 459,99 despite rounding")
	}

	// A real discount must survive: R$ 4.899 before, R$ 4.599 now, financed
	// at 12 x R$ 383,25 of the *current* price.
	genuine := ParseInstallments("12x R$ 383,25 sem juros")
	if genuine.IsInstallmentTotal(489900) {
		t.Error("a genuine former price was mistaken for the financing total")
	}
	if !genuine.IsInstallmentTotal(459900) {
		t.Error("the financing total of the current price should still match")
	}

	// No plan, nothing to conclude.
	if (InstallmentPlan{}).IsInstallmentTotal(449900) {
		t.Error("claimed a match with no installment plan")
	}
	if amazon.IsInstallmentTotal(0) {
		t.Error("claimed a match against a zero price")
	}
}
