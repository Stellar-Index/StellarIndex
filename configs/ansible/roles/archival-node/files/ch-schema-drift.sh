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
#   live    a fresh `SHOW CREATE` sweep straight off the server. That
#           is the DEFAULT since 2026-09-09. Before it, the default read
#           the newest ch-schema-snapshot capture — up to a day old, and
#           not the question the unit's Description asks. Both halves of
#           what that cost are written out at "which live side" below.
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
#   ch-schema-drift.sh                       # DEFAULT: fresh SHOW CREATE sweep
#   LIVE=1 ch-schema-drift.sh                # the same thing, said out loud
#   LIVE=0 ch-schema-drift.sh                # the newest daily snapshot instead
#   LIVE_SCHEMA=/path/schema.sql ch-schema-drift.sh   # an explicit capture
#
# The intent side resolves from $INTENT, else the copy the archival-node
# role ships to the host, else this checkout — see the block below the
# variable defaults. A bare `ch-schema-drift.sh` on a host therefore
# compares against the shipped copy; it does not need an environment.
#
# Exit code:
#   0  no drift
#   1  DRIFT — live differs from tier1_schema.sql on a compared attribute
#   2  cannot compare. ClickHouse unreachable; no intent to compare
#      against; no snapshot found under LIVE=0; or, under LIVE=0, a
#      capture older than the intent file, which cannot substantiate
#      "declared but absent live" for anything the intent added since.
#      NOT 0: "we could not check" must never read as "we checked and
#      it's fine", which is the failure ADR-0043 exists to prevent.
set -uo pipefail

# Lives in the role's files/ dir because that is where this repo keeps
# textfile-collector emitters and where scripts/ci/lint-metric-refs.sh
# looks for the producer of a stellarindex_* metric named in an alert
# rule (EMITTER_PATHS) — a .prom emitter outside those paths reads as a
# dead alert reference to that linter.
#
# INSTALLED_INTENT is where 18-pgbackrest-backup.yml's "Ship the repo's
# Tier-1 lake DDL as the drift check's intent side" puts the founding DDL
# on a host. It must stay equal to that task's dest, to the role's
# ch_schema_drift_intent default (defaults/main.yml) and to the unit
# template's fallback — ch-schema-drift-test.sh pins all four, because
# these four copies of one path drifting apart is the whole bug.
INSTALLED_INTENT="${INSTALLED_INTENT:-/usr/local/share/stellarindex/tier1_schema.sql}"

INTENT="${INTENT:-}"
SNAPSHOT_DIR="${SNAPSHOT_DIR:-/var/lib/stellarindex/ch-schema-snapshot}"
LIVE_SCHEMA="${LIVE_SCHEMA:-}"
# Unset and "0" mean different things now that live is the default, so
# this cannot collapse to ${LIVE:-1}: an operator who typed LIVE=0 asked
# for the snapshot, and an operator who typed nothing gets live.
live_explicit="${LIVE-}"
CH_HTTP="${CH_HTTP:-http://127.0.0.1:8123/}"
CH_DATABASE="${CH_DATABASE:-stellar}"
TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
# Where every ansible binary deploy records the tag it just installed —
# one file per binary, contents = the tag, mtime = the deploy
# (docs/operations/deployed-versions.md, "the live source of record").
# This is the SAME "what is this host running" mechanism deploy.yml's
# config-apply baseline and the binary-version-skew probe read; the
# convergence gate below reuses it rather than inventing a second one.
DEPLOYED_VERSIONS_DIR="${DEPLOYED_VERSIONS_DIR:-/var/lib/stellarindex/deployed-versions}"

note() { echo "ch-schema-drift: $*" >&2; }
ch() { curl -sSf --max-time 120 "$CH_HTTP" --data-binary "$1"; }

# ─── where the repo's intent comes from ─────────────────────────────
# Three candidates, tried in this order:
#
#   1. $INTENT — set by ch-schema-drift.service (Environment=INTENT=,
#      rendered from the role's ch_schema_drift_intent), or by an
#      operator aiming the check at one specific file. An explicit INTENT
#      that cannot be read is a HARD STOP below, never quietly replaced
#      by a fallback: a typo'd path must not silently compare against
#      something the operator did not name.
#   2. the copy the role SHIPS to the host. Neither r1 nor the test nets
#      have a checkout — that is why the role ships the DDL at all — but
#      until 2026-09-09 this script was the one link in that chain that
#      did not know the path, so the by-hand run ch-schema-restore.md
#      documents ("ch-schema-drift.sh", no environment) never found it.
#   3. this checkout: the role's files/ dir is five levels below the repo
#      root. Installed standalone at /usr/local/bin that arithmetic does
#      not fail, it CLAMPS — `cd /usr/local/bin/../../../../..` is `/` —
#      so INTENT became the literal `//deploy/clickhouse/tier1_schema.sql`
#      the 2026-09-09 testnet triage printed, a path that cannot exist. A
#      repo root is never `/`, so `/` means "not a checkout", and the
#      candidate is offered only when the file is genuinely there.
#
# When none resolves, REFUSE (exit 2) and name every path tried. "Could
# not check" must never read as "checked, and fine" — that equivalence is
# what ADR-0043 exists to break — but the refusal has to say enough that
# the operator can close it in one step instead of re-deriving this.
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
      # `CREATE TABLE x AS db.base;` clones the base table full definition with no
      # inline ENGINE/columns/ORDER BY of its own (the staging halves of
      # every truncate-fill-EXCHANGE cycle are declared this way). Emitting
      # empty engine/order/columns facts here made all six *_staging tables
      # read as 3 drifts each against the live fully-rendered DDL (2026-08-24,
      # the ch-schema-drift.service red). Emit an alias fact instead; the
      # comparer resolves it against the base declaration facts.
      if (kind == "table" && aliasbase != "" && engine == "" && cols == "") {
        printf "%s\talias\t%s\n", tbl, aliasbase
        tbl = ""; kind = ""; mvto = ""; engine = ""; partexpr = ""
        orderexpr = ""; cols = ""; incols = 0; depth = 0; aliasbase = ""
        return
      }
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
      orderexpr = ""; cols = ""; incols = 0; depth = 0; aliasbase = ""
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
      # AS-clone capture: `CREATE TABLE x AS stellar.base;` (no ENGINE, no
      # column list on the statement). ENGINE ... AS SELECT is a different
      # construct and is excluded by the ENGINE guard.
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
      # a same-line "(" opens the column list (tables only; the column
      # list of an MV is ClickHouse-inferred, deliberately not compared)
      if (kind == "table" && (line ~ /\($/ || line ~ /\( *[A-Za-z`]/)) { incols = 1; depth = 1 }
      next
    }
    tbl == "" { next }
    kind == "view" && mvto == "" { mvto = mvtarget(" " line) }
    # ─ AS-clone on a continuation line ─
    # tier1_schema.sql writes the clone as two lines:
    #   CREATE TABLE IF NOT EXISTS stellar.x_staging
    #   AS stellar.x;
    # so the statement-start capture above never sees it (2026-08-24: the
    # first fix only matched a same-line AS and the live check stayed red
    # while the symmetric intent-vs-intent self-test passed — the exact
    # same-shape blindness this harness header warns about).
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

# ─── is the INTENT side itself current? (2026-09-09) ────────────────
# THE BLIND SPOT this closes, measured on both test nets that morning by
# extracting the `transactions` DDL from each host's OWN shipped intent
# file and diffing it against that host's live schema:
#
#   testnet    intent shipped 09-08 = POST-#482   live = PRE-#482  -> DRIFT (correct)
#   futurenet  intent shipped 08-26 = PRE-#482    live = PRE-#482  -> CLEAN (false)
#
# Both hosts carry the SAME stale live schema (transactions.ingested_at
# last, where the repo has put it after `memo` since #482 merged
# 2026-09-02). testnet reported it. futurenet read CLEAN — not because
# anything was right, but because its intent file was two weeks stale as
# well. Two wrongs reading as a right.
#
# The structural cause: the intent side ARRIVES BY THE SAME CONVERGENCE
# THE CHECK IS SUPPOSED TO POLICE. /usr/local/share/stellarindex/
# tier1_schema.sql is shipped by the archival-node role, so a host that
# has not had the role applied compares last fortnight's repo against
# today's server and calls the agreement clean. The check goes green
# EXACTLY WHEN A HOST IS FURTHEST BEHIND, which is the one shape a
# control must never have. Reading live by default (the 2026-09-09 fix
# above) does not touch this: it corrected the LIVE side.
#
# So the intent side now carries provenance, stamped at ship time by the
# role's "Ship the repo's Tier-1 lake DDL" task:
#
#   -- Intent-Version: v0.67.0                the release the shipped copy came from
#   -- Intent-Schema-Commit: <sha> (<date>)   when the DDL content last changed
#
# and the check compares Intent-Version against WHAT THIS HOST IS
# RUNNING. That second half is deliberately not a new mechanism: it is
# the deployed-versions sidecar, the same "live source of record" that
# deploy.yml's config-apply baseline reads and that
# docs/operations/deployed-versions.md names — lowest tag across the
# release-managed binaries, stellarindex-migrate excluded, exactly as
# deploy.yml computes it (#427). Lowest, because config from a release
# is unapplied if ANY binary predates it; migrate excluded because it
# legitimately lags and gates no config surface.
#
# THREE OUTCOMES, not two. When the intent predates the deployed release
# the check does NOT report drift, and does not pass. Reporting drift
# would blame the schema for a provisioning gap and send the operator to
# ch-schema-restore.md's "decide which side is wrong" procedure — the
# wrong runbook, and the one that ends in someone editing tier1_schema.sql
# to match a server it was never compared against. It is a REFUSAL:
# exit 2, this script's existing "could not check", because a reference
# that is not the repo's current statement cannot answer "is live what
# the repository says it should be". The three states are distinguishable
# in the metric surface, never by exit code alone:
#
#   converged + clean     intent_converged 1, divergent 0
#   converged + drifted   intent_converged 1, divergent > 0
#   NOT CONVERGED         intent_converged 0, divergent ABSENT (nothing compared)
#
# Why a false refusal is the cheap direction. A release that changes no
# config surface still moves the sidecars, so a host can read NOT
# CONVERGED while its DDL is in fact current. That costs one idempotent
# `ansible-playbook … archival-node.yml` run. The opposite error costs
# what futurenet already had: a 60M-ledger re-derive against an ORDER BY
# the repo does not declare, which does not error — it silently writes a
# mis-sorted table. This is also the position the tree already takes:
# scripts/ci/ansible-drift.baseline names this very task among the seven
# changed-with-no-entry tasks and rules that repo-ahead-of-host "IS
# drift — the fix is to apply the playbook", not an allowance. And
# unlike the absent-binary count binary-version-skew.yml rejects, this
# state is always CLEARABLE by an action the operator controls, so it
# cannot become the permanently-firing alert that is the same as no alert.

# version_core <string> — the leading vMAJOR.MINOR.PATCH, or empty.
# BOTH sides are truncated the same way, so the comparison is symmetric.
# The bootstrap path (14-stellarindex-services.yml) writes sidecars as
# `git describe --tags --always --dirty`, e.g. v0.47.2-3-g1a2b3c4-dirty;
# truncating that to v0.47.2 understates how new the host is, which errs
# toward "converged" — the quiet direction for a value that is only ever
# a tie-breaker.
version_core() {
  local v="$1"
  [[ "$v" =~ (v[0-9]+\.[0-9]+\.[0-9]+) ]] && printf '%s' "${BASH_REMATCH[1]}"
}

# version_lt <a> <b> — true when a orders strictly before b.
# `sort -V` reads to EOF, so this is not the `| head` shape
# scripts/ci/lint-shell-sigpipe.sh refuses under pipefail.
version_lt() {
  [[ "$1" == "$2" ]] && return 1
  local ordered
  ordered="$(printf '%s\n%s\n' "$1" "$2" | sort -V)"
  [[ "${ordered%%$'\n'*}" == "$1" ]]
}

# emit_intent_metrics <verified> <converged-or-empty> — the .prom for a
# run that made NO comparison. Written on every refusal below, because a
# refusal that wrote nothing would leave node_exporter serving the LAST
# run's `divergent 0` forever, which is the same false clean by a slower
# route.
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

# The host's release: lowest core version across the release-managed
# sidecars, migrate excluded. `awk 1` rather than `cat` because
# tasks/deploy-one-binary.yml writes these with `copy: content:`, which
# emits NO trailing newline — the bug that fed deploy.yml a mashed
# `v0.46.1v0.44.7v0.28.1` token and failed every deploy closed.
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
  # An intent the role has NOT stamped is itself a provisioning gap when
  # it is the SHIPPED copy: the task that installs this script and the
  # task that ships the DDL are adjacent in the same role, so a stamped
  # script beside an unstamped shipped intent means the ship did not
  # converge. Refusing here closes the side door the warning below would
  # otherwise leave open. An intent the OPERATOR named — a checkout's
  # deploy/clickhouse/tier1_schema.sql, a copy pulled down for a by-hand
  # comparison — is not shipped by anything and carries no stamp by
  # construction; that run is UNVERIFIED, not refused, because refusing
  # it would break the by-hand invocation ch-schema-restore.md documents.
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
  # UNVERIFIED: the comparison still runs — it is the best answer
  # available and suppressing it would lose real drift — but the verdict
  # is explicitly unvouched, in the log and in the metric, so a clean
  # result here can never be mistaken for a converged clean.
  note "WARNING: cannot verify that the intent side is current; the verdict below"
  note "is UNVOUCHED (stellarindex_ch_schema_drift_intent_verified 0)."
  if [[ "$intent_stamped" -eq 0 ]]; then
    note "    no \"-- Intent-Version:\" stamp in $INTENT"
  elif [[ -z "$intent_release" ]]; then
    # The role ships `unknown` when the controller is not a git checkout.
    # Distinct from "no stamp": the ship task DID run, it just could not
    # resolve a release, so the fix is on the controller, not the host.
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

# WHICH LIVE SIDE, and why the default is a fresh sweep (2026-09-09).
#
# Until this change the default compared the intent against the newest
# DAILY SNAPSHOT while the unit called itself "repo intent vs live".
# Both halves of that gap were measured, on r1 and on the test nets:
#
#   FALSE DRIFT. A deploy ships a new tier1_schema.sql at any hour of the
#   day; the capture it was compared against had been taken at 05:41. Every
#   table the new intent declares is missing from a file written before it
#   existed — so the check reported 8 tables "ABSENT from the live schema"
#   that were all live, one of them holding 20.9M rows, and it would have
#   cleared itself at the next morning's capture. Red for a reason that is
#   not real and then green on its own is the worst shape a control has:
#   it teaches the operator to wait it out.
#
#   HIDDEN DRIFT, the worse half. A comparison that never reads the server
#   does not measure live-vs-intent at all. And when ClickHouse is DOWN the
#   snapshot mode does not fail — it reports "0 divergent" off a capture
#   that ages silently, which is exactly the "we could not check" ==
#   "we checked and it is fine" equivalence ADR-0043 and this script's
#   exit-2 contract exist to break.
#
# The stated reason for reading the snapshot ("one SHOW CREATE sweep per
# day serves both the backup and the check") does not survive measurement:
# the sweep is ~45 SHOW CREATEs over loopback HTTP — the same work
# ch-schema-snapshot.sh already does daily — and this whole comparison is
# sub-second. The check also runs ON the node it queries (CH_HTTP defaults
# to loopback; the unit is After=clickhouse-server.service), so
# "ClickHouse is not reachable from here" is not a normal state for it.
#
# The snapshot mode is KEPT, as an explicit choice rather than the default:
# a retained capture is the only way to ask "did the repo match live on
# 2026-08-01?", and 90 days of them are on the box for exactly that.
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
# Exported as stellarindex_ch_schema_drift_live: a 0-divergent verdict
# measured against a capture is only as current as the capture, and
# nothing else in the metric set says which one was read.
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
  # The capture's OWN stamp, which ch-schema-snapshot.sh writes into the
  # schema.sql header. Preferred over the file's mtime in the message
  # because copying a snapshot rewrites the mtime and not the header.
  # Matched on the stamp shape rather than the surrounding prose so the
  # em dash on that header line never has to survive a locale.
  captured="$(sed -n 's/^-- ClickHouse schema snapshot .*\([0-9]\{8\}T[0-9]\{6\}Z\).*$/\1/p' "$newest")"
  captured="${captured%%$'\n'*}"
  live_file="$newest"
  live_origin="the snapshot $newest${captured:+ (captured $captured)}"

  # THE FALSE-DRIFT GUARD. A capture taken BEFORE the intent file was
  # installed cannot answer "is this declared table live?" — anything the
  # intent declares after the capture was written is absent from it by
  # construction. Reporting that as drift is what put 8 live tables on
  # r1's ABSENT list on 2026-09-09. Refuse instead: exit 2 is this
  # script's "could not check", and could-not-check is precisely the
  # state. It is not a mute — when the capture is at least as new as the
  # intent this mode still reports real drift, unchanged.
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
    note "DRIFT $t: declared in $(basename "$INTENT") but ABSENT from $live_origin"
    drift=$((drift + 1)); divergent_tables=$((divergent_tables + 1))
    continue
  fi
  bad=0
  keys="engine partition order columns"
  # AS-clone declarations resolve to their base's facts (see the parser's
  # alias note). An alias whose base is undeclared is itself drift.
  intent_src="$t"
  alias_base="$(fact "$work/intent.facts" "$t" alias)"
  if [[ -n "$alias_base" ]]; then
    if ! grep -qx "$alias_base" <<<"$declared"; then
      note "DRIFT $t: declared AS $alias_base, but $alias_base is not declared in $(basename "$INTENT")"
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
  # The live side can be alias-parsed too (a snapshot taken from a file in
  # the human-written form, incl. the harness's intent-vs-intent case) —
  # resolve it the same way.
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
    # The convergence half. intent_verified is this gate's own canary
    # (same idea as stellarindex_binary_version_probe_success): a 0 says
    # the drift verdict beside it is UNVOUCHED because the intent's
    # provenance or the host's release could not be read. A run that
    # reaches here is by construction converged-or-unverified — the
    # not-converged path refuses above and never emits these gauges
    # alongside a divergent count.
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
