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
#   - so is a pipe into `grep -q` / `grep -l`, bare or in a flag cluster —
#     the regex named `grep -m N` and not `grep -q` for its first two
#     versions, which left 44 sites scanning clean across the four roots;
#   - `|| grep -q x`, and an `&& grep -q x` later on the same line, are NOT
#     instances: neither reads from the pipe;
#   - the same line with a `# sigpipe-ok:` marker passes;
#   - the marker is honoured anywhere in the comment block above the line;
#   - a comment merely QUOTING the bad shape is not an instance of it;
#   - a script WITHOUT pipefail/-e is ignored (it cannot be killed by this);
#   - a clean tree passes;
#   - a root with no pipefail scripts FAILS rather than passing vacuously;
#   - a gate script under scripts/ci is caught (the root that was missing).
#
# A fixture has to CONTAIN the shape the gate hunts for, which makes this
# file its own offender now that scripts/ci is a default root. Those bodies
# are therefore written on ONE line — $'…\n…' — so the `# sigpipe-ok:` marker
# can sit on the source line OUTSIDE the quoted body: it silences the gate
# for this file without disarming the fixture the gate is tested against.
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

mk bad offender.sh $'set -euo pipefail\nmc ls bucket/ | sort | head -n 4 > /tmp/out.txt'   # sigpipe-ok: fixture text, scanned by the gate and never executed
check "pipe into head under pipefail is caught" 1 "$TMP/bad"

mk fixed fixed.sh 'set -euo pipefail
mc ls bucket/ | sort > /tmp/all.txt
head -n 4 /tmp/all.txt > /tmp/out.txt'
check "write-then-slice passes" 0 "$TMP/fixed"

mk awkexit offender.sh $'set -euo pipefail\nprintf "%s\\n" "$big" | awk "/x/{print; exit}" > /tmp/out.txt'   # sigpipe-ok: fixture text, scanned by the gate and never executed
check "pipe into an early-exit awk is caught (not just head)" 1 "$TMP/awkexit"

mk sedq offender.sh $'set -euo pipefail\nmc ls bucket/ | sed -n "1p;q" > /tmp/out.txt'   # sigpipe-ok: fixture text, scanned by the gate and never executed
check "pipe into sed with q is caught" 1 "$TMP/sedq"

mk grepm offender.sh $'set -euo pipefail\nmc ls bucket/ | grep -m 1 thing > /tmp/out.txt'   # sigpipe-ok: fixture text, scanned by the gate and never executed
check "pipe into grep -m is caught" 1 "$TMP/grepm"

mk col0pipe offender.sh $'set -euo pipefail\nmc ls bucket/ \\\n| head -n 4 > /tmp/out.txt'   # sigpipe-ok: fixture text, scanned by the gate and never executed
check "continuation line starting with a pipe into head is caught" 1 "$TMP/col0pipe"

mk orgrepq ok.sh $'set -euo pipefail\nprobe || grep -q x /etc/hosts'
check "|| followed by grep -q is not a pipe" 0 "$TMP/orgrepq"

mk grepq offender.sh $'set -euo pipefail\nmc ls bucket/ | grep -q thing'   # sigpipe-ok: fixture text, scanned by the gate and never executed
check "pipe into grep -q is caught (the spelling the tree writes)" 1 "$TMP/grepq"

mk grepqcluster offender.sh $'set -euo pipefail\nmc ls bucket/ | grep -Fxq -- "$want"'   # sigpipe-ok: fixture text, scanned by the gate and never executed
check "pipe into a grep flag CLUSTER ending in q is caught" 1 "$TMP/grepqcluster"

mk grepl offender.sh $'set -euo pipefail\nmc ls bucket/ | grep -l thing'   # sigpipe-ok: fixture text, scanned by the gate and never executed
check "pipe into grep -l is caught (it stops at the first match too)" 1 "$TMP/grepl"

mk grepqok ok.sh $'set -euo pipefail\nmc ls bucket/ | grep -q thing   # sigpipe-ok: the listing is four short lines'   # sigpipe-ok: fixture text, scanned by the gate and never executed
check "the same grep -q line with a marker passes" 0 "$TMP/grepqok"

mk grepqfixed ok.sh 'set -euo pipefail
listing="$(mc ls bucket/)"
grep -q thing <<<"$listing"'
check "the here-string rewrite of grep -q passes" 0 "$TMP/grepqfixed"

mk grepcount ok.sh 'set -euo pipefail
n=$(mc ls bucket/ | grep -c thing)'
check "a grep that reads to EOF (-c) passes" 0 "$TMP/grepcount"

echo "lint-shell-sigpipe-test: what is not this pipe's consumer"

# `||` is a shell OR, not a pipe: the grep after it reads a file and there is
# no upstream process to leave writing into a closed descriptor.
mk orlist ok.sh 'set -euo pipefail
[ -f /x ] || grep -q thing /etc/hosts'
check "grep -q after || is not a pipe" 0 "$TMP/orlist"

# A second, un-piped grep later on the SAME line does not belong to the
# earlier pipe — deploy-baseline-test.sh:176 is exactly this shape, and
# reading it as one pipeline would fail a line that is already correct.
mk laterand ok.sh 'set -euo pipefail
if [ "$(printf %s "$out" | grep -c v1)" -eq 2 ] && ! grep -q v2 <<<"$out"; then :; fi'
check "an && grep -q later on the line is not the pipe's consumer" 0 "$TMP/laterand"

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

mk nopipefail loose.sh $'#!/bin/bash\nmc ls bucket/ | sort | head -n 4 > /tmp/out.txt'   # sigpipe-ok: fixture text, scanned by the gate and never executed
check "script without pipefail/-e is out of scope (and root is then vacuous)" 1 "$TMP/nopipefail"

mk mixed guarded.sh 'set -o pipefail
echo hi'
mk mixed loose.sh $'#!/bin/bash\nmc ls bucket/ | sort | head -n 4'   # sigpipe-ok: fixture text, scanned by the gate and never executed
check "a pipefail script present makes the root non-vacuous; loose file ignored" 0 "$TMP/mixed"

mkdir -p "$TMP/empty"
check "a root with no pipefail scripts FAILS rather than passing vacuously" 1 "$TMP/empty"

echo "lint-shell-sigpipe-test: the gate's own directory"

# The blind spot: scripts/ci was not a default root, so the 73 pipefail
# scripts under it — every gate and every self-test — went unscanned for this
# gate's whole life, and a `printf … | head -1` in check-public-dataset-test.sh
# took down a full verify run before the roots were widened.
mk scripts/ci gate.sh $'set -euo pipefail\nbig=$(git rev-parse HEAD)\nx=$(printf \'%s\\n\' "$big" | head -1)'   # sigpipe-ok: fixture text, scanned by the gate and never executed
check "a gate script under a scripts/ci root is caught" 1 "$TMP/scripts/ci"

mk okci gate.sh $'set -euo pipefail\nbig=$(git rev-parse HEAD)\nx=$(printf \'%s\\n\' "$big" | head -1)   # sigpipe-ok: rev-parse emits one 40-byte line'   # sigpipe-ok: fixture text, scanned by the gate and never executed
check "the same gate line with a marker passes" 0 "$TMP/okci"

echo "lint-shell-sigpipe-test: the real tree"
check "the repo's own scripts are clean" 0 "configs/ansible/roles/archival-node/files scripts/ops scripts/dev scripts/ci"

# The DEFAULT roots are what CI runs; an explicit-root case cannot see them
# narrow again. Pin that a no-argument run covers exactly the four roots.
default_out="$(bash "$LINT" 2>&1)"
explicit_out="$(bash "$LINT" "configs/ansible/roles/archival-node/files scripts/ops scripts/dev scripts/ci" 2>&1)"
if [ "$default_out" = "$explicit_out" ]; then
  echo "  ok   the default roots are the four documented roots (scripts/ci included)"
  pass=$((pass + 1))
else
  echo "  FAIL the default roots drifted from the four documented roots"
  echo "       default:  $default_out"
  echo "       explicit: $explicit_out"
  fail=$((fail + 1))
fi

echo "lint-shell-sigpipe-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
