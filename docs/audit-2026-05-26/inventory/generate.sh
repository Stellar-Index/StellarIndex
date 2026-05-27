#!/usr/bin/env bash
# inventory/generate.sh — regenerates the inventory artefacts in
# this directory from the current working tree.
#
# Run from repo root: `bash docs/audit-2026-05-26/inventory/generate.sh`
#
# Outputs are committed; the script is idempotent. Re-run during
# the audit whenever the tree changes materially.

set -uo pipefail
# Intentionally NOT `set -e`: many checks (`ls` on optional paths,
# `grep` returning nothing) are non-fatal informational steps.

cd "$(git rev-parse --show-toplevel)"
DEST="docs/audit-2026-05-26/inventory"
mkdir -p "$DEST"

# ─── repo-snapshot.md ───────────────────────────────────────────
cat > "$DEST/repo-snapshot.md" <<EOF
# Repo Snapshot

Generated $(date -u +%Y-%m-%dT%H:%M:%SZ) by \`inventory/generate.sh\`.

## State

- Commit: \`$(git rev-parse HEAD)\`
- Branch: \`$(git rev-parse --abbrev-ref HEAD)\`
- Worktree clean: $([ -z "$(git status --porcelain)" ] && echo "yes" || echo "**dirty — review before trusting inventory**")
- Tracked files: $(git ls-files | wc -l | tr -d ' ')
- Untracked files: $(git ls-files --others --exclude-standard | wc -l | tr -d ' ')

## Top-level directories

\`\`\`
$(ls -d */ 2>/dev/null | sort)
\`\`\`

## Recent commits

\`\`\`
$(git log --oneline -10)
\`\`\`
EOF

# ─── area-counts.md ─────────────────────────────────────────────
{
  echo "# Area Counts"
  echo
  echo "Generated $(date -u +%Y-%m-%dT%H:%M:%SZ)."
  echo
  echo "| Area | Tracked files |"
  echo "| --- | --- |"
  for d in cmd internal pkg web migrations test scripts docs deploy configs docker .github examples openapi; do
    if [ -d "$d" ]; then
      n=$(git ls-files "$d/" 2>/dev/null | wc -l | tr -d ' ')
      echo "| \`$d/\` | $n |"
    fi
  done
  echo
  echo "## Go LOC by area"
  echo
  echo "\`\`\`"
  for d in cmd/ internal/ pkg/; do
    n=$(git ls-files "$d*.go" 2>/dev/null | xargs wc -l 2>/dev/null | tail -1)
    printf "%-20s %s\n" "$d" "$n"
  done
  echo "\`\`\`"
} > "$DEST/area-counts.md"

# ─── file-coverage.tsv stub ─────────────────────────────────────
# One row per tracked file. Auditor fills file_kind / workstream / status.
{
  printf "file_path\tfile_kind\tworkstream\tstatus\tevidence_refs\tnotes\n"
  git ls-files | while read -r f; do
    # Heuristic file_kind from extension/path
    kind="unknown"
    case "$f" in
      *_test.go) kind="test" ;;
      *.go) kind="runtime" ;;
      migrations/*.sql) kind="migration" ;;
      .github/workflows/*) kind="workflow" ;;
      scripts/*|examples/curl/*.sh) kind="script" ;;
      docs/*) kind="documentation" ;;
      deploy/*|configs/*|docker/*) kind="config" ;;
      web/*) kind="frontend" ;;
      test/fixtures/*) kind="fixture" ;;
      *.yaml|*.yml|*.toml|*.json|*.csv) kind="config" ;;
      *.md) kind="documentation" ;;
      *.sh) kind="script" ;;
      *.js|*.mjs|*.ts|*.tsx) kind="frontend" ;;
      LICENSE|CODE_OF_CONDUCT.md|CONTRIBUTING.md|SECURITY.md|CODEOWNERS|README.md|CLAUDE.md|CHANGELOG.md|VERSIONS.md|AGENTS.md) kind="policy" ;;
      Makefile|go.mod|go.sum|.gitignore|.gitleaks.toml|.gitattributes) kind="config" ;;
      .gitkeep) kind="asset" ;;
      *) kind="unknown" ;;
    esac
    # Default workstream: heuristic, auditor revises
    ws="?"
    case "$f" in
      cmd/ratesengine-ops/*backfill*) ws="W29" ;;
      cmd/ratesengine-ops/verify_archive*) ws="W34" ;;
      cmd/*) ws="W13" ;;
      internal/sources/external/*) ws="W08" ;;
      internal/sources/sorobanevents/*) ws="W27" ;;
      internal/sources/*) ws="W07" ;;
      internal/storage/*) ws="W09" ;;
      internal/api/*) ws="W11" ;;
      internal/aggregate/*) ws="W10" ;;
      internal/divergence/*) ws="W10" ;;
      internal/auth/*) ws="W19" ;;
      internal/customerwebhook/*) ws="W32" ;;
      internal/supply/*) ws="W12" ;;
      internal/canonical/*|internal/scval/*) ws="W05" ;;
      internal/dispatcher/*|internal/pipeline/*|internal/ledgerstream/*|internal/hashdb/*|internal/archivecompleteness/*) ws="W06" ;;
      internal/obs/*|internal/obstest/*) ws="W14" ;;
      internal/metadata/*) ws="W12" ;;
      internal/cachekeys/*) ws="W09" ;;
      internal/ratelimit/*|internal/platform/*|internal/usage/*) ws="W19" ;;
      internal/notify/*) ws="W19" ;;
      internal/incidents/*) ws="W11" ;;
      internal/consumer/*|internal/stellarrpc/*|internal/events/*) ws="W02" ;;
      internal/currency/*) ws="W12" ;;
      internal/config/*) ws="W02" ;;
      internal/version/*) ws="W01" ;;
      migrations/*) ws="W09" ;;
      .github/workflows/*) ws="W03" ;;
      docs/adr/*) ws="W02" ;;
      docs/operations/runbooks/*) ws="W14" ;;
      docs/operations/wasm-audits/*) ws="W24" ;;
      docs/operations/*) ws="W13" ;;
      docs/architecture/*) ws="W02" ;;
      docs/reference/*) ws="W25" ;;
      docs/audit-*) ws="W16" ;;
      docs/*) ws="W16" ;;
      web/explorer/*) ws="W17" ;;
      web/dashboard/*) ws="W17" ;;
      web/status/*) ws="W17" ;;
      deploy/monitoring/*) ws="W14" ;;
      deploy/systemd/*) ws="W18" ;;
      deploy/*) ws="W18" ;;
      configs/ansible/*) ws="W18" ;;
      configs/prometheus/*|configs/alertmanager/*|configs/loki/*) ws="W14" ;;
      configs/caddy/*) ws="W19" ;;
      configs/healthchecks/*) ws="W14" ;;
      configs/audit/*) ws="W24" ;;
      configs/*) ws="W18" ;;
      docker/*) ws="W18" ;;
      scripts/ci/*) ws="W03" ;;
      scripts/dev/*) ws="W03" ;;
      scripts/ops/*) ws="W13" ;;
      test/integration/*) ws="W15" ;;
      test/chaos/*|test/load/*) ws="W15" ;;
      test/fixtures/*) ws="W07" ;;
      pkg/client/*) ws="W11" ;;
      openapi/*) ws="W11" ;;
      examples/*) ws="W25" ;;
      .gitignore|.gitleaks.toml|.gitattributes|commitlint.config.js) ws="W01" ;;
      .github/PULL_REQUEST_TEMPLATE.md|.github/ISSUE_TEMPLATE/*|.github/RELEASE_NOTES_TEMPLATE.md) ws="W01" ;;
      .github/dependabot.yml) ws="W04" ;;
      .golangci.yml|.spectral.yaml) ws="W03" ;;
      README.md|CLAUDE.md|CHANGELOG.md|LICENSE|CODE_OF_CONDUCT.md|CONTRIBUTING.md|SECURITY.md|CODEOWNERS|VERSIONS.md|AGENTS.md|Makefile|go.mod|go.sum) ws="W01" ;;
      *) ws="?" ;;
    esac
    printf "%s\t%s\t%s\ttodo\t\t\n" "$f" "$kind" "$ws"
  done
} > "$DEST/file-coverage.tsv"

# ─── adr-inventory.md ───────────────────────────────────────────
{
  echo "# ADR Inventory"
  echo
  echo "Generated $(date -u +%Y-%m-%dT%H:%M:%SZ)."
  echo
  echo "| ADR | Title | Status | Audit-status |"
  echo "| --- | --- | --- | --- |"
  for f in docs/adr/[0-9][0-9][0-9][0-9]-*.md; do
    [ -f "$f" ] || continue
    n=$(basename "$f" | cut -d- -f1)
    title=$(head -1 "$f" | sed 's/^# *//; s/^ADR-[0-9]* *: *//')
    status=$(grep -m1 -E '^Status:' "$f" 2>/dev/null | sed 's/Status: *//' || echo "?")
    echo "| $n | $title | $status | todo |"
  done
} > "$DEST/adr-inventory.md"

# ─── migration-inventory.md ─────────────────────────────────────
{
  echo "# Migration Inventory"
  echo
  echo "Generated $(date -u +%Y-%m-%dT%H:%M:%SZ)."
  echo
  echo "| Number | Up filename | Has matching down | Workstream |"
  echo "| --- | --- | --- | --- |"
  for up in migrations/[0-9][0-9][0-9][0-9]_*.up.sql; do
    [ -f "$up" ] || continue
    n=$(basename "$up" | cut -d_ -f1)
    base=$(basename "$up" .up.sql)
    down="migrations/${base}.down.sql"
    [ -f "$down" ] && hasdown="yes" || hasdown="**no**"
    echo "| $n | \`$(basename "$up")\` | $hasdown | W09 |"
  done
} > "$DEST/migration-inventory.md"

# ─── source-decoder-inventory.md ────────────────────────────────
{
  echo "# Source-Decoder Inventory"
  echo
  echo "Generated $(date -u +%Y-%m-%dT%H:%M:%SZ)."
  echo
  echo "## On-chain sources under \`internal/sources/\` (excluding \`external/\`)"
  echo
  echo "| Source | Files | LOC | Has tests | Has fixture | Workstream |"
  echo "| --- | --- | --- | --- | --- | --- |"
  for d in internal/sources/*/; do
    name=$(basename "$d")
    if [ "$name" = "external" ] || [ "$name" = "forex" ] || [ "$name" = "frankfurter" ]; then
      continue
    fi
    files=$(git ls-files "$d" | wc -l | tr -d ' ')
    loc=$(git ls-files "$d*.go" 2>/dev/null | xargs wc -l 2>/dev/null | tail -1 | awk '{print $1}')
    [ -z "$loc" ] && loc="0"
    hastest=$(ls "$d"*_test.go 2>/dev/null | head -1)
    [ -n "$hastest" ] && hastest="yes" || hastest="no"
    hasfx=$(ls "test/fixtures/$name" 2>/dev/null | head -1)
    [ -n "$hasfx" ] && hasfx="yes" || hasfx="no"
    ws="W07"
    [ "$name" = "sorobanevents" ] && ws="W27"
    echo "| $name | $files | $loc | $hastest | $hasfx | $ws |"
  done
  echo
  echo "## External adapters under \`internal/sources/external/\`"
  echo
  echo "| Adapter | Files | LOC | Has tests | Workstream |"
  echo "| --- | --- | --- | --- | --- |"
  for d in internal/sources/external/*/; do
    name=$(basename "$d")
    files=$(git ls-files "$d" | wc -l | tr -d ' ')
    loc=$(git ls-files "$d*.go" 2>/dev/null | xargs wc -l 2>/dev/null | tail -1 | awk '{print $1}')
    [ -z "$loc" ] && loc="0"
    hastest=$(ls "$d"*_test.go 2>/dev/null | head -1)
    [ -n "$hastest" ] && hastest="yes" || hastest="no"
    echo "| $name | $files | $loc | $hastest | W08 |"
  done
} > "$DEST/source-decoder-inventory.md"

# ─── workflow-inventory.md ──────────────────────────────────────
{
  echo "# Workflow Inventory"
  echo
  echo "Generated $(date -u +%Y-%m-%dT%H:%M:%SZ)."
  echo
  echo "| Workflow | Triggers (rough) |"
  echo "| --- | --- |"
  for f in .github/workflows/*.yml; do
    triggers=$(grep -A1 '^on:' "$f" | tr '\n' ' ' | sed 's/  */ /g')
    echo "| \`$(basename "$f")\` | $triggers |"
  done
} > "$DEST/workflow-inventory.md"

# ─── api-route-inventory.md (rough — auditor refines) ────────────
{
  echo "# API Route Inventory (rough)"
  echo
  echo "Generated $(date -u +%Y-%m-%dT%H:%M:%SZ). Auditor refines."
  echo
  echo "## Handlers"
  echo
  ls internal/api/v1/*.go | grep -v _test
  echo
  echo "## Routes mounted in server.go"
  echo
  echo "\`\`\`"
  grep -nE '"GET |"POST |"PUT |"PATCH |"DELETE |s\.mux\.Handle|http\.Handle' internal/api/v1/server.go 2>/dev/null | head -40
  echo "\`\`\`"
} > "$DEST/api-route-inventory.md"

# ─── alert-rule-inventory.md ────────────────────────────────────
{
  echo "# Alert Rule Inventory"
  echo
  echo "Generated $(date -u +%Y-%m-%dT%H:%M:%SZ)."
  echo
  echo "## Multi-host rules: \`deploy/monitoring/rules/\`"
  echo
  for f in deploy/monitoring/rules/*.yml; do
    [ -f "$f" ] || continue
    echo "### $(basename "$f")"
    grep -E '^\s*- alert:' "$f" | sed 's/^/    /'
    echo
  done
  echo "## R1 overlay: \`configs/prometheus/rules.r1/\`"
  echo
  for f in configs/prometheus/rules.r1/*.yml; do
    [ -f "$f" ] || continue
    echo "### $(basename "$f")"
    grep -E '^\s*- alert:' "$f" | sed 's/^/    /'
    echo
  done
} > "$DEST/alert-rule-inventory.md"

# ─── runbook-inventory.md ───────────────────────────────────────
{
  echo "# Runbook Inventory"
  echo
  echo "Generated $(date -u +%Y-%m-%dT%H:%M:%SZ)."
  echo
  echo "| Runbook | First-section heading |"
  echo "| --- | --- |"
  for f in docs/operations/runbooks/*.md; do
    [ "$(basename "$f")" = "_template.md" ] && continue
    [ -f "$f" ] || continue
    title=$(head -1 "$f" | sed 's/^# *//')
    echo "| \`$(basename "$f")\` | $title |"
  done
} > "$DEST/runbook-inventory.md"

# ─── external-source-inventory.md ───────────────────────────────
# Best-effort from registry.go content
{
  echo "# External Source Inventory"
  echo
  echo "Generated $(date -u +%Y-%m-%dT%H:%M:%SZ)."
  echo
  echo "Adapter directories:"
  echo
  for d in internal/sources/external/*/; do
    n=$(basename "$d")
    echo "- \`$n\`"
  done
} > "$DEST/external-source-inventory.md"

# ─── metric-name-inventory.md ───────────────────────────────────
{
  echo "# Metric Name Inventory"
  echo
  echo "Generated $(date -u +%Y-%m-%dT%H:%M:%SZ)."
  echo
  echo "Metric names registered in \`internal/obs/metrics.go\`:"
  echo
  echo "\`\`\`"
  grep -hE '(prometheus\.NewCounterVec|prometheus\.NewGaugeVec|prometheus\.NewHistogramVec|prometheus\.NewSummaryVec)' internal/obs/metrics.go | head -50
  echo "..."
  echo "\`\`\`"
  echo
  echo "Auditor cross-references against:"
  echo "- \`docs/reference/metrics/README.md\`"
  echo "- \`deploy/monitoring/rules/*.yml\` rule expressions"
  echo "- \`configs/prometheus/rules.r1/*.yml\` rule expressions"
} > "$DEST/metric-name-inventory.md"

# ─── docker-systemd-inventory.md ────────────────────────────────
{
  echo "# Docker + Systemd Inventory"
  echo
  echo "Generated $(date -u +%Y-%m-%dT%H:%M:%SZ)."
  echo
  echo "## Dockerfiles"
  echo
  for f in docker/*.Dockerfile; do
    [ -f "$f" ] || continue
    echo "- \`$f\`"
  done
  echo
  echo "## Systemd units under \`deploy/systemd/\`"
  echo
  for f in deploy/systemd/*; do
    [ -f "$f" ] || continue
    echo "- \`$(basename "$f")\`"
  done
} > "$DEST/docker-systemd-inventory.md"

# ─── webfrontend-inventory.md ───────────────────────────────────
{
  echo "# Web Frontend Inventory"
  echo
  echo "Generated $(date -u +%Y-%m-%dT%H:%M:%SZ)."
  echo
  for d in web/*/; do
    n=$(basename "$d")
    [ "$n" = "node_modules" ] && continue
    [ -f "$d/package.json" ] || continue
    echo "## \`web/$n/\`"
    echo
    files=$(git ls-files "$d" | wc -l | tr -d ' ')
    echo "- Tracked files: $files"
    [ -f "$d/package.json" ] && {
      name=$(grep '"name"' "$d/package.json" | head -1 | sed 's/.*: *"//;s/".*//')
      echo "- Package: \`$name\`"
    }
    echo
  done
} > "$DEST/webfrontend-inventory.md"

# ─── dependency-inventory.md ────────────────────────────────────
{
  echo "# Dependency Inventory"
  echo
  echo "Generated $(date -u +%Y-%m-%dT%H:%M:%SZ)."
  echo
  echo "## Direct Go deps (from \`go.mod\`)"
  echo
  echo "\`\`\`"
  awk '/^require *\(/,/^\)/' go.mod | grep -vE 'require|^\)' | head -40
  echo "..."
  echo "\`\`\`"
  echo
  echo "## Frontend deps (per package.json)"
  echo
  for d in web/*/; do
    [ -f "$d/package.json" ] || continue
    echo "### \`web/$(basename "$d")\`"
    echo
    echo "\`\`\`"
    grep -E '"dependencies"|"devDependencies"' "$d/package.json" || true
    echo "\`\`\`"
  done
} > "$DEST/dependency-inventory.md"

# ─── every-event-coverage.tsv stub ──────────────────────────────
# Auditor populates per W35
if [ ! -f "$DEST/every-event-coverage.tsv" ]; then
  printf "source\tcontract\tevent_topic_0\tevent_topic_1\tclassified_by_decoder\tpersisted_to_table\ttested\tnotes\n" > "$DEST/every-event-coverage.tsv"
fi

echo "Inventory regenerated under $DEST"
ls -la "$DEST"
