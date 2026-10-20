---
title: Runbook — external pollers, CEX streams, FX feed
last_verified: 2026-10-06
status: draft
severity: P2 for `stellarindex_external_poller_stale`, `stellarindex_external_fx_feed_stale`, `stellarindex_cex_stream_subscription_rejected` and `stellarindex_cex_stream_entry_skips`; P3 for the rest
---

# Runbook — external pollers, CEX streams, FX feed

## At a glance

External pollers, CEX streams and FX feed alerts; each section below is one alert.

Rules: `configs/prometheus/rules.r1/external-pollers.yml` (r1) and `deploy/monitoring/rules/external-pollers.yml` (multi-host template); exprs, `for:` and severities match.

## Shared context

- `external.Connector` pollers (CoinGecko, CoinMarketCap, CryptoCompare, ECB, ExchangeRatesAPI, Tiingo) and CEX WebSocket streamers (Binance, Coinbase, Kraken, Bitstamp) run in `stellarindex-indexer`; metrics on `:9464`.
- `massive` (fiat FX, `internal/sources/external/forex` worker) runs in `stellarindex-api`, not the poller framework: no `stellarindex_external_poller_*` series; its alerts are the `fx_*` ones below.
- A lost poller drops the venue out of its pairs' consensus; `/v1/price` keeps serving from the remaining sources (possibly with `flags.single_source`).
- Logs (the second line shows `auth_mode=anonymous|demo|pro`):

```sh
ssh root@136.243.90.96 'journalctl -u stellarindex-indexer --since "1 hour ago" --no-pager | grep -E "poller error|poller stopping|produced no rows|no applicable pairs" | grep <source>'
journalctl -u stellarindex-indexer --no-pager | grep -F 'external poller enabled' | grep -F 'source=coingecko' | tail -1
```

- Verify a fix: `stellarindex_external_poller_polls_total{source="<source>",outcome="success"}` increments and `stellarindex_external_poller_last_success_unix` advances:

```sh
ssh root@136.243.90.96 "curl -s http://localhost:9464/metrics | grep -E 'stellarindex_external_poller_(polls|last_success).*<source>'"
```

- Keys live in `/etc/default/stellarindex` (templated from `configs/ansible/roles/archival-node/templates/stellarindex.env.j2`): `COINGECKO_API_KEY` (Pro, header `x-cg-pro-api-key`, wins), `COINGECKO_DEMO_API_KEY` (header `x-cg-demo-api-key`), `MASSIVE_API_KEY`, others per vendor. Codify a key change, never hand-edit: set the vault variable (for example `vault_coingecko_demo_api_key` in `inventory/r1.secrets.yml`) with the vault helper, then, from `configs/ansible/`:

- Codify a key change, never hand-edit (the next playbook run overwrites it):

```sh
# on the workstation, from configs/ansible/
ansible-vault edit inventory/r1.secrets.yml     # set vault_coingecko_demo_api_key
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
  --tags stellarindex --check --diff            # always --check --diff first
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
  --tags stellarindex
```

  The handler restarts the unit; by hand: `ssh root@136.243.90.96 'systemctl daemon-reload && systemctl restart stellarindex-indexer'` (API-side keys: `systemctl restart stellarindex-api`).

## stellarindex_external_poller_stale

`(time() - stellarindex_external_poller_last_success_unix{source!~"ecb|tiingo"}) > 1800`, `for: 5m`, P2. A poller has had no successful `PollOnce` for 30 min. `source` names the poller.

### Check

Run the shared log grep and decode the latest error:

| Error contains | Cause | Action |
|---|---|---|
| `http 429` | rate-limited | higher-tier key (CoinGecko below) |
| `http 401` / `http 403` | auth failure | rotate / re-issue the key |
| `http 5..` | venue outage | wait; check the venue status page |
| `http: timeout` | network slowness | check r1 public egress |
| `dial tcp: ... no route` | DNS / firewall | check r1 networking, ufw, DNS |
| `decode` / `unmarshal` | venue changed shape | patch the decoder |
| `produced no rows` | `outcome="empty"`: renamed slug or delisted pair | fix the id in the source's seed/config |
| `no applicable pairs` | `outcome="idle"`: no configured pair maps to the source | fix the pair list in the indexer TOML or remove the source |

### Likely causes

- Venue rejecting us (auth, rate limit) or down.
- Network path broken.
- Paid key expired (CMC / CryptoCompare at annual renewal).

### Fix

1. CoinGecko `http 429`: register a free demo key, set it via the codified flow above, confirm `auth_mode=demo` (or `pro`) on startup; see [CoinGecko 429 pattern](#coingecko-429-pattern).
2. Expired paid key: renew in the vendor dashboard, rerun the vault and playbook cycle.
3. Venue outage: wait; record in `docs/operations/incidents/` if over 1 h.
4. Verify per the shared section.

## stellarindex_external_poller_stale_ecb

`(time() - stellarindex_external_poller_last_success_unix{source="ecb"}) > 43200` (12 h), `for: 10m`, P3. ECB publishes once per EU business day and the poller interval is 6 h, so 12 h is two missed cycles. Causes: rates endpoint down, lost outbound HTTPS, or a deadlocked poller goroutine. Check and fix as [`stellarindex_external_poller_stale`](#stellarindex_external_poller_stale) with `<source>` = `ecb`.

## stellarindex_external_poller_stale_tiingo

`(time() - stellarindex_external_poller_last_success_unix{source="tiingo"}) > 7200` (2 h), `for: 10m`, P3. Poll interval is 1 h; one whole cycle was skipped. Causes: Tiingo API down or throttling, token expired, or lost outbound HTTPS. Check and fix as [`stellarindex_external_poller_stale`](#stellarindex_external_poller_stale) with `<source>` = `tiingo`.

## stellarindex_external_poller_stale_exchangeratesapi

`(time() - stellarindex_external_poller_last_success_unix{source="exchangeratesapi"}) > 7200` (2 h), `for: 10m`, P3. Poll interval is 1 h, the plan's refresh; the plan's 10,000 calls a month are shared with another service, so never poll faster. Causes: API down or throttling, access key expired, monthly quota exhausted, or lost outbound HTTPS. Check and fix as [`stellarindex_external_poller_stale`](#stellarindex_external_poller_stale) with `<source>` = `exchangeratesapi`.

## stellarindex_external_poller_error_rate_high

`for: 15m`, informational (escalation is `stellarindex_external_poller_stale`):

```promql
(
  rate(stellarindex_external_poller_polls_total{outcome="error"}[15m])
  /
  ignoring(outcome) (
    sum without (outcome) (
      rate(stellarindex_external_poller_polls_total{outcome=~"success|error"}[15m])
    )
  )
) > 0.5
```

The denominator is success + error only (`skipped` is excluded so a post-429 cooldown does not dilute it). Keep `sum without (outcome)` + `ignoring(outcome)`: a bare sum matches nothing and never fires. Exchange-class venues (`binance`, `kraken`, `bitstamp`, `coinbase`, `exchangeratesapi`) are `IncludeInVWAP: true`, so a loss degrades the aggregate for their pairs.

### Check

```sh
curl -s 'http://localhost:9090/api/v1/query?query=sum%20by%20(source)%20(rate(stellarindex_external_poller_polls_total%7Boutcome%3D%22error%22%7D%5B15m%5D))'
journalctl -u stellarindex-indexer -n 500 --no-pager | grep -iE 'poller error.*source=' | tail -20
curl -sv 'https://api.coingecko.com/api/v3/simple/price?ids=stellar&vs_currencies=usd' 2>&1 | head -20
```

`/v1/sources?include=stats` shows the vendor with a stale `last_event_unix`. 429 = rate limit; 401/403 = key rotated or revoked (CoinGecko 403 often means the public tier is denied).

### Likely causes

- Rate limit or tier (below), key revoked, vendor outage.
- Schema drift: parse path `internal/sources/external/<vendor>/parse.go` (streaming venues) or the poller (poller-only vendors).
- A source polled hourly has few samples per window, so one error can exceed 0.5 (check absolute counters).

### Fix

1. 429: raise `[external.<vendor>] poll_interval` in `/etc/stellarindex.toml`, then `systemctl restart stellarindex-indexer`.
2. 401/403: rotate the key in `/etc/default/stellarindex`, restart the owning binary, confirm `auth_mode=`.
3. Vendor outage: nothing to do.
4. Schema drift: code update ([release-process](../release-process.md)).
5. Verify the ratio falls under 0.5 (allow the 15 min window plus `for: 15m`).

### CoinGecko 429 pattern

The poller (`internal/sources/external/coingecko/poller.go`) backs off exponentially from 60 s to 1 h, honours `Retry-After`, and treats 403 like 429.

| Tier | Cap | Symptom | Action |
|---|---|---|---|
| Public (no auth) | ~5-15 req/min, IP-throttled | 403, no `Retry-After` | free demo key, set `COINGECKO_DEMO_API_KEY=` in `/etc/default/stellarindex`, restart the indexer |
| Demo (free) | 30 req/min, 10,000 calls/day | 429 | check the daily budget |
| Pro (paid) | 500+ req/min | 429 only past the cap | check status.coingecko.com; raise the poll interval until the next billing cycle |

Budget: the `coingecko` poller makes one batched `/simple/price` per 300 s (~288/day); the divergence CoinGecko reference makes ~288/day per binary. That is well inside 10,000/day, so a 429 means no key (`auth_mode=anonymous`), several binaries sharing one key and egress IP (an ad-hoc `stellarindex-ops verify-external` counts too), or vendor trouble.

```sh
curl -sv -H "x-cg-demo-api-key: KEY" \
  "https://api.coingecko.com/api/v3/simple/price?ids=stellar&vs_currencies=usd" 2>&1 | head -20
journalctl -u stellarindex-indexer -n 500 --no-pager | \
  grep -iE 'coingecko.*(throttled|backing off)' | tail -5
```

## stellarindex_external_fx_feed_stale

`(time() - max(stellarindex_external_fx_last_quote_unix)) > 21600` (6 h), `for: 15m`, P2. The hourly `massive` forex worker (API binary) stopped committing fresh rates. The gauge advances only on a committed `fx_quotes` write with a fresh upstream rate. No immediate impact: fiat-quoted pairs price off the last-good `fx_quotes` row (7-day lookback), then break silently.

### Check

```sh
ssh root@136.243.90.96 \
  'journalctl -u stellarindex-api --since "8 hours ago" --no-pager \
    | grep -E "forex: (fx_quotes persisted|rates fetch failed|names fetch failed|fx_quotes persist failed|primary failed|fallback failed)"'
ssh root@136.243.90.96 \
  'grep -c MASSIVE_API_KEY /etc/default/stellarindex || echo "KEY NOT SET"'
```

Healthy: one `fx_quotes persisted` line per hour (~803 rows/tick on r1).

| Log line | Stage | Likely cause |
|---|---|---|
| `forex: rates fetch failed` / `names fetch failed` | upstream HTTP | 429 / 401 / subscription lapse / 5xx |
| `forex: fx_quotes persist failed` | DB write | Postgres down, migration missing ([fx-history-missing](data-freshness.md#fx-history-missing)), disk full |
| no forex lines | worker not running | crashed / dry-run / not wired |

### Likely causes

- `massive` is a PAID feed: lapsed subscription or rotated key (`http 401/403/429`).
- Empty `MASSIVE_API_KEY` (every fetch 401s).
- DB write failure.

### Fix

1. Check the massive.com dashboard (subscription, quota).
2. Update `MASSIVE_API_KEY` in the r1 env file, `systemctl restart stellarindex-api`.
3. Verify: seconds after restart the log shows `forex: fx_quotes persisted` and the gauge re-stamps.
4. Standby: the worker falls back to ECB daily rates (`forex.ECBProvider`, always on), so `stellarindex_external_fx_last_quote_unix{source="ecb"}` keeps the alert clear while `massive` is down. `forex: primary failed - serving from fallback` means the standby serves; `forex: fallback failed` or `rates fetch failed on every source` means both are down and the 7-day lookback is your time budget. Both dry past 7 days needs a code change to `FXQuoteAtOrBefore`: escalate.

Capture for a postmortem: `SELECT ticker, MAX(bucket) FROM fx_quotes GROUP BY ticker`.

## stellarindex_external_fx_feed_absent

`absent(stellarindex_external_fx_last_quote_unix)`, `for: 30m`, ticket. The worker refreshes on startup, so absence means it never succeeded since API start.

### Check

- API binary is up and NOT in dry-run mode (dry-run exits before the forex goroutine spawns).
- `MASSIVE_API_KEY` set and valid (see [fx_feed_stale](#stellarindex_external_fx_feed_stale) checks).
- The `fx_quotes` hypertable migration is applied ([fx-history-missing](data-freshness.md#fx-history-missing)).

### Fix

Fix the missing piece, restart `stellarindex-api`, and verify the gauge appears.

## stellarindex_external_fx_rate_rejections

`sum by (reason) (increase(stellarindex_external_fx_rate_rejected_total{reason!~"history_deviation_stuck|deviation_history_conflict_stuck"}[3h])) > 2`, `for: 30m`, P3. `worker.go` gates every current rate through a 50% sanity band (`maxRateDeviation`) before writing `fx_quotes`; refused tickers are HELD on their last accepted row (data was withheld, not wrong).

`reason`: `deviation` (moved over 50%, unconfirmed by a second fetch); `non_positive` (rate <= 0); `non_finite`; `history_deviation` (trailing-7d bar off by over 50%); `history_deviation_stuck` (12 consecutive refusals); `deviation_history_conflict` (two-fetch confirmation vetoed by the 7-day history majority); `deviation_history_conflict_stuck`. The `_stuck` variants are excluded from the alert and WARN-logged.

### Check

1. Which ticker (no ticker label): `journalctl -u stellarindex-api | grep 'forex: rejected upstream rate'` (fields `ticker`, `rate`, `previous`, `reason`, `band`).
2. Is the move real? Compare with ECB or any public quote. A clean power of ten between `rate` and `previous` is an upstream decimal shift.
3. Many tickers with `non_positive` / `non_finite`: upstream shape change; see [fx_feed_stale](#stellarindex_external_fx_feed_stale).

### Likely causes

- Real move (redenomination, managed-float break): the two-strike arm accepts after two agreeing fetches.
- Upstream decimal or unit-scale shift: the guard did its job, nothing corrupted.
- Known upstream defects: ETB history bar = 44 (real ~155-165/USD); UZS current bar ~1820 (true ~11,800).

### Fix

1. Real move: nothing in code; if it keeps firing the upstream oscillates between scales: report it and consider pinning the currency out of the fiat-quote surface.
2. Decimal shift: open an upstream ticket.
3. A bad bar reached `fx_quotes` before the band existed: correct with an upsert for the `(ticker, bucket)`, then re-derive fiat-quoted aggregates over the window.
4. Do NOT widen the band or disable the guard; changing `maxRateDeviation` is a code change with a test.

## stellarindex_fx_fixings_refresh_stale

`(time() - max(stellarindex_fx_fixings_last_refresh_unix)) > 10800` (3 h), `for: 15m`, ticket. The `fx_fixings` appender (end of every forex refresh) has not completed a cycle; closed fiat-cross buckets convert at the newest `fx_fixings` bar. Check the forex worker first ([fx_feed_stale](#stellarindex_external_fx_feed_stale)).

## stellarindex_fx_fixings_series_stale

`(time() - max(stellarindex_fx_fixings_newest_bar_end_unix)) > 14400 and on() ((day_of_week() >= 2 and day_of_week() <= 5) or (day_of_week() == 1 and hour() >= 2))`, `for: 15m`, ticket. Cycles complete but the newest stored bar is over 4 h old on a trading day.

### Check

- API logs for `fx_fixings bar fetch failed` (vendor error per ticker) and `fx_fixings insert failed` (DB); counters `stellarindex_fx_fixings_fetch_errors_total`, `stellarindex_fx_fixings_write_errors_total`.
- Newest bar per ticker:

```sql
SELECT ticker, MAX(bar_end) FROM fx_fixings GROUP BY ticker ORDER BY 2;
```

### Fix

Refill a gap once the vendor is back (idempotent; the live appender re-offers only the last 48 h):

```sh
go run ./scripts/ops/fx-history-backfill --series=fixings --from=YYYY-MM-DD --ticker=EUR,GBP
```

## stellarindex_fx_fixings_quote_disagreement

`max by (ticker) (stellarindex_fx_fixings_quote_disagrees) == 1`, `for: 2h`, ticket. An accepted hourly fixing is outside [0.5, 1.5] of the guarded daily rate for its day: the two series disagree about one currency.

### Check

Grep API logs for `fx_fixings bar disagrees` (ticker, `bar_start`, close, guarded rate). Decide which series is wrong against an independent source (ECB, the OXR corroborator gauge).

### Fix

1. Wrong daily rate: see [rate_rejections](#stellarindex_external_fx_rate_rejections).
2. Wrong hourly bar: INSERT the right close for the same `(ticker, grain, bar_start)` at generation `max + 1` (readers take the highest). Never `UPDATE` or `DELETE` an `fx_fixings` row: it is the record of what every served closed cross converted at.

## stellarindex_cex_stream_subscription_rejected

`max by (source, symbol) (stellarindex_cex_stream_subscription_rejected) == 1`, `for: 5m`, P2. The venue answered our subscribe for `symbol` with `success:false`: that pair delivers no trades while the socket looks healthy.

### Check

Indexer log `kraken rejected the trade subscription` (or `bitstamp ...`); `venue_error` holds the venue text (e.g. `Currency pair not supported XLM/GBP`).

### Fix

Update `DefaultPairList` in `internal/sources/external/kraken/pairs.go` (bitstamp: `bitstamp/pairs.go`) and deploy. The gauge clears when the venue next acknowledges the symbol; a symbol removed from config keeps its last value until the indexer restarts.

## stellarindex_cex_stream_entry_skips

`sum by (source, reason) (rate(stellarindex_cex_stream_entry_skips_total[15m])) > 0`, `for: 30m`, P2. Trade entries inside well-formed frames are being dropped continuously (kraken streamer). Log: `kraken trade entry skipped` (once per reason per minute; `err` names the symbol or value).

### Likely causes

- `unknown_symbol`: the venue renamed a configured pair; update `internal/sources/external/kraken/pairs.go::DefaultPairList`.
- `bad_qty` / `bad_price` / `bad_timestamp`: the venue changed a field encoding; compare the logged value with `parse.go::buildTrade` and fix with a test pinned to the new wire shape.

Whole-frame failures are a different signal: [decode-errors](ingestion.md#stellarindex_ingestion_decode_error).

## stellarindex_cex_stream_stalled

`sum by (source) (increase(stellarindex_cex_stream_disconnect_total{reason="stall"}[15m])) > 0`, `for: 5m`, P3. `wsclient.ErrStreamStalled`: the venue stopped answering pings but TCP has not noticed (half-open socket), a silent gap in trade coverage.

### Check

```sh
curl -fs http://localhost:9464/metrics | grep -E '^stellarindex_(cex_stream_disconnect_total|external_dust_dropped_total)'
journalctl -u stellarindex-indexer | grep -E 'stream stalled' | tail -20
```

### Fix

The reconnect loop (`internal/sources/external/wsclient/`) already forces a fresh dial. If the rate stays high, suspect venue-side infra (a stuck load-balancer node); restarting the indexer forces a fresh outbound connection.

## stellarindex_external_dust_dropped_high

Dust share (`dust_dropped / (dust_dropped + trade_inserts)`, 15 min rate, per source) > 0.5 with dust above 0.05/s, `for: 30m`, P3. Most of a venue's streamed trades are being dropped as sub-$0.001 dust (floor per quote asset, `forwardTrade` in `internal/sources/external/runner.go`). A volume burst raises the dust rate but not its share, so it does not fire. Check with the metrics curl in [cex_stream_stalled](#stellarindex_cex_stream_stalled); compare with the venue's own trade feed.

- The venue changed a symbol's price/amount scaling: the guard keys off the quote/base ratio, so a decimals regression reads as "everything is dust". Never assume scaling is uniform (`Decimals` per source). Fix the scaling and verify the share returns to baseline (a few percent).
- A new sub-cent listing genuinely dominates the venue's fills: the guard is dropping real trades. Fix the floor for that quote asset in `forwardTrade`; do not silence the alert.

## Related

- [Alerts catalogue](../alerts-catalog.md)
