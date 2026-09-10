#!/usr/bin/env bash
# check-scheduled-controls.sh — the dead-control detector (#496).
#
# A scheduled workflow is a CONTROL: it exists to notice something and
# say so. When one stops working, nothing says so — its silence reads
# exactly like "nothing to report". ansible-drift.yml is the recorded
# case: zero green scheduled runs from 2026-07-20 to 2026-09-07, seven
# weeks unnoticed, while a three-way divergence in the rollup statement
# cap (r1 7200 / template 600 / code 1800) sat behind it.
#
# THE RULE. A scheduled workflow that has produced no PASSING scheduled
# run for N days is not delivering signal. Two states satisfy that and
# both need a person:
#
#   - the control is broken (credentials lapsed, cron disabled, the
#     runner cannot reach the host), or
#   - the condition it guards has been unaddressed for N days, so the
#     alarm has been continuously on and nobody has acted.
#
# The report never claims which; it names the workflow, its green/total
# scheduled ratio and the date of its last green scheduled run, and
# leaves the diagnosis to the reader. That is why there is no
# "expected to fail" exclusion list — nothing in this repo is expected
# to fail, and a permanently-red control is a defect in either reading.
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
#                  control is called dead: one flake, one bad week,
#                  then it is a pattern. Constant in OPPORTUNITIES, so
#                  the 2-hourly and the weekly job are held to the
#                  same standard rather than the same clock.
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
# found a dead control would otherwise report ITSELF as a dead control
# on the next run, and the alarm would sustain itself. There is no
# opt-out list and adding one would defeat the purpose.
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
# Exit: 0 = every control live, 1 = at least one dead,
#       2 = the gate could not run (no scheduled workflow found, or no
#           workflow could be read). Never a silent pass.
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
unknown=0
excluded=0
dead_list=""
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

  runs="$(workflow_runs "$base" || true)"
  total=0
  green=0
  last_green=""
  while IFS=$'\t' read -r conclusion created_at; do
    [ -z "$conclusion" ] && continue
    total=$((total + 1))
    if [ "$conclusion" = "success" ]; then
      green=$((green + 1))
      [ -z "$last_green" ] && last_green="$created_at"
    fi
  done <<EOF
$runs
EOF

  verdict="live"
  detail=""
  if [ -n "$last_green" ]; then
    age="$(days_since "$last_green")"
    since="last green scheduled run ${last_green%%T*} (${age}d ago)"
    if [ "$age" -ge "$n_days" ]; then verdict="DEAD"; detail="no green scheduled run in ${age}d"; fi
  else
    age="$(days_since "$created")"
    if [ "$total" -eq 0 ]; then
      since="last green scheduled run: never — the cron has not fired since the workflow was registered ${created%%T*} (${age}d ago)"
    else
      since="last green scheduled run: never — all ${total} scheduled runs since ${created%%T*} (${age}d ago) failed"
    fi
    if [ "$age" -ge "$n_days" ]; then verdict="DEAD"; detail="never produced a green scheduled run in ${age}d"; fi
  fi

  # A disabled schedule is the purest dead control: it cannot fire at all.
  if [ "$state" != "active" ]; then
    verdict="DEAD"
    detail="the workflow is ${state} — its schedule cannot fire"
  fi

  line="$(printf '  %-5s %-26s %-8s N=%-3s green %s/%s scheduled  %s' \
    "$verdict" "$base" "$label" "${n_days}d" "$green" "$total" "$since")"
  report="${report}${line}"$'\n'
  if [ "$verdict" = "DEAD" ]; then
    dead=$((dead + 1))
    dead_list="${dead_list}${dead_list:+,}${base}"
    report="${report}        └─ ${detail}"$'\n'
  fi
done
shopt -u nullglob

# ── Verdict ─────────────────────────────────────────────────────────
printf '%s' "$report"
echo "scheduled-controls: assessed ${assessed} of ${requested} scheduled workflow(s) in ${WORKFLOW_DIR}; ${dead} dead, ${unknown} unreadable, ${excluded} excluded (self)."
echo "scheduled-controls: thresholds GRACE_RUNS=${GRACE_RUNS} FLOOR_DAYS=${FLOOR_DAYS} CAP_DAYS=${CAP_DAYS}."
echo "scheduled-controls-dead: ${dead_list}"

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

if [ "$dead" -gt 0 ]; then
  echo "scheduled-controls: ${dead} scheduled control(s) have produced no passing scheduled run past their threshold: ${dead_list}" >&2
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

echo "scheduled-controls: every scheduled control has passed within its threshold."
exit 0
