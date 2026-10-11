#!/usr/bin/env bash
# Daily completeness-verdict refresh (compute-completeness.service, ADR-0033).
#
# ONE `stellarindex-ops compute-completeness -ch -pass` call proves recognition +
# substrate once at full range for the whole catalogue, then reconciles each
# source's projection from its own watermark. Why one pass, not a per-source,
# per-chunk loop:
#   1. Every invocation re-ran the global DistinctTopicShapes recognition scan
#      (~60s, independent of -source/-from); a source pinned far below tip
#      repeated it hundreds of times and blew the timeout, leaving verdicts stale.
#   2. Chunking existed for SDEX reconcile memory in ClickHouse (25k OK, 68k OOM);
#      reDeriveSDEXCensusViaDecoder now windows the join at 25k itself.
#   3. Driving off completeness_snapshots never reached never-seeded sources
#      (blend_emitter/blend_backstop/sorocredit); -pass iterates the catalogue.
#   4. Substrate is proved at full tip, so the write is never blocked by the
#      monotonic-tip guard. The tool still refuses to regress an advanced verdict
#      or upgrade a failing one without a full re-verify.
#
# DSN sourcing matches compute-archive-to.sh (peer-auth fails under systemd's
# restricted user-switch context).
set -euo pipefail
# Read a systemd EnvironmentFile VERBATIM, never source it: its values are
# unquoted, so the shell would expand `$`, split on `;&|` and eat quotes inside a
# secret, yielding a mangled DSN. Same reader as run-heavy-job.sh.
# usage: load_env_file FILE [export]
load_env_file() {
  local line
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      [A-Za-z_]*=*)
        if [ "${2:-}" = export ]; then
          export "${line?}"
        else
          printf -v "${line%%=*}" '%s' "${line#*=}"
        fi
        ;;
    esac
  done < "$1"
}
load_env_file /etc/default/stellarindex

# Debian's pg_wrapper `psql` aborts ("Invalid data directory for cluster")
# for users that cannot read the data dir, as User=stellarindex cannot; call the
# versioned binary directly.
PSQL="/usr/lib/postgresql/${PG_VERSION:-15}/bin/psql"

DSN="$STELLARINDEX_POSTGRES_DSN"
CH_ADDR="${CH_ADDR:-127.0.0.1:9300}"
CONFIG="${CONFIG_PATH:-/etc/stellarindex.toml}"
OPS=/usr/local/bin/stellarindex-ops

TIP=$("$PSQL" "$DSN" -tA -c \
  "SELECT last_ledger FROM ingestion_cursors WHERE source='ledgerstream'" 2>/dev/null | tr -d '[:space:]')
if [ -z "$TIP" ] || [ "$TIP" = "0" ]; then
  echo "compute-completeness: ledgerstream tip unresolved; bailing" >&2
  exit 1
fi
# Reconcile a margin BELOW the live cursor: the served-tier drain trails the lake
# by seconds-minutes, and reconciling up to the cursor reads that lag as
# "expected>0 served=0", a false red.
TIP=$(( TIP - 100 ))

# -pass runs sources serially, soroswap-router and sep41 last, so a too-small
# budget starves the same tail nightly. PASS_TIMEOUT (EnvironmentFile) overrides;
# keep it below the unit's 180 min budget (TimeoutStartSec=54000 = that + 43200 s lock wait).
PASS_TIMEOUT="${PASS_TIMEOUT:-170m}"

# Arguments replace `-pass -timeout $PASS_TIMEOUT`: the -sdex and -sep41 units pass
# `-source <name> -timeout 360m` for weekly full re-proofs, and still need the tip margin.
if [ "$#" -gt 0 ]; then
  MODE=("$@")
else
  MODE=(-pass -timeout "$PASS_TIMEOUT")
fi

echo "compute-completeness: ${MODE[*]} refresh to tip=$TIP"
if ! "$OPS" compute-completeness -config "$CONFIG" -ch -ch-addr "$CH_ADDR" \
     -to "$TIP" -write "${MODE[@]}" </dev/null; then
  echo "compute-completeness: ${MODE[*]} FAILED (tip=$TIP)" >&2
  exit 1
fi
echo "compute-completeness: ${MODE[*]} complete (tip=$TIP)"
