#!/usr/bin/env bash
# lint-ch-apply-scope.sh — deploy/clickhouse/ is not a bootstrap manifest.
#
# ── THE DEFECT THIS EXISTS FOR (2026-09-09) ──────────────────────────────
#
# configs/ansible/roles/archival-node/tasks/08-clickhouse.yml used to copy
# `deploy/clickhouse/*.sql` with with_fileglob and execute every file it
# found, wherever clickhouse_apply_schema is true — which is BOTH
# configs/ansible/inventory/testnet.yml and .../futurenet.yml. But that
# directory holds one founding DDL (tier1_schema.sql) and fifteen OPERATOR
# artifacts for the EXISTING deployment (r1): per-feature DDL mirrors with
# their windowed-backfill runbooks, plus four genuine migrations. Four of
# them said so in their own headers — "NOT auto-applied by any bootstrap",
# "A FRESH deployment does NOT need this file", and in one case
# "FREEZE-GATED: do NOT run" — and every one of those claims was FALSE on a
# test net.
#
# Most of it was invisible: every CREATE in every non-tier1 file is
# byte-identical to tier1_schema.sql's own and tier1 is applied first, so
# `IF NOT EXISTS` made them no-ops. The two that were not built the cut-over
# halves — stellar.ledger_entries_current_v2 (+_mv) and
# stellar.contract_events_daily_v2 (+_mv) — as exact duplicates of their v1
# tables on hosts that have no v1 to cut over FROM. Confirmed live on the
# testnet host: identical column list and order, identical engine, ORDER BY
# and indexes; the MVs differ only in their TO target and read the same
# source with the same SELECT. Those hosts do double materialized-view write
# work and hold a second copy of the same rows, permanently, for a migration
# they will never run.
#
# The fix made the apply set a default-deny allow-list. This gate is the
# other half: an allow-list nobody is forced to maintain rots into a silent
# omission, and a plain "put migrations in a subdirectory" convention fails
# open the moment somebody drops the next one in the wrong place.
#
# ── THE RULES ─────────────────────────────────────────────────────────────
#
#   1. Every deploy/clickhouse/*.sql declares exactly one
#      `-- si-apply-scope: fresh-host|operator` in its HEADER (the leading
#      comment run). A NEW file is therefore unclassified and RED until
#      somebody says which it is — that is the property a directory
#      convention cannot give.
#   2. The ansible allow-list (`clickhouse_fresh_host_schema:` in
#      08-clickhouse.yml) is EXACTLY the set of fresh-host-scoped files, in
#      both directions. A fresh-host file missing from the list would be a
#      lake object nothing creates; a listed file that is not fresh-host is
#      the original defect.
#   3. tier1_schema.sql is fresh-host and is FIRST in that list — it CREATEs
#      the `stellar` database everything else needs.
#   4. A fresh-host file contains ONLY `CREATE … IF NOT EXISTS`. Mutating and
#      destructive verbs (ALTER/DROP/RENAME/EXCHANGE/INSERT/TRUNCATE/
#      OPTIMIZE/DETACH/ATTACH/SYSTEM/SET) belong to an operator with a
#      runbook, never to an unattended provision.
#   5. Every object an OPERATOR file creates is also created by a fresh-host
#      file, with an equivalent statement (comments stripped, whitespace
#      collapsed) — unless that file declares it `-- si-cutover-object: X`.
#      This is what makes "apply tier1_schema.sql alone" provably sufficient
#      instead of incidentally sufficient: a genuinely-new fresh-host table
#      cannot hide inside a migration, and a mirror cannot silently drift
#      from the canonical DDL.
#   6. A si-cutover-object is NOT declared by any fresh-host file. A
#      completed cut-over ends by renaming v2 onto the base name and
#      dropping the v2 names, so codifying v2 in the founding DDL would make
#      SUCCESS read as schema drift forever.
#   7. The task file does not glob deploy/clickhouse/*.sql — reintroducing
#      with_fileglob there is the defect itself.
#
# Exit 0 = clean. Exit 1 = findings (each printed with its rule). Prints a
# self-accounting line, so a scan of zero files cannot pass silently.

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root" || exit 1

CH_DIR="${CH_DIR:-deploy/clickhouse}"
TASK_FILE="${TASK_FILE:-configs/ansible/roles/archival-node/tasks/08-clickhouse.yml}"

fail=0
note() { echo "lint-ch-apply-scope: $*" >&2; fail=$((fail + 1)); }

for p in "$CH_DIR" "$TASK_FILE"; do
  if [ ! -e "$p" ]; then
    echo "lint-ch-apply-scope: FAIL — not found: $p (gate must not pass vacuously)" >&2
    exit 1
  fi
done

# (bash 3.2 on macOS has no mapfile — read loops.)
sql_files=()
while IFS= read -r f; do sql_files+=("$f"); done < <(find "$CH_DIR" -maxdepth 1 -type f -name '*.sql' | sort)

if [ "${#sql_files[@]}" -eq 0 ]; then
  echo "lint-ch-apply-scope: FAIL — no *.sql under $CH_DIR (gate must not pass vacuously)" >&2
  exit 1
fi

# ── Parser ───────────────────────────────────────────────────────────────
# Emits one record per line, tab-separated:
#   SCOPE   <value>                       header marker
#   CUTOVER <object>                      header marker
#   CREATE  <object>  <normalized stmt>   a CREATE, comments stripped
#   VERB    <verb>                        any other statement's leading verb
# The header is the leading run of blank/`--` lines; a marker below the
# first SQL statement is deliberately NOT seen, so it cannot hide halfway
# down a runbook.
parse() {
  awk '
    function emit(stmt,   lo, pfx, n, a, name) {
      gsub(/[ \t\r\n]+/, " ", stmt)
      sub(/^ +/, "", stmt); sub(/ +$/, "", stmt)
      if (stmt == "") return
      lo = tolower(stmt)
      if (match(lo, /^create +(or +replace +)?(materialized +)?(table|view|database|dictionary) +(if +not +exists +)?[a-z0-9_.]+/)) {
        pfx = substr(lo, 1, RLENGTH)
        n = split(pfx, a, " ")
        name = a[n]
        print "CREATE\t" name "\t" lo
      } else {
        n = split(lo, a, " ")
        print "VERB\t" a[1]
      }
    }
    BEGIN { inheader = 1; buf = "" }
    {
      t = $0
      sub(/^[ \t]+/, "", t)
      if (inheader) {
        if (t == "") {
          # blank line: still header
        } else if (t ~ /^--/) {
          if (match(t, /^--[ \t]*si-apply-scope:[ \t]*/)) {
            v = substr(t, RSTART + RLENGTH); sub(/[ \t]+$/, "", v); print "SCOPE\t" v
          } else if (match(t, /^--[ \t]*si-cutover-object:[ \t]*/)) {
            v = substr(t, RSTART + RLENGTH); sub(/[ \t]+$/, "", v); print "CUTOVER\t" tolower(v)
          }
        } else {
          inheader = 0
        }
      }
      s = $0
      sub(/^[ \t]*--.*$/, "", s)     # whole-line comment
      sub(/[ \t]--[ \t].*$/, "", s)  # trailing inline comment
      buf = buf " " s
      while ((i = index(buf, ";")) > 0) {
        emit(substr(buf, 1, i - 1))
        buf = substr(buf, i + 1)
      }
    }
    END { emit(buf) }
  ' "$1"
}

# ── Pass 1: scope markers, and the fresh-host object catalogue ───────────
fresh_files=""     # " a.sql b.sql "
operator_files=""
n_obj=0
declare_count=0

scope_of() { # $1 = basename → prints scope, or nothing
  case " $fresh_files " in *" $1 "*) echo fresh-host; return;; esac
  case " $operator_files " in *" $1 "*) echo operator; return;; esac
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
# The fresh-host object catalogue is one file per object ($tmp/fresh/<name>
# holding its normalized statement) rather than a greppable blob: #475 —
# `… | grep -q` under `set -o pipefail` is a coin flip once the producer
# outsizes the 64 KiB pipe buffer, and tier1_schema.sql's 60 statements are
# comfortably past that.
mkdir -p "$tmp/fresh"

for f in "${sql_files[@]}"; do
  b="$(basename "$f")"
  parse "$f" > "$tmp/$b.rec"
  scopes="$(awk -F'\t' '$1=="SCOPE"{print $2}' "$tmp/$b.rec")"
  n="$(printf '%s\n' "$scopes" | grep -c . )"
  if [ "$n" -eq 0 ]; then
    note "rule 1: $f declares no \`-- si-apply-scope:\` in its header." \
         "Add \`-- si-apply-scope: fresh-host\` (a fresh provision applies it) or" \
         "\`-- si-apply-scope: operator\` (an operator runs it by hand against an existing deployment)."
    continue
  fi
  if [ "$n" -gt 1 ]; then
    note "rule 1: $f declares $n si-apply-scope markers; exactly one is allowed."
    continue
  fi
  case "$scopes" in
    fresh-host) fresh_files="$fresh_files $b" ;;
    operator)   operator_files="$operator_files $b" ;;
    *) note "rule 1: $f declares si-apply-scope: '$scopes' — must be 'fresh-host' or 'operator'." ;;
  esac
done

for b in $fresh_files; do
  while IFS="$(printf '\t')" read -r kind name stmt; do
    [ "$kind" = CREATE ] || continue
    printf '%s' "$stmt" > "$tmp/fresh/$name"
    n_obj=$((n_obj + 1))
    case "$stmt" in
      *"if not exists"*) : ;;
      *) note "rule 4: $CH_DIR/$b creates $name without IF NOT EXISTS." \
              "A fresh-host file is applied unattended and must be re-runnable." ;;
    esac
  done < "$tmp/$b.rec"
  verbs="$(awk -F'\t' '$1=="VERB"{print $2}' "$tmp/$b.rec" | sort -u | tr '\n' ' ')"
  if [ -n "${verbs// /}" ]; then
    note "rule 4: $CH_DIR/$b is fresh-host but contains non-CREATE statement(s): ${verbs% }." \
         "Mutating/destructive DDL belongs to an operator with a runbook, not to an unattended provision."
  fi
done

# ── Pass 2: every operator-created object is a fresh-host object too ─────
for b in $operator_files; do
  cutovers=" $(awk -F'\t' '$1=="CUTOVER"{print $2}' "$tmp/$b.rec" | tr '\n' ' ')"
  while IFS="$(printf '\t')" read -r kind name stmt; do
    [ "$kind" = CREATE ] || continue
    case "$cutovers" in
      *" $name "*)
        # rule 6: a cut-over target must NOT be codified as founding DDL.
        if [ -f "$tmp/fresh/$name" ]; then
          note "rule 6: $CH_DIR/$b declares si-cutover-object: $name, but a fresh-host file also creates it." \
               "A completed cut-over renames v2 onto the base name and drops the v2 name, so codifying it makes SUCCESS read as drift forever."
        fi
        continue ;;
    esac
    fresh_stmt=""
    [ -f "$tmp/fresh/$name" ] && fresh_stmt="$(cat "$tmp/fresh/$name")"
    if [ -z "$fresh_stmt" ]; then
      note "rule 5: $CH_DIR/$b creates $name, which no fresh-host file creates." \
           "A fresh host applies only the fresh-host set, so this object would never exist there." \
           "Either add it to $CH_DIR/tier1_schema.sql, or declare it \`-- si-cutover-object: $name\` if it is a transient cut-over half."
    elif [ "$fresh_stmt" != "$stmt" ]; then
      note "rule 5: $CH_DIR/$b's declaration of $name has DRIFTED from the fresh-host one." \
           "A fresh host gets the fresh-host copy, so the two must agree."
      echo "        operator:   $stmt" >&2
      echo "        fresh-host: $fresh_stmt" >&2
    fi
  done < "$tmp/$b.rec"
done

# ── Pass 3: the ansible allow-list ───────────────────────────────────────
# Comment lines are dropped first: the task file EXPLAINS the removed
# with_fileglob at length, and a gate that fires on the prose describing the
# defect is a gate people delete.
task_code="$(sed -e 's/^[[:space:]]*#.*$//' "$TASK_FILE")"
case "$task_code" in
  *"$CH_DIR/*"*)
    note "rule 7: $TASK_FILE has a live $CH_DIR/* glob (with_fileglob or otherwise)." \
         "That directory is not a bootstrap manifest — name the fresh-host files in clickhouse_fresh_host_schema instead." ;;
esac

declare_count="$(grep -cE '^[[:space:]]*clickhouse_fresh_host_schema:[[:space:]]*$' "$TASK_FILE")"
if [ "$declare_count" -ne 1 ]; then
  note "rule 2: $TASK_FILE declares clickhouse_fresh_host_schema $declare_count time(s); exactly one is required." \
       "That key IS the fresh-host apply set; this gate has nothing to compare against without it."
else
  listed="$(awk '
    /^[[:space:]]*clickhouse_fresh_host_schema:[[:space:]]*$/ { collecting = 1; next }
    collecting {
      if ($0 ~ /^[[:space:]]*-[[:space:]]+[^[:space:]]+\.sql[[:space:]]*$/) {
        gsub(/^[[:space:]]*-[[:space:]]+/, ""); gsub(/[[:space:]]+$/, ""); print; next
      }
      collecting = 0
    }' "$TASK_FILE")"
  first="${listed%%$'\n'*}"
  [ "$first" = tier1_schema.sql ] || note \
    "rule 3: clickhouse_fresh_host_schema starts with '${first:-<empty>}', not tier1_schema.sql." \
    "tier1 CREATEs the \`stellar\` database every later file needs, so it must be applied first."

  for b in $listed; do
    if [ ! -f "$CH_DIR/$b" ]; then
      note "rule 2: clickhouse_fresh_host_schema names $b, which does not exist under $CH_DIR/."
      continue
    fi
    s="$(scope_of "$b")"
    [ "$s" = fresh-host ] || note \
      "rule 2: clickhouse_fresh_host_schema names $b, whose si-apply-scope is '${s:-unclassified}'." \
      "A fresh provision would run an operator artifact unattended — that is the defect this gate exists for."
  done
  listed_flat=" ${listed//$'\n'/ } "
  for b in $fresh_files; do
    case "$listed_flat" in
      *" $b "*) : ;;
      *) note "rule 2: $CH_DIR/$b is si-apply-scope: fresh-host but is NOT in clickhouse_fresh_host_schema." \
              "A fresh host would never create its objects." ;;
    esac
  done
fi

n_fresh=0; for b in $fresh_files; do n_fresh=$((n_fresh + 1)); done
n_op=0; for b in $operator_files; do n_op=$((n_op + 1)); done

if [ "$fail" -ne 0 ]; then
  cat >&2 <<EOF

deploy/clickhouse/ holds ONE founding DDL and a pile of operator artifacts.
The apply set is the allow-list in
$TASK_FILE
(clickhouse_fresh_host_schema), and every file in $CH_DIR/ must say which
kind it is in its header:

    -- si-apply-scope: fresh-host   applied unattended on every fresh
                                    provision where clickhouse_apply_schema
                                    is true (testnet, futurenet)
    -- si-apply-scope: operator     run by hand, against an EXISTING
                                    deployment, with its runbook

An operator artifact may create an object of its own only by declaring
\`-- si-cutover-object: <name>\` — reserved for the transient v2 halves of a
cut-over, which are renamed onto the base name and dropped when it lands.
EOF
  echo "lint-ch-apply-scope: ${#sql_files[@]} file(s) examined, ${fail} finding(s)." >&2
  exit 1
fi

echo "lint-ch-apply-scope: OK — ${#sql_files[@]} of ${#sql_files[@]} file(s) in $CH_DIR/ classified" \
     "(${n_fresh} fresh-host declaring ${n_obj} object(s), ${n_op} operator);" \
     "the allow-list in $TASK_FILE matches exactly, and every object an operator" \
     "artifact creates is declared identically by a fresh-host file."
