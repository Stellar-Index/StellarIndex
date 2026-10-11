#!/usr/bin/env bash
# lint-lexicon.sh - domain-lexicon + idiom ratchet (docs/architecture/lexicon.md,
# docs/engineering-standards.md "Go idioms").
#
# ZERO rules fail immediately: `func Fetch*`/`func Make*` (use Get/List/...Batch/New/Load),
# and non-slog logging imports.
# RATCHET rules (classes coin, positional-logger, variadic-option) are grandfathered per file in
# scripts/ci/lint-lexicon.baseline as `<class> <file>`. A new deviation fails; a stale entry
# fails until deleted, so the baseline only shrinks (additions need a Baseline-Growth: trailer).
#
# Exit 0 clean, non-zero on any violation. Wired into verify.sh,
# `make lint-lexicon`, and CI's import-checks job.
#
# Usage: lint-lexicon.sh [ROOT]   (ROOT defaults to the repo root; the
# self-test points it at fixture trees, same convention as
# lint-apikey-scan.sh / lint-http-timeouts.sh). LEXICON_BASELINE overrides
# the ratchet baseline path for the same reason.
set -euo pipefail
cd "$(dirname "$0")/../.."

root="${1:-.}"
BASELINE="${LEXICON_BASELINE:-scripts/ci/lint-lexicon.baseline}"
fail=0

# Bare dirs (not ./internal) so grep output matches the baseline's path form.
if [ "$root" = "." ]; then
  dirs=(internal cmd pkg)
else
  dirs=("$root/internal" "$root/cmd" "$root/pkg")
fi

# ─── ZERO rules ──────────────────────────────────────────────────────

# 1) Verb lexicon: no Fetch*/Make* funcs in production Go.
hits=$(grep -rnE 'func (\([^)]+\) )?(Fetch|Make[A-Z])[A-Za-z]*\(' \
  --include='*.go' --exclude='*_test.go' "${dirs[@]}" 2>/dev/null || true)
if [ -n "$hits" ]; then
  echo "LEXICON: Fetch/Make verb — use Get (keyed read) / List (slice) / Load (embedded) / New (ctor)."
  echo "         See docs/architecture/lexicon.md (verb lexicon)."
  printf '%s\n' "$hits" | sed 's/^/  /'
  fail=1
fi

# 2) slog is the only logger. The stdlib arm tolerates an import alias but is anchored so
#    paths merely ending in /log do not match.
hits=$(grep -rnE '"(github\.com/rs/zerolog|go\.uber\.org/zap[a-z/]*|github\.com/sirupsen/logrus)"|(^|[[:space:]])([A-Za-z_][A-Za-z0-9_]* +)?"log"[[:space:]]*$' \
  --include='*.go' --exclude='*_test.go' "${dirs[@]}" 2>/dev/null || true)
if [ -n "$hits" ]; then
  echo "LEXICON: non-slog logger import — log/slog is the only logger (engineering-standards, Go idioms)."
  printf '%s\n' "$hits" | sed 's/^/  /'
  fail=1
fi

# ─── RATCHET rules ───────────────────────────────────────────────────

current=$(mktemp)
trap 'rm -f "$current"' EXIT

{
  # coin: Coin-identifiers for assets. Vendor names (Coinbase, CoinGecko, CoinMarketCap,
  # cmcCoin) and Stellar's totalCoins are not violations.
  # Capture the filtered output and test emptiness: BSD grep -q ignores -v, and head -1 would close the pipe.
  grep -rl 'Coin' --include='*.go' --exclude='*_test.go' "${dirs[@]}" 2>/dev/null | \
    while IFS= read -r f; do
      kept="$(grep -oE '[A-Za-z_]*Coin[A-Za-z_]*' "$f" | grep -vE 'Coinbase|CoinGecko|Coingecko|CoinMarketCap|cmcCoin|[Tt]otalCoins' || true)"
      if [ -n "$kept" ]; then
        echo "coin $f"
      fi
    done

  # positional-logger: `logger *slog.Logger` as a positional ctor param.
  grep -rlE 'func New[A-Za-z]*\([^)]*logger \*slog\.Logger' \
    --include='*.go' --exclude='*_test.go' "${dirs[@]}" 2>/dev/null | \
    sed 's/^/positional-logger /'

  # variadic-option: `...Option` functional-options ctor.
  grep -rlE 'func New[A-Za-z]*\([^)]*\.\.\.[A-Za-z]*Option\)' \
    --include='*.go' --exclude='*_test.go' "${dirs[@]}" 2>/dev/null | \
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
  printf '%s\n' "$new_violations" | sed 's/^/  + /'
  fail=1
fi

if [ -n "$stale_entries" ]; then
  echo "LEXICON: stale baseline entr(y/ies) — the file no longer deviates. Delete the line(s)"
  echo "         from $BASELINE (the baseline is shrink-only)."
  printf '%s\n' "$stale_entries" | sed 's/^/  - /'
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  exit 1
fi
echo "lint-lexicon: OK (zero rules clean; ratchet matches baseline)."
