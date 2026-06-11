# Wave 2 — cross-cutting matrices (condensed)

## Live verification (orchestrator, r1 2026-06-11)
- V-01: r1 /etc/ratesengine.toml sets `[ingestion.projector] enabled=true, persist_per_source=true` (explicit) + clickhouse_projector_source=true → G9-01/G16-01 sep41 loss is **LATENT not live**. But Default() does NOT set PersistPerSource (zero-value false) while the doc-tag says `default:"true"` → an operator enabling the projector without the explicit line silently enters Phase-4 sole-writer and the critical goes live. Confirms N4-04(d) + upgrades the compound risk.

## RFP execution matrix (67 delivered / 6 partial / 0 not-delivered / 4 diverged-unregistered)
- RFP-01 MED: 30s freshness re-scoped to /price/tip (43b640e5) — defensible but unilateral; ≥14 days of formal breach prior; unregistered customer-facing. conf=high
- RFP-02 MED: multi-zone/99.99%/read-replicas commitments are R1-single-host — tracked internally, NO proposal-corrections entry. conf=high
- RFP-03 MED: p95≤200ms unproven from client vantage — probe is localhost; only client-side measurement read p95=246ms; sla-proof artifact never produced; live LHR probe today: tail 539ms/1.59s. conf=high
- RFP-04 LOW: "self-hosted RPC nodes" claim vs Galexie reality — unregistered (spirit holds). conf=high
- RFP-05 LOW: "containerized + load-balanced" vs bare-metal systemd — matrix acknowledges, corrections register doesn't. conf=high
- RFP-06 LOW: open-source commitment unmet until v1.0 flip — on schedule (week ~8 of 10); HIGH if slips past Jun 30. conf=high
- RFP-07 MED: coverage-matrix.md (launch-gating doc) a month stale — five ❌ cells now wrong in the FAVOURABLE direction; lists shipped status page as open. conf=high
- RFP-08 LOW: ctx-proposal self-contradicts on Soroswap reserves [= D1-09]. conf=high
- RFP-09 LOW: "orderbook depth ingested+normalized" — no depth surface exists (lake-only at best). conf=med
- RFP-10 LOW: "callback alerts integrated into discord/slack" — generic webhooks only; Slack/Discord payload format won't render. conf=med
- RFP-11 LOW: SEV response SLAs never drill-verified (playbook exists, no drill record). conf=high
- RFP-12 INFO: rfp-matrix row A5 stellar-rpc design quotable out of context (banner exists). conf=high
- RFP-13 INFO: SEP-10 challenge 503 on prod (R-009 signing seed unset, month-old). conf=high
- EXCEEDED: chainlink 516-feed ingest; +5 venues beyond proposal; sub-hour retention removed entirely.

## Observability chain matrix (111 emitted; 106 alerts; 21 can never fire; 22 orphans)
- OBS-01 HIGH: complete dead-reference enumeration — 16 metric names in live rules with NO emitter; 18/106 alerts can never fire. NEW beyond Wave 1: ratesengine_cagg_last_refresh_unix + _cagg_refresh_interval_seconds (cagg-stale alert), ratesengine_uncompressed_chunks_older_than_7d (compression-lag), ratesengine_pgbackrest_last_success_unix + _expected_interval_seconds (BOTH backup-failure alerts fictional — pgbackrest_exporter emits pgbackrest_*). Backups can silently stop with zero paging; cagg/retention drift are documented past-incident classes. conf=high
- OBS-02 MED: stellar.yml header's "Active" claim false [= N4-03/D4-02 confirmed]. conf=high
- OBS-03 MED: selector-level kills 3 more — timescale_primary_down (page!) dead in BOTH envs; redis_master_down both; NEW multi-host infra host_down job="node" vs node_exporter. conf=high
- OBS-04 MED: multi-host meta.yml exporter-down alerts PERMA-FIRE on any template deploy (absent_over_time on never-defined jobs ×3). conf=high
- OBS-05 LOW: 22 orphan emitted metrics enumerated — all 6 cross-region divergence counters watched by nothing (a cross-region divergence increments a counter nobody alerts on); sla-probe ×3, supply-snapshot ×5, archive run-duration, galexie tip ×2, HA textfiles ×5 (which are exactly what OBS-03's fix needs). conf=high
- OBS-06 LOW: +1 NEW runbook dead metric (archive_completeness_runs_total ×2 runbooks); confirms Wave-1 set. conf=high
- OBS-07 LOW: postgres-ping-failing runbook selects outcome label on trade_inserts_total (wrong metric of pair); README gap-detector labels [= D6-02]. Rules themselves label-clean. conf=high
- OBS-08 INFO: source_lag_ledgers zombie (legacy-orchestrator-only emitter); 2 stale comments. conf=high

## CLAUDE.md/README claim verification
- CMD-01 HIGH: "Add a new CEX connector" recipe almost entirely stale — cex/ subdir, five-file convention, consumer.Source interface, internal/sources/registry.go, test/fixtures/external/ ALL wrong vs actual external.Connector + external/registry.go + per-package layout. conf=high
- CMD-02 MED: "uniform 10^8" claim false for FX pollers (10^6) [= G10-10 in CLAUDE.md too]. conf=high
- CMD-03 MED: repo map presents hashdb as active drift detector — unwired library [docs angle of G15-04]. conf=high
- CMD-04 MED: README lists status page as remaining launch blocker — shipped; contradicts CLAUDE.md. conf=high
- CMD-05 MED: repo map omits 6 internal/ packages + 4 docs/ dirs + deploy/clickhouse + comms. conf=high
- CMD-06 MED: CAP-67 trap "our decoder handles both" overstates — handles both EVENT SHAPES; no pre-P23 ops+effects parsing exists anywhere (historical-completeness risk). conf=med
- CMD-07 LOW: invariant-7 source lists stale (defindex missing from projected; soroswap_router from non-projected). conf=high
- CMD-08 LOW: divergence "CMC" cross-check claim — CMC deferred in code. conf=high
- CMD-09 LOW: README GPG-key pointer circular [= N1-06]. conf=high
- CMD-10 INFO: stablecoin proxy list omits DAI/USDP/EURC. conf=high
- CMD-11 INFO: verify step list omits vuln; make dev "full stack" overstated. conf=high
- CMD-12 INFO: "45 audit docs" count drift (48). conf=med
- CMD-13 INFO: consumer "legacy" annotation understates load-bearing Event/Source types. conf=med
