#!/usr/bin/env bash
# config-backup.sh — daily encrypted backup of a host's config and secrets
# (off-site-backup-plan.md §3).
#
# Nothing here can be re-derived: the rendered stellarindex.toml, the
# pgBackRest cipher pass, ClickHouse users and disks, MinIO and Galexie config,
# and the systemd units. The tarball is streamed straight into age, so the
# plaintext never touches disk; only the holder of the offline age identity can
# open it. Restore: off-site-backup-plan.md §3.
#
# Emits config_backup.prom (node_exporter textfile collector):
#   stellarindex_config_backup_last_success_unix
#   stellarindex_config_backup_bytes
#   stellarindex_config_backup_offsite_configured
#   stellarindex_config_backup_offsite_last_success_unix (push only)
# Alerts: stellarindex_config_backup_stale / _offsite_stale
# (deploy/monitoring/rules/storage.yml).
#
# Exit: 0 ok, 1 no backup written, 2 backup written but not pushed off-site.
set -uo pipefail

AGE_RECIPIENT="${AGE_RECIPIENT:-}"
OUT_DIR="${OUT_DIR:-/var/lib/stellarindex/config-backup}"
TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
RETAIN_DAYS="${RETAIN_DAYS:-30}"
BACKUP_MC_TARGET="${BACKUP_MC_TARGET:-}"
PG_DATABASE="${PG_DATABASE:-stellarindex}"
# Globs expand at run time; a path absent on this host is skipped.
BACKUP_PATHS="${BACKUP_PATHS:-/etc/stellarindex* /etc/default/stellarindex* /etc/default/minio /etc/pgbackrest* /etc/postgresql /etc/clickhouse-server /etc/clickhouse-keeper /etc/clickhouse-client /root/.clickhouse-client /root/.mc /etc/galexie /etc/stellar /etc/minio /etc/caddy /etc/redis /etc/prometheus /etc/loki /etc/systemd/system}"

note() { echo "config-backup: $*" >&2; }

write_metrics() {
  local ok="$1" bytes="$2" offsite_ok="$3"
  [[ "$TEXTFILE_DIR" == "/dev/null" ]] && return
  mkdir -p "$TEXTFILE_DIR"
  local out="$TEXTFILE_DIR/config_backup.prom" tmp
  tmp="$out.tmp.$$"
  {
    echo "# HELP stellarindex_config_backup_last_success_unix Unix time of the most recent encrypted config backup."
    echo "# TYPE stellarindex_config_backup_last_success_unix gauge"
    if [[ "$ok" -eq 1 ]]; then
      echo "stellarindex_config_backup_last_success_unix $(date +%s)"
    fi
    echo "# HELP stellarindex_config_backup_bytes Size of the most recent encrypted config backup."
    echo "# TYPE stellarindex_config_backup_bytes gauge"
    echo "stellarindex_config_backup_bytes $bytes"
    echo "# HELP stellarindex_config_backup_offsite_configured 1 if an off-site target (BACKUP_MC_TARGET) is configured, else 0."
    echo "# TYPE stellarindex_config_backup_offsite_configured gauge"
    if [[ -n "$BACKUP_MC_TARGET" ]]; then
      echo "stellarindex_config_backup_offsite_configured 1"
    else
      echo "stellarindex_config_backup_offsite_configured 0"
    fi
    if [[ "$offsite_ok" -eq 1 ]]; then
      echo "# HELP stellarindex_config_backup_offsite_last_success_unix Unix time of the most recent verified off-site push of the config backup."
      echo "# TYPE stellarindex_config_backup_offsite_last_success_unix gauge"
      echo "stellarindex_config_backup_offsite_last_success_unix $(date +%s)"
    fi
  } > "$tmp"
  chmod 644 "$tmp"
  mv "$tmp" "$out"
}

fail() { note "$1"; write_metrics 0 0 0; exit 1; }

[[ -n "$AGE_RECIPIENT" ]] || fail "AGE_RECIPIENT is unset; refusing to write an unencrypted backup"
command -v age >/dev/null 2>&1 || fail "age is not installed"
mkdir -p "$OUT_DIR" || fail "cannot create $OUT_DIR"
chmod 700 "$OUT_DIR"

stage="$(mktemp -d)" || fail "cannot create a staging directory"
trap 'rm -rf "$stage"' EXIT

rel=()
for p in ${BACKUP_PATHS}; do
  [[ -e "$p" ]] && rel+=("${p#/}")
done
[[ ${#rel[@]} -gt 0 ]] || fail "none of BACKUP_PATHS exists on this host; refusing to write an empty backup"

# Schema only: pgBackRest holds the data, this makes a fresh-cluster rebuild
# readable without it.
extra=()
if command -v pg_dump >/dev/null 2>&1 && command -v runuser >/dev/null 2>&1 && id postgres >/dev/null 2>&1; then
  if runuser -u postgres -- pg_dump --schema-only "$PG_DATABASE" > "$stage/pg-schema.sql" 2>"$stage/pg_dump.err"; then
    extra=(-C "$stage" pg-schema.sql)
  else
    note "pg_dump --schema-only $PG_DATABASE failed ($(grep -m1 'error:' "$stage/pg_dump.err" || tail -n1 "$stage/pg_dump.err")); continuing without it"
  fi
fi

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
out="$OUT_DIR/config-$stamp.tar.gz.age"
tar -czf - -C / "${rel[@]}" ${extra[@]+"${extra[@]}"} 2>"$stage/tar.err" \
  | age -r "$AGE_RECIPIENT" -o "$out.part"
st=("${PIPESTATUS[@]}")
# GNU tar exits 1 when a file changed while it was read; the archive is still whole.
if [[ "${st[0]}" -gt 1 || "${st[1]}" -ne 0 ]]; then
  rm -f "$out.part"
  note "tar rc=${st[0]} age rc=${st[1]}: $(tail -n 3 "$stage/tar.err" 2>/dev/null)"
  fail "backup not written"
fi
if [[ "$(head -c 21 "$out.part")" != "age-encryption.org/v1" ]]; then
  rm -f "$out.part"
  fail "output lacks the age header; refusing to keep it"
fi
mv "$out.part" "$out"
chmod 600 "$out"
bytes="$(wc -c < "$out" | tr -d ' ')"
note "wrote $out (${bytes}B, ${#rel[@]} paths)"

find "$OUT_DIR" -maxdepth 1 -name 'config-*.tar.gz.age*' -mtime "+$RETAIN_DAYS" -delete 2>/dev/null || true

rc=0
offsite_ok=0
if [[ -n "$BACKUP_MC_TARGET" ]]; then
  mc_clean=(env -u AWS_ACCESS_KEY_ID -u AWS_SECRET_ACCESS_KEY -u AWS_ENDPOINT_URL
            -u AWS_REGION -u AWS_SESSION_TOKEN -u AWS_DEFAULT_REGION)
  # mc writes to a local path when the alias is unknown, so prove the alias
  # resolves to another host before trusting a push.
  alias_name="${BACKUP_MC_TARGET%%/*}"
  alias_url=""
  if command -v mc >/dev/null 2>&1; then
    alias_out=$("${mc_clean[@]}" mc alias list "$alias_name" 2>/dev/null)
    alias_url=$(awk '/URL/ { print $3; exit }' <<<"$alias_out")
  fi
  dest="$BACKUP_MC_TARGET/$(basename "$out")"
  if [[ -z "$alias_url" ]]; then
    note "OFFSITE PUSH REFUSED: '$alias_name' is not a configured mc alias"
    rc=2
  elif [[ "$alias_url" == *//127.0.0.1* || "$alias_url" == *//localhost* || "$alias_url" == *//::1* ]]; then
    note "OFFSITE PUSH REFUSED: alias '$alias_name' resolves to this host"
    rc=2
  elif "${mc_clean[@]}" mc cp --quiet "$out" "$dest" >/dev/null 2>&1 \
     && stat_out=$("${mc_clean[@]}" mc stat --json "$dest" 2>/dev/null) \
     && grep -q "\"size\":${bytes}[,}]" <<<"$stat_out"; then
    offsite_ok=1
    note "pushed to $dest (size verified)"
  else
    note "OFFSITE PUSH FAILED to $dest; the backup exists only on this host"
    rc=2
  fi
fi

write_metrics 1 "$bytes" "$offsite_ok"
exit "$rc"
