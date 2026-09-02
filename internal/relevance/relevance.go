// Package relevance drops the listings a search returns that are not the thing
// being tracked: accessories, spare parts and lookalikes.
//
// Deliberately absent: any automatic price-based rule. It is tempting, because
// a PS5 console costs ten times a PS5 stand, but it does not survive contact
// with real queries. "lego millennium falcon" legitimately returns the 74-piece
// polybag at R$84 and the 7541-piece UCS set at R$14.999, and every
// price-anchored variant tested (median, top-N median, 75th percentile) threw
// away one end or the other. Silently dropping a real listing from a price
// tracker is worse than showing one extra row, so price stays under the user's
// control via the min/max on a watch.
package relevance

import (
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/thunderjr/price-tracker-bot/internal/source"
)

// Reason explains why an offer was dropped.
type Reason string

const (
	// ReasonAccessory: the title describes something made *for* the product.
	ReasonAccessory Reason = "accessory"
	// ReasonExcluded: the watch's own exclusion list matched.
	ReasonExcluded Reason = "excluded"
	// ReasonPrice: outside the watch's min/max.
	ReasonPrice Reason = "price"
	// ReasonDuplicate: the same listing id came back twice in one search.
	ReasonDuplicate Reason = "duplicate"
	// ReasonInternational: a cross-border listing, whose price excludes
	// Brazilian import tax.
	ReasonInternational Reason = "international"
	// ReasonUnrelated: the title carries too few of the query's words to be
	// what was searched for.
	ReasonUnrelated Reason = "unrelated"
	// ReasonMissingTerm: a term the watch requires is absent.
	ReasonMissingTerm Reason = "missing"
)

// internationalRe catches cross-border listings that say so in the title.
// Mercado Livre also flags them structurally, which the meli source reads; on
// Amazon the title is the only signal there is.
var internationalRe = regexp.MustCompile(
	`\b(international version|global version|versao internacional|importado do exterior|produto importado|envio internacional|compra internacional)\b`)

// accessoryNouns name a thing sold alongside a product rather than the product.
// Matched as whole words against an accent-stripped, lowercased title.
//
// Every entry here has to be a noun that is never the product itself. "base"
// and "controle" are deliberately missing: "Console PS5 + 2 Controles" is a
// console, not a controller.
var accessoryNouns = []string{
	"suporte", "suportes", "stand", "stands",
	"capa", "capas", "capinha", "case", "estojo", "maleta",
	"skin", "adesivo", "adesivos", "pelicula", "protetor", "protetora",
	"cabo", "cabos", "carregador", "carregamento",
	"headset", "fone", "fones", "earbuds",
	"cooler", "cooling", "ventilador", "ventoinha", "ventoinhas", "resfriamento", "refrigeracao",
	"grip", "grips", "empunhadura",
	"volante", "pedaleira",
	"organizador", "suportes",
	"iluminacao", "luminaria",
	"dock", "hub",
	"minifigura", "minifiguras", "minifigure",
	"tampa", "tampas", "faceplate",
}

// Nouns deliberately left out, each because a real listing uses them:
//   base      - "base" appears in console model names
//   controle  - "Console PS5 + 2 Controles" is a console
//   bolsa     - "Bolsa Plastica Lego ... 30708" is the polybag set itself
//   kit       - "Kit De Construcao Lego" is a building set

// accessoryPhrases are multi-word markers of an add-on.
var accessoryPhrases = []string{
	// Bare "compativel" rather than "compativel com": listings write it as
	// "Compativel C/Playstation 5" just as often.
	"compativel",
	"compatible with",
	"kit de luz",
	"kit de luzes",
	"kit de iluminacao",
	"light kit",
	"lighting kit",
	// "YEABRICKS LED Light for Lego-43031" -- the qualifier is mid-title, so
	// the leading-qualifier rule never sees it.
	"led light for",
	"light for lego",
	"luz para lego",
	"unidade de disco",
	"disc drive",
	"optical drive",
	"estacao de carregamento",
	"charging station",
	"placa de expansao",
}

// leadingQualifier catches titles that open by naming what they are *for*:
// "Para Console Ps5 Slim De Facil Instalacao".
//
// Only the opening counts. A qualifier in the middle is normal for the real
// product too -- "Pacote Astro Bot da versao digital Slim para Sony
// PlayStation 5" is a console.
var leadingQualifier = regexp.MustCompile(`^(para|for|kit para|acessorio para) `)

// lookalike marks a clone sold under the brand's name: "Estilo lego ...".
var lookalike = regexp.MustCompile(`\b(estilo|tipo|similar a|inspirado em|nao oficial) (lego|playstation|nintendo|xbox|apple)\b`)

// knockoff marks unbranded building blocks sold against a branded set:
// "Bloco De Montar Star Wars", "Blocos Compativel Star Wars".
var knockoff = regexp.MustCompile(`\b(blocos?|modelo de blocos) (de )?(montar|compativel|compativeis)\b`)

// Query-coverage tuning. These only matter for a query specific enough to
// have several meaningful words.
const (
	// minCoverageTokens is the shortest query worth checking coverage on.
	// "ps5 slim" is two words and both are near-universal in its results;
	// there is nothing to discriminate with.
	minCoverageTokens = 3
	// coverageRatio is the share of query words a title must carry.
	coverageRatio = 0.5
	// onTargetRatio is how much of the result set must match the query
	// completely before coverage is trusted at all. If hardly anything is a
	// full match, the query is worded differently from the listings -- "ps5"
	// against titles that all say "PlayStation 5" -- and filtering on words
	// would throw away the real product.
	onTargetRatio = 0.5
	// minCoverageOffers is the smallest result set those ratios mean anything on.
	minCoverageOffers = 8
)

// stopwords carry no discriminating power in a pt-BR product title.
var stopwords = map[string]bool{
	"de": true, "da": true, "do": true, "das": true, "dos": true,
	"e": true, "com": true, "para": true, "em": true, "a": true, "o": true,
	"the": true, "of": true, "and": true, "for": true, "with": true,
}

// Options are the per-watch controls for the cases no heuristic should decide.
type Options struct {
	// Exclude drops any offer whose title contains one of these terms.
	Exclude []string
	// Require drops any offer whose title is missing one of these terms.
	//
	// This is the escape hatch for a marketplace that answers a specific query
	// with filler. The automatic coverage rule stands down when a query is
	// worded differently from the listings -- it cannot tell "Ps5 Slim" for
	// "Playstation 5 Slim" apart from "Lego Minecraft" for "LEGO Olivia
	// Rodrigo" -- and guessing wrong there costs real listings. Naming the
	// term settles it.
	Require []string
	// MinCents and MaxCents bound the price. Zero means unbounded.
	MinCents int64
	MaxCents int64
	// AllowInternational keeps cross-border listings. They are dropped by
	// default: the price shown excludes Brazilian import tax, so it is not
	// the price you pay, and it undercuts every domestic listing in a
	// "cheapest offer" comparison.
	AllowInternational bool
}

// Drop records one rejected offer.
type Drop struct {
	Source     string
	ExternalID string
	Title      string
	Price      int64
	Reason     Reason
	Match      string // the term or pattern that fired
}

// Report describes what a Filter call removed.
type Report struct {
	Drops []Drop
}

// Dropped is how many offers were removed.
func (r *Report) Dropped() int {
	if r == nil {
		return 0
	}
	return len(r.Drops)
}

// Count returns how many offers were dropped for a reason.
func (r *Report) Count(reason Reason) int {
	n := 0
	for _, d := range r.Drops {
		if d.Reason == reason {
			n++
		}
	}
	return n
}

// Print writes a human-readable summary of what was dropped and why, so a scan
// that looks wrong can be checked from the CLI.
func (r *Report) Print(w io.Writer, limit int) {
	if r == nil || len(r.Drops) == 0 {
		return
	}

	fmt.Fprintf(w, "  ── dropped %d ──\n", len(r.Drops))
	for i, d := range r.Drops {
		if limit > 0 && i == limit {
			fmt.Fprintf(w, "  ... and %d more dropped\n", len(r.Drops)-limit)
			break
		}
		fmt.Fprintf(w, "  %-10s %-14s %s\n", d.Reason, "["+d.Match+"]", truncate(d.Title, 58))
	}
}

// Filter removes offers that are not the product being tracked, keeping the
// original order. The query is used to avoid filtering away the very thing
// being searched for: a search for "suporte ps5" keeps stands.
func Filter(query string, offers []source.Offer, opts Options) ([]source.Offer, *Report) {
	q := normalize(query)
	report := &Report{}

	// If the query is itself for an accessory, accessory filtering is turned
	// off wholesale rather than exempting only the matching word. Someone
	// tracking "capa ps5" wants a "Capa Protetora contra Poeira" -- exempting
	// just "capa" would still drop it on "protetora".
	wantsAccessory := looksLikeAccessory(q)

	// Computed over the raw result set, before anything is removed, so the
	// judgement of "is this query on target" is not skewed by our own filtering.
	needCoverage, tokens := coverageRequirement(q, offers)

	kept := make([]source.Offer, 0, len(offers))
	seen := make(map[string]bool, len(offers))

	for _, o := range offers {
		title := normalize(o.Title)

		if seen[o.ExternalID] {
			report.Drops = append(report.Drops, drop(o, ReasonDuplicate, o.ExternalID))
			continue
		}

		if term, ok := missingRequired(title, opts.Require); ok {
			report.Drops = append(report.Drops, drop(o, ReasonMissingTerm, term))
			continue
		}
		if term, ok := matchesExclude(title, opts.Exclude); ok {
			report.Drops = append(report.Drops, drop(o, ReasonExcluded, term))
			continue
		}
		if !opts.AllowInternational && isInternational(o, title) {
			report.Drops = append(report.Drops, drop(o, ReasonInternational, "importado"))
			continue
		}
		if bound, ok := outOfRange(o.PriceCents, opts); ok {
			report.Drops = append(report.Drops, drop(o, ReasonPrice, bound))
			continue
		}
		if match, ok := isAccessory(title); ok && !wantsAccessory {
			report.Drops = append(report.Drops, drop(o, ReasonAccessory, match))
			continue
		}
		if needCoverage > 0 && countTokens(title, tokens) < needCoverage {
			report.Drops = append(report.Drops, drop(o, ReasonUnrelated,
				fmt.Sprintf("<%d/%d", needCoverage, len(tokens))))
			continue
		}

		seen[o.ExternalID] = true
		kept = append(kept, o)
	}

	return kept, report
}

func drop(o source.Offer, reason Reason, match string) Drop {
	return Drop{
		Source:     o.Source,
		ExternalID: o.ExternalID,
		Title:      o.Title,
		Price:      o.PriceCents,
		Reason:     reason,
		Match:      match,
	}
}

// coverageRequirement decides how many query words a title must carry, and
// which words those are. It returns 0 when the check should not run.
//
// Marketplaces answer a specific query with loosely related stock when they
// run out of real matches: "LEGO Olivia Rodrigo" comes back with Minecraft and
// Disney sets, which share only the word "lego". Requiring half the query's
// words removes those. The on-target guard is what keeps this safe -- it only
// engages once the result set proves the query is worded the way the listings
// are.
func coverageRequirement(query string, offers []source.Offer) (int, []string) {
	tokens := queryTokens(query)
	if len(tokens) < minCoverageTokens || len(offers) < minCoverageOffers {
		return 0, nil
	}

	full := 0
	for _, o := range offers {
		if countTokens(normalize(o.Title), tokens) == len(tokens) {
			full++
		}
	}
	if float64(full) < float64(len(offers))*onTargetRatio {
		return 0, nil
	}

	need := int(math.Ceil(float64(len(tokens)) * coverageRatio))
	return need, tokens
}

// queryTokens splits a normalized query into its meaningful words.
func queryTokens(query string) []string {
	var out []string
	for _, w := range strings.Fields(query) {
		if len(w) > 1 && !stopwords[w] {
			out = append(out, w)
		}
	}
	return out
}

func countTokens(title string, tokens []string) int {
	n := 0
	for _, t := range tokens {
		if containsWord(title, t) {
			n++
		}
	}
	return n
}

// isInternational reports whether a listing ships from abroad, either because
// the marketplace flagged it or because the title says so.
func isInternational(o source.Offer, title string) bool {
	return o.International || internationalRe.MatchString(title)
}

// isAccessory reports whether a title describes an add-on rather than a
// product in its own right, and which marker said so.
func isAccessory(title string) (string, bool) {
	if m := leadingQualifier.FindString(title); m != "" {
		return strings.TrimSpace(m), true
	}
	if m := lookalike.FindString(title); m != "" {
		return m, true
	}
	if m := knockoff.FindString(title); m != "" {
		return m, true
	}

	for _, phrase := range accessoryPhrases {
		if strings.Contains(title, phrase) {
			return phrase, true
		}
	}
	for _, noun := range accessoryNouns {
		if containsWord(title, noun) {
			return noun, true
		}
	}
	return "", false
}

// looksLikeAccessory reports whether the search is for an accessory, in which
// case accessories are the point and none of them should be filtered out.
func looksLikeAccessory(query string) bool {
	_, ok := isAccessory(query)
	return ok
}

// missingRequired returns the first required term the title lacks.
func missingRequired(title string, require []string) (string, bool) {
	for _, term := range require {
		if term = normalize(term); term != "" && !strings.Contains(title, term) {
			return term, true
		}
	}
	return "", false
}

func matchesExclude(title string, exclude []string) (string, bool) {
	for _, term := range exclude {
		term = normalize(term)
		if term != "" && strings.Contains(title, term) {
			return term, true
		}
	}
	return "", false
}

func outOfRange(cents int64, opts Options) (string, bool) {
	if opts.MinCents > 0 && cents < opts.MinCents {
		return "< min", true
	}
	if opts.MaxCents > 0 && cents > opts.MaxCents {
		return "> max", true
	}
	return "", false
}

// containsWord reports whether s contains word on word boundaries, so "case"
// does not match "showcase" and "capa" does not match "capacete".
func containsWord(s, word string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], word)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(word)

		beforeOK := start == 0 || !isWordByte(s[start-1])
		afterOK := end == len(s) || !isWordByte(s[end])
		if beforeOK && afterOK {
			return true
		}
		i = start + 1
	}
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9'
}

// normalize lowercases, strips accents and collapses punctuation to spaces, so
// "Iluminação" and "iluminacao" are the same word.
func normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	for _, r := range strings.ToLower(s) {
		if d, ok := deaccent[r]; ok {
			b.WriteRune(d)
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			continue
		}
		b.WriteByte(' ')
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

var deaccent = map[rune]rune{
	'á': 'a', 'à': 'a', 'ã': 'a', 'â': 'a', 'ä': 'a',
	'é': 'e', 'ê': 'e', 'è': 'e', 'ë': 'e',
	'í': 'i', 'î': 'i', 'ì': 'i', 'ï': 'i',
	'ó': 'o', 'õ': 'o', 'ô': 'o', 'ò': 'o', 'ö': 'o',
	'ú': 'u', 'û': 'u', 'ù': 'u', 'ü': 'u',
	'ç': 'c', 'ñ': 'n',
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// gapFactor is how much of a jump between neighbouring prices counts as a
// break between two kinds of product rather than ordinary price spread.
// It is an integer because a price is integer cents, and multiplying one by a
// float to compare it is how rounding creeps into money.
const gapFactor = 3

// minAbove is how many listings must sit above the gap for it to be worth
// suggesting; one or two is noise.
const minAbove = 3

// SuggestFloor looks for a price floor worth offering the user.
//
// It never filters anything by itself. Automatic price rules were tried and
// rejected -- see the package comment -- but the underlying observation still
// holds often enough to be worth *asking* about: when a search returns a PS5
// console at R$4.184 and a PS5 game at R$69, the sorted prices show one large
// empty gap, and everything above it is the product the user meant.
//
// A suggestion is only made when the gap is unmistakable, so a query with a
// naturally wide price range (LEGO sets from a polybag to a UCS model) yields
// nothing and the user is left alone.
func SuggestFloor(offers []source.Offer) (int64, bool) {
	if len(offers) < 8 {
		return 0, false
	}

	prices := make([]int64, 0, len(offers))
	for _, o := range offers {
		if o.PriceCents > 0 {
			prices = append(prices, o.PriceCents)
		}
	}
	slices.Sort(prices)

	// Walk from the top so the suggestion keeps the expensive group, and stop
	// at the first gap that leaves enough listings above it.
	for i := len(prices) - 1; i > 0; i-- {
		above := len(prices) - i
		if above < minAbove {
			continue
		}
		if prices[i] >= prices[i-1]*gapFactor {
			// Sit the floor just under the cheapest of the upper group.
			return prices[i] * 9 / 10, true
		}
	}
	return 0, false
}
