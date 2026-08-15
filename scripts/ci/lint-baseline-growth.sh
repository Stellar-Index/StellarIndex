#!/usr/bin/env bash
# lint-baseline-growth.sh — the anti-self-bypass tripwire (CS-098).
#
# The lint gates in this repo carry self-editable escape hatches:
#   - scripts/ci/*.baseline               (grandfathered violations:
#                                          imports, lexicon, migration-compat,
#                                          rule-equivalence, ansible-drift)
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
# unless a commit message in the range carries an explicit, auditable
# trailer that NAMES the grown file:
#
#     Baseline-Growth: <watched-path> — <reason>
#
# The trailer doesn't make growth "allowed by default" — it makes it
# impossible to do SILENTLY. A reviewer (or the operator reading
# `git log`) sees the declaration next to the change.
#
# TWO properties this gate must hold, and the bugs (CID-1, audit
# 2026-08-14) that this revision closes:
#
#   1. PER-FILE SCOPING. A trailer excuses growth ONLY in the file it
#      names. The previous revision greped the whole range log for ANY
#      `Baseline-Growth:` line and, on the first match, cleared the fail
#      flag for ALL four watched surfaces at once — so a single innocuous
#      "docs-sample" trailer anywhere in the range blanket-approved a
#      broad .gitleaks.toml widening AND a .gitleaksignore fingerprint
#      that together hid a real secret. Now each grown file needs a
#      trailer that names IT.
#
#   2. NO SELF-HEAL ON DIRECT-PUSH. On direct-push-to-main the caller
#      passes BASE_SHA=github.event.before. A push that grows a baseline
#      undeclared reds exactly one run — but the FAILED commit still lands
#      on main, and the NEXT push's `before` is that failed tip, so its
#      diff window starts ABOVE the growth and the gate goes green with no
#      trailer ever consulted (observed live: 47b7c662 grew .gitleaksignore
#      undeclared, fb8c3aac healed main). We defend by re-deriving a
#      trustworthy base: walk BASE_SHA's first-parent chain back past any
#      tip commit that ITSELF introduced watched growth not declared in
#      its OWN message (a failed/undeclared tip), landing on the last
#      commit whose allowlist state was clean-or-declared, and diff from
#      there. So an undeclared growth stays caught on every push until it
#      is declared — it cannot heal by being pushed over.
#
#      NB: a *durable* last-green base (a stateless script cannot see
#      which past run was green, only the git shape) would additionally
#      close a slower adversarial heal — an actor who lands one or more
#      allowlist-free commits ON TOP of the failed growth before the next
#      push moves `before` onto a clean tip the walk stops at. Closing
#      that fully needs the workflow to supply a durable base (e.g. the
#      last successful run's sha) rather than github.event.before; see
#      the CID-1 note in .github/workflows/ci.yml's BASE_SHA env.
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

# added_entries <base> <head> <file> <grep-pattern-for-entry-lines>
# Prints lines ADDED to <file> between <base> and <head> that match the
# entry pattern (ignoring comments/blank lines).
added_entries() {
  local base="$1" head="$2" file="$3" pattern="$4"
  git diff "${base}...${head}" -- "$file" \
    | grep -E '^\+[^+]' \
    | sed 's/^+//' \
    | grep -vE '^\s*(#|$)' \
    | grep -E "$pattern" || true
}

# detect_growth <base> <head>
# Emits one TAB-separated "<watched-file>\t<added-entry>" line for every
# entry ADDED to any watched baseline/allowlist between <base> and <head>.
# The four surfaces below are exactly the escape hatches this gate exists
# to keep shrink-only; each contributes its file path as the scope token
# a Baseline-Growth trailer must name.
detect_growth() {
  local base="$1" head="$2" f added line

  # 1) scripts/ci/*.baseline: every non-comment line is an entry. Enumerate
  #    the baseline files that actually changed in the range (a working-tree
  #    glob would be wrong here — this runs against arbitrary base/head pairs,
  #    including the parent..commit windows the base-walk below inspects).
  while IFS= read -r f; do
    [[ -n "$f" ]] || continue
    added="$(added_entries "$base" "$head" "$f" '.')"
    while IFS= read -r line; do
      [[ -n "$line" ]] && printf '%s\t%s\n' "$f" "$line"
    done <<<"$added"
  done < <(git diff --name-only "${base}...${head}" -- 'scripts/ci/*.baseline' 2>/dev/null)

  # 2) KNOWN_INERT allowlist inside lint-metric-refs.sh: entries are bare
  #    stellarindex_* metric names. Matching on the metric-name shape (rather
  #    than parsing the array) keeps this robust to reformatting; a comment
  #    mentioning a metric is excluded by the comment filter in added_entries.
  added="$(added_entries "$base" "$head" scripts/ci/lint-metric-refs.sh '^\s*stellarindex_[a-z0-9_]+\s*$')"
  while IFS= read -r line; do
    [[ -n "$line" ]] && printf '%s\t%s\n' scripts/ci/lint-metric-refs.sh "$line"
  done <<<"$added"

  # 3) .gitleaksignore: every non-comment line is a per-finding fingerprint
  #    (commit:path:rule:line) that exempts a secret-scan hit — the same
  #    shrink-only allowlist shape as a *.baseline (W5-ci-2). Adding one in
  #    the same change that introduces the leak it hides silently defeats the
  #    gitleaks gate.
  added="$(added_entries "$base" "$head" .gitleaksignore '.')"
  while IFS= read -r line; do
    [[ -n "$line" ]] && printf '%s\t%s\n' .gitleaksignore "$line"
  done <<<"$added"

  # 4) .gitleaks.toml: the [allowlist]/[[rules.allowlists]] path & regex
  #    exemptions. The file is "allowlists only, no custom rules" (its own
  #    header), so every array-element line — one beginning with a quote after
  #    optional whitespace — is an exemption that WIDENS what gitleaks ignores;
  #    table headers ([...]), key=value lines (description=, paths=, id=) and
  #    comments all start with a non-quote char and are excluded.
  local gitleaks_toml_entry=$'^[[:space:]]*[\x27"]'
  added="$(added_entries "$base" "$head" .gitleaks.toml "$gitleaks_toml_entry")"
  while IFS= read -r line; do
    [[ -n "$line" ]] && printf '%s\t%s\n' .gitleaks.toml "$line"
  done <<<"$added"
}

# has_scoped_trailer <watched-file> <log-body>
# True iff <log-body> carries a Baseline-Growth trailer whose value NAMES
# <watched-file>. Both greps read from here-strings (a REDIRECTION, not a
# pipeline element) so `grep -q` closing its input early cannot make
# `set -o pipefail` observe a SIGPIPE'd writer — the exact trap the
# range-size regression (PR #38) taught this file to avoid. Piping into
# grep -q here, in any form, reintroduces it.
has_scoped_trailer() {
  local file="$1" body="$2" trailers
  trailers="$(grep -iE '^Baseline-Growth:[[:space:]]*\S' <<<"$body" || true)"
  [[ -n "$trailers" ]] || return 1
  grep -Fq -- "$file" <<<"$trailers"
}

# commit_has_undeclared_growth <commit>
# True iff <commit> itself ADDED a watched-allowlist entry that its OWN
# commit message does not declare with a scoped trailer. Such a commit is a
# failed/undeclared tip that a moving BASE_SHA must not be allowed to skip
# over (the direct-push self-heal). A root commit (no parent) is treated as
# not-undeclared so the walk terminates.
commit_has_undeclared_growth() {
  local c="$1" parent grown file entry msg
  parent="$(git rev-parse -q --verify "${c}^" 2>/dev/null)" || return 1
  grown="$(detect_growth "$parent" "$c")"
  [[ -n "${grown//[[:space:]]/}" ]] || return 1
  msg="$(git log -1 --format=%B "$c")"
  while IFS=$'\t' read -r file entry; do
    [[ -n "$file" ]] || continue
    has_scoped_trailer "$file" "$msg" || return 0
  done <<<"$grown"
  return 1
}

# Re-derive a trustworthy base (CID-1 heal defence). Walk BASE_SHA's
# first-parent chain back past every tip commit that introduced undeclared
# watched growth, landing on the last commit whose allowlist state was
# clean or properly declared. The guard bounds the walk so a pathological
# history can never spin.
effective_base="$BASE_SHA"
walk_steps=0
while git rev-parse -q --verify "${effective_base}^{commit}" >/dev/null 2>&1 \
   && commit_has_undeclared_growth "$effective_base"; do
  effective_base="$(git rev-parse "${effective_base}^")"
  walk_steps=$((walk_steps + 1))
  if [[ "$walk_steps" -gt 500 ]]; then
    echo "lint-baseline-growth: base-walk exceeded 500 commits — using ${effective_base}." >&2
    break
  fi
done
if [[ "$effective_base" != "$BASE_SHA" ]]; then
  echo "lint-baseline-growth: BASE_SHA ${BASE_SHA} sits on undeclared growth;" \
       "re-derived base ${effective_base} (CID-1 anti-heal)."
fi

# Range log body, captured into a variable rather than piped into grep -q
# (see has_scoped_trailer for the SIGPIPE-under-pipefail reason).
log_body="$(git log --format=%B "${effective_base}..HEAD")"

fail=0
declined=""
while IFS=$'\t' read -r file entry; do
  [[ -n "$file" ]] || continue
  if has_scoped_trailer "$file" "$log_body"; then
    continue
  fi
  case "$declined" in
    *"|$file|"*) : ;;
    *) echo "UNDECLARED GROWTH: $file"; declined="${declined}|$file|" ;;
  esac
  echo "  + $entry"
  fail=1
done <<<"$(detect_growth "$effective_base" HEAD)"

if [[ "$fail" -eq 0 ]]; then
  echo "lint-baseline-growth: no undeclared baseline/allowlist growth."
  exit 0
fi

cat <<'EOF'

lint-baseline-growth: FAIL — a lint baseline/allowlist grew without a
matching declaration. Baselines are shrink-only (CS-098): adding an entry
in the same change that introduces the violation it hides silently defeats
the gate.

If the growth is genuinely intended (e.g. documenting a new deliberately-
inert alert, or a reviewed false-positive secret suppression), add a
trailer to the commit message that NAMES the grown file — one trailer per
file (a trailer only excuses the file it names):

    Baseline-Growth: <watched-path> — <why this entry is legitimate>

e.g.

    Baseline-Growth: .gitleaksignore — public account id in a unit test, not a credential

EOF
exit 1
