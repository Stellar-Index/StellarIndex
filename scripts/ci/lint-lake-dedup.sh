#!/usr/bin/env bash
# lint-lake-dedup.sh — an AGGREGATING read of a duplicate-bearing lake
# table must collapse that table on its full identity first.
#
# `stellar.transactions` and `stellar.operations` are
# ReplacingMergeTree(ingested_at) archives whose duplicate rows are not
# merged away, and not merged away in BULK. Measured on r1 2026-09-07
# over ledgers 63,000,000-63,099,999: EVERY one of the 33,380,486
# distinct `(ledger_seq, tx_index)` keys in `stellar.transactions`
# carries more than one row, and `stellar.operations` runs at exactly
# 2.0x (170,836 rows against 85,418 distinct keys over 100k ledgers).
# A `count()`, `sum()` or `groupArray()` over either table therefore
# reports a multiple of the truth, and a join to either MULTIPLIES the
# other side by the duplicate count of each key it matches.
#
# WHY THIS EXISTS — three near-misses in one day (2026-09-07):
#
#   1. a sponsors board built on a naive join inflated its top account
#      from 176,573 to 296,799;
#   2. a verification query reported 489,253 rows dropped that were not
#      — it had counted `stellar.operations` without deduplicating it;
#   3. it is why the shipped sponsors gate resolves the success flag
#      with `argMax(t.successful, t.ingested_at)` over a GROUP BY on
#      the operation identity, rather than with a bare equality.
#
# None of the three is visible in a total: the inflated board still
# summed to a plausible number, and the "dropped rows" reconciliation
# still balanced against itself. That is what makes the class dangerous
# and what makes it worth a gate.
#
# ── THE RULE, stated plainly ──────────────────────────────────────────
#
# A read of `stellar.transactions` or `stellar.operations` is IN SCOPE
# only when its OWN select list applies a MULTIPLICITY-SENSITIVE
# aggregate — one whose value changes when a row is repeated:
#
#     count countIf sum sumIf avg avgIf groupArray groupArrayIf
#     topK sumMap avgWeighted corr covarPop covarSamp
#     quantile* median* stddev* varPop varSamp
#
# Deliberately NOT in that set, because each is idempotent under an
# exact duplicate and each is the tree's own collapse vocabulary:
# `min`, `max`, `any`, `anyIf`, `argMax`, `argMin`, `uniq`, `uniqExact`,
# `uniqCombined`, `countDistinct`, `groupUniqArray`.
#
# An in-scope read must carry, in its own query block, one of:
#
#   * `FINAL` on the read itself                (engine-level collapse)
#   * `GROUP BY` covering the table's identity  (collapse by grouping)
#   * `LIMIT 1 BY` covering that identity       (collapse by pick-one)
#   * a `uniqExact`/`countDistinct`-family call over that identity
#   * `SELECT DISTINCT` governing the read      (collapse by projection)
#
# Identities are the tables' full ORDER BY keys
# (deploy/clickhouse/tier1_schema.sql):
#
#     stellar.transactions  (ledger_seq, tx_index)
#     stellar.operations    (ledger_seq, tx_index, op_index)
#                        or (ledger_seq, tx_hash,  op_index)
#
# ── WHAT THIS DELIBERATELY DOES NOT FLAG ─────────────────────────────
#
# A per-entity DETAIL reader legitimately returns what the ledger
# contained, row for row, and the repo has about five of them —
# explorer_reader.go, sdex_op_reader.go, contract_call_op_reader.go,
# classic_movement_reader.go, participant_backfill.go. A gate that
# fired on all of them would be noise nobody keeps, and it would be
# WRONG: those reads carry `FINAL` or `LIMIT 1 BY` where duplicates
# would be visible to a caller, and where they do not, no aggregate is
# reading them. Keying scope on the aggregate — not on the table — is
# what separates "this number is a multiple of the truth" from "this
# listing returns rows".
#
# Also not flagged, and each is a decision rather than an oversight:
#
#   * Go `//` comments. Go comments in this tree are prose ABOUT the
#     query beside them, and they quote fragments constantly ("a join
#     to stellar.transactions on the full (ledger_seq, tx_index)
#     identity"). Operator recipes live in `.sql` headers, which ARE
#     read — see deploy/clickhouse/ops_by_source.sql's Step 3, whose
#     `countDistinct(ledger_seq, tx_index)` exists because the first
#     verify run flagged 20 of 20 accounts on exactly this class.
#   * a read whose owning select applies only duplicate-idempotent
#     aggregates. `SELECT source_account, max(ledger_seq) … GROUP BY
#     source_account` over the archive is exact under duplicates.
#   * a statement whose aggregate sits ABOVE a subquery that already
#     collapses. The read belongs to the innermost select, so
#     `count() FROM (SELECT … FROM stellar.operations GROUP BY
#     ledger_seq, tx_index, op_index)` is out of scope by construction.
#
# ── ESCAPE HATCH ─────────────────────────────────────────────────────
#
# scripts/ci/lint-lake-dedup.baseline, one `<file>:<table>  <reason>`
# entry per grandfathered site, reason REQUIRED. It is a
# scripts/ci/*.baseline, so growing it needs a `Baseline-Growth:`
# commit trailer naming it (lint-baseline-growth.sh), and a stale entry
# — a site that no longer violates — fails this gate until the line is
# deleted. The baseline shrinks monotonically.
#
# Usage: lint-lake-dedup.sh [repo-root]
#   The optional root exists for scripts/ci/lint-lake-dedup-test.sh,
#   which runs this gate against fixture trees.
#
# Exit 0 clean, non-zero on any violation.
set -euo pipefail
cd "${1:-$(dirname "$0")/../..}"

BASELINE="scripts/ci/lint-lake-dedup.baseline"

die() { echo "lint-lake-dedup: FAIL — $*" >&2; exit 1; }

# Subject files: every tracked-or-untracked .go/.sql that NAMES a
# duplicate-bearing table. A file that never mentions one cannot
# violate, and pre-filtering keeps the gate at one awk pass over ~60
# files rather than the whole tree.
#
# `find` rather than `git ls-files`: this runs against fixture trees
# that are not checkouts. The prunes mirror lint-imports.sh's SKIP_DIRS.
subjects=()
while IFS= read -r f; do
  [ -n "$f" ] || continue
  subjects+=("$f")
done < <(
  find . \( -name .git -o -name vendor -o -name node_modules \
            -o -name .discovery-repos \) -prune -o \
       -type f \( -name '*.go' -o -name '*.sql' \) -print \
    | sed 's#^\./##' \
    | sort \
    | while IFS= read -r f; do
        if grep -qF -e 'stellar.transactions' -e 'stellar.operations' "$f"; then
          echo "$f"
        fi
      done
)

if [ "${#subjects[@]}" -eq 0 ]; then
  die "no .go/.sql file names stellar.transactions or stellar.operations.
  A gate with an empty subject set passes forever. If the lake tables were
  renamed, rename them here too — do not delete the check."
fi

# ── The analyser ─────────────────────────────────────────────────────
#
# Emits one TAB-separated `<file>\t<line>\t<table>\t<snippet>` record per
# violating read, and a final `#counts <reads> <aggregating>` self-accounting
# line so a pattern that silently stopped matching reads as a fault rather
# than as a clean run.
violations="$(
  awk '
    # ── identity + vocabulary ────────────────────────────────────────
    function word(s, w) {
      return s ~ ("(^|[^A-Za-z0-9_])" w "([^A-Za-z0-9_]|$)")
    }
    # covers_identity — does a column list name a full identity of tbl?
    function covers_identity(list, tbl) {
      if (tbl == "stellar.transactions")
        return word(list, "ledger_seq") && word(list, "tx_index")
      return word(list, "ledger_seq") && word(list, "op_index") &&
             (word(list, "tx_index") || word(list, "tx_hash"))
    }
    # A multiplicity-sensitive aggregate: its value changes when a row
    # is repeated. countDistinct/uniq*/argMax/min/max/any are absent on
    # purpose — each is idempotent under an exact duplicate.
    function mult_agg(s) {
      return s ~ /(^|[^A-Za-z0-9_])(count|countIf|sum|sumIf|avg|avgIf|groupArray|groupArrayIf|topK|sumMap|avgWeighted|corr|covarPop|covarSamp|quantile[A-Za-z0-9]*|median[A-Za-z0-9]*|stddev[A-Za-z]*|varPop|varSamp)[ ]*\(/
    }
    # clause_after — the column list following `kw`, cut at the next
    # clause keyword. Capped: a column list is short, and an uncapped
    # window would accept identity names belonging to another clause.
    function clause_after(s, kw, from,    p, rest, cut, i, terms, n, t) {
      p = index(substr(s, from), kw)
      if (p == 0) return ""
      rest = substr(s, from + p - 1 + length(kw), 200)
      n = split(" HAVING | ORDER BY | LIMIT | SETTINGS | UNION | WINDOW | FORMAT |;", terms, "\\|")
      cut = length(rest) + 1
      for (i = 1; i <= n; i++) {
        t = index(rest, terms[i])
        if (t > 0 && t < cut) cut = t
      }
      return substr(rest, 1, cut - 1)
    }
    # any_clause_covers — true if ANY occurrence of `kw` in `blk` is
    # followed by a list covering the identity. Per-occurrence, never a
    # union across clauses: two half-lists do not make a collapse.
    function any_clause_covers(blk, kw, tbl,    from, p, list) {
      from = 1
      while ((p = index(substr(blk, from), kw)) > 0) {
        list = clause_after(blk, kw, from)
        if (covers_identity(list, tbl)) return 1
        from = from + p - 1 + length(kw)
      }
      return 0
    }
    # uniq_covers — a uniqExact/countDistinct-family call over the
    # identity is the aggregate-side collapse (the form
    # accountOpTypeCountsQuery uses instead of count()).
    function uniq_covers(blk, tbl,    from, s, m, args) {
      from = 1
      while (1) {
        s = substr(blk, from)
        if (match(s, /(uniqExact|uniqCombined[0-9]*|uniqHLL12|uniqTheta|uniq|countDistinct)[ ]*\(/) == 0) return 0
        m = from + RSTART - 1 + RLENGTH
        args = substr(blk, m, 160)
        if (index(args, ")") > 0) args = substr(args, 1, index(args, ")") - 1)
        if (covers_identity(args, tbl)) return 1
        from = m
      }
    }
    # block_of — the query block owning a read: from its own SELECT to
    # the closing paren of its subquery, its terminating semicolon, or
    # the next depth-0 SELECT (a UNION arm). Nested selects sit at a
    # deeper paren depth and stay INSIDE the block, which is correct:
    # they are part of this query text.
    function block_of(s, start,    i, depth, ch, n) {
      depth = 0
      n = length(s)
      for (i = start; i <= n; i++) {
        ch = substr(s, i, 1)
        if (ch == "(") depth++
        else if (ch == ")") { if (depth == 0) break; depth-- }
        else if (ch == ";") break
        else if (depth == 0 && i > start && substr(s, i, 6) == "SELECT") break
      }
      return substr(s, start, i - start)
    }

    # ── unit accumulation ────────────────────────────────────────────
    function reset_unit() { U = ""; Uline = 0 }
    function add(text) {
      if (U == "") Uline = FNR
      U = U " " text
    }
    function flush() {
      if (U != "" && (index(U, "stellar.transactions") || index(U, "stellar.operations")))
        analyse(U, Uline)
      reset_unit()
    }

    # ── the read-by-read verdict ─────────────────────────────────────
    function analyse(raw, line,    N, from, s, pos, len, tbl, i,
                     owner, clause, blk, tail, snip) {
      N = raw
      gsub(/[\n\t]/, " ", N)
      while (N ~ /  /) gsub(/  /, " ", N)
      from = 1
      while (1) {
        s = substr(N, from)
        if (match(s, /(FROM|JOIN) stellar\.(transactions|operations)([^A-Za-z0-9_]|$)/) == 0) return
        # RSTART/RLENGTH are globals that any later match() clobbers,
        # so take the extent of THIS read before calling anything else.
        pos = from + RSTART - 1
        len = RLENGTH
        tbl = (substr(N, pos, len) ~ /transactions/) \
              ? "stellar.transactions" : "stellar.operations"
        from = pos + len - 1
        examined++

        # The owning SELECT: the last one before the read.
        owner = 0
        for (i = pos; i >= 1; i--)
          if (substr(N, i, 6) == "SELECT") { owner = i; break }
        if (owner == 0) continue            # INSERT/ALTER target, not a read

        clause = substr(N, owner, pos - owner)
        if (clause ~ /^SELECT[ ]+DISTINCT([ ]|$)/) continue   # collapsed by projection
        if (!mult_agg(clause)) continue                       # not an aggregating read
        aggregating++

        blk = block_of(N, owner)

        # FINAL attached to this read (optionally through an alias).
        tail = substr(N, pos + len - 1)
        if (tail ~ /^[ ]*(AS[ ]+[A-Za-z_][A-Za-z0-9_]*[ ]+|[A-Za-z_][A-Za-z0-9_]*[ ]+)?FINAL([^A-Za-z0-9_]|$)/) continue
        if (any_clause_covers(blk, "GROUP BY", tbl)) continue
        if (any_clause_covers(blk, "LIMIT 1 BY", tbl)) continue
        if (uniq_covers(blk, tbl)) continue

        snip = blk
        if (length(snip) > 220) snip = substr(snip, 1, 220) " …"
        printf "%s\t%d\t%s\t%s\n", FILENAME, line, tbl, snip
      }
    }

    # ── extractors ───────────────────────────────────────────────────
    #
    # Go: raw-string literals only, spliced across `+ ident +` glue so a
    # query assembled from constants is analysed as one statement.
    # Interpreted strings and // comments are not SQL carriers here.
    function go_line(l,    n, p, i, inside, part, cut) {
      if (!in_raw) {
        cut = index(l, "//")
        # A // outside a raw string starts a comment; a backtick before
        # it means the literal is still open and the marker is content.
        if (cut > 0 && index(substr(l, 1, cut), "`") == 0) l = substr(l, 1, cut - 1)
      }
      n = split(l, p, "`")
      inside = in_raw
      for (i = 1; i <= n; i++) {
        part = p[i]
        if (inside) {
          add(part)
        } else {
          glue = glue part
          if (i < n) {
            # Glue between two literals: `+ ident +` splices them,
            # anything else ends the statement.
            if (U != "" && glue !~ /^[ \t]*\+[ \t]*([A-Za-z_][A-Za-z0-9_.]*[ \t]*\+[ \t]*)*$/) flush()
            glue = ""
          }
        }
        # A backtick separates part i from part i+1; with n parts there
        # are n-1 separators, so the LAST part never toggles. Toggling
        # per part instead dropped every other line of a multi-line raw
        # literal (n == 1 flipped the state on a line with no backtick).
        if (i < n) inside = !inside
      }
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
        if (body !~ /^[ \t]*$/) { in_comment = 1; add(body) }
        return
      }
      if (in_comment) { flush(); in_comment = 0 }
      rest = l
      while ((p = index(rest, ";")) > 0) {
        add(substr(rest, 1, p))
        flush()
        rest = substr(rest, p + 1)
      }
      add(rest)
    }

    FNR == 1 { flush(); in_raw = 0; in_comment = 0; glue = ""
               is_sql = (FILENAME ~ /\.sql$/) }
    { if (is_sql) sql_line($0); else go_line($0) }
    END { flush(); printf "#counts %d %d\n", examined + 0, aggregating + 0 }
  ' "${subjects[@]}"
)"

examined="$(printf '%s\n' "$violations" | awk '/^#counts /{print $2}')"
aggregating="$(printf '%s\n' "$violations" | awk '/^#counts /{print $3}')"
violations="$(printf '%s\n' "$violations" | grep -v '^#counts ' || true)"

if [ -z "${examined:-}" ] || [ "$examined" -eq 0 ]; then
  die "the analyser classified 0 lake-table reads across ${#subjects[@]} subject file(s).
  Every one of them NAMES a lake table, so a zero here means the FROM/JOIN
  extraction no longer matches this tree — a gate reporting clean because it
  read nothing. Fix the extractor, do not delete the check."
fi

# ── baseline ─────────────────────────────────────────────────────────
#
# `<file>:<table>  <reason>`; the reason is required and is the whole
# point of the hatch — an unexplained exemption is the thing baselines
# are for stopping.
declare -a base_keys=()
if [ -f "$BASELINE" ]; then
  lineno=0
  while IFS= read -r raw; do
    lineno=$((lineno + 1))
    case "$raw" in ''|'#'*) continue ;; esac
    key="${raw%%[[:space:]]*}"
    reason="${raw#"$key"}"
    reason="${reason#"${reason%%[![:space:]]*}"}"
    if [ -z "$key" ] || [ "${#reason}" -lt 10 ]; then
      die "$BASELINE:$lineno has no reason:
    $raw
  Format is \`<file>:<table>  <reason>\`. An exemption without a stated
  reason is the failure mode a baseline exists to prevent."
    fi
    base_keys+=("$key")
  done < "$BASELINE"
fi

in_baseline() {
  local k="$1" b
  for b in ${base_keys+"${base_keys[@]}"}; do
    [ "$b" = "$k" ] && return 0
  done
  return 1
}

fail=0
observed=""
while IFS=$'\t' read -r file line tbl snip; do
  [ -n "$file" ] || continue
  key="${file}:${tbl}"
  observed="${observed}|${key}|"
  in_baseline "$key" && continue
  if [ "$fail" -eq 0 ]; then
    echo "lint-lake-dedup: FAIL — aggregating read(s) of a duplicate-bearing lake table with no collapse:" >&2
  fi
  fail=1
  echo "  $file:$line  $tbl" >&2
  echo "    $snip" >&2
done <<<"$violations"

stale=0
for b in ${base_keys+"${base_keys[@]}"}; do
  case "$observed" in
    *"|$b|"*) : ;;
    *) echo "lint-lake-dedup: baseline stale: $b no longer violates — remove it from $BASELINE." >&2
       stale=1 ;;
  esac
done

if [ "$fail" -ne 0 ]; then
  cat >&2 <<'EOF'

Collapse the table on its full identity before the aggregate reads it:

    stellar.transactions  (ledger_seq, tx_index)
    stellar.operations    (ledger_seq, tx_index, op_index)
                       or (ledger_seq, tx_hash,  op_index)

Any one of these satisfies the gate — FINAL on the read, a GROUP BY or
LIMIT 1 BY over the identity, a uniqExact/countDistinct over it, or a
SELECT DISTINCT governing the read. The shipped sponsors gate
(internal/storage/clickhouse/account_sponsors_rollup.go) is the worked
example: it groups on the operation identity and resolves the joined
transaction's flag with argMax(t.successful, t.ingested_at).

Over ledgers 63,000,000-63,099,999 on r1, EVERY distinct
(ledger_seq, tx_index) key in stellar.transactions carries more than one
row and stellar.operations runs at 2.0x, so an uncollapsed count() is a
multiple of the truth — and the totals still look plausible.

If the read is genuinely exempt, add it to scripts/ci/lint-lake-dedup.baseline
with a reason and a `Baseline-Growth:` commit trailer naming that file.
EOF
  exit 1
fi
[ "$stale" -eq 0 ] || exit 1

echo "lint-lake-dedup: OK — ${examined} lake-table read(s) across ${#subjects[@]} file(s)," \
     "${aggregating} of them aggregating; every aggregating one collapses on its full" \
     "identity (${#base_keys[@]} grandfathered)."
