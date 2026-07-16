# Stellar Index — Audit findings (2026-07-16, commit f84e2d0b)

> Cold, adversarial, systematic audit per the audit-suite methodology. Recipe: `../recipe.md`. Recon dumps: `recon/`. Findings are CONFIRMED by an independent skeptic (or PLAUSIBLE where blocked on runtime/external state). Ranked exposure-then-severity. This doc is appended per bounded chunk as each completes (crash-safe progress artifact).
>
> Exposure: LIVE (default prod config) · GATED (needs a flag/mode on) · BRANCH (unmerged) · GATE (launch/legal blocker, not a code defect).
> Severity: critical · high · medium · low · info.

**Status:** ✅ AUDIT COMPLETE — all 4 chunks + synthesis done. See `executive-summary.md` for the headline, `coverage-ledger.md` for what was/wasn't examined, `remediation-plan.md` for the wave order, `forward-path.md` for the strategic direction.

| Chunk | Scope | Status |
|---|---|---|
| 1 | money / pricing / supply / divergence / external sources + 27 mechanical sweeps | ✅ done (361 agents; `converged:false` — hit 3-wave cap still finding, so money surface is deep-but-not-exhaustive; 117 raw confirmed → ~39 distinct) |
| 2 | ingest / storage / completeness / ops / migrations | ✅ done (130 agents; `converged:false` at 2-wave cap; 56 confirmed) |
| 3 | api / auth / platform / config / binaries | ✅ done (57 agents; converged:false; 32 confirmed) |
| 4 | infra / cicd / web / obs / plans | ✅ done (79 agents; converged:false; 41 confirmed) |

> Exposure note: severities/exposures below already fold in the running-config reality (`recon/operational-reality.md`) — e.g. CS-040 and /metrics are GATED because R1's config doesn't enable the triggering path.

---

## CHUNK 1 — money / pricing / supply (confirmed, deduped from 117 → distinct)

### LIVE · HIGH
- **M1 · [MNY-03] INV-3 re-derive trap — ON CONFLICT DO NOTHING permanently freezes wrong money values** (×3 corroborated). `trades.usd_volume`, `asset_supply_history`, `oracle_updates.price`, `sep41_supply_events.amount`, + ~15 protocol tables: derived value is NOT in the PK, so a corrected re-derive (peg fix → `backfill_external.go:132` / `ch-rebuild`) is silently discarded (`rowsInserted=0`). Only DELETE/TRUNCATE+re-derive fixes it. Poisons every downstream `SUM(usd_volume)` CAGG served by the API. *Fix:* DO UPDATE on the value columns, or DELETE-first re-derive tooling + a divergence guard test. `internal/storage/timescale/trades.go:440,638`, `supply.go:63`, `oracle.go:41`.
- **M2 · [INV-7/MNY-07] Non-7dp decimals normalization is missing on ~10 serve paths (systemic)** — `AdjustPrice` is applied on the main `/price` closed-bucket path but NOT on: `/price/at`, `/price/changes`, `/price/tip`+`/price/tip/stream`, `/observations`+`/observations/stream`, `/oracle/x_last_price`, `/assets/{id}` F2 (price + `market_cap_usd` + `fdv_usd`), `/chart?price_type=market_cap` (hardcoded `10^7`), the stablecoin-proxy fallback tier, and the SEP-40 `prices()` snapshot. A confirmed non-7dp SEP-41 token serves mis-scaled absolute prices/market-caps. *Fix:* normalize at a chokepoint (the reader), not per-handler. `price_at.go:124`, `price_changes.go:224`, `price_tip.go:168`, `observations.go:138`, `oracle_sep40.go:345`, `assets_f2.go:327-406`, `chart.go:935,1058`, `price.go:826-836`.
- **M3 · [MNY-01] fx_quotes triangulation FX-leg snap is INVERTED** — served fiat-quoted pairs (XLM/EUR, XLM/GBP, …) via the FX snap come out inverted. NEW (not in recon). `internal/storage/timescale/fx_quotes.go:133-259`.
- **M4 · [MNY-INV6] Supply snapshots stamped with wall-clock write-time, not ledger-close time** — `ObservedAt: observedAt.UTC()` instead of the ledger's close time → corrupts point-in-time supply/observation queries. NEW. `supply/xlm.go:199`, `classic.go:193`, `sep41.go:225`, `ops/supply/supply.go:268`.
- **M5 · [MNY] Published-VWAP outlier filter is single-pass mean/σ (masking-vulnerable), and provably rejects nothing for windows <~18 trades** — feeds the orchestrator-published VWAP (default σ=4); MAD guard only covers 3 serve-time sites. `aggregate/outliers.go:28-110`, `orchestrator.go:797-803`.
- **M6 · [REL] Soroban event batch-insert failures silently drop rows permanently** — `dispatcher_adapter.go:259-269` drops a failed batch with `logger.Warn`, no retry/dead-letter, contradicting the "never silently lost" doc. Corroborates recon.
- **M7 · [MNY-01] /v1/changes serves money as JSON float64 numbers** (INV-2 violation) — the one API surface breaking money-as-string, end to end (`changes.go:32-55` ← `change_summary.go` ← `changesummary/rollup.go:37` ← migration 0022).
- **P1 · [PRF-07] Missing per-handler DB query timeout on /vwap, /twap, /ohlc(single-bar), /assets/{id}/supply** — raw `r.Context()` to an unbounded `trades` scan (retention removed, migration 0031); `/ohlc` multi-bar + `/history` correctly wrap 8s. Sibling `TradesInRange` also lacks the `SET LOCAL statement_timeout` its neighbours use. Unauth DoS. `vwap.go:175`, `twap.go:70`, `ohlc.go:135`, `asset_supply.go:96`.
- **R1 · [REL absence] No default Postgres `statement_timeout` on the serving pool** — the systemic root cause behind P1; one pool-level setting bounds every query. `trades.go:852`, `cmd/stellarindex-api/main.go:1101`.
- **S1 · [INV-6] F-1320 supply dormancy exception can't distinguish a crashed/frozen component producer from genuinely-dormant** — a stalled observer at the same ledger for two ticks reads "dormant" and publishes stale supply. `supply/refresher.go:296-352`.

### LIVE · MEDIUM
- **M8 · [MNY] Global Tier-2 headline price = plain arithmetic mean of aggregator sources, no outlier/divergence rejection** — one bad CoinGecko/CMC/CryptoCompare print moves the served global price ~50%. `aggregate/global.go:249-364`.
- **M9 · [MNY-22] CoinGecko divergence reference has NO upstream-staleness gate** (×several) — unlike Chainlink (3h/76h); a frozen CG price suppresses or manufactures `divergence_warning`. Also CG/CryptoCompare stamp `Timestamp=time.Now()` not upstream, and the supply cross-check refs (Stellar Dashboard + CG) lack it too. `divergence/coingecko.go:190-226`, `supply.go:474-660`.
- **M10 · [MNY] Float64 on served money — fiat cross-rate `/v1/price` fallback (`price.go:911`), fiat market-cap (`assets_global.go:297`, `chart.go:885`)** vs the exact big.Rat crypto path.
- **M11 · [MNY-22] `GuardServedVWAP` fails fully-open on thin history (<5 samples) and only rejects ≥10× deviations** — a 3–9× manipulation passes. `aggregate/served_guard.go:43,56,89`.
- **M12 · [MNY-22] `OracleUpdate.Validate` does not reject Asset==Quote** — a degenerate self-priced oracle update validates. `canonical/oracle.go:113-163`.
- **M13 · [MNY] confidence `approxUSDVolume` divides quote by 1e7 while CEX quotes are 1e8 → 10× liquidity overstatement** in the confidence score. `orchestrator/confidence.go:244-275`.
- **M14 · [MNY] Supply overlay applies a SEP-1-declared `max_supply=0`, publishing max<circulating** — a hostile/sloppy issuer TOML poisons the served supply. `supply/overlay.go:79-90`.
- **D1 · [DAT] Projector cursor advances past sink failures** (RFC-2) — `projector.go:393-401` advances unconditionally on stream success; the `SinkFunc` type has no error return (`:94`), so the documented "does not advance on sink failure" (`:91-93`) is unimplementable. Silent projection loss, caught only by reconcile. Corroborates recon.
- **D2 · [INV-5] Enqueue-not-persist cursor advance** — cursor + `ledger_ingest_log` advance on channel-enqueue, not durable write; hard crash loses buffered events with the cursor moved past them. `indexer/main.go:1454-1480`, `sink.go:139-150`.
- **D3 · [DAT] Discovery `AsyncSink` suppresses a contract's FIRST sighting when the `Record` write fails** — the seen-set is marked before the write succeeds. `canonical/discovery/sink.go:110-190`.
- **D4 · [DAT] Customer-webhook `EnqueueDelivery` lacks idempotency** — a caller retry double-delivers (anomaly/divergence/price-alert fan-out). (Stripe webhook dedup is correct — noted GOOD.) `platform/postgresstore/webhook_store.go:234`, `customerwebhook/fanout.go:100`.
- **PRV1 · [PRV] Customer PII (email) logged in dashboardauth handlers** (`handlers.go:256,290,344`); remote IP in per-request logger (LOW); staff email in admin lookup (INFO).
- **P2 · [PRF] Validation-skipping DoS levers — `/assets/{id}/holders`, `/contracts/{id}/interactions`, `/pools` base/quote** take raw params into CH `FINAL`/50k-row scans without ParseAsset/IsContractID. Corroborates recon. `explorer/account_state.go:268`, `contracts_list.go:110`, `markets.go:211`.
- **P3 · [PRF absence] No max from/to window-span validation on raw-scan endpoints; [CON] no cost/weight in the rate limiter** — a heavy `/ohlc`/`/vwap` costs the same token as a cheap `/price`.
- **N1 · [REL/OBS negspace] Absent operator controls:** no runtime kill-switch/quarantine to disable a compromised feed source's VWAP contribution; no manual serve-suppression/freeze; no durable audit trail (operator/range/reason) for `ch-rebuild`/`projected-rebuild` DELETE+re-derives.

### GATED (running config doesn't enable the trigger — go LIVE if enabled)
- **G1 · [MNY-04] CS-040 decimals hardcode mismatch (GATED)** — `orchestrator.go:1268-1279` hardcodes 8 for `fiat:USD`; FX registry stamps 6; the `windowUSDVolume` corrector (`:1410-1442`) is dead code; insert-time `trades.go:23` `externalUSDVolumeDecimals=8` same. GATED because R1 doesn't enable polygon-forex/exchangeratesapi and `min_usd_volume=0`. Reactivates a ~100× volume-gate understatement + mixed-scale VWAP if those FX venues are enabled.
- **G2 · [MNY-08/13] CEX synthetic tx_hash truncation** — same-millisecond trades collapse (identity omits granularity), and CEX backfill synthetic tx_hash omits granularity → un-dedupable double-counted VWAP volume. `binance/parse.go:122`, `coinbase/backfill.go:242`, `scale/scale.go:115`.
- **G3 · [DAT/MNY] SEP-41 Alg-3 negative-total corruption misrouted to the benign non-paging outcome on a transient `SEP41GenesisBaselineSeeded` lookup error** — a genuine negative supply is downgraded to "missing baseline" (no page). `supply/storage_sep41_reader.go:127`, `sep41.go:172-189`.
- **G4 · [MNY-05] Verified-currency catalogue loader doesn't validate numeric fields** (`circulating_supply` etc.) — a bad seed.yaml value ships. `currency/verified.go:231-289`.
- **G5 · [REL-02] decimalsguard latches the fired-dedup BEFORE the persistence write** — a failed persist means the guard never re-fires for that (source,asset). `decimalsguard/guard.go:374-406`.
- **G6 · [HLT] Dead code — `windowUSDVolume` (never called), `api_usage_events` table (no writer implemented), `cmd/tmpxdrdump/` (empty).** Corroborates recon.
- **G7 · [OBS] /metrics loopback asymmetry** (indexer/aggregator listeners) — GATED: bound to 127.0.0.1 + nftables + Caddy on R1; defense-in-depth only. Corroborates recon + operational-reality.
- **G8 · [INV-6] XLM Alg-1 circulating supply has no negative clamp** (the CS-038 clamp applied to classic/SEP-41 but not XLM). `supply/xlm.go:164`.

### BRANCH / GATE / LOW-INFO (tail)
- ClickHouse supply_flows/ledger_entry_changes rely on DB-level dedup not app idempotency (BRANCH); Docker base images tag-pinned not digest (DEP, BRANCH); `ClassicSupplyComponents.Trustline` docstring claims an issuer-exclusion the code doesn't do (DOC-drift, LOW); `scval.Display` renders U256/I256 as bare type-name not value (INV-1, LOW); hardcoded `0.0.0.0:3000` bind warn-only (CFG, LOW); fiat catalogue entries carry ISO-code `coingecko_id` slugs (INFO); stablecoin-fiat-proxy merge can fold heterogeneous-decimal trades into one VWAP (COR, LOW).

> **Coverage caveat:** chunk 1 did NOT converge (hit the 3-wave cap while still producing findings) — the money surface is deeply but not exhaustively covered. The convergence-skeptic sample + a targeted re-run could surface more; recorded as budget-truncated, NOT clean.

---

## CHUNK 2 — ingest / storage / completeness / ops / migrations (56 confirmed, deduped; escalations of chunk-1 items noted)

### LIVE · HIGH
- **C2-1 · [RFC-2/DAT] Projector advances its cursor past silently-swallowed SINK write failures — permanent loss for the sole-writer sep41 domain** (ESCALATES chunk-1 D1 to HIGH). `SinkFunc` has no error return (`projector.go:94`); `HandleEvent` logs+swallows every Insert error (and recovers panics); `processEventSafely` only soft-fails on a *decode* error, so a *sink* failure is invisible → `eventsEmitted` counts it OK → cursor advances to `toLedger`. Under the production posture (projector on, `persist_per_source=true`) the dispatcher SKIPS sep41 (sole-writer), so a transient PG fault (deadlock/reset/statement-timeout, or a CHECK reject like 0096 `amount>0`) during a sep41_supply cycle permanently drops that mint row; `-resume` skips it; `ReconcileRunningTotals` re-sums the equally-short table; metrics report "ok". Under-counts served supply until the completeness cron notices, then INV-3 forces a DELETE+re-derive. *Fix:* give `SinkFunc`/`HandleEvent` an error return; advance the cursor only to the highest fully-committed ledger; proven-red test injecting a mid-cycle persist error.
- **C2-2 · [REL] Archive-phase trailing-missing tolerance silently skips up to 65,536 ledgers on initial backfill** — `maybeTolerateTrailingMissing` (`ledgerstream.go:265-298`, `pipeline/datastore.go:60`) treats a missing trailing window (default 65536) as end-of-stream, so a real archive gap up to 65k ledgers is silently accepted as complete. NEW — a direct source of the "we had to re-backfill" class.
- **C2-3 · [DAT/RFC-4] Served `complete` axis reads true while a projection drop in the older range is invisible** — `retentionStart = tip−1.5M` hardcode (`compute_completeness.go:196-199`) floors the projection reconcile, and `retentionFloor` shrinks it to first-served, so a served-tier drop below tip−1.5M never flags. The public `complete` verdict can be true with a real hole. (The lake axis is unaffected; this is the served axis.)
- **C2-4 · [DAT-06] Current-state reads resolve "latest entry" by `ledger_seq` alone and don't exclude change-removed entries → resurrect deleted state / return before-image** (CS-021 class, reconfirmed + broadened across readers) — `account_state_reader.go:72,133`, `soroswap_pair_state_reader.go:88,156`, `account_balance_reader.go:47` + `sac_balance_seed.go:210` (argMax before-image AND column-mixing across ledgers). **The SAC-balance-seed one feeds supply → the cross-check divergence.**
- **C2-5 · [REL fail-open] compute-completeness swallows a recognition-scan error and still writes `recognition_ok`/`lake_complete`** — a scan failure produces a FALSE "complete" verdict (`compute_completeness.go:143-149,242-267`). The trust story's own gate fails open. High for a "verified explorer".
- **C2-6 · [CON/DAT] 8-worker last-writer-wins on `*_observations` upserts persists a NON-final intra-ledger balance** — the 8 PersistEvents workers don't preserve order and observations use DO UPDATE last-writer-wins, so a stale intra-ledger change can win → wrong supply component. `account_observations.go:43`, `classic_supply_observations.go:41`, `sac_balances/dispatcher_adapter.go:82` (also collapses multiple same-ledger changes to one row).
- **C2-7 · [REL-02] ADR-0041 block-and-retry backpressure protects only trade-shaped events; band-oracle + supply + other per-event writes drop on fault** — the durability guarantee is narrower than believed. `sink.go:291,531`.
- **C2-8 · [REL absence] The per-event sink path (`HandleEvent` type-switch) has no durability at all** — every arm logs+returns on error; no retry/dead-letter (the root cause under C2-1/C2-7). `sink.go:615,640,779`.
- **C2-9 · [OBS absence] No audit trail for destructive ops re-derives** — an append-only `audit_log` table exists but `ch-rebuild`/`ch-reproject`/`projected-rebuild` TRUNCATE+re-derive without writing to it. `platform/audit.go`, `ch_rebuild.go:129`.

### LIVE · MEDIUM (distinct)
- **C2-10 · [RFC-7] Migration 0108 adds `lake_complete DEFAULT false` with no backfill** — every pre-existing snapshot reads `lake_complete=false` until recomputed; the new axis lies until the cron catches up. `0108:31-32`.
- **C2-11 · [DAT] `soroban_events` landing zone truncates events with >4 topics** — Aquarius ≥4-token-pool events lose topics at `reconstructTopics` (`want>4 → 4`). Silent decode-loss for multi-token pools. `sorobanevents/events.go:189`, `reconstruct.go:71`.
- **C2-12 · [DAT] RMT-without-FINAL over-counts in served explorer/protocol readers** — `ProtocolEventBreakdown` (`protocol_reader.go:94`), `NetworkThroughput`/`OperationTypeStats` (`explorer_reader.go:285,325`), `verify-contiguity Check-2` uses `count()` not `uniqExact` (`contiguity_reader.go:173`) → un-merged duplicate parts inflate served counts.
- **C2-13 · [DAT-09] Same-op event collapse** — `cctp_events`/`rozo_events` collapse multiple same-type events in one op (missing event-index discriminator, `0038:83`/`0039:62`); live batch trade path skips the classic-asset/issuer registry hook (`trades.go:704`) so `classic_assets`/`issuers` under-populate.
- **C2-14 · [DAT durability] Two more enqueue-not-persist cursor advances** — `ops/ingest/backfill.go:383` (backfill advances per-ledger cursor after enqueue) and `census_backfill.go:120` (resume checkpoint strides past mid-range skipped ledgers → permanent substrate gap).
- **C2-15 · [REL fail-open] `reconcile-balances` exit code counts only MISMATCH, ignoring ERROR** — a run where every account errored exits 0 (looks clean). `reconcile_balances.go:99`.
- **C2-16 · [RFC-4] Oracle projection reconcile uses window-total netting** (`aggregateReconcile`) — re-opens the CS-084 "discrepancies net to 0" class for oracle sources. `reconciliation_catalogue.go:245`.
- **C2-17 · [REL] Graceful-shutdown drain defeated for events on the racy `select` arm** — a shutdown can drop in-flight events. `sink.go:262-294,467-505`.
- **C2-18 · [DAT-03] Migration 0105 `classic_movements` is a dead applied table** whose own runbook contradicts the caller-less writer (superseded by ADR-0048; promised cleanup migration still MISSING).
- **C2-19 · negspace absences (LIVE, operator-safety):** no guarded (dry-run/backup/confirm) re-derive command; no dead-letter/quarantine for DATA-fault events; `stellarindex-migrate down` has no destructive-op guard; no advisory lock coordinating concurrent ops re-derives (they can race the live projector — ties to the one-heavy-job-is-convention gap); no runtime kill-switch to halt ingestion of a single poisoning source; no test/DB guarantee for the I6 "ledgers-row-last commit marker" invariant.
- **INV-3 (again, MEDIUM)** — re-confirmed at `trades.go:440` + `supply.go:58` (cross-chunk dup of chunk-1 M1; keystone fix).

### LOW / GATED / tail
- LOW LIVE: `bumpEntryCount` inflates `source_entry_counts` on replay (`sink.go:1325`); stale `G12-03` comment now false (`sink.go:431` — the live sink DOES populate changes now, AGT-08 drift); `SeedSourceEntryCounts` REPLACE races live ADD (`diagnostics.go:497`); non-sargable `bucket + INTERVAL <= now()` on unauth CAGG reads (`aggregates.go:141,200` — the exact prior perf-incident shape).
- HIGH GATED: `projected-rebuild`/`ch-rebuild` checkpoint a window "done" despite silently-swallowed sink failures (RFC-2 applied to the ops re-derive path — the re-derive itself can silently under-write).

> **Coverage caveat:** chunk 2 also did NOT converge (2-wave cap). Ingest/storage/completeness is deeply covered but not exhaustive. Cross-chunk dedup with chunk 1: INV-3, projector-cursor, retentionStart are the same underlying issues surfaced from more angles (corroboration, not new count).

## CHUNK 3 — api / auth / platform / config / binaries (32 confirmed, deduped)

### LIVE · HIGH
- **C3-1 · [PRF/SEC] Explorer lake-read handlers have NO per-request timeout → shared 8-connection ClickHouse pool exhaustion (unauth DoS)** — every handler in `internal/api/v1/explorer/` passes raw `r.Context()` (34 sites, zero `WithTimeout`) to the shared `ExplorerReader` (`MaxOpenConns:8`, `max_execution_time:30`). `AssetHolders` runs TWO `ledger_entries_current FINAL` scans/request on an unvalidated asset_id, no cache. 8 concurrent slow requests saturate the pool; every lake-backed endpoint (holders/accounts/state/contracts/movements/SAC/liquidity/lending) then blocks on acquisition. Server `WriteTimeout` doesn't cancel the query. Sustained anon attack = sustained outage. *Fix:* per-handler `WithTimeout(8s)` (the pattern siblings already use) + a request-timeout middleware + validate ids up front. Broadens chunk-1 P1/P2.
- **C3-2 · [PRF absence] No per-request timeout on /ohlc, /vwap, /twap DB range scans** (×2, corroborates chunk-1 P1). The systemic root: no pool-level `statement_timeout` + no timeout middleware.
- **C3-3 · [OBS absence] No detection/alerting over privileged actions + the audit trail is incomplete** — no alert path for admin key-mint/tier-override/status-notice, and destructive ops re-derives aren't audit-logged (corroborates chunk-2 C2-9).

### LIVE · MEDIUM
- **C3-4 · [API-02] The SDK `Flags` struct drops the server's `divergence_checked` flag** — `pkg/client` silently omits a safety flag the API emits (CS-087 class: a consumer can't tell "divergence not checked" from "checked, no divergence"). Hand-written types drift the reflection test doesn't catch for a dropped field.
- **C3-5 · [SEC] Auth runs before rate-limit → invalid-credential requests are NEVER throttled** — a wrong API key fails auth before reaching the limiter, so credential-stuffing / key-guessing is unlimited. (The Auth-before-RateLimit order is deliberate for *subject* keying but leaves rejected-auth unthrottled.)
- **C3-6 · [PRF] `/account/keys` list + revoke do an O(N) SCAN of the entire `apikey:*` keyspace** — degrades as keys grow; a per-account index is needed.
- **C3-7 · [REL/CFG] `stellarindex-indexer -dry-run` is NOT dry** — it starts async sink goroutines and opens a live ClickHouse connection. Corroborates recon.
- **C3-8 · [SEC absence] No per-IP/subject cap on concurrently-held SSE connections** — the CS-013 FD-exhaustion class is STILL present (prior "fix" incomplete): an anon client leaks goroutine+conn+FD per stalled stream.
- **C3-9 · [SEC absence] No strkey/contract-ID format validation on path params** (corroborates chunk-1 P2) — malformed ids reach ClickHouse before any 400.
- **C3-10/11/12 · [SEC/PRV absence] No operator suspend/freeze/ban affordance; no admin kill-switch to revoke a leaked/compromised key; no data-subject (GDPR) affordance for stored customer PII (email/billing).**

### LIVE · LOW/INFO
- Dead exported `VerifiedCurrencyListItem` (godoc links a nonexistent symbol, AGT-06); `AssetHolders`/`ContractInteractions` empty-check-only validation (API); API graceful shutdown signals cancel but doesn't join workers before exit (REL-01); `isSafeImageURL` scheme-only permits `http://`/private hosts (INFO, corroborates chunk-1); `auth_mode="none"` + `allowed_origins=["*"]` most-permissive default ships (INFO); no idempotency-key on resource-creating POSTs (INFO).

### GATED
- **C3-13 · [SEC, HIGH GATED] rate-limit FAIL-OPEN: no in-process fallback limiter when Redis is absent at boot** — if `rdb` is nil at startup, requests are UN-rate-limited (answers the recon verify-item: fail-open). Also `/account/keys` self-service is 503'd under `auth_backend=postgres` (split-brain guard — GOOD).
- **C3-14 · [CFG-05, HIGH GATED] Two ops archive commands use bare `config.Load()` (no `ApplyEnvOverrides`)** → they read the placeholder TOML DSN, diverging from every other binary (the env-override trap's other half).
- **C3-15 · [CFG, HIGH GATED] `redis_password_env`/`clickhouse_serving_password_env` doc tags say "reference, not value" but the code reads them as the VALUE** — the two-conflicting-conventions footgun is a confirmed doc-vs-behaviour bug: a maintainer following the doc ships a broken secret. Corroborates recon.
- **C3-16 · [MNY-ENTITLEMENT, MED GATED] Enterprise Stripe checkout upgrades tier + Redis keys but skips the lifecycle/downgrade path** — no per-key rate-limit downgrade on subscription cancel; no Stripe reconciliation job (a missed webhook leaves entitlement wrong).
- **C3-17 · [SEC-AUTH, MED GATED] 6-digit login-code brute-force resistance leans on the OPTIONAL `LoginThrottle`** — off ⇒ ~10^6 brute-forceable magic codes.
- **C3-18 · [SEC-06, MED GATED] `warnUnsafeBind` mis-parses IPv6 listen addresses** — a public IPv6 all-interfaces bind ships without the warning.
- **C3-19 · [SEC-06/SEC, GATED] /metrics loopback nuance** — the Go `loopbackOnly` guard on the api is defeated by single-host colocation (a colocated process reaches 127.0.0.1); the indexer/aggregator standalone listeners have no in-process guard at all (mitigated on R1 by nftables + bind addr, hence GATED).
- **C3-20 · [CFG-01, MED GATED] `clickhouse_projector_source` requires `clickhouse_live_sink` — documented but never enforced** at config-validate; a misconfig silently mis-reads.
- **C3-21 · [OBS, LOW GATED] Usage rollup sweeps only today+yesterday** → >~2 days rollup-worker downtime permanently loses usage/billing counts.

### GATE
- **C3-22 · [SEC] No in-process fallback rate limiter when Redis is absent at boot** (the GATE framing of C3-13 — a launch-hardening blocker).

> Prior-audit re-verify (chunk-3): **CS-013 SSE FD-exhaust CONFIRMED still present** (C3-8). CS-012 SSE crash, CS-124 dashboard CSRF, CS-100 org_verified-not-enforced, the 8 IDOR candidates, SQL/CH injection — NOT surfaced as confirmed (appear fixed/sound; carried to reviewed-not-carried for the record). `converged:false` (2-wave cap).

## CHUNK 4 — infra / cicd / obs / web / plans (41 confirmed, deduped)

### LIVE · HIGH — the 3am story is broken (detectability)
- **C4-1 · [OBS-08] `runbook_url` is a LABEL, not an ANNOTATION, on 266/270 alerts → the Discord page/ticket template can NEVER render the runbook link** — both templates gate on `{{ if .Annotations.runbook_url }}`, always false. Every page arrives with no runbook (140/270 hand-duplicate the link in `description`; 126 have none). The `lint-docs §9` "runbook_url points to an existing file" check is YAML-blind (grep over raw text) → false confidence. *Fix:* move `runbook_url` to `annotations` across both rule trees + make the lint YAML-aware. Two galexie-archive alerts use a third inconsistent key (`runbook`).
- **C4-2 · [REL-06] `run-ch-supply.sh` reports exit 0 when a supply_flows seed chunk fails** (`set -uo pipefail` without `-e`; `|| echo FAILED` + implicit exit 0) → `ch-supply.service` reports SUCCESS to systemd; failure visible only by grepping the log. Served SEP-41 supply understated with no operator signal until the next daily run self-heals. Contrast `run-compute-completeness.sh` (`set -euo pipefail` + `exit $rc`).
- **C4-3 · [OBS-01] soroswap-router + defindex-flow (+ discovery) persist failures are invisible to Prometheus** — three event-persist failure paths increment no metric; a silent write-loss can't alert.
- **C4-4 · [OBS-02] The ledgerstream tier-read PAGE alert can never fire** — its metric's `Registry` is nil at every production entry point → a dead alert (looks like coverage, provides none).
- **C4-5 · [OBS absence] No delivery guarantee/self-check on the SEV-1 paging path** — the sole page path (Alertmanager→Discord) has no dead-man's-switch on itself; a broken webhook = silent no-paging.
- **C4-6 · [OBS absence] The alerts that fire when the API serves a provably-wrong number are not wired/scheduled** — `verify-served-values` has no timer + no alert (corroborates chunk-1: no scheduler runs it). Data-correctness drift is undetected.
- **C4-7 · [CID absence] No CI lint enforces migrations/README rule 9 (every up-migration has a down)** — a one-way migration can ship (ties to the destructive-migration rollback gaps).

### LIVE · MEDIUM — doc/plan drift (the 6 false claims confirmed) + ops
- **C4-8 · [DOC] The six CLAUDE.md/arch-doc false claims CONFIRMED:** projector reads PG `soroban_events` (actually CH `contract_events` default); ratelimit "token bucket" (actually fixed-window); `storage-layering-spec.md` (status:current) contradicted by the shipped D8/`internal/domain` refactor; [+ multi-region "ratified" and ha-plan roles-never-invoked, from recon]. Plus `projector ProjectorEventsDecoded{outcome="ok"}` counts sink-lost events as OK (metric masks chunk-2 C2-1).
- **C4-9 · [DOC/DOM] Plan-validity CONFIRMED:** ROADMAP #72 still prescribes the CH PROJECTION that perf-todo.md evaluated + REJECTED; ROADMAP still scopes obsolete "Phoenix/Blend pool-internal readers" for Alg-2 while the code's 2026-07-06 verdict superseded it (fix is the operator `seed-sac-balances -full-history`); ROADMAP self-contradicts on #34; #66 Alchemy Token-API half contradicts Stellar-focus; migration-0105 promised cleanup missing.
- **C4-10 · [CID-09] `ansible-drift.yml` trusts the r1 host key via live `ssh-keyscan` (TOFU each run)** while `deploy.yml` pins it → MITM window in the drift check.
- **C4-11 · [OBS-02] Alertmanager inhibit rule can never suppress page/ticket stacking** — `equal:[alertname]` mismatch → operators get duplicate noise.
- **C4-12 · [PRF absence] No rate limit / input-cardinality bound on the CF `/og/*` edge function** — the OG SSRF-area endpoint (CS-009) is unthrottled.

### LIVE · LOW / GATED / BRANCH / GATE
- **C4-13 · [SEC-XSS, MED GATED] `isSafeHref` fails OPEN on control-character-obfuscated schemes** (`java\tscript:` etc.) → a stored-XSS bypass of the markdown link chokepoint. NEW — the recon assumed isSafeHref was solid.
- **C4-14 · [INF-11, LOW] The "mandatory" `run-heavy-job.sh` wrapper is BYPASSED by the actual heavy systemd timers** — they call ops directly, so the memory cap + root-watchdog don't apply to the scheduled heavy jobs (only ad-hoc ones). Compounds the one-heavy-job-no-lock gap during Phase 0.
- **C4-15 · [AGT-07, test-vacuity] `TestHTTPMetrics_Fast5xxDoesNotCountAsSuccess` never calls the real middleware; `TestNewLogger_*` never inspect log output** — tests that don't test their claim (GATE severity). Test-integrity gap.
- **C4-16 · [CID absence, LOW] No main-branch CI-health tripwire** — the sustained-red-main went unnoticed 24h+; there's no alert on it. Also: no degraded-mode fallback for the 2 install-gated scanners (they hard-fail on missing secret → the dead gates); no pre-migration DB backup/restore-point by the deploy pipeline.
- **C4-17 · [CID-24, LOW] Generated Postman collection stale** (the openapi-lint CI red — confirmed).
- LOW/INFO: dead `NEXT_PUBLIC_API_BASE_URL` + stale CSP allowance (web/status, HLT); `RedirectToStatus` deep-link comment drift (DOC); `SearchModal` no result-count aria-live (ACC); rules.r1 README drift; ingestion.yml runbook recommends a nonexistent ops subcommand; divergent systemd-unit copies only ansible-drift-checked (CID-17, BRANCH); rule-quality gates (dead-metric-ref, rule-tree-equivalence) currently disabled/GATE.

### Web/explorer re-verify (chunk-4)
- XSS chokepoints `serializeJsonLd`/`isSafeHomeDomain` hold; **`isSafeHref` does NOT (C4-13)**. CF OG edge function: no injection beyond the unthrottled-fetch (C4-12) — the double-decode SSRF (CS-009) appears mitigated but the DoS/throttle gap remains. Two-axis CoveragePanel (4d034432): reviewed sound, no finding. No client secrets. a11y: one aria-live gap (SearchModal).

> `converged:false` (2-wave cap). Chunk 4 confirms the CI-red root cause, the doc/plan drifts, and a systematically broken detection/paging layer — the "verified explorer" can serve a wrong number or lose data with no operator signal.
