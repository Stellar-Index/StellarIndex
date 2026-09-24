#!/usr/bin/env bash
# verify-lane-errexit-test.sh — regression test for CA2-A38-correct-0.
#
# In lane_mode=1 (the default whenever the machine/VM has less than
# VERIFY_LANE_MEM_FLOOR_MB — every container `make prepush`, since the Docker
# VM there measured 7.65 GiB), lane_b and lane_c must be run as backgrounded
# jobs whose exit code is read with `wait`. Calling a function directly on
# the left of `||` (`lane_b ... || lane_rc_b=$?`) turns off `set -e` for the
# whole function body, so a failing `make test`/`make lint` inside lane_b was
# swallowed whenever a later step in the same function still exited 0 — and
# verify.sh went on to print ALL CHECKS PASSED.
#
# This extracts the real serial-mode dispatch block out of verify.sh (so the
# test fails if the actual code regresses back to the unfixed form) and runs
# it with a stand-in lane_b that fails on its first step and succeeds on its
# last — the exact shape that hid the bug.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
VERIFY="$PWD/scripts/dev/verify.sh"

start_line="$(awk '$0 == "lane_rc_a=0; lane_rc_b=0; lane_rc_c=0; lane_rc_d=0" { print NR; exit }' "$VERIFY")"
if [ -z "$start_line" ]; then
    echo "FAIL: could not locate the lane-dispatch block start in verify.sh (anchor moved)" >&2
    exit 1
fi
end_line="$(awk -v s="$start_line" 'NR > s && /^fi$/ { print NR; exit }' "$VERIFY")"
if [ -z "$end_line" ]; then
    echo "FAIL: could not locate the lane-dispatch block end in verify.sh (anchor moved)" >&2
    exit 1
fi

block="$(sed -n "${start_line},${end_line}p" "$VERIFY")"

result="$(
    set -euo pipefail
    LANEDIR="$(mktemp -d)"
    trap 'rm -rf "$LANEDIR"' EXIT
    # shellcheck disable=SC2329 # invoked indirectly, from the eval'd block below
    lane_a() { :; }
    # shellcheck disable=SC2329 # invoked indirectly, from the eval'd block below
    lane_d() { :; }
    # Fails on its first step, succeeds on its last — the shape that hid a
    # real failure under the buggy `lane_b ... || lane_rc_b=$?` form.
    # shellcheck disable=SC2329 # invoked indirectly, from the eval'd block below
    lane_b() { echo fail-step; false; echo later-step; true; }
    # shellcheck disable=SC2329 # invoked indirectly, from the eval'd block below
    lane_c() { :; }
    # shellcheck disable=SC2034 # read by the eval'd block below
    lane_mode=1
    eval "$block"
    # shellcheck disable=SC2154 # set by the eval'd block above
    echo "lane_rc_b=${lane_rc_b}"
)"
status=$?

echo "$result"

if [ "$status" -ne 0 ] && [[ "$result" != *$'\n'"lane_rc_b="* && "$result" != "lane_rc_b="* ]]; then
    echo "FAIL: lane-dispatch block errored before reporting lane_rc_b" >&2
    exit 1
fi

if [[ "$result" == *$'\n'"lane_rc_b=0" || "$result" == "lane_rc_b=0" ]]; then
    echo "FAIL: lane_b's failure was swallowed (lane_rc_b=0) — the serial-mode" >&2
    echo "      dispatch block regressed to calling lane_b directly on the" >&2
    echo "      left of || instead of backgrounding it and using wait" >&2
    exit 1
fi

echo "PASS: serial-mode lane_b failure is correctly reported (lane_rc_b != 0)"
