#!/usr/bin/env bash
# F-1205 follow-up (codex audit-2026-05-12): compute ARCHIVE_TO
# for archive-completeness.service from the indexer cursor.
# Uses the same DSN as the application binaries (sourced from
# /etc/default/stellaratlas) rather than peer-auth which fails
# under systemd's restricted user-switch context.
set -euo pipefail
. /etc/default/stellaratlas
TO=$(psql "$STELLARATLAS_POSTGRES_DSN" -tA -c 'SELECT GREATEST(MAX(last_ledger) - 64, 2) FROM ingestion_cursors WHERE last_ledger > 0' 2>/dev/null | tr -d '[:space:]')
if [ -z "$TO" ] || [ "$TO" = "0" ]; then
  echo "compute-archive-to: indexer cursor not yet advanced; bailing" >&2
  exit 1
fi
echo "ARCHIVE_TO=$TO" > /run/archive-completeness.env
