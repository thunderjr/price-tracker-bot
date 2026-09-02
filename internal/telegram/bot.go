// Package telegram is the bot front end: commands, the inline-keyboard
// manager, and alert delivery.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/thunderjr/price-tracker-bot/internal/config"
	"github.com/thunderjr/price-tracker-bot/internal/store"
	"github.com/thunderjr/price-tracker-bot/internal/tracker"
)

// statsWindow is the trailing period the manager summarizes.
const statsWindow = 30 * 24 * time.Hour

// pendingTTL is how long a prompted target-price reply stays expected.
const pendingTTL = 5 * time.Minute

// watchRow pairs a watch with its headline numbers.
type watchRow struct {
	Watch store.Watch
	Stats store.WatchStats
}

// Bot serves the Telegram front end.
type Bot struct {
	api     *tgbot.Bot
	cfg     *config.Config
	store   *store.Store
	tracker *tracker.Tracker
	log     *slog.Logger

	// pending remembers which watch a chat is about to send a target price
	// for, set by the target button and consumed by the next reply.
	pendingMu sync.Mutex
	pending   map[int64]pendingTarget
}

// pendingTarget remembers everything needed to fold the reply back into the
// manager message and clean up the prompt afterwards.
type pendingTarget struct {
	watchID    int64
	managerMsg int
	promptMsg  int
	page       int64
	expires    time.Time
}

// New builds the bot.
func New(cfg *config.Config, st *store.Store, tr *tracker.Tracker, log *slog.Logger) (*Bot, error) {
	if log == nil {
		log = slog.Default()
	}

	b := &Bot{
		cfg:     cfg,
		store:   st,
		tracker: tr,
		log:     log,
		pending: map[int64]pendingTarget{},
	}

	api, err := tgbot.New(cfg.TelegramToken, tgbot.WithDefaultHandler(b.handle))
	if err != nil {
		return nil, fmt.Errorf("telegram: new bot: %w", err)
	}
	b.api = api
	return b, nil
}

// Start runs the update loop until ctx is cancelled.
func (b *Bot) Start(ctx context.Context) {
	b.registerCommands(ctx)
	b.api.Start(ctx)
}

func (b *Bot) registerCommands(ctx context.Context) {
	_, err := b.api.SetMyCommands(ctx, &tgbot.SetMyCommandsParams{
		Commands: []models.BotCommand{
			{Command: "track", Description: "Rastrear uma busca"},
			{Command: "manage", Description: "Gerenciar buscas rastreadas"},
			{Command: "list", Description: "Listar buscas em texto"},
			{Command: "help", Description: "Como usar"},
		},
	})
	if err != nil {
		b.log.Warn("could not register bot commands", "err", err)
	}
}

// handle is the single entry point for every update. Routing lives here so the
// allowlist check cannot be forgotten on a new handler.
func (b *Bot) handle(ctx context.Context, api *tgbot.Bot, update *models.Update) {
	switch {
	case update.CallbackQuery != nil:
		b.handleCallback(ctx, update.CallbackQuery)
	case update.Message != nil:
		b.handleMessage(ctx, update.Message)
	}
}

func (b *Bot) handleMessage(ctx context.Context, msg *models.Message) {
	chatID := msg.Chat.ID

	// The allowlist holds user ids, so authorize the sender -- the same
	// identity callbacks are checked against. In a private chat the two are
	// the same number; in a group this is what keeps strangers out.
	sender := chatID
	if msg.From != nil {
		sender = msg.From.ID
	}
	if !b.cfg.Allowed(sender) {
		b.log.Warn("message from a user that is not allowed", "user", sender, "chat", chatID)
		return
	}

	text := strings.TrimSpace(msg.Text)

	// A pending target prompt swallows the next plain message.
	if !strings.HasPrefix(text, "/") {
		if p, ok := b.takePending(chatID); ok {
			b.applyTarget(ctx, chatID, msg.ID, p, text)
			return
		}
	}

	cmd, args, _ := strings.Cut(text, " ")
	cmd, _, _ = strings.Cut(cmd, "@") // /track@MyBot in groups
	args = strings.TrimSpace(args)

	switch cmd {
	case "/start", "/help":
		b.send(ctx, chatID, helpText(b.cfg.BestMoveThreshold))
	case "/track":
		b.cmdTrack(ctx, chatID, args)
	case "/manage":
		b.cmdManage(ctx, chatID)
	case "/list":
		b.cmdList(ctx, chatID)
	default:
		if strings.HasPrefix(text, "/") {
			b.send(ctx, chatID, "Comando desconhecido\\. Veja /help\\.")
		}
	}
}

// --- outgoing helpers ---

// send posts a message. The error is returned as well as logged: an alert is
// already recorded as fired by the time it gets here, so a caller that reports
// success without checking turns a rate-limited send into a finding nobody
// ever hears about.
func (b *Bot) send(ctx context.Context, chatID int64, text string) error {
	_, err := b.api.SendMessage(ctx, &tgbot.SendMessageParams{
		ChatID:             chatID,
		Text:               text,
		ParseMode:          models.ParseModeMarkdown,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: ptr(true)},
	})
	if err != nil {
		b.log.Error("send message failed", "chat", chatID, "err", err)
		return fmt.Errorf("telegram: send to chat %d: %w", chatID, err)
	}
	return nil
}

func (b *Bot) sendWithKeyboard(ctx context.Context, chatID int64, text string, kb *models.InlineKeyboardMarkup) error {
	_, err := b.api.SendMessage(ctx, &tgbot.SendMessageParams{
		ChatID:             chatID,
		Text:               text,
		ParseMode:          models.ParseModeMarkdown,
		ReplyMarkup:        kb,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: ptr(true)},
	})
	if err != nil {
		b.log.Error("send keyboard message failed", "chat", chatID, "err", err)
		return fmt.Errorf("telegram: send to chat %d: %w", chatID, err)
	}
	return nil
}

// edit rewrites a message in place. Re-rendering an identical view is normal
// (a refresh that changed nothing), and Telegram calls that an error, so that
// one case is treated as success.
func (b *Bot) edit(ctx context.Context, chatID int64, messageID int, text string, kb *models.InlineKeyboardMarkup) {
	_, err := b.api.EditMessageText(ctx, &tgbot.EditMessageTextParams{
		ChatID:             chatID,
		MessageID:          messageID,
		Text:               text,
		ParseMode:          models.ParseModeMarkdown,
		ReplyMarkup:        kb,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: ptr(true)},
	})
	if err != nil && !isNotModified(err) {
		b.log.Error("edit message failed", "chat", chatID, "message", messageID, "err", err)
	}
}

// answer clears the client's loading spinner. Every callback path must reach
// this, including the ones that do nothing, or the button spins forever.
func (b *Bot) answer(ctx context.Context, queryID, text string) {
	_, err := b.api.AnswerCallbackQuery(ctx, &tgbot.AnswerCallbackQueryParams{
		CallbackQueryID: queryID,
		Text:            text,
	})
	if err != nil {
		b.log.Warn("answer callback failed", "err", err)
	}
}

// deleteMessage removes a message, ignoring the usual reasons Telegram says
// no (already gone, older than 48h, no permission in a group).
func (b *Bot) deleteMessage(ctx context.Context, chatID int64, messageID int) {
	if messageID == 0 {
		return
	}
	if _, err := b.api.DeleteMessage(ctx, &tgbot.DeleteMessageParams{ChatID: chatID, MessageID: messageID}); err != nil {
		b.log.Debug("delete message failed", "chat", chatID, "message", messageID, "err", err)
	}
}

// isNotModified reports Telegram's "you edited a message into itself" error.
func isNotModified(err error) bool {
	return err != nil && strings.Contains(err.Error(), "message is not modified")
}

// --- alert delivery ---

// Deliver reports a scan's findings as one message per watch.
//
// Never one message per alert. A single scan across a catalogue routinely
// turns up dozens of findings, and sending them individually buries the chat:
// a real scan once produced 56 notifications in one go. Findings are grouped
// by watch, summarized into a headline, and followed by the three cheapest
// offers with their links.
//
// A watch that could not be delivered is reported, not just logged: its alerts
// are already recorded as fired, so the caller is the only one left that can
// tell the difference between "nothing to say" and "said nothing".
func (b *Bot) Deliver(ctx context.Context, alerts []tracker.Alert) error {
	byWatch := map[int64][]tracker.Alert{}
	order := make([]int64, 0, len(alerts))
	for _, a := range alerts {
		if _, seen := byWatch[a.Watch.ID]; !seen {
			order = append(order, a.Watch.ID)
		}
		byWatch[a.Watch.ID] = append(byWatch[a.Watch.ID], a)
	}

	var errs []error
	for _, watchID := range order {
		group := byWatch[watchID]
		w := group[0].Watch

		offers, err := b.store.WatchOffers(ctx, watchID, statsWindow, digestOffers)
		if err != nil {
			b.log.Error("digest offers failed", "watch", watchID, "err", err)
			errs = append(errs, err)
			continue
		}
		stats, err := b.store.Stats(ctx, watchID, statsWindow)
		if err != nil {
			b.log.Error("digest stats failed", "watch", watchID, "err", err)
			errs = append(errs, err)
			continue
		}
		if err := b.send(ctx, w.ChatID, formatDigest(w, group, offers, stats.Products, nil)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// --- pending target prompts ---

func (b *Bot) setPending(chatID int64, p pendingTarget) {
	p.expires = time.Now().Add(pendingTTL)

	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	b.pending[chatID] = p
}

func (b *Bot) takePending(chatID int64) (pendingTarget, bool) {
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()

	p, ok := b.pending[chatID]
	if !ok {
		return pendingTarget{}, false
	}
	delete(b.pending, chatID)
	if time.Now().After(p.expires) {
		return pendingTarget{}, false
	}
	return p, true
}

// --- shared loading ---

// loadRow reads a watch plus its stats, checking it belongs to the chat.
func (b *Bot) loadRow(ctx context.Context, chatID, watchID int64) (watchRow, error) {
	w, err := b.store.Watch(ctx, watchID)
	if err != nil {
		return watchRow{}, err
	}
	if w.ChatID != chatID {
		return watchRow{}, store.ErrNotFound
	}

	st, err := b.store.Stats(ctx, w.ID, statsWindow)
	if err != nil {
		return watchRow{}, err
	}
	return watchRow{Watch: *w, Stats: st}, nil
}

func (b *Bot) loadRows(ctx context.Context, chatID int64) ([]watchRow, error) {
	watches, err := b.store.Watches(ctx, chatID)
	if err != nil {
		return nil, err
	}

	rows := make([]watchRow, 0, len(watches))
	for _, w := range watches {
		st, err := b.store.Stats(ctx, w.ID, statsWindow)
		if err != nil {
			return nil, err
		}
		rows = append(rows, watchRow{Watch: w, Stats: st})
	}
	return rows, nil
}

func ptr[T any](v T) *T { return &v }

var errNoWatch = errors.New("telegram: watch not found for this chat")
