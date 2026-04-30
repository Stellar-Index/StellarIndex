# Master Tracker

Status legend:

- `todo`: not started
- `in_progress`: active
- `blocked`: cannot proceed without resolving a dependency
- `done`: completed for this snapshot
- `excluded`: explicitly out of scope with reason recorded

## Workspace Controls

- [x] Inventory generated for current checkout.
- [x] Commit SHA recorded in `inventory/repo-snapshot.md`.
- [x] Worktree state recorded in `inventory/repo-snapshot.md`.
- [x] Per-file tracker initialized.
- [x] Evidence log initialized.
- [x] Cross-file interaction log initialized.
- [x] Findings register initialized.
- [x] Exclusions register initialized.

## Snapshot Outcome

- Findings status: `31 closed`, `0 open`.
- Final verification: `make verify` passed on 2026-04-30.
- Remediation state: complete for this repo snapshot.

## Workstream Tracker

| ID | Workstream | Status | Primary output | Notes |
| --- | --- | --- | --- | --- |
| 1 | Inventory, governance, and hygiene | `done` | this file + findings/exclusions | Inventory closed for this snapshot. Root governance/control docs reviewed, exclusions formalized, and the file tracker now has terminal status on every tracked file. |
| 2 | Architecture and ADR truth | `done` | findings + cross-file interactions | Repo orientation, ADR baseline, and top-level architecture/docs surfaces reconciled against live code. Material architecture drift is logged in the findings register. |
| 3 | Build, local stack, release, reproducibility | `done` | findings + evidence log | Build/release/deploy control surfaces were reviewed and remediated to the current snapshot truth where needed. |
| 4 | Dependencies, provenance, supply chain | `done` | findings + evidence log | Dependency/provenance surfaces were reviewed and remediated to a pinned local/CI toolchain posture for this snapshot. |
| 5 | Canonical types, parsing, numeric invariants | `done` | findings + cross-file interactions | Canonical/type surfaces closed for this snapshot through package review plus full-suite verification. No new discrete finding was added beyond the already-logged data-integrity and contract issues that depend on these types. |
| 6 | Ingest transport, dispatch, pipeline integrity | `done` | findings + journey evidence | Shared processor/cursor contract, dispatcher path, discovery backpressure path, and live observability seams reviewed. Permanent-skip and monitoring gaps are logged. |
| 7 | On-chain source decoder correctness | `done` | findings + per-source evidence | Source decoder packages closed for this snapshot. Decoder/adapter docs drift and ingest observability gaps were logged where present. |
| 8 | External source fleet and source-class policy | `done` | findings + journey evidence | External framework, registry, runtime wiring, and public catalogue surfaces reviewed. Phantom-source drift and class/runtime mismatches are now logged. |
| 9 | Storage, schema, migration correctness | `done` | findings + reconciliation notes | Storage/migration/CAGG surfaces closed for this snapshot, including trade-identity reconciliation and deterministic closed-bucket contributor ordering. |
| 10 | Aggregation, anomaly, freeze, confidence | `done` | findings + journey evidence | Aggregation/orchestrator surfaces closed with the current triangulation ship state made explicit in public/docs surfaces. |
| 11 | API lifecycle, middleware, contracts, auth | `done` | findings + reconciliation notes | Handler, middleware, auth, and public contract surfaces were remediated to the live runtime behavior for this snapshot. |
| 12 | Supply derivation and Freighter F2 | `done` | findings + journey evidence | Supply/F2 surfaces closed for this snapshot by wiring the read path and narrowing the published contract to the implemented scope. |
| 13 | Operator tooling, archive completeness, cross-region safety | `done` | findings + journey evidence | Operator CLI, archive verification/completeness, and cross-region control surfaces were remediated to truthful scope and stable payload comparison semantics. |
| 14 | Observability, testing, CI, documentation truth | `done` | findings + reconciliation notes | Observability/docs-truth/CI pass closed with final `make verify` success for the remediated snapshot. |

## Mandatory Passes

### Top-Down Architecture Pass

- [x] Runtime binaries traced.
- [x] Major package boundaries traced.
- [x] Storage and cache boundaries traced.
- [x] External dependency boundaries traced.
- [x] Operator/deploy/CI boundaries traced.

### Bottom-Up File Pass

- [x] Every tracked file marked in `inventory/file-coverage.tsv`.
- [x] Every tracked file has a terminal status.
- [x] Every skipped file has an exclusion entry or a note.
- [x] Every file with material behavior has evidence refs.

### End-to-End Journeys

- [x] On-chain trade journey.
- [x] Band contract-call journey.
- [x] Redstone event plus op-args journey.
- [x] External venue trade journey.
- [x] `/v1/price` closed-bucket journey.
- [x] `/v1/price/tip` rolling-window journey.
- [x] `/v1/assets/{id}` plus SEP-1 overlay journey.
- [x] API key auth journey.
- [x] SEP-10 challenge/token/auth journey.
- [x] Supply snapshot to asset-detail F2 journey.
- [x] Backfill journey.
- [x] Verify-archive journey.
- [x] Cross-region monitor journey.

### Hostile-Environment Pass

- [x] Bad input validation paths.
- [x] Timeout and cancellation behavior.
- [x] Duplicate and idempotent write paths.
- [x] Redis unavailable / fail-open paths.
- [x] Postgres unavailable paths.
- [x] Partial archive / stale archive paths.
- [x] Outlier, freeze, stale-data, and confidence degradation paths.

### Documentation-Truth Pass

- [x] Root docs vs repo layout.
- [x] Architecture docs vs code.
- [x] ADRs vs implementation.
- [x] Config docs vs config code.
- [x] OpenAPI/docs vs handlers.
- [x] Metrics docs vs registered metrics.
- [x] Runbooks/alerts/docs vs workflows and binaries.

### Negative-Space Pass

- [x] Legacy topology residue.
- [x] Mounted-but-not-fully-implemented surfaces.
- [x] Dead files or misleading names.
- [x] Stale tests giving false confidence.
- [x] Stale docs giving false confidence.

## Closure Criteria

- [x] Findings register reflects the remediated audit state as of the final snapshot.
- [x] Exclusions register is explicit and justified.
- [x] Cross-file interaction log covers every major runtime seam.
- [x] File coverage tracker has no `todo` rows.
- [x] Final report can be assembled from evidence without re-reading memory.
