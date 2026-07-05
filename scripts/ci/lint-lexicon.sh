#!/usr/bin/env bash
# lint-lexicon.sh — domain-lexicon + idiom ratchet (maintainability
# audit 2026-07-01, D2 + D6).
#
# The lexicon is docs/architecture/lexicon.md; the idiom rules are
# docs/engineering-standards.md "Go idioms". This script enforces the
# cheap, grep-able subset in two modes:
#
#   ZERO rules — patterns the repo has NONE of today and must never
#   gain (a violation fails immediately):
#     - `func Fetch…` / `func Make…` — the verb lexicon is
#       Get (single keyed read) / List (slice) / …Batch (multi-key) /
#       New (constructor) / Load (embedded or file data). No synonyms.
#     - non-slog logging imports (zap / zerolog / logrus / std "log")
#       — slog is the only logger in this codebase.
#
#   RATCHET rules — deprecated vocabulary/idioms that already exist
#   (renames ride other changes; see lexicon.md "migration rule") but
#   must not SPREAD. Baseline = scripts/ci/lint-lexicon.baseline, one
#   `<class> <file>` entry per grandfathered file:
#     - coin              file uses Coin* identifiers for what the
#                         lexicon calls an asset (Coinbase/CoinGecko
#                         the venues are excluded — they're names).
#     - positional-logger constructor takes `logger *slog.Logger`
#                         positionally (canonical: Logger rides in
#                         the trailing Options struct).
#     - variadic-option   constructor takes `...Option` functional
#                         options (canonical: a plain Options struct).
#
#   A file NOT in the baseline that gains a deviation fails the build.
#   A baseline entry whose file no longer deviates is STALE and fails
#   the build until the line is deleted — the baseline shrinks
#   monotonically (deletion-only edits don't trip
#   lint-baseline-growth.sh; additions require a Baseline-Growth:
#   commit trailer, same as lint-imports.baseline).
#
# Exit 0 clean, non-zero on any violation. Wired into verify.sh,
# `make lint-lexicon`, and CI's import-checks job.
set -euo pipefail
cd "$(dirname "$0")/../.."

BASELINE="scripts/ci/lint-lexicon.baseline"
fail=0

# ─── ZERO rules ──────────────────────────────────────────────────────

# 1) Verb lexicon: no Fetch*/Make* funcs in production Go.
hits=$(grep -rnE 'func (\([^)]+\) )?(Fetch|Make[A-Z])[A-Za-z]*\(' \
  --include='*.go' --exclude='*_test.go' internal/ cmd/ pkg/ 2>/dev/null || true)
if [ -n "$hits" ]; then
  echo "LEXICON: Fetch/Make verb — use Get (keyed read) / List (slice) / Load (embedded) / New (ctor)."
  echo "         See docs/architecture/lexicon.md (verb lexicon)."
  echo "$hits" | sed 's/^/  /'
  fail=1
fi

# 2) slog is the only logger.
hits=$(grep -rnE '"(github\.com/rs/zerolog|go\.uber\.org/zap[a-z/]*|github\.com/sirupsen/logrus)"|^[[:space:]]*"log"$' \
  --include='*.go' --exclude='*_test.go' internal/ cmd/ pkg/ 2>/dev/null || true)
if [ -n "$hits" ]; then
  echo "LEXICON: non-slog logger import — log/slog is the only logger (engineering-standards, Go idioms)."
  echo "$hits" | sed 's/^/  /'
  fail=1
fi

# ─── RATCHET rules ───────────────────────────────────────────────────

current=$(mktemp)
trap 'rm -f "$current"' EXIT

{
  # coin: Coin-identifiers for assets. NOT violations: the vendor
  # names Coinbase / CoinGecko / CoinMarketCap (+ the CMC poller's
  # cmcCoin wire type, which mirrors the vendor's JSON) and
  # totalCoins (Stellar's own LedgerHeader field for lumens in
  # existence — protocol vocabulary, not ours). A file counts only
  # if it has a Coin token that survives that filter.
  # NOTE: not `grep -vq` — BSD grep's -q reports raw match status and
  # ignores -v, silently dropping files on macOS. head -1 + -n test is
  # portable.
  grep -rl 'Coin' --include='*.go' --exclude='*_test.go' internal/ cmd/ pkg/ 2>/dev/null | \
    while IFS= read -r f; do
      if [ -n "$(grep -oE '[A-Za-z_]*Coin[A-Za-z_]*' "$f" | grep -vE 'Coinbase|CoinGecko|Coingecko|CoinMarketCap|cmcCoin|[Tt]otalCoins' | head -1)" ]; then
        echo "coin $f"
      fi
    done

  # positional-logger: `logger *slog.Logger` as a positional ctor param.
  grep -rlE 'func New[A-Za-z]*\([^)]*logger \*slog\.Logger' \
    --include='*.go' --exclude='*_test.go' internal/ cmd/ pkg/ 2>/dev/null | \
    sed 's/^/positional-logger /'

  # variadic-option: `...Option` functional-options ctor.
  grep -rlE 'func New[A-Za-z]*\([^)]*\.\.\.[A-Za-z]*Option\)' \
    --include='*.go' --exclude='*_test.go' internal/ cmd/ pkg/ 2>/dev/null | \
    sed 's/^/variadic-option /'
} | LC_ALL=C sort -u > "$current"

baseline_entries=$(mktemp)
trap 'rm -f "$current" "$baseline_entries"' EXIT
if [ -f "$BASELINE" ]; then
  grep -vE '^[[:space:]]*(#|$)' "$BASELINE" | LC_ALL=C sort -u > "$baseline_entries"
else
  : > "$baseline_entries"
fi

new_violations=$(LC_ALL=C comm -13 "$baseline_entries" "$current")
stale_entries=$(LC_ALL=C comm -23 "$baseline_entries" "$current")

if [ -n "$new_violations" ]; then
  echo "LEXICON: new deviation(s) not in $BASELINE — new code must use the canonical term/idiom"
  echo "         (asset not coin; Logger in the Options struct, not positional / ...Option)."
  echo "         See docs/architecture/lexicon.md + engineering-standards.md 'Go idioms'."
  echo "$new_violations" | sed 's/^/  + /'
  fail=1
fi

if [ -n "$stale_entries" ]; then
  echo "LEXICON: stale baseline entr(y/ies) — the file no longer deviates. Delete the line(s)"
  echo "         from $BASELINE (the baseline is shrink-only)."
  echo "$stale_entries" | sed 's/^/  - /'
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  exit 1
fi
echo "lint-lexicon: OK (zero rules clean; ratchet matches baseline)."
