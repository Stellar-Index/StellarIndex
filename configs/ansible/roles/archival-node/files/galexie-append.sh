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

# SDF's primary archive — same source our captive-core trusts. Env-overridable
# (pubnet default) so a test net points at its OWN archive (core-testnet /
# core-futurenet) — the galexie env file sets SDF_HAS_URL per network.
SDF_HAS_URL="${SDF_HAS_URL:-https://history.stellar.org/prd/core-live/core_live_001/.well-known/stellar-history.json}"

# CHECKPOINT_MARGIN applies only to the fresh-deploy fallback.
CHECKPOINT_MARGIN="${CHECKPOINT_MARGIN:-128}"

# MC_BIN / GALEXIE_BIN are overridable for tests only; production always
# gets the hardcoded defaults (predictable binary location for the
# service account, not a PATH lookup).
MC_BIN="${MC_BIN:-/usr/local/bin/mc}"
GALEXIE_BIN="${GALEXIE_BIN:-/usr/local/bin/galexie}"

# --- 1. Probe galexie-live for the highest exported LCM ----------
# galexie-writer's MinIO policy includes ListBucket on galexie-live.
# mc binary lives at /usr/local/bin/mc; we create a temp alias from
# the AWS env vars systemd loaded for us so this works without a
# persistent ~/.mc config on the galexie user.
# A missing mc binary, an unset AWS_ENDPOINT_URL, or a failed `mc alias set`
# all mean we CANNOT know whether galexie-live already holds exported
# ledgers. Treat each as fatal (exit non-zero, let systemd's
# Restart=on-failure retry) rather than silently falling through to the
# fresh-deploy fallback below — that fallback can skip past ledgers this
# deploy already exported, opening a gap (see #50 in the header comment).
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

# Keys go in on stdin (mc reads ACCESSKEY then SECRETKEY, one per line,
# when they are omitted from argv) so the bucket-writer secret never
# sits in /proc/<pid>/cmdline — this runs on EVERY galexie restart
# (secret-on-argv, scripts/ci/lint-ansible-tasks.sh).
if ! printf '%s\n%s\n' "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" \
     | "$MC_BIN" alias set live "$AWS_ENDPOINT_URL" >/dev/null 2>&1; then
  echo "galexie-append.sh: mc alias set failed against $AWS_ENDPOINT_URL" >&2
  exit 1
fi

# List all chunk-dirs at top, find the highest-numbered LCM inside
# the latest chunk. Filenames look like FC43AFEC--62672915.xdr.zst;
# the integer after `--` is the ledger sequence.
last_exported=$(
  "$MC_BIN" ls --recursive live/galexie-live/ 2>/dev/null \
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

if [[ -n "$last_exported" && "$last_exported" -gt 1 ]]; then
  start=$(( last_exported + 1 ))
  echo "galexie-append.sh: galexie-live last-exported=$last_exported → resuming at $start"
elif [[ -n "${GALEXIE_START:-}" && "${GALEXIE_START}" -gt 1 ]]; then
  # --- 2a. Fresh deploy, explicit start ---------------------------
  # GALEXIE_START (set per network) pins the fresh-deploy start so galexie
  # and the indexer's backfill_from_ledger begin at the SAME ledger — a
  # coherent from-recent bring-up on a test net (the archive-tip fallback
  # below is dynamic and would not match the indexer's fixed floor).
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
