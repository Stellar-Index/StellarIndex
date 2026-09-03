#!/usr/bin/env bash
# lint-http-timeouts — refuse http.DefaultClient in production Go code.
#
# THE BUG CLASS (#371 F5). `http.DefaultClient` has NO timeout. A server
# that accepts the connection and then stops sending leaves the caller
# blocked forever — not slow, HUNG. Two real instances shipped:
#
#   internal/sources/external/kraken/backfill_trades.go — a backfill that
#     never returns looks like a slow venue, so nobody investigates.
#   internal/ops/ingest/directory_sync.go — an operator command that
#     hangs at the terminal, mid-sync.
#
# A context deadline is NOT a substitute: it bounds the call only when
# the CALLER supplies one, and every one of these had a ctx parameter
# already. The fix is a client with an explicit Timeout, so the bound
# holds regardless of what the caller passed.
#
# Test files are exempt: a hung test fails the run loudly, which is the
# outcome we want anyway.
#
# ESCAPE HATCH: `// http-timeout-ok: <reason>` on the line or in the
# comment block directly above it. State the reason.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
roots=("${@:-internal cmd pkg}")
# shellcheck disable=SC2206
read -r -a roots <<<"${roots[*]}"

fail=0
checked=0
while IFS= read -r f; do
  [ -f "$f" ] || continue
  case "$f" in *_test.go) continue ;; esac
  checked=$((checked + 1))
  while IFS= read -r line; do
    n="${line%%:*}"
    body=$(sed -n "${n}p" "$f")
    # A comment ABOUT the rule is not a use of it.
    case "$(printf '%s' "$body" | sed 's/^[[:space:]]*//')" in
      '//'*) continue ;;
    esac
    # Look back over the contiguous comment block for the marker.
    prev=""; back=$((n - 1))
    while [ "$back" -ge 1 ]; do
      pl=$(sed -n "${back}p" "$f")
      case "$(printf '%s' "$pl" | sed 's/^[[:space:]]*//')" in
        '//'*) prev="$prev$pl"; back=$((back - 1)) ;;
        *) break ;;
      esac
    done
    case "$body$prev" in *http-timeout-ok:*) continue ;; esac
    printf 'lint-http-timeouts: %s:%s uses http.DefaultClient, which has NO timeout — use &http.Client{Timeout: ...}\n' "$f" "$n"
    printf '    %s\n' "$body"
    fail=$((fail + 1))
  done < <(grep -n 'http\.DefaultClient' "$f")
done < <(find "${roots[@]}" -type f -name '*.go' 2>/dev/null | sort)

if [ "$checked" -eq 0 ]; then
  echo "lint-http-timeouts: FAIL — no Go files found under ${roots[*]}; the gate would be vacuous" >&2
  exit 1
fi
if [ "$fail" -gt 0 ]; then
  echo "lint-http-timeouts: FAIL — $fail unbounded HTTP client use(s) in $checked file(s)" >&2
  exit 1
fi
echo "lint-http-timeouts: OK — $checked production Go file(s), no unbounded http.DefaultClient"
