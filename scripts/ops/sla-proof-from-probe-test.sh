#!/usr/bin/env bash
# sla-proof-from-probe-test.sh — fixture tests for the probe-aggregate SLA
# proof generator (scripts/ops/sla-proof-from-probe.sh).
#
# The defect this closes: the weekly SLA proof had exactly one source, a
# k6 soak against a target that does not exist, so no dated proof report
# has ever landed on any branch and the scheduled run was red for weeks.
# The generator aggregates the probe series that were already there — and
# the whole risk of doing that is publishing a number that is not what it
# says it is. Percentiles do not average; a window with holes is not a
# clean window; a loopback probe is not an end-to-end measurement. So
# what is pinned here is mostly REFUSAL and mostly LABELLING.
#
# The generator runs once a week against a Prometheus no runner can reach
# without a port-forward, so these fixtures are the only place its
# behaviour is exercised on a PR. Every case is offline: the render cases
# feed it a saved extract, and the one fetch case stands up a stub HTTP
# server on loopback rather than reaching a real Prometheus.
#
# The base fixture, test/sla-probe/extract-2026-09-15.json, is a REAL
# capture from r1's Prometheus (2026-09-15, 7-day window, read through an
# ssh port-forward) rather than an invented approximation, so the label
# sets, the host label, the 18 API builds inside the window and the
# breaching runs are the ones the live system produced.
#
# Run: bash scripts/ops/sla-proof-from-probe-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
GEN="$PWD/scripts/ops/sla-proof-from-probe.sh"
REAL="$PWD/test/sla-probe/extract-2026-09-15.json"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

expect() { # expect <name> <want-rc> [<substring of output>]
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

assert_empty_dir() { # assert_empty_dir <name> <dir>
  local n
  n="$(find "$2" -type f | wc -l)"
  if [ "$n" -eq 0 ]; then
    echo "ok: $1"
    pass=$((pass + 1))
  else
    echo "FAIL: $1 — a refusal wrote $n file(s) into $2" >&2
    find "$2" -type f | sed 's/^/    /' >&2
    fail=$((fail + 1))
  fi
}

# The provenance every case starts from. Individual cases unset or
# override one field to prove each is load-bearing.
base_env() {
  export SLA_PROOF_PROBE_TARGET='http://localhost:3000/v1'
  export SLA_PROOF_PROBE_CONCURRENCY='1'
  export SLA_PROOF_COMMIT='c8a1b0daf0000000000000000000000000000000'
  unset SLA_PROOF_RUN_URL SLA_PROOF_NOTE
}

render() { # render <extract> <out-dir> [extra args...]
  local ex="$1" out="$2"; shift 2
  mkdir -p "$out"
  OUT="$(bash "$GEN" --extract "$ex" --out-dir "$out" "$@" 2>&1)"
  RC=$?
}

fetch() { # fetch <prom-url> <out-dir> [extra args...]
  local url="$1" out="$2"; shift 2
  mkdir -p "$out"
  OUT="$(bash "$GEN" --prom-url "$url" --out-dir "$out" "$@" 2>&1)"
  RC=$?
}

if [ ! -s "$REAL" ]; then
  echo "FAIL: the real extract fixture $REAL is missing — every case below" \
       "would pass vacuously on an invented shape" >&2
  exit 1
fi

# ── Doctored fixtures, all derived from the REAL capture ────────────────
python3 - "$REAL" "$TMP" <<'PY'
import copy
import json
import sys

src, tmp = sys.argv[1], sys.argv[2]
with open(src) as fh:
    base = json.load(fh)


def write(name, doc):
    with open("%s/%s.json" % (tmp, name), "w") as fh:
        json.dump(doc, fh)


def clone():
    return copy.deepcopy(base)


def each(doc, key, fn):
    for row in doc["series"].get(key, []):
        try:
            row["value"][1] = "%r" % fn(float(row["value"][1]),
                                        row.get("metric", {}))
        except (TypeError, ValueError):
            continue


# `clean` — every bound inside its target, availability at 100 %, no
# gaps. The ONLY fixture that may render a PASS; if this one ever goes
# red the failure is in the generator, not in r1.
clean = clone()
each(clean, "p95_max", lambda v, m: 42.0)
each(clean, "p99_max", lambda v, m: 120.0)
each(clean, "p95_typ", lambda v, m: 12.0)
each(clean, "p99_typ", lambda v, m: 20.0)
each(clean, "p50_max", lambda v, m: 8.0)
each(clean, "p95_min", lambda v, m: 1.0)
each(clean, "p95_over_frac", lambda v, m: 0.0)
each(clean, "p99_over_frac", lambda v, m: 0.0)
each(clean, "avail_min", lambda v, m: 100.0)
each(clean, "avail_under_frac", lambda v, m: 0.0)
each(clean, "avail_weighted_num", lambda v, m: 81400.0)
each(clean, "avail_weighted_den", lambda v, m: 814.0)
clean["series_gaps"] = []
# One build, so the multi-build caveat must switch to its singular form.
clean["series"]["api_versions"] = [
    {"metric": {"version": "v0.81.0"}, "value": [clean["last_sample"], "1"]}
]
write("clean", clean)

# `gapped` — byte-identical numbers to `clean`, plus one 4-hour hole in
# the series. A window with a hole in it cannot prove a claim about that
# window however good the surviving numbers look.
gapped = copy.deepcopy(clean)
gapped["series_gaps"] = [{"after": gapped["first_sample"] + 3600,
                          "seconds": 14400}]
write("gapped", gapped)

# `thin` — same as `clean` but only a fraction of the expected scrapes
# landed. Distinct from `gapped`: no single hole is wide enough to be
# reported, yet most of the window is missing.
thin = copy.deepcopy(clean)
each(thin, "scrape_count", lambda v, m: 5000.0)
write("thin", thin)

# `short` — the retention case. Prometheus here keeps the 15-day default
# and has been restarted, so a requested week is not necessarily a
# retained week.
short = copy.deepcopy(clean)
short["effective_seconds"] = 3600
short["first_sample"] = short["last_sample"] - 3600
write("short", short)

# `twohosts` — a second deployment's series merged in. One table
# describing two hosts describes neither.
twohosts = copy.deepcopy(clean)
for key in ("p95_max", "avail_min", "samples_avg"):
    extra = []
    for row in twohosts["series"].get(key, []):
        alt = copy.deepcopy(row)
        alt["metric"]["host"] = "r2"
        extra.append(alt)
    twohosts["series"][key].extend(extra)
write("twohosts", twohosts)

# Headline series absent, one family at a time. k6's export could omit
# p(99); this generator's equivalent is a quantile whose series never
# made it into the window.
for family in ("p95_max", "p99_max", "avail_min", "samples_avg"):
    doc = copy.deepcopy(clean)
    doc["series"][family] = []
    write("no_" + family, doc)

# One CELL unevaluable while every family is present (#513). The family
# refusal above cannot see these; the verdict is computed per cell, and
# before the fix a cell the series could not fill rendered "n/a" and
# counted as a pass. Each fixture is `clean` with exactly one headline
# cell broken, one family at a time and one failure shape at a time —
# a missing row, a non-numeric value, a NaN, an infinity from a zero
# denominator — so any single shape regressing is named by its case.


def drop_endpoint(doc, key, endpoint):
    doc["series"][key] = [r for r in doc["series"].get(key, [])
                          if r.get("metric", {}).get("endpoint") != endpoint]


def set_cell(doc, key, endpoint, text):
    for r in doc["series"].get(key, []):
        if r.get("metric", {}).get("endpoint") == endpoint:
            r["value"][1] = text


cell = copy.deepcopy(clean)
drop_endpoint(cell, "p95_max", "assets")
write("cell_p95_missing", cell)

cell = copy.deepcopy(clean)
set_cell(cell, "p99_max", "healthz", "not-a-number")
write("cell_p99_nonnumeric", cell)

cell = copy.deepcopy(clean)
set_cell(cell, "p99_max", "healthz", "NaN")
write("cell_p99_nan", cell)

cell = copy.deepcopy(clean)
drop_endpoint(cell, "avail_weighted_den", "price")
write("cell_avail_missing", cell)

cell = copy.deepcopy(clean)
set_cell(cell, "avail_weighted_num", "price", "+Inf")
write("cell_avail_inf", cell)

# An endpoint the probe issued requests to (it has a samples/run row)
# but that no latency or availability family names at all. Before the
# fix it vanished from the table entirely rather than standing in it
# with three empty cells.
cell = copy.deepcopy(clean)
for key in ("p95_max", "p99_max", "avail_min",
            "avail_weighted_num", "avail_weighted_den"):
    drop_endpoint(cell, key, "markets")
write("cell_endpoint_unmeasured", cell)

# `zero_samples` — the probe ran and issued nothing.
zero = copy.deepcopy(clean)
each(zero, "samples_avg", lambda v, m: 0.0)
write("zero_samples", zero)

# `public` — the same window measured against a public name instead of
# loopback. The network-path caveat must follow the configuration.
write("public", clean)

# `redated` — the filename is DERIVED from the last sample, never passed
# in. Move the window and the filename must move with it.
redated = copy.deepcopy(clean)
shift = 5 * 86400
redated["first_sample"] -= shift
redated["last_sample"] -= shift
write("redated", redated)

# `noseries` — structurally not an extract.
write("noseries", {"extract_version": 1, "generator": "x"})
PY

if [ ! -s "$TMP/clean.json" ]; then
  echo "FAIL: fixture generation produced nothing — every case below would" \
       "pass vacuously" >&2
  exit 1
fi

# ── Refusals: provenance ────────────────────────────────────────────────
# Each field is unset on its own, so none of them can be load-bearing by
# accident of another one also being required.
for v in SLA_PROOF_PROBE_TARGET SLA_PROOF_PROBE_CONCURRENCY SLA_PROOF_COMMIT; do
  base_env
  unset "$v"
  render "$TMP/clean.json" "$TMP/out-prov"
  expect "unset $v → rc 2 REFUSED" 2 "$v"
done
base_env
assert_empty_dir 'a provenance refusal writes NOTHING' "$TMP/out-prov"

# A committed report records the target verbatim, so a credential in it
# would be published. Refuse rather than strip.
base_env
export SLA_PROOF_PROBE_TARGET='http://probe:hunter2@api.example.invalid/v1'
render "$TMP/clean.json" "$TMP/out-cred"
expect 'target carrying userinfo → rc 2 REFUSED' 2 'userinfo'
assert_empty_dir 'the credential refusal writes NOTHING' "$TMP/out-cred"
base_env

# ── Refusals: inputs ────────────────────────────────────────────────────
render "$TMP/does-not-exist.json" "$TMP/out-missing"
expect 'a missing extract → rc 2 REFUSED' 2 'missing or empty'

printf 'not json at all\n' > "$TMP/garbage.json"
render "$TMP/garbage.json" "$TMP/out-garbage"
expect 'an unparseable extract → rc 2 REFUSED' 2 'not readable JSON'

render "$TMP/noseries.json" "$TMP/out-noseries"
expect "an extract with no series object → rc 2 REFUSED" 2 "carries no"

for family in p95_max p99_max avail_min samples_avg; do
  render "$TMP/no_${family}.json" "$TMP/out-absent"
  expect "absent headline series ${family} → rc 2 REFUSED" 2 'REFUSED'
done
assert_empty_dir 'an absent headline series writes NOTHING' "$TMP/out-absent"

render "$TMP/zero_samples.json" "$TMP/out-zero"
expect 'every endpoint at 0 samples/run → rc 2 REFUSED' 2 'measured nothing'

render "$TMP/twohosts.json" "$TMP/out-hosts"
expect 'two hosts merged into one table → rc 2 REFUSED' 2 'span 2 hosts'
render "$TMP/twohosts.json" "$TMP/out-hosts-ok" --host r1
expect 'the same two-host extract renders once a host is named' 0 'wrote'
HOSTED="$TMP/out-hosts-ok/$(ls "$TMP/out-hosts-ok")"
assert_contains 'the named host is the one recorded' "$HOSTED" "| Probe host | \`r1\` |"
assert_absent 'the other deployment is not silently folded in' "$HOSTED" 'r2'

render "$TMP/short.json" "$TMP/out-short"
expect 'an effective window under the floor → rc 2 REFUSED' 2 'below the --min-window'
assert_empty_dir 'the short-window refusal writes NOTHING' "$TMP/out-short"
render "$TMP/short.json" "$TMP/out-short-ok" --min-window 30m
expect 'the floor is real and configurable, not decorative' 0 'wrote'

OUT="$(bash "$GEN" --extract "$TMP/clean.json" --out-dir "$TMP/out-badwin" \
  --min-window 7 2>&1)"; RC=$?
expect 'a non-promql --min-window → rc 2 REFUSED' 2 'not a promql duration'

# ── Refusals: fetch mode ────────────────────────────────────────────────
# Nothing listens on port 1. A proof cannot be rendered from series that
# could not be read, and a half-read window must never be published as a
# whole one.
fetch 'http://127.0.0.1:1' "$TMP/out-unreach" --window 7d
expect 'an unreachable Prometheus → rc 2 REFUSED' 2 'unreachable'
assert_empty_dir 'the unreachable refusal writes NOTHING' "$TMP/out-unreach"

OUT="$(bash "$GEN" --prom-url 'http://127.0.0.1:1' --out-dir "$TMP/out-badw" \
  --window 'seven days' 2>&1)"; RC=$?
expect 'a non-promql --window → rc 2 REFUSED' 2 'not a promql duration'

# ── The PASS path, and the only fixture allowed to reach it ─────────────
render "$TMP/clean.json" "$TMP/out-clean"
expect 'a clean window inside every bound → rc 0 PROVEN' 0 'PROVEN — wrote'
CLEAN="$TMP/out-clean/$(ls "$TMP/out-clean")"
assert_contains 'the PASS report states its verdict' "$CLEAN" '**Verdict: PROVEN.**'
assert_contains 'the PASS report names the clean window' "$CLEAN" \
  'The window is continuous at the configured floor'
assert_contains 'a single build renders the singular provenance sentence' \
  "$CLEAN" 'It is evidence about the build named in Provenance'
assert_absent 'a single build does NOT render the moving-target caveat' \
  "$CLEAN" 'not a measurement of one build'
# The fully numeric case: a PROVEN report has every headline cell filled.
# This is what makes the cell cases below non-vacuous — a PASS that
# already carried an n/a would prove nothing about the arithmetic.
assert_absent 'the PASS report has no n/a in any headline cell' "$CLEAN" 'n/a |'
assert_absent 'the PASS report has no NOT PROVEN cell' "$CLEAN" '| NOT PROVEN |'
assert_absent 'the PASS report has no unevaluated-cell paragraph' "$CLEAN" \
  'could not be evaluated'
if [ "$(grep -c '| PROVEN |' "$CLEAN")" -eq 10 ]; then
  echo "ok: the PASS report carries a verdict on all 10 endpoints"
  pass=$((pass + 1))
else
  echo "FAIL: the PASS report does not carry 10 PROVEN endpoint rows" >&2
  fail=$((fail + 1))
fi

# ── One unevaluable cell is not a pass (#513) ───────────────────────────
# Every family is present, so none of the family refusals above fires;
# exactly one headline cell cannot be evaluated. The refusal is per
# family but the verdict is per cell, and before the fix that cell read
# "n/a" and contributed nothing to the conjunction — the week read PROVEN
# with part of one endpoint's SLA unmeasured. Each shape must render
# (a real measurement is retained), read NOT PROVEN, and name the cell.
for shape in cell_p95_missing cell_p99_nonnumeric cell_p99_nan \
             cell_avail_missing cell_avail_inf cell_endpoint_unmeasured; do
  render "$TMP/${shape}.json" "$TMP/out-${shape}"
  expect "${shape}: one unevaluable headline cell → rc 1, NOT PROVEN" 1 \
    'NOT PROVEN — wrote'
  expect "${shape}: the log names the unevaluable cell" 1 \
    'unevaluable headline cell(s)'
  CELLRPT="$TMP/out-${shape}/$(ls "$TMP/out-${shape}")"
  assert_contains "${shape}: the report refuses the PASS" "$CELLRPT" \
    '**Verdict: NOT PROVEN.**'
  assert_contains "${shape}: the report says why in the document" "$CELLRPT" \
    'could not be evaluated'
done

MISSING95="$TMP/out-cell_p95_missing/$(ls "$TMP/out-cell_p95_missing")"
assert_contains 'the n/a cell carries NOT PROVEN beside it, not a blank verdict' \
  "$MISSING95" "| \`assets\` | 814 | n/a | NOT PROVEN | 120.0 ms | PROVEN |"
assert_contains 'the unevaluated cell is named as endpoint + family' \
  "$MISSING95" "\`assets p95\`"
if [ "$(grep -c '| 42.0 ms | PROVEN | 120.0 ms | PROVEN | 100.000 % | PROVEN |' "$MISSING95")" -eq 9 ]; then
  echo "ok: the other nine endpoints keep their PROVEN cells"
  pass=$((pass + 1))
else
  echo "FAIL: an unevaluable cell on one endpoint changed the others' cells" >&2
  fail=$((fail + 1))
fi

INFAVAIL="$TMP/out-cell_avail_inf/$(ls "$TMP/out-cell_avail_inf")"
assert_absent 'an infinite availability is not printed as a number' \
  "$INFAVAIL" 'inf %'
assert_contains 'an infinite availability reads n/a and NOT PROVEN' \
  "$INFAVAIL" '| n/a | NOT PROVEN | 100.000 % |'

UNMEASURED="$TMP/out-cell_endpoint_unmeasured/$(ls "$TMP/out-cell_endpoint_unmeasured")"
assert_contains 'an endpoint with samples but no bound stays in the table' \
  "$UNMEASURED" "| \`markets\` | 814 | n/a | NOT PROVEN | n/a | NOT PROVEN | n/a | NOT PROVEN | n/a |"

# ── A window with holes is not a clean window ───────────────────────────
# `gapped` carries byte-identical numbers to `clean`. Only the hole
# differs, and it must be the difference between PROVEN and not.
render "$TMP/gapped.json" "$TMP/out-gapped"
expect 'identical numbers + one 4h hole → rc 1, NOT PROVEN' 1 'window not clean'
GAPPED="$TMP/out-gapped/$(ls "$TMP/out-gapped")"
assert_contains 'the gapped report says so in the document, not only the log' \
  "$GAPPED" '**This window is not clean.**'
assert_contains 'the gapped report names the hole' "$GAPPED" '4h'
assert_contains 'the gapped report refuses the PASS' "$GAPPED" \
  '**Verdict: NOT PROVEN.**'

render "$TMP/thin.json" "$TMP/out-thin"
expect 'coverage below the floor with no single wide hole → rc 1' 1 'window not clean'
THIN="$TMP/out-thin/$(ls "$TMP/out-thin")"
assert_contains 'the thin report reports its coverage' "$THIN" \
  'Scrapes of the probe series in the window'

# ── The labelling the whole change exists for ───────────────────────────
base_env
render "$REAL" "$TMP/out-real"
expect 'the real 7-day capture renders and is NOT PROVEN' 1 'NOT PROVEN'
REALRPT="$TMP/out-real/$(ls "$TMP/out-real")"

assert_contains 'the report says it is not a load test' "$REALRPT" \
  '**It is not a load test.**'
assert_contains 'the report names the concurrency that was NOT exercised' \
  "$REALRPT" 'Probe concurrency'
assert_contains 'the report says percentiles were not averaged' "$REALRPT" \
  '**Percentiles were not averaged, because percentiles do not average.**'
assert_contains 'the report names the statistic the headline actually is' \
  "$REALRPT" 'largest per-run percentile in the window'
assert_contains 'the report refuses a cross-endpoint roll-up' "$REALRPT" \
  '**Nothing is rolled up across endpoints.**'
assert_absent 'the report publishes no overall/merged percentile' "$REALRPT" \
  'Overall p95'
assert_contains 'the report states the sample count' "$REALRPT" 'samples/run'
assert_contains 'the report states the measured window' "$REALRPT" \
  '| Window measured (UTC) |'
assert_contains 'the report states the scrape coverage' "$REALRPT" \
  'Scrapes of the probe series in the window'
assert_contains 'the report separates a dead probe from a dead scrape' \
  "$REALRPT" 'Longest stretch with no recorded passing run'
assert_contains 'the real window spanned many builds and the report says so' \
  "$REALRPT" 'not a measurement of one build'
assert_contains 'freshness is reported without inventing a stricter bar' \
  "$REALRPT" '### Freshness — reported, not gated'

# The loopback caveat is DERIVED from the recorded target, not asserted.
assert_contains 'a loopback target yields the excludes-the-edge caveat' \
  "$REALRPT" "A client's p95 is strictly"
assert_contains 'the loopback caveat names the excluded hops' "$REALRPT" \
  'traverse DNS, TLS, the reverse proxy or any CDN'

base_env
export SLA_PROOF_PROBE_TARGET='https://api.stellarindex.io/v1'
render "$TMP/public.json" "$TMP/out-public"
expect 'a public probe target still renders' 0 'wrote'
PUBLIC="$TMP/out-public/$(ls "$TMP/out-public")"
assert_absent 'a public target does NOT claim the edge was excluded' \
  "$PUBLIC" "A client's p95 is strictly"
assert_contains 'a public target says what it DID traverse instead' \
  "$PUBLIC" 'which is not a loopback address'
base_env

# ── The filename is derived, never passed in ────────────────────────────
render "$TMP/redated.json" "$TMP/out-redated"
expect 'a window moved five days back renders' 0 'wrote'
REDATED="$(ls "$TMP/out-redated")"
CLEANNAME="$(ls "$TMP/out-clean")"
if [ "$REDATED" != "$CLEANNAME" ]; then
  echo "ok: the report filename follows the window it measured ($CLEANNAME -> $REDATED)"
  pass=$((pass + 1))
else
  echo "FAIL: moving the measured window did not move the filename —" \
       "a report whose name disagrees with its window is the mislabelling" \
       "this generator exists to prevent" >&2
  fail=$((fail + 1))
fi

# ── Fetch mode against a stub, so build_extract is not untested ─────────
# The promql construction, the window clamping and the gap detection all
# live in the fetch path and are reached by nothing above. The stub
# answers query_range with a series that STOPS for two hours in the
# middle, so the gap detector has something real to find.
STUB_PORT_FILE="$TMP/stub.port"
# Landed in a file and run from there rather than backgrounded behind a
# heredoc: bash echoes the entire job text when it reports killing the
# job, which buries the result line under a hundred lines of python.
cat > "$TMP/stub.py" <<'PY'
import json
import os
import re
import sys
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs, urlparse

tmp, port_file = sys.argv[1], sys.argv[2]
NOW = 1789400000.0
WINDOW = 7 * 86400
STEP = 300
ENDPOINTS = ["assets", "healthz", "price"]

# One two-hour hole, three days in.
GAP_START = NOW - WINDOW + 3 * 86400
GAP_END = GAP_START + 2 * 3600


def range_values():
    out, t = [], NOW - WINDOW
    while t <= NOW:
        if not (GAP_START <= t < GAP_END):
            out.append([t, "17"])
        t += STEP
    return out


def vec(value, extra=None):
    rows = []
    for ep in ENDPOINTS:
        metric = {"endpoint": ep, "host": "r1", "instance": "localhost:9100",
                  "job": "node_exporter"}
        metric.update(extra or {})
        rows.append({"metric": metric, "value": [NOW, str(value)]})
    return rows


def answer(expr):
    # Enough shape for the renderer; the point of this case is the fetch
    # path, not the arithmetic, which the extract cases already cover.
    if expr.startswith("count_over_time") and "[1h]" in expr:
        return [{"metric": {}, "value": [NOW, "240"]}]
    if "count_over_time" in expr:
        return [{"metric": {}, "value": [NOW, "40000"]}]
    if "availability_pct" in expr and "samples" in expr:
        return vec(81400.0)
    if "availability_pct" in expr and "bool" in expr:
        return vec(0.0)
    if "availability_pct" in expr:
        return vec(100.0)
    if "samples" in expr:
        return vec(814)
    if "bool" in expr:
        return vec(0.0)
    if "freshness" in expr:
        return []
    if "last_pass_timestamp" in expr:
        return [{"metric": {}, "value": [NOW, "600"]}]
    if "unit_failed" in expr:
        return [{"metric": {}, "value": [NOW, "0"]}]
    if "run_duration" in expr:
        return [{"metric": {}, "value": [NOW, "30"]}]
    if "binary_version_info" in expr:
        return [{"metric": {"version": "v0.81.0"}, "value": [NOW, "1"]}]
    if "quantile=\"0.99\"" in expr:
        return vec(120.0)
    if "quantile=\"0.5\"" in expr:
        return vec(8.0)
    return vec(42.0)


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def do_GET(self):
        u = urlparse(self.path)
        q = parse_qs(u.query)
        expr = (q.get("query") or [""])[0]
        if u.path.endswith("/query_range"):
            data = {"resultType": "matrix", "result": [
                {"metric": {"endpoint": "assets", "host": "r1",
                            "quantile": "0.95"},
                 "values": range_values()}]}
        else:
            data = {"resultType": "vector", "result": answer(expr)}
        body = json.dumps({"status": "success", "data": data}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


srv = HTTPServer(("127.0.0.1", 0), H)
with open(port_file, "w") as fh:
    fh.write(str(srv.server_address[1]))
srv.serve_forever()
PY
python3 "$TMP/stub.py" "$TMP" "$STUB_PORT_FILE" >/dev/null 2>&1 &
STUB_PID=$!
trap 'kill "$STUB_PID" 2>/dev/null; wait "$STUB_PID" 2>/dev/null; rm -rf "$TMP"' EXIT

for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
  [ -s "$STUB_PORT_FILE" ] && break
  sleep 0.2
done
STUB_PORT="$(cat "$STUB_PORT_FILE" 2>/dev/null || true)"

if [ -z "$STUB_PORT" ]; then
  echo "FAIL: the stub Prometheus never bound a port — the fetch path is" \
       "untested, which is where the promql and the window clamp live" >&2
  fail=$((fail + 1))
else
  base_env
  fetch "http://127.0.0.1:${STUB_PORT}" "$TMP/out-fetch" --window 7d \
    --extract-out "$TMP/fetched.json"
  expect 'fetch mode renders, and the two-hour hole refuses the PASS' 1 \
    'window not clean'
  FETCHED="$TMP/out-fetch/$(ls "$TMP/out-fetch")"
  assert_contains 'the fetch path found the hole it was given' "$FETCHED" \
    '**This window is not clean.**'
  assert_contains 'the fetch path measured the scrape interval' "$FETCHED" \
    '| Scrape interval (from the last hour) | 15 s |'
  assert_contains 'the extract records the promql it ran' "$TMP/fetched.json" \
    'max_over_time'
  assert_contains 'the extract records the host filter field' \
    "$TMP/fetched.json" 'host_filter'

  # Re-rendering the extract must reproduce the same verdict with no
  # network at all — the property the report tells its reader it has.
  render "$TMP/fetched.json" "$TMP/out-refetch"
  expect 'the saved extract re-renders offline to the same verdict' 1 \
    'window not clean'
fi

echo
echo "sla-proof-from-probe-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ] || exit 1
