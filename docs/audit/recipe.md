# Audit recipe — Stellar Index

> The project-specific overlay for `/audit`. Generic checklists know what *kinds* of bugs exist; this file knows where *this* project keeps them. Authored cold from primary evidence by five independent code-mapping agents (money/pricing · API/auth · ingest/storage/completeness · extraction-derived entry-points/config · web+plans) plus direct spot-verification. Re-verify every claim against current code before trusting it — this file drifts too.

- **Last derived:** 2026-07-16 against commit `f84e2d0b`. **HEAD later advanced to `4d034432`** (a concurrent agent's `feat(explorer): surface the two-axis completeness verdict on diagnostics`, pushed to main). `git diff f84e2d0b 4d034432` touches ONLY `CHANGELOG.md` + `web/explorer/src/app/diagnostics/{CoveragePanel,page}.tsx` — the money/ingest/api/auth/storage/ops surface is byte-identical, so all engine-side findings stand at HEAD. The one change: the "two-axis verdict UI MISSING" gap is now **CLOSED** by 4d034432 (reviewed sound — reads `lake_complete`/`lake_complete_sources`/`v.lake_complete` from the generated coverage types, correct ADR-0034 tooltips, `lake_complete=true/complete=false` handled honestly).
- **Derived by:** cold multi-agent recon (see `audit-2026-07-16/recon/*.md` for the full evidence dumps this file distils)
- **Companion:** `docs/audit/repo-prep.md` — gates, CI-parity checklist, commit convention, deploy-freeze constraints. Read it before remediating.
- **Repo layout:** Go monorepo (single module `github.com/Stellar-Index/StellarIndex`, ADR-0005), 1,339 Go files. `internal/` private, `pkg/client` public. 108 migration up/down pairs. Two Next 16 apps under `web/`.

---

## 1. Architecture at a glance

- **Stack:** Go (primary), TimescaleDB/Postgres (served tier), ClickHouse (raw lake, ADR-0034), Redis (cache + rate-limit + pub/sub), MinIO/S3 (galexie XDR archive = ultimate source of truth). Next.js 16 static-export explorer + status page. Ansible-managed single host.
- **Topology — ONE unclustered Hetzner host (R1).** indexer + aggregator + api + Postgres + ClickHouse + Redis + MinIO + Galexie (+2 embedded captive-core) all colocated (`configs/ansible/inventory/r1.yml:157`). R2/R3 are **NOT provisioned** (only `*.example.yml`; deploy region enum = `[r1]`). HA roles (haproxy/patroni/redis-sentinel) exist but **no playbook invokes them** — unwired designs. **Redis has NO AUTH** (network-isolation-dependent). This single-instance reality means: in-process assumptions are currently safe but several are only convention (hashdb "not safe for concurrent use"; one-indexer cursor presumption).
- **Six binaries:** `stellarindex-{indexer,aggregator,api,ops,migrate,sla-probe}`. indexer/aggregator/api are always-on daemons; ops is a 55-subcommand admin CLI (backfill/replay/ch-*/verify-*/seed-*); migrate is golang-migrate (advisory-locked); sla-probe is a periodic load driver. (`cmd/tmpxdrdump` is an empty dead dir.)
- **Data authority per entity:** trades → PG `trades`; prices → PG CAGGs `prices_1m…1mo`; ledger_entry_changes → **ClickHouse only** (sole store); soroban_events → in-transition (projector reads CH `contract_events` by default; PG `soroban_events` legacy, decommission-pending #39); completeness verdict → PG `completeness_snapshots`; API keys/accounts → PG (Redis is read-through cache); rate-limit + usage counters → **Redis is live-authoritative**; supply → PG by deliberate choice (not CH's broader supply_flows).
- **Subsystem seams (interaction-bug concentration):** ledgerstream→dispatcher→**8 parallel sink workers** (no per-source ordering, PK-dedup safe); dispatcher↔projector dual-writer (SinkMode truth table, ON CONFLICT resolves the race); aggregate→Redis→api (typed cache keys); openapi→pkg/client (HAND-WRITTEN, reflection-test-guarded) vs openapi→web types.ts (codegen, CI-drift-gated); completeness (ADR-0033 data verdict) vs archivecompleteness (ADR-0017 file structural) — distinct systems.

## 2. Entry points (flow-unit seeds)

Full extraction in `audit-2026-07-16/recon/entrypoints.md`. Summary:

| Entry point class | Count / examples | Auth | Mutates |
|---|---|---|---|
| Public GET /v1/* (pricing/catalogue/explorer/coverage) | ~100 routes | OPT (anon 60/min, key raises tier) | none |
| SSE streams | /price/stream, /price/tip/stream, /observations/stream, /ledger/stream, /oracle/streams | OPT | none |
| Account self-service | GET/POST/DELETE /v1/account/keys | KEY | keys |
| Admin | POST /admin/keys, PATCH /admin/accounts/{id}, status-notices | OP (X-Reason, audit-logged) | keys/tiers/banners |
| Dashboard (session) | /dashboard/keys, /webhooks (SSRF-checked), /price-alerts | SESS | keys/webhooks/alerts |
| Undocumented staff | GET /v1/account/admin/lookup (CI-exempted) | SESS+STAFF | none (PII read) |
| Signup / billing | POST /signup, GET /signup/verify, POST /webhooks/stripe | OPEN throttled / SIG | accounts/tiers |
| SEP-10 | /auth/sep10/{challenge,token} | OPEN (by design) | none |
| Utility (non-/v1) | /, /robots.txt, /.well-known/security.txt, /metrics | NONE/LOOPBACK | none |
| Edge (CF Pages Function) | web/explorer/functions/og/[[path]].js — server-side fetch at request time | OPEN | none |
| CLI subcommands | 55 ops + 6 migrate + supply sub-dispatch | operator | many DESTRUCTIVE (trim/rebuild/backfill) |
| Timers | 11 systemd + 6 ansible + 3 healthchecks + 4 GH cron | — | some (backfill/trim/rollup) |
| Background workers | ~14 aggregator + ~12 api + ~11 indexer goroutines | — | rollups/caches/webhooks/reaper |

Entry-point closure: 120 registered method+path patterns; 115 match OpenAPI 1:1, 1 intentionally-undocumented staff route, 4 non-/v1 utility. No spec path lacks a handler (planned_regex `^$`).

## 3. Money flows

Stellar Index moves no *user* money, but **served prices/amounts/supply/market-caps ARE the product** — value-correctness is the money surface. Full map in `audit-2026-07-16/recon/money-pricing.md`.

- **Ingress (price data):** on-chain trades (SDEX + Soroban DEXes via dispatcher/projector), CEX streamers (Binance/Bitstamp/Coinbase/Kraken — VWAP inputs), FX pollers (massive active; ecb/polygon/exchangeratesapi), oracle feeds (Reflector/Redstone/Band), aggregator refs (CoinGecko/CMC/CryptoCompare — divergence only).
- **Internal computation:** VWAP (exact big.Rat Σq/Σb, decimals-normalized) → Redis; CAGGs (SQL, per-row-rounded, unguarded — patched by GuardServedVWAP at serve time); TWAP/OHLC; triangulation; stablecoin→fiat proxy (aggregation-time); confidence; anomaly-freeze; supply Alg-1/2/3.
- **Egress (served values):** /v1/price*, /history, /chart, /ohlc, /vwap, /twap, /markets, /assets*, /oracle*, supply, market-cap. Plus the explorer + status page.
- **Value representation invariant:** *big.Int/big.Rat in Go → NUMERIC in SQL → **string** in JSON**. Amount type enforces this. Violations (float64 landing in served values) are the recurring money-bug class — see §7/§8.
- **Actors who can move "value" (i.e. change served numbers):** the ingest pipeline, the aggregator, ops re-derive commands (ch-rebuild/projector-replay/backfill — DELETE-then-derive on corrections), operator config (peg sets, watched sets, min_usd_volume).

## 4. Trust boundaries & threat model

| Adversary | Capability | Goal | Priority here |
|---|---|---|---|
| Unauth internet user | any public GET, crafted asset/contract/account params | DoS (unbounded scans), scrape, find foothold | **HIGH** — the whole API is anon-reachable; DoS levers are the live risk (§7) |
| Authenticated key/dashboard user | valid key/session, all self-service actions | reach another tenant's keys/usage (IDOR), escalate tier | MED — 8 IDOR candidates refuted in prior audit; re-verify |
| Malicious data issuer | controls on-chain contract/SEP-1 metadata (ORG_NAME, home_domain, image, asset code) | stored-XSS in explorer, issuer impersonation, look-alike contract injecting fabricated trades, client-side SSRF via logo | **HIGH** — the primary adversarial-DATA surface; gating (ADR-0035) + XSS chokepoints are the defense |
| Compromised upstream feed | controls a CEX/FX/oracle price | poison VWAP, manufacture/suppress divergence | MED — class-filter + freeze + divergence; but CoinGecko ref lacks staleness gate |
| Compromised dependency | in-process code | reach secrets/CI | MED — SHA-pinned deps, read-only CI token |
| On-path / operator-adjacent | reach internal ports on the host | scrape /metrics, hit unauthed Patroni/Redis | MED — depends entirely on host network isolation (Redis no-AUTH, /metrics loopback gap on indexer/aggregator) |
| Insider with repo/CI access | open PRs, edit baselines | ship past gates | MED — main-protection is a GitHub setting (not repo-verifiable); baseline-growth guard exists |

- **Assets:** integrity of the served data product (the #1 asset — "code-correct ≠ data-correct" is the standing gap), API-key secrets, customer PII (email, usage), availability.
- **Sensitive data:** API keys (hashed, PG), sessions, customer emails, Stripe events; secrets in ansible-vault (gitignored) → /etc/default/stellarindex.

## 5. Invariants (with enforcement tiers)

> The load-bearing section. Tier = **DB** (unbypassable) > **RT** (runtime, bypassable by new code) > **T** (test asserting the failure case) > **W** (watcher/reconciler, post-hoc) > **CI** (lint gate) > **C** (convention, weakest). Tier is set by the **weakest writer**. Hunt for tier demotion. Claims marked ⚑ are double-found by ≥2 independent agents (strongest static confidence); ⚠ marks a verify-item.

### INV-1 — i128/u128/u256 never truncates to int64/float (ADR-0003)
- **Tier: CI + T** (on-chain amounts) / **C** (off-chain magnitudes). ⚑
- Enforced by: `scripts/ci/lint-i128.sh` (grep `int64(x.Lo)`), `scripts/ci/lint-migrations.sh` (SQL side), and the strong `TestI128TruncationGuard` (repo-wide go/types walk, `internal/canonical/i128_truncation_guard_test.go`, `//i128:ok` escapes, zero non-test escapes). Every Soroban decoder routes through `scval.AsAmountFromI128/U128/U256`.
- **Weakest link:** the guard catches *conversion shapes*, not float round-trips of already-decoded JSON numbers — coinbase backfill + aggregator pollers transit float64 legally (§7 trap 1).

### INV-2 — every money column is NUMERIC; money crosses JSON as a string
- **Tier: DB** for columns / **C** for two writers. ⚑
- `lint-migrations.sh` checks column *types*; `canonical.Amount` marshals to JSON string. **Weakest writer:** `fx_quotes.RateUSD/InverseUSD` and `price_source_contributions.Volume/Weight` are float64-in-Go into NUMERIC columns (lint passes, precision capped at float64). `/v1/changes` serves money as JSON *numbers* — the one API violating strings-only.

### INV-3 — derived money values must be re-derivable ⚑⚠ (VIOLATED — trap, tier: NONE)
- `trades.usd_volume` and `asset_supply_history` both use **ON CONFLICT DO NOTHING** with no amount/price column in any PK (repo-wide sweep: zero amount/price/volume in any PK/UNIQUE). A replay/re-derive computing a *corrected* value for the same identity is silently absorbed; only DELETE/TRUNCATE + re-derive fixes it. Every downstream `sum(volume_usd)` inherits bad values permanently. This is the **#1 recurring failure class** (RFC-1).

### INV-4 — one writer per Soroban-derived data domain (ADR-0031/0032)
- **Tier: C** (convention). ⚑
- RT + T for the two *live* writers (projector sink + dispatcher events-goroutine; SinkMode truth table `internal/pipeline/sink.go:104-127`; lockstep AST test). But **ops re-derives (ch-rebuild/ch-reproject/projected-rebuild/backfill/COPY-merge) are convention** — absorbed by PK dedup, and `projected-rebuild -allow-live-overlap` is an explicit bypass. Also `routed_via.go:91` is a second live UPDATE to `trades` racing projector inserts. `classic-movements-backfill` writes **outside** HandleEvent, the lockstep guard, AND the ADR-0033 catalogue (verification off by default).

### INV-5 — coverage is data-derived, verdict never regresses, ledgers contiguous+hash-chained (ADR-0033)
- **Tier: W** (watcher, post-hoc only). ⚑
- Substrate census is decoder-independent (2nd LCM walk); no-regress WHERE guard (CS-083) on the verdict; contiguity/hashchain/hashdb detectors. **Weakest links:** ingest never *blocks* on a gap; `ledger_ingest_log` + cursor advance on **enqueue not durable write** (crash loses buffered events, cursor stays advanced — RFC-2); the **projector advances its cursor past sink failures** while its own doc claims it doesn't (`projector.go:91-93` vs `:393-401`) ⚑; hashdb is opt-in (default off); `retentionStart` is a stale hardcode contradicting the repo's own decision doc (§7).

### INV-6 — money amounts > 0, oracle price > 0 / decimals ≤ 38, supply ≥ 0
- **Tier: DB** (CHECK constraints) + RT (Validate). ⚠ **Weakest link:** `BatchInsertTrades` does NOT call Validate (relies on the sink) — DB CHECK holds the amount>0 line, but Validate-derived properties (lowercase tx_hash) are convention on the batch path. Supply freshness gate has an F-1320 dormancy exception that accepts a producer stalled at the same ledger for two ticks.

### INV-7 — serve only CLOSED buckets (ADR-0015); non-7dp assets never leak an unscaled price
- **Tier: RT** (closed-bucket) / **W+DB+RT with convention gaps** (decimals). Closed-bucket via sargable `bucket <= now()-interval` everywhere; no failure-case test found. AdjustPrice applied at most endpoints but **NOT** `/v1/price/at`, `/v1/price/changes` prices, or market-cap `10^7` hardcodes (§7 trap 7).

### INV-8 — contract identity gating (no attribution on topic bytes alone) (ADR-0035/0040)
- **Tier: RT + T.** All Soroban decoders gate `Matches()` on contract identity (factory/childgate/curated); comet is a one-pool allowlist. A new `Matches()` on topic bytes alone is forbidden. Re-verify each gate + its foreign-contract-reject test (prior CS-026 was this class).

### INV-9 — no Horizon / no stellar-rpc in prod ingest; xdr scoped to scval
- **Tier: CI** (lint-imports.sh, 4 rule families, baseline shrink-only + growth-guard). Verified live (exit 0, 6 grandfathered).

### INV-10 — auth: keys hashed + constant-time, ownership checked before mutation, rate-limit fail-closed
- **Tier: RT** (+ prior-audit T). ⚠ Re-verify: constant-time key compare, empty-subject-fails-closed, ownership before every mutation (8 IDOR candidates refuted previously), and whether rate-limit is fail-**open** or fail-closed when Redis is down (prior audit said "fixed" — re-derive).

### Weakest links (ranked — where a regression hides)
1. **INV-3** (re-derive trap) — no tier at all; a wrong money value is permanent. Any new derived-value column inherits this unless it enters a PK or gets DELETE-first re-derive tooling.
2. **INV-4/INV-5 projector cursor-past-sink-failure** — doc says one thing, code does another; silent projection loss caught only by reconcile.
3. **INV-1 off-chain float round-trips** — the guard's blind spot; fx_quotes/pollers/coinbase transit float64.
4. **INV-5 enqueue-not-durable cursor advance** — crash-window silent loss (ADR-0041 accepts it, but it's the fragile edge).
5. **One-heavy-job-at-a-time = convention only** (`run-heavy-job.sh` caps resources, takes no lock) — acute right now with Phase 0 running.

## 6. Checklist deltas by dimension

| Dimension | Additions (project-specific) | N/A (justified) | Elevated |
|---|---|---|---|
| COR | closed-bucket contract; direction-combine math; decimals normalization at every serve path | | pricing read-paths |
| INT | 8-worker sink ordering; dispatcher↔projector dual-write; the 5 lockstep sites (`go test -run TestLockstep`) | | ingest seams |
| MNY | i128/NUMERIC/string; VWAP/TWAP/OHLC exactness; per-source decimals (7 on-chain / 1e8 CEX / 1e6 FX); stablecoin-peg late-binding; re-derive trap; USD-volume gate scaling | | **ALL served-value paths + the re-derive trap** |
| SEC | issuer-controlled data (SEP-1 ORG_NAME/home_domain/image, asset code) as XSS/SSRF vector; contract-identity gating; XFF trusted-proxy; /metrics loopback; auth_mode/CORS defaults | | pricing/aggregation math (mostly integer, injection-clean) |
| CON | single-host in-process assumptions; hashdb single-process; projector per-source cursors; heavy-job-no-lock; Redis fixed-window under concurrency | | multi-machine rate-limit (single host today) — but note the design *claims* multi-region |
| DAT | ON CONFLICT DO NOTHING never-corrects; RMT eventual dedup; enqueue-not-durable cursor; classic_movements outside guards; ledger_entry_changes live-sink gap (G12-03) | | |
| REL | crash-before-drain loss; failed-batch-insert dropped with Warn (no dead-letter); backpressure (block-and-retry on-chain vs drop CEX vs drop CH-live); no circuit breakers | | |
| PRF | missing query timeouts (/ohlc,/twap,/vwap); validation-skipping DoS levers (/holders, /contracts/{id}/interactions); sargability; account_movements extreme-address; cold-read catalogue | | |
| API | hand-written pkg/client (reflection-test); 3-generator OpenAPI regen; cacheable-denials (no-store before WriteHeader); two-wire-shape /assets/{slug}; consistency-tier-by-URL | | |
| DEP | SHA-pinned actions + tool tarballs; go-stellar-sdk (not cdp-pipeline-workflow — known-buggy); no Horizon | | |
| CFG | env-override trap (STELLARINDEX_POSTGRES_DSN); two conflicting *_env conventions (name vs value); dangerous defaults (0.0.0.0 bind, [] CIDR, ["*"] CORS, region.id=r1); feature-flag matrix | | |
| TST | proven-red DB-backed tests for money/auth (testcontainers `make test-integration`); lockstep AST tests; failure-case assertions (not just happy-path) | | |
| OBS | /metrics loopback asymmetry; ansible-drift ≤13-change allowance; two divergent systemd-unit copies; runbook-per-alert requirement | | |
| HLT | dead code (api_usage_events, cache-prime, tmpxdrdump, windowUSDVolume, PG classic_movements store); doc-drift (6 CLAUDE.md/arch-doc false claims); stale ROADMAP rows | | |
| DOC | plan validity (ADRs/ROADMAP/perf-todo vs code); the two-axis verdict API-done/UI-missing split; #34 site promises vs backend; Alg-2 readers trap; #39 reader-inventory gap | | **the whole plan surface — explicit audit target this run** |
| ACC | explorer keyboard/focus-trap/screen-reader (prior LC-050/051/052); dashboard form-error announcement | | |
| I18N | fiat/currency/locale handling in pricing; the fiat-as-asset product-coherence issue (prior LC-001) | | |
| UXP | explorer entity pages; two-axis verdict UI (missing); stale "v1 in coming weeks" copy; dormant-pair stale=false | | |
| MBL | | N/A — no native/mobile app (static web only) | |
| LLM | | N/A — no LLM in the product (counter-searched: no anthropic/openai/genai imports) | |
| INF | single-host ansible; Caddy+CF trusted-proxy; run-heavy-job cgroups; backup/restore (pgbackrest); the HA-plan-vs-reality gap | | |
| CID | main-protection (GitHub-side, unverifiable from repo); baseline-growth guard; deploy stage→backup→rollback; migrations-never-rolled-back | | |
| PRV | customer email/usage PII; the undocumented staff PII lookup; IP in logs/rate-limit keys | | |
| DOM | Stellar protocol semantics (P23/CAP-67 events, Soroban schema evolution, per-protocol event arity — Phoenix 8/swap, Comet shared topic); supply algorithm domain truth | | per-protocol decoders |
| MDL | | N/A — no trained/predictive model (VWAP/MAD are deterministic statistics, audited under MNY) | |
| TNS | | N/A — no user-to-user interaction (read-only data product + API keys) | |
| NTF | customer webhooks (HMAC, SSRF-guarded, delivery worker); price alerts; transactional email (magic-link, signup) | | |
| AGT | the 2026-07-10 push (two-axis, verify-lake, classic-movements, ced-v2); anything doc-says-X-code-does-Y | | newest surface |

> N/A challenges (adversarial): MBL confirmed (no `ios/`/`android/`/react-native). LLM confirmed (grep clean). MDL — "VWAP is a model?" No: it's deterministic aggregation, no train/serve/eval; audited under MNY. TNS — API keys aren't user-to-user; no messaging/UGC surface. If a sweep contradicts any of these, reopen it.

## 7. Tech-specific traps

1. **float64 landing in served money** — fx_quotes, /v1/price fiat cross-rate (labelled "vwap"), fiat market-cap chart (vs crypto's big.Rat), /v1/changes (JSON numbers), coinbase backfill (only VWAP-contributing float path). The i128 guard does NOT catch these.
2. **ON CONFLICT DO NOTHING re-derive trap** (INV-3) — no amount/price in any PK; replays never correct values.
3. **CS-040 decimals regression (GATED high)** — `orchestrator.go:1268-1271` hardcodes 8 for fiat:USD; FX registry stamps 6; the corrector `windowUSDVolume` is dead code. Latent while connector-FX disabled; re-enabling polygon-forex/exchangeratesapi (both IncludeInVWAP:true) → ~100× volume-gate understatement + mixed-scale VWAP.
4. **CAGG `twap` column is NOT time-weighted** (equal-weight mean); real TWAP only at 1h/1d.
5. **Direction-combining** (SDEX both-orientations) uses a trade-count-weighted mean of {vwap, 1/vwap_flipped} — provably ≠ exact union VWAP.
6. **Trade-level outlier filter is σ/z-score (masking-vulnerable), not MAD**; MAD guard covers only 3 serve-time sites, not the published VWAP.
7. **Non-7dp decimals gaps** — /v1/price/at, /v1/price/changes prices, market-cap `10^7` hardcode, populateMarketCap default 7. A confirmed non-7dp token's market-cap/FDV is mis-scaled even where its price is normalized.
8. **retentionStart stale** — `compute_completeness.go:196` floors trades reconcile at tip−1.5M "~90d" but `trades` has no retention; the served axis can't flag history-that-should-be-served-but-isn't.
9. **Postgres sargability** — function-of-indexed-column in WHERE (`bucket+INTERVAL<=now()`) forces per-chunk scan (prior p95 50→400ms); rewrite `col<=now()-INTERVAL`.
10. **ClickHouse RMT eventual dedup** — a read that neither FINALs nor dedups over-counts un-merged parts (partitions 25/45/62 called out).
11. **Env-override trap** — `STELLARINDEX_POSTGRES_DSN` silently replaces the TOML DSN; running ops re-derives without sourcing `/etc/default/stellarindex` → auth failure. Two conflicting `*_env` conventions (name vs value).
12. **Jinja string-truthiness** — `when: <var>` on a string needs `| bool` (the F-1220 migrations_skip class).
13. **SEP-1 issuer data** — ORG_NAME/home_domain/image/asset-code are attacker-controlled; XSS chokepoints are `serializeJsonLd`/`isSafeHomeDomain`/`isSafeHref` — re-test on any new attacker-string render. `isSafeImageURL` is scheme-only (client-side SSRF via logo).
14. **Missing per-handler query timeout** — /ohlc,/twap,/vwap pass raw context; WriteTimeout doesn't cancel the DB query.
15. **SEP-41 transfer data is i128 OR a map** — type-test before MustI128(). Multi-event ops (Phoenix 8/swap, reflector fanout-stride) need event-index in identity or they collapse.
16. **ced-v2 DDL is operator-applied, not in migrations/** — deployed-vs-repo drift; same for the CH tier1 schema file generally.

## 8. Recurring failure classes (the project's memory)

- **RFC-1 — Code-correct ≠ data-correct.** The code is provably sound but a served *value* is wrong (XLM market-cap +58% from a no-op reserve exclusion, CS-010; dormant pairs stale=false, CS-017; the CS-040 decimals regression). Any money finding must ask "is the served *number* right," not just "is the arithmetic right." Detect by reconciling served values against ground truth.
- **RFC-2 — Silent loss at a durability edge.** Cursor/ledger_ingest_log advance on enqueue not durable write (CS-028); projector cursor advances past sink failures; failed batch-insert dropped with Warn, no dead-letter; crash-before-drain. Detect: trace every cursor/watermark advance to the durability point it actually guarantees.
- **RFC-3 — Invariant by convention, not guard-rail.** i128 analyzer (now a real test), one-writer projector (convention for ops writers), tenant isolation (handlers not SQL, CS-008), one-heavy-job (no lock). A regression wouldn't be caught. Detect: for each invariant, find the weakest writer and ask what stops the next code path from skipping the check.
- **RFC-4 — Watermark/verdict overwrite → false "complete".** CS-083 (stale-tip complete=true), CS-084 (reconcile nets to 0), retentionStart floor. Detect: can the verdict read "complete" while a real hole exists below the reconcile floor?
- **RFC-5 — Gate on topic bytes → look-alike injection.** CS-026 (comet/aquarius/etc. matched on topic alone). Detect: any `Matches()` without a contract-identity gate + foreign-contract-reject test.
- **RFC-6 — Newest surface holds the security/availability bugs.** CF edge functions + SSE held the only prior Highs (CS-009 SSRF, CS-012 crash, CS-013 FD-exhaust). The 2026-07-10 push (two-axis, verify-lake, classic-movements, ced-v2) is this run's newest surface.
- **RFC-7 — Doc/plan drift as a first-class finding.** 6 CLAUDE.md/arch-doc false claims; ROADMAP rows contradicting code (Alg-2 readers, #72 PROJECTION, #34, #16 endpoint); a stale public-linked backlog. A plan that prescribes obsolete work wastes the next campaign. Detect: reconcile every ADR/ROADMAP/perf-todo claim against code.
- **RFC-8 — Fail-open abuse/DoS windows.** Missing query timeouts, validation-skipping scans, rate-limit-if-Redis-down, CoinGecko divergence with no staleness gate. Detect: what happens to each guard when its dependency is absent or the input is adversarially sparse?

## 9. Hot spots (audit first, hardest)

- **The 2026-07-10 push** (commits since ~2026-07-08): two-axis verdict (`1d56d5b2`), verify-lake/hashchain/contiguity (`d03d3ee7`/`e2df55bb`/`c41f2708` + underflow fix `33645e74` — re-probe saturating-subtraction siblings), reconcile-balances, ced-v2 rebuild, ADR-0047→0048 classic-movements, CAP-33 sponsored-creation fix. Newest = least-audited.
- **The money serve paths** — /v1/price fallback tiers, market-cap, /v1/changes, the CS-040 gate.
- **The completeness verdict** — retentionStart, projector-cursor-past-failure, classic_movements-outside-guards, G12-03 live-sink gap.
- **Issuer-controlled render paths** in the explorer (XSS chokepoints) + the CF OG edge function (prior SSRF).
- **Unauth DoS levers** — /ohlc,/twap,/vwap timeouts; /holders, /contracts/{id}/interactions validation gaps.
- **The plan surface** — this run explicitly audits ADRs/ROADMAP/perf-todo for validity, not just the code.
- **Prior-audit re-verification set** (fast-shipped remediation is highest-risk; re-derive against current code): CS-010 (XLM mcap), CS-012/013 (SSE crash/FD), CS-017 (stale VWAP), CS-100 (org_verified enforcement), CS-124 (dashboard CSRF), CS-118/119 (root services/user), CS-009 (OG SSRF), CS-008 (SQL-level tenant scoping), CS-021 (ledger_entries_current ordering).

## 10. Scope defaults & exclusions

- **In scope by default:** all of `internal/`, `cmd/`, `pkg/`, `migrations/`, `web/`, `configs/`, `deploy/`, `scripts/`, `.github/`, and the **plan surface** (`docs/adr/`, `notes/ROADMAP.md`, `notes/BACKLOG.md`, `docs/operations/perf-todo.md`, `docs/architecture/*`).
- **Excluded (with reason):** generated files (`web/explorer/src/api/types.ts`, `docs/reference/api/*`, `examples/postman/*` — codegen, drift-gated); vendored SDK (`go-stellar-sdk` — trusted, SHA-pinned); the operator-applied CH DDL is IN scope for drift but not for line-review of ClickHouse internals; live R1 state (verify-items flagged, but no r1 access this run — deploy freeze).
- **Deploy-freeze constraint:** audit is read-only anyway; remediation is code-only (no release/deploy/r1-ops while Phase 0 runs). See repo-prep.md.
- **Where audit outputs land:** `docs/audit/audit-2026-07-16/`.

---

*Update this file whenever architecture, money flows, trust boundaries, or invariants change — and append any newly-learned trap or failure class after every audit. Flag any invariant whose tier weakened since last derivation; that is a finding. The recon dumps this distils are under `audit-2026-07-16/recon/`.*
