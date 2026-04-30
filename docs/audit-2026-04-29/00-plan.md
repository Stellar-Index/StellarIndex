# Audit Plan

## Objective

Execute a full cold adversarial audit of this repository with explicit
coverage of:

- every tracked file
- every material cross-file interaction
- every runtime surface
- every documented invariant
- every major code-to-doc and code-to-test contract

The audit is repo-specific. Donor-project items that do not map to this
repository are excluded and replaced by Rates Engine-specific work.

## Repo-Specific Workstreams

### 1. Inventory, Governance, and Hygiene

Audit:

- root docs and repo map truth
- `CODEOWNERS`, PR template, release-note template, contribution flow
- changelog and upstream version pinning
- ignored operational files, generated residue, misleading names
- exact commit SHA and dirty-worktree caveats

Primary evidence:

- `AGENTS.md`, `CLAUDE.md`, `README.md`
- `CODEOWNERS`
- `.github/`
- `CHANGELOG.md`
- `VERSIONS.md`
- `.gitignore`

### 2. Architecture and ADR Truth

Audit:

- root guidance vs live code
- ADR invariants vs implementation
- architecture docs vs binaries and packages
- documented production path vs residual legacy topology
- undocumented architecture changes and stale claims

Primary evidence:

- `docs/adr/*.md`
- `docs/architecture/*.md`
- `CLAUDE.md`
- `cmd/*`
- `internal/*`

### 3. Build, Local Stack, Release, and Reproducibility

Audit:

- `Makefile` targets vs actual commands
- compose dev stack vs docs
- migration workflow
- release process and release note expectations
- workflow parity, local verify path, generated-doc drift checks

Primary evidence:

- `Makefile`
- `deploy/docker-compose/*`
- `migrations/*`
- `.github/workflows/*`
- `docs/operations/release-process.md`

### 4. Dependencies, Provenance, and Supply Chain

Audit:

- direct dependencies by role and necessity
- heavy transitive risk
- third-party binaries and contract-code trust assumptions
- workflow-installed tooling and scanners
- pinned SHA discipline and provenance posture

Primary evidence:

- `go.mod`, `go.sum`
- `VERSIONS.md`
- `.github/workflows/ci.yml`
- `docs/discovery/VERSIONS.md`

### 5. Canonical Types, Parsing, and Numeric Invariants

Audit:

- `*big.Int` handling
- asset, pair, and strkey identity
- string/JSON/DB representation boundaries
- SCVal parsing and typed decode assumptions
- invariant-preserving validation on write and read paths

Primary evidence:

- `internal/canonical/*`
- `internal/scval/*`
- `internal/events/event.go`
- tests in the same packages

### 6. Ingest Transport, Dispatch, and Pipeline Integrity

Audit:

- `ledgerstream` callback semantics
- dispatcher routing and first-match precedence
- non-fatal error handling
- op decoding, contract-call decoding, event decoding
- persistence pipeline and cursor handling
- discovery side-channel correctness

Primary evidence:

- `internal/ledgerstream/*`
- `internal/dispatcher/*`
- `internal/pipeline/*`
- `cmd/ratesengine-indexer/main.go`
- `cmd/ratesengine-ops/backfill.go`

### 7. On-Chain Source Decoder Correctness

Audit every file in:

- `internal/sources/soroswap`
- `internal/sources/aquarius`
- `internal/sources/phoenix`
- `internal/sources/comet`
- `internal/sources/reflector`
- `internal/sources/redstone`
- `internal/sources/band`
- `internal/sources/sdex`

For each source:

- README claims
- topic/body schema handling
- correlation state
- reject paths
- fixture realism
- adapter and dispatcher integration
- tests vs actual risk surface

### 8. External Source Fleet and Source-Class Policy

Audit:

- external framework and runner
- venue-specific pollers/streamers/backfills
- registry metadata and class/subclass policy
- stablecoin-proxy boundary
- off-chain connector error handling and idempotency

Primary evidence:

- `internal/sources/external/*`
- `cmd/ratesengine-indexer/main.go`
- `internal/aggregate/stablecoin.go`

### 9. Storage, Schema, and Migration Correctness

Audit:

- migrations up/down symmetry
- hypertable/CAGG assumptions
- typed storage adapters
- idempotent inserts
- query semantics and closed-bucket guards
- discovery, supply, cursor, baseline, oracle, and trades storage

Primary evidence:

- `migrations/*`
- `internal/storage/timescale/*`
- `test/integration/*`

### 10. Aggregation, Anomaly, Freeze, and Confidence

Audit:

- VWAP, TWAP, OHLC, triangulation, outlier filtering
- stablecoin fiat proxy
- anomaly thresholds and classification
- baseline refresh and bootstrap policy
- confidence-score computation
- freeze marker semantics and API exposure

Primary evidence:

- `internal/aggregate/*`
- `cmd/ratesengine-aggregator/main.go`
- `docs/adr/0018-api-consistency-surfaces.md`
- `docs/adr/0019-anomaly-response-and-confidence-scoring.md`

### 11. API Lifecycle, Middleware, Contracts, and Auth

Audit:

- route registration
- middleware order
- request ID, logging, recovery, headers, cache control, CORS
- rate limit behavior
- auth modes, API keys, SEP-10
- handler input validation and error envelopes
- SSE and per-surface consistency contracts
- OpenAPI and docs drift

Primary evidence:

- `internal/api/streaming/*`
- `internal/api/v1/*`
- `internal/auth/*`
- `openapi/rates-engine.v1.yaml`
- `docs/reference/api-design.md`

### 12. Supply Derivation and Freighter F2 Surfaces

Audit:

- classic and SEP-41 supply computation
- cross-check rules
- overlay policy
- storage and API surfacing of supply values
- operator audit tooling

Primary evidence:

- `internal/supply/*`
- `internal/storage/timescale/supply.go`
- `internal/api/v1/assets_f2.go`
- `cmd/ratesengine-ops/supply.go`

### 13. Operator Tooling, Archive Completeness, and Cross-Region Safety

Audit:

- every `ratesengine-ops` subcommand
- archive completeness package
- verify-archive control flow
- cross-region check and monitor
- hubble cross-check tools
- systemd and monitoring rule alignment

Primary evidence:

- `cmd/ratesengine-ops/*`
- `internal/archivecompleteness/*`
- `deploy/systemd/*`
- `deploy/monitoring/rules/*`

### 14. Observability, Testing, CI, and Documentation Truth

Audit:

- metrics registry vs metrics docs
- runbooks vs alerts and code
- unit/integration coverage by risk area
- CI path vs local path
- stale documentation and residual discovery drift

Primary evidence:

- `internal/obs/*`
- `docs/reference/metrics/README.md`
- `docs/operations/*`
- `.github/workflows/*`
- `scripts/ci/*`
- `scripts/dev/*`

## Mandatory Passes

- Top-down architecture pass.
- Bottom-up file pass.
- User-journey pass.
- Operator-journey pass.
- Hostile-environment pass.
- Documentation-truth pass.
- Negative-space pass.

## Required Deliverables

- Per-file coverage status for every tracked file.
- Cross-file interaction log with evidence.
- Findings register with severity and evidence.
- Exclusions register.
- Reconciliation results for every mandated comparison.
- Final audit report built from the evidence produced here.
