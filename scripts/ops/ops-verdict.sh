#!/usr/bin/env bash
# shellcheck shell=bash
# ops-verdict.sh — sourced helpers that refuse to call an operation
# successful when it did nothing.
#
# THE BUG CLASS. Four variants of one shape were found on 2026-09-07,
# all of them "an operation that succeeded at doing nothing":
#
#   1. Waiting on a `Type=oneshot` unit with
#        while systemctl is-active --quiet "$u"; do sleep 30; done
#      exits on iteration ZERO. `is-active` returns 0 only for
#      ActiveState=active|reloading, and a `RemainAfterExit=no` oneshot
#      is never `active` at any point in its life — it goes
#      inactive → activating → inactive. Measured on r1 the same day:
#      creators-rollup.service ran 11:32:26 → 11:48:29 CEST and its
#      ActiveEnterTimestamp is EMPTY (ActiveEnterTimestampMonotonic=0),
#      while every RemainAfterExit=yes oneshot on the host carries one.
#      The loop therefore falls through immediately and the caller reads
#      `systemctl show -p Result`, which still holds the PREVIOUS run's
#      value. A rollup cycle 23 windows into a 65-window walk reported
#      RESULT=success by quoting a result from hours earlier.
#   2. `Result` is not evidence that a unit ran. It is `success` by
#      default: apport-autoreport.service on r1 has never run since boot
#      (InactiveExitTimestampMonotonic=0) and reports
#      Result=success, ExecMainStatus=0.
#   3. A backgrounded verify.sh reported exit 0 twice while its
#      `ALL CHECKS PASSED` sentinel was absent — it had died on a
#      missing toolchain, and the sentinel is the only honest verdict.
#   4. An `alter_job` disarm matched zero rows and exited 0; an ansible
#      apply path skipped its own verdict step and counted as success.
#
# WHAT THESE HELPERS DO ABOUT IT. Each one requires POSITIVE evidence
# that work happened, and each reports a distinct exit code per outcome
# rather than folding "nothing happened" into an existing one:
#
#   wait_for_oneshot   the unit left activating AND its completion
#                      timestamp advanced past a baseline taken before
#                      the wait — otherwise it refuses to report at all.
#   gate_passed        the gate's own sentinel is present in its log,
#                      the expected number of times. Never an exit code.
#   require_output     the command produced at least N rows; silence is
#                      the failure.
#   require_affected   the psql command tag says at least N rows were
#                      touched, for statements that cannot RETURN.
#
# USAGE
#
#   . scripts/ops/ops-verdict.sh
#
#   base=$(oneshot_baseline creators-rollup.service)
#   systemctl start --no-block creators-rollup.service
#   wait_for_oneshot creators-rollup.service 3600 "$base"
#
#   gate_passed /tmp/verify.out "$GATE_SENTINEL_VERIFY"
#   require_output "disarm" 1 -- psql -tAq -f disarm.sql
#
# This file is SOURCED, so it deliberately sets no shell options: a
# `set -euo pipefail` here would silently change the caller's shell.
# Every function is written to be correct with or without them, and
# returns rather than exits so a caller can decide what a failure means.
#
# The systemctl to call is $OPS_VERDICT_SYSTEMCTL, so the same helper
# serves a local unit and a remote one:
#
#   OPS_VERDICT_SYSTEMCTL="ssh root@r1 systemctl" wait_for_oneshot …
#
# Self-test: bash scripts/ops/ops-verdict-test.sh

# ─── configuration ──────────────────────────────────────────────────
#
# `:=` rather than `=` so re-sourcing the library keeps an operator's
# override, and so `readonly` is never imposed on the caller's shell.

: "${OPS_VERDICT_SYSTEMCTL:=systemctl}"
# Default wait, in seconds. Generous on purpose: a wait helper whose
# own default is the thing that fails teaches operators to delete it.
# Measured on r1 2026-09-07 — creators-rollup 963 s, sponsors-rollup
# 704 s.
: "${OPS_VERDICT_TIMEOUT:=3600}"
: "${OPS_VERDICT_POLL:=5}"

# The gate sentinels this repo actually emits. Exported as names so a
# caller cannot misspell one into a grep that can never match — which
# is the same silent-pass shape the helper exists to refuse.
: "${GATE_SENTINEL_VERIFY:=ALL CHECKS PASSED}"          # scripts/dev/verify.sh
: "${GATE_SENTINEL_PREPUSH:=ALL REQUIRED CHECKS PASSED}" # scripts/dev/prepush.sh
: "${GATE_SENTINEL_CONTAINER:=ALL LINUX CHECKS PASSED}"  # docker/verify/entrypoint.sh

# ─── systemd plumbing ───────────────────────────────────────────────

# _ops_systemctl runs the configured systemctl. The command string is
# split into words so the remote form ("ssh root@r1 systemctl") works;
# unit names never contain spaces, so nothing is lost.
_ops_systemctl() {
  local -a cmd
  read -r -a cmd <<<"${OPS_VERDICT_SYSTEMCTL}"
  "${cmd[@]}" "$@"
}

# _ops_show prints the KEY=VALUE block for one unit.
#
# Deliberately NOT `--value`: with several -p flags systemd prints the
# values in ITS order, not the order asked for, so a positional read is
# wrong on a host with a different systemd. Parsing KEY= is order-free.
_ops_show() { # _ops_show <unit> <property>...
  local unit=$1
  shift
  local -a args=()
  local p
  for p in "$@"; do args+=(-p "$p"); done
  _ops_systemctl show "$unit" "${args[@]}" 2>/dev/null
}

# _ops_field extracts one KEY's value from a KEY=VALUE block. Returns 1
# when the key is absent, so a caller can tell "empty" from "missing".
_ops_field() { # _ops_field <block> <key>
  local blob=$1 key=$2 line
  while IFS= read -r line; do
    case $line in
      "$key="*)
        printf '%s\n' "${line#"$key"=}"
        return 0
        ;;
    esac
  done <<<"$blob"
  return 1
}

# oneshot_baseline prints the unit's completion token: the monotonic
# microsecond stamp of the moment it last finished. `0` means it has not
# completed once since boot.
#
# This is the value wait_for_oneshot compares against, and it must be
# read BEFORE the run is triggered. Nothing else on the unit
# distinguishes this run from the last one: ActiveState, Result and
# ExecMainStatus all survive a run and are all `success`-shaped on a
# unit that has never executed.
#
# The token is InactiveEnterTimestampMonotonic for a
# RemainAfterExit=no unit (which ends `inactive`) and
# ActiveEnterTimestampMonotonic for a RemainAfterExit=yes one (which
# ends `active` and would otherwise never satisfy the wait).
oneshot_baseline() { # oneshot_baseline <unit>
  local unit=${1:-}
  if [ -z "$unit" ]; then
    echo "oneshot_baseline: usage: oneshot_baseline <unit>" >&2
    return 4
  fi
  local blob remain
  blob=$(_ops_show "$unit" LoadState RemainAfterExit \
    InactiveEnterTimestampMonotonic ActiveEnterTimestampMonotonic)
  if [ -z "$blob" ]; then
    echo "oneshot_baseline: $unit — systemctl show returned nothing" >&2
    return 4
  fi
  remain=$(_ops_field "$blob" RemainAfterExit || echo no)
  if [ "$remain" = yes ]; then
    _ops_field "$blob" ActiveEnterTimestampMonotonic || echo 0
  else
    _ops_field "$blob" InactiveEnterTimestampMonotonic || echo 0
  fi
}

# wait_for_oneshot waits for a oneshot unit to finish and reports its
# result — but only when it can prove the result belongs to THIS
# invocation.
#
#   wait_for_oneshot <unit> [timeout_s] [baseline]
#
# `baseline` is an oneshot_baseline value taken before the run was
# triggered. Omitted, one is taken on entry, which is correct when the
# unit is already running or about to be started, and fails CLOSED
# (exit 3, refused) when the run had already finished — the safe
# direction, because the alternative is quoting a stale result.
#
# It waits on ActiveState, never on `systemctl is-active`: is-active
# exits 0 only for ActiveState=active (or reloading), and a
# RemainAfterExit=no oneshot is never in that state — 93 of 93 such
# units on r1 have ActiveEnterTimestampMonotonic=0 — so every loop
# conditioned on it exits immediately. See the header for the
# measurement.
#
# Exit codes are distinct per outcome — a new state never reuses an
# existing code, so a caller's `if` cannot inherit the wrong meaning:
#
#   0  a NEW run completed and Result=success
#   1  a NEW run completed and FAILED (Result / ExecMainStatus reported)
#   2  timed out: still activating when the timeout expired
#   3  REFUSED to report: the unit is idle but its completion stamp did
#      not advance past the baseline — a stale result, a run that never
#      started (a failed Condition), or a reboot mid-wait
#   4  usage or precondition error (no unit, not loaded, no systemctl)
wait_for_oneshot() { # wait_for_oneshot <unit> [timeout_s] [baseline]
  local unit=${1:-}
  local timeout=${2:-$OPS_VERDICT_TIMEOUT}
  local baseline=${3:-}

  if [ -z "$unit" ]; then
    echo "wait_for_oneshot: usage: wait_for_oneshot <unit> [timeout_s] [baseline]" >&2
    return 4
  fi
  case $timeout in
    '' | *[!0-9]*)
      echo "wait_for_oneshot: timeout '$timeout' is not a whole number of seconds" >&2
      return 4
      ;;
  esac

  local blob load remain
  blob=$(_ops_show "$unit" LoadState RemainAfterExit)
  if [ -z "$blob" ]; then
    echo "wait_for_oneshot: $unit — systemctl show returned nothing (is ${OPS_VERDICT_SYSTEMCTL} reachable?)" >&2
    return 4
  fi
  load=$(_ops_field "$blob" LoadState || echo "")
  if [ "$load" != loaded ]; then
    echo "wait_for_oneshot: $unit — LoadState=${load:-<absent>}, not loaded; nothing to wait for" >&2
    return 4
  fi
  remain=$(_ops_field "$blob" RemainAfterExit || echo no)

  local token=InactiveEnterTimestampMonotonic
  local -a terminal=(inactive failed)
  if [ "$remain" = yes ]; then
    token=ActiveEnterTimestampMonotonic
    terminal=(active failed)
  fi

  if [ -z "$baseline" ]; then
    baseline=$(oneshot_baseline "$unit") || return 4
  fi
  case $baseline in
    '' | *[!0-9]*)
      echo "wait_for_oneshot: $unit — baseline '$baseline' is not a monotonic microsecond count" >&2
      return 4
      ;;
  esac

  # The poll interval also drives the elapsed-time counter, so it is
  # floored at one second: a zero or non-numeric interval would leave
  # `waited` at 0 forever and turn the timeout into a spin.
  local poll=${OPS_VERDICT_POLL}
  case $poll in
    '' | *[!0-9]*) poll=5 ;;
  esac
  if [ "$poll" -lt 1 ]; then poll=1; fi

  local waited=0 state='' stamp=0 done_state=''
  while :; do
    blob=$(_ops_show "$unit" ActiveState SubState Result ExecMainStatus \
      ConditionResult "$token")
    state=$(_ops_field "$blob" ActiveState || echo "")
    stamp=$(_ops_field "$blob" "$token" || echo 0)
    # `case` rather than `[ … ] && …`: a trailing false test would end
    # the loop body non-zero and kill a caller running under `set -e`.
    done_state=''
    case " ${terminal[*]} " in
      *" $state "*) done_state=$state ;;
    esac
    # Terminal AND the stamp moved: this run is ours to report.
    if [ -n "$done_state" ] && [ "${stamp:-0}" -gt "$baseline" ]; then
      break
    fi
    if [ "$waited" -ge "$timeout" ]; then
      if [ -n "$done_state" ]; then
        # Idle, but the stamp never advanced. Reporting Result here is
        # exactly the defect: it would quote the previous run.
        echo "wait_for_oneshot: unit=$unit verdict=stale-result state=$state" \
          "$token=$stamp baseline=$baseline condition=$(_ops_field "$blob" ConditionResult || echo '?')" >&2
        echo "wait_for_oneshot: $unit never completed a NEW run in ${timeout}s — refusing to report a result from a previous one" >&2
        return 3
      fi
      echo "wait_for_oneshot: unit=$unit verdict=timeout state=$state waited=${waited}s timeout=${timeout}s" >&2
      return 2
    fi
    sleep "$poll"
    waited=$((waited + poll))
  done

  local result status
  result=$(_ops_field "$blob" Result || echo "")
  status=$(_ops_field "$blob" ExecMainStatus || echo "")
  if [ "$state" = failed ] || [ "$result" != success ]; then
    echo "wait_for_oneshot: unit=$unit verdict=failed state=$state result=${result:-?}" \
      "exec_status=${status:-?} waited=${waited}s" >&2
    return 1
  fi
  echo "wait_for_oneshot: unit=$unit verdict=success state=$state result=$result" \
    "exec_status=${status:-?} waited=${waited}s $token=$stamp"
  return 0
}

# ─── gate verdicts ──────────────────────────────────────────────────

# gate_passed asserts a gate's own sentinel is in its log, the expected
# number of times. It never looks at an exit code, because an exit code
# is what lied: a backgrounded verify.sh reported 0 twice with the
# sentinel absent.
#
#   gate_passed <logfile> <sentinel> [expected_count]
#
# expected_count defaults to 1 and MORE than that fails, because two
# scripts in this tree emit `ALL CHECKS PASSED` — scripts/dev/verify.sh
# (bare) and scripts/ci/site-crawl-check.sh (prefixed) — plus
# scripts/ops/dns-perimeter-check.sh. In a combined log a substring
# match cannot tell them apart, so an unexpected count is surfaced
# rather than accepted.
#
#   0  present exactly expected_count times
#   1  ABSENT — the gate never reached its verdict
#   2  the log is missing, unreadable or empty
#   3  present, but a different number of times than expected
gate_passed() { # gate_passed <logfile> <sentinel> [expected_count]
  local log=${1:-} sentinel=${2:-} want=${3:-1} n

  if [ -z "$log" ] || [ -z "$sentinel" ]; then
    echo "gate_passed: usage: gate_passed <logfile> <sentinel> [expected_count]" >&2
    return 2
  fi
  if [ ! -r "$log" ]; then
    echo "gate_passed: $log is missing or unreadable — the gate produced no log, which is not a pass" >&2
    return 2
  fi
  if [ ! -s "$log" ]; then
    echo "gate_passed: $log is EMPTY — the gate produced no output, which is not a pass" >&2
    return 2
  fi

  # grep -c on a FILE: no pipe, so no early-exit consumer and no
  # SIGPIPE (scripts/ci/lint-shell-sigpipe.sh). -c exits 1 on zero
  # matches, which is a count, not an error.
  n=$(grep -cF -- "$sentinel" "$log") || n=0

  if [ "$n" -eq 0 ]; then
    echo "gate_passed: $log does NOT contain '$sentinel' — the gate did not reach its verdict (exit code says nothing)" >&2
    return 1
  fi
  if [ "$n" -ne "$want" ]; then
    echo "gate_passed: $log contains '$sentinel' $n time(s), expected $want — another gate in this tree emits the same sentinel; name the count you expect" >&2
    return 3
  fi
  echo "gate_passed: $log — '$sentinel' present $n time(s) as expected"
  return 0
}

# ─── "it matched nothing" verdicts ──────────────────────────────────

# _ops_split_cmd consumes the "<args> -- <cmd...>" convention shared by
# require_output and require_affected, leaving the command in
# _OPS_CMD. Returns 1 when no `--` separator is present.
_ops_split_cmd() {
  _OPS_CMD=()
  local seen=0 a
  for a in "$@"; do
    if [ "$seen" -eq 1 ]; then
      _OPS_CMD+=("$a")
    elif [ "$a" = "--" ]; then
      seen=1
    fi
  done
  [ "$seen" -eq 1 ] && [ "${#_OPS_CMD[@]}" -gt 0 ]
}

# require_output runs a command and fails when it produced fewer than
# min_rows non-blank lines. Silence is the failure mode being hunted: a
# disarm that matched no job, a sweep that found no chunk, a reconcile
# that compared nothing all exit 0 with empty output.
#
#   require_output <label> <min_rows> -- <cmd...>
#
# It is a strict superset of the exit-code check — a command that FAILS
# also fails here — so replacing a bare invocation with this one can
# never weaken a caller. The command's own output is echoed through, and
# a self-accounting line is printed so a run that did nothing says so in
# its own log.
#
#   0  at least min_rows rows
#   1  fewer than min_rows rows (including zero)
#   2  the command itself failed
#   4  usage error
require_output() { # require_output <label> <min_rows> -- <cmd...>
  local label=${1:-} min=${2:-1}
  if [ -z "$label" ] || ! _ops_split_cmd "$@"; then
    echo "require_output: usage: require_output <label> <min_rows> -- <cmd...>" >&2
    return 4
  fi
  case $min in
    '' | *[!0-9]*)
      echo "require_output: min_rows '$min' is not a number" >&2
      return 4
      ;;
  esac

  local out rc=0 n
  out=$("${_OPS_CMD[@]}") || rc=$?
  if [ -n "$out" ]; then printf '%s\n' "$out"; fi
  if [ "$rc" -ne 0 ]; then
    echo "require_output: $label — command exited $rc" >&2
    return 2
  fi
  # Non-blank lines only: a trailing newline is not a row.
  n=$(printf '%s\n' "$out" | grep -c '[^[:space:]]') || n=0
  echo "require_output: $label — $n row(s) of a required minimum of $min"
  if [ "$n" -lt "$min" ]; then
    echo "require_output: $label matched NOTHING (or too little) and still exited 0 — that is the failure, not a pass" >&2
    return 1
  fi
  return 0
}

# require_affected is require_output for a statement that cannot
# RETURN its rows: it reads the psql COMMAND TAG (`UPDATE 3`,
# `DELETE 0`, `SELECT 0`, `INSERT 0 7`) and fails when the count is
# below min_rows.
#
#   require_affected <label> <min_rows> -- <psql cmd...>
#
# Prefer `RETURNING` + require_output where the statement allows it —
# a returned row names WHAT changed. This exists for the ones that do
# not, such as the `alter_job` disarm that matched zero rows and exited
# 0. Run psql WITHOUT -q so the tag is printed.
#
#   0  the last command tag reports at least min_rows
#   1  the tag reports fewer than min_rows
#   2  the command itself failed
#   3  no command tag in the output — nothing can be proven, so this is
#      a failure and not a pass
#   4  usage error
require_affected() { # require_affected <label> <min_rows> -- <cmd...>
  local label=${1:-} min=${2:-1}
  if [ -z "$label" ] || ! _ops_split_cmd "$@"; then
    echo "require_affected: usage: require_affected <label> <min_rows> -- <cmd...>" >&2
    return 4
  fi
  case $min in
    '' | *[!0-9]*)
      echo "require_affected: min_rows '$min' is not a number" >&2
      return 4
      ;;
  esac

  local out rc=0 line tag='' n=''
  out=$("${_OPS_CMD[@]}") || rc=$?
  if [ -n "$out" ]; then printf '%s\n' "$out"; fi
  if [ "$rc" -ne 0 ]; then
    echo "require_affected: $label — command exited $rc" >&2
    return 2
  fi
  # Last tag wins: a script of several statements is judged on the one
  # that ran last, which is the one the caller was waiting for.
  while IFS= read -r line; do
    case $line in
      'INSERT '[0-9]*' '[0-9]*)
        tag=$line
        n=${line##* }
        ;;
      'UPDATE '[0-9]* | 'DELETE '[0-9]* | 'SELECT '[0-9]* | 'MERGE '[0-9]* | 'COPY '[0-9]*)
        tag=$line
        n=${line##* }
        ;;
    esac
  done <<<"$out"

  if [ -z "$n" ]; then
    echo "require_affected: $label — no psql command tag in the output; run psql without -q so the tag is printed" >&2
    return 3
  fi
  echo "require_affected: $label — tag '$tag' reports $n row(s) of a required minimum of $min"
  if [ "$n" -lt "$min" ]; then
    echo "require_affected: $label affected $n row(s) and still exited 0 — that is the failure, not a pass" >&2
    return 1
  fi
  return 0
}
