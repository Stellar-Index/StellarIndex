# Audit №3 — coverage ledger (HEAD f8c099ee, 3,321 files)

Systematic proof of what was examined. Every unit ends FINDING / EXAMINED-SOUND /
NOT-EXAMINED. RECON = deep-covered in the recon phase (recon/*.md). WAVE-n = finder wave.

## Recon-covered (deep, cited, skeptic-verified findings)
- R-money: internal/aggregate money paths + internal/storage/timescale money paths → recon/money-*.md (M-A..M-G)
- R-ingest: internal/{dispatcher,projector,pipeline,completeness}, internal/ops/chops, internal/storage/clickhouse {state_write,ops_by_source,completeness,account,sdex_orderbook} → recon/ingest-invariants.md (W-1,F-2..F-10,D-1)
- R-web: web/explorer security surface, internal/config, internal/auth surface, CI gate structure → recon/web-config-trust.md (R1..R10)
- R-entry: all 127 routes + 60 CLI + 45 workers + timers → recon/entrypoints.md
- R-arch: topology + data authority + 6 seams → recon/architecture.md (13 contradictions)

## Finder waves (this sweep)
- WAVE-1 (decoders/sources): sources/external, sources/<onchain>, sources/<observers>, dispatcher decoders, scval/xdrjson/events/contractid
- WAVE-2 (api/auth/platform): api/v1 handlers (pricing/catalogue/explorer/account/admin/dashboard/signup/stripe/sep10), auth, ratelimit, streaming, platform, usage, customerwebhook, pricealerts, notify
- WAVE-3 (aggregate/supply/canonical/ops-cli): aggregate non-money (freeze/divergence/confidence/mev/baseline), canonical, supply, ops/{ingest,archive,diagnostics}
- WAVE-4 (storage/migrations/public/cmd/small-pkgs): storage remainder, migrations DDL, pkg/client, cmd mains, small pkgs
- WAVE-5 (non-go): web component correctness, configs/ansible+prometheus+monitoring, scripts/ci, .github/workflows, test/

## File closure
git ls-files = 3,321. Reconciliation appended per wave (every file → ≥1 unit or explicit NOT-EXAMINED).

## Dispositions (appended per wave completion)
WAVE-1 DONE (5 finders): sources/external (47), sources/{soroswap,soroswap_router,
aquarius,phoenix,comet}, sources/{blend*,sorocredit,defindex,cctp,rozo,band,reflector,
sdex}, sources/{sep41_*,trustlines,accounts,claimable_balances,sac_balances,
liquidity_pools,classicmovements,sorobanevents}, scval/xdrjson/events/contractid/
canonical. Net 0 crit/high; 3 MED (W1-supply-1 sac_balances canon under-report,
W1-defi-1 sdex zero-leg batch failure, W1-sub-1 AsBool-panic+no-recover pair); LOW/INFO
rest. Re-queues: gating-seed-walk (forged-creation), chainlink/frankfurter,
live/lake state-write parity test, blend/storage.go+consumer.go, migration PK 0058/0063.

WAVE-2 DONE (4 finders): api/v1 pricing+history handlers, catalogue+explorer+defi handlers,
auth/mutating/billing surface, platform/usage/webhook/notify/incidents. Net 0 crit/high;
4 MED (SAC-label spoof [security], movements filter-after-limit, usage over-count, postgres
kill-switch cache [gated off-r1]); pricing DoS refuted (0037 index). Re-queue: ratelimit
internals, status.go honesty, assets_f2 market-cap decimals, catalogue caches, streaming internals.

WAVE-3 DONE (4 finders): aggregate freeze/anomaly/mev/confidence/baseline, divergence/domain/
guards, ops ingest/chops CLI, ops archive/diagnostics/supply CLI. Net 2 HIGH candidates
(freeze sibling-release, resume-stalled usd_volume NULL) → skeptics running; decimalsguard
100× price MED; F-2 extended to backfill/router (≥4 offenders); destructive/verify ops SOUND.
Re-queue: opsutil job_heartbeat, ch_gate/gated_recon_seed.

WAVE-4 DONE (4 finders): migrations DDL (120 pairs), cmd mains wiring, storage clickhouse/
timescale remainder + redis, pkg/client + ratelimit/streaming/ledgerstream/hashdb/metadata/
currency/obs. Net 1 CONFIRMED HIGH (W4-obs-1 metric-cardinality DoS — the top finding),
1 MED (aggregator panic isolation), 1 MED (dup events), migrations CLEAN, ratelimit fail-closed
math SOUND, SSRF/streaming SOUND, pkg/client money-strings SOUND. SKEPTICS: freeze-1 CONFIRMED
HIGH; resume-stalled CONFIRMED but DOWNGRADED MED (recoverable, gen-0); freeze-2 DOWNGRADED LOW.

WAVE-5 DONE (3 finders): web/explorer+status component correctness, configs/ansible+caddy+
prometheus+monitoring rules, scripts/ci gates + workflows + scripts/dev/ops. Net: CI+monitoring
"verification-layer gaps" cluster (3 dead alerts, 5 false-green gates/monitors) — the most
launch-relevant; web 1 MED (partial-bucket /diagnostics) + LOW chart-hardening; format/chart
core + injection-clean workflows + rule-tree equivalence + destructive-ops-limits SOUND.

═══ ALL 5 WAVES + RECON COMPLETE. 2 CONFIRMED HIGH, ~26 MED, 7 refuted. See executive-summary.md. ═══

## Negative-space (what SHOULD exist but doesn't — surfaced across waves, verified)
- Panic isolation on the aggregator (API has it; W4-cmd-1). recover() around dispatcher Decode
  (W1-sub-1). A dirty-window guard on ALL below-watermark writers (≥4 miss it; recon F-2 + W3-ops).
- promtool tests on the 25/30 untested rule files (dead alerts persisted for lack of them; W5-mon).
- A frontend ADR-0003 lint (Go-only today; recon R9). A JSON-LD/react-no-danger gate (R6).
  A vacuous-run guard on hubble-check (every sibling has one; W3-archive-2).
- .gitleaks.toml/.gitleaksignore in the anti-bypass tripwire (W5-ci-2). A curl-error=FAIL in
  route-sweep + reconcile-vs-horizon (W5-ci-3/4). verify.sh main-CI-health check (W5-ci-6).

## Dry-wave / NOT-EXAMINED residual (documented; low expected yield; a future incremental pass)
chainlink/frankfurter connectors; clickhouse readers grep-scanned no-trap (wasm/ttl/contiguity/
hashchain/tx_index/pool-state); dashboard/detail/embed web views; ansible haproxy/patroni/loki/
redis-sentinel/prometheus sub-roles; ~half the ci-lint internals; soroswap factory-seed genesis-walk
completeness (gating-seed forged-creation question); live/lake state-write parity test; opsutil
job_heartbeat; streaming/redispub. NO unexamined money/security FLOW left un-reasoned-about.
