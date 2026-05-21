#!/bin/bash
#
# galexie append wrapper (gap-safe resume).
#
# Galexie's `append` subcommand requires --start <ledger>. We pick
# the start ledger as follows:
#
#   1. Probe MinIO galexie-live for the highest exported LCM —
#      resume from `last_exported + 1`. This is the path on every
#      RESTART of an already-running deployment (the common case);
#      it guarantees no gap when the service is restarted after
#      having uploaded ledgers previously (#50: gap created by ZFS
#      migration on 2026-05-21 because the wrapper used to skip to
#      the archive tip every time).
#
#   2. If the bucket is empty (fresh deploy), fall back to the
#      original "archive tip minus a safety margin" logic. SDF's
#      .well-known/stellar-history.json gives the archive's current
#      tip; we floor to a checkpoint boundary and subtract margin
#      so captive-core has solid ground.
#
# CRITICAL: never start at the live network tip — archives publish
# checkpoints with ~5-15 min lag; starting at "now" makes every HAS
# request 404 and captive-core spins.
#
# 2026-04-23: removed the "wait for primary stellar-core" preamble.
# 2026-05-21: added resume-from-last-exported logic (#50).

set -euo pipefail

CONF=/etc/galexie/galexie.toml

# SDF's primary archive — same source our captive-core trusts.
SDF_HAS_URL="https://history.stellar.org/prd/core-live/core_live_001/.well-known/stellar-history.json"

# CHECKPOINT_MARGIN applies only to the fresh-deploy fallback.
CHECKPOINT_MARGIN="${CHECKPOINT_MARGIN:-128}"

# --- 1. Probe galexie-live for the highest exported LCM ----------
# galexie-writer's MinIO policy includes ListBucket on galexie-live.
# mc binary lives at /usr/local/bin/mc; we create a temp alias from
# the AWS env vars systemd loaded for us so this works without a
# persistent ~/.mc config on the galexie user.
last_exported=""
if [[ -x /usr/local/bin/mc && -n "${AWS_ENDPOINT_URL:-}" ]]; then
  MC_ALIAS_DIR=$(mktemp -d)
  trap 'rm -rf "$MC_ALIAS_DIR"' EXIT
  export MC_CONFIG_DIR="$MC_ALIAS_DIR"

  if /usr/local/bin/mc alias set live "$AWS_ENDPOINT_URL" \
       "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" >/dev/null 2>&1; then
    # List all chunk-dirs at top, find the highest-numbered LCM inside
    # the latest chunk. Filenames look like FC43AFEC--62672915.xdr.zst;
    # the integer after `--` is the ledger sequence.
    last_exported=$(
      /usr/local/bin/mc ls --recursive live/galexie-live/ 2>/dev/null \
        | awk '{
            n = split($NF, parts, "--")
            if (n >= 2) {
              # last segment is "<hex>--<ledger>.xdr.zst" — take the
              # final field, drop ".xdr.zst", treat as integer
              ledger = parts[n]
              sub(/\.xdr\.zst$/, "", ledger)
              if (ledger ~ /^[0-9]+$/ && ledger+0 > max) max = ledger+0
            }
          } END { if (max > 0) print max }'
    )
  fi
fi

if [[ -n "$last_exported" && "$last_exported" -gt 1 ]]; then
  start=$(( last_exported + 1 ))
  echo "galexie-append.sh: galexie-live last-exported=$last_exported → resuming at $start"
else
  # --- 2. Fresh-deploy fallback: archive tip minus margin -------
  archive_tip=""
  for i in $(seq 1 30); do
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

exec /usr/local/bin/galexie append --config-file "$CONF" --start "$start"
