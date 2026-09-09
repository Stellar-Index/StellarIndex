#!/usr/bin/env bash
# render-sla-proof-test.sh — fixture tests for the SLA proof renderer
# (scripts/ci/render-sla-proof.sh). Issue #378.
#
# The defect: the weekly k6 run had no way to leave anything durable
# behind. Its export and its run summary both age out, and the only thing
# scripts/ci/check-sla-evidence.sh counts as evidence — a committed
# docs/operations/sla-proof-<YYYY-MM-DD>.md — was produced by an
# unfollowable manual procedure, so none has ever existed on any branch.
# The renderer is the implementation of that promotion, and k6-weekly runs
# it once a week against secrets that do not exist, so these fixtures are
# the only place its behaviour is exercised on a PR.
#
# What is pinned here is mostly REFUSAL. A renderer that emits a report no
# matter what it was handed re-creates the original defect in a new
# spelling: an all-green document under the name of a run that measured
# nothing.
#
# Run: bash scripts/ci/render-sla-proof-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
RENDER="$PWD/scripts/ci/render-sla-proof.sh"
HISTORICAL="$PWD/test/load/reports/2026-06-13/00-acceptance.json"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

expect() {
  local name="$1" want_rc="$2" want_sub="${3:-}"
  if [ "$RC" -ne "$want_rc" ]; then
    echo "FAIL: $name — exit $RC, want $want_rc" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  if [ -n "$want_sub" ] && ! grep -qF -- "$want_sub" <<<"$OUT"; then
    echo "FAIL: $name — output missing '$want_sub'" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

assert_contains() { # assert_contains <name> <file> <substring>
  if grep -qF -- "$3" "$2"; then
    echo "ok: $1"
    pass=$((pass + 1))
  else
    echo "FAIL: $1 — '$2' does not contain '$3'" >&2
    fail=$((fail + 1))
  fi
}

assert_absent() { # assert_absent <name> <file> <substring>
  if grep -qF -- "$3" "$2"; then
    echo "FAIL: $1 — '$2' unexpectedly contains '$3'" >&2
    fail=$((fail + 1))
  else
    echo "ok: $1"
    pass=$((pass + 1))
  fi
}

# The provenance every case starts from. Individual cases unset or
# override one field to prove each is load-bearing.
base_env() {
  export SLA_PROOF_TARGET='https://api.staging.example.invalid/v1'
  export SLA_PROOF_SCENARIO='test/load/scenarios/06-mixed-realistic.js'
  export SLA_PROOF_K6_VERSION='k6 v0.50.0 (go1.21.8, linux/amd64)'
  export SLA_PROOF_COMMIT='8bb11d38c0ffee1234567890abcdef0123456789'
  export SLA_PROOF_STARTED_AT='2026-09-13T02:00:11Z'
  export SLA_PROOF_ENDED_AT='2026-09-13T02:13:29Z'
  export SLA_PROOF_RUN_URL='https://example.invalid/actions/runs/1234'
  unset SLA_PROOF_PROM_SINK SLA_PROOF_NOTE
}

# render <summary> <out-dir> — invoke the renderer with the ambient env.
render() {
  OUT="$(bash "$RENDER" --summary "$1" --out-dir "$2" 2>&1)"
  RC=$?
}

# ── Fixtures ────────────────────────────────────────────────────────────
# Built from the REAL checked-in acceptance export so the shapes are the
# ones k6 0.50.0 actually emits, not an invented approximation.
mkdir -p "$TMP/out"
python3 - "$HISTORICAL" "$TMP" <<'PY'
import json
import sys

src, tmp = sys.argv[1], sys.argv[2]
with open(src) as fh:
    base = json.load(fh)


def write(name, doc):
    with open("%s/%s.json" % (tmp, name), "w") as fh:
        json.dump(doc, fh)


def clone():
    return json.loads(json.dumps(base))


# 1. `historical` — the checked-in export verbatim. k6's DEFAULT trend
#    stats omit p(99), so this is the real-world shape that renders a
#    report claiming half of ADR-0009 while looking complete.
write("historical", base)

# 2. `pass` — a complete export: p(99) present, thresholds all held,
#    endpoint-tagged submetrics, soak-shaped volume.
ok = clone()
m = ok["metrics"]
m["http_req_duration"]["p(99)"] = 98.4
m["http_req_duration"]["thresholds"] = {"p(95)<200": False, "p(99)<500": False}
for ep, p95 in (("price", 44.1), ("batch", 120.7)):
    m["http_req_duration{endpoint:%s}" % ep] = {
        "avg": p95 / 2, "min": 1.0, "med": p95 / 2.5, "max": p95 * 8,
        "p(90)": p95 * 0.9, "p(95)": p95, "p(99)": p95 * 1.6}
m["http_reqs"] = {"count": 180321, "rate": 299.4}
write("pass", ok)

# 3. `breach` — same run, p95 over budget and k6 recording the breach.
#    A breached proof is still evidence and must still be written.
bad = json.loads(json.dumps(ok))
bm = bad["metrics"]
bm["http_req_duration"]["p(95)"] = 412.5
bm["http_req_duration"]["p(99)"] = 980.0
bm["http_req_duration"]["thresholds"] = {"p(95)<200": True, "p(99)<500": True}
bm["http_req_failed"] = {"value": 0.0042, "passes": 760, "fails": 179561,
                         "thresholds": {"rate<0.001": True}}
write("breach", bad)

# 4. `empty-run` — a run that started and issued nothing. Every headline
#    key is present, so only the volume check can catch it.
none = json.loads(json.dumps(ok))
none["metrics"]["http_reqs"] = {"count": 0, "rate": 0.0}
write("empty-run", none)

# 5. `no-thresholds` — a complete distribution that asserts nothing. Not
#    the canonical proof run; nothing in it says pass or fail.
mute = json.loads(json.dumps(ok))
for metric in mute["metrics"].values():
    if isinstance(metric, dict):
        metric.pop("thresholds", None)
write("no-thresholds", mute)

# 6. `not-a-summary` — valid JSON, no `metrics` object.
write("not-a-summary", {"hello": "world"})
PY

printf 'this is not json {' > "$TMP/corrupt.json"
: > "$TMP/zero-bytes.json"

# ── Refusals: nothing may be written ────────────────────────────────────
# Each case below must leave the output directory EMPTY. A renderer that
# writes a partial report on bad input recreates #378's false-evidence
# artifact under a new name.
refused_writes_nothing() { # refused_writes_nothing <name>
  local found
  found="$(find "$TMP/out" -name 'sla-proof-*.md' -type f)"
  if [ -z "$found" ]; then
    echo "ok: $1 wrote no report"
    pass=$((pass + 1))
  else
    echo "FAIL: $1 — a REFUSED render still wrote:" >&2
    printf '%s\n' "$found" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    rm -f "$TMP/out"/sla-proof-*.md
  fi
}

base_env

# THE load-bearing regression. k6 does not export p(99) unless
# --summary-trend-stats asks for it, and the real checked-in export proves
# it: rendering it would publish "p99 | n/a" inside a document that reads
# as a complete proof of a two-number SLA.
render "$TMP/historical.json" "$TMP/out"
expect 'the real p99-less export is REFUSED, not rendered with "n/a"' 2 'p(99)'
refused_writes_nothing 'the p99-less export'

render "$TMP/corrupt.json" "$TMP/out"
expect 'unparseable JSON → rc 2' 2 'not readable JSON'
refused_writes_nothing 'the corrupt export'

render "$TMP/zero-bytes.json" "$TMP/out"
expect 'a zero-byte export (the scenario died) → rc 2' 2 'missing or empty'
refused_writes_nothing 'the empty export'

render "$TMP/missing.json" "$TMP/out"
expect 'an absent export → rc 2' 2 'missing or empty'

render "$TMP/not-a-summary.json" "$TMP/out"
# shellcheck disable=SC2016  # the backticks are literal report/message text
expect 'JSON without a metrics object → rc 2' 2 'carries no `metrics` object'
refused_writes_nothing 'the non-summary JSON'

render "$TMP/empty-run.json" "$TMP/out"
expect 'a run that issued 0 requests is not evidence → rc 2' 2 'issued no requests'
refused_writes_nothing 'the zero-request run'

render "$TMP/no-thresholds.json" "$TMP/out"
expect 'an export declaring no thresholds → rc 2' 2 'declares no thresholds'
refused_writes_nothing 'the threshold-less export'

# ── Provenance is mandatory, field by field ─────────────────────────────
# "The artifact must name its own provenance." Each field is proven
# load-bearing individually, so dropping one from the workflow cannot
# quietly degrade the report into an unattributable number.
for field in SLA_PROOF_TARGET SLA_PROOF_SCENARIO SLA_PROOF_K6_VERSION \
             SLA_PROOF_COMMIT SLA_PROOF_STARTED_AT SLA_PROOF_ENDED_AT; do
  base_env
  unset "$field"
  render "$TMP/pass.json" "$TMP/out"
  expect "unset ${field} → rc 2 naming it" 2 "unset provenance: ${field}"
  refused_writes_nothing "the ${field}-less run"
done

base_env
export SLA_PROOF_STARTED_AT='last Tuesday'
render "$TMP/pass.json" "$TMP/out"
expect 'an unparseable run window → rc 2' 2 'is not an'
refused_writes_nothing 'the undated run'

# The report is committed to a PUBLIC repo and records the target
# verbatim, so a target carrying userinfo would publish a credential.
base_env
export SLA_PROOF_TARGET='https://loadkey:s3cr3t@api.staging.example.invalid/v1'
render "$TMP/pass.json" "$TMP/out"
expect 'a target URL carrying userinfo → rc 2' 2 'contains userinfo'
refused_writes_nothing 'the credentialed target'

# ── The happy path, and what the artifact must actually say ─────────────
base_env
render "$TMP/pass.json" "$TMP/out"
expect 'a complete export renders → rc 0 PASS' 0 'render-sla-proof: PASS'

# The workflow reads the written path as the LAST line of stdout
# (`proof_path="${out##*$'\n'}"` in k6-weekly.yml). If the renderer ever
# prints anything after it, the workflow copies and commits a diagnostic
# line instead of a file. Pin the contract on both exit paths.
if [ "${OUT##*$'\n'}" = "$TMP/out/sla-proof-2026-09-13.md" ]; then
  echo "ok: the written path is the last line of stdout (the workflow parses it)"
  pass=$((pass + 1))
else
  echo "FAIL: the last line of stdout is not the report path — k6-weekly.yml" \
       "would commit '${OUT##*$'\n'}'" >&2
  fail=$((fail + 1))
fi

REPORT="$TMP/out/sla-proof-2026-09-13.md"
if [ ! -s "$REPORT" ]; then
  echo "FAIL: the report was not written to the date derived from SLA_PROOF_ENDED_AT" >&2
  fail=$((fail + 1))
else
  echo "ok: the filename is derived from the run's END date, not from an argument"
  pass=$((pass + 1))

  # Provenance: every field an auditor needs to re-check the claim.
  assert_contains 'the report names the target' "$REPORT" \
    'https://api.staging.example.invalid/v1'
  assert_contains 'the report names the run window' "$REPORT" \
    '2026-09-13T02:00:11Z'
  assert_contains 'the report names the commit measured' "$REPORT" \
    '8bb11d38c0ffee1234567890abcdef0123456789'
  assert_contains 'the report names the measuring instrument' "$REPORT" \
    'k6 v0.50.0'
  assert_contains 'the report names the scenario' "$REPORT" \
    '06-mixed-realistic.js'
  assert_contains 'the report names the claim it is testing' "$REPORT" \
    'ADR-0009'
  assert_contains 'the report names the CI run that produced it' "$REPORT" \
    'https://example.invalid/actions/runs/1234'
  assert_contains 'the report is self-describing about its generator' "$REPORT" \
    'scripts/ci/render-sla-proof.sh'
  # shellcheck disable=SC2016  # the backticks are literal markdown in the report
  assert_contains 'the report carries both headline numbers' "$REPORT" \
    '| `http_req_duration` p99 |'
  assert_contains 'the report states the verdict up front' "$REPORT" \
    '**Verdict: PASS.**'
  # shellcheck disable=SC2016  # the backticks are literal markdown in the report
  assert_contains 'the report carries the per-endpoint breakdown it was given' \
    "$REPORT" '| `batch` |'
  assert_contains 'the report states what it does NOT prove' "$REPORT" \
    'does not prove'
  # The credential must never reach a committed file, in any case.
  assert_absent 'the report carries no API key' "$REPORT" 's3cr3t'
fi

# check-sla-evidence.sh must actually accept what the renderer produces.
# Two gates that disagree about the filename would leave the feed red
# forever while a perfectly good report sat in the directory.
OUT="$(SLA_EVIDENCE_DIR="$TMP/out" SLA_EVIDENCE_NOW=1789344000 \
  K6_TARGET=x STELLARINDEX_LOAD_API_KEY=y \
  bash "$PWD/scripts/ci/check-sla-evidence.sh" 2>&1)"; RC=$?
expect 'check-sla-evidence.sh accepts the rendered report as evidence' 0 \
  'sla-proof-2026-09-13.md'

# ── A breached run is still evidence and must still be retained ─────────
base_env
render "$TMP/breach.json" "$TMP/out"
expect 'a breached threshold → rc 1 (red run), report still written' 1 \
  'render-sla-proof: FAIL'
if [ "${OUT##*$'\n'}" = "$REPORT" ]; then
  echo "ok: the path is still the last line on the breach path"
  pass=$((pass + 1))
else
  echo "FAIL: on a breach the last line of stdout is '${OUT##*$'\n'}', not the" \
       "report path — the workflow would fail to publish the FAIL proof" >&2
  fail=$((fail + 1))
fi
if grep -qF -- '**Verdict: FAIL.**' "$REPORT" \
   && grep -qF -- 'BREACHED' "$REPORT"; then
  echo "ok: the breached run is written as a FAIL proof, not withheld"
  pass=$((pass + 1))
else
  echo "FAIL: the breached run did not overwrite the report with a FAIL verdict" >&2
  fail=$((fail + 1))
fi
if grep -qF -- '| 412.5 ms | FAIL |' "$REPORT"; then
  echo "ok: the breaching measurement is stated, not rounded away"
  pass=$((pass + 1))
else
  echo "FAIL: the report does not carry the breaching p95 with a FAIL verdict" >&2
  fail=$((fail + 1))
fi

# ── Determinism ─────────────────────────────────────────────────────────
# Same export + same provenance must give byte-identical output. A report
# that varies run to run cannot be diffed, and a diff is how a reader
# checks that a re-render did not quietly change a number.
base_env
render "$TMP/pass.json" "$TMP/out"
cp "$REPORT" "$TMP/first.md"
render "$TMP/pass.json" "$TMP/out"
if cmp -s "$TMP/first.md" "$REPORT"; then
  echo "ok: rendering is deterministic (byte-identical on re-run)"
  pass=$((pass + 1))
else
  echo "FAIL: two renders of the same export differ" >&2
  diff "$TMP/first.md" "$REPORT" | sed 's/^/    /' >&2
  fail=$((fail + 1))
fi

echo
echo "render-sla-proof-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ] || exit 1
