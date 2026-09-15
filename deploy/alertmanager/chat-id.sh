#!/bin/sh
# Prints the chat id to put in ALERT_CHAT_ID.
#
# This exists so the bot token never has to be pasted into a browser address
# bar. The usual instruction — open https://api.telegram.org/bot<TOKEN>/getUpdates
# — puts a live credential into browser history, into whatever syncs that
# history across devices, and quite often into a screenshot. Running it here
# keeps the token in the file it already lives in.
#
# Read-only against the Telegram API: getMe and getUpdates change nothing, and
# nothing is written to .env. The value is printed for you to paste, because a
# script that edits a file full of secrets is a worse trade than one line of
# copying.
set -eu

if [ -z "${ALERT_BOT_TOKEN:-}" ]; then
	echo "ALERT_BOT_TOKEN is not set in .env" >&2
	exit 1
fi

api() { wget -qO- "https://api.telegram.org/bot${ALERT_BOT_TOKEN}/$1" 2>/dev/null || true; }

me=$(api getMe)
case "$me" in
*'"ok":true'*)
	echo "bot: @$(echo "$me" | sed -n 's/.*"username":"\([^"]*\)".*/\1/p')"
	;;
"")
	echo "no response from Telegram — check network access" >&2
	exit 1
	;;
*)
	# Deliberately prints Telegram's description and not the token. A wrong
	# token and an unreachable network produce very different fixes.
	echo "token rejected: $(echo "$me" | sed -n 's/.*"description":"\([^"]*\)".*/\1/p')" >&2
	exit 1
	;;
esac

# Only private chats. A group id would also appear here, and an alert routed to
# a group is a different decision from an alert routed to you — not one to make
# by accident because a group happened to be first in the list.
ids=$(api getUpdates |
	tr '{' '\n' |
	sed -n 's/.*"id":\(-\?[0-9]*\),"first_name.*"type":"private".*/\1/p' |
	sort -u)

if [ -z "$ids" ]; then
	cat >&2 <<'MSG'

No chats found. Telegram only reports chats that have spoken to the bot, and a
bot cannot message anyone first (SPEC C1/C2) — so open the bot in Telegram and
press Start, then run this again.

If you pressed Start a while ago, press it again or send any message: Telegram
keeps undelivered updates for 24 hours and then drops them, so an old Start is
no longer visible here. Alertmanager itself never consumes them — it only calls
sendMessage — so nothing else is competing for this queue.
MSG
	exit 1
fi

echo
echo "put this in .env:"
for id in $ids; do
	echo "ALERT_CHAT_ID=$id"
done
