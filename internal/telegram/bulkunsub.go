package telegram

import (
	"context"
	"html"
	"log/slog"
	"strings"

	"github.com/mymmrac/telego"
)

// maxNamesInReceipt bounds the "removed these" line. Twenty names would push
// the list itself off the screen, which is the thing the receipt sits above.
const maxNamesInReceipt = 5

// handleSelection answers every selection button: flip a row, tick the page,
// clear, or apply.
//
// All four share a decoder and a staleness check, because all four address rows
// by position and a page that shifted underneath makes a position mean somebody
// else.
func (b *Bot) handleSelection(ctx context.Context, cq *telego.CallbackQuery, action, rest string, log *slog.Logger) {
	sel, index, ok := decodeSelection(rest)
	if !ok {
		log.Debug("malformed selection callback", "data", cq.Data)
		b.answerCallback(ctx, cq.ID, "Ця кнопка більше не працює.", log)
		return
	}

	items, _, err := b.subs.List(ctx, cq.From.ID, listPageSize, sel.page*listPageSize)
	if err != nil {
		log.Error("list subscriptions failed", "err", err)
		b.answerCallback(ctx, cq.ID, "Не вдалося прочитати підписки. Спробуй ще раз.", log)
		return
	}

	if pageDigest(items) != sel.digest {
		// The page changed between the tap that drew it and this one — another
		// device, or a catch-up that added nothing but a redraw. Refusing and
		// redrawing is the only safe answer: the ticks point at positions.
		b.answerCallback(ctx, cq.ID, "Список змінився — обери ще раз.", log)
		b.renderList(ctx, cq, sel.page, log)
		return
	}

	switch action {
	case cbPick:
		b.answerCallback(ctx, cq.ID, "", log)
		next := sel.toggle(index)
		b.renderListWith(ctx, cq, sel.page, &next, "", log)

	case cbPickAll:
		b.answerCallback(ctx, cq.ID, "", log)
		next := selectAll(sel.page, items)
		b.renderListWith(ctx, cq, sel.page, &next, "", log)

	case cbPickNone:
		b.answerCallback(ctx, cq.ID, "", log)
		next := newSelection(sel.page, items)
		b.renderListWith(ctx, cq, sel.page, &next, "", log)

	case cbPickApply:
		b.applySelection(ctx, cq, sel, log)

	default:
		b.answerCallback(ctx, cq.ID, "Ця кнопка більше не працює.", log)
	}
}

// applySelection unsubscribes from everything ticked.
func (b *Bot) applySelection(ctx context.Context, cq *telego.CallbackQuery, sel selection, log *slog.Logger) {
	items, _, err := b.subs.List(ctx, cq.From.ID, listPageSize, sel.page*listPageSize)
	if err != nil {
		log.Error("list subscriptions failed", "err", err)
		b.answerCallback(ctx, cq.ID, "Не вдалося прочитати підписки. Спробуй ще раз.", log)
		return
	}

	mbids, names, ok := sel.mbids(items)
	if !ok {
		b.answerCallback(ctx, cq.ID, "Список змінився — обери ще раз.", log)
		b.renderList(ctx, cq, sel.page, log)
		return
	}
	if len(mbids) == 0 {
		b.answerCallback(ctx, cq.ID, "Нічого не обрано.", log)
		return
	}

	removed, err := b.subs.UnsubscribeMany(ctx, cq.From.ID, mbids)
	if err != nil {
		log.Error("bulk unsubscribe failed", "artists", len(mbids), "err", err)
		b.answerCallback(ctx, cq.ID, "Не вдалося відписатись. Спробуй ще раз.", log)
		return
	}

	log.Info("bulk unsubscribed", "requested", len(mbids), "removed", removed)
	b.answerCallback(ctx, cq.ID, "Відписано", log)

	// Fresh selection, because every position after a removed row has shifted.
	// Keeping the old ticks would leave them pointing at whoever moved up.
	b.renderListWith(ctx, cq, sel.page, nil, unsubscribeReceipt(names), log)
}

// unsubscribeReceipt names what just went, so a mis-tap is visible rather than
// merely counted.
func unsubscribeReceipt(names []string) string {
	if len(names) == 0 {
		return ""
	}

	shown := names
	suffix := ""
	if len(shown) > maxNamesInReceipt {
		shown = shown[:maxNamesInReceipt]
		suffix = "…"
	}

	escaped := make([]string, len(shown))
	for i, n := range shown {
		escaped[i] = html.EscapeString(n)
	}
	return "✅ Відписано: " + strings.Join(escaped, ", ") + suffix
}
