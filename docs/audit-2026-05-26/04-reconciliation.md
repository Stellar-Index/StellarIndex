# Reconciliation Runbook

This document is the **docs-vs-code** reconciliation runbook.
Every claim in a doc must reach code, test, or runtime evidence
before we count it as "verified". Mismatches become findings.

For each reconciliation row below:

1. Read the doc claim.
2. Locate the corresponding code path (file:line).
3. Decide one of: `true`, `stale`, `partial`, `contradicted`.
4. Log evidence ID.
5. If `stale` / `partial` / `contradicted`, file a finding
   (severity per the rubric).

## R01. ADR alignment

| ID | Doc claim (ADR text) | Code path to verify | Status |
| --- | --- | --- | --- |
| ADR-0001 | "Horizon is not in our architecture" | `scripts/ci/lint-imports.sh` rule; `internal/ledgerstream/` never imports horizon | `todo` |
| ADR-0002 | "Self-hosted storage is S3-compatible — not local filesystem" | `internal/storage/timescale/` queries; pipeline config | `todo` |
| ADR-0003 | "i128/u128 never truncates to int64" | every `xdr.Int128Parts` parse site; ADR-0003 test | `todo` |
| ADR-0004 | "Tier-1 three-validator aspiration" | aspirational; no current code | `todo` |
| ADR-0005 | "One Go module, monorepo" | `go.mod` only at root | `todo` |
| ADR-0006 | "TimescaleDB for price time-series" | migrations + storage drivers | `todo` |
| ADR-0007 | "Redis cache schema" | `internal/cachekeys/` is sole key builder | `todo` |
| ADR-0008 | "HA topology" | reality on r1 + designed topology in r2/r3 plans | `todo` |
| ADR-0009 | "Latency budget" | SLA probe thresholds + rule expressions | `todo` |
| ADR-0010 | "Off-chain fiat representation" | `internal/canonical/asset_fiat.go` | `todo` |
| ADR-0011 | "Supply algorithm" | `internal/supply/{classic,xlm,sep41}.go` | `todo` |
| ADR-0012 | "Quorum set composition" | core config | `todo` |
| ADR-0013 | "go-stellar-sdk for XDR / SCVal" | scval imports; xdr-scoped-to-scval lint | `todo` |
| ADR-0014 | "Crypto-ticker representation" | `internal/canonical/asset_crypto.go` | `todo` |
| ADR-0015 | "Last-closed-bucket rate serving" | `internal/api/v1/price.go` + closed_bucket internal test | `todo` |
| ADR-0016 | "Per-region storage strategy" | r1/r2/r3 deployment-state docs vs cold-tier code | `todo` |
| ADR-0017 | "Archive completeness invariants" | `internal/archivecompleteness/` + tier A/B/C/D | `todo` |
| ADR-0018 | "API consistency surfaces" | `internal/api/v1/envelope.go` + every handler | `todo` |
| ADR-0019 | "Anomaly response + confidence scoring" | `internal/aggregate/{anomaly,confidence,freeze}/` | `todo` |
| ADR-0020 | "Chart API contract" | `internal/api/v1/chart.go` | `todo` |
| ADR-0021 | "Account-entry observer" | `internal/sources/accounts/` | `todo` |
| ADR-0022 | "Classic supply observers" | `internal/sources/{trustlines,claimable_balances,liquidity_pools,sac_balances}/` | `todo` |
| ADR-0023 | "SEP-41 supply observer" | `internal/sources/sep41_supply/` | `todo` |
| ADR-0024 | "Redis HA via Sentinel" | redis-sentinel ansible role + connection wiring | `todo` |
| ADR-0025 | "Caddy + Cloudflare trusted proxy" | Caddyfile + middleware reading X-Real-IP | `todo` |
| ADR-0026 | "Stablecoin fiat-proxy late binding" | `internal/aggregate/stablecoin.go` + depeg_test | `todo` |
| ADR-0027 | "LCM cache tiering" | `internal/ledgerstream/{seamed,tiered}.go` + LedgerstreamConfig | `todo` |
| ADR-0028 | "RWA asset representation" | `internal/canonical/asset_rwa.go` | `todo` |
| ADR-0029 | "soroban_events landing zone" | `internal/sources/sorobanevents/` + migration 0041 + RawEventSink | `todo` |

## R02. Architecture doc alignment

| Doc | Code path to verify | Status |
| --- | --- | --- |
| `docs/architecture/aggregation-plan.md` | `internal/aggregate/orchestrator/` + handlers | `todo` |
| `docs/architecture/cctp-stellar-coverage.md` | `internal/sources/cctp/` + cctp_events table | `todo` |
| `docs/architecture/chaos-suite-design-note.md` | `test/chaos/scenarios/` | `todo` |
| `docs/architecture/coins-to-assets-migration.md` | route removal hygiene + `/v1/assets/*` handlers | `todo` |
| `docs/architecture/contract-call-coverage-audit.md` | dispatcher `ContractCallDecoder` + Band relay observation | `todo` |
| `docs/architecture/contract-schema-evolution.md` | every Soroban decoder reads-by-name; topic dispatch | `todo` |
| `docs/architecture/coverage-matrix.md` | `internal/sources/external/registry.go` + W35 register | `todo` |
| `docs/architecture/ecosystem-review-2026-04-23.md` | reality check vs current state | `todo` |
| `docs/architecture/explorer-data-inventory.md` | `web/explorer/` consumption | `todo` |
| `docs/architecture/explorer-implementation-plan.md` | actual explorer state | `todo` |
| `docs/architecture/ha-plan.md` | r1 single-host vs HA design | `todo` |
| `docs/architecture/haproxy-ansible-role-design-note.md` | ansible role + reality | `todo` |
| `docs/architecture/infrastructure/multi-region-topology.md` | r2/r3 absence | `todo` |
| `docs/architecture/ingest-pipeline.md` | dispatcher + ADR-0029 raw-event hook | `todo` |
| `docs/architecture/k6-load-tests-design-note.md` | `test/load/` reality | `todo` |
| `docs/architecture/launch-readiness-backlog.md` | open items vs current state | `todo` |
| `docs/architecture/loki-ansible-role-design-note.md` | ansible role + r1 deployment | `todo` |
| `docs/architecture/multi-network-assets-migration.md` | `/v1/assets/{slug}` two-shape behaviour | `todo` |
| `docs/architecture/oracle-manipulation-defense.md` | divergence + outlier paths | `todo` |
| `docs/architecture/patroni-ansible-role-design-note.md` | ansible role + reality | `todo` |
| `docs/architecture/platform-spec.md` | `internal/platform/` | `todo` |
| `docs/architecture/prometheus-ansible-role-design-note.md` | ansible role + reality | `todo` |
| `docs/architecture/redis-sentinel-ansible-role-design-note.md` | ansible role + reality | `todo` |
| `docs/architecture/r2-r3-bringup.md` | r2/r3 status | `todo` |
| `docs/architecture/repo-hygiene-plan.md` | repo state | `todo` |
| `docs/architecture/rozo-stellar-coverage.md` | `internal/sources/rozo/` + rozo_events table | `todo` |
| `docs/architecture/semver-policy.md` | cut-release.sh + tags | `todo` |
| `docs/architecture/status-page-hosting-comparison.md` | web/status reality | `todo` |
| `docs/architecture/storage-considerations.md` | actual storage strategy | `todo` |
| `docs/architecture/supply-pipeline.md` | `internal/supply/` | `todo` |

## R03. Operations doc alignment

| Doc | What it claims | Status |
| --- | --- | --- |
| `docs/operations/alerts-catalog.md` | every alert + threshold | `todo` |
| `docs/operations/archival-node-bringup.md` | bring-up steps | `todo` |
| `docs/operations/archive-completeness.md` | tier A/B/C/D | `todo` |
| `docs/operations/backfill-procedure.md` | backfill steps; NEW: per-source backfill | `todo` |
| `docs/operations/cagg-broad-recompute.md` | broad recompute path | `todo` |
| `docs/operations/cdn-setup.md` | Cloudflare setup | `todo` |
| `docs/operations/cf-pages-setup.md` | explorer + status deploy | `todo` |
| `docs/operations/chaos-wave1-runbook.md` | chaos drills | `todo` |
| `docs/operations/customer-demo-script.md` | every step works today | `todo` |
| `docs/operations/deploy-workflow.md` | deploy.yml reality | `todo` |
| `docs/operations/explorer-deployment.md` | explorer deploy | `todo` |
| `docs/operations/galexie-backfill.md` | galexie backfill | `todo` |
| `docs/operations/github-actions-sha-pinning.md` | actions/setup-go pin (2026-05-26 incident relevant) | `todo` |
| `docs/operations/hubble-event-counts.md` | event count reconciliation | `todo` |
| `docs/operations/launch-day-checklist.md` | every step | `todo` |
| `docs/operations/lcm-cache-tiering.md` | ADR-0027 implementation | `todo` |
| `docs/operations/multi-region-cutover.md` | r2/r3 cutover | `todo` |
| `docs/operations/perf-todo.md` | perf items remaining | `todo` |
| `docs/operations/post-launch-queries.md` | analytical queries | `todo` |
| `docs/operations/pre-launch-hardening.md` | hardening checklist | `todo` |
| `docs/operations/public-flip.md` | flip steps | `todo` |
| `docs/operations/r1-ansible-drift-2026-05-22.md` | ansible drift register | `todo` |
| `docs/operations/r1-deployment-state.md` | r1 vs reality | `todo` |
| `docs/operations/r2-deployment-state.md` | r2 absence | `todo` |
| `docs/operations/r3-deployment-state.md` | r3 absence | `todo` |
| `docs/operations/release-process.md` | cut-release.sh + release.yml | `todo` |
| `docs/operations/rollback.md` | rollback steps | `todo` |
| `docs/operations/runbooks/*.md` | every runbook vs its alert | `todo` per-runbook |
| `docs/operations/wasm-audits/*.md` | per-source audit log | `todo` per-source |

## R04. RFP / proposal alignment

| Doc | Status |
| --- | --- |
| `docs/stellar-rfp.md` | `todo` — row-by-row |
| `docs/freighter-rfp.md` | `todo` — row-by-row |
| `docs/ctx-proposal.md` | `todo` — F1..F12 features |
| `docs/discovery/proposal-corrections.md` | `todo` — each correction |
| `docs/discovery/rfp-requirements-matrix.md` | `todo` |

## R05. Reference doc alignment

| Doc | Source of truth | Status |
| --- | --- | --- |
| `docs/reference/api/` | `openapi/rates-engine.v1.yaml` + handlers | `todo` |
| `docs/reference/config/` | `internal/config/schema.go` | `todo` |
| `docs/reference/metrics/` | `internal/obs/metrics.go` | `todo` |
| `docs/reference/api-design.md` | actual API contract | `todo` |

## R06. CHANGELOG ↔ PR ↔ Code

For every entry under `## [v0.5.0-rc.NN]` since rc.71:

| Entry | PR | Code path | Status |
| --- | --- | --- | --- |
| (populated by audit) | | | `todo` |

## R07. CLAUDE.md ↔ reality

CLAUDE.md has many factual claims. Each should hold:

| Section | Claim | Status |
| --- | --- | --- |
| "What this repo is" | "pre-v1 at time of writing" | `todo` |
| "Build + test commands" | every command works | `todo` |
| "Repo map" | every directory listed exists; nothing major omitted | `todo` |
| "Invariants" | each numbered invariant is enforced | `todo` |
| "Things that will surprise you" | each surprise is still true | `todo` |
| "Common task recipes" | each recipe still applies | `todo` |
| "Where to find design rationale" | links resolve | `todo` |

## R08. Memory ↔ reality (NEW)

For each memory entry in
`~/.claude/projects/-Users-ash-code-ratesengine/memory/*.md`:

1. Read the claim.
2. Locate code/runtime evidence.
3. Mark `still-true`, `obsolete`, `superseded-by-PR-####`,
   `contradicted-by-code`.

Known seed entries to start with (audit must enumerate ALL):

| Memory file | Claim | Status |
| --- | --- | --- |
| `feedback_reenable_trades_compression.md` | re-enable job 1000 after backfills end | `obsolete` (job 1000 already scheduled=t per 2026-05-26 probe) |
| `feedback_quiet_checksum_was_a_noop.md` | rc.72 quiet-checksum fix is no-op; rc.77 fd-2 wrap fixed it | `todo` (re-verify rc.77 fix still holds in current binary) |
| `feedback_cold_tier_premature_enable.md` | §3 + §4 must go together | `still-true` (verify cold-tier rollout discipline) |
| `feedback_fd2_wrap_drain_on_exit.md` | rc.77 wrap dropped short-lived process output; rc.78 fixed | `todo` |
| `feedback_r1_sql_quoting.md` | never inline `$$..$$` over SSH | `still-true` (operational discipline) |
| `feedback_branch_before_commit.md` | always create branch before first commit (for PR work) | `todo` |
| `feedback_commit_cadence.md` | live-in-development phase: commit per-unit on main | `todo` |
| `feedback_release_cadence.md` | bundle session fixes into one tag | `todo` |
| `feedback_session_size.md` | pause every ~10 PRs/commits | `todo` |
| `project_every_event_principle.md` | every event for every Soroban protocol — W35 | `todo` |
| `project_soroban_events_landing_zone.md` | design-of-record for ADR-0029 | `todo` |
| `project_62_diagnosis_2026_05_25.md` | verify-archive trailing-edge issue | `still-true` (closed by rc.81 fix; verify still works) |
| `project_open_backlog.md` | task IDs still open | `todo` |
| `project_session_2026_05_26_resume.md` | this session's resume notes | `todo` (will be obsoleted when session closes) |

(Full list to be enumerated during the audit.)
