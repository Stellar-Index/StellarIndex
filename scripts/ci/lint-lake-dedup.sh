#!/usr/bin/env bash
# lint-lake-dedup.sh — an AGGREGATING read of a duplicate-bearing lake
# table must read it through FINAL, or sit above a subquery that collapses it.
#
# `stellar.transactions`, `stellar.operations` and the amount-carrying
# `stellar.supply_flows`, `stellar.account_movements`,
# `stellar.movements_by_asset` and `stellar.contract_events` are
# ReplacingMergeTree(ingested_at) archives whose duplicates are not merged away
# in BULK: on r1 every `(ledger_seq, tx_index)` key in `stellar.transactions`
# has carried more than one row, and `stellar.operations` runs at 2.0x. A
# `count()`, `sum()` or `groupArray()` over either reports a multiple of the
# truth, and a join to either MULTIPLIES the other side by each key's duplicate
# count.
#
# WHY A GATE: a naive join inflates a leaderboard, an undeduplicated count
# reports rows "dropped" that were not, and a bare equality on the success flag
# needs `argMax(t.successful, t.ingested_at)` over a GROUP BY on the operation
# identity instead. None shows in a total: an inflated board still sums to a
# plausible number and a "dropped rows" reconciliation balances against itself.
#
# ── THE RULE ──────────────────────────────────────────────────────────
#
# A read of one of these tables (after FROM, JOIN or a comma) is IN SCOPE only
# when its OWN query block (select list, HAVING, ORDER BY; nested selects
# excluded) applies a MULTIPLICITY-SENSITIVE aggregate (value changes when a row is
# repeated), matched in any letter case and with any combinator suffix
# (`COUNT(*)`, `sumIf`, `countState`, `sumKahan`):
#
#     count* sum* avg* groupArray* array_agg group_concat topK* corr*
#     covar[_]pop* covar[_]samp* var[_]pop* var[_]samp* quantile* median*
#     stddev* deltaSum* exponential* histogram simpleLinearRegression
#     stochastic* groupBitXor entropy skew* kurt* sequenceCount sparkbar
#     contingency cramersV theilsU rankCorr maxIntersections *TTest
#     mannWhitneyUTest
#
# NOT in that set (idempotent under an exact duplicate, and the tree's own
# collapse vocabulary): `min`, `max`, `any`, `anyIf`, `argMax`, `argMin`,
# `uniq`, `uniqExact`, `uniqCombined`, `countDistinct`, `groupUniqArray`.
#
# An in-scope read must carry `FINAL` on the read itself. Nothing else in the
# same SELECT collapses the rows the aggregate consumes:
#
#   * a `GROUP BY` over the identity makes each group hold every version of
#     one key, so `sum()` in that SELECT still adds each version;
#   * `LIMIT 1 BY` and `SELECT DISTINCT` run after aggregation;
#   * a `uniqExact` over the identity is exact itself and says nothing about
#     the `sum()` beside it.
#
# The other honest form is two-level: an inner SELECT that picks one version
# per key (`argMax(col, ingested_at) … GROUP BY <identity>`, no
# multiplicity-sensitive aggregate) with the aggregate above it. That inner
# read is out of scope by the rule above.
#
# ── WHAT THIS DELIBERATELY DOES NOT FLAG ─────────────────────────────
#
# Per-entity DETAIL readers (explorer_reader.go, sdex_op_reader.go,
# contract_call_op_reader.go, classic_movement_reader.go,
# participant_backfill.go) legitimately return the ledger row for row; they
# carry `FINAL` or `LIMIT 1 BY` where duplicates would be visible. Keying scope
# on the aggregate, not the table, separates "this number is a multiple of the
# truth" from "this listing returns rows".
#
# Also not flagged, each a decision:
#
#   * SQL in a double-quoted Go string is not analysed; production Go that
#     writes a lake read that way FAILS (use a raw literal), `stellar.%s`
#     included, as does a raw read whose table is a bare `%s` verb or a
#     spliced identifier. Test files are exempt: they quote fragments in
#     assertions. A raw `stellar.%s` read IS analysed, as table `stellar.%s`:
#     the table is unknown here, so an aggregating one needs FINAL or a
#     baseline entry. A file is a subject when it names a watched table,
#     `stellar.%s`, or a `FROM %s` verb (outside internal/storage/timescale,
#     the Postgres tier), so a constant defined in another file cannot hide
#     its readers. A `+ name +` splice of a raw-literal constant from the
#     same package is analysed with the constant's text. Not seen: a Sprintf
#     verb in a select list, a function-call splice, and a statement split
#     across separate builder calls.
#   * Go `//` comments: prose ABOUT the query beside them, constantly quoting
#     fragments. Operator recipes live in `.sql` headers, which ARE read (see
#     deploy/clickhouse/ops_by_source.sql Step 3, whose
#     `countDistinct(ledger_seq, tx_index)` exists because verify flagged 20 of
#     20 accounts on this class).
#   * a read whose owning select applies only duplicate-idempotent aggregates
#     (`SELECT source_account, max(ledger_seq) … GROUP BY source_account`).
#   * a statement whose aggregate sits ABOVE a subquery that already collapses.
#     The read belongs to the innermost select, so `count() FROM (SELECT … FROM
#     stellar.operations GROUP BY ledger_seq, tx_index, op_index)` is out of scope.
#     The gate does not check that the inner GROUP BY is the full key. A
#     subquery or CTE with no GROUP BY, DISTINCT, LIMIT 1 BY or idempotent
#     aggregate collapses nothing: its read is in scope when the block
#     consuming it aggregates, and needs FINAL like any other.
#
# ── ESCAPE HATCH ─────────────────────────────────────────────────────
#
# scripts/ci/lint-lake-dedup.baseline: one
# `<file>:<key>  <N> read(s): #<fingerprint> <reason>` entry per grandfathered
# file and table (or `#interp`), reason REQUIRED. The count and fingerprint
# name the excused statements, so a further, missing or swapped read fails;
# the gate prints the entry to copy. Growing or changing it needs a
# `Baseline-Growth:` commit trailer (lint-baseline-growth.sh), and a stale
# entry (a site that no longer violates) fails the gate until deleted.
#
# Usage: lint-lake-dedup.sh [repo-root]
#   The optional root is for scripts/ci/lint-lake-dedup-test.sh (fixture trees).
#
# Exit 0 clean, non-zero on any violation.
set -euo pipefail
cd "${1:-$(dirname "$0")/../..}"

BASELINE="scripts/ci/lint-lake-dedup.baseline"
# Every ReplacingMergeTree in the `stellar` database (deploy/clickhouse/*.sql,
# tier1_schema.sql, the Go DDL in supply_flows.go / account_movements.go):
# each holds superseded versions of a key until a merge that may never run,
# so none is exempt by table. Rollups and state tables are listed as well as
# the archives; their sanctioned aggregating reads sit in the baseline with
# the reason each is safe.
LAKE_TABLES="ledgers|transactions|operations|operation_results|operation_participants"
LAKE_TABLES="$LAKE_TABLES|contract_events|ledger_entry_changes|supply_flows|tx_hash_index"
LAKE_TABLES="$LAKE_TABLES|ledger_entries_current|ledger_entries_current_v2|ttl_live_until"
LAKE_TABLES="$LAKE_TABLES|account_movements|movements_by_asset|ops_by_source|account_op_types"
LAKE_TABLES="$LAKE_TABLES|account_activity|contract_active_ledgers|contract_instance_changes"
LAKE_TABLES="$LAKE_TABLES|contract_instance_changes_v2|sponsorship_events|sponsorship_totals_buckets"
LAKE_TABLES="$LAKE_TABLES|lending_state_hourly|account_entry_ledgers|account_claimables"
LAKE_TABLES="$LAKE_TABLES|claimable_removals|asset_entry_ledgers|trades_by_account"
LAKE_TABLES="$LAKE_TABLES|trades_by_account_windows|cap67_movements_watermark|entry_history_watermark"
# A Sprintf verb in the table position (`%s`, `%[1]s`): the table is unknown,
# so it is watched. Bracket expressions, not `\[`, so awk -v keeps it intact.
VERB_RE='%([[][0-9]+[]])?[sqv]'
READ_TABLES="${LAKE_TABLES}|${VERB_RE}"

die() { echo "lint-lake-dedup: FAIL — $*" >&2; exit 1; }

# LAKE_TABLES must cover every ReplacingMergeTree the DDL creates, or the next
# one is unwatched by default. Fixture trees carry no DDL and are skipped; the
# real tree (tier1_schema.sql present) must derive at least one table.
if [ -d deploy/clickhouse ] || [ -d internal/storage/clickhouse ]; then
  ddl_files=()
  while IFS= read -r f; do ddl_files+=("$f"); done < <(
    find deploy/clickhouse internal/storage/clickhouse -maxdepth 1 \
      \( -name '*.sql' -o -name '*.go' \) ! -name '*_test.go' 2>/dev/null | sort)
  rmt_tables=""
  if [ "${#ddl_files[@]}" -gt 0 ]; then
    # Each file is judged as one whitespace-collapsed string split at CREATE,
    # so a multi-line, `=`-less, tab-separated or backtick-quoted DDL derives
    # the same as the one-line form. `%s.` is the Go DDL's Sprintf database.
    rmt_tables="$(LC_ALL=C awk '
      function scan(t,    n, p, k, c) {
        n = split(t, p, /create /)
        for (k = 2; k <= n; k++) {
          c = p[k]; sub(/;.*$/, "", c)
          if (!match(c, /^(or replace )?table (if not exists )?(stellar|%[sv]) ?\. ?[a-z0-9_]+/)) continue
          if (c !~ /engine ?=? ?(replicated|shared)?replacingmergetree/) continue
          c = substr(c, RSTART, RLENGTH); sub(/^.*\. ?/, "", c); print c
        }
      }
      FNR == 1 && NR > 1 { scan(t); t = "" }
      # Comments go first: a ";" or "create " in a column comment would
      # otherwise end or split the CREATE body before its ENGINE clause.
      { l = tolower($0); sub(/--.*$/, "", l)
        if (FILENAME ~ /\.go$/) sub(/\/\/.*$/, "", l)
        gsub(/[`"\t\r]/, " ", l)
        while (l ~ /  /) gsub(/  /, " ", l)
        sub(/^ /, "", l); sub(/ $/, "", l); if (l != "") t = t " " l }
      END { scan(t) }
    ' "${ddl_files[@]}" | sort -u)"
  fi
  if [ -z "$rmt_tables" ] && [ -f deploy/clickhouse/tier1_schema.sql ]; then
    die "derived no ReplacingMergeTree table from deploy/clickhouse or internal/storage/clickhouse.
  A drift check that derives nothing passes forever. Fix the derivation, do not delete the check."
  fi
  for t in $rmt_tables; do
    case "|${LAKE_TABLES}|" in
      *"|${t}|"*) : ;;
      *) die "stellar.${t} is a ReplacingMergeTree in the DDL but is not in LAKE_TABLES.
  Add it to LAKE_TABLES in scripts/ci/lint-lake-dedup.sh so its aggregating reads are watched." ;;
    esac
  done
fi

# Subject files: every tracked-or-untracked .go/.sql that NAMES a
# duplicate-bearing table (others cannot violate; the pre-filter keeps this to
# one awk pass over ~60 files).
#
# `find` not `git ls-files`: fixture trees are not checkouts. The prunes mirror
# lint-imports.sh's SKIP_DIRS plus .claude, where agent worktrees put whole
# repo copies in the walk.
subjects=()
verb_only="|"
# shellcheck disable=SC2016  # backticks are Go raw-string delimiters
while IFS= read -r f; do
  [ -n "$f" ] || continue
  case "$f" in
    '>'*) f="${f#>}"; verb_only="${verb_only}${f}|" ;;
  esac
  subjects+=("$f")
done < <(
  find . \( -name .git -o -name vendor -o -name node_modules \
            -o -name .discovery-repos -o -name .claude \) -prune -o \
       -type f \( -name '*.go' -o -name '*.sql' \) -print \
    | sed 's#^\./##' \
    | sort \
    | while IFS= read -r f; do
        if grep -qiE "stellar\\.(${READ_TABLES})([^a-z0-9_]|\$)" "$f"; then
          echo "$f"
        elif [[ "$f" == *.go && "$f" != *_test.go && "$f" != internal/storage/timescale/* ]] &&
             grep -qiE '(from|join)[ ]+%(\[[0-9]+\])?[sqv]|(from|join)[ ]*["`][ ]*\+|stellar\.["`][ ]*\+' "$f"; then
          # names no watched table: its table comes from elsewhere
          echo ">$f"
        fi
      done
)

if [ "${#subjects[@]}" -eq 0 ]; then
  die "no .go/.sql file names a watched lake table (stellar.{${LAKE_TABLES}}).
  A gate with an empty subject set passes forever. If the lake tables were
  renamed, rename them here too — do not delete the check."
fi

# ── canary ───────────────────────────────────────────────────────────
#
# `examined == 0` below proves the FROM/JOIN extraction still matches, but not
# that the AGGREGATE half works: a mult_agg()
# regression would report a clean "0 aggregating", indistinguishable from a tree
# that aggregates nothing. One synthetic, guaranteed-uncollapsed aggregating
# read goes through the SAME analyser and must come back flagged.
canary_dir="$(mktemp -d "${TMPDIR:-/tmp}/lint-lake-dedup-canary.XXXXXX")"
trap 'rm -rf "$canary_dir"' EXIT
canary_file="$canary_dir/canary.sql"
cat > "$canary_file" <<'SQL'
SELECT count() AS canary FROM stellar.transactions WHERE ledger_seq > 1;
SQL

# ── Spliced constants ────────────────────────────────────────────────
#
# `<dir>\t<name>\t<text>` for every raw-literal `name = `…`` (const, var or :=)
# in the non-test Go of each subject's package, so `+ aggCols +` is analysed
# with the SQL it stands for: an aggregate or a lake read held in a constant
# is judged inside the statement that splices it.
defs_file="$canary_dir/defs"
go_dirs="$(for f in "${subjects[@]}"; do case "$f" in (*.go) dirname "${f#>}" ;; esac; done | sort -u)"
: > "$defs_file"
if [ -n "$go_dirs" ]; then
  # shellcheck disable=SC2016  # backticks are Go raw-string delimiters
  while IFS= read -r d; do
    find "$d" -maxdepth 1 -type f -name '*.go' ! -name '*_test.go'
  done <<<"$go_dirs" | sort | LC_ALL=C xargs awk '
    function cut(s) { if (match(s, /--([ \t]|$)/)) s = substr(s, 1, RSTART - 1); return s }
    function emit() { gsub(/[\t\r]/, " ", val); print dir "\t" name "\t" val }
    FNR == 1 { inraw = 0; dir = FILENAME; if (!sub(/\/[^\/]*$/, "", dir)) dir = "." }
    inraw {
      j = index($0, "`")
      if (!j) { val = val " " cut($0); next }
      val = val " " cut(substr($0, 1, j - 1)); inraw = 0
      if (substr($0, j + 1) ~ /^[ \t,]*(\/\/.*)?$/) emit()
      next
    }
    match($0, /^[ \t]*((const|var)[ \t]+)?[A-Za-z_][A-Za-z0-9_]*([ \t]+string)?[ \t]*:?=[ \t]*`/) {
      name = substr($0, 1, RLENGTH - 1); rest = substr($0, RLENGTH + 1)
      sub(/^[ \t]*((const|var)[ \t]+)?/, "", name); sub(/[^A-Za-z0-9_].*$/, "", name)
      j = index(rest, "`")
      if (!j) { val = cut(rest); inraw = 1; next }
      val = cut(substr(rest, 1, j - 1))
      if (substr(rest, j + 1) ~ /^[ \t,]*(\/\/.*)?$/) emit()
    }' > "$defs_file"
  # A file that splices a constant holding a watched read is a subject too:
  # its statement is the consumer that may aggregate that read.
  read_defs="$(awk -F'\t' -v re="stellar\\.(${READ_TABLES})([^a-z0-9_]|\$)" 'tolower($3) ~ re { print $2 }' "$defs_file" | sort -u | paste -sd'|' -)"
  if [ -n "$read_defs" ]; then
    while IFS= read -r d; do
      for f in "$d"/*.go; do
        f="${f#./}"
        case "$f" in (*_test.go) continue ;; esac
        case " ${subjects[*]} " in (*" $f "*) continue ;; esac
        grep -qE "\\+[[:space:]]*(${read_defs})([^A-Za-z0-9_]|\$)" "$f" && subjects+=("$f")
      done
    done <<<"$go_dirs"
  fi
fi

# ── The analyser ─────────────────────────────────────────────────────
#
# Emits one TAB-separated `<file>\t<line>\t<table>\t<snippet>` record per
# violating read plus a final `#counts <reads> <aggregating>` self-accounting
# line, so a pattern that stopped matching reads as a fault, not a clean run.
violations="$(
  LC_ALL=C awk -v READ_RE="(from|join|,) ?stellar\\.(${READ_TABLES})([^a-z0-9_]|\$)" \
               -v VERB_FILES="$verb_only" -v DEFS="$defs_file" -v INTERP_RE="\"[^\"]*(from|join)[ ]+stellar\\.(${READ_TABLES})([^a-z0-9_]|\$)" '
    BEGIN {
      while ((getline dl < DEFS) > 0) {
        i = index(dl, "\t"); dd = substr(dl, 1, i - 1); dl = substr(dl, i + 1)
        i = index(dl, "\t"); k = dd SUBSEP substr(dl, 1, i - 1); dv = substr(dl, i + 1)
        DEF[k] = (k in DEF && DEF[k] != dv) ? DEF[k] " " dv : dv
      }
      close(DEFS)
    }

    # ── identity + vocabulary ────────────────────────────────────────
    function word(s, w) {
      return s ~ ("(^|[^A-Za-z0-9_])" w "([^A-Za-z0-9_]|$)")
    }
    # A multiplicity-sensitive aggregate: its value changes when a row
    # is repeated. countDistinct/uniq*/argMax/min/max/any are absent on
    # purpose — each is idempotent under an exact duplicate.
    # Takes lowercased text. The suffix admits combinators and variants
    # (sumIf, countState, sumKahan); countDistinct is the one idempotent
    # name the prefix would otherwise catch.
    # The underscore forms (var_pop, stddev_samp, array_agg) are the SQL
    # standard spellings ClickHouse accepts beside the camel-case ones.
    function mult_agg(s) {
      gsub(/countdistinct[ ]*\(/, "", s)
      gsub(/count[ ]*\([ ]*distinct[ ]/, "", s)
      return s ~ /(^|[^a-z0-9_])(count|sum|avg|grouparray|array_agg|group_?concat|topk|corr|covar_?pop|covar_?samp|var_?pop|var_?samp|quantile|median|stddev|deltasum|exponential|histogram|simplelinearregression|stochastic|groupbitxor|entropy|skew|kurt|sequencecount|sparkbar|contingency|cramersv|theilsu|rankcorr|maxintersections|[a-z]*ttest|mannwhitneyutest)[a-z0-9_]*[ ]*\(/
    }
    # block_of — the query block owning a read: from its own SELECT to
    # the closing paren of its subquery, its terminating semicolon, or
    # the next depth-0 SELECT (a UNION arm). Nested selects sit at a
    # deeper paren depth and stay INSIDE the block, which is correct:
    # they are part of this query text.
    function is_select(s, i) {
      return substr(s, i, 6) == "select" &&
             (i == 1 || substr(s, i - 1, 1) !~ /[a-z0-9_]/) &&
             substr(s, i + 6, 1) !~ /[a-z0-9_]/
    }
    function block_of(s, start,    i, depth, ch, n) {
      depth = 0
      n = length(s)
      for (i = start; i <= n; i++) {
        ch = substr(s, i, 1)
        if (ch == "(") depth++
        else if (ch == ")") { if (depth == 0) break; depth-- }
        else if (ch == ";") break
        else if (depth == 0 && i > start && is_select(s, i)) break
      }
      return substr(s, start, i - start)
    }

    # own_text — a block with every parenthesised nested select blanked, so
    # a scalar subquery in the select list is not read as this block.
    function own_text(b,    i, j, n, depth, ch, out, inner) {
      n = length(b); out = ""
      for (i = 1; i <= n; i++) {
        ch = substr(b, i, 1)
        if (ch != "(") { out = out ch; continue }
        depth = 1
        for (j = i + 1; j <= n && depth > 0; j++) {
          ch = substr(b, j, 1)
          if (ch == "(") depth++; else if (ch == ")") depth--
        }
        inner = substr(b, i, j - i)
        if (inner ~ /^\([ ]*select[^a-z0-9_]/) out = out "()"
        else out = out "(" own_text(substr(inner, 2, length(inner) - 2)) ")"
        i = j - 1
      }
      return out
    }

    # with_prefix - the WITH-expression clause directly before the owning
    # SELECT: an aggregate defined there consumes the same rows.
    function with_prefix(s, owner,    i, depth, ch, w) {
      depth = 0; w = 0
      for (i = owner - 1; i >= 1; i--) {
        ch = substr(s, i, 1)
        if (ch == ")") depth++
        else if (ch == "(") { if (depth == 0) break; depth-- }
        else if (ch == ";") break
        else if (depth == 0 && is_select(s, i)) break
        else if (depth == 0 && substr(s, i, 4) == "with" && (i == 1 || substr(s, i - 1, 1) !~ /[a-z0-9_]/) &&
                 substr(s, i + 4, 1) !~ /[a-z0-9_]/) { w = i; break }
      }
      return w ? substr(s, w, owner - w) : ""
    }

    # collapses - a block whose own rows are one per key (or one row): a
    # GROUP BY, DISTINCT, LIMIT 1 BY or an idempotent aggregate. Takes the
    # lowercased own_text of the block.
    function collapses(c) {
      return c ~ /(^|[^a-z0-9_])(group by|distinct|limit 1 by)([^a-z0-9_]|$)/ || idem_agg(c)
    }
    # idem_agg - an idempotent aggregate call by its whole name (plus
    # combinators) that is not a window: max(x) OVER (...) keeps every row,
    # and max2()/minus() are row-level scalars.
    function idem_agg(c,    p, j, n, depth, ch) {
      while (match(c, /(^|[^a-z0-9_])(min|max|any|anylast|anyheavy|argmax|argmin|uniq|uniqexact|uniqcombined|uniqcombined64|uniqhll12|uniqtheta|groupuniqarray)(if|state|merge|ornull|ordefault|array|distinct)*[ ]*\(/)) {
        p = RSTART + RLENGTH - 1
        depth = 1; n = length(c)
        for (j = p + 1; j <= n && depth > 0; j++) {
          ch = substr(c, j, 1)
          if (ch == "(") depth++; else if (ch == ")") depth--
        }
        c = substr(c, j)
        if (c !~ /^[ ]*over[ ]*[(a-z_]/) return 1
      }
      return 0
    }
    # enclosing_open - the unmatched "(" left of pos in this statement, or 0.
    function enclosing_open(s, pos,    i, depth, ch) {
      depth = 0
      for (i = pos - 1; i >= 1; i--) {
        ch = substr(s, i, 1)
        if (ch == ")") depth++
        else if (ch == "(") { if (depth == 0) return i; depth-- }
        else if (ch == ";") return 0
      }
      return 0
    }
    # owner_of - the SELECT owning position pos: the last one before it at
    # the same paren depth, or 0.
    function owner_of(s, pos,    i, depth, ch) {
      depth = 0
      for (i = pos; i >= 1; i--) {
        ch = substr(s, i, 1)
        if (ch == ")") depth++
        else if (ch == "(") { if (depth == 0) return 0; depth-- }
        else if (depth == 0 && is_select(s, i)) return i
      }
      return 0
    }
    # consumed - a read whose own block does not aggregate still feeds an
    # aggregate when it sits in a FROM/JOIN subquery (or a CTE) that does not
    # collapse its rows and any block above it, or reading the CTE, aggregates.
    function consumed(s, owner, hops,    blk, open, outer, pre, j, depth, ch, alias, from, ref, refown, found) {
      if (hops > 32) return 1
      blk = own_text(block_of(s, owner))
      if (collapses(blk)) return 0
      open = enclosing_open(s, owner)
      # A parenthesised UNION arm, or a doubled paren, passes its rows up.
      while (open > 0 && substr(s, 1, open - 1) ~ /(\(|(^|[^a-z0-9_])union( all| distinct)?)[ ]*$/)
        open = enclosing_open(s, open)
      if (open == 0) return 0
      pre = substr(s, 1, open - 1)
      if (pre ~ /(^|[^a-z0-9_])(from|join)[ ]*$/) {
        outer = owner_of(s, open - 1)
        if (outer == 0) return 0
        if (mult_agg(own_text(with_prefix(s, outer) block_of(s, outer)))) return 1
        return consumed(s, outer, hops + 1)
      }
      if (pre !~ /(^|[^a-z0-9_])as[ ]*$/) return 0
      alias = pre; sub(/[ ]*as[ ]*$/, "", alias)
      if (!match(alias, /[a-z_][a-z0-9_]*$/)) return 1
      alias = substr(alias, RSTART, RLENGTH)
      depth = 1
      for (j = open + 1; j <= length(s) && depth > 0; j++) {
        ch = substr(s, j, 1)
        if (ch == "(") depth++; else if (ch == ")") depth--
      }
      found = 0; from = j
      while (match(substr(s, from), "(^|[^a-z0-9_])(from|join)[ ]+" alias "([^a-z0-9_.]|$)")) {
        ref = from + RSTART; from = ref + RLENGTH - 1
        found = 1
        refown = owner_of(s, ref)
        if (refown == 0) continue
        if (mult_agg(own_text(with_prefix(s, refown) block_of(s, refown)))) return 1
        if (consumed(s, refown, hops + 1)) return 1
      }
      # No reader of the CTE in this text: it is spliced into a consumer the
      # gate cannot see (a CTE in its own constant), so fail closed.
      return found ? 0 : 1
    }

    # ── unit accumulation ────────────────────────────────────────────
    # A unit is flushed when the NEXT file starts, so it carries the file
    # it began in: FILENAME at flush time is the wrong one.
    function reset_unit() { U = ""; Uline = 0; Ufile = "" }
    function add(text) {
      if (U == "") { Uline = FNR; Ufile = FILENAME }
      U = U " " text
    }
    function flush() {
      if (U != "" && index(U, "stellar."))
        analyse(U, Uline, Ufile)
      reset_unit()
    }

    # ── the read-by-read verdict ─────────────────────────────────────
    function analyse(raw, line, file,    N, L, from, s, pos, len, tbl, i,
                     owner, clause, blk, tail, snip, full, depth, ch, direct) {
      N = raw
      gsub(/[\n\t]/, " ", N)
      while (N ~ /  /) gsub(/  /, " ", N)
      # SQL keywords and function names are case-insensitive, so every test
      # runs on the lowercased copy; N is kept for the report.
      L = tolower(N)
      from = 1
      while (1) {
        s = substr(L, from)
        if (match(s, READ_RE) == 0) return
        # RSTART/RLENGTH are globals that any later match() clobbers,
        # so take the extent of THIS read before calling anything else.
        pos = from + RSTART - 1
        len = RLENGTH
        tbl = substr(L, pos, len)
        sub(/^.*stellar\./, "stellar.", tbl)
        sub(/%[[][0-9]+[]]/, "%", tbl)
        sub(/[^a-z0-9_.%].*$/, "", tbl)
        from = pos + len - 1
        examined++

        # The owning SELECT: the last one before the read at the same
        # paren depth. A subquery that closed before the read is not it.
        owner = 0; depth = 0
        for (i = pos; i >= 1; i--) {
          ch = substr(L, i, 1)
          if (ch == ")") depth++
          else if (ch == "(") { if (depth == 0) break; depth-- }
          else if (depth == 0 && is_select(L, i)) { owner = i; break }
        }
        if (owner == 0) continue            # INSERT/ALTER target, not a read

        # Its whole block, minus nested selects: an aggregate in HAVING or
        # ORDER BY consumes the same rows as one in the select list.
        clause = own_text(with_prefix(L, owner) block_of(L, owner))
        direct = mult_agg(clause)
        if (!direct && !consumed(L, owner, 0)) continue  # not an aggregating read
        aggregating++

        # FINAL attached to this read (optionally through an alias).
        tail = substr(L, pos + len - 1)
        if (tail ~ /^[ ]*(as[ ]+[a-z_][a-z0-9_]*[ ]+|[a-z_][a-z0-9_]*[ ]+)?final([^a-z0-9_]|$)/) continue

        blk = block_of(L, owner)
        full = substr(N, owner, length(blk))
        snip = full
        # A read aggregated only by a consumer is fingerprinted with its whole statement, so a
        # baselined entry fails once the consumer changes.
        if (!direct) full = N
        if (length(snip) > 220) snip = substr(snip, 1, 220) " …"
        printf "%s\t%d\t%s\t%s\t%s\n", file, line, tbl, snip, full
      }
    }

    # ── extractors ───────────────────────────────────────────────────
    #
    # Go: raw-string literals only, spliced across `+ ident +` glue so a
    # query assembled from constants is analysed as one statement.
    # Interpreted strings and // comments are not SQL carriers here.
    # Blanks interpreted strings and rune literals and drops a trailing
    # // comment, so every backtick left delimits a raw literal. A lone
    # backtick in "`" or in a rune would otherwise invert the raw state for
    # the rest of the file. raw is the state at the start of the line.
    # An interpreted string becomes the token _s_, so a quoted table
    # spliced between raw literals still reads as a splice. Q collects the
    # line outside raw literals with its strings intact, for interp().
    function go_mask(l, raw,    out, i, n, c, j) {
      out = ""; Q = ""; n = length(l); i = 1
      while (i <= n) {
        if (raw) {
          j = index(substr(l, i), "`")
          if (j == 0) return out substr(l, i)
          out = out substr(l, i, j); Q = Q " "; i += j; raw = 0
          continue
        }
        c = substr(l, i, 1)
        if (c == "`") { raw = 1; out = out c; Q = Q " "; i++; continue }
        if (c == "/" && substr(l, i, 2) == "//") return out
        if (c == "\"" || c == "\047") {
          j = i + 1
          while (j <= n && substr(l, j, 1) != c) { if (substr(l, j, 1) == "\\") j++; j++ }
          out = out " _s_ "; Q = Q substr(l, i, j - i + 1); i = j + 1
          continue
        }
        out = out c; Q = Q c; i++
      }
      return out
    }

    # splice_defs - add the text of each same-package constant the glue names.
    function splice_defs(g,    d, id) {
      d = FILENAME; if (!sub(/\/[^\/]*$/, "", d)) d = "."
      while (match(g, /[A-Za-z_][A-Za-z0-9_]*/)) {
        id = substr(g, RSTART, RLENGTH); g = substr(g, RSTART + RLENGTH)
        if ((d SUBSEP id) in DEF) add(DEF[d SUBSEP id])
      }
    }

    # interp - report an unexaminable read: SQL that is not one raw literal.
    function interp() {
      if (FILENAME ~ /_test\.go$/) return
      printf "#interp\t%s:%d\t%s\n", FILENAME, FNR, norm($0)
      ireported = 1
    }
    function norm(s) {
      gsub(/[\t\r]/, " ", s); while (s ~ /  /) gsub(/  /, " ", s)
      sub(/^ /, "", s); sub(/ $/, "", s)
      return s
    }

    function go_line(l,    n, p, i, inside, part) {
      l = go_mask(l, in_raw)
      # Only raw literals are analysed. A read in a double-quoted string in
      # production Go would never be examined, so it is reported instead.
      part = Q
      # A file naming no watched table is judged on upper-case SQL only:
      # its lower-case "from" strings are log and error prose.
      if (part ~ /(FROM|JOIN)[ ]+%(\[[0-9]+\])?[sqv]|(FROM|JOIN)[ ]*"[ ]*\+/ || tolower(part) ~ /(from|join)[ ]+stellar\.[ ]*"[ ]*\+/) interp()
      else if (!verb_file && (tolower(part) ~ INTERP_RE || tolower(part) ~ /(from|join)[ ]*"[ ]*\+[ ]*"?[ ]*(stellar|$)/)) interp()
      n = split(l, p, "`")
      inside = in_raw
      for (i = 1; i <= n; i++) {
        part = p[i]
        if (inside) {
          # A `--` comment inside a raw SQL string is prose, not a
          # clause — explorer_reader.go and protocol_reader.go both
          # write documentation lines like `-- FINAL: ...` inside their
          # query literals. Cut only at an actual comment MARKER (`--`
          # followed by whitespace or end of line), never at the first
          # `--` anywhere in the string: a CLI flag (`--port`) or a
          # quoted literal (`'--'`) is content, not a comment, and
          # truncating there would erase the table name or the clause
          # that makes the read a violation.
          if (match(part, /--([ \t]|$)/) > 0) part = substr(part, 1, RSTART - 1)
          add(part)
          lastraw = tolower(part)
          if (lastraw ~ /(from|join)[ ]+%(\[[0-9]+\])?[sqv]/) interp()
          # A literal ending in FROM/JOIN takes its table from the next
          # operand, whether or not another literal follows it.
          ireported = 0
          tailfrom = lastraw ~ /(from|join)[ ]*(stellar\.)?[ ]*$/
        } else {
          glue = glue part
          if (i < n) {
            # Glue between two literals: `+ ident +` or `+ f(x) +` splices
            # them into one statement, anything else ends it. A spliced
            # same-package constant contributes its text; any other spliced
            # expression stands in as `?`, so a clause after it (a CTE
            # consumer, a HAVING) is still judged with the read.
            identglue = glue ~ /^[ \t]*\+[ \t]*([A-Za-z_][A-Za-z0-9_.]*[ \t]*\+[ \t]*)*$/
            splice = identglue || glue ~ /^[ \t]*\+.*\+[ \t]*$/
            if (tailfrom && !ireported && splice && glue ~ /[A-Za-z_]/) interp()
            tailfrom = 0
            if (U != "" && !splice) flush()
            else if (U != "" && !identglue) add("?")
            else if (U != "") splice_defs(glue)
            glue = ""
          }
        }
        # A backtick separates part i from part i+1; with n parts there
        # are n-1 separators, so the LAST part never toggles. Toggling
        # per part instead dropped every other line of a multi-line raw
        # literal (n == 1 flipped the state on a line with no backtick).
        if (i < n) inside = !inside
      }
      if (!inside && tailfrom && !ireported && glue ~ /^[ \t]*\+[ \t]*[A-Za-z_(]/) { interp(); tailfrom = 0 }
      if (!inside) glue = glue " "
      in_raw = inside
    }

    # SQL: comment runs and executable statements are separate units.
    # A comment run breaks on a blank comment line or on an
    # UNINDENTED one — in this tree prose sits at column 0 and an
    # operator command is indented, so that boundary keeps one header
    # block from lending its GROUP BY to the command beside it.
    function sql_line(l,    body, rest, p) {
      if (l ~ /^[ \t]*--/) {
        sub(/^[ \t]*--/, "", l)
        body = l
        if (body ~ /^[ \t]*$/ || body !~ /^[ \t]/) flush()
        if (body !~ /^[ \t]*$/) {
          in_comment = 1
          # The prose of a header recipe can carry a second "--" marker
          # documenting a clause it is NOT using ("-- GROUP BY
          # ledger_seq, tx_index" or "-- countDistinct, NOT count():
          # ..."). Cut there too, exactly like the code-line branch
          # below, so that prose is never read as a real clause -- a
          # flat header recipe has no run-boundary or ")" to end the
          # read before it, unlike a multi-statement file.
          if (match(body, /--([ \t]|$)/) > 0) body = substr(body, 1, RSTART - 1)
          if (body !~ /^[ \t]*$/) add(body)
        }
        return
      }
      if (in_comment) { flush(); in_comment = 0 }
      rest = l
      # An inline trailing `--` comment on a CODE line is prose, not
      # SQL, whether or not the statement ever reaches a terminating
      # `;` on this line — but only when `--` is an actual comment
      # MARKER (followed by whitespace or end of line). A bare `index`
      # would also cut a `--` that is content: a quoted literal
      # (`'--'`) or a CLI flag glued into the same statement text. Cut
      # at the marker, before the `;` scan, so a fake `;` hiding inside
      # the comment cannot end a statement early either.
      if (match(rest, /--([ \t]|$)/) > 0) rest = substr(rest, 1, RSTART - 1)
      while ((p = index(rest, ";")) > 0) {
        add(substr(rest, 1, p))
        flush()
        rest = substr(rest, p + 1)
      }
      add(rest)
    }

    FNR == 1 { flush(); in_raw = 0; in_comment = 0; glue = ""; tailfrom = 0; ireported = 0
               is_sql = (FILENAME ~ /\.sql$/)
               verb_file = index(VERB_FILES, "|" FILENAME "|") > 0 }
    { if (is_sql) sql_line($0); else go_line($0) }
    END { flush(); printf "#counts %d %d\n", examined + 0, aggregating + 0 }
  ' "$canary_file" "${subjects[@]}"
)"

examined="$(printf '%s\n' "$violations" | awk '/^#counts /{print $2}')"
aggregating="$(printf '%s\n' "$violations" | awk '/^#counts /{print $3}')"
interpreted="$(printf '%s\n' "$violations" | awk -F'\t' '$1 == "#interp"')"
violations="$(printf '%s\n' "$violations" | grep -v '^#counts \|^#interp' || true)"

if [ -z "${examined:-}" ] || [ -z "${aggregating:-}" ]; then
  die "the analyser produced no #counts line — a gate reporting clean over
  an empty accounting stream. Fix the analyser, do not delete the check."
fi

# The canary is a subject too, so its +1/+1 is in `examined`/`aggregating`
# above; require its violation record (by filename, field 1) before trusting
# either count: proof the classifier is awake, not just quiet.
canary_report="$(printf '%s\n' "$violations" | awk -F'\t' -v f="$canary_file" '$1 == f')"
violations="$(printf '%s\n' "$violations" | awk -F'\t' -v f="$canary_file" '$1 != f')"
if [ -z "$canary_report" ]; then
  die "the analyser did not flag its own canary aggregate
  ($canary_file: a bare count() over stellar.transactions with no FINAL).
  The aggregate classifier, mult_agg(), is broken silently, not merely
  quiet on this tree. Fix the classifier, do not delete the check."
fi
examined=$((examined - 1))
aggregating=$((aggregating - 1))

if [ "$examined" -eq 0 ]; then
  die "the analyser classified 0 lake-table reads across ${#subjects[@]} subject file(s).
  Every one of them NAMES a lake table, so a zero here means the FROM/JOIN
  extraction no longer matches this tree — a gate reporting clean because it
  read nothing. Fix the extractor, do not delete the check."
fi

# ── baseline ─────────────────────────────────────────────────────────
#
# `<file>:<key>  <N> read(s): #<fingerprint> <reason>`, where <key> is
# `stellar.<table>` or `#interp` (a read in a double-quoted or spliced string).
# N and the fingerprint (cksum of the sorted, whitespace-normalised text of
# the excused sites: the owning SELECT for a table, the source line for
# #interp) pin the entry to the reads it was granted for, so one more, one
# fewer or one swapped fails. The gate prints the entry to copy; the reason
# is required, as an unexplained exemption is what baselines exist to stop.
ENTRY_RE='^([0-9]+) reads?: #([0-9]+) +(.{10,})$'
declare -a base_keys=() base_counts=() base_fps=()
if [ -f "$BASELINE" ]; then
  lineno=0
  while IFS= read -r raw; do
    lineno=$((lineno + 1))
    case "$raw" in ''|'#'*) continue ;; esac
    key="${raw%%[[:space:]]*}"
    reason="${raw#"$key"}"
    reason="${reason#"${reason%%[![:space:]]*}"}"
    case "$key" in
      *:stellar.*|*:#interp) : ;;
      *) die "$BASELINE:$lineno has a key of no known kind (\`<file>:stellar.<table>\` or \`<file>:#interp\`):
    $raw" ;;
    esac
    [[ "$reason" =~ $ENTRY_RE ]] || die "$BASELINE:$lineno is not \`<file>:<key>  <N> read(s): #<fingerprint> <reason>\`:
    $raw
  N and the fingerprint name the reads the entry excuses and the reason says
  why; copy the entry the gate prints for the site and write its reason."
    base_keys+=("$key")
    base_counts+=("${BASH_REMATCH[1]}")
    base_fps+=("${BASH_REMATCH[2]}")
  done < "$BASELINE"
fi

# base_index <key>: the index of its baseline entry, or status 1.
base_index() {
  local i=0
  while [ "$i" -lt "${#base_keys[@]}" ]; do
    [ "${base_keys[$i]}" = "$1" ] && { printf '%s' "$i"; return 0; }
    i=$((i + 1))
  done
  return 1
}
fp_of() { LC_ALL=C sort | cksum | awk '{print $1}'; }

# One TAB record per site: key, fingerprinted text, location, snippet.
sites="$(
  printf '%s\n' "$violations" | awk -F'\t' 'NF >= 5 { print $1 ":" $3 "\t" $5 "\t" $1 ":" $2 "  " $3 "\t" $4 }'
  printf '%s\n' "$interpreted" | awk -F'\t' 'NF >= 3 && !seen[$2]++ {
    f = $2; sub(/:[0-9]+$/, "", f); print f ":#interp\t" $3 "\t" $2 "\t" }'
)"
keys="$({ cut -f1 <<<"$sites"; printf '%s\n' ${base_keys+"${base_keys[@]}"}; } | { grep -v '^$' || true; } | LC_ALL=C sort -u)"

fail=0
interp_fail=0
stale=0
while IFS= read -r key; do
  [ -n "$key" ] || continue
  ksites="$(awk -F'\t' -v k="$key" '$1 == k' <<<"$sites")"
  got_n=0
  got_fp=""
  if [ -n "$ksites" ]; then
    got_n="$(grep -c . <<<"$ksites")"
    got_fp="$(cut -f2 <<<"$ksites" | fp_of)"
  fi
  if idx="$(base_index "$key")"; then
    [ "$got_n" = "${base_counts[$idx]}" ] && [ "$got_fp" = "${base_fps[$idx]}" ] && continue
    if [ "$got_n" -eq 0 ]; then
      # An entry for a table this copy does not watch is not stale: CI judges
      # a PR with the base branch's copy, which must accept the entries of the
      # PR that adds the table.
      case "$key" in
        *:stellar.*) [[ "${key##*:stellar.}" =~ ^(${READ_TABLES})$ ]] || continue ;;
      esac
      echo "lint-lake-dedup: baseline stale: $key no longer has a violating read — remove it from $BASELINE." >&2
      stale=1
      continue
    fi
    why="its entry states ${base_counts[$idx]} read(s) #${base_fps[$idx]}, the gate finds $got_n #$got_fp"
  else
    why="not in $BASELINE"
  fi
  case "$key" in
    *:#interp) interp_fail=1
       echo "lint-lake-dedup: FAIL — $key: a lake-table read in a double-quoted or spliced Go string is not examined by this gate ($why):" >&2 ;;
    *) fail=1
       echo "lint-lake-dedup: FAIL — $key: aggregating read(s) of a duplicate-bearing lake table with no collapse ($why):" >&2 ;;
  esac
  while IFS=$'\t' read -r _ _ loc snip; do
    echo "  $loc" >&2
    [ -z "$snip" ] || echo "    $snip" >&2
  done <<<"$ksites"
  echo "  baseline entry: $key  $got_n read$([ "$got_n" -eq 1 ] || echo s): #$got_fp <reason>" >&2
done <<<"$keys"

if [ "$interp_fail" -ne 0 ]; then
  echo "Write the SQL as one raw (backtick) literal naming the table after FROM/JOIN." >&2
fi
if [ "$fail" -ne 0 ]; then
  cat >&2 <<'EOF'

Read the table through FINAL, or aggregate above a subquery that picks one
version per key (argMax(col, ingested_at) … GROUP BY the full ORDER BY key).
A GROUP BY, LIMIT 1 BY, SELECT DISTINCT or uniqExact in the same SELECT does
not collapse the rows a sum() or count() consumes. The shipped sponsors gate
(internal/storage/clickhouse/account_sponsors_rollup.go) is the worked
example of the two-level form.

Over ledgers 63,000,000-63,099,999 on r1, EVERY distinct
(ledger_seq, tx_index) key in stellar.transactions carries more than one
row and stellar.operations runs at 2.0x, so an uncollapsed count() is a
multiple of the truth — and the totals still look plausible.

If the read is genuinely exempt, add the printed entry to
scripts/ci/lint-lake-dedup.baseline with a reason and a `Baseline-Growth:`
commit trailer naming that file.
EOF
fi
[ "$fail" -eq 0 ] && [ "$interp_fail" -eq 0 ] && [ "$stale" -eq 0 ] || exit 1

echo "lint-lake-dedup: OK — ${examined} lake-table read(s) across ${#subjects[@]} file(s)," \
     "${aggregating} of them aggregating; every aggregating one reads through FINAL" \
     "(${#base_keys[@]} grandfathered)."
