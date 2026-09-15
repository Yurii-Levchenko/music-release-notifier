package telegram

import (
	"fmt"
	"html"
)

// User-facing strings that more than one path needs, kept together so the
// wording can be reviewed as a set rather than hunted through handlers.
//
// The rule they follow: say what happened and what to do next, never apologize
// and never blame the user. "MusicBrainz is unavailable" is information;
// "something went wrong" is not.
const (
	searchingText = "🔍 Шукаю…"

	// Prefixed to a card built from expired cache. The results are real and
	// usable, so this is a caveat, not a warning — but staying silent about it
	// would be presenting old data as current.
	staleNotice = "⚠️ <i>MusicBrainz зараз недоступний — показую збережені результати.</i>\n\n"
)

// upstreamDownText is the genuine dead end: upstream failed and nothing was
// cached. It names the cause, because "try again later" without a reason reads
// as the user having done something wrong.
func upstreamDownText() string {
	return "MusicBrainz зараз не відповідає, і збережених результатів для цього " +
		"запиту в мене немає.\n\nСпробуй ще раз за кілька хвилин."
}

func noResultsText(query string) string {
	return fmt.Sprintf(
		"Не знайшов нікого за запитом <b>%s</b>.\n\n"+
			"Спробуй іншу назву — або перевір написання на musicbrainz.org.",
		html.EscapeString(query))
}

// linkText answers a pasted URL.
//
// This is not a hypothetical input. A Spotify track link was pasted into this
// bot on 12.09.2026, went upstream as a search query, and came back with five
// unrelated artists — the search cache still holds the row. Reaching for a
// Spotify link is the natural thing to do in a bot about music releases, and
// answering it with nonsense teaches nothing about what would work.
//
// Spotify gets its own sentence because it is the link people actually have,
// and because "not supported" and "not supported yet" are different promises.
// Resolving one is not a small feature: the Web API needs an app owner with
// active Premium (C12), and the developer terms forbid passing Spotify content
// to another service (C28). The extension in S9 goes the other way round —
// subscribing from inside Spotify — which is why this says nothing about when.
func linkText(spotify bool) string {
	if spotify {
		return "Я шукаю виконавців за іменем, а не за посиланням — прочитати лінк " +
			"Spotify я поки не вмію.\n\nНапиши ім'я виконавця, і я знайду його."
	}
	return "Це схоже на посилання. Я шукаю виконавців за іменем — " +
		"напиши, будь ласка, саме ім'я."
}
