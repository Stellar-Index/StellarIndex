---
name: add-cex-connector
description: Add a new off-chain CEX or FX venue to Stellar Index via the external.Connector framework — streamer/poller/backfiller shape, scaling rules, registry metadata, VWAP-eligibility. Use when integrating an exchange or FX feed, or when external trades arrive mis-scaled.
---

# /add-cex-connector

Canonical checklist: `docs/contributing/add-cex-connector.md`. This
skill adds the traps and the checks.

## Shape

- Lives at `internal/sources/external/<venue>/` — NOT
  `internal/sources/<venue>/` (that's the on-chain convention) and
  NOT a `consumer.Source` (deleted legacy seam).
- Implement the `external.Connector` sub-interfaces you actually
  need: `Streamer` (live WS — copy binance/kraken), `Poller` (REST
  quote board — the FX pattern), `Backfiller` (historical candles).
- Use the SHARED helpers — do not copy-paste from a sibling
  (the pre-extraction era left 10 scaling copies):
  `internal/sources/external/scale` for amount scaling, the shared
  WS reconnect client if present (check CAPABILITY-INVENTORY.md's
  external section for the current names).
- Fixtures are inline golden frames in the package's `*_test.go`.

## The scaling trap (source of real incidents)

External amount scaling is NOT uniform: CEX + reference aggregators
normalise to 1e8; FX pollers use 1e6 (`DefaultDecimals = 6`). Always
set the venue's `Decimals` explicitly in its Metadata and test a
round-trip through `canonical.Trade` — a wrong exponent poisons USD
volume gates ~100× (CS-040 class).

## Registry + wiring

1. `Metadata` in `internal/sources/external/registry.go`: Class
   (`exchange` contributes to VWAP; `aggregator`/`oracle`/
   `authority_sanity` are reported-only), Subclass, `IncludeInVWAP`,
   `BackfillSafe` (off-chain = true), Decimals, weight.
2. `buildExternal` in `cmd/stellarindex-indexer/main.go` AND the
   parallel block in `cmd/stellarindex-ops` behind a
   `cfg.<Venue>.Enabled` gate (+ config struct with doc tags →
   `make docs-config`).
3. If the venue should appear in freshness monitoring, extend
   `data-freshness.sh`'s domain thresholds — a daily-publishing feed
   needs a daily-scale threshold (the ecb lesson: 4 days, not 3h).

## Checks

```sh
go test ./internal/sources/external/<venue>/ ./internal/sources/external/
go test ./internal/config/          # tag/Default lockstep (F-1327)
make docs-config                    # regenerate + commit
```

Then run the connector against the venue's REAL endpoint once
(sandbox key if needed) and eyeball a parsed `canonical.Trade` —
symbol mapping and decimal scaling are where vendor docs lie.

Vendor ToS note: raw per-trade redistribution is restricted by most
CEX terms (CS-115) — blended outputs are fine; check before wiring
the venue into `/v1/observations`.

Finish with **/verify-done**.
