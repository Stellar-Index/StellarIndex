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

[WAVE-2 in flight: api/v1 handlers, auth, ratelimit, streaming, platform/billing/webhooks]
