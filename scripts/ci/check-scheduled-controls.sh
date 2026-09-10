#!/usr/bin/env bash
# check-scheduled-controls.sh — the scheduled-control detector
# (#496, #502). It no longer detects only DEAD controls; the GitHub
# label it feeds is still `dead-control`.
#
# A scheduled workflow is a CONTROL: it exists to notice something and
# say so. When one stops working, nothing says so — its silence reads
# exactly like "nothing to report". ansible-drift.yml is the recorded
# case: zero green scheduled runs from 2026-07-20 to 2026-09-07, seven
# weeks unnoticed, while a three-way divergence in the rollup statement
# cap (r1 7200 / template 600 / code 1800) sat behind it.
#
# THE RULE. A scheduled workflow that has produced no PASSING scheduled
# run for N days is not delivering signal. Two states satisfy that,
# both need a person, and they need OPPOSITE people doing opposite
# things — so the verdict says which:
#
#   DEAD — no scheduled run AT ALL within N days (or the workflow is
#     disabled, or its cron has never fired). The schedule stopped: a
#     disabled workflow, a lapsed credential, a billing cap, a cron
#     that no longer parses. NOBODY IS CHECKING. Remedy: re-arm the
#     schedule, then judge what it reports.
#
#   FAIL — the schedule is still firing on time and every run within N
#     days was red. SOMEBODY IS CHECKING AND THE ANSWER IS IGNORED.
#     Remedy: act on what the control found. Re-arming anything is a
#     category error, because nothing is un-armed.
#
# Until 2026-09-10 this script called both DEAD, which is how #502 came
# to say "a control has stopped reporting" about ansible-drift.yml — a
# control whose failing step is named "Drift verdict (fails on drift,
# and NAMES the tasks)", which had fired every Monday for eight weeks
# and named two drifted tasks each time (#496). It had not stopped
# reporting. It was reporting, and being read as broken.
#
# THE SIGNAL. Both questions come out of the same run history that was
# already being fetched. DEAD is the age of the newest scheduled run of
# any decisive conclusion; FAIL is the age of the newest GREEN one.
# Green runs are a subset of decisive runs, so last-run age is never
# greater than last-green age, and DEAD plus FAIL is exactly the set
# this script called DEAD before: the split re-labels, it does not
# narrow. Nothing that was detected stops being detected.
#
# BOTH EXIT 1. A control that fires faithfully and fails is working,
# and there is a real argument that it should not fail this gate. It is
# rejected, twice over. First, a chronically-red control is not
# delivering signal even while it runs: once red is its steady state
# the NEXT red — the new finding — is invisible, which is the same
# blindness this script exists to remove. Second, exit 0 would have
# ci-health.yml close the tracking issue and go green, leaving an
# unactioned weekly alarm traceable only in a log nobody reads; that is
# the "expected to fail" exclusion list this script refuses, reached by
# another route. Nothing in this repo is expected to fail. What the
# split buys is not a quieter gate but a gate that names the right
# remedy — and a genuinely stopped control that can no longer hide
# behind "oh, that one always fails", because it is now a different
# word on a different line.
#
# A THIRD STATE, DECLARED AND NOT INFERRED (2026-09-10). FAIL as written
# above reads "somebody is checking and the answer is being ignored",
# and for almost every control that is the right sentence. For one shape
# it is a mislabel of its own. ansible-drift.yml REPORTS BY FAILING: its
# verdict step is named "Drift verdict (fails on drift, and NAMES the
# tasks)", and a red run there is not a broken control, it is the
# control speaking. Filing that under FAIL sends the reader to the
# workflow's plumbing when what they should open is the drift report —
# the same wrong-remedy mistake #502 made one level up. So the split is
# only half done until the third state is named:
#
#   ALARM — the schedule is firing, every run within N days was red, AND
#     the workflow declares that failing is how it reports. SOMEBODY IS
#     CHECKING AND THE ANSWER IS "YES, THERE IS A PROBLEM". Remedy: read
#     the finding. Nothing about the control needs fixing; it clears
#     when the thing it found is fixed.
#
# HOW A CONTROL DECLARES IT. One marker comment in the workflow file:
#
#     # scheduled-control: reports-by-failing "<name of the verdict step>"
#
# Not a list in this script, and not inferred. Inference — "does it have
# a step whose name contains 'verdict'?" — is a guess, and a guess that
# grows a workflow into a quieter class it never asked for is the worst
# possible direction for it to be wrong in. A list here is worse than it
# first looks: the person who writes a report-by-failing workflow is not
# the person who maintains this gate, so the declaration would live in a
# file that workflow's reviewer never opens; and a list keyed by file
# name rots silently the day the workflow is renamed. The marker sits in
# the file it describes, so adding one is a diff on that workflow, read
# by whoever reviews that workflow.
#
# WHY THE MARKER CANNOT HIDE A REAL FAILURE. The standing objection to
# any declaration is a workflow that is supposed to be green marking
# itself and going quiet. Four things stop it, and the first is the one
# that actually matters:
#
#   1. IT SILENCES NOTHING. ALARM exits 1, is counted, is named on its
#      own machine-readable line, holds the dead-control issue open and
#      fails ci-health.yml — everything FAIL does. The SET of controls
#      this gate flags is byte-for-byte what it would be with no marker
#      anywhere in the tree; only the sentence changes. There is no
#      quieter state to move to, so there is nothing to hide behind, and
#      a marker added in bad faith buys its author nothing at all.
#   2. IT CANNOT REACH DEAD. The marker only ever moves a verdict
#      between FAIL and ALARM. A control that has stopped being
#      scheduled is DEAD whatever it declares — that is the failure this
#      whole gate exists for, and no file gets to opt out of it.
#   3. THE CLAIM IS CHECKED AGAINST THE RUN. "Failing is how I report"
#      is only true of a run that got far enough to report anything. A
#      startup_failure never executed a step; a timed_out run was killed
#      mid-flight and its verdict step may never have been reached.
#      Neither rendered a report, so when the newest scheduled run
#      concluded either way the marker does not apply and the control
#      stays FAIL — at that point the plumbing IS the problem, which is
#      exactly the failure a marker must not be able to launder. This is
#      the inverse of the caveat below: green is only evidence if the
#      control rendered a verdict, and so is red.
#   4. THE MARKER IS CHECKED AGAINST THE FILE. It must name a step that
#      exists in that workflow, matched in full and not as a substring.
#      A marker naming a step that is not there is void here — the
#      control falls back to the stricter FAIL — and is a hard failure
#      of check-scheduled-controls-test.sh, which sweeps the real
#      workflow tree on every PR with no API and no token. A marker
#      therefore cannot outlive the step whose existence is the whole
#      basis of its claim.
#
# WHY SCHEDULED RUNS ONLY. Manual dispatch masks a dead schedule. Of
# ansible-drift.yml's 38 runs on 2026-09-07, 31 were workflow_dispatch
# and several were green; the newest run in its history was a green
# dispatch, so any check that reads "the latest run" — as
# scripts/ci/check-main-ci-health.sh does — called it healthy while
# every one of its 8 scheduled runs had failed. Only a scheduled run
# is evidence that the SCHEDULE works.
#
# A CAVEAT THIS CANNOT SEE. Green is only evidence if green means the
# control rendered a verdict. ansible-drift.yml used to skip its
# verdict step entirely in apply mode, so an apply run was green
# without checking anything (run 33418645334, 2026-08-31). That is
# fixed at the source — the workflow now re-runs the dry-run after an
# apply and always renders a verdict — because no generic reader of
# run history can tell a skipped step from a passing one cheaply.
#
# N IS DERIVED FROM THE CADENCE, not fixed. A weekly control judged by
# a daily control's clock is either paged for one flake or blind for a
# month. N = GRACE_RUNS fires' worth of wall-clock, clamped:
#
#     N_days = clamp(ceil(GRACE_RUNS * interval_hours / 24),
#                    FLOOR_DAYS, CAP_DAYS)
#
#   GRACE_RUNS=3   three consecutive chances to go green before the
#                  control is flagged: one flake, one bad week, then it
#                  is a pattern. Constant in OPPORTUNITIES, so the
#                  2-hourly and the weekly job are held to the same
#                  standard rather than the same clock. The same N
#                  governs both questions above — one policy, one set
#                  of knobs, rather than a second clock for firing.
#   FLOOR_DAYS=7   below a week a runner outage or a long weekend
#                  pages; and for a high-cadence control seven days of
#                  unbroken red is already unambiguous (a 2-hourly job
#                  has had 84 chances).
#   CAP_DAYS=35    no control, however infrequent, goes five weeks
#                  unexamined. Sized against the failure this exists to
#                  prevent: ansible-drift went seven.
#
# Yielding, for the crons in .github/workflows today: every-2-hours and
# daily → 7 days; weekly → 21 days; a monthly cron would be 35.
#
# THE ONLY EXCLUSION is self-reference: the workflow file this run
# belongs to (SELF_WORKFLOW_FILE). A detector that fails because it
# found a flagged control is itself scheduled-and-red, so it would
# otherwise report ITSELF as FAIL on the next run and the alarm would
# sustain itself. There is no opt-out list and adding one would defeat
# the purpose. The reports-by-failing marker above is not one: it moves
# a control between two flagged classes and cannot move it out of any,
# which is why it can be granted to a workflow by its own author without
# anybody having to trust that author's judgement about being green.
#
# Environment:
#   GH_REPO          owner/repo (gh reads this automatically in Actions)
#   WORKFLOW_DIR     directory of workflow YAML   (default .github/workflows)
#   GRACE_RUNS       fires of grace               (default 3)
#   FLOOR_DAYS       lower clamp on N             (default 7)
#   CAP_DAYS         upper clamp on N             (default 35)
#   SELF_WORKFLOW_FILE  basename to skip          (default ci-health.yml)
#   SCHEDULED_CONTROLS_FIXTURE  directory of <basename>.json files shaped
#                    like {"state":…,"created_at":…,"workflow_runs":[…]} —
#                    offline tests, no gh, no token.
#   NOW_EPOCH        override "now" for deterministic tests.
#
# Exit: 0 = every control live, 1 = at least one DEAD, FAIL or ALARM (or
#           a workflow that could not be read), 2 = the gate could not
#           run (no scheduled workflow found, or no workflow could be
#           read at all). Never a silent pass.
#
# Machine-readable, one line each, always emitted, empty when none:
#   scheduled-controls-dead:     <csv>  stopped being scheduled
#   scheduled-controls-failing:  <csv>  scheduled, and red past N
#   scheduled-controls-alarming: <csv>  scheduled, red past N, and red
#                                       is how this control reports
# .github/workflows/ci-health.yml reads ALL THREE, and the self-test asserts
# that parity: a class the caller does not read is a class that
# silently stops being tracked, which is this gate's own failure mode.
#
# Output is plain text with no ::error:: / ::warning:: workflow commands,
# matching check-main-ci-health.sh: the caller owns the annotation. The
# sweep runs on every ci-health trigger but only ACTS once a day, and a
# script-emitted annotation would post an error on every 2-hourly run of
# a job that concluded success.
set -euo pipefail

# GH_REPO is set for us inside GitHub Actions; outside it the API path
# becomes `repos//actions/workflows/...`, every read 404s, and the gate
# reports "10 scheduled workflow(s) found but none could be read from the
# API. The gate did not run." — which reads exactly like a dead control
# and is really an unrunnable checker. Derive it from the checkout when
# absent so the gate can be run by hand to verify a control, and leave
# the Actions path byte-identical.
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

# The two remedies, spelled out once. A stopped control and a failing
# control both need a person, but not the same person doing the same
# thing, and this report is the only place that distinction is read.
REMEDY_DEAD='nobody is checking what this guards. Re-arm the schedule — a disabled workflow, a lapsed credential, a billing cap — then judge what it reports.'
REMEDY_FAIL='this control IS reporting and its report is red. Act on what it found; re-arming nothing will help, because nothing is un-armed.'
REMEDY_ALARM='this control reports BY FAILING and it is failing, on schedule, as designed. Read the finding it named — do not re-arm it and do not re-run it. It goes green when the thing it found is fixed.'
FIXTURE="${SCHEDULED_CONTROLS_FIXTURE:-}"
NOW_EPOCH="${NOW_EPOCH:-$(date -u +%s)}"

# ── Portable ISO-8601 → epoch (GNU date and BSD/macOS date differ) ──
# Mirrors check-main-ci-health.sh's to_epoch. Trims a fractional part
# and a numeric offset, both of which the workflows API can emit.
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

# ── Cron enumeration ────────────────────────────────────────────────
# Reads the `schedule:` block of a workflow's top-level `on:` mapping
# and prints one cron expression per line. A hardcoded workflow list
# would leave every future scheduled workflow uncovered, which is the
# same blind spot one layer up.
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

# ── Cadence ─────────────────────────────────────────────────────────
# Approximate hours between fires of one 5-field cron expression.
# Deliberately approximate: it feeds a clamped multiple-of-3 threshold,
# so being one fire out never changes the verdict.
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

# ── The reports-by-failing declaration ──────────────────────────────
# Read out of the workflow file itself, for the reasons in the header.
# Two halves, because a declaration that is not checkable is a comment:
# the marker names a step, and the step must be there.

# <workflow file> → the step name the marker claims, empty when unmarked.
# The value is captured whole and sliced afterwards rather than piped
# into an early-exit consumer (scripts/ci/lint-shell-sigpipe.sh).
reporting_step_of() {
  local hits
  hits="$(sed -n 's/^[[:space:]]*#[[:space:]]*scheduled-control:[[:space:]]*reports-by-failing[[:space:]]*"\(.*\)"[[:space:]]*$/\1/p' "$1" 2>/dev/null || true)"
  printf '%s' "${hits%%$'\n'*}"
}

# <workflow file> <step name> → 0 when a real step carries exactly that
# name. Exact, not substring: a marker that matched a prefix could be
# satisfied by an unrelated step, and the point of naming the step is
# that deleting it invalidates the claim. Comment lines are skipped so
# the marker cannot satisfy itself.
declares_step() {
  awk -v want="$2" '
    {
      line = $0
      sub(/^[[:space:]]+/, "", line)
      sub(/^-[[:space:]]*/, "", line)
      if (line ~ /^#/) next
      if (line !~ /^name[[:space:]]*:/) next
      sub(/^name[[:space:]]*:[[:space:]]*/, "", line)
      sub(/[[:space:]]+$/, "", line)
      gsub(/^["'"'"']|["'"'"']$/, "", line)
      if (line == want) found = 1
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

# ── Run history ─────────────────────────────────────────────────────
# Same shape as check-main-ci-health.sh: only conclusions that are
# unambiguously green or red carry signal; cancelled / skipped /
# neutral runs are dropped so neither can manufacture or mask a verdict.
# They are dropped from the is-it-still-firing signal too, so a control
# whose every scheduled run was cancelled reads as not firing and lands
# DEAD rather than FAIL. That is the conservative direction: it errs
# toward the louder verdict, and a run that was cancelled before it
# concluded did not check anything either.
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
  # Retried: a sweep issues two calls per workflow in a tight burst, which
  # trips GitHub's secondary rate limit intermittently. An unretried blip
  # lands the workflow in the unreadable bucket, and an unreadable workflow
  # is one this gate did not actually check.
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

  # The declaration, resolved once per workflow and validated against
  # the file that carries it. A marker whose step is gone is VOID, not
  # trusted: the control falls back to the stricter class. That fallback
  # is silent-by-design here — check-scheduled-controls-test.sh sweeps
  # the real workflow tree on every PR and fails loudly on it, which is
  # where a broken marker is cheap to catch — but the report still says
  # so on the row, because a mechanism that stops working without a word
  # is this gate's own failure mode.
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
    # Newest-first, which is how the runs API orders and how the
    # fixtures are written: the first row of a kind is its most recent.
    # The newest run's CONCLUSION is kept as well as its date, because a
    # reports-by-failing marker only applies to a run that executed a
    # step (tooth 3 in the header).
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

  # Q1 — IS IT STILL BEING SCHEDULED? The age of the newest scheduled
  # run of any decisive conclusion. With no run at all the clock starts
  # at registration, so a workflow merged yesterday is not judged for
  # not having fired yet.
  if [ -n "$last_run" ]; then
    run_age="$(days_since "$last_run")"
    fired="last scheduled run ${last_run%%T*} (${run_age}d ago)"
  else
    run_age="$(days_since "$created")"
    fired="last scheduled run: never — the cron has not fired since the workflow was registered ${created%%T*} (${run_age}d ago)"
  fi

  # Q2 — IS IT PASSING? The age of the newest GREEN scheduled run, on
  # the same registration clock when there has never been one.
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

  # Q1 first: a no there makes Q2 unanswerable rather than negative —
  # a control that is not running has not failed, it has stopped.
  verdict="live"
  detail=""
  remedy=""
  if [ "$run_age" -ge "$n_days" ]; then
    verdict="DEAD"
    if [ "$total" -eq 0 ]; then
      detail="the cron has not fired once since the workflow was registered ${run_age}d ago"
    else
      detail="no scheduled run of any conclusion in ${run_age}d against a ${label} cadence — the schedule has stopped firing"
    fi
    remedy="$REMEDY_DEAD"
  elif [ "$green_age" -ge "$n_days" ]; then
    if [ "$green" -eq 0 ]; then
      detail="the schedule is firing (last run ${run_age}d ago) and has never produced a green scheduled run in ${green_age}d"
    else
      detail="the schedule is firing (last run ${run_age}d ago) but has produced no green scheduled run in ${green_age}d"
    fi
    # Q3 — IS RED THIS CONTROL'S WAY OF REPORTING? Only if it says so,
    # and only for a run that got far enough to say anything. In this
    # branch the newest decisive run is necessarily red: run_age is
    # under N and green_age is not, so a green newest run is arithmetic
    # nonsense. So the conclusion tested here is the newest RED one.
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

  # A disabled schedule is the purest stopped control: it cannot fire
  # at all, however recently it last did — and no declaration reaches
  # this, which is tooth 2 in the header: a marker moves a verdict
  # between FAIL and ALARM and can never move one out of DEAD.
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
  # A void marker is reported on every row that carries one, flagged or
  # not: on a live control it is the only chance to see it before the
  # day it would have mattered.
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

# Anti-vacuity: a parser that stops matching, or a directory that has
# moved, must fail — not report a clean sweep over nothing.
if [ "$requested" -eq 0 ]; then
  echo "scheduled-controls: found NO workflow with a schedule: trigger in ${WORKFLOW_DIR}. The cron enumerator or the directory is wrong — this is not a clean result." >&2
  exit 2
fi
if [ "$assessed" -eq 0 ] && [ "$requested" -gt "$excluded" ]; then
  echo "scheduled-controls: $(( requested - excluded )) scheduled workflow(s) found but none could be read from the API. The gate did not run." >&2
  exit 2
fi

# Three classes, three sentences, three remedies — and one exit code,
# because all three mean a control's output is not reaching anybody. See
# BOTH EXIT 1 in the header for why a faithfully-failing control still
# fails here, and WHY THE MARKER CANNOT HIDE A REAL FAILURE for why the
# declared class is no quieter than the undeclared one.
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

# "Could not check" is not "passed". A sweep that read only some of its
# controls has no basis for a clean verdict, and the one dead control is
# exactly what hides in the unreadable bucket — the failure this gate
# exists to prevent, turned on itself.
if [ "$unknown" -gt 0 ]; then
  echo "scheduled-controls: ${unknown} scheduled workflow(s) could not be read from the API after 3 attempts each, so this sweep did not assess them. Not reporting a clean pass over controls that were never checked." >&2
  exit 1
fi

echo "scheduled-controls: every scheduled control is still being scheduled and has passed within its threshold."
exit 0
