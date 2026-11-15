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
#     count* sum* avg* groupArray* topK* corr* covarPop* covarSamp*
#     quantile* median* stddev* varPop* varSamp*
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
#     writes a lake read that way FAILS (use a raw literal). Test files are
#     exempt: they quote fragments in assertions.
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
#     The gate does not check that the inner GROUP BY is the full key.
#
# ── ESCAPE HATCH ─────────────────────────────────────────────────────
#
# scripts/ci/lint-lake-dedup.baseline: one `<file>:<table>  <reason>` entry per
# grandfathered site, reason REQUIRED. Growing it needs a `Baseline-Growth:`
# commit trailer (lint-baseline-growth.sh), and a stale entry (a site that no
# longer violates) fails the gate until deleted. It shrinks monotonically.
#
# Usage: lint-lake-dedup.sh [repo-root]
#   The optional root is for scripts/ci/lint-lake-dedup-test.sh (fixture trees).
#
# Exit 0 clean, non-zero on any violation.
set -euo pipefail
cd "${1:-$(dirname "$0")/../..}"

BASELINE="scripts/ci/lint-lake-dedup.baseline"
# Not every ReplacingMergeTree in the lake: the two the rule was written for,
# the three that carry amounts, and contract_events, which every protocol
# count is taken from.
LAKE_TABLES="transactions|operations|supply_flows|account_movements|movements_by_asset|contract_events"

die() { echo "lint-lake-dedup: FAIL — $*" >&2; exit 1; }

# Subject files: every tracked-or-untracked .go/.sql that NAMES a
# duplicate-bearing table (others cannot violate; the pre-filter keeps this to
# one awk pass over ~60 files).
#
# `find` not `git ls-files`: fixture trees are not checkouts. The prunes mirror
# lint-imports.sh's SKIP_DIRS plus .claude, where agent worktrees put whole
# repo copies in the walk.
subjects=()
while IFS= read -r f; do
  [ -n "$f" ] || continue
  subjects+=("$f")
done < <(
  find . \( -name .git -o -name vendor -o -name node_modules \
            -o -name .discovery-repos -o -name .claude \) -prune -o \
       -type f \( -name '*.go' -o -name '*.sql' \) -print \
    | sed 's#^\./##' \
    | sort \
    | while IFS= read -r f; do
        if grep -qiE "stellar\\.(${LAKE_TABLES})" "$f"; then
          echo "$f"
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

# ── The analyser ─────────────────────────────────────────────────────
#
# Emits one TAB-separated `<file>\t<line>\t<table>\t<snippet>` record per
# violating read plus a final `#counts <reads> <aggregating>` self-accounting
# line, so a pattern that stopped matching reads as a fault, not a clean run.
violations="$(
  LC_ALL=C awk -v READ_RE="(from|join|,) ?stellar\\.(${LAKE_TABLES})([^a-z0-9_]|\$)" \
               -v INTERP_RE="\"[^\"]*(from|join) stellar\\.(${LAKE_TABLES})([^a-z0-9_]|\$)" '
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
    function mult_agg(s) {
      gsub(/countdistinct[ ]*\(/, "", s)
      gsub(/count[ ]*\([ ]*distinct[ ]/, "", s)
      return s ~ /(^|[^a-z0-9_])(count|sum|avg|grouparray|topk|corr|covarpop|covarsamp|quantile|median|stddev|varpop|varsamp)[a-z0-9]*[ ]*\(/
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
                     owner, clause, blk, tail, snip, depth, ch) {
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
        sub(/[^a-z0-9_.].*$/, "", tbl)
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
        clause = own_text(block_of(L, owner))
        if (!mult_agg(clause)) continue                       # not an aggregating read
        aggregating++

        # FINAL attached to this read (optionally through an alias).
        tail = substr(L, pos + len - 1)
        if (tail ~ /^[ ]*(as[ ]+[a-z_][a-z0-9_]*[ ]+|[a-z_][a-z0-9_]*[ ]+)?final([^a-z0-9_]|$)/) continue

        blk = block_of(L, owner)
        snip = substr(N, owner, length(blk))
        if (length(snip) > 220) snip = substr(snip, 1, 220) " …"
        printf "%s\t%d\t%s\t%s\n", file, line, tbl, snip
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
    function go_mask(l, raw,    out, i, n, c, j) {
      out = ""; n = length(l); i = 1
      while (i <= n) {
        if (raw) {
          j = index(substr(l, i), "`")
          if (j == 0) return out substr(l, i)
          out = out substr(l, i, j); i += j; raw = 0
          continue
        }
        c = substr(l, i, 1)
        if (c == "`") { raw = 1; out = out c; i++; continue }
        if (c == "/" && substr(l, i, 2) == "//") return out
        if (c == "\"" || c == "\047") {
          j = i + 1
          while (j <= n && substr(l, j, 1) != c) { if (substr(l, j, 1) == "\\") j++; j++ }
          out = out " "; i = j + 1
          continue
        }
        out = out c; i++
      }
      return out
    }

    function go_line(l,    n, p, i, inside, part, cut) {
      # Only raw literals are analysed. A read in a double-quoted string in
      # production Go would never be examined, so it is reported instead.
      if (!in_raw && index(l, "`") == 0 && FILENAME !~ /_test\.go$/) {
        part = l; cut = index(part, "//"); if (cut > 0) part = substr(part, 1, cut - 1)
        if (tolower(part) ~ INTERP_RE) printf "#interp\t%s:%d\n", FILENAME, FNR
      }
      if (index(l, "`") > 0) {
        l = go_mask(l, in_raw)
      } else if (!in_raw) {
        cut = index(l, "//")
        if (cut > 0) l = substr(l, 1, cut - 1)
      }
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

    FNR == 1 { flush(); in_raw = 0; in_comment = 0; glue = ""
               is_sql = (FILENAME ~ /\.sql$/) }
    { if (is_sql) sql_line($0); else go_line($0) }
    END { flush(); printf "#counts %d %d\n", examined + 0, aggregating + 0 }
  ' "$canary_file" "${subjects[@]}"
)"

examined="$(printf '%s\n' "$violations" | awk '/^#counts /{print $2}')"
aggregating="$(printf '%s\n' "$violations" | awk '/^#counts /{print $3}')"
interpreted="$(printf '%s\n' "$violations" | sed -n 's/^#interp\t/  /p')"
violations="$(printf '%s\n' "$violations" | grep -v '^#counts \|^#interp' || true)"
if [ -n "$interpreted" ]; then
  echo "lint-lake-dedup: FAIL — a lake-table read in a double-quoted Go string is not examined by this gate; write the SQL as a raw (backtick) literal:" >&2
  printf '%s\n' "$interpreted" >&2
  exit 1
fi

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
# `<file>:<table>  <reason>`; the reason is required: an unexplained exemption
# is what baselines exist to stop.
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
  # An entry for a table this copy does not watch is not stale: CI judges a PR
  # with the base branch's copy, which must accept the entries of the PR that
  # adds the table.
  [[ "${b##*:stellar.}" =~ ^(${LAKE_TABLES})$ ]] || continue
  case "$observed" in
    *"|$b|"*) : ;;
    *) echo "lint-lake-dedup: baseline stale: $b no longer violates — remove it from $BASELINE." >&2
       stale=1 ;;
  esac
done

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

If the read is genuinely exempt, add it to scripts/ci/lint-lake-dedup.baseline
with a reason and a `Baseline-Growth:` commit trailer naming that file.
EOF
  exit 1
fi
[ "$stale" -eq 0 ] || exit 1

echo "lint-lake-dedup: OK — ${examined} lake-table read(s) across ${#subjects[@]} file(s)," \
     "${aggregating} of them aggregating; every aggregating one reads through FINAL" \
     "(${#base_keys[@]} grandfathered)."
