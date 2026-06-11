# Corrections to ctx-proposal.md

**Purpose:** a catalogue of every place where Phase-1 discovery
surfaced a fact that contradicts or materially extends what's
written in the awarded proposal (`docs/ctx-proposal.md`).

We **do not** rewrite the proposal — the customer awarded on the
text as-written, and we honour the scope commitments there. But
before we build against the proposal, the implementation team
should read this list so we don't code something that's
contractually promised but technically wrong.

When we next prepare a customer-facing revision (RFP follow-up
doc, delivery-plan update, or similar), these corrections get
folded in.

## Format

Each item:

- **Proposal quote** — verbatim text from `ctx-proposal.md`.
- **Correction** — what the primary source actually says.
- **Source** — which audit doc in `docs/discovery/` holds the
  verified finding.
- **Impact** — does the correction change what we commit to
  deliver, or just what we do under the hood?

---

## Oracle integration — Reflector methods

### Proposal says

> "Integration is via direct Soroban contract calls using the
> SEP-40 interface: `lastprice(asset)` for current prices,
> `prices(asset, n)` for historical records, `twap(asset, n)` for
> time-weighted averages, and the cross-pair equivalents
> `x_last_price(base, quote)`, `x_prices`, and `x_twap`."

### Correction

SEP-40's canonical interface is `base`, `assets`, `decimals`,
`resolution`, `price`, `prices`, `lastprice` — **only**. No `twap`.
No `x_*` cross-pair methods. These are not in the SEP-40 spec and
**are not implemented in Reflector v3** (the current mainnet
version; verified by reading `reflector-contract` at SHA
`4c6368f5…4db6e1`).

TWAP and cross-pair calculations are done **off-chain by us** —
fetch `prices(asset, n)` and compute TWAP locally; fetch two
`lastprice` values (both in the same oracle's base) for
cross-pair.

### Source

[oracles/reflector.md](oracles/reflector.md).

### Impact

No scope change — we still deliver TWAP and cross-pair pricing.
Changes the implementation: we do the math, we don't call an
on-chain helper.

---

## Reflector is three contracts, not one

### Proposal says (implicitly treats Reflector as one integration)

> "Reflector is a decentralized oracle network native to Stellar
> and Soroban, fully compliant with SEP-40. It is the primary
> oracle integration due to its Stellar-native design and active
> mainnet deployment."

### Correction

Reflector publishes **three separate oracle contracts** on
mainnet, each with its own `base()`, `decimals()`, and asset list:

| Contract | Data source |
| -------- | ----------- |
| `CALI2BYU…OB2PLE6M` | Stellar Mainnet DEX |
| `CAFJZQWS…JLN34DLN` | External CEXs & DEXs |
| `CBKGPWGK…KOMJRN63` | Fiat FX |

Each must be integrated separately.

### Source

[oracles/reflector.md](oracles/reflector.md).

### Impact

None on deliverables. Three integrations where we assumed one.

---

## Soroswap SwapEvent does not carry post-state reserves

### Proposal says

> "Swap events include post-state reserves (`new_reserve_0`,
> `new_reserve_1`), which are used as the authoritative reserve
> state for TWAP construction."

### Correction

`SoroswapPair`'s `SwapEvent` body is `{to, amount_0_in,
amount_1_in, amount_0_out, amount_1_out}` — no reserves. Reserves
come in the **following** `SyncEvent` which the pair emits
immediately after via its internal `update()` call. Indexers must
correlate the two by `(ledger, tx_hash, op_index)`.

### Source

[dexes-amms/soroswap.md](dexes-amms/soroswap.md), verified against
`soroswap-core/contracts/pair/src/event.rs` + `lib.rs:472-476`.

### Impact

Reserve-based TWAP still works — we just read it from `Sync`, not
`Swap`. Implementation detail only.

---

## Band has a native Soroban contract (not via REST)

### Proposal says

> "Integration will be via the BandChain REST API for reference
> prices on supported symbol pairs."

### Correction

Band deployed a native Soroban `StandardReference` contract on
pubnet: **`CCQXWMZVM3KRTXTUPTN53YHL272QGKF32L7XEDNZ2S6OSUFK3NFBGG5M`**
(source: `bandprotocol/band-std-reference-contracts-soroban`).

We read prices via
`get_reference_data(Vec<(Symbol, Symbol)>) -> Vec<ReferenceDatum>`
on-chain. No REST polling needed. `ReferenceDatum.rate` is `u128`
and **E18-scaled**. No events emitted — we poll.

### Source

[oracles/band.md](oracles/band.md), verified against
`band-soroban/src/contract.rs` at SHA `90e22e14…aad8f`.

### Impact

None on deliverables. Simpler/safer path — on-chain signed values
instead of trusting a Cosmos-backed REST.

---

## Redstone deployment is larger than proposal listed

### Proposal says

> "Deployed price feeds include BTC, ETH, USDC, EUROC, EUROB,
> PYUSD, and others, with per-symbol Soroban contracts on mainnet."

### Correction

As of audit time there are **19 mainnet feeds**, including a
significant RWA set:

- Crypto: BTC, ETH, USDC, XLM, PYUSD
- Stables: EUROC/EUR, EUROB, MXNe
- RWA: BENJI, iBENJI, GILTS, CETES, KTB, TESOURO, USTRY
- Tokenised BTC: SolvBTC, SolvBTC/FUNDAMENTAL, SolvBTC.BBN/FUNDAMENTAL
- Inverse ETF: SPXU

Architecture is **one Adapter** (`CA526Y2N…HDXUSG`) + 19 thin
per-feed proxies. All 19 share the same WASM hash `3e464b6d…df5c`.

The Adapter emits a `["REDSTONE"]` event on every batch push
carrying a `Vec<PriceData>` — **event subscription is available**,
contrary to our original poll-only assumption.

### Source

[oracles/redstone.md](oracles/redstone.md), verified against
`redstone-finance/redstone-public-contracts` at SHA `15133304…35e0a6`.

### Impact

**Material upside** — our RWA pricing coverage is richer than we
promised. Worth highlighting in delivery review with the customer.

---

## Galexie + filesystem backend production caveat

### Proposal says

The proposal does not explicitly warn against the Filesystem
backend option.

### Correction

`go-stellar-sdk/support/datastore/filesystem.go` **silently drops
the 9 per-object metadata keys** (`start-ledger`, `end-ledger`,
`protocol-version`, `network-passphrase`, etc.) and carries an
explicit multi-process-write-unsafe warning. SDF documents this
backend as dev-only in their own config example.

Our design uses **MinIO via the S3 backend** (`endpoint_url`
override) to preserve metadata + concurrency safety. Captured as
a firm decision in [decisions.md](decisions.md).

### Source

[data-sources/galexie.md](data-sources/galexie.md), [decisions.md](decisions.md).

### Impact

None on deliverables (customer sees S3 or MinIO-flavoured S3). Our
self-hosted deploy kit explicitly documents this choice.

---

## i128 correctness invariant

### Proposal says

The proposal does not spell out i128 handling.

### Correction

Every Soroban amount (token balance, swap in/out, reserve, price)
is `i128`. Parsing it to `int64` silently truncates at ~922 billion
tokens — a real production incident at Stellar Expert was shared
with us during discovery.

We commit to:

- `NUMERIC` columns in Postgres / TimescaleDB.
- `*big.Int` or `decimal.Decimal` in Go.
- **Strings** on the JSON wire (JSON numbers are float64, precision
  loss above 2^53).

Captured as a firm, non-negotiable decision in
[decisions.md](decisions.md).

### Source

[decisions.md](decisions.md), [data-sources/withobsrvr-stellar-extract.md](data-sources/withobsrvr-stellar-extract.md).

### Impact

A correctness commitment not explicit in the proposal but implied
by the Stellar asset coverage. **Worth surfacing** to customer
stakeholders as a differentiator — competitors get this wrong.

---

## cdp-pipeline-workflow is not a forkable base

### Proposal says

The proposal does not commit to using any specific third-party
pipeline framework.

### Clarifying note

Customer reviewers may assume a withObsrvr-based approach given
the ecosystem overlap. For the record, we **verified multiple
correctness bugs** in `cdp-pipeline-workflow`
(see [data-sources/withobsrvr-cdp-pipeline-workflow.md](data-sources/withobsrvr-cdp-pipeline-workflow.md)):

- SDEX trade extractor reads the offer's *asked* price from the op
  body, not the executed fills from claim atoms.
- Soroswap router processor reads only `I128.Lo`, truncating at
  2^64 (the exact Stellar Expert-class bug).
- `CaptiveCoreInboundAdapter` has no cursor persistence — restart
  = gap.
- `SaveToTimescaleDB` uses row-by-row `INSERT`, no COPY / batching.

Our implementation instead depends on:

- `go-stellar-sdk/ingest` (SDF-authored) for ledger meta reading.
- `withObsrvr/stellar-extract` (audited correct) for typed row
  extraction.
- Our own consolidator/aggregator/serving code.

### Source

[data-sources/withobsrvr-cdp-pipeline-workflow.md](data-sources/withobsrvr-cdp-pipeline-workflow.md),
[data-sources/withobsrvr-stellar-extract.md](data-sources/withobsrvr-stellar-extract.md).

### Impact

None on deliverables. Implementation choice justification.

---

## Additional venues discovered during audit

### Proposal venue list

> "SDEX… Soroswap… Aquarius… Blend…"

### Extension

During discovery we identified and audited two additional Soroban
venues not in the proposal:

- **Phoenix DEX** — Stellar-native DeFi hub with constant-product +
  stableswap pools. Live on mainnet.
- **Comet** — Balancer-weighted AMM; live on pubnet at minimum via
  Blend's backstop pool.

Plus we noted **FxDAO, OrbitCDP, Laina, Slender, DeFindex, EquitX,
MaxFX, Hermes** — none of which are new spot trading venues; they
are oracle consumers, synthetic issuers, or perpetual futures.
Covered without per-protocol code via our existing SEP-41 + AMM
indexers.

### Source

[dexes-amms/phoenix.md](dexes-amms/phoenix.md),
[dexes-amms/comet.md](dexes-amms/comet.md),
[dexes-amms/residual-defi-protocols.md](dexes-amms/residual-defi-protocols.md).

### Impact

**Extended coverage** — if the customer asks whether we include
Phoenix / Comet, the answer is yes.

---

## Chainlink integration — HTTP cross-check only (for now)

### Proposal says

> "Stellar is part of Chainlink Scale and will be integrating
> Chainlink's Data Feeds, Data Streams, and the Cross-Chain
> Interoperability Protocol (CCIP). Once this has developed
> further we will be well positioned to extend support to cover
> this new functionality."

### Correction

Accurate as written — no Chainlink Soroban Data Feeds contracts are
live on pubnet at audit time (2026-04-22). Our Phase-1 Chainlink
integration is HTTP polling of their public Data Feeds API, used
as a divergence detector only.

### Source

[oracles/chainlink.md](oracles/chainlink.md).

### Impact

No correction — just an implementation note: Chainlink is
validation-only, not a VWAP contributor, until native Stellar Data
Feeds ship.

---

## Horizon deprecation

### Proposal implicit context

Proposal references "examples: stellar.expert, steexp.com" as
deliverable analogues. Those services historically relied on
Horizon.

### Correction (direction)

`stellar/go` monorepo (which housed Horizon's canonical code path)
was archived 2025-12-16. Horizon moved to its own dedicated repo
but the Stellar developer narrative is firmly "migrate off Horizon
to stellar-rpc + Galexie". Horizon **will not be a component of
our architecture** — captured as a firm decision in
[decisions.md](decisions.md).

### Source

[decisions.md](decisions.md) §Horizon, [data-sources/stellar-data-lakes.md](data-sources/stellar-data-lakes.md).

### Impact

None on deliverables. The "stellar.expert analogue" target is
met via Galexie-backed backfill + our own indexer + REST API.

---

## Data-freshness 30 s is served on `/v1/price/tip`, not `/v1/price`

*(registered 2026-06-12 — audit-2026-06-11 RFP-01)*

### Proposal says

> "The maximum allowed staleness for current price endpoints is 30
> seconds." (ctx-proposal.md §Data Freshness, line 329.)

### Correction

The 30-second freshness guarantee is met by the **closed-bucket
tip** endpoint `/v1/price/tip` (per ADR-0015's closed-bucket serving
contract), not the default `/v1/price`. `/v1/price` serves the most
recent **closed** aggregation bucket and can therefore be older than
30 s by design (it trades freshness for cross-region determinism —
every region serves the same closed bucket). The re-scope happened
in commit `43b640e5`; it was an internal decision and ran for ≥ 14
days as a formal freshness-target breach against the as-written
proposal before being registered here.

### Source

audit-2026-06-11 (RFP-01); [ADR-0015](../adr/0015-last-closed-bucket-rate-serving.md).

### Impact

Customer-facing if a consumer reads `/v1/price` expecting ≤ 30 s
freshness. The capability is delivered — it lives on `/v1/price/tip`.
Worth surfacing in the next customer revision so integrators point at
the right endpoint.

---

## Multi-zone / 99.99 % / read-replicas are single-host (R1) today

*(registered 2026-06-12 — audit-2026-06-11 RFP-02)*

### Proposal says

> "Multi-zone deployment is supported to eliminate single points of
> failure." (line 309.) "The target availability is 99.99 percent or
> greater." (line 335.) "Read replicas for storage systems" (line 347)
> and "Horizontal scaling of API nodes and read replicas" (line 401).

### Correction

Production today is a **single bare-metal host (R1, Hetzner
Frankfurt)** per [ADR-0008](../adr/0008-ha-topology.md). There is no
multi-zone deployment, no read-replica Postgres, and no
multi-instance API tier live. R2/R3 are designed (ADR-0016) but
deferred — adding them is mechanical but not done. The 99.99 %
availability number is an aspirational target, not a measured-or-
architecturally-guaranteed property on a single host. The
[ha-plan.md](../architecture/ha-plan.md) describes the *target*
multi-region HA topology; it is not the current deployment.

### Source

audit-2026-06-11 (RFP-02); [ADR-0008](../adr/0008-ha-topology.md),
[ADR-0016](../adr/0016-per-region-storage-strategy.md),
[r1-deployment-state.md](../operations/r1-deployment-state.md).

### Impact

Material if the customer relies on the 99.99 % / multi-zone language
as a present-tense guarantee. We are in the live-in-development phase
with no consumer traffic yet; the HA topology is sequenced, not
shipped. Should be stated plainly in the next customer revision.

---

## "Self-hosted RPC nodes" is Galexie + captive-core, not stellar-rpc

*(registered 2026-06-12 — audit-2026-06-11 RFP-04)*

### Proposal says

> "All Soroban event indexing uses self-hosted RPC nodes rather than
> third-party providers, with the same multi-instance redundancy
> applied to the Classic DEX ingestion path." (line 87.)

### Correction

We do not run stellar-rpc in production ingest. Soroban (and classic)
event indexing reads **Galexie's `LedgerCloseMeta` output from MinIO**
(`go-stellar-sdk/ingest.ApplyLedgerMetadata`), where Galexie spawns a
**captive-core** subprocess. stellar-rpc was removed from R1 on
2026-04-23 and now exists only for the `rpc-probe` operator
diagnostic + fixture capture (invariant 6,
[ingest-pipeline.md](../architecture/ingest-pipeline.md)). The spirit
of the claim holds — the data path is self-hosted, not a third-party
RPC provider — but "RPC nodes" is the wrong mechanism.

### Source

audit-2026-06-11 (RFP-04); [ADR-0001](../adr/0001-horizon-deprecated.md),
[ADR-0002](../adr/0002-minio-s3-compat-storage.md),
[r1-deployment-state.md](../operations/r1-deployment-state.md).

### Impact

None on deliverables — self-hosted data path is preserved. Wording
correction only; update "self-hosted RPC nodes" → "self-hosted
Galexie + captive-core ingestion" in the next revision.

---

## "Containerized + load-balanced" is bare-metal systemd (ADR-0008)

*(registered 2026-06-12 — audit-2026-06-11 RFP-05)*

### Proposal says

> "All services are containerized and deployed behind load-balanced
> entry points." (line 285.) "API servers are deployed behind load
> balancers and can scale horizontally." (line 485.) Plus the §11
> footnote describing "a huge kubernetes stack with Talos Linux."

### Correction

Production R1 runs the binaries as **systemd-managed services on a
single bare-metal host** (per [ADR-0008](../adr/0008-ha-topology.md))
— there is no container orchestrator, no Kubernetes, and no
multi-instance load balancer in front of the API. A Caddy reverse
proxy terminates TLS, but it fronts a single API process, not a
load-balanced pool. The release pipeline ships **binaries**, not
container images (the GHCR job was dropped — F-1221); the
`docker/*.Dockerfile` files remain only for self-hosters who want to
build their own images. The self-hosted-deployment templates
(Docker Compose) the proposal references are real and shipped; they
just describe the *self-host* path, not how R1 runs.

### Source

audit-2026-06-11 (RFP-05); [ADR-0008](../adr/0008-ha-topology.md);
release-process.md §Cut.

### Impact

None on deliverables — the API serves identically. Deployment-shape
wording correction; the customer-facing claim should match the
bare-metal systemd reality on R1 while keeping the containerized
self-host kit as the templated option.

---

## Apply to proposal on next revision

When we do the next customer-facing revision (e.g. ahead of
Phase 2 delivery review), fold every item above into the relevant
proposal section:

| Proposal section | Corrections applied |
| ---------------- | ------------------- |
| Oracle Networks → Reflector | §1, §2 |
| Soroban DEXs and AMMs → Soroswap | §3 |
| Oracle Networks → Band | §4 |
| Oracle Networks → Redstone | §5 |
| Data Ingestion → SDEX / Stellar Classic DEX | §11 (phoenix + comet additions) |
| Data Processing → Canonical Data Model | §7 (i128 invariant) |
| Open Source & Deployment Model | §6 (MinIO / datastore backend), self-hosted-RPC → Galexie, containerized → bare-metal systemd |
| Oracle Networks → Chainlink | §9 |
| Performance & SLA → Data Freshness | 30 s served on `/v1/price/tip` |
| Architecture & Scalability → Availability | multi-zone / 99.99 % / read-replicas are single-host (R1) today |
| Ingestion → Soroban DEX/AMM | "self-hosted RPC nodes" → Galexie + captive-core |
| *(new section)* Coverage extensions | Phoenix, Comet, RWA via Redstone |
