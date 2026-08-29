---
title: Runbook — external-poller-error-rate-high
last_verified: 2026-08-29
status: current
severity: P3
---

# Runbook — `stellarindex_external_poller_error_rate_high`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_external_poller_error_rate_high` |
| Severity | P3 — `severity: informational` (it does **not** raise a ticket; the escalation is `stellarindex_external_poller_stale`, which is where a poller that stops producing entirely lands) |
| Detected by | `deploy/monitoring/rules/external-pollers.yml` + `configs/prometheus/rules.r1/external-pollers.yml` |
| Expr | error rate ÷ (success + error) over `[15m]` > 0.5, `for: 15m` |
| Typical MTTR | 15–60 min |
| Impact | Depends on the source's class (`internal/sources/external/registry.go`). Exchange-class venues (`binance`, `kraken`, `bitstamp`, `coinbase`, `exchangeratesapi`) are `IncludeInVWAP: true`, so their loss degrades the aggregate for their pairs and sets `flags.reduced_redundancy = true` (ADR-0008). Aggregator-class (`coingecko`, `coinmarketcap`, `cryptocompare`) and authority-sanity (`ecb`) sources are `IncludeInVWAP: false` — losing them degrades cross-checks and FX sanity, not the VWAP itself. |

## Symptoms

The shipped rule (both trees):

```promql
(
  rate(stellarindex_external_poller_polls_total{outcome="error"}[15m])
  /
  (
    rate(stellarindex_external_poller_polls_total{outcome="success"}[15m])
    + rate(stellarindex_external_poller_polls_total{outcome="error"}[15m])
  )
) > 0.5
```

sustained `for: 15m`. Two things worth knowing about the shape:

- The denominator is **success + error**, not the whole counter. There is a
  third outcome, `skipped` (`internal/sources/external/runner.go` — a poller
  that reached its cooldown, or an oracle with no new round), and it is
  deliberately excluded: a poller sitting in a post-429 cooldown does not
  dilute the ratio.
- Both windows are 15 min and `for:` is 15 min, so a genuine trip needs ~30 min
  of degradation. A 5-minute vendor blip cannot fire this.

Corroborating signals:

- Indexer log shows repeated `WARN poller error source={vendor}` — the external
  pollers run in `stellarindex-indexer`, not the aggregator.
- `/v1/sources?include=stats` shows the affected vendor with a stale
  `last_event_unix`.

## Quick diagnosis (≤ 5 min)

```sh
# Which source(s) are erroring + at what rate
curl -s 'http://localhost:9090/api/v1/query?query=sum%20by%20(source)%20(rate(stellarindex_external_poller_polls_total%7Boutcome%3D%22error%22%7D%5B15m%5D))'

# The actual error message in the indexer log (pollers live in the indexer)
journalctl -u stellarindex-indexer -n 500 --no-pager | grep -iE 'poller error.*source=' | tail -20

# Manual probe of the vendor endpoint with our typical request
# (replace BASE/QUERY for the affected venue per internal/sources/external/<vendor>/)
curl -sv 'https://api.coingecko.com/api/v3/simple/price?ids=stellar&vs_currencies=usd' 2>&1 | head -20
```

Key signals:
- **HTTP 429** → vendor rate-limit. Check our poll cadence vs their published cap; upgrade to a paid tier if traffic grows. Vendor-specific guidance for CoinGecko in [§ Vendor-specific 429 patterns](#vendor-specific-429-patterns) below.
- **HTTP 401/403** → API key rotated or revoked; check the env var the binary reads (per `internal/sources/external/<vendor>/poller.go`). For CoinGecko specifically a 403 often means the public-no-auth tier was hit — see [§ Vendor-specific 429 patterns](#vendor-specific-429-patterns).
- **HTTP 5xx** → vendor outage; check their status page.
- **Connect timeout** → DNS or network egress issue; if *every* poller is erroring at once this is a host problem, not a vendor problem ([host-down](host-down.md) / [all-ingestion-down](all-ingestion-down.md)).
- **Schema parse error** → vendor changed their response shape; per CLAUDE.md "external sources" surprise list, this is recoverable but requires a code update.

### Vendor-specific 429 patterns

#### CoinGecko

Our poller (`internal/sources/external/coingecko/poller.go`) has built-in
cooldown handling: exponential backoff from `MinBackoff = 60s` to
`MaxBackoff = 1h`, honours `Retry-After`, and treats 403 the same as 429
(CoinGecko post-2024 returns 403 instead of 429 when the public-no-auth tier
is denied).

**Keys are env-wired, not config-file fields.** `cmd/stellarindex-indexer`
reads `COINGECKO_API_KEY` (Pro, sent as `x-cg-pro-api-key`) and
`COINGECKO_DEMO_API_KEY` (demo, sent as `x-cg-demo-api-key`) from the
process environment — on r1 that is `/etc/default/stellarindex`. Pro wins when
both are set. There is no `[external.coingecko] api_key` TOML surface for the
poller, which is deliberate: an operator can fix a key without a schema change.

| Tier | Request cap | Symptom when exceeded | Operator action |
| ---- | ----------- | --------------------- | --------------- |
| Public (no auth) | ~5-15 req/min, IP-throttled — increasingly tightened since late 2024 | HTTP 403 with no `Retry-After`. Poller arms 60s cooldown, doubles each consecutive denial. | Provision a free demo key at coingecko.com/api/pricing and set `COINGECKO_DEMO_API_KEY=` in `/etc/default/stellarindex`, then restart the indexer. |
| Demo (free signup) | 30 req/min, 10,000 calls/day | HTTP 429, sometimes with `Retry-After`. Same backoff path as public. | Check the daily budget below before assuming the cap is our fault. |
| Pro (paid) | 500 req/min (Analyst) → 1000 req/min (Pro) → custom (Enterprise) | HTTP 429 only when the paid cap is exceeded; rare. | Check whether CG is in incident state (status.coingecko.com); if not, raise the poll interval until the next billing cycle. |

**The daily budget, as shipped (F-0030).** Every CoinGecko caller batches, so
per-slug / per-pair arithmetic no longer applies:

| Caller | Shape | Calls/day |
| ------ | ----- | --------- |
| `coingecko` poller (indexer) | ONE batched `/simple/price?ids=…&vs_currencies=…` per tick, `DefaultPollInterval = 300s` | ~288 |
| divergence CoinGecko reference (aggregator + API) | ONE batched `/simple/price` per tick burst (`batchTTL` 25 s), and the divergence pass itself is gated by `divergence_min_interval_seconds` (default 300) | ~288 per binary |
| divergence **supply** CoinGecko reference (`/coins/{id}`) | off by default (free tier 429-throttled since 2026-06-19; needs a Pro key) | 0 |

That is comfortably inside the demo tier's 10,000/day. So a 429 on r1 today is
**not** a catalogue-growth problem — it is one of:

1. **No CG key configured at all** — the public tier is the new default-deny.
   Confirm with the indexer's startup line (below); `auth_mode=anonymous` is
   the tell.
2. **Several binaries sharing one key AND one egress IP** — indexer, aggregator
   and API all call CoinGecko from r1, and per-IP throttling counts them
   together. An ad-hoc `stellarindex-ops verify-external` run adds to the same
   bucket; run it on demand, not in a loop.
3. **A genuine CoinGecko incident** — status.coingecko.com.

Quick CG diagnosis on R1:

```sh
# Which CG tier is the binary using? Logged once at startup.
journalctl -u stellarindex-indexer --no-pager | \
  grep -F 'external poller enabled' | grep -F 'source=coingecko' | tail -1
# → … source=coingecko pairs=N poll_interval=5m0s auth_mode=anonymous|demo|pro

# Manual probe with the SAME key the binary uses (demo key travels as a HEADER):
curl -sv -H "x-cg-demo-api-key: KEY" \
  "https://api.coingecko.com/api/v3/simple/price?ids=stellar&vs_currencies=usd" 2>&1 | head -20

# The backoff state is reported in the poll error itself.
journalctl -u stellarindex-indexer -n 500 --no-pager | \
  grep -iE 'coingecko.*(throttled|backing off)' | tail -5
```

## Mitigation (≤ 15 min)

- [ ] Step 1 — if HTTP 429, slow down the poll cadence in `[external.<vendor>] poll_interval` (operator config in `/etc/stellarindex.toml`) and restart the indexer (`systemctl restart stellarindex-indexer`).
- [ ] Step 2 — if HTTP 401/403, rotate/provision the key in `/etc/default/stellarindex` (CoinGecko: `COINGECKO_API_KEY` / `COINGECKO_DEMO_API_KEY`) and restart the owning binary. Confirm the new tier on the `external poller enabled … auth_mode=` line.
- [ ] Step 3 — if vendor outage, no action needed; the aggregator's class-aware fallback (ADR-0008) keeps `/v1/price` serving from remaining sources. Update the status page only if `flags.reduced_redundancy=true` propagates to a customer-visible pair.
- [ ] Step 4 — if schema drift, the decoder needs a code update. The parse path depends on the vendor's shape: streaming CEX venues keep it in `internal/sources/external/<vendor>/parse.go` (binance, bitstamp, coinbase, kraken); poller-only vendors (coingecko, ecb, cryptocompare, coinmarketcap, exchangeratesapi, polygonforex) decode inline in their `poller.go`. External venues have **no** `dispatcher_adapter.go` — that file belongs to the on-chain Soroban sources under `internal/sources/<protocol>/`. Out-of-cycle release per `release-process.md`.
- [ ] Verification: the ratio drops below 0.5 and the alert clears. Give it a full 15-minute window plus the `for: 15m` hold — the rate windows are 15 min, so a fix is not visible immediately.

## Root cause analysis

For postmortem capture:
- Full poller log for the affected vendor over the previous 24 h.
- Vendor's status page screenshot at the time the alert fired.
- The vendor's parse path (see Step 4) diffed against the captured response if schema drift is suspected.

## Known false-positive patterns

- **Short bursts during vendor maintenance windows** — most CEX vendors have published maintenance windows. Cross-reference the firing time against their status page before investigating. The 15 min rate window + `for: 15m` already absorbs anything under ~30 min.
- **Very low poll volume.** A source polled once an hour has a handful of samples in a 15-minute window; a single error can put the ratio over 0.5. Check the absolute counters, not just the ratio, before treating it as systemic.

## Related

- `external-poller-stale.md` — adjacent alert when a poller stops producing entirely. Until 2026-08-29 **this** alert's `runbook_url` pointed there instead of here; both trees now point at this file.
- `aggregator-fx-snap-fallback-dominant.md` — fires when an FX vendor's failures push us to the snap fallback path.
- ADR-0008 — HA topology + reduced-redundancy flag semantics.
- CLAUDE.md "External sources" surprise list — vendor-specific schema quirks.

## Changelog

- 2026-08-29 — re-verified against HEAD (runbook Wave L, #319): severity is
  `informational`, not ticket; the expr uses `[15m]` windows with a
  success+error denominator and `for: 15m` (the page claimed 5m/5m throughout);
  the `skipped` outcome and its exclusion documented; CoinGecko quota section
  rewritten — the poller makes ONE batched `/simple/price` call per 300 s tick
  (F-0030), so the per-slug arithmetic ("~25-28 currencies at 60 s") was
  obsolete, and the divergence reference is batched too (the verifier's
  "per-pair lookups" reading was itself stale); keys are env-wired
  (`COINGECKO_API_KEY` / `COINGECKO_DEMO_API_KEY` in `/etc/default/stellarindex`),
  not a `DemoAPIKey` config field, and the demo key travels as a header;
  impact restated per source CLASS (`IncludeInVWAP`); the schema-drift step
  pointed at a non-existent external `dispatcher_adapter`. Rules-side: the
  alert's `runbook_url` pointed at `external-poller-stale.md` in both trees and
  now points here.
- 2026-06-12 — F-1330: fix metric name (`stellarindex_external_poller_polls_total`,
  not `_poller_total`); pollers run in `stellarindex-indexer` not the
  aggregator (log + restart targets corrected); config key is
  `[external.<vendor>] poll_interval`.
- 2026-05-12 — initial draft (audit-2026-05-12 F-1237 closure).
