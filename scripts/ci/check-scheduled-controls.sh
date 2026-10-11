#!/usr/bin/env bash
# check-scheduled-controls.sh - the scheduled-control detector (feeds the
# `dead-control` label). A scheduled workflow that has produced no PASSING
# scheduled run for N days is not delivering signal; the verdict says which
# person needs to do what:
#   DEAD  - no scheduled run at all within N days (disabled, lapsed
#           credential, billing cap, bad cron). Nobody is checking. Re-arm.
#   FAIL  - still firing, every run within N days red. The answer is ignored.
#           Act on the finding; re-arming is a category error.
#   ALARM - like FAIL, but the workflow declares that failing is how it
#           reports. Read the finding; the control needs no fix.
# DEAD is the age of the newest decisive scheduled run, FAIL/ALARM the age of
# the newest GREEN one; DEAD plus FAIL plus ALARM is the full flagged set.
#
# BOTH EXIT 1. A control that fires and fails is working, but a
# chronically red control hides its NEXT red, and exit 0 would let
# ci-health.yml close the tracking issue: an "expected to fail" list by
# another route. The split buys the right remedy, not a quieter gate.
#
# HOW A CONTROL DECLARES ALARM. One marker comment in the workflow file:
#     # scheduled-control: reports-by-failing "<name of the verdict step>"
# Not a list here and not inferred: a list rots on rename and lives in a
# file the workflow's reviewer never opens; inference is a guess that errs
# toward quiet. The marker is a diff on the workflow it describes.
#
# WHY THE MARKER CANNOT HIDE A REAL FAILURE:
#   1. It silences nothing. ALARM exits 1, is counted, holds the issue open
#      and fails ci-health.yml. Only the sentence changes, so a marker in
#      bad faith buys nothing.
#   2. It cannot reach DEAD. It only moves FAIL <-> ALARM.
#   3. The claim is checked against the run. startup_failure and timed_out
#      runs never rendered a report, so when the newest run concluded either
#      way the control stays FAIL.
#   4. The marker is checked against the file. It must name a step that
#      exists (full match); otherwise it is void and the control is FAIL,
#      and check-scheduled-controls-test.sh fails on the real tree.
#
# Scheduled runs only: a green manual dispatch would mask a dead schedule.
# Green counts only if the control renders a verdict on every run; a generic
# reader cannot tell a skipped step from a pass.
#
# N is derived from the cadence, so a weekly control is not judged by a
# daily clock:  N_days = clamp(ceil(GRACE_RUNS * interval_hours / 24),
#                              FLOOR_DAYS, CAP_DAYS)
#   GRACE_RUNS=3  three chances to go green: one flake, one bad week, then a
#                 pattern. Same N for the DEAD and FAIL questions.
#   FLOOR_DAYS=7  below a week a runner outage or long weekend pages.
#   CAP_DAYS=35   no control goes five weeks unexamined.
# Today: 2-hourly and daily -> 7 days; weekly -> 21; monthly -> 35.
#
# The only exclusion is self-reference (SELF_WORKFLOW_FILE): the detector is
# scheduled-and-red when it flags something, so it would flag itself forever.
# The marker above is not an exclusion; it cannot move a control out of a
# flagged class.
#
# Environment:
#   GH_REPO          owner/repo (gh reads this automatically in Actions)
#   WORKFLOW_DIR     directory of workflow YAML   (default .github/workflows)
#   GRACE_RUNS       fires of grace               (default 3)
#   FLOOR_DAYS       lower clamp on N             (default 7)
#   CAP_DAYS         upper clamp on N             (default 35)
#   SELF_WORKFLOW_FILE  basename to skip          (default ci-health.yml)
#   SCHEDULED_CONTROLS_FIXTURE  directory of <basename>.json files shaped
#                    like {"state":...,"created_at":...,"workflow_runs":[...]};
#                    offline tests, no gh, no token.
#   NOW_EPOCH        override "now" for deterministic tests.
#
# Exit: 0 = every control live, 1 = at least one DEAD, FAIL or ALARM (or
#           a workflow that could not be read), 2 = the gate could not
#           run (no scheduled workflow found, or none could be read).
#
# Machine-readable, one line each, always emitted, empty when none:
#   scheduled-controls-dead:     <csv>  stopped being scheduled
#   scheduled-controls-failing:  <csv>  scheduled, and red past N
#   scheduled-controls-alarming: <csv>  scheduled, red past N, red is its report
# .github/workflows/ci-health.yml reads ALL THREE (the self-test asserts it):
# a class the caller ignores silently stops being tracked.
#
# Output has no ::error:: / ::warning:: commands: the caller owns annotation,
# and the sweep runs on every ci-health trigger but acts once a day.
set -euo pipefail

# Outside Actions GH_REPO is unset, every API read 404s and the gate reports
# "none could be read", which looks like a dead control. Derive it from the
# checkout.
if [ -z "${GH_REPO:-}" ]; then
    GH_REPO="$(gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null || true)"
    export GH_REPO
fi

cd "$(dirname "$0")/../.."

WORKFLOW_DIR="${WORKFLOW_DIR:-.github/workflows}"
GRACE_RUNS="${GRACE_RUNS:-3}"
FLOOR_DAYS="${FLOOR_DAYS:-7}"
CAP_DAYS="${CAP_DAYS:-35}"
SELF_WORKFLOW_FILE="${SELF_WORKFLOW_FILE:-ci-health.yml}"

# The remedies differ by person and action; this report is the only place
# that is read.
REMEDY_DEAD='nobody is checking what this guards. Re-arm the schedule — a disabled workflow, a lapsed credential, a billing cap — then judge what it reports.'
REMEDY_FAIL='this control IS reporting and its report is red. Act on what it found; re-arming nothing will help, because nothing is un-armed.'
REMEDY_ALARM='this control reports BY FAILING and it is failing, on schedule, as designed. Read the finding it named — do not re-arm it and do not re-run it. It goes green when the thing it found is fixed.'
FIXTURE="${SCHEDULED_CONTROLS_FIXTURE:-}"
NOW_EPOCH="${NOW_EPOCH:-$(date -u +%s)}"

# Portable ISO-8601 to epoch (GNU and BSD date differ); trims the fraction
# and numeric offset the API can emit.
to_epoch() {
  local ts="${1%%.*}"
  ts="${ts%%+*}"
  case "$ts" in *Z) ;; *) ts="${ts}Z" ;; esac
  date -u -d "$ts" +%s 2>/dev/null ||
    date -u -j -f "%Y-%m-%dT%H:%M:%SZ" "$ts" +%s 2>/dev/null ||
    echo 0
}

days_since() {  # <iso8601> → whole days, or -1 if unparseable
  local e
  e="$(to_epoch "$1")"
  if [ "${e:-0}" -le 0 ]; then echo -1; return; fi
  echo $(( (NOW_EPOCH - e) / 86400 ))
}

# Cron enumeration: one cron per line from the `schedule:` block of `on:`. A
# hardcoded workflow list would leave every future workflow uncovered.
crons_of() {
  awk '
    # A top-level key resets the context. The class excludes "#" so a
    # column-0 comment does not close the on: mapping.
    /^[^[:space:]#]/ {
      in_on   = ($0 ~ /^["'"'"']?on["'"'"']?[[:space:]]*:/)
      in_sched = 0
      next
    }
    !in_on   { next }
    /^[[:space:]]*#/ { next }
    # A key nested in on: — schedule: opens the block, any other closes it.
    /^[[:space:]]+[A-Za-z_][A-Za-z0-9_-]*[[:space:]]*:/ {
      in_sched = ($0 ~ /^[[:space:]]+schedule[[:space:]]*:/)
    }
    in_sched && /^[[:space:]]*-[[:space:]]*cron[[:space:]]*:/ {
      line = $0
      sub(/^[[:space:]]*-[[:space:]]*cron[[:space:]]*:[[:space:]]*/, "", line)
      sub(/[[:space:]]+#.*$/, "", line)
      gsub(/^["'"'"']|["'"'"']$/, "", line)
      sub(/[[:space:]]+$/, "", line)
      if (line != "") print line
    }
  ' "$1"
}

# Approximate hours between fires; it feeds a clamped multiple-of-3
# threshold, so being one fire out never changes the verdict.
cron_interval_hours() {
  awk -v expr="$1" '
    function span_count(f, span,   n, i, parts, tok, a, b, c) {
      if (f == "*") return span
      if (f ~ /^\*\/[0-9]+$/) { c = span / substr(f, 3); return (c < 1 ? 1 : c) }
      n = split(f, parts, ",")
      c = 0
      for (i = 1; i <= n; i++) {
        tok = parts[i]
        if (tok ~ /^[0-9]+-[0-9]+(\/[0-9]+)?$/) {
          split(tok, a, "-")
          b = a[2]; if (b ~ /\//) { split(a[2], a, "/"); b = a[1] }
          c += (b - a[1]) + 1
        } else { c += 1 }
      }
      return (c < 1 ? 1 : c)
    }
    BEGIN {
      n = split(expr, f, /[[:space:]]+/)
      if (n < 5) { print 168; exit }          # unparseable → assume weekly
      fires = span_count(f[1], 60) * span_count(f[2], 24)
      if (fires < 1) fires = 1
      if (f[5] != "*")      period = 7  / span_count(f[5], 7)
      else if (f[3] != "*") period = 30 / span_count(f[3], 30)
      else                  period = 1
      h = 24 * period / fires
      printf "%d\n", (h < 1 ? 1 : int(h + 0.999))
    }
  '
}

# The reports-by-failing declaration, read from the workflow file itself.
# The marker names a step and the step must exist, else it is only a comment.

# <workflow file> -> the claimed step name, empty when unmarked. Captured
# whole and sliced after, not piped to an early-exit reader (lint-shell-sigpipe.sh).
reporting_step_of() {
  local hits
  hits="$(sed -n 's/^[[:space:]]*#[[:space:]]*scheduled-control:[[:space:]]*reports-by-failing[[:space:]]*"\(.*\)"[[:space:]]*$/\1/p' "$1" 2>/dev/null || true)"
  printf '%s' "${hits%%$'\n'*}"
}

# <workflow file> <step name> -> 0 when a real STEP under some job's steps:
# has exactly that name (not a substring, so deleting the step voids the
# claim). Comments are skipped so the marker cannot satisfy itself. Anchored
# under steps: because a bare `name:` scan also hits job names, the workflow
# name and `with: name:` inputs.
declares_step() {
  awk -v want="$2" '
    function indent_of(s,   n) { n = s; sub(/[^ ].*$/, "", n); return length(n) }
    {
      ind = indent_of($0)
      trimmed = $0
      sub(/^[[:space:]]+/, "", trimmed)
      if (trimmed ~ /^#/) next

      if (trimmed ~ /^steps:[[:space:]]*$/) {
        have_steps = 1; steps_indent = ind; item_col = -1
        next
      }
      if (have_steps && ind <= steps_indent && trimmed != "") {
        have_steps = 0; item_col = -1
      }
      if (!have_steps) next

      if (trimmed ~ /^-[[:space:]]/) {
        item_col = ind + 2
        sub(/^-[[:space:]]*/, "", trimmed)
      } else if (item_col < 0 || ind != item_col) {
        next
      }
      if (trimmed !~ /^name[[:space:]]*:/) next
      sub(/^name[[:space:]]*:[[:space:]]*/, "", trimmed)
      sub(/[[:space:]]+$/, "", trimmed)
      gsub(/^["'"'"']|["'"'"']$/, "", trimmed)
      if (trimmed == want) found = 1
    }
    END { exit(found ? 0 : 1) }
  ' "$1"
}

threshold_days() {  # <interval hours> → N days
  local ih="$1" n
  n=$(( (GRACE_RUNS * ih + 23) / 24 ))
  [ "$n" -lt "$FLOOR_DAYS" ] && n="$FLOOR_DAYS"
  [ "$n" -gt "$CAP_DAYS" ] && n="$CAP_DAYS"
  echo "$n"
}

cadence_label() {  # <interval hours> → human word
  local ih="$1"
  if   [ "$ih" -le 1 ];   then echo "hourly"
  elif [ "$ih" -lt 24 ];  then echo "${ih}-hourly"
  elif [ "$ih" -lt 48 ];  then echo "daily"
  elif [ "$ih" -lt 240 ]; then echo "weekly"
  else echo "monthly"
  fi
}

# Run history: only unambiguously green or red conclusions carry signal.
# Cancelled/skipped runs are dropped from the firing signal too, so an
# all-cancelled control lands DEAD, the louder verdict.
JQ_RUNS='.workflow_runs[]
  | select(.conclusion=="success" or .conclusion=="failure"
           or .conclusion=="timed_out" or .conclusion=="startup_failure")
  | [.conclusion, .created_at]
  | @tsv'

workflow_meta() {  # <basename> → "<state>\t<created_at>", empty on failure
  if [ -n "$FIXTURE" ]; then
    jq -r '[(.state // "active"), (.created_at // "")] | @tsv' "$FIXTURE/$1.json" 2>/dev/null
    return
  fi
# Retried: two calls per workflow in a burst trip the secondary rate limit,
# and an unreadable workflow is one this gate did not check.
  local attempt out
  for attempt in 1 2 3; do
    if out="$(gh api "repos/${GH_REPO}/actions/workflows/$1" \
                --jq '[.state, .created_at] | @tsv' 2>/dev/null)" \
       && [ -n "$out" ]; then
      printf '%s\n' "$out"
      return 0
    fi
    [ "$attempt" -lt 3 ] && sleep $(( attempt * 2 ))
  done
  return 1
}

workflow_runs() {  # <basename> → "<conclusion>\t<created_at>" lines, newest first
  if [ -n "$FIXTURE" ]; then
    jq -r "$JQ_RUNS" "$FIXTURE/$1.json" 2>/dev/null
  else
    gh api \
      "repos/${GH_REPO}/actions/workflows/$1/runs?event=schedule&status=completed&per_page=100" \
      --jq "$JQ_RUNS" 2>/dev/null
  fi
}

# ── Assess ──────────────────────────────────────────────────────────
requested=0
assessed=0
dead=0
failing=0
alarming=0
unknown=0
excluded=0
dead_list=""
failing_list=""
alarming_list=""
report=""

shopt -s nullglob
for wf in "$WORKFLOW_DIR"/*.yml "$WORKFLOW_DIR"/*.yaml; do
  crons="$(crons_of "$wf")"
  [ -z "$crons" ] && continue
  base="$(basename "$wf")"
  requested=$((requested + 1))

  if [ "$base" = "$SELF_WORKFLOW_FILE" ]; then
    excluded=$((excluded + 1))
    report="${report}  skip  ${base} — the detector's own workflow (self-reference)"$'\n'
    continue
  fi

  # Tightest cadence wins: more crons means more chances to go green.
  interval=0
  while IFS= read -r c; do
    [ -z "$c" ] && continue
    ih="$(cron_interval_hours "$c")"
    if [ "$interval" -eq 0 ] || [ "$ih" -lt "$interval" ]; then interval="$ih"; fi
  done <<EOF
$crons
EOF
  [ "$interval" -eq 0 ] && interval=168
  n_days="$(threshold_days "$interval")"
  label="$(cadence_label "$interval")"

  meta="$(workflow_meta "$base" || true)"
  if [ -z "$meta" ]; then
    unknown=$((unknown + 1))
    report="${report}  ????  ${base} — could not read the workflow from the API; not assessed"$'\n'
    echo "scheduled-controls: could not read ${base} — counted UNKNOWN, not green." >&2
    continue
  fi
  state="$(printf '%s' "$meta" | cut -f1)"
  created="$(printf '%s' "$meta" | cut -f2)"
  assessed=$((assessed + 1))

# Resolved once per workflow. A marker whose step is gone is VOID and the
# control falls back to FAIL; the row says so, because a mechanism that fails
# silently is this gate's own failure mode.
  declared_step="$(reporting_step_of "$wf")"
  declared=0
  decl_note=""
  if [ -n "$declared_step" ]; then
    if declares_step "$wf" "$declared_step"; then
      declared=1
    else
      decl_note="its reports-by-failing marker names a step \"${declared_step}\" that this workflow does not have — the declaration is VOID and this control is judged as an ordinary one"
      echo "scheduled-controls: ${base} carries a reports-by-failing marker naming a step it does not have (\"${declared_step}\") — declaration ignored." >&2
    fi
  fi

  runs="$(workflow_runs "$base" || true)"
  total=0
  green=0
  last_green=""
  last_run=""
  last_conclusion=""
  while IFS=$'\t' read -r conclusion created_at; do
    [ -z "$conclusion" ] && continue
    total=$((total + 1))
# Newest first (API order, as fixtures are written). The newest
# conclusion is kept because the marker applies only to a run that
# executed a step (point 3 in the header).
    if [ -z "$last_run" ]; then
      last_run="$created_at"
      last_conclusion="$conclusion"
    fi
    if [ "$conclusion" = "success" ]; then
      green=$((green + 1))
      [ -z "$last_green" ] && last_green="$created_at"
    fi
  done <<EOF
$runs
EOF

# Q1: is it still scheduled? With no run, the clock starts at registration,
# so a workflow merged yesterday is not judged for not having fired.
  if [ -n "$last_run" ]; then
    run_age="$(days_since "$last_run")"
    fired="last scheduled run ${last_run%%T*} (${run_age}d ago)"
  else
    run_age="$(days_since "$created")"
    fired="last scheduled run: never — the cron has not fired since the workflow was registered ${created%%T*} (${run_age}d ago)"
  fi

# Q2: is it passing? Newest GREEN run, same registration clock.
  if [ -n "$last_green" ]; then
    green_age="$(days_since "$last_green")"
    since="${fired}; last green scheduled run ${last_green%%T*} (${green_age}d ago)"
  else
    green_age="$(days_since "$created")"
    if [ "$total" -eq 0 ]; then
      since="${fired}; last green scheduled run: never"
    else
      since="${fired}; last green scheduled run: never — all ${total} scheduled runs since ${created%%T*} (${green_age}d ago) failed"
    fi
  fi

# Q1 first: a no makes Q2 unanswerable; a control not running has stopped,
# not failed.
  verdict="live"
  detail=""
  remedy=""
  if [ "$run_age" -lt 0 ] || [ "$run_age" -ge "$n_days" ]; then
    verdict="DEAD"
    if [ "$run_age" -lt 0 ]; then
# -1 means unparseable; without this an undateable run would pass.
      detail="the newest scheduled run's timestamp could not be parsed — cannot verify the schedule is firing"
    elif [ "$total" -eq 0 ]; then
      detail="the cron has not fired once since the workflow was registered ${run_age}d ago"
    else
      detail="no scheduled run of any conclusion in ${run_age}d against a ${label} cadence — the schedule has stopped firing"
    fi
    remedy="$REMEDY_DEAD"
  elif [ "$green_age" -lt 0 ]; then
    verdict="FAIL"
    detail="the schedule is firing (last run ${run_age}d ago) but the last green scheduled run's timestamp could not be parsed — cannot verify the control is passing"
    remedy="$REMEDY_FAIL"
  elif [ "$green_age" -ge "$n_days" ]; then
    if [ "$green" -eq 0 ]; then
      detail="the schedule is firing (last run ${run_age}d ago) and has never produced a green scheduled run in ${green_age}d"
    else
      detail="the schedule is firing (last run ${run_age}d ago) but has produced no green scheduled run in ${green_age}d"
    fi
# Q3: is red this control's way of reporting? Only if declared, and only
# for a run that got far enough to report. Here the newest decisive run is
# necessarily red, so last_conclusion is the newest RED one.
    if [ "$declared" -eq 1 ] && [ "$last_conclusion" = "failure" ]; then
      verdict="ALARM"
      detail="${detail} — and this workflow declares that failing is how it reports, in its step \"${declared_step}\", so those reds are findings and not a broken control"
      remedy="$REMEDY_ALARM"
    else
      verdict="FAIL"
      remedy="$REMEDY_FAIL"
      if [ "$declared" -eq 1 ]; then
        detail="${detail}. It declares that failing is how it reports, but its newest scheduled run concluded ${last_conclusion} — a run that never executed a step rendered no report, so the declaration does not apply and the plumbing is what is broken"
      fi
    fi
  fi

# A disabled schedule is the purest stopped control; no declaration
# reaches it (point 2 in the header).
  if [ "$state" != "active" ]; then
    verdict="DEAD"
    detail="the workflow is ${state} — its schedule cannot fire"
    remedy="$REMEDY_DEAD"
  fi

  line="$(printf '  %-5s %-26s %-8s N=%-3s green %s/%s scheduled  %s' \
    "$verdict" "$base" "$label" "${n_days}d" "$green" "$total" "$since")"
  report="${report}${line}"$'\n'
  case "$verdict" in
    DEAD)
      dead=$((dead + 1))
      dead_list="${dead_list}${dead_list:+,}${base}"
      ;;
    FAIL)
      failing=$((failing + 1))
      failing_list="${failing_list}${failing_list:+,}${base}"
      ;;
    ALARM)
      alarming=$((alarming + 1))
      alarming_list="${alarming_list}${alarming_list:+,}${base}"
      ;;
  esac
# A void marker is reported on every row, flagged or not: on a live
# control it is the only warning before it would matter.
  if [ -n "$decl_note" ]; then
    report="${report}        └─ ${decl_note}"$'\n'
  fi
  if [ "$verdict" != "live" ]; then
    report="${report}        └─ ${detail}"$'\n'
    report="${report}           remedy: ${remedy}"$'\n'
  fi
done
shopt -u nullglob

# ── Verdict ─────────────────────────────────────────────────────────
printf '%s' "$report"
echo "scheduled-controls: assessed ${assessed} of ${requested} scheduled workflow(s) in ${WORKFLOW_DIR}; ${dead} dead, ${failing} failing, ${alarming} alarming, ${unknown} unreadable, ${excluded} excluded (self)."
echo "scheduled-controls: thresholds GRACE_RUNS=${GRACE_RUNS} FLOOR_DAYS=${FLOOR_DAYS} CAP_DAYS=${CAP_DAYS}."
echo "scheduled-controls-dead: ${dead_list}"
echo "scheduled-controls-failing: ${failing_list}"
echo "scheduled-controls-alarming: ${alarming_list}"

# Anti-vacuity: a broken parser or moved directory must fail, not report a
# clean sweep over nothing.
if [ "$requested" -eq 0 ]; then
  echo "scheduled-controls: found NO workflow with a schedule: trigger in ${WORKFLOW_DIR}. The cron enumerator or the directory is wrong — this is not a clean result." >&2
  exit 2
fi
if [ "$assessed" -eq 0 ]; then
# Require assessed >= 1 even when the only scheduled workflow is the
# self-excluded detector, else a clean pass covers zero real checks.
  echo "scheduled-controls: $(( requested - excluded )) scheduled workflow(s) found but none could be read from the API (assessed=0). The gate did not run." >&2
  exit 2
fi

# Three classes, three remedies, one exit code: all mean a control's output
# is not reaching anybody (see BOTH EXIT 1 and WHY THE MARKER CANNOT HIDE A
# REAL FAILURE in the header).
if [ "$dead" -gt 0 ] || [ "$failing" -gt 0 ] || [ "$alarming" -gt 0 ]; then
  if [ "$dead" -gt 0 ]; then
    echo "scheduled-controls: ${dead} scheduled control(s) have STOPPED BEING SCHEDULED — no scheduled run at all within their threshold, so nothing is checking what they guard. Re-arm the schedule: ${dead_list}" >&2
  fi
  if [ "$failing" -gt 0 ]; then
    echo "scheduled-controls: ${failing} scheduled control(s) are STILL BEING SCHEDULED and have been red past their threshold — they are reporting and the report is not being acted on. Read the verdict, do not re-arm anything: ${failing_list}" >&2
  fi
  if [ "$alarming" -gt 0 ]; then
    echo "scheduled-controls: ${alarming} scheduled control(s) are REPORTING A FINDING — they declare that failing is how they report, they are firing on schedule, and they have been red past their threshold. Open what they found; the control itself is not broken and re-running it will not clear this: ${alarming_list}" >&2
  fi
  exit 1
fi

# "Could not check" is not "passed": a dead control hides exactly in the
# unreadable bucket.
if [ "$unknown" -gt 0 ]; then
  echo "scheduled-controls: ${unknown} scheduled workflow(s) could not be read from the API after 3 attempts each, so this sweep did not assess them. Not reporting a clean pass over controls that were never checked." >&2
  exit 1
fi

echo "scheduled-controls: every scheduled control is still being scheduled and has passed within its threshold."
exit 0
