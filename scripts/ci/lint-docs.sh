#!/usr/bin/env bash
# Doc-code consistency linter for Stellar Index.
#
# Runs in CI; fails the build if docs have drifted from code.
# Based on the pattern from ~/code/loop-app/scripts/lint-docs.sh —
# adapted for our Go + OpenAPI + Stellar-specific surface.
#
# Design principles (docs/engineering-standards.md §5):
#
#   1. Never two sources of truth.
#   2. Explain why, not what.
#   3. Decisions go in ADRs; narrative docs don't record decisions.
#   4. Every config option / metric / endpoint must round-trip
#      between code and reference docs.
#
# This script enforces (1) + (4). The others are reviewer-enforced.

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$REPO_ROOT"

ERROR_FILE=$(mktemp)
echo "0" > "$ERROR_FILE"

err() {
  echo "  ERROR: $1" >&2
  count=$(cat "$ERROR_FILE")
  echo "$((count + 1))" > "$ERROR_FILE"
}

# ─── 0. Load-bearing source-of-truth inputs must exist ──────────────────────
#
# Every check below is gated on `if [ -f X ] && [ -f Y ]`, which turns a
# missing/renamed load-bearing input into a SILENT SKIP: the check's body
# never runs, the error counter stays 0, and the script prints "passed".
# Renaming (say) config.go or the OpenAPI spec would hollow out its doc-sync
# check with no signal. Assert the load-bearing config/source/spec paths up
# front so a rename fails the lint instead. Optional/best-effort inputs
# (runbooks, incidents, web CSP headers, R1 overlays) are intentionally NOT
# listed here — they are legitimately absent in some checkouts.

require_path() { # require_path <-f|-d> <path>
  case "$1" in
    -f) [ -f "$2" ] || err "REQUIRED input missing: file '$2' not found — a rename/move would silently skip its doc-sync check below; restore it or update lint-docs.sh" ;;
    -d) [ -d "$2" ] || err "REQUIRED input missing: directory '$2' not found — a rename/move would silently skip its doc-sync check below; restore it or update lint-docs.sh" ;;
  esac
}

echo "Checking load-bearing inputs exist..."
require_path -f internal/config/config.go
require_path -f docs/reference/config/README.md
require_path -d internal/api/v1
require_path -f openapi/stellar-index.v1.yaml
require_path -f docs/reference/api/stellar-index.v1.yaml
require_path -d internal/obs
require_path -f docs/reference/metrics/README.md

# ─── 1. Every config `toml:"..."` tag must appear in the generated ref ──────
#
# The generated reference (docs/reference/config/README.md) uses the
# TOML field name (`toml:"xxx_yyy"` → "xxx_yyy" in the table), not the
# Go field identifier. So we check TOML names, not Go field names —
# the wire contract is what operators see.

echo "Checking config reference sync..."
if [ -f internal/config/config.go ] && [ -f docs/reference/config/README.md ]; then
  # Extract every `toml:"name"` tag value from config.go. Keeps only
  # the name (no commas, no omitempty).
  # CS-131: [a-z0-9_]+ (was [a-z_]+, which silently skipped digit-bearing
  # tags like s3_*, sep10, sep41, phase2 — a rename of one would stay green).
  grep -oE 'toml:"[a-z0-9_]+"' internal/config/config.go | \
    sed -E 's/toml:"([a-z0-9_]+)"/\1/' | sort -u | while read -r tomlname; do
      if ! grep -qF "$tomlname" docs/reference/config/README.md; then
        err "Config TOML key '$tomlname' in config.go missing from docs/reference/config/README.md — run 'make docs-config' to regen"
      fi
  done
fi

# ─── 2. Every API route handler must be in OpenAPI ──────────────────────────
#
# Matches the idiom the v1 Server uses:
#   s.mux.HandleFunc("GET /v1/<path>", s.handleX)
# The OpenAPI spec lists routes WITHOUT the /v1 prefix (that's the
# server's base URL), so we strip /v1 before comparing.

echo "Checking API routes vs OpenAPI..."
if [ -d internal/api/v1 ] && [ -f openapi/stellar-index.v1.yaml ]; then
  # Forward: handlers that aren't in the spec (client misses them).
  # CS-052: match BOTH HandleFunc("VERB /v1…") and mux.Handle("VERB /v1…")
  # — the latter is used for middleware-wrapped routes and previously slipped
  # past this check (that's how the undocumented staff route escaped).
  # internal_routes_re allow-lists routes deliberately kept out of the public
  # spec (staff/PII endpoints); add a route here with a reason to exempt it.
  internal_routes_re='^/account/admin/'  # staff-only lookup, intentionally not public
  grep -rhoE 'Handle(Func)?\("[A-Z]+ /v1[^"]*"' internal/api/v1/ 2>/dev/null | \
    sed -E 's|.*"[A-Z]+ /v1||; s|"$||' | \
    sed -E 's|^$|/|' | \
    sort -u | while IFS= read -r route; do
      [ -z "$route" ] && continue
      if [[ "$route" =~ $internal_routes_re ]]; then continue; fi
      # OpenAPI path entries look like `  /ohlc:` at 2-space indent.
      if ! grep -qE "^  ${route}:" openapi/stellar-index.v1.yaml; then
        err "Route '$route' is registered in handlers but missing from OpenAPI spec"
      fi
  done

  # Reverse: spec entries that have no handler (clients 404).
  # The planned_regex below is the explicit allow-list of
  # "documented but not yet shipped" — deliberately adjusted in
  # a docs PR when endpoints land or get cut. Empty today —
  # every spec path has a handler. If you add a new doc-but-stub
  # endpoint, add it here and remove it once the handler lands.
  planned_regex='^$'
  grep -oE "^  /[^:]+:" openapi/stellar-index.v1.yaml | \
    sed -E 's|^  ||; s|:$||' | sort -u | while IFS= read -r route; do
      [ -z "$route" ] && continue
      if [[ "$route" =~ $planned_regex ]]; then
        continue
      fi
      # Fixed-string search (no regex) so Go 1.22 path params
      # like /assets/{asset_id} don't get interpreted as regex
      # quantifiers. Enumerate methods we use today — extend the
      # list when we add write verbs.
      found=0
      for method in GET POST PUT PATCH DELETE; do
        # CS-052: check both HandleFunc( and mux.Handle( registrations.
        if grep -qrF "HandleFunc(\"${method} /v1${route}\"" internal/api/v1/ 2>/dev/null \
          || grep -qrF "Handle(\"${method} /v1${route}\"" internal/api/v1/ 2>/dev/null; then
          found=1
          break
        fi
      done
      if [ "$found" -eq 0 ]; then
        err "OpenAPI path '$route' has no handler. Add a handler or add it to planned_regex in lint-docs.sh"
      fi
  done
fi

# ─── 2b. Generated API reference must be in sync with the source spec ────────
#
# `make docs-api` copies openapi/stellar-index.v1.yaml verbatim next to the
# rendered index.html. CI's `openapi` job enforces this too — but that job is
# PR-only + path-filtered, so a direct-to-main push that edits the spec
# without re-running `make docs-api` slips the desync onto main (observed
# once: the reference was 66 paths while the spec shipped 73). This lint
# runs inside verify.sh on every push, so the gap is closed locally
# regardless of the CI trigger.

echo "Checking generated API reference sync..."
if [ -f openapi/stellar-index.v1.yaml ] && [ -f docs/reference/api/stellar-index.v1.yaml ]; then
  if ! diff -q openapi/stellar-index.v1.yaml docs/reference/api/stellar-index.v1.yaml >/dev/null 2>&1; then
    err "docs/reference/api/stellar-index.v1.yaml is out of sync with openapi/stellar-index.v1.yaml — run 'make docs-api' and commit the result"
  fi
fi

# ─── 3. Every Prometheus metric must be documented in metrics reference ─────

echo "Checking metrics registry..."
if [ -d internal/obs ] && [ -f docs/reference/metrics/README.md ]; then
  # Every metric registered in internal/obs must appear in the
  # reference doc. Scope is all prometheus `Name: "..."` fields —
  # not just stellarindex_*/ctx_* — so `http_requests_total` and
  # `http_request_duration_seconds` (unprefixed per standard
  # Prometheus convention) are also enforced.
  # BSD sed (macOS default) doesn't support \s — use [[:space:]].
  grep -rhE 'Name:[[:space:]]*"[a-z][a-z0-9_]+"' internal/obs/ 2>/dev/null | \
    sed -E 's|.*Name:[[:space:]]*"([^"]+)".*|\1|' | sort -u | while read -r metric; do
      if ! grep -qF "$metric" docs/reference/metrics/README.md; then
        err "Metric '$metric' is registered in code but not in docs/reference/metrics/README.md"
      fi
  done
fi

# ─── 4. No references to deleted files / renamed concepts ───────────────────

echo "Checking for stale references..."
stale_patterns=(
  "horizon\.stellar\.org"        # Horizon deprecated — ADR-0001
  "stellarindex\.ctx\.io"         # old placeholder domain
  "ctx-indexer\|ctx-aggregator\|ctx-api\|ctx-ops\|ctx-migrate" # old binary names (we use stellarindex- prefix now — adjust if you change the policy)
  "CTX Rates"                    # old project name (now "Stellar Index")
)
for pattern in "${stale_patterns[@]}"; do
  matches=$(grep -rnE "$pattern" \
    README.md \
    CLAUDE.md \
    AGENTS.md \
    CONTRIBUTING.md \
    SECURITY.md \
    CODE_OF_CONDUCT.md \
    CHANGELOG.md \
    docs/reference/ \
    docs/architecture/ \
    docs/operations/ \
    docs/development/ \
    2>/dev/null | grep -v "node_modules\|_archive/\|discovery/" || true)
  if [ -n "$matches" ]; then
    err "Stale reference to '$pattern' in active docs:"
    echo "$matches" | sed 's/^/    /' >&2
  fi
done

# ─── 5. No forbidden tech-debt markers without issue links ──────────────────

echo "Checking TODO discipline..."
# Every TODO/FIXME/XXX in Go code must be of the form TODO(#N):
if [ -d internal ] || [ -d cmd ]; then
  # Match EVERY TODO/FIXME/XXX, then let the second grep exempt only the
  # tracked `(#123)` form. The pattern used to end in `[^(]`, which meant
  # the first grep never fired on a parenthesised TODO at all — so the
  # exemption grep was dead weight and `TODO(later):`, `FIXME(nobody)` and
  # a bare `// TODO` at end-of-line all passed silently. Cold audit
  # 2026-08-04.
  bad_todos=$(grep -rnE '//[[:space:]]*(TODO|FIXME|XXX)' \
    internal/ cmd/ pkg/ 2>/dev/null | \
    grep -vE '//\s*(TODO|FIXME|XXX)\(#[0-9]+\)' || true)
  if [ -n "$bad_todos" ]; then
    err "TODO/FIXME/XXX without linked issue number (must be 'TODO(#123): …'):"
    echo "$bad_todos" | sed 's/^/    /' >&2
  fi
fi

# ─── 6. Frontmatter freshness on 'current' docs ─────────────────────────────

echo "Checking doc frontmatter freshness..."
today=$(date -u +%s)
stale_threshold=$((90 * 24 * 60 * 60))   # 90 days in seconds
fail_threshold=$((180 * 24 * 60 * 60))   # 180 days — hard fail

# Iterate over 'current' docs. Docs without frontmatter are ignored at
# this level — we're not forcing frontmatter on every file, only on
# docs in the opt-in 'current' tracking set (architecture/, operations/, adr/).
find docs/architecture docs/operations docs/adr -type f -name '*.md' 2>/dev/null | while read -r f; do
  # Skip generated docs, archive, templates.
  if grep -q "GENERATED FILE - DO NOT EDIT" "$f" 2>/dev/null; then continue; fi
  if [[ "$f" == *"_archive"* ]] || [[ "$f" == *"_template"* ]]; then continue; fi

  # Extract last_verified date from frontmatter if present.
  verified=$(awk '/^last_verified:/{print $2; exit}' "$f" 2>/dev/null | tr -d '"')
  if [ -z "$verified" ]; then continue; fi

  verified_epoch=$(date -u -j -f "%Y-%m-%d" "$verified" +%s 2>/dev/null || \
                   date -u -d "$verified" +%s 2>/dev/null || echo "")
  if [ -z "$verified_epoch" ]; then continue; fi

  age=$((today - verified_epoch))
  if [ "$age" -gt "$fail_threshold" ]; then
    err "Doc '$f' is STALE — last_verified $verified is > 180 days old"
  elif [ "$age" -gt "$stale_threshold" ]; then
    echo "  WARN: doc '$f' last_verified $verified is > 90 days old — refresh soon" >&2
  fi
done

# ─── 7. Generated-file banner intact ────────────────────────────────────────
#
# Only the three generated subdirs under docs/reference/ are machine-
# produced. docs/reference/*.md at the top level is hand-written
# narrative (e.g. api-design.md).

echo "Checking generated-file banners..."
# docs/reference/metrics/README.md is the ONLY hand-written file
# under docs/reference/ — there's no metrics generator yet (would
# need a Prometheus-registry walker). It's still lint-enforced
# for drift via section 3. Exempt only by exact path.
#
# Enumerate only existing subdirs — `find` errors on missing ones
# with set -e + pipefail, silently killing the script before later
# sections run.
gen_dirs=()
for d in docs/reference/api docs/reference/config docs/reference/metrics; do
  [ -d "$d" ] && gen_dirs+=("$d")
done
if [ ${#gen_dirs[@]} -gt 0 ]; then
  find "${gen_dirs[@]}" -type f -name '*.md' 2>/dev/null | while read -r f; do
    if [ "$f" = "docs/reference/metrics/README.md" ]; then
      continue
    fi
    if ! head -1 "$f" | grep -qF "GENERATED FILE"; then
      err "Generated file '$f' is missing the 'GENERATED FILE - DO NOT EDIT' banner at line 1"
    fi
  done
fi

# ─── 8. Every ADR has valid status + not-superseded-unless-noted ────────────

echo "Checking ADR integrity..."
if [ -d docs/adr ]; then
  for adr in docs/adr/[0-9]*.md; do
    [ -f "$adr" ] || continue
    status=$(awk '/^status:/{print $2; exit}' "$adr")
    if [[ ! "$status" =~ ^(Proposed|Accepted|Superseded|Rejected)$ ]]; then
      err "ADR '$adr' has invalid status '$status' (must be Proposed|Accepted|Superseded|Rejected)"
    fi
    superseded_by=$(awk '/^superseded_by:/{print $2; exit}' "$adr" | tr -d '"')
    if [ "$status" = "Superseded" ] && { [ "$superseded_by" = "null" ] || [ -z "$superseded_by" ]; }; then
      err "ADR '$adr' marked Superseded but 'superseded_by' is null"
    fi
  done
fi

# ─── 9. Every alert rule's runbook_url must live in ANNOTATIONS + exist ─────
#
# Prometheus alert rules ship with `runbook_url` so the pager routes
# oncall to a specific diagnosis page. Two things must hold:
#   (a) it lives under `annotations:`, NOT `labels:` — Alertmanager keeps
#       .Labels and .Annotations strictly separate and both Discord
#       templates render the runbook line from .Annotations.runbook_url,
#       so a runbook_url stashed in `labels:` renders nothing (audit C4-1:
#       266/270 alerts had it in the wrong block → no page ever showed a
#       runbook link);
#   (b) a local runbook target points at a file that exists — a 404 URL
#       dumps the responder on a GitHub error page at 3 AM.
# The pre-C4-1 version of this check was a blind grep over raw text, so it
# matched a `runbook_url:` string in either block and never noticed (a).
# This now delegates to the YAML-aware scripts/ci/lint-runbook-annotations.py
# which PARSES each rule and asserts (a) + (b) per alert across both trees.

echo "Checking alert-rule runbook_url annotations..."
if runbook_out=$(python3 scripts/ci/lint-runbook-annotations.py 2>&1); then
  : # ok
else
  while IFS= read -r line; do
    [ -z "$line" ] && continue
    case "$line" in
      *"problem(s) found"*) continue ;;  # summary line, not a distinct error
    esac
    err "runbook-annotations:$line"
  done <<< "$runbook_out"
fi

# ─── 10. Every alert rule must have a row in the alerts catalogue ──────────
#
# Catalogue is docs/operations/alerts-catalog.md; every rule file's
# `alert: <name>` must appear verbatim somewhere in that doc. Caught
# the `stellarindex_ingestion_insert_errors` drift on 2026-04-23 —
# the alert was live but the catalogue didn't list it.

echo "Checking alerts-catalog drift..."
if [ -d deploy/monitoring/rules ] && [ -f docs/operations/alerts-catalog.md ]; then
  grep -rhE '^[[:space:]]*-[[:space:]]*alert:[[:space:]]*' deploy/monitoring/rules/ 2>/dev/null | \
    sed -E 's|.*alert:[[:space:]]*||' | sort -u | while IFS= read -r alert; do
      [ -z "$alert" ] && continue
      if ! grep -qF "$alert" docs/operations/alerts-catalog.md; then
        err "alert rule '$alert' not listed in docs/operations/alerts-catalog.md"
      fi
    done
fi

# ─── 11. Runbook body references to `stellarindex_source_*` metrics ─────────
#
# Narrow rule: only `stellarindex_source_*` (the namespace fully
# owned by internal/obs/metrics.go). External-exporter metrics
# (stellarindex_stellar_core_*, pgbackrest_*, etc.) are intentionally
# out of scope — those live in node-side exporters we don't control.
#
# Caught `stellarindex_source_last_event_age_seconds` drift on
# 2026-04-23 — runbook referenced a metric name that never existed.

echo "Checking runbook metric-name freshness..."
if [ -d docs/operations/runbooks ] && [ -f internal/obs/metrics.go ]; then
  # Build the allowed set: names registered in obs.metrics.go +
  # alert names in Prometheus rules (runbooks use either). `|| true`
  # because under set -e + pipefail, grep returning 1 for no-match
  # would kill the whole script — we explicitly want an empty set
  # if no matches.
  allowed=$(mktemp)
  {
    (grep -hE 'Name:[[:space:]]*"stellarindex_source_[a-z_]+"' internal/obs/metrics.go 2>/dev/null || true) | \
      sed -E 's|.*"(stellarindex_source_[a-z_]+)".*|\1|'
    (grep -rhE '^[[:space:]]*-[[:space:]]*alert:[[:space:]]*stellarindex_source_' deploy/monitoring/rules/ 2>/dev/null || true) | \
      sed -E 's|.*alert:[[:space:]]*||'
  } | sort -u > "$allowed"

  # Extract every stellarindex_source_* token from runbook bodies.
  (grep -rhoE 'stellarindex_source_[a-z_]+' docs/operations/runbooks/ 2>/dev/null || true) | \
    sort -u | while IFS= read -r metric; do
      [ -z "$metric" ] && continue
      if ! grep -qxF "$metric" "$allowed"; then
        err "runbook references unknown metric '$metric' (not in internal/obs or rules/)"
      fi
    done
  rm -f "$allowed"
fi

# ─── 12. Every runbook referenced from alerts-catalog must exist ────────────
#
# Symmetric counterpart to §9 (which checks rule-file → runbook). The
# catalog is the operator-facing index; a stale `runbooks/X.md` link
# in it means oncall clicks through to a 404. Caught nothing yet —
# verified clean as of 2026-04-27 — but adding the check before the
# next runbook reorganisation introduces drift.

echo "Checking alerts-catalog runbook link freshness..."
if [ -f docs/operations/alerts-catalog.md ] && [ -d docs/operations/runbooks ]; then
  grep -oE 'runbooks/[a-z0-9-]+\.md' docs/operations/alerts-catalog.md | sort -u | while IFS= read -r path; do
    [ -z "$path" ] && continue
    if [ ! -f "docs/operations/$path" ]; then
      err "alerts-catalog references missing runbook: docs/operations/$path"
    fi
  done
fi

# ─── 13. Every operational runbook should be referenced ────────────────────
#
# Orphan runbooks are stale by definition — a runbook nobody can
# find isn't a runbook. Allow-list the four that intentionally
# stand alone (template, bring-up procedures, dead-man's switch).
# All other docs/operations/runbooks/*.md must appear in either
# alerts-catalog.md or sev-playbook.md or be cross-referenced from
# another runbook (chained-procedure case).

# ─── Prometheus rules: assert each multi-host rule has an R1 sibling ──
#
# Multi-host rules in `deploy/monitoring/rules/` use underscored job
# names (matching the ansible multi-host scrape config). R1's single-
# host overlay at `configs/prometheus/rules.r1/` mirrors the same
# alerts with hyphenated job names. Silent drift between the two —
# editing the multi-host file alone leaves R1 with a stale rule. This
# check flags any multi-host file that has no matching R1 sibling so
# reviewers catch the drift at CI time.

echo "Checking Prometheus rule pairing (multi-host ↔ R1 overlay)..."
if [ -d deploy/monitoring/rules ] && [ -d configs/prometheus/rules.r1 ]; then
  for r in deploy/monitoring/rules/*.yml; do
    fname="${r##*/}"
    if [ ! -f "configs/prometheus/rules.r1/$fname" ]; then
      err "Multi-host rule file $r has no configs/prometheus/rules.r1/$fname sibling. Either add an R1 overlay or remove this rule."
    fi
  done
fi

echo "Checking runbook orphans..."
if [ -d docs/operations/runbooks ]; then
  for r in docs/operations/runbooks/*.md; do
    fname="${r##*/}"
    case "$fname" in
      _template.md|README.md|bootstrap-archival-node.md|first-archival-node-deployment.md|deadmansswitch.md|post-phase0-deploy-sequence.md|consolidated-deploy-plan-2026-07-18.md|production-readiness-master-plan-2026-07-18.md|phase-a-capacity-relief-2026-07-18.md|off-site-backup-plan.md|storage-breakdown-2026-07-20.md) continue ;;
    esac
    # Look for a reference in alerts-catalog, sev-playbook, or peer runbooks.
    if ! grep -qrF "runbooks/$fname" docs/operations/ 2>/dev/null; then
      err "orphan runbook with no referrer: $r — link from alerts-catalog, sev-playbook, or another runbook"
    fi
  done
fi

# ─── 14. Alert-runbook section presence ────────────────────────────────────
#
# Per the runbook template at docs/operations/runbooks/_template.md
# (wave 78 refresh + wave 81 normalisation), `## At a glance` and
# `## Related` are the two universally-required sections on every
# alert runbook. Without `## At a glance` an operator paged at
# 3 AM has to read the body prose to learn severity / MTTR /
# impact; without `## Related` the cross-link graph rots and
# adjacent runbooks become undiscoverable.
#
# Exclude procedural runbooks (bring-up, disaster recovery,
# SEV-comms procedures, one-off operator notes) — they're
# legitimately shaped differently. The allow-list mirrors the
# orphan-lint exclusions above plus three procedural runbooks
# (`dr-activation`, `sev-status-page-update`, the dated operator
# note) flagged as not-alert-shaped during the wave-81 survey.

echo "Checking alert-runbook section presence..."
if [ -d docs/operations/runbooks ]; then
  for r in docs/operations/runbooks/*.md; do
    fname="${r##*/}"
    case "$fname" in
      _template.md|README.md|bootstrap-archival-node.md|first-archival-node-deployment.md|deadmansswitch.md|post-phase0-deploy-sequence.md|consolidated-deploy-plan-2026-07-18.md|production-readiness-master-plan-2026-07-18.md|phase-a-capacity-relief-2026-07-18.md|off-site-backup-plan.md|storage-breakdown-2026-07-20.md) continue ;;
      dr-activation.md|sev-status-page-update.md|operator-unblock-2026-05-08.md) continue ;;
    esac
    if ! grep -q "^## At a glance" "$r" 2>/dev/null; then
      err "runbook missing '## At a glance' section: $r — see docs/operations/runbooks/_template.md"
    fi
    if ! grep -q "^## Related" "$r" 2>/dev/null; then
      err "runbook missing '## Related' section: $r — see docs/operations/runbooks/_template.md"
    fi
  done
fi

# ─── 15. Incident post-mortem follow-up forcing function ──────────────────
#
# Fail if any user-facing incident (internal/incidents/data/*.md,
# served by /v1/incidents) is older than 30 days AND still has
# unchecked `[ ]` checkboxes in its body. Closes the meta-failure-
# mode where post-mortem action items rot indefinitely: the
# 2026-05-10 SEV-2 (redis-writes-blocked-disk-full) shipped with 4
# `[ ]` follow-ups and 17 days later the same cascade recurred
# (2026-05-26) with those follow-ups still unchecked. CI now
# enforces the cadence so a future post-mortem either gets its
# items closed within a month, or the unchecked items get
# explicitly rewritten as accepted-debt (`[~]` is treated as
# checked / acknowledged).
#
# Date is sourced from the filename slug `<YYYY-MM-DD>-<slug>.md`
# — matches the convention enforced by `internal/incidents`
# (frontmatter `started_at:` may be richer, but filename is the
# stable surface and the only thing this lint reads).

echo "Checking incident post-mortem follow-ups..."
INCIDENT_DIR="internal/incidents/data"
if [ -d "$INCIDENT_DIR" ]; then
  NOW_EPOCH=$(date -u +%s)
  THIRTY_DAYS_AGO=$((NOW_EPOCH - 30 * 86400))
  for incident in "$INCIDENT_DIR"/*.md; do
    [ -f "$incident" ] || continue
    fname="${incident##*/}"
    # Skip templates / underscored scratch files (mirrors
    # internal/incidents/incidents.go Load() behaviour).
    case "$fname" in
      _*) continue ;;
    esac
    # Extract YYYY-MM-DD from the slug; bail if no leading date.
    date_str=$(echo "$fname" | grep -oE '^[0-9]{4}-[0-9]{2}-[0-9]{2}' || true)
    [ -z "$date_str" ] && continue
    # BSD-first date parse (matches section 6 convention).
    incident_epoch=$(date -u -j -f "%Y-%m-%d" "$date_str" +%s 2>/dev/null || \
                     date -u -d "$date_str" +%s 2>/dev/null || echo "")
    [ -z "$incident_epoch" ] && continue
    if [ "$incident_epoch" -ge "$THIRTY_DAYS_AGO" ]; then
      # Inside the 30-day grace window — unchecked items still OK.
      continue
    fi
    # Count unchecked `[ ]` checkboxes (markdown task-list shape).
    # `[x]` / `[X]` / `[~]` are all treated as done/acknowledged.
    unchecked=$(grep -cE '^[[:space:]]*-[[:space:]]+\[ \]' "$incident" || true)
    if [ "$unchecked" -gt 0 ]; then
      err "incident '$incident' is older than 30 days and has $unchecked unchecked '[ ]' follow-up checkbox(es) — close them, mark as acknowledged with '[~]', or rewrite the action item."
    fi
  done
fi

# ─── 16. Production CSP must not permit http://localhost ──────────────────
#
# An earlier revision left `http://localhost:3000` in the Cloudflare
# Pages CSP `connect-src` of the explorer + status sites as a
# dev-convenience that leaked into production. The Next dev server
# doesn't apply _headers anyway, so the dev-build use case is moot;
# permitting localhost in prod CSP is pure config drift between dev and
# prod. This guard fails CI if it regresses.

for hf in web/explorer/public/_headers web/status/public/_headers; do
  if [ -f "$hf" ] && grep -qE 'Content-Security-Policy:.*localhost' "$hf"; then
    err "$hf permits 'localhost' in a Content-Security-Policy header — forbidden in production builds. Remove the localhost permit; dev work uses 'next dev' which doesn't read _headers."
  fi
done

# ─── 17. k6 AlertManager-silence matchers must exist and be non-paging ─────
#
# test/load/scenarios/lib/alertmanager.js hardcodes the default silence
# matchers the 99-spike load scenario posts to AlertManager. This is a
# two-sided drift risk (audit-2026-06-14 R-A20-1 and its follow-up):
#   (a) an alertname that matches NO deployed alert -> the silence is a
#       silent no-op and on-call pages during the planned burst (the
#       original finding: defaults were 'APIHighLatencyP95' when the
#       real alert is 'stellarindex_api_latency_p95_high').
#   (b) an alertname that IS deployed but carries `severity: page` ->
#       the silence masks a real SEV-1 for the run's duration (the
#       inverse, over-silencing failure — a stray
#       'alertname=stellarindex_api_error_rate_critical' shipped this
#       exact bug for several weeks).
# Both classes are re-derived here from the rule files (the source of
# truth), not hand-verified once, so a future alert rename or
# severity bump can't silently reopen either one.

echo "Checking k6 AlertManager-silence matcher targets..."
AM_JS="test/load/scenarios/lib/alertmanager.js"
if [ -f "$AM_JS" ]; then
  # Pull only the alertname=... tokens out of the `const matchers = (...)`
  # assignment — NOT the amtool dry-run example in the header comment,
  # which intentionally lists the same names for operator copy-paste.
  matcher_block=$(awk '/^const matchers = \(ENV\.ALERTMANAGER_SILENCE_MATCHERS/,/\.split\(.,.\);/' "$AM_JS")
  alertnames=$(echo "$matcher_block" | grep -oE 'alertname=[A-Za-z0-9_]+' | sed 's/alertname=//' | sort -u)
  if [ -z "$alertnames" ]; then
    err "$AM_JS: could not extract any default 'alertname=' matcher from the 'const matchers = (...)' assignment — the lint's regex has drifted from the source, update scripts/ci/lint-docs.sh §17"
  fi
  for name in $alertnames; do
    missing=""
    for dir in deploy/monitoring/rules configs/prometheus/rules.r1; do
      if ! grep -rq "alert: $name\$" "$dir"/*.yml 2>/dev/null; then
        missing="$missing $dir"
      fi
    done
    if [ -n "$missing" ]; then
      err "$AM_JS default matcher 'alertname=$name' does not match any '- alert: $name' rule in:$missing — the 99-spike silence would be a no-op for this alert and on-call pages during the planned burst"
      continue
    fi
    # Extract the alert's own rule block (from its `- alert: NAME` line
    # up to the next `- alert:`/`- record:` line) and pull `severity:`
    # from inside it. A fixed -A context window is NOT safe here: a
    # multi-line `expr: |` block pushes `labels:`/`severity:` past a
    # small window (e.g. p95_high's severity line is the 7th line after
    # `alert:`, not the 6th) — an under-sized window makes the lookup
    # grep match nothing, and under `set -eo pipefail` that silently
    # aborts the whole lint script instead of just this check.
    sev=$(awk -v name="$name" '
      $0 ~ ("alert: " name "$") { infile=1; next }
      infile && /^ *- (alert|record):/ { exit }
      infile && /severity:/ {
        line=$0; sub(/^.*severity:[ \t]*/, "", line); sub(/[ \t]*$/, "", line); print line; exit
      }
    ' configs/prometheus/rules.r1/*.yml 2>/dev/null || true)
    if [ "$sev" = "page" ]; then
      err "$AM_JS silences '$name', which is severity:page (SEV-1) in configs/prometheus/rules.r1 — a load-test silence must never mask a real page; remove it from the default matcher list (see the SCOPE comment at the top of $AM_JS)"
    fi
  done
fi

# ─── 18. r1's ZFS `data` pool topology — ONE source of truth ───────────────
#
# The raidz1-vs-raidz2 split (#289) is the canonical "corrected in one
# tree, never propagated" defect in this repo. r1's `data` pool is a
# single-parity raidz1 vdev — live-verified 2026-07-17 and corroborated
# by arithmetic (the ~16.8 TB footprint measured that day does not fit
# the ~13.85 TB two parity drives would leave on these four devices) —
# yet a year of docs, an ADR and the ansible per-region comment still
# described it as raidz2, i.e. promised an operator a second drive of
# margin that does not exist and sized every capacity plan off the wrong
# usable figure. The 2026-07-17 fix reached the rule trees and two
# runbooks and stopped there.
#
# AUTHORITY: `zfs_data_pool_type` in configs/ansible/inventory/r1.example.yml
# — the machine-readable value that would actually rebuild the pool, so
# doc drift and IaC drift are the same check. Not a literal in this
# script: that would just be a third source of truth.
#
# RULE: in the r1-scoped files below, a PARAGRAPH that names any raidz
# level must also name the live one. That permits deliberate contrast
# and dated history ("raidz2 at bringup; raidz1 since 2026-05-21") and
# rejects what actually drifts — a bare, unqualified assertion of the
# wrong level. Dated decision records (ADR-0016/0027, the superseded
# first-archival-node-deployment runbook, docs/audit/**) are NOT listed:
# they record what was decided/believed at a date and carry inline
# corrections instead.

echo "Checking r1 ZFS pool topology (single source of truth)..."
R1_INVENTORY="configs/ansible/inventory/r1.example.yml"
r1_pool_type=$(awk -F'"' '/^[[:space:]]*zfs_data_pool_type:[[:space:]]*"/{print $2; exit}' \
  "$R1_INVENTORY" 2>/dev/null || true)
if [ -z "$r1_pool_type" ]; then
  err "could not read zfs_data_pool_type from $R1_INVENTORY — that value is the authority for r1's pool topology; restore it or update scripts/ci/lint-docs.sh §18"
else
  # Files that describe r1's pool AS IT RUNS TODAY.
  r1_topology_files=(
    "$R1_INVENTORY"
    configs/prometheus/rules.r1/infra.yml
    deploy/monitoring/rules/infra.yml
    deploy/monitoring/rule-tests/infra_test.yml
    docs/architecture/ha-plan.md
    docs/architecture/storage-considerations.md
    docs/architecture/infrastructure/multi-region-topology.md
    docs/operations/r1-deployment-state.md
    docs/operations/r3-deployment-state.md
    docs/operations/self-hosting.md
    docs/operations/multi-region-cutover.md
    docs/operations/lcm-cache-tiering.md
    docs/operations/runbooks/zfs-degraded.md
    docs/operations/runbooks/zfs-pool-full.md
    docs/operations/runbooks/zfs-snapshots.md
    docs/operations/runbooks/nvme-smart.md
    docs/operations/runbooks/db-disk-full.md
    configs/ansible/inventory/r3.example.yml
  )
  # …and the subset that must SAY it, so the gate can't be satisfied by
  # gutting every mention (or by flipping the authority alone).
  r1_topology_must_state=(
    configs/prometheus/rules.r1/infra.yml
    deploy/monitoring/rules/infra.yml
    docs/operations/runbooks/zfs-degraded.md
    docs/operations/r1-deployment-state.md
    docs/architecture/storage-considerations.md
  )
  r1_files_checked=0
  for f in "${r1_topology_files[@]}"; do
    if [ ! -f "$f" ]; then
      err "r1 pool-topology check: '$f' is listed as r1-scoped but does not exist — a rename would silently drop it from the check; fix the path in scripts/ci/lint-docs.sh §18"
      continue
    fi
    r1_files_checked=$((r1_files_checked + 1))
    # Paragraph = run of non-blank lines. Report the paragraph's first line.
    bad=$(awk -v auth="$r1_pool_type" '
      function flush() {
        if (para != "" && para ~ /raidz[0-9]/ && index(para, auth) == 0)
          printf "%d\n", start
        para = ""; start = 0
      }
      /^[[:space:]]*$/ { flush(); next }
      { if (start == 0) start = FNR; para = para $0 "\n" }
      END { flush() }
    ' "$f")
    if [ -n "$bad" ]; then
      for ln in $bad; do
        err "$f:$ln — this paragraph names a raidz level other than r1's live '$r1_pool_type' (authority: $R1_INVENTORY) without naming '$r1_pool_type' too. a doc that names the wrong parity level promises an operator a margin the box may not have. Correct it, or keep the historical mention in the same paragraph as the live one."
      done
    fi
  done
  for f in "${r1_topology_must_state[@]}"; do
    if [ -f "$f" ] && ! grep -q "$r1_pool_type" "$f"; then
      err "$f no longer states r1's pool topology ('$r1_pool_type') anywhere — deleting the statement is how this fact went missing for a year (#289); it must be stated, not merely not-contradicted"
    fi
  done
  # The ansible role default is deliberately raidz2 (right shape for a
  # FRESH node) — so the defaults file is not paragraph-scanned. What IS
  # checked is its per-region comment, which is where the false "r1 =
  # raidz2" claim lived.
  ANSIBLE_DEFAULTS="configs/ansible/roles/archival-node/defaults/main.yml"
  r1_comment=$(grep -E '^#[[:space:]]+r1 = ' "$ANSIBLE_DEFAULTS" 2>/dev/null || true)
  if [ -z "$r1_comment" ]; then
    err "$ANSIBLE_DEFAULTS: the per-region '#   r1 = <topology>' comment is gone — it is the line that told operators r1 ran raidz2 for a year (#289); keep it and keep it true"
  elif ! echo "$r1_comment" | grep -q "$r1_pool_type"; then
    err "$ANSIBLE_DEFAULTS: per-region comment says '$r1_comment' but r1's live topology is '$r1_pool_type' ($R1_INVENTORY). The role DEFAULT may stay raidz2 for fresh nodes; the comment must not describe r1 wrongly"
  fi
  echo "  r1 pool topology: '$r1_pool_type' (authority $R1_INVENTORY) — checked $r1_files_checked of ${#r1_topology_files[@]} r1-scoped files"
fi

# ─── Summary ────────────────────────────────────────────────────────────────

count=$(cat "$ERROR_FILE")
rm "$ERROR_FILE"

if [ "$count" -gt 0 ]; then
  echo ""
  echo "❌ Doc lint failed with $count error(s)."
  exit 1
fi
echo "✅ Doc lint passed."
