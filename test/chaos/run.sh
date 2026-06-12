#!/usr/bin/env bash
# Top-level chaos-suite runner. Iterates the scenarios under
# scenarios/ in lexical order and prints a final summary table.
#
# Each scenario is its own self-contained bash script; this runner is
# nothing more than a `for` + a refusal to run against production.
#
# Usage:
#   ./test/chaos/run.sh                  # run all scenarios
#   ./test/chaos/run.sh 01 03            # run a subset (matches by prefix)
#   CHAOS_TARGET=http://staging:3000 \
#     ./test/chaos/run.sh                # override target
#
# Exits non-zero if any scenario fails. CI consumers can rely on the
# exit code; humans can read the per-run report under reports/.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCENARIO_DIR="$SCRIPT_DIR/scenarios"

# Production-safety guard. Same shape as the lib/common.sh check, but
# we duplicate it here so the runner refuses to even iterate scenarios
# against a prod-shaped target.
case "${CHAOS_TARGET:-http://localhost:3000}" in
    *production*|*api.stellarindex.io*|*prod.*)
        echo "FATAL: CHAOS_TARGET=$CHAOS_TARGET looks like production. Refusing." >&2
        exit 2
        ;;
esac

# Ensure the report dir exists; common.sh will append per-scenario.
export CHAOS_REPORT_DIR="${CHAOS_REPORT_DIR:-$SCRIPT_DIR/reports}"
mkdir -p "$CHAOS_REPORT_DIR"

# Determine scenarios to run. Filters by prefix when args are given.
selected=()
if [ "$#" -gt 0 ]; then
    for arg in "$@"; do
        for f in "$SCENARIO_DIR/$arg"*.sh; do
            [ -f "$f" ] && selected+=("$f")
        done
    done
else
    for f in "$SCENARIO_DIR"/[0-9]*.sh; do
        [ -f "$f" ] && selected+=("$f")
    done
fi

if [ "${#selected[@]}" -eq 0 ]; then
    echo "no scenarios matched" >&2
    exit 2
fi

# Run each in sequence. A scenario failure does NOT halt the run —
# the loop records the failure and continues to the next scenario
# (the harness is more useful as "run all and tell me everything
# that's broken" than "stop at first fail"). Each scenario's own
# EXIT trap heals the state it perturbed, so the docker stack stays
# healthy between scenarios. The overall run exits non-zero iff any
# scenario failed (see the summary block below).
total=0
passed=0
failed_names=()
for scenario in "${selected[@]}"; do
    total="$((total + 1))"
    name="$(basename "$scenario" .sh)"
    echo "──── ${name} ────"
    if "$scenario"; then
        passed="$((passed + 1))"
    else
        rc="$?"
        echo "scenario $name failed (exit=$rc)"
        failed_names+=("$name")
        # Continue to next scenario rather than stopping; the harness
        # is more useful as a "run all and tell me everything that's
        # broken" tool than a "stop at first fail" tool.
    fi
    echo ""
done

echo "──── summary ────"
echo "passed: $passed / $total"
if [ "${#failed_names[@]}" -gt 0 ]; then
    echo "failed: ${failed_names[*]}"
    exit 1
fi
