# stellar-ledger-data-indexer

**Status:** ⚠️ Reference only — **not a drop-in for our rates engine**.
Useful as a canonical example of how to drive the Go Ingest SDK from a
Galexie data lake into Postgres; **not** a DEX/trade indexer.

**Repo:** <https://github.com/stellar/stellar-ledger-data-indexer>
**Verified against:** `main.go`, `cmd/root.go`, `internal/config.go`,
`internal/input/ledgerMetaDataReader.go`,
`internal/db/migrations/*.sql`, `config-test.toml`, `go.mod` at clone
time (2026-04-22).

## What it actually does

Reads `LedgerCloseMeta` objects out of a Galexie GCS bucket, transforms
them, and writes **Soroban contract state (`contract_data`) and TTL
records** to Postgres. Nothing else.

The full set of migrations is:

```
20250807-create-contract-data.sql
20250807-create-ttl.sql
20251017-polish-indexes.sql
20260203-add-ttl-to-contract-data.sql
20260209-update-indexes.sql
20260210-add-additional-lab-backend-indexes.sql
20260211-add-additional-lab-backend-indexes-1.sql
20260211-add-additional-lab-backend-indexes-2.sql
20260211-add-additional-lab-backend-indexes-3.sql
20260225-reindex-contract-data-live-until.sql
```

Two tables total: `contract_data` and `ttl`. No `trades`, no
`liquidity_pools`, no `offers`, no `path_payments`, no `price_history`.
Grep for `CreateTrade`, `ClaimAtom`, `ManageOffer`, `PathPayment`, or
`trade` in the repo returns zero hits.

Confirms this is SDF's **Laboratory / Explorer backend indexer**
(recent migration names include "lab-backend"), not a price-relevant
indexer.

## What we can steal from it

The ingestion plumbing is the canonical CDP-consumer pattern and is
exactly what we need for our own Soroban event indexer. Key snippet
(`internal/input/ledgerMetaDataReader.go:108-124`):

```go
pubConfig := ingest.PublisherConfig{
    DataStoreConfig:       a.dataStoreConfig,
    BufferedStorageConfig: ingest.DefaultBufferedStorageBackendConfig(a.dataStoreConfig.Schema.LedgersPerFile),
}
pubConfig.BufferedStorageConfig.RetryLimit = 20
pubConfig.BufferedStorageConfig.RetryWait  = 3

return ingest.ApplyLedgerMetadata(ledgerRange, pubConfig, ctx,
    func(lcm xdr.LedgerCloseMeta) error {
        for _, processor := range a.processors {
            if err := processor.Process(ctx, utils.Message{Payload: lcm}); err != nil {
                return err
            }
        }
        return nil
    })
```

Three things to lift for our own pipeline:

1. **`ingest.ApplyLedgerMetadata(range, config, ctx, handler)`** is the
   SDK entry point that fans `LedgerCloseMeta` into a handler while
   hiding all Galexie file-walking / buffered-read / retry logic.
2. **`ingest.DefaultBufferedStorageBackendConfig(ledgersPerFile)`** is
   the idiomatic way to match consumer config to producer (Galexie)
   layout.
3. The **DB-aware bounded/unbounded resolver** in `GetLedgerBound`
   (`ledgerMetaDataReader.go:54-93`) — if the DB already has ledger N,
   start from N+1, else start from genesis or the CLI flag. We'll adapt
   this for our own cursor table.

## CLI surface (verified, `cmd/root.go:35-42`)

```
stellar-ledger-data-indexer [--start N] [--end N]
                            [--config-file PATH]    (default "config.toml")
                            [--backfill]            (ignore DB cursor)
                            [--metrics-port PORT]   (default 8080)
```

Env-var mapping via Viper `KebabToConstantCase`: `--start` → `START`,
`--metrics-port` → `METRICS_PORT`, etc.

- `end = 1` (the default) is the **unbounded sentinel**. Ledgers start
  at 2 in Stellar, so 1 is safely "no bound".
- `--backfill` disables the DB-cursor shortcut — useful for re-indexing
  a range.

## Config schema (verified, `internal/config.go:60-68`)

```toml
[datastore_config]
type = "GCS"

[datastore_config.params]
destination_bucket_path = "path/to/galexie/bucket"

[datastore_config.schema]
ledgers_per_file    = 1
files_per_partition = 64000

[stellar_core_config]
network = "pubnet"
# or network_passphrase + captive_core_toml_path (mutually exclusive with `network`)

[postgres_config]
host     = "postgres"
user     = "postgres"
database = "postgres"
port     = 5432
```

The validation logic is strict: setting both `network` and
(`network_passphrase`/`captive_core_toml_path`) is explicitly rejected
(`config.go:108-111`).

## Critical fact for us: SDF's public Galexie bucket path

The repo's `config-test.toml` uses:

```toml
destination_bucket_path = "sdf-ledger-close-meta/v1/ledgers/pubnet"
```

That's the **SDF public data lake**. Two immediate takeaways:

- SDF publishes with `ledgers_per_file = 1` — the best possible
  freshness/granularity setting (single ledger per object).
- The bucket is on GCS. GCP egress is free within the same region, but
  comes out of our pocket if we consume from our colo.

Captured more fully in
[stellar-data-lakes.md](stellar-data-lakes.md) (to be written).

## Why it's not suitable as our primary pipeline

The indexer hardcodes `contract_data` + `ttl` as the only processors
(`internal/contract/contract_data.go`,
`internal/contract/contract_code.go`, `internal/contract/ttl.go`,
`internal/contract/contract_events.go`). We'd either:

- **Fork and extend** with our own `Processor` implementations
  (SDEX trades, Soroban swap events, liquidity pool state).
- **Rewrite** using the same `ingest.ApplyLedgerMetadata` pattern in a
  fresh repo so our schema and dependency choices are independent.

For a production DEX price feed the second is cleaner — we don't want
our schema entangled with SDF's "lab-backend" schema.

## Dependency landscape

`go.mod` imports from `github.com/stellar/go-stellar-sdk` (new SDK
location). Go 1.25 required (consistent with Galexie).

## Open items

- [ ] Read `contract/contract_events.go` to confirm whether *any* event
      extraction logic exists that we could reuse for Soroswap / Blend
      events (likely only schema-agnostic event copy, not per-protocol
      decoding).
- [ ] Inspect the integration-test fixtures (they use live SDF pubnet
      GCS data) — may be a useful way to exercise our own pipeline
      against real data.

## References

- Repo: <https://github.com/stellar/stellar-ledger-data-indexer>
- Galexie audit: [galexie.md](galexie.md)
- CDP overview: [composable-data-platform.md](composable-data-platform.md)
