#!/usr/bin/env bash
# lint-ch-day-zone.sh — no ClickHouse calendar read that takes the SERVER's time zone. Production's
# server zone is not UTC: zone-less today()/yesterday(), a calendar function of a zone-less now(), and
# toDateTime of a Date without a zone all name the server's day, an hour or two off the UTC day the API
# serves. Pass the zone: toDate(now('UTC')), toStartOfDay(now('UTC')), toDateTime(max(day), 'UTC').
#
# Scans non-test .go under internal/storage/clickhouse/ and any other non-test .go under internal/,
# cmd/, pkg/ or scripts/ that imports clickhouse-go, plus deploy/clickhouse/*.sql. Whole-line -- and
# // comments are skipped. A line carrying `chzone:ok <reason>` is allowed.
set -euo pipefail

root="${LINT_ROOT:-$(git rev-parse --show-toplevel)}"

fail() {
  echo "lint-ch-day-zone: FAIL — $*" >&2
  exit 2
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

{
  for d in internal cmd pkg scripts; do
    [ -d "$root/$d" ] || continue
    find "$root/$d" -type f -name '*.go' ! -name '*_test.go' -exec grep -l -e 'clickhouse-go/v2' {} + || true
  done
  if [ -d "$root/internal/storage/clickhouse" ]; then
    find "$root/internal/storage/clickhouse" -type f -name '*.go' ! -name '*_test.go' -print
  fi
  if [ -d "$root/deploy/clickhouse" ]; then
    find "$root/deploy/clickhouse" -type f -name '*.sql' -print
  fi
} | LC_ALL=C sort -u >"$tmp/files"
files=()
while IFS= read -r f; do
  files+=("$f")
done <"$tmp/files"
n_files="${#files[@]}"
[ "$n_files" -gt 0 ] || fail "no ClickHouse .go/.sql file under $root; the scan matched nothing, so it proves nothing."

status=0
# shellcheck disable=SC2016 # awk program, not shell expansion
LC_ALL=C awk -v rootdir="$root/" -v countfile="$tmp/count" '
BEGIN {
  cal = "(toDate|toDate32|toStartOf[A-Za-z0-9]+|toYYYYMM|toYYYYMMDD|toMonday|toYear|toMonth|toDayOfMonth|toDayOfWeek|toHour|formatDateTime)"
  calls = "(^|[^A-Za-z0-9_.])(" cal "|today|yesterday|now|now64|toDateTime|toDateTime64)[ \t]*\\("
  rToday = "(^|[^A-Za-z0-9_.])(today|yesterday)[ \t]*\\([ \t]*\\)"
  rNow = "(^|[^A-Za-z0-9_.])" cal "[ \t]*\\([ \t]*now(64)?[ \t]*\\([ \t]*[0-9]*[ \t]*\\)"
  rCast = "(^|[^A-Za-z0-9_.])" cal "[ \t]*\\([ \t]*toDateTime(64)?[ \t]*\\([^,()]*\\)"
  rDate = "(^|[^A-Za-z0-9_.])toDateTime(64)?[ \t]*\\([ \t]*(day|month|(max|min|any|anyLast)[ \t]*\\([ \t]*(day|month)[ \t]*\\))[ \t]*\\)"
  rDateFn = "(^|[^A-Za-z0-9_.])toDateTime(64)?[ \t]*\\([ \t]*(toDate|toDate32|toStartOf[A-Za-z0-9]+|toMonday)[ \t]*\\([^()]*\\)[ \t]*\\)"
}
FNR == 1 { file = FILENAME; sub("^" rootdir, "", file) }
/^[ \t]*(--|\/\/)/ { next }
{
  if ($0 !~ calls) next
  checked++
  if ($0 ~ /chzone:ok/) next
  why = ""
  if ($0 ~ rToday) why = "today()/yesterday() is the server zone'\''s day; use toDate(now('\''UTC'\''))"
  else if ($0 ~ rNow) why = "a calendar function of a zone-less now() takes the server zone; use now('\''UTC'\'')"
  else if ($0 ~ rCast) why = "a calendar function of a one-argument toDateTime() takes the server zone; pass '\''UTC'\''"
  else if ($0 ~ rDate || $0 ~ rDateFn) why = "toDateTime() of a Date is the server zone'\''s midnight; pass '\''UTC'\''"
  if (why != "") {
    bad++
    printf "%s:%d: %s\n    %s\n", file, FNR, why, $0 > "/dev/stderr"
  }
}
END { printf "%d %d\n", checked + 0, bad + 0 > countfile }
' "${files[@]}" || status=$?

[ -s "$tmp/count" ] || fail "the scan did not complete (awk exit $status)."
read -r checked bad <"$tmp/count"
[ "$checked" -gt 0 ] || fail "0 date/time calls parsed in $n_files file(s); the scan matched nothing, so it proves nothing."
if [ "$bad" -ne 0 ]; then
  echo "lint-ch-day-zone: FAIL — $bad of $checked ClickHouse date/time call line(s) in $n_files file(s) take the server's zone (allow a justified one with chzone:ok <reason>)." >&2
  exit 1
fi
[ "$status" -eq 0 ] || fail "awk exit $status."
echo "lint-ch-day-zone: OK — $checked ClickHouse date/time call line(s) across $n_files file(s) name their zone."
