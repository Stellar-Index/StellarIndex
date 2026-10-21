#!/usr/bin/env bash
# Doc-code consistency linter: fails the build when docs drift from code.
# Enforces one source of truth and code<->reference round-trips
# (docs/engineering-standards.md section 5). Run: bash scripts/ci/lint-docs.sh

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
# Checks below are gated on `[ -f X ]`, so a renamed input would silently skip its check
# and still print "passed". Optional inputs (runbooks, incidents, overlays) are not listed.

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
# The reference table uses TOML names, not Go field names: that is what operators see.

echo "Checking config reference sync..."
if [ -f internal/config/config.go ] && [ -f docs/reference/config/README.md ]; then
  # Digits included: tags like s3_*, sep10, sep41 and phase2 must be checked too.
  grep -oE 'toml:"[a-z0-9_]+"' internal/config/config.go | \
    sed -E 's/toml:"([a-z0-9_]+)"/\1/' | sort -u | while read -r tomlname; do
      if ! grep -qF "$tomlname" docs/reference/config/README.md; then
        err "Config TOML key '$tomlname' in config.go missing from docs/reference/config/README.md — run 'make docs-config' to regen"
      fi
  done
fi

# ─── 2. Every API route handler must be in OpenAPI ──────────────────────────
#
# The spec lists routes without the /v1 prefix (the server base URL), so strip it first.

echo "Checking API routes vs OpenAPI..."
if [ -d internal/api/v1 ] && [ -f openapi/stellar-index.v1.yaml ]; then
  # Match both HandleFunc("VERB /v1...") and mux.Handle(...): the latter registers
  # middleware-wrapped routes. internal_routes_re allow-lists routes kept out of the spec,
  # each needing a reason.
  internal_routes_re='^$'
  # handlePublic( and public.Handle(mux, mount credential-optional routes, handleAdmin( the operator
  # tier; tests register fixtures.
  grep -rhoE --exclude='*_test.go' '(Handle(Func)?\(|handlePublic\(|handleAdmin\(|\.Handle\(mux, )"[A-Z]+ /v1[^"]*"' internal/api/v1/ 2>/dev/null | \
    sed -E 's|.*"[A-Z]+ /v1||; s|"$||' | \
    sed -E 's|^$|/|' | \
    sort -u | while IFS= read -r route; do
      [ -z "$route" ] && continue
      if [[ "$route" =~ $internal_routes_re ]]; then continue; fi
      if ! grep -qE "^  ${route}:" openapi/stellar-index.v1.yaml; then
        err "Route '$route' is registered in handlers but missing from OpenAPI spec"
      fi
  done

  # Reverse: spec entries with no handler. planned_regex allow-lists documented-but-unshipped
  # routes; remove an entry once its handler lands.
  planned_regex='^$'
  grep -oE "^  /[^:]+:" openapi/stellar-index.v1.yaml | \
    sed -E 's|^  ||; s|:$||' | sort -u | while IFS= read -r route; do
      [ -z "$route" ] && continue
      if [[ "$route" =~ $planned_regex ]]; then
        continue
      fi
      # Fixed-string search so path params like /assets/{asset_id} are not read as regex.
      found=0
      for method in GET POST PUT PATCH DELETE; do
        if grep -qrF --exclude='*_test.go' \
          -e "HandleFunc(\"${method} /v1${route}\"" -e "Handle(\"${method} /v1${route}\"" \
          -e "handlePublic(\"${method} /v1${route}\"" -e ".Handle(mux, \"${method} /v1${route}\"" \
          -e "handleAdmin(\"${method} /v1${route}\"" \
          internal/api/v1/ 2>/dev/null; then
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
# CI's openapi job is PR-only and path-filtered, so a direct push to main could desync the
# copy made by `make docs-api`; this runs in verify.sh on every push.

echo "Checking generated API reference sync..."
if [ -f openapi/stellar-index.v1.yaml ] && [ -f docs/reference/api/stellar-index.v1.yaml ]; then
  if ! diff -q openapi/stellar-index.v1.yaml docs/reference/api/stellar-index.v1.yaml >/dev/null 2>&1; then
    err "docs/reference/api/stellar-index.v1.yaml is out of sync with openapi/stellar-index.v1.yaml — run 'make docs-api' and commit the result"
  fi
fi

# Resolved-parameter uniqueness: an operation must not declare a parameter inline and also
# $ref the shared component for the same (name, in); generators pick one arbitrarily.
# Spectral only warns on this and CI fails on error, so it is enforced here.
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

# An unquoted comma in a flow-mapping (`{ ... }`) `description:` splits it:
# the description truncates and the remainder becomes a null-valued key.
# Quote any flow-mapping description that contains a comma.
echo "Checking OpenAPI for comma-split flow-mapping descriptions..."
if [ -f openapi/stellar-index.v1.yaml ] && command -v python3 >/dev/null 2>&1; then
  null_out=$(python3 - <<'PY' 2>&1 || true
import sys
try:
    import yaml
except ImportError:
    sys.exit(0)  # PyYAML absence is already fail-closed by lint-rule-structure
spec = yaml.safe_load(open("openapi/stellar-index.v1.yaml", encoding="utf-8")) or {}

def walk(node, path=""):
    if isinstance(node, dict):
        for k, v in node.items():
            if v is None and "/example/" not in path:
                print(f"{path}: bogus null-valued key {k!r} — likely an unquoted comma inside a nearby flow-mapping description")
            walk(v, f"{path}/{k}")
    elif isinstance(node, list):
        for i, item in enumerate(node):
            walk(item, f"{path}[{i}]")

walk(spec)
PY
)
  if [ -n "$null_out" ]; then
    while IFS= read -r line; do
      [ -n "$line" ] && err "OpenAPI: $line"
    done <<< "$null_out"
  fi
fi

# Migration self-citation: an up.sql header must cite its OWN number, so a reader
# following the reference does not land on a different migration. Header comments may be
# corrected on shipped migrations (migrations/README.md "Amending a shipped migration"); no exemption list.
echo "Checking migration self-citation..."
for mig in migrations/[0-9][0-9][0-9][0-9]_*.up.sql; do
  [ -f "$mig" ] || continue
  base=$(basename "$mig")
  num=$(printf '%s' "$base" | cut -c1-4)
  first=$(head -1 "$mig")
  # Strip ADR-NNNN / CS-NNN / F-NNNN ids first: they are four digits but not migration numbers.
  cited=$(printf '%s' "$first" | sed -E 's/(ADR|CS|F)-[0-9]+//g' \
          | grep -oE '\b[0-9]{4}\b' || true)
  cited="${cited%%$'\n'*}"   # first token; slicing a captured value cannot
                             # close the pipe on the greps that filled it
  if [ -n "$cited" ] && [ "$cited" != "$num" ]; then
    err "$mig: header cites migration $cited but this file IS $num — a reader following that reference lands on a different migration. Fix the header (a header comment IS correctable on a shipped migration — see migrations/README.md 'Amending a shipped migration' — then run scripts/ci/lint-migration-immutability.sh --write in the same commit)."
  fi
done

# ─── 3. Every Prometheus metric must be documented in metrics reference ─────

echo "Checking metrics registry..."
if [ -d internal/obs ] && [ -f docs/reference/metrics/README.md ]; then
  # Scope is every prometheus `Name: "..."` field, including unprefixed ones such as
  # http_requests_total. BSD sed lacks \s, so use [[:space:]].
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
  # Citations whose PR/issue number now resolves to an unrelated item.
  # (–|-) alternation because the en-dash is 3 UTF-8 bytes and CI greps in C locale.
  "\(#1004\)"
  "#1001"
  "#1002"
  "#1083\b"
  "#1108\b"
  "PR #1042"
  "#1066(–|-)#1073"
  "#1132"
  "#1272\b"
  "#1271\b"
  "#1263\b"
  "#1270\b"
  "#1268\b"
  "Deferred #1347\b"
  "#1353"
  "#1369"
  "dependabot #1371/#1372"
  "R-013.*#1265"
  "R-021.*#1264"
  "per #948"
  "#39\b"
  "#845\b"
  "#854\b"
  "#879\b"
  "\(#888\)\|shipped in #888"
  "#970\b"
  "#971\b"
  "#973\b"
  "\(#975\)"
  "#1196\b"
  "PR #1198"
  "PR #1231"
  "#201\b"
  "PR #1216"
  "PR #1223"
  "PR #?168\b"
  "PR #1227"
  "PR #1232"
  "PR #1233"
  "shipped in #1251"
  "\(PR #1254\)"
  "PR #1255"
  "PR #1230"
  "#1225\b"
  "#1226\b"
  "#1217\b"
  "#1218\b"
  "#1219\b"
  "#1224\b"
  "#1368\b"
  "\(#1370\)"
  "#1262\b"
  "PR #1041"
  "\(added in #314\)"
  "PRs #263(–|-)#270"
)
for pattern in "${stale_patterns[@]}"; do
  matches=$(grep -rnE "$pattern" \
    README.md \
    AGENTS.md \
    AGENTS.md \
    CONTRIBUTING.md \
    SECURITY.md \
    CODE_OF_CONDUCT.md \
    CHANGELOG.md \
    docs/reference/ \
    docs/audit/recipe.md \
    docs/architecture/ \
    docs/design/ \
    docs/operations/ \
    docs/development/ \
    2>/dev/null | grep -v "node_modules\|_archive/\|discovery/" || true)
  if [ -n "$matches" ]; then
    err "Stale reference to '$pattern' in active docs:"
    # shellcheck disable=SC2001  # per-LINE indent of a multi-line
    # variable; ${var//…} has no line anchor, so the suggested
    # parameter expansion cannot express this.
    echo "$matches" | sed 's/^/    /' >&2
  fi
done

# ─── 5. No forbidden tech-debt markers without issue links ──────────────────

echo "Checking TODO discipline..."
# Every TODO/FIXME/XXX in Go code must be of the form TODO(#N):
if [ -d internal ] || [ -d cmd ]; then
  # Match every TODO/FIXME/XXX, then exempt only `(#123)`, so `TODO(later):` and a bare `// TODO` fail.
  bad_todos=$(grep -rnE '//[[:space:]]*(TODO|FIXME|XXX)' \
    internal/ cmd/ pkg/ 2>/dev/null | \
    grep -vE '//\s*(TODO|FIXME|XXX)\(#[0-9]+\)' || true)
  if [ -n "$bad_todos" ]; then
    err "TODO/FIXME/XXX without linked issue number (must be 'TODO(#123): …'):"
    # shellcheck disable=SC2001  # per-LINE indent; see the note above.
    echo "$bad_todos" | sed 's/^/    /' >&2
  fi
fi

# ─── 6. Frontmatter freshness on 'current' docs ─────────────────────────────

echo "Checking doc frontmatter freshness..."
# ISO dates compare as strings, so age each stamp against two precomputed cutoffs instead of forking `date`.
today=$(date -u +%s)
stale_cut=$(date -u -r $((today - 90 * 86400)) +%F 2>/dev/null || date -u -d "@$((today - 90 * 86400))" +%F)
fail_cut=$(date -u -r $((today - 180 * 86400)) +%F 2>/dev/null || date -u -d "@$((today - 180 * 86400))" +%F)

# 'current' docs only. Outside architecture/ and design/ a MISSING last_verified is an error.
# Exempt: docs/adr (gated by section 8), RECORD subtrees (evidence, postmortems, incidents, notes,
# wasm-audits) and files named `*-YYYY-MM-DD.md`; the date must be in the filename so a living doc cannot opt out.
find docs/architecture docs/operations docs/design docs/contributing \
     docs/protocols docs/methodology -type f -name '*.md' -print0 2>/dev/null | \
  xargs -0 awk 'BEGIN {
    for (i = 1; i < ARGC; i++) {
      f = ARGV[i]; gen = 0; v = ""
      while ((getline line < f) > 0) {
        if (index(line, "GENERATED FILE - DO NOT EDIT")) gen = 1
        if (v == "" && line ~ /^last_verified:/) { split(line, a, " "); v = a[2]; gsub(/"/, "", v) }
      }
      close(f)
      print f "\t" gen "\t" v
    }
  }' | while IFS=$'\t' read -r f gen verified; do
  if [ "$gen" = 1 ]; then continue; fi
  if [[ "$f" == *"_archive"* ]] || [[ "$f" == *"_template"* ]]; then continue; fi

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

  [[ "$verified" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || continue

  if [[ "$verified" < "$fail_cut" ]]; then
    err "Doc '$f' is STALE — last_verified $verified is > 180 days old"
  elif [[ "$verified" < "$stale_cut" ]]; then
    echo "  WARN: doc '$f' last_verified $verified is > 90 days old — refresh soon" >&2
  fi
done

# ─── 7. Generated-file banner intact ────────────────────────────────────────
#
# Only the generated subdirs under docs/reference/ are machine-produced; top-level *.md there is hand-written.

echo "Checking generated-file banners..."
# docs/reference/metrics/README.md is hand-written (no generator; drift is checked by section 3): exempt by exact path.
# Enumerate only existing subdirs: `find` on a missing one would kill the script under set -e + pipefail.
gen_dirs=()
for d in docs/reference/api docs/reference/config docs/reference/metrics; do
  [ -d "$d" ] && gen_dirs+=("$d")
done
if [ ${#gen_dirs[@]} -gt 0 ]; then
  find "${gen_dirs[@]}" -type f -name '*.md' 2>/dev/null | while read -r f; do
    if [ "$f" = "docs/reference/metrics/README.md" ]; then
      continue
    fi
    # sigpipe-ok: `head -1` writes one short banner line, far under a pipe buffer.
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

# Every ADR file needs a row in docs/adr/README.md's index; the loop above only checks its own frontmatter.
if [ -d docs/adr ] && [ -f docs/adr/README.md ]; then
  index_section=$(awk '/^## Index/{flag=1} flag' docs/adr/README.md)
  for adr in docs/adr/[0-9]*.md; do
    [ -f "$adr" ] || continue
    num=$(basename "$adr" | grep -oE '^[0-9]+')
    # A row is linked ("| [0051](...)") or plain ("| 0036 | ..."); match the table cell, not any mention.
    if ! grep -qE "^\| (\[$num\]|$num \|)" <<<"$index_section"; then
      err "ADR '$adr' (ADR-$num) has no row in docs/adr/README.md's Index table"
    fi
  done
fi

# ─── 9. Every alert rule's runbook_url must live in ANNOTATIONS + exist ─────
#
# runbook_url must sit under `annotations:` (templates read .Annotations; in `labels:` it renders nothing)
# and a local target must exist. lint-runbook-annotations.py parses each rule and asserts both.

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
# Name presence only; scripts/ci/lint-alerts-catalog.py checks severity and parity both ways.

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
  # Echo "checked N of M" even when green: a silent pass looks like a gate that never ran.
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
# Only `stellarindex_*` prefixes that internal/obs alone writes (RUNBOOK_METRIC_PREFIXES). Exporter and
# SDK-shared namespaces are out of scope; add a prefix only when obs is the sole writer, each with its
# trailing `_` so scrape-job labels like job="stellarindex_indexer" stay out. Declared ansible vars that share
# the prefix are subtracted (derived from the repo, not an allowlist), so a phantom metric is still caught.

RUNBOOK_METRIC_PREFIXES='stellarindex_(source|cursor|indexer|backfill|trade|postgres_ping)_'

echo "Checking runbook metric-name freshness..."
if [ -d docs/operations/runbooks ] && [ -f internal/obs/metrics.go ]; then
  # Allowed set: names registered in obs/metrics.go plus alert names in both rule trees.
  # `|| true` keeps a no-match grep from aborting under set -e + pipefail.
  allowed=$(mktemp)
  {
    (grep -hE "Name:[[:space:]]*\"${RUNBOOK_METRIC_PREFIXES}[a-z0-9_]+\"" internal/obs/metrics.go 2>/dev/null || true) | \
      sed -E 's|.*"(stellarindex_[a-z0-9_]+)".*|\1|'
    (grep -rhE "^[[:space:]]*-[[:space:]]*alert:[[:space:]]*${RUNBOOK_METRIC_PREFIXES}" deploy/monitoring/rules/ configs/prometheus/rules.r1/ 2>/dev/null || true) | \
      sed -E 's|.*alert:[[:space:]]*||'
  } | sort -u > "$allowed"

  # Ansible inventory variables sharing the prefix: not metrics, not phantoms.
  # Declared as `name:` at column 0 in a role's defaults/vars.
  notmetrics=$(mktemp)
  (grep -rhoE "^${RUNBOOK_METRIC_PREFIXES}[a-z0-9_]+" configs/ansible/ 2>/dev/null || true) | \
    sort -u > "$notmetrics"

  # Histogram child series (_bucket/_sum/_count) resolve to their parent.
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
# run-heavy-job.sh is the COMMIT wrapper. Write-gated stellarindex-ops subcommands default to DRY RUN, so one
# run under it without `-write` reports success (e.g. rehydrate "complete ... errors=0") having written nothing.
# The gated set is derived from source so new subcommands are covered; scope is docs/operations/runbooks/.

echo "Checking runbook heavy-job commands pass -write..."
if [ -d docs/operations/runbooks ] && [ -d internal/ops ]; then
  gated=$(mktemp)
  # `|| true` on both greps: no-match (exit 1) must mean an empty set, not an abort.
  for gf in $(grep -rlE 'opsutil\.RegisterWriteGate\(|Bool\("write"' \
                --include='*.go' internal/ops cmd/stellarindex-ops 2>/dev/null || true); do
    case "$gf" in *_test.go) continue ;; esac
    # `[a-z0-9 -]+` WITH a space: two-word flagsets ("supply snapshot", "supply seed-...") would otherwise
    # be missing from the gated set and their missing `-write` go unflagged.
    (grep -ohE 'flag\.NewFlagSet\("[a-z0-9 -]+"' "$gf" 2>/dev/null || true) | \
      sed -E 's|.*"(.*)"|\1|'
  done | sort -u > "$gated"

  # Join backslash-continued lines so a multi-line wrapper invocation matches as one command, in ONE pass
  # over all runbooks (a per-runbook loop was ~5k pipelines). awk, not sed `:a;/\\$/N;ta`: BSD awk and gawk agree.
  wrapped=$(mktemp)
  for rb in docs/operations/runbooks/*.md; do
    if [ -f "$rb" ]; then printf '%s\0' "$rb"; fi
  done | xargs -0 awk 'BEGIN {
    for (i = 1; i < ARGC; i++) {
      f = ARGV[i]
      while ((getline line < f) > 0) {
        while (sub(/\\$/, "", line)) { if ((getline nxt < f) > 0) line = line " " nxt; else break }
        if (index(line, "run-heavy-job.sh")) print f "\t" line
      }
      close(f)
    }
  }' > "$wrapped"
  while IFS= read -r sub; do
    [ -z "$sub" ] && continue
    # `|| true`: no-match exits 1, which would abort under set -e + pipefail.
    # An explicit -dry-run is exempt: it is a deliberate preview (often shown just before the -write run),
    # and flagging it would train responders to ignore this check.
    offenders=$(grep -E "run-heavy-job\.sh.*stellarindex-ops[[:space:]]+${sub}([[:space:]]|\$)" "$wrapped" | \
      grep -vE '(^|[[:space:]])-{1,2}write([[:space:]]|$)' | \
      grep -vE '(^|[[:space:]])-{1,2}dry-run([[:space:]]|$)' || true)
    [ -z "$offenders" ] && continue
    printf '%s\n' "$offenders" | while IFS=$'\t' read -r rb bad; do
      [ -z "$bad" ] && continue
      err "$rb: heavy-job command runs write-gated '$sub' without -write — it is a DRY RUN and will report success having written nothing: ${bad}"
    done
  done < "$gated"
  rm -f "$wrapped"
  rm -f "$gated"
fi

# ─── 12. Every runbook referenced from alerts-catalog must exist ────────────
#
# Counterpart to section 9: a stale `runbooks/X.md` link in the catalog sends oncall to a 404.

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
# Orphans are allow-listed only for standalone ones (template, bring-up, dead-man's switch); all others must appear in
# alerts-catalog.md, sev-playbook.md or another runbook.

# ─── Prometheus rules: assert each multi-host rule has an R1 sibling ──
#
# Multi-host rules use underscored job names, the R1 overlay (configs/prometheus/rules.r1/) hyphenated ones;
# a multi-host file with no R1 sibling means silent drift.

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
      _template.md|README.md|bootstrap-archival-node.md|off-site-backup-plan.md) continue ;;
    esac
    if ! grep -qrF "runbooks/$fname" docs/operations/ 2>/dev/null; then
      err "orphan runbook with no referrer: $r — link from alerts-catalog, sev-playbook, or another runbook"
    fi
  done
fi

# ─── 14. Alert-runbook section presence ────────────────────────────────────
#
# Every alert runbook needs `## At a glance` (severity/MTTR/impact for a 3 AM page) and `## Related` (cross-links).
# Procedural runbooks (bring-up, DR, SEV comms, operator notes) are excluded; the list mirrors the orphan exclusions.

echo "Checking alert-runbook section presence..."
if [ -d docs/operations/runbooks ]; then
  for r in docs/operations/runbooks/*.md; do
    fname="${r##*/}"
    case "$fname" in
      _template.md|README.md|bootstrap-archival-node.md|off-site-backup-plan.md) continue ;;
      dr-activation.md|sev-status-page-update.md) continue ;;
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
# Fail if an incident (internal/incidents/data/*.md) is older than 30 days with unchecked `[ ]` boxes; `[~]` counts as
# accepted. The date comes from the filename slug `<YYYY-MM-DD>-<slug>.md`, the only thing read.

echo "Checking incident post-mortem follow-ups..."
INCIDENT_DIR="internal/incidents/data"
if [ -d "$INCIDENT_DIR" ]; then
  NOW_EPOCH=$(date -u +%s)
  THIRTY_DAYS_AGO=$((NOW_EPOCH - 30 * 86400))
  for incident in "$INCIDENT_DIR"/*.md; do
    [ -f "$incident" ] || continue
    fname="${incident##*/}"
    case "$fname" in
      _*) continue ;;
    esac
    date_str=$(echo "$fname" | grep -oE '^[0-9]{4}-[0-9]{2}-[0-9]{2}' || true)
    [ -z "$date_str" ] && continue
    incident_epoch=$(date -u -j -f "%Y-%m-%d" "$date_str" +%s 2>/dev/null || \
                     date -u -d "$date_str" +%s 2>/dev/null || echo "")
    [ -z "$incident_epoch" ] && continue
    if [ "$incident_epoch" -ge "$THIRTY_DAYS_AGO" ]; then
      continue
    fi
    # `[x]` / `[X]` / `[~]` count as done or acknowledged.
    unchecked=$(grep -cE '^[[:space:]]*-[[:space:]]+\[ \]' "$incident" || true)
    if [ "$unchecked" -gt 0 ]; then
      err "incident '$incident' is older than 30 days and has $unchecked unchecked '[ ]' follow-up checkbox(es) — close them, mark as acknowledged with '[~]', or rewrite the action item."
    fi
  done
fi

# ─── 16. Production CSP must not permit http://localhost ──────────────────
#
# A dev-convenience localhost origin in the Pages CSP `connect-src` leaked to production; the dev server ignores _headers.

hf=web/explorer/public/_headers
if [ -f "$hf" ] && grep -qE 'Content-Security-Policy:.*localhost' "$hf"; then
  err "$hf permits 'localhost' in a Content-Security-Policy header — forbidden in production builds. Remove the localhost permit; dev work uses 'next dev' which doesn't read _headers."
fi

# ─── 16b. The explorer runbook must describe the _headers that ship ────────
#
# The runbook's CSP and connect-src origins must match the _headers that ship.

EXPLORER_HEADERS="web/explorer/public/_headers"
EXPLORER_RUNBOOK="docs/operations/explorer-deployment.md"
if [ -f "$EXPLORER_HEADERS" ] && [ -f "$EXPLORER_RUNBOOK" ]; then
  root_csp=$(awk '/^\/\*$/ {in_root=1; next} /^[^ #]/ {in_root=0} in_root && /Content-Security-Policy:/ {print; exit}' "$EXPLORER_HEADERS")
  [ -n "$root_csp" ] || err "$EXPLORER_HEADERS has no Content-Security-Policy in its /* rule — the runbook check below would pass vacuously."
  for origin in $(printf '%s\n' "$root_csp" | sed -n 's/.*connect-src \([^;]*\);.*/\1/p' | tr ' ' '\n' | grep '^https://'); do
    grep -qF "$origin" "$EXPLORER_RUNBOOK" \
      || err "$EXPLORER_RUNBOOK does not name connect-src origin $origin that $EXPLORER_HEADERS's /* CSP ships."
  done
  if [[ "$root_csp" != *frame-ancestors* ]] \
    && grep -qF "frame-ancestors 'none'" "$EXPLORER_RUNBOOK"; then
    err "$EXPLORER_RUNBOOK documents frame-ancestors 'none', but $EXPLORER_HEADERS's /* CSP deliberately carries no frame-ancestors (it made every /embed/* iframe unframeable)."
  fi
  while IFS= read -r doc; do
    [ -f "$doc" ] || err "$EXPLORER_HEADERS cites $doc, which does not exist."
  done < <(grep -oE 'docs/[A-Za-z0-9_./-]+\.md' "$EXPLORER_HEADERS" | sort -u)
fi

# ─── 17. k6 AlertManager-silence matchers must exist and be non-paging ─────
#
# test/load/scenarios/lib/alertmanager.js hardcodes silence matchers. An alertname matching no deployed alert is a no-op
# (pages during the burst); one carrying `severity: page` masks a real SEV-1. Both are re-derived from the rule files.

echo "Checking k6 AlertManager-silence matcher targets..."
AM_JS="test/load/scenarios/lib/alertmanager.js"
if [ -f "$AM_JS" ]; then
  # Only the alertname=... tokens in the `const matchers = (...)` assignment, not the amtool example in the header.
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
    # Take severity from the alert's own rule block (to the next `- alert:`/`- record:`). A fixed -A window is unsafe:
    # a long `expr: |` pushes `severity:` past it, the grep matches nothing and set -eo pipefail aborts the lint.
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
# AUTHORITY: `zfs_data_pool_type` in configs/ansible/inventory/r1.example.yml (not a literal here: that would be a third
# source). RULE: in the r1-scoped files below, a PARAGRAPH naming any raidz level must also name the live one; dated
# decision records (ADRs, audit docs) are not listed because they carry inline corrections.

echo "Checking r1 ZFS pool topology (single source of truth)..."
R1_INVENTORY="configs/ansible/inventory/r1.example.yml"
r1_pool_type=$(awk -F'"' '/^[[:space:]]*zfs_data_pool_type:[[:space:]]*"/{print $2; exit}' \
  "$R1_INVENTORY" 2>/dev/null || true)
if [ -z "$r1_pool_type" ]; then
  err "could not read zfs_data_pool_type from $R1_INVENTORY — that value is the authority for r1's pool topology; restore it or update scripts/ci/lint-docs.sh §18"
else
  r1_topology_files=(
    "$R1_INVENTORY"
    configs/prometheus/rules.r1/infra.yml
    deploy/monitoring/rules/infra.yml
    deploy/monitoring/rule-tests/infra_test.yml
    docs/architecture/ha-plan.md
    docs/architecture/storage-considerations.md
    docs/operations/r1-deployment-state.md
    docs/operations/self-hosting.md
    docs/operations/lcm-cache-tiering.md
    docs/operations/runbooks/infra.md
    docs/operations/runbooks/zfs-snapshots.md
    docs/operations/runbooks/postgres.md
    configs/ansible/inventory/r3.example.yml
  )
  # …and the subset that must SAY it, so gutting every mention cannot satisfy the gate.
  r1_topology_must_state=(
    configs/prometheus/rules.r1/infra.yml
    deploy/monitoring/rules/infra.yml
    docs/operations/runbooks/infra.md
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
  # The role default is deliberately raidz2 (right for a fresh node), so the defaults file is not
  # paragraph-scanned; only its per-region comment is checked.
  ANSIBLE_DEFAULTS="configs/ansible/roles/archival-node/defaults/main.yml"
  r1_comment=$(grep -E '^#[[:space:]]+r1 = ' "$ANSIBLE_DEFAULTS" 2>/dev/null || true)
  if [ -z "$r1_comment" ]; then
    err "$ANSIBLE_DEFAULTS: the per-region '#   r1 = <topology>' comment is gone — it is the line that told operators r1 ran raidz2 for a year (#289); keep it and keep it true"
  elif ! grep -q "$r1_pool_type" <<<"$r1_comment"; then
    err "$ANSIBLE_DEFAULTS: per-region comment says '$r1_comment' but r1's live topology is '$r1_pool_type' ($R1_INVENTORY). The role DEFAULT may stay raidz2 for fresh nodes; the comment must not describe r1 wrongly"
  fi
  echo "  r1 pool topology: '$r1_pool_type' (authority $R1_INVENTORY) — checked $r1_files_checked of ${#r1_topology_files[@]} r1-scoped files"
fi

# ─── 20. The self-hosting path: what a stranger types in their first hour ──
#
# Everything docs/operations/self-hosting.md tells a reader to type is a claim about this repo, so each is re-derived here.
# Config KEYS are checked against the schema by internal/config's TestSelfHostingGuide_ConfigRefs.

echo "Checking the self-hosting path..."
SELF_HOSTING="docs/operations/self-hosting.md"
if [ ! -f "$SELF_HOSTING" ]; then
  err "REQUIRED input missing: '$SELF_HOSTING' not found — it is the only end-to-end bring-up guide this repo ships; restore it or update lint-docs.sh §20"
fi

# (a) `git clone <url>` then `cd <dir>`: git names the checkout after the URL basename, so a mismatched
# `cd` works only on a case-insensitive filesystem.
if [ -f "$SELF_HOSTING" ]; then
  cd_drift=$(awk '
    /git clone [^ ]+/ {
      url = $0
      sub(/.*git clone[[:space:]]+/, "", url)
      sub(/[[:space:]].*/, "", url)
      sub(/\.git$/, "", url)
      n = split(url, parts, "/")
      repo = parts[n]
      clone_at = FNR
      next
    }
    # A `cd` further than a couple of lines below the clone is a different step.
    repo != "" && FNR > clone_at + 3 { repo = "" }
    repo != "" && $1 == "cd" {
      if ($2 != repo) printf "%d %s %s\n", FNR, $2, repo
      repo = ""
    }
  ' "$SELF_HOSTING")
  printf '%s\n' "$cd_drift" | while read -r ln got want; do
    [ -n "$ln" ] || continue
    err "$SELF_HOSTING:$ln — 'cd $got' immediately after a clone that creates '$want/'. The step fails outright on a case-sensitive filesystem; write 'cd $want'."
  done
fi

# (b) Every localhost:<port> the `make dev` banner advertises must be published by deploy/docker-compose/dev.yaml.
DEV_COMPOSE="deploy/docker-compose/dev.yaml"
if [ -f Makefile ] && [ -f "$DEV_COMPOSE" ]; then
  dev_recipe=$(awk '/^dev:/ { f = 1; next } /^[^\t]/ { f = 0 } f' Makefile)
  # Normalise compose's ${VAR:-1234} host-port defaults so ':-' is not read as a port separator.
  published=$(sed -E 's/\$\{[A-Za-z_]+:-([0-9]+)\}/\1/g' "$DEV_COMPOSE" |
    awk -F: '/^[[:space:]]*-[[:space:]]*"?[0-9.]+:[0-9]+:[0-9]+"?/ { gsub(/"/, "", $(NF - 1)); print $(NF - 1) }')
  advertised=$(printf '%s\n' "$dev_recipe" | grep -oE 'localhost:[0-9]+' | cut -d: -f2 | sort -u || true)
  for port in $advertised; do
    if ! grep -qx "$port" <<<"$published"; then
      err "Makefile's 'dev' target advertises http://localhost:$port, but $DEV_COMPOSE publishes no such port. 'make dev' is the first command in $SELF_HOSTING §4.1 — sending a first-time operator to a port nothing listens on is where self-hosting stops. Print what actually comes up."
    fi
  done
fi

# (c) The README that claims "Self-hostable." must link the guide.
if [ -f README.md ] && ! grep -q "operations/self-hosting.md" README.md; then
  err "README.md advertises the project as self-hostable but never links $SELF_HOSTING — a reader who takes the claim seriously has nowhere to go. Link it from the 'Start here' list."
fi

# (d) A default has one authority: Default() in internal/config/config.go. Prose claiming "off by default"
# for a knob that is on is the part nothing else checked.
for knob in ClickHouseLiveSink:clickhouse_live_sink ClickHouseProjectorSource:clickhouse_projector_source; do
  go_name=${knob%%:*}
  toml_name=${knob##*:}
  knob_default=$(awk -v n="$go_name" \
    '$0 ~ "^[[:space:]]*" n ":[[:space:]]*(true|false),[[:space:]]*$" { v = $2; sub(/,$/, "", v); print v; exit }' \
    internal/config/config.go)
  if [ -z "$knob_default" ]; then
    err "could not read $go_name's runtime default from internal/config/config.go's Default() — that assignment is §20d's authority; restore it or update lint-docs.sh"
    continue
  fi
  [ "$knob_default" = "true" ] || continue
  for f in internal/config/config.go cmd/stellarindex-indexer/main.go "$SELF_HOSTING"; do
    [ -f "$f" ] || continue
    # Proximity, not paragraph: a wider window would sweep in Default()'s body, one blank-free run.
    bad=$(awk -v gn="$go_name" -v tn="$toml_name" -v window=6 '
      { line[FNR] = $0 }
      END {
        for (i = 1; i <= FNR; i++) {
          if (tolower(line[i]) !~ /off by default/) continue
          lo = (i - window < 1 ? 1 : i - window)
          hi = (i + window > FNR ? FNR : i + window)
          for (j = lo; j <= hi; j++)
            if (index(line[j], gn) || index(line[j], tn)) { print i; break }
        }
      }
    ' "$f")
    for ln in $bad; do
      err "$f:$ln — calls $go_name / $toml_name off by default, but Default() sets it true. A knob documented as opt-in that is in fact on decides the deployment's topology for an operator who never chose it."
    done
  done
done

# ─── 21. Phoenix's synthetic XLM SAC test-fixture id must not resurface ─────
#
# phoenix/events.go documents CDLZFC3SY… as a synthetic fixture id, not the XLM SAC. A README republishing it as a
# mainnet address reopens that trap. Fixtures are exempt; prose docs are not.

echo "Checking Phoenix's synthetic XLM SAC test-fixture id hasn't resurfaced in a README..."
PHOENIX_FAKE_XLM_SAC="CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
while IFS= read -r -d '' readme; do
  [ -f "$readme" ] || continue
  if grep -q "$PHOENIX_FAKE_XLM_SAC" "$readme"; then
    err "$readme republishes '$PHOENIX_FAKE_XLM_SAC' as if it were a real address — internal/sources/phoenix/events.go documents this as the synthetic test/integration-fixture id, NOT a real XLM SAC on any network. Use aquarius.MainnetXLMSAC or canonical.Asset.SacContractID() instead; do not print this id in a README."
  fi
# Tracked files only: `find .` also walked .claude/worktrees/ (~25s) and let untracked copies redden the lint.
done < <(git ls-files -z -- ':(glob)**/README.md')

# ─── Explorer README claims must match package.json and src/app ────────────
#
# A documented `pnpm <script>  # <cmd>` must be a real script starting with <cmd>; a backticked `@scope/pkg` or Stack-list
# package must be a declared dependency; a backticked `/<route>/*` must be a directory under src/app.

echo "Checking web/explorer docs against package.json and src/app..."
if [ -f web/explorer/README.md ] && [ -f web/explorer/package.json ] && command -v python3 >/dev/null 2>&1; then
  explorer_out=$(python3 - <<'PY' 2>&1 || true
import json, os, re
readme_path = "web/explorer/README.md"
readme = open(readme_path, encoding="utf-8").read()
pkg = json.load(open("web/explorer/package.json", encoding="utf-8"))
scripts = pkg.get("scripts") or {}
deps = set(pkg.get("dependencies") or {}) | set(pkg.get("devDependencies") or {})
tools = {v.split()[0] for v in scripts.values() if v.split()}
for name, comment in re.findall(r"^pnpm ([\w:.-]+)[ \t]+#[ \t]*(.+)$", readme, re.M):
    if name not in scripts:
        print(f"{readme_path} documents 'pnpm {name}' but package.json has no '{name}' script")
        continue
    cmd = comment.split(" (")[0].strip()
    if cmd.split()[0] in tools and not scripts[name].startswith(cmd):
        print(f"{readme_path} documents 'pnpm {name}' as '{cmd}' but package.json runs '{scripts[name]}'")
stack = re.search(r"^## Stack\n(.*?)(?=^## )", readme, re.M | re.S)
named = set(re.findall(r"`(@[\w.-]+/[\w.-]+)`", readme))
if stack:
    named |= set(re.findall(r"\[([^\]\s]+)\]\(https?://", stack.group(1)))
for dep in sorted(named - deps):
    print(f"{readme_path} names package '{dep}' but web/explorer/package.json does not depend on it")
for doc in ("web/explorer/README.md", "web/explorer/AGENTS.md", "docs/architecture/design-system.md"):
    if not os.path.isfile(doc):
        continue
    text = open(doc, encoding="utf-8").read()
    for route in sorted(set(re.findall(r"`/([\w-]+)/\*`", text))):
        if not os.path.isdir(os.path.join("web/explorer/src/app", route)):
            print(f"{doc} names route '/{route}/*' but web/explorer/src/app/{route}/ does not exist")
PY
)
  if [ -n "$explorer_out" ]; then
    while IFS= read -r line; do
      [ -n "$line" ] && err "$line — correct the doc to what the explorer actually ships"
    done <<< "$explorer_out"
  fi
fi

# ─── 22. deploy/comms/README.md's "calls X.md" claims must be true ─────────
#
# A runbook that never links its template leaves the operator drafting from a blank page mid-incident.
echo "Checking customer-comms template callers..."
COMMS_README="deploy/comms/README.md"
if [ ! -f "$COMMS_README" ]; then
  echo "  (skipped §22 — $COMMS_README is gone)"
else
  comms_out=$(python3 - "$COMMS_README" <<'PY'
import os, re, sys

readme = sys.argv[1]
base = os.path.dirname(readme)
text = open(readme, encoding="utf-8").read()
m = re.search(r"^## Cross-references\n(.*?)(?=^## |\Z)", text, re.S | re.M)
if not m:
    print(f"{readme} has no '## Cross-references' section")
    sys.exit(0)
for bullet in re.split(r"^- ", m.group(1), flags=re.M)[1:]:
    calls = re.findall(r"calls `([\w.-]+\.md)`", bullet)
    link = re.search(r"\]\(([^)#\s]+)", bullet)
    if not calls or not link:
        continue
    doc = os.path.normpath(os.path.join(base, link.group(1)))
    if not os.path.isfile(doc):
        print(f"{readme} names caller '{doc}' but that file does not exist")
        continue
    doc_text = open(doc, encoding="utf-8").read()
    targets = {
        os.path.normpath(os.path.join(os.path.dirname(doc), t))
        for t in re.findall(r"\]\(([^)#\s]+)", doc_text)
    }
    for tmpl in calls:
        want = os.path.normpath(os.path.join(base, tmpl))
        if want not in targets:
            print(f"{readme} says {doc} calls '{tmpl}' but {doc} never links {want}")
PY
)
  if [ -n "$comms_out" ]; then
    while IFS= read -r line; do
      [ -n "$line" ] && err "$line — link the template from the step that sends it, or correct the README"
    done <<< "$comms_out"
  fi
fi

# ─── Every internal/ and pkg/ package has a package comment ─────────────────
#
# engineering-standards.md section 4.2: parsed here, not via `go list`, because this job has no Go toolchain.
echo "Checking package doc comments..."
if pkgdoc_out=$(git ls-files -z --cached --others --exclude-standard -- 'internal/*.go' 'pkg/*.go' | python3 -c '
import os, re, sys
pkgs = {}
for f in sys.stdin.read().split("\0"):
    parts = f.split("/")
    if not f or f.endswith("_test.go") or not os.path.isfile(f) or any(
            p == "testdata" or p[:1] in "._" for p in parts[:-1]):
        continue
    lines = open(f, encoding="utf-8").read().splitlines()
    idx = next((i for i, l in enumerate(lines) if re.match(r"package\s+\w+", l)), None)
    documented = False
    if idx is not None and idx > 0 and lines[idx - 1].rstrip().endswith("*/"):
        documented = True
    j = (idx or 0) - 1
    while idx is not None and j >= 0 and lines[j].startswith("//"):
        if not re.match(r"//(go:|line |export |extern |\s*$)", lines[j]):
            documented = True
        j -= 1
    d = os.path.dirname(f)
    pkgs[d] = pkgs.get(d, False) or documented
for d in sorted(k for k, v in pkgs.items() if not v):
    print(d)
'); then
  while IFS= read -r pkgdir; do
    [ -n "$pkgdir" ] && err "Package '$pkgdir' has no package doc comment — add a doc.go opening with '// Package <name> …' (engineering-standards.md §14.10)"
  done <<< "$pkgdoc_out"
else
  err "package doc-comment check failed to run: $pkgdoc_out"
fi

# ─── Documented make targets exist ───────────────────────────────────────────
#
# A documented `make X` must exist in the Makefile; "no `make X`" documents an absence and is exempt.
if [ -f Makefile ]; then
  make_targets=$(grep -oE '^[A-Za-z0-9_.-]+:' Makefile | tr -d ':' | sort -u)
  # shellcheck disable=SC2016  # awk program: literal backticks and $, not shell
  while read -r loc target; do
    [ -n "$target" ] || continue
    grep -qxF -- "$target" <<<"$make_targets" ||
      err "$loc documents 'make $target', but the Makefile has no '$target' target — add the target or correct the doc."
  done < <(git ls-files -z -- 'docs/operations/*.md' 'docs/contributing/*.md' CONTRIBUTING.md README.md AGENTS.md |
    xargs -0 awk '
      FNR == 1 { fence = 0 }
      /^[[:space:]]*```/ { fence = !fence; next }
      {
        line = $0
        while (match(line, /`make [a-z0-9][A-Za-z0-9_.-]*[` ]/)) {
          pre = substr(line, 1, RSTART - 1)
          t = substr(line, RSTART + 6, RLENGTH - 7)
          if (pre !~ /[Nn]o $/) print FILENAME ":" FNR, t
          line = substr(line, RSTART + RLENGTH)
        }
        if (fence && match($0, /^[[:space:]]*(\$ )?make [a-z0-9][A-Za-z0-9_.-]*/)) {
          t = substr($0, RSTART, RLENGTH); sub(/.*make /, "", t)
          print FILENAME ":" FNR, t
        }
      }')
fi

# ─── CHANGELOG.md stays a rolling window ─────────────────────────────────────
#
# Newest five release sections only; the 1 MiB cap drops to 200 KiB once [Unreleased] is regenerated at the cut.
if [ -f CHANGELOG.md ]; then
  sections=$(grep -c '^## \[v' CHANGELOG.md || true)
  if [ "$sections" -gt 5 ]; then
    err "CHANGELOG.md keeps $sections release sections; the rolling window is 5 — older ones live at their tags (CONTRIBUTING.md §Changelog)"
  fi
  bytes=$(wc -c < CHANGELOG.md | tr -d ' ')
  if [ "$bytes" -gt 1048576 ]; then
    err "CHANGELOG.md is $bytes bytes, over the 1 MiB cap; [Unreleased] is written at the release cut, not per PR (CONTRIBUTING.md §Changelog)"
  fi
fi

# ─── Summary────────────────────────────────────────────────────────────────

count=$(cat "$ERROR_FILE")
rm "$ERROR_FILE"

if [ "$count" -gt 0 ]; then
  echo ""
  echo "❌ Doc lint failed with $count error(s)."
  exit 1
fi
echo "✅ Doc lint passed."
