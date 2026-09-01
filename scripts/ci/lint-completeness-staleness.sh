#!/usr/bin/env bash
# Cross-artifact guard: the served coverage-verdict staleness constants
# must stay CALIBRATED to the deployed completeness-audit cadence.
#
# internal/api/v1/coverage_verdicts.go hand-calibrates two constants that
# decide when GET /v1/coverage flags its verdict `stale` (the MNY-04 gate
# that stops "15/15 complete" reading healthy forever after the audit
# died):
#   - coverageVerdictStaleAge     (a Go time.Duration, e.g. 26 * time.Hour)
#   - coverageVerdictStaleLedgers (a ledger count, e.g. 34560)
# Both carry an in-code "CALIBRATED TO THE DEPLOYED CADENCE" note pinned
# to the DAILY completeness timer. Nothing cross-checked them: change the
# timer cadence (daily -> weekly) and the staleness flag silently either
# never trips or flaps on every normal cycle — recreating the exact
# failure class (MNY-04) the flag exists to prevent.
#
# The AUTHORITATIVE cadence is the r1-deployed, Ansible-rendered timer
# (docs/operations/maintainer-workflow.md: "r1 configuration is
# ansible-managed", enforced by
# ansible-drift.yml):
#   configs/ansible/roles/archival-node/templates/systemd/compute-completeness.timer.j2
# (templated + enabled in tasks/14-stellarindex-services.yml). NOTE:
# deploy/systemd/stellarindex-completeness.timer is a STALE, non-deployed
# reference file (still OnUnitActiveSec=1h from the retired hourly design;
# not installed by any Ansible task) and the code comment's reference to
# it is out of date — do NOT calibrate against it.
#
# This lint parses both constants and the timer's period and asserts:
#   AGE (hard, both directions):
#     timerPeriod + SLACK  <=  staleAge  <  2 * timerPeriod
#     - lower bound: a normal on-cadence run (a gap up to one period,
#       plus jitter/run-time) must NEVER trip stale;
#     - upper bound: a single GENUINELY-missed cycle MUST eventually trip
#       (staleAge under two periods), else a dead audit hides for cycles.
#   LEDGERS (hard, lower bound only — see fuzziness note below):
#     staleLedgers  >=  periodLedgers + LEDGER_SLACK
#     where periodLedgers = timerPeriod / ~5s-per-ledger (pubnet target).
#
# Fuzziness we deliberately DON'T assert: the ledger<->wall-clock link is
# approximate (pubnet close time drifts ~5-6s) and the two constants are
# calibrated to different horizons on purpose (age ~= 1 period + grace,
# ledgers ~= 2 periods per the in-code note), so we do NOT assert the
# ledger const equals the age const, nor an upper bound on it — only that
# it, too, comfortably exceeds one period so a cadence change can't leave
# it flapping/never-tripping. 5s/ledger is conservative for the lower
# bound (real close >=5s => fewer ledgers/period => easier to pass).
#
# Exit 0 clean, non-zero on any violation. Wired into CI's import-checks
# job alongside lint-i128 / lint-migrations.
set -euo pipefail
cd "$(dirname "$0")/../.."

GO_FILE="internal/api/v1/coverage_verdicts.go"
TIMER_FILE="configs/ansible/roles/archival-node/templates/systemd/compute-completeness.timer.j2"

# Tunables (seconds / ledgers).
AGE_SLACK_SECONDS=3600          # 1h grace above one period before "stale"
LEDGER_CLOSE_SECONDS=5          # pubnet target ledger-close (approx)
LEDGER_SLACK=$(( 3600 / LEDGER_CLOSE_SECONDS ))  # ~1h of ledgers

die() { echo "lint-completeness-staleness ❌ $*" >&2; exit 1; }

[[ -f "$GO_FILE" ]]    || die "constants file not found: $GO_FILE"
[[ -f "$TIMER_FILE" ]] || die "completeness timer template not found: $TIMER_FILE
  (the authoritative Ansible-deployed cadence). If it moved, update TIMER_FILE
  in $0 and re-derive the calibration in $GO_FILE."

# --- helpers -------------------------------------------------------------

# trim leading/trailing whitespace (parameter-expansion only; no sed).
trim() {
  local s="$1"
  s="${s#"${s%%[![:space:]]*}"}"
  s="${s%"${s##*[![:space:]]}"}"
  printf '%s' "$s"
}

# Extract the RHS of a single-line `const <name> [type] = <rhs>`.
extract_const_rhs() {
  local name="$1" file="$2" line rhs
  line=$(grep -E "^[[:space:]]*const[[:space:]]+${name}([[:space:]]+[A-Za-z0-9_.]+)?[[:space:]]*=" "$file" | head -n1 || true)
  [[ -n "$line" ]] || return 1
  rhs="${line#*=}"      # after first '='
  rhs="${rhs%%//*}"     # drop trailing // comment
  trim "$rhs"
}

# Evaluate a Go time.Duration literal (sum of `[coef *] time.{Hour,Minute,Second}`)
# to whole seconds. Fails on any unrecognised term.
go_duration_to_seconds() {
  local expr="$1" total=0 term coef unit
  expr="${expr// /}"          # strip spaces
  expr="${expr//+/ }"         # split points -> word boundaries
  for term in $expr; do
    [[ -n "$term" ]] || continue
    if   [[ "$term" == *time.Hour* ]];   then unit=3600
    elif [[ "$term" == *time.Minute* ]]; then unit=60
    elif [[ "$term" == *time.Second* ]]; then unit=1
    else echo "ERR unrecognised duration term: '$term'"; return 1; fi
    coef="${term//time.Hour/}"
    coef="${coef//time.Minute/}"
    coef="${coef//time.Second/}"
    coef="${coef//\*/}"
    [[ -n "$coef" ]] || coef=1
    [[ "$coef" =~ ^[0-9]+$ ]] || { echo "ERR bad coefficient in term: '$term'"; return 1; }
    total=$(( total + coef * unit ))
  done
  echo "$total"
}

# Parse a systemd time span (OnUnitActiveSec form: 1h / 30min / 1h30min / 90s / 1d / 1w).
systemd_span_to_seconds() {
  local s="$1" total=0 rest num unit mult
  s="${s// /}"
  rest="$s"
  while [[ "$rest" =~ ^([0-9]+)(us|ms|s|sec|second|seconds|min|m|minute|minutes|h|hr|hour|hours|d|day|days|w|week|weeks)(.*)$ ]]; do
    num="${BASH_REMATCH[1]}"; unit="${BASH_REMATCH[2]}"; rest="${BASH_REMATCH[3]}"
    case "$unit" in
      s|sec|second|seconds) mult=1 ;;
      min|m|minute|minutes) mult=60 ;;
      h|hr|hour|hours)      mult=3600 ;;
      d|day|days)           mult=86400 ;;
      w|week|weeks)         mult=604800 ;;
      *) echo "ERR span unit: '$unit'"; return 1 ;;
    esac
    total=$(( total + num * mult ))
  done
  [[ -z "$rest" ]] || { echo "ERR unparsed span remainder: '$rest'"; return 1; }
  echo "$total"
}

# Classify an OnCalendar= expression to a worst-case run PERIOD in seconds.
# Handles the systemd keywords, and the common explicit form
# `[WEEKDAY] Y-M-D H:M:S [TZ]`. Refuses (fail-closed) anything it can't
# cleanly classify so a human extends the lint rather than mis-calibrating.
oncalendar_to_seconds() {
  local cal="$1" low tok
  cal="$(trim "$cal")"
  low="$(printf '%s' "$cal" | tr '[:upper:]' '[:lower:]')"
  case "$low" in
    minutely)         echo 60;        return 0 ;;
    hourly)           echo 3600;      return 0 ;;
    daily)            echo 86400;     return 0 ;;
    weekly)           echo 604800;    return 0 ;;
    monthly)          echo 2592000;   return 0 ;;   # 30d, conservative
    quarterly)        echo 7776000;   return 0 ;;
    semiannually)     echo 15552000;  return 0 ;;
    yearly|annually)  echo 31536000;  return 0 ;;
  esac

  # Explicit calendar spec. Tokenise on whitespace; drop a trailing
  # timezone token (UTC, an Area/City zone, or a +/-HH:MM offset).
  local weekday="" datepart="" timepart=""
  for tok in $cal; do
    case "$tok" in
      UTC|utc) continue ;;
      */*)     continue ;;                 # Area/City tz e.g. Europe/Berlin
      [+-][0-9]*) continue ;;              # numeric offset
    esac
    if [[ "$tok" == *:* ]]; then
      timepart="$tok"
    elif [[ "$tok" == *-* ]]; then
      datepart="$tok"
    else
      weekday="$tok"                       # Mon / Tue / ... (or a list/range)
    fi
  done

  # A specific weekday (single day, no list/range) fires once per week.
  if [[ -n "$weekday" && "$weekday" != "*" ]]; then
    if [[ "$weekday" =~ ^(Mon|Tue|Wed|Thu|Fri|Sat|Sun|mon|tue|wed|thu|fri|sat|sun)$ ]]; then
      echo 604800; return 0
    fi
    echo "ERR unrecognised weekday field '$weekday' in OnCalendar='$cal' — extend the lint"; return 1
  fi

  # No weekday. Date must be the every-day wildcard for a sub-weekly cadence.
  if [[ "$datepart" != "*-*-*" ]]; then
    echo "ERR non-wildcard date '$datepart' in OnCalendar='$cal' (monthly/yearly?) — extend the lint"; return 1
  fi
  [[ -n "$timepart" ]] || { echo "ERR no time field in OnCalendar='$cal'"; return 1; }

  local hour="${timepart%%:*}"
  local rest="${timepart#*:}"
  local minute="${rest%%:*}"
  if [[ "$hour" == "*" && "$minute" == "*" ]]; then
    echo 60; return 0                      # every minute
  elif [[ "$hour" == "*" ]]; then
    echo 3600; return 0                    # every hour
  else
    echo 86400; return 0                   # once a day at a fixed hour
  fi
}

fmt_hms() {  # seconds -> "Xh Ym" for readable messages
  local s="$1"
  printf '%dh%dm' $(( s / 3600 )) $(( (s % 3600) / 60 ))
}

# --- parse constants -----------------------------------------------------

age_rhs=$(extract_const_rhs coverageVerdictStaleAge "$GO_FILE") \
  || die "could not find 'const coverageVerdictStaleAge' in $GO_FILE"
staleAge=$(go_duration_to_seconds "$age_rhs") \
  || die "could not parse coverageVerdictStaleAge = '$age_rhs' in $GO_FILE"

ledgers_rhs=$(extract_const_rhs coverageVerdictStaleLedgers "$GO_FILE") \
  || die "could not find 'const coverageVerdictStaleLedgers' in $GO_FILE"
[[ "$ledgers_rhs" =~ ^[0-9]+$ ]] \
  || die "coverageVerdictStaleLedgers = '$ledgers_rhs' is not a plain integer in $GO_FILE"
staleLedgers="$ledgers_rhs"

# --- parse timer period --------------------------------------------------

oncal=$(grep -E '^[[:space:]]*OnCalendar[[:space:]]*=' "$TIMER_FILE" | head -n1 | cut -d= -f2- || true)
onactive=$(grep -E '^[[:space:]]*OnUnitActiveSec[[:space:]]*=' "$TIMER_FILE" | head -n1 | cut -d= -f2- || true)

if [[ -n "$oncal" ]]; then
  cadence_src="OnCalendar=$(trim "$oncal")"
  timerPeriod=$(oncalendar_to_seconds "$(trim "$oncal")") \
    || die "unclassifiable OnCalendar in $TIMER_FILE: $(oncalendar_to_seconds "$(trim "$oncal")" 2>&1 || true)
  → extend oncalendar_to_seconds() in $0 to map this cadence to a period."
elif [[ -n "$onactive" ]]; then
  cadence_src="OnUnitActiveSec=$(trim "$onactive")"
  timerPeriod=$(systemd_span_to_seconds "$(trim "$onactive")") \
    || die "could not parse OnUnitActiveSec='$(trim "$onactive")' in $TIMER_FILE"
else
  die "no OnCalendar= or OnUnitActiveSec= found in $TIMER_FILE — cannot derive the audit cadence."
fi

[[ "$timerPeriod" -gt 0 ]] || die "derived a non-positive timer period ($timerPeriod s) from $TIMER_FILE"

periodLedgers=$(( timerPeriod / LEDGER_CLOSE_SECONDS ))

# --- assertions ----------------------------------------------------------

age_lo=$(( timerPeriod + AGE_SLACK_SECONDS ))
age_hi=$(( 2 * timerPeriod ))
ledger_lo=$(( periodLedgers + LEDGER_SLACK ))

fail=0

echo "lint-completeness-staleness: parsed"
echo "  coverageVerdictStaleAge     = ${age_rhs}  (= ${staleAge}s / $(fmt_hms "$staleAge"))"
echo "  coverageVerdictStaleLedgers = ${staleLedgers} ledgers"
echo "  timer cadence               = ${cadence_src}  (period = ${timerPeriod}s / $(fmt_hms "$timerPeriod"))"
echo "  from: $GO_FILE  and  $TIMER_FILE"

# AGE lower bound.
if [[ "$staleAge" -lt "$age_lo" ]]; then
  echo "lint-completeness-staleness ❌ coverageVerdictStaleAge ($(fmt_hms "$staleAge")) is NOT safely above the audit period." >&2
  echo "   The completeness timer runs every $(fmt_hms "$timerPeriod") (${cadence_src}, $TIMER_FILE)." >&2
  echo "   A normal on-cadence gap (up to one period + jitter/run-time) can reach or exceed the" >&2
  echo "   staleness threshold, so GET /v1/coverage would flag 'stale' on ordinary cycles (flapping)" >&2
  echo "   or the gate is otherwise mis-calibrated. Require staleAge >= period + $(fmt_hms "$AGE_SLACK_SECONDS") slack" >&2
  echo "   (>= ${age_lo}s). Fix coverageVerdictStaleAge in $GO_FILE (and the ledger twin) to match the cadence." >&2
  fail=1
fi

# AGE upper bound.
if [[ "$staleAge" -ge "$age_hi" ]]; then
  echo "lint-completeness-staleness ❌ coverageVerdictStaleAge ($(fmt_hms "$staleAge")) is >= two audit periods ($(fmt_hms "$age_hi"))." >&2
  echo "   With the timer at $(fmt_hms "$timerPeriod") (${cadence_src}, $TIMER_FILE), a genuinely-missed" >&2
  echo "   cycle would not surface as 'stale' for two or more cycles — the MNY-04 failure class this" >&2
  echo "   gate exists to prevent ('served complete forever after the audit died'). Lower" >&2
  echo "   coverageVerdictStaleAge in $GO_FILE to below 2 periods (< ${age_hi}s)." >&2
  fail=1
fi

# LEDGER lower bound (approximate; see header).
if [[ "$staleLedgers" -lt "$ledger_lo" ]]; then
  echo "lint-completeness-staleness ❌ coverageVerdictStaleLedgers (${staleLedgers}) is NOT safely above one audit period." >&2
  echo "   At ~${LEDGER_CLOSE_SECONDS}s/ledger the timer's $(fmt_hms "$timerPeriod") period is ~${periodLedgers} ledgers" >&2
  echo "   (${cadence_src}, $TIMER_FILE); the live-tip staleness gap must exceed that plus slack" >&2
  echo "   (>= ${ledger_lo} ledgers) or a normal cycle trips it / a cadence change leaves it mis-calibrated." >&2
  echo "   Fix coverageVerdictStaleLedgers in $GO_FILE to match the cadence." >&2
  fail=1
fi

if [[ "$fail" -eq 0 ]]; then
  echo "✅ completeness-staleness lint passed: staleAge ($(fmt_hms "$staleAge")) is within [period+slack, 2*period) = [$(fmt_hms "$age_lo"), $(fmt_hms "$age_hi")) and staleLedgers (${staleLedgers}) >= ${ledger_lo}."
fi
exit "$fail"
