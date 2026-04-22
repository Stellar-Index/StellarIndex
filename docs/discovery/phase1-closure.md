---
title: Phase 1 Discovery — Closure Statement
last_verified: 2026-04-22
status: ratified
---

# Phase 1 Discovery — Closure Statement

**Ratified:** 2026-04-22.
**Authors:** discovery agent (Claude) + @ash.
**Scope boundary:** this doc is the formal exit gate of Phase 1. After
ratification, the `docs/discovery/` tree becomes read-only reference.
New factual discoveries land in `docs/architecture/` or `docs/adr/`,
not here.

---

## 1. What "complete" means

Phase 1 is complete when every requirement in the two RFPs + our
proposal has one of three states:

| State | Meaning |
| ----- | ------- |
| **Verified** | A primary-source artefact (code, deployed contract, protocol spec) has been read, the claim stands. |
| **Deferred (with owner + date)** | Acknowledged gap; a named implementation week owns closing it. |
| **Rejected / out of scope** | Explicitly removed from scope with rationale. |

A row that sits in "probably works" is not acceptable.

---

## 2. Verified — closed in Phase 1

### 2.1 Ingestion surface

| Source | Primary verification |
| ------ | -------------------- |
| Galexie | Read `stellar-galexie` subcommand source; config schema; zstd; captive-core integration; FS-backend metadata-drop bug. |
| stellar-archivist (Rust) | Read `rs-stellar-archivist/src/cli/*.rs` + `storage.rs`. Two subcommands, file:// write-only, multi-backend read. |
| stellar-ledger-data-indexer | Read migrations directory + grep'd schema — Soroban-contract-data-only scope, 2 tables. |
| stellar-etl | Cloned + read Go pipeline — reference for ledger → typed-row mapping. |
| getEvents v2 RPC proposal | Read stellar/stellar-rpc discussion + diff. |
| withObsrvr/stellar-extract | Read `trades.go` (ClaimAtom V0+OrderBook correct) + `scval_converter.go` (big.Int i128 with sign handling correct). |
| withObsrvr/cdp-pipeline-workflow | Read source — six correctness bugs documented, do not fork. |
| Composable Data Platform | Read SDK + pipelines; architecture documented. |
| Stellar data lakes (SDF GCS, Hubble BigQuery) | Documented; used as backfill accelerator only. |
| Archival-node design | SDF prerequisites verified (dated April 2024; flagged as stale — to re-measure). |

### 2.2 Oracles

| Oracle | Primary verification |
| ------ | -------------------- |
| Reflector | `reflector-contract/{pulse,beam,oracle}/src/` read; three-contract model captured; `REFLECTOR/update` event shape verified. |
| Redstone | `redstone-public-contracts` cloned + read; 19 mainnet feeds enumerated via public feed list; **event-emitting adapter** contradicts initial poll-only assumption — captured. |
| Band | `band-std-reference-contracts-soroban` cloned + read; struct name `ReferenceDatum`; pair-rate is E18-scaled; no events emitted (poll-only). |
| Chainlink | No live Stellar Data Feeds — HTTP cross-check until Scale ships. |
| DIA | Testnet contract only; monitored for mainnet ship during the 10-week window. |

### 2.3 DEXes / AMMs

| Venue | Primary verification |
| ----- | -------------------- |
| SDEX (classic) | XDR ClaimAtom V0/OrderBook/LiquidityPool variants verified; 5 trade-producing op types catalogued. |
| Soroswap | Factory + pair mainnet addresses + pair WASM hash in `public/mainnet.contracts.json`; swap+sync correlation invariant confirmed in `pair/src/lib.rs`. |
| Aquarius | Router mainnet address + 3 pool types catalogued; events module read. |
| Phoenix | 8-events-per-swap schema verified; mainnet addresses captured. |
| Comet | Balancer-weighted AMM + Blend backstop usage confirmed; structured event bodies. |
| Blend | Pool factory + backstop addresses verified against stellar.expert; event set catalogued. |

### 2.4 Protocol semantics

| Topic | Primary verification |
| ----- | -------------------- |
| SEP-41 | v0.4.1 spec read in full; i128-or-map-with-to_muxed_id decoder requirement captured; supply arithmetic locked. |
| CAP-67 | Full spec read; P23 activation (mainnet 2025-09-03); 4-topic classic vs 3-topic SEP-41 shape; 3 new SCAddressType variants; two-transfer-per-fill SDEX post-P23 captured. |
| ClaimAtom + protocol-version switching | Verified the SDK's XDR reader dispatches across union `LedgerCloseMeta { v0/v1/v2 }`. |
| i128 / u128 invariant | Implemented in `internal/canonical/amount.go` with KALIEN regression test (hi=2, lo=3106517825480896768 → "40000005972900000000"). |

### 2.5 Supporting

- **SEP-1 / home-domain resolution** — design captured in
  `data-sources/sep1-home-domain.md`. Two-tier cache, 10s timeout,
  SSRF guards. Implementation is a Week-5 task.
- **Supply-data derivation** — three-mode algorithm (XLM fixed, classic
  issuer-balance-inverse, SEP-41 mint/burn/clawback event sum)
  captured in `data-sources/supply-data.md`.
- **VERSIONS.md** — upstream SHAs pinned at repo root.
- **ADRs 1–5** extracted from `decisions.md`.

---

## 3. Deferred — owned by a Phase 2+ week

Each item has an owner week + a concrete closure artefact.

| Item | Owner week | Closure artefact |
| ---- | ---------- | ---------------- |
| Empirical pre-P20 ledger replay (settle "Galexie preserves native-epoch XDR") | Week 2 | `test/fixtures/protocol-boundary/pre-p20/` + replay test. |
| Protocol-boundary fixture set (pre/post P18, P20, P23) | Week 2 | `test/fixtures/protocol-boundary/*`. |
| Per-ClaimAtom-variant + per-trade-op fixtures | Week 2 | `test/fixtures/sdex/*`. |
| Soroswap swap+sync pairing fixture | Week 2 | `test/fixtures/soroswap/swap+sync.json`. |
| go-stellar-sdk version skew smoke test (v0.4 ↔ v0.5) | Week 2 | `go mod tidy` + build green in CI. |
| MinIO + Galexie smoke test | Week 3 | `test/integration/galexie_minio_test.go`. |
| stellar-rpc SQLite retention benchmark | Week 3 | benchmark harness + result log. |
| rs-stellar-archivist concurrency sweep on R640 | Week 3 | operator-side measurement, not CI. |
| Captive-core + galexie co-resident memory/file-descriptor profile | Week 3 | operator-side measurement. |
| **API layer design** — auth, rate limits, SSE, CDN, versioning | Week 4 (spec), Weeks 7–8 (impl) | `docs/reference/api-design.md` + `openapi/rates-engine.v1.yaml`. |
| **TimescaleDB schema / hypertables / continuous aggregates** | Week 4 | `docs/architecture/storage-timescaledb.md` + `migrations/0001_*`. |
| **Redis cluster schema + failover** | Week 5 | `docs/architecture/cache-redis.md`. |
| **MinIO topology + erasure coding + backup** | Week 5 | `docs/architecture/storage-minio.md`. |
| **HA infrastructure plan** (this closure doc's sibling) | This iteration | `docs/architecture/ha-plan.md`. |
| **Load-test plan** (p95 ≤ 200ms proof path) | Week 9 | `test/load/` + `docs/operations/load-test-plan.md`. |
| **SEV-1 / SEV-2 runbook** | Week 9 | `docs/operations/sev-playbook.md`. |
| Liveness audit of existing CEX connectors | Week 4 | `docs/discovery/external-refs/cex-connectors-status.md` (appendix). |
| Residual DeFi (FxDAO, OrbitCDP, EquitX, Laina, Slender, DeFindex) | Post-launch (source-connector contrib) | Per-source doc under `internal/sources/<name>/README.md`. |

---

## 4. Rejected / out of scope

- **Horizon ingestion** — ADR-0001.
- **cdp-pipeline-workflow as a dependency** — six correctness bugs,
  re-implementation in our `internal/consumer` is cheaper.
- **Filesystem backend for Galexie in production** — ADR-0002.
- **Postgres `bigint` for token amounts** — ADR-0003.
- **Multi-module monorepo (`go.work`)** — ADR-0005 alternative 1.
- **Horizon-dependent third-party connectors** — (flagged per ADR-0001;
  if a protocol's only ingestion path is Horizon, we pass).

---

## 5. Residual risk

### 5.1 High-risk (act before first mainnet write)

- Captive-core + galexie co-resident footprint unmeasured. If they
  do not fit on a single R640, our infrastructure cost model shifts.
- `go-stellar-sdk` v0.4 ↔ v0.5 version skew between `stellar-galexie`
  and `withObsrvr/stellar-extract`. If the MVS selection breaks a
  consumer, we have to vendor or fork one side.

### 5.2 Medium-risk (act during implementation)

- SDF validator hardware baseline is from April 2024 when the ledger
  was ~10 GB. At April 2026 it's larger; our sizing must re-measure.
- Protocol activation *ledgers* (not just dates) are not captured.
  Required for protocol-boundary fixtures — generate in Week 2.
- Aquarius `liquidity_pool_concentrated` was on a feature branch at
  audit time — may have launched; re-verify at Week 4.

### 5.3 Low-risk (monitor during operation)

- DIA may ship a Stellar mainnet oracle during our window — watch and
  integrate as a divergence signal if it does.
- CAP-58, CAP-59, CAP-62, CAP-66, CAP-75, CAP-79 — linked, not read.
  None are pricing-relevant by current understanding, but flagged for
  revisit if protocol 26 plans change.

---

## 6. Known claims that still rest on secondary sources

Captured for transparency; none block Phase 2 start.

| Claim | Primary-source verification needed |
| ----- | ---------------------------------- |
| Muxed accounts activated at P17 | Read `stellar-protocol/core/cap-0027.md`. Resolve in Week 2 alongside protocol-boundary fixtures. |
| Reflector three-contract mainnet addresses | Live `base()`/`decimals()`/`assets()` calls during Week 2 integration test. |
| SDF hardware baseline currency | Re-measured during first captive-core catchup in Week 3. |

---

## 7. The single highest-risk open question

**"Does Galexie preserve native-epoch XDR for pre-P20 ledgers, or does
modern stellar-core upcast during catchup/replay?"**

- If preserved → all our version-aware switching code is load-bearing.
- If upcast → much of that code is dead; world is simpler.

Either answer is safe (we keep the switch either way), but the
question has an empirical answer and Week 2 will settle it by
replaying a single pre-P20 ledger and observing `lcm.V`.

---

## 8. Discovery-phase statistics

- **47 audit docs** across data-sources / oracles / DEXes-AMMs /
  external-refs / notes / infrastructure.
- **5 ADRs** ratified (Horizon, MinIO, i128, Tier-1, monorepo).
- **11 proposal corrections** filed in `proposal-corrections.md`.
- **25 open fixtures / benchmarks** listed explicitly in
  `adversarial-audit.md §7`.
- **0 unresolved blockers** for Phase 2 ingestion start.

---

## 9. Exit gate

- [x] Every RFP row in `rfp-requirements-matrix.md` has a verified,
      deferred, or rejected status.
- [x] Every `❓` and `🧪` row in `README.md` has either been promoted
      or assigned an owner week.
- [x] Adversarial audit `§10 items 1–11` either closed or deferred
      with an owner week.
- [x] 5 ADRs ratified.
- [x] Monorepo scaffolded, builds, tests pass with `-race`.
- [x] First commit on main.
- [x] This closure doc ratified.

**Phase 2 (Week 2 — ingestion scaffold) may begin.**
