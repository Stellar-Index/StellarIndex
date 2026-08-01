#!/usr/bin/env bash
# lint-baseline-growth.sh — the anti-self-bypass tripwire (CS-098).
#
# The lint gates in this repo carry self-editable escape hatches:
#   - scripts/ci/lint-imports.baseline   (grandfathered import violations)
#   - scripts/ci/lint-metric-refs.sh     (the KNOWN_INERT allowlist array)
#   - .gitleaksignore                     (per-finding secret-scan fingerprints)
#   - .gitleaks.toml                      ([allowlist]/[[rules.allowlists]]
#                                          path/regex exemptions)
#
# Each is designed to shrink monotonically, and the linters already
# fail on STALE entries — but nothing stopped a commit from GROWING
# the allowlist in the same change that introduces the violation it
# hides (audit 2026-06-30, CS-098; the gitleaks pair added for W5-ci-2,
# where a PR could add an ignore fingerprint or a broad path allowlist
# that silences a REAL leak in the same diff). This check closes that
# hole: any commit range that ADDS entries to a baseline/allowlist fails
# unless the commit message carries an explicit, auditable trailer:
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

# report_growth <banner> <added-lines> — print the banner, list every added
# entry indented, and mark the run failed. One helper so every allowlist block
# reports identically.
report_growth() {
  echo "$1"
  local line
  while IFS= read -r line; do
    [[ -n "$line" ]] && echo "  + $line"
  done <<<"$2"
  fail=1
}

# 1) *.baseline files: every non-comment line is an entry.
for f in scripts/ci/*.baseline; do
  [[ -e "$f" ]] || continue
  added="$(added_entries "$f" '.')"
  [[ -n "$added" ]] && report_growth "BASELINE GREW: $f" "$added"
done

# 2) KNOWN_INERT allowlist inside lint-metric-refs.sh: entries are
#    bare stellarindex_* metric names. Matching on the metric-name
#    shape (rather than parsing the array) keeps this robust to
#    reformatting; a comment mentioning a metric is excluded by the
#    comment filter above.
added="$(added_entries scripts/ci/lint-metric-refs.sh '^\s*stellarindex_[a-z0-9_]+\s*$')"
[[ -n "$added" ]] && report_growth \
  "ALLOWLIST GREW: scripts/ci/lint-metric-refs.sh KNOWN_INERT" "$added"

# 3) .gitleaksignore: every non-comment line is a per-finding fingerprint
#    (commit:path:rule:line) that exempts a secret-scan hit — the same
#    shrink-only allowlist shape as a *.baseline (W5-ci-2). Adding one in
#    the same change that introduces the leak it hides silently defeats the
#    gitleaks gate, and the gate's own anti-self-bypass restore (ci.yml's
#    import-checks job) pins THIS script to the base ref, so a PR cannot both
#    add the fingerprint and weaken the check that catches it.
added="$(added_entries .gitleaksignore '.')"
[[ -n "$added" ]] && report_growth \
  "ALLOWLIST GREW: .gitleaksignore (secret-scan fingerprint exemptions)" "$added"

# 4) .gitleaks.toml: the [allowlist]/[[rules.allowlists]] path & regex
#    exemptions. The file is "allowlists only, no custom rules" (its own
#    header), so every array-element line — one beginning with a quote after
#    optional whitespace — is an exemption that WIDENS what gitleaks ignores;
#    table headers ([...]), key=value lines (description=, paths=, id=) and
#    comments all start with a non-quote char and are excluded. A broad new
#    path/regex here can blind the scanner to a live file (W5-ci-2).
GITLEAKS_TOML_ENTRY=$'^[[:space:]]*[\x27"]'
added="$(added_entries .gitleaks.toml "$GITLEAKS_TOML_ENTRY")"
[[ -n "$added" ]] && report_growth \
  "ALLOWLIST GREW: .gitleaks.toml (secret-scan path/regex exemptions)" "$added"

if [[ "$fail" -eq 0 ]]; then
  echo "lint-baseline-growth: no baseline/allowlist growth."
  exit 0
fi

# Growth found — permitted only with an explicit declaration in a
# commit message within the range.
# NB: the log is captured into a variable rather than piped straight
# into `grep -q`. Under `set -o pipefail`, `grep -q` exits at the FIRST
# match and closes the pipe; `git log` — which emits ~160KB here, far
# more than the 64KB pipe buffer — then takes SIGPIPE and reports 141,
# which pipefail surfaces as the pipeline's status. The `if` would read
# that as "no trailer found" and fail a PR that HAD declared its growth.
# It only ever misfires in the fail-safe direction (SIGPIPE requires a
# match to have been found), but it made the gate nondeterministic:
# whether git finished writing before grep exited depended on scheduling,
# and this repo's branch history sits right on that boundary. Real
# instance: PR #38 passed locally and failed in CI on identical inputs.
# The here-string matters: it is a REDIRECTION, not a pipeline element,
# so `grep -q` closing its input early cannot make pipefail see a
# SIGPIPE'd writer. Piping into `grep -q` — in any form, `git log | ...`
# or `printf | ...` — reintroduces the bug.
log_body="$(git log --format=%B "${BASE_SHA}..HEAD")"
if grep -qE '^Baseline-Growth:\s*\S' <<<"$log_body"; then
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
