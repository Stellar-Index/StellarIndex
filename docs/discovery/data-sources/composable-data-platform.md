# Composable Data Platform (CDP)

**Status:** ✅ This is the architectural umbrella we're building against.
Galexie + Ingest SDK + downstream consumers form our ingestion
backbone. Not a single piece of software; a cohesive pattern of
several tools that interoperate.

**Verified against:**
- `stellar-docs/docs/data/indexers/README.mdx`
- `stellar-docs/docs/build/apps/ingest-sdk/overview.mdx`
  (via earlier WebFetch)
- `stellar-galexie` repo (see [galexie.md](galexie.md))
- `go-stellar-sdk/support/datastore`, `support/storage`,
  `historyarchive`, `ingest` packages
- `stellar-docs/docs/data/indexers/build-your-own/`

## What the CDP is

Per SDF's own framing (`ingest-sdk/overview.mdx` referenced earlier),
CDP is a pipeline pattern, not a monolith:

```
stellar-core ──► Galexie ──► Data Lake (GCS/S3) ──► Ingest SDK ──► Your app
                 (producer)   (zstd-XDR files)      (consumer)
```

Key property: **multiple implementations can slot into each stage**.
Producer is always Galexie today, but the Datastore interface is
generic. Consumer can be:

- Our own code using `ingest.ApplyLedgerMetadata(...)`.
- `stellar-rpc` (data-lake mode — replaces captive-core for historical
  retention; see [archival-nodes.md](archival-nodes.md)).
- `stellar-ledger-data-indexer` — SDF's reference Postgres indexer
  (see [stellar-ledger-data-indexer.md](stellar-ledger-data-indexer.md)).
- withObsrvr's `cdp-pipeline-workflow`, `bronze-copier`, `nebu` (see
  [withobsrvr-overview.md](withobsrvr-overview.md)).
- Third-party providers (Goldsky Mirror, Mercury Retroshades, etc).

## Component inventory

### Producer side

| Component | What it does | Our status |
| --- | --- | --- |
| **stellar-core** | Canonical network implementation; replicated state machine. | ✅ We run watchers in Phase 1; three Full Validators later. [archival-nodes.md](archival-nodes.md) |
| **captive-core** | `stellar-core` run as a subprocess by another Go program (Galexie, stellar-rpc) with `--in-memory` mode. | Used indirectly via Galexie and stellar-rpc. |
| **stellar-archivist / rs-stellar-archivist** | Tool to mirror/validate Stellar history archives. Our path: mirror SDF's public archive to our MinIO to pre-seed catchup. | ✅ [stellar-archivist.md](stellar-archivist.md) |
| **Galexie** | Captive-core-driven exporter. Writes zstd-XDR `LedgerCloseMeta` objects to a Datastore. | ✅ [galexie.md](galexie.md) |

### Datastore layer (the "lake")

Generic interface in `go-stellar-sdk/support/datastore`. Concrete
implementations:

| Backend | Our use? | Notes |
| --- | --- | --- |
| GCS (`gcs.go`) | If we consume SDF's public lake. | Full metadata support. |
| S3 (`s3.go`) | ✅ — via MinIO on our own hardware with `endpoint_url` override. | Full metadata support. |
| Filesystem (`filesystem.go`) | ❌ Dev only. Silently drops per-object metadata, unsafe for multi-process writes. See [decisions.md](../decisions.md). |

Object metadata keys attached to every LedgerCloseMeta file (from
`go-stellar-sdk/support/datastore/object_metadata.go`):

```
start-ledger                  end-ledger
start-ledger-close-time       end-ledger-close-time
protocol-version              core-version
network-passphrase            compression-type
version
```

`protocol-version` in that list is why our
[protocol-versions.md](../protocol-versions.md) doc cares — we can
route LedgerCloseMeta files to version-specific decoders without
opening their contents.

### Consumer side

| Component | What it is | Our use |
| --- | --- | --- |
| **Ingest SDK** (`go-stellar-sdk/ingest`) | Go packages: `ledgerbackend`, `ingest`, `xdr`, `historyarchive`, `support/datastore`, `support/storage`, `network`, `amount`. Drives the whole consumer side. | ✅ Direct dependency. Core loop: `ingest.ApplyLedgerMetadata(range, config, ctx, handler)`. |
| **LedgerTransactionReader** | `ingest.NewLedgerTransactionReaderFromLedgerCloseMeta(passphrase, lcm)` → iterate typed `LedgerTransaction` objects. SDK abstracts protocol version differences. | ✅ Primary extraction entry point. |
| **BufferedStorageBackend** | `ingest.DefaultBufferedStorageBackendConfig(ledgersPerFile)` — retry / worker / buffer config for consuming a Datastore. | ✅ Use the default + tweak retry limits like the reference indexer does. |
| **Processors** | SDF-maintained Go packages that parse specific event / op types. Listed in `data/indexers/build-your-own/processors/README.mdx`. Pluggable. | 🧪 Evaluate; likely prefer `withObsrvr/stellar-extract` which bundles 22 extractors in one library. |
| **stellar-rpc (data-lake mode)** | stellar-rpc can ingest from a Datastore instead of captive-core. `SERVE_LEDGERS_FROM_DATASTORE` config flag + `[buffered_storage_backend_config]` stanza. | 🧪 Optional — we probably run captive-core-mode stellar-rpc for live, and rely on Galexie-backed reads only when a user requests ledgers outside our retention window. |

## Why CDP replaces the old Horizon pipeline

Historically the ingestion path was stellar-core → Postgres (or
captive-core → Horizon → Postgres). That model had three problems CDP
fixes:

1. **Tight coupling** — Horizon defined the schema and the ingest
   logic together. Anyone wanting a different schema had to fork.
2. **Replayability** — reprocessing historical data required redoing
   the whole pipeline from core.
3. **Horizontal scaling** — the single-indexer model couldn't fan out.

With CDP:
- The Datastore is immutable. Anyone can spin up a consumer at any
  point in time without affecting producers.
- Multiple consumers can read the same lake concurrently.
- Reprocessing is a re-read of the lake, not a re-run of core.
- Hard fork experiment? Just write an alternative consumer.

This matches our self-hosted, open-source, multi-deployment story.

## How we use CDP

Our Phase-1 pipeline:

```
┌──────────────┐     ┌────────┐     ┌────────────────┐     ┌─────────────────┐
│ stellar-core │ ──► │ Galexie│ ──► │ MinIO (our S3) │ ──► │ Our CTX Rates   │
│ watcher      │     │        │     │ Datastore      │     │ consumer (Go)   │
└──────────────┘     └────────┘     └────────────────┘     │  └─ Ingest SDK  │
                                                           │  └─ stellar-    │
                                                           │     extract     │
                                                           │  └─ TimescaleDB │
                                                           │  └─ Redis cache │
                                                           │  └─ REST API    │
                                                           └─────────────────┘

                     parallel live-freshness path:
┌──────────────┐     ┌────────────┐
│ stellar-core │ ──► │ stellar-rpc│ ──► Our live-event consumer (SSE / poll)
│ watcher      │     │ (captive)  │
└──────────────┘     └────────────┘
```

Design notes:

- **One `stellar-core` watcher** serves both Galexie and stellar-rpc
  via their respective captive-core subprocesses? **No** — each runs
  its own captive-core to maintain the expected subprocess lifecycle.
  That means two captive-core instances per box. Hardware-wise that
  is fine on our R640; operationally we monitor both.
- **MinIO cluster on our colo** hosts the Datastore. S3-compatible
  API; all metadata preserved.
- **Our consumer** is a Go binary using the Ingest SDK +
  `withObsrvr/stellar-extract` for typed-row extraction. Publishes to
  TimescaleDB + Redis (hot cache) + in-process SSE stream.

## Relationship to other indexing paths

Per `docs/data/indexers/README.mdx`:

- **Portfolio APIs** (SaaS) — Alchemy (2026 H1 launch targeted for Stellar),
  Allium (Q1 2026), OBSRVR, Horizon (deprecated, [decisions.md](../decisions.md)).
  We are **not a portfolio API**; our scope is pricing specifically.
- **Streaming / transformation** — The Graph Substreams, Goldsky
  Mirror (Stellar-compatible), Mercury, SubQuery, Space and Time.
  These are commercial ETL-to-DB services for custom data shapes.
  We don't adopt any of them — we run our own CDP consumer for
  control and cost.
- **Big-data analytics** — Hubble (see
  [stellar-data-lakes.md](stellar-data-lakes.md)). Useful for
  ad-hoc discovery queries but not our production path.

## Open items

- [ ] Design the **two-captive-core** co-location: running Galexie's
      and stellar-rpc's captive-core side-by-side on one box, with
      clear resource budgets. Decision needed before infra build.
- [ ] Decide the MinIO cluster topology: single-node in colo for
      Phase 1, or 4-drive erasure-coded from day one?
- [ ] Confirm `SERVE_LEDGERS_FROM_DATASTORE` config actually works
      in stellar-rpc v26.0.0. We'd need this for the "historical
      reads without holding full event history locally" story.
- [ ] Pin exact versions across the stack for reproducibility:
      stellar-core, stellar-rpc, stellar-galexie, go-stellar-sdk,
      stellar-extract, rs-stellar-archivist. Capture in a
      `VERSIONS.md` at repo root when Phase 2 work begins.

## References

- Indexers overview: `stellar-docs/docs/data/indexers/README.mdx`
- Ingest SDK overview: `stellar-docs/docs/build/apps/ingest-sdk/overview.mdx`
- Galexie audit: [galexie.md](galexie.md)
- stellar-archivist audit: [stellar-archivist.md](stellar-archivist.md)
- stellar-ledger-data-indexer: [stellar-ledger-data-indexer.md](stellar-ledger-data-indexer.md)
- Archival node / hot-path plan: [archival-nodes.md](archival-nodes.md)
- Decisions: [../decisions.md](../decisions.md) (Horizon ❌, MinIO,
  i128, Tier-1 validators)
