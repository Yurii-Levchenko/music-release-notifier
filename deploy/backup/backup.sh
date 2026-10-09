#!/bin/sh
# Periodic pg_dump with verification and rotation.
#
# Runs in its own container off the same postgres image as the database, so
# pg_dump is never older than the server it is dumping — a mismatch that fails
# loudly on the dump but silently produces an unrestorable archive when the
# versions drift the other way.
#
# Logs one JSON line per run, shaped like the application's slog output, so
# Alloy lifts `level` into a Loki label and a failed backup lands in the same
# "Warnings and errors" panel as everything else. A backup job that fails where
# nobody is looking is the default outcome, and it is the one this avoids.
set -eu

DIR=${BACKUP_DIR:-/backups}
KEEP=${BACKUP_KEEP:-7}
INTERVAL=${BACKUP_INTERVAL_SECONDS:-86400}
RETRY=${BACKUP_RETRY_SECONDS:-300}
DB_WAIT=${BACKUP_DB_WAIT_SECONDS:-120}

# A failing attempt backs off from RETRY by doubling, up to this. Without a cap
# a database that is down for a day would leave the next attempt a day away
# from its recovery; without the doubling the same day would write 288 ERROR
# lines into the panel people read.
RETRY_MAX=3600

log() {
	# $1 level, $2 msg, $3 optional trailing JSON fields (with leading comma).
	printf '{"time":"%s","level":"%s","msg":"%s"%s}\n' \
		"$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1" "$2" "${3:-}"
}

# Every one of these ends up in arithmetic or in sleep, where a typo would
# either abort mid-loop or, for an interval of 0, dump on every iteration.
for setting in "KEEP=$KEEP" "INTERVAL=$INTERVAL" "RETRY=$RETRY" "DB_WAIT=$DB_WAIT"; do
	case ${setting#*=} in
	'' | *[!0-9]* | 0)
		log ERROR "invalid setting, want a positive integer" ",\"setting\":\"$setting\""
		exit 1
		;;
	esac
done
if [ "$RETRY" -gt "$RETRY_MAX" ]; then
	RETRY_MAX=$RETRY
fi

# Sleeps in the background and waits on it. A foreground sleep defers traps
# until it returns, and sh as PID 1 has no default action for SIGTERM: with a
# plain sleep the worker ignored every `docker compose down` until the 10 s
# kill timeout.
pause() {
	sleep "$1" &
	wait $! || true
}

trap 'log INFO "backup worker stopping"; exit 0' TERM INT

# Strips what would break the JSON above. Postgres error text is free-form and
# routinely contains quotes and newlines.
sanitize() {
	# shellcheck disable=SC1003 # deletes " and \, not an attempt to escape a quote
	tr -d '"\\' | tr '\n\r\t' '   ' | cut -c1-400
}

count_dumps() {
	find "$DIR" -maxdepth 1 -type f -name '*.dump' | wc -l | tr -d ' '
}

rotate() {
	total=$(count_dumps)
	excess=$((total - KEEP))
	# An if, not `[ … ] && return`: under set -e that idiom makes the function
	# return the status of a test that was merely false, which the caller then
	# reads as a failed rotation.
	if [ "$excess" -le 0 ]; then
		return 0
	fi

	# Names are UTC ISO-8601, so lexicographic order is chronological order —
	# no dependence on mtime, which a volume copy or a restore would rewrite.
	find "$DIR" -maxdepth 1 -type f -name '*.dump' | sort | head -n "$excess" |
		while read -r old; do
			rm -f "$old"
			log INFO "pruned an old dump" ",\"file\":\"$(basename "$old")\""
		done
}

run_backup() {
	stamp=$(date -u +%Y%m%dT%H%M%SZ)
	final="$DIR/releaseradar-$stamp.dump"
	tmp="$final.tmp"

	# --no-owner and --no-privileges so the dump restores into a database whose
	# role is not called releaseradar. That is not hypothetical: the first
	# restore will be onto a fresh VPS, and a dump that insists on the original
	# owner fails there, at the worst possible moment to discover it.
	if ! err=$(pg_dump --format=custom --no-owner --no-privileges --file="$tmp" 2>&1); then
		rm -f "$tmp"
		log ERROR "backup failed: pg_dump" ",\"detail\":\"$(printf '%s' "$err" | sanitize)\""
		return 1
	fi

	# Verify before it counts as a backup. An unverified file in the directory
	# would be worse than no file at all: it satisfies the freshness alert while
	# being unrestorable, so the first time anybody finds out is the restore.
	#
	# A full decode, not `pg_restore --list`. --list was the obvious choice and
	# it is not enough — measured: it reads only the table of contents, which
	# lives at the front of the archive, so it catches truncation and an empty
	# file but accepts a dump whose tail has been overwritten with garbage.
	# Decoding the whole archive to /dev/null catches the corrupted tail ("out
	# of memory" on a bad length field) and a corrupted middle ("could not read
	# from input file"), and on this database it costs under a millisecond. The
	# cost scales with the dump, and it is the right thing to spend it on: this
	# step is the entire difference between having a backup and having a file.
	if ! err=$(pg_restore --file=/dev/null "$tmp" 2>&1); then
		rm -f "$tmp"
		log ERROR "backup failed: dump did not verify" ",\"detail\":\"$(printf '%s' "$err" | sanitize)\""
		return 1
	fi

	# Rename last. Until this line the file ends in .tmp, which neither the
	# metrics collector nor rotation matches, so a dump interrupted halfway can
	# never be mistaken for a complete one.
	mv "$tmp" "$final"
	size=$(wc -c <"$final" | tr -d ' ')
	log INFO "backup complete" ",\"file\":\"$(basename "$final")\",\"size_bytes\":$size"

	# Only after a success. Rotating unconditionally would mean a job that has
	# been failing for a week quietly deletes the last good backup it has —
	# the failure mode that turns a bad day into an unrecoverable one.
	rotate
}

mkdir -p "$DIR"

# Clear leftovers from a container killed mid-dump. They are unverified by
# definition and nothing reads them, but they occupy the disk the next dump
# needs.
find "$DIR" -maxdepth 1 -type f -name '*.dump.tmp' -exec rm -f {} + 2>/dev/null || true

# Seconds since the newest dump was written; empty when there is none. The
# newest by mtime, which is the clock internal/backup reads for the freshness
# metric, so this schedule and the BackupStale alert cannot disagree about how
# old the last backup is.
newest_age() {
	newest=$(find "$DIR" -maxdepth 1 -type f -name '*.dump' -exec stat -c %Y {} + | sort -n | tail -n 1)
	if [ -z "$newest" ]; then
		return 0
	fi
	age=$(($(date +%s) - newest))
	# A dump stamped in the future (a clock change, a restored copy) counts as
	# just written rather than asking for a sleep longer than the interval.
	if [ "$age" -lt 0 ]; then
		age=0
	fi
	echo "$age"
}

# Waits, up to DB_WAIT, for the server to accept connections. On every boot
# of the machine Docker restarts db and backup at the same moment: depends_on
# is honoured by `compose up`, not by the daemon's restart policy. So the
# first attempt of every boot met "Connection refused" or "the database system
# is starting up" — measured: every boot from 16.09 to 06.10, 20 days without
# a dump. Not ready by the deadline is not an error here: the dump runs anyway
# and fails with pg_dump's own message, which says more than a timeout would.
wait_for_db() {
	if pg_isready --quiet --timeout=5; then
		return 0
	fi
	log INFO "database not accepting connections yet, waiting" ",\"max_seconds\":$DB_WAIT"
	deadline=$(($(date +%s) + DB_WAIT))
	until pg_isready --quiet --timeout=5; do
		if [ "$(date +%s)" -ge "$deadline" ]; then
			return 0
		fi
		pause 2
	done
}

log INFO "backup worker started" ",\"dir\":\"$DIR\",\"keep\":$KEEP,\"interval_seconds\":$INTERVAL,\"retry_seconds\":$RETRY"

backoff=$RETRY
while :; do
	# Due once the newest dump is INTERVAL old, counted from that dump rather
	# than from this process's start. Counting from the start meant a restart
	# 23 hours after a dump put the next one 47 hours after it, and a machine
	# that restarts daily would never reach the next one at all. It also keeps
	# `docker compose up -d` from taking a fresh dump each time, which would
	# spend the retention window on copies of the same minute.
	age=$(newest_age)
	if [ -n "$age" ] && [ "$age" -lt "$INTERVAL" ]; then
		due=$((INTERVAL - age))
		log INFO "next backup scheduled" ",\"in_seconds\":$due"
		pause "$due"
		continue
	fi

	wait_for_db
	if run_backup; then
		backoff=$RETRY
		continue
	fi

	# A failure retries in minutes, not after a full interval. Sleeping the
	# interval was the 20-day outage: the one attempt per boot failed, and the
	# next was a day away — further than the machine stayed up.
	log INFO "backup will be retried" ",\"in_seconds\":$backoff"
	pause "$backoff"
	backoff=$((backoff * 2))
	if [ "$backoff" -gt "$RETRY_MAX" ]; then
		backoff=$RETRY_MAX
	fi
done
