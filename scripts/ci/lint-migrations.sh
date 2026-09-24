#!/usr/bin/env bash
# Migration lint: money-column (ADR-0003) + file integrity (audit C4-7)
# + register completeness (wave-D PS-01) + ClickHouse money-column
# + hypertable index builds + CAGG re-materialization.
#
# Eight passes, all gating (exit non-zero on any violation):
#   1. money-column — monetary columns must be NUMERIC (ADR-0003).
#   2. file integrity — every NNNN_*.up.sql has a matching NON-EMPTY
#      *.down.sql (and no orphan downs), no duplicate NNNN prefixes, and
#      no empty files. Numbering GAPS are a non-fatal WARNING (this repo
#      legitimately skips numbers when a migration is squashed/removed).
#   3. register completeness — every NNNN_*.up.sql has a row in
#      migrations/README.md's register, and no row names a file that does
#      not exist.
#   4. ClickHouse money-column — the lake DDL under deploy/clickhouse/
#      must never hold a monetary column in Float32/Float64 (ADR-0003
#      applied to the substrate; see the pass for the type rule).
#   5. down-delete — no *.down.sql DELETEs/TRUNCATEs rows silently; a
#      narrowing down refuses with a RAISE EXCEPTION guard instead.
#   6. hypertable index — a CREATE INDEX on an existing hypertable uses
#      IF NOT EXISTS under SET LOCAL lock_timeout (see the pass).
#   7. CAGG re-materialization — a file that recreates a continuous
#      aggregate WITH NO DATA names its refresh (see the pass).
#   8. atomicity — no SQL after a file's first COMMIT/ROLLBACK (see the
#      pass).
#
# ── register-completeness detail ──
#
# migrations/README.md mandates a register row per migration, and the
# register had drifted: 0138-0143, 0145-0147, 0149 and 0150 were all
# missing when this pass was added. The reader who pays for that is the
# one the register exists for — someone bringing up a FRESH database
# (docs/operations/archival-node-bringup.md), for whom the row is where
# an "⚠ operator must re-materialize" warning lives. 0147 is exactly
# that case: it leaves nine CAGGs empty, and the fact that r1 already ran
# it in v0.40.0 does nothing for a new node.
#
# The register is prose, so this pass checks only PRESENCE, in both
# directions. It cannot check that a row says anything true.
#
# ── money-column detail ──
#
# Money is NUMERIC — never BIGINT / INTEGER / SMALLINT / INT8 / INT4 /
# INT2 / INT / MONEY / DOUBLE PRECISION / FLOAT[n] / REAL (JSON numbers
# are IEEE-754 doubles; i128 amounts overflow both int64 and 2^53, and a
# narrower integer/money type loses even more). For every
# migrations/*.up.sql this flags a column definition whose name looks
# monetary next to a non-NUMERIC numeric type.
#
# Escape hatch: append `-- lint-money:ok <reason>` on the flagged
# line. Reasons are mandatory — every escape is a design decision
# (e.g. SDEX price_n/price_d, a protocol-defined int32 rational pair
# whose money value lives in the sibling NUMERIC `price` column).
#
# This supersedes the SQL half of scripts/ci/lint-i128.sh (which now
# guards the Go side only); the deep Go-side guard is the go/types
# walk in internal/canonical/i128_truncation_guard_test.go.
#
# Exit 0 clean, non-zero on any violation. Wired into verify.sh + CI.
set -euo pipefail
cd "$(dirname "$0")/../.."
fail=0

indent() { printf '  %s\n' "${1//$'\n'/$'\n'  }"; }

# Monetary column-name stems. `_usd` matches only as a suffix of the
# column name (value_usd, volume_usd, …) so `usda`-style codes don't
# trip it. stroop/wei/circulating/market_cap carried over from the
# original lint-i128.sh name set; twap/vwap/tvl/wealth added 2026-08
# (a time/volume-weighted-average PRICE and total-value-locked / net-
# worth aggregate are money — must be NUMERIC, never float).
#
# Deliberately NOT in the stem set (each would false-positive on
# legitimate existing NON-money columns, and a stem that flags real
# code is worse than the gap it closes):
#   - median / mad — the volatility_baseline columns hold a *median
#     bucket-to-bucket VWAP percent-change return* + its MAD (0007/
#     0008): dimensionless statistics, correctly DOUBLE PRECISION, not
#     amounts. There is no genuine money-median column in the tree.
#   - rate — money rates already match via the `_usd` suffix rule
#     (rate_usd); bare `rate` would flag rate_limit_per_min (a request
#     count, 0027).
#   - cap — market cap already matches via `market_cap`; bare `cap`
#     would flag capacity/capture/…-style names.
name='[a-z0-9_]*(amount|price|supply|balance|volume|reserve|fee|stroop|wei|circulating|market_cap|twap|vwap|tvl|wealth)[a-z0-9_]*|[a-z0-9_]*_usd'
# Non-NUMERIC numeric types that must never hold money. Both the
# 128-bit-overflowing wide ints (bigint/int8) AND the narrower ints
# (integer/smallint/int4/int2/int) AND floats (double precision/float[n]
# /real) AND `money` (a fixed 2-decimal locale-dependent type — never
# correct for an i128 amount). NUMERIC is the only allowed home.
type='bigint|integer|smallint|int8|int4|int2|int|money|double precision|float[0-9]*|real'

for f in migrations/*.up.sql; do
  hits=$(grep -nEi "(^|[[:space:](,])\"?(${name})\"?[[:space:]]+(${type})\b" "$f" \
    | grep -vE '^[0-9]+:[[:space:]]*--' \
    | grep -viE -- '-- *lint-money:ok +[^ ]' || true)
  if [ -n "$hits" ]; then
    echo "lint-migrations ❌ ${f}: monetary column is not NUMERIC (ADR-0003):" >&2
    indent "$hits" >&2
    fail=1
  fi
  # Stale escapes: a lint-money:ok marker on a line the pattern does
  # not flag is dead weight — the allowlist only shrinks.
  stale=$(grep -nEi -- '-- *lint-money:ok' "$f" \
    | grep -vEi "(^|[[:space:](,])\"?(${name})\"?[[:space:]]+(${type})\b" || true)
  if [ -n "$stale" ]; then
    echo "lint-migrations ❌ ${f}: stale lint-money:ok marker (line no longer matches the lint) — remove it:" >&2
    indent "$stale" >&2
    fail=1
  fi
done

if [ "$fail" -eq 0 ]; then
  echo "✅ migration money-column lint passed."
fi

# ─── Pass 2: pairing / numbering / non-empty (audit C4-7) ───
# A missing or empty .down.sql means a migration can't be rolled back —
# a silent operational trap discovered only during an incident. This
# pass makes it a CI failure. Gaps are WARN-only (see header): the tree
# legitimately skips numbers (e.g. 0075, 0077-0079, 0084 today) when a
# migration is squashed out, so a hard no-gap rule would false-positive.

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

# ── pass 3: register completeness ──────────────────────────────────
REGISTER="migrations/README.md"
if [ ! -f "$REGISTER" ]; then
  echo "lint-migrations: $REGISTER is missing — the register cannot be checked." >&2
  fail=1
else
  # `|| true` is load-bearing: under `set -euo pipefail` a no-match grep
  # exits 1 and kills the script mid-assignment, so the vacuity check
  # below would never run — the exact way a guard goes quietly missing.
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
fi

# ── pass 4: ClickHouse money columns (ADR-0003) ────────────────────
# The lake DDL is money too, and ClickHouse has no NUMERIC. An on-chain
# amount is exact in Int64 (classic stroops are an XDR int64) or in
# Int128/Int256 (Soroban i128/u256); a USD figure belongs in
# Decimal(38, n) or the decimal String the aggregator already writes.
# Float32/Float64 are never a home for money — 53 bits of mantissa is
# the same 2^53 cliff ADR-0003 forbids in JSON. Same name stems as
# pass 1. Flagged columns are keyed table.column (a name alone would
# exempt every later column of that name in the file).
#
# Escape hatch: the same inline `-- lint-money:ok <reason>` as pass 1.
#
# ch_float_baseline lists the Float64 money columns that predate this
# pass, keyed file:table.column. Each is a documented display magnitude
# (its DDL header says so) mirrored between the operator file and
# tier1_schema.sql, and the fix belongs with that DDL: retype, or carry
# the inline escape with its reason. An entry that no longer matches is
# stale and FAILS, so this list only shrinks.
CH_DIR="${CH_DIR:-deploy/clickhouse}"
ch_float_baseline='account_cohort_rollup.sql:stellar.asset_month_usd_prices.volume_usd
account_cohort_rollup.sql:stellar.account_cohort_positions.amount
tier1_schema.sql:stellar.asset_month_usd_prices.volume_usd
tier1_schema.sql:stellar.account_cohort_positions.amount'

# ch_money_floats prints one `line<TAB>table.column<TAB>raw` per money
# column typed Float32/Float64 (Nullable or not) in $1. Table context
# comes from the enclosing CREATE TABLE / ALTER TABLE; a line that is a
# comment or carries a reasoned lint-money:ok escape is skipped.
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

# ── pass 5: downs never delete rows silently (#357 F1, #595) ───────
# A down that narrows a CHECK must REFUSE while offending rows exist
# (`DO $$ … IF EXISTS … RAISE EXCEPTION … $$`, see 0070's down), not
# DELETE them: a rollback past it would otherwise discard production
# rows and report success. Any executable DELETE/TRUNCATE in a down is a
# violation unless its line carries `-- lint-down-delete:ok <reason>`
# (e.g. removing only the seed row the matching up inserted).
del_re='^[[:space:]]*(DELETE[[:space:]]+FROM|TRUNCATE)\b'
down_files=0
for f in migrations/*.down.sql; do
  [ -e "$f" ] || continue
  down_files=$((down_files + 1))
  hits=$(grep -nEi "$del_re" "$f" | grep -viE -- '-- *lint-down-delete:ok +[^ ]' || true)
  if [ -n "$hits" ]; then
    echo "lint-migrations ❌ ${f}: down deletes rows silently — refuse with a RAISE EXCEPTION guard instead (see 0070's down):" >&2
    indent "$hits" >&2
    fail=1
  fi
  stale=$(grep -nEi -- '-- *lint-down-delete:ok' "$f" | grep -vEi "^[0-9]+:${del_re#^}" || true)
  if [ -n "$stale" ]; then
    echo "lint-migrations ❌ ${f}: stale lint-down-delete:ok marker (line is not a DELETE/TRUNCATE) — remove it:" >&2
    indent "$stale" >&2
    fail=1
  fi
done
if [ "$down_files" -eq 0 ]; then
  echo "lint-migrations ❌ no migrations/*.down.sql found — the down-delete pass cannot pass vacuously" >&2
  fail=1
fi

# ── Passes 6 and 7: operational hazards on populated tables ──
#
# MIG_DIR exists so scripts/ci/lint-migrations-test.sh can point these
# two passes at fixture trees; the passes above always read migrations/.
MIG_DIR="${MIG_DIR:-migrations}"

# sql_stmts <file>: one SQL statement per line, `--` comments stripped
# and whitespace collapsed, so a statement split across lines matches.
sql_stmts() {
  sed -E 's/--.*$//' "$1" | tr '\n\t' '  ' \
    | awk 'BEGIN { RS = ";" } { gsub(/ +/, " "); sub(/^ /, ""); if ($0 != "") print }'
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
    s ~ /^set (local )?lock_timeout/ { lt = 1 }
    s ~ /^create (unique )?index / && s !~ /^create (unique )?index concurrently/ {
      j = (w[2] == "unique") ? 4 : 3; ine = (w[j] == "if"); if (ine) j += 3
      tbl = ""
      for (i = j; i <= n; i++) if (w[i] == "on") { tbl = (w[i+1] == "only") ? w[i+2] : w[i+1]; break }
      sub(/\(.*/, "", tbl); sub(/^public\./, "", tbl)
      k++; idx[k] = w[j]; on[k] = tbl; ok[k] = ine
    }
    END { for (i = 1; i <= k; i++) if (!(on[i] in here)) printf "%s|%s|%d|%d\n", idx[i], on[i], ok[i], lt + 0 }'
}

# Pass 6 — CREATE INDEX on an existing hypertable. The in-transaction
# build holds a SHARE lock that blocks every write to the table for the
# whole build, and a partial index still scans every row. 0037's header
# is the recipe: `IF NOT EXISTS`, so an operator's CREATE INDEX
# CONCURRENTLY pre-build turns the migration into a no-op, and
# `SET LOCAL lock_timeout`, so the build cannot queue every writer behind
# an open transaction. Shipped migrations are immutable, so the ones that
# predate this pass are listed below (0150's operator
# note is in the README register); an entry that no longer matches is
# stale and fails (the list only shrinks).
hyper_index_baseline='0025_create_routers_and_attribution.up.sql:trades_routed_via_idx
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
  grep -qiE 'create +(unique +)?index' "$f" || continue
  b="$(basename "$f")"
  while IFS='|' read -r idx tbl ine lt; do
    [ -n "$idx" ] || continue
    grep -qx "$tbl" <<<"$hypertables" || continue
    hi_checked=$((hi_checked + 1))
    [ "$ine" = 1 ] && [ "$lt" = 1 ] && continue
    if grep -qx "${b}:${idx}" <<<"$hyper_index_baseline"; then
      hi_seen="${hi_seen}${b}:${idx}"$'\n'
      continue
    fi
    missing=""
    [ "$ine" = 1 ] || missing="IF NOT EXISTS"
    [ "$lt" = 1 ] || missing="${missing:+${missing} and }SET LOCAL lock_timeout"
    echo "lint-migrations ❌ ${f}: CREATE INDEX ${idx} ON ${tbl} lacks ${missing} — ${tbl} is an existing hypertable, and the in-transaction build blocks every write to it for the whole build. Write CREATE INDEX IF NOT EXISTS (so a CREATE INDEX CONCURRENTLY pre-build makes the migration a no-op) under SET LOCAL lock_timeout, and name the pre-build in the header and the README register row (0037 is the recipe)." >&2
    fail=1
  done < <(hyper_indexes "$f")
done
while IFS= read -r entry; do
  [ -n "$entry" ] || continue
  [ -e "${MIG_DIR}/${entry%%:*}" ] || continue
  if ! grep -qx "$entry" <<<"$hi_seen"; then
    echo "lint-migrations ❌ stale hyper_index_baseline entry ${entry} — it no longer names a non-conforming hypertable index; remove it (the list only shrinks)" >&2
    fail=1
  fi
done <<<"$hyper_index_baseline"
echo "lint-migrations: hypertable-index pass checked ${hi_checked} index build(s) on existing hypertables under ${MIG_DIR}."

# Pass 7 — a continuous aggregate dropped and recreated WITH NO DATA is
# EMPTY until someone refreshes it, and a migration cannot do that:
# refresh_continuous_aggregate refuses a transaction block, and
# golang-migrate runs each file as one. So the file must NAME the
# refresh (`refresh_continuous_aggregate('<view>'`, in its operator
# header) for every view it recreates — a down included, because a
# rollback empties the views exactly as the up did.
cagg_files=0
for f in "$MIG_DIR"/*.sql; do
  grep -qi 'with no data' "$f" || continue
  stmts="$(sql_stmts "$f")"
  grep -qi 'with no data' <<<"$stmts" || continue
  cagg_files=$((cagg_files + 1))
  dropped="$(grep -iE '^drop materialized view ' <<<"$stmts" \
    | sed -E 's/^[Dd][Rr][Oo][Pp] [Mm][Aa][Tt][Ee][Rr][Ii][Aa][Ll][Ii][Zz][Ee][Dd] [Vv][Ii][Ee][Ww] ([Ii][Ff] [Ee][Xx][Ii][Ss][Tt][Ss] )?//' \
    | tr ',' '\n' | awk '{ print tolower($1) }' | sed 's/^public\.//' | sort -u || true)"
  named="$(grep -oE "refresh_continuous_aggregate\( *'[a-z0-9_]+'" "$f" | sed -E "s/.*'([a-z0-9_]+)'/\1/" | sort -u || true)"
  unnamed=""
  for v in $dropped; do
    grep -qx "$v" <<<"$named" || unnamed="${unnamed}${v} "
  done
  if [ -n "$unnamed" ]; then
    echo "lint-migrations ❌ ${f}: recreates continuous aggregate(s) WITH NO DATA and names no refresh for: ${unnamed}— they serve nothing until re-materialized. Write the ordered CALL refresh_continuous_aggregate('<view>', …) sequence into the header (hierarchical views after their parent is whole) and the README register row." >&2
    fail=1
  fi
done
echo "lint-migrations: CAGG re-materialization pass inspected ${cagg_files} WITH NO DATA file(s) under ${MIG_DIR}."

# ── pass 8: atomicity ───────────────────────────────────────────────
# golang-migrate sends a whole file as ONE simple-protocol query, which
# Postgres runs as one implicit transaction. An explicit COMMIT/ROLLBACK
# ends it, so every statement after the first one runs in a fresh
# transaction: a failure there leaves the earlier half durable and the
# version dirty (GH #1158). BEGIN; … COMMIT; around the whole body is
# fine; anything but comments after the COMMIT is not. TXN_DIR is the
# fixture seam for scripts/ci/lint-migrations-test.sh.
#
# 0030's up is the one shipped instance. Its up body is immutable, so it
# is exempted here and 0168 restores what a failed tail would lose.
TXN_DIR="${TXN_DIR:-migrations}"
txn_exempt="0030_asset_supply_history_unique_constraint.up.sql"
txn_files=0
for f in "$TXN_DIR"/*.sql; do
  [ -e "$f" ] || continue
  txn_files=$((txn_files + 1))
  [ "$(basename "$f")" = "$txn_exempt" ] && continue
  after="$(awk '
    { line = $0; sub(/--.*/, "", line); gsub(/^[ \t]+|[ \t]+$/, "", line)
      if (line == "") next
      if (ended) { print FNR ": " $0; exit }
      if (toupper(line) ~ /^(COMMIT|ROLLBACK|ABORT)([ \t]+(WORK|TRANSACTION))?[ \t]*;$/) ended = 1 }
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


if [ "$fail" -eq 0 ]; then
  echo "✅ migration lint passed (money-column + pairing/numbering/non-empty + register + ClickHouse money + down-delete, ${down_files} downs + hypertable index + CAGG re-materialization + atomicity)."
fi
exit "$fail"
