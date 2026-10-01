#!/usr/bin/env bash
# check-selftest-runners.sh — every scripts/ci/*-test.sh must be executed by
# a workflow under .github/workflows/ or by scripts/dev/verify.sh.
#
# A self-test that no runner executes guards nothing: it stays green while the
# gate it covers regresses. lint-changed.sh does not count as a runner — it is
# an opt-in hook and runs a *-test.sh only when that file is itself in the diff.
#
# Pre-existing orphans are listed in selftest-runners.baseline. The gate fails
# on an orphan not listed there AND on a listed entry that is now run or no
# longer exists, so the baseline can only shrink.
#
# Env overrides (used by the self-test): CI_DIR, WORKFLOWS_DIR, VERIFY_SH,
# BASELINE.
set -euo pipefail

cd "$(dirname "$0")/../.."

CI_DIR="${CI_DIR:-scripts/ci}"
WORKFLOWS_DIR="${WORKFLOWS_DIR:-.github/workflows}"
VERIFY_SH="${VERIFY_SH:-scripts/dev/verify.sh}"
BASELINE="${BASELINE:-scripts/ci/selftest-runners.baseline}"

for f in "$VERIFY_SH" "$BASELINE"; do
  if [ ! -f "$f" ]; then
    echo "check-selftest-runners: FAIL — file not found: $f" >&2
    exit 1
  fi
done

# Only executions count (`./scripts/ci/x-test.sh`, `bash scripts/ci/x-test.sh`),
# not bare mentions such as ci.yml's restore loop, which passes paths to git show.
invoked="$(cat "$WORKFLOWS_DIR"/*.yml "$VERIFY_SH" 2>/dev/null |
  grep -oE '(\./|bash +)scripts/ci/[A-Za-z0-9._-]+-test\.sh' |
  sed -E 's#^(\./|bash +)scripts/ci/##' | sort -u || true)"
if [ -z "$invoked" ]; then
  echo "check-selftest-runners: FAIL — no scripts/ci/*-test.sh invocations found in $WORKFLOWS_DIR or $VERIFY_SH." >&2
  echo "  (Did the extraction pattern drift? This check must not pass vacuously.)" >&2
  exit 1
fi

tests="$(find "$CI_DIR" -maxdepth 1 -name '*-test.sh' -exec basename {} \; | sort)"
if [ -z "$tests" ]; then
  echo "check-selftest-runners: FAIL — no *-test.sh found under $CI_DIR." >&2
  exit 1
fi

baseline="$(sed -E 's/#.*//; s/[[:space:]]+//g' "$BASELINE" | grep -v '^$' | sort -u || true)"

bad=0
while IFS= read -r t; do
  grep -qxF "$t" <<<"$invoked" && continue
  grep -qxF "$t" <<<"$baseline" && continue
  echo "check-selftest-runners: $CI_DIR/$t is run by no workflow and not by $VERIFY_SH" >&2
  bad=$((bad + 1))
done <<<"$tests"

while IFS= read -r b; do
  [ -n "$b" ] || continue
  if ! grep -qxF "$b" <<<"$tests"; then
    echo "check-selftest-runners: stale baseline entry $b — $CI_DIR/$b does not exist; remove it from $BASELINE" >&2
    bad=$((bad + 1))
  elif grep -qxF "$b" <<<"$invoked"; then
    echo "check-selftest-runners: stale baseline entry $b — it now has a runner; remove it from $BASELINE" >&2
    bad=$((bad + 1))
  fi
done <<<"$baseline"

if [ "$bad" -gt 0 ]; then
  cat >&2 <<EOF
check-selftest-runners: FAIL — $bad problem(s).
Wire each new self-test into .github/workflows/ci.yml (import-checks) AND
scripts/dev/verify.sh (check-verify-parity.sh requires both); never add a new
entry to $BASELINE.
EOF
  exit 1
fi

echo "check-selftest-runners: OK — $(grep -c . <<<"$tests") self-tests, $(grep -c . <<<"$baseline" || true) baselined orphan(s), the rest have a runner."
