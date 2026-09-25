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
| Impact | Every configured reference is dark for the affected pairs, so `RefreshPair` writes a `SuccessCount=0` cache entry. `flags.divergence_warning` freezes at its last evaluated value and `flags.divergence_checked` reads `false`; the persistence streak and the webhook latch are left untouched, so a warning that was firing is neither cleared nor re-sent when references return. A **live depeg during the outage would go unflagged** (false-negative). Aggregate price endpoints keep serving; only the divergence flag is blind. |

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
curl -fs 'https://api.coingecko.com/api/v3/simple/price?ids=stellar&vs_currencies=usd'   # CoinGecko (free tier, no key)
#   Chainlink: grep 'rpc_url' the aggregator config, curl it with an eth_chainId JSON-RPC payload.
```

## Most likely causes (2026-07)

1. **CoinGecko free-tier 429 throttling** — this alert's `CoinGeckoReference`
   (the PRICE reference, `divergence.coingecko`) is free-tier only; it has no
   API key field and cannot auth as Pro. It has been 429-throttled since
   2026-06-19 and there is no key-based fix for it — if CoinGecko is the
   only reference covering a pair, expect intermittent `no_reference` until
   another reference (Chainlink, an on-chain oracle) covers the pair too.
   Do NOT confuse this with the separate SUPPLY cross-check
   (`divergence.supply.coingecko`), which DOES take `COINGECKO_API_KEY` and
   auto-switches to the Pro host when it's set — that key has no effect
   here.
2. **Chainlink RPC dark** — the divergence reference has its OWN `rpc_url`
   (separate from ingest). `llamarpc` now Cloudflare-challenges; confirm
   `CHAINLINK_RPC_URL` points at a working provider (it feeds both ingest +
   divergence since the audit-2026-06-19 fix).
3. **Egress blocked** — firewall/DNS change on the aggregator host cut all
   outbound HTTPS. Every reference goes dark at once.

## Mitigation (≤ 60 min)

- [ ] Restore the failing reference: for CoinGecko wait out the 429 or add a
      non-CoinGecko reference for the affected pair (`COINGECKO_API_KEY` only
      helps the separate supply cross-check, not this price path); point
      `CHAINLINK_RPC_URL` at a live provider. Restart the aggregator after
      any config change.
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
- **Only slow references cover the pair** — a quote observed more than
  `divergence.MaxComparableAge` before the comparison (1h; the FX budget for
  fiat/fiat pairs) is recorded in the cached result's `failures` as
  `too_stale_to_compare` and does not vote. A pair covered only by
  daily-heartbeat feeds (Redstone, Band) reads `no_reference` between their
  pushes. Not an outage; add a fresher reference for the pair.

## Partial outage — `stellarindex_divergence_reference_failing` / `stellarindex_divergence_pair_below_quorum`

`no_reference` needs EVERY reference dark for a pair. One reference going
dark leaves every pass `ok`, yet a pair covered by exactly
`min_sources_for_warning` references then drops below quorum: its verdict
is carried forward, never re-evaluated, and a live depeg on it cannot warn.
These two alerts read the per-reference signal the pass-level counter lacks.

- `stellarindex_divergence_reference_failing{reference}` — more than half of
  that reference's lookups failed for 30+ min (`asset_unsupported` and
  `too_stale_to_compare` excluded: coverage and feed cadence, not outages).
- `stellarindex_divergence_pair_below_quorum{pair}` — every refresh of the
  pair for an hour had fewer responders than `min_sources_for_warning`.

```promql
# Which outcome is the reference producing?
sum by (reference, outcome) (rate(stellarindex_divergence_reference_total[15m]))
# Which pairs are disarmed right now?
stellarindex_divergence_pair_quorum_met == 0
```

`price_unavailable` from CoinGecko is its freshness gate failing closed (a
missing or stale `last_updated_at`); `timeout` /
`overall_deadline_exceeded` is a slow upstream; `panicked` also moves
`stellarindex_worker_panics_total`. Remediate the reference as in
*Mitigation* above. A pair whose only other coverage is a daily-heartbeat
feed sits below quorum between pushes; that is structural, not an outage —
add a fresher reference for the pair.

## Related

- [`docs/operations/runbooks/divergence-refresh-error-dominant.md`](divergence-refresh-error-dominant.md) — the erroring (not dark) sibling.
- [`docs/architecture/aggregation-plan.md`](../../architecture/aggregation-plan.md) — divergence service architecture.
- `internal/divergence/` (`ErrNoReferenceResponded`) + `internal/aggregate/orchestrator/divergence_refresh.go`.

## Changelog

- 2026-07-01 — initial draft alongside the CS-088 `no_reference` outcome.
- 2026-09-23 — references older than the comparability ceiling no longer vote.
- 2026-09-25 — partial-outage section for the per-reference and quorum alerts (GH-679).
