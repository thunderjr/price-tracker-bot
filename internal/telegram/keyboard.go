package telegram

import (
	"fmt"

	"github.com/go-telegram/bot/models"

	"github.com/thunderjr/price-tracker-bot/internal/source"
)

// pageSize is how many watches fit on one page of the manager.
const pageSize = 8

func button(text, action string, watchID, arg int64) models.InlineKeyboardButton {
	return models.InlineKeyboardButton{Text: text, CallbackData: encode(action, watchID, arg)}
}

// listKeyboard builds the manager's list view: one button per watch, plus
// paging when there are more than a screenful.
func listKeyboard(rows []watchRow, page int64) *models.InlineKeyboardMarkup {
	start := int(page) * pageSize
	if start < 0 || start >= len(rows) {
		start = 0
		page = 0
	}
	end := min(start+pageSize, len(rows))

	kb := make([][]models.InlineKeyboardButton, 0, end-start+2)
	for _, r := range rows[start:end] {
		kb = append(kb, []models.InlineKeyboardButton{
			button(watchButtonLabel(r), actDetail, r.Watch.ID, page),
		})
	}

	if pages := (len(rows) + pageSize - 1) / pageSize; pages > 1 {
		var nav []models.InlineKeyboardButton
		if page > 0 {
			nav = append(nav, button("‹", actList, 0, page-1))
		}
		nav = append(nav, button(fmt.Sprintf("%d/%d", page+1, int64(pages)), actNoop, 0, 0))
		if int(page) < pages-1 {
			nav = append(nav, button("›", actList, 0, page+1))
		}
		kb = append(kb, nav)
	}

	if len(rows) > 0 {
		kb = append(kb, []models.InlineKeyboardButton{
			button("🔄 Escanear tudo", actScanAll, 0, page),
			button("✕ Fechar", actClose, 0, 0),
		})
	} else {
		kb = append(kb, []models.InlineKeyboardButton{button("✕ Fechar", actClose, 0, 0)})
	}
	return &models.InlineKeyboardMarkup{InlineKeyboard: kb}
}

// watchButtonLabel packs the query, its best price and its state into one line.
// Button text is plain -- Telegram does not render markup here, so it must not
// be escaped.
func watchButtonLabel(r watchRow) string {
	label := truncate(r.Watch.Query, 22)
	if r.Stats.Products == 0 {
		label += " · —"
	} else {
		label += " · " + source.FormatBRL(r.Stats.BestCents)
	}
	if !r.Watch.Active {
		label += " ⏸"
	}
	if r.Watch.TargetCents > 0 && r.Stats.Products > 0 && r.Stats.BestCents <= r.Watch.TargetCents {
		label += " 🎯"
	}
	return label
}

// detailKeyboard builds the per-watch action grid.
func detailKeyboard(r watchRow, page int64) *models.InlineKeyboardMarkup {
	pause := "⏸ Pausar"
	if !r.Watch.Active {
		pause = "▶️ Retomar"
	}

	return &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{
		{
			button("🔄 Escanear", actScan, r.Watch.ID, page),
			button("🎯 Alvo", actTarget, r.Watch.ID, page),
		},
		{
			button(pause, actTogglePause, r.Watch.ID, page),
			button("🗑 Remover", actDeleteAsk, r.Watch.ID, page),
		},
		{
			button("🛒 Ver ofertas", actOffers, r.Watch.ID, page),
			button("‹ Voltar", actList, 0, page),
		},
	}}
}

// confirmDeleteKeyboard replaces the actions with a yes/no, in place, so a
// stray tap on the bin never destroys anything.
func confirmDeleteKeyboard(watchID, page int64) *models.InlineKeyboardMarkup {
	return &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{
		button("✓ Confirmar", actDeleteDo, watchID, page),
		button("✕ Cancelar", actDetail, watchID, page),
	}}}
}

// backKeyboard is the single control shown under a transient view.
func backKeyboard(watchID, page int64) *models.InlineKeyboardMarkup {
	return &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{
		button("‹ Voltar", actDetail, watchID, page),
	}}}
}
