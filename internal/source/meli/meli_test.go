package meli

import "testing"

func TestSlug(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"ps5", "ps5"},
		{"PS5", "ps5"},
		{"LEGO Millennium Falcon", "lego-millennium-falcon"},
		{"  steam   deck  ", "steam-deck"},
		{"café expresso", "café-expresso"},
		{"iphone 15 pro max 256gb", "iphone-15-pro-max-256gb"},
		{"nintendo switch (oled)", "nintendo-switch-oled"},
	} {
		if got := Slug(tc.in); got != tc.want {
			t.Errorf("Slug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestProductID(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://www.mercadolivre.com.br/console-playstation5-slim/p/MLB57081243#polycard_client=search", "MLB57081243"},
		{"https://produto.mercadolivre.com.br/MLB-3771888163-console-ps5", "MLB3771888163"},
		{"https://www.mercadolivre.com.br/x/p/MLB29001054?wid=MLB7501959058", "MLB29001054"},
		{"https://www.mercadolivre.com.br/ofertas", ""},
		{"", ""},
	} {
		if got := ProductID(tc.in); got != tc.want {
			t.Errorf("ProductID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Card links carry a tracking fragment that changes on every scan. Keeping it
// would make the same product look new each time.
func TestCleanURL(t *testing.T) {
	in := "https://www.mercadolivre.com.br/console/p/MLB57081243#polycard_client=search-desktop&position=1&tracking_id=3b5efabf"
	want := "https://www.mercadolivre.com.br/console/p/MLB57081243"
	if got := CleanURL(in); got != want {
		t.Errorf("CleanURL = %q, want %q", got, want)
	}
}

func TestToOffer(t *testing.T) {
	r := rawOffer{
		Title:     "  Console  PlayStation®5 Slim  Digital ",
		URL:       "https://www.mercadolivre.com.br/x/p/MLB57081243#polycard_client=search",
		Price:     "4.047",
		ListPrice: "4.599",
		Seller:    "PlayStation",
		Rating:    "4.8",
		SiteFlags: []string{"12% OFF", "12% OFF", ""},
	}

	o, ok := r.toOffer()
	if !ok {
		t.Fatal("toOffer returned false")
	}
	if o.ExternalID != "MLB57081243" {
		t.Errorf("ExternalID = %q", o.ExternalID)
	}
	if o.Title != "Console PlayStation®5 Slim Digital" {
		t.Errorf("Title = %q", o.Title)
	}
	if o.PriceCents != 404700 || o.ListPriceCents != 459900 {
		t.Errorf("prices = %d/%d", o.PriceCents, o.ListPriceCents)
	}
	if o.Discount() != 12 {
		t.Errorf("Discount = %d, want 12", o.Discount())
	}
	if o.Rating != 4.8 {
		t.Errorf("Rating = %v", o.Rating)
	}
	if len(o.SiteFlags) != 1 || o.SiteFlags[0] != "12% OFF" {
		t.Errorf("SiteFlags = %v", o.SiteFlags)
	}
}

func TestToOfferRejectsUnusable(t *testing.T) {
	for name, r := range map[string]rawOffer{
		"no price": {Title: "x", URL: "https://x/p/MLB1", Price: ""},
		"no id":    {Title: "x", URL: "https://www.mercadolivre.com.br/ofertas", Price: "10"},
	} {
		if _, ok := r.toOffer(); ok {
			t.Errorf("%s: toOffer returned true", name)
		}
	}
}

// A "previous price" at or below the current price is noise, not a promotion.
func TestToOfferIgnoresNonDiscountListPrice(t *testing.T) {
	r := rawOffer{Title: "x", URL: "https://x/p/MLB1", Price: "100", ListPrice: "90"}
	o, ok := r.toOffer()
	if !ok {
		t.Fatal("toOffer returned false")
	}
	if o.ListPriceCents != 0 {
		t.Errorf("ListPriceCents = %d, want 0", o.ListPriceCents)
	}
}
