#!/usr/bin/env bash
# check-sla-evidence.sh — decision core for the weekly SLA-evidence run
# (.github/workflows/k6-weekly.yml), issue #316.
#
# k6-weekly.yml is the ONLY feed into the monthly proof-of-SLA report
# (docs/operations/sla-proof-procedure.md). Until 2026-08-29 its scheduled
# path emitted `::notice::` and exited 0 whenever the target secrets were
# unset, so every scheduled run since the cron was restored concluded
# `success` with `Install k6` / `Compile-check` / `Run scenario` all
# `skipped` (runs 30733884244, 31292354359, 31922897932, 32614013429) —
# and no `docs/operations/sla-proof-<YYYY-MM-DD>.md` has ever landed. A
# green badge for an SLA regression alarm that has never measured anything
# is worse than no badge: silence read as success.
#
# This script is the verdict the workflow branches on. It is deterministic
# and offline (no gh, no network, no k6) so it is exercised on every PR by
# scripts/ci/check-sla-evidence-test.sh rather than only once a week.
#
# TWO SOURCES NOW FEED LEG 2, AND LEG 2 DID NOT CHANGE. The proof report
# can be produced by a k6 load run (scripts/ci/render-sla-proof.sh) or by
# aggregating the SLA probe (scripts/ops/sla-proof-from-probe.sh). Both
# write the SAME artefact — docs/operations/sla-proof-<YYYY-MM-DD>.md with
# the same filename shape — so the presence-and-freshness leg below reads
# either one without a line of change, and none was made to it. What did
# have to change is leg 1: it asked only "are the k6 secrets set", so a
# feed producing evidence from the probe every Sunday would still have
# been reported RED forever for the absence of a load target it no longer
# needs. Leg 1 now asks which source the CALLER depends on.
#
# Env:
#   SLA_EVIDENCE_SOURCE        which producer must be able to run:
#                              `k6` (the load run), `probe` (the probe
#                              aggregate), or `any` (default — either one
#                              is enough). Callers that branch on rc 1 to
#                              skip their own steps must name their source
#                              explicitly; `any` is the right answer only
#                              for "is this feed alive at all".
#   K6_TARGET                  load-run base URL     (workflow secret)
#   STELLARINDEX_LOAD_API_KEY  load-test API key     (workflow secret)
#   SLA_PROBE_PROM_URL         Prometheus holding the stellarindex_sla_probe_*
#                              series the aggregate reads
#   SLA_EVIDENCE_DIR           where landed proof reports live
#                              (default: docs/operations)
#   SLA_PROOF_MAX_AGE_DAYS     proof staleness threshold (default: 45 —
#                              the procedure's MONTHLY cadence plus two
#                              weeks of operator slack. The probe-aggregate
#                              feed runs WEEKLY, so 45 days tolerates six
#                              missed runs; tightening it is a cadence
#                              decision for the procedure doc to make, not
#                              a default to change quietly here.)
#   SLA_EVIDENCE_NOW           epoch-seconds clock override (tests only)
#
# Exit codes — the workflow branches on all three, so they are a contract:
#   0  HEALTHY   a source is configured AND a proof report is inside the
#                window.
#   1  NO TARGET nothing can run: skip the producer's steps, and do NOT
#                report the run as a success.
#   2  NO PROOF  the run can proceed, but no proof report inside the window
#                has landed, so the feed still isn't producing its artefact.
#                Deliberately distinct from 1: a stale report must never
#                block the very run that would let an operator write a
#                fresh one.
set -euo pipefail

cd "$(dirname "$0")/../.."

SLA_EVIDENCE_SOURCE="${SLA_EVIDENCE_SOURCE:-any}"
case "$SLA_EVIDENCE_SOURCE" in
  k6|probe|any) ;;
  *)
    echo "sla-evidence: SLA_EVIDENCE_SOURCE='${SLA_EVIDENCE_SOURCE}' is not" \
         "one of k6 | probe | any." >&2
    exit 1 ;;
esac

SLA_EVIDENCE_DIR="${SLA_EVIDENCE_DIR:-docs/operations}"
SLA_PROOF_MAX_AGE_DAYS="${SLA_PROOF_MAX_AGE_DAYS:-45}"
now_epoch="${SLA_EVIDENCE_NOW:-$(date -u +%s)}"

# ── Leg 1: can a producer run at all? ───────────────────────────────────
# Each source is assessed on its own and reported on its own, so a caller
# that needs one of them is never told about the other's secrets. The
# wording of the k6 branch is unchanged: the workflow, the tracking issue
# body and the fixtures all read these lines.
k6_ready=true
missing=""
if [ -z "${K6_TARGET:-}" ]; then
  k6_ready=false
  missing="K6_TARGET"
fi
if [ -z "${STELLARINDEX_LOAD_API_KEY:-}" ]; then
  k6_ready=false
  missing="${missing:+${missing} }STELLARINDEX_LOAD_API_KEY"
fi

probe_ready=true
probe_missing=""
if [ -z "${SLA_PROBE_PROM_URL:-}" ]; then
  probe_ready=false
  probe_missing="SLA_PROBE_PROM_URL"
fi

case "$SLA_EVIDENCE_SOURCE" in
  k6)    target_ready="$k6_ready" ;;
  probe) target_ready="$probe_ready" ;;
  any)
    target_ready=false
    if [ "$k6_ready" = true ] || [ "$probe_ready" = true ]; then
      target_ready=true
    fi ;;
esac

if [ "$SLA_EVIDENCE_SOURCE" != "probe" ]; then
  if [ "$k6_ready" = true ]; then
    echo "sla-evidence: target configured — the weekly load run can execute."
  else
    echo "sla-evidence: NO TARGET — unset secret(s): ${missing}."
  fi
fi
if [ "$SLA_EVIDENCE_SOURCE" != "k6" ]; then
  if [ "$probe_ready" = true ]; then
    echo "sla-evidence: probe aggregate configured — the weekly proof can be" \
         "rendered from ${SLA_PROBE_PROM_URL}."
  else
    echo "sla-evidence: NO PROBE SOURCE — unset: ${probe_missing}."
  fi
fi
echo "sla-evidence: source=${SLA_EVIDENCE_SOURCE}."

# ── Leg 2: has the feed actually produced its artefact recently? ────────
# Match ONLY the dated report filenames the procedure prescribes. The
# glob deliberately excludes sla-proof-procedure.md and
# sla-proof-template.md, which live in the same directory and are the
# recipe and the blank form — not evidence.
newest_proof=""
newest_date=""
for f in "$SLA_EVIDENCE_DIR"/sla-proof-[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9].md; do
  [ -e "$f" ] || continue
  d="$(basename "$f")"
  d="${d#sla-proof-}"
  d="${d%.md}"
  # ISO-8601 dates sort lexicographically, so > is a date comparison here.
  if [ -z "$newest_date" ] || [ "$d" \> "$newest_date" ]; then
    newest_date="$d"
    newest_proof="$f"
  fi
done

# Portable ISO date → epoch (GNU date and BSD/macOS date differ) — same
# idiom as scripts/ci/check-main-ci-health.sh.
to_epoch() {
  date -u -d "$1" +%s 2>/dev/null || date -u -j -f "%Y-%m-%d" "$1" +%s 2>/dev/null || echo 0
}

proof_fresh=false
if [ -z "$newest_proof" ]; then
  echo "sla-evidence: NO PROOF — no sla-proof-<YYYY-MM-DD>.md has ever landed in ${SLA_EVIDENCE_DIR}/."
else
  proof_epoch="$(to_epoch "$newest_date")"
  if [ "$proof_epoch" -eq 0 ]; then
    echo "sla-evidence: NO PROOF — newest report ${newest_proof} has an unparseable date '${newest_date}'."
  else
    age_days=$(( (now_epoch - proof_epoch) / 86400 ))
    if [ "$age_days" -le "$SLA_PROOF_MAX_AGE_DAYS" ]; then
      proof_fresh=true
      echo "sla-evidence: newest proof ${newest_proof} is ${age_days} day(s) old."
    else
      echo "sla-evidence: NO PROOF — newest proof ${newest_proof} is ${age_days} day(s) old."
    fi
  fi
fi
echo "sla-evidence: threshold SLA_PROOF_MAX_AGE_DAYS=${SLA_PROOF_MAX_AGE_DAYS}."

# ── Verdict ─────────────────────────────────────────────────────────────
if [ "$target_ready" != true ]; then
  if [ "$SLA_EVIDENCE_SOURCE" = "probe" ]; then
    cat <<EOF
sla-evidence: RED (rc=1) — the SLA-evidence feed cannot produce evidence:
  SLA_PROBE_PROM_URL is unset, so nothing can read the
  stellarindex_sla_probe_* series the weekly proof is aggregated from.
  Point it at the Prometheus scraping the probe host — from CI that is an
  ssh port-forward, see .github/workflows/sla-proof-weekly.yml. The probe
  itself keeps running either way; what is missing is the reader.
EOF
  else
    cat <<EOF
sla-evidence: RED (rc=1) — the SLA-evidence feed cannot produce evidence:
  the load target is not configured, so nothing measures the p95 <= 200 ms
  claim this workflow exists to defend. Mint K6_TARGET_STAGING +
  STELLARINDEX_LOAD_API_KEY against a production-shaped target (never
  production itself — test/load/scenarios/lib/env.js refuses prod hosts),
  or render the proof from the probe aggregate instead
  (SLA_EVIDENCE_SOURCE=probe). See docs/operations/sla-proof-procedure.md.
EOF
  fi
  exit 1
fi

# Which producer is actually the live one, for the wording below. The k6
# branch keeps its exact bytes: the fixtures and the tracking-issue body
# read these lines, and only a caller that has NO k6 source but DOES have
# the probe one gets the other sentence.
speak_probe=false
if [ "$SLA_EVIDENCE_SOURCE" = "probe" ]; then
  speak_probe=true
elif [ "$SLA_EVIDENCE_SOURCE" = "any" ] && [ "$k6_ready" != true ]; then
  speak_probe=true
fi

if [ "$proof_fresh" != true ]; then
  if [ "$speak_probe" = true ]; then
    cat <<EOF
sla-evidence: RED (rc=2) — the probe aggregate can be read, but no proof
  report inside the ${SLA_PROOF_MAX_AGE_DAYS}-day window has landed in
  ${SLA_EVIDENCE_DIR}/. Reading the series is only half the feed: render
  and commit ${SLA_EVIDENCE_DIR}/sla-proof-<YYYY-MM-DD>.md with
  scripts/ops/sla-proof-from-probe.sh per
  docs/operations/sla-proof-procedure.md.
EOF
  else
    cat <<EOF
sla-evidence: RED (rc=2) — the load run can execute, but no proof report
  inside the ${SLA_PROOF_MAX_AGE_DAYS}-day window has landed in
  ${SLA_EVIDENCE_DIR}/. The run is only half the feed: promote the run's
  summary to ${SLA_EVIDENCE_DIR}/sla-proof-<YYYY-MM-DD>.md per
  docs/operations/sla-proof-procedure.md ("Write the report").
EOF
  fi
  exit 2
fi

if [ "$speak_probe" = true ]; then
  echo "sla-evidence: OK — probe aggregate configured and ${newest_proof} is inside the ${SLA_PROOF_MAX_AGE_DAYS}-day window."
else
  echo "sla-evidence: OK — target configured and ${newest_proof} is inside the ${SLA_PROOF_MAX_AGE_DAYS}-day window."
fi
exit 0
