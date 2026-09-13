package telegram

import (
	"errors"
	"strings"
	"time"

	"github.com/mymmrac/telego/telegoapi"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/notify"
)

// classify maps a Bot API failure onto the three-way disposition the outbox
// understands. This is the whole reason nothing above internal/notify has to
// know what a Telegram error looks like.
//
// Two rules matter more than the table itself:
//
//  1. Never widen "permanent" by accident. A permanent verdict deletes
//     someone's subscriptions, so anything unrecognized stays transient.
//  2. 401 is never permanent. A bad token makes every send fail; treating that
//     as "the user blocked us" would wipe the entire subscription table on one
//     bad deploy.
//
// The Bot API docs warn that error_code "contents are subject to change", and
// there is no stable machine-readable error id, so matching on the description
// substring is unavoidable rather than sloppy.
func classify(err error) (notify.Disposition, time.Duration) {
	var apiErr *telegoapi.Error
	if !errors.As(err, &apiErr) {
		// Transport failure, context cancellation, JSON decode error. Retry.
		return notify.Transient, 0
	}

	desc := strings.ToLower(apiErr.Description)

	switch apiErr.ErrorCode {
	case 429:
		// Honor retry_after verbatim. Ignoring it escalates to much longer
		// lockouts, so this value is not a suggestion.
		var wait time.Duration
		if apiErr.Parameters != nil && apiErr.Parameters.RetryAfter > 0 {
			wait = time.Duration(apiErr.Parameters.RetryAfter) * time.Second
		}
		return notify.Transient, wait

	case 403:
		// The bot cannot talk to this chat, and cannot fix that by retrying.
		return notify.Permanent, 0

	case 400:
		switch {
		// The chat is gone or was never reachable.
		case contains(desc,
			"chat not found",
			"private chat not found",
			"peer_id_invalid",
			"user not found"):
			return notify.Permanent, 0

		// A migrated group keeps working under a new id. Not this project's
		// case today (we only message private chats), but mapping it to
		// permanent would silently drop a live chat, so keep it transient and
		// let the caller notice migrate_to_chat_id.
		case strings.Contains(desc, "upgraded to a supergroup"):
			return notify.Transient, 0

		// Our bug: the message we built is malformed. Dropping the user's
		// subscriptions over a broken caption would be absurd.
		case contains(desc,
			"can't parse entities",
			"message is too long",
			"caption is too long",
			"reply markup is too long",
			"wrong file identifier",
			"wrong remote file identifier",
			"failed to get http url content",
			"wrong type of the web page content"):
			return notify.BadMessage, 0
		}
		// Unknown 400 — probably ours, but do not guess. Retry and let it
		// surface in the logs and the attempts counter.
		return notify.Transient, 0

	case 401:
		// Bad or revoked token. Global outage, never the user's fault.
		return notify.Transient, 0

	case 409:
		// Two pollers, or a webhook still registered. Deployment problem.
		return notify.Transient, 0
	}

	// 5xx and anything else: Telegram's side, retry with backoff. Note the body
	// on a 502 can be non-JSON HTML, which lands here as a non-*telegoapi.Error.
	return notify.Transient, 0
}

func contains(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// wrap turns a Bot API error into the error type the outbox expects.
func wrap(err error) error {
	if err == nil {
		return nil
	}
	disposition, retryAfter := classify(err)
	return &notify.DeliveryError{
		Disposition: disposition,
		RetryAfter:  retryAfter,
		Err:         err,
	}
}

// IsUserGone reports whether an error means this chat is unreachable for good.
// Used by the bot worker to mark a user blocked without going through the
// outbox.
func IsUserGone(err error) bool {
	d, _ := classify(err)
	return d == notify.Permanent
}
