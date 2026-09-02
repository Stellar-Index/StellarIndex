#!/usr/bin/env bash
# lint-shell-sigpipe-test.sh — fixture tests for the pipe-into-head gate
# (scripts/ci/lint-shell-sigpipe.sh, #475).
#
# The gate exists because the failure it prevents is INVISIBLE in review and
# INTERMITTENT in production: `sort | head -n N` under pipefail only breaks
# once the producer outsizes the 64 KiB pipe buffer, so it passes every test
# on small input and then exits 2 on a third of real runs (r1's
# galexie-archive-fill, three failures, the last 2026-09-02 18:19:35 UTC).
# A gate that stops tripping would restore exactly that, so its behaviour is
# pinned here rather than assumed:
#
#   - a pipefail script piping into head is CAUGHT (the core case);
#   - the same line with a `# sigpipe-ok:` marker passes;
#   - the marker is honoured anywhere in the comment block above the line;
#   - a comment merely QUOTING the bad shape is not an instance of it;
#   - a script WITHOUT pipefail/-e is ignored (it cannot be killed by this);
#   - a clean tree passes;
#   - a root with no pipefail scripts FAILS rather than passing vacuously.
#
# Run: bash scripts/ci/lint-shell-sigpipe-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-shell-sigpipe.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
check() { # check <desc> <want-exit> <dir>
  local desc="$1" want="$2" dir="$3" got
  bash "$LINT" "$dir" >/dev/null 2>&1
  got=$?
  if [ "$got" -eq "$want" ]; then
    echo "  ok   $desc"
    pass=$((pass + 1))
  else
    echo "  FAIL $desc (exit $got, want $want)"
    fail=$((fail + 1))
  fi
}

mk() { # mk <dir> <file> <body>
  mkdir -p "$TMP/$1"
  printf '%s\n' "$3" > "$TMP/$1/$2"
}

echo "lint-shell-sigpipe-test: detection"

mk bad offender.sh 'set -euo pipefail
mc ls bucket/ | sort | head -n 4 > /tmp/out.txt'
check "pipe into head under pipefail is caught" 1 "$TMP/bad"

mk fixed fixed.sh 'set -euo pipefail
mc ls bucket/ | sort > /tmp/all.txt
head -n 4 /tmp/all.txt > /tmp/out.txt'
check "write-then-slice passes" 0 "$TMP/fixed"

echo "lint-shell-sigpipe-test: escape hatch"

mk okinline ok.sh 'set -euo pipefail
pgrep -f thing | head -1   # sigpipe-ok: pgrep output is a few bytes'
check "marker on the line itself passes" 0 "$TMP/okinline"

mk okblock ok.sh 'set -euo pipefail
# sigpipe-ok: the producer emits at most three short lines,
# which cannot fill a pipe buffer before head is done.
pgrep -f thing | head -1'
check "marker anywhere in the comment block above passes" 0 "$TMP/okblock"

mk quoted doc.sh 'set -euo pipefail
# Never write `mc ls bucket/ | sort | head -n 4` here — see #475.
mc ls bucket/ | sort > /tmp/all.txt'
check "a comment quoting the bad shape is not an instance" 0 "$TMP/quoted"

echo "lint-shell-sigpipe-test: scope"

mk nopipefail loose.sh '#!/bin/bash
mc ls bucket/ | sort | head -n 4 > /tmp/out.txt'
check "script without pipefail/-e is out of scope (and root is then vacuous)" 1 "$TMP/nopipefail"

mk mixed guarded.sh 'set -o pipefail
echo hi'
mk mixed loose.sh '#!/bin/bash
mc ls bucket/ | sort | head -n 4'
check "a pipefail script present makes the root non-vacuous; loose file ignored" 0 "$TMP/mixed"

mkdir -p "$TMP/empty"
check "a root with no pipefail scripts FAILS rather than passing vacuously" 1 "$TMP/empty"

echo "lint-shell-sigpipe-test: the real tree"
check "the repo's own scripts are clean" 0 "configs/ansible/roles/archival-node/files scripts/ops scripts/dev"

echo "lint-shell-sigpipe-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
