# Cross-File Interactions

W26 owns this ledger. Each `XFI-####` row records one material
seam in the system. Closure rule: every required interaction
class (W26 §classes) has at least one fully-traced XFI row.

## Class taxonomy (recap from W26)

| Class | Description |
| --- | --- |
| XFI-CLASS-001 | binary → config → pkg |
| XFI-CLASS-002 | decoder → sink → store |
| XFI-CLASS-003 | workflow → script → artifact |
| XFI-CLASS-004 | alert → runbook → service |
| XFI-CLASS-005 | route → handler → store |
| XFI-CLASS-006 | metric → registration → scrape → rule |
| XFI-CLASS-007 | ADR text → import-boundary lint |
| XFI-CLASS-008 | migration → reader → writer |
| XFI-CLASS-009 | cache key → builder → consumer |
| XFI-CLASS-010 | live R1 traffic → handler → store query |
| XFI-CLASS-011 | RFP row → code path |
| XFI-CLASS-012 | runbook step → operator subcommand |
| XFI-CLASS-013 | systemd unit → binary subcommand |
| XFI-CLASS-014 | proposal feature → API → SDK |
| XFI-CLASS-015 | soroban_events.Capture → AsyncSink → store (ADR-0029) |
| XFI-CLASS-016 | soroban_events row → Reconstruct → decoder → store (SQL backfill) |
| XFI-CLASS-017 | ledgerstream.TolerateTrailingMissing → parseTrailingMissingSeq → walk-complete |
| XFI-CLASS-018 | WASM audit doc → BackfillSafe flag → backfill subcommand allowed |
| XFI-CLASS-019 | Stripe webhook signature → idempotency-key → tier-upgrade → middleware |
| XFI-CLASS-020 | Customer webhook subscription → fanout → SSRF check → HMAC sign → delivery |

## Rows

| ID | Class | A | B | Workstream | Status |
| --- | --- | --- | --- | --- | --- |

(Audit populates as it runs. Seed rows:)

| ID | Class | A | B | Workstream | Status |
| --- | --- | --- | --- | --- | --- |
| XFI-0001 | XFI-CLASS-015 | `internal/dispatcher/dispatcher.go::dispatchOne` calls `rawEventSink.PushEvent(ev)` | `internal/sources/sorobanevents/dispatcher_adapter.go::AsyncSink.PushEvent` blocks on full channel; worker → `Store.InsertSorobanEventsBatch` | W27, W28 | `todo` |
| XFI-0002 | XFI-CLASS-016 | `cmd/ratesengine-ops/cctp_backfill.go::cctpBackfill` calls `store.StreamSorobanEvents` | per row: `sorobanevents.Reconstruct` → `cctp.Decoder.Decode` → `store.InsertCCTPEvent` | W27, W29 | `todo` |
| XFI-0003 | XFI-CLASS-017 | `internal/ledgerstream/ledgerstream.go::Stream` returns SDK error | `maybeTolerateTrailingMissing` parses + decides | W28 | `todo` |
| XFI-0004 | XFI-CLASS-018 | `docs/operations/wasm-audits/cctp.md` audit decision | `internal/sources/external/registry.go` BackfillSafe=true for cctp | W24 | `todo` |
| XFI-0005 | XFI-CLASS-006 | `internal/obs/metrics.go::AggregatorVWAPWritesTotal` declaration + `.Inc()` at `internal/aggregate/orchestrator/orchestrator.go:776` | `configs/prometheus/rules.r1/aggregator.yml:18-20` alert `rate(ratesengine_aggregator_vwap_writes_total[5m]) == 0` | W14 | **F-0080** — unguarded; cascade-fragile |
| XFI-0006 | XFI-CLASS-005 | `internal/api/v1/server.go:981` `GET /v1/price` route | `handlePrice` → `prices.LatestPrice` (Redis hot path) → `priceFallback` chain → `writeJSON` envelope. Returns stale-flagged LKG under F-0039. | W11 | **traced J20** (journeys-traces/J20-price-under-cascade.md) |
| XFI-0007 | XFI-CLASS-006 | `internal/obs/metrics.go::PriceStalenessSeconds` declaration + emit at `internal/aggregate/orchestrator/orchestrator.go::emitStalenessGauges` (end-of-tick) | `configs/prometheus/rules.r1/api.yml::ratesengine_api_price_stale` alert `> 120 for: 5m` | W14 | **F-0104** — cascade-fragile (gauge emitted by failing aggregator) |
| XFI-0008 | XFI-CLASS-009 | `internal/cachekeys/cachekeys.go::VWAP(asset, quote, window)` builder | consumed by `internal/api/v1/price.go::tryRedisVWAPFallback` (line 426) + `internal/aggregate/orchestrator/orchestrator.go` writer | W10 | seam intact; live verified via J20 |
| XFI-0009 | XFI-CLASS-004 | `configs/prometheus/rules.r1/aggregator.yml::ratesengine_aggregator_silent` alert `runbook_url:` field | `docs/operations/runbooks/aggregator-silent.md` runbook | W14 | seam intact (runbook exists) |
| XFI-0010 | XFI-CLASS-019 | `internal/api/v1/stripe_webhook.go:1022-1029` HMAC-SHA256 verify + dedupe via `StripeEventStore.AppendStripeEvent` | tier-upgrade flow → `apikey_postgres.GetByHash` → middleware decides Subject | W33, W19 | **F-0005 POSITIVE** (well-defended) |
| XFI-0011 | XFI-CLASS-020 | Customer webhook subscription → fanout dispatcher | `internal/customerwebhook/ssrf.go::ssrfGuardedDialContext` (DNS-rebind safe) + `isInternalIP` block list → HMAC-sign → POST | W32, W19 | **F-0006 POSITIVE** (SSRF + HMAC done right) |
| XFI-0012 | XFI-CLASS-013 | `configs/ansible/roles/galexie/templates/galexie.service.j2` systemd unit | `cmd/ratesengine-ops` subcommands for galexie scan/trim/rehydrate | W13, W18 | seam intact; Type=notify pattern across r1 services |
| XFI-0013 | XFI-CLASS-006 | `redis_rdb_last_bgsave_status` metric from `redis_exporter` | `configs/prometheus/rules.r1/storage.yml::ratesengine_redis_writes_blocked` alert `== 0 for: 1m` | W14 | **F-0085** — redis_exporter DOWN so metric never reaches Prometheus |
| XFI-0014 | XFI-CLASS-011 | RFP row "real-time fiat rates" | `internal/sources/external/cex/{binance,kraken,bitstamp,coinbase}` + `external/fx/{polygon-forex,exchangeratesapi,ecb}` | W08, W20 | RFP row → 7-source code path; F-0097 POSITIVE shows the surface |
| XFI-0015 | XFI-CLASS-008 | Migration `0041_create_soroban_events.up.sql` schema | `internal/storage/timescale/soroban_events.go` reader/writer + 6 backfill subcommands (cctp/rozo/soroswap-skim/comet-liquidity/phoenix/blend) | W09, W27 | **F-0079 POSITIVE** — design implemented as spec'd |
| XFI-0016 | XFI-CLASS-014 | ADR-0028 (Proposed) `AssetType="rwa"` | `internal/canonical/asset_rwa.go::NewRWAAsset` + 8-code allow-list + RedStone feed-id map at `internal/sources/redstone/feeds.go` | W31 | **F-0110** — ADR Proposed but code shipped |
| XFI-0017 | XFI-CLASS-005 | `GET /v1/account/me` route | requires magic-link session / API key / SEP-10 token; correctly returns 401 RFC-7807 on missing auth | W19, W11 | seam intact (live-verified EV-0045) |
| XFI-0018 | XFI-CLASS-005 | `GET /v1/auth/sep10/challenge` route | `s.handleSEP10Challenge` → SEP-10 validator (returns 503 `sep10-unavailable` when not wired — F-0093) | W19, W11 | configuration drift; not a code defect |
| XFI-0019 | XFI-CLASS-010 | live `GET /v1/diagnostics/ingestion` request | `s.handleDiagnosticsIngestion` → reads multiple storage paths; returns all-zeros while peers report fresh (**F-0095**) | W11, W13 | inconsistent storage reads — open Wave 1 |
