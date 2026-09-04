#!/usr/bin/env bash
# check-fleet-release-drift-test.sh — fixture tests for the test-net
# release-drift tripwire (scripts/ci/check-fleet-release-drift.sh).
#
# fleet-release-drift.yml only fires on a schedule against the live public
# API hosts, and deploy.yml's parity step only runs on a real deploy, so
# these fixtures are the only place the verdict is exercised on a PR. The
# drift that motivated the check (launch-plan 1.7: testnet + futurenet at
# v0.57.0 / migrations head 0150 while r1 served v0.58.0 / 0153, one day
# after a catch-up) is pinned RED here with the exact versions; the
# in-step fleet is pinned GREEN; the same-day release cascade that would
# have hidden the drift behind a per-release grace window is pinned RED on
# the real tag dates; and the ways the check could lie — a redirecting
# hostname or an upstream error page reading as "in step", an unreadable
# host reading as "no drift" — are pinned to the DEGRADED exit. So is the
# way a host could FORGE the verdict: a served "version" carrying
# newlines, the heredoc terminator and a `verdict=in-step` line must
# reach the report as one prefixed line in the version alphabet, never
# as lines of its own. A pre-release reference (r1 on v0.60.0-rc.1) is
# pinned fail-closed — DRIFT at once, no grace, the missing base tag
# named — and `-dirty` describe labels are pinned to their base. No
# network: the one case that exercises the curl call runs it against a
# PATH shim that never opens a socket, pinning --fail-with-body.
#
# The "how far behind" counts and the grace anchor come from throwaway git
# repos built here with the real tag sequence v0.57.0 → v0.58.0 → v0.59.0
# → v0.59.1 and each tag's migrations/ head as it is on origin (0150, 0153,
# 0154, 0154). One repo spaces the tags a day apart so the grace boundary
# is easy to read; the other carries the tags' REAL committer dates
# (2026-09-03: v0.58.0 01:26:58Z, v0.59.0 06:37:00Z, v0.59.1 07:30:52Z).
# Every real release tag is lightweight, so that repo's tags are too; the
# day-spaced repo mixes annotated and lightweight so both date paths run.
#
# Run: bash scripts/ci/check-fleet-release-drift-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
CHECK="$PWD/scripts/ci/check-fleet-release-drift.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

# ── Fixture git repos: tags + migrations heads as on origin ───────────
# new_repo <dir> — an empty repo with deterministic identity settings.
new_repo() {
  mkdir -p "$1/migrations"
  git -C "$1" init -q
  git -C "$1" config user.email fixture@example.invalid
  git -C "$1" config user.name fixture
  git -C "$1" config commit.gpgsign false
  git -C "$1" config tag.gpgsign false
}

# tag_at <repo> <tag> <migrations-head> <epoch> [light] — add migrations
# up to <head>, commit at <epoch>, and tag with that date; `light` makes a
# lightweight tag (no tagger date — the date is the commit's), which is
# what every real release tag is.
tag_at() {
  local repo="$1" tag="$2" head="$3" epoch="$4" kind="${5:-annotated}" n f
  for ((n = 1; n <= head; n++)); do
    f="$repo/migrations/$(printf '%04d' "$n")_fixture.up.sql"
    [ -e "$f" ] || echo "-- $n" > "$f"
  done
  git -C "$repo" add -A
  GIT_AUTHOR_DATE="@$epoch +0000" GIT_COMMITTER_DATE="@$epoch +0000" \
    git -C "$repo" commit -q -m "release $tag" --allow-empty
  if [ "$kind" = light ]; then
    git -C "$repo" tag "$tag"
  else
    GIT_COMMITTER_DATE="@$epoch +0000" git -C "$repo" tag -a "$tag" -m "$tag"
  fi
}

DAY=86400

# Day-spaced repo: v0.59.1 tagged at T0; NOW is set per case relative to it.
REPO="$TMP/repo"
new_repo "$REPO"
T0=1756880000
tag_at "$REPO" v0.57.0 150 $((T0 - 5 * DAY))
tag_at "$REPO" v0.58.0 153 $((T0 - DAY))
tag_at "$REPO" v0.59.0 154 $((T0 - 3600)) light
tag_at "$REPO" v0.59.1 154 "$T0"

# Real-date repo: the tags' committer dates on origin, all lightweight.
REPO_REAL="$TMP/repo-real"
new_repo "$REPO_REAL"
R570=1788265576   # 2026-08-31 12:26:16Z
R580=1788398818   # 2026-09-03 01:26:58Z
R590=1788417420   # 2026-09-03 06:37:00Z
R591=1788420652   # 2026-09-03 07:30:52Z
tag_at "$REPO_REAL" v0.57.0 150 "$R570" light
tag_at "$REPO_REAL" v0.58.0 153 "$R580" light
tag_at "$REPO_REAL" v0.59.0 154 "$R590" light
tag_at "$REPO_REAL" v0.59.1 154 "$R591" light
# The scheduled run after that cascade: 2026-09-04 07:25:00Z.
SCHEDULED_RUN=1788506700

# ── Fixture bodies: what /v1/version answers ──────────────────────────
body() { printf '{"data":{"build_date":"2026-09-03T07:42:16Z","commit":"106e1047","dirty":"false","go_version":"go1.25.13","version":"%s"},"as_of":"2026-09-04T08:28:22Z"}' "$1"; }

# fixture <dir> name=version ... — write <dir>/<name>.json per pair; a
# value of "-" writes nothing (the host is unreachable); "redirect"
# writes the body a 301-to-mainnet hostname actually returns; "gateway"
# writes the error page the KVM host's Caddy serves while a VM is down.
fixture() {
  local dir="$1"; shift
  rm -rf "$dir"; mkdir -p "$dir"
  local pair name ver
  for pair in "$@"; do
    name="${pair%%=*}"; ver="${pair#*=}"
    case "$ver" in
      -) ;;
      redirect) printf 'Redirecting to https://api.stellarindex.io/v1/version\n' > "$dir/$name.json" ;;
      gateway) printf '<html><body><h1>502 Bad Gateway</h1></body></html>\n' > "$dir/$name.json" ;;
      *) body "$ver" > "$dir/$name.json" ;;
    esac
  done
}

# run <fixture-dir> <now-epoch> [extra env assignments...] — a later
# assignment overrides an earlier one, so FLEET_GIT_DIR=… in the extras
# switches the tag repo.
run() {
  local dir="$1" now="$2"; shift 2
  OUT="$(env FLEET_FIXTURE_DIR="$dir" FLEET_NOW="$now" FLEET_GIT_DIR="$REPO" \
         GITHUB_OUTPUT="$TMP/out.txt" "$@" bash "$CHECK" 2>&1)"
  RC=$?
}

# Substrings are matched literally (-F): the expected text carries
# parentheses, dots and `$(` that a regex would read otherwise.
expect() {
  local name="$1" want_rc="$2" want_sub="${3:-}"
  if [ "$RC" -ne "$want_rc" ]; then
    echo "FAIL: $name — exit $RC, want $want_rc" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1)); return
  fi
  if [ -n "$want_sub" ] && ! grep -qF -- "$want_sub" <<<"$OUT"; then
    echo "FAIL: $name — output missing '$want_sub'" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1)); return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

# A negative assertion is only evidence when the script actually ran: a
# missing or crashed script would otherwise satisfy every expect_absent.
ran() {
  if [ "$RC" -eq 127 ] || ! grep -q 'fleet-release-drift:' <<<"$OUT"; then
    echo "FAIL: $1 — the check did not run (exit $RC)" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1)); return 1
  fi
}

expect_absent() {
  local name="$1" sub="$2"
  ran "$name" || return
  if grep -qF -- "$sub" <<<"$OUT"; then
    echo "FAIL: $name — output must not contain '$sub'" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1)); return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

# Every report line carries the script's prefix or is a catch-up
# command. A host-supplied newline that reached the report would put a
# line there without the prefix — the line a GITHUB_OUTPUT parser would
# read as a key or as the heredoc terminator, or the runner as a
# `::workflow-command::`.
expect_lines_prefixed() {
  local name="$1" stray
  ran "$name" || return
  stray="$(grep -vE '^(fleet-release-drift: |  gh workflow run )' <<<"$OUT" || true)"
  if [ -n "$stray" ]; then
    echo "FAIL: $name — report line(s) without the prefix:" >&2
    printf '%s\n' "$stray" | sed 's/^/    /' >&2
    fail=$((fail + 1)); return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

expect_output() {
  local name="$1" line="$2"
  if grep -qxF -- "$line" "$TMP/out.txt"; then
    echo "ok: $name"; pass=$((pass + 1))
  else
    echo "FAIL: $name — GITHUB_OUTPUT missing '$line'" >&2
    sed 's/^/    /' "$TMP/out.txt" >&2
    fail=$((fail + 1))
  fi
}

# expect_lines_bounded <name> <max-bytes> — neither the report nor
# GITHUB_OUTPUT carries a line longer than <max-bytes>. The sanitisers'
# 120-byte caps and the version pattern's own bounds are what make this
# true; a version the pattern did not bound sails past both.
expect_lines_bounded() {
  local name="$1" max="$2" worst
  ran "$name" || return
  worst="$( { printf '%s\n' "$OUT"; cat "$TMP/out.txt" 2>/dev/null || true; } \
    | LC_ALL=C awk '{ if (length($0) > m) m = length($0) } END { print m + 0 }')"
  if [ "$worst" -gt "$max" ]; then
    echo "FAIL: $name — longest line is $worst bytes, want at most $max" >&2
    fail=$((fail + 1)); return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

# Source-level assertions, for the two protections that live OUTSIDE
# this script's decision core: reverting either leaves every fixture
# below green, so each is asserted against the file that carries it.
expect_file() {
  local name="$1" file="$2" pattern="$3"
  if grep -qE -- "$pattern" "$file" 2>/dev/null; then
    echo "ok: $name"; pass=$((pass + 1))
  else
    echo "FAIL: $name — $file has no line matching /$pattern/" >&2
    fail=$((fail + 1))
  fi
}

expect_file_absent() {
  local name="$1" file="$2" pattern="$3" hit
  hit="$(grep -nE -- "$pattern" "$file" 2>/dev/null || true)"
  if [ -z "$hit" ]; then
    echo "ok: $name"; pass=$((pass + 1))
  else
    echo "FAIL: $name — $file matches /$pattern/:" >&2
    printf '%s\n' "$hit" | sed 's/^/    /' >&2
    fail=$((fail + 1))
  fi
}

expect_file_count() {
  local name="$1" file="$2" needle="$3" want="$4" got
  got="$(grep -cF -- "$needle" "$file" 2>/dev/null || true)"
  [ -n "$got" ] || got=0
  if [ "$got" = "$want" ]; then
    echo "ok: $name"; pass=$((pass + 1))
  else
    echo "FAIL: $name — $file has $got line(s) containing '$needle', want $want" >&2
    fail=$((fail + 1))
  fi
}

# GITHUB_OUTPUT holds nothing but key=value lines the script authored,
# with exactly one verdict.
expect_output_wellformed() {
  local name="$1" stray n
  stray="$(grep -vE '^[a-z0-9_]+=' "$TMP/out.txt" || true)"
  n="$(grep -c '^verdict=' "$TMP/out.txt" || true)"
  if [ -n "$stray" ] || [ "$n" != 1 ]; then
    echo "FAIL: $name — GITHUB_OUTPUT is not one key=value per line with one verdict (found $n):" >&2
    sed 's/^/    /' "$TMP/out.txt" >&2
    fail=$((fail + 1)); return
  fi
  echo "ok: $name"; pass=$((pass + 1))
}

# ── The launch-plan 1.7 drift, one day after the catch-up ────────────
# r1 at v0.59.1 for 2 days; both test nets still v0.57.0.
fixture "$TMP/drift" r1=v0.59.1 testnet=v0.57.0 futurenet=v0.57.0
rm -f "$TMP/out.txt"
run "$TMP/drift" $((T0 + 2 * DAY))
expect 'v0.57.0 test nets vs v0.59.1 r1, 48h old → DRIFT' 1 'DRIFT — 2 host(s) behind r1 v0.59.1'
expect 'names the release count' 1 'testnet   v0.57.0  BEHIND by 3 release(s)'
expect 'names the migration count and heads' 1 '4 migration(s) (head 0150, r1 0154)'
expect 'names the oldest release lacked and for how long' 1 'lacking v0.58.0 for 72h'
expect 'prints the catch-up dispatch for testnet' 1 'gh workflow run deploy.yml -f region=testnet -f version=v0.59.1 -f binaries=stellarindex-indexer,stellarindex-api,stellarindex-ops'
expect 'prints the catch-up dispatch for futurenet' 1 'gh workflow run deploy.yml -f region=futurenet -f version=v0.59.1'
expect_output 'GITHUB_OUTPUT verdict=drift' 'verdict=drift'
expect_output 'GITHUB_OUTPUT reference_version' 'reference_version=v0.59.1'
expect_output 'GITHUB_OUTPUT testnet_version' 'testnet_version=v0.57.0'
expect_output 'GITHUB_OUTPUT testnet_state=behind' 'testnet_state=behind'
expect_output 'GITHUB_OUTPUT testnet_lag' 'testnet_lag=lacking v0.58.0 for 72h'
expect_output 'GITHUB_OUTPUT behind_count=2' 'behind_count=2'

# One behind, one in step: the report must single out the laggard.
fixture "$TMP/one" r1=v0.59.1 testnet=v0.57.0 futurenet=v0.59.1
run "$TMP/one" $((T0 + 2 * DAY))
expect 'only testnet behind → DRIFT names one host' 1 'DRIFT — 1 host(s) behind'
expect 'futurenet reads in step' 1 'futurenet v0.59.1  in step'
expect_absent 'no futurenet dispatch when it is in step' 'region=futurenet'

# ── In step → quiet ───────────────────────────────────────────────────
fixture "$TMP/equal" r1=v0.59.1 testnet=v0.59.1 futurenet=v0.59.1
rm -f "$TMP/out.txt"
run "$TMP/equal" $((T0 + 2 * DAY))
expect 'whole fleet at v0.59.1 → OK' 0 'OK — testnet, futurenet in step with r1 v0.59.1'
expect_absent 'no dispatch commands when in step' 'gh workflow run'
expect_output 'GITHUB_OUTPUT verdict=in-step' 'verdict=in-step'
expect_output 'GITHUB_OUTPUT behind_count=0' 'behind_count=0'

# A follower AHEAD of the reference (futurenet on a newer build by design)
# is not drift.
fixture "$TMP/ahead" r1=v0.58.0 testnet=v0.58.0 futurenet=v0.59.1
rm -f "$TMP/out.txt"
run "$TMP/ahead" $((T0 + 2 * DAY))
expect 'follower ahead of r1 → OK, reported as ahead' 0 'futurenet v0.59.1  ahead of r1'
expect 'the verdict names the follower ahead as ahead' 0 'OK — testnet in step with r1 v0.58.0; futurenet v0.59.1 ahead of the reference.'
expect_absent 'a follower ahead is not worded as in step' 'testnet, futurenet in step'
expect_output 'GITHUB_OUTPUT futurenet_state=ahead' 'futurenet_state=ahead'
expect_output 'GITHUB_OUTPUT verdict=in-step with a follower ahead (nothing behind)' 'verdict=in-step'

# ── Grace: the only release the test nets lack is 6h old — they have not
#    had their turn yet, so this is a deploy in progress, not drift ────
fixture "$TMP/fresh" r1=v0.59.1 testnet=v0.59.0 futurenet=v0.59.0
rm -f "$TMP/out.txt"
run "$TMP/fresh" $((T0 + 6 * 3600))
expect 'lacking only a 6h-old release → within grace, exit 0' 0 'within the 24h grace'
expect 'grace names the release and its age' 0 'lacking v0.59.1 for 6h'
expect_output 'GITHUB_OUTPUT verdict=grace' 'verdict=grace'
expect_output 'GITHUB_OUTPUT testnet_state=behind-in-grace' 'testnet_state=behind-in-grace'
expect_absent 'no dispatch commands inside the grace' 'gh workflow run'
# Exactly at the boundary the grace has expired.
run "$TMP/fresh" $((T0 + 24 * 3600))
expect 'behind at exactly 24h → DRIFT' 1 'DRIFT'
# Grace 0 (deploy-time parity): behind is behind, however fresh.
run "$TMP/fresh" $((T0 + 60)) FLEET_GRACE_HOURS=0
expect 'grace 0 → behind reported immediately' 1 'DRIFT'

# ── The same-day release cascade: the grace is anchored on the OLDEST
#    release a follower lacks, never on r1's newest tag ───────────────
# v0.58.0 was tagged 30h ago and v0.59.1 6h ago; the test nets are still
# on v0.57.0. Anchoring on the newest tag would read "released 6h ago,
# within grace" and re-open the window on every r1 release; the follower
# has lacked v0.58.0 for 30h and that is drift.
fixture "$TMP/cascade" r1=v0.59.1 testnet=v0.57.0 futurenet=v0.57.0
rm -f "$TMP/out.txt"
run "$TMP/cascade" $((T0 + 6 * 3600))
expect 'v0.58.0 lacked for 30h behind a 6h-old v0.59.1 → DRIFT' 1 'DRIFT — 2 host(s) behind r1 v0.59.1'
expect 'the anchor is the oldest release lacked, not the newest' 1 'lacking v0.58.0 for 30h'
expect_absent 'a fresh newest tag grants no grace' 'within the 24h grace'
expect_output 'GITHUB_OUTPUT verdict=drift after the cascade' 'verdict=drift'

# The same cascade on the REAL tag dates, at the instant the scheduled
# run fires the next morning (07:25Z): v0.59.1 is 23h old, v0.58.0 29h.
fixture "$TMP/real" r1=v0.59.1 testnet=v0.57.0 futurenet=v0.57.0
run "$TMP/real" "$SCHEDULED_RUN" FLEET_GIT_DIR="$REPO_REAL"
expect '2026-09-04 07:25Z on the real tag dates → DRIFT' 1 'DRIFT — 2 host(s) behind r1 v0.59.1'
expect 'real dates: lacked v0.58.0 for 29h' 1 'lacking v0.58.0 for 29h'
expect 'lightweight tag dated from its commit' 1 'v0.59.1 (served; migrations head 0154; tagged 23h ago)'
# In step on the real dates → OK (the lightweight-tag path also on green).
fixture "$TMP/real-ok" r1=v0.59.1 testnet=v0.59.1 futurenet=v0.59.1
run "$TMP/real-ok" "$SCHEDULED_RUN" FLEET_GIT_DIR="$REPO_REAL"
expect 'real dates, fleet in step → OK' 0 'OK — testnet, futurenet in step'

# ── Issue churn: once past the grace, a fresh r1 release must not turn
#    the verdict quiet (the workflow would close the open issue) ───────
# Day 1: r1 on v0.58.0 for 25h, test nets on v0.57.0 → drift, issue opened.
fixture "$TMP/churn1" r1=v0.58.0 testnet=v0.57.0 futurenet=v0.57.0
run "$TMP/churn1" $((T0 + 3600))
expect 'day 1: v0.58.0 lacked for 25h → DRIFT' 1 'DRIFT'
# Two hours on: r1 released v0.59.0 3h ago; test nets unchanged.
fixture "$TMP/churn2" r1=v0.59.0 testnet=v0.57.0 futurenet=v0.57.0
rm -f "$TMP/out.txt"
run "$TMP/churn2" $((T0 + 2 * 3600))
expect 'a 3h-old r1 release does not reset the lag → still DRIFT' 1 'DRIFT — 2 host(s) behind r1 v0.59.0'
expect 'lag still counted from v0.58.0' 1 'lacking v0.58.0 for 26h'
expect_output 'GITHUB_OUTPUT verdict stays drift' 'verdict=drift'
# A partial catch-up (both now on v0.58.0) lacks only the 3h-old
# release → grace, and the verdict is NOT in-step, so the workflow keeps
# the issue open rather than closing it on "in step again".
fixture "$TMP/churn3" r1=v0.59.0 testnet=v0.58.0 futurenet=v0.58.0
rm -f "$TMP/out.txt"
run "$TMP/churn3" $((T0 + 2 * 3600))
expect 'partial catch-up: only a 3h-old release lacked → grace' 0 'lacking v0.59.0 for 3h'
expect_output 'GITHUB_OUTPUT verdict=grace, not in-step' 'verdict=grace'
# Mixed: one follower past the grace, one inside it → DRIFT, and the
# catch-up list names both (the in-grace one is behind too).
fixture "$TMP/mixed" r1=v0.59.0 testnet=v0.57.0 futurenet=v0.58.0
rm -f "$TMP/out.txt"
run "$TMP/mixed" $((T0 + 2 * 3600))
expect 'one past grace + one inside → DRIFT' 1 'DRIFT — 1 host(s) behind r1 v0.59.0'
expect 'the in-grace follower is reported as such' 1 'futurenet v0.58.0  BEHIND by 1 release(s)'
expect 'catch-up names the follower past the grace' 1 'region=testnet -f version=v0.59.0'
expect_output 'GITHUB_OUTPUT futurenet_state=behind-in-grace' 'futurenet_state=behind-in-grace'
expect_output 'GITHUB_OUTPUT testnet_state=behind' 'testnet_state=behind'

# Reference version supplied by the caller (deploy.yml passes the tag it
# just deployed) — no r1 fixture is needed, and the lag comes from the tags.
fixture "$TMP/noref" testnet=v0.57.0 futurenet=v0.57.0
run "$TMP/noref" $((T0 + 60)) FLEET_GRACE_HOURS=0 FLEET_REFERENCE_VERSION=v0.59.1
expect 'FLEET_REFERENCE_VERSION replaces the r1 read' 1 'reference r1 v0.59.1 (supplied;'

# Release age unknown (the reference tag is not in the checkout): the
# releases a follower lacks cannot be dated, so the grace CANNOT be
# applied; the check must say so and count the lag rather than assume
# fresh.
fixture "$TMP/notag" r1=v0.99.0 testnet=v0.59.1 futurenet=v0.59.1
run "$TMP/notag" $((T0 + 60))
expect 'reference tag unknown to git → lag counted, grace not applied' 1 'release age unknown'
expect 'unknown anchor is said per follower' 1 'grace not applied'

# ── Degraded: the check must never read "unreadable" as "in step" ────
fixture "$TMP/down" r1=v0.59.1 testnet=v0.59.1 futurenet=-
rm -f "$TMP/out.txt"
run "$TMP/down" $((T0 + 2 * DAY))
expect 'futurenet unreachable → DEGRADED exit 2' 2 'DEGRADED — could not read'
expect 'unreachable host named' 2 'futurenet unreadable'
expect_output 'GITHUB_OUTPUT verdict=degraded' 'verdict=degraded'
expect_output 'GITHUB_OUTPUT futurenet_state=unreadable' 'futurenet_state=unreadable'

# The bare explorer hostnames (testnet.stellarindex.io) 301 to mainnet's
# API. Following that redirect would report r1's own version as the test
# net's and read as "in step" forever; the body of an unfollowed redirect
# is not JSON and must land in DEGRADED.
fixture "$TMP/redir" r1=v0.59.1 testnet=redirect futurenet=v0.59.1
run "$TMP/redir" $((T0 + 2 * DAY))
expect 'redirect body → DEGRADED, not in step' 2 'testnet unreadable'
expect_absent 'redirect never reads as in step' 'testnet   v0.59.1  in step'

# The KVM host's Caddy answers 502 with an HTML page while a VM is down
# (a test-net reset, for one): an error page is unreadable, not in step.
fixture "$TMP/gateway" r1=v0.59.1 testnet=gateway futurenet=v0.59.1
run "$TMP/gateway" $((T0 + 2 * DAY))
expect 'upstream error page → DEGRADED' 2 'testnet unreadable'
expect_absent 'error page never reads as in step' 'testnet   v0.59.1  in step'

# A JSON body with no version field is equally unreadable.
mkdir -p "$TMP/novers"; body v0.59.1 > "$TMP/novers/r1.json"; body v0.59.1 > "$TMP/novers/futurenet.json"
printf '{"data":{}}' > "$TMP/novers/testnet.json"
run "$TMP/novers" $((T0 + 2 * DAY))
expect 'version field missing → DEGRADED' 2 'testnet unreadable'

# Reference unreadable: nothing can be compared; DEGRADED, and no host may
# be reported in step against a reference that was never read.
fixture "$TMP/refdown" r1=- testnet=v0.59.1 futurenet=v0.59.1
run "$TMP/refdown" $((T0 + 2 * DAY))
expect 'reference unreadable → DEGRADED' 2 'reference r1 unreadable'
expect_absent 'no in-step claim without a reference' 'v0.59.1  in step'

# Degraded outranks drift when both are present: the number of hosts
# behind is understated while one is unreadable.
fixture "$TMP/both" r1=v0.59.1 testnet=v0.57.0 futurenet=-
run "$TMP/both" $((T0 + 2 * DAY))
expect 'one behind + one unreadable → exit 2, drift still listed' 2 'testnet   v0.57.0  BEHIND'

# ── Version strings the fleet actually serves ─────────────────────────
# A bootstrap build labelled by `git describe` (v0.59.1-3-gabc1234) sits
# ON the tag's base and is compared as such, with the suffix kept visible.
fixture "$TMP/describe" r1=v0.59.1 testnet=v0.59.1-3-gabc1234 futurenet=v0.59.1
run "$TMP/describe" $((T0 + 2 * DAY))
expect 'git-describe build on the same base → in step' 0 'testnet   v0.59.1-3-gabc1234  in step'
# Anything that is not a version is unreadable, not "behind".
mkdir -p "$TMP/garbage"; body v0.59.1 > "$TMP/garbage/r1.json"; body v0.59.1 > "$TMP/garbage/futurenet.json"
body 'dev' > "$TMP/garbage/testnet.json"
run "$TMP/garbage" $((T0 + 2 * DAY))
expect 'version "dev" → DEGRADED' 2 "testnet unreadable — served version 'dev' (3 bytes) is not a release tag"

# `-dirty` is a describe label on the base, not a pre-release: a local
# build of the tag with uncommitted changes is in step with the tag, and
# a dirty reference does not make clean followers "ahead".
fixture "$TMP/dirty" r1=v0.59.1 testnet=v0.59.1-dirty futurenet=v0.59.1-3-gabc1234-dirty
run "$TMP/dirty" $((T0 + 2 * DAY))
expect 'v0.59.1-dirty sits on v0.59.1 → in step' 0 'testnet   v0.59.1-dirty  in step'
expect 'v0.59.1-3-gabc1234-dirty sits on v0.59.1 → in step' 0 'futurenet v0.59.1-3-gabc1234-dirty  in step'
fixture "$TMP/dirty-ref" r1=v0.59.1-dirty testnet=v0.59.1 futurenet=v0.59.1
run "$TMP/dirty-ref" $((T0 + 2 * DAY))
expect 'a dirty reference keeps clean followers in step' 0 'OK — testnet, futurenet in step with r1 v0.59.1-dirty.'
expect_absent 'no follower reads ahead of a dirty reference' 'ahead'

# ── A pre-release reference is fail-closed ────────────────────────────
# r1 serving v0.60.0-rc.1: no released tag dates what the followers lack
# (v0.60.0 is not cut), so no grace can be measured. Every follower
# behind is DRIFT at once, the message names the missing base tag, and
# the counts print `?` rather than a 0 that would read as "nothing
# lacked".
fixture "$TMP/rc" r1=v0.60.0-rc.1 testnet=v0.59.1 futurenet=v0.59.1
rm -f "$TMP/out.txt"
run "$TMP/rc" $((T0 + 60))
expect 'r1 on a pre-release, followers on the newest release → DRIFT, no grace' 1 'DRIFT — 2 host(s) behind r1 v0.60.0-rc.1'
expect 'the reference line says pre-release' 1 'reference r1 v0.60.0-rc.1 (served; migrations head ?; pre-release of v0.60.0, which is not a released tag; grace not applied)'
expect 'the follower line names the missing base tag' 1 'lacking v0.60.0 for ?h — grace not applied (r1 serves a pre-release of it, not a released tag)'
expect 'the counts are unknown, not 0' 1 'testnet   v0.59.1  BEHIND by ? release(s), ? migration(s) (head 0154, r1 ?)'
expect_absent 'a pre-release reference grants no grace' 'within the 24h grace'
expect_output 'GITHUB_OUTPUT verdict=drift under a pre-release reference' 'verdict=drift'
expect_output 'GITHUB_OUTPUT testnet_state=behind under a pre-release reference' 'testnet_state=behind'
# The same through FLEET_REFERENCE_VERSION (deploy.yml deploying an rc).
run "$TMP/rc" $((T0 + 60)) FLEET_REFERENCE_VERSION=v0.60.0-rc.1
expect 'a supplied pre-release reference → DRIFT, no grace' 1 'lacking v0.60.0 for ?h — grace not applied'
# A pre-release FOLLOWER: the next rc on futurenet is ahead of r1's
# release; an rc of r1's own release is behind it, with the grace
# measured from the release tag.
fixture "$TMP/rc-ahead" r1=v0.59.1 testnet=v0.59.1 futurenet=v0.60.0-rc.1
run "$TMP/rc-ahead" $((T0 + 2 * DAY))
expect 'follower on the next rc → ahead' 0 'futurenet v0.60.0-rc.1  ahead of r1'
fixture "$TMP/rc-behind" r1=v0.59.1 testnet=v0.59.1-rc.1 futurenet=v0.59.1
run "$TMP/rc-behind" $((T0 + 6 * 3600))
expect 'follower on an rc of the reference release → behind it, grace from the release tag' 0 'testnet   v0.59.1-rc.1  BEHIND by 1 release(s), 0 migration(s) (head 0154, r1 0154); lacking v0.59.1 for 6h'

# ── Host-supplied bytes cannot forge the verdict ──────────────────────
# fleet-release-drift.yml writes the report to GITHUB_OUTPUT as a
# multi-line value, and the runner's file-command parser keeps the LAST
# key=value it reads. A host whose "version" carries newlines, the
# heredoc terminator and a verdict_rc=0 / verdict=in-step line would —
# echoed raw — end the value early and hand the workflow an in-step
# verdict on a DEGRADED run: the close step would run and the failure
# would be silenced. The served bytes must reach the report as ONE
# prefixed line rendered in the version alphabet, no `verdict=` text
# anywhere, and the run must be DEGRADED.
forged="$(printf 'v0.59.1\nREPORT_EOF\nverdict_rc=0\nverdict=in-step\n`touch pwned`$(id)\r\n::error::forged')"
mkdir -p "$TMP/forged"; body v0.59.1 > "$TMP/forged/r1.json"; body v0.59.1 > "$TMP/forged/futurenet.json"
jq -cn --arg v "$forged" '{data:{version:$v}}' > "$TMP/forged/testnet.json"
rm -f "$TMP/out.txt"
run "$TMP/forged" $((T0 + 2 * DAY))
expect 'forged multi-line version → DEGRADED' 2 'testnet unreadable'
expect 'the served bytes are rendered in the version alphabet, with their size' 2 \
  "testnet unreadable — served version 'v0.59.1?REPORT?EOF?verdict?rc?0?verdict?in-step??touch?pwned???id?????error??forged' (83 bytes) is not a release tag"
expect_lines_prefixed 'forged version: every report line carries the prefix'
expect_absent 'forged version: no verdict= text anywhere in the report' 'verdict='
expect_absent 'forged version: no heredoc terminator' 'REPORT_EOF'
expect_absent 'forged version: no backtick' '`'
expect_absent 'forged version: no command substitution' '$('
expect_absent 'forged version: no workflow command' '::error::'
expect_output_wellformed 'forged version: GITHUB_OUTPUT is one key=value per line, one verdict'
expect_output 'forged version: GITHUB_OUTPUT verdict=degraded' 'verdict=degraded'
expect_output 'forged version: GITHUB_OUTPUT testnet_state=unreadable' 'testnet_state=unreadable'

# The same lines in a non-JSON body (an error page under the host's
# control) take the free-text path: flattened to one prefixed line,
# backticks dropped, so neither a key nor a fence can be planted.
mkdir -p "$TMP/forged-body"; body v0.59.1 > "$TMP/forged-body/r1.json"; body v0.59.1 > "$TMP/forged-body/futurenet.json"
printf '<html>\nREPORT_EOF\nverdict_rc=0\nverdict=in-step\n```\n::error::forged\n</html>\n' > "$TMP/forged-body/testnet.json"
rm -f "$TMP/out.txt"
run "$TMP/forged-body" $((T0 + 2 * DAY))
expect 'forged error page → DEGRADED' 2 'testnet unreadable — no .data.version in the response (<html> REPORT_EOF verdict_rc=0 verdict=in-step ::error::forged </html>)'
expect_lines_prefixed 'forged error page: every report line carries the prefix'
expect_absent 'forged error page: no backtick' '`'
expect_output_wellformed 'forged error page: GITHUB_OUTPUT is one key=value per line, one verdict'
expect_output 'forged error page: GITHUB_OUTPUT verdict=degraded' 'verdict=degraded'

# ── --fail-with-body is load-bearing ──────────────────────────────────
# Real curl exits 22 on an HTTP error status only with --fail /
# --fail-with-body; without the flag a 503 whose body happens to be a
# valid version document (a proxy serving a cached body over an error
# status) would be read as that version. A PATH shim stands in for curl:
# the reference answers 200; every other host answers the same body over
# a 503 — exit 22 with the body kept when the flag is present, exit 0
# when it is not. The shim never opens a socket.
mkdir -p "$TMP/bin"
cat > "$TMP/bin/curl" <<'SHIM'
#!/usr/bin/env bash
fail=0; url=""
for a in "$@"; do
  case "$a" in
    -f|--fail|--fail-with-body) fail=1 ;;
    http://*|https://*) url="$a" ;;
  esac
done
printf '{"data":{"version":"v0.59.1"}}\n'
case "$url" in
  https://api.stellarindex.io/*) exit 0 ;;
esac
if [ "$fail" = 1 ]; then
  echo "curl: (22) The requested URL returned error: 503" >&2
  exit 22
fi
exit 0
SHIM
chmod +x "$TMP/bin/curl"
rm -f "$TMP/out.txt"
run "$TMP/unused" $((T0 + 2 * DAY)) FLEET_FIXTURE_DIR= PATH="$TMP/bin:$PATH"
expect 'an error status over a version body is unreadable, not a version' 2 \
  'testnet unreadable — GET https://api.testnet.stellarindex.io/v1/version failed: {"data":{"version":"v0.59.1"}} curl: (22) The requested URL returned error: 503'
expect 'the reference (200) is read through the same curl' 2 'reference r1 v0.59.1 (served;'
expect_absent 'an error status never reads as in step' 'v0.59.1  in step'
expect_output 'GITHUB_OUTPUT verdict=degraded under an error status' 'verdict=degraded'

# ── A pre-release reference orders nothing but itself ─────────────────
# Every pre-release of a base packs to ONE version key, so r1 on
# v0.60.0-rc.2 beside a test net on v0.60.0-rc.1 compared EQUAL: "testnet
# v0.60.0-rc.1 in step", verdict in-step, exit 0 — and the workflow's
# close step then closed the tracking issue while a test net was a
# candidate behind. That was the one exit-0-while-behind path, and it
# contradicted the header, the deployment doc and the changelog, which
# all say a pre-release reference makes every behind follower drift at
# once. No ordering over pre-release suffixes is attempted: under a
# pre-release reference only an IDENTICAL string is in step.
fixture "$TMP/rc-pair" r1=v0.60.0-rc.2 testnet=v0.60.0-rc.1 futurenet=v0.60.0-rc.2
rm -f "$TMP/out.txt"
run "$TMP/rc-pair" $((T0 + 60))
expect 'an earlier rc of the reference rc is behind it, not in step' 1 'DRIFT — 1 host(s) behind r1 v0.60.0-rc.2'
expect 'the identical rc is in step' 1 'futurenet v0.60.0-rc.2  in step'
expect_absent 'a different rc of the same base never reads as in step' 'testnet   v0.60.0-rc.1  in step'
expect 'the earlier rc gets no grace' 1 'lacking v0.60.0 for ?h — grace not applied'
expect 'the catch-up names the test net on the earlier rc' 1 'region=testnet -f version=v0.60.0-rc.2'
expect_output 'GITHUB_OUTPUT testnet_state=behind on an earlier rc' 'testnet_state=behind'
expect_output 'GITHUB_OUTPUT futurenet_state=in-step on the identical rc' 'futurenet_state=in-step'
expect_output 'GITHUB_OUTPUT verdict=drift, so the issue stays open' 'verdict=drift'
# A fleet matching r1 byte for byte IS in step, candidate or not.
fixture "$TMP/rc-same" r1=v0.60.0-rc.2 testnet=v0.60.0-rc.2 futurenet=v0.60.0-rc.2
rm -f "$TMP/out.txt"
run "$TMP/rc-same" $((T0 + 60))
expect 'whole fleet on the same pre-release → OK' 0 'OK — testnet, futurenet in step with r1 v0.60.0-rc.2.'
expect_output 'GITHUB_OUTPUT verdict=in-step on an identical fleet' 'verdict=in-step'
# And a follower on the last RELEASE under that reference is drift at
# once, as the docs already said.
fixture "$TMP/rc-release" r1=v0.60.0-rc.2 testnet=v0.59.1 futurenet=v0.59.1
run "$TMP/rc-release" $((T0 + 60))
expect 'the last release under an rc reference → DRIFT, no grace' 1 'DRIFT — 2 host(s) behind r1 v0.60.0-rc.2'
expect 'that follower is behind the missing base tag' 1 'lacking v0.60.0 for ?h — grace not applied'
# A follower that already carries the reference's base — the cut release,
# or a newer one — is behind too under a pre-release reference (only the
# identical string is in step), and its message says so rather than
# claiming it lacks a base it has.
fixture "$TMP/rc-past" r1=v0.60.0-rc.2 testnet=v0.60.0 futurenet=v0.61.0
rm -f "$TMP/out.txt"
run "$TMP/rc-past" $((T0 + 60))
expect 'a follower past the reference candidate is not in step either' 1 'DRIFT — 2 host(s) behind r1 v0.60.0-rc.2'
expect 'the cut release is reported as not on the reference version' 1 'testnet   v0.60.0  BEHIND'
expect 'and is never said to lack a base it carries' 1 'not on v0.60.0-rc.2 — grace not applied (r1 serves a pre-release; only the identical version is in step)'
expect_absent 'a follower carrying the base is not said to lack it' 'v0.60.0  BEHIND by ? release(s), ? migration(s) (head ?, r1 ?); lacking'
expect_output 'GITHUB_OUTPUT testnet_state=behind past the candidate' 'testnet_state=behind'

# ── A leading-zero component must not kill the run ────────────────────
# v0.08.0 passes the version pattern, and its components reached bash
# arithmetic raw: `08` is not a valid octal literal, so version_key died
# with "value too great for base", `set -e` killed the run inside the
# follower loop, and the caller got exit 1 with an EMPTY GITHUB_OUTPUT
# and a raw bash error where the verdict should be — on a workflow whose
# non-zero exit opens an issue whose body is that report.
fixture "$TMP/leadzero" r1=v0.59.1 testnet=v0.08.0 futurenet=v0.59.1
rm -f "$TMP/out.txt"
run "$TMP/leadzero" $((T0 + 2 * DAY))
expect 'a leading-zero component compares as a number, not an octal literal' 1 'testnet   v0.08.0  BEHIND by 4 release(s)'
expect_absent 'no bash arithmetic error reaches the report' 'value too great for base'
expect_lines_prefixed 'leading zero: every report line carries the prefix'
expect_output_wellformed 'leading zero: GITHUB_OUTPUT is one key=value per line, one verdict'
expect_output 'leading zero: GITHUB_OUTPUT verdict=drift' 'verdict=drift'
expect_output 'leading zero: GITHUB_OUTPUT testnet_version' 'testnet_version=v0.08.0'

# ── A CONFORMING version is bounded too ───────────────────────────────
# The sanitisers cap what a host serves at 120 bytes, but only on the
# paths a MALFORMED value takes. A version that conformed was echoed raw
# into the report, the <name>_version output, the run summary table and
# the issue body: `v0.0.0-` and 10,000 characters produced a 10 kB report
# and a 10 kB output line, against a header, a deployment doc and a
# changelog that all say nothing a host serves reaches any of those
# uncapped. The pattern now bounds every part of a version, so an
# over-long one is not a version and takes the capped path.
long_suffix="$(head -c 10000 /dev/zero | tr '\0' 'a')"
mkdir -p "$TMP/overlong"
body v0.59.1 > "$TMP/overlong/r1.json"; body v0.59.1 > "$TMP/overlong/futurenet.json"
jq -cn --arg v "v0.0.0-$long_suffix" '{data:{version:$v}}' > "$TMP/overlong/testnet.json"
rm -f "$TMP/out.txt"
run "$TMP/overlong" $((T0 + 2 * DAY))
expect 'an over-long conforming version is not a version → DEGRADED' 2 'testnet unreadable'
expect 'the served bytes are capped and their true size named' 2 '(10007 bytes) is not a release tag'
expect_lines_bounded 'over-long version: no report or output line exceeds 256 bytes' 256
expect_lines_prefixed 'over-long version: every report line carries the prefix'
expect_output_wellformed 'over-long version: GITHUB_OUTPUT is one key=value per line, one verdict'
expect_output 'over-long version: GITHUB_OUTPUT testnet_state=unreadable' 'testnet_state=unreadable'
# The longest label a real build can serve still fits inside the bound:
# `git describe` with a full 40-hex object name on a dirty tree.
fixture "$TMP/longest-real" r1=v0.59.1 \
  testnet=v0.59.1-3-g0123456789abcdef0123456789abcdef01234567-dirty futurenet=v0.59.1
run "$TMP/longest-real" $((T0 + 2 * DAY))
expect 'the longest real describe label still sits on its base' 0 \
  'testnet   v0.59.1-3-g0123456789abcdef0123456789abcdef01234567-dirty  in step'
# A component wider than the key can hold is reported as not a version
# rather than silently colliding (v0.1000.0 and v1.0.0 pack alike).
fixture "$TMP/widecomp" r1=v0.59.1 testnet=v0.1000.0 futurenet=v0.59.1
run "$TMP/widecomp" $((T0 + 2 * DAY))
expect 'a four-digit component is not a version' 2 "served version 'v0.1000.0' (9 bytes) is not a release tag"

# ── An empty follower list is not an in-step fleet ────────────────────
# With no follower to compare, every counter stayed 0 and the verdict
# fell through to in-step, exit 0 — so the workflow's close step would
# close the tracking issue on a fleet nobody looked at. A misconfigured
# FLEET_FOLLOWERS is exactly how that happens: deploy.yml builds the
# list from its region input.
fixture "$TMP/nofollowers" r1=v0.59.1
rm -f "$TMP/out.txt"
run "$TMP/nofollowers" $((T0 + 2 * DAY)) FLEET_FOLLOWERS=' '
expect 'no follower listed → fail closed, never in step' 2 'NO FOLLOWERS'
expect_absent 'an empty fleet is never reported OK' 'OK —'
expect_output 'GITHUB_OUTPUT verdict=no-followers' 'verdict=no-followers'
expect_output_wellformed 'no followers: GITHUB_OUTPUT is one key=value per line, one verdict'
# Nothing readable AND nothing listed leaves the outputs array empty —
# where bash 3.2 (macOS /bin/bash, what scripts/dev/verify.sh runs)
# treated an empty [@] expansion as an unbound variable under `set -u`
# and died with exit 1 and an EMPTY GITHUB_OUTPUT, and where bash 4.4+
# instead handed printf no arguments and wrote one BLANK line into it.
fixture "$TMP/nothing"
rm -f "$TMP/out.txt"
run "$TMP/nothing" $((T0 + 2 * DAY)) FLEET_FOLLOWERS=' '
expect 'nothing readable and nothing listed → DEGRADED, not a crash' 2 'reference r1 unreadable'
expect_output 'GITHUB_OUTPUT verdict=degraded with an empty outputs array' 'verdict=degraded'
expect_output_wellformed 'empty outputs array: GITHUB_OUTPUT is one key=value per line, one verdict'
expect_file_count 'the outputs array is expanded exactly once' \
  "$CHECK" '"${outputs[@]}"' 1
expect_file_count 'that expansion sits behind a count guard' \
  "$CHECK" '[ "${#outputs[@]}" -gt 0 ]' 1

# ── The workflow's second layer cannot be silently reverted ───────────
# The script makes the report one prefixed line per host; the WORKFLOW
# adds a random heredoc delimiter around the multi-line `report` output,
# because the runner's file-command parser keeps the LAST key it reads
# and a guessable terminator followed by a forged `verdict=in-step` line
# inside the value would end the value early and rewrite the verdict.
# Reverting that delimiter to a literal leaves every fixture above green
# — the script is not what emits it — so the workflow file is asserted
# directly.
WORKFLOW=".github/workflows/fleet-release-drift.yml"
expect_file 'the workflow derives the report delimiter at random' \
  "$WORKFLOW" '^ *d="\$\(openssl rand -hex [0-9]+\)"$'
expect_file 'the report value is terminated by that delimiter' \
  "$WORKFLOW" "printf 'report<<%s"
expect_file_absent 'the report delimiter is never a literal token' \
  "$WORKFLOW" 'report<<[A-Za-z0-9_]'

echo
echo "check-fleet-release-drift-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ] || exit 1
