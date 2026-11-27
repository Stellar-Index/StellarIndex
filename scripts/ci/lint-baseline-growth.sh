#!/usr/bin/env bash
# lint-baseline-growth.sh - anti-self-bypass tripwire for self-editable lint escape hatches:
#   scripts/ci/*.baseline, the KNOWN_INERT array in lint-metric-refs.sh, .gitleaksignore,
#   .gitleaks.toml allowlists, scripts/ci/govulncheck-allow.txt, .golangci.yml (exclusions,
#   settings, run/issues, and linters removed from enable), configs/ansible/.ansible-lint
#   skip_list, deploy/systemd/ORPHANS, the waiver maps in lint-pk-discriminators,
#   lint-derive-generation-guard and the Go guard tests (`*Exempt*|*Allow(ed|list)*|*Waiver*|
#   *Ignore(d)* (:)= map`), the roles' systemd HARDENING-EXCEPTIONS, the RULES allow lists in
#   lint-imports.sh, .github/zizmor.yml and zizmor's floor flags in workflows, alternate
#   golangci/zizmor config files, diff-hiding .gitattributes lines, the inline migration
#   baselines in lint-migrations.sh, and (mutations only,
#   since new migrations append freely) scripts/ci/migration-immutability.sha256, the inline
#   escape markers in WATCHED_MARKERS in any file outside the lints and their self-tests (docs
#   are read for WATCHED_MARKERS_CASE only; the trailer names the file the marker was added
#   to), and the secret scan's inline allow marker in any file. A lowered count in a counted
#   baseline is a shrink, not growth.
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
WATCHED_MARKERS='chzone:ok|i128:ok|i128sum:ok|lint-money:ok|migration-compat:ok|lint-priceable:ok|lint-down-delete:ok|floatmoney:ok|decimals-7:|cap-ok:|asset-identity: single-form ok|si-swallow-ok:|unbounded-latest-ok:|apikey-scan-ok:|http-timeout-ok:|exhaustive:ignore|zizmor: ignore[[]'
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

# Content diffs ignore .gitattributes: a `-diff`/`binary` attribute would print "Binary files
# differ" instead of the added lines, and a textconv or external driver would rewrite them.
GIT_DIFF=(git diff --text --no-textconv --no-ext-diff)

# added_entries <base> <head> <file> <pattern>: non-comment lines added to <file> matching <pattern>.
added_entries() {
  local base="$1" head="$2" file="$3" pattern="$4"
  "${GIT_DIFF[@]}" "${base}...${head}" -- "$file" \
    | grep -E '^\+[^+]' \
    | sed 's/^+//' \
    | grep -vE '^\s*(#|$)' \
    | grep -E "$pattern" || true
}

# removed_entries <base> <head> <file> <pattern>: added_entries for the lines the range deletes.
removed_entries() {
  local base="$1" head="$2" file="$3" pattern="$4"
  "${GIT_DIFF[@]}" "${base}...${head}" -- "$file" \
    | grep -E '^-[^-]' \
    | sed 's/^-//' \
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
  "${GIT_DIFF[@]}" "${base}...${head}" -- "$file" | awk '
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

# golangci_waiver_added_lines <base> <head>: waiver-surface lines added, enabled linters removed,
# and removed qualifiers: without `generated:` the default skips every generated-banner file, and
# an exclusion rule without its path/text/source/linters key matches more.
golangci_waiver_added_lines() {
  local base="$1" head="$2" b h result
  b="$(mktemp)"; h="$(mktemp)"
  golangci_waiver_surface "$base" > "$b"
  golangci_waiver_surface "$head" > "$h"
  result="$(diff "$b" "$h" 2>/dev/null | awk '
    /^> / && $2 != "ENABLE" { print substr($0, 3) }
    /^< ENABLE / { print "removed from linters.enable: " $3 }
    /^< [a-z]+: (- )?(generated|path|path-except|text|source|linters):/ { print "removed qualifier: " substr($0, 3) }
  ' || true)"
  rm -f "$b" "$h"
  printf '%s' "$result"
}

# go_waiver_map_keys <rev> <file>: the entries of every `<x>Exempt…|<x>Allow(ed|list)…|<x>Waiver…|
# <x>Ignore(d)… (:)= map[…]{` literal in a Go file, plus <x>Exclu(de|ded|sion)…|<x>Exception… in a
# _test.go file; each key exempts one site from a guard test.
# Every key of the literal is read, any key expression, several per line included. A string or
# struct value is a reason and is dropped; any other value (a count, a bool) is part of the entry,
# so raising it is growth.
go_waiver_map_keys() {
  local intest=0
  case "$2" in *_test.go) intest=1 ;; esac
  git show "${1}:${2}" 2>/dev/null | awk -v intest="$intest" '
    function trim(s) { sub(/^[[:space:]]+/, "", s); sub(/[[:space:]]+$/, "", s); return s }
    function flush(   k, v) {
      if (col) {
        k = trim(substr(unit, 1, col - 1)); v = trim(substr(unit, col + 1))
        if (v ~ /^["`{&]/ || v == "") print k; else print k " " v
      }
      unit = ""; col = 0
    }
    function scan(s,   i, ch, n) {
      n = length(s)
      for (i = 1; i <= n; i++) {
        ch = substr(s, i, 1)
        if (instr) {
          if (depth >= 1) unit = unit ch
          if (ch == "\\" && q == "\"") { i++; if (depth >= 1) unit = unit substr(s, i, 1); continue }
          if (ch == q) instr = 0
          continue
        }
        if (ch == "/" && substr(s, i + 1, 1) == "/") break
        if (ch == "\"" || ch == "`") { instr = 1; q = ch; if (depth >= 1) unit = unit ch; continue }
        if (ch == "{") { depth++; if (depth == 1) { unit = ""; col = 0; continue } }
        if (ch == "}") {
          if (depth == 1) { flush(); depth = 0; inmap = 0; return }
          depth--
        }
        if (depth < 1) continue
        if (depth == 1 && ch == ",") { flush(); continue }
        if (depth == 1 && ch == ":" && !col) col = length(unit) + 1
        unit = unit ch
      }
      if (depth >= 1) unit = unit " "
    }
    # A map named …Proof(s) lists evidence (a test per covered site): growing it tightens a guard.
    # …Exclu…/…Exception… name a waiver map only in a _test.go guard; in served code they are domain
    # vocabulary (reason texts for excluded days, sources), and growing one hides nothing.
    !inmap && $0 !~ /Proofs?[[:space:]]*:?=/ && (/^[[:space:]]*(var[[:space:]]+)?[A-Za-z0-9_]*([Ee]xempt|[Aa]llow(ed|[Ll]ist)?|[Ww]aiver|[Ii]gnored?)[A-Za-z0-9_]*[[:space:]]*:?=[[:space:]]*map\[/ || (intest && /^[[:space:]]*(var[[:space:]]+)?[A-Za-z0-9_]*([Ee]xclu(de|ded|sions?)|[Ee]xceptions?)[A-Za-z0-9_]*[[:space:]]*:?=[[:space:]]*map\[/)) {
      inmap = 1; depth = 0; instr = 0; unit = ""; col = 0
      s = $0; sub(/^[^=]*=[[:space:]]*map\[/, "", s)
      # Collapse struct{…}/interface{…} in the value TYPE, innermost first, so the literal opens
      # at the first remaining `{`; groups after that brace are values and are left alone. A type
      # still open after collapsing spans lines: join the next one and collapse again.
      for (;;) {
        while (match(s, /(struct|interface)[[:space:]]*[{][^{}]*[}]/)) {
          pre = substr(s, 1, RSTART - 1); gsub(/(struct|interface)[[:space:]]*[{]/, "", pre)
          if (pre ~ /[{]/) break
          s = substr(s, 1, RSTART - 1) "T" substr(s, RSTART + RLENGTH)
        }
        if (s !~ /^[^{]*(struct|interface)[[:space:]]*[{]/ || (getline nxt) <= 0) break
        sub(/\/\/.*$/, "", nxt); s = s " " nxt
      }
      scan(s); next
    }
    inmap { scan($0) }
  '
}

# go_waiver_map_added_keys <base> <head> <file>: waiver-map keys in <head> absent from <base>.
go_waiver_map_added_keys() {
  local b
  b="$(go_waiver_map_keys "$1" "$3")"
  # Through the environment: -v aborts on a quote plus newline and expands backslashes in keys.
  go_waiver_map_keys "$2" "$3" | old="$b" awk '
    BEGIN { n = split(ENVIRON["old"], o, "\n"); for (i = 1; i <= n; i++) seen[o[i]] = 1 }
    !($0 in seen) { print }
  '
}

# imports_allow_entries <rev>: "<rule> <entry>" for each string in an `"allow"`/`"allow_internal"`
# list of lint-imports.sh's RULES, and "<rule> exempt_importers <entry>" for each in a layering
# rule's `"exempt_importers"`, so a new entry or one moved to another rule reads as new.
imports_allow_entries() {
  git show "${1}:scripts/ci/lint-imports.sh" 2>/dev/null | awk '
    { s = $0; sub(/#.*$/, "", s) }
    match(s, /"name"[[:space:]]*:[[:space:]]*"[^"]*"/) { r = substr(s, RSTART, RLENGTH); sub(/^[^:]*:[[:space:]]*/, "", r); rule = r }
    !inblk && s ~ /"(allow(_internal)?|exempt_importers)"[[:space:]]*:/ {
      inblk = 1; tag = (s ~ /"exempt_importers"/) ? " exempt_importers" : ""
      s = substr(s, index(s, ":") + 1)
    }
    inblk {
      while (match(s, /"[^"]*"/)) { print rule tag " " substr(s, RSTART, RLENGTH); s = substr(s, RSTART + RLENGTH) }
      if (index(s, "]") || index(s, "}") || index(s, "set()")) inblk = 0
    }
  '
}

# imports_allow_added <base> <head>: imports_allow_entries in <head> absent from <base>.
imports_allow_added() {
  local b
  b="$(imports_allow_entries "$1")"
  imports_allow_entries "$2" | old="$b" awk '
    BEGIN { n = split(ENVIRON["old"], o, "\n"); for (i = 1; i <= n; i++) seen[o[i]] = 1 }
    !($0 in seen) { print }
  '
}

# api_env_allow_words <rev>: the names in lint-systemd-hardening.sh's inline API_ENV_ALLOW list.
api_env_allow_words() {
  git show "${1}:scripts/ci/lint-systemd-hardening.sh" 2>/dev/null \
    | sed -nE 's/^[[:space:]]*API_ENV_ALLOW="([^"]*)".*/\1/p' | tr -s ' ' '\n' | grep -v '^$' || true
}

# api_env_allow_added <base> <head>: names in <head>'s API_ENV_ALLOW absent from <base>'s.
api_env_allow_added() {
  local b
  b="$(api_env_allow_words "$1")"
  api_env_allow_words "$2" | old="$b" awk '
    BEGIN { n = split(ENVIRON["old"], o, "\n"); for (i = 1; i <= n; i++) seen[o[i]] = 1 }
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
  #    array, quoted or bare, one or several per line, inline `X=(…)` included, with or without
  #    the trailing `# reason` the arrays ask for.
  added="$(added_entries "$base" "$head" scripts/ci/lint-metric-refs.sh \
    "^[[:space:]]*([A-Z_]+\\+?=\\()?[[:space:]]*([\"']?stellarindex_[a-z0-9_]+[\"']?[[:space:]]*)+\\)?[[:space:]]*(#.*)?\$")"
  while IFS= read -r line; do
    [[ -n "$line" ]] && printf '%s\t%s\n' scripts/ci/lint-metric-refs.sh "$line"
  done <<<"$added"

  # 3) .gitleaksignore: each non-comment line is a finding fingerprint.
  added="$(added_entries "$base" "$head" .gitleaksignore '.')"
  while IFS= read -r line; do
    [[ -n "$line" ]] && printf '%s\t%s\n' .gitleaksignore "$line"
  done <<<"$added"

  # 4) .gitleaks.toml: allowlists-only file. Any added line but a description, a title or a closing
  #    bracket counts, so an inline array, a new block or a commit exemption cannot hide by spelling.
  local gitleaks_toml_inert=$'^[[:space:]]*((description|title)[[:space:]]*=|\\][[:space:]]*,?[[:space:]]*$)'
  added="$(added_entries "$base" "$head" .gitleaks.toml '.' | grep -vE "$gitleaks_toml_inert" || true)"
  while IFS= read -r line; do
    [[ -n "$line" ]] && printf '%s\t%s\n' .gitleaks.toml "$line"
  done <<<"$added"
  # Removing a qualifier widens too: no `condition = "AND"` falls back to OR, no `useDefault`
  # drops the default rules, no `targetRules` applies an allowlist to every rule.
  local gitleaks_toml_qualifier='^[[:space:]]*(\[|(condition|useDefault|regexTarget|targetRules|paths?)[[:space:]]*=)'
  added="$(removed_entries "$base" "$head" .gitleaks.toml "$gitleaks_toml_qualifier")"
  while IFS= read -r line; do
    [[ -n "$line" ]] && printf '%s\tremoved: %s\n' .gitleaks.toml "$line"
  done <<<"$added"

  # 5) govulncheck-allow.txt: one accepted-risk id per line (a .txt, so the *.baseline glob misses it).
  #    The gate reads any GO- id outside a comment, indented or not.
  added="$(added_entries "$base" "$head" scripts/ci/govulncheck-allow.txt '^[^#]*GO-[0-9]{4}-[0-9]+')"
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

  # 10c) lint-systemd-hardening's HARDENING-EXCEPTIONS (one per role): each line waives a
  #      sandbox directive for a unit, and the file is not a *.baseline.
  while IFS= read -r f; do
    [[ -n "$f" ]] || continue
    added="$(added_entries "$base" "$head" "$f" '.')"
    while IFS= read -r line; do
      [[ -n "$line" ]] && printf '%s\t%s\n' "$f" "$line"
    done <<<"$added"
  done < <(git diff --name-only "${base}...${head}" -- \
    ':(glob)configs/ansible/roles/*/templates/systemd/HARDENING-EXCEPTIONS' 2>/dev/null)

  # 10d) lint-imports.sh's per-rule allow lists: CI runs the base copy, so a widened list is not
  #      used on the PR that adds it, but it is the base for every later one.
  added="$(imports_allow_added "$base" "$head")"
  while IFS= read -r line; do
    [[ -n "$line" ]] && printf '%s\t%s\n' scripts/ci/lint-imports.sh "$line"
  done <<<"$added"

  # 10e) the environment names the API process may see: growing the inline list admits a credential.
  added="$(api_env_allow_added "$base" "$head")"
  while IFS= read -r line; do
    [[ -n "$line" ]] && printf '%s\t%s\n' scripts/ci/lint-systemd-hardening.sh "$line"
  done <<<"$added"

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

  # 11) Inline escape markers (WATCHED_MARKERS): each added marker exempts one line. A marker
  #     governs the line below too (lint-unflagged-limit and lint-ansible-tasks read it on any
  #     previous line), so any line added below one is growth. The lints that honour them and
  #     their self-tests only name a marker, so they are skipped.
  #     Docs are read for the case-sensitive markers only: runbook SQL honours `DO NOT RUN`.
  # shellcheck disable=SC2016  # an awk program, not a shell expansion
  local marker_awk='
    function marked(s) { return (re != "" && tolower(s) ~ re) || s ~ cre }
    function comment(s) { return s ~ /^[[:space:]]*(\/\/|#|--)/ }
    /^\+\+\+ / { f = substr($0, 7); prev = ""; next }
    /^(diff --git|index) / || /^@@/ { prev = ""; next }
    /^-/ { next }
    {
      body = substr($0, 2); add = /^\+/
      # Any added non-blank line directly under a marker is governed by it, whether the marker
      # line is a comment or code, added or untouched context.
      if (add && (marked(body) || (marked(prev) && body !~ /^[[:space:]]*$/))) print f "\t" body
      prev = body
    }
  '
  "${GIT_DIFF[@]}" -U1 "${base}...${head}" -- . ':(exclude)*.md' ':(exclude)scripts/ci/lint-*' \
      ':(exclude)scripts/ci/*-test.sh' 2>/dev/null \
      | awk -v re="$WATCHED_MARKERS" -v cre="$WATCHED_MARKERS_CASE" "$marker_awk"
  "${GIT_DIFF[@]}" -U1 "${base}...${head}" -- '*.md' 2>/dev/null \
      | awk -v re="" -v cre="$WATCHED_MARKERS_CASE" "$marker_awk"

  # 15) Tool configs under another name: golangci-lint and zizmor also load these spellings, so
  #     adding one (or deleting the watched file) swaps in a config the steps above never read.
  while IFS= read -r f; do
    [[ -n "$f" ]] && printf '%s\t%s\n' "$f" "added alternate tool config"
  done < <(git diff --name-only --diff-filter=AR "${base}...${head}" -- \
    ':(glob)**/.golangci.yaml' ':(glob)**/.golangci.toml' ':(glob)**/.golangci.json' \
    ':(glob)**/zizmor.yml' ':(glob)**/zizmor.yaml' ':(glob)**/.golangci.yml' 2>/dev/null \
    | grep -vxE '\.golangci\.yml|\.github/zizmor\.yml' || true)
  while IFS= read -r f; do
    [[ -n "$f" ]] && printf '%s\t%s\n' "$f" "deleted watched tool config"
  done < <(git diff --name-only --diff-filter=DR "${base}...${head}" -- .golangci.yml .github/zizmor.yml 2>/dev/null)

  # 16) .gitattributes (any directory): -diff, binary, diff= or a textconv driver hides a
  #     path's content from reviewers and from the secret scan's history diff.
  while IFS= read -r f; do
    [[ -n "$f" ]] || continue
    added="$(added_entries "$base" "$head" "$f" '(^|[[:space:]])([-!]diff|binary|(diff|merge|linguist-generated)(=[^[:space:]]*)?)([[:space:]]|$)')"
    while IFS= read -r line; do
      [[ -n "$line" ]] && printf '%s\t%s\n' "$f" "$line"
    done <<<"$added"
  done < <(git diff --name-only "${base}...${head}" -- ':(glob)**/.gitattributes' 2>/dev/null)

  # 13) The secret scan's inline allow marker: gitleaks skips any line carrying it, in any file
  #     type, so one added marker lands a real secret with a green scan.
  "${GIT_DIFF[@]}" -U0 "${base}...${head}" 2>/dev/null | awk '
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
