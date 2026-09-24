#!/usr/bin/env bash
# ch-lake-backup.sh — the ClickHouse lake's DATA backup (ADR-0043 §2.4).
#
# ch-schema-snapshot.sh keeps the DDL; this keeps the rows. Without it the
# lake's only recovery path is a re-derive from the Galexie archive, which
# off-site-backup-plan.md measures at ~1–2 weeks against a restore measured
# in hours.
#
# Mechanism: ClickHouse's native BACKUP DATABASE into a backup disk that the
# role declares in config.d (an s3_plain disk on off-site object storage, so
# the credentials live in server config and never in a query text or the
# query_log). No third-party tool.
#
#   - A chain is one FULL backup plus the daily INCREMENTALS taken on top of
#     it (each `base_backup` = the previous link, so a daily ships only the
#     parts that are new since yesterday).
#   - A new chain starts every FULL_INTERVAL_DAYS. Older chains are removed
#     ONLY after the new full has been confirmed BACKUP_CREATED, keeping the
#     newest RETAIN_CHAINS — so there is never a moment with no restorable
#     chain.
#   - Local state (STATE_DIR) records the chain; a lost or unreadable state
#     file starts a new full, which is the expensive-but-correct direction.
#
# Emits (node_exporter textfile collector):
#   stellarindex_ch_lake_backup_configured      1 iff BACKUP_DISK is set
#   stellarindex_ch_lake_backup_last_success_unix   (clean runs only)
#   stellarindex_ch_lake_backup_last_full_unix      (clean runs only)
#   stellarindex_ch_lake_backup_last_bytes          bytes written by the run
#   stellarindex_ch_lake_backup_chain_length        links in the current chain
# Alert: stellarindex_ch_lake_backup_stale (storage.yml, both trees).
# Restore: docs/operations/runbooks/ch-lake-backup.md.
#
# Exit code: 0 clean (or not configured); 1 the backup failed; 2 the backup
# succeeded but pruning an old chain failed.
set -uo pipefail

CH_HTTP="${CH_HTTP:-http://127.0.0.1:8123/}"
CH_DATABASE="${CH_DATABASE:-stellar}"
BACKUP_DISK="${BACKUP_DISK:-}"
STATE_DIR="${STATE_DIR:-/var/lib/stellarindex/ch-lake-backup}"
TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
FULL_INTERVAL_DAYS="${FULL_INTERVAL_DAYS:-28}"
RETAIN_CHAINS="${RETAIN_CHAINS:-1}"
MAX_BANDWIDTH="${MAX_BANDWIDTH:-0}"
POLL_SECONDS="${POLL_SECONDS:-60}"
# Word-split on purpose: tests substitute `docker exec <c> clickhouse-disks …`.
CH_DISKS_CMD="${CH_DISKS_CMD:-clickhouse-disks -C /etc/clickhouse-server/config.xml}"

now="$(date -u +%s)"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
chain_file="$STATE_DIR/chain"
chains_file="$STATE_DIR/chains"
backup_ok=0
bytes=0
chain_len=0
full_unix=""
last_error=""

note() { echo "ch-lake-backup: $*" >&2; }
ch() { curl -sSf --max-time 120 "$CH_HTTP" --data-binary "$1"; }
disk_ref() { printf "Disk('%s', '%s')" "$BACKUP_DISK" "$1"; }

write_metrics() {
  [[ "$TEXTFILE_DIR" == "/dev/null" ]] && return 0
  mkdir -p "$TEXTFILE_DIR"
  local out="$TEXTFILE_DIR/ch_lake_backup.prom" tmp
  tmp="$out.tmp.$$"
  {
    echo "# HELP stellarindex_ch_lake_backup_configured 1 if a ClickHouse lake backup disk (BACKUP_DISK) is configured on this host, else 0."
    echo "# TYPE stellarindex_ch_lake_backup_configured gauge"
    if [[ -n "$BACKUP_DISK" ]]; then
      echo "stellarindex_ch_lake_backup_configured 1"
    else
      echo "stellarindex_ch_lake_backup_configured 0"
    fi
    if [[ "$backup_ok" -eq 1 ]]; then
      echo "# HELP stellarindex_ch_lake_backup_last_success_unix Unix time of the most recent BACKUP_CREATED ClickHouse lake backup."
      echo "# TYPE stellarindex_ch_lake_backup_last_success_unix gauge"
      echo "stellarindex_ch_lake_backup_last_success_unix $now"
      echo "# HELP stellarindex_ch_lake_backup_last_full_unix Unix time of the full backup the current chain is built on."
      echo "# TYPE stellarindex_ch_lake_backup_last_full_unix gauge"
      echo "stellarindex_ch_lake_backup_last_full_unix $full_unix"
      echo "# HELP stellarindex_ch_lake_backup_last_bytes Compressed bytes written by the most recent successful backup."
      echo "# TYPE stellarindex_ch_lake_backup_last_bytes gauge"
      echo "stellarindex_ch_lake_backup_last_bytes $bytes"
      echo "# HELP stellarindex_ch_lake_backup_chain_length Backups (full + incrementals) in the current chain."
      echo "# TYPE stellarindex_ch_lake_backup_chain_length gauge"
      echo "stellarindex_ch_lake_backup_chain_length $chain_len"
    fi
  } > "$tmp"
  chmod 644 "$tmp"
  mv "$tmp" "$out"
}

# Prints "full" or "incremental <base-path>". A chain file that is missing,
# empty or malformed yields "full": an incremental on a base we cannot name
# is a backup we cannot restore.
plan_next() {
  local first_ts last_path
  if [[ ! -s "$chain_file" ]]; then echo full; return; fi
  first_ts="$(head -n1 "$chain_file" | cut -f1)"
  last_path="$(tail -n1 "$chain_file" | cut -f2)"
  if ! [[ "$first_ts" =~ ^[0-9]+$ ]] || [[ -z "$last_path" ]]; then echo full; return; fi
  if (( now - first_ts >= FULL_INTERVAL_DAYS * 86400 )); then echo full; return; fi
  echo "incremental $last_path"
}

# Starts the BACKUP asynchronously and polls system.backups until it
# settles; a multi-TiB full outlives any sane HTTP request. Sets `bytes`.
run_backup() {
  local path="$1" base="${2:-}" settings="max_backup_bandwidth = $MAX_BANDWIDTH" id row status
  [[ -n "$base" ]] && settings="$settings, base_backup = $(disk_ref "$base")"
  id="$(ch "BACKUP DATABASE \`$CH_DATABASE\` TO $(disk_ref "$path") SETTINGS $settings ASYNC FORMAT TabSeparated" | cut -f1)" || {
    note "BACKUP was refused for $(disk_ref "$path")"
    return 1
  }
  [[ -n "$id" ]] || { note "BACKUP returned no operation id"; return 1; }
  while :; do
    row="$(ch "SELECT status, compressed_size, num_files, replaceRegexpOne(replaceAll(error, '\n', ' '), ',? Stack trace.*', '') FROM system.backups WHERE id = '$id' FORMAT TabSeparated")" || row=""
    status="$(cut -f1 <<<"$row")"
    case "$status" in
      CREATING_BACKUP) sleep "$POLL_SECONDS" ;;
      BACKUP_CREATED)
        bytes="$(cut -f2 <<<"$row")"
        if [[ "$(cut -f3 <<<"$row")" -gt 0 ]]; then return 0; fi
        note "BACKUP_CREATED with zero files for $path — not a backup"
        return 1 ;;
      "")
        # system.backups is in-memory: an id that vanished means the server
        # restarted under the backup, which therefore did not complete.
        note "backup $id is no longer in system.backups (server restart?)"
        return 1 ;;
      *)
        last_error="$(cut -f4 <<<"$row")"
        note "backup $path ended $status: $last_error"
        return 1 ;;
    esac
  done
}

# Removes every chain but the newest RETAIN_CHAINS. Called only after a
# full has been confirmed, so the chain being removed is never the last.
prune_chains() {
  local keep old failed=0
  keep="$(tail -n "$RETAIN_CHAINS" "$chains_file")"
  while IFS= read -r old; do
    [[ -z "$old" ]] && continue
    grep -qxF "$old" <<<"$keep" && continue
    # shellcheck disable=SC2086 # CH_DISKS_CMD is a command line by design
    if $CH_DISKS_CMD --disk "$BACKUP_DISK" --query "remove -r $CH_DATABASE/$old"; then
      note "pruned chain $old"
    else
      note "PRUNE FAILED for chain $old — it stays on the backup disk"
      failed=1
      keep="$keep"$'\n'"$old"
    fi
  done < "$chains_file"
  grep -xF -f <(printf '%s\n' "$keep") "$chains_file" > "$chains_file.tmp" && mv "$chains_file.tmp" "$chains_file"
  return "$failed"
}

main() {
  local plan base path chain_id rc=0
  if [[ -z "$BACKUP_DISK" ]]; then
    note "no BACKUP_DISK configured — the lake has NO data backup on this host (stellarindex_ch_lake_backup_stale tickets it)"
    write_metrics
    return 0
  fi
  mkdir -p "$STATE_DIR" || { note "cannot create $STATE_DIR"; write_metrics; return 1; }
  if [[ -n "$(ch "SELECT id FROM system.backups WHERE status = 'CREATING_BACKUP' AND startsWith(name, 'Disk(\\'$BACKUP_DISK\\'') FORMAT TabSeparated")" ]]; then
    note "a backup to $BACKUP_DISK is already running — not starting a second"
    write_metrics
    return 1
  fi
  plan="$(plan_next)"
  if [[ "$plan" == full ]]; then
    chain_id="$stamp"
    path="$CH_DATABASE/$chain_id/$stamp-full"
    base=""
  else
    base="${plan#incremental }"
    chain_id="$(cut -d/ -f2 <<<"$base")"
    path="$CH_DATABASE/$chain_id/$stamp-incr"
  fi
  note "starting ${plan%% *} backup to $(disk_ref "$path")"
  if ! run_backup "$path" "$base"; then
    # The base this chain extends is gone from the disk: every further
    # incremental would fail the same way, so the next run starts a new full.
    if [[ -n "$base" && "$last_error" == *BACKUP_NOT_FOUND* ]]; then
      note "base $base is missing on $BACKUP_DISK — the next run starts a new chain"
      rm -f "$chain_file"
    fi
    write_metrics
    return 1
  fi
  backup_ok=1
  if [[ "$plan" == full ]]; then
    printf '%s\t%s\n' "$now" "$path" > "$chain_file"
    echo "$chain_id" >> "$chains_file"
    prune_chains || rc=2
  else
    printf '%s\t%s\n' "$now" "$path" >> "$chain_file"
  fi
  full_unix="$(head -n1 "$chain_file" | cut -f1)"
  chain_len="$(grep -c . "$chain_file")"
  note "backup $path created ($bytes bytes written; chain length $chain_len)"
  write_metrics
  return "$rc"
}

main "$@"
