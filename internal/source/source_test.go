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
