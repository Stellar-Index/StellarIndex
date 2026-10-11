#!/usr/bin/env bash
# check-main-ci-health.sh — main-CI-red tripwire.
#
# Queries recent completed runs of the `ci` workflow on `main` and decides
# whether main has been red past a threshold: consecutive failures OR
# wall-clock age since the last green run. Decision core of
# .github/workflows/ci-health.yml, which turns RED into a tracking issue and
# a failed run.
#
# Uses the gh CLI with the workflow's token (GH_TOKEN); no new secrets.
#   GH_REPO           owner/repo (gh reads it automatically in Actions)
#   CI_WORKFLOW_FILE  workflow filename to watch     (default: ci.yml)
#   CI_HEALTH_BRANCH  branch to watch                (default: main)
#   FAIL_RUNS         consecutive red runs → RED     (default: 3)
#   FAIL_HOURS        hours since last green → RED   (default: 6)
#   CI_HEALTH_FIXTURE JSON file shaped like the API's {"workflow_runs":[...]},
#                     for offline tests instead of gh. An optional
#                     "latest_runs" array stands in for the unfiltered
#                     listing the staleness check reads.
#
# Exit code: 0 = healthy (main green, or completed runs exist but none carry
#            a health signal yet, e.g. cancelled-only).
#            1 = RED beyond threshold, or UNKNOWN: zero runs came back (the
#            same shape a wrong CI_WORKFLOW_FILE/CI_HEALTH_BRANCH produces,
#            so a fault, not healthy) or the run listing is stale.
#            A report is printed to stdout in all cases.
set -euo pipefail

CI_WORKFLOW_FILE="${CI_WORKFLOW_FILE:-ci.yml}"
CI_HEALTH_BRANCH="${CI_HEALTH_BRANCH:-main}"
FAIL_RUNS="${FAIL_RUNS:-3}"
FAIL_HOURS="${FAIL_HOURS:-6}"

# ── Fetch recent completed runs (newest first) ──
# jq reduces each run to "<conclusion>\t<created_at>\t<html_url>\t<head_sha>".
# Only unambiguous green/red conclusions are kept, so a cancelled run can't
# mask or manufacture a red streak.
JQ_FILTER='.workflow_runs[]
  | select(.conclusion=="success" or .conclusion=="failure"
           or .conclusion=="timed_out" or .conclusion=="startup_failure")
  | [.conclusion, .created_at, .html_url, (.head_sha[0:7])]
  | @tsv'

PER_PAGE=100
MAX_PAGES=5
NL=$'\n'

# fetch_page <n> — one page of completed runs, newest first. The fixture is
# sliced the way the API pages it.
fetch_page() {
  if [ -n "${CI_HEALTH_FIXTURE:-}" ]; then
    jq --argjson p "$1" --argjson n "$PER_PAGE" \
      '{workflow_runs: .workflow_runs[($p - 1) * $n : $p * $n]}' "$CI_HEALTH_FIXTURE"
  else
    gh api \
      "repos/${GH_REPO}/actions/workflows/${CI_WORKFLOW_FILE}/runs?branch=${CI_HEALTH_BRANCH}&status=completed&per_page=${PER_PAGE}&page=$1"
  fi
}

# latest_runs — the newest runs, unfiltered. fetch_page's filtered listing
# comes from a search index that has answered a month stale; this is the
# reference for it.
latest_runs() {
  if [ -n "${CI_HEALTH_FIXTURE:-}" ]; then
    jq '{workflow_runs: (.latest_runs // [])}' "$CI_HEALTH_FIXTURE"
  else
    gh api "repos/${GH_REPO}/actions/workflows/${CI_WORKFLOW_FILE}/runs?per_page=${PER_PAGE}"
  fi
}

# Rapid merges cancel most runs on main (cancel-in-progress), so one page can
# hold little signal. Page on until a green run is seen, reds exceed
# FAIL_RUNS, or history runs out.
collect_runs() {
  raw_count=0
  runs=""
  newest_listed=""
  page=1
  while [ "$page" -le "$MAX_PAGES" ]; do
    api_json="$(fetch_page "$page")"
    page_count="$(jq -r '.workflow_runs | length' <<<"$api_json")"
    raw_count=$((raw_count + page_count))
    page_newest="$(jq -r '[.workflow_runs[].created_at] | max // ""' <<<"$api_json")"
    if [[ "$page_newest" > "$newest_listed" ]]; then
      newest_listed="$page_newest"
    fi
    page_runs="$(jq -r "$JQ_FILTER" <<<"$api_json")"
    if [ -n "$page_runs" ]; then
      runs="${runs:+${runs}${NL}}${page_runs}"
    fi
    signal_count="$(grep -c . <<<"$runs" || true)"
    if grep -q '^success' <<<"$runs" || [ "$signal_count" -gt "$FAIL_RUNS" ] \
        || [ "$page_count" -lt "$PER_PAGE" ]; then
      break
    fi
    page=$((page + 1))
  done
  dropped=$((raw_count - signal_count))
}

# A run on the branch that finished over LISTING_LAG_MIN minutes ago and is
# newer than everything listed proves the listing stale. Re-ask; if still
# stale, report UNKNOWN, never judge.
LISTING_LAG_MIN=15
STALE_RETRIES=3
lag_cutoff="$(date -u -d "-${LISTING_LAG_MIN} minutes" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
  || date -u -v-"${LISTING_LAG_MIN}"M +%Y-%m-%dT%H:%M:%SZ)"
attempt=1
while :; do
  collect_runs
  newest_known="$(latest_runs | jq -r --arg b "$CI_HEALTH_BRANCH" --arg c "$lag_cutoff" \
    '[.workflow_runs[]
      | select(.head_branch == $b and .status == "completed" and .updated_at <= $c)
      | .created_at] | max // ""')"
  if [ -z "$newest_known" ] || [[ ! "$newest_listed" < "$newest_known" ]]; then
    break
  fi
  if [ "$attempt" -ge "$STALE_RETRIES" ]; then
    echo "ci-health: UNKNOWN — the '${CI_WORKFLOW_FILE}' run listing for '${CI_HEALTH_BRANCH}' is stale: its newest run was created ${newest_listed:-never (it is empty)}, but a run created ${newest_known} has completed. Asked ${STALE_RETRIES} times; no verdict is read from a stale listing."
    exit 1
  fi
  attempt=$((attempt + 1))
  [ -n "${CI_HEALTH_FIXTURE:-}" ] || sleep 20
done

# Count BEFORE the signal filter: a 200 with zero runs is what a wrong
# CI_WORKFLOW_FILE/CI_HEALTH_BRANCH also returns, and "probed nothing" must
# not read as "green". Only when runs exist but none are actionable (e.g.
# cancelled-only) is an empty `runs` a genuine "no signal yet".
if [ -z "$runs" ]; then
  if [ "$raw_count" -eq 0 ]; then
    echo "ci-health: UNKNOWN — zero '${CI_WORKFLOW_FILE}' runs at all came back for '${CI_HEALTH_BRANCH}'. This is indistinguishable from a misconfigured CI_WORKFLOW_FILE/CI_HEALTH_BRANCH and is NOT treated as healthy."
    exit 1
  fi
  echo "ci-health: no completed '${CI_WORKFLOW_FILE}' runs on '${CI_HEALTH_BRANCH}' with a health signal (found ${raw_count} completed run(s), none actionable) — nothing to assess."
  exit 0
fi
echo "ci-health: '${CI_WORKFLOW_FILE}' on '${CI_HEALTH_BRANCH}': scanned ${raw_count} completed run(s), ${dropped} dropped as cancelled/skipped/neutral."

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

# Streak age: now minus created_at of the OLDEST run in the streak.
now_epoch="$(date -u +%s)"
# ISO-8601 → epoch; GNU and BSD date differ.
to_epoch() {
  date -u -d "$1" +%s 2>/dev/null || date -u -j -f "%Y-%m-%dT%H:%M:%SZ" "$1" +%s 2>/dev/null || echo 0
}
streak_epoch="$(to_epoch "$streak_oldest_created")"
if [ "$streak_epoch" -gt 0 ]; then
  age_hours=$(( (now_epoch - streak_epoch) / 3600 ))
else
  age_hours=0
fi

echo "ci-health: '${CI_WORKFLOW_FILE}' on '${CI_HEALTH_BRANCH}' red streak = ${streak} run(s), oldest-in-streak ${streak_oldest_created} (~${age_hours}h ago)."
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
  echo "ci-health: RED — '${CI_WORKFLOW_FILE}' on '${CI_HEALTH_BRANCH}' has been red for ${streak} run(s) / ~${age_hours}h, past threshold."
  exit 1
fi

echo "ci-health: red but under threshold (streak ${streak} < ${FAIL_RUNS} and age ${age_hours}h < ${FAIL_HOURS}h) — not faulting yet."
exit 0
