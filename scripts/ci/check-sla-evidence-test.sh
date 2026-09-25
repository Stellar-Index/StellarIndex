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
  if [ -n "$want_sub" ] && ! grep -q -- "$want_sub" <<<"$OUT"; then
    echo "FAIL: $name — output missing '$want_sub'" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

mkdir -p "$TMP/empty" "$TMP/fresh" "$TMP/stale" "$TMP/decoys" "$TMP/mixed"
# Leg 2b (GH-744) opens the file and requires a `generator:` line, so
# every fixture standing in for a REAL landed report needs one — an
# empty file is no longer indistinguishable from evidence, which is the
# defect this leg exists to close.
gen_line='generator: scripts/ops/sla-proof-from-probe.sh'
printf '%s\n' "$gen_line" > "$TMP/fresh/sla-proof-2026-08-20.md"   # 9 days old
printf '%s\n' "$gen_line" > "$TMP/stale/sla-proof-2026-01-01.md"   # ~240 days old
printf '%s\n' "$gen_line" > "$TMP/mixed/sla-proof-2026-01-01.md"
printf '%s\n' "$gen_line" > "$TMP/mixed/sla-proof-2026-07-15.md"   # ~45 days: inside
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

# ── Leg 2b (GH-744): a matching filename is not itself evidence ─────────
# Before the fix, Leg 2 matched only the sla-proof-<date>.md filename and
# never opened the file — so a masked timer, a hand-edited stub, or a
# truncated write with the right name and mtime read HEALTHY.
mkdir -p "$TMP/no-generator" "$TMP/bad-digest" "$TMP/good-digest"
: > "$TMP/no-generator/sla-proof-2026-08-20.md"
run "$TMP/no-generator"
expect 'a dated file with no generator: line → rc 2, not HEALTHY' 2 \
  'carries no'

printf 'generator: scripts/ops/sla-proof-from-probe.sh\ndigest: sha256:%s\nHAND-EDITED AFTER RENDER\n' \
  "$(printf '0%.0s' $(seq 1 64))" > "$TMP/bad-digest/sla-proof-2026-08-20.md"
run "$TMP/bad-digest"
expect 'a digest that does not match its own bytes → rc 2, not HEALTHY' 2 \
  'does not match its own bytes'

# A digest computed the same way the generator computes it (zero the
# digest line, hash the rest) must verify clean.
body="generator: scripts/ops/sla-proof-from-probe.sh
digest: sha256:$(printf '0%.0s' $(seq 1 64))
Verdict: PROVEN.
"
printf '%s' "$body" > "$TMP/good-digest/sla-proof-2026-08-20.md"
if command -v sha256sum >/dev/null 2>&1; then
  real_digest="$(printf '%s' "$body" | sha256sum | awk '{print $1}')"
else
  real_digest="$(printf '%s' "$body" | shasum -a 256 | awk '{print $1}')"
fi
printf '%s' "$body" | sed "s/^digest: sha256:.*/digest: sha256:${real_digest}/" \
  > "$TMP/good-digest/sla-proof-2026-08-20.md"
run "$TMP/good-digest"
expect 'a digest that DOES match its own bytes → rc 0 HEALTHY' 0 \
  'OK — target configured'

# Threshold is real and configurable, not decorative.
OUT="$(SLA_EVIDENCE_DIR="$TMP/stale" SLA_EVIDENCE_NOW="$NOW_EPOCH" SLA_PROOF_MAX_AGE_DAYS=400 bash "$CHECK" 2>&1)"; RC=$?
expect 'SLA_PROOF_MAX_AGE_DAYS=400 admits the 240-day-old proof → rc 0' 0 'OK — target configured'

unset K6_TARGET STELLARINDEX_LOAD_API_KEY

# ── The second source: the probe aggregate ──────────────────────────────
# Leg 2 reads a committed sla-proof-<YYYY-MM-DD>.md and does not care which
# producer wrote it, so it needed no change and got none. Leg 1 did: it
# asked only "are the k6 secrets set", which would have reported a feed
# producing evidence from the probe every Sunday as RED forever, for the
# absence of a load target it no longer needs. These cases pin the source
# selector, and above all pin that naming one source never smuggles in the
# other's readiness.
run_src() { # run_src <source> <fixture-dir>
  OUT="$(SLA_EVIDENCE_SOURCE="$1" SLA_EVIDENCE_DIR="$2" \
    SLA_EVIDENCE_NOW="$NOW_EPOCH" bash "$CHECK" 2>&1)"
  RC=$?
}

unset SLA_PROBE_PROM_URL
run_src probe "$TMP/fresh"
expect 'source=probe with no SLA_PROBE_PROM_URL → rc 1' 1 'NO PROBE SOURCE'

export SLA_PROBE_PROM_URL='http://127.0.0.1:9090'
run_src probe "$TMP/fresh"
expect 'source=probe + a fresh proof → rc 0, no k6 secret required' 0 \
  'OK — probe aggregate configured'
run_src probe "$TMP/empty"
expect 'source=probe + no proof → rc 2, not rc 1' 2 'the probe aggregate can be read'

# The load-bearing separation: a configured probe source must NOT satisfy
# a caller that asked for k6, or k6-weekly would run k6 with no target.
run_src k6 "$TMP/fresh"
expect 'source=k6 is not satisfied by the probe source → rc 1' 1 'NO TARGET'

# ...and the reverse. A k6 target must not satisfy a caller that asked for
# the probe: the proof would be rendered from series nothing can read.
unset SLA_PROBE_PROM_URL
export K6_TARGET='https://api.staging.example.invalid/v1'
export STELLARINDEX_LOAD_API_KEY='not-a-real-key'
run_src probe "$TMP/fresh"
expect 'source=probe is not satisfied by the k6 secrets → rc 1' 1 'NO PROBE SOURCE'

# `any` is the "is this feed alive at all" question and takes either.
unset K6_TARGET STELLARINDEX_LOAD_API_KEY
export SLA_PROBE_PROM_URL='http://127.0.0.1:9090'
run_src any "$TMP/fresh"
expect 'source=any is satisfied by the probe source alone → rc 0' 0 \
  'OK — probe aggregate configured'
unset SLA_PROBE_PROM_URL
run_src any "$TMP/fresh"
expect 'source=any with neither source → rc 1, the shipped no-op state' 1 'NO TARGET'

# An unknown source is a caller bug, not a verdict. Never rc 0.
OUT="$(SLA_EVIDENCE_SOURCE=grafana SLA_EVIDENCE_DIR="$TMP/fresh" \
  SLA_EVIDENCE_NOW="$NOW_EPOCH" bash "$CHECK" 2>&1)"; RC=$?
expect 'an unknown SLA_EVIDENCE_SOURCE is refused, never silently green' 1 \
  'is not'

unset SLA_PROBE_PROM_URL K6_TARGET STELLARINDEX_LOAD_API_KEY


# ── Wiring: the verdict is worthless if the workflow ignores it ─────────
# The defect lived in the YAML, not only in the decision logic, so pin the
# properties that make the feed honest. The block is FAIL-CLOSED: a
# verifier proved on 2026-08-29 that replacing k6-weekly.yml with
# unparseable YAML made all the assertions vanish while the suite still
# printed "11 passed, 0 failed" and exited 0 — a gate that does not run
# reports clean by printing nothing. run_wiring therefore captures the
# interpreter's exit status AND counts the assertions it actually emitted,
# and the caller treats either a non-zero rc or a short count as a failure.
WIRING_EXPECTED=19

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

# NOTE: no apostrophes below. bash 3.2 (the macOS system bash) lexes quote
# characters INSIDE a quoted-delimiter heredoc when that heredoc sits in a
# $(...) command substitution, so an odd number of apostrophes in this block
# silently swallows the following lines and `bash -n` reports a syntax error
# a dozen lines away from the real cause.
#
# -- Evidence must actually be PRODUCED, not merely attempted (#378) -----
# #316 made a run that measures nothing go red. It did not make a run that
# DOES measure land anything: there was no --summary-export and no
# handleSummary, so the only output was console text in a log that ages
# out, and the upload step published the checked-in historical fixture
# under the name of the run that did not produce it.
upload_steps = [
    st for job in jobs.values() for st in steps_of(job)
    if "upload-artifact" in str(st.get("uses", ""))
]
k6_runs = [
    (st.get("run") or "") for job in jobs.values() for st in steps_of(job)
    if re.search(r"\bk6 run\b", st.get("run") or "")
]

check("the k6 run exports a machine-readable summary",
      bool(k6_runs) and all("--summary-export" in r for r in k6_runs),
      "no --summary-export and no handleSummary: the only output of the run "
      "is console text in a log that ages out, so nothing can be promoted "
      "to docs/operations/sla-proof-<YYYY-MM-DD>.md")

# ADR-0009 promises p95 <= 200 ms AND p99 <= 500 ms. The default k6
# exported trend stats are avg,min,med,max,p(90),p(95) -- p(99) is NOT
# among them, so a naive export lands evidence that reads "n/a" for half
# the SLA while looking complete. Confirmed against the real 2026-06-13
# export, which has no p(99) key for exactly this reason.
check("the summary export includes p(99), the second headline SLA number",
      bool(k6_runs) and all("p(99)" in r for r in k6_runs),
      "--summary-trend-stats does not request p(99): the landed evidence "
      "cannot support the p99 <= 500 ms claim")

# test/load/reports/ permanently contains checked-in historical evidence
# (README.md + 2026-06-13/00-acceptance.json, kept by the .gitignore rule
# `!/test/load/reports/2026-*/`). Uploading that directory wholesale
# republished an eleven-week-old all-green summary as the artifact of the
# current run: run 33299745709 had `Run scenario: skipped` and still
# shipped k6-summary-33299745709 (1,691 B) asserting 30,600 passes, 0 fails.
stale_uploads = [
    str(st.get("name", "?")) for st in upload_steps
    if str((st.get("with") or {}).get("path", "")).strip().rstrip("/")
    == "test/load/reports"
]
check("the evidence artifact is run-scoped, not the checked-in fixture dir",
      bool(upload_steps) and not stale_uploads,
      "step(s) %s upload bare test/load/reports/, which republishes the "
      "checked-in 2026-06-13 all-green k6 export as the artifact of a run "
      "that measured nothing" % stale_uploads)

# k6 defaults K6_PROMETHEUS_RW_SERVER_URL to http://localhost:9090 -- the
# localhost of the runner itself -- so an unconditional --out against an
# unset secret discards the whole run and looks identical to a working sink.
unguarded_rw = [
    r for r in k6_runs
    if "experimental-prometheus-rw" in r
    and "K6_PROMETHEUS_RW_SERVER_URL" not in r
]
check("the prometheus remote-write sink is conditional on its URL being set",
      not unguarded_rw,
      "`--out experimental-prometheus-rw` is passed unconditionally; with "
      "K6_PROMETHEUS_RW_SERVER_URL unset k6 streams the run into the "
      "localhost of the runner and discards it")

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

# -- The DURABLE half: a run must leave something behind (#378) ---------
# Everything above proves the run measures and reports. None of it proves
# the run RETAINS anything. summary.json lives in a 90-day artifact and
# the step summary lives in a job page; the only artefact
# check-sla-evidence.sh has ever counted is a committed
# docs/operations/sla-proof-<YYYY-MM-DD>.md, and until #378 the sole
# implementation of that promotion was a paragraph of manual procedure
# pointing at a Grafana host that does not exist. Six assertions pin the
# machinery that closes the loop.
RENDERER = os.path.join("scripts", "ci", "render-sla-proof.sh")
# EXECUTED, not merely mentioned — the same `./`-or-`bash ` prefix rule
# check-verify-parity.sh uses. Matching a bare path would be satisfied by
# the publish step naming the renderer in its commit message, which is a
# gate that passes on a comment.
RUNS_RENDERER = re.compile(r"(\./|bash +)scripts/ci/render-sla-proof\.sh")


def runs_renderer(step):
    return bool(RUNS_RENDERER.search(step.get("run") or ""))


render_steps = [
    (jname, idx, st)
    for jname, job in jobs.items()
    for idx, st in enumerate(steps_of(job))
    if runs_renderer(st)
]
check("the run renders the durable dated proof report",
      bool(render_steps),
      "no step runs %s: the run can export summary.json and still leave no "
      "docs/operations/sla-proof-<YYYY-MM-DD>.md, which is the only thing "
      "check-sla-evidence.sh counts as evidence" % RENDERER)

check("the proof renderer exists and is executable",
      os.access(RENDERER, os.X_OK),
      "%s is missing or not executable — the render step would fail every "
      "week for a reason unrelated to the SLA" % RENDERER)

publish_steps = [
    st for job in jobs.values() for st in steps_of(job)
    if "gh api" in (st.get("run") or "") and "contents/" in (st.get("run") or "")
]
check("the rendered report is committed, not merely uploaded",
      bool(publish_steps),
      "nothing commits the report. An artifact expires at 90 days and a run "
      "summary ages out with the job page, so a run that renders but does "
      "not publish still leaves the freshness leg permanently red")

render_jobs = sorted({jname for jname, _, _ in render_steps})
missing_write = [
    j for j in render_jobs
    if (jobs[j].get("permissions") or {}).get("contents") != "write"
]
check("the evidence job can actually write the report to the repo",
      bool(render_jobs) and not missing_write,
      "job(s) %s render a proof report but do not hold contents: write, so "
      "the commit that makes it durable cannot succeed" % missing_write)

# The verdict that drives the tracking issue and the exit status must be
# recomputed AFTER the report lands. Taken before, a first healthy run
# reports "no proof inside the window" for the absence of the file it just
# wrote, and the feed needs two weeks to call itself healthy once.
stale_verdict = []
for jname, job in jobs.items():
    sts = steps_of(job)
    render_at = [i for i, st in enumerate(sts) if runs_renderer(st)]
    if not render_at:
        continue
    consumed = set()
    for st in sts:
        cond = str(st.get("if", ""))
        if "verdict_rc" in cond:
            consumed.update(
                re.findall(r"steps\.([A-Za-z0-9_-]+)\.outputs\.verdict_rc", cond))
    for sid in sorted(consumed):
        at = [i for i, st in enumerate(sts) if st.get("id") == sid]
        if at and at[0] < max(render_at):
            stale_verdict.append("%s.%s" % (jname, sid))
check("the verdict driving the issue is recomputed AFTER the report lands",
      bool(render_steps) and not stale_verdict,
      "step id(s) %s produce the consumed verdict_rc BEFORE the render step; "
      "a first healthy run would go red for the absence of the report it had "
      "just written" % stale_verdict)

# k6 exits 99 when a declared threshold is BREACHED. That is a real
# measurement and the single most valuable report to retain, so the step
# must publish the code rather than abort the job on it — and something
# must still redden the run.
k6_rc_published = [
    st for job in jobs.values() for st in steps_of(job)
    if re.search(r"\bk6 run\b", st.get("run") or "")
    and "k6_rc=" in (st.get("run") or "")
]
red_on_k6 = any(
    "k6_rc !=" in str(st.get("if", ""))
    for job in jobs.values() for st in steps_of(job)
    if "exit 1" in (st.get("run") or "")
)
check("a breached threshold is retained as evidence AND reddens the run",
      bool(k6_rc_published) and red_on_k6,
      "the k6 step must publish k6_rc (so a breach still reaches the "
      "renderer) and a fail step must redden the run on it; otherwise "
      "either the most important proof is never written, or a breach "
      "passes quietly")

# A run that measured nothing must not close the tracking issue on the
# strength of a report some EARLIER run landed inside the 45-day window.
close_steps = [
    st for job in jobs.values() for st in steps_of(job)
    if "issue close" in (st.get("run") or "")
]
unguarded_close = [
    str(st.get("name", "?")) for st in close_steps
    if "proof_path" not in str(st.get("if", ""))
]
check("the tracking issue only closes on a run that itself produced a proof",
      bool(close_steps) and not unguarded_close,
      "step(s) %s close the sla-evidence issue without requiring THIS run to "
      "have rendered a report — a run that measured nothing would report the "
      "feed healthy" % unguarded_close)
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


# ── Wiring: the SCHEDULED producer ──────────────────────────────────────
# The block above is about k6-weekly.yml, which is now dispatch-only: its
# scenarios and its load run are a retained capability waiting on a target
# that does not exist. The workflow that actually runs every Sunday is
# sla-proof-weekly.yml, and every property #316 and #378 fought for has to
# hold there now or it holds nowhere. Same fail-closed shape as above.
EVIDENCE_EXPECTED=16

# run_evidence_wiring <evidence-workflow> <k6-workflow>
# Sets EV_OUT / EV_RC / EV_SEEN.
run_evidence_wiring() {
  EV_OUT="$(SLA_EV_WF="$1" SLA_K6_WF="$2" python3 - <<'PY' 2>&1
import os
import re
import sys

try:
    import yaml
except ImportError:
    print("FAIL: evidence wiring — PyYAML not available; refusing to pass "
          "vacuously (install pyyaml)")
    sys.exit(1)

# NOTE: no apostrophes below. bash 3.2 (the macOS system bash) lexes
# quote characters INSIDE a quoted-delimiter heredoc when that heredoc sits
# in a $(...) command substitution, so an apostrophe in this block swallows
# the following lines and `bash -n` reports a syntax error a hundred lines
# from the real cause. The workflow conditions this block matches contain
# single quotes, so they are spelled with chr(39).
Q = chr(39)
WF = os.environ["SLA_EV_WF"]
K6 = os.environ["SLA_K6_WF"]

raw = open(WF).read()
wf = yaml.safe_load(raw)
if not isinstance(wf, dict):
    print("FAIL: evidence wiring — %s did not parse as a mapping" % WF)
    sys.exit(1)
# PyYAML parses the bare key `on:` as the boolean True.
triggers = wf.get("on", wf.get(True)) or {}
jobs = wf.get("jobs", {}) or {}
steps = [st for job in jobs.values() for st in (job.get("steps") or [])]
runs = [st.get("run") or "" for st in steps]


def check(name, cond, detail=""):
    print(("ok: " if cond else "FAIL: ") + name
          + (("" if cond else " — " + detail) if detail else ""))


check("the evidence producer is actually scheduled",
      "schedule" in triggers,
      "%s has no schedule: trigger, so nothing produces the weekly proof "
      "and the feed is back where #316 found it" % WF)

k6_raw = yaml.safe_load(open(K6).read()) or {}
k6_triggers = k6_raw.get("on", k6_raw.get(True)) or {}
check("the retired k6 schedule is actually gone",
      "schedule" not in k6_triggers,
      "%s still carries a schedule: trigger. Two scheduled workflows both "
      "publishing docs/operations/sla-proof-<same-date>.md race each other, "
      "and the one with no target goes red forever" % K6)

# EXECUTED, not merely mentioned — the same prefix rule check-verify-parity
# uses. Matching a bare path would be satisfied by a commit message that
# names the generator, which is a gate that passes on a comment.
GEN = os.path.join("scripts", "ops", "sla-proof-from-probe.sh")
RUNS_GEN = re.compile(r"(\./|bash +)scripts/ops/sla-proof-from-probe\.sh")
gen_at = [i for i, r in enumerate(runs) if RUNS_GEN.search(r)]
check("the scheduled run renders the durable dated proof report",
      bool(gen_at),
      "no step runs %s, so the run can read the probe series and still leave "
      "no docs/operations/sla-proof-<YYYY-MM-DD>.md — the only thing "
      "check-sla-evidence.sh counts as evidence" % GEN)

check("the proof generator exists and is executable",
      os.access(GEN, os.X_OK),
      "%s is missing or not executable — the render step would fail every "
      "week for a reason unrelated to the SLA" % GEN)

check("the run consults scripts/ci/check-sla-evidence.sh",
      any("scripts/ci/check-sla-evidence.sh" in r for r in runs),
      "nothing calls the decision core")

# Without the source selector the decision core asks "are the k6 secrets
# set", which they are not and will not be — so this workflow would report
# RED forever for the absence of a load target it does not use.
check("the decision core is asked with SLA_EVIDENCE_SOURCE=probe",
      all("SLA_EVIDENCE_SOURCE=probe" in r
          for r in runs if "scripts/ci/check-sla-evidence.sh" in r),
      "a call to check-sla-evidence.sh does not name the probe source; it "
      "would be answered about the k6 load target, which is deliberately "
      "unset, and this workflow could never report itself healthy")

publish = [st for st in steps
           if "gh api" in (st.get("run") or "")
           and "contents/" in (st.get("run") or "")]
check("the rendered report is committed, not merely uploaded",
      bool(publish),
      "nothing commits the report. An artifact expires at 90 days and a run "
      "summary ages out with the job page, so a run that renders but does "
      "not publish leaves the freshness leg permanently red")

render_jobs = sorted({
    jname for jname, job in jobs.items()
    if any(RUNS_GEN.search(st.get("run") or "")
           for st in (job.get("steps") or []))
})
missing_write = [j for j in render_jobs
                 if (jobs[j].get("permissions") or {}).get("contents") != "write"]
check("the evidence job can actually write the report to the repo",
      bool(render_jobs) and not missing_write,
      "job(s) %s render a proof report but do not hold contents: write, so "
      "the commit that makes it durable cannot succeed" % missing_write)

# The verdict driving the issue and the exit status must be recomputed
# AFTER the report lands. Taken before, a first healthy run reports "no
# proof inside the window" for the absence of the file it just wrote.
stale_verdict = []
for jname, job in jobs.items():
    sts = job.get("steps") or []
    at = [i for i, st in enumerate(sts) if RUNS_GEN.search(st.get("run") or "")]
    if not at:
        continue
    consumed = set()
    for st in sts:
        cond = str(st.get("if", ""))
        if "verdict_rc" in cond:
            consumed.update(
                re.findall(r"steps\.([A-Za-z0-9_-]+)\.outputs\.verdict_rc", cond))
    for sid in sorted(consumed):
        idx = [i for i, st in enumerate(sts) if st.get("id") == sid]
        if idx and idx[0] < max(at):
            stale_verdict.append("%s.%s" % (jname, sid))
check("the verdict driving the issue is recomputed AFTER the report lands",
      bool(gen_at) and not stale_verdict,
      "step id(s) %s produce the consumed verdict_rc BEFORE the render step; "
      "a first healthy run would go red for the absence of the report it had "
      "just written" % stale_verdict)

# Only the steps that must fire on a BAD verdict. The close step is
# deliberately not among them: it runs on the healthy path and must stay
# skipped when a render failed, or a broken run would close the issue.
NEG = "verdict_rc != " + Q + "0" + Q
verdict_steps = [st for st in steps if NEG in str(st.get("if", ""))]
missing_always = [str(st.get("name", "?")) for st in verdict_steps
                  if "always()" not in str(st.get("if", ""))]
check("the verdict-driven steps survive a failing render (always())",
      bool(verdict_steps) and not missing_always,
      "step(s) %s carry an implicit success(): a failing render step would "
      "suppress the sla-evidence issue this workflow exists to file"
      % missing_always)

fail_steps = [st for st in steps if "exit 1" in (st.get("run") or "")]
check("a non-zero verdict FAILS the run (silence != success)",
      any(("verdict_rc != " + Q + "0" + Q) in str(st.get("if", ""))
          for st in fail_steps),
      "no step fails the run on a non-zero verdict — the badge would stay "
      "green while nothing produced evidence")

# A rendered-but-NOT-PROVEN week is the single most important report this
# feed produces. It must be retained (the publish step is gated on the
# path, not on the verdict) AND it must redden the run.
check("a NOT PROVEN week is retained as evidence AND reddens the run",
      any(("proof_rc != " + Q + "0" + Q) in str(st.get("if", ""))
          for st in fail_steps)
      and all("proof_rc" not in str(st.get("if", "")) or
              "proof_path" in str(st.get("if", "")) for st in publish),
      "either nothing reddens the run on a NOT PROVEN verdict, or the "
      "publish step is gated on the verdict rather than on a report having "
      "been written — the run whose finding matters most would leave "
      "nothing behind or pass quietly")

close_steps = [st for st in steps if "issue close" in (st.get("run") or "")]
unguarded_close = [str(st.get("name", "?")) for st in close_steps
                   if "proof_path" not in str(st.get("if", ""))]
check("the tracking issue only closes on a run that itself produced a proof",
      bool(close_steps) and not unguarded_close,
      "step(s) %s close the sla-evidence issue without requiring THIS run to "
      "have rendered a report — a run that read nothing would report the "
      "feed healthy on the strength of a file some earlier run wrote"
      % unguarded_close)

# The scheduled-control declaration is void unless it names a step that
# really exists, matched in full. A marker that outlives the step whose
# existence is its whole basis silently downgrades the class of this
# workflow.
marker = re.search(r"^#\s*scheduled-control:\s*reports-by-failing\s+\"(.+)\"\s*$",
                   raw, re.M)
names = {str(st.get("name", "")) for st in steps}
check("the reports-by-failing marker names a step that exists",
      bool(marker) and marker.group(1) in names,
      "the marker is absent or names a step this workflow does not have "
      "(%s); the scheduled-control sweep voids it and reports this control "
      "as broken plumbing rather than as a finding to read"
      % (marker.group(1) if marker else "no marker"))

# This job holds an ssh key. A secret interpolated into a run: body lands
# in the rendered shell; routed through env: it does not.
inline_secret = [str(st.get("name", "?")) for st in steps
                 if "secrets." in (st.get("run") or "")]
check("no secret is interpolated into a run: body",
      not inline_secret,
      "step(s) %s interpolate a ${{ secrets.* }} directly into the shell "
      "instead of routing it through env: (F-1298)" % inline_secret)

checkouts = [st for st in steps if "actions/checkout" in str(st.get("uses", ""))]
leaky = [str(st.get("name", st.get("uses", "?"))) for st in checkouts
         if (st.get("with") or {}).get("persist-credentials") is not False]
check("every checkout leaves no push credential on disk",
      bool(checkouts) and not leaky,
      "checkout(s) %s do not set persist-credentials: false, so a job that "
      "already carries an ssh key would also carry a git push token" % leaky)
PY
)"
  EV_RC=$?
  EV_SEEN="$(printf '%s\n' "$EV_OUT" | grep -c -E '^(ok|FAIL): ' || true)"
}

run_evidence_wiring ".github/workflows/sla-proof-weekly.yml" \
                    ".github/workflows/k6-weekly.yml"
printf '%s\n' "$EV_OUT"
while IFS= read -r line; do
  case "$line" in
    ok:*)   pass=$((pass + 1)) ;;
    FAIL:*) fail=$((fail + 1)) ;;
  esac
done <<EOF
$EV_OUT
EOF
echo "evidence wiring: ${EV_SEEN} of ${EVIDENCE_EXPECTED} structural assertions reported (python3 rc=${EV_RC})"
if [ "$EV_RC" -ne 0 ]; then
  echo "FAIL: evidence wiring block — python3 exited $EV_RC, so the structural" \
       "assertions did not all run" >&2
  fail=$((fail + 1))
fi
if [ "$EV_SEEN" -ne "$EVIDENCE_EXPECTED" ]; then
  echo "FAIL: evidence wiring block — only $EV_SEEN of $EVIDENCE_EXPECTED" \
       "assertions reported; a gate that does not run reports clean by" \
       "printing nothing" >&2
  fail=$((fail + 1))
fi

run_evidence_wiring "$TMP/unparseable.yml" ".github/workflows/k6-weekly.yml"
if [ "$EV_RC" -ne 0 ] || [ "$EV_SEEN" -ne "$EVIDENCE_EXPECTED" ]; then
  echo "ok: the evidence wiring block is fail-closed on an unparseable" \
       "workflow (rc=$EV_RC, ${EV_SEEN}/${EVIDENCE_EXPECTED} reported)"
  pass=$((pass + 1))
else
  echo "FAIL: the evidence wiring block passed vacuously on an unparseable" \
       "workflow — renaming or breaking sla-proof-weekly.yml would silently" \
       "delete these assertions" >&2
  fail=$((fail + 1))
fi

echo
echo "check-sla-evidence-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ] || exit 1
