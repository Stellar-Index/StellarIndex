#!/usr/bin/env bash
# lint-lake-dedup-test.sh — fixture tests for the lake-dedup gate
# (scripts/ci/lint-lake-dedup.sh).
#
# The gate exists because three separate reads of an un-merged
# duplicate-bearing archive went wrong in one day and none
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
#     (opTypeStatsQuery, whose own comment records the same class)
#     is CAUGHT;
#   - a GROUP BY over HALF the operations identity is CAUGHT — a
#     partial key is not a collapse;
#   - a sum() beside a uniqExact, under a GROUP BY on the identity, under
#     SELECT DISTINCT or LIMIT 1 BY is CAUGHT: none collapses its input;
#   - COUNT(*), countState(), sumKahan() and a lowercase statement are
#     CAUGHT, and so is a sum() over the three amount tables;
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

# A naive join inside an agent worktree is a stale copy of the tree, not
# a site: the walk prunes .claude so live worktrees neither fail the gate
# nor multiply its file count.
agentwt="$(mk agentwt)"
mkdir -p "$agentwt/.claude/worktrees/agent-x/$GODIR"
cp "$naive/$GODIR/sponsors.go" "$agentwt/.claude/worktrees/agent-x/$GODIR/sponsors.go"
check "a violation inside .claude/worktrees is not scanned" 0 "$agentwt"

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

# ── opTypeStatsQuery: FINAL is what makes count() exact ─
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
# whole identity, passes when it picks one version per key — otherwise the
# gate is just banning GROUP BY.
whole="$(mk whole)"
cat > "$whole/$GODIR/whole.go" <<'GO'
package clickhouse

const perTxFees = `SELECT ledger_seq, tx_index, argMax(fee_charged, ingested_at) AS fee
		FROM stellar.transactions
		WHERE ledger_seq BETWEEN ? AND ?
		GROUP BY ledger_seq, tx_index`
GO
check "a GROUP BY over the WHOLE transactions identity passes" 0 "$whole"

# ── nothing beside the aggregate collapses its input ─────────────────
# Each of these passed the gate once. A group holds every version of its
# key, DISTINCT and LIMIT BY run after aggregation, and an exact uniq says
# nothing about the sum next to it.
probe() { # probe <name> <desc> <table> <sql>
  local d
  d="$(mk "$1")"
  printf '%s\n' "$4" > "$d/$SQLDIR/probe.sql"
  catches "$2" "$d" "$3"
}
probe grp_sum "a sum() grouped on the identity is caught" stellar.transactions \
  'SELECT ledger_seq, tx_index, sum(fee_charged) AS fees FROM stellar.transactions WHERE ledger_seq > 1 GROUP BY ledger_seq, tx_index;'
probe grp_ver "a GROUP BY that adds the version column is caught" stellar.transactions \
  'SELECT sum(fee_charged) AS fees FROM stellar.transactions WHERE ledger_seq > 1 GROUP BY ledger_seq, tx_index, ingested_at;'
probe uniq_sum "a sum() beside a uniqExact over the identity is caught" stellar.transactions \
  'SELECT uniqExact(ledger_seq, tx_index) AS txs, sum(fee_charged) AS fees FROM stellar.transactions WHERE ledger_seq > 1;'
probe distinct "an aggregate under SELECT DISTINCT is caught" stellar.operations \
  'SELECT DISTINCT count() AS n FROM stellar.operations WHERE ledger_seq > 1;'
probe limitby "an aggregate with LIMIT 1 BY the identity is caught" stellar.operations \
  'SELECT ledger_seq, tx_index, op_index, count() AS n FROM stellar.operations GROUP BY ledger_seq, tx_index, op_index LIMIT 1 BY ledger_seq, tx_index, op_index;'

# ── SQL is case-insensitive and aggregates take suffixes ─────────────
probe upper "COUNT(*) is caught" stellar.operations \
  'SELECT COUNT(*) FROM stellar.operations WHERE ledger_seq > 1;'
probe uppersum "SUM() is caught" stellar.transactions \
  'SELECT SUM(fee_charged) FROM stellar.transactions WHERE ledger_seq > 1;'
probe state "countState() is caught" stellar.operations \
  'SELECT source_account, countState() AS n FROM stellar.operations GROUP BY source_account;'
probe kahan "sumKahan() is caught" stellar.transactions \
  'SELECT sumKahan(fee_charged) FROM stellar.transactions WHERE ledger_seq > 1;'
probe lower "a lowercase statement is caught" stellar.operations \
  'select count() from stellar.operations where ledger_seq > 1;'

# ── the amount tables are archives too ───────────────────────────────
probe flows "a sum() over supply_flows is caught" stellar.supply_flows \
  'SELECT contract_id, sum(amount) FROM stellar.supply_flows GROUP BY contract_id;'
probe moves "a sum() over account_movements is caught" stellar.account_movements \
  'SELECT address, sum(amount) FROM stellar.account_movements GROUP BY address;'
probe byasset "a count() over movements_by_asset is caught" stellar.movements_by_asset \
  'SELECT asset, count() FROM stellar.movements_by_asset GROUP BY asset;'

quiet="$(mk quiet_forms)"
cat > "$quiet/$SQLDIR/probe.sql" <<'SQL'
SELECT countDistinct(ledger_seq, tx_index) FROM stellar.transactions WHERE ledger_seq > 1;
SELECT COUNT(DISTINCT tx_hash) FROM stellar.transactions WHERE ledger_seq > 1;
SELECT contract_id, sum(amount) FROM stellar.supply_flows FINAL GROUP BY contract_id;
SELECT sum(fee) FROM (SELECT argMax(fee_charged, ingested_at) AS fee FROM stellar.transactions GROUP BY ledger_seq, tx_index);
SQL
check "countDistinct, FINAL and the two-level form pass" 0 "$quiet"

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

# ── a comment naming GROUP BY does not collapse anything ───
# sql_line() and go_line() only stripped a `--` comment whose LINE
# started with it. An inline trailing comment on an unterminated .sql
# statement, or a full-line `--` doc comment inside a Go raw string, was
# appended to the statement buffer verbatim — so prose mentioning the
# identity's own column names read as a real GROUP BY and greened a
# bare count().
sql_comment_bypass="$(mk sql_comment_bypass)"
cat > "$sql_comment_bypass/$SQLDIR/comment_bypass.sql" <<'SQL'
SELECT count() AS n FROM stellar.transactions WHERE ledger_seq > 1 -- GROUP BY ledger_seq, tx_index
SQL
catches "an inline .sql trailing comment naming GROUP BY does not collapse an unterminated read" \
  "$sql_comment_bypass" stellar.transactions

go_comment_bypass="$(mk go_comment_bypass)"
cat > "$go_comment_bypass/$GODIR/comment_bypass.go" <<'GO'
package clickhouse

const commentBypassQuery = `SELECT count() AS n
		FROM stellar.transactions
		-- GROUP BY ledger_seq, tx_index
		WHERE ledger_seq > 1`
GO
catches "a full-line -- doc comment inside a Go raw string naming GROUP BY does not collapse the read" \
  "$go_comment_bypass" stellar.transactions

# The `;`-terminated sibling must keep passing (FAIL, correctly) too —
# the fix must not depend on whether the statement ever terminates.
sql_comment_terminated="$(mk sql_comment_terminated)"
cat > "$sql_comment_terminated/$SQLDIR/comment_terminated.sql" <<'SQL'
SELECT count() AS n FROM stellar.transactions WHERE ledger_seq > 1; -- GROUP BY ledger_seq, tx_index
SQL
catches "the same inline comment on a terminated statement is also caught" \
  "$sql_comment_terminated" stellar.transactions

# ── the SAME bypass survives inside a header recipe's comment RUN ────
# sql_line()'s comment-run branch (the one that reads a header recipe
# like verify_bad above) stripped only the LEADING `--` that starts the
# comment line and then appended the rest of that line verbatim — so an
# embedded second `--` marker on the SAME prose line, naming a clause
# the recipe is not actually using, was read as a real clause. A flat
# header recipe (unlike a multi-statement file) has no `)` or run
# boundary to end the read before the prose, so this is the common
# shape, not an edge case: deploy/clickhouse/ops_by_source.sql:102
# carries exactly this trailing-comment style on a live header recipe.
header_comment_bypass_groupby="$(mk header_comment_bypass_groupby)"
cat > "$header_comment_bypass_groupby/$SQLDIR/header_bypass_groupby.sql" <<'SQL'
-- Step 3 - count transactions:
--   SELECT count() AS n FROM stellar.transactions WHERE ledger_seq > 1 -- GROUP BY ledger_seq, tx_index
SQL
catches "a header recipe's own trailing comment naming GROUP BY does not collapse the read it documents" \
  "$header_comment_bypass_groupby" stellar.transactions

header_comment_bypass_uniq="$(mk header_comment_bypass_uniq)"
cat > "$header_comment_bypass_uniq/$SQLDIR/header_bypass_uniq.sql" <<'SQL'
-- Step 4 - count transactions:
--   SELECT count() AS n FROM stellar.transactions WHERE ledger_seq > 1 -- uniqExact(ledger_seq, tx_index)
SQL
catches "a header recipe's own trailing comment naming uniqExact does not collapse the read it documents" \
  "$header_comment_bypass_uniq" stellar.transactions

# The un-terminated CODE-line sibling (no leading `--`, i.e. the
# comment-run branch never engages) must still be caught by the
# code-line fix landed earlier — regression guard that the two branches
# stay in lockstep.
header_comment_bypass_codeline="$(mk header_comment_bypass_codeline)"
cat > "$header_comment_bypass_codeline/$SQLDIR/header_bypass_codeline.sql" <<'SQL'
SELECT count() AS n FROM stellar.transactions WHERE ledger_seq > 1 -- GROUP BY ledger_seq, tx_index
SQL
catches "the code-line sibling of the same bypass stays caught" \
  "$header_comment_bypass_codeline" stellar.transactions

# ── the strip must be MARKER-aware, not a bare `index("--")` ─────────
# A prior attempt at this fix cut at the first `--` ANYWHERE in the
# line, which erases a `--` that is content rather than a comment
# marker — a CLI flag glued into a Go raw string, or a quoted literal
# in a .sql statement — taking the table name and the aggregate with
# it and silently swallowing the violation. Both shapes below carry a
# real, uncollapsed count() over a lake table that a marker-unaware
# strip erases; each tree also carries the ordinary compliant detail
# reader from mk(), so the examined==0 floor cannot mask the loss.
go_marker_content="$(mk go_marker_content)"
cat > "$go_marker_content/$GODIR/marker_content.go" <<'GO'
package clickhouse

const opsCountCLI = `clickhouse-client --port 9300 -q "SELECT count() FROM stellar.operations WHERE ledger_seq > 1"`
GO
catches "a CLI flag's -- inside a Go raw string does not erase the read it precedes" \
  "$go_marker_content" stellar.operations

sql_marker_content="$(mk sql_marker_content)"
cat > "$sql_marker_content/$SQLDIR/marker_content.sql" <<'SQL'
SELECT count() AS n, '--' AS marker FROM stellar.operations WHERE ledger_seq > 1;
SQL
catches "a quoted '--' literal in a .sql statement does not erase the read that follows it" \
  "$sql_marker_content" stellar.operations

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

# ── the aggregate/collapse floor (the canary) ─────────────────────────
# `examined == 0` proves the FROM/JOIN extraction is alive, but
# nothing proved the AGGREGATE half (mult_agg()) was — a regression
# there stops every real read from registering as aggregating, and a
# tree that legitimately aggregates nothing (like "good" above) looks
# identical from the outside. Simulate that regression by blinding
# mult_agg() in a throwaway copy of the gate: it must die on its own
# canary rather than print a clean "0 aggregating", and the SAME tree
# must still pass on the real, unblinded gate — the floor must not cost
# a false positive on a genuinely quiet tree.
blinded="$TMP/lint-lake-dedup-blinded.sh"
sed '/function mult_agg(s) {/a\
      return 0
' "$LINT" > "$blinded"
blind="$(mk blind)"
blind_out="$(bash "$blinded" "$blind" 2>&1)"
blind_got=$?
if [ "$blind_got" -ne 0 ] && grep -q 'did not flag its own canary' <<<"$blind_out"; then
  echo "  ok   a blinded aggregate classifier dies on its own canary, not a silent 0-aggregating pass"
  pass=$((pass + 1))
else
  echo "  FAIL a blinded aggregate classifier dies on its own canary (exit $blind_got)"
  fail=$((fail + 1))
fi
check "the same aggregates-nothing tree still passes on the real gate" 0 "$blind"

# ── which file a read belongs to, and where a raw literal ends ───────
# A file's last literal is analysed when the next file starts. Charged to
# that next file, its baseline entry would absorb the violation.
lastlit="$(mk lastlit)"
cat > "$lastlit/$GODIR/aaa_last.go" <<'GO'
package clickhouse

const entriesTotalSQL = `SELECT sum(amount) FROM stellar.account_movements WHERE address = ?`
GO
cat > "$lastlit/$GODIR/bbb_next.go" <<'GO'
package clickhouse

const nothing = 1
GO
out="$(bash "$LINT" "$lastlit" 2>&1)"
if grep -q 'aaa_last.go' <<<"$out" && ! grep -q 'bbb_next.go' <<<"$out"; then
  echo "  ok   a file's last literal is charged to that file"
  pass=$((pass + 1))
else
  echo "  FAIL a file's last literal is charged to that file"
  fail=$((fail + 1))
fi

# One lone backtick inside an interpreted string must not turn the rest
# of the file inside out.
tick="$(mk tick)"
cat > "$tick/$GODIR/quote.go" <<'GO'
package clickhouse

import "strings"

func quoteIdent(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

const feeBoard = `SELECT source_account, sum(fee_charged) AS fees
		FROM stellar.transactions GROUP BY source_account ORDER BY fees DESC`
GO
catches "a backtick in an interpreted string does not hide the next literal" "$tick" stellar.transactions

runetick="$(mk runetick)"
cat > "$runetick/$GODIR/rune.go" <<'GO'
package clickhouse

func isTick(r rune) bool { return r == '`' }

const opCount = `SELECT count() FROM stellar.operations WHERE ledger_seq > ?`
GO
catches "a backtick rune literal does not hide the next literal" "$runetick" stellar.operations

strquiet="$(mk strquiet)"
cat > "$strquiet/$GODIR/prose.go" <<'GO'
package clickhouse

// Help text that quotes a query is prose, not a read.
const help = "use `SELECT count() FROM stellar.operations` only with FINAL"
GO
check "a quoted query inside an interpreted string is not a read" 0 "$strquiet"

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

unwatched="$(mk unwatched)"
mkdir -p "$unwatched/scripts/ci"
printf '%s\n' "$GODIR/detail_reader.go:stellar.not_a_watched_table  the base copy of the gate does not watch this table yet" \
  > "$unwatched/scripts/ci/lint-lake-dedup.baseline"
check "a baseline entry for a table this copy does not watch is not stale" 0 "$unwatched"

# ── non-vacuity ──────────────────────────────────────────────────────
# Reads the first version of the gate did not attribute to their own select.
shape() { # shape <name> <sql> — a fixture tree whose extra read is <sql>
  local r
  r="$(mk "$1")"
  # shellcheck disable=SC2016 # a Go raw-literal backtick, not a shell expansion
  printf 'package clickhouse\n\nconst q = `%s`\n' "$2" > "$r/$GODIR/shape.go"
  echo "$r"
}

catches "a lake table after a comma (implicit cross join) is caught" \
  "$(shape comma 'SELECT sum(t.fee_charged) FROM stellar.ledgers l, stellar.transactions t WHERE t.ledger_seq = l.ledger_seq')" \
  stellar.transactions

catches "an aggregate in HAVING, after the read, is caught" \
  "$(shape having 'SELECT source_account FROM stellar.operations GROUP BY source_account HAVING count() > 5')" \
  stellar.operations

catches "an aggregate in ORDER BY, after the read, is caught" \
  "$(shape orderby 'SELECT source_account FROM stellar.operations GROUP BY source_account ORDER BY sum(op_index) DESC')" \
  stellar.operations

catches "a join after a closed subquery belongs to the outer aggregate" \
  "$(shape closedsub 'SELECT count() FROM (SELECT account FROM stellar.account_activity) a JOIN stellar.operations o ON o.source_account = a.account')" \
  stellar.operations

catches "a count() over contract_events is caught" \
  "$(shape events 'SELECT contract_id, count() FROM stellar.contract_events WHERE ledger_seq > 5 GROUP BY contract_id')" \
  stellar.contract_events

check "a scalar subquery aggregate in the select list is not this read" 0 \
  "$(shape scalar 'SELECT ledger_seq, (SELECT count() FROM stellar.ledgers) AS n FROM stellar.operations WHERE ledger_seq = 5')"

check "a count() above a collapsing subquery still passes" 0 \
  "$(shape above 'SELECT count() FROM (SELECT ledger_seq, tx_index, op_index FROM stellar.operations GROUP BY ledger_seq, tx_index, op_index)')"

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
