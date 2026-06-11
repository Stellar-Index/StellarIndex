# Wave 1 batch 4 — condensed findings

Slices: G21 (58/58), G22 (112/112), N1 (64/64), N2 (13/13), N3 (152/152), N4 (103/103), N5 (15/15). All files read.

## G21 cmd-clis
- G21-01 HIGH cursor-safety: backfill-router final checkpoint writes cursor=*to (not last-processed) with force=false — mid-range failure >30s after last periodic checkpoint marks unfinished range complete; -resume then exits 0 (backfill_router.go:202-215; census_backfill.go:144 does it right). conf=high
- G21-02 HIGH correctness: sla-probe freshness computed at aggregation time (time.Since after run ends) — inflates every sample by up to run duration; median bias ≈ duration/2; tip readings vs 30s target borderline-false-fail at 60s runs (main.go:370,273). conf=high
- G21-03 HIGH CI-gap: verify_archive_chunks_integration_test.go does NOT COMPILE under integration tag (missing chunkOrchestratorOpts arg) — proves ops integration suite hasn't run since the refactor (verified via go vet -tags integration). conf=high
- G21-04 MED: sla-probe multi-pair runs collide per-pair endpoint names — samples merged, double-reported; single-pair r1 unaffected (main.go:140,299). conf=high
- G21-05 MED stale-docs: backfill.go help/comments ×3 claim live 90-day trades retention (invariant-8 drift seeds) (backfill.go:86,454,548). conf=high
- G21-06 MED help-gap: 9 subcommands missing from ratesengine-ops help incl. projector-replay (the ONLY sanctioned catch-up) + find-data-gaps; hubble table name stale; verify-reconciliation -source help lists 5 of 14 (main.go:368-878). conf=high
- G21-07 MED ADR-conformance: backfill-router (retired, known-under-producing MinIO walk) still registered, no deprecation pointer to ch-rebuild -contract-calls (main.go:171, ch_rebuild.go:339). conf=med
- G21-08 MED UX: projector-replay accepts any -source string — typo no-ops exit 0 with garbled message (projector.go:62). conf=high
- G21-09 LOW: verify-archive Tier-D peer-diff lastCP rounding misses checkpoint when to%64==63; -to=0 default samples GENESIS ledgers while claiming "last few hours" (main.go:2594). conf=high
- G21-10 LOW: supply audit documented invocation order (positional-then-flags) can't work with Go flag pkg (supply.go:284). conf=high
- G21-11 LOW: trim/rehydrate-galexie-archive use bare config.Load (no env) — only subcommands skipping env resolution; trim is THE destructive one (trim_galexie_archive.go:84). conf=med [ties G19-03/09]
- G21-12 INFO dead: var _ = strings.Join placeholder; pre-Parse fs.Visit no-op. conf=high

## G22 pkg-test
- G22-01 HIGH: k6 load suite incompatible with current API — pairs.js uses 'USD'/'USDC'/'AQUA' (rejected by ParseAsset), 04-batch POSTs {pairs} vs handler {asset_ids}+DisallowUnknownFields, window=/resolution= params unparsed, 07 sends 30% traffic to retired /v1/coins → "FREIGHTER SLA PROOF" scenario measures 400/404 paths (pairs.js:8, 04-batch.js:53, 06:93, 07:74). conf=high
- G22-02 HIGH: integration suite unrunnable: migrations_test asserts retention policies on trades/oracle_updates that 0031/0040 REMOVED (enforces opposite of invariant 8); storage_test uses PG16-only 51_000_000 literal on pinned PG15 image; doc.go claims nightly CI that doesn't exist (no -tags=integration job anywhere). conf=high
- G22-03 MED SDK: Flags missing UnverifiedTickerCollision (own docs reference it) — wallet UIs can't read the R-018 collision flag (types.go:23 vs envelope.go:63). conf=high
- G22-04 MED SDK: doc.go claims full endpoint coverage — missing methods for /assets/verified, /assets/{id}/{network}, supply, contracts/transfers, diagnostics/ingestion, ledger/tip; Asset() silently mis-types slug responses into near-empty AssetDetail (doc.go:76, types.go:743). conf=high
- G22-05 MED chaos: 04-redis-misconf lenient post-heal path unreachable (die() exits; `||` never fires) — false regression verdict on fresh stacks (04-redis-misconf.sh:238). conf=high
- G22-06 LOW: http_status emits "000000" on curl failure (common.sh:103). conf=high
- G22-07 LOW stale-docs: chaos defaults 8080 vs API 3000; configs/dev.yaml doesn't exist; README table missing scenario 04. conf=high
- G22-08 LOW SDK: Retry-After never surfaced in APIError (server pins it on 503s); RevokeKey skips PathEscape; KeyCreated.ID doc-link wrong (client.go:140, endpoints.go:539). conf=high
- G22-09 LOW stale-docs: coins/currencies-retirement leftovers across pkg/client docs + examples (wrong price_type "tip", "default 30s" vs 5, dangling CoinsOptions block, run.sh "failures DO halt" above continue loop). conf=high
- G22-10 INFO coverage: no e2e LCM tests for soroswap swap+sync correlation or phoenix 8-event reassembly; ZERO integration coverage for projector (the only writer) and CH lake path. conf=high
- G22-11 INFO fixtures: no orphans; fixture dirs date-named vs README's wasm-hash convention (loses hash linkage). conf=high

## N1 governance-build
- N1-01 MED: make dev-seed + docs-serve invoke nonexistent scripts; CONTRIBUTING tells newcomers to run dev-seed (Makefile:77,342). conf=high
- N1-02 MED lint-bug: lint-docs.sh stale-binary-name pattern `ctx-indexer\|...` inside grep -E never matches (BRE escape in ERE) — guard is silent no-op (lint-docs.sh:126). conf=high
- N1-03 MED docs: CHANGELOG header claims CalVer; project is SemVer (CHANGELOG.md:6). conf=high
- N1-04 MED: commitlint.config.js claims CI enforcement that doesn't exist; scope-enum missing half of recent scopes. conf=high
- N1-05 MED docs: "Redocly" cited ×3; actual generator is Scalar (README:51, Makefile:316, .gitleaks.toml:62). conf=high
- N1-06 MED: SECURITY.md says pkg/client "planned" (shipped); GPG key pointer circular; sla-probe missing from scope (SECURITY.md:29-31). conf=high
- N1-07 LOW: CONTRIBUTING claims signed-commit branch protection; recent main commits all unsigned (CONTRIBUTING:164). conf=high
- N1-08 LOW shell: verify-cdn.sh ((PASS++)) under set -e dies when PASS=0 — exactly the no-CDN case it documents (verify-cdn.sh:16,71). conf=high
- N1-09 LOW: Makefile COVER_THRESHOLD dead + comment wrong (staticcheck doesn't enforce coverage); CONTRIBUTING says 80 vs 70. conf=high
- N1-10 LOW docs: CLAUDE.md "13 GETs" (now ~45); "docs-all regenerates from obs Name: fields" (docs-metrics is a no-op per F-1256) (CLAUDE.md, AGENTS.md:51). conf=high
- N1-11 LOW: CHANGELOG [v0.5.0-rc.38] section exists but tag never cut (rc.23 has "cancelled" convention; rc.38 doesn't). conf=high
- N1-12 LOW: no .dockerignore — COPY . . ships local data/secrets dirs into build context ×6 Dockerfiles (keep .git for buildvcs). conf=med
- N1-13 INFO: lint-actions-pinning THIRD_PARTY_RE dead branch. conf=high
- N1-14 INFO lint-policy: golangci disables staticcheck/errcheck/gosec wholesale in _test.go contradicting its header; G115 excluded repo-wide next to ADR-0003. conf=med
- N1-15 INFO: cut-release.sh still claims container images (F-1221 missed it); VERSIONS.md broken link; .gitignore missing /ratesengine-sla-probe. conf=high
- CLEAN: no replace directives; tool pins match VERSIONS.md; verify.sh exit-code handling correct; lint-imports baseline empty; no tracked secrets; gitleaks allowlists documented.

## N2 ci
- N2-01 MED security: ci.yml only workflow with NO permissions block — default-scope token across 12 jobs running arbitrary repo code; main unprotected (ci.yml:1-67). conf=high
- N2-02 MED: ci.yml comment claims trivy/SBOM live in security.yml — file doesn't exist; no container/dep scanning anywhere (ci.yml:203). conf=high
- N2-03 MED gate-gap: paths-ignore '**.md' + docs/** — lint-docs.sh NEVER runs on docs-only changes (the class most likely to break it) (ci.yml:8-14). conf=high
- N2-04 MED gate-gap: openapi job PR-only — direct pushes (the live-in-dev norm) skip Spectral + drift check, the exact F-1231 bypass (ci.yml:268). conf=high
- N2-05 MED memory-stale: "ci.yml is PR-only" institutional memory FALSE — push trigger re-enabled per F-1231; memory reference_ci_cost_model needs update. conf=high
- N2-06 LOW: deploy.yml health_grace_seconds type=string passed unvalidated to ansible -e; comment claims type=number enforcement (deploy.yml:51,294). conf=high
- N2-07 LOW: skip-ci label skips lint AND build (needs-propagation) but not test — undocumented asymmetric bypass (ci.yml:76). conf=med
- N2-08 LOW supply-chain: release.yml deletes+recreates existing release on re-run — published-tag assets silently replaceable; cosign "planned not wired" (release.yml:240). conf=med
- N2-09 LOW: tool checksums stored as repo SECRETS (unauditable; breaks fork PRs post-public-flip) — checksums are public values, belong in-tree (ci.yml:59,190,230). conf=high
- N2-10 INFO: api-docs.yml two floating-tag actions/* (policy-exempt but inconsistent); checkout/setup-go SHA drift across workflows. conf=high
- N2-11 INFO: PR template claims CI-enforced coverage-no-decrease; no such gate. conf=high
- N2-12 INFO: dead ISSUE_TEMPLATE paths-ignore; k6-weekly cron disabled (billing cap) = SLA regression alarm manual-only. conf=high

## N3 ansible
- N3-01 HIGH destructive-bug: prometheus role rules-source dir resolves to NONEXISTENT path (playbook_dir/../deploy vs ../../..) — find returns empty → stale-cleanup DELETES every alert rule on host + reloads Prometheus with zero rules; gated only by prometheus_pair group absence today (prometheus/defaults:45, 03-prometheus-configure.yml:19-46). conf=high
- N3-02 MED rebuild-gap: archival-node role chowns to ratesengine user it never creates + enables sla-probe.timer it never installs — fresh rebuild fails twice (10-observability.yml:31,91; 04-users.yml). conf=high
- N3-03 MED rebuild-gap: galexie-archive-writer MinIO user/policy never provisioned though role renders creds for it — first backfill fails with InvalidAccessKeyId (07-galexie.yml:212, 09-minio.yml). conf=high
- N3-04 MED secrets-var: reader secret referenced as BOTH vault_ratesengine_reader_secret_key and ratesengine_reader_secret_key — one render path breaks or empties (ratesengine.env.j2:24 vs 09-minio.yml:266). conf=med
- N3-05 MED: loki role config invalid for its own pinned Loki 3.2.0 (boltdb_shipper shared_store removed in 3.0; schema v12) and diverges from shipped R1 config (tsdb/v13) (loki/defaults:7, loki-config.yaml.j2:40). conf=med
- N3-06 MED unit-drift: TWO divergent canonical unit copies (role templates User=root/env-ratesengine vs deploy/systemd User=ratesengine/hardening); role enables BOTH sla-probe stacks (two 15-min probe loops) (templates/systemd vs deploy/systemd; 10-observability:91 + 17-healthchecks:223). conf=high
- N3-07 MED stale: node-healthcheck.sh checks legacy node_exporter service that the role deliberately stops (#33 cutover) — perpetual red heartbeat on migrated hosts; stellar-rpc remnants (node-healthcheck.sh:61, 10-observability:121). conf=med
- N3-08 MED rebuild-gap: role assumes redis-server + clickhouse-server exist but provisions neither; CH log tasks chown to missing user — fresh apply fails mid-play (15-log-discipline:187, 16-prometheus-exporters:54). conf=high
- N3-09 LOW secrets: MinIO/Redis creds passed on argv despite comments claiming env ("never land on argv") — visible in /proc/auditd (09-minio.yml:99, redis tasks). conf=high
- N3-10 LOW: prometheus role scrape ports diverge from live R1 (api 9464 vs 3000, agg 9464 vs 9465) — future multi-host deploy scrapes dead ports (prometheus.yml.j2:44 vs prometheus.r1.yml:47). conf=high
- N3-11 LOW: fail2ban jail.local has no restart handler — config changes inert on re-apply (12-hardening.yml:27). conf=high
- N3-12 LOW stale: README advertises deleted stellar-rpc tasks; Go preflight check claimed but absent; toml listen_addr comment contradicts value (README:44, 14-services:8, ratesengine.toml.j2:125). conf=high
- N3-13 LOW: promtail journal noise-drop regex double-escaped — never matches; noise ships to Loki (the disk-fill class) (promtail-config.yaml.j2:41). conf=med
- N3-14 INFO CLEAN: vault encrypted, no tracked secrets, ADR-0002 S3-only holds (F-0031 stayed removed), perms 0600/0640 + no_log consistent. conf=high

## N4 configs-deploy
- N4-01 HIGH dead-alert: supply-refresh _stalled can NEVER fire — timestamp() tracks scrape time (bounded by 15s while up; no-data when down); max() also masks per-asset stalls [confirms G14-03 alert side] (supply-refresh.yml:38). conf=high
- N4-02 HIGH dead-alert: infra ZFS/NVMe alerts reference metrics node_exporter doesn't emit (node_zfs_pool_state vs node_zfs_zpool_state lowercase; node_disk_io_errors_total + node_nvme_temperature_celsius don't exist) — zfs_pool_degraded PAGE dead on a host with a real 2026-05-17 zpool incident (infra.yml:58,73,88). conf=high
- N4-03 HIGH dead-alert: stellar.yml archive-divergence page ("SEV-1 correctness signal", header claims "Active") references ratesengine_archive_divergence_total; code emits ratesengine_verify_archive_mismatches_total — real divergence would NOT page (stellar.yml:27,96 vs metrics.go:1365). conf=high
- N4-04 HIGH config-drift: [confirms G19-01, adds 3 more fields] 7+ default: tags disagree with Default() — signup verification OFF, cookie_secure false, TLS probe disabled (kills its alert series), divergence interval 0 (F-0030 burn back), persist_per_source doc-true actual-FALSE (operator enabling projector per docs jumps to Phase-4 sole-writer → G9-01/G16-01 sep41 loss goes LIVE), auth_backend, dashboard fields (config.go:509,598-792). conf=high
- N4-05 MED dead-selectors: timescale_primary_down (up{job="postgres",role="primary"} — no such job), redis_master_down (redis_up{role=} — no role label), redis_replication_broken (redis_expected_slaves defined nowhere) (storage.yml:10, cache.yml:10,54). conf=high
- N4-06 MED coverage: source-stopped alert family missing all 4 newest sources (soroswap-router, defindex, cctp, rozo) — hardcoded label_replace allow-lists guarantee recurrence (ingestion.yml:20-95). conf=high
- N4-07 MED alertmanager: Slack templates read .Annotations.runbook_url but rules ship it as LABEL — runbook links never render; inhibit equal:[alertname,component] no-op (page/ticket siblings never share alertname) (alertmanager.r1.yml:89,62). conf=high
- N4-08 MED stale: prometheus READMEs claim cache/storage/stellar rules "intentionally NOT shipped" + exporters "not deployed" — false since F-0152 (rules.r1/README:24, README:16). conf=high
- N4-09 MED drift: rules.r1 ingestion gap-alert annotation instructs DELETED <source>-backfill subcommand; multi-host copy was fixed, R1 overlay (the deployed one) wasn't (rules.r1/ingestion.yml:317). conf=high
- N4-10 MED drift: deploy/systemd/sla-probe.service ships CONCURRENCY=4 + TEXTFILE_OUTPUT= empty — both reverted vs F-1305/F-1311/F-1300 fixes applied only to healthchecks wrapper; concurrency 4 self-induces the documented false-positive (sla-probe.service:55). conf=high
- N4-11 MED masking: divergence error_dominant (sum() with 2 references — one fully down: error==ok never >) + supply-refresh 50% variant — same class as the known max() masking (divergence.yml:74, supply-refresh.yml:101). conf=med
- N4-12 MED multi-host: infra host_down uses job="node" (ansible defines node_exporter) — never fires; meta.yml alerts on postgres/pgbackrest/minio jobs ansible never defines — fire PERMANENTLY on future R2/R3 (infra.yml:11, meta.yml:18,100). conf=high
- N4-13 LOW caddy: /metrics block present (lens pass); access-log healthz-skip comment unimplemented; real bypass surface is 0.0.0.0:3000 bind (defense rests on nftables) (Caddyfile.api:97,130; example.toml:233). conf=high
- N4-14 LOW stale ×6: tip-lag timer thresholds, loki README "default packaged", all_ingestion_down cites stellar-rpc, api.service cites HAProxy, comms README CalVer, monitoring README layout omits 4 rule files. conf=high
- N4-15 LOW hardening: ch-* oneshots run as root with zero hardening; daemons lack ProtectHome; timer-paired services keep [Install] (boot-time full-verify foot-gun) (ch-live-catchup.service:6). conf=high
- N4-16 LOW: dev compose binds PG/Redis/MinIO on all interfaces with fixed weak creds; no ClickHouse service despite ADR-0034 dual-sink (dev.yaml:35). conf=med
- N4-17 LOW: alertmanager apply.sh strip_block regex fragile (blank-line dependent), marker arg ignored (apply.sh:53). conf=med
- N4-18 LOW: verify-archive _run_stale measures timer FIRING not clean completion — "every attempt failed" never trips it (verify-archive.yml:62). conf=high
- N4-19 INFO: heartbeat coverage complete for 3 daemons; README tuning values lag F-1305 (concurrency 2 vs 1). conf=high

## N5 openapi-examples
- N5-01 HIGH contract: ≥8 endpoints declare BARE response schemas but handlers return the Envelope (issuers, issuer-detail, diagnostics/cursors, diagnostics/ingestion, contracts/transfers, network/stats, incidents, changes) — codegen clients can't parse; in-spec comments ADMIT the drift (spec:1336-2087, envelope.go:90). conf=high
- N5-02 MED enums: price_type missing "peg" (confirms G2-06) AND chart response enum missing "market_cap" while its own request param allows it (spec:4178,4332; chart.go:561). conf=high
- N5-03 MED params: Quote documented optional/default-USD but /history,/ohlc,/vwap,/twap 400 without it — shared component conflates two contracts (spec:3177; history.go:415). conf=high
- N5-04 MED: /readyz drift — "unready" missing from enum; 503 body is Envelope not problem+json as spec claims; checks/uptime/status_root undocumented (spec:145,3691; server.go:1308). conf=high
- N5-05 MED: /assets asset_class param (major behavior dispatch, explorer uses it) UNDOCUMENTED; spec prose claims filters ignored (false); "clamped" limit actually 400s (spec:221; assets.go:381,360). conf=high
- N5-06 MED: Postman collection ~1 month stale (5 endpoints missing); README describes nonexistent bearerToken variable + wrong baseUrl; no freshness check anywhere (examples/postman/). conf=high
- N5-07 LOW: curl 04-assets.sh uses nonexistent order= param; README claims bare "USD" quotes work (400s). conf=high
- N5-08 LOW: spec claims XLM rejected; code aliases XLM→native (F-0024) (spec:3161; asset.go:244). conf=high
- N5-09 LOW: 4 tags used-not-declared; /vwap+/twap tagged "prices" vs siblings "price". conf=high
- N5-10 LOW: invalid examples — 60-char G-strkey, k_ vs kid_ prefix (spec:1097,2627). conf=high
- N5-11 LOW: /signup omits 429 throttle + 500 + two response fields; /price/batch documents only 200 (spec:2604,596). conf=high
- N5-12 INFO policy: /changes serves prices as JSON numbers — spec matches code but contradicts "Amounts are always strings" (spec:1583; changes.go:36). conf=high
