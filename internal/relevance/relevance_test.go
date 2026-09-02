package relevance

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/thunderjr/price-tracker-bot/internal/source"
)

// The fixtures are real search results captured from both marketplaces, and
// the .labels files next to them are hand-labelled: which listings actually
// are the product being searched for, and which are accessories, games,
// knock-offs or a different device entirely.
//
// This is the accuracy check. It reports precision and recall rather than
// asserting an exact set, because these heuristics are never going to be
// perfect -- what matters is that they do not get worse, and above all that
// they do not throw away real listings.
type labelled struct {
	query  string
	offers []source.Offer
	want   map[string]bool // external id -> keep
	note   map[string]string
}

func load(t *testing.T, base, query string) labelled {
	t.Helper()

	raw, err := os.ReadFile("testdata/" + base + ".json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var offers []source.Offer
	if err := json.Unmarshal(raw, &offers); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}

	f, err := os.Open("testdata/" + base + ".labels")
	if err != nil {
		t.Fatalf("read labels: %v", err)
	}
	defer f.Close()

	want := map[string]bool{}
	note := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			t.Fatalf("bad label line: %q", line)
		}
		want[fields[1]] = fields[0] == "keep"
		note[fields[1]] = strings.Join(fields[2:], " ")
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	// A search can return the same listing twice, so labels are keyed by id
	// and compared against the unique set.
	unique := map[string]bool{}
	for _, o := range offers {
		unique[o.ExternalID] = true
	}
	if len(want) != len(unique) {
		t.Fatalf("%s: %d unique offers but %d labels", base, len(unique), len(want))
	}
	return labelled{query: query, offers: offers, want: want, note: note}
}

// score runs the filter and reports how it did against the labels.
func (l labelled) score(t *testing.T, opts Options) (falseDrops, falseKeeps []string) {
	t.Helper()

	kept, report := Filter(l.query, l.offers, opts)

	keptIDs := map[string]bool{}
	for _, o := range kept {
		keptIDs[o.ExternalID] = true
	}
	droppedBy := map[string]string{}
	for _, o := range l.offers {
		if !keptIDs[o.ExternalID] {
			droppedBy[o.ExternalID] = ""
		}
	}
	for _, d := range report.Drops {
		for _, o := range l.offers {
			if o.Title == d.Title {
				droppedBy[o.ExternalID] = string(d.Reason) + ":" + d.Match
			}
		}
	}

	for id, shouldKeep := range l.want {
		switch {
		case shouldKeep && !keptIDs[id]:
			falseDrops = append(falseDrops, id+" ("+l.note[id]+") dropped by "+droppedBy[id])
		case !shouldKeep && keptIDs[id]:
			falseKeeps = append(falseKeeps, id+" ("+l.note[id]+")")
		}
	}
	return falseDrops, falseKeeps
}

// falseDropBudget names the real listings each fixture is knowingly allowed to
// lose, and why. Everything else failing here is a regression.
//
// Dropping a real listing is the serious failure -- it vanishes silently from
// a price tracker -- so the budget is spelled out per listing rather than
// given as a number.
var falseDropBudget = map[string]map[string]string{
	"meli-lego": {
		// Both are Spanish-titled listings of sets the query names in English:
		// "Halcon Milenario" carries neither "millennium" nor "falcon", so the
		// coverage rule cannot see them. Each is a duplicate of a set that
		// stays covered by other listings in the same result set (75375 has a
		// dozen, 75030 has MLB2064513420), so the price history survives.
		// The rule earns this by removing listings that share only the word
		// "lego" with the query -- see the LEGO Olivia Rodrigo fixture.
		"MLB2070877844": "Spanish title: 'Halcon Milenary', set 75375 covered elsewhere",
		"MLB2022660510": "Spanish title: 'Halcon Milenario', set 75030 covered elsewhere",
	},
	"meli-olivia": {
		// Titled only "Lego Botanicals Buque De Flores 11507" -- the set is an
		// Olivia Rodrigo one, but the title never says so, so half the query
		// is missing. Set 11507 stays covered by the listings that do name it.
		"MLB5147183169": "title omits 'Olivia Rodrigo', set 11507 covered elsewhere",
		"MLB7285016910": "title omits 'Olivia Rodrigo', set 11507 covered elsewhere",
	},
}

func TestNoUnbudgetedFalseDrops(t *testing.T) {
	for _, tc := range []struct{ base, query string }{
		{"amazon-ps5", "ps5"},
		{"meli-ps5", "ps5"},
		{"kabum-ps5", "ps5"},
		{"meli-lego", "lego millennium falcon"},
		{"meli-olivia", "LEGO Olivia Rodrigo"},
	} {
		t.Run(tc.base, func(t *testing.T) {
			l := load(t, tc.base, tc.query)
			falseDrops, falseKeeps := l.score(t, Options{})

			budget := falseDropBudget[tc.base]
			for _, d := range falseDrops {
				id := strings.Fields(d)[0]
				if reason, ok := budget[id]; ok {
					t.Logf("known loss %s: %s", id, reason)
					continue
				}
				t.Errorf("dropped a real listing: %s", d)
			}

			shouldDrop := 0
			for _, keep := range l.want {
				if !keep {
					shouldDrop++
				}
			}
			caught := shouldDrop - len(falseKeeps)
			t.Logf("%s: %d offers, %d junk, caught %d (%d%%), %d real listings lost",
				tc.base, len(l.offers), shouldDrop, caught, pct(caught, shouldDrop), len(falseDrops))
		})
	}
}

// Recall floors, so a change that quietly stops catching accessories fails
// here rather than in production. These are deliberately set at what the
// heuristics genuinely achieve, not at what would be nice.
func TestRecallFloors(t *testing.T) {
	for _, tc := range []struct {
		base, query string
		minCaught   int
	}{
		// Amazon's junk for "ps5" is mostly games, which no title heuristic
		// separates from a console safely. Accessories are the catchable part.
		{"amazon-ps5", "ps5", 15},
		{"meli-ps5", "ps5", 0},
		// KaBuM! is the noisiest source for this query -- 14 of its 60 results
		// are consoles -- and the least catchable by title, because it names
		// the bundled controller in the console's own title ("Console Sony
		// PlayStation 5, SSD 825GB, Controle Sem Fio DualSense"). So
		// "controle" cannot be an accessory noun without dropping real
		// consoles, and its 33 controllers and games survive the title rules.
		// The price bound is what separates them: see
		// TestPriceFloorSeparatesCategoriesOnKabum.
		{"kabum-ps5", "ps5", 10},
		{"meli-lego", "lego millennium falcon", 16},
		// The case the coverage rule exists for: a specific query answered
		// with loosely related stock that shares only the brand name.
		{"meli-olivia", "LEGO Olivia Rodrigo", 21},
	} {
		t.Run(tc.base, func(t *testing.T) {
			l := load(t, tc.base, tc.query)
			_, falseKeeps := l.score(t, Options{})

			shouldDrop := 0
			for _, keep := range l.want {
				if !keep {
					shouldDrop++
				}
			}
			caught := shouldDrop - len(falseKeeps)
			if caught < tc.minCaught {
				t.Errorf("caught %d of %d junk listings, want at least %d\nstill kept:\n  %s",
					caught, shouldDrop, tc.minCaught, strings.Join(falseKeeps, "\n  "))
			}
		})
	}
}

// The price bound is how a user separates a console from its games, which no
// title heuristic can do. Check it actually closes that gap.
func TestPriceFloorSeparatesCategories(t *testing.T) {
	l := load(t, "amazon-ps5", "ps5")
	falseDrops, falseKeeps := l.score(t, Options{MinCents: 300000})

	for _, d := range falseDrops {
		t.Errorf("min price dropped a real console: %s", d)
	}
	if len(falseKeeps) > 1 {
		t.Errorf("min price left %d junk listings: %v", len(falseKeeps), falseKeeps)
	}
	t.Logf("with min R$ 3.000: %d junk listings survive", len(falseKeeps))
}

// KaBuM! is where the price bound earns its keep. Its titles hide 33 of the 36
// junk listings from every title heuristic, and the floor the live watch
// already carries removes all of them without touching a console.
func TestPriceFloorSeparatesCategoriesOnKabum(t *testing.T) {
	l := load(t, "kabum-ps5", "ps5")
	falseDrops, falseKeeps := l.score(t, Options{MinCents: 300000})

	for _, d := range falseDrops {
		t.Errorf("min price dropped a real console: %s", d)
	}
	if len(falseKeeps) > 0 {
		t.Errorf("min price left %d junk listings: %v", len(falseKeeps), falseKeeps)
	}
}

func pct(n, total int) int {
	if total == 0 {
		return 100
	}
	return n * 100 / total
}

// SuggestFloor must speak up exactly where a price rule would have been right,
// and stay silent everywhere else. Silence is the safe answer: a wrong
// suggestion pushes the user into throwing away real listings.
func TestSuggestFloor(t *testing.T) {
	for _, tc := range []struct {
		base, query string
		want        bool
		reason      string
	}{
		{"amazon-ps5", "ps5", true,
			"consoles from R$4.184 sit far above games and stands topping out at R$882"},
		{"meli-lego", "lego millennium falcon", false,
			"LEGO sets run continuously from an R$84 polybag to a R$14.999 UCS model"},
		{"meli-ps5", "ps5", false,
			"Mercado Livre returned consoles only; there is no second group"},
		{"kabum-ps5", "ps5", false,
			"KaBuM! fills the gap Amazon leaves: R$1.394 Edge controllers and R$1.899 Portals sit between the games and the R$3.999 consoles"},
	} {
		t.Run(tc.base, func(t *testing.T) {
			l := load(t, tc.base, tc.query)
			kept, _ := Filter(tc.query, l.offers, Options{})

			floor, ok := SuggestFloor(kept)
			if ok != tc.want {
				t.Fatalf("SuggestFloor = (%s, %v), want ok=%v because %s",
					source.FormatBRL(floor), ok, tc.want, tc.reason)
			}
			if !ok {
				return
			}

			// A suggestion the user accepts must not cost them a real listing.
			var lostReal, cutJunk int
			for _, o := range kept {
				if o.PriceCents >= floor {
					continue
				}
				if l.want[o.ExternalID] {
					lostReal++
					t.Errorf("floor %s would drop a real listing: %s (%s)",
						source.FormatBRL(floor), o.Title, source.FormatBRL(o.PriceCents))
				} else {
					cutJunk++
				}
			}
			t.Logf("%s: suggest %s, cuts %d junk, loses %d real",
				tc.base, source.FormatBRL(floor), cutJunk, lostReal)
		})
	}
}

func TestSuggestFloorNeedsEnoughData(t *testing.T) {
	few := []source.Offer{{PriceCents: 100}, {PriceCents: 1000000}}
	if _, ok := SuggestFloor(few); ok {
		t.Error("suggested a floor from two data points")
	}
	if _, ok := SuggestFloor(nil); ok {
		t.Error("suggested a floor from nothing")
	}
}

// Mercado Livre sometimes returns the same listing in two cards of one search.
// Recording it twice would double-count it and corrupt the price history.
func TestFilterDeduplicates(t *testing.T) {
	offers := []source.Offer{
		{ExternalID: "MLB1", Title: "Console PlayStation 5", PriceCents: 400000},
		{ExternalID: "MLB2", Title: "Console PlayStation 5 Slim", PriceCents: 450000},
		{ExternalID: "MLB1", Title: "Console PlayStation 5", PriceCents: 400000},
	}

	kept, report := Filter("ps5", offers, Options{})
	if len(kept) != 2 {
		t.Fatalf("kept %d offers, want 2", len(kept))
	}
	if report.Count(ReasonDuplicate) != 1 {
		t.Errorf("duplicate count = %d, want 1", report.Count(ReasonDuplicate))
	}
}

// Searching for the accessory itself must return accessories. Otherwise a
// watch on "suporte ps5" would come back permanently empty.
func TestFilterKeepsWhatTheQueryAsksFor(t *testing.T) {
	for _, tc := range []struct{ query, title string }{
		{"suporte ps5", "Suporte Vertical Playstation 5 Ps5 Em Metal"},
		{"capa ps5", "Capa Protetora contra Poeira para PS5"},
		{"kit de iluminacao lego millennium falcon", "Kit De Iluminação Led Para Lego Millennium Falcon"},
		{"headset ps5", "NUBWO Wireless Gaming Headset with Mic for Ps5"},
	} {
		offers := []source.Offer{{ExternalID: "X", Title: tc.title, PriceCents: 10000}}
		kept, report := Filter(tc.query, offers, Options{})
		if len(kept) != 1 {
			t.Errorf("query %q dropped its own subject %q: %v", tc.query, tc.title, report.Drops)
		}
	}
}

func TestNormalize(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Kit De Iluminação", "kit de iluminacao"},
		{"PlayStation®5 Slim", "playstation 5 slim"},
		{"Suporte  —  PS5", "suporte ps5"},
		{"CAPA Protetora", "capa protetora"},
	} {
		if got := normalize(tc.in); got != tc.want {
			t.Errorf("normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Word boundaries matter: "case" must not fire on "showcase".
func TestContainsWord(t *testing.T) {
	for _, tc := range []struct {
		s, word string
		want    bool
	}{
		{"capa protetora", "capa", true},
		{"capacete de moto", "capa", false},
		{"lego showcase display", "case", false},
		{"hard case rigido", "case", true},
		{"suporte", "suporte", true},
	} {
		if got := containsWord(tc.s, tc.word); got != tc.want {
			t.Errorf("containsWord(%q, %q) = %v, want %v", tc.s, tc.word, got, tc.want)
		}
	}
}

// Titles taken verbatim from listings that a live "ps5 slim" watch had
// accumulated -- the ones that made it useless. Each has to be recognized.
func TestFilterCatchesLiveJunk(t *testing.T) {
	for _, title := range []string{
		"Stand Mesa Base Vertical Playstation 5 Ps5 Slim Fat Todos",
		"Base Stand Vertical Compativel C/Playstation 5 Ps5 Slim Fat",
		"Suporte Horizontal Compatível Com Playstation 5 Slim Digital",
		"Suporte De Parede Painel Compatível Com Playstation 5 Slim",
		"Suporte Vertical para PS5/PS5 Slim - Base Antiderrapante",
		"Base Suporte Cooler com Dock Carregador de Controle e Led RGB",
		"Base Cooler Suporte Multifuncional Resfriamento Compativel",
		"Base de Carregamento PS5, Slim, Pro 6 em 1 para Gamers",
		"Base de Resfriamento para PS5, PS5 Pro e PS5 Slim",
		"Ps5 Slim Console Cooling Station, Ps5 Slim(Disc & Digital Edition)",
		"Tampas do console PlayStation®5 Slim - Midnight Black",
		"Unidade de disco para consoles PS5® Digital (PS5 Slim e Pro)",
		"PlayStation 5 Slim Ultra HD Blu-ray Disc Drive, Black [video game]",
		"Para Console Ps5 Slim De Fácil Instalação",
		"Para Console Ps5 Slim Uhd, Console De Jogos Optical Drive Ed",
	} {
		offers := []source.Offer{{ExternalID: "X", Title: title, PriceCents: 5000}}
		kept, _ := Filter("ps5 slim", offers, Options{})
		if len(kept) != 0 {
			t.Errorf("kept an accessory: %q", title)
		}
	}
}

// The consoles that sat alongside that junk must all survive.
func TestFilterKeepsLiveConsoles(t *testing.T) {
	for _, title := range []string{
		"Console Playstation®5 Slim Digital - Pacote Astro Bot E Gran Turismo 7",
		"Console PlayStation5 Slim Disk - Pacote ASTRO BOT e Gran Turismo 7",
		"Sony PlayStation 5 Slim 1TB cor branco",
		"Playstation 5 Slim Digital 825gb Branco Novo Cfi-2114 2 Controles",
		"Console Playstation 5 Slim Fisico -2 Controles Ps5 Nacional Branco",
		"Sony PlayStation 5 Slim CFI-2015B 825GB Digital cor branco 2023",
		"Console Playstation 5 Slim Edição Digital 825 Gb",
		"PlayStation®5 Pro",
		"Console PS5 c/Drive Slim 1 TB c/ 1 Controle + EA FC 26",
	} {
		offers := []source.Offer{{ExternalID: "X", Title: title, PriceCents: 450000}}
		kept, report := Filter("ps5 slim", offers, Options{})
		if len(kept) != 1 {
			t.Errorf("dropped a console: %q (%v)", title, report.Drops)
		}
	}
}

// A cross-border listing's price excludes Brazilian import tax, so it is not
// what you pay -- and being artificially low, it wins every "cheapest offer"
// comparison and hides the real best price.
func TestFilterDropsInternational(t *testing.T) {
	t.Run("flagged by the marketplace", func(t *testing.T) {
		offers := []source.Offer{
			{ExternalID: "A", Title: "Console PlayStation 5 Slim", PriceCents: 450000},
			{ExternalID: "B", Title: "Console PlayStation 5 Slim", PriceCents: 380000, International: true},
		}
		kept, report := Filter("ps5 slim", offers, Options{})
		if len(kept) != 1 || kept[0].ExternalID != "A" {
			t.Fatalf("kept %v, want only the domestic listing", kept)
		}
		if report.Count(ReasonInternational) != 1 {
			t.Errorf("international count = %d, want 1", report.Count(ReasonInternational))
		}
	})

	// Amazon has no structural marker, only the title.
	t.Run("declared in the title", func(t *testing.T) {
		for _, title := range []string{
			"Sony PlayStation 5 Slim Digital Console 825GB - International Version",
			"Console PlayStation 5 Global Version",
			"PlayStation 5 Slim - Versão Internacional",
			"Console PS5 Produto Importado",
		} {
			offers := []source.Offer{{ExternalID: "X", Title: title, PriceCents: 380000}}
			if kept, _ := Filter("ps5", offers, Options{}); len(kept) != 0 {
				t.Errorf("kept an international listing: %q", title)
			}
		}
	})

	t.Run("opt back in", func(t *testing.T) {
		offers := []source.Offer{
			{ExternalID: "B", Title: "Console PlayStation 5 Slim", PriceCents: 380000, International: true},
		}
		if kept, _ := Filter("ps5 slim", offers, Options{AllowInternational: true}); len(kept) != 1 {
			t.Error("AllowInternational did not keep the listing")
		}
	})
}

// Domestic listings must not be mistaken for imports. "Nacional" and a
// 国-free title are exactly the ones to keep.
func TestFilterKeepsDomestic(t *testing.T) {
	for _, title := range []string{
		"Console Playstation 5 Slim Fisico -2 Controles Ps5 Nacional Branco",
		"Sony PlayStation 5 Slim 1TB cor branco",
		"Console PlayStation 5 - Digital Edition",
		"PlayStation®5 Slim Edição Digital com 2 Jogos",
	} {
		offers := []source.Offer{{ExternalID: "X", Title: title, PriceCents: 450000}}
		kept, report := Filter("ps5", offers, Options{})
		if len(kept) != 1 {
			t.Errorf("dropped a domestic listing %q: %v", title, report.Drops)
		}
	}
}

// The coverage rule stands down whenever a query is worded differently from
// the listings, because it cannot tell that apart from genuine noise. Required
// terms are how the user settles it, and they must be exact.
func TestFilterRequiresTerms(t *testing.T) {
	l := load(t, "meli-olivia", "LEGO Olivia Rodrigo")

	kept, report := Filter("LEGO Olivia Rodrigo", l.offers, Options{Require: []string{"rodrigo"}})
	for _, o := range kept {
		if !strings.Contains(normalize(o.Title), "rodrigo") {
			t.Errorf("kept a listing without the required term: %q", o.Title)
		}
	}
	if report.Count(ReasonMissingTerm) == 0 {
		t.Error("no listing was dropped for missing the required term")
	}
	t.Logf("require rodrigo: %d of %d kept", len(kept), len(l.offers))

	// Every required term has to be present, not just one of them.
	both, _ := Filter("LEGO Olivia Rodrigo", l.offers, Options{Require: []string{"olivia", "rodrigo"}})
	for _, o := range both {
		title := normalize(o.Title)
		if !strings.Contains(title, "olivia") || !strings.Contains(title, "rodrigo") {
			t.Errorf("kept a listing missing one of the required terms: %q", o.Title)
		}
	}

	// Accents and case must not matter.
	acc, _ := Filter("x", []source.Offer{{ExternalID: "A", Title: "Buquê de Flores", PriceCents: 100}},
		Options{Require: []string{"BUQUE"}})
	if len(acc) != 1 {
		t.Error("required term did not match across accents and case")
	}
}

// Accessories that name what they are for mid-title, where the
// leading-qualifier rule cannot see them.
func TestFilterCatchesMidTitleQualifiers(t *testing.T) {
	for _, title := range []string{
		"YEABRICKS LED Light for Lego-43029 Editions Olivia Rodrigo's Concert Moon",
		"YEABRICKS LED Light for Lego-43031 Editions Olivia Rodrigo's Guitar",
		"Briksmax Light Kit For Lego Millennium Falcon",
		"Kit De Luz Briksmax Para Lego Millennium Falcon",
	} {
		offers := []source.Offer{{ExternalID: "X", Title: title, PriceCents: 20000}}
		if kept, _ := Filter("lego olivia rodrigo", offers, Options{}); len(kept) != 0 {
			t.Errorf("kept a lighting kit: %q", title)
		}
	}

	// The set itself must survive.
	for _, title := range []string{
		"LEGO Editions Lua do Show de Olivia Rodrigo 43029",
		"LEGO Botanicals Buquê de flores da Olivia Rodrigo 11507",
	} {
		offers := []source.Offer{{ExternalID: "X", Title: title, PriceCents: 40000}}
		if kept, r := Filter("lego olivia rodrigo", offers, Options{}); len(kept) != 1 {
			t.Errorf("dropped a real set %q: %v", title, r.Drops)
		}
	}
}
