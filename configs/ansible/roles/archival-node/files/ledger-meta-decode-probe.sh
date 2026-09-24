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

# The override is the same seam galexie-catchup-probe.sh and
# timescale-jobs-probe.sh carry, and exists for the same reason: it lets
# scripts/ci/ledger-meta-decode-probe-test.sh execute these exact bytes
# rather than a hand-copied twin. The service unit sets no environment, so
# a real run always takes the default.
TEXTFILE_DIR="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
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
COUNTS=()
for u in "${UNITS[@]}"; do
    out=$(journalctl -u "$u" --since "-${WINDOW}" --no-pager 2>&1)
    rc=$?
    if [ "$rc" -ne 0 ]; then
        # journalctl itself failed (journald unreachable, permission denied,
        # missing binary, ...) -- a real read failure, not "zero matches".
        # Fail open per the header: emit nothing this cycle and leave the
        # previous textfile (and its _updated_seconds) in place, so the
        # staleness guard pages instead of a fabricated 0 masking a real
        # decode failure.
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
