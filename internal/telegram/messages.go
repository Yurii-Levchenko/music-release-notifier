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
