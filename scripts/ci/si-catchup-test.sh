#!/usr/bin/env bash
# shellcheck disable=SC2016  # check() evals its single-quoted assertions late
# Self-test for configs/ansible/roles/archival-node/files/si-catchup.sh.
#
# The lane runner executes each queue line with the privileges it runs under
# (root on a host), so a queue or directory another user can write is a way to
# run commands as that user. Cases:
#   1. a private directory and queue run their step;
#   2. a group-writable directory is refused before anything runs;
#   3. a queue that is a symlink is refused;
#   4. a world-writable queue is refused;
#   5. a step deferred with 75 is retried without touching the lane log, so the
#      stall alert still sees how long the lane has made no progress.
# Ownership by a different user needs two uids and is not covered here.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/../../configs/ansible/roles/archival-node/files/si-catchup.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
pass=0; fail=0
ok() { echo "  ok   $1"; pass=$((pass + 1)); }
bad() { echo "  FAIL $1"; fail=$((fail + 1)); }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }

setup() {
  D="$WORK/$1"; rm -rf "$D"; mkdir -p "$D"; chmod 0755 "$D"
  printf 'step\ttouch %s/ran; touch %s/STOP\n' "$D" "$D" > "$D/a.queue"; chmod 0644 "$D/a.queue"
}
# shellcheck disable=SC2034  # rc is read inside check's eval strings
run() { CATCHUP_DIR="$D" bash "$SCRIPT" a >/dev/null 2>&1; rc=$?; }

echo "si-catchup-test: private directory"
setup private; run
check "the step runs and is recorded done" '[[ $rc == 0 && -e "$D/ran" ]] && grep -qx step "$D/a.done"'

echo "si-catchup-test: group-writable directory"
setup groupdir; chmod 0775 "$D"; run
check "refused, nothing ran" '[[ $rc == 1 && ! -e "$D/ran" ]]'

echo "si-catchup-test: queue is a symlink"
setup linkq; mv "$D/a.queue" "$WORK/elsewhere.queue"; ln -s "$WORK/elsewhere.queue" "$D/a.queue"; run
check "refused, nothing ran" '[[ $rc == 1 && ! -e "$D/ran" ]]'

echo "si-catchup-test: world-writable queue"
setup openq; chmod 0666 "$D/a.queue"; run
check "refused, nothing ran" '[[ $rc == 1 && ! -e "$D/ran" ]]'

echo "si-catchup-test: deferred step"
setup defer
printf 'step\tif [ -e %s/seen ]; then touch %s/STOP; exit 0; fi; touch %s/seen; exit 75\n' "$D" "$D" "$D" > "$D/a.queue"
mkdir "$WORK/bin"; printf '#!/bin/sh\nexit 0\n' > "$WORK/bin/sleep"; chmod +x "$WORK/bin/sleep"
PATH="$WORK/bin:$PATH" run
check "the step ran after the deferral" 'grep -qx step "$D/a.done"'
check "one START, no DEFER in the lane log" '[[ $(grep -c " START step" "$D/a.log") == 1 ]] && ! grep -q DEFER "$D/a.log"'
check "last lane log line is the START or DONE" '[[ $(tail -n 1 "$D/a.log") == *" DONE step "* ]]'

echo
echo "si-catchup-test: $pass passed, $fail failed"
[[ $fail == 0 ]]
