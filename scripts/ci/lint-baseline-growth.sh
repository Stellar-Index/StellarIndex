#!/usr/bin/env bash
# lint-baseline-growth.sh — the anti-self-bypass tripwire (CS-098).
#
# The lint gates in this repo carry self-editable escape hatches:
#   - scripts/ci/lint-imports.baseline   (grandfathered import violations)
#   - scripts/ci/lint-metric-refs.sh     (the KNOWN_INERT allowlist array)
#
# Each is designed to shrink monotonically, and the linters already
# fail on STALE entries — but nothing stopped a commit from GROWING
# the allowlist in the same change that introduces the violation it
# hides (audit 2026-06-30, CS-098). This check closes that hole: any
# commit range that ADDS entries to a baseline/allowlist fails unless
# the commit message carries an explicit, auditable trailer:
#
#     Baseline-Growth: <reason>
#
# The trailer doesn't make growth "allowed by default" — it makes it
# impossible to do SILENTLY. A reviewer (or the operator reading
# `git log`) sees the declaration next to the change.
#
# Usage: BASE_SHA=<sha> ./scripts/ci/lint-baseline-growth.sh
#   BASE_SHA — the comparison base (PR base sha, or the push event's
#              `before` sha). Unset/zero → check is skipped (first
#              push / manual local run without history context).
set -euo pipefail

cd "$(dirname "$0")/../.."

BASE_SHA="${BASE_SHA:-}"
ZERO_SHA="0000000000000000000000000000000000000000"

if [[ -z "$BASE_SHA" || "$BASE_SHA" == "$ZERO_SHA" ]]; then
  echo "lint-baseline-growth: no BASE_SHA — skipping (nothing to diff against)."
  exit 0
fi
if ! git cat-file -e "${BASE_SHA}^{commit}" 2>/dev/null; then
  echo "lint-baseline-growth: BASE_SHA ${BASE_SHA} not in local history — skipping." \
       "(checkout fetch-depth too shallow?)"
  exit 0
fi

fail=0

# added_entries <file> <grep-pattern-for-entry-lines>
# Prints lines ADDED to <file> since BASE_SHA that match the entry
# pattern (ignoring comments/blank lines).
added_entries() {
  local file="$1" pattern="$2"
  git diff "${BASE_SHA}...HEAD" -- "$file" \
    | grep -E '^\+[^+]' \
    | sed 's/^+//' \
    | grep -vE '^\s*(#|$)' \
    | grep -E "$pattern" || true
}

# 1) *.baseline files: every non-comment line is an entry.
for f in scripts/ci/*.baseline; do
  [[ -e "$f" ]] || continue
  added="$(added_entries "$f" '.')"
  if [[ -n "$added" ]]; then
    echo "BASELINE GREW: $f"
    echo "$added" | sed 's/^/  + /'
    fail=1
  fi
done

# 2) KNOWN_INERT allowlist inside lint-metric-refs.sh: entries are
#    bare stellarindex_* metric names. Matching on the metric-name
#    shape (rather than parsing the array) keeps this robust to
#    reformatting; a comment mentioning a metric is excluded by the
#    comment filter above.
added="$(added_entries scripts/ci/lint-metric-refs.sh '^\s*stellarindex_[a-z0-9_]+\s*$')"
if [[ -n "$added" ]]; then
  echo "ALLOWLIST GREW: scripts/ci/lint-metric-refs.sh KNOWN_INERT"
  echo "$added" | sed 's/^/  + /'
  fail=1
fi

if [[ "$fail" -eq 0 ]]; then
  echo "lint-baseline-growth: no baseline/allowlist growth."
  exit 0
fi

# Growth found — permitted only with an explicit declaration in a
# commit message within the range.
if git log --format=%B "${BASE_SHA}..HEAD" | grep -qE '^Baseline-Growth:\s*\S'; then
  echo "lint-baseline-growth: growth declared via 'Baseline-Growth:' trailer — allowed (audit trail in git log)."
  exit 0
fi

cat <<'EOF'

lint-baseline-growth: FAIL — a lint baseline/allowlist grew without a
declaration. Baselines are shrink-only (CS-098): adding an entry in
the same change that introduces the violation it hides silently
defeats the gate.

If the growth is genuinely intended (e.g. documenting a new
deliberately-inert alert), add an explicit trailer to the commit
message:

    Baseline-Growth: <why this entry is legitimate>

EOF
exit 1
