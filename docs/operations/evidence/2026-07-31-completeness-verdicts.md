---
title: Completeness verdicts — 16/17 complete; redstone at 99.9995% with 15 provably-ambiguous ledgers
captured: 2026-07-31 ~05:10Z (post-v0.21.7 redstone replay, order-preserving attribution)
command: stellarindex-ops compute-completeness -config /etc/stellarindex.toml -ch -source redstone (+ all-source verdict table)
verdict: 🔵 16/17 complete=t; redstone lake_complete=t, coverage=1.0000, 15 blind ledgers (from 1,626 three days ago)
---

# Completeness verdicts

All-source table (latest snapshots): **aquarius, band, blend, cctp,
comet, defindex, phoenix, reflector-cex, reflector-dex, reflector-fx,
rozo, sdex, sep41_supply, sep41_transfers, soroswap, soroswap-router
= complete. redstone = incomplete.**

## The redstone residue, honestly

The blind-event ladder this week: **1,626 → 170 → 15** (empty-batch
recognition + payload-median attribution + the order-preserving
subsequence alignment). The surviving 15 ledgers (first 62,056,824) are
a PROVABLY ambiguous class: a single surviving price whose value matches
TWO candidate feeds' signer medians — with one price the order
constraint adds nothing, and any attribution would be a guess. The
verifier keeps them blind BY DESIGN (honest-blind beats misattributed).
Redstone's substrate is verified contiguous + hash-chained from its
genesis; coverage = 1.0000; the served projection is exact everywhere
except these 15 single-event ledgers.

## The designed closure (queued, not hacked)

The adapter's write_prices STORES each accepted feed's price — the
transaction's ledger-entry writes name exactly the accepted feed set.
Plumbing state-write keys through the dispatcher into events.Event
(the same pattern OpArgs used, PR 166) gives the decoder exact
attribution with no heuristics, closing this class permanently and
benefiting any future storage-writing source. Queued as the next
engineering unit; until it lands, /v1/coverage honestly reports the
two-axis verdict (lake_complete=t, complete=f) for redstone.
