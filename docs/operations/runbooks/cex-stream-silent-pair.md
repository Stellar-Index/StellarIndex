---
title: Runbook — cex-stream-silent-pair
last_verified: 2026-09-24
status: draft
severity: P2
---

# Runbook — `stellarindex_cex_stream_subscription_rejected` / `stellarindex_cex_stream_entry_skips`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_cex_stream_subscription_rejected`, `stellarindex_cex_stream_entry_skips` |
| Severity | ticket |
| Detected by | Prometheus rules in `deploy/monitoring/rules/external-pollers.yml` and `configs/prometheus/rules.r1/external-pollers.yml` |
| Typical MTTR | 30 min (a pair-map code change and deploy) |
| Impact | One CEX pair delivers no trades while the socket stays healthy; the aggregator sees that pair's `source_count` drop by one. |

## Why this exists

A de-listed or renamed venue pair used to look exactly like a quiet
pair: the WebSocket stayed connected, no reconnect, no stall, no
decode error. Both signals below come from the kraken streamer
(`internal/sources/external/kraken/streamer.go::frameHandler`).

## Symptoms

- `stellarindex_cex_stream_subscription_rejected{source,symbol} == 1`:
  the venue answered our subscribe request for `symbol` with
  `success:false`. The streamer does NOT reconnect — kraken answers
  per symbol, so the other pairs on the socket are still live.
- `stellarindex_cex_stream_entry_skips_total{source,reason}` rising
  for 30 min: trade entries inside well-formed frames are being
  dropped. `reason` says which field failed (`unknown_symbol`,
  `bad_qty`, `bad_price`, `bad_timestamp`, `bad_trade_id`, `other`).

## Quick diagnosis (≤ 5 min)

1. Grep the indexer log for the venue's own words:
   - rejection: `kraken rejected the trade subscription` — the
     `venue_error` field is kraken's text (e.g. `Currency pair not
     supported XLM/GBP`).
   - skips: `kraken trade entry skipped` — logged once per reason per
     minute; `err` names the offending symbol or field value.
2. For `unknown_symbol`: the venue is sending a symbol that is not in
   `internal/sources/external/kraken/pairs.go::DefaultPairList` —
   usually a rename of a configured pair.
3. For `bad_qty` / `bad_price` / `bad_timestamp`: the venue changed a
   field's encoding (e.g. scientific notation, epoch timestamps).
   Compare the logged value with the parser in
   `internal/sources/external/kraken/parse.go::buildTrade`.

## Remediation

- Rejected or renamed pair: update `DefaultPairList` in
  `internal/sources/external/kraken/pairs.go` and deploy. The gauge
  clears to 0 when the venue next acknowledges the symbol; a symbol
  removed from the config keeps its last value until the indexer
  restarts. `symbol="unknown"` means the rejection named no configured
  symbol and also clears only on restart.
- Field re-encoding: fix `buildTrade` with a test pinned to the new
  wire shape.

## Related

- [external-poller-stale](external-poller-stale.md) — the polling-side
  staleness family, which cannot observe WebSocket streamers.
- [decode-errors](decode-errors.md) — whole-frame decode failures
  (`stellarindex_source_decode_errors_total`).
