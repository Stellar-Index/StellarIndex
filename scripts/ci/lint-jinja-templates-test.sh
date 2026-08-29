#!/usr/bin/env bash
# Self-test for lint-jinja-templates.sh: it must catch the r1 2026-08-29
# class (bash array-length idiom read as a Jinja comment) and every other
# unrenderable template, and must not flag templates ansible renders fine.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 2

pass=0; fail=0
ok()  { echo "  ok   $1"; pass=$((pass + 1)); }
bad() { echo "  FAIL $1"; fail=$((fail + 1)); }

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# A throwaway git repo so `git ls-files` sees only our fixtures.
run_on() { # run_on <dir> -> sets RC/OUT
  OUT=$(JINJA_LINT_ROOT="$1" "$REPO/scripts/ci/lint-jinja-templates.sh" 2>&1); RC=$?
}
REPO=$PWD

mk() { # mk <name> <content>
  d="$TMP/$1"; mkdir -p "$d"
  git -C "$d" init -q
  printf '%s' "$2" > "$d/t.j2"
  git -C "$d" add -A >/dev/null 2>&1
}

# 1. a template ansible renders fine
mk good '#!/bin/bash
STANZA="{{ pgbackrest_stanza }}"
{% if x %}echo hi{% endif %}
echo "${OTHER[@]}"
'
run_on "$TMP/good"
if [ "$RC" -eq 0 ]; then ok "clean template passes"; else bad "clean template rejected: $OUT"; fi
case "$OUT" in *"parsed 1 template"*) ok "counts the template";; *) bad "no count: $OUT";; esac

# 2. THE regression: bash array-length idiom (dollar brace hash)
mk arraylen '#!/bin/bash
REPOS=(1 2)
[ "${#REPOS[@]}" -gt 0 ] || REPOS=(1)
echo done
'
run_on "$TMP/arraylen"
if [ "$RC" -eq 1 ]; then ok "array-length idiom is caught"; else bad "array-length idiom passed (rc=$RC)"; fi
case "$OUT" in *"Missing end of comment tag"*) ok "names the jinja error";; *) bad "no jinja error: $OUT";; esac
case "$OUT" in *"t.j2:3"*) ok "reports the offending line";; *) bad "no line number: $OUT";; esac
case "$OUT" in *"raw block"*|*"count by hand"*) ok "prints the fix hint";; *) bad "no hint: $OUT";; esac

# 3. other unrenderable syntax still caught
mk unclosed '{% if x %}
never closed
'
run_on "$TMP/unclosed"
if [ "$RC" -eq 1 ]; then ok "unclosed block is caught"; else bad "unclosed block passed (rc=$RC)"; fi

mk badvar 'hello {{ name
'
run_on "$TMP/badvar"
if [ "$RC" -eq 1 ]; then ok "unclosed variable is caught"; else bad "unclosed variable passed (rc=$RC)"; fi

# 4. undefined vars/filters are NOT errors (parse-only contract)
mk undefined '{{ nope | some_ansible_only_filter }} {{ hostvars["x"]["y"] }}
'
run_on "$TMP/undefined"
if [ "$RC" -eq 0 ]; then ok "undefined vars/filters are not failures"; else bad "parse-only contract broken: $OUT"; fi

# 5. fail-closed when there are no templates at all (bad glob / wrong cwd)
mkdir -p "$TMP/empty" && git -C "$TMP/empty" init -q
run_on "$TMP/empty"
if [ "$RC" -eq 1 ]; then ok "no templates found fails closed"; else bad "empty tree passed (rc=$RC)"; fi

# 6. custom-delimiter templates are skipped, not failed
mk header '#jinja2: variable_start_string:"[%"
[ "${#ARR[@]}" -gt 0 ]
'
run_on "$TMP/header"
if [ "$RC" -eq 0 ]; then ok "#jinja2 header template is skipped"; else bad "custom-delimiter template failed: $OUT"; fi
case "$OUT" in *"skip (custom delimiters)"*) ok "says it skipped it";; *) bad "silent skip: $OUT";; esac

printf 'lint-jinja-templates-test: %d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
