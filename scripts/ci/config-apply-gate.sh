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
# This gate diffs the deploying version against the previous release tag
# over the config surfaces. If any changed and the operator did NOT pass
# config_acknowledged=true, it FAILS — a loud, NON-destructive forcing
# function (the binaries are already deployed by an earlier step; a red
# gate just says "config still needs applying", which is accurate and
# actionable). Acknowledging asserts the operator will apply / has applied
# the config per the runbook.
#
# Usage: config-apply-gate.sh <version-tag> [acknowledged]
#   <version-tag>  the deploying tag, e.g. v0.43.0
#   [acknowledged] "true" if the operator passed config_acknowledged=true
set -uo pipefail

VERSION="${1:?deploying version tag, e.g. v0.43.0}"
ACK="${2:-false}"

# Config surfaces a binary deploy does NOT apply.
SURFACES=(
  'configs/ansible/roles/archival-node/templates/'
  'configs/prometheus/rules.r1/'
  'deploy/monitoring/rules/'
  'deploy/systemd/'
  'deploy/clickhouse/'
)

# The previous release tag = the config baseline currently live on the host.
PREV="$(git describe --tags --abbrev=0 "${VERSION}^" 2>/dev/null || true)"
if [ -z "$PREV" ]; then
  echo "note: no release tag before ${VERSION}; skipping config-drift gate (first release / shallow clone)."
  exit 0
fi

CHANGED="$(git diff --name-only "$PREV" "$VERSION" -- "${SURFACES[@]}" 2>/dev/null || true)"

SUMMARY="${GITHUB_STEP_SUMMARY:-/dev/stdout}"

if [ -z "$CHANGED" ]; then
  echo "✓ no config-surface changes between ${PREV} and ${VERSION} — the binary deploy is complete."
  echo "- ✓ config-apply gate: no config-surface changes between \`${PREV}\` and \`${VERSION}\`." >>"$SUMMARY" 2>/dev/null || true
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
