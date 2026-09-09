#!/usr/bin/env bash
# ch-schema-drift-test.sh — self-test for ch-schema-drift.sh.
#
# Mirrors scripts/ops/ch-schema-snapshot-test.sh: a hermetic harness that
# feeds the checker synthetic inputs and asserts the verdict, so the
# comparison logic is exercised without a ClickHouse server.
#
# The fixtures matter as much as the assertions. LIVE_OK below is written
# in ClickHouse's OWN `SHOW CREATE TABLE` rendering — backticked column
# names, the database qualifier, an inferred column list on the
# materialized view, a trailing `SETTINGS index_granularity = 8192`, a
# tuple ORDER BY, and the newline layout the server actually emits —
# while the intent fixture is written the way a human writes
# tier1_schema.sql. If the normalizer only ever saw two files of the same
# shape (which a naive "diff the file against itself" test does), it
# would pass while being useless against a real snapshot.
#
# Usage: ./configs/ansible/roles/archival-node/files/ch-schema-drift-test.sh
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
drift="$here/ch-schema-drift.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0

# bt is a literal backtick — ClickHouse quotes identifiers with them in
# SHOW CREATE output, and building the sed patterns from a variable keeps
# them out of any quoting context that would try to evaluate them.
bt='`'

indent() { while IFS= read -r l; do printf '       | %s\n' "$l"; done <<<"$1"; }

# run <name> <want-rc> <intent-file> <live-file>
run() {
  local name="$1" want="$2" intent="$3" live="$4" out rc
  out="$(INTENT="$intent" LIVE_SCHEMA="$live" TEXTFILE_DIR=/dev/null \
         bash "$drift" 2>&1)"
  rc=$?
  if [[ "$rc" -eq "$want" ]]; then
    pass=$((pass + 1))
    echo "ok   — $name (rc=$rc)"
  else
    fail=$((fail + 1))
    echo "FAIL — $name: rc=$rc, want $want"
    indent "$out"
  fi
}

# expect_msg <name> <substring> <intent> <live>
expect_msg() {
  local name="$1" needle="$2" intent="$3" live="$4" out
  out="$(INTENT="$intent" LIVE_SCHEMA="$live" TEXTFILE_DIR=/dev/null \
         bash "$drift" 2>&1)"
  if grep -qF -- "$needle" <<<"$out"; then
    pass=$((pass + 1))
    echo "ok   — $name (reported '$needle')"
  else
    fail=$((fail + 1))
    echo "FAIL — $name: output did not name '$needle'"
    indent "$out"
  fi
}

# ─── fixtures ───────────────────────────────────────────────────────

cat > "$tmp/intent.sql" <<'SQL'
-- Founding DDL, written the way a human writes it.
CREATE DATABASE IF NOT EXISTS stellar;

CREATE TABLE IF NOT EXISTS stellar.ledgers
(
    ledger_seq   UInt32,
    close_time   DateTime('UTC'),
    ledger_hash  String,
    ingested_at  DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger_seq, 1000000)
ORDER BY ledger_seq;

CREATE TABLE IF NOT EXISTS stellar.account_movements
(
    address       String,
    ledger        UInt32,
    tx_hash       String,
    op_index      UInt32,
    amount        Int128,
    ingested_at   DateTime DEFAULT now(),
    INDEX idx_am_tx tx_hash TYPE bloom_filter(0.01) GRANULARITY 1
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger, 1000000)
ORDER BY (address, ledger, tx_hash, op_index);

CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.tx_hash_index_mv
TO stellar.tx_hash_index AS
SELECT tx_hash, ledger_seq
FROM stellar.transactions;
SQL

# ClickHouse's own SHOW CREATE rendering of an equivalent live schema.
cat > "$tmp/live-ok.sql" <<'SQL'
CREATE DATABASE IF NOT EXISTS stellar;

CREATE TABLE stellar.ledgers
(
    `ledger_seq` UInt32,
    `close_time` DateTime('UTC'),
    `ledger_hash` String,
    `ingested_at` DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger_seq, 1000000)
ORDER BY ledger_seq
SETTINGS index_granularity = 8192;

CREATE TABLE stellar.account_movements
(
    `address` String,
    `ledger` UInt32,
    `tx_hash` String,
    `op_index` UInt32,
    `amount` Int128,
    `ingested_at` DateTime DEFAULT now(),
    INDEX idx_am_tx tx_hash TYPE bloom_filter(0.01) GRANULARITY 1
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY intDiv(ledger, 1000000)
ORDER BY (address, ledger, tx_hash, op_index)
SETTINGS index_granularity = 8192;

CREATE MATERIALIZED VIEW stellar.tx_hash_index_mv TO stellar.tx_hash_index
(
    `tx_hash` String,
    `ledger_seq` UInt32
) AS
SELECT
    tx_hash,
    ledger_seq
FROM stellar.transactions;
SQL

mutate() { sed "$1" "$tmp/live-ok.sql" > "$2"; }

# ─── the clean case ─────────────────────────────────────────────────
run "clean: SHOW CREATE rendering of the same schema" 0 "$tmp/intent.sql" "$tmp/live-ok.sql"

# ─── each compared attribute, mutated one at a time ─────────────────
mutate 's/ORDER BY (address, ledger, tx_hash, op_index)/ORDER BY (address, ledger, op_index, tx_hash)/' "$tmp/live-order.sql"
run       "drift: ORDER BY reordered live"        1 "$tmp/intent.sql" "$tmp/live-order.sql"
expect_msg "drift: names the ORDER BY" "DRIFT account_movements.order" "$tmp/intent.sql" "$tmp/live-order.sql"

mutate 's/PARTITION BY intDiv(ledger, 1000000)/PARTITION BY intDiv(ledger, 5000000)/' "$tmp/live-part.sql"
run       "drift: PARTITION BY changed live"      1 "$tmp/intent.sql" "$tmp/live-part.sql"
expect_msg "drift: names the PARTITION BY" "DRIFT account_movements.partition" "$tmp/intent.sql" "$tmp/live-part.sql"

# ReplacingMergeTree version column swapped: same engine NAME, different
# dedup semantics. system.tables.engine would NOT catch this — the engine
# args only exist in SHOW CREATE, which is why the checker parses that.
sed 's/^ENGINE = ReplacingMergeTree(ingested_at)$/ENGINE = ReplacingMergeTree(ledger)/' "$tmp/live-ok.sql" > "$tmp/live-eng.sql"
run       "drift: ReplacingMergeTree version column changed" 1 "$tmp/intent.sql" "$tmp/live-eng.sql"
expect_msg "drift: names the engine" "DRIFT ledgers.engine" "$tmp/intent.sql" "$tmp/live-eng.sql"

sed "/${bt}op_index${bt} UInt32,/d" "$tmp/live-ok.sql" > "$tmp/live-col.sql"
run       "drift: column dropped live"            1 "$tmp/intent.sql" "$tmp/live-col.sql"
expect_msg "drift: names the columns" "DRIFT account_movements.columns" "$tmp/intent.sql" "$tmp/live-col.sql"

# A whole table never created on the box.
awk '/^CREATE TABLE stellar.account_movements/{skip=1} skip && /^SETTINGS index_granularity/{skip=0; next} !skip' \
  "$tmp/live-ok.sql" > "$tmp/live-missing.sql"
run       "drift: declared table absent live"     1 "$tmp/intent.sql" "$tmp/live-missing.sql"
expect_msg "drift: names the missing table" "ABSENT from the live schema" "$tmp/intent.sql" "$tmp/live-missing.sql"

# MV repointed at a different destination table.
sed 's/TO stellar.tx_hash_index$/TO stellar.tx_hash_index_v2/' "$tmp/live-ok.sql" > "$tmp/live-mv.sql"
run       "drift: materialized view repointed"    1 "$tmp/intent.sql" "$tmp/live-mv.sql"

# ─── the non-fatal directions ───────────────────────────────────────
# A live table absent from the founding DDL is counted, not failed.
cat "$tmp/live-ok.sql" > "$tmp/live-extra.sql"
cat >> "$tmp/live-extra.sql" <<'SQL'

CREATE TABLE stellar.holders_snapshot
(
    `address` String,
    `balance` Int128
)
ENGINE = ReplacingMergeTree
ORDER BY address
SETTINGS index_granularity = 8192;
SQL
run       "uncodified live table is not a failure" 0 "$tmp/intent.sql" "$tmp/live-extra.sql"
expect_msg "uncodified live table is reported"     "UNCODIFIED holders_snapshot" "$tmp/intent.sql" "$tmp/live-extra.sql"

# Type text re-rendered by ClickHouse is INFO, never drift.
sed "s/${bt}amount${bt} Int128,/${bt}amount${bt} Int128 CODEC(Delta, ZSTD(3)),/" "$tmp/live-ok.sql" > "$tmp/live-codec.sql"
run "type/CODEC re-rendering is not drift" 0 "$tmp/intent.sql" "$tmp/live-codec.sql"

# ─── cannot-compare must NOT read as clean ──────────────────────────
run "unreadable live schema exits 2, not 0" 2 "$tmp/intent.sql" "$tmp/does-not-exist.sql"
: > "$tmp/empty.sql"
run "empty live schema exits 2, not 0"      2 "$tmp/intent.sql" "$tmp/empty.sql"
run "empty intent exits 2, not 0"           2 "$tmp/empty.sql"  "$tmp/live-ok.sql"

# ─── the real repo intent must at least parse ───────────────────────
real_intent="$here/../../../../../deploy/clickhouse/tier1_schema.sql"
if [[ -r "$real_intent" ]]; then
  run "real tier1_schema.sql compares clean against itself" 0 \
    "$real_intent" "$real_intent"
fi

# ── AS-clone (alias) declarations ──────────────────────────────────
# tier1_schema.sql declares every *_staging table as a two-line clone
# (`CREATE TABLE x_staging` / `AS stellar.x;`) with no ENGINE/columns of
# its own, while SHOW CREATE renders the clone fully. The 2026-08-24
# incident: the parser emitted empty facts for the clone → 3 drifts per
# staging table forever; and the symmetric intent-vs-intent test above
# could never catch it. These fixtures are deliberately ASYMMETRIC.
cat > "$tmp/alias_intent.sql" <<SQL
CREATE TABLE IF NOT EXISTS stellar.base_t
(
    metric String,
    value  Int64
)
ENGINE = MergeTree
ORDER BY metric;

CREATE TABLE IF NOT EXISTS stellar.base_t_staging
AS stellar.base_t;
SQL
cat > "$tmp/alias_live.sql" <<SQL
CREATE TABLE stellar.base_t
(
    ${bt}metric${bt} String,
    ${bt}value${bt} Int64
)
ENGINE = MergeTree
ORDER BY metric
SETTINGS index_granularity = 8192;

CREATE TABLE stellar.base_t_staging
(
    ${bt}metric${bt} String,
    ${bt}value${bt} Int64
)
ENGINE = MergeTree
ORDER BY metric
SETTINGS index_granularity = 8192;
SQL
run "AS-clone staging table resolves against its base (no false drift)" 0 \
  "$tmp/alias_intent.sql" "$tmp/alias_live.sql"

# A clone whose LIVE half genuinely diverged from the base must still drift.
cat > "$tmp/alias_live_bad.sql" <<SQL
CREATE TABLE stellar.base_t
(
    ${bt}metric${bt} String,
    ${bt}value${bt} Int64
)
ENGINE = MergeTree
ORDER BY metric
SETTINGS index_granularity = 8192;

CREATE TABLE stellar.base_t_staging
(
    ${bt}metric${bt} String,
    ${bt}value${bt} Int64
)
ENGINE = MergeTree
ORDER BY value
SETTINGS index_granularity = 8192;
SQL
expect_msg "AS-clone with a diverged live ORDER BY still drifts" \
  "DRIFT base_t_staging.order" "$tmp/alias_intent.sql" "$tmp/alias_live_bad.sql"

# A clone of an undeclared base is itself drift, never a silent pass.
cat > "$tmp/alias_orphan.sql" <<SQL
CREATE TABLE IF NOT EXISTS stellar.ghost_staging
AS stellar.ghost;
SQL
cat > "$tmp/alias_orphan_live.sql" <<SQL
CREATE TABLE stellar.ghost_staging
(
    ${bt}metric${bt} String
)
ENGINE = MergeTree
ORDER BY metric
SETTINGS index_granularity = 8192;
SQL
expect_msg "AS-clone of an undeclared base reports drift" \
  "not declared" "$tmp/alias_orphan.sql" "$tmp/alias_orphan_live.sql"

# ─── how the check FINDS the repo's intent (2026-09-09) ─────────────
# Every case above hands the checker an explicit INTENT, so the branch
# that runs when nothing sets one was never exercised — and it shipped
# broken. That branch is not a corner: it is the by-hand shape
# ch-schema-restore.md documents ("Run it by hand: ch-schema-drift.sh"),
# the first thing an operator types when the unit goes red.
#
# Installed standalone at /usr/local/bin, the checkout-relative default
# `dirname/../../../../..` does not fail — it CLAMPS at `/`, so the check
# refused itself with
#
#   ch-schema-drift: repo intent //deploy/clickhouse/tier1_schema.sql is
#   not readable — nothing to compare against
#
# a double-slashed path that cannot exist on any host. It reads as schema
# drift and is a broken check, and while it stood the testnet had NO
# ClickHouse schema-drift coverage at all. Reproducing it needs a copy of
# the script OUTSIDE any checkout, which is what the fixture below is.

fake_bin="$tmp/opt/bin"
fake_share="$tmp/opt/share"
mkdir -p "$fake_bin" "$fake_share"
cp "$drift" "$fake_bin/ch-schema-drift.sh"
installed="$fake_bin/ch-schema-drift.sh"

# The host-side copy the role's "Ship the repo's Tier-1 lake DDL as the
# drift check's intent side" task installs. It carries one table the live
# fixture does not, so a run that reports THAT table can only have read
# THIS file — provenance, not merely "something got compared".
shipped="$fake_share/tier1_schema.sql"
cp "$tmp/intent.sql" "$shipped"
cat >> "$shipped" <<'SQL'

CREATE TABLE IF NOT EXISTS stellar.shipped_copy_probe
(
    probe String
)
ENGINE = MergeTree
ORDER BY probe;
SQL

# The same shipped copy without the probe, for the clean case.
shipped_clean="$fake_share/tier1_schema_clean.sql"
cp "$tmp/intent.sql" "$shipped_clean"

ok()  { pass=$((pass + 1)); echo "ok   — $1"; }
bad() { fail=$((fail + 1)); echo "FAIL — $1"; indent "$2"; }

# by_hand <installed-intent> <live> — the installed script with INTENT
# genuinely ABSENT from the environment (`env -u`, not the empty string),
# which is what a bare `ch-schema-drift.sh` on a host actually gets.
by_hand() {
  env -u INTENT INSTALLED_INTENT="$1" LIVE_SCHEMA="$2" TEXTFILE_DIR=/dev/null \
    bash "$installed" 2>&1
}

name="installed + no INTENT: falls back to the shipped copy and really compares"
out="$(by_hand "$shipped_clean" "$tmp/live-ok.sql")"; rc=$?
if [[ "$rc" -eq 0 ]] && grep -qF -- "compared 3 declared table(s)" <<<"$out"; then
  ok "$name (rc=0, compared 3)"
else
  bad "$name: rc=$rc want 0, and output must say 'compared 3 declared table(s)'" "$out"
fi

name="installed + no INTENT: the compared file IS the shipped copy"
out="$(by_hand "$shipped" "$tmp/live-ok.sql")"; rc=$?
if [[ "$rc" -eq 1 ]] \
  && grep -qF -- "DRIFT shipped_copy_probe" <<<"$out" \
  && grep -qF -- "compared 4 declared table(s)" <<<"$out"; then
  ok "$name (rc=1, named the table only that file declares)"
else
  bad "$name: rc=$rc want 1, naming 'DRIFT shipped_copy_probe' over 4 compared tables" "$out"
fi

name="installed + no INTENT + nothing shipped: refuses, and never builds a //path"
out="$(by_hand "$fake_share/never-shipped.sql" "$tmp/live-ok.sql")"; rc=$?
if [[ "$rc" -eq 2 ]] \
  && grep -qF -- "no repo intent to compare against" <<<"$out" \
  && grep -qF -- "$fake_share/never-shipped.sql" <<<"$out" \
  && ! grep -qF -- "//deploy/clickhouse" <<<"$out" \
  && ! grep -qF -- "compared " <<<"$out"; then
  ok "$name (rc=2, names every candidate tried)"
else
  bad "$name: rc=$rc want 2, must name the candidate it tried and must not print '//deploy/clickhouse' or compare anything" "$out"
fi

# The refusal must stay a refusal. An INTENT the operator set explicitly
# and that cannot be read is a HARD STOP — silently comparing against the
# shipped copy instead would answer a question nobody asked, and would be
# the "check that cannot find its reference passes anyway" failure this
# whole script exists to prevent.
name="explicit but unreadable INTENT still exits 2, never falls back"
out="$(INTENT="$tmp/typo-tier1.sql" INSTALLED_INTENT="$shipped_clean" \
       LIVE_SCHEMA="$tmp/live-ok.sql" TEXTFILE_DIR=/dev/null \
       bash "$installed" 2>&1)"; rc=$?
if [[ "$rc" -eq 2 ]] \
  && grep -qF -- "is not readable" <<<"$out" \
  && ! grep -qF -- "compared " <<<"$out"; then
  ok "$name (rc=2)"
else
  bad "$name: rc=$rc want 2 with 'is not readable' and no comparison" "$out"
fi

# ─── the shipped-copy path is written in four places ────────────────
# The script's fallback, the role default, the unit's Environment= and
# the copy task's dest must name the SAME file. Three of the four already
# agreed on 2026-09-09 and the script did not, which is exactly how the
# by-hand run ended up reading a path no host has. Pinned rather than
# trusted: these four live in four different files and nothing else
# compares them.
defaults_yml="$here/../defaults/main.yml"
unit_j2="$here/../templates/systemd/ch-schema-drift.service.j2"
ship_task="$here/../tasks/18-pgbackrest-backup.yml"

# jinja_default <file> — the literal inside `default('…')` on the line
# that reads ch_schema_drift_intent. Anchored on the variable name so a
# neighbouring default() for something else cannot be picked up.
jinja_default() {
  sed -n "s/.*ch_schema_drift_intent | default('\([^']*\)').*/\1/p" "$1"
}

# The `${INSTALLED_INTENT:-…}` default, matched as text. Spelled without
# the literal `${` so shellcheck does not read a deliberately-inert
# pattern as a botched expansion (SC2016); an empty capture fails the
# assertion below, so a rename cannot make this quietly stop looking.
script_path="$(sed -n 's/^INSTALLED_INTENT=.*:-\(.*\)}"$/\1/p' "$drift")"
role_path="$(sed -n 's/^ch_schema_drift_intent:[[:space:]]*"\(.*\)"[[:space:]]*$/\1/p' "$defaults_yml")"
unit_path="$(jinja_default "$unit_j2")"
ship_path="$(jinja_default "$ship_task")"

name="the shipped-intent path agrees across script, role default, unit and copy task"
if [[ -n "$script_path" && "$script_path" != *$'\n'* \
   && "$role_path"   == "$script_path" \
   && "$unit_path"   == "$script_path" \
   && "$ship_path"   == "$script_path" ]]; then
  ok "$name ($script_path)"
else
  bad "$name" \
"ch-schema-drift.sh INSTALLED_INTENT : ${script_path:-<not found>}
defaults/main.yml                   : ${role_path:-<not found>}
ch-schema-drift.service.j2          : ${unit_path:-<not found>}
18-pgbackrest-backup.yml dest       : ${ship_path:-<not found>}"
fi

echo
echo "ch-schema-drift-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
