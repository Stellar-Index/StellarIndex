#!/bin/sh
# pgbackrest-wal-drop-probe: publish when pgBackRest last dropped queued WAL.
# At archive-push-queue-max the async archiver discards the queue and reports
# success to Postgres, so this log line is the only record that point-in-time
# recovery has a gap until the next backup.
set -eu
LOG_DIR="${PGBACKREST_LOG_DIR:-/var/log/pgbackrest}"
DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"

if [ -d "$LOG_DIR" ] && [ ! -r "$LOG_DIR" ]; then
  echo "pgbackrest-wal-drop-probe: cannot read $LOG_DIR; keeping the previous textfile" >&2
  exit 1
fi
last=0
for f in "$LOG_DIR"/*archive-push*.log; do
  [ -e "$f" ] || continue
  if [ ! -r "$f" ]; then
    echo "pgbackrest-wal-drop-probe: cannot read $f; keeping the previous textfile" >&2
    exit 1
  fi
  line=$(grep 'dropped WAL file' "$f" | tail -n 1)
  [ -n "$line" ] || continue
  # Lines start "YYYY-MM-DD HH:MM:SS.mmm"; a line date cannot parse fails the run.
  ts=$(date -d "$(printf '%s' "$line" | cut -c1-19)" +%s)
  if [ "$ts" -gt "$last" ]; then last=$ts; fi
done

tmp=$(mktemp "$DIR/pgbackrest_wal_drop.prom.XXXXXX")
trap 'rm -f "$tmp"' EXIT
{
  echo "# HELP stellarindex_pgbackrest_wal_last_drop_unix Unix time of the newest 'dropped WAL file' warning in pgBackRest's archive-push logs; 0 when none."
  echo "# TYPE stellarindex_pgbackrest_wal_last_drop_unix gauge"
  echo "stellarindex_pgbackrest_wal_last_drop_unix $last"
} > "$tmp"
# mktemp creates 0600 and node_exporter silently skips a file it cannot read.
chmod 644 "$tmp"
mv "$tmp" "$DIR/pgbackrest_wal_drop.prom"
trap - EXIT
