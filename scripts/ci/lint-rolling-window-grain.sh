#!/usr/bin/env bash
# shellcheck disable=SC2016 # the awk program is single-quoted on purpose
# A SELECT that bounds an hourly-or-wider continuous aggregate's buckets by `NOW() - INTERVAL '<n> hours|minutes'` covers a
# different span than its label: `bucket >= now() - 24h` drops the bucket holding now-24h, so a "24h" figure covers 23-24h.
# Split the window (whole buckets from the view, the partial edges from trades or prices_1m, as perSourcePoolsCTE does) or align
# the bound with time_bucket. Scans backtick SQL literals in internal/storage Go, one SELECT at a time; no baseline.
# Usage: lint-rolling-window-grain.sh [--root DIR]
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
while [ $# -gt 0 ]; do
  case "$1" in
    --root) ROOT="$2"; shift 2 ;;
    *) echo "lint-rolling-window-grain: unknown arg $1" >&2; exit 2 ;;
  esac
done
[ -d "$ROOT/internal/storage" ] || { echo "lint-rolling-window-grain: no $ROOT/internal/storage" >&2; exit 2; }

files="$(cd "$ROOT" && find internal/storage -name '*.go' ! -name '*_test.go' | sort)"
total="$(printf '%s\n' "$files" | grep -c .)"
# Emits "path:line<TAB>bound" for every SELECT reading a coarse view on a rolling bucket bound.
found="$(cd "$ROOT" && printf '%s\n' "$files" | tr '\n' '\0' | xargs -0 awk -v q="'" '
  # Lines are joined with \002, not \n: BWK awk (macOS) also splits on newline whatever the separator.
  BEGIN { rolling = "(^|[^A-Z0-9_])BUCKET[ \t\002]*>=?[ \t\002]*NOW\\(\\)[ \t\002]*-[ \t\002]*INTERVAL[ \t\002]*" q "[^" q "]*(HOUR|MINUTE)S?" q }
  function scan(path, text,   parts, n, i, lit, up, segs, m, j, line, k, b) {
    n = split(text, parts, "`")
    line = 1
    for (i = 1; i <= n; i++) {
      if (i % 2 == 0) {
        lit = parts[i]; up = toupper(lit)
        gsub(/(^|[^A-Z0-9_])SELECT([^A-Z0-9_]|$)/, "\001&", up)
        m = split(up, segs, "\001")
        for (j = 1; j <= m; j++) {
          if (segs[j] ~ /(^|[^A-Z0-9_])(POOLS_PER_SOURCE_1H|PRICES_(1H|4H|1D|1W|1MO)|TWAP_(1H|1D))([^A-Z0-9_]|$)/ &&
              match(segs[j], rolling)) {
            b = substr(segs[j], RSTART, RLENGTH); gsub(/[ \t\002]+/, " ", b); sub(/^[^B]*/, "", b)
            print path ":" line "\t" b
          }
        }
      }
      k = gsub(/\002/, "\002", parts[i]); line += k
    }
  }
  FNR == 1 { if (cur != "") scan(cur, buf); cur = FILENAME; buf = $0; next }
  { buf = buf "\002" $0 }
  END { if (cur != "") scan(cur, buf) }')"

if [ -n "$found" ]; then
  echo "lint-rolling-window-grain: a rolling NOW()-relative bucket bound over an hourly-or-wider view (split the window or align it with time_bucket):"
  printf '%s\n' "$found" | sed 's/^/  /'
  exit 1
fi
echo "lint-rolling-window-grain: ok ($total storage Go file(s), 0 rolling bounds on coarse views)"
