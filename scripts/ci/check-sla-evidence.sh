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
# Env:
#   K6_TARGET                  load-run base URL     (workflow secret)
#   STELLARINDEX_LOAD_API_KEY  load-test API key     (workflow secret)
#   SLA_EVIDENCE_DIR           where landed proof reports live
#                              (default: docs/operations)
#   SLA_PROOF_MAX_AGE_DAYS     proof staleness threshold (default: 45 —
#                              the procedure's MONTHLY cadence plus two
#                              weeks of operator slack)
#   SLA_EVIDENCE_NOW           epoch-seconds clock override (tests only)
#
# Exit codes — the workflow branches on all three, so they are a contract:
#   0  HEALTHY   target configured AND a proof report inside the window.
#   1  NO TARGET nothing can run: skip the k6 steps, and do NOT report the
#                run as a success.
#   2  NO PROOF  the run can proceed, but no proof report inside the window
#                has landed, so the feed still isn't producing its artefact.
#                Deliberately distinct from 1: a stale report must never
#                block the very run that would let an operator write a
#                fresh one.
set -euo pipefail

cd "$(dirname "$0")/../.."

SLA_EVIDENCE_DIR="${SLA_EVIDENCE_DIR:-docs/operations}"
SLA_PROOF_MAX_AGE_DAYS="${SLA_PROOF_MAX_AGE_DAYS:-45}"
now_epoch="${SLA_EVIDENCE_NOW:-$(date -u +%s)}"

# ── Leg 1: can a load run happen at all? ────────────────────────────────
target_ready=true
missing=""
if [ -z "${K6_TARGET:-}" ]; then
  target_ready=false
  missing="K6_TARGET"
fi
if [ -z "${STELLARINDEX_LOAD_API_KEY:-}" ]; then
  target_ready=false
  missing="${missing:+${missing} }STELLARINDEX_LOAD_API_KEY"
fi

if [ "$target_ready" = true ]; then
  echo "sla-evidence: target configured — the weekly load run can execute."
else
  echo "sla-evidence: NO TARGET — unset secret(s): ${missing}."
fi

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
  cat <<EOF
sla-evidence: RED (rc=1) — the SLA-evidence feed cannot produce evidence:
  the load target is not configured, so nothing measures the p95 <= 200 ms
  claim this workflow exists to defend. Mint K6_TARGET_STAGING +
  STELLARINDEX_LOAD_API_KEY against a production-shaped target (never
  production itself — test/load/scenarios/lib/env.js refuses prod hosts),
  or retire the workflow. See docs/operations/sla-proof-procedure.md.
EOF
  exit 1
fi

if [ "$proof_fresh" != true ]; then
  cat <<EOF
sla-evidence: RED (rc=2) — the load run can execute, but no proof report
  inside the ${SLA_PROOF_MAX_AGE_DAYS}-day window has landed in
  ${SLA_EVIDENCE_DIR}/. The run is only half the feed: promote the run's
  summary to ${SLA_EVIDENCE_DIR}/sla-proof-<YYYY-MM-DD>.md per
  docs/operations/sla-proof-procedure.md ("Write the report").
EOF
  exit 2
fi

echo "sla-evidence: OK — target configured and ${newest_proof} is inside the ${SLA_PROOF_MAX_AGE_DAYS}-day window."
exit 0
