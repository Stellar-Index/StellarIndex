#!/bin/bash
#
# galexie append wrapper (gap-safe resume). `append` requires --start <ledger>:
#
#   1. Resume from the highest LCM exported to MinIO galexie-live + 1 (every
#      restart of a running deployment). Skipping to the archive tip instead
#      leaves a gap.
#   2. Empty bucket (fresh deploy): archive tip from SDF's
#      .well-known/stellar-history.json, floored to a checkpoint boundary,
#      minus a margin so captive-core has solid ground.
#
# CRITICAL: never start at the live network tip. Archives publish checkpoints
# ~5-15 min late, so every HAS request would 404 and captive-core spins.
#

set -euo pipefail

CONF=/etc/galexie/galexie.toml

# SDF's primary archive (what captive-core trusts). The galexie env file sets
# SDF_HAS_URL so each test net points at its own archive.
SDF_HAS_URL="${SDF_HAS_URL:-https://history.stellar.org/prd/core-live/core_live_001/.well-known/stellar-history.json}"

# CHECKPOINT_MARGIN applies only to the fresh-deploy fallback.
CHECKPOINT_MARGIN="${CHECKPOINT_MARGIN:-128}"

# MC_BIN / GALEXIE_BIN are overridable for tests only; production uses the
# fixed paths, not a PATH lookup.
MC_BIN="${MC_BIN:-/usr/local/bin/mc}"
GALEXIE_BIN="${GALEXIE_BIN:-/usr/local/bin/galexie}"

# --- 1. Probe galexie-live for the highest exported LCM ----------
# Uses a temp mc alias built from the systemd-loaded AWS env vars, so the galexie
# user needs no ~/.mc config. A missing mc, unset AWS_ENDPOINT_URL or failed
# `mc alias set` means we cannot know what galexie-live holds: fail (systemd
# Restart=on-failure retries) rather than fall through to the fresh-deploy path,
# which can skip exported ledgers and open a gap.
if [[ ! -x "$MC_BIN" ]]; then
  echo "galexie-append.sh: $MC_BIN not found or not executable" >&2
  exit 1
fi
if [[ -z "${AWS_ENDPOINT_URL:-}" ]]; then
  echo "galexie-append.sh: AWS_ENDPOINT_URL not set" >&2
  exit 1
fi

MC_ALIAS_DIR=$(mktemp -d)
trap 'rm -rf "$MC_ALIAS_DIR"' EXIT
export MC_CONFIG_DIR="$MC_ALIAS_DIR"

# Keys go in on stdin (mc reads ACCESSKEY then SECRETKEY) so the secret never
# sits in /proc/<pid>/cmdline (scripts/ci/lint-ansible-tasks.sh).
if ! printf '%s\n%s\n' "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" \
     | "$MC_BIN" alias set live "$AWS_ENDPOINT_URL" >/dev/null 2>&1; then
  echo "galexie-append.sh: mc alias set failed against $AWS_ENDPOINT_URL" >&2
  exit 1
fi

# Filenames look like FC43AFEC--62672915.xdr.zst; the integer after `--` is the ledger.
last_exported=$(
  "$MC_BIN" ls --recursive live/galexie-live/ 2>/dev/null \
    | awk '{
        n = split($NF, parts, "--")
        if (n >= 2) {
          # last segment is "<hex>--<ledger>.xdr.zst"
          ledger = parts[n]
          sub(/\.xdr\.zst$/, "", ledger)
          if (ledger ~ /^[0-9]+$/ && ledger+0 > max) max = ledger+0
        }
      } END { if (max > 0) print max }'
)

if [[ -n "$last_exported" && "$last_exported" -gt 1 ]]; then
  start=$(( last_exported + 1 ))
  echo "galexie-append.sh: galexie-live last-exported=$last_exported → resuming at $start"
elif [[ -n "${GALEXIE_START:-}" && "${GALEXIE_START}" -gt 1 ]]; then
  # --- 2a. Fresh deploy, explicit start ---------------------------
  # GALEXIE_START (per network) makes galexie and the indexer's
  # backfill_from_ledger begin at the SAME ledger; the dynamic archive-tip
  # fallback below would not match the indexer's fixed floor.
  start=$GALEXIE_START
  echo "galexie-append.sh: empty bucket — using configured GALEXIE_START=$start"
else
  # --- 2b. Fresh-deploy fallback: archive tip minus margin -------
  archive_tip=""
  for _ in $(seq 1 30); do
    if body=$(curl -sfm10 "$SDF_HAS_URL" 2>/dev/null); then
      archive_tip=$(echo "$body" | jq -r '.currentLedger // empty')
      if [[ -n "$archive_tip" && "$archive_tip" -gt 1 ]]; then
        break
      fi
    fi
    sleep 2
  done
  if [[ -z "$archive_tip" || "$archive_tip" -le 1 ]]; then
    echo "galexie-append.sh: could not read archive tip from $SDF_HAS_URL after 30 attempts" >&2
    exit 1
  fi
  start=$(( archive_tip - CHECKPOINT_MARGIN ))
  start=$(( (start / 64) * 64 ))
  [[ "$start" -le 1 ]] && start=64
  echo "galexie-append.sh: empty bucket — archive tip=$archive_tip, starting at $start (margin=$CHECKPOINT_MARGIN)"
fi

exec "$GALEXIE_BIN" append --config-file "$CONF" --start "$start"
