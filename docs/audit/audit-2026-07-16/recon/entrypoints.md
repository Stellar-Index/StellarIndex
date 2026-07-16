# Recon: entry-point extraction (HEAD f84e2d0b)

Extraction-derived flow-unit seed list for the audit. Key leads flagged for finder waves.

## Summary
- 120 registered method+path patterns: 115 match OpenAPI 1:1, 1 CI-exempted undocumented staff route (`GET /v1/account/admin/lookup`, SESS+STAFF), 4 non-versioned utility routes (`/`, `/robots.txt`, `/.well-known/security.txt`, `/metrics`).
- Auth tiers: OPEN / OPT (anon 60/min, key raises tier) / KEY / OP (TierOperator) / SESS / SESS+STAFF / SIG (Stripe HMAC) / LOOPBACK / NONE.
- 6 binaries: indexer, aggregator, api (daemons: `-config -dry-run -version`); ops (55 subcommands + `supply` 2nd-level dispatch); migrate (up/down/status/force/version); sla-probe.

## Mutating routes (audit for authz/IDOR/idempotency)
- Account self-service (KEY): POST/DELETE `/account/keys`.
- Admin (OP, X-Reason audit-logged): POST `/admin/keys`, PATCH `/admin/accounts/{id}`, POST `/admin/status-notices`, POST `/admin/status-notices/{id}/resolve`.
- Signup (OPEN, IP-throttled+email-locked): POST `/signup`, GET `/signup/verify` (token in URL).
- Stripe webhook (SIG): POST `/webhooks/stripe` — empty secret → hard 503 (fail-closed, good). Dedupes via StripeEventStore.
- Dashboard (SESS): keys, webhooks (SSRF-checked URL registration), price-alerts CRUD.
- SEP-10 (OPEN by design): `/auth/sep10/{challenge,token}`.

## LEADS for finders
1. **/metrics loopback asymmetry** — api binary wraps `/metrics` in `loopbackOnly` (server.go:1327, 404s non-loopback). indexer (main.go:1747-1763) + aggregator (~1450-1463) standalone `/metrics` listeners have NO wrapper — any host reaching `obsCfg.MetricsListen` scrapes. SEC/OBS: confirm bound to private iface by config, else info exposure. Also `cross-region-monitor` (:9479) + `verify-archive -metrics-listen` stand up throwaway HTTP servers.
2. **Dead code / drift**: `ops cache-prime` + `verify-invariants` (doc-only, never implemented, main.go:88-90); `cmd/tmpxdrdump/` empty dir; `web/status` unused NEXT_PUBLIC_API_BASE_URL (next.config.mjs:13); `supply seed-sac-balances` reachable but missing from `--help` usageBody (inverse drift).
3. **4 SSE endpoints with no first-party consumer**: `/price/stream`, `/price/tip/stream`, `/observations/stream`, `/oracle/streams` — registered/spec'd/tested, only `/ledger/stream` consumed (status page). Reachable by third-party clients; dead from UI. Verify resource/goroutine lifecycle on abandoned SSE conns.
4. **Cloudflare Pages Function** `web/explorer/functions/og/[[path]].js` — live runtime handler, server-side fetch to api.stellarindex.io/v1/price at request time. Separate exec env from Next static export + Go API. Distinct inbound entry point.
5. **Signup-reaper** worker (api main.go:1183) deletes orphan `accounts` rows (F-1255 race) — audit the delete predicate for over-deletion.
6. **Self-prewarm loop** (api main.go:1226) self-HTTP-calls `/v1/assets/{id}` every 60s — audit for feedback loops / smoke-noise.

## Background workers (per binary) — concurrency/failure audit targets
- indexer: projector (gated), main ledgerstream loop (613), hashdb drift verifier (gated), CH-live-sink watcher, routed-via tagger, discovery-drop watcher, PG ping watcher.
- aggregator: baseline refresher, change/protocol/asset-volume rollups, SEP-41 supply rollup + per-binding supply refresh (one goroutine per binding), supply cross-check, divergence supply cross-check (independent), freeze-recovery, gap detector (5m), MEV worker (5m), decimals-cache refresh + assumption guard, price-alert evaluator (off by default) → enqueues webhooks.
- api: forex-shim (1h), cache prewarm, TLS-cert-expiry probe (gated), backfill-coverage cache (5m), nonstandard-decimals guard cache, stream publisher (gated), ingestion-snapshot refresher (15s), Redis closed-bucket subscriber → /price/stream, customer-webhook delivery worker (5s drain, HMAC, SSRF-guarded), usage-rollup (5m), signup-reaper (gated), self-prewarm (60s).

## Timers (systemd + ansible + healthchecks + GH cron)
- systemd (11): archive-completeness (daily), ch-live-catchup (10m), config-assertions (hourly), galexie-archive-fill (hourly), galexie-archive-tip-lag (5m), galexie-archive-trim (monthly), sla-probe (15m), completeness-incremental (1h), supply-snapshot (daily), verify-archive-tier-a (daily).
- ansible-rendered adds: node-healthcheck, data-freshness (15m), ch-supply (daily), sep1-refresh (daily), pgbackrest-backup (daily), compute-completeness (daily).
- healthchecks: per-binary heartbeat (60s), smoke (5m), sla-probe (15m).
- GH cron: ansible-drift (Mon), k6-weekly (Sun), security (Mon), site-crawl (Mon).

## Outbound webhooks (customerwebhook, SSRF-guarded)
Producers: anomaly.freeze (aggregator), divergence.firing (aggregator), incident.sev1/resolved (ops emit-incident), price.alert (pricealerts worker). Delivery worker.go:218/247, ssrf.go guard.
