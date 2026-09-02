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

# Resolved-parameter uniqueness. An operation that declares a parameter
# INLINE and also $refs the shared component for the same (name, in)
# pair ships a spec with two conflicting definitions of one parameter —
# /v1/assets carried an inline `limit` with no default alongside
# `$ref: Limit` which has default 100 (wave-D KP-5). Generators pick one
# arbitrarily, so the rendered docs, the Postman collection and the
# explorer's generated types can each disagree about the same field.
#
# Spectral DOES flag this (operation-parameters) — at severity WARN,
# and CI runs the action at its default --fail-severity=error, so it
# never failed the build. Enforcing it here makes it a hard gate without
# re-tuning Spectral's global severity floor, which would light up
# unrelated warnings.
echo "Checking OpenAPI parameter uniqueness..."
if [ -f openapi/stellar-index.v1.yaml ] && command -v python3 >/dev/null 2>&1; then
  dup_out=$(python3 - <<'PY' 2>&1 || true
import sys
try:
    import yaml
except ImportError:
    sys.exit(0)  # PyYAML absence is already fail-closed by lint-rule-structure
spec = yaml.safe_load(open("openapi/stellar-index.v1.yaml", encoding="utf-8")) or {}
comps = (spec.get("components") or {}).get("parameters") or {}
bad = []
for path, item in (spec.get("paths") or {}).items():
    if not isinstance(item, dict):
        continue
    for method, op in item.items():
        if not isinstance(op, dict) or "parameters" not in op:
            continue
        seen = {}
        for prm in op["parameters"] or []:
            if not isinstance(prm, dict):
                continue
            if "$ref" in prm:
                key = prm["$ref"].rsplit("/", 1)[-1]
                target = comps.get(key) or {}
                ident = (target.get("name"), target.get("in"))
            else:
                ident = (prm.get("name"), prm.get("in"))
            if ident == (None, None):
                continue
            if ident in seen:
                bad.append(f"{method.upper()} {path}: parameter {ident[0]!r} (in: {ident[1]}) declared twice")
            seen[ident] = True
for b in bad:
    print(b)
PY
)
  if [ -n "$dup_out" ]; then
    while IFS= read -r line; do
      [ -n "$line" ] && err "OpenAPI: $line — an inline parameter and a \$ref to the shared component define the same field twice; keep the \$ref and fold any unique prose into the operation description"
    done <<< "$dup_out"
  fi
fi

# Migration self-citation. A migration's own header comment should cite
# its OWN number. 0125_projection_dirty_windows.up.sql opened with
# "0124 up", and 0096_create_blend_emitter_events.up.sql with "0095 up"
# — both real but DIFFERENT migrations, so a reader following the
# reference lands on an unrelated change (wave-D CV-7).
#
# This check originally EXEMPTED those two on the grounds that an
# applied migration is immutable and even a comment-only edit changes
# its checksum, so the drift could only be recorded. That reasoning was
# wrong, and it contradicted the other precedent on main (febf720a
# edited nine shipped downs and refreshed the baseline). The rule is now
# written down once, in migrations/README.md "Amending a shipped
# migration":
#
#   A shipped migration's UP body is immutable; its DOWN body and its
#   header COMMENTS may be corrected through the baseline-refresh path
#   (lint-migration-immutability --write) with a CHANGELOG line;
#   anything stored in the database (COMMENT ON, defaults) needs a new
#   migration.
#
# A `--` line above BEGIN; is not executed, so correcting one cannot
# make an applied database diverge from a fresh one — and the checksum
# baseline still moves in the same diff, so the edit is visible rather
# than silent. Both headers were corrected under that rule (#357 F2/F3),
# and there is consequently NO exemption list here: every up.sql must
# cite its own number, with no grandfathered set to grow.
#
# Deliberately narrow: it checks the mechanical half — a file
# disagreeing with its own filename — not "does every `migration NNNN`
# mention in the tree point at the right subject", which needs judgement
# about what each migration is FOR. internal/storage/timescale/
# freeze_events.go cites 0124 and is CORRECT (0124 really is
# freeze_reason_other); a blanket find-and-replace would have broken it.
echo "Checking migration self-citation..."
for mig in migrations/[0-9][0-9][0-9][0-9]_*.up.sql; do
  [ -f "$mig" ] || continue
  base=$(basename "$mig")
  num=$(printf '%s' "$base" | cut -c1-4)
  first=$(head -1 "$mig")
  # Strip ADR-NNNN / CS-NNN / F-NNNN ids first: they are four digits but
  # are not migration numbers. The first draft flagged
  # 0048_source_coverage_snapshots.up.sql, whose header opens "ADR-0031:",
  # and a lint that flags correct files gets disabled.
  cited=$(printf '%s' "$first" | sed -E 's/(ADR|CS|F)-[0-9]+//g' \
          | grep -oE '\b[0-9]{4}\b' | head -1 || true)
  if [ -n "$cited" ] && [ "$cited" != "$num" ]; then
    err "$mig: header cites migration $cited but this file IS $num — a reader following that reference lands on a different migration. Fix the header (a header comment IS correctable on a shipped migration — see migrations/README.md 'Amending a shipped migration' — then run scripts/ci/lint-migration-immutability.sh --write in the same commit)."
  fi
done

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

# Iterate over 'current' docs — architecture/, operations/, adr/,
# contributing/ (added 2026-09-02, issue #362: the contributor
# checklists were never walked, so `add-onchain-source.md` could and did
# drift from the wiring it prescribes) and protocols/ + methodology/
# (added 2026-09-02, issue #359).
#
# Under docs/operations, docs/contributing, docs/protocols and
# docs/methodology a MISSING last_verified is now an error, not a skip.
# The old `continue` meant 23 operator procedures — including three of
# the five files the #461 dangerous-instruction fix had just rewritten —
# sat outside the freshness lint entirely: opting out was as easy as not
# writing the frontmatter, and nothing said so. The PUBLIC trees are the
# same trap with a worse blast radius: docs/protocols/README.md tells
# each protocol team "each page carries a last_verified date", yet 15 of
# 17 protocol pages and 2 of 5 methodology pages carried none, so
# widening the scan roots alone would have been a no-op — every one of
# them would simply have been skipped. Under docs/architecture and
# docs/adr it stays advisory (ADRs are immutable records; the ADR checks
# live in §8).
#
# RECORD subtrees are exempt by design: evidence/, postmortems/,
# incidents/, notes/ and wasm-audits/ are dated artefacts of a moment,
# not living procedure. So is any file whose NAME ends in a date
# (`*-YYYY-MM-DD.md`) — a campaign ledger or remediation record names the
# day it describes, and stamping it `last_verified` would either lie
# about re-verification or hard-fail at 180 days for being exactly what
# it is. The date must be in the FILENAME, so a living procedure cannot
# opt out by accident. A freshness stamp on a post-mortem would
# demand periodic re-verification of something that must never change,
# and would hard-fail at 180 days for being exactly what it is.
find docs/architecture docs/operations docs/adr docs/contributing \
     docs/protocols docs/methodology -type f -name '*.md' 2>/dev/null | while read -r f; do
  # Skip generated docs, archive, templates.
  if grep -q "GENERATED FILE - DO NOT EDIT" "$f" 2>/dev/null; then continue; fi
  if [[ "$f" == *"_archive"* ]] || [[ "$f" == *"_template"* ]]; then continue; fi

  # Extract last_verified date from frontmatter if present.
  verified=$(awk '/^last_verified:/{print $2; exit}' "$f" 2>/dev/null | tr -d '"')
  if [ -z "$verified" ]; then
    case "$f" in
      docs/operations/evidence/*|docs/operations/postmortems/*|\
      docs/operations/incidents/*|docs/operations/notes/*|\
      docs/operations/wasm-audits/*|\
      *-[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9].md)
        continue ;;
      docs/operations/*|docs/contributing/*|docs/protocols/*|docs/methodology/*)
        err "Doc '$f' has no last_verified frontmatter — every living operator/contributor procedure and every public protocol/methodology page carries one so §6 can age it out. Add 'last_verified: YYYY-MM-DD' with the date you actually checked its claims (record-only subtrees evidence/ postmortems/ incidents/ notes/ wasm-audits/ are exempt)."
        continue ;;
      *)
        continue ;;
    esac
  fi

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
#
# The grep below is name-presence only, so it never noticed that the
# catalogue's SEVERITY column disagreed with the rules it described (190
# of 203 rows, issue #362) — including 15 rows labelled `P3` whose rules
# are `informational`, which alertmanager.r1.yml routes to a receiver
# with no delivery at all. The YAML-aware
# scripts/ci/lint-alerts-catalog.py checks that column, and does the
# name parity in BOTH directions across BOTH rule trees.

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

echo "Checking alerts-catalog severity parity..."
if catalog_out=$(python3 scripts/ci/lint-alerts-catalog.py 2>&1); then
  # Echo the self-accounting line even on the green path. A gate that
  # prints NOTHING when it passes is indistinguishable from a gate that
  # never ran (2026-07-24); the "checked N of M" line is what makes the
  # difference visible in the CI log.
  printf '  %s\n' "$(printf '%s\n' "$catalog_out" | grep 'problem(s) found' || true)"
else
  while IFS= read -r line; do
    [ -z "$line" ] && continue
    case "$line" in
      *"problem(s) found"*) continue ;;  # summary line, not a distinct error
    esac
    err "alerts-catalog:$line"
  done <<< "$catalog_out"
fi

# ─── 11. Runbook body references to obs-owned metric namespaces ─────────────
#
# Narrow rule: only the `stellarindex_*` namespaces fully owned by
# internal/obs/metrics.go (see RUNBOOK_METRIC_PREFIXES). External-
# exporter metrics (stellarindex_stellar_core_*, pgbackrest_*, …) and
# namespaces the vendored SDK also writes into
# (stellarindex_ledgerstream_* — the BufferedStorageBackend registers
# its own buffer_* series under that prefix) are deliberately OUT of
# scope: nothing in this repo declares their names, so enforcing them
# would be all false positives.
#
# Caught `stellarindex_source_last_event_age_seconds` drift on
# 2026-04-23 — runbook referenced a metric name that never existed.
#
# Widened 2026-08-29 (runbook re-verification wave K, issue #315).
# ledgerstream-tier-both-missing.md — a P1 page — told responders to
# read `stellarindex_indexer_ledger_lag_seconds` and
# `stellarindex_backfill_cursor` for months. Neither has ever been
# registered, so the documented triage for a P1 was "grep for a series
# that cannot exist". The `_source_`-only rule could not see either.
# A prefix here is a PROMISE that internal/obs is the only writer of
# that TIME-SERIES namespace; add one only when that is true, never to
# launder a name that has no producer. Each alternative needs its
# trailing `_`, which is also what keeps the multi-host SCRAPE-JOB
# label `job="stellarindex_indexer"` (no trailing underscore) out of
# scope — lint-metric-refs.sh skips job matchers for the same reason.
#
# The promise is about METRICS, not about the `stellarindex_` word:
# ansible's inventory namespace overlaps it (today exactly one
# collision, `stellarindex_backfill_from_ledger` in
# configs/ansible/roles/archival-node/defaults/main.yml, already cited
# by docs/operations/{testnet-futurenet-reset-runbook,archival-node-
# bringup}.md — both outside runbooks/, so §11 is green today). A
# runbook that legitimately names such a var would otherwise red CI as
# a false positive, so declared ansible variables are subtracted from
# the token set below. That set is DERIVED from the repo, not an
# allowlist: a phantom metric name is not an ansible variable, so it
# cannot be used to launder one (`stellarindex_backfill_cursor` — the
# gauge that motivated this widening — is still caught).

RUNBOOK_METRIC_PREFIXES='stellarindex_(source|cursor|indexer|backfill|trade|postgres_ping)_'

echo "Checking runbook metric-name freshness..."
if [ -d docs/operations/runbooks ] && [ -f internal/obs/metrics.go ]; then
  # Build the allowed set: names registered in obs.metrics.go +
  # alert names in Prometheus rules, BOTH trees (runbooks use either).
  # `|| true` because under set -e + pipefail, grep returning 1 for
  # no-match would kill the whole script — we explicitly want an empty
  # set if no matches.
  allowed=$(mktemp)
  {
    (grep -hE "Name:[[:space:]]*\"${RUNBOOK_METRIC_PREFIXES}[a-z0-9_]+\"" internal/obs/metrics.go 2>/dev/null || true) | \
      sed -E 's|.*"(stellarindex_[a-z0-9_]+)".*|\1|'
    (grep -rhE "^[[:space:]]*-[[:space:]]*alert:[[:space:]]*${RUNBOOK_METRIC_PREFIXES}" deploy/monitoring/rules/ configs/prometheus/rules.r1/ 2>/dev/null || true) | \
      sed -E 's|.*alert:[[:space:]]*||'
  } | sort -u > "$allowed"

  # Ansible inventory variables that share the prefix namespace — not
  # metrics, and not phantoms either (see the note above). Declared as
  # `name:` at column 0 in a role's defaults/vars, which is how ansible
  # itself declares them.
  notmetrics=$(mktemp)
  (grep -rhoE "^${RUNBOOK_METRIC_PREFIXES}[a-z0-9_]+" configs/ansible/ 2>/dev/null || true) | \
    sort -u > "$notmetrics"

  # Extract every in-scope metric token from runbook bodies. Histogram
  # child series (_bucket/_sum/_count) resolve to their parent.
  (grep -rhoE "${RUNBOOK_METRIC_PREFIXES}[a-z0-9_]+" docs/operations/runbooks/ 2>/dev/null || true) | \
    sed -E 's/_(bucket|sum|count)$//' | \
    sort -u | while IFS= read -r metric; do
      [ -z "$metric" ] && continue
      if grep -qxF "$metric" "$notmetrics"; then continue; fi
      if ! grep -qxF "$metric" "$allowed"; then
        err "runbook references unknown metric '$metric' (not in internal/obs or rules/)"
      fi
    done
  rm -f "$allowed" "$notmetrics"
fi

# ─── 11b. A runbook's heavy-job command must actually write ────────────────
#
# `/usr/local/sbin/run-heavy-job.sh` is r1's COMMIT wrapper: it takes
# the per-job singleton flock, the MemoryMax=20G scope and the
# ClickHouse ops_batch identity because the payload is about to do real
# work. Nothing about a preview needs any of that — so a write-gated
# stellarindex-ops subcommand invoked under the wrapper without
# `-write` is, without exception, a bug in the runbook.
#
# This is the class this file exists to stop being repeatable. The
# shared gate (internal/ops/opsutil/opsutil.go) makes DRY RUN the
# default: `DryRun() { return !*write }`. rehydrate-galexie-archive
# then buckets every not-in-hot path as `copied` in dry-run mode
# WITHOUT asking cold whether it holds the object, so a commit step
# missing `-write` logs `"rehydrate complete" … copied=N
# missing_in_cold=0 errors=0` and exits 0 — a success-shaped report
# having rehydrated nothing, handed to a responder mid-P1. The same
# omission on projector-replay rewinds no projector. Both shipped in
# the first cut of the wave-K runbook fixes (issue #315) and were
# caught by review, not by CI.
#
# The gated set is DERIVED from the source (files that register the
# shared gate or their own `-write` bool, keyed by the flagset name,
# which is the subcommand name) so a new gated subcommand is covered
# the day it lands. Scope is docs/operations/runbooks/ — the surface an
# operator copy-pastes from under pressure. Wrapper invocations
# elsewhere in docs/ are out of scope here; see §11's note on why this
# file stays narrow.

echo "Checking runbook heavy-job commands pass -write..."
if [ -d docs/operations/runbooks ] && [ -d internal/ops ]; then
  gated=$(mktemp)
  # `|| true` on both greps: no-match is exit 1, which set -e + pipefail
  # would turn into a script abort rather than an empty set.
  for gf in $(grep -rlE 'opsutil\.RegisterWriteGate\(|Bool\("write"' \
                --include='*.go' internal/ops cmd/stellarindex-ops 2>/dev/null || true); do
    case "$gf" in *_test.go) continue ;; esac
    # `[a-z0-9 -]+`, WITH a space. Five write-gated subcommands declare
    # two-word flagsets — "supply snapshot", "supply seed-observations",
    # "supply seed-sac-balances", "supply seed-claimable-balances",
    # "supply seed-sep41-genesis" — and the old `[a-z0-9-]+` could not
    # match a space, so every one of them was silently absent from the
    # gated set. A runbook telling a responder to run
    # `run-heavy-job.sh … stellarindex-ops supply snapshot -asset native`
    # without -write produced NO finding, while the identical omission on
    # a one-word subcommand was caught. That is exactly the failure this
    # check exists to stop: the command reports "complete … errors=0",
    # is success-shaped, and has written nothing (review sweep
    # 2026-08-31).
    (grep -ohE 'flag\.NewFlagSet\("[a-z0-9 -]+"' "$gf" 2>/dev/null || true) | \
      sed -E 's|.*"(.*)"|\1|'
  done | sort -u > "$gated"

  for rb in docs/operations/runbooks/*.md; do
    [ -f "$rb" ] || continue
    # Join backslash-continued shell lines so a multi-line invocation is
    # matched as one command — the wrapper form is always continued.
    # awk, not the sed `:a;/\\$/N;ta` idiom: the awk form behaves
    # identically under BSD awk (dev laptops) and gawk (CI runners).
    joined=$(awk '{ while (sub(/\\$/, "")) { if ((getline nxt) > 0) { $0 = $0 " " nxt } else { break } } print }' "$rb")
    while IFS= read -r sub; do
      [ -z "$sub" ] && continue
      # `|| true`: a no-match grep exits 1, which under set -e +
      # pipefail would abort the whole lint instead of meaning "clean".
      #
      # An EXPLICIT -dry-run is exempt. The forgotten-flag failure this
      # section exists to catch is a command that looks like it writes
      # and does not; a command that says -dry-run is a deliberate
      # preview, and several runbooks correctly show the dry run
      # immediately before the -write run (see
      # supply-cross-check-divergence.md §Mitigation). Flagging those
      # would train responders to ignore this check.
      offenders=$(printf '%s\n' "$joined" | \
        grep -E "run-heavy-job\.sh.*stellarindex-ops[[:space:]]+${sub}([[:space:]]|\$)" | \
        grep -vE '(^|[[:space:]])-{1,2}write([[:space:]]|$)' | \
        grep -vE '(^|[[:space:]])-{1,2}dry-run([[:space:]]|$)' || true)
      [ -z "$offenders" ] && continue
      printf '%s\n' "$offenders" | while IFS= read -r bad; do
        [ -z "$bad" ] && continue
        err "$rb: heavy-job command runs write-gated '$sub' without -write — it is a DRY RUN and will report success having written nothing: ${bad}"
      done
    done < "$gated"
  done
  rm -f "$gated"
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

# ─── 19. Agent-orientation docs: the claims a machine can re-derive ────────
#
# CLAUDE.md/AGENTS.md are the first thing an agent reads, so a false
# claim there is a defect with a blast radius of every subsequent
# session. Two of them are mechanically checkable, and both had
# actually drifted when this section was written (issue #326, the
# 2026-08-29 re-sweep):
#
#   (a) AGENTS.md duplicates CLAUDE.md's make-target block. #259 fixed
#       `make dev` ("the full stack" — dev.yaml has only Timescale/
#       Redis/MinIO) and `make docs-all` ("+ obs/*.go metric Name:
#       fields" — docs-metrics is an explicit no-op) in CLAUDE.md and
#       left AGENTS.md asserting both, six months after c3b2c382 had
#       aligned the same two files by hand. Duplicated prose drifts;
#       principle 1 above says pick one source of truth. AGENTS.md's
#       block must therefore be a VERBATIM subset of CLAUDE.md's —
#       shorten by dropping a line, never by rewording one.
#
#   (b) The status page moved into the explorer (web/explorer/src/app/
#       status/, stellarindex.io/status) and web/status/ became a
#       redirect-only Cloudflare Pages stub — but CLAUDE.md's repo map
#       still located the shipped page at web/status/, sending every
#       agent that reads it to the stub. Scope is deliberately the
#       three orientation docs a reader consults to learn WHERE things
#       live: the operational docs (cf-pages-setup, status-page-setup,
#       rollback, sev-playbook, the CF Pages bootstrap runbooks) name
#       web/status/ legitimately, because the Pages project that serves
#       the 301 is still real and still deployed. The check is
#       per-DOCUMENT, not per-line, so prose can be worded freely, and
#       it disarms itself if the page ever moves back.

echo "Checking agent-orientation doc claims..."

# (a) AGENTS.md quick-start ⊆ CLAUDE.md build+test commands, verbatim.
#
# extract_fenced_block <file> <heading> — the first ``` fence that
# follows <heading>. Empty output means the heading or the fence moved,
# which is a lint-drift error in itself (fail closed, never skip).
extract_fenced_block() {
  awk -v heading="$2" '
    $0 == heading { seen = 1; next }
    seen && /^```/ { fence = !fence; if (!fence) exit; next }
    seen && fence { print }
  ' "$1"
}

if [ -f CLAUDE.md ] && [ -f AGENTS.md ]; then
  claude_cmds=$(extract_fenced_block CLAUDE.md "## Build + test commands")
  agents_cmds=$(extract_fenced_block AGENTS.md "## Quick-start commands")
  if [ -z "$claude_cmds" ]; then
    err "could not extract the command block under '## Build + test commands' in CLAUDE.md — the heading or its \`\`\` fence moved; update lint-docs.sh §18a"
  elif [ -z "$agents_cmds" ]; then
    err "could not extract the command block under '## Quick-start commands' in AGENTS.md — the heading or its \`\`\` fence moved; update lint-docs.sh §18a"
  else
    while IFS= read -r line; do
      [ -z "${line// /}" ] && continue
      if ! printf '%s\n' "$claude_cmds" | grep -qxF "$line"; then
        err "AGENTS.md quick-start line is not verbatim in CLAUDE.md's '## Build + test commands' block: '$line' — the two drift apart every time one is edited alone (#326); copy CLAUDE.md's line exactly, or drop the line from AGENTS.md"
      fi
    done <<< "$agents_cmds"
  fi
else
  err "CLAUDE.md and/or AGENTS.md missing — the agent-orientation parity check (§18a) cannot run"
fi

# (b) While web/status/ is a redirect stub, an orientation doc that
# names it must also say where the status page actually lives.
STATUS_REDIRECTS="web/status/public/_redirects"
if [ ! -f "$STATUS_REDIRECTS" ]; then
  echo "  (skipped §18b — $STATUS_REDIRECTS is gone; web/status/ has been retired, so there is no stub to misdescribe)"
elif grep -q "https://stellarindex.io/status" "$STATUS_REDIRECTS"; then
  status_docs=$(grep -l "web/status" README.md CLAUDE.md AGENTS.md 2>/dev/null || true)
  for doc in $status_docs; do
    if ! grep -q "web/explorer/src/app/status" "$doc"; then
      err "$doc names 'web/status' but never says where the status page actually lives — since the move it ships from web/explorer/src/app/status/ (stellarindex.io/status) and web/status/ is a redirect-only stub ($STATUS_REDIRECTS 301s to it; see web/status/README.md). Name the explorer path so an agent reading this file isn't sent to the stub."
    fi
  done
else
  echo "  (skipped §18b — $STATUS_REDIRECTS no longer 301s to stellarindex.io/status; the page appears to have moved back)"
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
