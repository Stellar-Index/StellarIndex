#!/usr/bin/env bash
# lint-served-value-cadence.sh — the served-value truth harness must be
# SCHEDULED, and its alert thresholds calibrated to that schedule.
#
# `stellarindex-ops verify-served-values` is the only check on the served
# NUMBER rather than the pipe. It writes stellarindex_served_value_{ok,skipped,
# last_run_unix} to the node_exporter textfile collector; three alerts read them.
#
# Why: the tool, alerts and runbook shipped and nothing ever ran it (no unit,
# timer, task or cron). The alerts selected a series that never existed, which
# is silent by construction: `time() - <absent series>` is an empty vector.
# lint-metric-refs.sh only asks whether a metric has an emitter, not whether
# anything runs it.
#
# Assertions, in order:
#   1. the service template invokes the subcommand and writes its textfile
#      into the collector directory node_exporter scrapes;
#   2. the timer template exists and drives that service;
#   3. ansible INSTALLS both and enables the timer, proven by task SHAPE, not
#      a mention of the unit name (a removal block mentions it just as well,
#      and this role carries removal blocks for other ops jobs);
#   4. the units are enabled on the network they are correct for, so flipping
#      one default cannot silently unschedule the harness;
#   5. the timer period is calibrated to the staleness threshold the alert
#      carries, in BOTH rule trees:
#          period + SLACK  <=  threshold  <=  2 * period
#      Lower: an on-cadence run must never trip stale. Upper: at most two
#      missed cycles before the ticket fires ("has not completed in 2 days").
#      This is lint-completeness-staleness.sh's contract with the upper bound
#      inclusive (it fails at `>= 2 * period`), which lets the 48h threshold sit
#      on a 24h timer.
#
# Not asserted: that the alert fires when the series never existed, or that
# the failed-unit ticket rides out one bad day. Both are behaviour, proven by
# promtool in deploy/monitoring/rule-tests/served-values_test.yml.
#
# The same assertions cover every harness in configure_job below.
#
# Usage: lint-served-value-cadence.sh [repo-root]
#   The root exists for scripts/ci/lint-served-value-cadence-test.sh (fixture
#   trees). CADENCE_JOBS (space-separated) narrows the job list.
# Exit 0 clean, non-zero on any violation.
set -euo pipefail
cd "${1:-$(dirname "$0")/../..}"

UNIT_DIR="configs/ansible/roles/archival-node/templates/systemd"
TASK_DIR="configs/ansible/roles/archival-node/tasks"
DEFAULTS="configs/ansible/roles/archival-node/defaults/main.yml"
COLLECTOR_DIR="/var/lib/node_exporter/textfile_collector"

# configure_job <job> — sets UNIT, SUBCMD, ENABLED_VAR, STALE_ALERT, RULE_FILES.
configure_job() {
  case "$1" in
    served-values)
      UNIT="verify-served-values"
      SUBCMD="stellarindex-ops verify-served-values"
      ENABLED_VAR="verify_served_values_enabled"
      STALE_ALERT="stellarindex_served_value_check_stale"
      RULE_FILES=(
        "deploy/monitoring/rules/data-freshness.yml"
        "configs/prometheus/rules.r1/data-freshness.yml"
      ) ;;
    supply-verify-rollup)
      UNIT="supply-verify-rollup"
      SUBCMD="stellarindex-ops supply verify-rollup"
      ENABLED_VAR="supply_verify_rollup_enabled"
      STALE_ALERT="stellarindex_supply_verify_rollup_stale"
      RULE_FILES=(
        "deploy/monitoring/rules/supply-verify-rollup.yml"
        "configs/prometheus/rules.r1/supply-verify-rollup.yml"
      ) ;;
    verify-lake)
      UNIT="verify-lake"
      SUBCMD="stellarindex-ops verify-lake"
      ENABLED_VAR=""
      STALE_ALERT="stellarindex_lake_verify_stale"
      RULE_FILES=(
        "deploy/monitoring/rules/storage.yml"
        "configs/prometheus/rules.r1/storage.yml"
      ) ;;
    verify-network-state)
      UNIT="verify-network-state"
      SUBCMD="stellarindex-ops verify-network-state"
      ENABLED_VAR=""
      STALE_ALERT="stellarindex_network_state_verify_stale"
      RULE_FILES=(
        "deploy/monitoring/rules/storage.yml"
        "configs/prometheus/rules.r1/storage.yml"
      ) ;;
    *) die "unknown job '$1' in CADENCE_JOBS" ;;
  esac
  SERVICE="$UNIT_DIR/$UNIT.service.j2"
  TIMER="$UNIT_DIR/$UNIT.timer.j2"
}

# enabled_expr_ok <job> <expr> — the network gate must resolve true on the
# host the job exists for, by a shape this gate can read without jinja.
enabled_expr_ok() {
  case "$1:$2" in
    served-values:*"== 'pubnet'"*|served-values:*'== "pubnet"'*) return 0 ;;
    *:true|*:'"true"') return 0 ;;
    # Installed wherever SEP-41 contracts are watched — the pipeline it checks.
    supply-verify-rollup:*"stellarindex_watched_sep41_contracts"*"length"*"> 0"*) return 0 ;;
  esac
  return 1
}

# 1h of grace above one period: RandomizedDelaySec plus the run itself.
PERIOD_SLACK_SECONDS=3600

die() { echo "lint-served-value-cadence: FAIL — $*" >&2; exit 1; }

check_job() {

# ── 1. the service runs the harness and writes where node_exporter reads ──

[ -f "$SERVICE" ] || die "no service template at $SERVICE.
  Its alerts read a textfile only this job writes; without a unit they
  select a series that never exists."

grep -q "$SUBCMD" "$SERVICE" \
  || die "$SERVICE does not invoke \`$SUBCMD\`."

grep -q -- "-textfile" "$SERVICE" \
  || die "$SERVICE does not pass -textfile.
  Without it the harness prints its gauges to stdout (the operator
  spot-run mode) and node_exporter has nothing to scrape."

grep -q "$COLLECTOR_DIR" "$SERVICE" \
  || die "$SERVICE does not write into $COLLECTOR_DIR.
  A .prom outside the collector directory is never scraped."

# ── 2. the timer drives that service ──────────────────────────────────────

[ -f "$TIMER" ] || die "no timer template at $TIMER.
  A service with no timer runs only when an operator remembers to, which
  is the state both alert layers shipped in."

grep -qx "Unit=$UNIT.service" "$TIMER" \
  || die "$TIMER does not declare Unit=$UNIT.service."

# ── 3. ansible RENDERS both units and ENABLES the timer ───────────────────

[ -d "$TASK_DIR" ] || die "task directory not found: $TASK_DIR"

# Prove the task SHAPE, not a mention: a removal block names the units as
# readily as an install block. Split each task file into `- name:` blocks and
# require two shapes:
#   TEMPLATE — a task running ansible.builtin.template that names BOTH units
#              (rendered through a `loop:`, so the name is not in `dest:`);
#   ENABLE   — a task running ansible.builtin.systemd on the timer with
#              `enabled: true` AND `state: started`.
# bash 3.2 (macOS) has no mapfile: fill the array with a read loop, and
# refuse an empty task tree rather than give awk no files.
task_files=()
while IFS= read -r f; do task_files+=("$f"); done < <(
  find "$TASK_DIR" -type f \( -name '*.yml' -o -name '*.yaml' \) | sort)
[ "${#task_files[@]}" -gt 0 ] || die "no task files under $TASK_DIR.
  A gate with an empty subject set passes forever."

shapes=$(awk -v unit="$UNIT" '
    function flush() {
      if (has_template && has_service && has_timer) print "TEMPLATE"
      if (has_systemd && has_timer && has_enabled && has_started) print "ENABLE"
      has_template = has_systemd = has_service = has_timer = 0
      has_enabled = has_started = 0
    }
    FNR == 1 { flush() }
    {
      line = $0
      # Strip comments: a task documented as "replaces the old
      # verify-served-values.timer" must not read as one that installs it.
      sub(/^[[:space:]]*#.*$/, "", line)
      sub(/[[:space:]]#.*$/, "", line)
    }
    line ~ /^[[:space:]]*-[[:space:]]+name:/ { flush() }
    line ~ /ansible\.builtin\.template:/ { has_template = 1 }
    line ~ /ansible\.builtin\.systemd:/  { has_systemd = 1 }
    index(line, unit ".service") { has_service = 1 }
    index(line, unit ".timer")   { has_timer = 1 }
    line ~ /^[[:space:]]*enabled:[[:space:]]*(true|yes)[[:space:]]*$/ { has_enabled = 1 }
    line ~ /^[[:space:]]*state:[[:space:]]*started[[:space:]]*$/      { has_started = 1 }
    END { flush() }
  ' "${task_files[@]}" | sort -u)

case "$shapes" in
  *TEMPLATE*) ;;
  *) die "no task under $TASK_DIR RENDERS $UNIT.{service,timer}.
  Expected one ansible.builtin.template task naming both units. A template
  nothing renders is an ORPHAN: it reads as a deployed unit and is not one
  (deploy-systemd-authoritative.py's ORPHAN class). Naming the units in a
  removal task does not count — that is the opposite change." ;;
esac

case "$shapes" in
  *ENABLE*) ;;
  *) die "no task under $TASK_DIR enables $UNIT.timer.
  Expected an ansible.builtin.systemd task on that timer carrying both
  \`enabled: true\` and \`state: started\`. A unit file on disk with no
  enabled timer runs exactly as often as no unit file at all." ;;
esac

# TAGGED — every block naming the units must carry `tags: [ops-jobs]`: the
# operator apply is `--tags ops-jobs`, so an untagged block is rendered by
# nothing. A lost tag once let the confirm step read green off a sibling unit.
untagged=$(awk -v unit="$UNIT" '
  function flush() { if (index(blk, unit) && blk !~ /tags:[[:space:]]*\[[^]]*ops-jobs/) bad++; blk="" }
  FNR == 1 { flush() }
  /^[[:space:]]*#/ { next }
  /^- name:/ { flush() }
  { blk = blk "\n" $0 }
  END { flush(); print bad+0 }
' "${task_files[@]}" 2>/dev/null || echo 1)
if [ "${untagged:-1}" -ne 0 ]; then
  die "$untagged task block(s) touching $UNIT lack tags: [ops-jobs].
  The documented apply is --tags ops-jobs, so an untagged block is rendered by
  nothing and the harness stays unscheduled while every other assertion passes."
fi

# ── 4. the harness is enabled where its ground truth is correct ───────────

# Both truth sources are pubnet authorities, so the install is network-gated;
# if that one line resolves false on pubnet the harness is unscheduled while
# assertions 1-3 stay green. A SHAPE check, not a jinja render: accepts an
# unconditional `true` or an `== 'pubnet'` comparison and refuses anything else.
# An empty ENABLED_VAR means installed unconditionally (assertion 3 is the proof).
if [ -n "$ENABLED_VAR" ]; then
[ -f "$DEFAULTS" ] || die "role defaults not found: $DEFAULTS"

enabled_expr=$(awk -v v="$ENABLED_VAR" '
  index($0, v ":") == 1 { sub(/^[^:]*:[[:space:]]*/, "", $0); print; exit }
' "$DEFAULTS")

[ -n "$enabled_expr" ] || die "$DEFAULTS does not define $ENABLED_VAR.
  The install block is gated on it, so an undefined variable is an
  unscheduled harness."

enabled_expr_ok "$job" "$enabled_expr" || die "$ENABLED_VAR in $DEFAULTS does not resolve true where $UNIT must run:
    $enabled_expr
  Flipping this one default unschedules the job with every other assertion
  still green."
fi

# ── 5. cadence vs. the staleness threshold the alert carries ──────────────

# Only the plain daily `*-*-* HH:MM:SS` form is understood; anything else is
# refused rather than guessed, since a mis-parsed period gives a bogus verdict.
on_calendar=$(awk -F= '/^OnCalendar=/ { sub(/^OnCalendar=/, "", $0); print; exit }' "$TIMER")
[ -n "$on_calendar" ] || die "$TIMER has no OnCalendar= line."

case "$on_calendar" in
  '*-*-* '[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*) period_seconds=86400 ;;
  *) die "unrecognised OnCalendar '$on_calendar' in $TIMER.
  This gate understands the daily \`*-*-* HH:MM:SS\` form only. If the
  cadence genuinely changed, extend the parser here and re-derive the
  bounds — do not delete the check." ;;
esac

lower=$(( period_seconds + PERIOD_SLACK_SECONDS ))
upper=$(( period_seconds * 2 ))

for rules in "${RULE_FILES[@]}"; do
  [ -f "$rules" ] || die "rule file not found: $rules"

  threshold=$(awk -v alert="$STALE_ALERT" '
    $0 ~ ("- alert: " alert "$") { inalert = 1; next }
    inalert && /^ *- alert:/                          { exit }
    inalert && /^ *for:/                              { exit }
    inalert && match($0, />[ ]*[0-9]+/) {
      s = substr($0, RSTART, RLENGTH); gsub(/[^0-9]/, "", s); print s; exit
    }
  ' "$rules")

  [ -n "$threshold" ] || die "could not read the staleness threshold from
  $STALE_ALERT in $rules.
  The expr shape changed; fix the parser rather than dropping the gate."

  if [ "$threshold" -lt "$lower" ]; then
    die "$rules: staleness threshold ${threshold}s is below one timer period
  plus slack (${lower}s). A healthy on-cadence host would ticket every day."
  fi
  if [ "$threshold" -gt "$upper" ]; then
    die "$rules: staleness threshold ${threshold}s exceeds two timer periods
  (${upper}s). More than two missed runs would pass before the ticket fires,
  and the summary claims two days."
  fi
done

echo "lint-served-value-cadence: OK — $UNIT rendered, enabled and scheduled" \
     "every ${period_seconds}s; staleness threshold within [${lower}s, ${upper}s]" \
     "in ${#RULE_FILES[@]} rule tree(s)"
}

for job in ${CADENCE_JOBS:-served-values supply-verify-rollup verify-lake verify-network-state}; do
  configure_job "$job"
  check_job
done
