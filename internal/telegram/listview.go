package telegram

import (
	"fmt"
	"html"
	"strconv"
	"strings"

	"github.com/mymmrac/telego"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/storage"
)

// Callback prefixes for the selection buttons.
const (
	cbPick      = "pick"  // flip one row
	cbPickAll   = "pall"  // tick every row on the page
	cbPickNone  = "pnone" // clear
	cbPickApply = "pdrop" // unsubscribe from what is ticked
)

// renderList draws one page of /list, with whatever is currently ticked.
//
// The numbers select rather than unsubscribe. They used to unsubscribe on the
// first tap, with no confirmation — which made bulk tidying impossible and a
// mis-tap irreversible at the same time. Selecting first fixes both: several
// artists can go at once, and nothing goes until a second, deliberate press.
//
// What a bulk unsubscribe removed is reported in a message of its own rather
// than here. This one is edited in place on every tap, so a receipt written
// into it would vanish the moment somebody pressed anything else — and a
// receipt exists precisely so a wrong choice stays visible afterwards.
func renderList(
	items []storage.Subscription, total, page int, sel selection,
) (string, *telego.InlineKeyboardMarkup) {
	var b strings.Builder

	fmt.Fprintf(&b, "🔔 <b>Твої підписки</b> — %d\n\n", total)

	offset := page * listPageSize
	for i, item := range items {
		mark := ""
		if sel.selected(i) {
			mark = "✓ "
		}
		fmt.Fprintf(&b, "%s%d. %s\n", mark, offset+i+1, html.EscapeString(item.Name))
	}

	if n := sel.count(); n > 0 {
		fmt.Fprintf(&b, "\nОбрано: %d. Натисни «Відписатись», щоб підтвердити.", n)
	} else {
		b.WriteString("\nНатисни номери, щоб обрати кого прибрати.")
	}

	pages := (total + listPageSize - 1) / listPageSize
	if pages > 1 {
		fmt.Fprintf(&b, "\nСторінка %d з %d.", page+1, pages)
	}

	return b.String(), listKeyboard(items, page, pages, sel)
}

func listKeyboard(items []storage.Subscription, page, pages int, sel selection) *telego.InlineKeyboardMarkup {
	var rows [][]telego.InlineKeyboardButton

	const perRow = 5
	offset := page * listPageSize
	for i := range items {
		if i%perRow == 0 {
			rows = append(rows, nil)
		}
		label := strconv.Itoa(offset + i + 1)
		if sel.selected(i) {
			label = "✓" + label
		}
		rows[len(rows)-1] = append(rows[len(rows)-1], telego.InlineKeyboardButton{
			Text: label,
			// The button carries the selection as it stands plus the row to
			// flip, and the handler does the flipping. That way every button on
			// the page describes the same state, and none of it lives on the
			// server.
			CallbackData: sel.encode(cbPick, i),
		})
	}

	switch {
	case sel.count() > 0:
		rows = append(rows, []telego.InlineKeyboardButton{
			{
				Text:         fmt.Sprintf("🗑 Відписатись (%d)", sel.count()),
				CallbackData: sel.encode(cbPickApply, -1),
			},
			{
				Text:         "Зняти вибір",
				CallbackData: sel.encode(cbPickNone, -1),
			},
		})
	case len(items) > 1:
		// "All" is how somebody clears the whole list, so there is no separate
		// destructive command to build or to guard — it ticks the page and then
		// goes through the same confirmation as any other selection.
		rows = append(rows, []telego.InlineKeyboardButton{{
			Text:         "Обрати всі на сторінці",
			CallbackData: sel.encode(cbPickAll, -1),
		}})
	}

	if pages > 1 {
		var nav []telego.InlineKeyboardButton
		if page > 0 {
			nav = append(nav, telego.InlineKeyboardButton{
				Text: "◀", CallbackData: cbList + ":" + strconv.Itoa(page-1),
			})
		}
		nav = append(nav, telego.InlineKeyboardButton{
			Text:         fmt.Sprintf("%d / %d", page+1, pages),
			CallbackData: cbNoop,
		})
		if page < pages-1 {
			nav = append(nav, telego.InlineKeyboardButton{
				Text: "▶", CallbackData: cbList + ":" + strconv.Itoa(page+1),
			})
		}
		rows = append(rows, nav)
	}

	return &telego.InlineKeyboardMarkup{InlineKeyboard: rows}
}
