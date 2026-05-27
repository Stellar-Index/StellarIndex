# Audit Plan — 2026-05-26

## Objective

Execute a maximally granular, **cold**, **adversarial** audit of
the current repository snapshot **and** the live R1 deployment,
with explicit coverage of:

- every tracked file (2104 in this snapshot)
- every material cross-file interaction
- every runtime binary (6 binaries under `cmd/*`) and operator path
- every claimed invariant (ADR-0001 through ADR-0029)
- every API route, response envelope, OpenAPI declaration, and
  removed-route hygiene
- every SQL migration (45 ups + 45 downs) and the resulting
  Timescale schema, including the soroban_events landing zone +
  four decoder backfill tables
- every Redis key contract, prewarmer, and cache-miss path
- every external source (CEX/FX/aggregator/oracle) adapter,
  including the new `chainlink` adapter
- every on-chain decoder (Soroban + classic) including the five
  new sources (`cctp`, `defindex`, `rozo`, `sorobanevents`,
  `soroswap_router`) and the six new per-source backfill
  subcommands
- every Prometheus rule, Alertmanager route, runbook, and SLA
  probe contract
- every CI workflow, lint script, and release control
- every web frontend (`web/explorer`, `web/dashboard`, `web/status`)
- every config knob, env var, and feature flag
- every code-to-doc, code-to-test, code-to-RFP, code-to-proposal,
  code-to-memory contract
- every gap relative to CoinGecko / CoinMarketCap parity
- every Stellar-specific surface that CG/CMC cannot match

This audit is **cold**: prior audits (04-29, 05-02, 05-12) and
CLAUDE.md identify areas to inspect, but **no claim is accepted**
without live code, live test, or live R1 evidence in **this audit's**
evidence log. Closed prior findings are re-opened in scope and
re-tested.

This audit is **adversarial**. The frame is: "if someone wanted to
break this system, mis-trust this data, exfiltrate keys, exhaust
the budget, drown the API, or embarrass the brand on launch day,
how would they do it?" See [10-attack-tree.md](10-attack-tree.md).

## Why now

- **Live on R1, no public traffic yet.** This is the last cheap
  window to find correctness, security, and contract bugs before
  the public flip.
- **Substantial post-baseline delta.** Since 2026-05-12 the
  codebase added 17 migrations, 5 source packages, 1 external
  adapter, 3 ADRs, 6 backfill subcommands, the soroban_events
  raw-event landing zone (ADR-0029), and cross-cutting
  correctness changes (back-pressure cursor coherence;
  trailing-edge tolerance; verify-archive lifecycle; fd-2 wrap
  drain).
- **Operational lessons cost data.** The 2026-05-26 fill walk lost
  ~18.86M rows of ~4.66B (~0.40%) before the back-pressure fix
  shipped. The 2026-05-17 ZFS pool exhaustion incident exposed
  storage planning gaps. These are now design lessons; the audit
  re-verifies the fixes hold under stress.
- **CG/CMC parity is the floor; Stellar depth is the moat.**
  Parity is not optional. Where parity is impossible, the gap
  must be either closed via fallback (federated metadata) or
  *documented* as a product-positioning choice — never silently
  absent. The Stellar-specific edge (DEX/AMM trades, oracle feeds,
  supply derivation, SEP-1 overlays, SEP-40 compatibility,
  classic + Soroban unification, soroban_events as a queryable
  raw-event lake) must be deeper than CG/CMC's because that is
  the only durable differentiator.

## Workstream Catalogue

Each workstream has its own sub-plan under [workstreams/](workstreams/).
Workstreams W01..W26 are the 2026-05-12 baseline taxonomy,
re-scoped with all post-baseline files folded in. W27..W35 are
new and address the post-baseline delta.

### Baseline (W01..W26 — re-scoped)

- **[W01](workstreams/W01-snapshot-governance-hygiene.md)** —
  Snapshot, governance, repo hygiene. New artifacts to inspect:
  the audit-2026-05-12 directory itself (does it claim things
  that are no longer true?), the audit-2026-04-29 directory,
  CLAUDE.md drift since 2026-05-12, new memory files
  (`feedback_fd2_wrap_drain_on_exit`, `feedback_cold_tier_premature_enable`,
  `project_every_event_principle`, etc.), 53 commits since
  baseline.
- **[W02](workstreams/W02-architecture-adrs.md)** — Architecture,
  ADRs, negative space. Now ADR-0001..0029 (was ADR-0001..0026).
  ADR-0027 (cold-tier), ADR-0028 (RWA), ADR-0029 (soroban_events
  landing zone) are new. Verify each invariant is honoured by
  current code.
- **[W03](workstreams/W03-build-ci-release.md)** — Build,
  reproducibility, CI/CD, release controls. Note: 2026-05-26
  GitHub Actions incident broke `actions/setup-go@<sha>` for
  three release.yml runs; audit our pin choices.
- **[W04](workstreams/W04-dependency-provenance.md)** —
  Dependency, provenance, supply chain. New deps: any
  introduced by chainlink adapter, sorobanevents writer, etc.
- **[W05](workstreams/W05-canonical-numeric-serialization.md)** —
  Canonical identity, numeric safety, serialization. New
  surface: `internal/sources/sorobanevents.{Capture,Reconstruct}`
  round-trips XDR; `internal/scval.{EncodeArgsAsScVec,DecodeScVecToArgs}`
  inverses.
- **[W06](workstreams/W06-ingest-transport.md)** — Ingest
  transport, dispatcher, persistence pipeline. New seams:
  `Dispatcher.SetRawEventSink` (ADR-0029); back-pressure
  semantics on `sorobanevents.AsyncSink.PushEvent` (rc.80);
  `ledgerstream.TolerateTrailingMissing` (rc.81); ctx-cancel
  early-stop watchdogs in backfill driver + live indexer.
- **[W07](workstreams/W07-onchain-source-decoders.md)** —
  On-chain source decoders. Now covers 23 sources (was 15):
  added `cctp`, `defindex`, `rozo`, `sorobanevents`,
  `soroswap_router`. Note: the "every event" coverage gate is
  W35 — W07 audits decoder correctness per-source; W35 audits
  coverage completeness across sources.
- **[W08](workstreams/W08-external-source-fleet.md)** —
  External source fleet + policy. Adds `chainlink` as a
  divergence reference (off-chain HTTP, not on-chain).
- **[W09](workstreams/W09-storage-schema-cache-migrations.md)** —
  Storage, schema, cache, migrations. Now covers
  migrations 0001..0045 (was 0001..0028). The four decoder
  tables (0042 comet_liquidity, 0043 soroswap_skim_events,
  0044 phoenix_liquidity + phoenix_stake_events, 0045
  blend_positions + blend_emissions + blend_admin) are new
  audit surface.
- **[W10](workstreams/W10-aggregation-divergence-anomaly.md)** —
  Aggregation, divergence, freeze, confidence, anomaly. Adds
  the chainlink reference path; verifies the depeg test
  (`internal/divergence/depeg_test.go`).
- **[W11](workstreams/W11-api-runtime-streaming-auth.md)** —
  API runtime, contracts, streaming, auth. Now covers 50
  handler files (was ~60 — some routes were removed; verify the
  removals are clean).
- **[W12](workstreams/W12-supply-metadata-asset-detail.md)** —
  Supply, metadata, asset detail enrichment.
- **[W13](workstreams/W13-operator-tooling-archive.md)** —
  Operator tooling. Adds the six per-source backfill subcommands
  (audited for correctness in W29) and the new diagnostic paths
  (`scan-soroban-events`, `verify-decoders`,
  `extract-wasm-from-galexie`, `wasm-history-merge-jsonl`).
- **[W14](workstreams/W14-observability-metrics-alerts.md)** —
  Observability, metrics, alerts, SLA.
- **[W15](workstreams/W15-tests-and-ci-reality.md)** — Tests,
  CI reality, regression confidence. Note: 28 integration test
  files now (was 19 at baseline).
- **[W16](workstreams/W16-documentation-truth.md)** —
  Documentation truth, RFP/proposal/ADR alignment. The
  prior-audit residue (audit-2026-04-29, audit-2026-05-02,
  audit-2026-05-12) is itself documentation to be reconciled
  against current code.
- **[W17](workstreams/W17-web-frontends-explorer-dashboard-status.md)** —
  Web frontends.
- **[W18](workstreams/W18-deployment-infrastructure.md)** —
  Deployment, infrastructure, ansible roles.
- **[W19](workstreams/W19-security-secrets-billing.md)** —
  Security, secrets, auth, billing. New surface: Stripe webhook
  handler (`internal/api/v1/stripe_webhook.go`); customer
  webhook fanout (`internal/customerwebhook/`).
- **[W20](workstreams/W20-cgcmc-parity-execution.md)** — CG/CMC
  parity (execution against the matrix in
  [08-cgcmc-parity-matrix.md](08-cgcmc-parity-matrix.md)).
- **[W21](workstreams/W21-r1-live-state-execution.md)** — R1
  live state vs claimed state. **Immediate finding seed**: r1's
  root partition (`/dev/md1` 49G) is 100% full at audit start
  time (2026-05-26 23:14 UTC) — see W21 + R1-### probe and
  cross-check whether any alert covers root-disk exhaustion.
- **[W22](workstreams/W22-launch-readiness-public-flip.md)** —
  Launch readiness, public-flip.
- **[W23](workstreams/W23-multi-region-determinism.md)** —
  Multi-region determinism (R2, R3) vs ADR-0015/0016.
- **[W24](workstreams/W24-contract-schema-evolution-wasm-history.md)**
  — Contract schema evolution + WASM history. Adds the new
  CCTP/Rozo audits + the migration-0017 wasm_history table
  population workflow.
- **[W25](workstreams/W25-generated-artifacts-and-drift.md)** —
  Generated artifacts + drift.
- **[W26](workstreams/W26-cross-file-interactions.md)** —
  Cross-file interactions and system coupling (audit-blocking
  gate). Closure now requires all W01..W35 terminal, not just
  W01..W25.

### New (W27..W35)

- **[W27](workstreams/W27-soroban-events-landing-zone.md)** —
  Soroban events landing zone (ADR-0029). The entire raw-events
  architecture: `Dispatcher.SetRawEventSink` hook,
  `sorobanevents.{Row, Capture, Reconstruct, AsyncSink}`,
  migration 0041 (soroban_events hypertable, PK including
  ledger_close_time per TS103), `Store.{InsertSorobanEventsBatch,
  StreamSorobanEvents, CountSorobanEventsInRange}`, `scan-soroban-events`
  diagnostic, the 2026-05-26 fill walk (drop incident + re-walk),
  the SQL-as-historical-backfill design that replaces MinIO walks.
- **[W28](workstreams/W28-backpressure-cursor-coherence.md)** —
  Back-pressure / ctx-shutdown semantics. Cursor-coherence
  guarantee: producer cursor must not advance past durable
  writes. AsyncSink lifecycle (Start → Push → Stop) including
  the shutdown-race drop semantics. ctx-cancel early-stop
  watchdogs in `cmd/ratesengine-ops/backfill.go` and
  `cmd/ratesengine-indexer/main.go`.
  `ledgerstream.TolerateTrailingMissing` with delivery caveat
  (SDK BufferedStorageBackend cancels its context on missing
  file, dropping pre-fetched ledgers — operators must clamp
  `-to` for 100% coverage).
- **[W29](workstreams/W29-per-source-backfills.md)** —
  Per-source backfill subcommands. Six commands:
  `cctp-backfill`, `rozo-backfill`, `soroswap-skim-backfill`,
  `comet-liquidity-backfill`, `phoenix-backfill`,
  `blend-backfill`. Audit: contract+topic filter correctness,
  decoder reuse, idempotency, error handling, dry-run pattern,
  reconstruct correctness, output count expectations.
- **[W30](workstreams/W30-cold-tier-read-path.md)** — Cold-tier
  read path (ADR-0027). `ledgerstream.TieredDataStore`,
  `internal/pipeline/datastore.LedgerstreamConfig` cold-tier
  opt-in, fallback semantics, trim safety, multi-source
  determinism, the §3+§4-together rule (per
  `feedback_cold_tier_premature_enable`).
- **[W31](workstreams/W31-rwa-asset-representation.md)** — RWA
  asset representation (ADR-0028). `canonical.AssetRWA`, the
  classification taxonomy, where it surfaces in API, what's not
  yet covered.
- **[W32](workstreams/W32-customer-webhook-fanout.md)** —
  Customer webhook fanout. `internal/customerwebhook/`: fanout
  semantics, retry policy, SSRF defence (`ssrf.go`), worker
  lifecycle, billing intersection.
- **[W33](workstreams/W33-stripe-billing-integration.md)** —
  Stripe billing integration. `internal/api/v1/stripe_webhook.go`:
  webhook signature verification, idempotency, abuse vectors,
  charge state machine, refund handling.
- **[W34](workstreams/W34-verify-archive-lifecycle.md)** —
  Verify-archive Type=notify lifecycle. Per-chunk resume across
  restarts, WatchdogSec replacing TimeoutStartSec, state-file
  persistence semantics, the 2026-05-25 trailing-edge bug
  (project_62_diagnosis_2026_05_25) and its rc.81 fix
  (TolerateTrailingMissing in walker config).
- **[W35](workstreams/W35-every-event-coverage.md)** —
  Granular-coverage mission audit. For every Soroban source,
  enumerate every event the contract emits on-chain and verify
  the decoder's `classify()` claims it. The known gaps closed
  in rc.80 (Comet 4-of-5 → 5-of-5, Phoenix swap-only → +4
  actions, Soroswap 4-of-5 → 5-of-5, Blend 3-of-21 → 21-of-21)
  are re-verified cold against the actual on-chain event set,
  not against the PR description.

## Mandatory Passes

- **Top-down architecture pass.** Re-derive the system from
  `cmd/*` entry points; map every reachable function call.
- **Bottom-up file pass.** Every tracked file gets a terminal
  status in `inventory/file-coverage.tsv`.
- **End-to-end user journey pass.** Execute every journey in
  [03-journeys.md](03-journeys.md). Record traces in
  [journeys-traces/](journeys-traces/).
- **End-to-end operator journey pass.** Execute the operator
  subset of journeys.
- **Hostile-environment pass.** Execute the adversarial vectors
  in [10-attack-tree.md](10-attack-tree.md).
- **Live-runtime pass.** Execute the R1 probe protocol from
  [12-r1-live-probe-protocol.md](12-r1-live-probe-protocol.md).
  Cover: services, timers, listening ports, disk usage (note
  the 100% root partition observed at audit start), Postgres,
  Redis, MinIO, Galexie, stellar-core captive process, Caddy,
  TLS cert expiry.
- **Documentation-truth pass.** Reconcile docs against code
  per [04-reconciliation.md](04-reconciliation.md) R01..Rn.
- **Negative-space pass.** Identify what we *don't* test, don't
  alert on, don't monitor, and don't document.
- **CG/CMC parity pass.** Execute the matrix in
  [08-cgcmc-parity-matrix.md](08-cgcmc-parity-matrix.md).
- **Stellar-depth pass.** Execute the matrix in
  [09-stellar-coverage-matrix.md](09-stellar-coverage-matrix.md).
- **Prior-audit delta pass.** Re-test every closed finding in
  04-29, 05-02, and 05-12 cold; record whether the closure is
  real.
- **Recent-change pass.** Re-audit every commit from rc.71
  (the prior baseline) through rc.81 individually; deleted
  routes, schema additions, new sources, new ADRs, new
  subcommands.
- **Memory-truth pass.** Every entry in
  `~/.claude/projects/-Users-ash-code-ratesengine/memory/`
  is a claim about prior incidents or operator preferences.
  Verify each is still true; obsolete entries become findings.
- **Granular-coverage pass.** For each Soroban source, count
  every emitted event on-chain vs decoder coverage. Gaps are
  W35 findings; full coverage is W35 evidence.

## Required Deliverables

- complete tracker state ([01-tracker.md](01-tracker.md))
- complete file inventory with terminal statuses
  ([inventory/file-coverage.tsv](inventory/file-coverage.tsv))
- per-workstream sub-plans completed ([workstreams/](workstreams/))
- evidence log ([evidence/log.md](evidence/log.md))
- cross-file interaction ledger
  ([evidence/cross-file-interactions.md](evidence/cross-file-interactions.md))
- R1 probe transcripts ([evidence/r1-probes/](evidence/r1-probes/))
- journey traces ([journeys-traces/](journeys-traces/))
- findings register ([05-findings-register.md](05-findings-register.md))
- exclusions register ([06-exclusions-register.md](06-exclusions-register.md))
- remediation plan tied to findings
  ([07-remediation-plan.md](07-remediation-plan.md))
- CG/CMC parity matrix completed
  ([08-cgcmc-parity-matrix.md](08-cgcmc-parity-matrix.md))
- Stellar coverage matrix completed
  ([09-stellar-coverage-matrix.md](09-stellar-coverage-matrix.md))
- attack-tree outcomes recorded ([10-attack-tree.md](10-attack-tree.md))
- granular-coverage register (W35 output)

## Closure Criteria

The audit is complete only when:

- every workstream W01..W35 has terminal status in the tracker
- every mandatory pass is terminal
- `inventory/file-coverage.tsv` has no `todo`
- every finding has evidence references and a disposition
- every exclusion is explicit and re-entry-evidence-listed
- the remediation plan maps to every open finding
- the CG/CMC parity matrix is fully filled (no blank rows)
- the Stellar coverage matrix is fully filled
- the granular-coverage register has one row per (source,
  on-chain-event) tuple
- at least one full R1 probe transcript is in
  `evidence/r1-probes/`
- W26 confirms every required cross-file interaction class has
  at least one fully-traced `XFI-####` row
