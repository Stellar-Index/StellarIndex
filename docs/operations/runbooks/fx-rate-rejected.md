---
title: Runbook — fx-rate-rejected
last_verified: 2026-07-26
status: draft
severity: P3
---

# Runbook — `stellarindex_external_fx_rate_rejections`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_external_fx_rate_rejections` (P3 / ticket) |
| Detected by | Prometheus rule in `deploy/monitoring/rules/external-pollers.yml` and the R1 single-host overlay `configs/prometheus/rules.r1/external-pollers.yml`. |
| Typical MTTR | 15–60 min: identify the ticker from the API logs, decide whether the upstream move is real, then either accept it or fix the feed. |
| Impact | One or more fiat tickers are **held on their last accepted `fx_quotes` row**. Fiat-quoted `usd_volume` for those currencies is being converted at a stale rate. No wrong number is published — the guard's whole purpose is that it refuses to publish one — but the held rate ages. |

## What this fires on

`internal/sources/external/forex/worker.go` gates every "current" rate
through a sanity band (`maxRateDeviation`, 50%) before writing it to
`fx_quotes`, and counts each refusal on
`stellarindex_external_fx_rate_rejected_total{source,reason}` (C2-030,
audit-2026-07-23).

The band exists because `fx_quotes` is the denominator of every
fiat-quoted `usd_volume` the X2.5 triangulation derives: one bad upstream
bar — a decimal shift, a unit-scale change, a redenomination applied
upstream without a ticker change — would silently re-scale that
currency's entire conversion history. Before the band, `persistSnapshot`
wrote whatever the upstream said.

The guard is **two-strike**: an out-of-band rate is held on first
sighting, and accepted if the NEXT fetch agrees with it. So a genuine
devaluation (EGP fell ~38% in a day in Mar 2024; NGN ~40% in Jun 2023)
costs one refresh interval of lag, not a permanent wedge.

That is why the alert threshold is `> 2 rejections in 3h` sustained for
30 min rather than `> 0`: the worker refreshes hourly, so a single bad
bar plus its confirmation cannot trip it. A firing alert means the
upstream keeps disagreeing and the confirmation arm is **not** clearing
it.

Reasons (the `reason` label):

- **`deviation`** — moved more than 50% from the last accepted rate and
  a second fetch has not confirmed it.
- **`non_positive`** — the rate came back `<= 0` (an empty upstream
  field). `1/rate` feeds `InverseUSD`, so this would poison the row in
  both directions.
- **`non_finite`** — NaN or ±Inf (a parse that produced garbage).

## Quick diagnosis (≤ 5 min)

1. **Which ticker(s)?** The metric is deliberately not labelled by
   ticker (~150 currencies would be pure cardinality). The ticker is on
   the WARN log line:

   ```
   journalctl -u stellarindex-api | grep 'forex: rejected upstream rate'
   ```

   Each line carries `ticker`, `rate` (the refused value), `previous`
   (the last accepted one), `reason`, and `band`.

2. **Is the move real?** Compare `rate` against an independent source
   (ECB reference rates, or any public FX quote for that pair). A
   redenomination or a managed-float break is real; a clean power of ten
   between `rate` and `previous` is a decimal shift upstream.

3. **Is it the whole feed or one ticker?** `reason="non_positive"` /
   `"non_finite"` across many tickers means the upstream response shape
   changed — check `stellarindex_external_fx_feed_stale` too, and the
   `massive` subscription state (see [fx-feed-stale](fx-feed-stale.md)).

## Remediation

- **The move is REAL and the upstream keeps reporting it** → nothing to
  do in code: the two-strike arm accepts it as soon as two consecutive
  fetches agree, and the alert clears. If it is firing anyway, the
  upstream is oscillating between the old and new scale — that is a feed
  bug; report it and consider temporarily pinning the currency out of
  the fiat-quote surface.
- **The move is a decimal shift / unit-scale change upstream** → the
  guard did its job; no `fx_quotes` row was corrupted. Open an upstream
  ticket. Nothing to backfill.
- **A bad bar reached `fx_quotes` BEFORE the band shipped** → the row is
  per `(ticker, bucket)`; correct it with an
  `InsertFXQuoteBatch`-equivalent upsert for the affected day, then
  re-derive any fiat-quoted aggregates over that window.
- **The rejection is wrong and the band is too tight** → `maxRateDeviation`
  is a documented constant with its rationale in the code. Changing it is
  a code change with a test, not a runtime toggle.

## Do NOT

- **Do not widen the band to silence the alert.** The band is what keeps
  a mis-scaled bar out of the denominator of every fiat-quoted volume.
  Silencing it re-opens exactly the defect it closed.
- **Do not disable the guard to "let the real rate through".** The
  confirmation arm already does that, on the next refresh, without
  losing the protection.
- Do not assume a firing alert means data is wrong. It means data was
  **withheld**. The stale-rate exposure is the cost; a wrong rate would
  have been worse.

## Related

- [fx-feed-stale](fx-feed-stale.md) — the FX feed has stopped writing
  entirely (the absence case, rather than the refusal case).
- [external-poller-error-rate-high](external-poller-error-rate-high.md) —
  upstream is erroring rather than returning bad values.
