#!/usr/bin/env bash
# lint-http-timeouts — refuse an unbounded HTTP client in production Go code.
#
# THE BUG CLASS (#371 F5). A client with no Timeout has NONE: a server that
# accepts the connection and then stops sending leaves the caller blocked
# forever — not slow, HUNG. Two real instances shipped:
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
# #1256: `http.DefaultClient` was the only shape this gate matched. Three
# other spellings of the same unbounded client passed clean: the package-
# level shortcuts (`http.Get`/`Post`/`Head`/`PostForm`, which use
# DefaultClient internally), and any `http.Client{` composite literal —
# var-declared or inline — that never sets Timeout.
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

# is_rule_comment <line> — a comment ABOUT the rule is not a use of it.
is_rule_comment() {
  case "$(printf '%s' "$1" | sed 's/^[[:space:]]*//')" in
    '//'*) return 0 ;;
    *) return 1 ;;
  esac
}

# annotated <file> <line> — does the line, or the contiguous comment block
# directly above it, carry the escape hatch?
annotated() {
  local f="$1" n="$2" body prev back pl
  body=$(sed -n "${n}p" "$f")
  prev=""; back=$((n - 1))
  while [ "$back" -ge 1 ]; do
    pl=$(sed -n "${back}p" "$f")
    case "$(printf '%s' "$pl" | sed 's/^[[:space:]]*//')" in
      '//'*) prev="$prev$pl"; back=$((back - 1)) ;;
      *) break ;;
    esac
  done
  case "$body$prev" in *http-timeout-ok:*) return 0 ;; esac
  return 1
}

# report <file> <line> <reason>
report() {
  local f="$1" n="$2" reason="$3" body
  body=$(sed -n "${n}p" "$f")
  printf 'lint-http-timeouts: %s:%s %s\n' "$f" "$n" "$reason"
  printf '    %s\n' "$body"
  fail=$((fail + 1))
}

# check_client_literals <file> — a `http.Client{...}` composite literal
# (possibly multi-line) with no `Timeout:` field anywhere inside it.
check_client_literals() {
  local f="$1"
  while IFS=$'\t' read -r n has; do
    [ "$has" = "0" ] || continue
    is_rule_comment "$(sed -n "${n}p" "$f")" && continue
    annotated "$f" "$n" && continue
    report "$f" "$n" 'declares &http.Client{...} with no Timeout field — set Timeout explicitly'
  done < <(awk '
    { lines[NR] = $0 }
    END {
      for (i = 1; i <= NR; i++) {
        if (lines[i] !~ /http\.Client[[:space:]]*\{/) continue
        depth = 0; text = ""; j = i
        while (j <= NR) {
          l = lines[j]
          nopen = gsub(/\{/, "{", l)
          nclose = gsub(/\}/, "}", l)
          text = text lines[j] "\n"
          depth += nopen - nclose
          j++
          if (depth <= 0) break
        }
        has = (text ~ /Timeout[[:space:]]*:/) ? 1 : 0
        printf "%d\t%d\n", i, has
      }
    }
  ' "$f")
}

while IFS= read -r f; do
  [ -f "$f" ] || continue
  case "$f" in *_test.go) continue ;; esac
  checked=$((checked + 1))

  while IFS= read -r line; do
    n="${line%%:*}"
    body=$(sed -n "${n}p" "$f")
    is_rule_comment "$body" && continue
    annotated "$f" "$n" && continue
    report "$f" "$n" 'uses http.DefaultClient, which has NO timeout — use &http.Client{Timeout: ...}'
  done < <(grep -n 'http\.DefaultClient' "$f")

  while IFS= read -r line; do
    n="${line%%:*}"
    body=$(sed -n "${n}p" "$f")
    is_rule_comment "$body" && continue
    annotated "$f" "$n" && continue
    report "$f" "$n" 'calls http.Get/Post/Head/PostForm, which use http.DefaultClient (NO timeout) — build an &http.Client{Timeout: ...} instead'
  done < <(grep -n 'http\.\(Get\|Post\|Head\|PostForm\)(' "$f")

  check_client_literals "$f"
done < <(find "${roots[@]}" -type f -name '*.go' 2>/dev/null | sort)

if [ "$checked" -eq 0 ]; then
  echo "lint-http-timeouts: FAIL — no Go files found under ${roots[*]}; the gate would be vacuous" >&2
  exit 1
fi
if [ "$fail" -gt 0 ]; then
  echo "lint-http-timeouts: FAIL — $fail unbounded HTTP client use(s) in $checked file(s)" >&2
  exit 1
fi
echo "lint-http-timeouts: OK — $checked production Go file(s), no unbounded HTTP client use"
