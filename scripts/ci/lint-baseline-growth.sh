#!/usr/bin/env bash
# lint-baseline-growth.sh - anti-self-bypass tripwire for self-editable lint escape hatches:
#   scripts/ci/*.baseline, the KNOWN_INERT array in lint-metric-refs.sh, .gitleaksignore,
#   .gitleaks.toml allowlists, scripts/ci/govulncheck-allow.txt, .golangci.yml (exclusions,
#   settings, run/issues, and linters removed from enable), configs/ansible/.ansible-lint
#   skip_list, deploy/systemd/ORPHANS, the waiver maps in lint-pk-discriminators,
#   lint-derive-generation-guard and the Go guard tests (`*Exempt*|*Allowlist*|*Waiver* = map`),
#   .github/zizmor.yml and zizmor's floor flags in workflows, the inline migration baselines in
#   lint-migrations.sh, and (mutations only,
#   since new migrations append freely) scripts/ci/migration-immutability.sha256, the inline
#   escape markers in WATCHED_MARKERS in any file outside the lints, their self-tests and docs
#   (the trailer names the file the marker was added to), and the secret scan's inline allow
#   marker in any file. A lowered count in a counted baseline is a shrink, not growth.
#
# Linters fail on stale entries, but nothing stopped one commit from growing an allowlist while
# introducing the violation it hides. Any range that ADDS entries fails unless a commit message
# in the range carries a trailer naming the grown file:
#
#     Baseline-Growth: <watched-path> — <reason>
#
# Properties:
#   1. Per-file scoping: a trailer excuses growth only in the file in its path slot.
#   2. No self-heal on direct push: BASE_SHA (github.event.before) can be a failed tip, which
#      would hide its growth from the next run. So the base is re-derived by walking first-parent
#      history back past tips that add undeclared growth. A durable last-green base from the
#      workflow would also close the case where clean commits land on top of the growth.
#
# Usage: BASE_SHA=<sha> ./scripts/ci/lint-baseline-growth.sh
#   BASE_SHA — the comparison base (PR base sha, or the push event's
#              `before` sha). Unset/zero → check is skipped (first
#              push / manual local run without history context).
set -euo pipefail

cd "$(dirname "$0")/../.."

BASE_SHA="${BASE_SHA:-}"
ZERO_SHA="0000000000000000000000000000000000000000"

# Every inline escape marker a lint honours is listed in exactly one of these (lowercase EREs);
# lint-baseline-growth-test.sh fails on a `<name>:ok` / `<name>-ok:` marker in neither.
# UNWATCHED: style and shell-robustness lints whose waiver cannot hide a data or security defect.
WATCHED_MARKERS='i128:ok|i128sum:ok|lint-money:ok|migration-compat:ok|lint-priceable:ok|lint-down-delete:ok|floatmoney:ok|decimals-7:|cap-ok:|asset-identity: single-form ok|si-swallow-ok:|unbounded-latest-ok:|apikey-scan-ok:|http-timeout-ok:|exhaustive:ignore|//[[:space:]]*nolint|zizmor: ignore[[]'
# Case-sensitive, as the lints honouring them match: lowercase "do not run" prose exempts nothing.
WATCHED_MARKERS_CASE='DO NOT RUN'
# shellcheck disable=SC2034  # read by the self-test's registry check
UNWATCHED_MARKERS='sigpipe-ok:|lint-quotes:ok'

if [[ -z "$BASE_SHA" || "$BASE_SHA" == "$ZERO_SHA" ]]; then
  echo "lint-baseline-growth: no BASE_SHA — skipping (nothing to diff against)."
  exit 0
fi
if ! git cat-file -e "${BASE_SHA}^{commit}" 2>/dev/null; then
  echo "lint-baseline-growth: BASE_SHA ${BASE_SHA} not in local history — skipping." \
       "(checkout fetch-depth too shallow?)"
  exit 0
fi

# added_entries <base> <head> <file> <pattern>: non-comment lines added to <file> matching <pattern>.
added_entries() {
  local base="$1" head="$2" file="$3" pattern="$4"
  git diff "${base}...${head}" -- "$file" \
    | grep -E '^\+[^+]' \
    | sed 's/^+//' \
    | grep -vE '^\s*(#|$)' \
    | grep -E "$pattern" || true
}

# baseline_added_entries <base> <head> <file>: like added_entries, but a counted entry
# (`<key>|<n>` or `<key> <n> read(s)…`) whose key was removed with a count >= the new one is a
# shrink, not growth: the owning lints fail on a stale count, so lowering one must not need a trailer.
# A `#<digits>` fingerprint after the count names WHICH reads are excused, so a shrink must keep
# it: a new fingerprint at the same or a lower count is a swapped excused set, i.e. growth.
baseline_added_entries() {
  local base="$1" head="$2" file="$3"
  git diff "${base}...${head}" -- "$file" | awk '
    function counted(s,   rest) {
      if (match(s, /[|[:space:]][0-9]+$/) || match(s, /[|[:space:]][0-9]+[[:space:]]+read/)) {
        key = substr(s, 1, RSTART)
        rest = substr(s, RSTART + 1)
        n = rest
        sub(/[^0-9].*$/, "", n)
        fp = ""
        if (match(rest, /#[0-9]+/)) fp = substr(rest, RSTART, RLENGTH)
        return 1
      }
      return 0
    }
    /^(\+\+\+|---) / { next }
    { body = substr($0, 2) }
    body ~ /^[[:space:]]*(#|$)/ { next }
    /^-/ { if (counted(body) && (!(key in old) || n + 0 > old[key])) { old[key] = n + 0; oldfp[key] = fp }; next }
    /^\+/ { plus[++np] = body }
    END {
      for (i = 1; i <= np; i++) {
        s = plus[i]
        if (counted(s) && (key in old) && n + 0 <= old[key] && fp == oldfp[key]) continue
        print s
      }
    }
  ' || true
}

# golangci_waiver_surface <rev>: every .golangci.yml line that can relax a check (linters.*
# incl. settings and exclusions, run:, issues:), each `linters.enable` item as `ENABLE <name>`.
# output:, formatters: and version: cannot hide a finding, so they are not read as growth.
golangci_waiver_surface() {
  local rev="$1"
  git show "${rev}:.golangci.yml" 2>/dev/null | awk '
    /^[^[:space:]#]/ { top = $0; sub(/:.*/, "", top); sec = "" }
    /^[[:space:]]*(#|$)/ { next }
    top == "output" || top == "formatters" || top == "version" { next }
    top == "linters" && /^  [^[:space:]]/ { sec = $0; sub(/:.*/, "", sec); sub(/^ +/, "", sec) }
    top == "linters" && sec == "enable" && /^[[:space:]]+-/ {
      l = $0; sub(/^[[:space:]]*-[[:space:]]*/, "", l); sub(/[[:space:]]*#.*$/, "", l)
      print "ENABLE " l; next
    }
    { l = $0; sub(/^[[:space:]]+/, "", l); print top ": " l }
  '
}

# golangci_waiver_added_lines <base> <head>: waiver-surface lines added, plus enabled linters removed.
golangci_waiver_added_lines() {
  local base="$1" head="$2" b h result
  b="$(mktemp)"; h="$(mktemp)"
  golangci_waiver_surface "$base" > "$b"
  golangci_waiver_surface "$head" > "$h"
  result="$(diff "$b" "$h" 2>/dev/null | awk '
    /^> / && $2 != "ENABLE" { print substr($0, 3) }
    /^< ENABLE / { print "removed from linters.enable: " $3 }
  ' || true)"
  rm -f "$b" "$h"
  printf '%s' "$result"
}

# go_waiver_map_keys <rev> <file>: the keys of every `<x>Exempt…|<x>Allowlist…|<x>Waiver… = map[…]{`
# literal in a Go file; each key exempts one site from a guard test.
go_waiver_map_keys() {
  git show "${1}:${2}" 2>/dev/null | awk '
    !depth && /^[[:space:]]*(var[[:space:]]+)?[A-Za-z0-9_]*([Ee]xempt|[Aa]llow[Ll]ist|[Ww]aiver)[A-Za-z0-9_]*[[:space:]]*=[[:space:]]*map\[/ { inmap = 1 }
    inmap {
      if (depth == 1 && match($0, /^[[:space:]]*"[^"]*"[[:space:]]*:/)) {
        k = substr($0, RSTART, RLENGTH); sub(/^[[:space:]]+/, "", k); print k
      }
      line = $0; o = gsub(/[{]/, "", line); c = gsub(/[}]/, "", line)
      depth += o - c
      if (depth <= 0) { inmap = 0; depth = 0 }
    }
  '
}

# go_waiver_map_added_keys <base> <head> <file>: waiver-map keys in <head> absent from <base>.
go_waiver_map_added_keys() {
  local b
  b="$(go_waiver_map_keys "$1" "$3")"
  go_waiver_map_keys "$2" "$3" | awk -v old="$b" '
    BEGIN { n = split(old, o, "\n"); for (i = 1; i <= n; i++) seen[o[i]] = 1 }
    !($0 in seen) { print }
  '
}

# mutated_migration_hash_entries <base> <head>: new lines in migration-immutability.sha256
# are normal (new migration); the hazard is an existing basename's hash rewritten to match an
# edited shipped migration, so emit only entries whose hash changed.
mutated_migration_hash_entries() {
  local base="$1" head="$2" file="scripts/ci/migration-immutability.sha256" b h
  b="$(mktemp)"; h="$(mktemp)"
  git show "${base}:${file}" > "$b" 2>/dev/null || true
  git show "${head}:${file}" > "$h" 2>/dev/null || true
  awk '
    FNR == NR {
      if ($0 ~ /^[[:space:]]*(#|$)/) next
      want[$2] = $1
      next
    }
    {
      if ($0 ~ /^[[:space:]]*(#|$)/) next
      if (($2 in want) && want[$2] != $1) print $1 "  " $2
    }
  ' "$b" "$h" || true
  rm -f "$b" "$h"
}

# detect_growth <base> <head>: emits "<watched-file>\t<added-entry>" for each entry added to a
# watched surface; the file path is the scope token a trailer must name.
detect_growth() {
  local base="$1" head="$2" f added line

  # 1) scripts/ci/*.baseline: enumerate files changed in the range, not a working-tree glob,
  #    since this runs against arbitrary base/head pairs.
  while IFS= read -r f; do
    [[ -n "$f" ]] || continue
    added="$(baseline_added_entries "$base" "$head" "$f")"
    while IFS= read -r line; do
      [[ -n "$line" ]] && printf '%s\t%s\n' "$f" "$line"
    done <<<"$added"
  done < <(git diff --name-only "${base}...${head}" -- 'scripts/ci/*.baseline' 2>/dev/null)

  # 2) The waiver arrays in lint-metric-refs.sh: match the stellarindex_* name shape, not the
  #    array, with or without the trailing `# reason` the arrays ask for.
  added="$(added_entries "$base" "$head" scripts/ci/lint-metric-refs.sh '^\s*stellarindex_[a-z0-9_]+\s*(#.*)?$')"
  while IFS= read -r line; do
    [[ -n "$line" ]] && printf '%s\t%s\n' scripts/ci/lint-metric-refs.sh "$line"
  done <<<"$added"

  # 3) .gitleaksignore: each non-comment line is a finding fingerprint.
  added="$(added_entries "$base" "$head" .gitleaksignore '.')"
  while IFS= read -r line; do
    [[ -n "$line" ]] && printf '%s\t%s\n' .gitleaksignore "$line"
  done <<<"$added"

  # 4) .gitleaks.toml: allowlists-only file, so each quoted array element widens what is ignored.
  local gitleaks_toml_entry=$'^[[:space:]]*[\x27"]'
  added="$(added_entries "$base" "$head" .gitleaks.toml "$gitleaks_toml_entry")"
  while IFS= read -r line; do
    [[ -n "$line" ]] && printf '%s\t%s\n' .gitleaks.toml "$line"
  done <<<"$added"

  # 5) govulncheck-allow.txt: one accepted-risk id per line (a .txt, so the *.baseline glob misses it).
  added="$(added_entries "$base" "$head" scripts/ci/govulncheck-allow.txt '^GO-')"
  while IFS= read -r line; do
    [[ -n "$line" ]] && printf '%s\t%s\n' scripts/ci/govulncheck-allow.txt "$line"
  done <<<"$added"

  # 6) .golangci.yml; see golangci_waiver_added_lines().
  added="$(golangci_waiver_added_lines "$base" "$head")"
  while IFS= read -r line; do
    [[ -n "$line" ]] && printf '%s\t%s\n' .golangci.yml "$line"
  done <<<"$added"

  # 7) .ansible-lint skip_list: each entry silences a rule class repo-wide, found by auto-discovery.
  added="$(added_entries "$base" "$head" configs/ansible/.ansible-lint '^\s*-\s+\S')"
  while IFS= read -r line; do
    [[ -n "$line" ]] && printf '%s\t%s\n' configs/ansible/.ansible-lint "$line"
  done <<<"$added"

  # 8) deploy/systemd/ORPHANS: declaring a unit silences the orphan check for it.
  added="$(added_entries "$base" "$head" deploy/systemd/ORPHANS '.')"
  while IFS= read -r line; do
    [[ -n "$line" ]] && printf '%s\t%s\n' deploy/systemd/ORPHANS "$line"
  done <<<"$added"

  # 9) migration-immutability.sha256: mutated entries only.
  added="$(mutated_migration_hash_entries "$base" "$head")"
  while IFS= read -r line; do
    [[ -n "$line" ]] && printf '%s\t%s\n' scripts/ci/migration-immutability.sha256 "$line"
  done <<<"$added"

  # 10) The in-source waiver maps of the two Go lints: each `"table": "reason"` line exempts a table.
  for f in scripts/ci/lint-pk-discriminators/main.go scripts/ci/lint-derive-generation-guard/main.go; do
    added="$(added_entries "$base" "$head" "$f" '^\s*"[a-z0-9_]+":\s*"')"
    while IFS= read -r line; do
      [[ -n "$line" ]] && printf '%s\t%s\n' "$f" "$line"
    done <<<"$added"
  done

  # 10b) Go guard tests' waiver maps (closedVWAPSeamExempt, directionExempt, …): a new key exempts
  #      a site from a money or API guard, found by the map's name, not a hand-kept file list.
  while IFS= read -r f; do
    [[ -n "$f" ]] || continue
    added="$(go_waiver_map_added_keys "$base" "$head" "$f")"
    while IFS= read -r line; do
      [[ -n "$line" ]] && printf '%s\t%s\n' "$f" "$line"
    done <<<"$added"
  done < <(git diff --name-only "${base}...${head}" -- '*.go' 2>/dev/null)

  # 14) zizmor: its config file waives audits repo-wide, and the workflow step sets its floor.
  added="$(added_entries "$base" "$head" .github/zizmor.yml '.')"
  while IFS= read -r line; do
    [[ -n "$line" ]] && printf '%s\t%s\n' .github/zizmor.yml "$line"
  done <<<"$added"
  while IFS= read -r f; do
    [[ -n "$f" ]] || continue
    added="$(added_entries "$base" "$head" "$f" 'min-(confidence|severity)|persona')"
    while IFS= read -r line; do
      [[ -n "$line" ]] && printf '%s\t%s\n' "$f" "$line"
    done <<<"$added"
  done < <(git diff --name-only "${base}...${head}" -- '.github/workflows/*' 2>/dev/null)

  # 12) The inline baselines in lint-migrations.sh: each line naming a shipped migration
  #     (`NNNN_x.up.sql[:name]`, opening `list='` or closing `'` included) exempts it from a pass.
  added="$(added_entries "$base" "$head" scripts/ci/lint-migrations.sh \
    "^[[:space:]]*([a-z_]+=[\"'])?[0-9]{4}_[a-z0-9_]+\.(up|down)\.sql(:[a-z0-9_]+)?[\"']?[[:space:]]*\$")"
  while IFS= read -r line; do
    [[ -n "$line" ]] && printf '%s\t%s\n' scripts/ci/lint-migrations.sh "$line"
  done <<<"$added"

  # 11) Inline escape markers (WATCHED_MARKERS): each added marker exempts one line. A marker on
  #     its own comment line governs the line below, so a rewrite of that line is growth too.
  #     The lints that honour them, their self-tests and the docs only name a marker, so they are
  #     skipped.
  git diff -U1 "${base}...${head}" -- . ':(exclude)*.md' ':(exclude)scripts/ci/lint-*' \
      ':(exclude)scripts/ci/*-test.sh' 2>/dev/null \
      | awk -v re="$WATCHED_MARKERS" -v cre="$WATCHED_MARKERS_CASE" '
    function marked(s) { return tolower(s) ~ re || s ~ cre }
    /^\+\+\+ / { f = substr($0, 7); prev = ""; next }
    /^(diff --git|index) / || /^@@/ { prev = ""; next }
    /^-/ { next }
    {
      body = substr($0, 2)
      if (/^\+/ && (marked(body) || (marked(prev) && prev ~ /^[[:space:]]*(\/\/|#|--)/))) print f "\t" body
      prev = body
    }
  '

  # 13) The secret scan's inline allow marker: gitleaks skips any line carrying it, in any file
  #     type, so one added marker lands a real secret with a green scan.
  git diff -U0 "${base}...${head}" 2>/dev/null | awk '
    /^\+\+\+ / { f = substr($0, 7); next }
    /^\+/ && tolower($0) ~ /gitleaks[:]allow/ { print f "\t" substr($0, 2) }
  '
}

# has_scoped_trailer <watched-file> <log-body>: true iff a Baseline-Growth trailer names the file.
# Greps read here-strings, not pipes: grep -q closing early would SIGPIPE a writer under pipefail
# (pinned in lint-baseline-growth-test.sh).
has_scoped_trailer() {
  local file="$1" body="$2" paths
  # Only the path slot scopes a trailer: a file named in some other trailer's reason is not declared.
  paths="$(awk 'tolower($0) ~ /^baseline-growth:[[:space:]]*[^[:space:]]/ {
    sub(/^[^:]*:[[:space:]]*/, ""); sub(/[[:space:]].*$/, ""); print
  }' <<<"$body")"
  [[ -n "$paths" ]] || return 1
  grep -Fxq -- "$file" <<<"$paths"
}

# commit_has_undeclared_growth <commit>: true iff the commit added a watched entry its own message
# does not declare. A root commit counts as clean so the walk terminates.
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

# Re-derive a trustworthy base: walk back past tips with undeclared growth to the last clean or
# declared commit. The step cap stops a pathological history from spinning.
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

# Captured into a variable, not piped into grep -q (SIGPIPE under pipefail; see has_scoped_trailer).
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
