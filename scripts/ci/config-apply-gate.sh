#!/usr/bin/env bash
# config-apply-gate.sh — deploy.yml post-deploy forcing function.
#
# deploy-binary.yml swaps BINARIES ONLY: it does NOT render the ansible
# stellarindex.toml, sync Prometheus rules, install systemd units, or apply
# DB schema. So when a release changed any of those surfaces, the feature
# they gate ships DEAD and SILENT unless an operator applies the config too
# (the 2026-08-25 declared-peg + rules.d incidents — see
# docs/operations/deploy-config-apply.md).
#
# The surfaces below are the role's WHOLE config-bearing tree, not just
# templates/: on 2026-08-28 a new node_exporter probe + its systemd units
# shipped as INLINE `content:` blocks inside tasks/10-observability.yml
# (and role scripts live in files/), so a release that touched only
# tasks/ or files/ would have passed this gate and deployed the feature
# dead. A near-miss, caught by hand — hence the two extra entries.
#
# 2026-08-28 (audit deploy-ansible-gate-4): the list was still narrower
# than what the role actually renders. roles/…/defaults/main.yml feeds
# every template (galexie_ledgers_per_file → galexie.toml.j2), inventory
# host_vars do the same, handlers/ decide what restarts, and the role
# COPIES files from outside configs/ansible entirely: configs/healthchecks/*
# (17-stellarindex-healthchecks.yml), scripts/ops/config-assertions.sh
# (15-log-discipline.yml), scripts/ops/{ch-schema-snapshot,restore-drill}.sh
# and scripts/dev/r1-smoke.sh. A defaults-only or healthchecks-only
# release passed this gate. Deliberately NOT listed: playbooks/ and
# tasks/deploy-one-binary.yml (the deploy itself runs them — a change
# there is applied, not dead), migrations/ (deploy-binary.yml migrates)
# and bin/.
#
# This gate diffs the deploying version against a baseline over the config
# surfaces. The baseline is the version live on the host when the caller
# knows it (3rd arg — e.g. read from the host's
# /var/lib/stellarindex/deployed-versions/<binary> sidecar), else the
# previous release tag by ancestry. Ancestry is WRONG for a skip-ahead
# deploy (host on v0.45.0, deploying v0.47.2 diffs only v0.47.1..v0.47.2)
# and for a rollback, so the baseline actually used is always printed.
# If any surface changed and the operator did NOT pass
# config_acknowledged=true, it FAILS — a loud, NON-destructive forcing
# function (the binaries are already deployed by an earlier step; a red
# gate just says "config still needs applying", which is accurate and
# actionable). Acknowledging asserts the operator will apply / has applied
# the config per the runbook.
#
# Fail-closed: an unresolvable version/baseline or a failing `git diff`
# is an ERROR, not "no changes" — a gate that cannot see the diff has no
# basis for a green.
#
# Usage: config-apply-gate.sh <version-tag> [acknowledged] [baseline-tag] [applied]
#   <version-tag>   the deploying tag, e.g. v0.43.0
#   [acknowledged]  "true" if the operator passed config_acknowledged=true
#   [baseline-tag]  the version live on the host, if known; defaults to
#                   the previous release tag by ancestry
#   [applied]       space-separated surface prefixes this deploy already
#                   applied AND VERIFIED automatically — see below
#
# The [applied] argument exists so automation can retire surfaces from
# this gate ONE AT A TIME as they become self-applying, without weakening
# it for the rest. Today the only such surface is
# configs/prometheus/rules.r1/, applied by configs/prometheus/apply-rules.sh
# in the deploy job — which validates with promtool, installs atomically,
# reconciles deletions, reloads, and then POLLS /api/v1/rules until every
# expected alert is loaded and healthy, restoring a backup if not.
#
# The bar for adding to this list is that last clause: the applier must
# VERIFY the surface is live and fail if it is not. A step that merely
# copies a file has not earned an exemption — it reproduces the exact
# 2026-09-01 failure this gate exists to catch, where a merged ClickHouse
# alert watched nothing and a deleted alert kept firing. The caller must
# also pass a surface here ONLY when the apply actually succeeded on this
# run; a pre-declared list would clear the gate for a step that never ran.
set -uo pipefail

VERSION="${1:?deploying version tag, e.g. v0.43.0}"
ACK="${2:-false}"
BASELINE="${3:-}"
APPLIED="${4:-}"

# Config surfaces a binary deploy does NOT apply. Directory prefixes are
# git pathspecs: the trailing slash matches everything beneath them.
# Keep in lockstep with config-apply-gate-test.sh (one fixture per entry).
SURFACES=(
  'configs/ansible/roles/'
  'configs/ansible/inventory/'
  'configs/healthchecks/'
  'configs/prometheus/rules.r1/'
  'configs/alertmanager/'
  'deploy/monitoring/rules/'
  'deploy/systemd/'
  'deploy/clickhouse/'
  # Repo scripts the archival-node role copies onto the host verbatim.
  'scripts/ops/config-assertions.sh'
  'scripts/ops/ch-schema-snapshot.sh'
  'scripts/ops/restore-drill.sh'
  'scripts/dev/r1-smoke.sh'
)

if ! git rev-parse -q --verify "${VERSION}^{commit}" >/dev/null; then
  echo "::error::config-apply gate: deploying version '${VERSION}' does not resolve to a commit in this checkout — cannot diff config surfaces; refusing to pass (fail-closed). Fetch tags (fetch-depth: 0) or check the tag name."
  exit 1
fi

if [ -n "$BASELINE" ]; then
  if ! git rev-parse -q --verify "${BASELINE}^{commit}" >/dev/null; then
    echo "::error::config-apply gate: host baseline '${BASELINE}' does not resolve to a commit in this checkout — cannot diff config surfaces; refusing to pass (fail-closed)."
    exit 1
  fi
  PREV="$BASELINE"
  echo "baseline: ${PREV} (host-reported live version)"
else
  # Ancestry fallback: the previous release tag = the config baseline
  # ASSUMED live on the host.
  PREV="$(git describe --tags --abbrev=0 "${VERSION}^" 2>/dev/null || true)"
  if [ -z "$PREV" ]; then
    echo "note: no release tag before ${VERSION}; skipping config-drift gate (first release / shallow clone)."
    exit 0
  fi
  echo "baseline: ${PREV} (previous release tag by ancestry; for a skip-ahead or rollback deploy pass the host's live version as the 3rd argument)"
fi

if ! CHANGED="$(git diff --name-only "$PREV" "$VERSION" -- "${SURFACES[@]}")"; then
  echo "::error::config-apply gate: git diff ${PREV}..${VERSION} failed — cannot determine which config surfaces changed; refusing to pass (fail-closed)."
  exit 1
fi

SUMMARY="${GITHUB_STEP_SUMMARY:-/dev/stdout}"

# Subtract surfaces this deploy applied AND verified automatically. They
# stay in SURFACES above (so the diff still SEES them, and a run that did
# not apply them still gates on them) — they are only removed from what
# the operator is asked to acknowledge.
# Matching is an anchored PATH-PREFIX test, not a substring search. A
# substring match would let a loose token ("rules") silently exempt
# deploy/monitoring/rules/ as well as configs/prometheus/rules.r1/ —
# quietly widening the exemption to a surface nothing applied, which is
# the failure mode this whole gate exists to prevent.
AUTO_APPLIED=""
if [ -n "$APPLIED" ] && [ -n "$CHANGED" ]; then
  remaining=""
  while IFS= read -r path; do
    [ -z "$path" ] && continue
    exempt=""
    for prefix in $APPLIED; do
      case "$path" in
        "$prefix"*) exempt=1; break ;;
      esac
    done
    if [ -n "$exempt" ]; then
      AUTO_APPLIED="${AUTO_APPLIED}${path}"$'\n'
    else
      remaining="${remaining}${path}"$'\n'
    fi
  done <<EOF
$CHANGED
EOF
  CHANGED="$(printf '%s' "$remaining")"
fi

if [ -n "$AUTO_APPLIED" ]; then
  n=$(printf '%s' "$AUTO_APPLIED" | grep -c . || true)
  echo "✓ ${n} config surface(s) were applied and VERIFIED automatically by this deploy:"
  printf '%s' "$AUTO_APPLIED" | sed 's/^/    /'
  {
    echo "- ✓ config-apply gate: **${n}** surface(s) applied *and verified live* automatically by this deploy (not left to the operator)."
  } >>"$SUMMARY" 2>/dev/null || true
fi

if [ -z "$CHANGED" ]; then
  if [ -n "$AUTO_APPLIED" ]; then
    echo "✓ every changed config surface between ${PREV} and ${VERSION} was applied and verified automatically — nothing left for the operator."
    echo "- ✓ config-apply gate: every changed surface was auto-applied and verified; no operator action outstanding." >>"$SUMMARY" 2>/dev/null || true
  else
    echo "✓ no config-surface changes between ${PREV} and ${VERSION} — the binary deploy is complete."
    echo "- ✓ config-apply gate: no config-surface changes between \`${PREV}\` and \`${VERSION}\`." >>"$SUMMARY" 2>/dev/null || true
  fi
  exit 0
fi

{
  echo "## ⚠️ Config-apply required — a binary deploy does NOT apply these"
  echo ""
  echo "Release **${VERSION}** changed these config/schema surfaces since **${PREV}**."
  echo "\`deploy-binary.yml\` swaps binaries only. Apply them per"
  echo "\`docs/operations/deploy-config-apply.md\` and **verify each landed on the host**"
  echo "(grep the live surface — never trust \"deploy OK\"):"
  echo ""
  echo '```'
  echo "$CHANGED"
  echo '```'
} >>"$SUMMARY" 2>/dev/null || true

if [ "$ACK" = "true" ]; then
  echo "::notice::config-apply acknowledged for ${VERSION} — operator asserts the $(echo "$CHANGED" | grep -c . ) changed config surface(s) are/will be applied per the runbook."
  echo "" >>"$SUMMARY" 2>/dev/null || true
  echo "_Operator passed \`config_acknowledged=true\` — gate satisfied._" >>"$SUMMARY" 2>/dev/null || true
  exit 0
fi

echo "::error::Release ${VERSION} changed $(echo "$CHANGED" | grep -c . ) config surface(s) a binary deploy does NOT apply. Apply them per docs/operations/deploy-config-apply.md, then re-run deploy with -f config_acknowledged=true (or pass it now if you have already applied them). Binaries are already deployed; this gate only flags the outstanding config-apply."
exit 1
