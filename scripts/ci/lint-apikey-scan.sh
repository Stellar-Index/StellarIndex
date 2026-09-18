#!/usr/bin/env bash
# lint-apikey-scan.sh — no new walk of the API-key credential keyspace
# (class finding K051, reverification 2026-09-18).
#
# The defect this pins shut: a lookup of credential records by something
# other than their Redis key — "which keys does this owner hold", "which
# record has this KeyID" — written as `SCAN apikey:*` plus one GET per
# match. It reads as a harmless helper, it passes every test (the test
# keyspace has three keys in it), and on a request path it costs O(every
# credential in the deployment) against the single-threaded Redis that is
# also the rate limiter. Open registration makes that keyspace
# attacker-sized. Four of them shipped, three behind /v1/account/keys,
# and each new one was written by copying the one before it.
#
# The lookups now read the `apikey-index:v1` hash
# (internal/auth/list_keys.go). Exactly ONE walk remains on purpose —
# `walkAPIKeyRecords`, which builds that index and serves lookups while
# it is unusable. This gate keeps it at one.
#
# Rules, over production Go (tests are exempt — they seed and inspect
# keyspaces freely):
#
#   1. Under internal/auth: no Redis scan-family call at all —
#      `.Scan(` `.ScanType(` `.Keys(` `.SScan(` `.HScan(` `.ZScan(`.
#      The package holds no SQL, so there is no honest `.Scan(` to
#      confuse this with; if that changes, mark the site.
#   2. Under internal/ cmd/ pkg/: nobody builds the credential wildcard
#      — `APIKey("*")` or a literal `apikey:*`. This is the tell that
#      survives a scan split across lines or moved to another package.
#   3. A site is exempt only with an `apikey-scan-ok: <reason>` marker
#      on the line or in the comment block directly above it, and the
#      whole tree may hold AT MOST ONE marked site. A second walk needs
#      this script edited in the same diff, which is the review.
#   4. A marker that exempts nothing FAILS, so the allowance can only
#      shrink.
#
# Usage: lint-apikey-scan.sh [ROOT]   (ROOT defaults to the repo root;
# the self-test points it at fixture trees).
#
# Sibling: internal/auth/apikey_scan_lint_test.go runs this script and
# its self-test under `go test`, so the ban is enforced wherever the Go
# suite runs even before this file is named in verify.sh / ci.yml.
#
# Exit 0 clean, 1 on any violation or a vacuous run.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
root="${1:-.}"
max_sanctioned=1

auth_dir="$root/internal/auth"
tree_dirs=()
for d in internal cmd pkg; do
  [ -d "$root/$d" ] && tree_dirs+=("$root/$d")
done

scan_call='\.(Scan|ScanType|Keys|SScan|HScan|ZScan)\('
wildcard='APIKey\("\*"\)|apikey:\*'

# marked FILE LINE — is the site exempted by a marker on the line or in
# the contiguous comment block above it?
marked() {
  local f="$1" n="$2" body prev="" back pl
  body=$(sed -n "${n}p" "$f")
  back=$((n - 1))
  while [ "$back" -ge 1 ]; do
    pl=$(sed -n "${back}p" "$f")
    case "$(printf '%s' "$pl" | sed 's/^[[:space:]]*//')" in
      '//'*) prev="$prev$pl"; back=$((back - 1)) ;;
      *) break ;;
    esac
  done
  case "$body$prev" in *apikey-scan-ok:*) return 0 ;; esac
  return 1
}

is_comment() { # is_comment FILE LINE
  case "$(sed -n "${2}p" "$1" | sed 's/^[[:space:]]*//')" in
    '//'*) return 0 ;;
  esac
  return 1
}

fail=0
checked=0
sites=""      # "file:line" of every violation candidate, marked or not
sanctioned="" # the marked subset

consider() { # consider FILE LINE WHY
  local f="$1" n="$2" why="$3" site="$1:$2"
  is_comment "$f" "$n" && return 0
  case " $sites " in *" $site "*) return 0 ;; esac # both rules, one site
  sites="$sites $site"
  if marked "$f" "$n"; then
    sanctioned="$sanctioned $site"
    return 0
  fi
  printf 'lint-apikey-scan: %s %s\n' "$site" "$why"
  printf '    %s\n' "$(sed -n "${n}p" "$f")"
  fail=$((fail + 1))
}

if [ -d "$auth_dir" ]; then
  while IFS= read -r f; do
    case "$f" in *_test.go) continue ;; esac
    checked=$((checked + 1))
    while IFS= read -r hit; do
      consider "$f" "${hit%%:*}" \
        "is a Redis scan in internal/auth — resolve the record through the apikey-index (findRecordByKeyID / ListKeysForIdentifier), do not walk the keyspace"
    done < <(grep -nE "$scan_call" "$f")
  done < <(find "$auth_dir" -type f -name '*.go' | sort)
fi

# One recursive grep, not one per file: this sweep covers ~2,000 files
# and runs inside `go test ./internal/auth`.
if [ "${#tree_dirs[@]}" -gt 0 ]; then
  while IFS= read -r hit; do
    f="${hit%%:*}"
    rest="${hit#*:}"
    case "$f" in *_test.go) continue ;; esac
    consider "$f" "${rest%%:*}" \
      "builds the apikey:* wildcard — a walk of the credential keyspace costs O(every key in the deployment)"
  done < <(grep -rnE --include='*.go' "$wildcard" "${tree_dirs[@]}" | sort)
fi

if [ "$checked" -eq 0 ]; then
  echo "lint-apikey-scan: FAIL — no production Go files under $auth_dir; the gate would be vacuous" >&2
  exit 1
fi

# Rule 3: at most one sanctioned walk.
n_sanctioned=0
for s in $sanctioned; do n_sanctioned=$((n_sanctioned + 1)); done
if [ "$n_sanctioned" -gt "$max_sanctioned" ]; then
  echo "lint-apikey-scan: $n_sanctioned sites carry apikey-scan-ok, at most $max_sanctioned is allowed:$sanctioned" >&2
  fail=$((fail + 1))
fi

# Rule 4: a marker that exempts nothing is stale.
if [ "${#tree_dirs[@]}" -gt 0 ]; then
  n_markers=$(find "${tree_dirs[@]}" -type f -name '*.go' ! -name '*_test.go' -exec grep -l 'apikey-scan-ok:' {} + | wc -l | tr -d ' ')
  n_marked_files=0
  seen_files=""
  for s in $sanctioned; do
    sf="${s%:*}"
    case " $seen_files " in *" $sf "*) ;; *) seen_files="$seen_files $sf"; n_marked_files=$((n_marked_files + 1)) ;; esac
  done
  if [ "$n_markers" -gt "$n_marked_files" ]; then
    echo "lint-apikey-scan: an apikey-scan-ok marker exempts nothing (found in $n_markers file(s), used in $n_marked_files) — delete the stale marker" >&2
    fail=$((fail + 1))
  fi
fi

if [ "$fail" -gt 0 ]; then
  echo "lint-apikey-scan: FAIL — $fail problem(s) in $checked internal/auth file(s) + the wildcard sweep" >&2
  exit 1
fi
echo "lint-apikey-scan: OK — $checked internal/auth production file(s) and ${#tree_dirs[@]} tree root(s) swept, $n_sanctioned sanctioned walk(s) (max $max_sanctioned)"
