---
title: Per-source recipe — Hubble event-count cross-check (Soroban)
last_verified: 2026-04-27
status: living procedure
---

# Hubble event-count cross-check for Soroban sources

`stellarindex-ops hubble-soroban-events` emits per-ledger counts from
`hubble-public.crypto_stellar.history_contract_events`. Combine it with each
source's events-to-trades ratio to cross-check decoder coverage. SDEX uses
`hubble-check` instead (Hubble has a decoded `history_trades` view); Soroban
DEXes and oracles do not.

## When to run

After a WASM-history audit flips a Soroban source's `BackfillSafe` to `true`, a
since-inception backfill batch lands, or a decoder change ships. It is a
regression gate: a dropped topic shape or wrong ratio makes the Hubble count and
our row count diverge.

## Per-source recipe

Run the Hubble query, run our-side query, compare with the multiplier. All
examples use `-from 51000000 -to 62250000 -bigquery-project <BQ_PROJECT>`;
`-output total` prints N events. Our side is `SELECT COUNT(*) FROM trades WHERE
source = '<source>' AND ledger BETWEEN <from> AND <to>` (M rows) unless noted.

### Soroswap

2 events per trade (`swap` + `sync`); filter topic[1]='swap' for one per trade.

```sh
stellarindex-ops hubble-soroban-events \
  -from 51000000 -to 62250000 \
  -bigquery-project <BQ_PROJECT> \
  -contracts <pair-contract-A>,<pair-contract-B>,<pair-contract-C>... \
  -topic0 SoroswapPair \
  -topic1 swap \
  -output total
```

**Expected N == M** (one trade row per swap-sync correlation). Enumerate pair
contracts from distinct contract IDs in `trades` under `source='soroswap'`, or
walk factory `new_pair` events.

### Aquarius

1 event per trade.

```sh
stellarindex-ops hubble-soroban-events \
  -from 51000000 -to 62250000 \
  -bigquery-project <BQ_PROJECT> \
  -contracts <pool-A>,<pool-B>... \
  -topic0 trade \
  -output total
```

Compare to `source = 'aquarius'`. **Expected N == M.**

### Phoenix

8 events per trade (one per field). Filter topic[0]='swap' AND
topic[1]='offer_amount' (conventional pick, strictly numeric; any of the 8 field
names works) for one per trade.

```sh
stellarindex-ops hubble-soroban-events \
  -from 51000000 -to 62250000 \
  -bigquery-project <BQ_PROJECT> \
  -contracts <pool-A>,<pool-B>... \
  -topic0 swap \
  -topic1 offer_amount \
  -output total
```

Compare to `source = 'phoenix'`. **Expected N == M.** Without the topic[1] filter
you get 8N; if `total / 8 != trades`, events are missing and the correlation is
dropping incomplete RawSwaps.

### Comet

1 event per trade.

```sh
stellarindex-ops hubble-soroban-events \
  -from 51000000 -to 62250000 \
  -bigquery-project <BQ_PROJECT> \
  -contracts <pool-A>,<pool-B>... \
  -topic0 POOL \
  -topic1 swap \
  -output total
```

Compare to `source = 'comet'`. **Expected N == M.**

### Reflector (DEX / CEX / FX)

1 event fans out to N OracleUpdate rows (one per (asset, price) in the prices Vec).

```sh
# Per variant: substitute the contract from cfg.Oracle.Reflector.{DEX,CEX,FX}Contract.
stellarindex-ops hubble-soroban-events \
  -from 51000000 -to 62250000 \
  -bigquery-project <BQ_PROJECT> \
  -contracts <reflector-DEX-or-CEX-or-FX> \
  -topic0 REFLECTOR \
  -topic1 update \
  -output total
```

```sql
SELECT COUNT(*) FROM oracle_updates
 WHERE source = 'reflector-dex'  -- or -cex / -fx
   AND ledger BETWEEN 51000000 AND 62250000;
```

**Expected N <= M** (typically much less); roughly `M / N ~ N_assets`.

### Redstone

1 event per write_prices call, fanning to N OracleUpdate rows (one per feed in
the call's feed_ids op-arg).

```sh
stellarindex-ops hubble-soroban-events \
  -from 51000000 -to 62250000 \
  -bigquery-project <BQ_PROJECT> \
  -contracts <redstone-adapter> \
  -topic0 REDSTONE \
  -output total
```

Compare to `source = 'redstone'`. **Expected N <= M.**

### Band

**Not applicable.** Band's Soroban contract emits zero events (AGENTS.md); the
decoder reads InvokeContract op args via the `ContractCallDecoder` hook. The
per-WASM-hash decoder audit is the only safety net.

## When totals differ

In rough order of likelihood:

1. **Contract-list incompleteness.** Re-enumerate pair/pool contracts from
   on-chain history and re-run.
2. **Topic-filter typo.** Strings are exact and case-sensitive. Most Soroban
   events use `ScvSymbol`; Soroswap's `SoroswapPair` and Phoenix use `ScvString`,
   including the space-bearing `actual received amount` (Q2). The filter matches
   raw XDR in Hubble's `topics` column as either type; any other topic type
   (address, number) never matches.
3. **Range edge effects.** Re-run with a wider `from` / `to`.
4. **Decoder bug.** The case the check exists to catch. Drill down per ledger
   with `-output json` or `-output csv` to find the range, then inspect the
   decoder against that range's WASM hash via `stellarindex-ops wasm-history`.
5. **Identical-event collision on Hubble's side.** The query dedups with
   `COUNT(DISTINCT contract_event_xdr)` to strip overlapping-batch-load
   duplicates, but `contract_event_xdr` has no tx hash or op index, so two
   distinct same-ledger events with byte-identical contents (e.g. a bot calling
   twice) collapse to one, under-reporting N. If a small persistent gap
   survives 1-4, check for repeated identical calls in that ledger first.

## Cost preview

Dry-run before a full-range query:

```sh
stellarindex-ops hubble-soroban-events ... -dry-run-bytes
```

Typical: 20-40 GB scan per 1M-ledger range, ~$0.20 at $5/TB on-demand. Every
real query also carries `-max-bytes-billed` (default 100000000000, 100 GB): a
query that would scan more fails before billing. Raise it deliberately for a
wider range; `0` is refused because BigQuery reads it as uncapped.
