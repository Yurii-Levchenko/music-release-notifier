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

log() {
	# $1 level, $2 msg, $3 optional trailing JSON fields (with leading comma).
	printf '{"time":"%s","level":"%s","msg":"%s"%s}\n' \
		"$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1" "$2" "${3:-}"
}

# Strips what would break the JSON above. Postgres error text is free-form and
# routinely contains quotes and newlines.
sanitize() {
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

log INFO "backup worker started" ",\"dir\":\"$DIR\",\"keep\":$KEEP,\"interval_seconds\":$INTERVAL"

# find takes minutes, and integer division floors: any interval under a minute
# would ask for "-mmin -0", match nothing, and silently turn the skip below
# into a no-op that dumps on every loop.
SKIP_MINUTES=$((INTERVAL / 60))
if [ "$SKIP_MINUTES" -lt 1 ]; then
	SKIP_MINUTES=1
fi

while :; do
	# Skip the dump if a recent one already exists. Without this, every
	# `docker compose up -d` takes a fresh dump, and a machine that restarts
	# often would spend its retention window on copies of the same minute
	# while genuinely old days age out of it.
	recent=$(find "$DIR" -maxdepth 1 -type f -name '*.dump' -mmin "-$SKIP_MINUTES" | wc -l | tr -d ' ')
	if [ "$recent" -gt 0 ]; then
		log INFO "recent dump exists, skipping this run"
	else
		run_backup || true
	fi
	sleep "$INTERVAL"
done
