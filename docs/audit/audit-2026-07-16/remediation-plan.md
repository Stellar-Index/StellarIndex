# Remediation wave order — audit 2026-07-16 (DRAFT, finalized after chunks 3-4)

Posture: **full-autonomous-commit-all**. Base: current main `4d034432`. Gate: `make verify` for all; **`make test-integration` (Docker testcontainers) additionally for money/supply/auth/migration fixes** with a proven-red DB-backed test. Deploy freeze: migration FILES are written+tested+committed; **never run on R1** (Phase 0). Migrations serialised one-owner-per-wave. Branch: `remediation/audit-2026-07-16`. CHANGELOG is the shared-collision surface (other agent paused). Every fix: re-derive → best-practice fix + proven-red test → verify (test red on unfixed, gate green) → commit on branch → PR → merge on green (CI green except the 2 [OP] secret gates, logged pre-existing).

## Wave 0 — CI baseline repair (unblocks clean green-vs-red judgement)
- **W0.1** `make docs-postman` regen + commit (`examples/postman/*` missing `lake_complete`) → clears openapi lint. Mechanical.
- **[OP logged, not blocking]** set repo secrets `PROM_TARBALL_SHA256=19700bdd42ec31ee162e4079ebda4cd0a44432df4daa637141bdbea4b1cd8927`, `GITLEAKS_TARBALL_SHA256=5bc41815076e6ed6ef8fbecc9d9b75bcae31f39029ceb55da08086315316e3ba`. Until set, the promtool-rule + gitleaks jobs stay red (dead-at-install) — a CID finding; merges proceed past them as known-pre-existing infra.

## Wave 1 — KEYSTONE: INV-3 idempotent-corrective re-derive (ends the re-backfill treadmill)
The single highest-leverage fix (see forward-path.md). PROTECTED CLASS (money + migration) → strongest verify panel.
- **W1.1** Make the PG served-tier derived-value writers idempotent-corrective: add a `derive_generation bigint` (or reuse an existing monotonic) and switch `ON CONFLICT DO NOTHING` → `DO UPDATE SET <value cols>, derive_generation = EXCLUDED.derive_generation WHERE EXCLUDED.derive_generation >= <table>.derive_generation` on `trades` (usd_volume), `asset_supply_history`, `oracle_updates`, `sep41_supply_events`, and the ~15 protocol tables. Migration (serialised owner) + writer change (`trades.go`, `supply.go`, `oracle.go`, `sep41_supply_events.go`, protocol writers) + a `pipeline.HandleEvent`/projector generation stamp.
- **Proven-red DB test:** insert a row with a wrong value → re-derive with a higher generation → assert the value is CORRECTED (fails on unfixed code where DO NOTHING keeps the stale value).
- **[DECISION — safe default applied, logged for review]** Mechanism = DO-UPDATE-with-generation-guard (vs versioned table). Rationale: least schema churn; the generation guard prevents a regressed decoder from clobbering good data. Generation source = the re-derive run's start ledger-tip (monotonic per correction).

## Wave 2 — Money-value correctness batch (fix once, re-derive once)
Depends on W1 (so the corrective re-derive is incremental). PROTECTED (money).
- **W2.1** Decimals normalization at the READER chokepoint (M2) — one place applies `AdjustPrice`, covering /price/at, /price/changes, /price/tip(+stream), /observations(+stream), /oracle/x_last_price, /assets/{id} F2, /chart market-cap (drop the 10^7 hardcode), stablecoin-proxy fallback, SEP-40 prices. Remove per-handler drift.
- **W2.2** FX-leg snap inversion in triangulation (M3) — `fx_quotes.go`.
- **W2.3** Supply snapshot ledger-close timestamp, not wall-clock (M4) — `supply/{xlm,classic,sep41}.go`, `ops/supply`.
- **W2.4** `/v1/changes` money-as-string (M7) — `changes.go` + `change_summary.go` + rollup + migration 0022 (serialised owner).
- **W2.5** CS-040 decimals unification + delete dead `windowUSDVolume` (G1) — `orchestrator.go`, `trades.go` insert-time, `registry.go`. GATED but fix now so raising `min_usd_volume` is safe later.
- **W2.6** Supply overlay reject SEP-1 `max_supply=0` (M14); `OracleUpdate.Validate` require Asset≠Quote (M12); XLM Alg-1 negative clamp (G8); SEP-41 negative-total not misrouted on transient error (G3).

## Wave 3 — Durability & cursor honesty (stop the silent losses that force re-backfills)
- **W3.1** Projector `SinkFunc`/`HandleEvent` error return; advance cursor only to highest fully-committed ledger; distinguish transient (retry) from deterministic (skip) (C2-1, D1). PROTECTED (data-integrity). Proven-red test injecting a mid-cycle persist error.
- **W3.2** Per-event sink dead-letter/quarantine + extend block-and-retry backpressure beyond trade-shaped events (C2-7, C2-8, M6).
- **W3.3** Archive-phase trailing-missing: cap the silent-skip window + surface a gap alert instead of accepting up to 65k ledgers (C2-2).
- **W3.4** Enqueue-vs-durable cursor advance (D2, C2-14) — advance after durable persist, or document+alert the crash-window; census-backfill resume stride fix.
- **W3.5** 8-worker last-writer-wins on `*_observations` — key on (asset, ledger, change_index) or resolve by max change-index, not arrival order (C2-6).

## Wave 4 — Completeness verdict honesty + before-image reads
- **W4.1** compute-completeness fail-open: a recognition/scan error must NOT write `recognition_ok`/`lake_complete=true` (C2-5). PROTECTED (data-trust).
- **W4.2** Current-state readers exclude change-removed entries / return after-image, not before-image (C2-4, C2-9, C2-10; CS-021 class). Affects account/soroswap-pair state + SAC balance seed → supply.
- **W4.3** RMT-without-FINAL over-count in served explorer/protocol readers (C2-12) — FINAL or argMax dedup.
- **W4.4** retentionStart stale hardcode → actual-min-served (C2-3, RFC-4); oracle window-netting reconcile → per-ledger (C2-16); 0108 lake_complete backfill.
- **W4.5** classic-movements-backfill inside the lockstep guard + ADR-0033 catalogue, verification on by default.

## Wave 5 — Availability/DoS + API + auth + config (chunks 1-3)
- **Timeouts (the systemic root):** add a pool-level `statement_timeout` + a request-timeout middleware so every handler inherits a bounded ctx; per-handler `WithTimeout(8s)` on /vwap,/twap,/ohlc + ALL `internal/api/v1/explorer/` handlers (P1, R1, C3-1, C3-2). Raise/segregate the explorer ClickHouse pool; add a short-TTL cache on the FINAL-scan endpoints. Window-span cap on raw-scan endpoints.
- **Input validation** on /holders, /contracts/{id}/interactions, /pools (P2, C3-9); rate-limiter cost weighting (P3, C3-6 keyspace SCAN → per-account index).
- **Auth:** in-process fallback rate limiter when Redis absent at boot (C3-13/C3-22 — currently FAIL-OPEN); throttle invalid-credential requests (C3-5); per-IP SSE connection cap (C3-8, CS-013); make LoginThrottle mandatory (C3-17). PROTECTED (auth) → strong panel.
- **Security:** `isSafeHref` control-char scheme bypass (C4-13, XSS) — PROTECTED; `isSafeImageURL` reject private/link-local; SDK restore `divergence_checked` flag (C3-4).
- **Config:** fix the two conflicting `*_env` conventions + their doc tags (C3-15); `warnUnsafeBind` IPv6 parse (C3-18); enforce `clickhouse_projector_source`⇒`clickhouse_live_sink` at validate (C3-19); ops archive commands use `LoadWithEnv` not bare `Load` (C3-14); `-dry-run` must not open live sinks (C3-7); region.id mis-deploy guard.
- **Platform:** webhook `EnqueueDelivery` idempotency (D4); PII-in-logs redaction (PRV1); Stripe reconciliation job + per-key rate-limit downgrade on subscription change (C3-16, C3-20); GDPR data-subject affordance (C3-11, likely DEFER as product/legal [DECIDE]).

## Wave 6 — Detectability / the 3am story (chunk-4 obs) — make wrong-numbers and data-loss PAGE
- `runbook_url` label→annotation across both rule trees + make lint-docs §9 YAML-aware (C4-1). PROTECTED (IaC).
- `run-ch-supply.sh` exit-non-zero on chunk failure + emit a success/failure gauge (C4-2).
- Emit persist-failure metrics for soroswap-router/defindex/discovery paths (C4-3); fix the nil-Registry dead page alert (C4-4); fix the Alertmanager inhibit `equal` mismatch (C4-11).
- Schedule + alert `verify-served-values` so a provably-wrong served number pages (C4-6); a SEV-1 paging-path self-check / deadmansswitch coverage (C4-5); a main-branch-CI-red tripwire (C4-16).
- `ProjectorEventsDecoded{outcome}` must count a sink-lost event as a failure, not "ok" (C4-14 — ties to W3.1).
- CI: add the down-migration lint (C4-7); degraded-mode fallback (or fix via the secrets) for the install-gated scanners.

## Wave 7 — Operator safety + hygiene + dead code
- Singleton lock in `run-heavy-job.sh` + route the heavy systemd timers THROUGH it (C4-14/INF-11 — acute during Phase 0); guarded (dry-run/backup/confirm) re-derive command + audit-trail for destructive re-derives (C2-9, C3-3); ops re-derive advisory lock (C2 negspace); `stellarindex-migrate down` destructive guard + pre-migration backup/restore-point (C2, C4 GATE).
- Dead-code removal: `windowUSDVolume`, `api_usage_events`, `cmd/tmpxdrdump`, `VerifiedCurrencyListItem`, dead `NEXT_PUBLIC_API_BASE_URL` + stale CSP (G6, C3, C4).
- Test-vacuity fixes: `TestHTTPMetrics_Fast5xx`, `TestNewLogger_*` (C4-15).
- Divergent systemd-unit copies: reconcile deploy/systemd vs ansible templates (C4, CID-17).

## Wave 8 — Plans/docs truth-up (chunk-4 DOC/DOM)
- Correct ROADMAP stale rows (Alg-2 readers SUPERSEDED, #72 PROJECTION rejected, #34 self-contradiction, #16 endpoint exists, two-axis DONE, #66 Alchemy re-scope); fix the 6 CLAUDE.md/arch-doc false claims (projector-reads-CH, ratelimit-fixed-window, multi-region, ha-plan, storage-layering-spec, D8); write the missing migration-0105 cleanup migration (serialised owner); enumerate #39's live `soroban_events` readers in the decommission plan; un-link the stale launch-readiness-backlog from the public site or refresh it.

## Business-VALUE decisions (safe default applied + logged; NOT blocking — user away)
Per the user's away-directive, I apply the conservative default, implement it, and log for post-hoc review:
- INV-3 mechanism → DO-UPDATE+generation-guard (done, W1).
- `min_usd_volume` → leave at 0 in code default logic; do NOT change R1 config (operator/deploy-gated). Raising to 10000 is [OP], logged.
- Retention/serve-window → keep the two-axis honest split (no infeasible genesis backfill of PG); no code change to policy.
- Genesis backfill scope → no code change (operator/data decision, [OP] logged).
- Peg set / stablecoin proxy thresholds → no change (existing config); flag as [DECIDE] logged.

## Coordination + limits
- Other agent PAUSED; I own main during the campaign; CHANGELOG edits serialized by me. Rebase the other agent's work after.
- Hard limits (never crossed): no force-push, no deploy, no r1 ops, no secret rotation, no running migrations on prod. Those stay [OP].
