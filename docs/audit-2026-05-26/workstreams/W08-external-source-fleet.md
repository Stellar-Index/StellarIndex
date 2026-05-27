# W08 — External source fleet + policy

## Scope

Every non-Stellar venue feeding our aggregator + every
divergence-reference oracle.

- `internal/sources/external/binance/`
- `internal/sources/external/bitstamp/`
- `internal/sources/external/coinbase/`
- `internal/sources/external/kraken/`
- `internal/sources/external/cryptocompare/`
- `internal/sources/external/coingecko/`
- `internal/sources/external/coinmarketcap/`
- `internal/sources/external/ecb/`
- `internal/sources/external/exchangeratesapi/`
- `internal/sources/external/polygonforex/`
- `internal/sources/external/chainlink/` (NEW)
- `internal/sources/external/registry.go`
- `internal/sources/external/framework.go`, `runner.go`
- `internal/sources/forex/` (CSV-based circulation_data)
- `internal/sources/frankfurter/`

## Inputs

- registry.go (class + BackfillSafe flags)
- per-vendor adapter + tests
- `docs/operations/runbooks/external-poller-{error-rate-high,stale}.md`

## Per-vendor loop

| Check | Result | Evidence |
| --- | --- | --- |
| 1. Vendor truth (API docs URL, current rate limits, redistribution licence) | | |
| 2. Auth hygiene (env-only; key-absence-disables, not panic) | | |
| 3. Normalisation (uniform 10^8 amount scale per CLAUDE.md) | | |
| 4. Pair normalisation matches canonical Pair | | |
| 5. Retry + backoff + jitter | | |
| 6. Clock-skew tolerance | | |
| 7. Registry class (exchange / aggregator / oracle / authority_sanity) | | |
| 8. Inclusion policy (aggregator-feeding or divergence-only) | | |
| 9. Backfill safety (BackfillSafe true requires deterministic vendor history) | | |
| 10. Failure-mode coverage (5xx, 429, 401, timeout, malformed body) | | |

## NEW: chainlink-specific

- Chainlink is added as `ClassOracle` divergence reference.
- HTTP RPC client; not a streaming source.
- Used by `internal/divergence/chainlink.go`.
- Verify: pair binding (no wrong-pair signature poisoning).
- Verify: graceful skip on RPC outage (doesn't poison
  divergence_observations with nulls).

## NEW: forex CSV-vs-API

- `internal/sources/forex/circulation_data.csv` is the
  per-fiat-currency reserve seed.
- Worker refreshes from API endpoints; verify the CSV is the
  fallback, not the primary.
- `cmd/ratesengine-ops/circulation-fetch.sh` (NEW) — verify
  this script is the canonical update flow.

## Closure criteria

11 external + 2 sibling (forex/frankfurter) per-vendor tables
filled. Findings on:

- any vendor where pair normalisation could be exploited
- any class mis-assignment (an oracle silently feeding the
  exchange-class VWAP)
- any retry storm under sustained 5xx
