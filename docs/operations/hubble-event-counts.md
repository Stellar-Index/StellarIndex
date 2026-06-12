---
title: Per-source recipe — Hubble event-count cross-check (Soroban)
last_verified: 2026-04-27
status: living procedure
---

# Hubble event-count cross-check for Soroban sources

The `stellarindex-ops hubble-soroban-events` subcommand emits per-
ledger counts of events from
`hubble-public.crypto_stellar.history_contract_events`. It's a
primitive — operators combine it with the per-source knowledge of
the (events ↔ trades) ratio to cross-check decoder coverage.

This doc captures the per-source filter recipe + the comparison
math. SDEX uses the dedicated `hubble-check` subcommand instead
(Hubble has a decoded `history_trades` view of classic SDEX);
Soroban DEXes / oracles do not.

## When to run

After any of:

- A WASM-history audit completes for a Soroban source and flips
  its `BackfillSafe` flag to `true`.
- A since-inception backfill batch lands for a Soroban source.
- A decoder change ships for a Soroban source.

The check is a regression gate: if the decoder dropped a topic
shape or got the event-vs-trade ratio wrong, the Hubble count and
our row count diverge.

## Per-source recipe

For each source, run the Hubble query, then the our-side query,
then compare with the documented multiplier.

### Soroswap

> 2 events per trade (`swap` + `sync`). Filter to topic[1]='swap'
> for one event per trade.

```sh
stellarindex-ops hubble-soroban-events \
  -from 51000000 -to 62250000 \
  -bigquery-project <BQ_PROJECT> \
  -contracts <pair-contract-A>,<pair-contract-B>,<pair-contract-C>... \
  -topic0 SoroswapPair \
  -topic1 swap \
  -output total
# → N events
```

```sql
-- Our side:
SELECT COUNT(*) FROM trades
 WHERE source = 'soroswap'
   AND ledger BETWEEN 51000000 AND 62250000;
-- → M trades
```

**Expected: N == M.** Each Soroswap pair contract emits one
`("SoroswapPair","swap")` event per trade; we record one trade
row per swap-sync correlation.

To enumerate pair contracts: query our trades hypertable for
distinct contract IDs we've ingested under `source='soroswap'`,
or walk factory `new_pair` events.

### Aquarius

> 1 event per trade. Filter to topic[0]='trade'.

```sh
stellarindex-ops hubble-soroban-events \
  -from 51000000 -to 62250000 \
  -bigquery-project <BQ_PROJECT> \
  -contracts <pool-A>,<pool-B>... \
  -topic0 trade \
  -output total
# → N events
```

Compare to `COUNT(*) WHERE source = 'aquarius'`. **Expected: N == M.**

### Phoenix

> 8 events per trade (one per field). Filter to topic[0]='swap'
> AND topic[1]='offer_amount' for one event per trade. Any one of
> the 8 field names works as a filter — `offer_amount` is the
> conventional pick because it's strictly numeric.

```sh
stellarindex-ops hubble-soroban-events \
  -from 51000000 -to 62250000 \
  -bigquery-project <BQ_PROJECT> \
  -contracts <pool-A>,<pool-B>... \
  -topic0 swap \
  -topic1 offer_amount \
  -output total
# → N events (= number of distinct swaps)
```

Compare to `COUNT(*) WHERE source = 'phoenix'`. **Expected: N == M.**

If you DON'T add a topic[1] filter you get 8N events back —
useful to confirm the 8-events-per-swap correlation is healthy.
If `total / 8 != trades`, some events are missing for some
swaps and the correlation is dropping incomplete RawSwaps.

### Comet

> 1 event per trade. Filter topic[0]='POOL', topic[1]='swap'.

```sh
stellarindex-ops hubble-soroban-events \
  -from 51000000 -to 62250000 \
  -bigquery-project <BQ_PROJECT> \
  -contracts <pool-A>,<pool-B>... \
  -topic0 POOL \
  -topic1 swap \
  -output total
# → N events
```

Compare to `COUNT(*) WHERE source = 'comet'`. **Expected: N == M.**

### Reflector (DEX / CEX / FX)

> 1 event fans out to N OracleUpdate rows in our DB (one per
> (asset, price) entry in the event's prices Vec).

```sh
# Per variant: substitute the contract from cfg.Oracle.Reflector.{DEX,CEX,FX}Contract.
stellarindex-ops hubble-soroban-events \
  -from 51000000 -to 62250000 \
  -bigquery-project <BQ_PROJECT> \
  -contracts <reflector-DEX-or-CEX-or-FX> \
  -topic0 REFLECTOR \
  -topic1 update \
  -output total
# → N events
```

```sql
-- Our side:
SELECT COUNT(*) FROM oracle_updates
 WHERE source = 'reflector-dex'  -- or -cex / -fx
   AND ledger BETWEEN 51000000 AND 62250000;
-- → M oracle updates
```

**Expected: N <= M (typically much less).** Each event fans out
to multiple rows; the ratio depends on how many assets are in
the event's prices vector. As a rough check: `M / N ~ N_assets`.

### Redstone

> 1 event per write_prices call → fans to N OracleUpdate rows
> (one per feed in the call's feed_ids op-arg).

```sh
stellarindex-ops hubble-soroban-events \
  -from 51000000 -to 62250000 \
  -bigquery-project <BQ_PROJECT> \
  -contracts <redstone-adapter> \
  -topic0 REDSTONE \
  -output total
# → N events
```

Compare to `COUNT(*) WHERE source = 'redstone'`. **Expected:
N <= M.** Same fanout pattern as Reflector.

### Band

**Not applicable.** Band's Soroban contract emits zero events
(CLAUDE.md "Band's Soroban contract emits zero events"). Decoder
operates on InvokeContract op args via the
`ContractCallDecoder` hook. There's no event count to cross-check
against. Per-WASM-hash decoder audit is the only safety net.

## When totals differ

Possible explanations, in rough order of likelihood:

1. **Contract-list incompleteness.** Did you miss a pair / pool
   contract? Re-enumerate from on-chain history and re-run.
2. **Topic-filter typo.** topic[0] / topic[1] strings need exact
   case-sensitive match. Most Soroban events use `ScvSymbol`
   identifiers (no spaces); Phoenix uses `ScvString` including
   the space-bearing `actual received amount` (Q2).
3. **Range edge effects.** Ledger boundaries can clip events
   across `from` / `to`. Re-run with a wider range.
4. **Decoder bug.** This is the case the cross-check exists to
   catch — decoder dropping events. Action: per-ledger drill-down
   with `-output json` or `-output csv` to find the affected
   range, then inspect the decoder against that range's WASM
   hash via `stellarindex-ops wasm-history`.

## Cost preview

Always dry-run before a full-range query:

```sh
stellarindex-ops hubble-soroban-events ... -dry-run-bytes
```

Typical: 20–40 GB scan per 1M-ledger range → ~$0.20 at $5/TB on-
demand. Reservation pricing essentially free at our scale.
