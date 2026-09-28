#!/usr/bin/env bash
# lint-migration-commands-test.sh — fixture tests for the
# migration-header-command gate (scripts/ci/lint-migration-commands.sh).
#
# The gate exists because a migration header handed the operator its
# only abort mechanism and that command matched ZERO rows — the
# predicate named the materialization hypertable where the jobs view
# reports the aggregate's user view name. `alter_job` over an empty set
# prints nothing and exits 0, so the disarm read as a success and the
# policy would have dropped 66 GB the next day.
#
# So the red case pinned here is THAT command, verbatim, in a migration
# with no test — and the green case is the same command once a test
# names the migration and holds a slice of it. The rest of the verdicts
# keep the gate off the prose it lives among:
#
#   - the repo's own tree passes;
#   - the zero-row disarm command with no test is CAUGHT;
#   - the same command with a test that names the migration and pins it
#     PASSES;
#   - a test that names the migration but shares no slice of the command
#     is NOT coverage — blanket coverage by filename is the hole this
#     closes;
#   - a `DO NOT RUN:` form is exempt, on the line and across lines;
#   - a `...` sketch is exempt;
#   - prose beginning with an uppercase verb and carrying a semicolon
#     ("DROP TABLE removes the hypertable …; CASCADE is not needed") is
#     NOT a command, nor is "one INSERT per swap (low volume, sparse hot
#     path);", nor is a fragment with no terminator;
#   - the baseline needs a reason, silences the command it names, and
#     fails STALE once that command is covered or gone;
#   - an empty migrations tree FAILS rather than passing vacuously.
#
# Run: bash scripts/ci/lint-migration-commands-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-migration-commands.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
check() { # check <desc> <want-exit> <root>
  local desc="$1" want="$2" root="$3" got err
  # Captures stderr only (2>&1, then >/dev/null): an exit code alone
  # cannot tell a real pass from a subshell that crashed and left the
  # reader loop an empty/truncated list — a `<( )` process substitution's
  # exit status never reaches its reader, so that crash prints to stderr
  # but the script still exits 0. A bash parser diagnostic there is a
  # gate that passed for the wrong reason, want or not.
  err="$(bash "$LINT" "$root" 2>&1 >/dev/null)"
  got=$?
  if [ "$got" -eq "$want" ] && ! grep -q ': line [0-9][0-9]*: syntax error' <<<"$err"; then
    echo "  ok   $desc"
    pass=$((pass + 1))
  elif [ "$got" -eq "$want" ]; then
    echo "  FAIL $desc (exit $got matched, but bash reported: ${err##*$'\n'})"
    fail=$((fail + 1))
  else
    echo "  FAIL $desc (exit $got, want $want)"
    fail=$((fail + 1))
  fi
}

# catches <desc> <root> <needle> — fails AND names the command, so a
# pass-by-accident elsewhere cannot masquerade as coverage of the case.
catches() {
  local desc="$1" root="$2" needle="$3" out got
  out="$(bash "$LINT" "$root" 2>&1)"
  got=$?
  # A here-string, not a pipe: `grep -q` closes its input on the first
  # match, and under a pipefail shell the SIGPIPE'd writer would decide
  # the verdict (lint-shell-sigpipe.sh).
  if [ "$got" -ne 0 ] && grep -qF "$needle" <<<"$out"; then
    echo "  ok   $desc"
    pass=$((pass + 1))
  else
    echo "  FAIL $desc (exit $got, did not report: $needle)"
    fail=$((fail + 1))
  fi
}

MIG=0157_example_policy.up.sql

# mk <name> — a tree with one migration whose header is prose only.
mk() {
  local r="$TMP/$1"
  mkdir -p "$r/migrations" "$r/internal/storage/timescale"
  cat > "$r/migrations/$MIG" <<'SQL'
-- 0157 up — attach an example policy.
--
-- Prose about the policy, with no command an operator could paste.

BEGIN;

SELECT 1;

COMMIT;
SQL
  echo "$r"
}

# arm_header <root> <predicate-column> — append the arm/disarm block.
arm_header() { # <root> <column>
  local r="$1" col="$2"
  cat >> "$r/migrations/$MIG" <<SQL

-- DISARM (before ANY re-materialisation):
--
--   SELECT alter_job(job_id, scheduled => false)
--     FROM timescaledb_information.jobs
--    WHERE proc_name = 'policy_retention'
--      AND $col = 'prices_1m';
SQL
}

echo "lint-migration-commands-test: header-command verdicts"

check "the repo's own tree passes" 0 "$PWD"

prose_only="$(mk prose_only)"
check "a header with prose only passes" 0 "$prose_only"

# ── THE HISTORICAL DEFECT: a disarm command nothing tested ───────────
# The predicate is the one that matched zero rows on r1. The gate has
# no opinion about the predicate — it has an opinion about the command
# shipping with nothing that asserts on it, which is the step whose
# absence let the wrong predicate through.
untested="$(mk untested)"
arm_header "$untested" materialization_hypertable_name
catches "the untested zero-row disarm command is caught" \
  "$untested" "SELECT alter_job(job_id, scheduled => false)"

# ── the same command, pinned by a test that names the migration ──────
tested="$(mk tested)"
arm_header "$tested" hypertable_name
cat > "$tested/internal/storage/timescale/policy_test.go" <<'GO'
package timescale

import (
	"strings"
	"testing"
)

// Pins the disarm command in migrations/0157_example_policy.up.sql.
// timescaledb_information.jobs reports the aggregate's USER VIEW name in
// hypertable_name, so a predicate written against the materialization
// hypertable matches zero rows — and alter_job over zero rows exits 0.
func TestPolicyDisarmMatchesTheViewName(t *testing.T) {
	sql := readRepoFile(t, "migrations/0157_example_policy.up.sql")
	if strings.Contains(sql, "materialization_hypertable_name") {
		t.Error("disarm selects the job by materialization_hypertable_name")
	}
	if !strings.Contains(sql, "SELECT alter_job(job_id, scheduled => false)") {
		t.Error("0157 no longer states the disarm command")
	}
}
GO
check "the same command with a test that names and pins it passes" 0 "$tested"

# A test that names the migration but shares no slice is not coverage.
namedonly="$(mk namedonly)"
arm_header "$namedonly" hypertable_name
cat > "$namedonly/internal/storage/timescale/policy_test.go" <<'GO'
package timescale

import "testing"

// Reads the file for an unrelated reason. Naming it must not blanket
// every command in its header.
func TestMigrationFileExists(t *testing.T) {
	_ = readRepoFile(t, "migrations/0157_example_policy.up.sql")
}
GO
catches "a test that only NAMES the migration is not coverage" \
  "$namedonly" "SELECT alter_job(job_id, scheduled => false)"

# ── DO NOT RUN is a warning, not a recipe ────────────────────────────
donotrun="$(mk donotrun)"
cat >> "$donotrun/migrations/$MIG" <<'SQL'

-- Never the NULL-start form:
--
--     DO NOT RUN: CALL refresh_continuous_aggregate('twap_1h', NULL, now());
SQL
check "a DO NOT RUN form on the command's own line is exempt" 0 "$donotrun"

donotrun2="$(mk donotrun2)"
cat >> "$donotrun2/migrations/$MIG" <<'SQL'

-- Never the NULL-start form migrations 0081 / 0126 / 0147 document
-- (DO NOT RUN: CALL refresh_continuous_aggregate('twap_1h', NULL,
-- now());) — post-drop it deletes the TWAP history.
SQL
check "a DO NOT RUN form wrapped across lines is exempt" 0 "$donotrun2"

# Removing the marker turns the same text back into a recipe, so the
# exemption is the marker and not the shape of the command.
armed="$(mk armed)"
cat >> "$armed/migrations/$MIG" <<'SQL'

-- Re-materialise afterwards:
--
--     CALL refresh_continuous_aggregate('twap_1h', NULL, now());
SQL
catches "the same form WITHOUT the marker is caught" \
  "$armed" "CALL refresh_continuous_aggregate('twap_1h', NULL, now());"

# ── shapes that are not commands ─────────────────────────────────────
sketch="$(mk sketch)"
cat >> "$sketch/migrations/$MIG" <<'SQL'

-- Backfill shape, for orientation only:
--
--   INSERT INTO comet_liquidity (...) SELECT ... FROM soroban_events
--   WHERE topic_0_sym = 'POOL';
SQL
check "an ellipsis sketch is exempt (it cannot be pasted and run)" 0 "$sketch"

prose_semi="$(mk prose_semi)"
cat >> "$prose_semi/migrations/$MIG" <<'SQL'

-- DROP TABLE removes the hypertable, its chunks, indexes and
-- compression settings in one statement; CASCADE is not needed.
--
-- The blend_auctions table sees one INSERT per swap (low volume,
-- sparse hot path); the brief lock is therefore acceptable.
SQL
check "prose beginning with an uppercase verb is not a command" 0 "$prose_semi"

fragment="$(mk fragment)"
cat >> "$fragment/migrations/$MIG" <<'SQL'

-- The read path this index serves is
--
--   SELECT DISTINCT ON (source) ... FROM trades WHERE base_asset = $1
--   ORDER BY source, ts DESC
SQL
check "a quoted fragment with no terminator is not a command" 0 "$fragment"

# ── baseline mechanics ───────────────────────────────────────────────
based="$(mk based)"
arm_header "$based" materialization_hypertable_name
mkdir -p "$based/scripts/ci"
cat > "$based/scripts/ci/lint-migration-commands.baseline" <<'BASE'
# Grandfathered, shrink-only.
migrations/0157_example_policy.up.sql#SELECTalter_job(job_id,scheduled=>false)FROMtimescaledb_information.jobsWHEREpro  covered by the follow-up test in the next change
BASE
check "a baseline entry with a reason silences the command it names" 0 "$based"

noreason="$(mk noreason)"
arm_header "$noreason" materialization_hypertable_name
mkdir -p "$noreason/scripts/ci"
printf '%s\n' \
  'migrations/0157_example_policy.up.sql#SELECTalter_job(job_id,scheduled=>false)FROMtimescaledb_information.jobsWHEREproc' \
  > "$noreason/scripts/ci/lint-migration-commands.baseline"
check "a baseline entry with no reason is refused" 1 "$noreason"

stale="$(mk stale)"
mkdir -p "$stale/scripts/ci"
printf '%s\n' \
  'migrations/0157_example_policy.up.sql#SELECTalter_job(job_id,scheduled=>false)  the command was removed from the header' \
  > "$stale/scripts/ci/lint-migration-commands.baseline"
check "a stale baseline entry fails until it is deleted" 1 "$stale"

# ── docs/ fenced ```sql DELETE/UPDATE (GH-795) ────────────────────────
docs_unbound="$(mk docs_unbound)"
mkdir -p "$docs_unbound/docs"
cat > "$docs_unbound/docs/bad-runbook.md" <<'MD'
# Bad runbook

```sql
DELETE FROM trades WHERE source IN ('aquarius');
```
MD
catches "a docs/ DELETE with no time-bound predicate is caught" \
  "$docs_unbound" "DELETE FROM trades WHERE source IN ('aquarius')"

docs_bound="$(mk docs_bound)"
mkdir -p "$docs_bound/docs"
cat > "$docs_bound/docs/good-runbook.md" <<'MD'
# Good runbook

```sql
DELETE FROM trades WHERE source IN ('aquarius') AND ts >= '<from>'::timestamptz;
```
MD
check "a docs/ DELETE with a ts predicate passes" 0 "$docs_bound"

docs_do_not_run="$(mk docs_do_not_run)"
mkdir -p "$docs_do_not_run/docs"
cat > "$docs_do_not_run/docs/warning-runbook.md" <<'MD'
# Warning runbook

```sql
-- DO NOT RUN: this deletes every row.
DELETE FROM trades;
```
MD
check "a DO NOT RUN docs/ block is exempt" 0 "$docs_do_not_run"

# ── the operator corpus: NULL-start refresh of a view over a retained source
# The historical defect: both alert-rule trees and the twap-history-missing
# runbook told the on-call to run the NULL-start TWAP refresh that 0156
# marks DO NOT RUN, and no gate read either file.
echo "lint-migration-commands-test: operator-corpus verdicts"

# twap <name> — a tree whose migrations attach retention to prices_1m and
# build twap_1h on it, next to prices_1d built on trades (no retention).
twap() {
  local r
  r="$(mk "$1")"
  cat > "$r/migrations/0150_example_twap.up.sql" <<'SQL'
-- 0150 up — an example TWAP view over a retained aggregate.
--
-- Prose only.

BEGIN;

SELECT add_retention_policy(
         'prices_1m',
         drop_after => INTERVAL '90 days');

CREATE MATERIALIZED VIEW twap_1h
WITH (timescaledb.continuous) AS
SELECT time_bucket('1 hour', bucket) AS bucket, avg(twap) AS twap
FROM prices_1m
GROUP BY 1
WITH NO DATA;

CREATE MATERIALIZED VIEW prices_1d
WITH (timescaledb.continuous) AS
SELECT time_bucket('1 day', ts) AS bucket, count(*) AS n
FROM trades
GROUP BY 1
WITH NO DATA;

COMMIT;
SQL
  mkdir -p "$r/docs/operations/runbooks" "$r/deploy/monitoring/rules"
  echo "$r"
}

out="$(bash "$LINT" "$PWD" 2>&1)"
if grep -qF "NULL-start refreshes of: twap_1d twap_1h;" <<<"$out"; then
  echo "  ok   the repo's hazard set is derived as exactly twap_1d twap_1h"
  pass=$((pass + 1))
else
  echo "  FAIL the repo's hazard set is not twap_1d twap_1h: ${out##*NULL-start}"
  fail=$((fail + 1))
fi

rb="$(twap runbook_null)"
cat > "$rb/docs/operations/runbooks/twap.md" <<'MD'
2. Re-materialize:

   ```sql
   CALL refresh_continuous_aggregate('twap_1h', NULL, now());
   ```
MD
catches "a runbook's NULL-start TWAP refresh is caught" \
  "$rb" "docs/operations/runbooks/twap.md:4"

rule="$(twap rule_templated)"
cat > "$rule/deploy/monitoring/rules/f.yml" <<'YML'
          description: |
            Fix: `CALL refresh_continuous_aggregate('{{ $labels.view }}', NULL, now());`
YML
catches "an alert annotation's templated NULL-start refresh is caught" \
  "$rule" "deploy/monitoring/rules/f.yml:2"

multi="$(twap multiline)"
cat > "$multi/docs/operations/runbooks/twap.md" <<'MD'
CALL refresh_continuous_aggregate(
       'twap_1h'::regclass,
       NULL, now());
MD
catches "a NULL start split across lines is caught" \
  "$multi" "docs/operations/runbooks/twap.md:1"

named="$(twap named_arg)"
cat > "$named/docs/operations/runbooks/twap.md" <<'MD'
CALL refresh_continuous_aggregate('twap_1h', window_start => NULL, window_end => now());
MD
catches "a named window_start => NULL is caught" \
  "$named" "docs/operations/runbooks/twap.md:1"

warned="$(twap warned)"
cat > "$warned/docs/operations/runbooks/twap.md" <<'MD'
-- DO NOT RUN: CALL refresh_continuous_aggregate('twap_1h', NULL, now());
MD
check "a NULL-start TWAP refresh under DO NOT RUN passes" 0 "$warned"

windowed="$(twap windowed)"
cat > "$windowed/docs/operations/runbooks/twap.md" <<'MD'
CALL refresh_continuous_aggregate(
       'twap_1h',
       '2026-01-01'::timestamptz, '2026-02-01'::timestamptz,
       force => true);
MD
check "a windowed TWAP refresh passes" 0 "$windowed"

unretained="$(twap unretained)"
cat > "$unretained/docs/operations/runbooks/p.md" <<'MD'
CALL refresh_continuous_aggregate('prices_1d', NULL, NULL);
MD
check "a NULL-start refresh of a view over an unretained source passes" 0 "$unretained"

removed="$(twap removed)"
cat > "$removed/migrations/0151_example_unretain.up.sql" <<'SQL'
-- 0151 up — remove the example retention.

BEGIN;
SELECT remove_retention_policy('prices_1m', if_exists => true);
COMMIT;
SQL
cp "$rb/docs/operations/runbooks/twap.md" "$removed/docs/operations/runbooks/twap.md"
check "once a later migration removes the retention the view is no hazard" 0 "$removed"

newmig="$(twap new_migration)"
cat >> "$newmig/migrations/$MIG" <<'SQL'

-- Recreated WITH NO DATA; re-materialize with
-- CALL refresh_continuous_aggregate('twap_1h', NULL, now());
SQL
catches "a migration from 0156 on is held to the rule too" \
  "$newmig" "migrations/$MIG:12  (view: twap_1h)"

# ── the operator corpus: unbounded DML on a compressed hypertable ─────
# The historical defect: scripts/ops/recompute-usd-volume-soroban.sql ran
# this UPDATE, with no `ts` predicate, in one transaction with the
# decompression cap lifted — every trades chunk a result relation.
echo "lint-migration-commands-test: unbounded-DML verdicts"

# comp <name> <ops-sql> — a tree whose migrations make trades a compressed
# hypertable on ts, plus one ops script holding <ops-sql>.
comp() {
  local r
  r="$(mk "$1")"
  cat > "$r/migrations/0149_example_trades.up.sql" <<'SQL'
-- 0149 up — an example compressed hypertable.

BEGIN;
CREATE TABLE trades (ts timestamptz NOT NULL, source text, usd_volume numeric,
                     quote_asset text, quote_amount numeric, ledger bigint);
SELECT create_hypertable(
    'trades',
    'ts',
    chunk_time_interval => INTERVAL '1 day');
CREATE TABLE plain (id bigint, v numeric);
ALTER TABLE trades SET (timescaledb.compress);
SELECT add_compression_policy('trades', INTERVAL '7 days');
COMMIT;
SQL
  mkdir -p "$r/scripts/ops"
  printf '%s\n' "$2" > "$r/scripts/ops/fix.sql"
  echo "$r"
}

unbounded="$(comp unbounded "BEGIN;
UPDATE trades
   SET usd_volume = (quote_amount::numeric / 10000000::numeric)
 WHERE usd_volume IS NULL
   AND quote_asset IN (
     'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75'
   )
   AND quote_amount > 0;
COMMIT;")"
catches "the deleted script's UPDATE with no ts predicate is caught" \
  "$unbounded" "scripts/ops/fix.sql:2  (trades: needs a WHERE on ts)"

ledger="$(comp ledger_only "DELETE FROM trades WHERE source = 'x' AND ledger BETWEEN 1 AND 2;")"
catches "a DELETE bounded by ledger, not the time column, is caught" \
  "$ledger" "scripts/ops/fix.sql:1  (trades: needs a WHERE on ts)"

setonly="$(comp set_only "UPDATE trades SET ts = ts WHERE source = 'x';")"
catches "the time column in SET but not WHERE is caught" \
  "$setonly" "scripts/ops/fix.sql:1  (trades: needs a WHERE on ts)"

bounded="$(comp bounded "UPDATE trades t
   SET usd_volume = t.quote_amount / 10000000
 WHERE t.usd_volume IS NULL
   AND t.ts >= '2026-05-01' AND t.ts < '2026-05-01 01:00';")"
check "the same UPDATE bounded by ts passes" 0 "$bounded"

plain="$(comp plain_table "UPDATE plain SET v = 0 WHERE id = 1;")"
check "DML on a table that is not a compressed hypertable passes" 0 "$plain"

# ── non-vacuity ──────────────────────────────────────────────────────
empty="$TMP/empty"
mkdir -p "$empty/migrations"
check "an empty migrations tree fails rather than passing vacuously" 1 "$empty"

nocomments="$TMP/nocomments"
mkdir -p "$nocomments/migrations"
printf 'BEGIN;\nSELECT 1;\nCOMMIT;\n' > "$nocomments/migrations/$MIG"
check "a migration tree with no header at all fails rather than passing" 1 "$nocomments"

echo "----"
echo "lint-migration-commands-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
