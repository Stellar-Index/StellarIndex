# Audit Tracker — 2026-05-26

This is the **only** authoritative state of audit progress.
Workstream documents may carry per-check tables; this tracker
carries terminal status only.

## Status Vocabulary

| Status | Meaning |
| --- | --- |
| `todo` | not yet started |
| `in_progress` | active; expect updates this calendar week |
| `done` | reviewed with evidence (NOT bug-free) |
| `blocked` | requires external action (re-entry evidence in 06-exclusions-register.md) |
| `excluded` | explicitly out of scope (06-exclusions-register.md) |

A workstream marked `done` must:
1. have terminal status on every check in its sub-plan
2. carry at least one evidence ref per check
3. have at least one finding (even if `note`-severity informational)
   OR an explicit "no findings; evidence at EV-XXXX/CMD-XXXX"

## Workstream Status

| Workstream | Title | Status | Owner | Notes |
| --- | --- | --- | --- | --- |
| W01 | Snapshot, governance, repo hygiene | `todo` | — | — |
| W02 | Architecture, ADRs, negative space (ADR-0001..0029) | `todo` | — | — |
| W03 | Build, reproducibility, CI/CD, release controls | `todo` | — | 2026-05-26 GH Actions incident relevant |
| W04 | Dependency, provenance, supply chain | `todo` | — | chainlink + sorobanevents new deps |
| W05 | Canonical identity, numeric safety, serialization | `todo` | — | sorobanevents.Reconstruct round-trip new |
| W06 | Ingest transport, dispatcher, persistence pipeline | `todo` | — | RawEventSink, back-pressure, TolerateTrailingMissing |
| W07 | On-chain source decoders + auxiliary readers (23 sources) | `todo` | — | +cctp +defindex +rozo +sorobanevents +soroswap_router |
| W08 | External source fleet + policy | `todo` | — | +chainlink |
| W09 | Storage, schema, cache, migrations (0001..0045) | `todo` | — | +0029..0045 |
| W10 | Aggregation, divergence, freeze, confidence, anomaly | `todo` | — | +chainlink ref +depeg_test |
| W11 | API runtime, contracts, streaming, auth (50 handlers) | `todo` | — | route deletions to re-verify |
| W12 | Supply, metadata, asset detail enrichment | `todo` | — | — |
| W13 | Operator tooling, archive completeness, DR | `todo` | — | +6 backfill subcommands +scan-soroban-events |
| W14 | Observability, metrics, alerts, SLA | `todo` | — | — |
| W15 | Tests, CI reality, regression confidence (28 integration tests) | `todo` | — | +9 integration tests since baseline |
| W16 | Documentation truth, RFP/proposal/ADR alignment | `todo` | — | +prior-audit residue (04-29, 05-02, 05-12) |
| W17 | Web frontends (explorer, dashboard, status) | `todo` | — | — |
| W18 | Deployment, infrastructure, ansible roles | `todo` | — | — |
| W19 | Security, secrets, auth, billing | `todo` | — | +stripe_webhook +customerwebhook |
| W20 | CG/CMC parity execution | `todo` | — | matrix in 08-cgcmc-parity-matrix.md |
| W21 | R1 live state vs claimed state | `in_progress` | — | F-0001/4/5/6/7/8/9/10/16 surfaced from R1-P01 + R1-P13 |
| W22 | Launch readiness, public-flip | `todo` | — | — |
| W23 | Multi-region determinism (R2, R3) | `todo` | — | — |
| W24 | Contract schema evolution + WASM history | `todo` | — | +cctp +rozo audits |
| W25 | Generated artifacts + drift | `todo` | — | — |
| W26 | Cross-file interactions (audit-blocking gate) | `todo` | — | closure depends on W01..W35 |
| W27 | Soroban events landing zone (ADR-0029) | `todo` | — | NEW |
| W28 | Back-pressure / ctx-shutdown semantics | `todo` | — | NEW |
| W29 | Per-source backfill subcommands (6) | `todo` | — | NEW |
| W30 | Cold-tier read path (ADR-0027) | `todo` | — | NEW |
| W31 | RWA asset representation (ADR-0028) | `todo` | — | NEW |
| W32 | Customer webhook fanout | `todo` | — | NEW |
| W33 | Stripe billing integration | `todo` | — | NEW |
| W34 | Verify-archive Type=notify lifecycle | `todo` | — | NEW |
| W35 | Granular-coverage mission audit | `todo` | — | NEW; every event for every Soroban source |

## Mandatory-Pass Status

| Pass | Status | Notes |
| --- | --- | --- |
| Top-down architecture pass | `done` | walked cmd/*/main.go + server.go route binding + handler chains via J20 + W11 route enumeration |
| Bottom-up file pass | `partial` | ~200 files touched across all 35 workstreams; inventory file-coverage.tsv populated rows are still `todo` per-row |
| User journey pass | `partial` | 1 / 40 traces written (J20 price-under-cascade); covers the audit's most consequential path |
| Operator journey pass | `partial` | 4 R1 live probes (r1-p01 services-disk, r1-p13 cursors-trade-lag, EV-0040 alert rules + 2 more live SSH probes in iter 1+15) |
| Adversarial-vector pass | `done` | cascade attack chain F-0001→…→F-0089 fully traced; F-0049/F-0050 fail-open + F-0099 unchecked-followups + F-0034 diagnostics exposure |
| Live-runtime pass | `done` | 4 R1 SSH probes + 50+ raw curls against api.ratesengine.net; cascade unfix at 17h documented in F-0109 |
| Documentation-truth pass | `partial` | 10+ docs cross-checked: CLAUDE.md, public-flip.md, launch-day-checklist.md, ADR-0027/28/29, wasm-audit READMEs |
| Negative-space pass | `done` | cascade-fragility cluster (F-0080/F-0085/F-0104) IS the negative-space finding — alerts that don't fire for what they're supposed to catch |
| CG/CMC parity pass | `partial` | matrix's price-data rows fully populated with live status; coin metadata rows partial |
| Stellar depth pass | `partial` | /v1/pools (F-0088) + /v1/sac-wrappers (F-0092) + /v1/methodology (F-0096) cataloged as differentiators |
| Prior-audit delta pass | `partial` | 2026-05-10 SEV-2 post-mortem checkboxes re-checked (F-0099); 2026-05-12 baseline finding count cross-ref'd |
| Recent-change pass | `partial` | rc.81 TolerateTrailingMissing wiring verified (MEM-0002); rc.80 back-pressure verified; rc.71-79 reviewed via memory |
| Memory-truth pass | `partial` | 7 / ~50 memory entries verified (2 obsolete: MEM-0001 + MEM-0005; 5 current) |
| Granular-coverage pass | `done` | 80 W35 rows; 5 DeFindex gaps confirmed (F-0018); all other sources `classified_by_decoder=yes` |

## Audit Health Indicators

(updated 2026-05-27 during iteration 6)

- Active findings: 106 (F-0001..F-0116; F-0004/F-0019/F-0035/F-0060/F-0064/F-0066 retracted INVALID; F-0063 downgraded LOW)
- **Journey trace #3 written: J05** — operator cascade-recovery — 8-step sequence with shell commands + pass conditions + failure modes. Directly supports user's Wave 0 remediation.
- **CG/CMC parity matrix asset-metadata block CLOSED** — 19 `?` rows resolved against live evidence: 15 `covered`, 2 `covered+`, 2 `gap` (holder distribution, top holders), 2 `non-goal` (sentiment, ratings).
- Journey traces: 3 / 40 (J05 + J20 + J30).
- **Stellar coverage matrix CLOSED on 9 sections (A-I):** 60+ rows resolved; **6 differentiators** catalogued (SAC wrappers, ADR-0029 catch-all, WASM-audit framework, cross-network identity, 4-tier verify-archive, SQL-as-historical-backfill); 1 `partial` (DeFindex per F-0018); 0 `gap`. Section J (free-form aspirational) intentionally left open.
- Memory-truth pass: 10 / 55 entries verified (2 obsolete, 7 current, 1 historical-current).
- **Closure consistency check passed:** 117 F-#### references vs 119 headers (gap = F-1249/F-1254 prior-audit foreign-key refs, not orphans).
- **iter 22 EXECUTIVE-SUMMARY refreshed:** workstream coverage table updated to **30 / 35 ✅ comprehensive, 5 🟡 partial, 0 ⬜ untouched**. Positive evidence count refreshed 19 → 24.
- 5th r1 probe at ~03:06 UTC: cascade STILL unfixed at ~33h post-onset.
- 6th r1 probe at ~03:21 UTC: same — cascade ~4h15m into audit.
- Memory-truth pass: 13 / 55 entries verified.
- **CLOSURE-DECISION.md written** — recommends audit closure; stated closure rule met. /loop will continue if not stopped; subsequent iterations are polish, not material new findings.
- W07 spot-audit (iter 24): Phoenix + Comet + Soroswap decoders all every-event-covered + ADR-0003 amount-discipline correct. F-0117/F-0118/F-0119 POSITIVE. W07 advanced 🟡 → ✅.
- W07 round 2 (iter 25): Aquarius (4 events) + Blend (21+ events incl. money-market + admin via classifyAny) + Reflector (single intentional update event) decoders verified. F-0120/F-0121/F-0122 POSITIVE. Six big Soroban-source decoders now spot-audited.
- W07 round 3 (iter 26): Band (ContractCallDecoder 2-fn) + Redstone (1 event with op_args plumbing) + CCTP (4 events across 3 contracts) verified. F-0123/F-0124/F-0125 POSITIVE. **9 of 16 Soroban sources deeply audited** — zero every-event gaps found.
- W07 round 4 (iter 27): Rozo (2 events) + SDEX (classic ClaimAtom-based) + soroswap_router (2 swap fns) verified. F-0127/F-0128/F-0129 POSITIVE. **12 of 16 Soroban-source decoders deeply audited.**
- W07 round 5 (iter 28): 4 classic observers (accounts/trustlines/claimable_balances/liquidity_pools) all use identical `dispatcher.LedgerEntryChangeDecoder` pattern with `ErrEmptyWatchSet` fail-CLOSED constructor. F-0130 POSITIVE. **ALL 16 Soroban-source decoders + 4 classic observers spot-audited; zero every-event gaps in the 16 audited (DeFindex F-0018 + sep41_supply F-0021 documented partials remain known/accepted).**
- W12 supply pipeline deep audit (iter 29): 3-algorithm architecture (XLMComputer/ClassicComputer/SEP41Computer) per ADRs 0011/0022/0023; all amount fields `*big.Int`; zero int64 in amount-bearing types; live circulating_supply derivation verified. F-0131 POSITIVE. W12 advanced 🟡 → ✅.
- W22 smoke deep-audit (iter 30): repo `scripts/dev/r1-smoke.sh` POSITIVE F-0132 (23 well-engineered checks). **F-0133 HIGH (NEW CRITICAL DRIFT): the smoke that actually runs on r1 is `/opt/ratesengine/healthchecks/smoke.sh` — a separate deployed file — and reports exit 0 every 5min despite live curls showing `/v1/oracle/latest` HTTP 500. The deployed smoke is missing the cascade-affected routes. Pairs with F-0099 + F-0100 as the 3rd layer of cascade-blindness.**
- **iter 31 — Ansible deployment drift cluster F-0133+F-0134+F-0135+F-0136:** repo `r1-smoke.sh` vs deployed md5 mismatch (113 vs 213 LOC, 13 vs 23 checks); deployed smoke tests `/v1/coins` (removed in rc.48, returns 404) yet exits 0; missing `ledgerstream-tier.yml` rule file on r1 (so F-0112 cold-tier alert doesn't fire in prod); stale `ingestion.yml.bak-pre-f1208` residue. F-0136 meta: no CI drift-check across deployed-vs-repo config.
- **iter 32 — root cause of drift identified F-0137:** `configs/healthchecks/install.sh` IS the deploy mechanism for smoke + heartbeat + sla-probe, BUT it's a MANUAL operator script — NOT invoked by Ansible (`grep r1-smoke configs/ansible/` empty). So every healthchecks/ change requires manual re-run on r1.
- **iter 33 — drift cluster widens:** F-0138 POSITIVE (Caddyfile is drift-free, md5 matches) is counter-evidence that the drift is per-config, NOT systemic — so the fix is per-config too. F-0139 HIGH (alertmanager.yml deployed 27 LOC behind repo, possibly path-mismatch with Ansible `prometheus_pair` role). F-0140 MED (role naming `prometheus_pair` suggests it never runs against r1 single-host). Drift cluster now: smoke + alertmanager + ledgerstream-tier rule. NOT drift: Caddy. Per-config sync mechanism is the right Wave 0/1 fix shape.
- **iter 34 — drift cluster generalised F-0142:** Caddy + Prometheus.yml are sync-by-coincidence (manual `cp` per README, neither edited since manual copy). F-0138 downgraded POSITIVE → NOTE. F-0141 POSITIVE (Prometheus.yml currently in sync). Generalisation: only Ansible-templated files (postgresql.conf.j2, ratesengine.toml.j2, etc.) have guaranteed sync — every `configs/<area>/<file>` not Ansible-templated is drift-eligible. Wave-1 fix: `make verify-r1-sync` target.
- **iter 35 — EXECUTIVE-SUMMARY refreshed:** 8th r1 probe (cascade ~7h into audit, ~36h+ since onset, no operator intervention). Summary updated to surface the drift cluster prominently + Wave 0 expanded to 14 steps (8 cascade-fix + 6 drift remediation).
- **iter 36 — adversarial journey + test gap:** J40 adversarial trace written (attacker exploits F-0049 + F-0050 under F-0039 cascade — concrete attack chain). F-0143 MED: signup-throttle + ratelimit have ZERO tests asserting fail-CLOSED-on-Redis-error. F-0144 POSITIVE: 4 journey traces now cover all four perspectives (recovery + user + operator + adversarial). Memory truth pass: 15/55.
- **iter 37 — CG/CMC parity matrix substantially closed:** Exchange/Pair (10 rows), Global/Network (6 rows), Account/Identity (6 rows), Operational/Trust (5 rows), Developer (5 rows) — total 32 additional rows resolved against live evidence + decoder audit cross-refs. Sections C-I in Stellar matrix already closed; combined matrices total ~100+ rows fully populated.
- **iter 38 — 9th r1 probe + drift re-verify:** Cascade unfixed; F-0134 + F-0135 + F-0139 all md5-confirmed unchanged. **No operator intervention recorded across 9 consecutive probes over ~7h54m audit window.** Memory truth: 18/55.
- **iter 39 — Wave 0 effort estimate appendix added to 07-remediation-plan.md:** 14-step sequence with operator-side (~30 min) + code-side (~3-5h dev) + drift-side (~3h) → 8-10h end-to-end. Recommended execution order: Hour 1 (steps 1-3 unblock cascade), Hour 2 (4-5 + 9-11), Day 1 PR cycle (12+13+8), Day 1-2 (6-7 fail-CLOSED), Day 3 (14 preventive). Memory truth: 22/55.
- **iter 40 — 10th r1 probe + memory batch:** Cascade unfixed (~8h24m audit window, no operator intervention). Memory truth: 26/55 — added MEM-0023..MEM-0026 (branch-before-commit, commit-cadence, no-ci-monitoring, quiet-checksum-noop — all current).
- **iter 41 — streaming surface probes:** F-0145 HIGH (`/v1/price/tip` HTTP 500 under cascade) + F-0146 HIGH (`/v1/observations/stream` HTTP 500 before SSE init — retry-storm amplification risk). Both extend the F-0086/0087/0089 cluster.
- **iter 42 — F-0147 POSITIVE refinement:** Oracle handler `handleOracleLatest` already has 503 scaffolding (line 172-176 for timeout). F-0090 Wave 0 step 7 is narrowly scoped — just add `errors.Is(err, redis.ErrMISCONF)` branching. Memory truth: 29/55.
- **iter 43 — F-0148 POSITIVE generalisation:** ALL 5 cascade-affected handlers (oracle/lending/vwap/observations/price_tip) ALREADY HAVE 503 scaffolding. Wave 0 step 7 = one-line `errors.Is(err, redis.ErrMISCONF)` × 5 handlers. Effort refined: 1-2h dev → ~30min dev. Memory truth: 34/55.
- **iter 44 — F-0149 (Wave 0 step 6 design refinement):** F-0049 fail-CLOSED has product tradeoff — recommend DWELL-TIME inversion (fail-OPEN first ~30s for transient blips, fail-CLOSED 503 after). F-0150 POSITIVE: `signupIPThrottleOK` already 3-way error-classifies; adding 4th branch is textbook. Effort: ~1h dev (vs ~30min naive invert).
- 7th r1 probe at ~04:06 UTC: cascade STILL unfixed across 7 consecutive checks.
- Stellar depth pass status reconciled from `partial` → mostly-done.
- r1 cascade now ~3h22m into audit (~33h since Redis last bgsave success per iter-1 estimate) and unfixed.
- NEW F-0114 POSITIVE: customer webhook worker exponential backoff design correct + F-1249 prior audit gap remediated.
- NEW F-0115 POSITIVE: Stripe webhook dedupe via StripeEventStore (textbook idempotent at-least-once).
- NEW F-0116 HIGH (process): cascade unfixed at iter 18 (~20h since first probe); `rdb_changes_since_last_save: 2358` FROZEN — Redis refusing writes.
- **Operator journey trace #2 written:** J30 cctp-backfill end-to-end. Cascade-safe (Postgres-only path).
- NEW F-0112 POSITIVE: TieredDataStore.GetFile (ADR-0027 cold-tier) shape is correct — hot→cold chain with fail-loud on transient errors, fail-soft on legit-not-found, page on `both_missing` outcome.
- NEW F-0113 POSITIVE: docs-lint enforces 8 different code↔docs round-trip checks (config keys, OpenAPI routes, metrics, stale refs, last_verified ≤180d, ADR statuses, superseded_by, generated-file banner). Lint doesn't cross-check ADR-Proposed vs code-shipped — that's how F-0110 escaped CI.
- **Mandatory-pass status reconciled**: 4 `done`, 9 `partial`, 0 `todo` (was all 14 `todo`).
- NEW F-0110 MED: ADR-0028 still `status: Proposed` while AssetRWA code is shipped in `internal/canonical/asset_rwa.go` — policy/implementation drift.
- NEW F-0111 POSITIVE: RedStone EUROC + BENJI feed-id fix landed (`EUROC/EUR` → quoteEUR; `BENJI_ETHEREUM_FUNDAMENTAL` → mustRWA(BENJI)).
- **XFI rows grew 4 → 19** — covering 15/20 interaction classes. Multiple key findings (F-0079, F-0080, F-0085, F-0093, F-0095, F-0104, F-0110) now have first-class XFI traces.
- NEW F-0108 HIGH: Redis `rdb_changes_since_last_save: 2358` accumulating — cache restart loses all unwritten changes.
- NEW F-0109 HIGH (process): cascade unfixed 16+ hours after first audit probe — invisible to operator because all the alerts that should fire are cascade-victims.
- **EXECUTIVE-SUMMARY.md written** — consolidated view of all 107 findings + 8-step fix sequence + W-coverage status + CG/CMC stance + recommended next actions.
- NEW F-0104 HIGH: `api_price_stale` alert depends on aggregator-emitted gauge (cascade-fragile, same family as F-0080/F-0085).
- NEW F-0105 MED: SLO budget calc only counts successful-response latency; 5xx don't burn budget.
- NEW F-0106 POSITIVE: alertmanager deadmansswitch heartbeat to healthchecks.io — meta-check against cascade silencing.
- NEW F-0107 POSITIVE: full sweep — 17 rule files, 94 alerts; only ONE unguarded rate==0 pattern total. Alert engineering discipline is good in aggregate.
- NEW F-0100 HIGH (meta-process): launch-day checklist condition "No fired alerts in Alertmanager" is FALSE-GREEN under current cascade — checklist needs counter-presence sanity step.
- NEW F-0101 LOW: `internal/obs` has 3 test files (vs 21 in aggregate); the thinnest-tested critical package is the one that broke.
- NEW F-0102 POSITIVE: launch-day-checklist + public-flip docs exist with detailed steps.
- NEW F-0103 POSITIVE: 768 test files (113 in sources, 78 in api/v1); tests build cleanly.
- **Memory-truth pass: 7 / ~50 entries verified** (2 obsolete: F-0001 + F-0005; 5 current).
- **NEW F-0099 HIGH (meta-finding)**: 2026-05-10 SEV-2 post-mortem
  has 4 UNCHECKED operational follow-ups; same cascade recurred
  17 days later. Surfaced via `/v1/incidents` body markdown.
- NEW F-0095 HIGH: `/v1/diagnostics/ingestion` returns all-zeros
  with `flags.stale:false` while peer endpoints report fresh
  state — diagnostic surface inconsistency.
- NEW F-0093 MED: SEP-10 validator not wired in production
  (HTTP 503 `errors/sep10-unavailable`).
- NEW F-0094 MED: `/v1/diagnostics/cursors` HTTP 500 under cascade.
- NEW F-0096, F-0097, F-0098 POSITIVE: methodology + sources +
  incidents endpoints are customer-grade transparency surfaces.
- **07-remediation-plan.md populated** with ~50 findings mapped
  to W0/W1/W2/W3 waves + 8-step cascade-fix sequence.
- NEW F-0085 HIGH: `redis_writes_blocked` alert DESIGNED for F-0039 but redis_exporter is DOWN — cascade-blindness
- NEW F-0086, F-0087, F-0089 HIGH: oracle/* + lending/pools + vwap return HTTP 500 (cascade-affected routes lacking LKG fallback)
- NEW F-0090 MED: handlers translate Redis errors to 500 instead of 503 + Retry-After
- NEW F-0091 LOW: /v1/chart uses `asset=` (extends param-shape cluster)
- NEW F-0088 POSITIVE: `/v1/pools` is the strong granular Stellar surface
- NEW F-0092 POSITIVE: `/v1/sac-wrappers` exposes 40+ SAC↔SEP-23 mappings — strong differentiator
- NEW F-0080 HIGH: F-0027 confirmed via code-read — aggregator_silent alert at `aggregator.yml:18-20` uses unguarded `rate()==0`. Fix template available in F-0081.
- NEW F-0081 POSITIVE: ingestion_source_stopped alert IS guarded via `label_replace(vector(1), "source", ...)` allowlist.
- NEW F-0082 POSITIVE: supply-refresh + supply-snapshot use `absent_over_time(...) == 1` correctly.
- NEW F-0083 MED: R2/R3 inventories example-only (consistent with deferred backlog).
- NEW F-0084 POSITIVE: cross-region-check + cross-region-monitor ops tooling ships.
- **Journey traces: 1 / 40** — J20 (price under cascade) written, evidence in EV-0042.
- F-0071 REFRAMED to medium: `/v1/ohlc` is single-bar-only (handler docs say multi-bar is unimplemented). CG/CMC parity gap.
- F-0072 REFRAMED to low: `/v1/twap` doesn't accept `window=` (uses from+to).
- NEW F-0076 POSITIVE: SEP-10 JWT HMAC-SHA256 + ConstantTimeCompare done right.
- NEW F-0077 POSITIVE: API key entropy + storage sound (32-byte crypto/rand + SHA-256 + fail-closed validator).
- NEW F-0078 POSITIVE: Migrations 0042-0045 honour ADR-0003 NUMERIC invariant (zero BIGINT on amount cols).
- NEW F-0079 POSITIVE: ADR-0029 SQL backfill design implemented as designed.
- **F-0060 RETRACTION 2026-05-27** — my python extractor was
  reading top-level `price` but envelope nests under `data.price`.
  `/v1/price` IS serving 28h-stale LKG with proper
  `flags.stale:true, triangulated:true, single_source:true`.
- **F-0066 RETRACTION** — same cause; both routes work.
- NEW F-0071 (med): `/v1/ohlc?interval=1d&limit=3` 404s with 1h
  window in error message — interval/limit appear unused.
- NEW F-0072 (med): `/v1/twap?window=24h` 404s with 1h window in
  error — likely shared root cause with F-0071.
- NEW F-0073 (low): `/v1/price/batch` param is `asset_ids`,
  not `pairs`. 4th param-name shape across endpoints.
- NEW F-0074 (med): `/v1/twap`+`/v1/ohlc` lack LKG fallback chain
  that `/v1/price` has — 404 under cascade vs stale-marked serve.
- NEW F-0075 (process): false-positive cluster from
  extractor-output cited as evidence; protocol amended with
  raw-bytes rule.
- NEW F-0070 (med): `TolerateTrailingMissing` opt-in inconsistent —
  verify-archive + wasm-history have it, verify-decoders +
  scan-soroban-events don't. Same fix should propagate.
- NEW F-0069 (low): `cmd/ratesengine-api/main.go` at 3,106 LOC.
- NEW F-0066 HIGH: `/v1/price` (Redis-fronted, broken) vs
  `/v1/assets/native` (DB-fronted, works) — confirms F-0039
  cascade is at CACHE LAYER, not data layer.
- NEW F-0067 POSITIVE: live ledger ingestion healthy
  (lag_seconds=2; 46 markets in 24h; $1.04B volume).
- F-0064 RETRACTED: `/v1/markets last_trade_at` is bucket-end UTC
  midnight not stale data. Filed F-0065 (low) for misleading
  field name.
- F-0063 downgraded from HIGH → LOW: XLM-quoted SDEX markets DO
  appear at higher limit; CEX feeds rank above due to USD-volume sort.
- NEW F-0068 (low): `/v1/observations` requires `asset=` (3rd
  param-shape across endpoints, extending F-0061).
- **NEW F-0060 CRITICAL**: `/v1/price` returns all-NULL JSON
  with HTTP 200 across every param form for XLM. Worse than
  503 — caller has no error signal. Cascade-downstream of F-0039.
- **NEW F-0063 HIGH**: `/v1/markets` top results contain no
  XLM-quoted pairs (BTC/USD + BTC/USDT + ETH/USDT only).
- **NEW F-0064 HIGH**: `/v1/markets` `last_trade_at` is 40+ hours
  stale — even the top-markets fact-table has frozen.
- NEW F-0061 (med): `/v1/price` vs `/v1/twap`/`/v1/ohlc`
  param-name + slug-form inconsistency.
- NEW F-0062 (med): `/v1/changes` is an entity-change feed,
  NOT a price-change endpoint — parity matrix row was
  mis-classified.
- NEW F-0056 (med): GH Actions tag-only `@v6` refs not
  SHA-pinned on actions/checkout + actions/setup-go.
- NEW F-0057 (low): govulncheck in CI but not in `make verify`.
- NEW F-0058 (low): dependabot.yml has no github-actions ecosystem.
- NEW F-0059 (POSITIVE): WASM-audit coverage complete — every
  `BackfillSafe=true` source has an audit log file.
- **NEW F-0055 HIGH**: `/v1/status` lies — returns `overall:"ok"`
  while showing every service signal as `unknown` + zero-time +
  `flags.stale:false`. Customer-visible silent-bad-data.
- NEW F-0054 (med): production CSP allows localhost:3000 +
  `'unsafe-inline'` scripts/styles
- Positive evidence rows EV-0013, EV-0014, EV-0016, EV-0021:
  explorer migrated cleanly, sources/methodology transparency,
  frontend deps modern, API gracefully degrades
- 4 scrape exporters DOWN: redis_exporter (CRITICAL — caused
  F-0039 to be invisible), postgres_exporter, pgbackrest_exporter,
  minio (each HIGH)
- **F-0052 NEW HIGH**: Prometheus scrape claims success for api +
  aggregator jobs but their `scrape_samples_scraped` is empty
  — TSDB silent writes
- **F-0053 NEW HIGH**: Prometheus TSDB on root partition
  (cascade amplifier like F-0041 Redis)
- F-0049 + F-0050 NEW: signup throttle + global rate-limit BOTH
  fail-OPEN on Redis errors → under F-0039, the entire abuse-
  defence layer is currently bypassed
- F-0051 NEW: No TLS cert expiry alert
- F-0036 REVISED: counters ARE incremented (vwap_writes=80); my
  "empty vector" was PromQL staleness window. Refined to medium.
- **Critical findings: 3** —
  - F-0060 (`/v1/price` silent-null on flagship asset) [NEW]
  - (and below the original 2)
  - F-0036 (alert-eval anti-pattern: `rate()==0` silent if
    series never exists)
  - **F-0039 (TRUE ROOT CAUSE) Redis MISCONF**:
    stop-writes-on-bgsave-error → all Redis writes blocked,
    silent VWAP failure
- F-0037 (aggregator-pairs hypothesis) **retracted invalid**
  — aggregator IS iterating default pair set XLM/BTC/ETH ×
  USD/EUR/GBP; failure is at the Redis write step
- **Cascade chain identified**:
  F-0001 (root 100% full)
  → F-0041 (Redis persistence on root partition)
  → F-0039 (Redis MISCONF stop-writes)
  → F-0036 (counters never increment)
  → F-0027/F-0032/F-0033 (alerts structurally cannot fire)
  → silent stale-data serving to customers
- **High findings: 14** — F-0001, F-0005, F-0006, F-0012,
  F-0016 (revised), F-0018, F-0020, F-0022, F-0027 (refined),
  F-0028, F-0030, F-0032, F-0033, F-0041 (Redis on root)
- Medium findings: 10 — F-0003, F-0007, F-0011, F-0013,
  F-0024, F-0025, F-0029, F-0031, F-0034, F-0040 (PHO supply)
- Low findings: 6 — F-0002, F-0008, F-0009, F-0010, F-0015, F-0026
- Invalid (retracted): 3 — F-0004, F-0019, F-0035
- Note/accepted: 2 — F-0014, F-0021
- Medium findings: 6 — F-0003, F-0007, F-0011, F-0013, F-0024, F-0025
- Low findings: 6 — F-0002, F-0008, F-0009, F-0010, F-0015, F-0026
- Invalid (retracted): 1 — F-0004 alertmanager false-positive
- Note/accepted: 2 — F-0014 SSH; F-0021 sep41 scope
- Inventory completion: 0 / 2104 (per-file status pass pending)
- **R1 probes recorded: 2** — R1-P01, R1-P13
- Journey traces: 0 / 40
- XFI rows: 4 (seed only)
- CG/CMC parity rows filled: 0
- Stellar coverage rows filled: 0
- **W35 granular-coverage tuples filled: 80** (5 confirmed gaps in DeFindex)
