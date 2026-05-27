# Mandatory Journeys

Every journey is traced end-to-end from input to output, with code
and evidence references at each hop. Trace logs go in
[journeys-traces/](journeys-traces/) using
[journeys-traces/_template.md](journeys-traces/_template.md).

The journey set is intentionally granular (one per source, one per
oracle, one per external venue, one per page family, one per
operator workflow) so each trace is unambiguously closeable.

## Conventions

Each journey's trace must record:

- **Inputs.** What enters the system (XDR ledger, HTTP request,
  cron tick, vendor REST response).
- **Hops.** Every package boundary the data crosses, with
  file:line references.
- **Sinks.** Where data finally lands (DB row, Redis key, HTTP
  response, log line, metric, alert).
- **Failure modes.** What happens at each hop on bad input or
  upstream failure.
- **Tests.** Which test files exercise this journey end-to-end.
- **Live R1 evidence.** Where applicable, capture a live trace
  of the journey on R1.

## Data-Plane Journeys (J01..J20)

### J01. On-Chain Trade Ingest (Soroswap)

Galexie writes LedgerCloseMeta to MinIO →
`internal/ledgerstream` reads → `internal/dispatcher` matches
event topic → `internal/sources/soroswap.Decode` extracts
`SwapEvent` + paired `SyncEvent` → `internal/pipeline.Sink`
writes Trade → `internal/storage/timescale/trades.go` inserts
into `trades_*` hypertable → continuous aggregate refresh →
served via API J05.

Validate: SwapEvent without paired SyncEvent (skip + warn? panic?
silently lose reserves?). Validate: post-rc.80 soroban_events
shadow capture (every soroswap event also lands in
soroban_events via the RawEventSink hook).

### J02. On-Chain Trade Ingest (Phoenix 8-event grouping)

Galexie → ledgerstream → dispatcher → Phoenix decoder groups all
8 per-field events by `(ledger, tx_hash, op_index)` → emits one
Trade per swap → sink → trades hypertable.

Validate: 7-of-8 events arriving (truncated tx). Validate:
out-of-order arrival within ledger. Validate: the 4 new actions
(provide_liquidity / withdraw_liquidity / bond / unbond — PR
#27) flow into `phoenix_liquidity` + `phoenix_stake_events`
without disturbing the swap path's buffer.

### J03. On-Chain Trade Ingest (SDEX classic)

Galexie → ledgerstream → dispatcher →
`internal/sources/sdex` decodes classic operations + effects →
emits Trade(s) → sink → trades hypertable. Validate post-CAP-67
unified events vs pre-Whisk operations+effects path (CLAUDE.md
surprise list).

### J04. Oracle Observation Ingest (Reflector / Redstone / Band)

For each oracle:

- **Reflector** (3 contracts: DEX/CEX/FX): decoder → oracle
  observation table → divergence/aggregator references.
- **Redstone Adapter**: `WritePrices` event grouped with op_args
  for feed_id mapping → oracle table → consumer.
- **Band**: Soroban contract emits *zero events*. Dispatcher's
  ContractCallDecoder observes `relay()` / `force_relay()`
  invoke ops → decoder reads op args → oracle table.

Validate: feed_id-count mismatch in Redstone
(`ErrFeedIDCountMismatch` skips the whole event). Validate: Band
invoke arg drift.

### J05. Closed-Bucket Price Path (`/v1/price`)

Client GET `/v1/price?asset=...&quote=...` →
`cmd/ratesengine-api` → middleware → `internal/api/v1/price.go`
→ reads last-closed bucket from Timescale via
`internal/storage/timescale/aggregates.go` → Redis cache lookup
via `internal/cachekeys` → builds envelope with confidence
(W10), divergence_warning (J19), triangulation flag (J06),
freeze flag → returns.

Verify ADR-0015 contract: serves the last *closed* bucket only,
not the in-progress one. Verify error envelope when asset is
unknown.

### J06. Triangulation Path

`/v1/price?asset=XLM&quote=GBP` where no direct XLM/GBP feed
exists → triangulate via `XLM/USD * USD/GBP` →
`internal/aggregate/triangulate.go` → response carries
`triangulated: true`. Validate transitive divergence + provenance.

### J07. Stablecoin Fiat-Proxy Path

Trade ingested as `XLM/USDT` → aggregator maps `USDT → USD` per
ADR-0026 (late binding) → `/v1/price?asset=XLM&quote=USD`
serves the result.

Validate during simulated USDT depeg: late binding does NOT hide
the depeg — divergence_warning fires
(`internal/divergence/depeg_test.go`).

### J08. CCTP Bridge-Out Path (NEW)

CCTP contract emits `deposit_for_burn` →
`internal/sources/cctp.DecodeDepositForBurn` →
`cctp.Event` → `internal/pipeline/sink.persistCCTPEvent` →
`cctp_events` hypertable. Validate: paired `mint_and_withdraw`
on destination (lands separately via different contract).
Validate: `BackfillSafe = true` after rc.80 — historical
backfill via `ratesengine-ops cctp-backfill -from N -to M`
re-feeds soroban_events rows through the live decoder.

### J09. Rozo Intent-Bridge Path (NEW)

Rozo payment contract emits `payment` event with
`{amount, destination, from, memo}` →
`internal/sources/rozo.DecodePayment` → `rozo.Event` →
`rozo_events` hypertable. Validate: `flush` event (intent
settlement) → `flush` branch of decoder. Validate: backfill
subcommand `rozo-backfill`.

### J10. soroban_events Capture (NEW — ADR-0029)

Every Soroban contract event the dispatcher routes is ALSO
written to `soroban_events` as a raw row via the RawEventSink
hook. Hop: dispatcher → `dispatcher.RawEventSink.PushEvent` →
`sorobanevents.AsyncSink.PushEvent` (BLOCKING on full buffer
post-rc.80) → background worker batch → `Store.InsertSorobanEventsBatch`
→ `soroban_events` hypertable.

Validate: PushEvent blocks under back-pressure (no silent drops
in steady state) → cursor doesn't advance past durable writes.
Validate: shutdown race — Stop() closes stopping channel,
in-flight PushEvent returns dropped++; cursor stays behind drop
sequences. Validate: ctx-cancel early-stop watchdog in
`backfill.go` and `cmd/ratesengine-indexer/main.go` unblocks
PushEvent on SIGTERM.

### J11. soroban_events SQL Backfill (NEW — six subcommands)

`ratesengine-ops <source>-backfill -from N -to M` →
`config.LoadWithEnv` → `timescale.Open` →
`Store.StreamSorobanEvents(from, to, contracts, topics, fn)` →
per row: `sorobanevents.Reconstruct` → `<source>.Decoder.Decode`
→ `Store.Insert<Source>Event`. Six instances:
- `cctp-backfill` → `cctp_events`
- `rozo-backfill` → `rozo_events`
- `soroswap-skim-backfill` → `soroswap_skim_events`
  (two-tuple topic; byte-equality filter on `topic_1_xdr`)
- `comet-liquidity-backfill` → `comet_liquidity` (filters out
  swap kind; swap already in trades from live)
- `phoenix-backfill` → `phoenix_liquidity` +
  `phoenix_stake_events` (per-action correlation buffer)
- `blend-backfill` → `blend_positions` + `blend_emissions` +
  `blend_admin` (20 topic kinds → 3 target tables)

Validate idempotency: re-run produces identical row counts (ON
CONFLICT DO NOTHING on each per-source PK).

### J12. Live FX Ingest (Forex circulation_data + Frankfurter)

CSV at `internal/sources/forex/circulation_data.csv` →
worker → cache → aggregator FX snap → `/v1/fx` /
`/v1/assets/{slug}` enrichment. Validate: stale-FX guard rail.

### J13. SEP-1 Resolution

`/v1/assets/{slug}` request → `internal/metadata.LCMResolver` →
SEP-1 toml fetch → DNS validator → SSRF defence
(`internal/metadata/ssrf_internal_test.go`) → caching →
overlay. Validate: malicious SEP-1 (private-IP target, TLS
downgrade, oversized body).

### J14. Stripe Webhook (NEW)

`POST /v1/stripe-webhook` → signature verification (HMAC-SHA256
against Stripe-Signature header + STRIPE_WEBHOOK_SECRET) →
event dispatch (subscription.updated, charge.succeeded,
charge.refunded, invoice.paid) → DB update (apikey tier,
billing rows) → consumer notification. Validate: replay-attack
defence (idempotency-key); webhook secret rotation; unsigned
request rejected with 401.

### J15. Customer Webhook Fanout (NEW)

Trade/oracle/freeze event → `customerwebhook.Worker` → per-key
subscription lookup → fanout to subscriber URL → SSRF defence
(`customerwebhook/ssrf.go`) → retry policy → delivery metrics.
Validate: subscriber URL pointing to localhost / private IPs
is rejected; failed deliveries don't block ingest.

### J16. SEP-10 Authentication

`POST /v1/auth/sep10/challenge` (anonymous) → server signs
challenge transaction with SERVER_KEY → client signs with their
Stellar key → `POST /v1/auth/sep10/verify` → both signatures
verified → JWT minted with 24h expiry → client uses
Authorization: Bearer JWT on subsequent calls.

### J17. Supply Snapshot

`cmd/ratesengine-ops supply` → reads classic supply observers
(`internal/supply/classic.go`), XLM supply
(`internal/supply/xlm.go`), SEP-41 supply
(`internal/supply/sep41.go`) → composes snapshot → writes
textfile collector format → node_exporter scrapes →
Prometheus → `/v1/assets/{slug}.supply` field.

### J18. Verify-Archive Bootstrap + Resume (NEW — W34)

systemd timer `verify-archive-tier-a.timer` fires → systemd
service starts `ratesengine-ops verify-archive -tier chain
-from-last-verified` → reads `/var/lib/ratesengine/verify-archive-state.json`
→ pins prior run's `to` for resume → parallel chunk walker
(12 chunks) → each chunk reads ledger range from galexie →
hash-chain verify → on missing-file: rc.81's
`TolerateTrailingMissing=true` converts to walk-complete →
state updated → Prometheus textfile metric updated.

### J19. Divergence Check (with Chainlink — NEW)

`internal/divergence/worker.go` ticks every N minutes → for each
verified asset, fetch CoinGecko price → fetch
Chainlink HTTP RPC price → fetch our internal price → compute
divergence percentage → flag if > threshold → store in
`divergence_observations`. Validate: chainlink RPC outage doesn't
poison the divergence record (graceful skip).

### J20. Anomaly Freeze

Aggregator detects price spike beyond
`internal/aggregate/anomaly` thresholds → emits freeze event
→ `freeze_events` hypertable → API responses for that
asset/pair carry `freeze: true` flag for the freeze window →
post-freeze recovery sweep
(`internal/aggregate/freeze/recovery.go`) → freeze flag clears
when condition resolves.

## Operator-Plane Journeys (J21..J32)

### J21. Backfill — Soroban-era Ingest

Operator runs `ratesengine-ops backfill -from N -to M -parallel
12 -source soroswap,phoenix,...` → per-chunk cursor seeding →
parallel ledgerstream reads → dispatcher → decoders → sinks.
Validate: cursor coherence (back-pressure prevents producer
from advancing past durable writes); ctx-cancel watchdog;
trailing-edge tolerance for `-to` overshoots.

### J22. soroban-events Fill Walk

Operator runs `ratesengine-ops backfill -source soroban-events
-from N -to M -parallel 12 -refresh-caggs=false` → 12 per-chunk
AsyncSinks → per-chunk dispatcher with RawEventSink wired →
batched inserts into `soroban_events`. Validate: rc.80
back-pressure semantics — no drops on the 2026-05-26 re-walk
(unlike rc.79's drop-happy run which lost 18.86M rows).

### J23. Cold-Tier Read

Cold-tier-enabled (ADR-0027) config → `LedgerstreamConfig`
populates `ColdDataStore` → `streamTiered` builds
`TieredDataStore` → reads miss locally → fallback to cold
(aws-public-blockchain S3) → trim safely possible
(must run §3+§4 together per `feedback_cold_tier_premature_enable`).

### J24. WASM History Audit

Operator runs `ratesengine-ops wasm-history -from N -to M
-parallel K -contracts ...` → walks each ledger →
`extract-wasm-from-galexie` → produces JSONL of (ledger,
contract_id, wasm_hash) tuples → operator inspects → if zero
upgrades observed for the audit range, `BackfillSafe` flag
flipped `false → true` for that source.

### J25. WASM Audit Documentation

Per-source audit doc at
`docs/operations/wasm-audits/<source>.md`. Each lists:
contracts watched, ranges scanned, WASM hashes observed,
decoder coverage decision, BackfillSafe flag final state, audit
decision SQL queries.

### J26. SLA Probe

`cmd/ratesengine-sla-probe` fires every 5 min via timer → hits
13 GETs across `/v1/healthz`, `/v1/readyz`, `/v1/version`,
`/v1/price`, `/v1/markets`, `/v1/assets/{id}`, etc → measures
p50/p95/p99 latencies + freshness → writes textfile collector
format → node_exporter scrapes → Prometheus → rules.r1 fires
on SLA breach.

### J27. Release Cut

Operator runs `bash scripts/dev/cut-release.sh v0.5.0-rc.NN` →
script verifies: SemVer shape, branch=main, clean tree, sync
with origin, CHANGELOG section non-empty, verify.sh passes →
tags + pushes → `release.yml` fires → builds binaries for
linux/amd64 → computes SHA256SUMS → publishes GitHub Release
with auto-extracted notes.

### J28. Deploy to R1

Operator runs `gh workflow run deploy.yml -f region=r1 -f
version=v0.5.0-rc.NN -f binaries=...` → workflow downloads
binaries from Release → verifies SHA256SUMS → Ansible playbook
over SSH does stage → backup → atomic install → restart →
health probe → auto-rollback on failure. Validate: failed
probe actually rolls back; backup retention (5 most-recent).

### J29. Cursor-Stuck Diagnosis

Per `feedback_frozen_indexer_cursor_diagnostic`: `mc stat
<bucket>/<prefix>/<cursor+1>.xdr.zst` — first hypothesis is
that galexie hasn't written the next file. Validate: runbook
at `docs/operations/runbooks/cursor-stuck.md` matches.

### J30. r1-Smoke Health Check

`bash scripts/dev/r1-smoke.sh` runs 13 GETs against local r1
with jq shape assertions; exit code = number of failures → cron
+ healthchecks.io ping. Validate: every assertion still holds.

### J31. Verify Decoders

`ratesengine-ops verify-decoders` walks every registered source
and confirms its decoder's claim surface matches its known
on-chain events. Validate: returns non-zero on any source
where W35 finds a gap.

### J32. Scan Soroban Events Diagnostic

`ratesengine-ops scan-soroban-events -from N -to M -topic0
<symbol> -contract <strkey> -limit K` walks the MinIO archive
(not soroban_events!) with a filter and prints matching events.
Used to investigate decoders / WASM upgrades pre-soroban_events.

## Adversarial Journeys (J33..J40)

### J33. Decoder DoS via Malformed XDR

Send a malformed Soroswap SwapEvent via a chain replay; verify
the dispatcher counts + drops without panicking.

### J34. Aggregator Poison via Single Hostile Source

Hostile Binance proxy returns prices 10x off true. Verify: our
multi-source VWAP + Chainlink divergence reference + outlier
detection isolate the bad source within configured tolerance.

### J35. Rate-Limit Bypass via Forged X-Real-IP

Caddy + Cloudflare set X-Real-IP; we trust Cloudflare's IP
list. Attacker spoofs X-Real-IP from a non-Cloudflare ingress.
Verify: only Caddy-trusted-proxy IPs accept the header.

### J36. API Key Exfiltration

Find every code path that touches API keys; verify each path
treats them as secrets (no logging, no `String()` method that
emits, no debug endpoint dump).

### J37. Webhook SSRF

Customer registers webhook URL `http://localhost:5432` (or
private IPs). Verify: `customerwebhook/ssrf.go` rejects.

### J38. Hostile Ledger Stream Cancel

SIGTERM mid-ingest. Verify back-pressure ctx-cancel watchdog
unblocks PushEvent (rc.80 fix); verify cursor doesn't advance
past dropped events.

### J39. SQL Injection via Asset Slug

`/v1/assets/{slug}` accepts an arbitrary string. Verify: no
direct interpolation into psql; every path goes through
parameterized queries.

### J40. Stripe Webhook Replay

Capture a valid Stripe webhook; replay it. Verify: idempotency
keys prevent double-billing.

## Trace Procedure

Each journey's trace file follows the template at
[journeys-traces/_template.md](journeys-traces/_template.md):

1. Inputs (concrete examples)
2. Hops (file:line per hop)
3. Sinks (table / Redis key / metric / log line / HTTP response)
4. Failure modes (per hop)
5. Tests that exercise (file refs)
6. Live R1 trace excerpt (if applicable)
7. Findings observed (link to F-####)
8. Status: `todo` / `in_progress` / `done`
