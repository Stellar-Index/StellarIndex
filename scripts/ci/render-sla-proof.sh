#!/usr/bin/env bash
# render-sla-proof.sh — turn one k6 summary export into the durable,
# self-describing SLA proof report the weekly run is supposed to leave
# behind. Issue #378.
#
# WHY THIS EXISTS. #316 made a weekly run that measures NOTHING go red.
# A later pass made a run that DOES measure export `summary.json` and
# render its headline numbers into the job summary. Neither closed the
# loop, because the step that makes the evidence DURABLE — promoting the
# numbers to docs/operations/sla-proof-<YYYY-MM-DD>.md, which is the only
# thing scripts/ci/check-sla-evidence.sh counts as evidence — was a
# manual instruction in a procedure that cannot be followed: it directs
# the operator at a Grafana host that does not exist and at promql over
# `k6_*` series that only exist when an unset remote-write secret is set.
# So the feed's durable half had no implementation at all, and no dated
# proof report has ever landed on any branch.
#
# This script is that implementation. It is deterministic and offline (no
# network, no k6, no gh), so it is exercised on every PR by
# scripts/ci/render-sla-proof-test.sh rather than once a week.
#
# THE RULE IT ENFORCES: an unlabelled number is not evidence. The report
# is refused outright (rc 2, nothing written) unless the run can name its
# own provenance — target, window, method, scenario, commit, k6 version —
# and unless the export actually carries the numbers the claim is about.
# A run that cannot produce evidence must fail loudly; it must never
# publish a partial report that reads as a pass.
#
# Usage:
#   render-sla-proof.sh --summary <k6-summary.json> [--out-dir <dir>]
#
# The filename is DERIVED from SLA_PROOF_ENDED_AT, never passed in: a
# report whose name disagrees with the window it measured is the
# mislabelling this script exists to prevent. The written path is echoed
# as the last line of stdout.
#
# Required provenance (env; every one must be non-empty):
#   SLA_PROOF_TARGET      base URL that was measured
#   SLA_PROOF_SCENARIO    scenario path under test/load/scenarios/
#   SLA_PROOF_K6_VERSION  `k6 version` output of the binary that ran
#   SLA_PROOF_COMMIT      git SHA the target was serving
#   SLA_PROOF_STARTED_AT  run start, ISO-8601 UTC (…Z)
#   SLA_PROOF_ENDED_AT    run end, ISO-8601 UTC (…Z)
#
# Optional provenance:
#   SLA_PROOF_RUN_URL     CI run permalink (recorded as a local run if unset)
#   SLA_PROOF_PROM_SINK   remote-write sink description (default: none)
#   SLA_PROOF_NOTE        free text appended to "Notes / caveats"
#
# Exit codes — the workflow branches on all three, so they are a contract:
#   0  PASS     report written; every declared k6 threshold held.
#   1  FAIL     report written; at least one declared threshold was
#               BREACHED. Still evidence — a failing proof is a real
#               measurement and must be retained — but the run is red.
#   2  REFUSED  nothing written: the inputs cannot support a labelled
#               claim (missing/unparseable export, missing provenance,
#               zero requests, absent headline metric, credential in the
#               target URL).
set -euo pipefail

cd "$(dirname "$0")/../.."

usage() {
  cat >&2 <<'EOF'
usage: render-sla-proof.sh --summary <k6-summary.json> [--out-dir <dir>]

Renders docs/operations/sla-proof-<YYYY-MM-DD>.md from a k6
--summary-export JSON. The date comes from SLA_PROOF_ENDED_AT.
See the header of this script for the required provenance env vars.
EOF
}

SUMMARY=""
OUT_DIR="docs/operations"
while [ $# -gt 0 ]; do
  case "$1" in
    --summary)  SUMMARY="${2:-}"; shift 2 ;;
    --out-dir)  OUT_DIR="${2:-}"; shift 2 ;;
    -h|--help)  usage; exit 0 ;;
    *) echo "render-sla-proof: unknown argument '$1'" >&2; usage; exit 2 ;;
  esac
done

if [ -z "$SUMMARY" ]; then
  echo "render-sla-proof: REFUSED — --summary is required." >&2
  usage
  exit 2
fi

if [ ! -s "$SUMMARY" ]; then
  cat >&2 <<EOF
render-sla-proof: REFUSED (rc=2) — the k6 summary export '${SUMMARY}' is
  missing or empty, so this run measured nothing. A run that cannot
  produce evidence must fail; it must not publish a report.
EOF
  exit 2
fi

# ── Provenance gate ─────────────────────────────────────────────────────
# Every field here answers one of "what, against what, when, by which
# method, at which commit". A report missing any of them is a number
# without a claim attached, which is exactly what #378 is about.
missing=""
for v in SLA_PROOF_TARGET SLA_PROOF_SCENARIO SLA_PROOF_K6_VERSION \
         SLA_PROOF_COMMIT SLA_PROOF_STARTED_AT SLA_PROOF_ENDED_AT; do
  if [ -z "${!v:-}" ]; then
    missing="${missing:+${missing} }${v}"
  fi
done
if [ -n "$missing" ]; then
  cat >&2 <<EOF
render-sla-proof: REFUSED (rc=2) — unset provenance: ${missing}.
  An SLA proof states what was measured, when, against which target, by
  what method, and at which commit. Without those the numbers are
  unattributable and prove nothing, so no report is written.
EOF
  exit 2
fi

# A committed report is public. The target is recorded verbatim, so a URL
# carrying userinfo would publish a credential. Refuse rather than
# silently strip: the caller must fix the input.
case "$SLA_PROOF_TARGET" in
  *"@"*)
    cat >&2 <<'EOF'
render-sla-proof: REFUSED (rc=2) — SLA_PROOF_TARGET contains userinfo
  ("user:pass@host"). The proof report is committed to a public repo and
  records the target verbatim. Pass a credential-free URL; the load key
  travels in STELLARINDEX_LOAD_API_KEY, never in the target.
EOF
    exit 2 ;;
esac

for stamp in SLA_PROOF_STARTED_AT SLA_PROOF_ENDED_AT; do
  case "${!stamp}" in
    [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z) ;;
    *)
      cat >&2 <<EOF
render-sla-proof: REFUSED (rc=2) — ${stamp}='${!stamp}' is not an
  ISO-8601 UTC instant (YYYY-MM-DDTHH:MM:SSZ). The run window is what
  makes the numbers re-checkable against the server-side histogram, so an
  unparseable window is a refusal, not a footnote.
EOF
      exit 2 ;;
  esac
done

mkdir -p "$OUT_DIR"

# Ties the report to the exact bytes it was rendered from. `shasum -a 256`
# is present on macOS and on the ubuntu runner; sha256sum is not on macOS.
if command -v shasum >/dev/null 2>&1; then
  SUMMARY_SHA="$(shasum -a 256 "$SUMMARY")"
else
  SUMMARY_SHA="$(sha256sum "$SUMMARY")"
fi
SUMMARY_SHA="${SUMMARY_SHA%% *}"
export SUMMARY_SHA

export SLA_PROOF_SUMMARY_PATH="$SUMMARY"
export SLA_PROOF_OUT_DIR="$OUT_DIR"

# Export explicitly so the renderer below sees them whether the caller
# exported them, set them inline, or assigned them as plain shell
# variables in a wrapper.
export SLA_PROOF_TARGET SLA_PROOF_SCENARIO SLA_PROOF_K6_VERSION
export SLA_PROOF_COMMIT SLA_PROOF_STARTED_AT SLA_PROOF_ENDED_AT
export SLA_PROOF_RUN_URL="${SLA_PROOF_RUN_URL:-}"
export SLA_PROOF_PROM_SINK="${SLA_PROOF_PROM_SINK:-}"
export SLA_PROOF_NOTE="${SLA_PROOF_NOTE:-}"

set +e
python3 - <<'PY'
import hashlib
import json
import os
import sys

summary_path = os.environ["SLA_PROOF_SUMMARY_PATH"]
out_dir = os.environ["SLA_PROOF_OUT_DIR"]

REFUSED = 2


def refuse(msg):
    sys.stderr.write("render-sla-proof: REFUSED (rc=2) — %s\n" % msg)
    sys.exit(REFUSED)


try:
    with open(summary_path) as fh:
        doc = json.load(fh)
except (OSError, ValueError) as exc:
    refuse("the k6 summary export %s is not readable JSON (%s). Nothing "
           "was written." % (summary_path, exc))

if not isinstance(doc, dict):
    refuse("the k6 summary export %s is not a JSON object." % summary_path)

metrics = doc.get("metrics")
if not isinstance(metrics, dict) or not metrics:
    refuse("the k6 summary export %s carries no `metrics` object — it is "
           "not a --summary-export." % summary_path)


def metric(name):
    m = metrics.get(name)
    return m if isinstance(m, dict) else {}


dur = metric("http_req_duration")
failed = metric("http_req_failed")
reqs = metric("http_reqs")

# ── The export must actually carry the claim's numbers ──────────────────
# ADR-0009 promises p95 <= 200 ms AND p99 <= 500 ms. k6's DEFAULT exported
# trend stats are avg,min,med,max,p(90),p(95) — p(99) is not among them,
# so an export taken without an explicit --summary-trend-stats renders a
# report whose p99 row reads "n/a" while the document still looks like a
# complete proof. The real 2026-06-13 acceptance export is exactly that
# shape. Refuse it: half a claim published as a whole one is the failure
# mode this file exists to prevent.
required = [
    ("http_req_duration", "p(95)", dur.get("p(95)")),
    ("http_req_duration", "p(99)", dur.get("p(99)")),
    ("http_req_duration", "med", dur.get("med")),
    ("http_req_failed", "value", failed.get("value")),
    ("http_reqs", "count", reqs.get("count")),
]
absent = ["%s.%s" % (m, k) for m, k, v in required if v is None]
if absent:
    refuse(
        "the export is missing %s. ADR-0009 claims p95 <= 200 ms AND "
        "p99 <= 500 ms; k6 does not export p(99) unless "
        "--summary-trend-stats asks for it, so a report rendered from this "
        "export would read \"n/a\" for half the SLA while looking "
        "complete. Re-run with "
        "--summary-trend-stats 'avg,min,med,max,p(90),p(95),p(99)'."
        % ", ".join(absent))

count = reqs.get("count") or 0
if count <= 0:
    refuse("the export records %s requests. A run that issued no requests "
           "measured nothing and is not evidence." % count)

# ── Verdicts ────────────────────────────────────────────────────────────
# k6 serialises each threshold as {expression: lastFailed}, so a TRUE
# value means the threshold was BREACHED.
breaches = []
declared = []
for name in sorted(metrics):
    m = metrics[name]
    if not isinstance(m, dict):
        continue
    for expr, tripped in sorted((m.get("thresholds") or {}).items()):
        declared.append((name, expr, bool(tripped)))
        if tripped:
            breaches.append((name, expr))

if not declared:
    refuse("the export declares no thresholds, so nothing in it asserts a "
           "pass or a fail. The canonical scenario declares "
           "`http_req_duration: p(95)<200, p(99)<500` and "
           "`http_req_failed: rate<0.001`; an export without them is not "
           "the canonical proof run.")

overall = "FAIL" if breaches else "PASS"


def ms(value):
    return "n/a" if value is None else "%.1f ms" % value


def pct(value):
    return "n/a" if value is None else "%.3f %%" % (value * 100)


def verdict(value, limit):
    if value is None:
        return "n/a"
    return "PASS" if value <= limit else "FAIL"


started = os.environ["SLA_PROOF_STARTED_AT"]
ended = os.environ["SLA_PROOF_ENDED_AT"]
report_date = ended[:10]
out_path = os.path.join(out_dir, "sla-proof-%s.md" % report_date)

target = os.environ["SLA_PROOF_TARGET"]
scenario = os.environ["SLA_PROOF_SCENARIO"]
k6_version = os.environ["SLA_PROOF_K6_VERSION"].strip().replace("\n", " ")
commit = os.environ["SLA_PROOF_COMMIT"]
run_url = os.environ.get("SLA_PROOF_RUN_URL", "").strip()
prom_sink = os.environ.get("SLA_PROOF_PROM_SINK", "").strip()
note = os.environ.get("SLA_PROOF_NOTE", "").strip()
summary_sha = os.environ["SUMMARY_SHA"]

# Everything below is a literal rendering of the export. Nothing is
# hand-entered, and nothing is inferred from a source other than the
# summary + the provenance env.
out = []
w = out.append

w("---")
w("title: SLA proof report — %s" % report_date)
w("status: evidence (generated — do not hand-edit)")
w("generator: scripts/ci/render-sla-proof.sh")
w("related:")
w("  - docs/adr/0009-latency-budget.md")
w("  - docs/operations/sla-proof-procedure.md")
w("  - %s" % scenario)
w("---")
w("")
w("# SLA proof report — %s" % report_date)
w("")
w("**Verdict: %s.**" % overall)
w("")
w("Generated by [`scripts/ci/render-sla-proof.sh`](../../scripts/ci/render-sla-proof.sh)")
w("from the `--summary-export` of a single k6 run. Every number below is a")
w("literal reading of that one export; nothing is hand-entered, and the")
w("renderer refuses to write a report at all when the export lacks a")
w("headline metric or the run lacks provenance.")
w("")
w("## Provenance")
w("")
w("| Field | Value |")
w("| --- | --- |")
w("| Claim under test | ADR-0009: `http_req_duration` p95 ≤ 200 ms, p99 ≤ 500 ms, `http_req_failed` rate < 0.1 % |")
w("| Method | k6 client-side timing (`http_req_duration`), whole-run aggregate over the scenario's ramp + soak + drain |")
w("| Scenario | `%s` |" % scenario)
w("| Target | `%s` |" % target)
w("| Run window (UTC) | `%s` → `%s` |" % (started, ended))
w("| Commit measured | `%s` |" % commit)
w("| k6 version | `%s` |" % k6_version)
w("| Metrics sink | %s |" % (prom_sink or "none — the summary export is the only durable output"))
w("| CI run | %s |" % (run_url or "not a CI run (rendered from a local export)"))
w("| Summary export | `%s`, sha256 `%s` |" % (os.path.basename(summary_path), summary_sha))
w("")
w("## Result")
w("")
w("| Metric | Threshold | Measured | Verdict |")
w("| --- | --- | --- | --- |")
w("| `http_req_duration` p95 | ≤ 200 ms | %s | %s |"
  % (ms(dur.get("p(95)")), verdict(dur.get("p(95)"), 200.0)))
w("| `http_req_duration` p99 | ≤ 500 ms | %s | %s |"
  % (ms(dur.get("p(99)")), verdict(dur.get("p(99)"), 500.0)))
w("| `http_req_failed` rate | < 0.1 % | " + pct(failed.get("value"))
  + " | %s |" % verdict(failed.get("value"), 0.001))
w("")
w("Distribution (whole run): min %s, med %s, p90 %s, p95 %s, p99 %s, max %s, avg %s."
  % (ms(dur.get("min")), ms(dur.get("med")), ms(dur.get("p(90)")),
     ms(dur.get("p(95)")), ms(dur.get("p(99)")), ms(dur.get("max")),
     ms(dur.get("avg"))))
w("")
w("Volume: **{:,} requests** at {:.1f} req/s over the window."
  .format(int(count), reqs.get("rate") or 0.0))
w("")
w("### Thresholds declared by the scenario")
w("")
w("Verdicts as k6 recorded them in the export — this is the scenario's own")
w("pass/fail bar, not a re-derivation.")
w("")
w("| Metric | Threshold | Verdict |")
w("| --- | --- | --- |")
for name, expr, tripped in declared:
    w("| `%s` | `%s` | %s |" % (name, expr, "BREACHED" if tripped else "PASS"))
w("")

# ── Per-endpoint breakdown ──────────────────────────────────────────────
# Only from `endpoint:` submetrics the scenario actually tagged. A
# scenario that tags nothing gets an explicit "not tagged", never invented
# rows.
endpoints = []
for name in sorted(metrics):
    if not name.startswith("http_req_duration{endpoint:"):
        continue
    tag = name[len("http_req_duration{endpoint:"):].rstrip("}")
    m = metrics[name]
    if isinstance(m, dict):
        endpoints.append((tag, m))

w("### Per-endpoint breakdown")
w("")
if endpoints:
    w("| Endpoint | med | p95 | p99 | max |")
    w("| --- | --- | --- | --- | --- |")
    for tag, m in endpoints:
        w("| `%s` | %s | %s | %s | %s |"
          % (tag, ms(m.get("med")), ms(m.get("p(95)")),
             ms(m.get("p(99)")), ms(m.get("max"))))
else:
    w("The export carries no `endpoint:`-tagged `http_req_duration`")
    w("submetrics, so this run has no per-endpoint breakdown. Only the")
    w("aggregate above is evidenced.")
w("")

checks = ((doc.get("root_group") or {}).get("checks") or {})
if isinstance(checks, dict) and checks:
    w("### Checks")
    w("")
    w("| Check | Passes | Fails |")
    w("| --- | --- | --- |")
    for cname in sorted(checks):
        c = checks[cname] or {}
        w("| `%s` | %s | %s |"
          % (cname, c.get("passes", "n/a"), c.get("fails", "n/a")))
    w("")

w("## What this report does and does not prove")
w("")
w("- It is a **client-side** measurement from a single source address.")
w("  Network latency between the runner and the target is inside every")
w("  number; the server-side histogram")
w("  (`http_request_success_duration_seconds`) is the independent second")
w("  measurement and is not part of this document.")
w("- The percentiles are **whole-run** aggregates, so the ramp and drain")
w("  phases are inside them, not the soak window alone. That biases the")
w("  numbers conservative (the ramp is the least-warm part of the run).")
w("- It says nothing about **availability**. The uptime SLO has its own")
w("  measurement and is not evidenced here.")
w("- It is evidence about the target named above **at the concurrency the")
w("  scenario applied**. It does not transfer to a differently-shaped")
w("  deployment.")
w("")
w("## Notes / caveats")
w("")
w(note if note else "None recorded for this run.")
w("")
w("## Reproduce")
w("")
w("```sh")
w("export K6_TARGET=%s" % target)
w("export STELLARINDEX_LOAD_API_KEY='<load-test key from vault>'")
w("k6 run \\")
w("  --summary-export summary.json \\")
w("  --summary-trend-stats 'avg,min,med,max,p(90),p(95),p(99)' \\")
w("  %s" % scenario)
w("SLA_PROOF_TARGET=$K6_TARGET \\")
w("  SLA_PROOF_SCENARIO=%s \\" % scenario)
w("  SLA_PROOF_K6_VERSION=\"$(k6 version)\" \\")
w("  SLA_PROOF_COMMIT=\"$(git rev-parse HEAD)\" \\")
w("  SLA_PROOF_STARTED_AT='<run start, YYYY-MM-DDTHH:MM:SSZ>' \\")
w("  SLA_PROOF_ENDED_AT='<run end, YYYY-MM-DDTHH:MM:SSZ>' \\")
w("  scripts/ci/render-sla-proof.sh --summary summary.json")
w("```")

body = "\n".join(out) + "\n"
with open(out_path, "w") as fh:
    fh.write(body)

digest = hashlib.sha256(body.encode("utf-8")).hexdigest()
print("render-sla-proof: %s — wrote %s (sha256 %s)"
      % (overall, out_path, digest[:16]))
if breaches:
    print("render-sla-proof: BREACHED %s"
          % ", ".join("%s %s" % (n, e) for n, e in breaches))
print(out_path)
sys.exit(1 if breaches else 0)
PY
rc=$?
set -e
exit "$rc"
