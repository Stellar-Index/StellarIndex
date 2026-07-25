#!/usr/bin/env bash
# restore-drill-test.sh — pins the restore drill's invocation contract.
#
# THE DEFECT THIS EXISTS FOR (found 2026-07-25):
# scripts/ops/restore-drill.sh's optional ClickHouse re-derive stage
# invoked `stellarindex-ops ch-backfill … -database drill_scratch`. That
# flag has never existed. ch-backfill parses with flag.ContinueOnError,
# so the invocation died at PARSE time, the stage recorded a generic
# "ch-backfill sample failed", and `| tail -5` swallowed the reason. The
# ADR-0043 §2.2 re-derive drill — the thing that turns "the lake is
# re-derivable" from a claim into a measured fact — therefore never ran
# once between shipping and this test.
#
# Nothing caught it because it is a drift between a SHELL script and a
# GO flag set, and no gate looked across that seam. This test is that
# gate: every flag restore-drill.sh passes to ch-backfill must be
# declared in internal/ops/chops/ch_backfill.go's FlagSet.
#
# Static, root-free, second-fast. The deployed box gets the same check at
# runtime from restore-drill.sh's own CH preflight (which reads
# `ch-backfill -help`, so it also catches a stale binary on disk); this
# catches it in CI before it ships.
#
# Run: bash scripts/ops/restore-drill-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1

DRILL="scripts/ops/restore-drill.sh"
CH_BACKFILL_GO="internal/ops/chops/ch_backfill.go"

pass=0
fail=0
ok()   { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad()  { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

for f in "$DRILL" "$CH_BACKFILL_GO"; do
  [[ -r "$f" ]] || { echo "restore-drill-test: missing $f" >&2; exit 2; }
done

# ─── the flag sets ──────────────────────────────────────────────────
# Declared: every fs.<Type>("name", …) in ch-backfill's FlagSet.
declared="$(grep -oE 'fs\.(String|Bool|Int|Uint|Int64|Uint64|Duration|Float64)\("[a-z0-9-]+"' "$CH_BACKFILL_GO" \
  | sed -E 's/.*\("([a-z0-9-]+)"/\1/' | sort -u)"
if [[ -z "$declared" ]]; then
  echo "restore-drill-test: parsed ZERO flags out of $CH_BACKFILL_GO — the extraction" >&2
  echo "  broke (renamed FlagSet? different helper?), and a vacuous pass here is exactly" >&2
  echo "  the failure mode this test exists to prevent." >&2
  exit 2
fi

# The drill's REAL ch-backfill invocation — the one that runs the
# re-derive, not the `-help` preflight probe, not prose in a comment or a
# note(). Anchored on the binary immediately preceding the subcommand, so
# it matches both the current `"$OPS_BIN" ch-backfill` form and the
# pre-fix inline `/usr/local/bin/stellarindex-ops ch-backfill` one. The
# invocation spans continuation lines, so capture through the line that
# redirects stderr.
ch_invocation() {
  awk '
    /^[[:space:]]*#/ { next }                                  # comments are prose
    !inv && /(stellarindex-ops|OPS_BIN"?)[[:space:]]+ch-backfill/ && !/-help/ { inv = 1 }
    inv { print; if ($0 ~ /2>&1/) inv = 0 }
  ' "$DRILL"
}

# Passed: the `-flag` tokens in that invocation.
passed="$(ch_invocation | grep -oE '(^|[[:space:]])-[a-z][a-z0-9-]*' | tr -d ' ' | sed 's/^-//' | sort -u)"
if [[ -z "$passed" ]]; then
  echo "restore-drill-test: found no ch-backfill invocation in $DRILL — either the drill" >&2
  echo "  stage was removed or this test's extraction drifted; refusing to pass vacuously." >&2
  exit 2
fi

# ─── 1. every passed flag is declared ───────────────────────────────
while IFS= read -r flag; do
  [[ -z "$flag" ]] && continue
  if grep -qxF "$flag" <<<"$declared"; then
    ok "restore-drill passes -$flag, which ch-backfill declares"
  else
    bad "restore-drill passes -$flag, which ch-backfill DOES NOT declare — flag.ContinueOnError
       rejects the whole invocation at parse time and the CH re-derive stage can never run
       (the 2026-07-25 '-database drill_scratch' defect). Declared flags: $(tr '\n' ' ' <<<"$declared")"
  fi
done <<<"$passed"

# ─── 2. the stage must read history from an explicit bucket ─────────
# ch-backfill's no-seam default is the (trimmed) live bucket, which
# cannot hold a window ~1M ledgers below the tip — the 5179250a
# wrong-bucket class. The drill must not rely on that default.
if grep -qxF "bucket" <<<"$passed"; then
  ok "the re-derive stage passes -bucket explicitly (does not inherit the live default)"
else
  bad "the re-derive stage does not pass -bucket — ch-backfill's no-seam default is the
       trimmed live bucket, which cannot hold the drill's historic window"
fi

# ─── 3. the stage must not write to the live lake ───────────────────
# There is no scratch-database mode (clickhouse.Open pins `stellar`), so
# a writing drill writes to production. -dry-run is the ADR-0043 §2.2
# compliant shape on this host.
if grep -qxF "dry-run" <<<"$passed"; then
  ok "the re-derive stage runs -dry-run (non-destructive, per the script's own header)"
else
  bad "the re-derive stage does not pass -dry-run — with no scratch-database mode available
       it would re-derive into the LIVE lake, which is not a non-destructive drill"
fi

# ─── 4. the failure output must not be truncated ────────────────────
# `| tail -5` on the invocation is what hid the parse error for weeks.
if ch_invocation | grep -q 'tail -'; then
  bad "the ch-backfill invocation pipes through 'tail -' — truncating the stage's output is
       how the original defect stayed invisible; print it in full"
else
  ok "the ch-backfill invocation does not truncate its output"
fi

echo "restore-drill-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]] || exit 1
