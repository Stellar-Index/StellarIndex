# Remediation Plan

This is the post-audit remediation plan for the 2026-04-29 cold
adversarial audit snapshot.

Execution status for this snapshot: complete as of 2026-04-30.
See `05-findings-register.md` and `evidence/log.md` for closure
evidence and final verification.

It is intentionally execution-oriented:

- grouped by remediation wave, not by audit section
- tied directly to finding IDs
- biased toward reducing correctness and false-green risk first

## Triage Model

Remediation order:

1. stop live corruption / silent data-loss / panic paths
2. restore control-plane truthfulness where monitoring is false-green
3. repair user-visible contract drift
4. close deploy/docs/release drift that can recreate the same class of defect

## Wave 0: Immediate Safety Fixes

Target: same day / first patch train.

### R-0001: Stop cursor advancement on dispatcher failure

- Findings: `F-0022`
- Goal: a dispatcher rejection must fail the ledger callback and block cursor advancement in both live-tail and backfill paths.
- Primary surfaces:
  - `internal/pipeline/processor.go`
  - `cmd/ratesengine-indexer/main.go`
  - `cmd/ratesengine-ops/backfill.go`
- Acceptance:
  - dispatcher error returns non-nil to the caller
  - live indexer does not upsert cursor for rejected ledgers
  - backfill does not mark a rejected ledger complete
  - regression tests cover both live and replay callers

### R-0002: Fix live cursor metric cardinality

- Findings: `F-0025`
- Goal: remove the panic-class metric write defect in the current indexer path.
- Primary surfaces:
  - `cmd/ratesengine-indexer/main.go`
  - `internal/obs/metrics.go`
- Acceptance:
  - all cursor-gauge writes use the declared label set
  - runtime path is exercised by a test

### R-0003: Remove filesystem backend from production Ansible path

- Findings: `F-0031`
- Goal: make it impossible for the archival-node role to render a production Galexie filesystem datastore.
- Primary surfaces:
  - `configs/ansible/roles/archival-node/defaults/main.yml`
  - `configs/ansible/roles/archival-node/templates/galexie.toml.j2`
  - `configs/ansible/README.md`
- Acceptance:
  - no `filesystem` option remains in the production role
  - docs explicitly say S3-compatible only
  - any dev-only filesystem use is isolated to local dev surfaces

## Wave 1: Restore Monitoring Truth

Target: immediately after Wave 0.

### R-0004: Wire decode-error and orphan-event metrics into the live ingest path

- Findings: `F-0023`
- Goal: documented ingest alerts must observe real production emissions.
- Primary surfaces:
  - `cmd/ratesengine-indexer/main.go`
  - dispatcher/source adapter boundaries
  - `internal/obs/metrics.go`
- Acceptance:
  - decode failures increment `ratesengine_source_decode_errors_total`
  - orphan events increment `ratesengine_source_orphan_events_total`
  - alert/runbook examples match actual labels
  - tests cover emission on representative source failures

### R-0005: Rebuild source-health metrics around the live architecture

- Findings: `F-0026`
- Goal: source-health alerts must be driven by the active `ledgerstream -> dispatcher` indexer, not legacy orchestrator-only gauges.
- Primary surfaces:
  - `cmd/ratesengine-indexer/main.go`
  - `internal/obs/metrics.go`
  - `deploy/monitoring/rules/ingestion.yml`
  - `docs/operations/runbooks/source-stopped.md`
  - `docs/operations/runbooks/ingestion-lag.md`
- Acceptance:
  - either new live gauges/counters exist for enabled-source health, or the alert model is rewritten around metrics the live binary already emits
  - legacy-only assumptions are removed from docs and alerts

### R-0006: Expose discovery-drop loss live

- Findings: `F-0024`
- Goal: discovery drop pressure must be visible before shutdown.
- Primary surfaces:
  - `internal/canonical/discovery/sink.go`
  - `cmd/ratesengine-indexer/main.go`
  - `internal/obs/metrics.go`
  - `docs/reference/metrics/README.md`
- Acceptance:
  - live counter/gauge for dropped discovery hits
  - metrics doc entry exists
  - alert or explicit operator query path exists

### R-0007: Align monitoring-validation docs with real enforcement

- Findings: `F-0030`
- Goal: monitoring docs must either describe the current weaker state accurately or the missing `promtool` CI/test path must be added.
- Decision:
  - preferred: add real rule validation/test automation
  - fallback: reduce docs to the enforcement that actually exists
- Primary surfaces:
  - `deploy/monitoring/README.md`
  - `.github/workflows/ci.yml`
  - `test/monitoring/` if added
- Acceptance:
  - docs and CI say the same thing

## Wave 2: Data Integrity and Cross-Region Correctness

Target: next hardening train after observability truth is restored.

### R-0008: Reconcile trade identity with storage dedupe

- Findings: `F-0010`
- Goal: canonical trade identity, DB constraints, and replay semantics must match exactly.
- Primary surfaces:
  - `internal/canonical/trade.go`
  - `internal/storage/timescale/trades.go`
  - `migrations/0001_create_trades_hypertable.up.sql`
  - `migrations/0004_relax_trades_ledger_for_offchain.up.sql`
- Acceptance:
  - one explicit identity model for on-chain and off-chain trades
  - schema and `ON CONFLICT` logic match the documented model
  - replay/idempotency tests cover off-chain and on-chain cases

### R-0009: Make closed-bucket source ordering deterministic

- Findings: `F-0016`
- Goal: closed-bucket payloads must be stable across regions when underlying data matches.
- Primary surfaces:
  - `migrations/0002_create_price_aggregates.up.sql`
  - `internal/storage/timescale/aggregates.go`
  - `internal/api/v1/price.go`
  - `internal/api/v1/price_stream.go`
- Acceptance:
  - `sources` ordering is explicitly deterministic
  - API docs and ADR guarantees are satisfied
  - cross-region tests cover full payload, not just price

### R-0010: Expand cross-region monitor/check payload comparison

- Findings: `F-0017`
- Goal: operator controls must validate all user-visible consistency fields that matter for ADR-0015.
- Primary surfaces:
  - `cmd/ratesengine-ops/cross_region_check.go`
  - `cmd/ratesengine-ops/cross_region_monitor.go`
  - tests
- Acceptance:
  - comparisons include `sources`, flags, and other contract-critical fields
  - tests fail on non-price payload drift

### R-0011: Finish archive-completeness control scope

- Findings: `F-0019`
- Goal: the daily green path must cover the full ADR-0017 contract set or stop claiming it does.
- Primary surfaces:
  - `internal/archivecompleteness/*`
  - `cmd/ratesengine-ops/main.go`
  - metrics and runbooks
- Acceptance:
  - primary archive checks are implemented and surfaced
  - reports/metrics distinguish primary vs cross-anchor coverage
  - docs describe the exact enforced scope

## Wave 3: Feature Completion and Public Contract Repair

### R-0012: Wire supply snapshots end-to-end

- Findings: `F-0020`, `F-0021`
- Goal: either ship a real supply writer path and API wiring or stop presenting F2 as live.
- Primary surfaces:
  - `cmd/ratesengine-api/main.go`
  - `internal/storage/timescale/supply.go`
  - supply computation entrypoints
  - operator supply tooling
- Acceptance:
  - production writer exists for `asset_supply_history`
  - API binary wires `Supply`
  - `supply_basis` behavior matches policy and docs

### R-0013: Resolve phantom-source catalogue drift

- Findings: `F-0029`
- Goal: `/v1/sources` must describe the executable fleet, not aspirational registry entries.
- Options:
  - remove unsupported venues from registry/API/tests
  - or implement full config/runtime support for them
- Preferred:
  - remove until implemented

### R-0014: Repair account/auth/public contract mismatches

- Findings: `F-0011`, `F-0012`, `F-0013`, `F-0015`, `F-0018`
- Goal: public docs, OpenAPI, and handlers must converge.
- Primary surfaces:
  - `internal/api/v1/account.go`
  - `internal/api/v1/auth_sep10.go`
  - `openapi/rates-engine.v1.yaml`
  - `docs/reference/api-design.md`
  - `docs/getting-started.md`
- Acceptance:
  - placeholder routes are clearly marked or completed
  - status codes match spec
  - public historical endpoints are documented accurately
  - unsupported wire features are removed from docs or implemented

### R-0015: Decide triangulation ship state explicitly

- Findings: `F-0014`
- Goal: remove the partial-feature state.
- Options:
  - fully wire triangulation into the served/public contract with provenance flags
  - or remove/defer the live write path and docs until it is actually shipped

## Wave 4: Docs, Governance, and Residual Drift

### R-0016: Clean repo/source architecture docs

- Findings: `F-0001`, `F-0002`, `F-0003`, `F-0004`, `F-0005`, `F-0006`, `F-0027`, `F-0028`
- Goal: make the repo map and operator docs trustworthy again.
- Primary surfaces:
  - root docs
  - architecture docs
  - deployment/readme surfaces
  - source package READMEs

### R-0017: Fix abuse/rate-limit contract gaps

- Findings: `F-0008`, `F-0009`
- Goal: either implement authenticated key-based throttling and trusted-proxy controls, or reduce the exposed contract to what is actually safe.

### R-0018: Lock tool provenance and release process to repo reality

- Findings: `F-0007`
- Goal: replace mutable `@latest` installs and ad hoc scanner fetches where feasible, or record the intentional exceptions as policy.

## Recommended Execution Order

1. `F-0022`, `F-0025`, `F-0031`
2. `F-0023`, `F-0026`, `F-0024`, `F-0030`
3. `F-0010`, `F-0016`, `F-0017`, `F-0019`
4. `F-0020`, `F-0021`, `F-0029`, `F-0011`, `F-0012`, `F-0015`, `F-0018`, `F-0014`
5. Remaining docs/governance cleanup

## Closure Rules

A finding should move from `open` only when:

- the code or docs change is merged
- the relevant tests exist or were updated
- the finding note in `05-findings-register.md` is updated with the closing change
- any dependent docs/runbooks/alerts/OpenAPI surfaces were reconciled in the same change
