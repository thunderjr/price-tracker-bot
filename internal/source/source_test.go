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

// A target typed with a decimal point read as thousands: "3499.90" became
// R$ 349.990,00, and the target alert then fired on every listing forever.
// The marketplaces' own "4.184" must keep meaning four thousand.
func TestParseBRLDecimalPoint(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"3499.90", 349990},
		{"3499.9", 349990}, // a thousands group is always three digits
		{"0.50", 50},
		{"4.184", 418400},
		{"1.234.567,89", 123456789},
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
		in   string
		want InstallmentPlan
	}{
		// Amazon
		{"em até 10x de R$ 449,90 sem juros", InstallmentPlan{Count: 10, Each: 44990, Interest: InterestFree}},
		{"em até 12x de R$ 383,32 sem juros", InstallmentPlan{Count: 12, Each: 38332, Interest: InterestFree}},
		// Interest is stated, and costs real money: 12 x 11,08 is R$ 132,96
		// on an item priced R$ 118,72.
		{"em até 12x de R$ 11,08 com juros", InstallmentPlan{Count: 12, Each: 1108, Interest: InterestCharged}},
		// Mercado Livre
		{"12x R$ 459,99 sem juros", InstallmentPlan{Count: 12, Each: 45999, Interest: InterestFree}},
		{"12x de R$ 459,99 sem juros", InstallmentPlan{Count: 12, Each: 45999, Interest: InterestFree}},
		{
			"ou R$ 4.599,90 em 10x R$ 459,99 sem juros",
			InstallmentPlan{Count: 10, Each: 45999, Total: 459990, Interest: InterestFree},
		},
		// The other-payment figure is its own thing, not a plan.
		{"ou R$ 5.499 em outros meios", InstallmentPlan{OtherMeansCents: 549900}},
		{"ou R$ 749 em outros meios", InstallmentPlan{OtherMeansCents: 74900}},
		// No wording means unknown, never assumed free.
		{"10x R$ 449,90", InstallmentPlan{Count: 10, Each: 44990, Interest: InterestUnknown}},
		// Nothing to read
		{"", InstallmentPlan{}},
		{"frete grátis", InstallmentPlan{}},
	} {
		if got := ParseInstallments(tc.in); got != tc.want {
			t.Errorf("ParseInstallments(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

// A financed total shown as a struck-through "was" price has to be recognized
// whichever way the site quotes it.
func TestIsInstallmentTotalCoversOtherMeans(t *testing.T) {
	plan := ParseInstallments("ou R$ 5.499 em outros meios")
	if !plan.IsInstallmentTotal(549900) {
		t.Error("the other-payment total was not recognized as one")
	}
	if plan.IsInstallmentTotal(519900) {
		t.Error("an unrelated figure matched the other-payment total")
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

// Mercado Livre writes "sem juros" only when a plan is interest-free and says
// nothing at all when it is not, so silence plus a total above the cash price
// is interest. Showing that plan unlabelled reads as if it were free.
func TestResolveInterestFromArithmetic(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plan  InstallmentPlan
		price int64
		want  Interest
	}{{
		name:  "unlabelled plan costing more is charging interest",
		plan:  InstallmentPlan{Count: 12, Each: 44200},
		price: 459900, // 12x442,00 = 5.304,00
		want:  InterestCharged,
	}, {
		name:  "cent rounding is not interest",
		plan:  InstallmentPlan{Count: 10, Each: 45999},
		price: 459900, // 10x459,99 = 4.599,90
		want:  InterestUnknown,
	}, {
		name:  "a stated wording is never overridden",
		plan:  InstallmentPlan{Count: 12, Each: 44200, Interest: InterestFree},
		price: 459900,
		want:  InterestFree,
	}, {
		name:  "instalments at or below the cash price stay unknown",
		plan:  InstallmentPlan{Count: 10, Each: 41840},
		price: 418406,
		want:  InterestUnknown,
	}, {
		name:  "a single payment is not a plan",
		plan:  InstallmentPlan{Count: 1, Each: 999900},
		price: 418406,
		want:  InterestUnknown,
	}, {
		name:  "no price to compare against",
		plan:  InstallmentPlan{Count: 12, Each: 44200},
		price: 0,
		want:  InterestUnknown,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.plan.ResolveInterest(tc.price).Interest; got != tc.want {
				t.Errorf("ResolveInterest(%d) = %q, want %q", tc.price, got, tc.want)
			}
		})
	}
}

// ResolveInterestAgainst reads the wording off a published reference price, for
// a source that never writes "juros" at all. Kabum's plans total its card
// price exactly, so they are free and the gap to the cash price is a Pix
// discount -- the opposite conclusion to the one the cash price would give.
func TestResolveInterestAgainstAReferencePrice(t *testing.T) {
	cases := []struct {
		name string
		plan InstallmentPlan
		ref  int64
		want Interest
	}{
		{
			name: "plan totalling the card price is free",
			plan: InstallmentPlan{Count: 10, Each: 44990},
			ref:  449900,
			want: InterestFree,
		},
		{
			name: "rounding in the published total is still free",
			plan: InstallmentPlan{Count: 10, Each: 42094},
			ref:  420947,
			want: InterestFree,
		},
		{
			name: "plan above the card price is charging interest",
			plan: InstallmentPlan{Count: 12, Each: 41990},
			ref:  449900,
			want: InterestCharged,
		},
		{
			name: "a stated wording is never overridden",
			plan: InstallmentPlan{Count: 12, Each: 41990, Interest: InterestFree},
			ref:  449900,
			want: InterestFree,
		},
		{
			name: "no reference price leaves it unknown",
			plan: InstallmentPlan{Count: 10, Each: 44990},
			ref:  0,
			want: InterestUnknown,
		},
		{
			name: "a single payment is not a plan",
			plan: InstallmentPlan{Count: 1, Each: 449900},
			ref:  449900,
			want: InterestUnknown,
		},
		{
			name: "a plan cheaper than the card price stays unknown",
			plan: InstallmentPlan{Count: 10, Each: 30000},
			ref:  449900,
			want: InterestUnknown,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.plan.ResolveInterestAgainst(c.ref).Interest; got != c.want {
				t.Errorf("interest = %q, want %q", got, c.want)
			}
		})
	}
}
