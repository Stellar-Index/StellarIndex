#!/usr/bin/env bash
# ch-schema-drift.sh — the other half of ADR-0043 §2.1 (C6-008/C6-003).
#
# ch-schema-snapshot.sh answers "what IS the live schema" and keeps a
# daily copy of it. Nothing answered "is the live schema what this
# repository says it should be". The Tier-1 lake DDL has always been
# HAND-APPLIED — an operator pastes deploy/clickhouse/tier1_schema.sql
# (and the ad-hoc ALTERs that followed) into clickhouse-client. There is
# no migration runner for ClickHouse the way migrations/ + golang-migrate
# cover Postgres, so an ORDER BY changed by hand on r1, a column added
# during an incident, or a table that never got created at all, produced
# no signal anywhere. A 60M-ledger re-derive against a sort order that
# differs from the one the repo assumes does not error — it silently
# writes a differently-ordered table.
#
# This is the drift CHECK. It compares REPO INTENT against LIVE:
#
#   intent  deploy/clickhouse/tier1_schema.sql — the founding DDL, the
#           only machine-readable statement of what this repo believes
#           the lake's structure is.
#   live    the schema.sql that ch-schema-snapshot.sh captures from
#           `SHOW CREATE TABLE` every day (or, with LIVE=1, a fresh
#           SHOW CREATE straight off the server).
#
# WHAT IT COMPARES, and why not more. Both sides go through the SAME
# parser and the SAME normalizer, so ClickHouse's re-rendering of its own
# DDL mostly cancels out. Per table declared in tier1_schema.sql:
#
#   * existence      — declared in the repo, missing live = drift.
#   * ENGINE         — including its arguments, so a ReplacingMergeTree
#                      whose version column changed is caught.
#   * PARTITION BY   — ADR-0043 names this explicitly; a changed
#                      partition expression re-shapes every part.
#   * ORDER BY       — likewise. This is the one that silently corrupts
#                      a rebuild rather than failing it.
#   * column NAMES, in order — an added/dropped/reordered column.
#
# Column TYPES are reported as INFO, not drift. ClickHouse re-renders
# types, DEFAULT expressions, CODECs and TTLs in its own canonical form,
# and enforcing textual equality on those is a false-positive machine —
# which ends with the operator ignoring the check, i.e. worse than no
# check. The daily snapshot's schema.sql remains the authoritative record
# of the exact live types.
#
# The reverse direction — tables that exist live but are absent from
# tier1_schema.sql — is COUNTED AND LISTED but does not fail. That is not
# leniency, it is accuracy: tier1_schema.sql is the FOUNDING DDL (see
# ch-schema-snapshot.sh's own header), and materialized views, serving
# tables and later migrations have legitimately landed on top of it since.
# Failing on them would make this check red on day one for a known and
# accepted reason, which is exactly the "13 unnamed changed tasks" slack
# that scripts/ci/check-ansible-drift.sh was rewritten to abolish. Making
# that direction enforceable needs a NAMED baseline with a per-entry
# reason, under scripts/ci/ so scripts/ci/lint-baseline-growth.sh's
# Baseline-Growth tripwire covers it — an ungated allowlist anywhere else
# would just be the same hole with a new address. Tracked as follow-up;
# the uncodified count is exported as a metric so the growth is visible
# in the meantime.
#
# Usage:
#   ch-schema-drift.sh                       # daily timer shape: newest snapshot
#   LIVE=1 ch-schema-drift.sh                # query ClickHouse directly
#   LIVE_SCHEMA=/path/schema.sql ch-schema-drift.sh   # an explicit capture
#
# Exit code:
#   0  no drift
#   1  DRIFT — live differs from tier1_schema.sql on a compared attribute
#   2  cannot compare (no snapshot found / ClickHouse unreachable). NOT 0:
#      "we could not check" must never read as "we checked and it's fine",
#      which is the failure mode ADR-0043 exists to prevent.
set -uo pipefail

# Lives in the role's files/ dir because that is where this repo keeps
# textfile-collector emitters and where scripts/ci/lint-metric-refs.sh
# looks for the producer of a stellarindex_* metric named in an alert
# rule (EMITTER_PATHS) — a .prom emitter outside those paths reads as a
# dead alert reference to that linter. The repo root is five levels up
# when run from a checkout; on r1 the script is installed standalone and
# INTENT is set explicitly by the systemd unit, so this is only the
# from-a-checkout default.
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." && pwd)"

INTENT="${INTENT:-$repo_root/deploy/clickhouse/tier1_schema.sql}"
SNAPSHOT_DIR="${SNAPSHOT_DIR:-/var/lib/stellarindex/ch-schema-snapshot}"
LIVE_SCHEMA="${LIVE_SCHEMA:-}"
LIVE="${LIVE:-0}"
CH_HTTP="${CH_HTTP:-http://127.0.0.1:8123/}"
CH_DATABASE="${CH_DATABASE:-stellar}"
TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"

note() { echo "ch-schema-drift: $*" >&2; }
ch() { curl -sSf --max-time 120 "$CH_HTTP" --data-binary "$1"; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# ─── the parser ─────────────────────────────────────────────────────
# One awk program, applied to BOTH sides, so any asymmetry in the
# comparison is a bug in one place rather than a divergence between two
# hand-kept parsers. Emits one normalized fact per line:
#
#   <table>\tengine\t<engine incl. args>
#   <table>\tpartition\t<expr>
#   <table>\torder\t<expr>
#   <table>\tcolumns\t<name,name,name,...>
#   <table>\ttype\t<column>\t<declared type text>
#
# Normalization (identical on both sides): comments stripped, backticks
# dropped, whitespace runs collapsed, one outer paren pair removed from
# key expressions (SHOW CREATE renders a single-column ORDER BY bare and
# a tuple parenthesised; tier1_schema.sql is not consistent about it).
parse_schema() {
  awk '
    function norm(s) {
      gsub(/`/, "", s)
      gsub(/[ \t\r\n]+/, " ", s)
      sub(/^ +/, "", s); sub(/ +$/, "", s)
      sub(/;$/, "", s)
      sub(/ +$/, "", s)
      # strip ONE outer paren pair: "(a, b)" -> "a, b"
      if (s ~ /^\(.*\)$/) {
        inner = substr(s, 2, length(s) - 2)
        # only when the parens actually wrap the whole expression
        depth = 0; ok = 1
        for (i = 1; i <= length(inner); i++) {
          c = substr(inner, i, 1)
          if (c == "(") depth++
          else if (c == ")") { depth--; if (depth < 0) { ok = 0; break } }
        }
        if (ok && depth == 0) s = inner
      }
      gsub(/ *, */, ", ", s)
      return s
    }
    function flush_table() {
      if (tbl == "") return
      printf "%s\tkind\t%s\n", tbl, kind
      if (kind == "view") {
        # Materialized views are compared on EXISTENCE + destination
        # table only. SHOW CREATE renders an MV with a ClickHouse-
        # inferred column list and a re-formatted SELECT body that the
        # founding DDL does not write, so comparing either would report
        # drift on every MV, forever. What actually matters — and what
        # ADR-0043 calls out ("an MV that is missing after a rebuild is
        # a silently empty serving surface") — is that it exists and
        # still feeds the table the repo says it feeds.
        printf "%s\tto\t%s\n", tbl, norm(mvto)
      } else {
        printf "%s\tengine\t%s\n", tbl, norm(engine)
        printf "%s\tpartition\t%s\n", tbl, norm(partexpr)
        printf "%s\torder\t%s\n", tbl, norm(orderexpr)
        printf "%s\tcolumns\t%s\n", tbl, cols
      }
      tbl = ""; kind = ""; mvto = ""; engine = ""; partexpr = ""
      orderexpr = ""; cols = ""; incols = 0; depth = 0
    }
    function mvtarget(s,   x) {
      x = s
      if (x !~ / TO +/) return ""
      sub(/.* TO +/, "", x)
      sub(/[ (].*$/, "", x)
      gsub(/`/, "", x)
      sub(/^[A-Za-z0-9_]+\./, "", x)
      return x
    }
    {
      line = $0
      sub(/--.*$/, "", line)           # line comments
      gsub(/\r/, "", line)
    }
    # ─ statement start ─
    line ~ /CREATE +(TABLE|MATERIALIZED +VIEW)/ {
      flush_table()
      kind = (line ~ /MATERIALIZED +VIEW/) ? "view" : "table"
      t = line
      sub(/.*CREATE +(TABLE|MATERIALIZED +VIEW)( +IF +NOT +EXISTS)? +/, "", t)
      sub(/[ (].*$/, "", t)
      gsub(/`/, "", t)
      sub(/^[A-Za-z0-9_]+\./, "", t)   # drop the database qualifier
      tbl = t
      mvto = (kind == "view") ? mvtarget(line) : ""
      incols = 0; depth = 0
      # a same-line "(" opens the column list (tables only; the column
      # list of an MV is ClickHouse-inferred, deliberately not compared)
      if (kind == "table" && (line ~ /\($/ || line ~ /\( *[A-Za-z`]/)) { incols = 1; depth = 1 }
      next
    }
    tbl == "" { next }
    kind == "view" && mvto == "" { mvto = mvtarget(" " line) }
    # ─ column list ─
    kind == "table" && incols == 0 && line ~ /^ *\( *$/ { incols = 1; depth = 1; next }
    incols == 1 {
      # NB: `close` is an awk builtin — these must not be named open/close.
      nopen = gsub(/\(/, "(", line); nclose = gsub(/\)/, ")", line)
      if (line ~ /^ *\) *$/ || (nclose > nopen && depth + nopen - nclose <= 0)) {
        incols = 2
        depth = 0
      } else {
        depth += nopen - nclose
        c = line
        sub(/^ +/, "", c); sub(/,? *$/, "", c)
        # Skip table-level clauses inside the parens. Anchored with an
        # explicit [ (] rather than \b — \b is a GNU-awk extension and
        # this has to parse identically under BWK awk and mawk too.
        if (c !~ /^(INDEX|PROJECTION|CONSTRAINT|PRIMARY|ALIAS)[ (]/ && c != "") {
          gsub(/`/, "", c)
          name = c; sub(/[ (].*$/, "", name)
          rest = c; sub(/^[A-Za-z0-9_]+ +/, "", rest)
          if (name != "" && name ~ /^[A-Za-z_][A-Za-z0-9_]*$/) {
            cols = (cols == "" ? name : cols "," name)
            printf "%s\ttype\t%s\t%s\n", tbl, name, rest
          }
        }
        next
      }
    }
    # ─ table-level clauses ─
    { rest = line }
    rest ~ /ENGINE *=/ {
      e = rest; sub(/.*ENGINE *= */, "", e)
      sub(/ +(PARTITION|ORDER|PRIMARY|SAMPLE|TTL|SETTINGS|AS) .*$/, "", e)
      engine = e
    }
    rest ~ /PARTITION +BY/ {
      p = rest; sub(/.*PARTITION +BY */, "", p)
      sub(/ +(ORDER|PRIMARY|SAMPLE|TTL|SETTINGS|AS) .*$/, "", p)
      partexpr = p
    }
    rest ~ /ORDER +BY/ {
      o = rest; sub(/.*ORDER +BY */, "", o)
      sub(/ +(PARTITION|PRIMARY|SAMPLE|TTL|SETTINGS|AS) .*$/, "", o)
      orderexpr = o
    }
    rest ~ /;/ { flush_table() }
    END { flush_table() }
  ' "$1"
}

# ─── resolve the live side ──────────────────────────────────────────
if [[ ! -r "$INTENT" ]]; then
  note "repo intent $INTENT is not readable — nothing to compare against"
  exit 2
fi

live_file=""
live_origin=""
if [[ "$LIVE" == "1" ]]; then
  tables="$(ch "SELECT name FROM system.tables
               WHERE database = '$CH_DATABASE' AND NOT is_temporary
               ORDER BY name FORMAT TabSeparated")" || {
    note "ClickHouse unreachable at $CH_HTTP — cannot compare"
    exit 2
  }
  live_file="$work/live-schema.sql"
  : > "$live_file"
  while IFS= read -r t; do
    [[ -z "$t" ]] && continue
    if ! ch "SHOW CREATE TABLE \`$CH_DATABASE\`.\`$t\` FORMAT TabSeparatedRaw" >> "$live_file"; then
      note "SHOW CREATE failed for $CH_DATABASE.$t — refusing a partial comparison"
      exit 2
    fi
    printf ';\n\n' >> "$live_file"
  done <<<"$tables"
  live_origin="live SHOW CREATE via $CH_HTTP"
elif [[ -n "$LIVE_SCHEMA" ]]; then
  live_file="$LIVE_SCHEMA"
  live_origin="$LIVE_SCHEMA"
else
  # Newest snapshot day directory that actually holds a schema.sql.
  newest="$(find "$SNAPSHOT_DIR" -mindepth 2 -maxdepth 2 -name schema.sql 2>/dev/null | sort | tail -1)"
  if [[ -z "$newest" ]]; then
    note "no snapshot schema.sql under $SNAPSHOT_DIR — run ch-schema-snapshot.sh first (or LIVE=1)"
    exit 2
  fi
  live_file="$newest"
  live_origin="snapshot $newest"
fi

if [[ ! -r "$live_file" ]]; then
  note "live schema $live_file is not readable — cannot compare"
  exit 2
fi

parse_schema "$INTENT"    > "$work/intent.facts"   || { note "failed to parse $INTENT"; exit 2; }
parse_schema "$live_file" > "$work/live.facts"     || { note "failed to parse $live_file"; exit 2; }

if [[ ! -s "$work/intent.facts" ]]; then
  note "parsed ZERO tables out of $INTENT — refusing to report 'no drift' from an empty intent"
  exit 2
fi
if [[ ! -s "$work/live.facts" ]]; then
  note "parsed ZERO tables out of $live_file — refusing to report 'no drift' from an empty live side"
  exit 2
fi

fact() { awk -F'\t' -v t="$2" -v k="$3" '$1==t && $2==k {print $3; exit}' "$1"; }
tables_in() { awk -F'\t' '$2=="kind" {print $1}' "$1" | sort -u; }

declared="$(tables_in "$work/intent.facts")"
livetabs="$(tables_in "$work/live.facts")"

drift=0
compared=0
divergent_tables=0

while IFS= read -r t; do
  [[ -z "$t" ]] && continue
  compared=$((compared + 1))
  if ! grep -qx "$t" <<<"$livetabs"; then
    note "DRIFT $t: declared in $(basename "$INTENT") but ABSENT from the live schema"
    drift=$((drift + 1)); divergent_tables=$((divergent_tables + 1))
    continue
  fi
  bad=0
  keys="engine partition order columns"
  if [[ "$(fact "$work/intent.facts" "$t" kind)" == "view" ]]; then
    keys="to"
    if [[ "$(fact "$work/live.facts" "$t" kind)" != "view" ]]; then
      note "DRIFT $t: declared as a MATERIALIZED VIEW in the repo but live as a table"
      drift=$((drift + 1)); divergent_tables=$((divergent_tables + 1))
      continue
    fi
  fi
  for key in $keys; do
    want="$(fact "$work/intent.facts" "$t" "$key")"
    got="$(fact "$work/live.facts" "$t" "$key")"
    if [[ "$want" != "$got" ]]; then
      note "DRIFT $t.$key:"
      note "    repo: $want"
      note "    live: $got"
      drift=$((drift + 1)); bad=1
    fi
  done
  [[ "$bad" -eq 1 ]] && divergent_tables=$((divergent_tables + 1))
done <<<"$declared"

# Type differences: reported, never fatal. See the header for why.
while IFS=$'\t' read -r t _ col want; do
  [[ -z "$t" ]] && continue
  got="$(awk -F'\t' -v t="$t" -v c="$col" '$1==t && $2=="type" && $3==c {print $4; exit}' "$work/live.facts")"
  if [[ -n "$got" && "$want" != "$got" ]]; then
    note "INFO $t.$col type text differs — repo: '$want' | live: '$got'"
  fi
done < <(awk -F'\t' '$2=="type"' "$work/intent.facts")

uncodified=0
while IFS= read -r t; do
  [[ -z "$t" ]] && continue
  if ! grep -qx "$t" <<<"$declared"; then
    uncodified=$((uncodified + 1))
    note "UNCODIFIED $t: exists live, absent from $(basename "$INTENT")"
  fi
done <<<"$livetabs"

note "compared $compared declared table(s) against $live_origin: $divergent_tables divergent, $uncodified uncodified live table(s)"

# ─── metrics ────────────────────────────────────────────────────────
if [[ "$TEXTFILE_DIR" != "/dev/null" ]]; then
  mkdir -p "$TEXTFILE_DIR"
  out="$TEXTFILE_DIR/ch_schema_drift.prom"
  tmp="$out.tmp.$$"
  {
    echo "# HELP stellarindex_ch_schema_drift_last_run_unix Unix time of the most recent completed repo-vs-live ClickHouse schema comparison."
    echo "# TYPE stellarindex_ch_schema_drift_last_run_unix gauge"
    echo "stellarindex_ch_schema_drift_last_run_unix $(date +%s)"
    echo "# HELP stellarindex_ch_schema_drift_tables Tables declared in deploy/clickhouse/tier1_schema.sql that were compared."
    echo "# TYPE stellarindex_ch_schema_drift_tables gauge"
    echo "stellarindex_ch_schema_drift_tables $compared"
    echo "# HELP stellarindex_ch_schema_drift_divergent Declared tables whose live engine/partition/order/columns differ from the repo, or that are missing live. Alert on > 0."
    echo "# TYPE stellarindex_ch_schema_drift_divergent gauge"
    echo "stellarindex_ch_schema_drift_divergent $divergent_tables"
    echo "# HELP stellarindex_ch_schema_drift_uncodified Tables present live but absent from tier1_schema.sql (informational; expected > 0 while the founding DDL is not the whole schema)."
    echo "# TYPE stellarindex_ch_schema_drift_uncodified gauge"
    echo "stellarindex_ch_schema_drift_uncodified $uncodified"
  } > "$tmp"
  chmod 644 "$tmp"
  mv "$tmp" "$out"
fi

if [[ "$drift" -gt 0 ]]; then
  note "SCHEMA DRIFT: $drift divergence(s) across $divergent_tables table(s)."
  note "Either the live schema was changed without codifying it (update"
  note "$INTENT in the same change that applied it), or the repo declares"
  note "something never applied. Do NOT silence this by editing the repo to"
  note "match live without understanding which direction is correct — an"
  note "ORDER BY that differs from what a re-derive assumes writes a"
  note "silently mis-sorted table. See docs/operations/runbooks/ch-schema-restore.md."
  exit 1
fi
exit 0
