#!/usr/bin/env bash
# check-public-dataset-test.sh — fixture tests for the AWS Public
# Blockchain dataset drift tripwire (scripts/ci/check-public-dataset.sh).
#
# public-dataset-check.yml only fires weekly against the live bucket, so
# these fixtures are the only place the verdict is exercised on a PR.
# Every drift class the workflow exists to catch (gap inside the trimmed
# range, gap above it, malformed/misnamed partition, manifest change,
# stalled publication) is pinned RED here, and the intact shape — a
# synthetic 1003-partition listing identical in structure to the live
# bucket on 2026-08-29 — is pinned GREEN. No network, no aws cli.
#
# Run: bash scripts/ci/check-public-dataset-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
CHECK="$PWD/scripts/ci/check-public-dataset.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

GOOD_MANIFEST='{"networkPassphrase":"Public Global Stellar Network ; September 2015","version":"1.0","compression":"zstd","ledgersPerBatch":1,"batchesPerPartition":64000}'

# gen_listing <n-partitions> — the live naming scheme, as `aws s3 ls`
# prints it (descending-hex sort key, trailing slash, .config.json row).
gen_listing() {
  local n="$1" i start end
  echo "2026-08-29 00:00:00        139 .config.json"
  for ((i = n - 1; i >= 0; i--)); do
    start=$((i * 64000)); end=$((start + 63999))
    printf '                           PRE %08X--%d-%d/\n' $((0xFFFFFFFF - start)) "$start" "$end"
  done
}

# run <listing-text> <manifest-json> [tip]
run() {
  printf '%s\n' "$1" > "$TMP/listing.txt"
  printf '%s' "$2" > "$TMP/config.json"
  OUT="$(PUBLIC_DATASET_LISTING="$TMP/listing.txt" PUBLIC_DATASET_MANIFEST="$TMP/config.json" \
         PUBLIC_DATASET_TIP="${3:-64170676}" GITHUB_OUTPUT="$TMP/out.txt" bash "$CHECK" 2>&1)"
  RC=$?
}

expect() {
  local name="$1" want_rc="$2" want_sub="${3:-}"
  if [ "$RC" -ne "$want_rc" ]; then
    echo "FAIL: $name — exit $RC, want $want_rc" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1)); return
  fi
  if [ -n "$want_sub" ] && ! printf '%s' "$OUT" | grep -q -- "$want_sub"; then
    echo "FAIL: $name — output missing '$want_sub'" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1)); return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

FULL="$(gen_listing 1003)"   # 0..64191999 — the live shape on 2026-08-29

# ── Intact ──────────────────────────────────────────────────────────
run "$FULL" "$GOOD_MANIFEST" 64170676
expect 'live-shaped 1003-partition listing + manifest → OK' 0 'OK — 1003 contiguous'
if grep -q '^partition_count=1003$' "$TMP/out.txt" && grep -q '^last_partition=FC2D7BFF--64128000-64191999$' "$TMP/out.txt"; then
  echo "ok: GITHUB_OUTPUT carries partition_count + last_partition"; pass=$((pass + 1))
else
  echo "FAIL: GITHUB_OUTPUT missing count/last" >&2; cat "$TMP/out.txt" >&2; fail=$((fail + 1))
fi

# Tip unknown (Horizon down) must NOT count as drift.
run "$FULL" "$GOOD_MANIFEST" "not-a-number"
expect 'tip source unreachable → freshness skipped, still OK' 0 'freshness assertion SKIPPED'

# ── Gap INSIDE the trimmed range (the exposure that matters most:
#    r1 has no local copy of [64000, 49983999]) ──────────────────────
run "$(printf '%s\n' "$FULL" | grep -v -- '--25600000-25663999/')" "$GOOD_MANIFEST"
expect 'partition 25600000 missing → DRIFT (gap + trimmed-range uncovered)' 1 'GAP: ledgers 25600000..25663999'
expect 'partition 25600000 missing → names the trimmed range' 1 'trimmed range \[64000, 49983999\] is NOT fully covered'

# Gap ABOVE the trimmed range: still drift (contiguity), but the trimmed
# range itself is intact and must not be reported as uncovered.
run "$(printf '%s\n' "$FULL" | grep -v -- '--60800000-60863999/')" "$GOOD_MANIFEST"
expect 'partition 60800000 missing → DRIFT (gap)' 1 'GAP: ledgers 60800000..60863999'
if printf '%s' "$OUT" | grep -q 'trimmed range .* NOT fully covered'; then
  echo "FAIL: gap above trimmed range wrongly reported as trimmed-range hole" >&2; fail=$((fail + 1))
else
  echo "ok: gap above trimmed range does not implicate the trimmed range"; pass=$((pass + 1))
fi

# ── Naming / shape drift ────────────────────────────────────────────
run "$(printf '%s\n' "$FULL" | sed 's/FFFF05FF--64000-127999/FFFF05FF--64000-127998/')" "$GOOD_MANIFEST"
expect 'partition with 63,999 ledgers → DRIFT (not a 64000 range)' 1 'not a 64000-aligned'

run "$(printf '%s\n' "$FULL" | sed 's/FFFF05FF--64000-127999/FFFF05FE--64000-127999/')" "$GOOD_MANIFEST"
expect 'wrong descending-hex key → DRIFT' 1 'hex prefix != 0xFFFFFFFF-start'

run "$(printf '%s\n' "$FULL" | sed 's/FFFF05FF--64000-127999/ledgers-64000-127999/')" "$GOOD_MANIFEST"
expect 'renamed partition scheme → DRIFT (malformed)' 1 "malformed partition prefix 'ledgers-64000-127999'"

run "$(printf '%s\n%s\n' "$FULL" '                           PRE FFFF05FF--64000-127999/')" "$GOOD_MANIFEST"
expect 'duplicate partition → DRIFT (overlap)' 1 'overlaps/duplicates'

run "$(printf '%s\n' "$FULL" | head -1)" "$GOOD_MANIFEST"
expect 'listing with only .config.json → DRIFT (empty)' 1 'listing is EMPTY'

# ── Manifest drift ──────────────────────────────────────────────────
run "$FULL" '{"networkPassphrase":"Test SDF Network ; September 2015","compression":"zstd","ledgersPerBatch":1,"batchesPerPartition":64000}'
expect 'testnet passphrase in pubnet manifest → DRIFT' 1 'manifest networkPassphrase='

run "$FULL" '{"networkPassphrase":"Public Global Stellar Network ; September 2015","compression":"zstd","ledgersPerBatch":64,"batchesPerPartition":1000}'
expect 'ledgersPerBatch 64 / 1000 per partition → DRIFT' 1 'manifest ledgersPerBatch=64'

run "$FULL" 'not json'
expect 'manifest not JSON → DRIFT' 1 'not valid JSON'

# ── Stalled publication ─────────────────────────────────────────────
# Last partition ends at 64191999; a tip of 64320000 puts it exactly
# 2 partitions + 1 ledger behind → RED. 64319999 is within slack → OK.
run "$FULL" "$GOOD_MANIFEST" 64320000
expect 'dataset > 2 partitions behind tip → DRIFT (stalled)' 1 'partitions behind'
run "$FULL" "$GOOD_MANIFEST" 64319999
expect 'dataset exactly 2 partitions behind tip → OK (within slack)' 0 'OK —'

echo
echo "check-public-dataset-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ] || exit 1
