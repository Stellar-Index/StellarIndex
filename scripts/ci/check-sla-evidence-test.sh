#!/usr/bin/env bash
# check-sla-evidence-test.sh — fixture tests for the SLA-evidence decision
# core (scripts/ci/check-sla-evidence.sh) plus the k6-weekly wiring it
# depends on. Issue #316.
#
# The defect: k6-weekly.yml's scheduled path exited 0 with a `::notice::`
# whenever the target secrets were unset, so four months of scheduled runs
# concluded `success` with every real step `skipped` and the SLA-evidence
# feed produced nothing. The workflow itself runs once a week and only
# against secrets that don't exist, so these fixtures are the ONLY place
# the verdict — and the wiring that consumes it — is exercised on a PR.
#
# Run: bash scripts/ci/check-sla-evidence-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
CHECK="$PWD/scripts/ci/check-sla-evidence.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

# Frozen clock so the fixtures are date-stable forever. Cases stay well
# clear of the exact threshold: BSD `date -j -f %Y-%m-%d` fills in the
# CURRENT time of day for unspecified fields while GNU `date -d` uses
# midnight, so a day-boundary case would be platform-dependent.
NOW_EPOCH=1787961600   # 2026-08-29T00:00:00Z

# run <fixture-dir> — invoke the decision core with the ambient
# K6_TARGET / STELLARINDEX_LOAD_API_KEY the caller exported.
run() {
  OUT="$(SLA_EVIDENCE_DIR="$1" SLA_EVIDENCE_NOW="$NOW_EPOCH" bash "$CHECK" 2>&1)"
  RC=$?
}

expect() {
  local name="$1" want_rc="$2" want_sub="${3:-}"
  if [ "$RC" -ne "$want_rc" ]; then
    echo "FAIL: $name — exit $RC, want $want_rc" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  if [ -n "$want_sub" ] && ! printf '%s' "$OUT" | grep -q -- "$want_sub"; then
    echo "FAIL: $name — output missing '$want_sub'" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

mkdir -p "$TMP/empty" "$TMP/fresh" "$TMP/stale" "$TMP/decoys" "$TMP/mixed"
: > "$TMP/fresh/sla-proof-2026-08-20.md"                   # 9 days old
: > "$TMP/stale/sla-proof-2026-01-01.md"                   # ~240 days old
: > "$TMP/mixed/sla-proof-2026-01-01.md"
: > "$TMP/mixed/sla-proof-2026-07-15.md"                   # ~45 days: inside
# The two files that really do sit in docs/operations/ next to the reports
# — the recipe and the blank form. Neither is evidence.
: > "$TMP/decoys/sla-proof-procedure.md"
: > "$TMP/decoys/sla-proof-template.md"
: > "$TMP/decoys/sla-proof-draft.md"

# ── The load-bearing regression: today's scheduled-run state ────────────
# Both secrets unset and no proof report — the exact shape of runs
# 30733884244 / 31292354359 / 31922897932 / 32614013429, every one of
# which GitHub recorded as `success`. It must never be rc 0 again.
unset K6_TARGET STELLARINDEX_LOAD_API_KEY
run "$TMP/empty"
expect 'unconfigured target + no proof (the shipped no-op) → rc 1, never green' 1 'NO TARGET'

# A missing target is fatal on its own, even when the docs DO carry a
# recent proof: nothing measured anything this week.
run "$TMP/fresh"
expect 'unconfigured target but a fresh proof on disk → still rc 1' 1 'NO TARGET'

export K6_TARGET='https://api.staging.example.invalid/v1'
run "$TMP/empty"
expect 'only K6_TARGET set → rc 1 naming the missing key' 1 'unset secret(s): STELLARINDEX_LOAD_API_KEY'

unset K6_TARGET
export STELLARINDEX_LOAD_API_KEY='not-a-real-key'
run "$TMP/empty"
expect 'only the API key set → rc 1 naming the missing target' 1 'unset secret(s): K6_TARGET'

# ── Configured target: the verdict now turns on landed evidence ─────────
export K6_TARGET='https://api.staging.example.invalid/v1'

run "$TMP/fresh"
expect 'configured target + 9-day-old proof → rc 0 healthy' 0 'OK — target configured'

run "$TMP/empty"
expect 'configured target but no proof has ever landed → rc 2' 2 'has ever landed'

run "$TMP/stale"
expect 'configured target + 240-day-old proof → rc 2 (stale)' 2 'RED (rc=2)'

run "$TMP/mixed"
expect 'newest proof wins when several are present → rc 0' 0 'sla-proof-2026-07-15.md'

# rc 2 must stay DISTINCT from rc 1: the workflow still runs k6 on rc 2,
# because a stale report must not block the run that refreshes it.
run "$TMP/stale"
expect 'stale proof is rc 2, not rc 1 (the run must still execute)' 2 'the load run can execute'

# Non-vacuity: the recipe and the blank template live in the same
# directory as the reports. Counting either as evidence would make this
# gate permanently, silently green.
run "$TMP/decoys"
expect 'procedure/template/undated files are not evidence → rc 2' 2 'has ever landed'

# Threshold is real and configurable, not decorative.
OUT="$(SLA_EVIDENCE_DIR="$TMP/stale" SLA_EVIDENCE_NOW="$NOW_EPOCH" SLA_PROOF_MAX_AGE_DAYS=400 bash "$CHECK" 2>&1)"; RC=$?
expect 'SLA_PROOF_MAX_AGE_DAYS=400 admits the 240-day-old proof → rc 0' 0 'OK — target configured'

unset K6_TARGET STELLARINDEX_LOAD_API_KEY

# ── Wiring: the verdict is worthless if the workflow ignores it ─────────
# The defect lived in the YAML, not only in the decision logic, so pin the
# four properties that make the feed honest. PyYAML is fail-closed here
# (same convention as scripts/ci/lint-rule-structure.py) — a skipped
# structural check is how the original green no-op survived.
wiring_out="$(python3 - <<'PY' 2>&1
import sys
try:
    import yaml
except ImportError:
    print("FAIL: wiring — PyYAML not available; refusing to pass vacuously "
          "(install pyyaml)")
    sys.exit(0)

wf = yaml.safe_load(open(".github/workflows/k6-weekly.yml"))
# PyYAML parses the bare key `on:` as the boolean True.
triggers = wf.get("on", wf.get(True)) or {}
jobs = wf.get("jobs", {})

def check(name, cond, detail=""):
    print(("ok: " if cond else "FAIL: ") + name + (("" if cond else " — " + detail) if detail else ""))

check("k6-weekly runs the compile gate on pull_request",
      "pull_request" in triggers,
      "no pull_request trigger: a scenario syntax error can still sit undetected")

compile_steps = [
    (jname, job, step)
    for jname, job in jobs.items()
    for step in job.get("steps", [])
    if "make test-load-check" in (step.get("run") or "")
]
check("the scenario compile-check is unconditional (no secrets gate)",
      any("if" not in step and "if" not in job for _, job, step in compile_steps),
      "every `make test-load-check` step/job is behind an `if:` — that is the "
      "condition that kept it from ever running")

runs = [step.get("run") or "" for job in jobs.values() for step in job.get("steps", [])]
check("the run consults scripts/ci/check-sla-evidence.sh",
      any("scripts/ci/check-sla-evidence.sh" in r for r in runs),
      "nothing calls the decision core")

fail_steps = [
    step for job in jobs.values() for step in job.get("steps", [])
    if "verdict_rc != '0'" in str(step.get("if", "")) and "exit 1" in (step.get("run") or "")
]
check("a non-zero verdict FAILS the run (silence != success)",
      bool(fail_steps),
      "no step fails the run on a non-zero verdict — the badge would stay green")

mk = open("Makefile").read()
check("test-load-check seeds __ENV for `k6 archive`",
      "-e K6_TARGET=" in mk and "-e STELLARINDEX_LOAD_API_KEY=" in mk,
      "`k6 archive` does not inherit system env; without -e the scenarios' "
      "init guard throws and the compile gate can never pass (run 30542038490)")
PY
)"
printf '%s\n' "$wiring_out"
while IFS= read -r line; do
  case "$line" in
    ok:*)   pass=$((pass + 1)) ;;
    FAIL:*) fail=$((fail + 1)) ;;
  esac
done <<EOF
$wiring_out
EOF

echo
echo "check-sla-evidence-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ] || exit 1
