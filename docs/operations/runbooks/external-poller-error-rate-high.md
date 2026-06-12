---
title: Runbook — external-poller-error-rate-high
last_verified: 2026-06-12
status: draft
severity: P3
---

# Runbook — `stellarindex_external_poller_error_rate_high`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_external_poller_error_rate_high` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/external-pollers.yml` |
| Typical MTTR | 15–60 min |
| Impact | A specific external poller (CEX or FX vendor) is erroring on most of its scrapes. The aggregator falls back to its remaining sources for VWAP — the customer-visible price still serves but with `flags.reduced_redundancy=true` (ADR-0008). Sustained errors means a vendor is throttling, has changed schema, or is in maintenance. |

## Symptoms

- `sum by (source) (rate(stellarindex_external_poller_polls_total{outcome="error"}[5m])) / sum by (source) (rate(stellarindex_external_poller_polls_total[5m])) > 0.5` for ≥ 5 min.
- Indexer log shows repeated `WARN poller error source={vendor}` (the external pollers run in `stellarindex-indexer`, not the aggregator).
- `/v1/sources?include=stats` shows the affected vendor with stale `last_event_unix`.

## Quick diagnosis (≤ 5 min)

```sh
# Which source(s) are erroring + at what rate
curl -s 'http://localhost:9090/api/v1/query?query=sum%20by%20(source)%20(rate(stellarindex_external_poller_polls_total%7Boutcome%3D%22error%22%7D%5B5m%5D))'

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
- **Connect timeout** → DNS or network egress issue; jump to `host-network` diagnostics.
- **Schema parse error** → vendor changed their response shape; per CLAUDE.md "external sources" surprise list, this is recoverable but requires a code update.

### Vendor-specific 429 patterns

#### CoinGecko (audit-2026-05-12 F-1208)

CoinGecko has three pricing tiers and the 429/403 behaviour
differs across them. Our poller (`internal/sources/external/
coingecko/poller.go`) has built-in cooldown handling:
exponential backoff from `MinBackoff = 60s` to `MaxBackoff = 1h`,
honours `Retry-After`, and treats 403 the same as 429 (CoinGecko
post-2024 returns 403 instead of 429 when the public-no-auth
tier is denied).

| Tier | Request cap | Symptom when exceeded | Operator action |
| ---- | ----------- | --------------------- | --------------- |
| Public (no auth) | ~5-15 req/min, IP-throttled — increasingly tightened since late 2024 | HTTP 403 with no `Retry-After`. Poller arms 60s cooldown, doubles each consecutive denial. | Provision a free demo key at coingecko.com/api/pricing and set the `DemoAPIKey` field via the indexer's CG poller config. |
| Demo (free signup) | 30 req/min | HTTP 429, sometimes with `Retry-After`. Same backoff path as public. | Reduce CG poll frequency, or upgrade to Analyst. |
| Pro (paid) | 500 req/min (Analyst) → 1000 req/min (Pro) → custom (Enterprise) | HTTP 429 only when the paid cap is exceeded; rare. | Check whether CG is in incident state (status.coingecko.com); if not, raise the poll interval until the next billing cycle. |

Quick CG diagnosis on R1:

```sh
# Which CG key is the binary using? Logs disclose Pro/Demo/none.
journalctl -u stellarindex-indexer -n 1000 --no-pager | \
  grep -iE 'coingecko.*key|coingecko.*tier' | tail -5

# Manual probe with the SAME key the binary uses (replace KEY):
curl -sv "https://api.coingecko.com/api/v3/simple/price?ids=stellar&vs_currencies=usd&x_cg_demo_api_key=KEY" 2>&1 | head -20

# Check the cooldown state from logs (poller logs `cooldown armed for Xs` on each backoff).
journalctl -u stellarindex-indexer -n 500 --no-pager | \
  grep -iE 'coingecko.*cooldown|coingecko.*backoff' | tail -5
```

Common 429 causes ranked by likelihood on a healthy R1:

1. **No CG API key configured** — the public tier is the new
   default-deny. Provision a demo key (free signup at
   coingecko.com/api/pricing).
2. **Demo key cap exceeded by the verified-currency catalogue
   growth** — every catalogue entry adds one slug to the polled
   set. At 30 req/min the cap is reached around 25-28 verified
   currencies polled at the default 60s interval. Raise the
   interval OR upgrade to Pro.
3. **Multiple binaries (indexer + ops `verify-external`) using
   the same key against the same IP** — both count toward the
   per-IP cap. Prefer the indexer-only path during steady-state;
   only run the ops verifier on demand.

## Mitigation (≤ 15 min)

- [ ] Step 1 — if HTTP 429, slow down the poll cadence in `[external.<vendor>] poll_interval` (operator config in `/etc/stellarindex.toml`) and restart the indexer (`systemctl restart stellarindex-indexer`).
- [ ] Step 2 — if HTTP 401/403, rotate the API key env var via the secrets vault and restart the indexer.
- [ ] Step 3 — if vendor outage, no action needed; the aggregator's class-aware fallback (ADR-0008) keeps `/v1/price` serving from remaining sources. Update the status page only if `flags.reduced_redundancy=true` propagates to a customer-visible pair.
- [ ] Step 4 — if schema drift, the decoder needs a code update; jump to the source's dispatcher_adapter and update the parse path. Out-of-cycle release per `release-process.md`.
- [ ] Verification: error rate drops below 50% within 5 min of mitigation.

## Root cause analysis

For postmortem capture:
- Full poller log for the affected vendor over the previous 24 h.
- Vendor's status page screenshot at the time the alert fired.
- Diff of `internal/sources/external/<vendor>/parse.go` against the captured response if schema drift suspected.

## Known false-positive patterns

- **Short bursts during vendor maintenance windows** — most CEX vendors have published maintenance windows. Cross-reference the firing time against their status page before paging.
- **Network egress from R1 briefly degraded** — Hetzner has periodic single-AZ network blips that resolve within 2–5 min. The alert's `for: 5m` window captures this but a 6-min window can still squeak through.

## Related

- `external-poller-stale.md` — adjacent alert when a poller stops producing entirely.
- `aggregator-fx-snap-fallback-dominant.md` — fires when an FX vendor's failures push us to the snap fallback path.
- ADR-0008 — HA topology + reduced-redundancy flag semantics.
- CLAUDE.md "External sources" surprise list — vendor-specific schema quirks.

## Changelog

- 2026-06-12 — F-1330: fix metric name (`stellarindex_external_poller_polls_total`,
  not `_poller_total`); pollers run in `stellarindex-indexer` not the
  aggregator (log + restart targets corrected); config key is
  `[external.<vendor>] poll_interval`.
- 2026-05-12 — initial draft (audit-2026-05-12 F-1237 closure).
