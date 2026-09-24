---
title: Runbook — chainlink-feed-decimals
last_verified: 2026-09-24
status: draft
severity: P3
---

# Runbook — `stellarindex_chainlink_feed_decimals_mismatch` / `stellarindex_chainlink_feed_decimals_verify_failed`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_chainlink_feed_decimals_mismatch`, `stellarindex_chainlink_feed_decimals_verify_failed` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/divergence.yml` (+ R1 overlay) |
| Typical MTTR | 5–60 min |
| Impact | `_mismatch`: the affected Chainlink feed (`{consumer, pair}`) is REFUSED entirely — no divergence cross-check or oracle row for that pair from that consumer. `_verify_failed`: readings keep flowing at the last known scale, but the scale is unverified. |

## Why this exists

Both Chainlink readers (`internal/divergence` cross-check,
`internal/sources/external/chainlink` ingest poller) verify a feed's
configured `decimals` against the AggregatorV3 proxy's on-chain
`decimals()` before trusting a reading. A wrong or drifted configured
value would otherwise scale every reading by `10^(configured-actual)`
silently — a permanent false divergence, or `10^(d-8)`-off oracle
rows, with no signal at all. Two failure modes, two counters:

- **Mismatch** (fail CLOSED) — configured and on-chain decimals
  disagree. The feed is refused (`ErrPriceUnavailable`) until they
  agree; refused readings increment
  `stellarindex_chainlink_feed_decimals_mismatch_total{consumer,pair}`.
- **Verify failed** (fail OPEN) — the `decimals()` RPC call itself
  failed. The reader keeps serving at the last known value (configured
  or previously verified) and increments
  `stellarindex_chainlink_feed_decimals_verify_failed_total{consumer,pair}`.

Both counters are pre-seeded to zero for every configured feed at
construction (`NewChainlinkReference`, `NewPoller`), so an absent
series and a healthy one both read as a real zero.

## Symptoms

- `_mismatch`: `increase(stellarindex_chainlink_feed_decimals_mismatch_total[15m]) > 0` for
  the affected `{consumer, pair}` — that feed has produced nothing for
  5+ minutes.
- `_verify_failed`: a sustained non-zero rate on
  `stellarindex_chainlink_feed_decimals_verify_failed_total` for 30+
  minutes — pricing keeps working, but decimals() keeps erroring.

## Quick diagnosis (≤ 5 min)

```sh
# Which consumer/pair is affected?
curl -fs http://localhost:9465/metrics | grep '^stellarindex_chainlink_feed_decimals_'

# Configured decimals for the pair (divergence.chainlink.feeds / external.chainlink.feed_map)
grep -A3 '<pair>' /etc/stellarindex/config.toml

# Confirm the on-chain decimals() view directly (0x313ce567 selector)
curl -s -X POST "$CHAINLINK_RPC_URL" -H 'content-type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"<feed address>","data":"0x313ce567"},"latest"]}'
```

## Mitigation (≤ 60 min)

- [ ] **Mismatch**: fix the operator's `decimals` entry for the pair to
      match the on-chain value, or omit the field entirely to adopt
      decimals() automatically. Restart the affected process.
- [ ] **Verify failed**: confirm `CHAINLINK_RPC_URL` (or the
      divergence/ingest-specific RPC endpoint) is reachable and not
      rate-limited; point at a different provider if the current one
      is degraded.
- [ ] Verify: the counter's rate returns to zero and stays there.

## Known false-positive patterns

- A feed added to config for the first time can briefly show
  `_verify_failed` while the RPC connection warms up — the `for: 30m`
  window should absorb a single cold-start blip.

## Related

- `internal/divergence/chainlink_decimals.go`, `internal/sources/external/chainlink/decimals.go`.
- [`docs/reference/metrics/README.md`](../../reference/metrics/README.md) — metric definitions.

## Changelog

- 2026-09-24 — initial draft (GH-641 remainder: rule + catalog row had never shipped).
