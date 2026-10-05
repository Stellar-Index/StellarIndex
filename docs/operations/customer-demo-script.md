---
title: API walkthrough script
last_verified: 2026-05-04
status: operator runbook
---

# API walkthrough script

A 30-minute walk through every surface a new user needs, with `curl` commands
they can re-run. They should leave able to make a first real request unaided.

## Pre-flight (T-30 min)

- [ ] **Demo URL pinned:** `https://api.stellarindex.io/v1` (post-cutover) or
      `https://staging.stellarindex.io/v1` (pre-cutover dry-run).
- [ ] **Demo API key minted:** tier-`apikey`, reasonable rpm; note it for cleanup.
- [ ] **Tabs open:** explorer (`https://stellarindex.io`), API reference,
      getting-started, status page.
- [ ] **Terminal env:**
      ```sh
      export BASE='https://api.stellarindex.io/v1'
      export KEY='ak_demo_...'
      ```
- [ ] **Wireshark / network inspector NOT open.**

## The walk-through

About 5 min per stage unless noted; total 25-30 min.

### Stage 1 — "Is this thing on?" (2 min)

```sh
curl -s "$BASE/healthz" | jq .
curl -s "$BASE/version" | jq .
```

- Both are cheap, unauthenticated, monitoring-suitable; `version` shows the
  CalVer tag of the responding build.
- **Hand the user `https://stellarindex.io`**: each panel's `<>` button shows
  the API call behind it, so they can follow the walk-through. They leave with
  that tab pinned.

### Stage 2 — Closed-bucket pricing (5 min)

```sh
curl -sH "Authorization: Bearer $KEY" \
  "$BASE/price?base=native&quote=fiat:USD" | jq .
```

- **Closed-bucket:** byte-identical across r1/r2/r3 and constant within a
  minute. Re-run the request; the response is identical.
- **`flags`:** `stale`, `divergence_warning`, `frozen`, etc., each documented.
- **`confidence`** + **`confidence_factors`** (ADR-0019): 0..1 score with the
  per-factor decomposition on the wire.
- **`sources`:** contributing venues; names link to `/v1/sources`.

### Stage 3 — Tip pricing + consistency vs freshness (3 min)

```sh
curl -sH "Authorization: Bearer $KEY" \
  "$BASE/price/tip?asset=native&quote=fiat:USD" | jq .
```

- Tip is **rolling-window**, not closed-bucket: a different consistency
  contract (ADR-0018). Use tip for UI freshness, `/v1/price` for execution and
  reporting. The URLs are deliberately distinct; no query param flips between them.
- **Parameter name:** `/v1/price/tip` and `/v1/price/stream` take `asset=`
  (quote defaults to `fiat:USD`); `/v1/price`, `/v1/observations` and
  `/v1/history/*` take `base=`. `base=` on tip or stream is a **400
  `missing-asset`**; don't find that out in front of a customer.

### Stage 4 — Per-source observations (3 min)

```sh
curl -sH "Authorization: Bearer $KEY" \
  "$BASE/observations?base=native&quote=fiat:USD" | jq '.data[:3]'
```

- Raw aggregator inputs, for consumers applying their own policy.
- `?source=binance` or `?source=sdex` filters to one venue (exchange or
  on-chain source); a data vendor such as `?source=coingecko` returns 400
  (its rows are served only beside other sources). `?aggregate=latest`
  collapses to one row per source.

### Stage 5 — Historical data (3 min)

```sh
curl -sH "Authorization: Bearer $KEY" \
  "$BASE/history/since-inception?asset=native&quote=fiat:USD&granularity=1d" \
  | jq '.data[:3]'
```

- Since-inception coverage; Galexie replays from ledger 2.
- Granularities 1m / 15m / 1h / 4h / 1d / 1w / 1mo (the CAGGs the closed-bucket
  path uses).
- CDN caches aggressively (s-maxage=86400): sub-10ms p99 once the edge is warm.

### Stage 6 — SSE streaming (3 min)

```sh
curl -NH "Authorization: Bearer $KEY" \
  "$BASE/price/stream?asset=native&quote=fiat:USD"
```

Run 60 seconds; one `data: {...}` line per closed bucket.
- Last-Event-ID resumption: reconnect with the last ID and the server replays
  missed buckets from the Hub's ring buffer.
- A 15s heartbeat comment line keeps proxies happy.

### Stage 7 — Asset detail (3 min)

```sh
curl -sH "Authorization: Bearer $KEY" \
  "$BASE/assets/native" | jq .
```

- F2 fields: `total_supply`, `circulating_supply`, `max_supply`,
  `market_cap_usd`, `fdv_usd`, `volume_24h_usd`, `supply_basis`,
  `change_24h_pct`.
- Supply per ADR-0011: XLM / classic / SEP-41 algorithms all wired.

### Stage 8 — SDK demo (4 min)

Open `pkg/client/example_test.go` (or getting-started).
- Generic `Envelope[T]`: type-safe at the call site.
- `pkg/client` is SemVer-pinned; v0.x policy in
  `docs/architecture/semver-policy.md`.

### Stage 9 — Q&A (5 min)

- **"What if Reflector goes down?"** Diversity factor lowers confidence;
  operators alert on confidence < 0.5 sustained; multi-source mitigates a
  single-oracle outage.
- **"How do you handle USDC depegs?"** Stablecoin classifier, per-class anomaly
  thresholds, freeze policy (ADR-0019). Demo `flags.frozen` when a freeze fires.
- **"What's the SLA?"** Show `/sla` and the SLA-probe results dashboard.
- **"Can you commit to 99.99%?"** The published commitment is ≥ 99.9 %
  (`/sla`, #487). 99.99 % is the design target of the multi-region topology
  (ADR-0050), not offered until that ships and an off-host probe has measured it
  for ≥ 30 days.
- **"Private deployment?"** Apache-2.0 source, ansible roles and bringup runbook
  are public: `docs/operations/archival-node-bringup.md`.

## Post-demo

- [ ] Share the recording if recorded.
- [ ] Send `onboarding-email.md` with the user's production key; rotate the demo
      key out the same day.
- [ ] Open a feedback issue for any confusing surface or feature request.

## Cross-references

- [`docs/getting-started.md`](../getting-started.md): the written walkthrough.
- [`deploy/comms/onboarding-email.md`](../../deploy/comms/onboarding-email.md): post-demo email.
- [`launch-day-checklist.md`](launch-day-checklist.md) §T-3: schedule the demo.
