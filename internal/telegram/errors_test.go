package telegram

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mymmrac/telego/telegoapi"

	"github.com/Yurii-Levchenko/music-release-notifier/internal/notify"
)

func apiErr(code int, desc string) error {
	return &telegoapi.Error{ErrorCode: code, Description: desc}
}

// The error strings below are the real ones emitted by the official Bot API
// server, taken from its source rather than from a blog post.
func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want notify.Disposition
	}{
		// Permanent: this chat can never be reached again, drop the subscription.
		{"blocked by user", apiErr(403, "Forbidden: bot was blocked by the user"), notify.Permanent},
		{"deactivated", apiErr(403, "Forbidden: user is deactivated"), notify.Permanent},
		{"cannot initiate", apiErr(403, "Forbidden: bot can't initiate conversation with a user"), notify.Permanent},
		{"kicked", apiErr(403, "Forbidden: bot was kicked from the group chat"), notify.Permanent},
		{"chat not found", apiErr(400, "Bad Request: chat not found"), notify.Permanent},
		{"private chat not found", apiErr(400, "Bad Request: private chat not found"), notify.Permanent},
		{"peer id invalid", apiErr(400, "Bad Request: PEER_ID_INVALID"), notify.Permanent},

		// Our bug: keep the subscription, drop only this message.
		{"parse entities", apiErr(400, "Bad Request: can't parse entities: unsupported start tag"), notify.BadMessage},
		{"message too long", apiErr(400, "Bad Request: message is too long"), notify.BadMessage},
		{"markup too long", apiErr(400, "Bad Request: reply markup is too long"), notify.BadMessage},
		{"bad photo url", apiErr(400, "Bad Request: failed to get HTTP URL content"), notify.BadMessage},

		// Transient: retry.
		{"flood", apiErr(429, "Too Many Requests: retry after 35"), notify.Transient},
		{"internal", apiErr(500, "Internal Server Error"), notify.Transient},
		{"bad gateway", apiErr(502, "Bad Gateway"), notify.Transient},
		{"conflict", apiErr(409, "Conflict: terminated by other getUpdates request"), notify.Transient},
		{"migrated", apiErr(400, "Bad Request: group chat was upgraded to a supergroup chat"), notify.Transient},

		// A bad token fails every send. Treating it as permanent would delete
		// every subscription in the database on one bad deploy.
		{"unauthorized is NOT permanent", apiErr(401, "Unauthorized"), notify.Transient},

		// Anything unrecognized must not widen "permanent".
		{"unknown 400", apiErr(400, "Bad Request: something brand new"), notify.Transient},
		{"non-api error", errors.New("dial tcp: connection refused"), notify.Transient},
		{"nil-safe wrapped", fmt.Errorf("send: %w", apiErr(500, "Internal Server Error")), notify.Transient},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := classify(tc.err)
			if got != tc.want {
				t.Fatalf("classify(%v) = %s, want %s", tc.err, got, tc.want)
			}
		})
	}
}

// retry_after lives inside parameters, not at the top level, and is in seconds.
// Getting this wrong turns one 429 into a multi-hour lockout.
func TestClassifyHonorsRetryAfter(t *testing.T) {
	err := &telegoapi.Error{
		ErrorCode:   429,
		Description: "Too Many Requests: retry after 35",
		Parameters:  &telegoapi.ResponseParameters{RetryAfter: 35},
	}

	disposition, wait := classify(err)
	if disposition != notify.Transient {
		t.Fatalf("disposition = %s, want transient", disposition)
	}
	if wait != 35*time.Second {
		t.Fatalf("retry after = %v, want 35s", wait)
	}
}

func TestClassify429WithoutParameters(t *testing.T) {
	// Some 429s arrive with no parameters block; the caller's backoff applies.
	_, wait := classify(apiErr(429, "Too Many Requests"))
	if wait != 0 {
		t.Fatalf("retry after = %v, want 0", wait)
	}
}

func TestWrapProducesDeliveryError(t *testing.T) {
	err := wrap(apiErr(403, "Forbidden: bot was blocked by the user"))

	var de *notify.DeliveryError
	if !errors.As(err, &de) {
		t.Fatalf("wrap did not produce *notify.DeliveryError, got %T", err)
	}
	if de.Disposition != notify.Permanent {
		t.Fatalf("disposition = %s, want permanent", de.Disposition)
	}
	// The original must stay reachable for logging.
	var apiE *telegoapi.Error
	if !errors.As(err, &apiE) {
		t.Fatal("original telegoapi.Error is not unwrappable")
	}
}

func TestWrapNilIsNil(t *testing.T) {
	if err := wrap(nil); err != nil {
		t.Fatalf("wrap(nil) = %v, want nil", err)
	}
}

// DispositionOf is what the outbox calls. An unrecognized error reaching it must
// default to transient, never to deleting subscriptions.
func TestDispositionOfUnknownErrorIsTransient(t *testing.T) {
	d, _ := notify.DispositionOf(errors.New("something we have never seen"))
	if d != notify.Transient {
		t.Fatalf("DispositionOf(unknown) = %s, want transient", d)
	}
}

func TestIsUserGone(t *testing.T) {
	if !IsUserGone(apiErr(403, "Forbidden: bot was blocked by the user")) {
		t.Fatal("blocked user should count as gone")
	}
	if IsUserGone(apiErr(429, "Too Many Requests: retry after 5")) {
		t.Fatal("rate limiting must not count as gone")
	}
	if IsUserGone(apiErr(401, "Unauthorized")) {
		t.Fatal("bad token must not count as the user being gone")
	}
}

// Telegram failing to download our cover URL and Telegram rejecting the URL
// itself are both 400s classified BadMessage, but only the first can succeed
// on a retry. Conflating them silently downgraded a release to a text-only
// message because archive.org was slow for two seconds (seen live 09.09.2026).
func TestCoverFetchFailureIsDistinguishedFromABadURL(t *testing.T) {
	retryable := []string{
		"telego: sendPhoto: api: 400 \"Bad Request: failed to get HTTP URL content\"",
		"Bad Request: WEBPAGE_CURL_FAILED",
		"Bad Request: IMAGE_PROCESS_FAILED",
	}
	for _, msg := range retryable {
		if !isCoverFetchFailure(errors.New(msg)) {
			t.Errorf("a fetch failure was treated as hopeless: %q", msg)
		}
	}

	hopeless := []string{
		"Bad Request: wrong file identifier/HTTP URL specified",
		"Bad Request: can't parse entities",
		"Bad Request: caption is too long",
	}
	for _, msg := range hopeless {
		if isCoverFetchFailure(errors.New(msg)) {
			t.Errorf("retrying this cannot help and only delays the text message: %q", msg)
		}
	}

	if isCoverFetchFailure(nil) {
		t.Error("nil was reported as a fetch failure")
	}
}
