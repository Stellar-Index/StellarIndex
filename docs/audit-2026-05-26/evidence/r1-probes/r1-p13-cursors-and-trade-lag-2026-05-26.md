# R1-P13 — backfill / ingest cursor state + trade lag

## Subject

Confirm: live ingest cursor advancing; per-Stellar-source trade
freshness; soroban-events fill walk progress; verify-archive
state.

## When

`2026-05-26T21:50:00Z` (approx)

## Where

`root@136.243.90.96` (R1).

## Claim being tested

The R1-P01 probe surfaced sla-probe verdict-fail with
`/v1/price freshness=98453s (27h)`. This probe digs into
whether the live indexer is making progress, whether on-chain
sources are flowing, and whether the backfills are behaving.

## Command(s)

Two SQL files scp'd to r1 and run via psql -f:

- `price-freshness-audit.sql` — trades + caggs + cursors
- `cursor-shape.sql` — full ingestion_cursors table + max(ledger)

## Output (essential extracts)

```
=== latest trade per source (top 10) ===
  source  |            latest             |       lag
----------+-------------------------------+-----------------
 kraken   | 2026-05-26 21:48:21.920216+00 | 00:00:02
 bitstamp | 2026-05-26 21:48:19.734+00    | 00:00:05
 binance  | 2026-05-26 21:44:29.316+00    | 00:03:55
 coinbase | 2026-05-26 21:28:40.277123+00 | 00:19:44
 sdex     | 2026-05-26 14:59:00+00        | 06:49:24    ← 7h frozen
 aquarius | 2026-05-26 14:56:56+00        | 06:51:28
 soroswap | 2026-05-26 14:43:17+00        | 07:05:07
 comet    | 2026-05-26 14:42:48+00        | 07:05:36
 phoenix  | 2026-05-26 14:40:17+00        | 07:08:07

=== Continuous aggregates ===
prices_1m   latest_bucket 21:46  lag 02m26s    healthy
prices_15m  latest_bucket 21:30  lag 18m26s    healthy (bucket-boundary)
prices_1h   latest_bucket 20:00  lag 01h48m    healthy (1h-boundary)

=== ingest cursors ===
ledgerstream     last_ledger 62745863  (current)
backfill various 62642779.. (soroban-events fill in progress)

=== max ledger across Stellar sources in trades ===
62745864   ← current

=== ingestion_cursors schema ===
source       text NOT NULL
sub_source   text NOT NULL
last_ledger  integer NOT NULL
last_updated timestamptz NOT NULL DEFAULT now()
```

## Interpretation

Apparent contradiction:
- `ledgerstream` cursor at ledger 62,745,863 (current)
- `max(ledger) FROM trades WHERE source IN (sdex,...,phoenix)` = 62,745,864 (also current)
- BUT `max(ts) FROM trades GROUP BY source` shows Stellar sources stuck at 14:43-14:59 (~7h ago)

Possible explanations to disambiguate via next probe:
1. Live indexer IS reading ledgers and writing SOME trades from
   ledger 62,745,864, but maybe one external CEX trade with that
   ledger via some path
2. Stellar sources legitimately produced ZERO trades from 14:43
   onward (low-liquidity window — but 7h of dead silence across
   five DEXes is implausible)
3. Live ingest path is broken specifically for Stellar event
   decoding while ledgerstream cursor advances

The drill-down query `onchain-lag-detail.sql` answers which of
these is right by:
- counting trades-per-hour per source (look for the cliff)
- inspecting what's at ledger 62,745,864
- checking soroban_events for life signs

## Findings raised

- F-0016 (NEW) — Stellar on-chain ingest 7h frozen
- F-0017 (NEW, needs evidence) — CAGG refresh lag — may be
  invalid once cross-referenced against CAGG refresh schedule

## Disposition

`claim-contradicted` for the "live ingest healthy" assumption —
external CEX is flowing but Stellar on-chain may be partially
stalled. Drill-down probe pending.
