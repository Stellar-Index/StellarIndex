#!/usr/bin/env bash
# govulncheck-gated.sh — govulncheck with a documented accepted-risk allowlist.
#
# The per-PR `vuln` job (.github/workflows/ci.yml) runs govulncheck as a hard
# blocking gate. github.com/lib/pq@v1.12.3 — the Postgres driver (ADR-0006) —
# carries a batch of disclosed, UNPATCHED vulns (v1.12.3 is already the latest
# release; lib/pq is unmaintained), so bare `govulncheck ./...` reds every PR
# with no way to land unrelated work. These lib/pq risks are all triggered by a
# malicious/compromised PostgreSQL server or a pre-auth MITM, and stellarindex
# only ever talks to its OWN Postgres over localhost — so they are accepted risks
# (rationale + per-id justification live in scripts/ci/govulncheck-allow.txt).
#
# This wrapper keeps the gate SHARP while accepting exactly those reviewed vulns:
#
#   * It runs `govulncheck -format json ./...` and computes the set of vulns that
#     are actually CALLED — a finding with a call-stack frame whose top element
#     names a function. That mirrors govulncheck's own exit-3 semantics: a vuln
#     that is merely imported-but-not-called (module/package frames only, no
#     function) never gated and is ignored here too.
#   * It FAILS (exit 1, with a ::error:: annotation) if ANY called vuln id is not
#     present in the allowlist — a new lib/pq CVE, or a vuln in any other module,
#     reds CI until a human reviews and justifies it. The allowlist is
#     accept-by-review, never accept-by-default; it cannot pass vacuously.
#   * It passes (exit 0) only when every called vuln is an accepted entry.
#
# Note: `govulncheck -format json` intentionally exits 0 even when vulns are found
# (the JSON is a data stream; the verdict is the consumer's job) — so this script,
# not govulncheck's exit code, is the gate. A non-zero govulncheck exit with no
# parseable JSON is treated as a scan failure and reds CI.
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
# An entry is the GO-id token on a line; inline `# reason` comments and blank /
# pure-comment lines are ignored. Bash 3.2-compatible (no mapfile).
ALLOWED=()
while IFS= read -r id; do
  [[ -n "$id" ]] && ALLOWED+=("$id")
done < <(sed -E 's/#.*$//' "$ALLOWLIST" | grep -oE 'GO-[0-9]{4}-[0-9]+' | sort -u)

if [[ "${#ALLOWED[@]}" -eq 0 ]]; then
  echo "::error::govulncheck-gated: allowlist '$ALLOWLIST' parsed to zero ids — refusing to run a gate with no defined policy." >&2
  exit 1
fi

is_allowed() {
  # exact-line match against the allowed set (empty-array safe under set -u)
  printf '%s\n' ${ALLOWED[@]+"${ALLOWED[@]}"} | grep -Fxq -- "$1"
}

# ── Run the scan ──
json_out="$(mktemp)"
scan_err="$(mktemp)"
trap 'rm -f "$json_out" "$scan_err"' EXIT

set +e
govulncheck -format json "${TARGETS[@]}" >"$json_out" 2>"$scan_err"
scan_rc=$?
set -e

# `govulncheck -format json` exits 0 on a COMPLETED scan whether or not vulns
# are found (the JSON stream is data; computing the verdict is this script's
# job). A non-zero exit therefore means govulncheck itself failed — a build or
# tooling error, never "vulnerabilities found". Fail closed: a broken or partial
# scan must red CI, never pass vacuously as "no vulnerabilities".
if [[ "$scan_rc" -ne 0 ]]; then
  echo "::error::govulncheck-gated: govulncheck exited non-zero ($scan_rc) — the scan failed to complete (build/tooling error), which is NOT the same as 'no vulnerabilities'." >&2
  echo "---- govulncheck stderr ----" >&2
  cat "$scan_err" >&2
  exit 1
fi
# Defence in depth: even on a 0 exit, require a parseable JSON stream before
# trusting an empty finding set.
if ! jq -e . "$json_out" >/dev/null 2>&1; then
  echo "::error::govulncheck-gated: govulncheck exited 0 but did not emit parseable JSON — treating as scan failure, NOT as 'no vulnerabilities'." >&2
  echo "---- govulncheck stderr ----" >&2
  cat "$scan_err" >&2
  exit 1
fi

# ── Compute the set of CALLED vuln ids ──
# Slurp the whole JSON-object stream (-s) into an array, keep `finding` objects
# whose call trace has a top frame with a non-empty `function` (i.e. reachable /
# called), and collect their distinct OSV ids.
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

# Stale-allowlist notice (informational only — never fails): an accepted id that
# is no longer called can eventually be pruned, but a stale entry must not red CI.
for id in "${ALLOWED[@]}"; do
  if ! printf '%s\n' ${accepted_seen[@]+"${accepted_seen[@]}"} | grep -Fxq -- "$id"; then
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
