#!/usr/bin/env bash
# lint-lake-dedup-test.sh — fixture tests for the lake-dedup gate
# (scripts/ci/lint-lake-dedup.sh).
#
# The gate exists because three separate reads of an un-merged
# duplicate-bearing archive went wrong in one day (2026-09-07) and none
# of them was visible in a total. A gate against that class is only
# worth its line count if it fails on the shapes that reproduce it, so
# the verdicts are pinned here against the ACTUAL defects rather than
# against invented ones:
#
#   - THE NAIVE SPONSORS JOIN that inflated its top account from
#     176,573 to 296,799 is CAUGHT — on BOTH sides of the join, because
#     a join condition collapses neither;
#   - the SHIPPED replacement (GROUP BY the operation identity,
#     argMax(t.successful, t.ingested_at)) PASSES;
#   - THE VERIFICATION QUERY that reported 489,253 rows dropped that
#     were not is CAUGHT in a .sql header, and its countDistinct form
#     passes;
#   - dropping `FINAL` from the tree's one aggregating read
#     (opTypeStatsQuery, whose own comment records the same class as
#     audit C2-12) is CAUGHT;
#   - a GROUP BY over HALF the operations identity is CAUGHT — a
#     partial key is not a collapse;
#   - the five per-entity detail readers' shape is NOT flagged, nor is
#     a max()-only rollup, nor a count() above a collapsing subquery,
#     nor a SELECT DISTINCT sample, nor prose;
#   - the baseline needs a reason, silences the site it names, and
#     fails STALE once the site stops violating;
#   - a tree naming no lake table FAILS rather than passing vacuously.
#
# Run: bash scripts/ci/lint-lake-dedup-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-lake-dedup.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
check() { # check <desc> <want-exit> <root>
  local desc="$1" want="$2" root="$3" got
  bash "$LINT" "$root" >/dev/null 2>&1
  got=$?
  if [ "$got" -eq "$want" ]; then
    echo "  ok   $desc"
    pass=$((pass + 1))
  else
    echo "  FAIL $desc (exit $got, want $want)"
    fail=$((fail + 1))
  fi
}

# catches <desc> <root> <table> — the gate fails AND names the table, so
# a pass-by-accident on some other site cannot masquerade as coverage.
catches() {
  local desc="$1" root="$2" table="$3" out got
  out="$(bash "$LINT" "$root" 2>&1)"
  got=$?
  # A here-string, not a pipe: `grep -q` closes its input on the first
  # match, and under a pipefail shell the SIGPIPE'd writer would decide
  # the verdict (lint-shell-sigpipe.sh).
  if [ "$got" -ne 0 ] && grep -q "  .*$table\$" <<<"$out"; then
    echo "  ok   $desc"
    pass=$((pass + 1))
  else
    echo "  FAIL $desc (exit $got, did not report a violation on $table)"
    fail=$((fail + 1))
  fi
}

GODIR=internal/storage/clickhouse
SQLDIR=deploy/clickhouse

# mk <name> — a tree whose ONLY lake read is a compliant detail reader,
# so every fixture below adds exactly the shape it is testing.
mk() {
  local r="$TMP/$1"
  mkdir -p "$r/$GODIR" "$r/$SQLDIR"
  cat > "$r/$GODIR/detail_reader.go" <<'GO'
package clickhouse

// A per-entity detail reader returns what the ledger contained, row for
// row. No aggregate reads it, so the gate has nothing to say about it —
// this is the shape of the five readers the gate must stay quiet on.
const recentOperationsQuery = `SELECT ledger_seq, tx_index, op_index, op_type
		FROM stellar.operations
		WHERE ledger_seq > ?
		ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC
		LIMIT ?`
GO
  echo "$r"
}

echo "lint-lake-dedup-test: collapse verdicts on duplicate-bearing lake reads"

check "the repo's own tree passes" 0 "$PWD"

good="$(mk good)"
check "a tree whose only read is a detail reader passes" 0 "$good"

# ── THE NAIVE SPONSORS JOIN (176,573 → 296,799) ──────────────────────
naive="$(mk naive)"
cat > "$naive/$GODIR/sponsors.go" <<'GO'
package clickhouse

const sponsorsBoard = `SELECT o.source_account AS sponsor, count() AS sponsorships_started
	 FROM stellar.operations AS o
	 INNER JOIN stellar.transactions AS t
	    ON t.ledger_seq = o.ledger_seq AND t.tx_index = o.tx_index
	  WHERE o.ledger_seq BETWEEN ? AND ?
	    AND t.successful = 1
	  GROUP BY sponsor
	  ORDER BY sponsorships_started DESC`
GO
catches "the naive sponsors join is caught on the operations side" "$naive" stellar.operations
catches "the naive sponsors join is caught on the transactions side" "$naive" stellar.transactions

# ── THE SHIPPED REPLACEMENT ──────────────────────────────────────────
# Group on the operation identity and resolve the joined transaction's
# flag with argMax over the version column. This is the form on main.
fixed="$(mk fixed)"
cat > "$fixed/$GODIR/sponsors.go" <<'GO'
package clickhouse

const opBegin = "OperationTypeBeginSponsoringFutureReserves"

var sponsorsRollupStatements = []string{
	`INSERT INTO stellar.account_sponsors_ops (lseq, tidx, oidx, otype, src)
	 SELECT o.ledger_seq, o.tx_index, o.op_index,
	        argMax(o.op_type, o.ingested_at) AS otype,
	        argMax(o.source_account, o.ingested_at) AS src
	 FROM stellar.transactions AS t
	 INNER JOIN (
	     SELECT ledger_seq, tx_index, op_index, op_type, source_account, ingested_at
	     FROM stellar.operations
	     WHERE ledger_seq BETWEEN ? AND ?
	       AND op_type IN ('` + opBegin + `')
	 ) AS o ON t.ledger_seq = o.ledger_seq AND t.tx_index = o.tx_index
	 WHERE t.ledger_seq BETWEEN ? AND ?
	 GROUP BY o.ledger_seq, o.tx_index, o.op_index
	 HAVING argMax(t.successful, t.ingested_at) = 1`,
}
GO
check "the shipped argMax + identity GROUP BY replacement passes" 0 "$fixed"

# ── THE VERIFICATION QUERY (489,253 rows "dropped" that were not) ────
verify_bad="$(mk verify_bad)"
cat > "$verify_bad/$SQLDIR/reconcile.sql" <<'SQL'
-- ── Step 3: verify ──────────────────────────────────────────────────
-- The window's operation count must equal what the projection landed:
--
--   SELECT count() AS ops
--     FROM stellar.operations
--    WHERE ledger_seq BETWEEN 63000000 AND 63099999;
--
-- Expect equality.
SQL
catches "a header verification query counting operations undeduplicated is caught" \
  "$verify_bad" stellar.operations

verify_ok="$(mk verify_ok)"
cat > "$verify_ok/$SQLDIR/reconcile.sql" <<'SQL'
-- ── Step 3: verify ──────────────────────────────────────────────────
--
--   SELECT uniqExact((ledger_seq, tx_index, op_index)) AS ops
--     FROM stellar.operations
--    WHERE ledger_seq BETWEEN 63000000 AND 63099999;
--
-- Expect equality.
SQL
check "the countDistinct/uniqExact form of the same query passes" 0 "$verify_ok"

# ── opTypeStatsQuery: FINAL is what makes count() exact (audit C2-12) ─
final_ok="$(mk final_ok)"
cat > "$final_ok/$GODIR/op_type_stats.go" <<'GO'
package clickhouse

const opTypeStatsQuery = `SELECT op_type, toInt64(count()) AS c
		FROM stellar.operations FINAL
		WHERE ledger_seq > (SELECT max(ledger_seq) FROM stellar.operations) - ?
		GROUP BY op_type
		ORDER BY c DESC`
GO
check "count() over the archive WITH FINAL passes" 0 "$final_ok"

final_gone="$(mk final_gone)"
sed 's/ FINAL$//' "$final_ok/$GODIR/op_type_stats.go" > "$final_gone/$GODIR/op_type_stats.go"
grep -q 'FROM stellar.operations$' "$final_gone/$GODIR/op_type_stats.go" \
  || echo "  (fixture: FINAL was not removed)"
catches "the same query with FINAL removed is caught" "$final_gone" stellar.operations

# ── a partial key is not a collapse ──────────────────────────────────
partial="$(mk partial)"
cat > "$partial/$GODIR/partial.go" <<'GO'
package clickhouse

const perTxOpCounts = `SELECT ledger_seq, tx_index, count() AS ops
		FROM stellar.operations
		WHERE ledger_seq BETWEEN ? AND ?
		GROUP BY ledger_seq, tx_index`
GO
catches "a GROUP BY over HALF the operations identity is caught" "$partial" stellar.operations

# The same shape on transactions, where (ledger_seq, tx_index) IS the
# whole identity, must pass — otherwise the gate is just banning GROUP BY.
whole="$(mk whole)"
cat > "$whole/$GODIR/whole.go" <<'GO'
package clickhouse

const perTxFees = `SELECT ledger_seq, tx_index, argMax(fee_charged, ingested_at) AS fee, count() AS n
		FROM stellar.transactions
		WHERE ledger_seq BETWEEN ? AND ?
		GROUP BY ledger_seq, tx_index`
GO
check "a GROUP BY over the WHOLE transactions identity passes" 0 "$whole"

# ── the collapse belongs to ONE query block ──────────────────────────
# A UNION arm that collapses must not excuse the arm beside it: block
# scoping is what makes the per-read verdict mean anything.
union="$(mk union)"
cat > "$union/$GODIR/union.go" <<'GO'
package clickhouse

const perWindowCounts = `SELECT count() AS c FROM stellar.operations WHERE ledger_seq BETWEEN ? AND ?
	UNION ALL
	SELECT uniqExact((ledger_seq, tx_index, op_index)) AS c
	  FROM stellar.operations WHERE ledger_seq BETWEEN ? AND ?`
GO
catches "an uncollapsed UNION arm beside a collapsed one is caught" \
  "$union" stellar.operations

# FINAL reached through an alias is still FINAL.
aliased="$(mk aliased)"
cat > "$aliased/$GODIR/aliased.go" <<'GO'
package clickhouse

const windowOps = `SELECT count() AS c
		FROM stellar.operations AS o FINAL
		WHERE o.ledger_seq BETWEEN ? AND ?`
GO
check "FINAL reached through an alias passes" 0 "$aliased"

# An EXECUTABLE .sql statement, not only a header recipe.
stmt="$(mk stmt)"
cat > "$stmt/$SQLDIR/rollup.sql" <<'SQL'
INSERT INTO stellar.tx_counts
SELECT count() AS n FROM stellar.transactions WHERE ledger_seq > 1;
SQL
catches "an executable .sql statement is checked, not only header prose" \
  "$stmt" stellar.transactions

# ── shapes the gate must stay quiet on ───────────────────────────────
maxonly="$(mk maxonly)"
cat > "$maxonly/$SQLDIR/account_activity.sql" <<'SQL'
CREATE MATERIALIZED VIEW IF NOT EXISTS stellar.account_activity_ops_mv
TO stellar.account_activity AS
SELECT source_account AS account_id, max(ledger_seq) AS last_ledger, max(close_time) AS last_seen
FROM stellar.operations
WHERE source_account != ''
GROUP BY account_id;
SQL
check "a max()-only watermark rollup is not flagged" 0 "$maxonly"

nested="$(mk nested)"
cat > "$nested/$GODIR/nested.go" <<'GO'
package clickhouse

const distinctTxCount = `SELECT count() AS n FROM (
		SELECT ledger_seq, tx_index
		  FROM stellar.transactions
		 WHERE ledger_seq BETWEEN ? AND ?
		 GROUP BY ledger_seq, tx_index)`
GO
check "count() above a subquery that already collapses is not flagged" 0 "$nested"

sample="$(mk sample)"
cat > "$sample/$SQLDIR/sample.sql" <<'SQL'
-- ── Step 3: verify ──────────────────────────────────────────────────
--
--   SELECT countIf(a != b) FROM (
--     SELECT
--       (SELECT countDistinct(ledger_seq, tx_index) FROM stellar.transactions WHERE source_account = t.sa) AS a,
--       (SELECT countDistinct(ledger_seq, tx_index) FROM stellar.ops_by_source
--         WHERE source_account = t.sa AND op_index = 4294967295) AS b,
--       sa
--     FROM (SELECT DISTINCT source_account AS sa FROM stellar.transactions
--           WHERE ledger_seq > (SELECT max(ledger_seq) - 10000 FROM stellar.ledgers)
--           LIMIT 20) t)
--
-- Expect 0.
SQL
check "the shipped countDistinct + SELECT DISTINCT verify recipe passes" 0 "$sample"

prose="$(mk prose)"
cat > "$prose/$GODIR/prose.go" <<'GO'
package clickhouse

// The gate is a join to stellar.transactions on the full (ledger_seq,
// tx_index) transaction identity, because stellar.operations carries no
// success column and a bare count() over either would inflate.
//
//	SELECT count() FROM stellar.operations WHERE ledger_seq > 0
const unrelated = `SELECT 1`
GO
check "prose and a Go comment quoting the bad query are not flagged" 0 "$prose"

# ── baseline mechanics ───────────────────────────────────────────────
based="$(mk based)"
cp "$naive/$GODIR/sponsors.go" "$based/$GODIR/sponsors.go"
mkdir -p "$based/scripts/ci"
cat > "$based/scripts/ci/lint-lake-dedup.baseline" <<'BASE'
# Grandfathered sites, shrink-only.
internal/storage/clickhouse/sponsors.go:stellar.operations    board rewrite lands in the next change
internal/storage/clickhouse/sponsors.go:stellar.transactions  board rewrite lands in the next change
BASE
check "a baseline entry with a reason silences the site it names" 0 "$based"

noreason="$(mk noreason)"
cp "$naive/$GODIR/sponsors.go" "$noreason/$GODIR/sponsors.go"
mkdir -p "$noreason/scripts/ci"
printf '%s\n' \
  'internal/storage/clickhouse/sponsors.go:stellar.operations' \
  > "$noreason/scripts/ci/lint-lake-dedup.baseline"
check "a baseline entry with no reason is refused" 1 "$noreason"

stale="$(mk stale)"
mkdir -p "$stale/scripts/ci"
printf '%s\n' \
  'internal/storage/clickhouse/detail_reader.go:stellar.operations  no longer aggregating' \
  > "$stale/scripts/ci/lint-lake-dedup.baseline"
check "a stale baseline entry fails until it is deleted" 1 "$stale"

# ── non-vacuity ──────────────────────────────────────────────────────
empty="$TMP/empty"
mkdir -p "$empty/$GODIR"
cat > "$empty/$GODIR/nothing.go" <<'GO'
package clickhouse

const q = `SELECT 1`
GO
check "a tree naming no lake table fails rather than passing vacuously" 1 "$empty"

echo "----"
echo "lint-lake-dedup-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
