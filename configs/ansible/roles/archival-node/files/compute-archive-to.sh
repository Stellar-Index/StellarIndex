#!/usr/bin/env bash
# F-1205 follow-up (codex audit-2026-05-12): compute ARCHIVE_TO
# for archive-completeness.service from the indexer cursor.
# Uses the same DSN as the application binaries (sourced from
# /etc/default/stellarindex) rather than peer-auth which fails
# under systemd's restricted user-switch context.
set -euo pipefail
# Read a systemd EnvironmentFile VERBATIM — never `.`/source it. Its
# values are unquoted (that is what systemd wants), so the shell would
# expand `$`, split on `;`/`&`/`|`/whitespace and eat quotes inside a
# secret: the services keep working while this path gets a mangled DSN
# (deploy-ansible-secrets-5). Same reader as run-heavy-job.sh.
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
TO=$(psql "$STELLARINDEX_POSTGRES_DSN" -tA -c 'SELECT GREATEST(MAX(last_ledger) - 64, 2) FROM ingestion_cursors WHERE last_ledger > 0' 2>/dev/null | tr -d '[:space:]')
if [ -z "$TO" ] || [ "$TO" = "0" ]; then
  echo "compute-archive-to: indexer cursor not yet advanced; bailing" >&2
  exit 1
fi
echo "ARCHIVE_TO=$TO" > /run/archive-completeness.env
