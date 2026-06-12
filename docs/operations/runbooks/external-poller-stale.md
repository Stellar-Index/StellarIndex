---
title: Runbook — external-poller-stale
last_verified: 2026-05-13
status: draft
severity: P3
---

# Runbook — `stellarindex_external_poller_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_external_poller_stale` |
| Severity | P3 (ticket) |
| Detected by | Prometheus rule in `deploy/monitoring/rules/external-pollers.yml` (and `configs/prometheus/rules.r1/external-pollers.yml` R1 overlay) |
| Typical MTTR | 5–30 min for a config/key issue; vendor outages can run hours |
| Impact | Divergence-detection blind spot for the affected source; aggregated-source sanity reads degraded. The aggregator's class-aware fallback (ADR-0008) keeps `/v1/price` serving from remaining sources, but `flags.reduced_redundancy=true` may surface to customers on affected pairs. |

## Symptoms

The named external poller (CoinGecko, CoinMarketCap, CryptoCompare,
ECB, ExchangeRatesAPI, PolygonForex, Binance, Coinbase, Kraken,
Bitstamp) has not produced a single successful `PollOnce` in the
last 30 minutes. Either the venue is rejecting our calls (auth /
rate-limit), the venue is down, or the network path is broken.

Companion alert `stellarindex_external_poller_error_rate_high` fires
at the *informational* tier when error rate > 50% sustained 15 min
— a softer "something's degrading" signal that doesn't yet block
data flow. See
[`external-poller-error-rate-high.md`](external-poller-error-rate-high.md)
for the per-vendor 429 triage matrix; CoinGecko in particular has
specific pricing-tier behaviour worth checking BEFORE rotating
keys.

## Quick diagnosis (≤ 5 min)

1. **Identify the source.** The alert label `{{ $labels.source }}`
   names which poller (e.g. `coingecko`, `binance`).

2. **Check the indexer log on r1** (or the active region):

   ```sh
   ssh root@136.243.90.96 \
     'journalctl -u stellarindex-indexer --since "1 hour ago" \
       --no-pager | grep -E "poller error|poller stopping" | grep <source>'
   ```

3. **Decode the most recent error string.** Common patterns:

   | Error contains              | Cause                            | Action                              |
   |-----------------------------|----------------------------------|-------------------------------------|
   | `http 429`                  | rate-limited                     | provision a higher-tier API key     |
   | `http 401` / `http 403`     | auth failure                     | rotate / re-issue the API key       |
   | `http 5..`                  | venue outage                     | wait + verify upstream status page  |
   | `http: timeout`             | network slowness                 | check r1 → public network egress    |
   | `dial tcp: ... no route`    | DNS / IP-allowlist / firewall    | check r1 networking + ufw + DNS     |
   | `decode` / `unmarshal`      | venue API changed shape          | bug — patch the decoder, file PR    |

## Mitigation

### CoinGecko throttled (post-2024 unauthenticated-tier tightening)

Symptom: error contains `http 429` repeated every minute.

Fix: register a free demo API key at
[coingecko.com/en/developers/dashboard](https://www.coingecko.com/en/developers/dashboard),
add `COINGECKO_DEMO_API_KEY=<key>` to the indexer's environment file
(usually `/etc/systemd/system/stellarindex-indexer.service.d/env.conf`
or `/etc/stellarindex.toml.env` depending on the systemd unit's
`EnvironmentFile=`), then:

```sh
systemctl daemon-reload
systemctl restart stellarindex-indexer
```

Verify on next startup the indexer log shows
`source=coingecko ... auth_mode=demo` (or `pro` if a paid key).

### Paid-tier API key expired

Symptom: error contains `http 401` or `http 403` *with* a key set in
env. Common for CMC / CryptoCompare on annual renewal.

Fix: renew/rotate the key in the venue's dashboard, update the env
file, restart the indexer.

### Venue outage

Symptom: error contains `http 5..` or `connection refused`.

Action: check the venue's status page. If confirmed down, just
wait — the alert will clear once the venue recovers. Capture the
incident in `docs/operations/incidents/` if outage > 1h.

## Verification (post-fix)

```sh
curl -s http://r1:9100/metrics \
  | grep -E 'stellarindex_external_poller_(polls|last_success).*<source>'
```

You should see:
- `stellarindex_external_poller_polls_total{source="<source>",outcome="success"}` incrementing
- `stellarindex_external_poller_last_success_unix{source="<source>"}` reflecting the recent poll

## Related

- [`external-poller-error-rate-high.md`](external-poller-error-rate-high.md)
  — softer companion alert (informational tier) when error rate
  > 50% but the source is still emitting some successes. Per-
  vendor 429 triage matrix lives in this runbook; consult it
  BEFORE rotating CoinGecko keys (the post-2024 pricing-tier
  shape often masquerades as a credential problem).
- [`fx-history-missing.md`](fx-history-missing.md) — adjacent
  forex-side gap: when a deployment is missing the `fx_quotes`
  hypertable migration, the `forex` poller's `success` outcomes
  stay healthy on the metric (the upstream HTTP fetch succeeds)
  but the persist-to-DB step fails on every tick. Different
  signal — an INFO-level `forex: fx_quotes persist failed` log
  rather than a missed poll — but same broad family.

## Why this exists

Pre-2026-05-09 the only signal of a sustained-failing poller was a
WARN log per failed poll. A poller in steady-state failure (e.g.
CoinGecko 429s every 60s for 13 hours) was effectively invisible to
Prometheus — discovery required someone manually `journalctl`-ing
the indexer. The metric + alert close that gap.

See the launch incident report and PR #1139 (CoinGecko backoff +
demo-key support) and PR #1140 (this metric + alert) for the
discovery-path narrative.
