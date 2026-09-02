package telegram

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/thunderjr/price-tracker-bot/internal/source"
	"github.com/thunderjr/price-tracker-bot/internal/store"
	"github.com/thunderjr/price-tracker-bot/internal/tracker"
)

// scanTimeout bounds a scan kicked off from a button. The browser can be slow;
// the update loop must not be.
const scanTimeout = 5 * time.Minute

// offersShown is how many offers a detail view lists.
const offersShown = digestOffers

// --- commands ---

func (b *Bot) cmdTrack(ctx context.Context, chatID int64, args string) {
	spec := ParseTrackArgs(args)
	if spec.Query == "" {
		b.send(ctx, chatID, trackUsage)
		return
	}

	w, err := b.store.CreateWatch(ctx, chatID, spec)
	if err != nil {
		b.log.Error("create watch failed", "chat", chatID, "query", spec.Query, "err", err)
		b.send(ctx, chatID, "Não consegui criar a busca\\.")
		return
	}

	b.send(ctx, chatID, "✅ Rastreando "+bold(w.Query)+describeFilters(*w)+"\\. Escaneando agora…")
	go b.scanAndReport(w.ID, chatID)
}

func (b *Bot) cmdList(ctx context.Context, chatID int64) {
	rows, err := b.loadRows(ctx, chatID)
	if err != nil {
		b.log.Error("list watches failed", "chat", chatID, "err", err)
		b.send(ctx, chatID, "Não consegui ler suas buscas\\.")
		return
	}
	b.send(ctx, chatID, formatList(rows))
}

func (b *Bot) cmdManage(ctx context.Context, chatID int64) {
	rows, err := b.loadRows(ctx, chatID)
	if err != nil {
		b.log.Error("manage failed", "chat", chatID, "err", err)
		b.send(ctx, chatID, "Não consegui ler suas buscas\\.")
		return
	}
	b.sendWithKeyboard(ctx, chatID, listHeader(rows), listKeyboard(rows, 0))
}

func listHeader(rows []watchRow) string {
	if len(rows) == 0 {
		return "📋 Nenhuma busca rastreada\\.\n\nUse `/track ps5` para começar\\."
	}
	return fmt.Sprintf("📋 *Rastreando %d busca\\(s\\)*\n\nToque em uma para ver detalhes e ações\\.", len(rows))
}

// --- callbacks ---

func (b *Bot) handleCallback(ctx context.Context, q *models.CallbackQuery) {
	// The From user is who pressed the button; it is not necessarily the chat
	// owner, and it is the identity the allowlist applies to.
	if !b.cfg.Allowed(q.From.ID) {
		b.log.Warn("callback from a user that is not allowed", "user", q.From.ID)
		b.answer(ctx, q.ID, "Não autorizado.")
		return
	}
	if q.Message.Message == nil {
		b.answer(ctx, q.ID, "")
		return
	}

	chatID := q.Message.Message.Chat.ID
	messageID := q.Message.Message.ID

	cb, err := decode(q.Data)
	if err != nil {
		// Almost always a button from before a redeploy.
		b.log.Warn("undecodable callback", "data", q.Data, "err", err)
		b.answer(ctx, q.ID, "Botão expirado. Envie /manage de novo.")
		return
	}

	switch cb.Action {
	case actNoop:
		b.answer(ctx, q.ID, "")
	case actSetFloor:
		b.applyFloor(ctx, q.ID, chatID, messageID, cb.WatchID, cb.Arg)
	case actClose:
		b.answer(ctx, q.ID, "")
		b.closeManager(ctx, chatID, messageID)
	case actList:
		b.answer(ctx, q.ID, "")
		b.showList(ctx, chatID, messageID, cb.Arg)
	case actDetail:
		b.answer(ctx, q.ID, "")
		b.showDetail(ctx, chatID, messageID, cb.WatchID, cb.Arg)
	case actOffers:
		b.answer(ctx, q.ID, "")
		b.showOffers(ctx, chatID, messageID, cb.WatchID, cb.Arg)
	case actTogglePause:
		b.togglePause(ctx, q.ID, chatID, messageID, cb.WatchID, cb.Arg)
	case actTarget:
		b.promptTarget(ctx, q.ID, chatID, messageID, cb.WatchID, cb.Arg)
	case actDeleteAsk:
		b.askDelete(ctx, q.ID, chatID, messageID, cb.WatchID, cb.Arg)
	case actDeleteDo:
		b.doDelete(ctx, q.ID, chatID, messageID, cb.WatchID, cb.Arg)
	case actScan:
		b.scanFromButton(ctx, q.ID, chatID, messageID, cb.WatchID, cb.Arg)
	case actScanAll:
		b.scanAllFromButton(ctx, q.ID, chatID, messageID, cb.Arg)
	default:
		b.answer(ctx, q.ID, "Ação desconhecida.")
	}
}

func (b *Bot) closeManager(ctx context.Context, chatID int64, messageID int) {
	_, err := b.api.DeleteMessage(ctx, &tgbot.DeleteMessageParams{ChatID: chatID, MessageID: messageID})
	if err != nil {
		// Telegram refuses to delete messages older than 48h; leave a stub.
		b.edit(ctx, chatID, messageID, "📋 _Gerenciador fechado\\._", nil)
	}
}

func (b *Bot) showList(ctx context.Context, chatID int64, messageID int, page int64) {
	rows, err := b.loadRows(ctx, chatID)
	if err != nil {
		b.log.Error("show list failed", "chat", chatID, "err", err)
		return
	}
	b.edit(ctx, chatID, messageID, listHeader(rows), listKeyboard(rows, page))
}

func (b *Bot) showDetail(ctx context.Context, chatID int64, messageID int, watchID, page int64) {
	row, err := b.row(ctx, chatID, messageID, watchID, page)
	if err != nil {
		return
	}
	b.edit(ctx, chatID, messageID, formatDetail(row), detailKeyboard(row, page))
}

func (b *Bot) showOffers(ctx context.Context, chatID int64, messageID int, watchID, page int64) {
	row, err := b.row(ctx, chatID, messageID, watchID, page)
	if err != nil {
		return
	}

	offers, err := b.store.WatchOffers(ctx, watchID, statsWindow, offersShown)
	if err != nil {
		b.log.Error("watch offers failed", "watch", watchID, "err", err)
		return
	}
	b.edit(ctx, chatID, messageID,
		formatDigest(row.Watch, nil, offers, row.Stats.Products, nil), backKeyboard(watchID, page))
}

func (b *Bot) togglePause(ctx context.Context, queryID string, chatID int64, messageID int, watchID, page int64) {
	row, err := b.row(ctx, chatID, messageID, watchID, page)
	if err != nil {
		b.answer(ctx, queryID, "Busca não encontrada.")
		return
	}

	if err := b.store.SetWatchActive(ctx, watchID, !row.Watch.Active); err != nil {
		b.log.Error("toggle pause failed", "watch", watchID, "err", err)
		b.answer(ctx, queryID, "Não consegui alterar.")
		return
	}

	if row.Watch.Active {
		b.answer(ctx, queryID, "Pausada.")
	} else {
		b.answer(ctx, queryID, "Retomada.")
	}
	b.showDetail(ctx, chatID, messageID, watchID, page)
}

// promptTarget asks for a price. A ForceReply has to be its own message --
// there is no way to attach one to the manager -- so the prompt and the user's
// answer are both deleted once the value lands, leaving only the manager.
func (b *Bot) promptTarget(ctx context.Context, queryID string, chatID int64, messageID int, watchID, page int64) {
	if _, err := b.loadRow(ctx, chatID, watchID); err != nil {
		b.answer(ctx, queryID, "Busca não encontrada.")
		return
	}
	b.answer(ctx, queryID, "")

	prompt, err := b.api.SendMessage(ctx, &tgbot.SendMessageParams{
		ChatID:    chatID,
		Text:      "🎯 Envie o preço alvo \\(ex\\.: `3500` ou `3.499,90`\\)\\.\nEnvie `0` para remover o alvo\\.",
		ParseMode: models.ParseModeMarkdown,
		ReplyMarkup: &models.ForceReply{
			ForceReply:            true,
			InputFieldPlaceholder: "3500",
			Selective:             true,
		},
	})
	if err != nil {
		b.log.Error("target prompt failed", "chat", chatID, "err", err)
		return
	}

	p := pendingTarget{watchID: watchID, managerMsg: messageID, page: page}
	if prompt != nil {
		p.promptMsg = prompt.ID
	}
	b.setPending(chatID, p)
}

// applyTarget consumes the reply to a target prompt and folds the result back
// into the manager message.
func (b *Bot) applyTarget(ctx context.Context, chatID int64, replyMsgID int, p pendingTarget, text string) {
	if _, err := b.loadRow(ctx, chatID, p.watchID); err != nil {
		b.deleteMessage(ctx, chatID, p.promptMsg)
		b.send(ctx, chatID, "Busca não encontrada\\.")
		return
	}

	cents := source.ParseBRL(text)
	if cents == 0 && strings.TrimSpace(text) != "0" {
		// Keep the prompt alive so the next message is still treated as the
		// answer, instead of making the user press the button again.
		b.setPending(chatID, p)
		b.send(ctx, chatID, "Não entendi esse valor\\. Tente `3500` ou `3.499,90`\\.")
		return
	}

	if err := b.store.SetWatchTarget(ctx, p.watchID, cents); err != nil {
		b.log.Error("set target failed", "watch", p.watchID, "err", err)
		b.send(ctx, chatID, "Não consegui salvar o alvo\\.")
		return
	}

	b.deleteMessage(ctx, chatID, p.promptMsg)
	b.deleteMessage(ctx, chatID, replyMsgID)

	if p.managerMsg != 0 {
		b.showDetail(ctx, chatID, p.managerMsg, p.watchID, p.page)
		return
	}
	// The manager message is gone; fall back to a plain confirmation.
	if cents == 0 {
		b.send(ctx, chatID, "Alvo removido\\.")
		return
	}
	b.send(ctx, chatID, fmt.Sprintf("🎯 Alvo definido em %s\\.", money(cents)))
}

func (b *Bot) askDelete(ctx context.Context, queryID string, chatID int64, messageID int, watchID, page int64) {
	row, err := b.row(ctx, chatID, messageID, watchID, page)
	if err != nil {
		b.answer(ctx, queryID, "Busca não encontrada.")
		return
	}

	b.answer(ctx, queryID, "")
	text := fmt.Sprintf("🗑 Remover *%s* do rastreamento?\n\n_O histórico de preços é mantido\\._", esc(row.Watch.Query))
	b.edit(ctx, chatID, messageID, text, confirmDeleteKeyboard(watchID, page))
}

func (b *Bot) doDelete(ctx context.Context, queryID string, chatID int64, messageID int, watchID, page int64) {
	if _, err := b.loadRow(ctx, chatID, watchID); err != nil {
		b.answer(ctx, queryID, "Busca não encontrada.")
		b.showList(ctx, chatID, messageID, page)
		return
	}

	if err := b.store.DeleteWatch(ctx, watchID); err != nil {
		b.log.Error("delete watch failed", "watch", watchID, "err", err)
		b.answer(ctx, queryID, "Não consegui remover.")
		return
	}
	b.answer(ctx, queryID, "Removida.")
	b.showList(ctx, chatID, messageID, page)
}

// applyFloor accepts a suggested price floor. arg carries whole reais, which
// keeps the payload inside Telegram's 64 byte callback budget.
func (b *Bot) applyFloor(ctx context.Context, queryID string, chatID int64, messageID int, watchID, reais int64) {
	w, err := b.store.Watch(ctx, watchID)
	if err != nil || w.ChatID != chatID {
		b.answer(ctx, queryID, "Busca não encontrada.")
		return
	}

	if err := b.store.SetWatchBounds(ctx, watchID, reais*100, w.MaxCents); err != nil {
		b.log.Error("set bounds failed", "watch", watchID, "err", err)
		b.answer(ctx, queryID, "Não consegui salvar.")
		return
	}

	b.answer(ctx, queryID, "Filtro aplicado.")
	b.edit(ctx, chatID, messageID,
		"✅ Rastreando só ofertas acima de "+money(reais*100)+" em "+bold(w.Query)+
			"\\.\n\n_Jogos e acessórios ficam de fora a partir do próximo scan\\._", nil)
}

// --- scanning from buttons ---

func (b *Bot) scanFromButton(ctx context.Context, queryID string, chatID int64, messageID int, watchID, page int64) {
	row, err := b.row(ctx, chatID, messageID, watchID, page)
	if err != nil {
		b.answer(ctx, queryID, "Busca não encontrada.")
		return
	}
	// The lock that actually decides this lives in the tracker, since the
	// scheduler has to share it; this is only so the user hears why nothing
	// happened instead of watching a spinner.
	if b.tracker.ScanInProgress(watchID) {
		b.answer(ctx, queryID, "Essa busca já está sendo escaneada.")
		return
	}

	b.answer(ctx, queryID, "Escaneando…")
	b.edit(ctx, chatID, messageID,
		fmt.Sprintf("⏳ Escaneando *%s*…", esc(row.Watch.Query)), nil)

	// A scan drives a real browser and can take a minute. Detaching keeps the
	// update loop responsive, and it must not inherit the update's context.
	go func() {
		bg, cancel := context.WithTimeout(context.Background(), scanTimeout)
		defer cancel()

		alerts := b.runScan(bg, watchID)
		b.showDetail(bg, chatID, messageID, watchID, page)
		if err := b.Deliver(bg, alerts); err != nil {
			b.log.Error("deliver alerts failed", "watch", watchID, "err", err)
		}
	}()
}

func (b *Bot) scanAllFromButton(ctx context.Context, queryID string, chatID int64, messageID int, page int64) {
	rows, err := b.loadRows(ctx, chatID)
	if err != nil {
		b.answer(ctx, queryID, "Não consegui ler suas buscas.")
		return
	}

	var active []store.Watch
	for _, r := range rows {
		if r.Watch.Active {
			active = append(active, r.Watch)
		}
	}
	if len(active) == 0 {
		b.answer(ctx, queryID, "Nenhuma busca ativa.")
		return
	}

	b.answer(ctx, queryID, fmt.Sprintf("Escaneando %d busca(s)…", len(active)))
	b.edit(ctx, chatID, messageID, fmt.Sprintf("⏳ Escaneando %d busca\\(s\\)…", len(active)), nil)

	go func() {
		bg, cancel := context.WithTimeout(context.Background(), scanTimeout*time.Duration(len(active)))
		defer cancel()

		for _, w := range active {
			alerts := b.runScan(bg, w.ID)
			if err := b.Deliver(bg, alerts); err != nil {
				b.log.Error("deliver alerts failed", "watch", w.ID, "err", err)
			}
		}
		b.showList(bg, chatID, messageID, page)
	}()
}

// runScan scans one watch and returns the alerts it produced.
func (b *Bot) runScan(ctx context.Context, watchID int64) []tracker.Alert {
	w, err := b.store.Watch(ctx, watchID)
	if err != nil {
		b.log.Error("scan: load watch failed", "watch", watchID, "err", err)
		return nil
	}

	res, err := b.tracker.Scan(ctx, *w)
	if errors.Is(err, tracker.ErrScanInProgress) {
		b.log.Info("scan skipped, watch already being scanned", "watch", watchID)
		return nil
	}
	if err != nil {
		b.log.Error("scan failed", "watch", watchID, "query", w.Query, "err", err)
		return nil
	}
	b.log.Info("scan done", "watch", watchID, "query", w.Query,
		"found", res.Found, "tracked", res.Tracked, "filtered", res.Filtered,
		"pruned", res.Pruned, "alerts", len(res.Alerts), "skipped", len(res.Skipped))
	return res.Alerts
}

// scanAndReport scans a freshly created watch and posts what it found.
func (b *Bot) scanAndReport(watchID, chatID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), scanTimeout)
	defer cancel()

	w, err := b.store.Watch(ctx, watchID)
	if err != nil {
		return
	}

	res, err := b.tracker.Scan(ctx, *w)
	if errors.Is(err, tracker.ErrScanInProgress) {
		// The scheduler picked this watch up first; its report will arrive.
		return
	}
	if err != nil {
		b.log.Error("initial scan failed", "watch", watchID, "err", err)
		b.send(ctx, chatID, fmt.Sprintf("⚠️ Não consegui buscar *%s* agora\\. Vou tentar de novo no próximo ciclo\\.", esc(w.Query)))
		return
	}
	if err := b.ReportScan(ctx, *w, res); err != nil {
		b.log.Error("report scan failed", "watch", watchID, "err", err)
	}
}

// ReportScan posts a watch's current offers to its chat, along with how many
// listings were filtered out and, when the prices split into two clear groups,
// a button to track only the upper one.
func (b *Bot) ReportScan(ctx context.Context, w store.Watch, res *tracker.Result) error {
	offers, err := b.store.WatchOffers(ctx, w.ID, statsWindow, offersShown)
	if err != nil {
		return err
	}

	text := formatDigest(w, res.Alerts, offers, res.Tracked, res.Skipped)
	if res.Filtered > 0 {
		text += fmt.Sprintf("\n\n_%d acessórios e itens repetidos filtrados\\._", res.Filtered)
	}

	// The results split into two distant price groups, which almost always
	// means games and accessories mixed in with the real product. Offer the
	// cut rather than making it: only the user knows which group they meant.
	if res.Suggestion > 0 {
		reais := res.Suggestion / 100
		text += "\n\n💡 Os preços se dividem em dois grupos\\. Rastrear só o grupo acima de " +
			money(reais*100) + "?"
		return b.sendWithKeyboard(ctx, w.ChatID, text, &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{{
				button("✂️ Sim, filtrar", actSetFloor, w.ID, reais),
				button("Manter tudo", actNoop, 0, 0),
			}},
		})
	}

	return b.send(ctx, w.ChatID, text)
}

// row loads a watch for a callback, reporting failures into the message.
func (b *Bot) row(ctx context.Context, chatID int64, messageID int, watchID, page int64) (watchRow, error) {
	row, err := b.loadRow(ctx, chatID, watchID)
	if err == nil {
		return row, nil
	}

	if errors.Is(err, store.ErrNotFound) {
		b.showList(ctx, chatID, messageID, page)
		return watchRow{}, errNoWatch
	}
	b.log.Error("load watch failed", "watch", watchID, "err", err)
	return watchRow{}, err
}

// --- argument parsing ---

// Track argument grammar, pipe-separated after the query:
//
//	/track ps5 | min 3000 | max 6000 | alvo 4200 | -portal -ps4
//
// min and max decide which listings the watch follows at all -- the only way
// to say "the console, not its games", since no title heuristic can tell those
// apart safely. alvo is the price to be alerted at. Terms after "-" are
// dropped by title.
var internationalRe = regexp.MustCompile(`(?i)^(internacional|importado|international)$`)

var clauseRe = regexp.MustCompile(`(?i)^(min|minimo|max|maximo|ate|alvo|target)\s*:?\s*(.+)$`)

const trackUsage = "Use:\n" +
	"`/track ps5`\n" +
	"`/track ps5 | min 3000`  — só consoles, sem jogos e acessórios\n" +
	"`/track ps5 | min 3000 | alvo 4200`\n" +
	"`/track lego millennium falcon | -iluminacao`  — exclui termos\n" +
	"`/track lego olivia rodrigo | +rodrigo`  — exige termos no título\n" +
	"`/track ps5 | internacional`  — inclui importados \\(preço sem imposto\\)"

// ParseTrackArgs splits a /track argument string into a watch spec. The CLI
// shares it so both entry points accept exactly the same grammar.
func ParseTrackArgs(args string) store.WatchSpec {
	parts := strings.Split(args, "|")

	spec := store.WatchSpec{Query: strings.Join(strings.Fields(parts[0]), " ")}
	for _, clause := range parts[1:] {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}

		// Cross-border listings are dropped by default because their price
		// excludes import tax; this asks for them back.
		if internationalRe.MatchString(clause) {
			spec.AllowInternational = true
			continue
		}
		if strings.HasPrefix(clause, "-") {
			spec.Exclude = append(spec.Exclude, parseTerms(clause, "-")...)
			continue
		}
		if strings.HasPrefix(clause, "+") {
			spec.Require = append(spec.Require, parseTerms(clause, "+")...)
			continue
		}

		m := clauseRe.FindStringSubmatch(clause)
		if m == nil {
			continue
		}
		cents := source.ParseBRL(m[2])
		switch strings.ToLower(m[1]) {
		case "min", "minimo":
			spec.MinCents = cents
		case "max", "maximo", "ate":
			spec.MaxCents = cents
		case "alvo", "target":
			spec.TargetCents = cents
		}
	}
	return spec
}

// parseTerms reads "-portal -ps4" or "+olivia +rodrigo" into terms.
func parseTerms(clause, prefix string) []string {
	var out []string
	for _, f := range strings.Fields(clause) {
		if t := strings.TrimSpace(strings.TrimPrefix(f, prefix)); t != "" {
			out = append(out, t)
		}
	}
	return out
}
