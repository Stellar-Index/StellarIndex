#!/usr/bin/env bash
# govulncheck-gated.sh — govulncheck with a documented accepted-risk allowlist.
#
# The per-PR `vuln` job (.github/workflows/ci.yml) is a hard gate.
# github.com/lib/pq@v1.10.9 (indirect, via golang-migrate's postgres driver;
# the app's own driver is pgx/v5, ADR-0006) carries disclosed, UNPATCHED vulns
# (lib/pq is unmaintained), so bare `govulncheck ./...` would red every PR.
# They need a malicious/compromised Postgres server or a pre-auth MITM, and
# stellarindex only talks to its own Postgres over localhost, so they are
# accepted risks (per-id rationale: scripts/ci/govulncheck-allow.txt).
#
# The gate stays sharp while accepting exactly those reviewed vulns:
#   * Runs `govulncheck -format json ./...` and takes the vulns actually
#     CALLED: a finding whose call-stack top frame names a function. This
#     mirrors govulncheck's exit-3 semantics; imported-but-not-called vulns
#     never gated and are ignored.
#   * FAILS (exit 1, ::error:: annotation) if ANY called vuln id is not in the
#     allowlist. Accept-by-review, never by default; cannot pass vacuously.
#   * Passes (exit 0) only when every called vuln is an accepted entry.
#
# `govulncheck -format json` exits 0 even when vulns are found (the JSON is a
# data stream), so this script, not govulncheck's exit code, is the gate. A
# non-zero exit with no parseable JSON is a scan failure and reds CI.
#
# Usage:  scripts/ci/govulncheck-gated.sh [scan-target ...]     (default: ./...)
# Env:    GOVULNCHECK_ALLOWLIST   allowlist path (default scripts/ci/govulncheck-allow.txt)
set -euo pipefail

cd "$(dirname "$0")/../.."

ALLOWLIST="${GOVULNCHECK_ALLOWLIST:-scripts/ci/govulncheck-allow.txt}"
if [[ "$#" -gt 0 ]]; then
  TARGETS=("$@")
else
  TARGETS=("./...")
fi

# ── Preconditions ──
for tool in govulncheck jq; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "::error::govulncheck-gated: required tool '$tool' not found on PATH." >&2
    exit 1
  fi
done
if [[ ! -f "$ALLOWLIST" ]]; then
  echo "::error::govulncheck-gated: allowlist '$ALLOWLIST' not found." >&2
  exit 1
fi

# ── Load the allowlist ──
# An entry is the GO-id token on a line; inline `# reason` comments and blank
# or comment-only lines are ignored. Bash 3.2-compatible (no mapfile).
ALLOWED=()
while IFS= read -r id; do
  [[ -n "$id" ]] && ALLOWED+=("$id")
done < <(sed -E 's/#.*$//' "$ALLOWLIST" | grep -oE 'GO-[0-9]{4}-[0-9]+' | sort -u)

if [[ "${#ALLOWED[@]}" -eq 0 ]]; then
  echo "::error::govulncheck-gated: allowlist '$ALLOWLIST' parsed to zero ids — refusing to run a gate with no defined policy." >&2
  exit 1
fi

is_allowed() {
  # Exact-line match, empty-array safe under set -u. The list is landed
  # before the match, not piped: `grep -Fxq` exits at its first hit and
  # pipefail would report that SUCCESSFUL match as a failed lookup once
  # printf outgrew the pipe buffer.
  grep -Fxq -- "$1" <<<"$(printf '%s\n' ${ALLOWED[@]+"${ALLOWED[@]}"})"
}

# ── Run the scan ──
json_out="$(mktemp)"
scan_err="$(mktemp)"
trap 'rm -f "$json_out" "$scan_err"' EXIT

set +e
govulncheck -format json "${TARGETS[@]}" >"$json_out" 2>"$scan_err"
scan_rc=$?
set -e

# JSON mode exits 0 on a COMPLETED scan whether or not vulns are found, so a
# non-zero exit means govulncheck itself failed (build/tooling). Fail closed:
# a broken scan must never pass as "no vulnerabilities".
if [[ "$scan_rc" -ne 0 ]]; then
  echo "::error::govulncheck-gated: govulncheck exited non-zero ($scan_rc) — the scan failed to complete (build/tooling error), which is NOT the same as 'no vulnerabilities'." >&2
  echo "---- govulncheck stderr ----" >&2
  cat "$scan_err" >&2
  exit 1
fi
# Defence in depth: on a 0 exit, still require parseable JSON before trusting
# an empty finding set.
if ! jq -e . "$json_out" >/dev/null 2>&1; then
  echo "::error::govulncheck-gated: govulncheck exited 0 but did not emit parseable JSON — treating as scan failure, NOT as 'no vulnerabilities'." >&2
  echo "---- govulncheck stderr ----" >&2
  cat "$scan_err" >&2
  exit 1
fi

# ── Compute the set of CALLED vuln ids ──
# Slurp the JSON stream (-s); keep `finding`s whose top trace frame has a
# non-empty `function` (reachable/called); collect distinct OSV ids.
called_ids="$(jq -rs '
  [ .[]
    | select(has("finding")) | .finding
    | select((.trace // []) | length > 0)
    | select(.trace[0].function != null and .trace[0].function != "")
    | .osv
  ] | unique | .[]' "$json_out")"

# ── Classify called ids against the allowlist ──
accepted_seen=()
unaccepted=()
if [[ -n "$called_ids" ]]; then
  while IFS= read -r id; do
    [[ -n "$id" ]] || continue
    if is_allowed "$id"; then
      accepted_seen+=("$id")
    else
      unaccepted+=("$id")
    fi
  done <<< "$called_ids"
fi

echo "govulncheck-gated: allowlist '$ALLOWLIST' (${#ALLOWED[@]} accepted id(s))."

if [[ "${#accepted_seen[@]}" -gt 0 ]]; then
  echo "govulncheck-gated: accepted-risk vulns present and CALLED (allowlisted, not failing):"
  for id in "${accepted_seen[@]}"; do
    echo "  - $id"
  done
fi

# Stale-allowlist notice (informational, never fails): an accepted id no
# longer called can be pruned, but a stale entry must not red CI.
for id in "${ALLOWED[@]}"; do
  if ! grep -Fxq -- "$id" <<<"$(printf '%s\n' ${accepted_seen[@]+"${accepted_seen[@]}"})"; then
    echo "::notice::govulncheck-gated: allowlisted vuln $id is not currently called — candidate for pruning once the dependency is gone."
  fi
done

if [[ "${#unaccepted[@]}" -gt 0 ]]; then
  echo "::error::govulncheck-gated: CALLED vulnerabilit(y/ies) NOT in the accepted-risk allowlist — CI is red until each is reviewed and either fixed or justified in $ALLOWLIST:" >&2
  for id in "${unaccepted[@]}"; do
    echo "::error::  unaccepted called vuln: $id  (details: https://pkg.go.dev/vuln/$id)" >&2
  done
  exit 1
fi

if [[ "${#accepted_seen[@]}" -eq 0 ]]; then
  echo "govulncheck-gated: no called vulnerabilities found — clean."
fi
echo "govulncheck-gated: PASS — every called vulnerability is an accepted-risk allowlist entry."
exit 0
