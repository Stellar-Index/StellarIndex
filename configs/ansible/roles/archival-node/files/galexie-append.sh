#!/bin/bash
#
# galexie append wrapper.
#
# Galexie's `append` subcommand requires --start <ledger>. On a
# fresh deploy there's no prior cursor, so we discover the current
# network tip from the local stellar-core HTTP API and start there.
#
# On a restart after galexie has already exported some ledgers,
# we'd ideally resume from "last-exported-ledger + 1" rather than
# re-query the tip. TODO: probe MinIO galexie-live for the latest
# object and derive the start from its filename. For Phase-1 we
# always start from current tip; duplicate writes are content-
# addressed so reprocessing is idempotent.

set -euo pipefail

CONF=/etc/galexie/galexie.toml
CORE_URL=http://127.0.0.1:11626/info

# Wait up to 90s for stellar-core to be queryable. It may be mid-
# catchup or still starting the HTTP server. Without this guard the
# wrapper fails immediately on boot if galexie starts before core.
for i in $(seq 1 90); do
  if ledger=$(curl -sfL "$CORE_URL" 2>/dev/null | jq -r '.info.ledger.num // empty'); then
    if [[ -n "$ledger" && "$ledger" -gt 1 ]]; then
      break
    fi
  fi
  sleep 1
done

if [[ -z "${ledger:-}" || "$ledger" -le 1 ]]; then
  echo "galexie-append.sh: stellar-core not reachable at $CORE_URL after 90s" >&2
  exit 1
fi

echo "galexie-append.sh: starting galexie append --start $ledger"
exec /usr/local/bin/galexie append --config-file "$CONF" --start "$ledger"
