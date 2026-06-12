#!/usr/bin/env bash
# Incremental completeness verify (ADR-0033 + ADR-0034).
#
# Re-verifies the certified ClickHouse lake against the served tier, but ONLY
# over [min(watermark), tip] — the ledgers that arrived (or regressed) since the
# last run — so each run is minutes, not the hours a full genesis→tip sweep
# takes. The watermark for a source still extends to tip when its incremental
# window is clean; a regression pulls it back to the failing ledger.
#
# READ-ONLY on served data: it recomputes completeness_snapshots (the verdict)
# and never rewrites trades/oracle_updates/etc. If a source regresses it EXITS
# NON-ZERO with the source + failing range so the systemd unit (and
# Healthchecks.io) surface it — the repair (ch-rebuild -write over that range)
# stays a deliberate, reviewed action, never automatic.
#
# Installed as stellaratlas-completeness.service, fired hourly by
# stellaratlas-completeness.timer. A full genesis→tip sweep (catches the rarer
# case where OLD data changed underneath us) is a separate periodic/manual run:
#   stellaratlas-ops compute-completeness -config /etc/stellaratlas.toml -ch
set -uo pipefail
set -a
. /etc/default/stellaratlas-ops
set +a

PSQL=(psql "$STELLARATLAS_POSTGRES_DSN" -tAc)
OPS=/usr/local/bin/stellaratlas-ops
CFG=/etc/stellaratlas.toml

# from = lowest verified watermark across real sources (exclude the system
# "recognition" pseudo-row). 0 (no snapshots yet, first run) => full verify.
FROM=$("${PSQL[@]}" "SELECT COALESCE(min(watermark),0) FROM completeness_snapshots WHERE source <> 'recognition'")
FROM=${FROM:-0}
echo "$(date -u +%FT%TZ) completeness: incremental verify from=${FROM} (0=full)"

nice -n 15 ionice -c2 -n7 "$OPS" compute-completeness -config "$CFG" -ch -from "$FROM"
rc=$?
if [ "$rc" -ne 0 ]; then
	echo "$(date -u +%FT%TZ) completeness: verify command failed rc=$rc" >&2
	exit "$rc"
fi

# Surface any source still incomplete (a real gap, a reconcile artifact, or a
# regression) with its failing range for triage.
FAILED=$("${PSQL[@]}" "SELECT string_agg(source || ' (' || detail || ')', E'\n  ') FROM completeness_snapshots WHERE NOT complete AND source <> 'recognition'")
if [ -n "$FAILED" ]; then
	echo "$(date -u +%FT%TZ) completeness: INCOMPLETE sources — verify + (if a real gap) re-derive the failing range:" >&2
	echo "  $FAILED" >&2
	exit 1
fi

echo "$(date -u +%FT%TZ) completeness: all sources verified complete to tip"
