#!/usr/bin/env bash
# Prove each lint-adr-refs check can fail, against a fixture tree outside any git checkout.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE
LINT="$PWD/scripts/ci/lint_adr_refs.py"
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
PASS=0; FAIL=0

adr() { # adr <decision text> [extra line]
  printf -- '---\nadr: 0001\ntitle: t\nstatus: Accepted\n---\n\n# ADR-0001: t\n\n## Context\n\nWhy.\n\n## Decision\n\n%s\n\n## Invariant\n\nKeep it.\n\n## Consequences\n\nCost.\n\n## Evidence\n\nA test.\n%b' "$1" "${2:-}"
}
reset() {
  rm -rf "$T/r"; mkdir -p "$T/r/docs/adr" "$T/r/scripts/ci" "$T/r/internal"
  cp docs/adr/_template.md "$T/r/docs/adr/"
  printf '| [0001](0001-a.md) | Accepted |\n| 0002 | *Planned* |\n' > "$T/r/docs/adr/README.md"
  printf '## Money — invariant 1\n\n- **[2]** rule\n' > "$T/r/AGENTS.md"
  : > "$T/r/scripts/ci/lint-adr-refs.baseline"
  adr "We do X." > "$T/r/docs/adr/0001-a.md"
  python3 "$LINT" --root "$T/r" --record >/dev/null
}
check() { # check <name> <ok|red>
  local rc; python3 "$LINT" --root "$T/r" >"$T/out" 2>&1; rc=$?
  if { [ "$2" = ok ] && [ "$rc" -eq 0 ]; } || { [ "$2" = red ] && [ "$rc" -ne 0 ]; }; then
    printf '  ok   %s\n' "$1"; PASS=$((PASS+1))
  else
    printf '  FAIL %s (rc=%s, wanted %s)\n' "$1" "$rc" "$2"; sed 's/^/       /' "$T/out"; FAIL=$((FAIL+1))
  fi
}

reset; printf '// ADR-0001 ADR-0002 AGENTS.md invariant 1, AGENTS.md invariant 2, docs/adr/0001-a.md#decision\n' > "$T/r/internal/a.go"
check "conforming ADR, recorded hash and resolving citations pass" ok
adr "We do   X." > "$T/r/docs/adr/0001-a.md";               check "reflowed Decision passes" ok
adr "We do Y." > "$T/r/docs/adr/0001-a.md";                 check "changed Decision is rejected" red
python3 "$LINT" --root "$T/r" --record >/dev/null;          check "--record does not overwrite a recorded hash" red
reset; adr "We do X." '\n## Alternatives\n\nNone.\n' > "$T/r/docs/adr/0001-a.md"; check "extra section is rejected" red
reset; adr "We do X." '\n> **Amendment (2026-01-01):** changed.\n' > "$T/r/docs/adr/0001-a.md"; check "amendment blockquote is rejected" red
reset; adr "We do X." | sed 's/^Why\.$/a\nb\nc\nd/' > "$T/r/docs/adr/0001-a.md"; check "four-line Context is rejected" red
reset; { adr "We do X."; seq 1 150; } > "$T/r/docs/adr/0001-a.md"; check "ADR over 150 lines is rejected" red
reset; { adr "We do X."; seq 1 50; } > "$T/r/docs/adr/0001-a.md"; check "template ADRs over a 60-line median are rejected" red
reset; echo 0001-a.md > "$T/r/scripts/ci/lint-adr-refs.baseline"; check "baseline entry that conforms is rejected" red
reset; printf '## Context\n' > "$T/r/docs/adr/0003-b.md"; echo 0003-b.md > "$T/r/scripts/ci/lint-adr-refs.baseline"; check "non-conforming ADR in the baseline passes" ok
reset; rm "$T/r/docs/adr/decision-hashes.txt";              check "Accepted ADR without a recorded hash is rejected" red
reset; echo '-- ADR-0009' > "$T/r/internal/a.sql";          check "unknown ADR number is rejected" red
reset; echo '# AGENTS.md invariant 9' > "$T/r/internal/a.yml"; check "unknown AGENTS.md invariant is rejected" red
reset; echo 'see docs/adr/0001-a.md#nope' > "$T/r/internal/a.go"; check "missing anchor is rejected" red
reset; echo 'see docs/adr/gone.md#decision' > "$T/r/internal/a.go"; check "missing citation target is rejected" red

echo "lint-adr-refs self-test: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
