#!/usr/bin/env bash
# check-sla-evidence.sh — decision core for the weekly SLA-evidence run
# (.github/workflows/k6-weekly.yml).
#
# k6-weekly.yml is the ONLY feed into the monthly proof-of-SLA report
# (docs/operations/sla-proof-procedure.md). A scheduled run that skips every
# measuring step must not conclude `success`: a green badge on an alarm that
# never measured anything is silence read as success.
#
# This script is the verdict the workflow branches on. Deterministic and
# offline (no gh, network or k6), so scripts/ci/check-sla-evidence-test.sh
# exercises it on every PR.
#
# Two producers feed the same artefact: a k6 load run
# (scripts/ci/render-sla-proof.sh) or the SLA probe aggregate
# (scripts/ops/sla-proof-from-probe.sh). Both write
# docs/operations/sla-proof-<YYYY-MM-DD>.md, so leg 2 (presence and
# freshness) reads either. Leg 1 must ask which source the CALLER depends on,
# or a probe-fed feed would be RED forever for lacking a load target.
#
# Env:
#   SLA_EVIDENCE_SOURCE        which producer must be able to run: `k6`,
#                              `probe`, or `any` (default; either suffices).
#                              Callers that branch on rc 1 to skip their own
#                              steps must name their source; `any` only
#                              answers "is this feed alive at all".
#   K6_TARGET                  load-run base URL     (workflow secret)
#   STELLARINDEX_LOAD_API_KEY  load-test API key     (workflow secret)
#   SLA_PROBE_PROM_URL         Prometheus holding the stellarindex_sla_probe_*
#                              series the aggregate reads
#   SLA_EVIDENCE_DIR           where proof reports live (default: docs/operations)
#   SLA_PROOF_MAX_AGE_DAYS     staleness threshold (default 45: the monthly
#                              cadence plus two weeks of slack; the weekly
#                              probe feed tolerates six missed runs).
#                              Tightening it is a cadence decision for the
#                              procedure doc, not a quiet default change.
#   SLA_EVIDENCE_NOW           epoch-seconds clock override (tests only)
#
# Exit codes — the workflow branches on all three, so they are a contract:
#   0  HEALTHY   a source is configured AND a proof report is inside the
#                window.
#   1  NO TARGET nothing can run: skip the producer's steps, and do NOT
#                report the run as a success.
#   2  NO PROOF  the run can proceed, but no proof report inside the window
#                has landed. Distinct from 1: a stale report must never block
#                the very run that lets an operator write a fresh one.
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
# Each source is assessed and reported on its own, so a caller needing one is
# never told about the other's secrets. The k6 wording is fixed: the workflow,
# the tracking issue body and the fixtures read these lines.
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
# Match ONLY the dated report filenames; the glob excludes
# sla-proof-procedure.md, which is the recipe, not evidence.
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

# Portable ISO date → epoch (GNU vs BSD date); idiom from check-main-ci-health.sh.
to_epoch() {
  date -u -d "$1" +%s 2>/dev/null || date -u -j -f "%Y-%m-%d" "$1" +%s 2>/dev/null || echo 0
}

# Portable SHA-256; idiom from lint-migration-immutability.sh.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum -- "$1" | awk '{print $1}'
  else
    shasum -a 256 -- "$1" | awk '{print $1}'
  fi
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

# ── Leg 2b: is the freshest filename actually evidence? ─────────────────
# A filename match proves nothing (a masked timer, hand edit or truncated
# write still matches). A real report carries:
#   1. a `generator:` front-matter line naming its producer;
#   2. if it has a self-referential `digest: sha256:<hex>` line (only
#      sla-proof-from-probe.sh embeds one), that digest must reproduce from
#      the file's own bytes with the digest line zeroed, as the generator does.
# Neither check runs without a candidate file; an absent proof stays NO PROOF.
proof_integrity_ok=true
if [ -n "$newest_proof" ]; then
  if ! grep -q '^generator: .\+$' "$newest_proof"; then
    proof_integrity_ok=false
    echo "sla-evidence: NO PROOF — ${newest_proof} carries no 'generator:'" \
         "front-matter line, so it cannot be attributed to either producer."
  fi
  digest_line="$(grep -m1 '^digest: sha256:' "$newest_proof" || true)"
  if [ -n "$digest_line" ]; then
    declared="${digest_line#digest: sha256:}"
    placeholder="$(printf '0%.0s' $(seq 1 64))"
    zeroed="$(mktemp)"
    sed "s/^digest: sha256:.*/digest: sha256:${placeholder}/" \
      "$newest_proof" > "$zeroed"
    recomputed="$(sha256_of "$zeroed")"
    rm -f "$zeroed"
    if [ "$declared" != "$recomputed" ]; then
      proof_integrity_ok=false
      echo "sla-evidence: NO PROOF — ${newest_proof}'s embedded digest" \
           "(${declared}) does not match its own bytes (${recomputed})." \
           "The file was edited, truncated, or corrupted after render."
    else
      echo "sla-evidence: ${newest_proof}'s embedded digest matches its own bytes."
    fi
  fi
fi

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

# Which producer is live, for the wording below. The k6 branch keeps its exact
# bytes (fixtures and the tracking-issue body read them); only a caller with
# the probe source and no k6 gets the other sentence.
speak_probe=false
if [ "$SLA_EVIDENCE_SOURCE" = "probe" ]; then
  speak_probe=true
elif [ "$SLA_EVIDENCE_SOURCE" = "any" ] && [ "$k6_ready" != true ]; then
  speak_probe=true
fi

if [ "$proof_integrity_ok" != true ]; then
  cat <<EOF
sla-evidence: RED (rc=2) — ${newest_proof} matches the dated-filename
  shape but failed integrity verification (see above): a filename is not
  evidence, and a report with no attributable generator or a digest that
  does not reproduce from its own bytes must be treated the same as no
  report at all.
EOF
  exit 2
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
  docs/operations/sla-proof-procedure.md ("Run").
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
