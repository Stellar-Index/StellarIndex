#!/usr/bin/env bash
# check-scheduled-controls-test.sh — fixture tests for the dead-control
# detector (scripts/ci/check-scheduled-controls.sh, #496).
#
# The defect this pins: the repo's existing liveness watchdog reads the
# LATEST run of a workflow regardless of trigger, so ansible-drift.yml —
# 0 green out of 8 scheduled runs across seven weeks — read HEALTHY,
# because the newest run in its history was a green manual dispatch.
# Measured on 2026-09-07:
#
#     CI_WORKFLOW_FILE=ansible-drift.yml FAIL_RUNS=2 FAIL_HOURS=216 \
#       scripts/ci/check-main-ci-health.sh   → exit 0, "not faulting yet"
#
# So the cases below are written around the two properties that fix it:
# only SCHEDULED runs count, and the threshold is derived from the
# workflow's own cron cadence rather than one flat number.
#
# Offline: SCHEDULED_CONTROLS_FIXTURE feeds the run history and
# WORKFLOW_DIR the cron source, so no gh, no token, no network.
# NOW_EPOCH pins "now" so the day arithmetic is deterministic.
#
# Run: bash scripts/ci/check-scheduled-controls-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
CHECK="$PWD/scripts/ci/check-scheduled-controls.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
asserts=0

# Fixed "now" so every fixture date below is exact, not drifting.
NOW="2026-09-07T12:00:00Z"
NOW_E=$(date -u -j -f "%Y-%m-%dT%H:%M:%SZ" "$NOW" +%s 2>/dev/null || date -u -d "$NOW" +%s)

ago() {  # <days> → ISO-8601 that many days before NOW
  local e=$(( NOW_E - $1 * 86400 ))
  date -u -r "$e" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d "@$e" +%Y-%m-%dT%H:%M:%SZ
}

reset_case() {
  rm -rf "$TMP/wf" "$TMP/fx"
  mkdir -p "$TMP/wf" "$TMP/fx"
}

# workflow <basename> <cron>
workflow() {
  cat > "$TMP/wf/$1" <<YAML
name: ${1%.yml}

# A comment that mentions cron: 0 0 * * * and must not be enumerated.
on:
  schedule:
    - cron: '$2'
  workflow_dispatch: {}

jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - run: echo cron: not-a-trigger
YAML
}

# history <basename> <state> <created_days_ago> [<conclusion>:<days_ago> ...]
history() {
  local base="$1" state="$2" created="$3"; shift 3
  local runs="" r c d
  for r in "$@"; do
    c="${r%%:*}"; d="${r##*:}"
    runs="${runs}${runs:+,}{\"conclusion\":\"$c\",\"created_at\":\"$(ago "$d")\"}"
  done
  printf '{"state":"%s","created_at":"%s","workflow_runs":[%s]}\n' \
    "$state" "$(ago "$created")" "$runs" > "$TMP/fx/${base}.json"
}

run_check() {
  OUT="$(WORKFLOW_DIR="$TMP/wf" SCHEDULED_CONTROLS_FIXTURE="$TMP/fx" \
    NOW_EPOCH="$NOW_E" SELF_WORKFLOW_FILE="${SELF:-none.yml}" \
    bash "$CHECK" 2>&1)"
  RC=$?
}

expect() {
  local name="$1" want_rc="$2" want_sub="${3:-}"
  asserts=$((asserts + 1))
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

expect_absent() {
  local name="$1" bad_sub="$2"
  asserts=$((asserts + 1))
  if grep -qF -- "$bad_sub" <<<"$OUT"; then
    echo "FAIL: $name — output should not contain '$bad_sub'" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1)); return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

# ── The two cases the brief names ───────────────────────────────────

# ansible-drift's real shape on 2026-09-07: weekly cron, registered 53
# days ago, every one of its scheduled runs failed, no green ever.
reset_case
workflow ansible-drift.yml '17 6 * * 1'
history ansible-drift.yml active 53 \
  failure:0 failure:7 failure:14 failure:21 failure:28
run_check
expect '0 green of 5 scheduled (weekly, 53d) → DEAD' 1 'DEAD  ansible-drift.yml'
expect '…and names the ratio' 1 'green 0/5 scheduled'
expect '…and names never-green with the registration date' 1 'never — all 5 scheduled runs since'
expect '…and lists it machine-readably' 1 'scheduled-controls-dead: ansible-drift.yml'

# deploy-protection's real shape: 7 green of 7, newest 0 days old.
reset_case
workflow deploy-protection.yml '43 7 * * 1'
history deploy-protection.yml active 44 \
  success:0 success:7 success:14 success:21 success:28 success:35 success:42
run_check
expect '7/7 green → live, gate passes' 0 'live  deploy-protection.yml'
expect '…and names the last green date' 0 'green 7/7 scheduled'
expect_absent '…and flags nothing' 'scheduled-controls-dead: deploy-protection.yml'

# ── N is derived from the cadence, not flat ─────────────────────────
# The same 8-day-old last-green is LIVE for a weekly control (N=21) and
# DEAD for a daily one (N=7). A single flat N cannot express both; this
# pair is what makes the derivation load-bearing.

reset_case
workflow weekly.yml '0 6 * * 1'
history weekly.yml active 60 failure:1 success:8 success:15
run_check
expect 'weekly, last green 8d ago → live (N=21d)' 0 'live  weekly.yml'
expect '…and shows the weekly threshold' 0 'N=21d'

reset_case
workflow daily.yml '25 7 * * *'
history daily.yml active 60 failure:1 failure:3 failure:5 success:8
run_check
expect 'daily, last green 8d ago → DEAD (N=7d)' 1 'DEAD  daily.yml'
expect '…and shows the daily threshold' 1 'N=7d'

# The weekly boundary itself: 20 days is inside three fires of grace,
# 22 is not.
reset_case
workflow weekly.yml '0 6 * * 1'
history weekly.yml active 60 failure:6 failure:13 success:20
run_check
expect 'weekly, last green 20d ago → live (under N=21d)' 0 'live  weekly.yml'

reset_case
workflow weekly.yml '0 6 * * 1'
history weekly.yml active 60 failure:1 failure:8 failure:15 success:22
run_check
expect 'weekly, last green 22d ago → DEAD (over N=21d)' 1 'DEAD  weekly.yml'

# A 2-hourly control gets the 7-day floor, not 3 fires (6 hours) — a
# runner blip must not page.
reset_case
workflow busy.yml '17 */2 * * *'
history busy.yml active 60 failure:0 failure:0 failure:0 success:2
run_check
expect '2-hourly, red for 2d → live (7d floor, not 3 fires)' 0 'N=7d'

# A monthly control is capped at 35 days, not 3 months.
reset_case
workflow monthly.yml '0 3 1 * *'
history monthly.yml active 200 failure:2 success:33
run_check
expect 'monthly, last green 33d ago → live (cap N=35d)' 0 'N=35d'

reset_case
workflow monthly.yml '0 3 1 * *'
history monthly.yml active 200 failure:2 success:40
run_check
expect 'monthly, last green 40d ago → DEAD (cap N=35d)' 1 'DEAD  monthly.yml'

# ── Shapes that must not be read as green ───────────────────────────

# Cancelled/skipped runs carry no health signal, exactly as
# check-main-ci-health.sh treats them — a cancelled run at the tip must
# not stand in for a pass.
reset_case
workflow weekly.yml '0 6 * * 1'
history weekly.yml active 60 cancelled:1 cancelled:8 cancelled:15 success:30
run_check
expect 'cancelled runs are not green → DEAD on the 30d-old real pass' 1 'DEAD  weekly.yml'
expect '…and they are not counted in the ratio either' 1 'green 1/1 scheduled'

# A disabled workflow cannot fire at all, however recent its last pass.
reset_case
workflow weekly.yml '0 6 * * 1'
history weekly.yml disabled_inactivity 60 success:0
run_check
expect 'disabled schedule → DEAD even with a green run today' 1 'the workflow is disabled_inactivity'

# A cron that has never fired at all.
reset_case
workflow weekly.yml '0 6 * * 1'
history weekly.yml active 60
run_check
expect 'no scheduled run ever, 60d old → DEAD' 1 'the cron has not fired'

# ── Shapes that must NOT be flagged ─────────────────────────────────

# A workflow merged two days ago has not had its first weekly fire.
reset_case
workflow fresh.yml '0 6 * * 1'
history fresh.yml active 2
run_check
expect 'newly registered weekly workflow, no runs yet → live' 0 'live  fresh.yml'

# Self-reference: the detector's own workflow is the one exclusion.
reset_case
workflow ci-health.yml '17 */2 * * *'
history ci-health.yml active 60 failure:0 failure:1 failure:2
SELF=ci-health.yml run_check
expect 'the detector does not report itself' 0 'skip  ci-health.yml'
expect '…and says why' 0 'self-reference'

# ── Enumeration is by schedule: trigger, not a hardcoded list ───────

# A workflow with no schedule: trigger is not a control and is skipped;
# a `cron:` string in a comment or a step is not a trigger.
reset_case
workflow weekly.yml '0 6 * * 1'
history weekly.yml active 60 success:1
cat > "$TMP/wf/push-only.yml" <<'YAML'
name: push-only
on:
  push:
    branches: [main]
  # - cron: '0 0 * * *'   (a comment, not a trigger)
jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - run: echo "- cron: '0 0 * * *'"
YAML
run_check
expect 'a push-only workflow is not a control' 0 'assessed 1 of 1 scheduled workflow'
expect_absent '…and is not named in the report' 'push-only.yml'

# ── Anti-vacuity: the gate must never pass over nothing ─────────────

reset_case
cat > "$TMP/wf/push-only.yml" <<'YAML'
name: push-only
on:
  push:
    branches: [main]
jobs:
  j:
    runs-on: ubuntu-latest
    steps: [{run: 'true'}]
YAML
run_check
expect 'no scheduled workflow found at all → exit 2, not a clean pass' 2 'found NO workflow with a schedule'

reset_case
workflow weekly.yml '0 6 * * 1'
# No fixture json → the API read fails for every workflow.
run_check
expect 'every workflow unreadable → exit 2, not a clean pass' 2 'The gate did not run'

# One readable, one not: the unreadable one is UNKNOWN, never green.
reset_case
workflow weekly.yml '0 6 * * 1'
workflow other.yml '0 6 * * 1'
history weekly.yml active 60 success:1
run_check
# A partial sweep still REPORTS everything it managed to assess — that part
# was right and is kept. What changed: it must not also return success.
# Measured live 2026-09-07 before this was tightened: two workflows fell into
# the unreadable bucket on a secondary-rate-limit blip, one of them the only
# genuinely dead control, and the sweep printed "every scheduled control has
# passed" and exited 0. A gate that cannot read a control has not checked it,
# and this gate exists precisely to stop a control's silence reading as health.
expect 'a partially unreadable sweep still reports what it read' 1 "????  other.yml"
expect '…and self-accounts the shortfall' 1 'assessed 1 of 2 scheduled workflow(s)'
expect '…and refuses to call it a pass' 1 'did not assess them'

# ── Wiring the fixtures cannot reach ────────────────────────────────
# The whole point is that only SCHEDULED runs count, and that the
# workflow list comes from the tree rather than a hardcoded array.
# Both live in the gh/api call, which fixture mode bypasses — so assert
# them against the source, and count the assertions so a rename cannot
# silently drop them.

wiring=0
src="$(cat "$CHECK")"
if grep -q 'event=schedule' <<<"$src"; then wiring=$((wiring + 1)); else
  echo "FAIL: the run query must filter event=schedule — a green manual dispatch is not evidence the schedule works" >&2
  fail=$((fail + 1))
fi
asserts=$((asserts + 1))
if grep -q 'status=completed' <<<"$src"; then wiring=$((wiring + 1)); else
  echo "FAIL: the run query must filter status=completed — an in-flight run has no conclusion" >&2
  fail=$((fail + 1))
fi
asserts=$((asserts + 1))
if grep -q 'WORKFLOW_DIR"/\*.yml' <<<"$src"; then wiring=$((wiring + 1)); else
  echo "FAIL: workflows must be enumerated from the tree, not a hardcoded list" >&2
  fail=$((fail + 1))
fi
asserts=$((asserts + 1))
if [ "$wiring" -eq 3 ]; then
  echo "ok: run query is scheduled-and-completed only, over an enumerated workflow dir"
  pass=$((pass + 1))
else
  echo "FAIL: wiring assertions emitted $wiring of 3" >&2
  fail=$((fail + 1))
fi
asserts=$((asserts + 1))

# ── ci-health.yml wires the detector, and its cron gate is real ─────
# The issue/fail steps are gated on one specific cron string. If that
# string ever stops matching the on: block the detector silently stops
# acting, which is the very failure mode it exists to catch.
CIH="$PWD/.github/workflows/ci-health.yml"
# Buffer before slicing: an early-exit consumer on a pipe under
# pipefail is a SIGPIPE trap (scripts/ci/lint-shell-sigpipe.sh).
gate_matches="$(grep -oE "DEAD_CONTROL_CRON: '[^']+'" "$CIH" || true)"
gate="$(printf '%s\n' "$gate_matches" | sed -n "1s/.*: '//p" | sed "s/'\$//")"
asserts=$((asserts + 1))
if [ -n "$gate" ] && grep -qF "cron: '$gate'" "$CIH"; then
  echo "ok: ci-health's dead-control cron gate ('$gate') exists in its own on: schedule block"
  pass=$((pass + 1))
else
  echo "FAIL: the dead-control job is gated on cron '$gate', which is not in ci-health.yml's schedule — the job would never fire on a schedule" >&2
  fail=$((fail + 1))
fi
# …and the gates must READ it from there, not restate it, so there is
# one place to change and no copy to fall out of step.
asserts=$((asserts + 1))
if grep -q 'github.event.schedule == env.DEAD_CONTROL_CRON' "$CIH" &&
   ! grep -q "github.event.schedule == '" "$CIH"; then
  echo "ok: the cadence gates read env.DEAD_CONTROL_CRON rather than restating the cron"
  pass=$((pass + 1))
else
  echo "FAIL: a cadence gate hardcodes a cron string instead of reading env.DEAD_CONTROL_CRON — the copies will drift" >&2
  fail=$((fail + 1))
fi
asserts=$((asserts + 1))
if grep -q '\./scripts/ci/check-scheduled-controls\.sh' "$CIH"; then
  echo "ok: ci-health.yml calls the detector"
  pass=$((pass + 1))
else
  echo "FAIL: ci-health.yml does not call scripts/ci/check-scheduled-controls.sh" >&2
  fail=$((fail + 1))
fi

echo
echo "check-scheduled-controls-test: ${pass} passed, ${fail} failed, ${asserts} assertions requested"
if [ "$asserts" -lt 31 ]; then
  echo "check-scheduled-controls-test: FAIL — only ${asserts} assertions ran; cases have been lost" >&2
  exit 1
fi
[ "$fail" -eq 0 ] || exit 1
