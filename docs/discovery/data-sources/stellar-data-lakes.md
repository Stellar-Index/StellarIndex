# Stellar public data lakes

**Status:** 🧪 Understand what exists; use selectively.
**Our own lake (MinIO + Galexie) is primary;** public lakes are
fallbacks, backfill accelerators, and discovery tools.

**Verified against:**
- `stellar-docs/docs/data/analytics/hubble/README.mdx`
- `stellar-docs/docs/data/analytics/hubble/analyst-guide/*.mdx`
- `stellar-docs/docs/data/analytics/hubble/data-catalog/data-dictionary/{bronze,silver,gold}/*.mdx`
- `stellar-docs/docs/data/analytics/README.mdx`
- `stellar-ledger-data-indexer/config-test.toml` (for the SDF GCS
  bucket path, see below)

## Three distinct "data lakes"

When people say "the Stellar public data lake" they could mean any of
three different things:

| # | Lake | Type | Access | Latency | Our use |
| - | ---- | ---- | ------ | ------- | ------- |
| 1 | SDF Galexie GCS bucket | Raw zstd-XDR `LedgerCloseMeta` files | `gs://sdf-ledger-close-meta/v1/ledgers/pubnet` | Near-realtime (5 s ledger cadence) | Optional backfill source |
| 2 | Hubble BigQuery dataset | Structured SQL tables (bronze/silver/gold) | `crypto-stellar.crypto_stellar` on GCP BigQuery | Intraday batch | Ad-hoc discovery queries only |
| 3 | Third-party analytics | Goldsky / Mercury / Allium / etc | Varies | Varies | Not planned |

## 1. SDF public Galexie bucket

**Location:** `gs://sdf-ledger-close-meta/v1/ledgers/pubnet`

Verified from `stellar-ledger-data-indexer/config-test.toml:5`:

```toml
[datastore_config]
type = "GCS"
[datastore_config.params]
destination_bucket_path = "sdf-ledger-close-meta/v1/ledgers/pubnet"
[datastore_config.schema]
ledgers_per_file    = 1
files_per_partition = 64000
```

Key facts:

- **Schema**: `ledgers_per_file = 1` — **one `LedgerCloseMeta` per
  object**. Maximum granularity; minimum per-read latency.
- **Partitioning**: `files_per_partition = 64000`. Partition directory
  name derived from `floor(ledger_seq / 64000) * 64000`.
- **Format**: zstd-compressed XDR-serialised
  `datastore.LedgerMetaArchive` (wraps `Vec<LedgerCloseMeta>`).
- **Metadata keys** attached per object (9 keys — see
  [composable-data-platform.md](composable-data-platform.md)):
  `start-ledger`, `end-ledger`, `start-ledger-close-time`,
  `end-ledger-close-time`, `protocol-version`, `core-version`,
  `network-passphrase`, `compression-type`, `version`.
- **Free to read**? GCS requester-pays ambiguous — see Open items.
  We pay egress from GCP to our colo if we use this heavily.

### Why we wouldn't use it as primary

1. **Egress cost** — continuously pulling from GCS to our colo is a
   recurring bill. Running our own Galexie against our own MinIO is a
   fixed cost.
2. **Dependency on SDF uptime** — if SDF changes the path or cadence,
   we break.
3. **Consistency window** — SDF's captive-core cadence becomes our
   critical path, not ours.
4. **Auditability** — for a pricing API we want to own the
   extraction end-to-end.

### Why we *would* use it

1. **Historical backfill accelerator.** Rather than running our own
   `CATCHUP_COMPLETE` (which takes weeks on pubnet), we mirror the
   relevant SDF bucket contents to our MinIO once, then switch to
   our own production Galexie for live writes.
2. **Bootstrap / disaster recovery** — if our Galexie lake is
   corrupted, re-seed from SDF's.
3. **Cross-validation** — occasional spot-checks that our
   independently-produced files match SDF's byte-for-byte (they
   should, since both come from canonical stellar-core).

### Open items on the SDF bucket

- [ ] Confirm read-access and billing model. Public read, or
      requester-pays? Check `gsutil ls -L gs://sdf-ledger-close-meta/`
      headers.
- [ ] Measure actual earliest-ledger available. SDF may not retain
      all the way to genesis — possibly only from Protocol 22 onwards
      or similar.
- [ ] Check whether testnet has an equivalent public bucket.
- [ ] Cross-verify with a `gcloud alpha storage ls` command at some
      point to validate the `v1/` versioning in the path.

## 2. Hubble — the BigQuery analytics dataset

**Location:** `crypto-stellar.crypto_stellar` on GCP BigQuery
(public dataset).

Per `stellar-docs/docs/data/analytics/hubble/README.mdx`:

> "Hubble is an open-source, publicly available dataset that provides
> a complete historical record of the Stellar network. … hosted on
> BigQuery … suitable for large, analytic workloads, historical data
> retrieval and complex data aggregation. **Hubble should not be used
> for real-time data retrieval and cannot submit transactions.**"

### Update cadence

"Intraday batches. There is no guarantee for same-day data
availability." Not suitable for our hot path.

### Cost model

SDF pays storage; users pay query compute (BigQuery pay-per-scan
pricing). So ad-hoc queries have a real dollar cost per TB scanned.
Partition-by-date + column projection keeps it affordable.

### Medallion schema (bronze / silver / gold)

Verified from the docs folder structure
(`data-catalog/data-dictionary/{bronze,silver,gold}/`).

#### Bronze (raw / append-only history)

Full chronological ingestion from the network:

```
account-signers          accounts              claimable-balances
config-settings          contract-code         contract-data
evicted-keys             history-assets        history-contract-events
history-effects          history-ledgers       history-operations
history-trades           history-transactions  liquidity-pools
offers                   restored-key          trustlines
ttl
```

**`history-trades`** and **`history-contract-events`** are the
pricing-relevant ones. We could validate our own SDEX trade
extraction against Hubble's `history_trades` once we have a working
pipeline.

#### Silver (curated / current-state)

```
account-signers-current       accounts-current             accounts-snapshot
claimable-balances-current    config-settings-current      contract-code-current
contract-data-current         contract-data-snapshot       enriched-history-operations
enriched-history-operations-soroban                        evicted-keys-snapshot
liquidity-pools-current       liquidity-pools-snapshot     offers-current
token-transfers-raw           trustlines-current           trustlines-snapshot
ttl-current
```

**`token-transfers-raw`** and **`enriched-history-operations-soroban`**
are potentially useful cross-checks for our Soroban-side extraction
(Soroswap / Aquarius / Blend events).

#### Gold (analytics aggregates)

```
asset-balances-daily-agg     daily-fee-stats-agg
fee-stats-agg                hourly-fee-agg-account
hourly-soroban-fee-agg-contract
ledger-fee-stats-agg         trade-agg       tvl-agg
```

**`trade-agg`** and **`tvl-agg`** are directly relevant — we can look
at how SDF structures their trade aggregates as a reference for our
own OHLC / VWAP tables (without adopting the schema directly).

### Stellar-ETL — the open-source pipeline behind Hubble

`stellar-docs/docs/data/analytics/README.mdx:12`:

> "Within Hubble, the stellar-etl product powers the extraction and
> transformation of Stellar network data into BigQuery. This
> open-source pipeline allows developers to extend or customize data
> ingestion workflows based on their analytical needs."

Repo to audit: `stellar/stellar-etl` (not yet cloned). Open item.
Worth understanding their transformation logic as a second reference
implementation alongside `stellar-extract`.

### How we use Hubble

- **Phase 1 discovery:** ad-hoc queries to understand pubnet data
  patterns — asset volume distributions, historic trade counts per
  pair, Soroban contract activity distributions, protocol upgrade
  dates. Budget a few dollars of BigQuery spend for this discovery.
- **Cross-validation:** once our own pipeline runs, periodically
  spot-check that `our_trades_count == Hubble history_trades count`
  for a given ledger range. Divergence > 0 is a bug in our
  extractor.
- **NOT production:** we do not call BigQuery in our serving path.
  Hot queries go to TimescaleDB + Redis only.

### Open items on Hubble

- [ ] Open a GCP project dedicated to CTX Rates discovery queries.
      Set a BigQuery query-cost budget alert (e.g. $50/month) to avoid
      surprises.
- [ ] Clone `stellar/stellar-etl` and audit its extraction logic as a
      complement to `stellar-extract`.
- [ ] Build a one-off notebook/query: per-asset historical trade
      counts, VWAP over different windows, top 100 pairs by volume.
      Use the outputs to inform our aggregation defaults.

## 3. Third-party analytics providers

Listed in `stellar-docs/docs/data/analytics/analytics-providers/analytics-providers.mdx`
and `stellar-docs/docs/data/indexers/README.mdx` but out of scope for
our ingestion. Noted so we know what the landscape looks like:

- **Goldsky Mirror** — Stellar-supported ETL-to-DB pipelines.
- **Mercury / Retroshades** — Stellar-native Soroban-focused indexer.
- **Allium** — commercial data platform, Stellar Q1 2026 target.
- **Space and Time** — "Proof of SQL" ZK-verifiable query service
  (launched Q4 2025 for Stellar).
- **OBSRVR Gateway / Flow** — see our withObsrvr audit docs.
- **Alchemy** — Stellar support targeted H1 2026.

We are not a consumer of any of these in our architecture. We may
*become one of these categories* to some consumers of our API over
time (specifically a Portfolio-API adjacent: we do pricing, not
full portfolio).

## Summary: which lake for which purpose

| Purpose                             | Lake                                 |
| ----------------------------------- | ------------------------------------ |
| Production live ingest              | **Our own MinIO + Galexie**          |
| Historical backfill (fast path)     | Mirror SDF Galexie bucket once, then our own lake |
| Ad-hoc research / validation SQL    | **Hubble BigQuery**                  |
| Cross-check our trade extraction    | Hubble `history_trades` vs. ours     |
| Schema inspiration                  | Hubble gold (`trade-agg`, `tvl-agg`) |
| Real-time customer queries          | **Our TimescaleDB + Redis**          |

Nothing in the public-lake layer becomes part of our serving path.
All three lake types inform the build; only the MinIO lake is on the
production critical path.

## References

- Analytics overview: `stellar-docs/docs/data/analytics/README.mdx`
- Hubble docs root: `stellar-docs/docs/data/analytics/hubble/`
- Indexers overview: `stellar-docs/docs/data/indexers/README.mdx`
- Related: [composable-data-platform.md](composable-data-platform.md),
  [galexie.md](galexie.md), [archival-nodes.md](archival-nodes.md),
  [stellar-ledger-data-indexer.md](stellar-ledger-data-indexer.md).
