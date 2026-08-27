#!/usr/bin/env bash
# ledger-meta-decode-probe — detect "we are behind a protocol upgrade" FAST.
#
# WHY THIS EXISTS (2026-08-27):
# galexie v27 could not decode Protocol-28 ledger meta. The futurenet archive
# backfill died at ledger 92747 with:
#
#   decoding LedgerCloseMetaV2: decoding GeneralizedTransactionSet:
#   decoding TransactionSetV1: decoding TransactionPhase:
#   decoding ParallelTxsComponent: decoding ParallelTxExecutionStage
#
# The failure was HANDLED SAFELY — galexie errors and stops rather than writing
# corrupt objects (fail-closed; the archive kept every valid ledger). But it was
# NOT diagnosable: it surfaced only as generic tip-lag / archive-gap, and the
# actual cause had to be dug out of journald by hand.
#
# The proactive guard (stellar-stack-version-probe -> protocol_lag, severity
# page) is the primary defence and SHOULD catch this first. Its blind spots:
#   * it runs DAILY and the alert is `for: 6h`, so ~30h worst case to page;
#   * it compares against RELEASED upstream versions, so it cannot know the
#     network actually started emitting the new XDR (futurenet/testnet lead
#     mainnet, and the breaking arm only appears once a ledger genuinely
#     CONTAINS the new structure — testnet ingested 240 P28 ledgers on a v27
#     galexie with no error at all before anything broke).
#
# So this probe is the REACTIVE backstop: it watches for an actual decode
# failure and names the cause, turning "ingestion mysteriously stalled" into
# "this component cannot decode current ledger meta — bump it".
#
# Fail-open by design: any probe error emits nothing rather than a false 0,
# so a broken probe cannot mask a real decode failure (the alert also has a
# staleness guard on _updated_seconds).

set -uo pipefail

TEXTFILE_DIR=/var/lib/node_exporter/textfile_collector
OUT="$TEXTFILE_DIR/ledger_meta_decode.prom"
TMP="$OUT.tmp.$$"
WINDOW="${DECODE_PROBE_WINDOW:-15min}"

[ -d "$TEXTFILE_DIR" ] || exit 0
trap 'rm -f "$TMP"' EXIT

# Units that decode ledger meta. galexie-backfill is a transient unit and may
# not exist; journalctl tolerates unknown units with --unit repeated.
UNITS=(galexie galexie-backfill stellarindex-indexer)

# The signature set. Deliberately narrow: these are XDR/protocol decode
# failures, NOT generic errors — a broad grep here would page on every
# transient hiccup and get muted, which is how a real one gets missed.
PATTERN='decoding LedgerCloseMeta|decoding GeneralizedTransactionSet|decoding TransactionPhase|ParallelTxExecutionStage|unsupported ledger version|unknown union arm|xdr:.*unknown|decoding cached ledger meta'

total=0
declare -A per_unit
for u in "${UNITS[@]}"; do
    n=$(journalctl -u "$u" --since "-${WINDOW}" --no-pager 2>/dev/null \
        | grep -cE "$PATTERN" 2>/dev/null || true)
    n=${n:-0}
    per_unit["$u"]=$n
    total=$(( total + n ))
done

{
    echo '# HELP stellarindex_ledger_meta_decode_failures Ledger-meta XDR decode failures observed in the probe window, per unit. NON-ZERO means a component cannot decode ledger meta the network is now producing — almost always "we are behind a protocol upgrade". Bump that component (see the runbook); the failure itself is fail-closed, so no corrupt data is written, but ingestion for that component is stopped.'
    echo '# TYPE stellarindex_ledger_meta_decode_failures gauge'
    for u in "${UNITS[@]}"; do
        echo "stellarindex_ledger_meta_decode_failures{unit=\"$u\"} ${per_unit[$u]}"
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
