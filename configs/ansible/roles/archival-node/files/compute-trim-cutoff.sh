#!/usr/bin/env bash
# ADR-0027 §Trim operator helper: compute TRIM_CUTOFF for the
# monthly galexie-archive-trim.service from the indexer cursor.
#
# The trim-galexie-archive subcommand requires an explicit
# --older-than-ledger <seq>. Translating "90 days ago" into a
# concrete ledger sequence has to happen at run time — systemd
# can't do arithmetic in the unit file. This helper does it by
# reading the indexer's latest cursor from postgres, subtracting
# 90 days of ledgers (17280/day × 90 = 1,555,200), and writing
# the result to /run/galexie-archive-trim.env which the service
# loads via EnvironmentFile.
#
# Mirrors the compute-archive-to.sh pattern (F-1205): use the same
# DSN as the application binaries (sourced from /etc/default/
# stellarindex) rather than peer-auth, which fails under systemd's
# restricted user-switch context.
#
# Safety:
#   - bails when the cursor hasn't advanced enough for 90 days of
#     headroom to make sense (a brand-new node has nothing to trim).
#   - bails when the resulting cutoff is below ledger 2 (the
#     first real ledger; ledger 1 is empty by Stellar design).
#   - persists the cutoff as the archive's hot floor BEFORE the trim
#     runs, and bails if it cannot. galexie-archive-fill reads that
#     file and does not re-mirror partitions below it; a floor from any
#     other source drifts from this rolling cutoff and turns trim and
#     fill into adversaries. The floor only ever rises: a lower
#     cutoff trims less, it does not restore what was trimmed.
set -euo pipefail

ARCHIVE_HOT_FLOOR_FILE=/var/lib/galexie-archive/hot-floor

# Read a systemd EnvironmentFile VERBATIM — never `.`/source it. Its
# values are unquoted (that is what systemd wants), so the shell would
# expand `$`, split on `;`/`&`/`|`/whitespace and eat quotes inside a
# secret: the services keep working while this path gets a mangled DSN
# (deploy-ansible-secrets-5). Same reader as run-heavy-job.sh.
# usage: load_env_file FILE [export]
load_env_file() {
  local line
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      [A-Za-z_]*=*)
        if [ "${2:-}" = export ]; then
          export "${line?}"
        else
          printf -v "${line%%=*}" '%s' "${line#*=}"
        fi
        ;;
    esac
  done < "$1"
}
load_env_file /etc/default/stellarindex

# 90 days @ 5s ledger close = 17280 ledgers/day × 90 = 1,555,200.
HOT_WINDOW_LEDGERS=1555200

TIP=$(psql "$STELLARINDEX_POSTGRES_DSN" -tA -c \
  'SELECT GREATEST(MAX(last_ledger), 0) FROM ingestion_cursors WHERE last_ledger > 0' \
  2>/dev/null | tr -d '[:space:]')

if [ -z "$TIP" ] || [ "$TIP" = "0" ]; then
  echo "compute-trim-cutoff: indexer cursor not advanced; bailing" >&2
  exit 1
fi

CUTOFF=$((TIP - HOT_WINDOW_LEDGERS))

if [ "$CUTOFF" -lt 2 ]; then
  echo "compute-trim-cutoff: tip=$TIP gives cutoff=$CUTOFF — not enough history yet; bailing" >&2
  exit 1
fi

FLOOR=$CUTOFF
if [ -e "$ARCHIVE_HOT_FLOOR_FILE" ]; then
  PREV=$(tr -d '[:space:]' < "$ARCHIVE_HOT_FLOOR_FILE")
  if ! [[ "$PREV" =~ ^[0-9]+$ ]]; then
    echo "compute-trim-cutoff: $ARCHIVE_HOT_FLOOR_FILE holds '$PREV', not a ledger; refusing to trim" >&2
    exit 1
  fi
  if [ "$PREV" -gt "$FLOOR" ]; then
    FLOOR=$PREV
  fi
fi
mkdir -p "$(dirname "$ARCHIVE_HOT_FLOOR_FILE")"
echo "$FLOOR" > "$ARCHIVE_HOT_FLOOR_FILE.tmp"
mv -f "$ARCHIVE_HOT_FLOOR_FILE.tmp" "$ARCHIVE_HOT_FLOOR_FILE"

echo "TRIM_CUTOFF=$CUTOFF" > /run/galexie-archive-trim.env
echo "compute-trim-cutoff: tip=$TIP cutoff=$CUTOFF hot-floor=$FLOOR (90d hot window)" >&2
