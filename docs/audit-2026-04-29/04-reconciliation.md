# Mandatory Reconciliation Passes

## R1. Code vs Docs

Required comparisons:

- `AGENTS.md` / `CLAUDE.md` vs runtime code
- `docs/architecture/*` vs binaries and packages
- `docs/reference/config/README.md` vs `internal/config/*`
- `docs/reference/api-design.md` vs `internal/api/v1/*`
- `docs/reference/metrics/README.md` vs `internal/obs/*`
- runbooks/alerts vs monitoring rules, systemd units, and code

## R2. Code vs Tests

Required comparisons:

- each high-risk package vs its tests
- each runtime binary vs direct test coverage of its wiring
- each decoder vs reject-path and fixture coverage
- storage methods vs integration coverage
- auth and middleware behavior vs tests

## R3. Tests vs CI

Required comparisons:

- unit tests present vs unit tests run in CI
- integration tests present vs how they are triggered
- doc drift tests present vs CI enforcement
- lint/import/security checks present vs CI enforcement

## R4. Shared Contracts vs Producers/Consumers

Required comparisons:

- `pkg/client/*` vs API handlers and envelope shapes
- shared asset/price/supply shapes vs handler outputs
- error-envelope behavior vs documented error contracts

## R5. OpenAPI vs Handlers

Required comparisons:

- mounted routes vs OpenAPI-described routes
- query params and path params vs handler parsing
- status codes vs handler behavior
- documented error surface vs actual problem bodies

## R6. ADRs vs Implementation

Required comparisons:

- ADR-0001 no Horizon
- ADR-0002 S3-compatible storage
- ADR-0003 no i128/u128 truncation
- ADR-0006 Timescale time-series assumptions
- ADR-0007 Redis cache schema
- ADR-0011 supply algorithm
- ADR-0015 last closed bucket
- ADR-0017 archive completeness
- ADR-0018 API consistency surfaces
- ADR-0019 anomaly/confidence/freeze

## R7. Discovery Docs vs Current Code

Required comparisons:

- `docs/discovery/adversarial-audit.md` vs current implementation state
- per-source discovery docs vs current source decoders
- proposal corrections vs current architecture docs

## R8. Operational Docs vs Runtime Surfaces

Required comparisons:

- runbooks vs actual metrics/alerts
- release docs vs workflows
- bootstrap and bringup docs vs Ansible/systemd/compose assets
- archive completeness docs vs `ratesengine-ops` and archive package
