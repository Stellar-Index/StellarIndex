#!/usr/bin/env bash
# ledger-meta-decode-probe — detect "we are behind a protocol upgrade" fast.
#
# An old galexie cannot decode newer-protocol ledger meta (e.g.
# "decoding ParallelTxExecutionStage"). It fails closed (errors and stops, no
# corrupt objects) but surfaced only as generic tip-lag / archive-gap.
#
# The primary guard (stellar-stack-version-probe -> protocol_lag) is slow: it
# runs daily with `for: 6h` (~30h worst case) and compares against RELEASED
# versions, so it cannot know the network started emitting the new XDR (the
# breaking arm appears only once a ledger contains the new structure).
#
# This is the reactive backstop: it names an actual decode failure ("this
# component cannot decode current ledger meta, bump it").
#
# Fail-open by design: any probe error emits nothing rather than a false 0
# (the alert also has a staleness guard on _updated_seconds).

set -uo pipefail

# The override lets scripts/ci/ledger-meta-decode-probe-test.sh run these exact
# bytes (same seam as galexie-catchup-probe.sh); the service unit sets none.
TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
OUT="$TEXTFILE_DIR/ledger_meta_decode.prom"
TMP="$OUT.tmp.$$"
WINDOW="${DECODE_PROBE_WINDOW:-15min}"

[ -d "$TEXTFILE_DIR" ] || exit 0
trap 'rm -f "$TMP"' EXIT

# Units that decode ledger meta. galexie-backfill is transient and may not exist;
# journalctl tolerates unknown units.
UNITS=(galexie galexie-backfill stellarindex-indexer)

# Deliberately narrow: XDR/protocol decode failures only. A broad grep would
# page on every hiccup and get muted, which is how a real one gets missed.
PATTERN='decoding LedgerCloseMeta|decoding GeneralizedTransactionSet|decoding TransactionPhase|ParallelTxExecutionStage|unsupported ledger version|unknown union arm|xdr:.*unknown|decoding cached ledger meta'

total=0
COUNTS=()
for u in "${UNITS[@]}"; do
    out=$(journalctl -u "$u" --since "-${WINDOW}" --no-pager 2>&1)
    rc=$?
    if [ "$rc" -ne 0 ]; then
        # A real read failure, not "zero matches": emit nothing and keep the
        # previous textfile so the staleness guard pages instead of a fabricated 0.
        printf 'ledger-meta-decode-probe: journalctl failed for unit %s (rc=%d): %s\n' \
            "$u" "$rc" "$out" >&2
        exit 0
    fi
    n=$(grep -cE "$PATTERN" <<<"$out")
    COUNTS+=("$n")
    total=$(( total + n ))
done

{
    echo '# HELP stellarindex_ledger_meta_decode_failures Ledger-meta XDR decode failures observed in the probe window, per unit. NON-ZERO means a component cannot decode ledger meta the network is now producing — almost always "we are behind a protocol upgrade". Bump that component (see the runbook); the failure itself is fail-closed, so no corrupt data is written, but ingestion for that component is stopped.'
    echo '# TYPE stellarindex_ledger_meta_decode_failures gauge'
    for i in "${!UNITS[@]}"; do
        echo "stellarindex_ledger_meta_decode_failures{unit=\"${UNITS[$i]}\"} ${COUNTS[$i]}"
    done
    echo '# HELP stellarindex_ledger_meta_decode_failures_total Sum across units in the probe window.'
    echo '# TYPE stellarindex_ledger_meta_decode_failures_total gauge'
    echo "stellarindex_ledger_meta_decode_failures_total ${total}"
    echo '# HELP stellarindex_ledger_meta_decode_probe_updated_seconds Unix time of the most recent successful probe run (staleness guard: a silent probe must not read as "no failures").'
    echo '# TYPE stellarindex_ledger_meta_decode_probe_updated_seconds gauge'
    echo "stellarindex_ledger_meta_decode_probe_updated_seconds $(date +%s)"
} > "$TMP"

chmod 0644 "$TMP"
mv -f "$TMP" "$OUT"
