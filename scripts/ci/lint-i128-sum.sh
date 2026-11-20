#!/usr/bin/env bash
# lint-i128-sum — no ClickHouse sum over an Int128 value without a reason (ADR-0003).
#
# ClickHouse sums Int128 in Int128 and wraps past 2^127 without an error, so a
# sum over an i128 Soroban amount serves a wrapped total. Sum those through
# toInt256. A sum over an Int128 value whose inputs are bounded far below 2^127
# (a classic Int64 stroop balance) is allowed with `i128sum:ok <reason>` in a
# comment on the same line or alone on the line above (`--` inside SQL).
#
# Flagged: sum / sumIf / sumState / sumMerge whose first argument
#   - is cast to Int128: toInt128(…), CAST(… AS Int128) / CAST(…, 'Int128'), …::Int128
#     (every Go file under internal/ and cmd/, and deploy/clickhouse/*.sql), or
#   - is a bare column whose name is Int128 somewhere in deploy/clickhouse/tier1_schema.sql
#     (ClickHouse sources only: internal/storage/clickhouse, Go files importing
#     clickhouse-go, and deploy/clickhouse/*.sql). The list is read at lint time.
# One line at a time: a sum whose first argument spans lines is not seen.
#
# Siblings: scripts/ci/lint-i128.sh (Go-side i128 truncation).
# Run: bash scripts/ci/lint-i128-sum.sh   Self-test: scripts/ci/lint-i128-sum-test.sh
set -euo pipefail
cd "$(dirname "$0")/../.."

schema=deploy/clickhouse/tier1_schema.sql
cols=$(grep -E '^[[:space:]]+[a-z_][a-z0-9_]*[[:space:]]+(Nullable\()?Int128\b' "$schema" \
  | awk '{print $1}' | sort -u | paste -sd'|' - || true)
if [ -z "$cols" ]; then
  echo "lint-i128-sum ❌ read no Int128 columns from $schema: the column scan is broken" >&2
  exit 1
fi

cast_files=()
col_files=()
while IFS= read -r f; do
  cast_files+=("$f")
  case "$f" in
  internal/storage/clickhouse/*) col_files+=("$f") ;;
  *) if grep -q 'clickhouse-go/v2' "$f"; then col_files+=("$f"); fi ;;
  esac
done < <(find internal cmd -name '*.go' ! -name '*_test.go' 2>/dev/null | sort)
for f in deploy/clickhouse/*.sql; do
  [ -e "$f" ] || continue
  cast_files+=("$f")
  col_files+=("$f")
done

# scan <cols-regex or empty> <files…>: prints offending lines, and "#sites N marked M" on stderr-free stdout last.
scan() {
  local colre="$1"
  shift
  awk -v colre="$colre" '
    function firstarg(s,   i, c, d) {
      d = 0
      for (i = 1; i <= length(s); i++) {
        c = substr(s, i, 1)
        if (c == "(") d++
        else if (c == ")") { if (d == 0) return substr(s, 1, i - 1); d-- }
        else if (c == "," && d == 0) return substr(s, 1, i - 1)
      }
      return s
    }
    function int128(a) {
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", a)
      if (a ~ /^toInt128\(/) return 1
      if (a ~ /^CAST\(/ && a ~ /[[:space:],'"'"'"]Int128['"'"'")]/) return 1
      if (a ~ /::Int128$/) return 1
      if (colre != "" && a ~ ("^([A-Za-z_][A-Za-z0-9_]*\\.)?(" colre ")$")) return 1
      return 0
    }
    FNR == 1 { prev = "" }
    {
      line = $0
      marked = (line ~ /i128sum:ok[[:space:]]+[^[:space:]]/) || (prev ~ /^[[:space:]]*(\/\/|--).*i128sum:ok[[:space:]]+[^[:space:]]/)
      prev = line
      if (line ~ /^[[:space:]]*(\/\/|--)/) next
      rest = line
      hit = 0
      while (match(rest, /(^|[^A-Za-z0-9_])sum(If|State|Merge)?\(/)) {
        rest = substr(rest, RSTART + RLENGTH)
        if (int128(firstarg(rest))) hit = 1
      }
      if (!hit) next
      sites++
      if (marked) ok++
      else print FILENAME ":" FNR ":" line
    }
    END { printf "#sites %d marked %d\n", sites, ok }' "$@"
}

fail=0
out=$(scan "" "${cast_files[@]}")
cout=$(scan "$cols" "${col_files[@]}")
hits=$(printf '%s\n%s\n' "$out" "$cout" | grep -v '^#sites' | sort -u || true)
sites=$(printf '%s\n' "$cout" | awk '/^#sites/ {print $2}')
marked=$(printf '%s\n' "$cout" | awk '/^#sites/ {print $4}')
if [ -n "$hits" ]; then
  echo "lint-i128-sum ❌ sum over an Int128 value wraps past 2^127 without an error (ADR-0003):" >&2
  echo "$hits" >&2
  echo "  → sum an i128 amount through toInt256; if the inputs are bounded far below 2^127 (Int64 stroops), mark the line i128sum:ok <reason>." >&2
  fail=1
fi
echo "lint-i128-sum: ${#cast_files[@]} file(s) scanned for Int128 casts, ${#col_files[@]} for the Int128 columns ($cols); ${sites:-0} ClickHouse-source sum site(s), ${marked:-0} marked"
if [ "$fail" -eq 0 ]; then
  echo "✅ i128 sum lint passed."
fi
exit "$fail"
