# Coverage ledger — audit 2026-07-16 (commit 4d034432; money/ingest/api engine code == audited f84e2d0b)

The systematic proof of what was examined. 2,872 tracked files; **2,278 fell under an audited unit scope**, **594 were never assigned to a unit** (recorded NOT-EXAMINED below with reason). Coverage is by (unit × dimension); finders returned per-file attestations within their scope (FINDING / EXAMINED-SOUND). This ledger reconciles the file set — it does not re-list every EXAMINED-SOUND note (those live in the chunk results / journals).

## Units EXAMINED (4 chunks, 23 units)
- **Chunk 1 (money):** money-canonical, money-aggregate, money-supply, money-divergence, money-api-serve, money-external-sources + 27 whole-tree mechanical sweeps. `converged:false` (3-wave cap) — deep, not exhaustive.
- **Chunk 2 (ingest/data):** ingest-pipeline, ingest-projector, ingest-decoders-onchain, storage-timescale, storage-clickhouse, migrations, completeness-ops. `converged:false` (2-wave cap).
- **Chunk 3 (api/auth/platform):** api-auth, api-platform, api-explorer, config-lifecycle, pkg-client. DONE (57 agents, `converged:false`, 32 confirmed).
- **Chunk 4 (infra/web/plans):** obs-monitoring, infra-cicd, web-explorer, web-status, plans-docs. DONE (79 agents, `converged:false`, 41 confirmed).

**Totals:** 627 agents across 4 chunks; ~168 raw confirmed → ~110–120 distinct after cross-chunk dedup; 0 critical, ~30 HIGH (~25 LIVE). All 4 chunks `converged:false` (deep-not-exhaustive — recorded honestly, not as completion).

Every `internal/` production package maps to ≥1 unit **except** the small utility packages listed under NOT-EXAMINED below.

## Dimensions N/A (recipe, adversarially challenged — see recipe §6)
- **MBL** — no native/mobile app (static web export). Challenge: grep for `ios/`/`android/`/react-native → none. Upheld.
- **LLM** — no LLM in the product. Challenge: grep anthropic/openai/genai → none. Upheld.
- **MDL** — no trained/predictive model (VWAP/MAD are deterministic statistics, audited under MNY). Upheld.
- **TNS** — no user-to-user interaction (read-only data product + API keys; no UGC/messaging). Upheld.

## NOT-EXAMINED (594 files never assigned to a unit) — reported as prominently as findings

| Group | Files | Reason / risk |
|---|---|---|
| **`test/` tree** (integration 54, fixtures 41, load 19, chaos 13) | **127** | **Most significant gap.** TST dimension was applied *within* each unit (finders checked whether the scope's tests assert failure cases), but the test tree itself was NOT deep-audited as a unit — so "do the integration/load/chaos harnesses actually catch the bugs this audit found, and are fixtures faithful to real lake bytes?" is UNVERIFIED. Recommend a dedicated TST pass (the CI already runs `make test-integration` green, so the harness works; the question is coverage adequacy). |
| `docs/operations/` runbooks + wasm-audits + deployment docs | 333 | Operational docs. The OBS unit audited `deploy/monitoring` + `configs/prometheus` rules and the alert↔runbook lint, but the runbook *bodies* (accuracy of the 3am procedures) weren't read. Low finding-yield but runbook drift is real (the deployment-state doc was 2 months stale). |
| `docs/{protocols,reference,methodology,blog,contributing,getting-started,engineering-standards}` | ~43 | Content + generated docs. `docs/reference` is generated (drift-gated in CI). Not deep-read; plans-docs covered adr/architecture/notes/perf-todo/CLAUDE/README. |
| `docker/` (6 Dockerfiles + README) | 7 | DEP/CID relevant (base-image digest pinning flagged as BRANCH in chunk 1). The tree wasn't assigned a unit; release-validate CI builds them. Low. |
| `examples/` (curl 16, postman 2, README) | 19 | Public example scripts; Postman is generated (and currently STALE — see the openapi-lint CI red / findings). |
| `internal/stellarrpc` (8), `internal/xdrjson` (7), `internal/nettools` (2), `internal/version` (2), `internal/httpx` (1), `internal/sdexclaim` (1) | 21 | Small utility packages, not assigned a named unit. Low risk: stellarrpc is diagnostics/fixtures-only (banned from prod ingest by CI lint), version is ldflags, nettools/httpx are foundation-pure. Incidentally traced where imported by audited units. |
| prior-audit dirs (`docs/audit-2026-06-30`, `docs/maintainability-audit-2026-07-01`, `docs/audit-2026-07-03-*`, `docs/remediation-2026-07-01`, `docs/upstream`) | ~26 | Prior audit corpus — inputs to planning (re-verification set in the recipe), not audit targets. |
| `.claude/skills` (9 project skills), root files (LICENSE, CODEOWNERS, CODE_OF_CONDUCT, SECURITY.md, .gitignore) | ~14 | Meta/boilerplate. `.claude/skills` (the repo's own review/deploy skills) not audited — low product risk. |

## Exposure grounding (from operational-reality.md)
Findings are exposure-tagged against the RUNNING config, not code defaults: R1 runs v0.16.0 binaries (main is 12 commits ahead, undeployed behind the Phase-0 freeze), `auth_mode=apikey_optional`, `min_usd_volume=0`, only free FX venues (not the polygon-forex/exchangeratesapi connectors that trigger CS-040), loopback metrics + nftables + Caddy. So CS-040 and /metrics are GATED, and any defect introduced by the 12 undeployed commits is PENDING-DEPLOY, not LIVE.

## Honesty statement
- Two chunks recorded `converged:false` (hit the wave cap while still producing findings). The money and ingest/storage surfaces are **deeply but not exhaustively** covered — a further pass would likely surface more, concentrated in the same failure classes (the recipe's RFCs). This is recorded, not hidden.
- The `test/` tree and the operational runbook bodies are the two named coverage gaps a follow-up should close.
