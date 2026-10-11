#!/usr/bin/env bash
# Incremental completeness verify (ADR-0033 + ADR-0034).
#
# Re-verifies the certified ClickHouse lake against the served tier, but ONLY
# over [min(watermark), tip] (ledgers that arrived or regressed since the last
# run), so a run takes minutes, not the hours of a genesis→tip sweep. A clean
# source's watermark extends to tip; a regression pulls it back.
#
# READ-ONLY on served data: it recomputes completeness_snapshots (the verdict)
# and never rewrites trades/oracle_updates/etc. If a source regresses it EXITS
# NON-ZERO with the source + failing range so the systemd unit (and
# Healthchecks.io) surface it — the repair (ch-rebuild -write over that range)
# stays a deliberate, reviewed action, never automatic.
#
# Not installed on r1 (the deployed job is the daily compute-completeness
# timer from the archival-node role); run by hand. A full genesis→tip sweep
# (catches OLD data changing underneath us) is a separate manual run:
#   stellarindex-ops compute-completeness -config /etc/stellarindex.toml -ch -write
set -uo pipefail
# Read a systemd EnvironmentFile VERBATIM, never `.`/source: its values are
# unquoted, so the shell would expand `$`, split on `;`/`&`/`|`/whitespace and
# eat quotes in a secret (services keep working; this path gets a mangled DSN).
# Same reader as run-heavy-job.sh. usage: load_env_file FILE [export]
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
load_env_file /etc/default/stellarindex-ops export

PSQL=(psql "$STELLARINDEX_POSTGRES_DSN" -tAc)
OPS=/usr/local/bin/stellarindex-ops
CFG=/etc/stellarindex.toml

# from = lowest verified watermark across real sources (not the "recognition"
# pseudo-row). 0 (no snapshots yet) => full verify.
FROM=$("${PSQL[@]}" "SELECT COALESCE(min(watermark),0) FROM completeness_snapshots WHERE source <> 'recognition'")
FROM=${FROM:-0}
echo "$(date -u +%FT%TZ) completeness: incremental verify from=${FROM} (0=full)"

nice -n 15 ionice -c2 -n7 "$OPS" compute-completeness -config "$CFG" -ch -from "$FROM" -write
rc=$?
if [ "$rc" -ne 0 ]; then
	echo "$(date -u +%FT%TZ) completeness: verify command failed rc=$rc" >&2
	exit "$rc"
fi

# Surface any still-incomplete source (real gap, reconcile artifact or
# regression) with its failing range.
FAILED=$("${PSQL[@]}" "SELECT string_agg(source || ' (' || detail || ')', E'\n  ') FROM completeness_snapshots WHERE NOT complete AND source <> 'recognition'")
if [ -n "$FAILED" ]; then
	echo "$(date -u +%FT%TZ) completeness: INCOMPLETE sources — verify + (if a real gap) re-derive the failing range:" >&2
	echo "  $FAILED" >&2
	exit 1
fi

echo "$(date -u +%FT%TZ) completeness: all sources verified complete to tip"
