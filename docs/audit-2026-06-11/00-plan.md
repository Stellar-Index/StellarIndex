# Audit Plan — 2026-06-11 full-product cold audit

## Objective

Execute a comprehensive cold audit of the entire repository:

- **every tracked file touched** (2,349 files at HEAD a375e0ad),
  with calibrated depth (full review for live code/docs;
  integrity-only for frozen archives — see the exclusions register);
- **every material cross-file interaction** verified via explicit
  completeness matrices (not just per-file reading — absences are
  findings too);
- every audit dimension below covered by at least one dedicated
  sweep;
- findings adversarially verified before entering the register.

The audit is **read-only**: no code or doc fixes land during the
audit. Output is a findings register (global F-numbering continues
at F-1316) plus a prioritized remediation plan.

## Audit dimensions (the full list)

| # | Dimension | What it covers |
|---|-----------|----------------|
| 1 | Invariant compliance | The 8 binding CLAUDE.md invariants (i128→big.Int/NUMERIC/string-JSON; no Horizon; S3-not-fs; monorepo boundaries; validator/HSM posture; Galexie→dispatcher→decoder only; one-writer-per-domain ADR-0031/32; CH-lake/PG-served ADR-0034) |
| 2 | Trap compliance | The 15 "things that will surprise you" used as per-source audit lenses (Soroswap SyncEvent, Phoenix 8-event, Comet shared topic, Reflector 3-contract/no-twap, Band E18/E9 + zero events, Redstone OpArgs zip, P23 dual-path, SEP-41 i128-or-map, external 10^8 scaling, stablecoin late-binding, contract upgrades-in-place, dual wire shapes, currency trust surface, …) |
| 3 | Security | authn/authz per route, rate limiting, input validation, SQL/command injection, SSRF in outbound fetchers, secrets-in-tree, dependency vulns, infra hardening, public-flip readiness |
| 4 | Decoder fidelity | each on-chain decoder vs its discovery doc; event-coverage vs the EVERY-event policy; WASM-version awareness; BackfillSafe gating |
| 5 | Aggregation math | big.Rat discipline, VWAP/TWAP windows, outlier policy, triangulation, stablecoin fiat-proxy, source-class filtering, freeze/LKG |
| 6 | Storage | migration consistency/reversibility, hypertable + CAGG policies, retention drift, indexes vs query patterns, NUMERIC discipline, CH schema parity with decoders |
| 7 | API contract chain | OpenAPI ↔ handlers ↔ envelope ↔ pkg/client ↔ examples ↔ smoke ↔ sla-probe ↔ docs/reference |
| 8 | Cross-file wiring | registries (sources, projector, external), pipeline sink type-switch, gap-detector targets, completeness census, heartbeats, BackfillSafe×WASM-audit-log pairing |
| 9 | Concurrency & resources | goroutine lifecycles, tickers, context propagation, pools, shutdown paths, races |
| 10 | Error handling | swallowed errors, wrapping consistency, retry storms, partial-failure semantics |
| 11 | DRY / duplication | cross-package copy-paste, dead code, unused exports, helper proliferation |
| 12 | Comment/code truth | stale comments, misleading names, TODO inventory |
| 13 | Test posture | per-package coverage vs risk, fixture freshness/reference, integration quality, build tags |
| 14 | Observability chain | metric declared → wired → documented → alerted (both rule dirs) → runbook → heartbeat |
| 15 | Ops/deploy | systemd ↔ Ansible ↔ R1 overlay consistency, deploy workflow, release process, DR/runbooks |
| 16 | CI/workflows | gate completeness, action pinning, workflow cost posture |
| 17 | Docs truth | every live doc accuracy vs code; ADR status hygiene; frozen archives integrity-only |
| 18 | RFP/proposal execution | every row of the requirements matrix + ctx-proposal commitments + corrections register → implementation evidence |
| 19 | Config drift | config.go ↔ example.toml ↔ reference docs ↔ deployment overlays |
| 20 | Web explorer | code quality, API-shape drift vs OpenAPI, static-export correctness |
| 21 | Dependencies & licensing | go.mod / VERSIONS.md pinning, archived upstreams, license compliance |
| 22 | Performance | unbounded queries, N+1, cache-key drift, hot-path allocations |

## Method — four waves

**Wave 0 (orchestrator):** mechanical gates run fresh
(govulncheck, secrets scan, docs-regen drift; `make verify` already
green at HEAD earlier today) + this plan + the coverage ledger
assigning every tracked file to exactly one Wave-1 slice.

**Wave 1 (≈30 slice agents, parallel):** cold-read every file in the
slice. Output: structured findings `{area, severity
critical|high|medium|low|info, title, files[file:line], evidence
quote, why-it-matters, suggested fix, confidence}` + a count of
files actually read (cross-checked against the ledger).

Slices (Go): api×5 (G1 core/middleware/auth, G2 pricing, G3
catalogue, G4 oracle/diag/streams, G5 remainder); sources×5 (G6
sdex/sorobanevents/soroswap/router/aquarius, G7
blend/comet/phoenix/defindex, G8 band/reflector/redstone/cctp/rozo,
G9 supply+balance observers+forex, G10 external connectors);
storage×2 (G11/G12); aggregate×2 (G13/G14); G15 ingest spine
(dispatcher/pipeline/ledgerstream/consumer/hashdb); G16
derived+verification (projector/completeness/archivecompleteness/
divergence/supply); G17 account/security cluster
(auth/ratelimit/usage/customerwebhook/notify/incidents); G18 types
cluster (canonical/scval/events/currency/metadata); G19 platform
cluster (config/obs/obstest/platform/cachekeys/stellarrpc/version);
cmd×2 (G20 daemons, G21 CLIs); G22 pkg+test; G23 migrations.

Slices (non-Go): N1 root governance + scripts + build config; N2
.github; N3 configs/ansible; N4 other configs + deploy; N5
openapi + examples; web×2 (N6/N7); docs: D1 root-level + blog +
methodology, D2 adr, D3 architecture, D4/D5 operations, D6
reference-drift + frozen-archive integrity sweep.

**Wave 2 (12 cross-cutting agents):** A wiring matrices; B API
contract chain; C RFP/proposal execution matrix; D security
deep-dive; E concurrency deep-dive; F duplication/DRY; G
observability chain; H docs-truth (CLAUDE.md/README/getting-started
claims vs code); I migrations↔schema↔queries; J config drift; K
deps/licensing/public-flip; L test-coverage mapping.

**Wave 3:** every High/Critical finding gets an independent
refute-by-default verification agent before the register.

**Wave 4 (orchestrator):** dedup vs known backlog/memory/CHANGELOG,
assign F-numbers, write `05-findings-register.md`,
`06-exclusions-register.md`, `07-remediation-plan.md`, commit.

## Plan refinement record

**Pass 1 → Pass 2 deltas** (gaps the first draft missed):
- Per-file reading alone cannot find *absences* — added Wave-2
  completeness matrices (e.g. consumer.Event types vs sink switch
  arms; sources vs gap-detector targets vs census; alerts vs
  runbooks vs both rule dirs; spec routes vs handlers vs client).
- Added adversarial verification wave (Wave 3) — prior sessions
  show plausible-but-wrong findings are a real failure mode.
- Pinned a severity rubric + structured output schema so 40+
  agents produce mergeable results.
- Gave each slice agent the *relevant* invariants/traps as lenses
  instead of the generic "find problems".
- Generated docs (docs/reference) audited for regen drift, not
  content; frozen archives (discovery/, prior audit dirs) audited
  for integrity only — recorded in the exclusions register.
- Added dedup-vs-known-issues step: chronic items already tracked
  (trades-chunk perf, pending WASM walks, partial event coverage
  for blend/comet/phoenix, ADR-0015 30s-vs-60s window drift found
  2026-06-11) enter the register as `pre-known` with refs, not as
  novel findings.

**Pass 2 → Pass 3 deltas** (practicality/sequencing):
- Slices sized ≤ ~12k LOC / ~60 files for read quality; thematic
  grouping (not alphabetical) so intra-slice cross-file bugs are
  visible; ledger catch-all guarantees totality.
- F-numbering continues the global sequence (F-1316+) since code
  comments cite F-numbers from prior audits.
- Read-only rule made explicit; fixes are post-audit work.
- Live r1 host state out of scope (repo artifacts only) — today's
  incident sweep already covered live state.
- Wave 1 launched in batches of ~6-7 agents to keep result
  integration tractable; Wave 2 starts only after Wave-1 matrices
  inputs exist; checkpoint commits after Wave 0 and Wave 4.
- Agents must report files-read counts; orchestrator reconciles
  against the ledger before Wave 4 (coverage proof).
- `test/fixtures` binary/XDR content excluded from line-review but
  reference-checked (orphan fixtures are findings).
