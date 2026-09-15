#!/usr/bin/env bash
# sla-proof-from-probe.sh — render the dated SLA proof report from the
# stellarindex-sla-probe series already in r1's Prometheus, instead of
# from a k6 load run against a target that does not exist.
#
# WHY THIS EXISTS. The weekly SLA proof had exactly one source: a k6
# soak against K6_TARGET_STAGING. That secret is deliberately unset, the
# scenarios refuse a production target by design
# (test/load/scenarios/lib/env.js), and the canonical scenario is a
# 300 rps / 10 min soak — so pointing it at the single production host
# was never on the table. k6-weekly.yml's own header offered two ways
# out, "provision a production-shaped target, or retire this workflow",
# and neither happened; the schedule stayed red for weeks and the
# scheduled-control sweep flagged it.
#
# There is a third source, and it has been running the whole time.
# stellarindex-sla-probe fires every 15 minutes on r1, writes
# per-endpoint p50/p95/p99, availability, freshness and sample counts
# into node_exporter's textfile collector, and Prometheus has been
# scraping them at 15 s for as long as its retention holds. That is a
# real, continuous measurement of served latency across ten endpoints.
# It is NOT a load test, and this script's whole job is to render it as
# proof of exactly what it measured and of nothing more.
#
# THE RULE IT ENFORCES, unchanged from scripts/ci/render-sla-proof.sh:
# an unlabelled number is not evidence. The report is refused outright
# (rc 2, nothing written) unless the run can name its own provenance and
# unless the series actually carry the numbers the claim is about. A run
# that cannot produce evidence must fail loudly; it must never publish a
# partial report that reads as a pass.
#
# WHAT THE WEEKLY FIGURE IS. Percentiles do not average. The probe
# exports one p50/p95/p99 PER RUN and never the underlying samples, so
# the request-level population percentile for the week is not
# recoverable from these series and this script does not pretend to
# compute it. What it publishes instead is a bound that is exactly true:
#
#   the pooled p95 over the window <= max(per-run p95)
#
# because every run has at least 95 % of its own samples at or below its
# own p95, so pooling them keeps at least 95 % at or below the largest
# of those values — for any run sizes, no uniformity assumed. The bound
# is one-directional and the report says so: a max under the target
# PROVES the target held; a max over it proves only that the bound is
# uninformative, never that the target was missed. Nothing in the report
# averages a percentile, and nothing rolls percentiles up across
# endpoints.
#
# Usage:
#   sla-proof-from-probe.sh [--prom-url URL] [--window 7d] [--host NAME]
#                           [--out-dir DIR] [--extract-out FILE]
#   sla-proof-from-probe.sh --extract FILE [--out-dir DIR]
#
# Two modes, one renderer. In FETCH mode the script queries Prometheus,
# builds a self-contained extract of every series it read, and renders
# it. In --extract mode it renders a previously-saved extract and does
# no network at all — which is how scripts/ops/sla-proof-from-probe-test.sh
# exercises every refusal path on a PR rather than once a week.
#
# The filename is DERIVED from the last sample in the effective window,
# never passed in: a report whose name disagrees with the window it
# measured is the mislabelling this script exists to prevent. The written
# path is echoed as the last line of stdout.
#
# Required provenance (env; every one must be non-empty). None of these
# is recoverable from the series, and each answers a question the
# numbers are meaningless without:
#   SLA_PROOF_PROBE_TARGET       base URL the probe requests. The report
#                                DERIVES its network-path caveat from
#                                this — a loopback target excludes TLS,
#                                the reverse proxy and any CDN, and the
#                                report must say which it was.
#   SLA_PROOF_PROBE_CONCURRENCY  the probe's -concurrency. This is the
#                                load condition that was NOT exercised;
#                                stating it is the difference between a
#                                latency measurement and a latency claim.
#   SLA_PROOF_COMMIT             the commit/release the measured API was
#                                serving.
#
# Optional provenance:
#   SLA_PROOF_RUN_URL   CI run permalink (recorded as a local run if unset)
#   SLA_PROOF_NOTE      free text appended to "Notes / caveats"
#
# Exit codes — the workflow branches on all three, so they are a contract
# and they mirror render-sla-proof.sh's:
#   0  PASS     report written; every per-endpoint bound held across a
#               window with no gaps.
#   1  FAIL     report written; at least one bound did not hold, or the
#               window has gaps that stop it proving anything. Still
#               evidence — a failing proof is a real measurement and must
#               be retained — but the run is red.
#   2  REFUSED  nothing written: the inputs cannot support a labelled
#               claim (Prometheus unreachable, headline series absent,
#               provenance missing, more than one host in the data,
#               effective window below the floor, credential in the
#               target URL).
set -euo pipefail

cd "$(dirname "$0")/../.."

usage() {
  cat >&2 <<'EOF'
usage: sla-proof-from-probe.sh [--prom-url URL] [--window 7d] [--host NAME]
                               [--out-dir DIR] [--extract-out FILE]
                               [--min-window 24h] [--min-coverage 0.95]
       sla-proof-from-probe.sh --extract FILE [--out-dir DIR]

Renders docs/operations/sla-proof-<YYYY-MM-DD>.md from the
stellarindex_sla_probe_* series in a Prometheus. The date comes from the
last sample inside the effective window. See the header of this script
for the required provenance env vars.
EOF
}

PROM_URL="${SLA_PROOF_PROM_URL:-http://127.0.0.1:9090}"
WINDOW="7d"
HOST=""
OUT_DIR="docs/operations"
EXTRACT_IN=""
EXTRACT_OUT=""
MIN_WINDOW="24h"
MIN_COVERAGE="0.95"

while [ $# -gt 0 ]; do
  case "$1" in
    --prom-url)     PROM_URL="${2:-}"; shift 2 ;;
    --window)       WINDOW="${2:-}"; shift 2 ;;
    --host)         HOST="${2:-}"; shift 2 ;;
    --out-dir)      OUT_DIR="${2:-}"; shift 2 ;;
    --extract)      EXTRACT_IN="${2:-}"; shift 2 ;;
    --extract-out)  EXTRACT_OUT="${2:-}"; shift 2 ;;
    --min-window)   MIN_WINDOW="${2:-}"; shift 2 ;;
    --min-coverage) MIN_COVERAGE="${2:-}"; shift 2 ;;
    -h|--help)      usage; exit 0 ;;
    *) echo "sla-proof-from-probe: unknown argument '$1'" >&2; usage; exit 2 ;;
  esac
done

if [ -n "$EXTRACT_IN" ] && [ ! -s "$EXTRACT_IN" ]; then
  cat >&2 <<EOF
sla-proof-from-probe: REFUSED (rc=2) — the extract '${EXTRACT_IN}' is
  missing or empty, so this run read nothing. A run that cannot produce
  evidence must fail; it must not publish a report.
EOF
  exit 2
fi

# ── Provenance gate ─────────────────────────────────────────────────────
# Every field here answers one of "against what, under what load, at
# which commit". A report missing any of them is a number without a claim
# attached.
missing=""
for v in SLA_PROOF_PROBE_TARGET SLA_PROOF_PROBE_CONCURRENCY SLA_PROOF_COMMIT; do
  if [ -z "${!v:-}" ]; then
    missing="${missing:+${missing} }${v}"
  fi
done
if [ -n "$missing" ]; then
  cat >&2 <<EOF
sla-proof-from-probe: REFUSED (rc=2) — unset provenance: ${missing}.
  An SLA proof states what was measured, against what, under which load
  condition and at which commit. None of those is recoverable from the
  Prometheus series, so without them the numbers are unattributable and
  no report is written.
EOF
  exit 2
fi

# A committed report is public and records the probe target verbatim, so
# a URL carrying userinfo would publish a credential. Refuse rather than
# silently strip: the caller must fix the input.
case "$SLA_PROOF_PROBE_TARGET" in
  *"@"*)
    cat >&2 <<'EOF'
sla-proof-from-probe: REFUSED (rc=2) — SLA_PROOF_PROBE_TARGET contains
  userinfo ("user:pass@host"). The proof report is committed to a public
  repo and records the target verbatim. Pass a credential-free URL; the
  probe's key travels in STELLARINDEX_PROBE_API_KEY, never in the target.
EOF
    exit 2 ;;
esac

mkdir -p "$OUT_DIR"

export SLA_PROBE_PROM_URL="$PROM_URL"
export SLA_PROBE_WINDOW="$WINDOW"
export SLA_PROBE_HOST="$HOST"
export SLA_PROBE_OUT_DIR="$OUT_DIR"
export SLA_PROBE_EXTRACT_IN="$EXTRACT_IN"
export SLA_PROBE_EXTRACT_OUT="$EXTRACT_OUT"
export SLA_PROBE_MIN_WINDOW="$MIN_WINDOW"
export SLA_PROBE_MIN_COVERAGE="$MIN_COVERAGE"

# Export explicitly so the renderer below sees them whether the caller
# exported them, set them inline, or assigned them as plain shell
# variables in a wrapper.
export SLA_PROOF_PROBE_TARGET SLA_PROOF_PROBE_CONCURRENCY SLA_PROOF_COMMIT
export SLA_PROOF_RUN_URL="${SLA_PROOF_RUN_URL:-}"
export SLA_PROOF_NOTE="${SLA_PROOF_NOTE:-}"

set +e
python3 - <<'PY'
import hashlib
import json
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

REFUSED = 2

# Targets. 200 / 500 ms and 99.9 % are what ADR-0009 states as the
# headline service SLA and what the probe's own verdict and
# deploy/monitoring/rules/sla-probe.yml already assert, so they are the
# bound this report gates on. ADR-0009's per-endpoint table is tighter
# for the smoke endpoints and silent on the catalogue ones; the rendered
# report says so rather than quietly inventing a bar.
P95_TARGET_MS = 200.0
P99_TARGET_MS = 500.0
AVAILABILITY_TARGET_PCT = 99.9

# Freshness bounds, from cmd/stellarindex-sla-probe/main.go. /price serves
# the ADR-0015 closed bucket and is structurally 30-150 s old by design;
# the 30 s pricing SLA is served and measured on /price/tip.
FRESHNESS_TARGET_SEC = {"price": 150.0, "price-tip": 30.0}

HTTP_TIMEOUT = 120


def refuse(msg):
    sys.stderr.write("sla-proof-from-probe: REFUSED (rc=2) — %s\n" % msg)
    sys.exit(REFUSED)


def parse_duration(text, what):
    """promql-style duration -> seconds. Deliberately strict."""
    m = re.fullmatch(r"(\d+)(s|m|h|d|w)", (text or "").strip())
    if not m:
        refuse("%s='%s' is not a promql duration (e.g. 90m, 24h, 7d, 2w)."
               % (what, text))
    mult = {"s": 1, "m": 60, "h": 3600, "d": 86400, "w": 604800}[m.group(2)]
    secs = int(m.group(1)) * mult
    if secs <= 0:
        refuse("%s='%s' is not a positive duration." % (what, text))
    return secs


def human_duration(secs):
    secs = int(round(secs))
    days, rem = divmod(secs, 86400)
    hours, rem = divmod(rem, 3600)
    mins = rem // 60
    parts = []
    if days:
        parts.append("%dd" % days)
    if hours:
        parts.append("%dh" % hours)
    if mins and not days:
        parts.append("%dm" % mins)
    return " ".join(parts) or "%ds" % secs


def iso(ts):
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(ts))


# ── Transport ───────────────────────────────────────────────────────────
class Prom(object):
    def __init__(self, base):
        self.base = base.rstrip("/")

    def call(self, path, params):
        url = "%s/api/v1/%s?%s" % (self.base, path, urllib.parse.urlencode(params))
        try:
            with urllib.request.urlopen(url, timeout=HTTP_TIMEOUT) as fh:
                doc = json.loads(fh.read().decode("utf-8"))
        except urllib.error.HTTPError as exc:
            refuse("Prometheus at %s answered HTTP %s for %s. Nothing was "
                   "written." % (self.base, exc.code, path))
        except (urllib.error.URLError, OSError) as exc:
            refuse("Prometheus at %s is unreachable (%s). A proof cannot be "
                   "rendered from series that could not be read, and a "
                   "half-read window must never be published as a whole one."
                   % (self.base, exc))
        except ValueError as exc:
            refuse("Prometheus at %s did not answer JSON (%s)." % (self.base, exc))
        if doc.get("status") != "success":
            refuse("Prometheus rejected a query: %s %s"
                   % (doc.get("errorType"), doc.get("error")))
        return doc["data"]


# ── Fetch ───────────────────────────────────────────────────────────────
PROBE_METRICS = (
    "stellarindex_sla_probe_latency_ms",
    "stellarindex_sla_probe_availability_pct",
    "stellarindex_sla_probe_samples",
)


def selector(metric, host, extra=""):
    parts = []
    if host:
        parts.append('host="%s"' % host)
    if extra:
        parts.append(extra)
    return "%s{%s}" % (metric, ",".join(parts))


def build_extract(prom_url, window_text, host):
    prom = Prom(prom_url)
    window_secs = parse_duration(window_text, "--window")
    queried_at = time.time()

    lat = selector("stellarindex_sla_probe_latency_ms", host, 'quantile="0.95"')

    # Pass 1 — where does the data actually START and END? Prometheus'
    # retention here is the 15-day default (retention.time is 0s, which
    # means "use the default", not "unbounded"), and the unit has been
    # restarted, so a 7-day window is not a fact to assume. The effective
    # window is measured, and every aggregate below is then evaluated
    # over the effective window rather than the requested one — otherwise
    # the report's own label would describe a range its numbers do not
    # cover.
    step = max(300, int(window_secs / 2000) + 1)
    data = prom.call("query_range", {
        "query": lat,
        "start": "%.3f" % (queried_at - window_secs),
        "end": "%.3f" % queried_at,
        "step": "%ds" % step,
    })
    series = data.get("result") or []
    if not series:
        refuse("no %s samples exist in the last %s at %s. The probe series "
               "this report is made of are absent, so there is nothing to "
               "prove and nothing is written."
               % (lat, window_text, prom_url))

    stamps = sorted({float(ts) for s in series for ts, _ in s["values"]})
    first_ts, last_ts = stamps[0], stamps[-1]

    # Series gaps: the textfile collector holds the last probe output
    # until the next run rewrites it, so a hole in THIS series means
    # scraping stopped, not that the probe did. Probe outages are
    # measured separately, below.
    gaps = []
    for a, b in zip(stamps, stamps[1:]):
        if b - a > 2 * step:
            gaps.append({"after": a, "seconds": b - a})

    eff_secs = max(int(round(last_ts - first_ts)), step)
    rng = "%ds" % eff_secs

    def q(expr):
        return prom.call("query", {"query": expr, "time": "%.3f" % last_ts}).get("result") or []

    # Subquery resolution. Every fraction-of-window and weighted figure
    # below is a subquery, and its step decides what "a sample" means.
    # Measured from the last hour of the series itself rather than
    # assumed, so a re-tuned scrape interval does not silently re-weight
    # the arithmetic.
    got = q("count_over_time(%s[1h])" % lat)
    try:
        per_hour = float((got[0].get("value") or [None, None])[1])
    except (IndexError, TypeError, ValueError):
        per_hour = 0.0
    sub_step = int(round(3600.0 / per_hour)) if per_hour > 0 else 15
    sub_step = max(sub_step, 1)

    lat_all = selector("stellarindex_sla_probe_latency_ms", host)
    avail = selector("stellarindex_sla_probe_availability_pct", host)
    samples = selector("stellarindex_sla_probe_samples", host)
    fresh = selector("stellarindex_sla_probe_freshness_sec", host)
    lastpass = selector("stellarindex_sla_probe_last_pass_timestamp", host)
    failed = selector("stellarindex_sla_probe_unit_failed", host)
    rundur = selector("stellarindex_sla_probe_run_duration_seconds", host)
    verinfo = selector("stellarindex_binary_version_info", host,
                       'binary="stellarindex-api"')

    queries = {
        # Bounds. max/min are immune to how many scrapes each run is held
        # for; the descriptive quantiles below are not, and say so.
        "p50_max": "max_over_time(%s[%s])" % (
            selector("stellarindex_sla_probe_latency_ms", host, 'quantile="0.5"'), rng),
        "p95_max": "max_over_time(%s[%s])" % (lat, rng),
        "p99_max": "max_over_time(%s[%s])" % (
            selector("stellarindex_sla_probe_latency_ms", host, 'quantile="0.99"'), rng),
        "p95_min": "min_over_time(%s[%s])" % (lat, rng),
        "p95_typ": "quantile_over_time(0.5, %s[%s])" % (lat, rng),
        "p99_typ": "quantile_over_time(0.5, %s[%s])" % (
            selector("stellarindex_sla_probe_latency_ms", host, 'quantile="0.99"'), rng),
        "p95_over_frac": "avg_over_time((%s > bool %g)[%s:%ds])"
                         % (lat, P95_TARGET_MS, rng, sub_step),
        "p99_over_frac": "avg_over_time((%s > bool %g)[%s:%ds])"
                         % (selector("stellarindex_sla_probe_latency_ms", host,
                                     'quantile="0.99"'), P99_TARGET_MS, rng, sub_step),
        "avail_min": "min_over_time(%s[%s])" % (avail, rng),
        # Availability is a RATIO over the window, not a percentile, so
        # unlike the latency rows it is estimated rather than bounded —
        # see the method note in the rendered report. Numerator and
        # denominator share one subquery step so the weighting cancels.
        "avail_weighted_num": "avg_over_time((%s * %s)[%s:%ds])"
                              % (avail, samples, rng, sub_step),
        "avail_weighted_den": "avg_over_time((%s)[%s:%ds])"
                              % (samples, rng, sub_step),
        "avail_under_frac": "avg_over_time((%s < bool %g)[%s:%ds])"
                            % (avail, AVAILABILITY_TARGET_PCT, rng, sub_step),
        "samples_avg": "avg_over_time(%s[%s])" % (samples, rng),
        "samples_min": "min_over_time(%s[%s])" % (samples, rng),
        "samples_max": "max_over_time(%s[%s])" % (samples, rng),
        "scrape_count": "count_over_time(%s[%s])" % (lat, rng),
        "scrape_count_1h": "count_over_time(%s[1h])" % lat,
        "fresh_max": "max_over_time(%s[%s])" % (fresh, rng),
        "fresh_typ": "quantile_over_time(0.5, %s[%s])" % (fresh, rng),
        "passing_runs": "changes(%s[%s])" % (lastpass, rng),
        "pass_present": "count_over_time(%s[%s])" % (lastpass, rng),
        "no_pass_max_sec": "max_over_time((time() - %s)[%s:%ds])"
                           % (lastpass, rng, sub_step),
        "unit_failed_frac": "avg_over_time(%s[%s])" % (failed, rng),
        "run_duration_max": "max_over_time(%s[%s])" % (rundur, rng),
        "run_duration_min": "min_over_time(%s[%s])" % (rundur, rng),
        "api_versions": "count by (version) (count_over_time(%s[%s]))" % (verinfo, rng),
    }

    return {
        "extract_version": 1,
        "generator": "scripts/ops/sla-proof-from-probe.sh",
        "prom_url": prom_url,
        "host_filter": host,
        "window_requested": window_text,
        "window_requested_seconds": window_secs,
        "queried_at": queried_at,
        "first_sample": first_ts,
        "last_sample": last_ts,
        "effective_seconds": eff_secs,
        "range_step_seconds": step,
        "subquery_step_seconds": sub_step,
        "series_gaps": gaps,
        "queries": queries,
        "series": {name: q(expr) for name, expr in sorted(queries.items())},
    }


# ── Read the inputs ─────────────────────────────────────────────────────
extract_in = os.environ.get("SLA_PROBE_EXTRACT_IN", "")
if extract_in:
    try:
        with open(extract_in) as fh:
            ex = json.load(fh)
    except (OSError, ValueError) as exc:
        refuse("the extract %s is not readable JSON (%s). Nothing was "
               "written." % (extract_in, exc))
    if not isinstance(ex, dict) or "series" not in ex:
        refuse("the extract %s carries no `series` object — it is not an "
               "extract this renderer produced." % extract_in)
    extract_source = "re-rendered from %s" % extract_in
else:
    ex = build_extract(os.environ["SLA_PROBE_PROM_URL"],
                       os.environ["SLA_PROBE_WINDOW"],
                       os.environ["SLA_PROBE_HOST"])
    extract_source = "read live from %s" % ex["prom_url"]

series = ex.get("series") or {}
if not isinstance(series, dict):
    refuse("the extract's `series` is not an object.")

# --host narrows the render in BOTH modes. In fetch mode the queries
# already carried the matcher and this is a no-op; in --extract mode it is
# the only place it can be applied, and without it a saved multi-host
# extract could never be rendered at all.
host_arg = os.environ.get("SLA_PROBE_HOST", "").strip()
if host_arg:
    for name, got in list(series.items()):
        if not isinstance(got, list):
            continue
        series[name] = [
            r for r in got
            if (r.get("metric") or {}).get("host", host_arg) == host_arg
        ]
    ex["host_filter"] = host_arg


def rows(name):
    got = series.get(name)
    return got if isinstance(got, list) else []


def by_endpoint(name):
    out = {}
    for r in rows(name):
        ep = (r.get("metric") or {}).get("endpoint")
        try:
            out[ep] = float((r.get("value") or [None, None])[1])
        except (TypeError, ValueError):
            continue
    return out


def scalar(name, default=None):
    for r in rows(name):
        try:
            return float((r.get("value") or [None, None])[1])
        except (TypeError, ValueError):
            return default
    return default


# ── Refusals that need the data ─────────────────────────────────────────
# One host, or none. Two hosts silently merged into one table is a
# cross-deployment roll-up wearing the name of a single deployment.
hosts = sorted({
    (r.get("metric") or {}).get("host")
    for name in ("p95_max", "avail_min", "samples_avg")
    for r in rows(name)
    if (r.get("metric") or {}).get("host")
})
if len(hosts) > 1 and not ex.get("host_filter"):
    refuse("the series span %d hosts (%s). Rolling several deployments into "
           "one table publishes a number that describes none of them. Re-run "
           "with --host <name>." % (len(hosts), ", ".join(hosts)))
probe_host = ex.get("host_filter") or (hosts[0] if hosts else "")

p95_max = by_endpoint("p95_max")
p99_max = by_endpoint("p99_max")
avail_min = by_endpoint("avail_min")
samples_avg = by_endpoint("samples_avg")

for label, table in (("stellarindex_sla_probe_latency_ms{quantile=\"0.95\"}", p95_max),
                     ("stellarindex_sla_probe_latency_ms{quantile=\"0.99\"}", p99_max),
                     ("stellarindex_sla_probe_availability_pct", avail_min),
                     ("stellarindex_sla_probe_samples", samples_avg)):
    if not table:
        refuse("the window carries no %s series. ADR-0009 claims p95 <= 200 ms "
               "AND p99 <= 500 ms AND availability; a report rendered without "
               "one of them would read \"n/a\" for part of the SLA while "
               "looking complete." % label)

if not any(v > 0 for v in samples_avg.values()):
    refuse("every endpoint records 0 samples per run. A probe that issued no "
           "requests measured nothing and is not evidence.")

eff_secs = int(ex.get("effective_seconds") or 0)
min_window = parse_duration(os.environ.get("SLA_PROBE_MIN_WINDOW", "24h"),
                            "--min-window")
if eff_secs < min_window:
    refuse("the retained series span only %s (%s -> %s), below the --min-window "
           "floor of %s. Prometheus here keeps the 15-day default and has been "
           "restarted, so a requested week is not necessarily a retained week. "
           "A window this short cannot carry a weekly claim, and shortening the "
           "claim silently to fit the data is the mislabelling this script "
           "refuses."
           % (human_duration(eff_secs), iso(ex["first_sample"]),
              iso(ex["last_sample"]), human_duration(min_window)))

# ── Coverage ────────────────────────────────────────────────────────────
# Two different outages, deliberately measured apart. A hole in the series
# means SCRAPING stopped. A frozen last_pass_timestamp means the PROBE
# stopped (or every run since failed) while node_exporter kept re-serving
# the last textfile it was given — so the series stays continuous and
# looks healthy. Only the second one is invisible without asking.
step = int(ex.get("range_step_seconds") or 300)
scrape_1h = scalar("scrape_count_1h", 0.0) or 0.0
scrape_interval = (3600.0 / scrape_1h) if scrape_1h > 0 else 0.0
scrape_count = scalar("scrape_count", 0.0) or 0.0
expected_scrapes = (eff_secs / scrape_interval) if scrape_interval > 0 else 0.0
coverage = (scrape_count / expected_scrapes) if expected_scrapes > 0 else 0.0

gaps = ex.get("series_gaps") or []
passing_runs = scalar("passing_runs", 0.0) or 0.0
no_pass_max = scalar("no_pass_max_sec")
unit_failed_frac = scalar("unit_failed_frac", 0.0) or 0.0
run_dur_max = scalar("run_duration_max")
run_dur_min = scalar("run_duration_min")

try:
    min_coverage = float(os.environ.get("SLA_PROBE_MIN_COVERAGE", "0.95"))
except ValueError:
    refuse("--min-coverage is not a number.")

# A window the probe did not cover cannot prove a claim about that window.
# It does not refuse the report — the measurement is real and worth
# retaining — it refuses the PASS.
window_clean = bool(coverage >= min_coverage and not gaps)


# ── Verdicts ────────────────────────────────────────────────────────────
def bound_verdict(value, target, lower_is_better=True):
    if value is None:
        return "n/a"
    held = value <= target if lower_is_better else value >= target
    return "PROVEN" if held else "NOT PROVEN"


# Availability is the one headline row that is NOT a percentile, so it
# does not get the percentile treatment. A 99.9 % availability SLO is a
# ratio over the window; gating it on the worst single 30-second probe
# run would publish NOT PROVEN for a window the SLO was comfortably met
# in, which overstates the bar in the opposite direction to averaging a
# p95 and is just as wrong. The window ratio is estimated by weighting
# each run's rate by its own sample count, numerator and denominator
# sharing one subquery step so the scrape weighting cancels.
avail_num = by_endpoint("avail_weighted_num")
avail_den = by_endpoint("avail_weighted_den")
avail_window = {}
for ep in set(avail_num) & set(avail_den):
    if avail_den[ep] > 0:
        avail_window[ep] = avail_num[ep] / avail_den[ep]

endpoints = sorted(set(p95_max) | set(p99_max) | set(avail_min))
not_proven = []
for ep in endpoints:
    if bound_verdict(p95_max.get(ep), P95_TARGET_MS) == "NOT PROVEN":
        not_proven.append("%s p95" % ep)
    if bound_verdict(p99_max.get(ep), P99_TARGET_MS) == "NOT PROVEN":
        not_proven.append("%s p99" % ep)
    if bound_verdict(avail_window.get(ep), AVAILABILITY_TARGET_PCT, False) == "NOT PROVEN":
        not_proven.append("%s availability" % ep)

overall = "PROVEN" if (not not_proven and window_clean) else "NOT PROVEN"

# ── Optional side output ────────────────────────────────────────────────
extract_out = os.environ.get("SLA_PROBE_EXTRACT_OUT", "")
extract_blob = json.dumps(ex, sort_keys=True, indent=2, default=float)
extract_sha = hashlib.sha256(extract_blob.encode("utf-8")).hexdigest()
if extract_out:
    with open(extract_out, "w") as fh:
        fh.write(extract_blob + "\n")


# ── Render ──────────────────────────────────────────────────────────────
def ms(value):
    return "n/a" if value is None else "%.1f ms" % value


def pct(value, places=3):
    return "n/a" if value is None else ("%." + str(places) + "f %%") % value


def secs(value):
    return "n/a" if value is None else "%.1f s" % value


first_ts = float(ex["first_sample"])
last_ts = float(ex["last_sample"])
report_date = time.strftime("%Y-%m-%d", time.gmtime(last_ts))
out_dir = os.environ["SLA_PROBE_OUT_DIR"]
out_path = os.path.join(out_dir, "sla-proof-%s.md" % report_date)

target = os.environ["SLA_PROOF_PROBE_TARGET"]
concurrency = os.environ["SLA_PROOF_PROBE_CONCURRENCY"]
commit = os.environ["SLA_PROOF_COMMIT"]
run_url = os.environ.get("SLA_PROOF_RUN_URL", "").strip()
note = os.environ.get("SLA_PROOF_NOTE", "").strip()

# The network-path caveat is DERIVED from the recorded target, never
# asserted. A probe repointed at the public name makes the opposite
# statement true, and the report must follow the configuration rather
# than repeat a sentence someone wrote when it was loopback.
host_part = urllib.parse.urlsplit(target).hostname or ""
loopback = host_part in ("localhost", "127.0.0.1", "::1", "0.0.0.0")

api_versions = sorted({
    (r.get("metric") or {}).get("version")
    for r in rows("api_versions")
    if (r.get("metric") or {}).get("version")
})

out = []
w = out.append

w("---")
w("title: SLA proof report — %s" % report_date)
w("status: evidence (generated — do not hand-edit)")
w("generator: scripts/ops/sla-proof-from-probe.sh")
w("related:")
w("  - docs/adr/0009-latency-budget.md")
w("  - docs/operations/sla-proof-procedure.md")
w("  - cmd/stellarindex-sla-probe/main.go")
w("---")
w("")
w("# SLA proof report — %s" % report_date)
w("")
w("**Verdict: %s.**" % overall)
w("")
w("Generated by")
w("[`scripts/ops/sla-proof-from-probe.sh`](../../scripts/ops/sla-proof-from-probe.sh)")
w("by aggregating the `stellarindex_sla_probe_*` series that")
w("`stellarindex-sla-probe` writes into node_exporter's textfile collector")
w("every 15 minutes. Every number below is a literal reading of those")
w("series; nothing is hand-entered, and the generator refuses to write a")
w("report at all when a headline series is absent, when the run cannot name")
w("its own provenance, or when the retained window is too short to carry a")
w("claim.")
w("")
w("## Read this before reading the numbers")
w("")
w("Four things this measurement is not, each of which would otherwise be")
w("assumed from the shape of the document:")
w("")
w("1. **It is not a load test.** The probe drives %s concurrent worker(s)"
  % concurrency)
w("   in ~%s bursts every 15 minutes; it measures served latency under"
  % (("%.0f s" % run_dur_max) if run_dur_max else "fixed-length"))
w("   whatever real traffic the host had at the time. ADR-0009 states")
w("   p95 ≤ 200 ms and p99 ≤ 500 ms **unconditionally** — the target is not")
w("   qualified by a request rate — so a low-concurrency measurement does")
w("   test the stated target. What it does **not** exercise is ADR-0009's")
w("   *enforcement* clause, which calls for a load test at volume. Nothing")
w("   here is evidence about behaviour under concurrency the probe never")
w("   applied.")
if loopback:
    w("2. **The probe measures `%s` from inside the box.** Its requests never" % target)
    w("   traverse DNS, TLS, the reverse proxy or any CDN, so the numbers")
    w("   below EXCLUDE every component ADR-0009's budget allocates outside")
    w("   the API process — 5 ms of TLS terminate + proxy ingress and 15 ms of")
    w("   network egress in the p95 table alone. **A client's p95 is strictly")
    w("   larger than every latency figure in this report.**")
else:
    w("2. **The probe measures `%s`,** which is not a loopback address, so" % target)
    w("   these numbers include whatever DNS, TLS and proxy time that path")
    w("   carries from the probe's vantage point on `%s`. They are still a"
      % (probe_host or "the probe host"))
    w("   single-vantage measurement and do not describe a client elsewhere on")
    w("   the internet.")
w("3. **Percentiles were not averaged, because percentiles do not average.**")
w("   The probe exports one p50/p95/p99 per run and never the underlying")
w("   samples, so the request-level population percentile for this window is")
w("   not recoverable and is not computed here. The headline column is the")
w("   **largest per-run percentile in the window**, which is a true upper")
w("   bound on the pooled one: every run has ≥ 95 % of its samples at or")
w("   below its own p95, so pooling runs keeps ≥ 95 % at or below the largest")
w("   of those values, for any run sizes. The bound runs one way — under the")
w("   target it PROVES the target held; over the target it proves only that")
w("   this bound cannot settle it.")
w("4. **Nothing is rolled up across endpoints.** There is no \"overall p95\"")
w("   in this document. The verdict is the conjunction of the per-endpoint")
w("   bounds, not a percentile of a merged population.")
w("")
w("## Provenance")
w("")
w("| Field | Value |")
w("| --- | --- |")
w("| Claim under test | ADR-0009: p95 ≤ %g ms, p99 ≤ %g ms; probe availability ≥ %g %% |"
  % (P95_TARGET_MS, P99_TARGET_MS, AVAILABILITY_TARGET_PCT))
w("| Method | aggregate of `stellarindex_sla_probe_*` gauges; per-run percentiles bounded, never averaged |")
w("| Instrument | `stellarindex-sla-probe`, systemd timer `stellarindex-sla-probe.timer` (15 min cadence) |")
w("| Probe target | `%s` |" % target)
w("| Probe concurrency | %s |" % concurrency)
w("| Probe host | `%s` |" % (probe_host or "not labelled"))
w("| Metrics source | `%s`, `%s` |"
  % (ex.get("prom_url", "unknown"), "node_exporter textfile collector"))
w("| Window requested | `%s` |" % ex.get("window_requested", "?"))
w("| Window measured (UTC) | `%s` → `%s` (%s) |"
  % (iso(first_ts), iso(last_ts), human_duration(eff_secs)))
w("| Commit at render time | `%s` |" % commit)
w("| API builds observed in window | %s |"
  % (("%d — %s" % (len(api_versions), ", ".join("`%s`" % v for v in api_versions)))
     if api_versions else "not exposed by `stellarindex_binary_version_info`"))
w("| CI run | %s |" % (run_url or "not a CI run (rendered locally)"))
w("| Extract | %s, sha256 `%s` |" % (extract_source, extract_sha))
w("")
w("## Window and coverage")
w("")
w("A window with holes in it is not a clean window, and averaging over")
w("what survived would hide exactly the minutes worth looking at. Both")
w("kinds of hole are measured, because they are different failures: the")
w("textfile collector keeps re-serving the last file the probe wrote, so a")
w("dead probe leaves a perfectly continuous series.")
w("")
w("| Coverage question | Measured |")
w("| --- | --- |")
w("| Scrapes of the probe series in the window | %d of ~%d expected (%s) |"
  % (int(scrape_count), int(round(expected_scrapes)),
     pct(coverage * 100.0, 2) if expected_scrapes else "n/a"))
w("| Scrape interval (from the last hour) | %s |"
  % ("%.0f s" % scrape_interval if scrape_interval else "n/a"))
w("| Holes in the series (scraping stopped) | %s |"
  % ("none" if not gaps
     else "; ".join("%s for %s" % (iso(g["after"]), human_duration(g["seconds"]))
                    for g in gaps[:6])))
w("| Passing probe runs recorded | %d |" % int(passing_runs))
w("| Longest stretch with no recorded passing run | %s |"
  % (human_duration(no_pass_max) if no_pass_max is not None else "n/a"))
w("| Window spent under the probe's own FAIL verdict | %s |"
  % pct(unit_failed_frac * 100.0, 2))
w("| Probe run duration (min/max) | %s / %s |"
  % (secs(run_dur_min), secs(run_dur_max)))
w("")
if window_clean:
    w("The window is continuous at the configured floor (coverage ≥ %g %%, no"
      % (min_coverage * 100.0))
    w("holes), so the aggregates below cover it rather than a surviving")
    w("fraction of it.")
else:
    w("**This window is not clean.** Coverage is %s against a floor of %g %%%s."
      % (pct(coverage * 100.0, 2), min_coverage * 100.0,
         " and the series has %d hole(s)" % len(gaps) if gaps else ""))
    w("The aggregates below describe the samples that exist; they do not")
    w("describe the missing minutes, and the verdict is therefore NOT PROVEN")
    w("regardless of how the bounds read.")
w("")
w("## Result — per endpoint")
w("")
w("`worst-run p95` / `worst-run p99` are the largest value any single probe")
w("run produced in the window — the bound described above. `samples/run` is")
w("the probe's own request count for that endpoint.")
w("")
w("| Endpoint | samples/run | worst-run p95 | ≤ %g ms | worst-run p99 | ≤ %g ms | window availability | ≥ %g %% | worst run |"
  % (P95_TARGET_MS, P99_TARGET_MS, AVAILABILITY_TARGET_PCT))
w("| --- | --- | --- | --- | --- | --- | --- | --- | --- |")
for ep in endpoints:
    w("| `%s` | %s | %s | %s | %s | %s | %s | %s | %s |"
      % (ep,
         "%.0f" % samples_avg[ep] if ep in samples_avg else "n/a",
         ms(p95_max.get(ep)), bound_verdict(p95_max.get(ep), P95_TARGET_MS),
         ms(p99_max.get(ep)), bound_verdict(p99_max.get(ep), P99_TARGET_MS),
         pct(avail_window.get(ep)),
         bound_verdict(avail_window.get(ep), AVAILABILITY_TARGET_PCT, False),
         pct(avail_min.get(ep))))
w("")
w("The two latency columns are **bounds**; the availability column is an")
w("**estimate**, and they are different kinds of number on purpose. A")
w("percentile cannot be pooled across runs, so the largest per-run value is")
w("published instead and is a true ceiling on the pooled one. Availability")
w("is a ratio and the SLO is a ratio over the window, so gating it on the")
w("worst single 30-second run would fail a window the SLO was met in — it is")
w("weighted by each run's own sample count instead. `worst run` is kept")
w("beside it because a window ratio hides a short total outage, and the")
w("`window below target` column further down says how much of the week that")
w("was.")
w("")
total_n = sum(samples_avg.values()) * (passing_runs or 0)
w("Sample size: **%s per endpoint per run** on average (min %s, max %s over"
  % ("%.0f" % (sum(samples_avg.values()) / len(samples_avg)),
     "%.0f" % min(by_endpoint("samples_min").values() or [0]),
     "%.0f" % max(by_endpoint("samples_max").values() or [0])))
w("the window), across at least **%d recorded passing runs** — on the order of"
  % int(passing_runs))
w("**%s requests across all %d endpoints** over %s. That is a real n, and it"
  % ("{:,}".format(int(total_n)), len(endpoints), human_duration(eff_secs)))
w("is also %s requests issued by one process from one vantage point, which is"
  % "{:,}".format(int(total_n)))
w("not the same evidence as the same count spread across real clients. Run")
w("sizes are not uniform, which is why nothing here is a simple average of")
w("per-run values.")
w("")
w("### Typical run — descriptive only")
w("")
w("The median of the per-run percentiles. This is a description of **the")
w("runs**, not a percentile of requests, and it is time-weighted: each run's")
w("value is held in the series until the next run overwrites it, and the")
w("timer carries up to 60 s of randomised delay, so runs are not weighted")
w("exactly equally. Read it as \"what a normal run looked like\"; do not")
w("quote it as a latency percentile.")
w("")
w("| Endpoint | worst-run p50 | median-run p95 | median-run p99 | best-run p95 | window over p95 target | window over p99 target | window below availability target |")
w("| --- | --- | --- | --- | --- | --- | --- | --- |")
p50_max = by_endpoint("p50_max")
p95_typ = by_endpoint("p95_typ")
p99_typ = by_endpoint("p99_typ")
p95_min = by_endpoint("p95_min")
p95_over = by_endpoint("p95_over_frac")
p99_over = by_endpoint("p99_over_frac")
avail_under = by_endpoint("avail_under_frac")
for ep in endpoints:
    w("| `%s` | %s | %s | %s | %s | %s | %s | %s |"
      % (ep, ms(p50_max.get(ep)),
         ms(p95_typ.get(ep)), ms(p99_typ.get(ep)), ms(p95_min.get(ep)),
         pct((p95_over.get(ep) or 0.0) * 100.0, 3),
         pct((p99_over.get(ep) or 0.0) * 100.0, 3),
         pct((avail_under.get(ep) or 0.0) * 100.0, 3)))
w("")
w("`worst-run p50` is the largest per-run p50 in the window. It sits in the")
w("descriptive table rather than the headline one because ADR-0009 makes no")
w("median claim. `window over … target` is the fraction of the window during")
w("which the most recent run's percentile was above the target — a")
w("time-weighted reading of how much of the week a breach was the standing")
w("state, not a count of requests.")
w("")

fresh_max = by_endpoint("fresh_max")
fresh_typ = by_endpoint("fresh_typ")
if fresh_max:
    w("### Freshness — reported, not gated")
    w("")
    w("Only endpoints that return an `observed_at` carry this series.")
    w("`/price` serves the ADR-0015 closed bucket and is structurally 30–150 s")
    w("old by design; the 30 s pricing SLA is served and measured on")
    w("`/price/tip`.")
    w("")
    w("These rows carry no verdict, and that is deliberate. The deployed")
    w("freshness bound is enforced by `stellarindex_sla_probe_freshness_breach`")
    w("in `deploy/monitoring/rules/sla-probe.yml`, which requires the breach to")
    w("be **sustained for 30 minutes** — because a single run over bound is")
    w("in-contract: `computeTip` escalates its window and falls back to the")
    w("closed bucket, so a pair with no recent trade legitimately serves a")
    w("60–120 s `observed_at` (ADR-0018). Gating a worst-single-run value")
    w("against a threshold the live alert deliberately does not gate that way")
    w("would invent a stricter bar than the system asserts.")
    w("")
    w("| Endpoint | stated bound | worst-run freshness | median-run freshness |")
    w("| --- | --- | --- | --- |")
    for ep in sorted(fresh_max):
        bound = FRESHNESS_TARGET_SEC.get(ep)
        w("| `%s` | %s | %s | %s |"
          % (ep,
             ("≤ %g s" % bound) if bound else "no stated bound",
             secs(fresh_max.get(ep)), secs(fresh_typ.get(ep))))
    w("")

w("## What this report does and does not prove")
w("")
w("- It proves a **bound**, not a value. Where a row reads PROVEN the")
w("  pooled percentile for that endpoint over this window cannot have")
w("  exceeded the target. Where it reads NOT PROVEN the pooled percentile")
w("  may still have been inside the target — the bound simply cannot settle")
w("  it, and the per-run breach that produced it is named in the")
w("  `window over target` column above.")
if loopback:
    w("- It excludes the **edge**. DNS, TLS terminate, the reverse proxy and")
    w("  any CDN are outside the measured path entirely. ADR-0009 defines its")
    w("  budget as edge-to-client and allocates 5 ms + 15 ms to exactly those")
    w("  hops, so a client-observed p95 is strictly larger than every figure")
    w("  here and this report does not evidence the end-to-end claim.")
w("- It says nothing about **behaviour under load**. `test/load/scenarios/`")
w("  holds the k6 scenarios that would, including the canonical 300 rps ×")
w("  10 min soak; they compile on every PR and are one `workflow_dispatch`")
w("  away from running, and they have no production-shaped target to run")
w("  against. See docs/operations/sla-proof-procedure.md.")
w("- It gates every endpoint at the headline %g / %g ms. ADR-0009's"
  % (P95_TARGET_MS, P99_TARGET_MS))
w("  per-endpoint table is **tighter** for `/healthz`, `/readyz` and")
w("  `/version` (p95 ≤ 5 ms, p99 ≤ 20 ms) and states no budget at all for")
w("  `/assets`, `/issuers`, `/markets`, `/diagnostics/cursors` or")
w("  `/price/tip`. A PROVEN row for a smoke endpoint here is therefore not")
w("  a test of its tighter budget.")
w("- The availability figure is the probe's own 2xx rate over its own")
w("  requests. It is not an uptime SLO: a window in which the probe was")
w("  the only client is not a window in which users were served.")
if len(api_versions) > 1:
    w("- It is **not a measurement of one build.** %d API builds were live"
      % len(api_versions))
    w("  inside this window (`%s` … `%s`), so the numbers describe a moving"
      % (api_versions[0], api_versions[-1]))
    w("  target and every deploy restart inside the window is in them. \"Commit")
    w("  at render time\" names where the tree was when this document was")
    w("  generated, not a single build the whole window ran.")
else:
    w("- It is evidence about the build named in Provenance on the host named")
    w("  there. It does not transfer to a differently-shaped deployment.")
w("")
w("## Notes / caveats")
w("")
w(note if note else "None recorded for this run.")
w("")
w("## Reproduce")
w("")
w("```sh")
w("# On the probe host, or through a port-forward to its Prometheus.")
w("SLA_PROOF_PROBE_TARGET='%s' \\" % target)
w("  SLA_PROOF_PROBE_CONCURRENCY='%s' \\" % concurrency)
w("  SLA_PROOF_COMMIT=\"$(git rev-parse HEAD)\" \\")
w("  scripts/ops/sla-proof-from-probe.sh \\")
w("    --prom-url %s \\" % ex.get("prom_url", "http://127.0.0.1:9090"))
w("    --window %s \\" % ex.get("window_requested", "7d"))
w("    --extract-out sla-probe-extract.json")
w("```")
w("")
w("The extract is the exact set of query results this report was rendered")
w("from, including the promql of every query. Re-rendering it with")
w("`--extract sla-probe-extract.json` reproduces every number in this")
w("document with no network access at all; the only line that differs is the")
w("`Extract` row above, which records that the second pass was a re-render.")

body = "\n".join(out) + "\n"
with open(out_path, "w") as fh:
    fh.write(body)

digest = hashlib.sha256(body.encode("utf-8")).hexdigest()
print("sla-proof-from-probe: %s — wrote %s (sha256 %s)"
      % (overall, out_path, digest[:16]))
if not_proven:
    print("sla-proof-from-probe: NOT PROVEN — %s" % ", ".join(not_proven))
if not window_clean:
    print("sla-proof-from-probe: window not clean — coverage %.4f, %d gap(s)"
          % (coverage, len(gaps)))
print(out_path)
sys.exit(0 if overall == "PROVEN" else 1)
PY
rc=$?
set -e
exit "$rc"
