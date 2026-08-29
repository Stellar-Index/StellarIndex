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
# properties that make the feed honest. The block is FAIL-CLOSED: a
# verifier proved on 2026-08-29 that replacing k6-weekly.yml with
# unparseable YAML made all the assertions vanish while the suite still
# printed "11 passed, 0 failed" and exited 0 — a gate that does not run
# reports clean by printing nothing. run_wiring therefore captures the
# interpreter's exit status AND counts the assertions it actually emitted,
# and the caller treats either a non-zero rc or a short count as a failure.
WIRING_EXPECTED=8

# run_wiring <workflow-path> <makefile-path> <scenario-dir>
# Sets WIRING_OUT / WIRING_RC / WIRING_SEEN.
run_wiring() {
  WIRING_OUT="$(SLA_WF="$1" SLA_MK="$2" SLA_SCENARIOS="$3" python3 - <<'PY' 2>&1
import os
import re
import sys

try:
    import yaml
except ImportError:
    print("FAIL: wiring — PyYAML not available; refusing to pass vacuously "
          "(install pyyaml)")
    sys.exit(1)

WF = os.environ["SLA_WF"]
MK = os.environ["SLA_MK"]
SCENARIOS = os.environ["SLA_SCENARIOS"]

wf = yaml.safe_load(open(WF))
# PyYAML parses the bare key `on:` as the boolean True.
triggers = wf.get("on", wf.get(True)) or {}
jobs = wf.get("jobs", {})


def check(name, cond, detail=""):
    print(("ok: " if cond else "FAIL: ") + name
          + (("" if cond else " — " + detail) if detail else ""))


def steps_of(job):
    return job.get("steps", []) or []


def needs_of(name):
    n = jobs[name].get("needs") or []
    return [n] if isinstance(n, str) else list(n)


check("k6-weekly runs the compile gate on pull_request",
      "pull_request" in triggers,
      "no pull_request trigger: a scenario syntax error can still sit undetected")

compile_steps = [
    (jname, job, step)
    for jname, job in jobs.items()
    for step in steps_of(job)
    if "make test-load-check" in (step.get("run") or "")
]
check("the scenario compile-check is unconditional (no secrets gate)",
      any("if" not in step and "if" not in job for _, job, step in compile_steps),
      "every `make test-load-check` step/job is behind an `if:` — that is the "
      "condition that kept it from ever running")

runs = [step.get("run") or "" for job in jobs.values() for step in steps_of(job)]
check("the run consults scripts/ci/check-sla-evidence.sh",
      any("scripts/ci/check-sla-evidence.sh" in r for r in runs),
      "nothing calls the decision core")

verdict_steps = [
    step for job in jobs.values() for step in steps_of(job)
    if "verdict_rc != '0'" in str(step.get("if", ""))
]
fail_steps = [s for s in verdict_steps if "exit 1" in (s.get("run") or "")]
check("a non-zero verdict FAILS the run (silence != success)",
      bool(fail_steps),
      "no step fails the run on a non-zero verdict — the badge would stay green")

mk = open(MK).read()
check("test-load-check seeds __ENV for `k6 archive`",
      "-e K6_TARGET=" in mk and "-e STELLARINDEX_LOAD_API_KEY=" in mk,
      "`k6 archive` does not inherit system env; without -e the scenarios' "
      "init guard throws and the compile gate can never pass (run 30542038490)")

# The verdict must not be reachable-only-if-the-scenarios-compile: a
# `needs:` from the evidence job onto the compile job means a babel syntax
# error SKIPS the verdict, the tracking issue and the ::error:: — the very
# mechanism #316 exists to guarantee runs.
gate_jobs = [
    jname for jname, job in jobs.items()
    if any("scripts/ci/check-sla-evidence.sh" in (s.get("run") or "")
           for s in steps_of(job))
]
compile_jobs = sorted({jname for jname, _, _ in compile_steps})
blocked = [(g, c) for g in gate_jobs for c in compile_jobs if c in needs_of(g)]
check("the evidence verdict does not depend on the compile job",
      bool(gate_jobs) and not blocked,
      "job(s) %s `needs:` the compile job %s — a scenario syntax error would "
      "skip the verdict and file no tracking issue"
      % ([g for g, _ in blocked], [c for _, c in blocked]))

# ...and must not be suppressed by the k6 run itself failing: without
# always(), every step carries an implicit success(), so a red `Run
# scenario` step silences the report this workflow exists to produce.
missing_always = [
    str(s.get("name", "?")) for s in verdict_steps
    if "always()" not in str(s.get("if", ""))
]
check("the non-zero-verdict steps survive a failing k6 run (always())",
      bool(verdict_steps) and not missing_always,
      "step(s) %s carry an implicit success(): a failing `Run scenario` "
      "would suppress the sla-evidence issue" % missing_always)

# The compile gate is only worth making mandatory if it can pass. The
# pinned k6 (0.50.0) transpiles with babel, which parses ARRAY spread but
# not OBJECT spread; 04-batch.js and 06-mixed-realistic.js shipped object
# spread from day one and had therefore never compiled — invisible because
# the gate sat behind secrets that do not exist. `k6 archive` is the
# authoritative check (the scenario-compile job runs it); this static
# assertion keeps the fast lane honest on machines without k6.
OBJ_SPREAD = re.compile(r"\{[^{}\n]*?\.\.\.")
offenders = []
scanned = 0
for root, _dirs, files in os.walk(SCENARIOS):
    for fname in sorted(files):
        if not fname.endswith(".js"):
            continue
        path = os.path.join(root, fname)
        scanned += 1
        with open(path) as fh:
            for lineno, line in enumerate(fh, 1):
                code = line.split("//", 1)[0]
                if OBJ_SPREAD.search(code):
                    offenders.append("%s:%d"
                                     % (os.path.relpath(path, SCENARIOS), lineno))
check("no scenario uses object spread (the pinned k6's babel cannot parse it)",
      scanned > 0 and not offenders,
      "%s — rewrite as Object.assign({}, a, {...}); `k6 archive` dies with "
      "\"Unexpected token\" and the weekly run cannot execute" % offenders)
PY
)"
  WIRING_RC=$?
  WIRING_SEEN="$(printf '%s\n' "$WIRING_OUT" | grep -c -E '^(ok|FAIL): ' || true)"
}

run_wiring ".github/workflows/k6-weekly.yml" "Makefile" "test/load/scenarios"
printf '%s\n' "$WIRING_OUT"
while IFS= read -r line; do
  case "$line" in
    ok:*)   pass=$((pass + 1)) ;;
    FAIL:*) fail=$((fail + 1)) ;;
  esac
done <<EOF
$WIRING_OUT
EOF
echo "wiring: ${WIRING_SEEN} of ${WIRING_EXPECTED} structural assertions reported (python3 rc=${WIRING_RC})"
if [ "$WIRING_RC" -ne 0 ]; then
  echo "FAIL: wiring block — python3 exited $WIRING_RC, so the structural" \
       "assertions did not all run; a missing or unparseable workflow must" \
       "never pass silently" >&2
  fail=$((fail + 1))
fi
if [ "$WIRING_SEEN" -ne "$WIRING_EXPECTED" ]; then
  echo "FAIL: wiring block — only $WIRING_SEEN of $WIRING_EXPECTED assertions" \
       "reported; a gate that does not run reports clean by printing nothing" >&2
  fail=$((fail + 1))
fi

# Fail-closed proof for the block above, on a fixture that cannot parse.
# This is the exact vacuity a verifier demonstrated on 2026-08-29.
printf 'on: [pull_request\njobs: : :\n  - not yaml\n' > "$TMP/unparseable.yml"
run_wiring "$TMP/unparseable.yml" "Makefile" "test/load/scenarios"
if [ "$WIRING_RC" -ne 0 ] || [ "$WIRING_SEEN" -ne "$WIRING_EXPECTED" ]; then
  echo "ok: the wiring block is fail-closed on an unparseable workflow" \
       "(rc=$WIRING_RC, ${WIRING_SEEN}/${WIRING_EXPECTED} reported)"
  pass=$((pass + 1))
else
  echo "FAIL: the wiring block passed vacuously on an unparseable workflow —" \
       "renaming or breaking k6-weekly.yml would silently delete these" \
       "assertions" >&2
  fail=$((fail + 1))
fi

echo
echo "check-sla-evidence-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ] || exit 1
