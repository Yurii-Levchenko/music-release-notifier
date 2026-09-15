#!/bin/sh
# Renders alertmanager.yml from the template, then hands off to Alertmanager.
#
# Alertmanager has no environment substitution of its own, and neither value it
# needs belongs in the repo: the bot token is a credential and the chat id is a
# personal Telegram account id.
#
# Invoked as `sh entrypoint.sh` from compose rather than as an executable, so it
# does not depend on the execute bit surviving a checkout on Windows, where
# core.filemode is off by default.
set -eu

fail() {
	echo "alertmanager: $1" >&2
	echo "alertmanager: refusing to start — set it in .env." >&2
	echo "alertmanager: create a second bot with @BotFather, send it one message," >&2
	echo "alertmanager: then read the chat id from" >&2
	echo "alertmanager:   https://api.telegram.org/bot<TOKEN>/getUpdates" >&2
	exit 1
}

# Refusing to start is the point, and it is the opposite of how the heartbeat
# degrades. A missing HEARTBEAT_URL leaves an app that still does its job, so it
# warns and carries on. Alertmanager has exactly one job: an instance that runs
# with no way to reach anybody still scrapes green on every dashboard while
# delivering nothing, and a silent alerting stack is worse than an absent one —
# an absent one is at least visible.
[ -n "${ALERT_BOT_TOKEN:-}" ] || fail "ALERT_BOT_TOKEN is empty"
[ -n "${ALERT_CHAT_ID:-}" ] || fail "ALERT_CHAT_ID is empty"

# Trim whitespace. Neither value can legitimately contain any, and a token
# copied out of BotFather with a trailing space fails as 401 Unauthorized — an
# error that reads like a wrong token rather than a stray character.
ALERT_BOT_TOKEN=$(printf '%s' "${ALERT_BOT_TOKEN}" | tr -d '[:space:]')
ALERT_CHAT_ID=$(printf '%s' "${ALERT_CHAT_ID}" | tr -d '[:space:]')

# sed is safe for these two specifically: a bot token is digits, a colon and
# [A-Za-z0-9_-], and a chat id is digits with an optional leading minus. Neither
# can contain the delimiter or an unescaped & that sed would expand.
sed -e "s|\${ALERT_BOT_TOKEN}|${ALERT_BOT_TOKEN}|g" \
	-e "s|\${ALERT_CHAT_ID}|${ALERT_CHAT_ID}|g" \
	/etc/alertmanager/alertmanager.yml.tmpl >/tmp/alertmanager.yml

# CI renders through this same script and then validates the result, so the
# substitution itself is covered rather than only the template it reads.
# Written as an if: `[ … ] && exit 0` returns non-zero when the test fails,
# which under `set -e` would abort every normal startup.
if [ "${RENDER_ONLY:-}" = "1" ]; then
	exit 0
fi

exec /bin/alertmanager \
	--config.file=/tmp/alertmanager.yml \
	--storage.path=/alertmanager
