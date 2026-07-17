#!/usr/bin/env bash
# check-main-ci-health.sh — main-CI-red tripwire (audit C4-16).
#
# The operator doesn't notice when `main` CI goes red for long periods
# (chronically-red main is a standing pain). This script queries the
# recent completed runs of the `ci` workflow on `main` and decides
# whether main has been red past a threshold — a consecutive-failure
# count OR a wall-clock age since the last green run. It is the decision
# core called by .github/workflows/ci-health.yml, which turns a RED
# verdict into an opened/updated tracking issue + a failed run.
#
# Self-contained: uses the gh CLI with the workflow's ${{ github.token }}
# (via GH_TOKEN) — no new secrets. Set:
#   GH_REPO           owner/repo (gh reads this automatically in Actions)
#   CI_WORKFLOW_FILE  workflow filename to watch     (default: ci.yml)
#   CI_HEALTH_BRANCH  branch to watch                (default: main)
#   FAIL_RUNS         consecutive red runs → RED     (default: 3)
#   FAIL_HOURS        hours since last green → RED    (default: 6)
#   CI_HEALTH_FIXTURE optional path to a JSON file shaped like the
#                     GitHub API's {"workflow_runs":[...]} — used for
#                     offline tests instead of calling gh.
#
# Exit code: 0 = healthy (main green, or not enough signal to fault),
#            1 = RED beyond threshold. A human-readable report is printed
#            to stdout in both cases.
set -euo pipefail

CI_WORKFLOW_FILE="${CI_WORKFLOW_FILE:-ci.yml}"
CI_HEALTH_BRANCH="${CI_HEALTH_BRANCH:-main}"
FAIL_RUNS="${FAIL_RUNS:-3}"
FAIL_HOURS="${FAIL_HOURS:-6}"

# ── Fetch recent completed runs (newest first) ──
# jq reduces each run to a single line: "<conclusion>\t<created_at>\t<html_url>\t<head_sha>".
# Only conclusions that are unambiguously green/red are kept; cancelled /
# skipped / neutral runs carry no health signal and are dropped so a
# cancelled run can't mask (or manufacture) a red streak.
JQ_FILTER='.workflow_runs[]
  | select(.conclusion=="success" or .conclusion=="failure"
           or .conclusion=="timed_out" or .conclusion=="startup_failure")
  | [.conclusion, .created_at, .html_url, (.head_sha[0:7])]
  | @tsv'

if [ -n "${CI_HEALTH_FIXTURE:-}" ]; then
  runs="$(jq -r "$JQ_FILTER" "$CI_HEALTH_FIXTURE")"
else
  runs="$(gh api \
    "repos/${GH_REPO}/actions/workflows/${CI_WORKFLOW_FILE}/runs?branch=${CI_HEALTH_BRANCH}&status=completed&per_page=30" \
    --jq "$JQ_FILTER")"
fi

if [ -z "$runs" ]; then
  echo "ci-health: no completed '${CI_WORKFLOW_FILE}' runs on '${CI_HEALTH_BRANCH}' with a health signal — nothing to assess."
  exit 0
fi

# ── Walk newest→oldest, measure the red streak at the tip ──
newest_conclusion=""
streak=0
streak_oldest_created=""
first_red_url=""
last_good_created=""
while IFS=$'\t' read -r conclusion created url _sha; do
  [ -z "$conclusion" ] && continue
  if [ -z "$newest_conclusion" ]; then
    newest_conclusion="$conclusion"
  fi
  if [ "$conclusion" = "success" ]; then
    last_good_created="$created"
    break
  fi
  # red run at (or continuing) the tip
  streak=$((streak + 1))
  streak_oldest_created="$created"
  [ -z "$first_red_url" ] && first_red_url="$url"
done <<EOF
$runs
EOF

if [ "$newest_conclusion" = "success" ] || [ "$streak" -eq 0 ]; then
  echo "ci-health: HEALTHY — latest '${CI_WORKFLOW_FILE}' run on '${CI_HEALTH_BRANCH}' is green."
  exit 0
fi

# Age of the red streak: now − created_at of the OLDEST run in the streak.
now_epoch="$(date -u +%s)"
# Portable ISO-8601 → epoch (GNU date and BSD/macOS date differ).
to_epoch() {
  date -u -d "$1" +%s 2>/dev/null || date -u -j -f "%Y-%m-%dT%H:%M:%SZ" "$1" +%s 2>/dev/null || echo 0
}
streak_epoch="$(to_epoch "$streak_oldest_created")"
if [ "$streak_epoch" -gt 0 ]; then
  age_hours=$(( (now_epoch - streak_epoch) / 3600 ))
else
  age_hours=0
fi

echo "ci-health: main '${CI_HEALTH_BRANCH}' red streak = ${streak} run(s), oldest-in-streak ${streak_oldest_created} (~${age_hours}h ago)."
echo "ci-health: thresholds FAIL_RUNS=${FAIL_RUNS} FAIL_HOURS=${FAIL_HOURS}."
if [ -n "$first_red_url" ]; then
  echo "ci-health: most recent failing run: ${first_red_url}"
fi
if [ -n "$last_good_created" ]; then
  echo "ci-health: last green run at ${last_good_created}."
else
  echo "ci-health: no green run in the fetched window."
fi

if [ "$streak" -ge "$FAIL_RUNS" ] || [ "$age_hours" -ge "$FAIL_HOURS" ]; then
  echo "ci-health: RED — main has been red for ${streak} run(s) / ~${age_hours}h, past threshold."
  exit 1
fi

echo "ci-health: red but under threshold (streak ${streak} < ${FAIL_RUNS} and age ${age_hours}h < ${FAIL_HOURS}h) — not faulting yet."
exit 0
