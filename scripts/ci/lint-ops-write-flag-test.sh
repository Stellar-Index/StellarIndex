#!/usr/bin/env bash
# lint-ops-write-flag-test.sh — fixture tests for scripts/ci/lint-ops-write-flag.sh.
#
# Each case builds a throwaway root holding a stub subcommandClasses map and one
# docs/operations page, and asserts the gate's exit code. LINT overrides the gate
# under test (used to prove these cases catch a broken copy).
#
# Run: bash scripts/ci/lint-ops-write-flag-test.sh
# shellcheck disable=SC2016 # fixtures hold literal markdown backticks
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="${LINT:-$PWD/scripts/ci/lint-ops-write-flag.sh}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

CLASSES='package main

var subcommandClasses = map[string]subcommandClass{
	"docs-config":                 ro(),
	"backfill":                    gate(),
	"freeze-unfreeze":             gate(),
	"archive-completeness verify": gate(),
}
'

pass=0
fail=0
n=0
# run_case <desc> <want-exit> <markdown> [classes-file-content]
run_case() {
  local desc="$1" want="$2" md="$3" classes="${4-$CLASSES}" root got
  n=$((n + 1))
  root="$TMP/case$n"
  mkdir -p "$root/cmd/stellarindex-ops" "$root/docs/operations"
  printf '%s' "$classes" >"$root/cmd/stellarindex-ops/write_gate_class_test.go"
  printf '%s\n' "$md" >"$root/docs/operations/page.md"
  LINT_ROOT="$root" bash "$LINT" >"$root/out" 2>&1
  got=$?
  if [ "$got" -eq "$want" ]; then
    pass=$((pass + 1))
  else
    fail=$((fail + 1))
    echo "FAIL: $desc — want exit $want, got $got" >&2
    sed 's/^/    /' "$root/out" >&2
  fi
}

F='```'

if bash "$LINT" >"$TMP/real" 2>&1; then
  pass=$((pass + 1))
else
  fail=$((fail + 1))
  echo "FAIL: the repo's own docs/operations must pass" >&2
  sed 's/^/    /' "$TMP/real" >&2
fi

run_case "mutating command without -write fails" 1 \
  "$F"$'sh\nstellarindex-ops backfill -config /etc/x.toml -from 1 -to 2\n'"$F"
run_case "mutating command with -write passes" 0 \
  "$F"$'sh\nstellarindex-ops backfill -config /etc/x.toml -from 1 -to 2 -write\n'"$F"
run_case "a preview marked with -dry-run passes" 0 \
  "$F"$'sh\nstellarindex-ops backfill -config /etc/x.toml -from 1 -dry-run\n'"$F"
run_case "prose saying dry run is not the marker" 1 \
  "$F"$'sh\nstellarindex-ops backfill -config /etc/x.toml -from 1   # dry run\n'"$F"
run_case "-write=false is not -write" 1 \
  "$F"$'sh\nstellarindex-ops backfill -from 1 -write=false\n'"$F"
run_case "-write on a continuation line passes" 0 \
  "$F"$'sh\nstellarindex-ops backfill \\\n  -config /etc/x.toml \\\n  -from 1 -write\n'"$F"
run_case "continuation lines without -write fail" 1 \
  "$F"$'sh\nsudo run-heavy-job.sh fill \\\n  /usr/local/bin/stellarindex-ops backfill \\\n  -from 1 -to 2\n'"$F"
run_case "inline code without -write fails" 1 \
  'Catch up with `stellarindex-ops backfill -source sdex -from 1`.'
run_case "inline code with -write passes" 0 \
  'Catch up with `stellarindex-ops backfill -source sdex -from 1 -write`.'
run_case "namespace verb without -write fails" 1 \
  "$F"$'sh\nssh r1 \'stellarindex-ops archive-completeness verify -from 1 -to 2\'\n'"$F"
run_case "namespace verb with -write inside an ssh wrapper passes" 0 \
  "$F"$'sh\nssh r1 \'stellarindex-ops archive-completeness verify -from 1 -to 2 -write\'\n'"$F"
run_case "a | in a placeholder and a ; in a quoted value do not end the command" 0 \
  "$F"$'sh\nstellarindex-ops freeze-unfreeze -event <a|b> -reason "down; checked" -write\n'"$F"
run_case "a read-only subcommand passes" 0 \
  "$F"$'sh\nstellarindex-ops docs-config -out x.md\nstellarindex-ops backfill -write -from 1\n'"$F"
run_case "a read-only mode of a gated command passes" 0 \
  'List them: `stellarindex-ops freeze-unfreeze -config /etc/x.toml -list`.'
run_case "a bare mention passes" 0 \
  'Runbook for `stellarindex-ops backfill`; see `stellarindex-ops backfill -write -from 1`.'
run_case "a later pipe does not hide a missing -write" 1 \
  "$F"$'sh\nstellarindex-ops backfill -from 1 2>&1 | tee log\n'"$F"
run_case "an unparseable gate map fails closed" 2 \
  "$F"$'sh\nstellarindex-ops backfill -from 1 -write\n'"$F" \
  $'package main\n\nvar subcommandClasses = map[string]subcommandClass{\n\t"backfill": gated(),\n}\n'
run_case "an empty gate-map file fails closed" 2 \
  'nothing' ''
run_case "docs with no gated invocation fail rather than pass vacuously" 2 \
  'No commands here.'

echo "lint-ops-write-flag-test: $pass passed, $fail failed ($((n + 1)) cases)"
[ "$fail" -eq 0 ]
