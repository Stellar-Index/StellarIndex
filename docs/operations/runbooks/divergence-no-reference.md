---
title: Runbook — divergence-no-reference
last_verified: 2026-07-01
status: living
severity: P3
---

# Runbook — `stellarindex_divergence_no_reference`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_divergence_no_reference` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/divergence.yml` (+ R1 overlay) |
| Typical MTTR | 5–60 min (usually upstream-reference recovery / key restore) |
| Impact | Every configured reference is dark for the affected pairs, so `RefreshPair` writes a `SuccessCount=0` cache entry. `flags.divergence_warning` freezes at its last value — a **live depeg during the outage would go unflagged** (false-negative). Aggregate price endpoints keep serving; only the divergence flag is blind. |

## Why this exists (vs the error-dominant alert)

`stellarindex_divergence_refresh_error_dominant` compares `refresh_error`
vs `ok`. A **total reference outage** is different: `RefreshPair` reaches
the references, they all fail to respond, `SuccessCount==0`, and — before
CS-088 — it returned nil and counted as `ok`, so the checker went blind
*silently*. It now emits `outcome="no_reference"`, which is neither
`refresh_error` nor `ok`, so **only this alert can see it**. If you're
reading this, the divergence checker is running blind, not erroring.

## Symptoms

- `rate(stellarindex_divergence_refresh_total{outcome="no_reference"}[5m])`
  exceeds the `ok` rate, sustained 30+ min.
- The `ok` and `refresh_error` rates are both near zero (nothing is
  succeeding OR erroring — everything returns zero references).
- Aggregator logs repeat `divergence refresh failed … outcome=no_reference`.

## Quick diagnosis (≤ 5 min)

```sh
# 1) Confirm the dominant outcome is no_reference (not refresh_error).
curl -fs http://localhost:9465/metrics | grep '^stellarindex_divergence_refresh_total'

# 2) Which references are wired?
journalctl -u stellarindex-aggregator | grep 'divergence refresher wired' | tail -1

# 3) Probe each reference from the aggregator host:
curl -fs 'https://pro-api.coingecko.com/api/v3/simple/price?ids=stellar&vs_currencies=usd&x_cg_pro_api_key=REDACTED'   # CoinGecko Pro
#   Chainlink: grep 'rpc_url' the aggregator config, curl it with an eth_chainId JSON-RPC payload.
```

## Most likely causes (2026-07)

1. **CoinGecko Pro key missing / unset** — the free tier has been 429ing
   since 2026-06-19; the code auto-switches to the Pro endpoint when
   `COINGECKO_API_KEY` is set (see the operator-actions register / P0-3). If
   the key isn't set, CoinGecko is effectively dark.
2. **Chainlink RPC dark** — the divergence reference has its OWN `rpc_url`
   (separate from ingest). `llamarpc` now Cloudflare-challenges; confirm
   `CHAINLINK_RPC_URL` points at a working provider (it feeds both ingest +
   divergence since the audit-2026-06-19 fix).
3. **Egress blocked** — firewall/DNS change on the aggregator host cut all
   outbound HTTPS. Every reference goes dark at once.

## Mitigation (≤ 60 min)

- [ ] Restore the failing reference (set `COINGECKO_API_KEY`; point
      `CHAINLINK_RPC_URL` at a live provider), restart the aggregator.
- [ ] If one reference will be down for a while, that's fine — the alert
      compares against `ok`, so ANY responding reference clears it. Only a
      *total* outage fires this.
- [ ] Verify `rate(stellarindex_divergence_refresh_total{outcome="ok"}[5m])`
      recovers; the alert auto-resolves after 30 min sustained.

## Known false-positive patterns

- **Cold start** — masked by `for: 30m`.
- **A brand-new pair with no reference coverage** — a pair we track that no
  configured reference lists will always return `no_reference`. If this is a
  known-uncovered pair, exclude it or accept the noise; it is not an outage.

## Related

- [`docs/operations/runbooks/divergence-refresh-error-dominant.md`](divergence-refresh-error-dominant.md) — the erroring (not dark) sibling.
- [`docs/architecture/aggregation-plan.md`](../../architecture/aggregation-plan.md) — divergence service architecture.
- `internal/divergence/` (`ErrNoReferenceResponded`) + `internal/aggregate/orchestrator/divergence_refresh.go`.

## Changelog

- 2026-07-01 — initial draft alongside the CS-088 `no_reference` outcome.
