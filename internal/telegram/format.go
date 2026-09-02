package telegram

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tgbot "github.com/go-telegram/bot"

	"github.com/thunderjr/price-tracker-bot/internal/source"
	"github.com/thunderjr/price-tracker-bot/internal/store"
	"github.com/thunderjr/price-tracker-bot/internal/tracker"
)

// digestOffers is how many offers a digest lists. Three links is a glance;
// ten is a wall.
const digestOffers = 3

// sourceLabel maps an internal source name to what the user sees.
func sourceLabel(name string) string {
	switch name {
	case "meli":
		return "Mercado Livre"
	case "amazon":
		return "Amazon"
	case "kabum":
		return "KaBuM!"
	default:
		return name
	}
}

// esc escapes text for MarkdownV2, which treats a long list of punctuation as
// markup. Product titles are full of it.
//
// The backslash is doubled first because EscapeMarkdown leaves it alone: a
// title carrying one would otherwise escape whatever character followed it,
// breaking a link or costing the whole message.
func esc(s string) string {
	return tgbot.EscapeMarkdown(strings.ReplaceAll(s, `\`, `\\`))
}

// link renders a MarkdownV2 inline link.
func link(text, url string) string {
	return "[" + esc(text) + "](" + strings.NewReplacer(")", "\\)", "\\", "\\\\").Replace(url) + ")"
}

func money(cents int64) string { return esc(source.FormatBRL(cents)) }

func writeSkipped(b *strings.Builder, skipped map[string]error) {
	if len(skipped) == 0 {
		return
	}
	names := make([]string, 0, len(skipped))
	for name := range skipped {
		names = append(names, sourceLabel(name))
	}
	fmt.Fprintf(b, "\n⚠️ Sem resposta de: %s", esc(strings.Join(names, ", ")))
}

// formatDigest renders one message for a whole watch: why it is worth looking
// at, then the three cheapest offers with their links. It is the only message
// shape the bot sends about a watch, so a scan report and a price alert always
// read the same.
//
// One message per watch, never one per alert. A scan can easily turn up
// dozens of findings across a catalogue, and sending those individually
// buries the chat -- which is exactly what happened before this existed.
func formatDigest(w store.Watch, alerts []tracker.Alert, offers []store.WatchOffer, total int, skipped map[string]error) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s *%s*\n", digestIcon(alerts), esc(w.Query))
	if headline := summarize(alerts); headline != "" {
		fmt.Fprintf(&b, "%s\n", headline)
	}
	// Say which figure the list is ranked on: the cheapest cash price and the
	// cheapest financed total are regularly different listings, so an
	// unexplained order looks wrong.
	if w.PriceMode == store.ModeInstallment {
		b.WriteString("💳 _por total parcelado_\n")
	}
	b.WriteString("\n")

	if w.TargetCents > 0 {
		fmt.Fprintf(&b, "🎯 Alvo: %s\n", money(w.TargetCents))
	}

	if len(offers) == 0 {
		b.WriteString("_Nenhuma oferta no momento\\._")
		writeSkipped(&b, skipped)
		return b.String()
	}

	for i, o := range offers {
		if i == digestOffers {
			break
		}
		fmt.Fprintf(&b, "*%s* · %s", money(o.Effective()), esc(sourceLabel(o.Source)))
		if d := discount(o.PriceCents, o.ListPriceCents); d > 0 {
			fmt.Fprintf(&b, " · \\-%d%%", d)
		}
		if o.LowCents > 0 && o.LowCents < o.Effective() {
			fmt.Fprintf(&b, " · mín 30d %s", money(o.LowCents))
		}
		b.WriteString("\n")
		for _, inst := range installmentLines(w.PriceMode, o) {
			fmt.Fprintf(&b, "%s\n", inst)
		}
		fmt.Fprintf(&b, "%s\n\n", link(truncate(o.Title, 64), o.URL))
	}

	// From the watch's real size, not from however many rows were fetched.
	if extra := total - digestOffers; extra > 0 {
		fmt.Fprintf(&b, "_\\+%d outras ofertas rastreadas_", extra)
	}
	writeSkipped(&b, skipped)
	return strings.TrimRight(b.String(), "\n")
}

// summarize condenses every finding in a scan into one line, most important
// first, with a count rather than a message each.
func summarize(alerts []tracker.Alert) string {
	if len(alerts) == 0 {
		return ""
	}

	// Count by kind, and keep the single best example of the strongest kind.
	counts := map[tracker.Kind]int{}
	var best tracker.Alert
	for _, a := range alerts {
		counts[a.Kind]++
		if best.Kind == "" || rank(a.Kind) < rank(best.Kind) ||
			(a.Kind == best.Kind && a.Discount() > best.Discount()) {
			best = a
		}
	}

	// The listing named in the headline is not necessarily one of the three
	// cheapest offers listed below, so link it here: otherwise the message
	// announces a price with no way to reach it.
	name := truncate(best.Product.Title, 46)
	title := esc(name)
	if best.Product.URL != "" {
		title = link(name, best.Product.URL)
	}

	line := esc(tracker.Describe(best.Candidate)) + " — " + title
	if n := len(alerts) - 1; n > 0 {
		line += fmt.Sprintf("\n_e mais %d %s_", n, plural(n, "alerta", "alertas"))
	}
	if counts[tracker.KindSiteFlag] > 0 && best.Kind == tracker.KindSiteFlag {
		line += "\n_Preço de referência informado pelo próprio anúncio\\._"
	}
	return line
}

// rank orders alert kinds by how much they deserve the headline.
func rank(k tracker.Kind) int {
	switch k {
	case tracker.KindTarget:
		return 0
	case tracker.KindNewLow:
		return 1
	case tracker.KindDropVsMedian:
		return 2
	case tracker.KindBestDrop:
		return 3
	case tracker.KindBestRise:
		return 4
	default:
		return 5
	}
}

func digestIcon(alerts []tracker.Alert) string {
	best := 6
	icon := "🔎"
	for _, a := range alerts {
		if r := rank(a.Kind); r < best {
			best, icon = r, alertIcon(a.Kind)
		}
	}
	return icon
}

func alertIcon(k tracker.Kind) string {
	switch k {
	case tracker.KindTarget:
		return "🎯"
	case tracker.KindNewLow:
		return "🏆"
	case tracker.KindDropVsMedian:
		return "🔻"
	case tracker.KindBestDrop:
		return "📉"
	case tracker.KindBestRise:
		return "📈"
	default:
		return "🏷"
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// formatList renders the plain-text watch list.
func formatList(rows []watchRow) string {
	if len(rows) == 0 {
		return "Nenhuma busca rastreada\\. Use `/track ps5` para começar\\."
	}

	var b strings.Builder
	fmt.Fprintf(&b, "📋 *%d busca\\(s\\) rastreada\\(s\\)*\n\n", len(rows))
	for i, r := range rows {
		fmt.Fprintf(&b, "%d\\. *%s*", i+1, esc(r.Watch.Query))
		if !r.Watch.Active {
			b.WriteString(" ⏸")
		}
		b.WriteString("\n")

		if r.Stats.Products == 0 {
			b.WriteString("   _ainda sem ofertas_\n")
			continue
		}
		fmt.Fprintf(&b, "   %s · %d ofertas", money(r.Stats.BestCents), r.Stats.Products)
		if r.Watch.TargetCents > 0 {
			fmt.Fprintf(&b, " · alvo %s", money(r.Watch.TargetCents))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// formatDetail renders one watch's detail view.
func formatDetail(r watchRow) string {
	var b strings.Builder

	fmt.Fprintf(&b, "🔎 *%s*", esc(r.Watch.Query))
	if !r.Watch.Active {
		b.WriteString("  ⏸ _pausada_")
	}
	b.WriteString("\n\n")

	if r.Stats.Products == 0 {
		b.WriteString("_Ainda sem ofertas registradas\\._\n")
	} else {
		label := "Melhor agora"
		if r.Watch.PriceMode == store.ModeInstallment {
			label = "Melhor parcelado"
		}
		fmt.Fprintf(&b, "%s: *%s*\n", label, money(r.Stats.BestCents))
		// The payment options for the cheapest offer, so the detail view
		// answers "what would this actually cost me" without a second tap.
		for _, inst := range installmentLines(r.Watch.PriceMode, r.Best) {
			fmt.Fprintf(&b, "%s\n", inst)
		}
		if r.Stats.LowCents > 0 && r.Stats.LowCents < r.Stats.BestCents {
			fmt.Fprintf(&b, "Mínimo 30d: %s\n", money(r.Stats.LowCents))
		}
		fmt.Fprintf(&b, "%d ofertas rastreadas\n", r.Stats.Products)
	}

	if f := describeFilters(r.Watch); f != "" {
		fmt.Fprintf(&b, "\nFiltros:%s\n", f)
	}
	fmt.Fprintf(&b, "\n_%s_", esc(scanAge(r.Watch.LastScanAt)))
	return b.String()
}

func scanAge(t time.Time) string {
	if t.IsZero() {
		return "nunca escaneada"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "escaneada agora"
	case d < time.Hour:
		return fmt.Sprintf("escaneada há %d min", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("escaneada há %d h", int(d.Hours()))
	default:
		return fmt.Sprintf("escaneada há %d dias", int(d.Hours()/24))
	}
}

// installmentLines renders every payment option the listing publishes: the
// financing plan with the site's own juros wording and what it adds up to,
// and separately what Mercado Livre quotes for paying another way.
//
// The interest wording is passed through rather than assumed. A plan marked
// "com juros" genuinely costs more -- "em até 12x de R$ 11,08 com juros" on a
// R$ 118,72 item comes to R$ 132,96 -- and a plan that says nothing either way
// is shown without a claim.
func installmentLines(mode store.PriceMode, o store.WatchOffer) []string {
	var out []string
	parcelado := mode == store.ModeInstallment

	if o.InstallmentCount > 1 && o.InstallmentEachCents > 0 {
		// In "parcelado" mode the plan is the headline figure, so this line
		// breaks it down rather than offering an alternative to it.
		line := fmt.Sprintf("%dx %s", o.InstallmentCount, money(o.InstallmentEachCents))
		if !parcelado {
			line = "ou " + line
		}
		if o.InstallmentInterest != "" {
			line += " " + esc(o.InstallmentInterest)
		}

		// The total always rides on the same line as the instalments it comes
		// from. It is the figure the offers are ranked by, so showing it
		// beside the plan is what lets the reader check the order -- and the
		// per-instalment amount on its own says nothing about what the plan
		// costs. Printed even when it matches the cash price, so its absence
		// never has to be interpreted.
		total := int64(o.InstallmentCount) * o.InstallmentEachCents
		line += fmt.Sprintf(" \\(total %s\\)", money(total))
		out = append(out, "_"+line+"_")
	}

	// The cash price is the option that would otherwise go unsaid once the
	// financed total leads.
	if parcelado && o.PriceCents > 0 && o.PriceCents != o.Effective() {
		out = append(out, fmt.Sprintf("_ou %s à vista_", money(o.PriceCents)))
	}

	if o.OtherMeansCents > o.PriceCents {
		out = append(out, fmt.Sprintf("_ou %s em outros meios_", money(o.OtherMeansCents)))
	}
	return out
}

func discount(price, list int64) int {
	if list <= price || price <= 0 {
		return 0
	}
	return int((list - price) * 100 / list)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// helpText renders /help. The movement threshold it quotes is configurable,
// so the number has to come from the config rather than be written into the
// text and drift away from it.
func helpText(bestMoveThreshold float64) string {
	pct := esc(strconv.FormatFloat(bestMoveThreshold*100, 'f', -1, 64))
	return `*Price Tracker*

Rastreia preços e promoções no Mercado Livre e na Amazon BR\.

*/track* ` + "`ps5`" + ` — rastrear uma busca
*/manage* — gerenciar buscas \(botões\)
*/list* — lista em texto

*Filtrando o que é rastreado*

Acessórios \(suporte, capa, cooler, kit de iluminação\), cópias e ofertas
internacionais são descartados automaticamente\.

Importados ficam de fora porque o preço anunciado não inclui o imposto de
importação — ele parece mais barato do que é\. Use ` + "`| internacional`" + ` para
incluí\-los\.

Jogos custam bem menos que um console, mas nenhum filtro de título separa
os dois com segurança — para isso use uma faixa de preço:

` + "`/track ps5 | min 3000`" + `
` + "`/track ps5 | min 3000 | alvo 4200`" + `
` + "`/track lego millennium falcon | -iluminacao -minifigura`" + `
` + "`/track lego olivia rodrigo | +rodrigo`" + `

Depois do primeiro scan, se os preços se dividirem em dois grupos bem
separados, o bot oferece o filtro em um toque\.

*À vista ou parcelado*

Em */manage*, o botão 💳 alterna a busca entre ordenar pelo preço à vista e
ordenar pelo total parcelado — a oferta mais barata à vista quase nunca é a
mais barata parcelada\. O alvo e os alertas passam a usar o mesmo valor\.

As duas formas de pagar aparecem em toda mensagem, com juros quando o
anúncio cobra juros\. O total do parcelamento vem na mesma linha das
parcelas — é por ele que as ofertas são ordenadas\.

*Alertas*

Você recebe uma mensagem quando o melhor preço de uma busca sobe ou desce
mais de ` + pct + `%, e também quando: cai bem abaixo da mediana de 30 dias,
bate o menor preço já registrado, atinge seu alvo, ou o anúncio passa a
marcar promoção\.

Descontos anunciados são conferidos: quando o valor riscado é só o mesmo
produto parcelado \(10x R$ 449,90 \= R$ 4\.499\), não conta como promoção\.`
}

func bold(s string) string { return "*" + esc(s) + "*" }

// describeFilters renders the constraints a watch applies, so the user can see
// at a glance why something is or is not being tracked.
func describeFilters(w store.Watch) string {
	var parts []string
	switch {
	case w.MinCents > 0 && w.MaxCents > 0:
		parts = append(parts, "entre "+money(w.MinCents)+" e "+money(w.MaxCents))
	case w.MinCents > 0:
		parts = append(parts, "acima de "+money(w.MinCents))
	case w.MaxCents > 0:
		parts = append(parts, "abaixo de "+money(w.MaxCents))
	}
	if w.TargetCents > 0 {
		parts = append(parts, "alvo "+money(w.TargetCents))
	}
	if len(w.Require) > 0 {
		parts = append(parts, "com "+esc(strings.Join(w.Require, ", ")))
	}
	if len(w.Exclude) > 0 {
		parts = append(parts, "sem "+esc(strings.Join(w.Exclude, ", ")))
	}
	if w.AllowInternational {
		parts = append(parts, "inclui importados")
	}
	if len(parts) == 0 {
		return ""
	}
	return " \\(" + strings.Join(parts, " · ") + "\\)"
}
