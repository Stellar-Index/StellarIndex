#!/usr/bin/env bash
# d2-ordinal-reproject.sh — RETIRED. Refuses to run, whatever its arguments.
#
# It recomputed stellar.ledger_entry_changes.intra_ledger_seq in SQL, ranking
# each ledger's rows by (tx_index, change_index): the per-transaction walk that
# dispatcher.EntryWalkVersion 1 described. The writer has emitted the
# ledger-wide three-phase walk (EntryWalkVersion 2: every tx's fee changes,
# then every tx's apply phase, then every post-apply refund) since 2026-07-26,
# and the lake cannot reproduce that order in SQL — fee, before, after and
# refund changes all carry op_index -1, so a row does not say which phase it
# came from. Running this stamps version-1 positions over version-2 ones.
#
# Re-derive through the Go walk instead: scripts/ops/ordinal-rederive-chunks.sh
# (ch-backfill -> extractLedgerEntryChanges), following
# docs/operations/runbooks/entry-walk-renumbering.md. The file is kept, not
# deleted, so a copy re-installed from this repo overwrites the old one on a
# host with a refusal rather than leaving it runnable.
set -euo pipefail

cat >&2 <<'MSG'
d2-ordinal-reproject.sh is retired: it computes intra_ledger_seq in the
per-transaction walk order (EntryWalkVersion 1), not the ledger-wide
three-phase order the writer uses (EntryWalkVersion 2).
Use scripts/ops/ordinal-rederive-chunks.sh and
docs/operations/runbooks/entry-walk-renumbering.md instead.
MSG
exit 2
