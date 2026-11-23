#!/usr/bin/env bash
# Migration lint: money-column (ADR-0003), file integrity, register
# completeness, ClickHouse money-column, hypertable index builds, CAGG
# re-materialization, atomicity, priceable leg division.
#
# Ten passes, all gating (exit non-zero on any violation):
#   1. money-column — monetary columns must be NUMERIC (ADR-0003).
#   2. file integrity — every NNNN_*.up.sql has a matching NON-EMPTY
#      *.down.sql (and no orphan downs), no duplicate NNNN prefixes, no empty
#      files. Numbering GAPS ALREADY in the tree are a non-fatal WARNING (the
#      repo legitimately skips numbers when a migration is squashed/removed),
#      but a NEW migration numbered below the comparison base's head (filed
#      INTO a gap) is FATAL: it is invisible to `migrate up` on any deployment
#      already past that number and silently divergent on a fresh one.
#   3. register completeness — every NNNN_*.up.sql has a row in
#      migrations/README.md's register, no row names a missing file, and every
#      row's description is a whole sentence.
#   4. ClickHouse money-column — the lake DDL under deploy/clickhouse/ must
#      never hold a monetary column in Float32/Float64 (ADR-0003 applied to the
#      substrate; see the pass for the type rule).
#   5. down-delete — no *.down.sql DELETEs/TRUNCATEs rows silently; a
#      narrowing down refuses with a RAISE EXCEPTION guard instead.
#   6. hypertable index — a CREATE INDEX on an existing hypertable uses
#      IF NOT EXISTS under SET LOCAL lock_timeout (see the pass).
#   7. CAGG re-materialization — a file that recreates a continuous
#      aggregate WITH NO DATA names its refresh (see the pass).
#   8. atomicity — no SQL after a file's first COMMIT/ROLLBACK (see the pass).
#   9. priceable division — an up.sql statement that divides by base_amount or
#      quote_amount carries the priceable filter (see the pass).
#  10. follow-up marker — an up.sql that blanks data names each rebuild in a
#      `-- REQUIRED-FOLLOWUP:` header line (see the pass).
#
# ── register-completeness detail ──
#
# The reader who pays for a missing register row is someone bringing up a FRESH
# database (docs/operations/archival-node-bringup.md), for whom the row is where
# an "⚠ operator must re-materialize" warning lives: 0147 leaves nine CAGGs
# empty, and r1 having run it does nothing for a new node.
#
# The register is prose, so this pass checks PRESENCE, in both directions, and
# SHAPE: a description starts with a capital, digit, backtick or `**` and ends
# with a full stop (optionally inside `**`). A row lifted from one wrapped
# header line stops mid-sentence or keeps only a tail, which presence alone
# passes. Shape cannot check that a row is true.
#
# ── money-column detail ──
#
# Money is NUMERIC — never BIGINT / INTEGER / SMALLINT / INT8 / INT4 / INT2 /
# INT / MONEY / DOUBLE PRECISION / FLOAT[n] / REAL (JSON numbers are IEEE-754
# doubles; i128 amounts overflow int64 and 2^53, narrower types lose more). For
# every migrations/*.up.sql this flags a column definition whose name looks
# monetary next to a non-NUMERIC numeric type.
#
# Single-line `name type` DDL only: a view/CAGG column typed by its expression
# (`… ::double precision AS volume_usd`), a type on the next line or a float
# DOMAIN pass it. The authoritative check is TestMoneyColumnsAreNumeric
# (test/integration/pg_migrations_test.go), which reads every column's resolved
# type from pg_catalog (domains and array elements resolved) and reuses the
# `name` stem pattern below.
#
# Escape hatch: append `-- lint-money:ok <reason>` on the flagged line and list
# the column in the test's moneyColumnExceptions. Reasons are mandatory (e.g.
# SDEX price_n/price_d, a protocol-defined int32 rational pair whose money value
# lives in the sibling NUMERIC `price` column).
#
# Supersedes the SQL half of scripts/ci/lint-i128.sh (Go side only now); the
# deep Go-side guard is the go/types walk in
# internal/canonical/i128_truncation_guard_test.go.
#
# Exit 0 clean, non-zero on any violation. Wired into verify.sh + CI.
set -euo pipefail
cd "$(dirname "$0")/../.."
fail=0

indent() { printf '  %s\n' "${1//$'\n'/$'\n'  }"; }

# MIGRATIONS_CONTENT_FLOOR=<N> (deploy only; unset = judge every file, as CI does): the content
# rules (passes 1 and 5-10) skip migrations numbered <= N, which the host has already applied and
# which are immutable. Numbering, pairing, register and back-numbering checks still cover all files.
floor="${MIGRATIONS_CONTENT_FLOOR:-}"
case "$floor" in
  *[!0-9]*) echo "lint-migrations ❌ MIGRATIONS_CONTENT_FLOOR='${floor}' is not a number — refusing to judge (fail-closed)" >&2; exit 1 ;;
esac
# above_floor <file|entry>: true when the migration is new to the host (or no floor is set).
above_floor() {
  local n
  [ -n "$floor" ] || return 0
  n="$(basename "$1")"; n="${n%%_*}"
  case "$n" in '' | *[!0-9]*) return 0 ;; esac
  [ "$((10#$n))" -gt "$((10#$floor))" ]
}

# Monetary column-name stems. `_usd` matches only as a column-name suffix
# (value_usd, volume_usd, …) so `usda`-style codes don't trip it. twap/vwap/tvl/
# wealth are included: a weighted-average PRICE and total-value-locked/net-worth
# aggregate are money.
#
# Deliberately NOT in the stem set (each would false-positive on existing
# NON-money columns, and a stem that flags real code is worse than the gap):
#   - median / mad — the volatility_baseline columns hold a median VWAP
#     percent-change return + its MAD (0007/0008): dimensionless statistics,
#     correctly DOUBLE PRECISION. No genuine money-median column exists.
#   - rate — money rates already match via `_usd` (rate_usd); bare `rate` would
#     flag rate_limit_per_min (a request count, 0027).
#   - cap — market cap already matches via `market_cap`; bare `cap` would flag
#     capacity/capture/…-style names.
name='[a-z0-9_]*(amount|price|supply|balance|volume|reserve|fee|stroop|wei|circulating|market_cap|twap|vwap|tvl|wealth)[a-z0-9_]*|[a-z0-9_]*_usd'
# Non-NUMERIC types that must never hold money: wide ints (bigint/int8), narrower
# ints (integer/smallint/int4/int2/int), floats (double precision/float[n]/real)
# and `money` (fixed 2-decimal, locale-dependent, never right for an i128
# amount). NUMERIC is the only allowed home, unbounded: NUMERIC(p,s) silently
# rounds a value with more than s fractional digits.
type='bigint|integer|smallint|int8|int4|int2|int|money|double precision|float[0-9]*|real'
tre="(${type})\\b|(numeric|decimal)[[:space:]]*\\("

for f in "${MIG_DIR:-migrations}"/*.up.sql; do
  above_floor "$f" || continue
  hits=$(grep -nEi "(^|[[:space:](,])\"?(${name})\"?[[:space:]]+(${tre})" "$f" \
    | grep -vE '^[0-9]+:[[:space:]]*--' \
    | grep -viE -- '-- *lint-money:ok +[^ ]' || true)
  if [ -n "$hits" ]; then
    echo "lint-migrations ❌ ${f}: monetary column is not NUMERIC (ADR-0003):" >&2
    indent "$hits" >&2
    fail=1
  fi
  # Stale escapes: a lint-money:ok marker on a line the pattern does not flag is
  # dead weight; the allowlist only shrinks.
  stale=$(grep -nEi -- '-- *lint-money:ok' "$f" \
    | grep -vEi "(^|[[:space:](,])\"?(${name})\"?[[:space:]]+(${tre})" || true)
  if [ -n "$stale" ]; then
    echo "lint-migrations ❌ ${f}: stale lint-money:ok marker (line no longer matches the lint) — remove it:" >&2
    indent "$stale" >&2
    fail=1
  fi
done

if [ "$fail" -eq 0 ]; then
  echo "✅ migration money-column lint passed."
fi

# ─── Pass 2: pairing / numbering / non-empty ───
# A missing or empty .down.sql means a migration can't be rolled back, a silent
# trap found only during an incident. EXISTING gaps are WARN-only (see header):
# the tree legitimately skips numbers (e.g. 0075, 0077-0079, 0084), so a hard
# no-gap rule would false-positive. A NEW migration filed INTO a gap is a
# different, fatal shape (below).

# Non-empty: every migration file must have content.
for f in migrations/*.sql; do
  [ -e "$f" ] || continue
  if [ ! -s "$f" ]; then
    echo "lint-migrations ❌ ${f}: migration file is empty" >&2
    fail=1
  fi
done

# up ↔ down pairing: every up needs a non-empty down; no orphan downs.
for up in migrations/*.up.sql; do
  [ -e "$up" ] || continue
  down="${up%.up.sql}.down.sql"
  if [ ! -f "$down" ]; then
    echo "lint-migrations ❌ ${up}: no matching down migration (${down##*/})" >&2
    fail=1
  elif [ ! -s "$down" ]; then
    echo "lint-migrations ❌ ${down}: down migration is empty (a down must reverse its up)" >&2
    fail=1
  fi
done
for down in migrations/*.down.sql; do
  [ -e "$down" ] || continue
  up="${down%.down.sql}.up.sql"
  if [ ! -f "$up" ]; then
    echo "lint-migrations ❌ ${down}: orphan down migration (no matching ${up##*/})" >&2
    fail=1
  fi
done

# migrate_versions: `<version>.<up|down>` for each name on stdin that
# golang-migrate's file source loads (its regex, top level of migrations/ only).
migrate_versions() {
  grep -E '^[0-9]+_.*\.(up|down)\..*$' | sed -E 's/^0*([0-9]+)_.*\.(up|down)\..*$/\1.\2/' | sort -u || true
}

# golang-migrate reads one directory level: a migration in a subdirectory never runs.
nested="$(find migrations -mindepth 2 -name '*.sql' 2>/dev/null || true)"
if [ -n "$nested" ]; then
  echo "lint-migrations ❌ migration file(s) below migrations/ — golang-migrate reads only the top level, so these never run:" >&2
  indent "$nested" >&2
  fail=1
fi

# golang-migrate loads any top-level NNN_x.(up|down).<ext>, but every pass and
# gate selects *.up.sql / *.down.sql. Holding the directory to that one shape
# is what makes those globs see every file the deploy will run.
nonconform="$(find "${MIG_DIR:-migrations}" -maxdepth 1 -type f 2>/dev/null | sed 's#.*/##' \
  | grep -E '^[0-9]+_.*\.(up|down)\..*$' | grep -vE '^[0-9]{4}_[a-z0-9_]+\.(up|down)\.sql$' || true)"
if [ -n "$nonconform" ]; then
  echo "lint-migrations ❌ file(s) golang-migrate loads that are not NNNN_name.(up|down).sql, so no migration gate reads them:" >&2
  indent "$nonconform" >&2
  fail=1
fi

# Duplicate NNNN prefixes among *.up.sql (two migrations claiming one number).
dupes=$(find migrations -maxdepth 1 -name '*.up.sql' \
  | sed -E 's#.*/([0-9]+)_.*#\1#' | sort | uniq -d || true)
if [ -n "$dupes" ]; then
  echo "lint-migrations ❌ duplicate migration number(s) among *.up.sql:" >&2
  indent "$dupes" >&2
  fail=1
fi

# Numbering gaps → WARNING only (non-fatal; see header).
gaps=$(find migrations -maxdepth 1 -name '*.up.sql' \
  | sed -E 's#.*/([0-9]+)_.*#\1#' | sort -n | awk '
    NR==1 { prev = $1 + 0; next }
    { cur = $1 + 0; while (prev + 1 < cur) { prev++; printf "%04d ", prev } prev = cur }
  ' || true)
if [ -n "$gaps" ]; then
  echo "lint-migrations ⚠️  numbering gap(s) (non-fatal — squashed/removed migrations): ${gaps}" >&2
fi

# ── back-numbering guard ──
# Nothing else asserted a NEW migration's number exceeds the head at the time it
# was added, so a file back-numbered into a gap sails through: immutability just
# appends it, the duplicate check is silent (no collision), and
# TestExpectedSchemaVersionMatchesMigrationsHead takes a MAX over the tree so the
# constant doesn't move. A deployment already past that number never runs it
# (`migrate up` only walks numbers above the applied one); a fresh database does:
# permanent, silent schema divergence.
#
# Compares against a base (BASE_SHA as in lint-baseline-growth.sh, with a local
# origin/main merge-base fallback) to find *.up.sql files NEW in this change, and
# requires each number to exceed every number existing at that base. Silently
# skips when no usable base resolves (shallow clone, no remote), the same
# tradeoff lint-baseline-growth.sh makes.
base_sha="${BASE_SHA:-}"
if [ -z "$base_sha" ] && git rev-parse -q --verify origin/main >/dev/null 2>&1; then
  base_sha="$(git merge-base origin/main HEAD 2>/dev/null || true)"
fi
if [ -n "$base_sha" ] && git cat-file -e "${base_sha}^{commit}" 2>/dev/null; then
  base_max=$(git ls-tree --name-only "${base_sha}:migrations" 2>/dev/null \
    | grep -E '^[0-9]+_.*\.up\.sql$' \
    | sed -E 's/^([0-9]+)_.*/\1/' | sed 's/^0*//' | sort -n | tail -1 || true)
  base_max="${base_max:-0}"
  # A rename onto a different number is a new migration as far as an applied
  # host can tell, so it is judged like an added file. A rename that keeps
  # its number is not.
  while IFS=$'\t' read -r st f to; do
    [ -n "$f" ] || continue
    if [ -n "$to" ]; then
      was="$(basename "$f" | sed -E 's/^([0-9]+)_.*/\1/' | sed 's/^0*//')"
      f="$to"
    else
      was=""
    fi
    n="$(basename "$f" | sed -E 's/^([0-9]+)_.*/\1/' | sed 's/^0*//')"
    [ -n "$n" ] || n=0
    if [ "${st#R}" != "$st" ]; then
      [ "$was" = "$n" ] && continue
      if [ "$n" -gt "$base_max" ]; then
        echo "lint-migrations ❌ ${f}: renumbered from ${was} — golang-migrate tracks the version number, so an applied host never runs the new one and a fresh database gets a different history (migrations/README.md)" >&2
        fail=1
        continue
      fi
    fi
    if [ "$n" -le "$base_max" ]; then
      echo "lint-migrations ❌ ${f}: new migration numbered ${n} does not exceed the comparison base's head (${base_max}) — a migration back-numbered into a gap is invisible to any deployment already past it (GH-1164)" >&2
      fail=1
    fi
  done < <(git diff --name-status -M100% --diff-filter=AR "${base_sha}...HEAD" -- 'migrations/*.up.sql' 2>/dev/null || true)
  # -M100% pairs only byte-identical renames: a rename that edits the body is a
  # delete plus an add here, so it cannot slip a changed migration past the
  # immutability manifest under a new file name.
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    echo "lint-migrations ❌ ${f}: a migration present at the comparison base is deleted — golang-migrate cannot advance a database whose version has no file (migrations/README.md)" >&2
    fail=1
  done < <(git diff --name-only -M100% --diff-filter=D "${base_sha}...HEAD" -- 'migrations/*.sql' 2>/dev/null || true)
  # git sees a move into a subdirectory or off the .up/.down suffix as a rename;
  # golang-migrate sees a version it no longer loads. Compare what it loads.
  mb="$(git merge-base "$base_sha" HEAD 2>/dev/null || true)"
  if [ -n "$mb" ]; then
    now_loaded="$(find migrations -maxdepth 1 -type f | sed 's#.*/##' | migrate_versions)"
    while IFS= read -r v; do
      [ -n "$v" ] || continue
      grep -qxF "$v" <<<"$now_loaded" && continue
      echo "lint-migrations ❌ migration ${v%.*} (${v##*.}) loads at the comparison base but not here — it was moved out of migrations/ or renamed off NNNN_name.(up|down).sql, so golang-migrate never runs it on a fresh database (migrations/README.md)" >&2
      fail=1
    done < <(git ls-tree --name-only "${mb}:migrations" 2>/dev/null | migrate_versions)
  fi
else
  echo "lint-migrations: no comparison base (BASE_SHA unset and origin/main unresolvable) — skipping the GH-1164 back-numbering guard." >&2
fi

# ── pass 3: register completeness ──────────────────────────────────
REGISTER="${REGISTER:-migrations/README.md}"
if [ ! -f "$REGISTER" ]; then
  echo "lint-migrations: $REGISTER is missing — the register cannot be checked." >&2
  fail=1
else
  # `|| true` is load-bearing: under `set -euo pipefail` a no-match grep exits 1
  # and kills the script mid-assignment, so the vacuity check below never runs.
  register_rows="$(grep -oE '^\| (0[0-9]{3}) \|' "$REGISTER" 2>/dev/null | grep -oE '0[0-9]{3}' | sort -u || true)"
  if [ -z "$register_rows" ]; then
    echo "lint-migrations: no register rows found in $REGISTER — the row shape changed" >&2
    echo "                 and this pass has gone vacuous. Fix the pattern, do not delete it." >&2
    fail=1
  fi
  missing=""
  for up in migrations/[0-9]*_*.up.sql; do
    [ -e "$up" ] || continue
    n="$(basename "$up" | cut -c1-4)"
    grep -qx "$n" <<<"$register_rows" || missing="${missing}${n} "
  done
  if [ -n "$missing" ]; then
    echo "lint-migrations: migration(s) with NO row in $REGISTER: ${missing}" >&2
    echo "                 Add one per migration (see the register's own mandate). The row is" >&2
    echo "                 where a fresh-database operator reads any '⚠ must re-materialize'" >&2
    echo "                 warning — 0147 leaves nine CAGGs empty and only the register says so." >&2
    fail=1
  fi
  orphans=""
  for n in $register_rows; do
    ls "migrations/${n}"_*.up.sql >/dev/null 2>&1 || orphans="${orphans}${n} "
  done
  if [ -n "$orphans" ]; then
    echo "lint-migrations: register row(s) naming a migration that does not exist: ${orphans}" >&2
    fail=1
  fi
  malformed="$(awk '
    /^[|] 0[0-9][0-9][0-9] [|] / {
      c = $0
      sub(/^[|] 0[0-9][0-9][0-9] [|] /, "", c)
      sub(/ [|][[:space:]]*$/, "", c)
      # A row may still carry a File cell before the description; descriptions hold no pipe.
      if (index(c, " | ")) c = substr(c, index(c, " | ") + 3)
      e = c; sub(/\**$/, "", e)
      if (e !~ /\.$/) printf "  %s (does not end in a full stop: ...%s)\n", substr($0, 3, 4), substr(c, length(c) - 39)
      else if (c !~ /^([A-Z0-9`]|\*\*)/) printf "  %s (does not start a sentence: %s...)\n", substr($0, 3, 4), substr(c, 1, 40)
    }' "$REGISTER")"
  if [ -n "$floor" ] && [ -n "$malformed" ]; then
    malformed="$(awk -v fl="$floor" '$1 + 0 > fl + 0' <<<"$malformed")"
  fi
  if [ -n "$malformed" ]; then
    echo "lint-migrations: register row(s) whose description is not a whole sentence:" >&2
    echo "$malformed" >&2
    echo "                 Write the full description from the migration's header." >&2
    fail=1
  fi
fi

# ── pass 4: ClickHouse money columns (ADR-0003) ────────────────────
# The lake DDL is money too, and ClickHouse has no NUMERIC. An on-chain amount is
# exact in Int64 (classic stroops are an XDR int64) or Int128/Int256 (Soroban
# i128/u256); a USD figure belongs in Decimal(38, n) or the decimal String the
# aggregator writes. Float32/Float64 are never a home for money: 53 bits of
# mantissa is the 2^53 cliff ADR-0003 forbids in JSON. Same name stems as pass 1.
# Flagged columns are keyed table.column (a name alone would exempt every later
# column of that name in the file).
#
# Escape hatch: the same inline `-- lint-money:ok <reason>` as pass 1.
#
# ch_float_baseline lists the Float64 money columns that predate this pass, keyed
# file:table.column, one per line of CH_FLOAT_BASELINE. Each is a documented
# display magnitude (its DDL header says so) mirrored between the operator file
# and tier1_schema.sql; the fix belongs with that DDL: retype, or carry the
# inline escape with its reason. An entry that no longer matches is stale and
# FAILS, so the list only shrinks. It lives in a *.baseline file because CI runs
# the base ref's copy of this script: an inline list could never shrink, as the
# base copy would call the entry a retype retires stale. Growth of the file is
# lint-baseline-growth.sh's to catch; a missing file is an empty baseline, the
# strictest reading.
CH_DIR="${CH_DIR:-deploy/clickhouse}"
CH_FLOAT_BASELINE="${CH_FLOAT_BASELINE:-scripts/ci/lint-migrations-ch-float.baseline}"
ch_float_baseline=''
if [ -f "$CH_FLOAT_BASELINE" ]; then
  ch_float_baseline="$(grep -vE '^[[:space:]]*(#|$)' "$CH_FLOAT_BASELINE" || true)"
fi

# ch_money_floats prints one `line<TAB>table.column<TAB>raw` per money column
# typed Float32/Float64 (Nullable or not) in $1. Table context comes from the
# enclosing CREATE TABLE / ALTER TABLE; a comment line or one carrying a reasoned
# lint-money:ok escape is skipped.
ch_money_floats() {
  awk -v name="$name" '
    {
      raw = $0; line = tolower($0)
      if (match(line, /create +(or +replace +)?table +(if +not +exists +)?[a-z0-9_.]+/) ||
          match(line, /alter +table +(if +exists +)?[a-z0-9_.]+/)) {
        n = split(substr(line, RSTART, RLENGTH), a, " "); tbl = a[n]
      }
      if (line ~ /^[ \t]*--/) next
      if (line ~ /-- *lint-money:ok +[^ ]/) next
      if (!match(line, "(^|[ \t(,])`?(" name ")`?[ \t]+(nullable\\()?float(32|64)([^a-z0-9_]|$)")) next
      col = substr(line, RSTART, RLENGTH)
      sub(/^[ \t(,]*`?/, "", col); sub(/`?[ \t].*$/, "", col)
      printf "%d\t%s.%s\t%s\n", NR, tbl, col, raw
    }' "$1"
}

ch_files=0
ch_seen=""
for f in "$CH_DIR"/*.sql; do
  [ -e "$f" ] || continue
  ch_files=$((ch_files + 1))
  b="$(basename "$f")"
  while IFS="$(printf '\t')" read -r ln key raw; do
    [ -n "$key" ] || continue
    if grep -qx "${b}:${key}" <<<"$ch_float_baseline"; then
      ch_seen="${ch_seen}${b}:${key}"$'\n'
      continue
    fi
    echo "lint-migrations ❌ ${f}:${ln}: money column ${key} is a float — ClickHouse money is Int64/Int128/Int256 (an exact on-chain integer) or Decimal, never Float32/Float64 (ADR-0003):" >&2
    echo "  ${raw}" >&2
    fail=1
  done < <(ch_money_floats "$f")
  stale=$(grep -nEi -- '-- *lint-money:ok' "$f" \
    | grep -vEi "(^|[[:space:](,])\`?(${name})\`?[[:space:]]+(Nullable\()?Float(32|64)\b" || true)
  if [ -n "$stale" ]; then
    echo "lint-migrations ❌ ${f}: stale lint-money:ok marker (line no longer matches the lint) — remove it:" >&2
    indent "$stale" >&2
    fail=1
  fi
done
if [ "$ch_files" -eq 0 ]; then
  echo "lint-migrations ❌ no *.sql under ${CH_DIR} — the ClickHouse money pass cannot pass vacuously" >&2
  fail=1
fi
while IFS= read -r entry; do
  [ -n "$entry" ] || continue
  # An entry for a file that is gone exempts nothing; only a present
  # file whose column no longer matches is a stale exemption.
  [ -e "${CH_DIR}/${entry%%:*}" ] || continue
  if ! grep -qx "$entry" <<<"$ch_seen"; then
    echo "lint-migrations ❌ stale ch_float_baseline entry ${entry} — the column is no longer a float money column; remove the entry (the list only shrinks)" >&2
    fail=1
  fi
done <<<"$ch_float_baseline"
echo "lint-migrations: ClickHouse money pass inspected ${ch_files} file(s) under ${CH_DIR}."

# ── pass 5: downs never delete rows silently ───────
# A down that narrows a CHECK must REFUSE while offending rows exist
# (`DO $$ … IF EXISTS … RAISE EXCEPTION … $$`, see 0070's down), not DELETE
# them: a rollback would otherwise discard production rows and report success.
# Any executable DELETE/TRUNCATE/drop_chunks in a down is a violation unless its
# line carries `-- lint-down-delete:ok <reason>` (e.g. removing only the seed row
# the matching up inserted). It is matched anywhere on the line outside quoted
# strings and comments: inside a one-line DO block, after a CTE's `(`, or as a
# DELETE whose FROM is on the next line. A character scanner blanks '…' / E'…'
# strings, "…" identifiers and -- / nested /* */ comments; dollar-quoted text
# is always scanned as code (a DO body is code), its own strings and comments
# ending at its closing tag. A raw line opening with DELETE FROM / TRUNCATE is
# a hit whatever the scanner made of it.
del_re='(^|[^a-z0-9_])(delete([[:space:]]+from([^a-z0-9_]|$)|[[:space:]]*$)|truncate([[:space:]]|;|$)|drop_chunks[[:space:]]*[(])'
down_files=0
for f in "${MIG_DIR:-migrations}"/*.down.sql; do
  [ -e "$f" ] || continue
  down_files=$((down_files + 1))
  above_floor "$f" || continue
  # HIT <n>:<line> for an unmarked delete, STALE <n>:<line> for a marker on a line without one.
  p5="$(awk -v re="$del_re" '
    function tagat(i,   t) { # the $tag$ opening at i, or ""
      if (substr(buf, i - 1, 1) ~ /[A-Za-z0-9_]/) return ""
      t = substr(buf, i + 1, 64)
      if (match(t, /^([A-Za-z_][A-Za-z0-9_]*)?[$]/)) return substr(buf, i, RLENGTH + 1)
      return ""
    }
    { raw[NR] = $0; buf = buf $0 "\n" }
    END {
      n = length(buf); ln = 1; out = ""; mode = ""; sp = 0
      for (i = 1; i <= n; i++) {
        c = substr(buf, i, 1); c2 = substr(buf, i, 2)
        if (c == "\n") { code[ln++] = out; out = ""; if (mode == "line") mode = ""; continue }
        if (c == "$" && sp && substr(buf, i, length(stk[sp])) == stk[sp]) {
          out = out stk[sp]; i += length(stk[sp]) - 1; sp--; mode = ""; continue
        }
        if (mode == "sq" || mode == "eq") {
          if (mode == "eq" && c == "\\") { if (substr(buf, i + 1, 1) != "\n") i++; continue }
          if (c2 == "'"''"'") { i++; continue }
          if (c == "'"'"'") { mode = ""; out = out " " }
          continue
        }
        if (mode == "dq") { if (c2 == "\"\"") i++; else if (c == "\"") { mode = ""; out = out " " }; continue }
        if (mode == "blk") {
          if (c2 == "/*") { bd++; i++ } else if (c2 == "*/") { i++; if (--bd == 0) { mode = ""; out = out " " } }
          continue
        }
        if (mode == "line") continue
        if (c == "'"'"'") {
          mode = (tolower(substr(buf, i - 1, 1)) == "e" && substr(buf, i - 2, 1) !~ /[A-Za-z0-9_$]/) ? "eq" : "sq"
          out = out " "; continue
        }
        if (c == "\"") { mode = "dq"; out = out " "; continue }
        if (c2 == "--") { mode = "line"; i++; continue }
        if (c2 == "/*") { mode = "blk"; bd = 1; i++; out = out " "; continue }
        if (c == "$" && (t = tagat(i)) != "") { stk[++sp] = t; out = out t; i += length(t) - 1; continue }
        out = out c
      }
      for (k = 1; k <= NR; k++) {
        r = tolower(raw[k]); ok = (r ~ /-- *lint-down-delete:ok/)
        del = (tolower(code[k]) ~ re) || (r ~ /^[[:space:]]*(delete[[:space:]]+from|truncate)([^a-z0-9_]|$)/)
        if (del && !(r ~ /-- *lint-down-delete:ok +[^ ]/)) print "HIT " k ":" raw[k]
        else if (ok && !del) print "STALE " k ":" raw[k]
      }
    }' "$f")"
  hits="$(sed -n 's/^HIT //p' <<<"$p5")"
  if [ -n "$hits" ]; then
    echo "lint-migrations ❌ ${f}: down deletes rows silently — refuse with a RAISE EXCEPTION guard instead (see 0070's down):" >&2
    indent "$hits" >&2
    fail=1
  fi
  stale="$(sed -n 's/^STALE //p' <<<"$p5")"
  if [ -n "$stale" ]; then
    echo "lint-migrations ❌ ${f}: stale lint-down-delete:ok marker (line is not a DELETE/TRUNCATE) — remove it:" >&2
    indent "$stale" >&2
    fail=1
  fi
done
# Vacuity is judged on the real tree; a test fixture (MIG_DIR) may carry no down.
if [ "$down_files" -eq 0 ] && [ -z "${MIG_DIR:-}" ]; then
  echo "lint-migrations ❌ no migrations/*.down.sql found — the down-delete pass cannot pass vacuously" >&2
  fail=1
fi

# ── Passes 6 and 7: operational hazards on populated tables ──
#
# MIG_DIR exists so scripts/ci/lint-migrations-test.sh can point these
# two passes at fixture trees; the passes above always read migrations/.
MIG_DIR="${MIG_DIR:-migrations}"

# sql_stmts <file>: one SQL statement per line, `--` and (nested) `/* */`
# comments stripped and whitespace collapsed, so a statement split across lines
# matches. One lexer pass: a comment marker or `;` inside a '…' / E'…' string
# or "…" identifier is content. A $$ body is still split at `;`, which is how
# blanking_stmts reads the statements of a DO block.
sql_stmts() {
  LC_ALL=C awk '
    function emit(s) { gsub(/[\n\t\r]/, " ", s); gsub(/ +/, " ", s); sub(/^ /, "", s); if (s != "") print s }
    { src = src $0 "\n" }
    END {
      n = length(src); st = ""; q = ""; depth = 0; i = 1
      while (i <= n) {
        c = substr(src, i, 1); c2 = substr(src, i, 2)
        if (depth > 0) {
          if (c2 == "*/") { depth--; i += 2; if (depth == 0) st = st " "; continue }
          if (c2 == "/*") { depth++; i += 2; continue }
          i++; continue
        }
        if (q != "") {
          st = st c
          if (q == "e" && c == "\\") { st = st substr(src, i + 1, 1); i += 2; continue }
          # An E-string closes on a quote only; "c == q" would close it at its first letter e.
          if (q == "e") { if (c == "\047") q = "" } else if (c == q) q = ""
          i++; continue
        }
        if (c2 == "--") { j = index(substr(src, i), "\n"); i = j ? i + j - 1 : n + 1; continue }
        if (c2 == "/*") { depth = 1; i += 2; continue }
        if (c == ";") { emit(st); st = ""; i++; continue }
        if (c == "\047") q = (substr(src, i - 1, 1) ~ /[eE]/ && substr(src, i - 2, 1) !~ /[A-Za-z0-9_]/) ? "e" : c
        else if (c == "\"") q = c
        st = st c; i++
      }
      emit(st)
    }' "$1"
}

# cagg_views <file>: when the file creates anything WITH NO DATA, every view it
# drops or creates WITH NO DATA, one bare lower-case name per line. Both leave
# the view empty until refreshed, so passes 7 and 10 key on this one list.
cagg_views() {
  sql_stmts "$1" | awk '
    function add(list,   n, p, i, t) {
      n = split(list, p, ",")
      for (i = 1; i <= n; i++) {
        t = p[i]; sub(/^ +/, "", t); sub(/ .*/, "", t); gsub(/"/, "", t); sub(/^public\./, "", t)
        if (t != "") v[t] = 1
      }
    }
    { s = tolower($0) }
    s ~ /with no data/ { nodata = 1 }
    s ~ /^drop materialized view / { sub(/^drop materialized view (if exists )?/, "", s); add(s) }
    s ~ /^create materialized view / && s ~ /with no data/ { sub(/^create materialized view (if not exists )?/, "", s); sub(/ .*/, "", s); add(s) }
    END { if (nodata) for (t in v) print t }' | sort -u
}

# hyper_indexes <file>: `<idx>|<table>|<if-not-exists 0/1>|<lock_timeout 0/1>`
# for every in-transaction CREATE INDEX on a table the file does not
# itself create (an index on a table born in the same file is free).
hyper_indexes() {
  sql_stmts "$1" | awk '
    { s = tolower($0); n = split(s, w, " ") }
    s ~ /^create (unlogged )?table / {
      t = (w[3] == "if") ? w[6] : w[3]
      if (w[3] == "unlogged") t = (w[4] == "if") ? w[7] : w[4]
      sub(/\(.*/, "", t); sub(/^public\./, "", t); here[t] = 1
    }
    # 0 (or DEFAULT, which is 0 unless the server overrides it) waits forever: no timeout.
    s ~ /^set (local )?lock_timeout/ {
      v = s; sub(/^set (local )?lock_timeout( to | ?= ?)/, "", v); gsub(/[\047" ]/, "", v)
      lt = (v ~ /^0+(us|ms|s|min|h|d)?$/ || v == "default") ? 0 : 1
    }
    s ~ /^reset (lock_timeout|all)/ { lt = 0 }
    # The timeout that guards a build is the one in force at it, not the file-final one.
    s ~ /^create (unique )?index / && s !~ /^create (unique )?index concurrently/ {
      j = (w[2] == "unique") ? 4 : 3; ine = (w[j] == "if"); if (ine) j += 3
      tbl = ""
      for (i = j; i <= n; i++) if (w[i] == "on") { tbl = (w[i+1] == "only") ? w[i+2] : w[i+1]; break }
      gsub(/"/, "", tbl); sub(/\(.*/, "", tbl); sub(/^public\./, "", tbl)
      k++; idx[k] = w[j]; on[k] = tbl; ok[k] = ine; kind[k] = "index"; ltat[k] = lt + 0
    }
    # ADD PRIMARY KEY / UNIQUE builds the same index under ACCESS EXCLUSIVE and
    # has no IF NOT EXISTS form, so only the lock_timeout is asked of it.
    s ~ /^alter table / && s ~ / add (constraint [^ ]+ )?(primary key|unique)[ (]/ && s !~ / using index / {
      tbl = s; sub(/^alter table (if exists )?(only )?/, "", tbl); sub(/ .*/, "", tbl); gsub(/"/, "", tbl); sub(/^public\./, "", tbl)
      c = "primary-key-or-unique"
      if (match(s, / add constraint [^ ]+/)) c = substr(s, RSTART + 16, RLENGTH - 16)
      k++; idx[k] = c; on[k] = tbl; ok[k] = 1; kind[k] = "constraint"; ltat[k] = lt + 0
    }
    END { for (i = 1; i <= k; i++) if (!(on[i] in here)) printf "%s|%s|%d|%d|%s\n", idx[i], on[i], ok[i], ltat[i], kind[i] }'
}

# Pass 6 — CREATE INDEX on an existing hypertable. The in-transaction build
# holds a SHARE lock that blocks every write for the whole build, and a partial
# index still scans every row. 0037's header is the recipe: `IF NOT EXISTS`, so
# an operator's per-chunk pre-build (timescaledb.transaction_per_chunk;
# hypertables reject CREATE INDEX CONCURRENTLY, see 0123) turns the migration
# into a no-op, and `SET LOCAL lock_timeout`, so the build cannot queue every
# writer behind an open transaction. Shipped migrations are immutable, so those
# predating this pass are listed below (0150's operator note is in the README
# register); an entry that no longer matches is stale and fails.
hyper_index_baseline='0025_create_routers_and_attribution.up.sql:trades_routed_via_idx
0030_asset_supply_history_unique_constraint.up.sql:asset_supply_history_asset_ledger_idx
0053_blend_pk_granularity.up.sql:blend_positions_pkey
0053_blend_pk_granularity.up.sql:blend_emissions_pkey
0053_blend_pk_granularity.up.sql:blend_admin_pkey
0054_blend_positions_event_index.up.sql:blend_positions_pkey
0055_defindex_flows_event_index.up.sql:defindex_flows_pkey
0056_soroswap_router_swaps_call_sig.up.sql:soroswap_router_swaps_pkey
0057_sep41_supply_events_event_index.up.sql:sep41_supply_events_pkey
0058_blend_auctions_event_index.up.sql:blend_auctions_pkey
0059_comet_liquidity_event_index.up.sql:comet_liquidity_pkey
0060_phoenix_event_index.up.sql:phoenix_liquidity_pkey
0060_phoenix_event_index.up.sql:phoenix_stake_events_pkey
0112_cctp_rozo_event_index.up.sql:cctp_events_pkey
0112_cctp_rozo_event_index.up.sql:rozo_events_pkey
0037_trades_pair_source_ts_index.up.sql:trades_pair_source_ts_idx
0083_sep41_transfers_ledger_idx.up.sql:sep41_transfers_ledger_idx
0091_aquarius_liquidity_pool_tokens_idx.up.sql:aquarius_liquidity_pool_token_idx
0101_soroswap_router_swaps_call_path.up.sql:soroswap_router_swaps_call_kind_ts_idx
0106_sep41_transfers_address_idx.up.sql:sep41_transfers_from_addr_ledger_idx
0106_sep41_transfers_address_idx.up.sql:sep41_transfers_to_addr_ledger_idx
0107_positions_view_user_indexes.up.sql:blend_backstop_events_user_ts_idx
0107_positions_view_user_indexes.up.sql:blend_positions_user_ts_idx
0123_trades_account_ts_indexes.up.sql:trades_maker_ts_idx
0123_trades_account_ts_indexes.up.sql:trades_taker_ts_idx
0150_add_trades_signer.up.sql:trades_signer_idx'

hypertables="$(for f in "$MIG_DIR"/*.up.sql; do grep -qi create_hypertable "$f" && sql_stmts "$f"; done \
  | grep -oiE "create_hypertable\( *'[a-z0-9_.]+'" \
  | sed -E "s/.*'([A-Za-z0-9_.]+)'/\1/; s/^public\.//" | tr '[:upper:]' '[:lower:]' | sort -u || true)"
if [ -z "$hypertables" ]; then
  echo "lint-migrations ❌ no create_hypertable() call found under ${MIG_DIR} — the hypertable-index pass has gone vacuous" >&2
  fail=1
fi
hi_seen=""
hi_checked=0
for f in "$MIG_DIR"/*.up.sql; do
  above_floor "$f" || continue
  stmts="$(sql_stmts "$f")"
  grep -qiE 'create (unique )?index|add (constraint|primary|unique)' <<<"$stmts" || continue
  b="$(basename "$f")"
  while IFS='|' read -r idx tbl ine lt kind; do
    [ -n "$idx" ] || continue
    grep -qx "$tbl" <<<"$hypertables" || continue
    hi_checked=$((hi_checked + 1))
    [ "$ine" = 1 ] && [ "$lt" = 1 ] && continue
    if grep -qx "${b}:${idx}" <<<"$hyper_index_baseline"; then
      hi_seen="${hi_seen}${b}:${idx}"$'\n'
      continue
    fi
    if [ "$kind" = constraint ]; then
      echo "lint-migrations ❌ ${f}: ADD CONSTRAINT ${idx} (PRIMARY KEY/UNIQUE) ON ${tbl} lacks SET LOCAL lock_timeout — ${tbl} is an existing hypertable, and the constraint builds a unique index on every chunk under ACCESS EXCLUSIVE, blocking reads and writes for the whole build. Set SET LOCAL lock_timeout so it cannot queue every session behind an open transaction, and name the maintenance window in the header and the README register row." >&2
      fail=1
      continue
    fi
    missing=""
    [ "$ine" = 1 ] || missing="IF NOT EXISTS"
    [ "$lt" = 1 ] || missing="${missing:+${missing} and }SET LOCAL lock_timeout"
    echo "lint-migrations ❌ ${f}: CREATE INDEX ${idx} ON ${tbl} lacks ${missing} — ${tbl} is an existing hypertable, and the in-transaction build blocks every write to it for the whole build. Write CREATE INDEX IF NOT EXISTS (so a per-chunk pre-build via timescaledb.transaction_per_chunk makes the migration a no-op; hypertables reject CONCURRENTLY) under SET LOCAL lock_timeout, and name the pre-build in the header and the README register row (0037 is the recipe)." >&2
    fail=1
  done < <(hyper_indexes "$f")
done
while IFS= read -r entry; do
  [ -n "$entry" ] || continue
  [ -e "${MIG_DIR}/${entry%%:*}" ] || continue
  above_floor "$entry" || continue
  if ! grep -qx "$entry" <<<"$hi_seen"; then
    echo "lint-migrations ❌ stale hyper_index_baseline entry ${entry} — it no longer names a non-conforming hypertable index; remove it (the list only shrinks)" >&2
    fail=1
  fi
done <<<"$hyper_index_baseline"
echo "lint-migrations: hypertable-index pass checked ${hi_checked} index build(s) on existing hypertables under ${MIG_DIR}."

# Pass 7 — a continuous aggregate dropped and recreated WITH NO DATA is EMPTY
# until refreshed, and a migration cannot do that: refresh_continuous_aggregate
# refuses a transaction block, and golang-migrate runs each file as one. So the
# file must NAME the refresh (`refresh_continuous_aggregate('<view>'`, in its
# operator header) for every view it recreates, a down included (a rollback
# empties the views as the up did). A view created WITH NO DATA under a new name
# is just as empty; shipped files that create one without naming its refresh:
cagg_refresh_baseline='0002_create_price_aggregates.up.sql
0034_oracle_price_aggregates.up.sql
0036_create_pools_per_source_cagg.up.sql
0217_trade_sums_1m.up.sql'
cr_seen=""
cagg_files=0
for f in "$MIG_DIR"/*.sql; do
  above_floor "$f" || continue
  stmts="$(sql_stmts "$f")"
  grep -qi 'with no data' <<<"$stmts" || continue
  cagg_files=$((cagg_files + 1))
  named="$(grep -oE "refresh_continuous_aggregate\( *'[a-z0-9_]+'" "$f" | sed -E "s/.*'([a-z0-9_]+)'/\1/" | sort -u || true)"
  unnamed=""
  for v in $(cagg_views "$f"); do
    grep -qx "$v" <<<"$named" || unnamed="${unnamed}${v} "
  done
  if [ -n "$unnamed" ] && grep -qx "$(basename "$f")" <<<"$cagg_refresh_baseline"; then
    cr_seen="${cr_seen}$(basename "$f")"$'\n'
  elif [ -n "$unnamed" ]; then
    echo "lint-migrations ❌ ${f}: recreates continuous aggregate(s) WITH NO DATA and names no refresh for: ${unnamed}— they serve nothing until re-materialized. Write the ordered CALL refresh_continuous_aggregate('<view>', …) sequence into the header (hierarchical views after their parent is whole) and the README register row." >&2
    fail=1
  fi
done
while IFS= read -r entry; do
  [ -n "$entry" ] || continue
  [ -e "${MIG_DIR}/${entry}" ] || continue
  above_floor "$entry" || continue
  if ! grep -qx "$entry" <<<"$cr_seen"; then
    echo "lint-migrations ❌ stale cagg_refresh_baseline entry ${entry} — it now names a refresh for every view it leaves empty; remove it (the list only shrinks)" >&2
    fail=1
  fi
done <<<"$cagg_refresh_baseline"
echo "lint-migrations: CAGG re-materialization pass inspected ${cagg_files} WITH NO DATA file(s) under ${MIG_DIR}."

# ── pass 8: atomicity ───────────────────────────────────────────────
# golang-migrate sends a whole file as ONE simple-protocol query, which Postgres
# runs as one implicit transaction. An explicit COMMIT/ROLLBACK ends it, so every
# later statement runs in a fresh transaction: a failure there leaves the earlier
# half durable and the version dirty. BEGIN; … COMMIT; around the whole body is
# fine; anything but comments after the COMMIT is not. END [WORK], COMMIT AND
# CHAIN and a COMMIT sharing a line with another statement count the same. TXN_DIR is the fixture
# seam for scripts/ci/lint-migrations-test.sh.
#
# 0030's up is the one shipped instance; its body is immutable, so it is
# exempted and 0168 restores what a failed tail would lose.
TXN_DIR="${TXN_DIR:-migrations}"
txn_exempt="0030_asset_supply_history_unique_constraint.up.sql"
txn_files=0
for f in "$TXN_DIR"/*.sql; do
  [ -e "$f" ] || continue
  txn_files=$((txn_files + 1))
  above_floor "$f" || continue
  [ "$(basename "$f")" = "$txn_exempt" ] && continue
  after="$(awk '
    function endstmt(    u) {
      u = toupper(st); gsub(/^[ \t]+|[ \t]+$/, "", u); st = ""
      if (u == "") return
      if (ended) { print first ": " firstraw; done = 1; exit }
      if (u ~ /^(COMMIT|ROLLBACK|ABORT|END)([ \t]+(WORK|TRANSACTION))?([ \t]+AND[ \t]+(NO[ \t]+)?CHAIN)?$/) ended = 1
    }
    { line = $0; n = length(line); i = 1
      while (i <= n) {
        c = substr(line, i, 1)
        if (dq != "") {
          if (c == "$" && match(substr(line, i), /^\$([A-Za-z_][A-Za-z0-9_]*)?\$/) && substr(line, i, RLENGTH) == dq) { i += RLENGTH; dq = ""; continue }
          i++; continue
        }
        if (sq) { if (c == "\047") sq = 0; i++; continue }
        if (c == "-" && substr(line, i, 2) == "--") break
        if (c == ";") { endstmt(); i++; continue }
        if (c !~ /[ \t]/ && st == "") { first = FNR; firstraw = line }
        if (c == "\047") sq = 1
        if (c == "$" && match(substr(line, i), /^\$([A-Za-z_][A-Za-z0-9_]*)?\$/)) { dq = substr(line, i, RLENGTH); st = st "$Q"; i += RLENGTH; continue }
        st = st c; i++
      }
    }
    END { if (!done) endstmt() }
  ' "$f")"
  if [ -n "$after" ]; then
    echo "lint-migrations ❌ ${f}:${after%%:*}: SQL after the file's COMMIT/ROLLBACK runs in a separate transaction — a failure there leaves the migration half-applied. Keep every statement inside the one transaction." >&2
    fail=1
  fi
done
if [ "$txn_files" -eq 0 ]; then
  echo "lint-migrations ❌ no *.sql under ${TXN_DIR} — the atomicity pass cannot pass vacuously" >&2
  fail=1
fi
echo "lint-migrations: atomicity pass inspected ${txn_files} file(s) under ${TXN_DIR}."

# ── pass 9: priceable leg division ──────────────────────────────────
# trades admits one zero leg (an SDEX rounding fill) with no CHECK, so a
# statement dividing by base_amount/quote_amount without the priceable filter
# `base_amount > 0 AND quote_amount > 0` either raises division_by_zero
# (aborting a cagg refresh) or publishes a zero price.
# Escape: `-- lint-priceable:ok <reason>` on any line of the statement; a marker
# on a statement with no leg division is stale and fails.
# Shipped files are immutable, so the pre-admission ones are listed below (0187
# injects its filter through format(), which this pass cannot see); an entry with
# no unguarded division fails.
priceable_baseline='0002_create_price_aggregates.up.sql
0036_create_pools_per_source_cagg.up.sql
0115_ohlc_extremes_notional_floor.up.sql
0147_ohlc_deterministic_tiebreak.up.sql
0166_twap_notional_floor.up.sql
0187_price_caggs_priceable_filter.up.sql'

# priceable_hits <file>: `U <line>` per unguarded, unescaped leg division
# and `S <line>` per stale escape marker; <line> is the statement's first.
priceable_hits() {
  awk '
    function check(   s) {
      s = stmt; gsub(/[ \t]+/, " ", s)
      div = (s ~ /\/ *(([a-z_]+ *)?\( *)*([a-z_]+\.)?(base_amount|quote_amount)([^a-z0-9_]|$)/)
      guard = (s ~ /([a-z_]+\.)?base_amount *> *0 and ([a-z_]+\.)?quote_amount *> *0/ ||
               s ~ /([a-z_]+\.)?quote_amount *> *0 and ([a-z_]+\.)?base_amount *> *0/)
      if (div && !guard && !esc) print "U " start
      if (esc && !div) print "S " escline
      stmt = ""; esc = 0; start = 0
    }
    {
      line = $0
      if (line ~ /--[ \t]*lint-priceable:ok[ \t]+[^ \t]/) { esc = 1; escline = FNR }
      sub(/--.*$/, "", line); line = tolower(line)
      while ((i = index(line, ";")) > 0) {
        if (start == 0) start = FNR
        stmt = stmt " " substr(line, 1, i - 1); check(); line = substr(line, i + 1)
      }
      if (line ~ /[^ \t]/ && start == 0) start = FNR
      stmt = stmt " " line
    }
    END { if (stmt ~ /[^ ]/ || esc) check() }' "$1"
}

pr_seen=""
pr_files=0
for f in "$MIG_DIR"/*.up.sql; do
  [ -e "$f" ] || continue
  pr_files=$((pr_files + 1))
  above_floor "$f" || continue
  b="$(basename "$f")"
  hits="$(priceable_hits "$f")"
  while read -r kind ln; do
    [ -n "$kind" ] || continue
    if [ "$kind" = S ]; then
      echo "lint-migrations ❌ ${f}:${ln}: stale lint-priceable:ok marker (its statement divides by no leg) — remove it" >&2
      fail=1
    elif grep -qx "$b" <<<"$priceable_baseline"; then
      pr_seen="${pr_seen}${b}"$'\n'
    else
      echo "lint-migrations ❌ ${f}:${ln}: divides by base_amount/quote_amount without the priceable filter — trades holds zero-leg rows, so add \`base_amount > 0 AND quote_amount > 0\` to the statement's WHERE or FILTER, or mark it \`-- lint-priceable:ok <reason>\`" >&2
      fail=1
    fi
  done <<<"$hits"
done
while IFS= read -r entry; do
  [ -n "$entry" ] || continue
  [ -e "${MIG_DIR}/${entry}" ] || continue
  above_floor "$entry" || continue
  if ! grep -qx "$entry" <<<"$pr_seen"; then
    echo "lint-migrations ❌ stale priceable_baseline entry ${entry} — it no longer holds an unguarded leg division; remove it (the list only shrinks)" >&2
    fail=1
  fi
done <<<"$priceable_baseline"
if [ "$pr_files" -eq 0 ]; then
  echo "lint-migrations ❌ no *.up.sql under ${MIG_DIR} — the priceable-division pass cannot pass vacuously" >&2
  fail=1
fi
echo "lint-migrations: priceable-division pass inspected ${pr_files} file(s) under ${MIG_DIR}."

# ── pass 10: required follow-up marker ──────────────────────────────
# An up.sql that blanks data (an unqualified DELETE, a TRUNCATE, or a continuous
# aggregate dropped and recreated WITH NO DATA) serves empty until an operator
# runs the rebuild the migration cannot run itself. deploy.yml's
# scripts/ci/migration-followup-gate.sh lists every
# `-- REQUIRED-FOLLOWUP: <command>` line in the migrations a deploy adds and
# refuses to start until they are acknowledged, so such a file must carry at
# least one. A marker in any other spelling, or with no command, is invisible to
# that gate and fails here. Shipped files predating the marker are listed below;
# the list only shrinks.
followup_baseline='0002_create_price_aggregates.up.sql
0034_oracle_price_aggregates.up.sql
0036_create_pools_per_source_cagg.up.sql
0064_create_dex_volume_cagg.up.sql
0066_create_supply_cagg.up.sql
0068_create_source_volume_cagg.up.sql
0081_create_twap_aggregates.up.sql
0115_ohlc_extremes_notional_floor.up.sql
0126_twap_sample_count.up.sql
0137_disarm_comet_replay_doublecount.up.sql
0147_ohlc_deterministic_tiebreak.up.sql
0164_disarm_cctp_rozo_replay_doublecount.up.sql
0166_twap_notional_floor.up.sql
0187_price_caggs_priceable_filter.up.sql
0217_trade_sums_1m.up.sql'

# A follow-up that rebuilds a projected source (projector-replay or
# projected-rebuild -source X) must sit in a file that also deletes X's
# projected-rebuild checkpoints (ingestion_cursors, source = 'projected-rebuild',
# sub_source LIKE 'X:%'). projected-rebuild -resume skips every checkpointed
# window without looking at the table, so a checkpoint written before the
# migration keeps those windows empty (0137, 0164, 0203). Shipped files that
# predate the rule:
rebuild_ckpt_baseline='0203_create_sushiswap_v3_position_events.up.sql'

# rebuild_ckpt_missing <file>: each source a rebuild follow-up names whose
# projected-rebuild checkpoints the file does not delete, one per line.
rebuild_ckpt_missing() {
  local srcs cleared src
  srcs="$(grep -E '^-- REQUIRED-FOLLOWUP: [^[:space:]]' "$1" \
    | grep -E 'projector-replay|projected-rebuild' \
    | sed -nE 's/.*[[:space:]]--?source[[:space:]=]+([^[:space:]]+).*/\1/p' \
    | tr ',A-Z' '\na-z' | sort -u || true)"
  [ -n "$srcs" ] || return 0
  cleared="$(sql_stmts "$1" | awk '
    { s = tolower($0) }
    (s ~ /^delete from ingestion_cursors / || s ~ / begin delete from ingestion_cursors /) \
      && s ~ /[^_]source ?= ?\047projected-rebuild\047/ {
      while (match(s, /like \047[^\047:]+:%\047/)) {
        print substr(s, RSTART + 6, RLENGTH - 9); s = substr(s, RSTART + RLENGTH)
      }
    }')"
  while IFS= read -r src; do
    [ -n "$src" ] || continue
    grep -qxF "$src" <<<"$cleared" || echo "$src"
  done <<<"$srcs"
}

# blanking_stmts <file>: `<relation>\t<statement>` for each relation the file
# blanks: TRUNCATE (every name in its list), DELETE with no WHERE or WHERE true,
# a table dropped and recreated with no INSERT or CREATE TABLE … AS refilling it,
# and cagg_views. A ranged delete (DELETE … WHERE, drop_chunks) is a repair, not a blank; the compat
# lint's dml class guards those.
# Inside a DO block the verb follows BEGIN/THEN/ELSE/LOOP (a DECLARE's `;`
# starts a new statement there). A CREATE FUNCTION body runs when called, not
# at migrate time, so it is skipped.
blanking_stmts() {
  sql_stmts "$1" | awk '
    function rel(t) { sub(/^ +/, "", t); sub(/^only /, "", t); sub(/[ (].*/, "", t); gsub(/"/, "", t); sub(/^.*\./, "", t); return t }
    function out(t, st) { t = rel(t); if (t != "") printf "%s\t%s\n", t, st }
    # ctas(<rest after CREATE TABLE>): true for CREATE TABLE <name> [(cols)] [WITH (…)] AS <query>.
    function ctas(r,   i, d, c) {
      sub(/^[^ (]+ ?/, "", r)
      if (r ~ /^\(/) {
        for (i = 1; i <= length(r); i++) { c = substr(r, i, 1); if (c == "(") d++; else if (c == ")" && --d == 0) break }
        r = substr(r, i + 1); sub(/^ /, "", r)
      }
      return r ~ /^(with \([^)]*\) )?as /
    }
    { s = tolower($0) }
    infn { if (gsub(/\$\$/, "&", s) % 2) infn = 0; next }
    s ~ /^create (or replace )?function / { if (gsub(/\$\$/, "&", s) % 2) infn = 1; next }
    {
      st = ""
      if (s ~ /^(truncate|delete from) /) st = s
      else if (match(s, /(^| )(begin|then|else|loop) (truncate|delete from) /)) { st = substr(s, RSTART); sub(/^ ?[a-z]+ /, "", st) }
      if (st ~ /^truncate /) { l = st; sub(/^truncate (table )?/, "", l); n = split(l, p, ","); for (i = 1; i <= n; i++) out(p[i], st); next }
      if (st ~ /^delete from / && (st !~ / where / || st ~ / where \(?true\)?( returning .*)? ?$/)) { l = st; sub(/^delete from /, "", l); out(l, st); next }
      # Any other position (a data-modifying CTE, a subquery): the verb ends
      # at its closing paren. String contents are blanked first.
      b = (st == "") ? s : ""; gsub(/\047[^\047]*\047/, "\047\047", b)
      while (match(b, /[ (](truncate|delete from) /)) {
        st = substr(b, RSTART + 1); b = substr(b, RSTART + RLENGTH)
        d = 0
        for (k = 1; k <= length(st); k++) { c = substr(st, k, 1); if (c == "(") d++; else if (c == ")" && --d < 0) break }
        st = substr(st, 1, k - 1)
        if (st ~ /^truncate on /) continue
        if (st ~ /^truncate /) { l = st; sub(/^truncate (table )?/, "", l); n = split(l, p, ","); for (i = 1; i <= n; i++) out(p[i], st); continue }
        if (st !~ / where / || st ~ / where \(?true\)?( returning .*)? ?$/) { l = st; sub(/^delete from /, "", l); out(l, st) }
      }
    }
    s ~ /^drop table / { l = s; sub(/^drop table (if exists )?/, "", l); n = split(l, p, ","); for (i = 1; i <= n; i++) gone[rel(p[i])] = 1; next }
    s ~ /^create (unlogged )?table / {
      l = s; sub(/^create (unlogged )?table (if not exists )?/, "", l); t = rel(l)
      if (ctas(l) && s !~ / with no data ?$/) { filled[t] = 1; next }
      if (t in gone) made[t] = s; next
    }
    s ~ /insert into / { l = s; sub(/.*insert into /, "", l); filled[rel(l)] = 1 }
    END { for (t in made) if (!(t in filled)) printf "%s\tdrop table %s, then %s\n", t, t, made[t] }'
  cagg_views "$1" | awk '{ printf "%s\tmaterialized view %s left empty (with no data)\n", $0, $0 }'
}

# uncovered_followup_targets <file> <blanks>: each relation the file blanks
# that no REQUIRED-FOLLOWUP line names, either whole or minus its last `_word`
# (cctp_events -> cctp, the `-source` a replay takes).
uncovered_followup_targets() {
  local markers name stem
  markers="$(grep -E '^-- REQUIRED-FOLLOWUP: [^[:space:]]' "$1")"
  cut -f1 <<<"$2" | sort -u | while IFS= read -r name; do
    [ -n "$name" ] || continue
    stem="${name%_*}"
    grep -qiE "(^|[^a-z0-9_])(${name}|${stem})([^a-z0-9_]|\$)" <<<"$markers" || echo "$name"
  done
}

fu_seen=""
rc_seen=""
fu_blanking=0
for f in "$MIG_DIR"/*.up.sql; do
  [ -e "$f" ] || continue
  above_floor "$f" || continue
  b="$(basename "$f")"
  bad_marker="$(grep -niE '^--[[:space:]]*required[-_ ]?follow[-_ ]?up[[:space:]]*:' "$f" \
    | grep -vE '^[0-9]+:-- REQUIRED-FOLLOWUP: [^[:space:]]' || true)"
  if [ -n "$bad_marker" ]; then
    echo "lint-migrations ❌ ${f}: malformed follow-up marker — write exactly \`-- REQUIRED-FOLLOWUP: <command>\` or the deploy gate never sees it:" >&2
    indent "$bad_marker" >&2
    fail=1
  fi
  ckpt_missing="$(rebuild_ckpt_missing "$f")"
  if [ -n "$ckpt_missing" ]; then
    if grep -qx "$b" <<<"$rebuild_ckpt_baseline"; then
      rc_seen="${rc_seen}${b}"$'\n'
    else
      echo "lint-migrations ❌ ${f}: a REQUIRED-FOLLOWUP rebuilds these sources but the file does not delete their projected-rebuild checkpoints — projected-rebuild -resume would skip every window checkpointed before this migration. Add DELETE FROM ingestion_cursors WHERE source = 'projected-rebuild' AND sub_source LIKE '<source>:%' (see 0206):" >&2
      indent "$ckpt_missing" >&2
      fail=1
    fi
  fi
  blanks="$(blanking_stmts "$f")"
  [ -n "$blanks" ] || continue
  fu_blanking=$((fu_blanking + 1))
  if grep -qE '^-- REQUIRED-FOLLOWUP: [^[:space:]]' "$f"; then
    unnamed="$(uncovered_followup_targets "$f" "$blanks")"
    if [ -n "$unnamed" ]; then
      echo "lint-migrations ❌ ${f}: blanks relations no \`-- REQUIRED-FOLLOWUP:\` line names — the deploy gate shows the operator only the marker lines, so the rebuild of each blanked table or view must be written there (migrations/README.md rule 12):" >&2
      indent "$unnamed" >&2
      fail=1
    fi
    continue
  fi
  if grep -qx "$b" <<<"$followup_baseline"; then
    fu_seen="${fu_seen}${b}"$'\n'
    continue
  fi
  echo "lint-migrations ❌ ${f}: blanks data and declares no \`-- REQUIRED-FOLLOWUP: <command>\` header line — the data serves empty until someone rebuilds it, and the deploy gate only asks for the rebuilds a migration names. Add one line per replay / refresh command (migrations/README.md rule 12). Blanking statement(s):" >&2
  indent "$(cut -f2 <<<"$blanks" | sort -u | cut -c1-120)" >&2
  fail=1
done
while IFS= read -r entry; do
  [ -n "$entry" ] || continue
  [ -e "${MIG_DIR}/${entry}" ] || continue
  above_floor "$entry" || continue
  if ! grep -qx "$entry" <<<"$fu_seen"; then
    echo "lint-migrations ❌ stale followup_baseline entry ${entry} — it no longer blanks data without a marker; remove it (the list only shrinks)" >&2
    fail=1
  fi
done <<<"$followup_baseline"
while IFS= read -r entry; do
  [ -n "$entry" ] || continue
  [ -e "${MIG_DIR}/${entry}" ] || continue
  above_floor "$entry" || continue
  if ! grep -qx "$entry" <<<"$rc_seen"; then
    echo "lint-migrations ❌ stale rebuild_ckpt_baseline entry ${entry} — its rebuild follow-ups now clear their checkpoints; remove it (the list only shrinks)" >&2
    fail=1
  fi
done <<<"$rebuild_ckpt_baseline"
echo "lint-migrations: follow-up marker pass found ${fu_blanking} data-blanking file(s) under ${MIG_DIR}."

if [ "$fail" -eq 0 ]; then
  echo "✅ migration lint passed (money-column + pairing/numbering/non-empty + register + ClickHouse money + down-delete, ${down_files} downs + hypertable index + CAGG re-materialization + atomicity + priceable division + follow-up marker)."
fi
exit "$fail"
