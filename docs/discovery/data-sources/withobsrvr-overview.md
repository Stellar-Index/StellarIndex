# withObsrvr — ecosystem overview & verdicts

**Org:** <https://github.com/orgs/withObsrvr/repositories> (70 repos total,
all appear active).

withObsrvr is an independent Stellar indexing operator. They ship a
multi-tier set of tools — a library (`stellar-extract`), a
contract/toolkit (`nebu`), a legacy monolithic pipeline
(`cdp-pipeline-workflow`), and a new pipeline orchestrator (`flowctl`).

This doc is the cross-repo summary. Per-repo deep-dives (verified from
source code at clone time 2026-04-22) live alongside this file.

## Verdict matrix

| Repo                     | Status | Use it?                                | Per-repo doc                                                           |
| ------------------------ | ------ | -------------------------------------- | ---------------------------------------------------------------------- |
| **stellar-extract**      | ✅     | **Direct library dependency** — typed extractors already cover most of what we need | [withobsrvr-stellar-extract.md](withobsrvr-stellar-extract.md) |
| nebu                     | 🧪     | Design inspiration; not a direct dep  | [withobsrvr-nebu.md](withobsrvr-nebu.md)                              |
| flowctl                  | ⚠️     | Overkill for us; good reference        | [withobsrvr-flowctl.md](withobsrvr-flowctl.md)                        |
| cdp-pipeline-workflow    | ❌     | Do not fork. Legacy; contains bugs and a worse design than their newer repos | [withobsrvr-cdp-pipeline-workflow.md](withobsrvr-cdp-pipeline-workflow.md) |
| obsrvr-bronze-copier     | ⚠️     | Reference for "bronze" Parquet layer; not a drop-in | covered in this file                                               |
| obsrvr-lake-writer       | ⚠️     | Reference for serialising writes to DuckDB; not applicable to our TimescaleDB path | covered in this file                                     |
| nebu-sql                 | ⚠️     | Niche DuckDB table-function tool; not a fit for our query layer | covered in this file                                     |
| prism (explorer)         | 🧪     | Not yet investigated — Stellar block explorer, probably out of scope | not in this index                                                  |
| ttp-processor-demo       | 🧪     | Not yet investigated — TTP reference processor demo | not in this index                                                  |
| *(60 other repos)*       | ❓     | Not yet triaged                        | —                                                                      |

## Why their repo count is so large (70)

It's mostly many small, single-responsibility repos:

- Per-processor repos (e.g. individual Stellar processors shipped as
  their own Go modules, aligned with nebu's "external processors each get
  their own repo" model).
- Per-SDK-language clients (`flowctl-sdk`, `flowctl-sdk-js`,
  `flowctl-sdk-js-quickstart`).
- Starter/template repos (`obsrvr-starter-kit`,
  `stellar-horizon-starter-kit`).
- Unrelated or adjacent projects (`python-fbas`, `kwickbit-payments`,
  `nomad-pack-web3-registry`).

So the effective surface of what's interesting to us is ~10 repos.

## Key architectural takeaways (cross-repo)

### They evolved from a monolith to a library-first design

The chronology of the repos tells a story:

- **cdp-pipeline-workflow** (oldest, ~2024): one binary with every
  source adapter, every processor, every sink crammed in. JSON-between-
  stages. Many processors are buggy (see per-repo doc).
- **nebu** (2025): re-think. Clean three-processor contract (Origin /
  Transform / Sink), protobuf between stages, typed emitters, explicit
  warning/fatal error model. "The real nebu is the contract." Each
  processor ships as its own repo + binary.
- **stellar-extract** (2025): factored out the extraction logic into a
  pure library, shared by nebu processors, history loaders, and streaming
  ingesters. Single source of truth for XDR → typed-row mapping.
- **flowctl** (2025+): the orchestrator layer that composes external
  processor binaries into pipelines, with a gRPC control plane.

For us, the lesson is: the **useful leverage is at the
`stellar-extract` layer**. The orchestration/pipeline tools are about
multi-tenant operator ergonomics, which we don't need for a single-
purpose rates engine.

### They converged on the same `go-stellar-sdk` as us

All live repos depend on `github.com/stellar/go-stellar-sdk`
(stellar-extract at `v0.5.0`, cdp-pipeline-workflow transitively).
No fork, no re-implementation of XDR. Good — less version skew risk.

### They use DuckDB / DuckLake heavily, not TimescaleDB

A surprising amount of their stack is built around DuckDB and an
internal concept called "DuckLake":

- `cdp-pipeline-workflow` consumers: `SaveToDuckDB`,
  `SaveToDuckLakeEnhanced`, `SaveContractToDuckDB`.
- `obsrvr-lake-writer`: a gRPC front for DuckDB writes
  (single-writer constraint workaround).
- `nebu-sql`: a DuckDB table function.

TimescaleDB exists (`SaveToTimescaleDB`) but is clearly a sideline. We
chose TimescaleDB for our own reasons (time-series retention policies,
continuous aggregates, SQL familiarity). We won't adopt DuckDB for the
hot path.

### The "bronze" pattern

Several repos reference **Bronze / Silver / Gold** layering
(data-engineering medallion architecture):

- Bronze = raw, immutable source data (ledger XDR as Parquet).
- Silver = normalised, enriched records (typed trade rows).
- Gold = aggregates ready for query (OHLC, VWAP).

For us:

- Bronze-equivalent = our Galexie data lake in MinIO. No additional
  tooling needed unless we want Parquet specifically.
- Silver-equivalent = our normalised `trades` / `swaps` /
  `orderbook_snapshots` tables in TimescaleDB.
- Gold-equivalent = TimescaleDB continuous-aggregate hypertables for
  OHLC and VWAP windows.

We can adopt the *terminology* without adopting the tooling.

## SDK protocol-version handling — confirmed via their CLAUDE.md

withObsrvr's own internal CLAUDE.md for cdp-pipeline-workflow explicitly
confirms what I flagged as research question #10 in
[../protocol-versions.md](../protocol-versions.md):

> "When processing Stellar transactions, **ALWAYS use SDK helper
> methods instead of accessing metadata directly.** The SDK abstracts
> protocol version differences (V1, V2, V3, V4) automatically."

Example from their doc (verbatim):

```go
// ✅ CORRECT — Uses SDK helper
txEvents, err := tx.GetTransactionEvents()

// ❌ WRONG — Direct metadata access
if tx.UnsafeMeta.V == 3 {
    events = tx.UnsafeMeta.V3.SorobanMeta.Events
}
```

So **Galexie preserves native-epoch XDR**, but the SDK ships version-
aware helpers. This partially resolves my earlier question: we don't
need per-epoch decoders if we stick to SDK helpers. We still need to
verify which specific helpers cover our trade/event extraction paths,
but the architectural concern is smaller than I initially feared.
Captured the update in [../protocol-versions.md](../protocol-versions.md).

## What we plan to do

1. Adopt `stellar-extract` as a direct dependency for our
   ledger→typed-row extraction, pinned to a specific commit SHA.
2. Study `nebu`'s processor contract as design inspiration for our own
   internal processor interfaces, but **do not** import the nebu
   packages — too early-stage to take as a hard dependency.
3. Skip `cdp-pipeline-workflow` entirely.
4. Revisit `flowctl` in Phase 6+ if we ever need multi-tenant pipeline
   orchestration. For Phase 1–5 our pipeline runs in-process.

## Obsrvr Bronze Copier — short audit

Repo: <https://github.com/withObsrvr/obsrvr-bronze-copier>
Layout: `cmd/bronze-copier`, `internal/`, standard Go pipeline shape.

What it does: copies raw Stellar ledger XDR from Galexie buckets into
"Bronze" Parquet files on S3/GCS/B2/local, with checkpointing, hash-
chained audit records, and a PostgreSQL catalog of partition metadata.

Why we don't need it: we already keep our data lake in zstd-XDR form
via Galexie, and we materialise Silver (TimescaleDB) directly from
that. We don't have a Parquet step in our plan, and the PAS (Public
Audit Stream) is overkill for a pricing API. **Status:** ⚠️ reference.

## Obsrvr Lake Writer — short audit

Repo: <https://github.com/withObsrvr/obsrvr-lake-writer>

What it does: a gRPC server that serialises DuckDB writes from multiple
ingest processes. Solves the "DuckDB catalog is single-writer" problem.

Why we don't need it: we use TimescaleDB (PostgreSQL), which supports
concurrent writes natively. **Status:** ⚠️ reference only, for the
gRPC-facade-over-a-stateful-singleton pattern if we ever need it.

## nebu-sql — short audit

Repo: <https://github.com/withObsrvr/nebu-sql>

What it does: embeds DuckDB and registers a `nebu('processor', start=N,
stop=M)` table function that shells out to a nebu processor binary and
streams its NDJSON stdout into DuckDB.

Why we don't need it: our query layer is a REST API on top of
TimescaleDB + Redis. Developer-ergonomics-over-DuckDB isn't on our
critical path. **Status:** ⚠️ reference for future interactive
analytics use cases only.

## Repos I have NOT yet triaged

Noted in the verdict matrix as ❓. If any of the below turn out to
matter, I'll add a per-repo doc:

- prism (Soroban-first block explorer)
- ttp-processor-demo (TTP = "token transfer processor" — reference)
- nebu-processor-registry
- flow-proto
- flowctl-sdk / flowctl-sdk-js
- obsrvr-stellar-components
- rs-stellar-history-archive-hasher
- stellarbeat (network monitoring)
- terminal / prism-related frontends
- the rest (~40 small / unrelated repos)

## References

- withObsrvr org: <https://github.com/orgs/withObsrvr/repositories>
- Per-repo deep-dives live alongside this file.
