# Recon: web/explorer frontend + PLANS/ROADMAP validity (HEAD f84e2d0b)

## Frontend
- **Next 16.2.10 / React 19.2.7** (NOT "Next.js 15" — CLAUDE.md/repo-prep stale; ADR-0044 documents 15→16). output:'export' static (OPEN_NEXT=1 switches to Workers SSR spike). ~70 route dirs, 59 generateStaticParams.
- Two data layers: build-time buildFetch.ts (FAIL-HARD — transport failure throws & fails build; null only for authoritative 4xx / CI stub); client-time TanStack Query hooks. Generated types src/api/types.ts (openapi-typescript, CI drift-gated).
- web/status = redirect stub only (301 + window.location.replace). Real status page = web/explorer/src/app/status/StatusPageClient.tsx (live 20-endpoint probe matrix every 30-120s).
- Protocol registry.ts mirrors Go protocols_registry.go (16 each, CI lint-protocol-registry-sync enforces).

## XSS chokepoints (verified GOOD — re-test on any new attacker-string render)
serializeJsonLd (seo.ts:45-70, escapes </script> + U+2028/9); isSafeHomeDomain (safe-domain.ts:20 strict hostname regex); isSafeHref (markdown.tsx:315, blocks javascript:). All dangerouslySetInnerHTML uses are JSON-LD via serializeJsonLd. No raw-HTML injection of asset/issuer strings. No client secrets (only NEXT_PUBLIC_* env). Dashboard keys server-issued show-once.
- LEAD (LOW): API base https://api.stellarindex.io duplicated as raw env fallback in 5 files instead of importing client.ts — drift risk.

## Two-axis verdict (lake_complete)
- **API EXISTS**: /v1/coverage serves BOTH Complete (:40) + LakeComplete (:49) per source + LakeCompleteSources summary (coverage_verdicts.go:28-72); tested (lake_complete=true,complete=false case). Types regenerated (types.ts:5708).
- **UI: LANDED in commit `4d034432` (2026-07-16, post-audit-SHA).** As of the audited f84e2d0b the UI was MISSING (no .tsx referenced lake_complete); a concurrent agent then shipped `feat(explorer): surface the two-axis completeness verdict on diagnostics` — CoveragePanel.tsx now renders both axes (served-tier `complete` + archive `lake_complete`) with a headline tally, per-source Lake column, tooltips, and an explainer. Reviewed SOUND (real generated-type fields, correct semantics). **This work item is now DONE — do NOT re-remediate.** (Status page still renders completeness_pct from health rows, not the lake axis — a minor residual if a status-page surface is also wanted.)

## #34 site-promised features vs backend
| Promise | Reality | Status |
|---|---|---|
| Multi-network/multi-chain | zero web/ matches; company page affirms "Stellar-only" | RETRACTION COMPLETE (ROADMAP:155 still lists open — contradiction) |
| CEX order-book depth | no ingestion anywhere | PLANNED-ONLY, honestly framed as gap |
| DEX TVL | lending TVL real (ADR-0039); soroswap reserves served; NO aggregate DEX-TVL number | PARTIAL (copy conservative-stale) |
| Per-token oracle layer (SEP-41↔SEP-41 usd_volume) | code comments (trades.go:107, soroban_volume.go:83, assets_f2.go:100) still say token↔token=0; oracle READ endpoints exist but not wired as pricing layer | PLANNED-ONLY — **but ROADMAP:148 claims ✅DONE 2026-07-09. VERIFY-ITEM: code comments vs ROADMAP disagree.** |
| Paid forex 1h/24h | fx_quotes/massive exists (copy stale-conservative) | verify-item |
| Backlog link | company page links launch-readiness-backlog.md which lacks order-book/DEX-TVL rows + is last_verified 2026-05-13 | site points users at STALE doc for promises it doesn't contain |
| GitHub org / security@ | org not created (redirects holding), security@ mailbox possibly never created (BACKLOG #65) | site promise on unverified redirect |
- Also: company page "v1 ships in coming weeks" unchanged since ~May; current v0.16.3.

## PLAN AUDIT — the user's listed work items
1. **Alg-2 pool-internal readers (PHO/BLND/EURC/KALE) = TRAP.** ROADMAP:165/406 sell "protocol-specific pool-state readers (bigger effort)"; but crosscheck.go:80-104 records the **2026-07-06 verdict SUPERSEDED that** — the balances ARE ordinary Vec(Symbol("Balance"),Address(pool)) SAC storage entries; fix = `supply seed-sac-balances -full-history` (SHIPPED, StreamSACBalanceSeedsFullHistory, migration 0102). ROADMAP:239 records the resolution. **Remaining work = r1 operator seed run + verify 4 alerts clear, NOT new readers.** DOC-CONTRADICTS-CODE + self-contradiction. (Genuine residual = true subset-compare, BACKLOG #59(b), crosscheck.go:59-79.)
2. **#39 CH Phase 8 decommission = SAFE direction, INCOMPLETE plan.** Preconditions met (ADR-0029 Superseded, projector reads CH default, CH reconcile path exists). BUT plan does NOT enumerate live PG soroban_events readers that break if dropped: soroban_events.go:157/338/380/393 (census count/max), topic_samples.go:161/242/300 (diagnostics, ops surface), per_source_gaps.go:278 (gap-detector target Table:"soroban_events"), ledger_ingest_log.go:223-231 (chunk-pruning coupling), per-source README INSERT…SELECT backfill flows. Dropping the table first breaks census verify + gap scanning + topic diagnostics.
3. **#66 Alchemy = re-scope.** Prices-API half coherent (4th divergence ref). Token-API half is "non-Stellar token coverage" — contradicts Stellar-focus R-018. Both paid features on free-tier key. Keep Prices half only. (token_supply sub-item stale: already dropped.)
4. **#72 account_movements perf: perf-todo RIGHT, ROADMAP:169 WRONG.** perf-todo §5 rejects the PROJECTION fix (co-partitioned, can't reduce fan-out, table already address-leading) — VERIFIED vs tier1_schema.sql:452-478 (PARTITION BY intDiv(ledger,1e6), ORDER BY (address,ledger,…)). Accept-and-monitor + re-eval post-Phase-0 is sound. #72 has NO BACKLOG anchor (colliding second tracker).

## Doc-vs-doc / doc-vs-code contradictions (DOC dimension)
1. ROADMAP #34 self-contradiction (:155 open vs :229 done).
2. Alg-2 diagnosis (:165/406 vs crosscheck.go + :239).
3. #72 fix (:169 vs perf-todo, DDL-verified).
4. Migration 0105 classic_movements applied-unpopulated; promised cleanup migration DOESN'T EXIST yet; dead PG store code lingers (classic_movements.go caller-less).
5. ROADMAP #16 tail (:136 "endpoint = future unit") vs code (endpoint EXISTS: explorer_movements_test.go, AccountMovements.tsx).
6. registry.ts:9-11 comment ("CI doesn't cross-check") vs ci.yml:202 (it does).
7. ROADMAP:51 verdict item doesn't distinguish API-done vs UI-missing.
8. launch-readiness-backlog.md stale (2026-05-13) but PUBLIC SITE links it as "the roadmap" + lacks the items site claims.
9. ROADMAP:148 SEP-41 usd_volume DONE vs code comments token↔token=0 — verify-item.
10. Residual-defi census: ROADMAP:230 "USDx/EURx/GBPx watched" vs census doc "captured by nothing" — watched-set is r1 config (not in repo), unverifiable from repo → r1 verify.
11. Next 15→16 drift in archived docs (low weight).
