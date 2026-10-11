#!/usr/bin/env bash
# ch-schema-drift.sh — compares the repo's Tier-1 lake DDL (intent) against the live
# ClickHouse schema (ADR-0043 §2.1).
#
# The lake DDL is hand-applied; ClickHouse has no migration runner. An ORDER BY changed by
# hand, a column added during an incident or a table never created gives no signal, and a
# re-derive against a different sort order silently writes a mis-sorted table.
#
#   intent  deploy/clickhouse/tier1_schema.sql, the founding DDL.
#   live    a fresh SHOW CREATE sweep off the server by default (see "which live side").
#
# Both sides go through the same parser and normalizer, so ClickHouse's re-rendering of its
# own DDL mostly cancels out. Per table declared in the intent:
#
#   * existence, ENGINE with its arguments (catches a changed ReplacingMergeTree version
#     column), PARTITION BY, ORDER BY, and column names in order
#   * secondary INDEX (name + TYPE + params + GRANULARITY, exact text) and PROJECTION /
#     CONSTRAINT names, in BOTH directions: declared-but-missing and live-but-undeclared
#
# Cut-over objects (v2 halves marked `-- si-cutover-object:`, deliberately absent from
# tier1_schema.sql) are compared the same way WHEN LIVE, from every marked *.sql beside the
# intent or the colon-separated INTENT_CUTOVER list. Absent live means no cut-over in progress.
#
# Column TYPES are INFO only: ClickHouse re-renders types, DEFAULTs, CODECs and TTLs, and
# textual equality there is a false-positive machine that trains operators to ignore the check.
#
# Tables live but absent from tier1_schema.sql are counted and listed, never failed: the
# founding DDL is not the whole schema (MVs, serving tables and later migrations landed on
# top). Enforcing that needs a NAMED baseline under scripts/ci/ so lint-baseline-growth.sh
# covers it; until then the uncodified count is exported as a metric.
#
# Usage:
#   ch-schema-drift.sh                       # DEFAULT: fresh SHOW CREATE sweep
#   LIVE=1 ch-schema-drift.sh                # the same thing, said out loud
#   LIVE=0 ch-schema-drift.sh                # the newest daily snapshot instead
#   LIVE_SCHEMA=/path/schema.sql ch-schema-drift.sh   # an explicit capture
#   INTENT_CUTOVER=a.sql:b.sql ch-schema-drift.sh     # cut-over DDL by name ("" = none)
#
# The intent side resolves from $INTENT, else the copy the archival-node role ships to the
# host, else this checkout, so a bare run on a host needs no environment.
#
# Exit code:
#   0  no drift
#   1  DRIFT — live differs from tier1_schema.sql on a compared attribute
#   2  cannot compare: ClickHouse unreachable; no intent; no snapshot under LIVE=0; under
#      LIVE=0 a capture older than the intent; or the intent predates the deployed release.
#      NOT 0: "we could not check" must never read as "we checked and it's fine" (ADR-0043).
set -uo pipefail

# Lives in the role's files/ dir because scripts/ci/lint-metric-refs.sh looks there
# (EMITTER_PATHS) for the producer of a stellarindex_* metric named in an alert rule.
#
# INSTALLED_INTENT is where 18-pgbackrest-backup.yml's "Ship the repo's Tier-1 lake DDL..."
# task puts the DDL on a host. It must equal that task's dest, the role's
# ch_schema_drift_intent default and the unit template's fallback;
# ch-schema-drift-test.sh pins all four.
INSTALLED_INTENT="${INSTALLED_INTENT:-/usr/local/share/stellarindex/tier1_schema.sql}"

INTENT="${INTENT:-}"
SNAPSHOT_DIR="${SNAPSHOT_DIR:-/var/lib/stellarindex/ch-schema-snapshot}"
LIVE_SCHEMA="${LIVE_SCHEMA:-}"
# Not ${LIVE:-1}: an explicit LIVE=0 asks for the snapshot, unset gets live.
live_explicit="${LIVE-}"
CH_HTTP="${CH_HTTP:-http://127.0.0.1:8123/}"
# CH as ops_monitor once the role renders this file; /dev/null (no
# credential, CH `default`) until then, so no deploy order strands this script.
CH_NETRC="${CH_NETRC:-/etc/clickhouse-client/ops-monitor.netrc}"
if [[ ! -r "$CH_NETRC" ]]; then
  echo "ch-schema-drift.sh: credential file $CH_NETRC unreadable; using ClickHouse default user" >&2
  CH_NETRC=/dev/null
fi
CH_DATABASE="${CH_DATABASE:-stellar}"
TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
# Per-binary sidecars (contents = tag, mtime = deploy) written by every ansible binary
# deploy; the same "what is this host running" source deploy.yml and the
# binary-version-skew probe read (docs/operations/deployed-versions.md).
DEPLOYED_VERSIONS_DIR="${DEPLOYED_VERSIONS_DIR:-/var/lib/stellarindex/deployed-versions}"

note() { echo "ch-schema-drift: $*" >&2; }
ch() { curl -sSf --max-time 120 --netrc-file "$CH_NETRC" "$CH_HTTP" --data-binary "$1"; }

# ─── where the repo's intent comes from ─────────────────────────────
# Tried in order:
#   1. $INTENT (ch-schema-drift.service sets it, or an operator). An explicit INTENT that
#      cannot be read is a hard stop, never replaced by a fallback: a typo must not compare
#      against a file nobody named.
#   2. the copy the role ships to the host. Hosts have no checkout, and the by-hand run in
#      ch-schema-restore.md relies on this path.
#   3. this checkout (the role's files/ dir is five levels below the repo root). Installed
#      standalone at /usr/local/bin the arithmetic clamps at `/` and would yield the
#      impossible `//deploy/clickhouse/tier1_schema.sql`, so `/` means "not a checkout".
#
# With none resolvable, exit 2 naming every path tried: "could not check" must never read
# as "checked, and fine", and the operator needs the one-step fix.
checkout_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../../.." 2>/dev/null && pwd)"
[[ "$checkout_root" == "/" ]] && checkout_root=""
checkout_intent="${checkout_root:+$checkout_root/deploy/clickhouse/tier1_schema.sql}"

if [[ -z "$INTENT" ]]; then
  if [[ -r "$INSTALLED_INTENT" ]]; then
    INTENT="$INSTALLED_INTENT"
  elif [[ -n "$checkout_intent" && -r "$checkout_intent" ]]; then
    INTENT="$checkout_intent"
  else
    note "no repo intent to compare against — INTENT is unset and no candidate is readable:"
    note "    shipped copy   $INSTALLED_INTENT"
    note "    this checkout  ${checkout_intent:-(not running from a checkout)}"
    note "Re-apply the archival-node role (its \"Ship the repo's Tier-1 lake DDL\""
    note "task installs the shipped copy), or set INTENT to a readable copy of"
    note "deploy/clickhouse/tier1_schema.sql. See docs/operations/runbooks/ch-schema-restore.md."
    exit 2
  fi
fi

# ─── cut-over objects: the operator DDL beside the intent ─────
# deploy/clickhouse/*.sql files marked `-- si-cutover-object: X` declare the v2 halves of
# an in-flight migration. They stay out of tier1_schema.sql (a completed cut-over would read
# as drift forever; lint-ch-apply-scope.sh rule 6), but unparsed they would land in the
# UNCODIFIED bucket and a wrong version column or ORDER BY would go uncompared. Each marked
# file beside the intent (or the INTENT_CUTOVER list; empty = none) goes through the same
# parser and is compared like a declared table WHEN LIVE.
cutover_files=()
if [[ -n "${INTENT_CUTOVER+x}" ]]; then
  IFS=: read -r -a cutover_files <<<"$INTENT_CUTOVER"
else
  for f in "$(dirname "$INTENT")"/*.sql; do
    [[ -r "$f" && "$f" != "$INTENT" ]] || continue
    grep -q '^-- si-cutover-object:' "$f" && cutover_files+=("$f")
  done
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# ─── the parser ─────────────────────────────────────────────────────
# One awk program applied to BOTH sides, so any asymmetry is a bug in one place. Emits one
# normalized fact per line:
#
#   <table>\tengine\t<engine incl. args>
#   <table>\tpartition\t<expr>
#   <table>\torder\t<expr>
#   <table>\tcolumns\t<name,name,name,...>
#   <table>\ttype\t<column>\t<declared type text>
#
# Normalization: comments stripped, backticks dropped, whitespace collapsed, one outer paren
# pair removed from key expressions (SHOW CREATE renders a single-column ORDER BY bare and
# a tuple parenthesised; tier1_schema.sql is inconsistent).
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
      # `CREATE TABLE x AS db.base;` (the *_staging halves of truncate-fill-EXCHANGE cycles)
      # has no inline ENGINE/columns/ORDER BY; empty facts would read as 3 drifts per staging
      # table against the fully rendered live DDL. Emit an alias fact instead; the comparer
      # resolves it against the base declaration.
      if (kind == "table" && aliasbase != "" && engine == "" && cols == "") {
        printf "%s\talias\t%s\n", tbl, aliasbase
        tbl = ""; kind = ""; mvto = ""; engine = ""; partexpr = ""
        orderexpr = ""; cols = ""; incols = 0; depth = 0; aliasbase = ""; inproj = 0
        return
      }
      if (kind == "view") {
        # MVs compare on existence + destination table only: SHOW CREATE adds an inferred
        # column list and reformats the SELECT, so comparing either would drift forever. A
        # missing or repointed MV is the silently empty serving surface ADR-0043 names.
        printf "%s\tto\t%s\n", tbl, norm(mvto)
      } else {
        printf "%s\tengine\t%s\n", tbl, norm(engine)
        printf "%s\tpartition\t%s\n", tbl, norm(partexpr)
        printf "%s\torder\t%s\n", tbl, norm(orderexpr)
        printf "%s\tcolumns\t%s\n", tbl, cols
      }
      tbl = ""; kind = ""; mvto = ""; engine = ""; partexpr = ""
      orderexpr = ""; cols = ""; incols = 0; depth = 0; aliasbase = ""; inproj = 0
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
      # AS-clone: `CREATE TABLE x AS stellar.base;` (ENGINE ... AS SELECT is excluded by the ENGINE guard).
      aliasbase = ""
      if (kind == "table" && line !~ /ENGINE/ && line ~ / AS +[A-Za-z0-9_.\x60]+ *;? *$/) {
        a = line
        sub(/.* AS +/, "", a)
        sub(/ *;? *$/, "", a)
        gsub(/\x60/, "", a)
        sub(/^[A-Za-z0-9_]+\./, "", a)
        aliasbase = a
      }
      incols = 0; depth = 0
      # a same-line "(" opens the column list (tables only; an MV column list is inferred, not compared)
      if (kind == "table" && (line ~ /\($/ || line ~ /\( *[A-Za-z`]/)) { incols = 1; depth = 1 }
      next
    }
    tbl == "" { next }
    kind == "view" && mvto == "" { mvto = mvtarget(" " line) }
    # ─ AS-clone on a continuation line ─
    # tier1_schema.sql writes the clone as two lines (CREATE TABLE ... x_staging / AS stellar.x;),
    # which the statement-start capture misses. Without this the live check goes red while the
    # symmetric intent-vs-intent self-test stays green.
    kind == "table" && incols == 0 && cols == "" && engine == "" && line ~ /^ *AS +[A-Za-z0-9_.\x60]+ *;? *$/ {
      a = line
      sub(/^ *AS +/, "", a)
      sub(/ *;? *$/, "", a)
      gsub(/\x60/, "", a)
      sub(/^[A-Za-z0-9_]+\./, "", a)
      aliasbase = a
    }
    # ─ column list ─
    kind == "table" && incols == 0 && line ~ /^ *\( *$/ { incols = 1; depth = 1; next }
    incols == 1 {
      # NB: `close` is an awk builtin — these must not be named open/close.
      nopen = gsub(/\(/, "(", line); nclose = gsub(/\)/, ")", line)
      # A lone ")" closes the column list only at its own depth: a SHOW CREATE PROJECTION
      # body is an indented ( ... ) block whose closing paren sits one level deeper.
      if ((line ~ /^ *\) *$/ && depth <= 1) || (nclose > nopen && depth + nopen - nclose <= 0)) {
        incols = 2
        depth = 0
      } else {
        depth += nopen - nclose
        # Inside a multi-line PROJECTION body (name captured on its PROJECTION line): not columns.
        if (inproj) { if (depth <= 1) inproj = 0; next }
        c = line
        sub(/^ +/, "", c); sub(/,? *$/, "", c)
        # Explicit [ (] instead of \b: \b is GNU-awk only and this must parse under BWK awk and mawk.
        if (c !~ /^(INDEX|PROJECTION|CONSTRAINT|PRIMARY|ALIAS)[ (]/ && c != "") {
          gsub(/`/, "", c)
          name = c; sub(/[ (].*$/, "", name)
          rest = c; sub(/^[A-Za-z0-9_]+ +/, "", rest)
          # Column alignment in the repo DDL is not a type difference.
          gsub(/[ \t]+/, " ", rest)
          if (name != "" && name ~ /^[A-Za-z_][A-Za-z0-9_]*$/) {
            cols = (cols == "" ? name : cols "," name)
            printf "%s\ttype\t%s\t%s\n", tbl, name, rest
          }
        } else if (c ~ /^INDEX[ (]/) {
          # Secondary indices are real DRIFT, not INFO: type + params + GRANULARITY is exact
          # declared text on both sides. Otherwise a retuned bloom_filter rate (e.g.
          # idx_lec_key_xdr) would report no drift either way.
          ic = c
          sub(/^INDEX +/, "", ic)
          gsub(/`/, "", ic)
          iname = ic; sub(/[ (].*$/, "", iname)
          irest = ic; sub(/^[A-Za-z0-9_]+ +/, "", irest)
          if (iname != "" && iname ~ /^[A-Za-z_][A-Za-z0-9_]*$/) {
            printf "%s\tindex\t%s\t%s\n", tbl, iname, norm(irest)
          }
        } else if (c ~ /^(PROJECTION|CONSTRAINT) /) {
          # Projections and constraints compare by name only, both directions: ClickHouse
          # re-renders their bodies (SELECT, CHECK) the way it does an MV SELECT.
          pc = c
          pkind = (pc ~ /^PROJECTION /) ? "projection" : "constraint"
          sub(/^(PROJECTION|CONSTRAINT) +/, "", pc)
          gsub(/`/, "", pc)
          pname = pc; sub(/[ (].*$/, "", pname)
          if (pname != "" && pname ~ /^[A-Za-z_][A-Za-z0-9_]*$/) {
            printf "%s\t%s\t%s\t%s\n", tbl, pkind, pname, pkind
          }
          # The body may open on a later line (SHOW CREATE puts it there).
          if (pkind == "projection" && (depth > 1 || c !~ /\(/)) inproj = 1
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

# ─── is the INTENT side itself current? ────────────────────────────
# The intent is shipped by the same convergence this check polices, so a host that missed
# the role compares an old repo against today's server and calls it clean: green exactly
# when the host is furthest behind. Seen on the test nets: testnet (fresh intent, stale
# live) correctly read DRIFT, futurenet (stale intent, same stale live) read CLEAN. Reading
# live by default ("which live side") corrects the other side and does not fix this.
#
# So the role stamps the shipped intent at ship time:
#
#   -- Intent-Version: v0.67.0                the release the shipped copy came from
#   -- Intent-Schema-Commit: <sha> (<date>)   when the DDL content last changed
#
# and this check compares Intent-Version against the release the host runs: the lowest tag
# in the deployed-versions sidecars (as deploy.yml computes it), stellarindex-migrate
# excluded. Lowest because config from a release is unapplied if ANY binary predates it;
# migrate excluded because it legitimately lags and gates no config surface.
#
# THREE outcomes. An intent older than the deployed release is neither drift nor clean:
# reporting drift would send the operator to ch-schema-restore.md's "decide which side is
# wrong" and toward editing tier1_schema.sql to match a server it was never compared
# against. It is a refusal (exit 2, "could not check"), distinguishable in the metrics:
#
#   converged + clean     intent_converged 1, divergent 0
#   converged + drifted   intent_converged 1, divergent > 0
#   NOT CONVERGED         intent_converged 0, divergent ABSENT (nothing compared)
#
# A false refusal is the cheap direction: a release with no config change still moves the
# sidecars, costing one idempotent archival-node.yml run, whereas the opposite error is a
# 60M-ledger re-derive against an undeclared ORDER BY. scripts/ci/ansible-drift.baseline
# rules the same way (repo-ahead-of-host "IS drift"). The state is always clearable by the
# operator, so it cannot become a permanently-firing alert.

# version_core <string> — the leading vMAJOR.MINOR.PATCH, or empty. Applied to both sides.
# Bootstrap sidecars are `git describe` output (v0.47.2-3-g1a2b3c4-dirty); truncating
# understates the host's age, which errs toward "converged", the quiet direction for a tie-breaker.
version_core() {
  local v="$1"
  [[ "$v" =~ (v[0-9]+\.[0-9]+\.[0-9]+) ]] && printf '%s' "${BASH_REMATCH[1]}"
}

# version_lt <a> <b> — true when a orders strictly before b. `sort -V` reads to EOF, avoiding
# the `| head` shape lint-shell-sigpipe.sh refuses under pipefail.
version_lt() {
  [[ "$1" == "$2" ]] && return 1
  local ordered
  ordered="$(printf '%s\n%s\n' "$1" "$2" | sort -V)"
  [[ "${ordered%%$'\n'*}" == "$1" ]]
}

# emit_intent_metrics <verified> <converged-or-empty> — the .prom for a run that made NO
# comparison. Written on every refusal so node_exporter does not keep serving the last
# run's `divergent 0`.
emit_intent_metrics() {
  [[ "$TEXTFILE_DIR" == "/dev/null" ]] && return 0
  local verified="$1" converged="$2" out tmp
  mkdir -p "$TEXTFILE_DIR"
  out="$TEXTFILE_DIR/ch_schema_drift.prom"
  tmp="$out.tmp.$$"
  {
    echo "# HELP stellarindex_ch_schema_drift_last_run_unix Unix time of the most recent completed repo-vs-live ClickHouse schema comparison."
    echo "# TYPE stellarindex_ch_schema_drift_last_run_unix gauge"
    echo "stellarindex_ch_schema_drift_last_run_unix $(date +%s)"
    echo "# HELP stellarindex_ch_schema_drift_intent_verified 1 = the check established BOTH the shipped intent's release stamp and the release this host runs, so intent_converged is a real verdict. 0 = one side was unavailable and no convergence verdict exists."
    echo "# TYPE stellarindex_ch_schema_drift_intent_verified gauge"
    echo "stellarindex_ch_schema_drift_intent_verified $verified"
    if [[ -n "$converged" ]]; then
      echo "# HELP stellarindex_ch_schema_drift_intent_converged 1 = the shipped intent is from a release at least as new as the one this host runs. 0 = NOT CONVERGED: the intent predates the deployed release, so no comparison was made and the drift gauges are absent for that run."
      echo "# TYPE stellarindex_ch_schema_drift_intent_converged gauge"
      echo "stellarindex_ch_schema_drift_intent_converged $converged"
    fi
  } > "$tmp"
  chmod 644 "$tmp"
  mv "$tmp" "$out"
}

intent_release="$(sed -n 's/^-- *Intent-Version: *\([^ ]*\).*$/\1/p' "$INTENT")"
intent_release="${intent_release%%$'\n'*}"
intent_schema_commit="$(sed -n 's/^-- *Intent-Schema-Commit: *\(.*[^ ]\) *$/\1/p' "$INTENT")"
intent_schema_commit="${intent_schema_commit%%$'\n'*}"
intent_stamped=0
[[ -n "$intent_release" ]] && intent_stamped=1
intent_release="$(version_core "$intent_release")"

# Host release: lowest core version across the release-managed sidecars, migrate excluded.
# `awk 1` not `cat`: deploy-one-binary.yml writes them via `copy: content:` with no trailing
# newline, which once fed deploy.yml a mashed `v0.46.1v0.44.7v0.28.1` token.
host_release=""
if [[ -d "$DEPLOYED_VERSIONS_DIR" ]]; then
  host_cores=""
  for sidecar in "$DEPLOYED_VERSIONS_DIR"/stellarindex-*; do
    [[ -f "$sidecar" ]] || continue
    case "$sidecar" in */stellarindex-migrate) continue ;; esac
    sidecar_raw="$(awk 1 "$sidecar" 2>/dev/null)"
    sidecar_core="$(version_core "${sidecar_raw%%$'\n'*}")"
    [[ -n "$sidecar_core" ]] && host_cores+="$sidecar_core"$'\n'
  done
  if [[ -n "$host_cores" ]]; then
    host_release="$(printf '%s' "$host_cores" | sort -V)"
    host_release="${host_release%%$'\n'*}"
  fi
fi

intent_verified=0
intent_converged=""
if [[ -n "$intent_release" && -n "$host_release" ]]; then
  intent_verified=1
  intent_converged=1
  if version_lt "$intent_release" "$host_release"; then
    intent_converged=0
    note "NOT CONVERGED: the repo intent PREDATES the release this host is running."
    note "    intent   $INTENT"
    note "    stamped  $intent_release${intent_schema_commit:+ (DDL last changed $intent_schema_commit)}"
    note "    host     $host_release (lowest across $DEPLOYED_VERSIONS_DIR, migrate excluded)"
    note "Reported as neither drift nor clean on purpose: this is a PROVISIONING"
    note "gap, and a reference that is not the repo's current statement cannot"
    note "answer whether live matches the repo. It is the shape measured on"
    note "futurenet on 2026-09-09, where a two-week-old intent agreed with a"
    note "two-week-old live schema and this check read CLEAN."
    note "Re-apply the archival-node role, which re-ships the intent:"
    note "    cd configs/ansible && ansible-playbook -i inventory/<host>.yml \\"
    note "        playbooks/archival-node.yml --tags ch-schema-drift"
    note "Then re-run this check; the verdict it gives afterwards is trustworthy."
    note "See docs/operations/runbooks/ch-schema-restore.md."
    emit_intent_metrics 1 0
    exit 2
  fi
elif [[ "$intent_stamped" -eq 0 && "$INTENT" == "$INSTALLED_INTENT" ]]; then
  # An unstamped SHIPPED copy means the ship task did not converge (it sits beside this
  # script's install task), and passing it would leave a side door open. An intent the
  # OPERATOR named (a checkout's tier1_schema.sql) is never stamped by construction: that
  # run is UNVERIFIED, not refused, so the by-hand run in ch-schema-restore.md keeps working.
  note "NOT CONVERGED: the SHIPPED intent carries no provenance stamp."
  note "    intent   $INTENT"
  note "The archival-node role stamps every copy it ships with an"
  note "\"-- Intent-Version:\" line. An unstamped shipped copy was installed by a"
  note "role older than this check, so nothing can vouch that it is the repo's"
  note "current DDL — which is exactly how futurenet checked a two-week-old"
  note "intent against a two-week-old live schema and read CLEAN on 2026-09-09."
  note "Re-apply the archival-node role:"
  note "    cd configs/ansible && ansible-playbook -i inventory/<host>.yml \\"
  note "        playbooks/archival-node.yml --tags ch-schema-drift"
  note "See docs/operations/runbooks/ch-schema-restore.md."
  emit_intent_metrics 0 0
  exit 2
fi

if [[ "$intent_verified" -eq 0 ]]; then
  # UNVERIFIED: still compare (suppressing would lose real drift) but mark the verdict
  # unvouched in the log and metric so a clean result is never read as a converged clean.
  note "WARNING: cannot verify that the intent side is current; the verdict below"
  note "is UNVOUCHED (stellarindex_ch_schema_drift_intent_verified 0)."
  if [[ "$intent_stamped" -eq 0 ]]; then
    note "    no \"-- Intent-Version:\" stamp in $INTENT"
  elif [[ -z "$intent_release" ]]; then
    # The role ships `unknown` when the controller is not a git checkout: the fix is on the
    # controller, not the host.
    note "    the \"-- Intent-Version:\" stamp in $INTENT names no release"
    note "    (the controller could not resolve one when the role shipped it)"
  else
    note "    intent stamped $intent_release"
  fi
  if [[ -z "$host_release" ]]; then
    note "    no readable release-managed sidecar under $DEPLOYED_VERSIONS_DIR"
  else
    note "    host running $host_release"
  fi
fi

# WHICH LIVE SIDE: the default is a fresh sweep.
#
# Comparing against the newest DAILY snapshot fails two ways (measured on r1 and the test nets):
#   FALSE DRIFT. A deploy ships a new tier1_schema.sql mid-day against a morning capture, so
#   newly declared tables read ABSENT (8 on r1, one holding 20.9M rows) until the next
#   capture: red for no real reason, then green on its own, which teaches operators to wait.
#   HIDDEN DRIFT. It never reads the server, so with ClickHouse down it reports "0 divergent"
#   off an ageing capture: the "could not check" == "fine" equivalence ADR-0043 exists to break.
#
# The snapshot's rationale (one sweep per day serves backup and check) fails on cost: ~45
# SHOW CREATEs over loopback, sub-second. The check runs ON the node it queries (CH_HTTP is
# loopback, the unit is After=clickhouse-server.service), so unreachable is not a normal state.
#
# Snapshot mode is kept as an explicit choice for "did the repo match live on a past date?"
# (90 days of captures are retained).
#
# Precedence, most specific first:
#   1. LIVE=1 set explicitly       — a fresh sweep, said out loud.
#   2. LIVE_SCHEMA=<file>          — a capture the operator named.
#   3. LIVE=0 set explicitly       — the newest daily snapshot.
#   4. nothing set                 — a fresh sweep. THE DEFAULT.
if [[ "$live_explicit" == "1" ]]; then
  live_mode="live"
elif [[ -n "$LIVE_SCHEMA" ]]; then
  live_mode="file"
elif [[ "$live_explicit" == "0" ]]; then
  live_mode="snapshot"
else
  live_mode="live"
fi

live_file=""
live_origin=""
# Exported as stellarindex_ch_schema_drift_live: a 0-divergent verdict is only as current as the capture read.
live_is_fresh=0

case "$live_mode" in
live)
  tables="$(ch "SELECT name FROM system.tables
               WHERE database = '$CH_DATABASE' AND NOT is_temporary
               ORDER BY name FORMAT TabSeparated")" || {
    note "ClickHouse unreachable at $CH_HTTP — cannot compare against live."
    note "Fix ClickHouse first; this check is downstream of it. To compare"
    note "against the newest retained capture instead — accepting that it is"
    note "up to a day old and cannot see anything applied since — run:"
    note "    LIVE=0 ch-schema-drift.sh"
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
  live_is_fresh=1
  ;;
file)
  live_file="$LIVE_SCHEMA"
  live_origin="the capture $LIVE_SCHEMA"
  ;;
snapshot)
  # Newest snapshot day directory that actually holds a schema.sql.
  newest="$(find "$SNAPSHOT_DIR" -mindepth 2 -maxdepth 2 -name schema.sql 2>/dev/null | sort | tail -1)"
  if [[ -z "$newest" ]]; then
    note "no snapshot schema.sql under $SNAPSHOT_DIR — run ch-schema-snapshot.sh"
    note "first, or drop LIVE=0 to compare against live."
    exit 2
  fi
  # The capture's own stamp from the schema.sql header, preferred over mtime (a copy rewrites
  # mtime, not the header). Matched on the stamp shape so the header's em dash needs no locale.
  captured="$(sed -n 's/^-- ClickHouse schema snapshot .*\([0-9]\{8\}T[0-9]\{6\}Z\).*$/\1/p' "$newest")"
  captured="${captured%%$'\n'*}"
  live_file="$newest"
  live_origin="the snapshot $newest${captured:+ (captured $captured)}"

  # FALSE-DRIFT GUARD: a capture older than the intent cannot say whether a table declared
  # after it is live (measured: 8 false ABSENT on r1), so refuse with exit 2 rather than
  # report drift. Not a mute: a capture at least as new as the intent still reports real drift.
  if [[ "$INTENT" -nt "$live_file" ]]; then
    note "REFUSING to compare: the repo intent is NEWER than the capture."
    note "    intent   $INTENT"
    note "    capture  $live_origin"
    note "A capture written before the intent was installed cannot tell a"
    note "table that is genuinely missing live from one that was declared"
    note "after the capture was taken — every newly-declared table would"
    note "read as ABSENT from a live schema this run never looked at."
    note "Compare against live instead (ch-schema-drift.sh, or LIVE=1), or"
    note "take a fresh capture first (ch-schema-snapshot.sh)."
    note "See docs/operations/runbooks/ch-schema-restore.md."
    exit 2
  fi
  ;;
esac

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

for f in ${cutover_files[@]+"${cutover_files[@]}"}; do
  [[ -z "$f" ]] && continue
  [[ -r "$f" ]] || { note "cut-over DDL $f is not readable — cannot compare its objects"; exit 2; }
  parse_schema "$f" > "$work/cutover.facts" || { note "failed to parse $f"; exit 2; }
  while IFS= read -r obj; do
    obj="${obj##*.}"
    [[ -z "$obj" ]] && continue
    if ! grep -q "^$obj"$'\t'"kind"$'\t' "$work/cutover.facts"; then
      note "cut-over DDL $(basename "$f") marks $obj as si-cutover-object but does not CREATE it — cannot compare"
      exit 2
    fi
    awk -F'\t' -v t="$obj" '$1==t' "$work/cutover.facts" >> "$work/intent.facts"
    printf '%s\tcutover\t%s\n' "$obj" "$(basename "$f")" >> "$work/intent.facts"
  done < <(sed -n 's/^-- si-cutover-object: *//p' "$f" | tr -d '\r ')
done

fact() { awk -F'\t' -v t="$2" -v k="$3" '$1==t && $2==k {print $3; exit}' "$1"; }
tables_in() { awk -F'\t' '$2=="kind" {print $1}' "$1" | sort -u; }
# Named sub-objects of one table: <name>\t<compared text>.
subobjects_in() { awk -F'\t' -v t="$2" -v k="$3" '$1==t && $2==k {print $3"\t"$4}' "$1"; }
sub_kinds="index projection constraint"

declared="$(tables_in "$work/intent.facts")"
livetabs="$(tables_in "$work/live.facts")"

drift=0
compared=0
divergent_tables=0

while IFS= read -r t; do
  [[ -z "$t" ]] && continue
  label="$(basename "$INTENT")"
  cutover_src="$(fact "$work/intent.facts" "$t" cutover)"
  [[ -n "$cutover_src" ]] && label="$cutover_src"
  if ! grep -qx "$t" <<<"$livetabs"; then
    if [[ -n "$cutover_src" ]]; then
      note "CUTOVER $t: declared in $cutover_src, not live — no cut-over in progress"
      continue
    fi
    compared=$((compared + 1))
    note "DRIFT $t: declared in $label but ABSENT from $live_origin"
    drift=$((drift + 1)); divergent_tables=$((divergent_tables + 1))
    continue
  fi
  compared=$((compared + 1))
  bad=0
  keys="engine partition order columns"
  # AS-clone declarations resolve to their base's facts; an alias whose base is undeclared is drift.
  intent_src="$t"
  alias_base="$(fact "$work/intent.facts" "$t" alias)"
  if [[ -n "$alias_base" ]]; then
    if ! grep -qx "$alias_base" <<<"$declared"; then
      note "DRIFT $t: declared AS $alias_base, but $alias_base is not declared in $label"
      drift=$((drift + 1)); divergent_tables=$((divergent_tables + 1))
      continue
    fi
    intent_src="$alias_base"
  fi
  if [[ "$(fact "$work/intent.facts" "$t" kind)" == "view" ]]; then
    keys="to"
    if [[ "$(fact "$work/live.facts" "$t" kind)" != "view" ]]; then
      note "DRIFT $t: declared as a MATERIALIZED VIEW in the repo but live as a table"
      drift=$((drift + 1)); divergent_tables=$((divergent_tables + 1))
      continue
    fi
  fi
  # The live side can be alias-parsed too (e.g. a snapshot in the human-written form); resolve it the same way.
  live_src="$t"
  live_alias="$(fact "$work/live.facts" "$t" alias)"
  [[ -n "$live_alias" ]] && live_src="$live_alias"
  for key in $keys; do
    want="$(fact "$work/intent.facts" "$intent_src" "$key")"
    got="$(fact "$work/live.facts" "$live_src" "$key")"
    if [[ "$want" != "$got" ]]; then
      note "DRIFT $t.$key:"
      note "    repo: $want"
      note "    live: $got"
      drift=$((drift + 1)); bad=1
    fi
  done
  # ─── named sub-objects: indices, projections, constraints — BOTH ways ──
  # Index TYPE+params+GRANULARITY is exact declared text, compared textually; projection
  # SELECT and constraint CHECK are re-rendered by ClickHouse, so those compare by name.
  # Either direction is drift (declared but missing/different live, or live-only). The
  # uncodified accounting below counts TABLES, so an extra index on a declared table would never reach it.
  for skind in $sub_kinds; do
    sk="$(tr '[:lower:]' '[:upper:]' <<<"$skind")"
    while IFS=$'\t' read -r sname swant; do
      [[ -z "$sname" ]] && continue
      sgot="$(awk -F'\t' -v t="$live_src" -v k="$skind" -v n="$sname" \
        '$1==t && $2==k && $3==n {print $4; exit}' "$work/live.facts")"
      if [[ -z "$sgot" ]]; then
        note "DRIFT $t.$sk $sname: declared in $label but ABSENT from $live_origin"
        drift=$((drift + 1)); bad=1
      elif [[ "$swant" != "$sgot" ]]; then
        note "DRIFT $t.$sk $sname:"
        note "    repo: $swant"
        note "    live: $sgot"
        drift=$((drift + 1)); bad=1
      fi
    done < <(subobjects_in "$work/intent.facts" "$intent_src" "$skind")
    while IFS=$'\t' read -r sname _; do
      [[ -z "$sname" ]] && continue
      if ! grep -qx -- "$sname" <<<"$(subobjects_in "$work/intent.facts" "$intent_src" "$skind" | cut -f1)"; then
        note "DRIFT $t.$sk $sname: present in $live_origin but not declared in $label"
        drift=$((drift + 1)); bad=1
      fi
    done < <(subobjects_in "$work/live.facts" "$live_src" "$skind")
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
    note "UNCODIFIED $t: exists in $live_origin, absent from $(basename "$INTENT")"
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
    echo "# HELP stellarindex_ch_schema_drift_live 1 = the comparison read a fresh SHOW CREATE sweep off ClickHouse; 0 = it read a captured file (a daily snapshot, or an operator-named capture). A 0-divergent verdict is only as current as what it read."
    echo "# TYPE stellarindex_ch_schema_drift_live gauge"
    echo "stellarindex_ch_schema_drift_live $live_is_fresh"
    # intent_verified is this gate's own canary (cf. stellarindex_binary_version_probe_success):
    # 0 means the drift verdict beside it is UNVOUCHED. The not-converged path refuses above
    # and never emits these gauges beside a divergent count.
    echo "# HELP stellarindex_ch_schema_drift_intent_verified 1 = the check established BOTH the shipped intent's release stamp and the release this host runs, so intent_converged is a real verdict. 0 = one side was unavailable and no convergence verdict exists."
    echo "# TYPE stellarindex_ch_schema_drift_intent_verified gauge"
    echo "stellarindex_ch_schema_drift_intent_verified $intent_verified"
    if [[ -n "$intent_converged" ]]; then
      echo "# HELP stellarindex_ch_schema_drift_intent_info The provenance of the intent side actually compared, and the release this host is running."
      echo "# TYPE stellarindex_ch_schema_drift_intent_info gauge"
      echo "stellarindex_ch_schema_drift_intent_info{intent_version=\"$intent_release\",host_version=\"$host_release\"} 1"
      echo "# HELP stellarindex_ch_schema_drift_intent_converged 1 = the shipped intent is from a release at least as new as the one this host runs. 0 = NOT CONVERGED: the intent predates the deployed release, so no comparison was made and the drift gauges are absent for that run."
      echo "# TYPE stellarindex_ch_schema_drift_intent_converged gauge"
      echo "stellarindex_ch_schema_drift_intent_converged $intent_converged"
    fi
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
