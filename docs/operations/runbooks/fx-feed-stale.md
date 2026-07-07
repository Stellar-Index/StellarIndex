---
title: Runbook — fx-feed-stale
last_verified: 2026-07-07
status: draft
severity: P2
---

# Runbook — `stellarindex_external_fx_feed_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_external_fx_feed_stale` (and companion `stellarindex_external_fx_feed_absent`) |
| Severity | P2 (ticket) |
| Detected by | Prometheus rule in `deploy/monitoring/rules/external-pollers.yml` (and `configs/prometheus/rules.r1/external-pollers.yml` R1 overlay) |
| Typical MTTR | 5–30 min for a subscription/key issue; vendor outages can run hours |
| Impact | No customer impact *yet* — every fiat-quoted pair (XLM/EUR, XLM/GBP, …) is still priced off the last-good `fx_quotes` row via the 7-day forex-snap lookback. Left unfixed, fiat pairs silently break once that 7-day window expires. This alert exists to fix the feed *before* the cliff. |

## Why this exists

The active fiat-FX feed is `massive` (massive.com = Polygon's FX
backend), running as the `internal/sources/forex` worker in the **API**
binary. It fetches USD rates hourly and writes them to the `fx_quotes`
hypertable. The X2.5 triangulation forex-snap
(`Store.FXQuoteAtOrBefore`) reads `fx_quotes` with a **7-day lookback**
to price every fiat-quoted pair.

Two properties combined to make a dead feed invisible:

1. `massive` does **not** run under the `external.Connector` poller
   framework, so it emits **no** `stellarindex_external_poller_*`
   series — `stellarindex_external_poller_stale` can never fire for it.
2. The forex-snap's 7-day lookback keeps pricing fiat pairs off a stale
   row, so nothing downstream degrades for up to a week.

On 2026-07-07 the feed was found silent for ~4h (last `fx_quote`
19:17 UTC, zero poller logs) with **no alert**. The
`stellarindex_external_fx_last_quote_unix{source}` gauge + these alerts
close that gap: the gauge advances only on a committed non-empty
`fx_quotes` write, so its staleness is a true liveness signal for the
feed, fireable long before the 7-day cliff.

## Symptoms

- `stellarindex_external_fx_feed_stale`: the freshest
  `stellarindex_external_fx_last_quote_unix` across all FX sources is
  > 6 h old for > 15 min. The hourly forex worker has stopped
  committing rows.
- `stellarindex_external_fx_feed_absent`: the gauge has **no series**
  for 30 min — the worker has never once written since API startup
  (dead worker, unset/invalid `MASSIVE_API_KEY`, or a persist path
  wedged from the first tick).

## Quick diagnosis (≤ 5 min)

1. **Check the forex worker log on r1** (the feed runs in the API
   binary, not the indexer):

   ```sh
   ssh root@136.243.90.96 \
     'journalctl -u stellarindex-api --since "8 hours ago" --no-pager \
       | grep -E "forex: (fx_quotes persisted|rates fetch failed|names fetch failed|fx_quotes persist failed)"'
   ```

   Healthy steady state is one `fx_quotes persisted` line per hour
   (r1 baseline: ~803 rows/tick, dead-regular hourly).

2. **Decode which stage broke:**

   | Log line                         | Stage                    | Likely cause                          |
   |----------------------------------|--------------------------|---------------------------------------|
   | `forex: rates fetch failed`      | upstream HTTP fetch      | 429 / 401 / subscription lapse / 5xx  |
   | `forex: names fetch failed`      | upstream HTTP fetch      | same as above (names endpoint)        |
   | `forex: fx_quotes persist failed`| DB write                 | Postgres down, `fx_quotes` migration missing, disk full |
   | *no forex lines at all*          | worker not running       | binary crashed / dry-run mode / not wired |

3. **Confirm the upstream credential is set** — the worker reads
   `MASSIVE_API_KEY` from the systemd `EnvironmentFile`. When empty,
   every fetch 401s and no row is ever written (→ the `absent` alert):

   ```sh
   ssh root@136.243.90.96 \
     'grep -c MASSIVE_API_KEY /etc/default/stellarindex || echo "KEY NOT SET"'
   ```

## Mitigation (≤ 15 min)

### massive.com subscription lapse / 429 (most likely — paid feed)

`massive` is a **paid** feed. The failure profile mirrors the
CoinGecko-tier class: a 429 or a lapsed subscription surfaces as
repeated `forex: rates fetch failed` with an `http 401/403/429` error
string.

- [ ] Check the massive.com dashboard for subscription status / quota.
- [ ] If the key was rotated / lapsed, update `MASSIVE_API_KEY` in the
      r1 env file, then `systemctl restart stellarindex-api`.
- [ ] Verification: within seconds of restart the log shows
      `forex: fx_quotes persisted` and the gauge re-stamps — the alert
      clears at the next evaluation.

### massive stays dry — re-enable the ECB / Frankfurter fallback

If `massive` cannot be restored quickly, the connector-path FX
fallbacks can serve the forex-snap in the interim. Per the note in
`internal/sources/external/registry.go`, `polygon-forex` /
`exchangeratesapi` / `ecb` are the same-role sources (currently
disabled). The forex-snap reads `fx_quotes`-first and falls back to
`trades` filtered by `FXSources()`, so re-enabling a fallback keeps
fiat pairs priced while `massive` is down.

- [ ] Enable a fallback via its `cfg.<venue>.enabled` gate and redeploy
      the indexer (these run under the dispatcher-parallel external
      path, not the API's forex worker).
- [ ] Note: `ecb` is daily-grain sovereign FX (fallback quality, not
      primary) — acceptable as a stopgap, not a long-term substitute.

### Worker not running at all (`absent` alert)

- [ ] Confirm the API binary is up and NOT in dry-run mode (dry-run
      exits before the forex goroutine spawns — see
      `cmd/stellarindex-api/main.go`).
- [ ] Confirm the `fx_quotes` hypertable migration is applied — a
      missing table makes every persist fail (see companion runbook
      [`fx-history-missing.md`](fx-history-missing.md)).

## Root cause analysis

Capture for the postmortem: the forex-worker log window spanning the
last good write to the first failure, the massive.com dashboard state
(quota / subscription / billing), and the `fx_quotes` freshest-bucket
per ticker (`SELECT ticker, MAX(bucket) FROM fx_quotes GROUP BY ticker`).

## Known false-positive patterns

- **API restart during the alert window.** A restart re-stamps the
  gauge within seconds (the worker refreshes on startup), so a brief
  restart does not trip the 6h/15m threshold. If the alert fires right
  after a deploy, check the worker actually completed its first refresh.
- **Sustained upstream fetch failures under 6h.** The feed legitimately
  skips a write on a transient upstream blip; 6h (6 missed hourly
  cycles) is the tolerance band. Shorter gaps are expected and do not
  fire.

## Related

- [`external-poller-stale.md`](external-poller-stale.md) — sibling
  liveness alert for the `external.Connector` poller fleet (CoinGecko,
  Binance, ECB, …). `massive` is deliberately NOT covered there (it runs
  the forex worker, not a poller), which is exactly the gap this runbook's
  alert fills.
- [`fx-history-missing.md`](fx-history-missing.md) — adjacent forex-side
  failure: the upstream fetch succeeds but the DB persist fails every
  tick (missing `fx_quotes` migration). Different signal (a
  `forex: fx_quotes persist failed` log), same feed.
- Implementation: `internal/sources/forex/worker.go`
  (`persistSnapshot` — the gauge stamp point), the metric in
  `internal/obs/metrics.go` (`ExternalFXLastQuoteUnix`), and the
  forex-snap read path in `internal/storage/timescale/fx_quotes.go`
  (`fxQuotesSnapLookback` = the 7-day cliff this alert beats).

## Changelog

- 2026-07-07 — initial draft. Closes the missing-staleness-alert gap for
  the active fiat-FX feed found silent ~4h with no page.
