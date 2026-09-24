---
title: Runbook — cex-stream-disconnect-storm
last_verified: 2026-09-24
status: draft
severity: P3
---

# Runbook — `stellarindex_cex_stream_stalled` / `stellarindex_external_dust_dropped_high`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_cex_stream_stalled`, `stellarindex_external_dust_dropped_high` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/external-pollers.yml` (+ R1 overlay) |
| Typical MTTR | 5–30 min |
| Impact | `_stalled`: the venue's WebSocket connection looks open but has stopped delivering trades — a silent gap in trade coverage until the OS eventually notices the dead socket. `_dropped_high`: a venue is discarding far more trades as "dust" than usual, which can mean real volume is being thrown away. |

## Symptoms

- `_stalled`: `increase(stellarindex_cex_stream_disconnect_total{reason="stall"}[15m]) > 0`
  for 5+ min for a given `source`. `wsclient.ErrStreamStalled` — the
  venue stopped answering pings but TCP has not noticed.
- `_dropped_high`: `rate(stellarindex_external_dust_dropped_total[15m]) > 1`
  sustained 30+ min for a given `source` — well above the normal
  low-and-steady dust background.

## Quick diagnosis (≤ 5 min)

```sh
# Which source and reason is firing?
curl -fs http://localhost:9465/metrics | grep -E '^stellarindex_(cex_stream_disconnect_total|external_dust_dropped_total)'

# Recent stall / dust-drop log lines
journalctl -u stellarindex-indexer | grep -E 'stream stalled|dropped as dust' | tail -20
```

## Mitigation (≤ 30 min)

- [ ] **Stalled**: the reconnect loop (`wsclient.Loop`) already forces a
      fresh dial on `ErrStreamStalled`; if the rate stays elevated,
      suspect the venue-side infra (a stuck load balancer node) rather
      than our client — cycling the process forces a fresh outbound
      connection immediately.
- [ ] **High dust drop**: compare against the venue's own trade feed for
      the same window — if legitimate sub-cent fills spiked (a
      low-price/high-volume listing event), this is expected and can be
      acknowledged; if not, check whether the venue changed a symbol's
      price/amount scaling (the dust guard keys off quote/base ratio,
      so a decimals regression reads as "everything is dust").
- [ ] Verify: the counter's rate returns to its prior baseline.

## Known false-positive patterns

- A venue-wide low-price listing event (new sub-cent token) can
  legitimately spike `_dropped_high` — cross-check the venue's own
  volume dashboard before treating it as a bug.

## Related

- `internal/sources/external/wsclient/wsclient.go` (stall detection).
- `internal/sources/external/runner.go` (dust guard, `forwardTrades`).
- [`docs/operations/runbooks/cex-stream-silent-pair.md`](cex-stream-silent-pair.md) — the sibling per-symbol rejection/skip alerts.

## Changelog

- 2026-09-24 — initial draft (GH-941 remainder: rule + catalog row had never shipped).
