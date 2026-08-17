#!/usr/bin/env bash
# ADR-0033 completeness-verdict refresh — whole-pass driver.
#
# compute-completeness.service runs this on a daily timer. It makes ONE
# `stellarindex-ops compute-completeness -ch -pass` invocation, which proves
# recognition + substrate ONCE at full range for the entire source catalogue and
# then reconciles EACH source's projection incrementally from its own watermark,
# advancing every source's verdict in a single process.
#
# Why one -pass call, NOT the old per-source, per-chunk loop:
#   1. The old loop re-invoked the tool for every source AND every 25k chunk,
#      and EACH invocation re-ran the load-heaviest step — the global
#      DistinctTopicShapes recognition scan (~60s over full history), identical
#      regardless of -source/-from. A source pinned far below tip (e.g. aquarius,
#      recognition-capped near its genesis) walked hundreds of chunks, so that
#      one 60s scan ran hundreds of times per night — the 3h52m timeout that
#      left the alphabetical tail's verdicts days stale. -pass runs it ONCE.
#   2. The old loop's whole reason to chunk was the SDEX projection reconcile's
#      ClickHouse memory (25k OK, 68k OOMs). That is now bounded INSIDE the tool:
#      reDeriveSDEXCensusViaDecoder windows the CH join at 25k regardless of the
#      requested range, so a single -pass call is memory-safe.
#   3. The old loop drove off `SELECT DISTINCT source FROM completeness_snapshots`,
#      so a never-seeded source (blend_emitter/blend_backstop/sorocredit) never
#      appeared and never got a verdict. -pass iterates the CATALOGUE, so every
#      source is computed — a never-seeded one reconciles from genesis on its
#      first pass.
#   4. -pass proves substrate at FULL tip, so its write advances the tip and is
#      never blocked by the CS-083 write guard (the low-tip substrate flap a
#      chunked run could never clear). The tool still refuses to REGRESS an
#      advanced verdict (CS-083 monotonic-tip guard) and never upgrades a failing
#      verdict without a full re-verify (INV-5).
#
# Same DSN sourcing as compute-archive-to.sh (peer-auth fails under systemd's
# restricted user-switch context).
set -euo pipefail
. /etc/default/stellarindex

# Debian's pg_wrapper `psql` stats the cluster data dir to pick a version and
# aborts with "Invalid data directory for cluster 15 main" for any user that
# cannot read it — which User=stellarindex (2026-07-03 non-root hardening)
# cannot. Call the versioned binary directly to bypass the wrapper.
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
# Reconcile a safety margin BELOW the live cursor: the served-tier drain
# legitimately trails the lake by seconds-minutes under load, and a
# per-ledger reconcile right up to the cursor reads that lag as
# "expected>0 served=0" — a false red (seen 2026-07-06: sdex "205
# mismatched ledgers" that were simply not drained yet).
TIP=$(( TIP - 100 ))

echo "compute-completeness: whole-pass refresh to tip=$TIP"
if ! "$OPS" compute-completeness -config "$CONFIG" -ch -ch-addr "$CH_ADDR" \
     -pass -to "$TIP" </dev/null; then
  echo "compute-completeness: pass FAILED (tip=$TIP)" >&2
  exit 1
fi
echo "compute-completeness: pass complete (tip=$TIP)"
