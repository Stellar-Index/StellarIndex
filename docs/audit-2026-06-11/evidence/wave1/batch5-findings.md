# Wave 1 batch 5 — condensed findings (web + docs)

Slices: WA explorer/src (143/143), WB dashboard/status/configs (61/61), D1 (10/10), D2 (36/36), D3 (40/40), D4 (97/97), D5 (201/201), D6 (276/276 calibrated depth). Wave 1 coverage: 2,349/2,349 tracked files.

## WA web explorer src
- WA-01 HIGH static-export: "live" prices baked at BUILD time with no client refresh — embed widgets, asset/pair headline prices, cursor-lag badges frozen until redeploy; copy claims "live" (assets/[slug]/page.tsx:557, embed/*). conf=high
- WA-02 MED security: issuer home_domain (on-chain attacker-controlled) rendered as clickable https:// links unvalidated — phishing surface (IssuersTable.tsx:161). conf=high
- WA-03 MED content: /methodology claims freeze→"503 frozen"; API contract + /anomalies say LKG + flags.frozen (methodology/page.tsx:132). conf=high
- WA-04 MED drift: sitemap reads contract_id from /lending/pools; consumers use pool → /lending/undefined/ URLs (sitemap.ts:259). conf=med
- WA-05 MED dead-UI: VerifiedCurrencyView reads price_usd/description never populated by the identity-only catalogue — cross-chain pages never show price (catalogue.ts:90). conf=high
- WA-06 MED units: hardcoded 1e7/1e8 scaling + unconditional "$" — quote-volume 10× off for classic pairs; non-USD quotes mislabeled (markets/[pair]/page.tsx:430). conf=med
- WA-07 MED drift: useNetworkStats unwraps {data} but spec defines bare object [ties N5-01 family]; /pools + /lending spec schemas empty placeholders (hooks.ts:237). conf=med
- WA-08 MED auth: /account dashboard sends credentialed CORS requests the API's own comment says are refused — contradictory assumptions (AccountDashboard.tsx:62 vs hooks.ts:163). conf=med — settle vs cors.go (D1-10 evidence says cors.go now emits Allow-Credentials)
- WA-09 LOW XSS: markdown link renderer no scheme sanitisation (javascript: live) — build-time-trusted today (lib/markdown.tsx:275). conf=high [= WB-03]
- WA-10 LOW: embed currency widget links retired /currencies/{ticker} → 404 for every embedded consumer (embed/currency:135). conf=high
- WA-11 LOW: convert page "dynamic fallback" comment false under output:export — non-hub pairs 404 (convert:91). conf=med
- WA-12 LOW: issuer detail still 2s fetch timeout — the exact bug class fixed at 8s+retry on assets pages; bakes "Issuer not found" (issuers/[g_strkey]:56). conf=high
- WA-13 LOW a11y: SearchModal advertises ↑↓/↵ keys not implemented (SearchModal.tsx:264). conf=high
- WA-14 LOW: verified badge keyed on fragile slug equality; API-side verified stamp would be robust (hooks.ts:52). conf=med
- WA-15 LOW DRY: 3 chart panels, 4 frontmatter parsers, 3 markdown tokenizers, twin tables, dup BLEND_POOL_META (multiple). conf=high
- WA-16 LOW: /research card claims ghcr.io container pushes (F-1221 stale) (lib/operations.ts:30). conf=high
- WA-17 LOW: teaching surfaces use rejected param forms (oracle lastprice asset=native; /v1/assets/XLM; OHLC Interval) (OraclesView:245, HomeTryAPI:48). conf=med
- WA-18 LOW: literal \` sequences render on /methodology (methodology:58). conf=high
- WA-19 INFO: no-op replace; 50k vs 60k rpm tier mismatch; "Sandboxed" iframes without sandbox attr; MarketsTabPanel side-detection never matches on slug pages; converter "All currencies" = 10. conf=high

## WB dashboard/status/configs
- WB-01 HIGH security: dashboard (most sensitive surface: session auth + plaintext key display) ships NO _headers — no CSP/frame-ancestors/HSTS; both sibling apps have them (web/dashboard has no public/). conf=high
- WB-02 MED resilience: status page near-blank when API fully down (everything gated on /v1/status fetch); incident history live-fetched despite build-time corpus — outage shows "No past incidents" (status page.tsx:600,566). conf=high
- WB-03 LOW XSS: status markdown renderer href verbatim [= WA-09]. conf=high
- WB-04 LOW: SSE "live" badge persists stale data after stream death — frozen ledger + green pulse (status page.tsx:1200). conf=med
- WB-05 LOW: dashboard conflates API-down with signed-out → bounced to /signin during outages (auth.tsx:42). conf=high
- WB-06 LOW: incident postmortem links 404 when live corpus ahead of static export (page.tsx:1117). conf=med
- WB-07 LOW: "View source" incident links point at PRIVATE repo — 404 for customers (incident/[slug]:94; also llms.txt). conf=high
- WB-08 INFO deps healthy: next 15.5.18 post-CVE, registry-only lockfiles, no lifecycle scripts; eslint-config-next 15.0.4 skew. conf=high

## D1 docs-root
- D1-01 MED: getting-started first curl unquoted — & backgrounds command (getting-started.md:24). conf=high
- D1-02 MED: key prefix shown rate_ vs real rek_ (:75). conf=high
- D1-03 MED: claims key revocation "not shipped" — DELETE endpoint exists (:81). conf=high
- D1-04 MED: triangulated flag described as "reserved/false" — actively stamped today (:55). conf=high
- D1-05 LOW: SDK method list massively undersells (8 listed vs ~36 shipped) (:152). conf=high
- D1-06 LOW: "make dev … + API" — compose has no API service, no CH (:201). conf=high
- D1-07 MED: launch-task-list self-contradicts — §A/C/F cells say 🟡/❌ for items §G marks ✅ shipped (:116-331). conf=high
- D1-08 MED: launch-task-list asserts 1m/15m retention caps (removed 0031) + /v1/coins deletion "waits" (removed rc.48); not re-walked in 6 weeks (:128,531). conf=high
- D1-09 MED: ctx-proposal.md (immutable awarded artifact) edited in-place with corrections — now self-contradictory on Soroswap reserves; line 87 RPC claim inconsistently unannotated (:93,95). conf=high
- D1-10 LOW: review-2026-05-13 lacks point-in-time frontmatter/resolution log (fixed findings read as open). conf=high
- D1-11 LOW: blog post documents removed /v1/currencies + /v1/coins live with no editor's note (blog/2026-05-07:35). conf=high
- D1-12 INFO: ctx-proposal 1.2MB inline base64 PNGs break tooling (single 608KB line). conf=high
- D1-13 INFO: BSD-only date -v in examples. conf=high

## D2 adr
- D2-01 HIGH drift: ADR-0015 wire contract never implemented — no window param exists; default is 1m/5m not 30s; ADR-0018 restates differently without amending 0015 (0015:53,60 vs price.go:272). conf=high
- D2-02 HIGH drift: ADR-0017 four contracts narrowed to cross-anchor-only (F-0019) — ADR never marked; false integrity guarantee for cold readers (0017:54). conf=high
- D2-03 MED: ADR-0011 SEP-1 max_supply precedence step dead code (supply.Overlay zero callers); self_declared flag never shipped (0011:74). [= G16-05] conf=high
- D2-04 MED: hashdb orphan package vs ADR-0033 "feeder" claim + CLAUDE.md drift-detector claim (0033:153). [= G15-04] conf=high
- D2-05 MED: ADR README index stale — 0034 MISSING entirely; 0028/0029 statuses wrong (README:72-77). conf=high
- D2-06 LOW: 0029 frontmatter status Accepted despite superseded_by 0034 (0029:4). conf=high
- D2-07 MED: ADR-0024 (Sentinel) contradicts 0007+0008 (Cluster) — no markers either side (0007:108, 0008:59, 0024:40). conf=high
- D2-08 MED: ADR-0001/0008/0013 still list stellar-rpc + core watchers as production sources (removed 2026-04-23, invariant 6) (0001:45). conf=high
- D2-09 LOW: ADR-0005/0003 cite nonexistent deploy/k8s/, per-binary Docker images, CalVer, pkg/types (0005:51-75). conf=high
- D2-10 LOW: ADR-0003 Enforcement claims custom golangci int64-amount analyzer — none exists; db-migrate-status BIGINT refusal also absent (0003:116). conf=high
- D2-11 LOW: ADR-0030 references deleted cascade-window-drain runbook (0030:71). conf=high
- D2-12 INFO: ADR-0034 decommission list / 0029 banner ahead of reality (PG soroban_events still live + load-bearing) — needs phase qualifier (0034:81). conf=med
- D2-13 INFO: numbering clean 0001-0034; immutability respected; nit — 0034 "amends 0030-0033" lacks reciprocal markers; 0031 cites wrong migration filename. conf=high

## D3 architecture
- D3-01 HIGH drift: BINDING ingest-pipeline.md (invariant 6's reference) predates projector/soroban_events/CH — omits dual-sink + projector + lake backfill; cites nonexistent routes.go + "PR 165b" future tense; one of four dispatcher seams documented (ingest-pipeline.md:9-102). conf=high
- D3-02 HIGH drift: ha-plan.md (ratified) prescribes the EXACT retention policies invariant 8 calls drift (trades 90d, prices_1m/15m 30d); nonexistent tables events_raw/supplies (ha-plan.md:205). conf=high
- D3-03 MED: clickhouse-phase4 doc says raw trades = PG 90-day window — contradicts invariant 8; two current authoritative docs disagree (phase4:143). conf=high
- D3-04 MED: ha-plan §3.8 "one indexer process per source (~11)"; multi-region §4 stellar-rpc subscriptions — production is one dispatcher process, rpc removed (ha-plan:180,329). conf=high
- D3-05 MED: multi-region claims R1 holds full ~7TB mirror + runs all four tiers — Move A executed 2026-05-20 (~22GB remain), Tier E dormant (multi-region:266). conf=high
- D3-06 MED: contract-schema-evolution "5 of 5 sources" stale (13 audit logs); step 3 cites removed RPC path (schema-evolution:224,203). conf=high
- D3-07 MED: binding aggregation-plan omits shipped anomaly/freeze/confidence stages (ADR-0019 phases 1+2) (aggregation-plan:42). conf=high
- D3-08 MED governance: FIVE architecture docs evade the CI freshness gate (no/wrong-format last_verified; lint skips empty) (storage-considerations, contract-call-coverage-audit, rollout-plan, cctp, rozo; lint-docs.sh:180). conf=high
- D3-09 LOW: storage-considerations ("start here" for r1 storage) predates CH lake entirely; inventory wrong since 2026-05-20 (storage-considerations:32). conf=high
- D3-10 LOW: adr-0031-0032-rollout-plan still "Proposed" though completed rc.91-98; explorer plans likewise; web/showcase path stale (rollout-plan:4). conf=high
- D3-11 LOW: oracle-manipulation-defense Layer 5 "planned NOT shipped" contradicted by own gap table (shipped) (oracle-defense:390,489). conf=high
- D3-12 LOW: multi-network-assets-migration never closed out — claims /v1/coins deletion deferred (removed rc.48); wrong seed path; CG-augmentation framing conflicts with CLAUDE.md (mna-migration:4,304). conf=high
- D3-13 LOW: coverage-matrix S6.5/S7.2 retention "✅ verified" rows now contradict invariant 8; launch-readiness-backlog violates own weekly cadence (coverage-matrix:172). conf=med
- D3-14 INFO: repo-hygiene TODO(#0) self-contradiction; supply-pipeline lacks CH cross-ref. conf=high

## D4 runbooks
- D4-01 HIGH dead-SQL: projector-lag + projector-replay (ratified, the ONLY catch-up docs) query nonexistent source_cursors table + updated_at col (real: ingestion_cursors.last_updated) (projector-lag:32, projector-replay:45). conf=high
- D4-02 HIGH dead-metric: archive-divergence.md claims alert "live, can fire today" via nonexistent scripts/ops/archive-cross-check.sh; metric has no producer [ties N4-03] (archive-divergence:14). conf=high
- D4-03 HIGH dead-flags: archive-completeness flags fictional in 4 runbooks (-range/-checks/-input-file/-force-all-sources/--region/--tier don't exist) (archive-completeness-stale:39 etc.). conf=high
- D4-04 HIGH dead-config: supply-refresh mitigation TOML uses wrong keys (watched_classic vs watched_classic_assets) — pasting the fix silently reproduces the alert (never-initialized:49). conf=high
- D4-05 HIGH: price-divergence.md (RFP-leaning runbook) fully non-executable — dead metrics, wrong columns, wrong port, wrong asset id (price-divergence:22-41). conf=high
- D4-06 MED dead-SQL: trades pair/timestamp/observed_at columns in 4 runbooks (real: ts/base_asset/quote_asset) (class-drop-spike:39 etc.). conf=high
- D4-07 MED: anomaly-freeze-engaged manual-unfreeze subcommand + z-score/confidence metrics fictional — P1 override branch unexecutable (anomaly-freeze:100,38). conf=high
- D4-08 MED: external-poller-error-rate metric name wrong (_total vs _polls_total) + window contradiction (poller-error-rate:22). conf=high
- D4-09 MED: ledgerstream-tier-both-missing — 2 phantom metrics, wrong port, fictional --source flag (tier-both-missing:39-136). conf=high
- D4-10 MED wrong-port: TEN runbooks probe aggregator metrics on 9464/9091/9100 (aggregator=9465; 9091 matches nothing) (10 files). conf=high
- D4-11 MED: insert-errors.md kubectl remnants; db-disk-full k8s PVC advice — no k8s exists (insert-errors:36). conf=high
- D4-12 MED: ingest-gap-detected routes triage to nonexistent alert name (real: timescale_connections_saturated) (:38). conf=high
- D4-13 MED: sdex-gap-detected asserts deleted *-backfill family exists — contradicts projector-replay.md in same dir (:68). conf=high
- D4-14 MED: all-ingestion-down rollback uses CalVer tag glob '20*' — matches nothing (SemVer) (:123). conf=high
- D4-15 MED topology-fiction: ~10 runbooks describe api-01..03/HAProxy-VIP/Patroni/etcd/Sentinel fleet that doesn't exist on single-host r1 — no posture notes (api-down:31 etc.). conf=high
- D4-16 INFO: 5 self-acknowledged inert runbooks (core-lag/core-peers/rpc-lag/archive-publish/ingestion-lag) — model behavior; their dead rules still ship in rules.r1. conf=high
- D4-17 LOW: supply-snapshot subcommand name wrong in 3 runbooks (supply snapshot, not supply-snapshot). conf=high
- D4-18 LOW: -sources vs -source flag (duplicate-flood:93). conf=high
- D4-19 LOW: bootstrap-archival-node treats standalone rpc/core as expected; sibling got SUPERSEDED banner, this didn't (:129). conf=med
- D4-20 LOW: wrong unit names ratesengine-redis / bare postgres/redis (freeze-recovery:96, slo-burn:38). conf=high
- D4-21 INFO: operator-unblock-2026-05-08 orphan task list in runbooks namespace. conf=med
- CLEAN: no unbounded DISTINCT/LAG trades scans in any runbook; verify-archive flags correct; projector-replay source list matches registry.

## D5 ops-rest
- D5-01 HIGH: r1-deployment-state.md a MONTH stale — zero mention of ClickHouse/ADR-0034; blend flag claim false even at its own date; release-process's update loop broken (r1-deployment-state:3-354). conf=high
- D5-02 HIGH: archival-node-bringup NOT executable — refetch-history-archive script exists nowhere in repo; archive-writer MinIO user never created; no redis/CH steps; "five binaries" stale (bringup:53-133). [ties N3-02/03/08] conf=high
- D5-03 HIGH: rollback.md built on nonexistent deploy/ansible/ paths, CalVer tags, wrong config schema, wrong status-page paths — every section fails under incident pressure (rollback:63-176). conf=high
- D5-04 MED: CalVer remnants in 4 operator docs incl. launch-day tag command that would never trigger release.yml (launch-day:125, public-flip:40). conf=high
- D5-05 MED: deploy-workflow.md behind deploy.yml — sla-probe default missing, migrations staging undocumented, false "no --version flag" claim (deploy-workflow:40,133). conf=high
- D5-06 MED: release-process invents stellar-core --version inference step; HAProxy drain fiction (release-process:113). conf=high
- D5-07 MED: cctp/rozo/defindex wasm-audit frontmatter contradicts own approval verdicts + registry (cctp.md:4 vs :175). conf=high
- D5-08 MED: audit scope swap-path-only while BackfillSafe gates whole event families (EVERY-event) [= G7-09 generalized] (phoenix.md:24). conf=med
- D5-09 MED: multi-region-cutover references nonexistent configs/ansible/site.yml (cutover:64). conf=high
- D5-10 MED: two docs invoke renamed showcase-deploy.yml (break-glass path) (explorer-deployment:133). conf=high
- D5-11 LOW: alerts-catalog attributes archive-divergence to nonexistent script; otherwise catalog↔rules CLEAN both ways (106/106). conf=high
- D5-12 LOW: backfill-procedure.md pre-projector/pre-lake (backfill-procedure:27). conf=med
- D5-13 LOW: sev-playbook header/§8 swap drill cadences; aspirational tooling unmarked; break-glass.md absent (sev-playbook:11,341). conf=high
- D5-14 LOW: redis drill exercises nonexistent runbook (drills/sev2-redis:6). conf=high
- D5-15 LOW: wasm-audits README inventory badly stale (README:28). conf=high
- D5-16 LOW: deploy.yml health_grace_seconds comment false [= N2-06]. conf=high
- D5-17 INFO: misc dead refs (dev.yml vs dev.yaml; staging URLs; 4h vs daily timer; anchor drift). conf=high

## D6 ref-archives
- D6-01 HIGH drift: DEPLOYED OpenAPI copy (docs.ratesengine.net + /openapi.yaml) missing the live /assets/{id}/supply endpoint + schemas — drift gate is PR-only CI, commit-to-main bypasses permanently [= W0-02 mechanism + endpoint identified] (reference/api yaml:417). conf=high
- D6-02 MED drift: metrics README gap-detector family wrong label sets (missing table label), 100×-wrong latency prose, stale target list — name-only lint can't see it (metrics/README:39-118 vs metrics.go:211). conf=high
- D6-03 LOW: api-design.md cites nonexistent scripts/ci/lint-openapi.sh as enforcing compat rules (api-design:523). conf=high
- D6-04 LOW: wrangler tool cache committed in generated docs tree (CF account_id exposed) (reference/api/.wrangler/). conf=high
- D6-05 INFO: api-design endpoint catalogue omits supply endpoint (same root cause as D6-01). conf=high
- CLEAN: prior audit dirs structurally intact; sampled dispositions consistent; 3 spot-checked closed findings genuinely closed; discovery archive properly frozen; 6 live-referenced discovery docs match current decoder code.
