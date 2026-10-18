#!/bin/sh
# Mutes or unmutes an alert, and says so in the alert chat.
#
# Runs inside the alertmanager container (`make silence` / `make unsilence`),
# where amtool and the alert bot's token already are — the token never appears
# on the host's command line.
#
# Why announce it: a silence set from a laptop terminal is invisible from the
# chat where the alert arrived. Without the message, the chat shows an alert
# that simply stops repeating, which looks exactly like alerting having broken.
#
#   ACTION=add    ALERT=<alertname> FOR=<duration> REASON=<text>
#   ACTION=expire ID=<silence id>
set -eu

AM="amtool --alertmanager.url=http://localhost:9093"

# Plain text, no parse mode (same reason as the alert template). Quotes and
# backslashes are dropped from free text so the JSON body stays valid.
say() {
	# shellcheck disable=SC1003 # deletes " and \, not an attempt to escape a quote
	text=$(printf '%s' "$1" | tr -d '"\\')
	wget -qO- --header 'Content-Type: application/json' \
		--post-data "{\"chat_id\":${ALERT_CHAT_ID},\"text\":\"${text}\"}" \
		"https://api.telegram.org/bot${ALERT_BOT_TOKEN}/sendMessage" >/dev/null 2>&1 ||
		echo "silence is set, but the alert chat could not be told" >&2
}

case "${ACTION:-}" in
add)
	id=$($AM silence add alertname="$ALERT" --duration="$FOR" --comment="$REASON" --author=owner)
	echo "$id"
	say "🔕 ${ALERT} заглушено на ${FOR}. Причина: ${REASON}. Зняти: make unsilence ID=${id}"
	;;
expire)
	$AM silence expire "$ID"
	say "🔔 Глушіння ${ID} знято, алерт знову надсилатиметься."
	;;
*)
	echo "ACTION must be add or expire" >&2
	exit 2
	;;
esac
