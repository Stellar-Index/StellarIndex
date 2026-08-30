---
title: Runbook — external-poller-stale
last_verified: 2026-08-29
status: draft
severity: P2 for `_stale`; P3 for `_stale_ecb` + `_error_rate_high`
---

# Runbook — the `stellarindex_external_poller_*` family

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts (three route here) | `stellarindex_external_poller_stale` (> 1800 s, `{source!="ecb"}`, `for: 5m`, `severity: ticket`)<br>`stellarindex_external_poller_stale_ecb` (> 43200 s = 12 h, `for: 10m`, `severity: informational`)<br>`stellarindex_external_poller_error_rate_high` (> 50 % errors, `for: 15m`, `severity: informational`) |
| Severity | P2 for `_stale`; P3 for `_stale_ecb` + `_error_rate_high` |
| Detected by | `configs/prometheus/rules.r1/external-pollers.yml` (the overlay r1 actually loads); multi-host template: `deploy/monitoring/rules/external-pollers.yml`. Both trees carry the same exprs. |
| Typical MTTR | 5–30 min for a config/key issue; vendor outages can run hours |
| Impact | The affected venue drops out of its pairs' consensus. Whether that moves a price at all depends on the source's `Class` / `IncludeInVWAP` row in `internal/sources/external/registry.go` (see [`aggregation-plan.md`](../../architecture/aggregation-plan.md)) — oracle- and lending-class sources never contribute to VWAP. `/v1/price` keeps serving from the remaining sources; a thinner consensus surfaces as `flags.single_source` and, on affected pairs, elevated `flags.divergence_warning`. It is **not** `flags.reduced_redundancy` — that flag is the ADR-0017 cross-region completeness signal R2/R3 set, unrelated to pollers. Note CoinGecko has TWO independent code paths — the ingest poller (this alert) and `divergence.CoinGeckoReference`, which does its own HTTP — so a stale poller does not by itself blind the cross-reference layer. |

## Symptoms

The named external poller (CoinGecko, CoinMarketCap, CryptoCompare,
ECB, ExchangeRatesAPI, PolygonForex, Binance, Coinbase, Kraken,
Bitstamp) has stopped producing successful `PollOnce` calls. Either
the venue is rejecting our calls (auth / rate-limit), the venue is
down, or the network path is broken.

**Read the alert name before you read a threshold — the staleness
budget is NOT 30 minutes for every source:**

| Alert | Matcher | Stale after | `for:` | Severity |
| ----- | ------- | ----------- | ------ | -------- |
| `stellarindex_external_poller_stale` | `{source!="ecb"}` | 1800 s (30 min) | 5 m | ticket (P2) |
| `stellarindex_external_poller_stale_ecb` | `{source="ecb"}` | 43200 s (**12 h**) | 10 m | informational (P3) |

ECB is split out because it publishes once per EU business day and
the poller's own interval is 6 h
(`internal/sources/external/ecb/poller.go::DefaultPollInterval`), so
the 30-minute rule would have fired after every *successful* ECB
poll. 12 h is two missed cycles. Treating an ECB page as "30 minutes
stale" misreads it by 24×.

Two more scope facts worth knowing before you start digging:

- **`massive` emits no `external_poller` series at all.** The active
  fiat-FX feed is the `internal/sources/external/forex` worker inside
  the API binary; it writes `fx_quotes` directly and never runs under
  the `external.Connector` poller framework, so it is invisible to
  both alerts above. Its own family is
  `stellarindex_external_fx_feed_{stale,absent}` →
  [`fx-feed-stale.md`](fx-feed-stale.md).
- **`stellarindex_external_poller_error_rate_high` also carries this
  runbook's `runbook_url`** in both rule trees, so its page lands
  here. (`docs/operations/alerts-catalog.md` lists the sibling
  runbook for it instead — a catalog/rule drift worth closing; the
  `runbook_url` annotation is what Alertmanager actually renders.)
  It fires at the *informational* tier when the error
  rate is > 50 % sustained 15 min — a softer "something's degrading"
  signal that doesn't yet block data flow. See
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
[coingecko.com/en/developers/dashboard](https://www.coingecko.com/en/developers/dashboard).
The key belongs in `/etc/default/stellarindex` — that is the
`EnvironmentFile=` of `stellarindex-indexer.service` (templated from
`configs/ansible/roles/archival-node/templates/stellarindex.env.j2`,
which already carries `COINGECKO_DEMO_API_KEY`).

**Codify it, don't hand-edit it.** Per the r1 rule in CLAUDE.md, a
hand fix is overwritten by the next playbook run:

```sh
# on the workstation, from configs/ansible/
ansible-vault edit inventory/r1.secrets.yml     # set vault_coingecko_demo_api_key
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
  --tags stellarindex --check --diff            # always --check --diff first
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
  --tags stellarindex
```

The role's handler restarts the unit; to force it by hand:

```sh
ssh root@136.243.90.96 'systemctl daemon-reload && systemctl restart stellarindex-indexer'
```

Verify on next startup the indexer log shows
`source=coingecko ... auth_mode=demo` (or `pro` if a paid key).

### Paid-tier API key expired

Symptom: error contains `http 401` or `http 403` *with* a key set in
env. Common for CMC / CryptoCompare on annual renewal.

Fix: renew/rotate the key in the venue's dashboard, then re-run the
vault + playbook cycle above (same env file, same handler).

### Venue outage

Symptom: error contains `http 5..` or `connection refused`.

Action: check the venue's status page. If confirmed down, just
wait — the alert will clear once the venue recovers. Capture the
incident in `docs/operations/incidents/` if outage > 1h.

## Verification (post-fix)

```sh
# :9464 is the indexer's metrics port (:9100 is node_exporter), and it
# is loopback-bound — query it from the host.
ssh root@136.243.90.96 "curl -s http://localhost:9464/metrics \
  | grep -E 'stellarindex_external_poller_(polls|last_success).*<source>'"
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

The ECB split (`{source!="ecb"}` + the 12 h `_stale_ecb` rule) came
later, from F-1208 (codex audit-2026-05-13): the original blanket
30-minute rule fired after every successful ECB poll.

## Changelog

- 2026-05-13 — initial draft alongside the
  `stellarindex_external_poller_last_success_unix` wiring.
- 2026-08-29 — re-verified against HEAD (runbook re-verification
  wave K). Documented the ECB split (the blanket "30 minutes" was
  wrong by 24× for ECB) and the third alert that routes here;
  recorded that `massive` emits no `external_poller` series;
  corrected the impact row (ADR-0008 is HA topology, and
  `flags.reduced_redundancy` is the ADR-0017 cross-region signal, not
  a poller signal); repointed the API-key fix at
  `/etc/default/stellarindex` via ansible-vault; verification curl
  → `:9464` on r1's loopback. Dropped the dead PR references
  (#1139/#1140 predate this repository's numbering).
