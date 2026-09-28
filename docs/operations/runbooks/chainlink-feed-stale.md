---
title: Runbook — chainlink-feed-stale
last_verified: 2026-09-28
status: draft
severity: P3
---

# Runbook — `stellarindex_chainlink_feed_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_chainlink_feed_stale` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/external-pollers.yml` (+ `configs/prometheus/rules.r1/external-pollers.yml`) |
| Typical MTTR | 10–60 min |
| Impact | One Chainlink ingest feed (`{pair}`) has written no current round to `oracle_updates` for 30+ min. Its sibling feeds, and the poller's own `stellarindex_external_poller_stale`, can stay green. |

## Why this exists

The Chainlink poller reads every configured feed each tick, and the
framework only marks a tick `error` when no feed at all produced an
update. One dark feed among healthy siblings was therefore visible only
as a WARN line every 30 s. The poller now records every feed's outcome on
`stellarindex_chainlink_feed_polls_total{pair,outcome}` and advances
`stellarindex_chainlink_feed_last_success_unix{pair}` only when the
feed returned a round within its `max_age_hours`. The gauge is seeded
to 0 at startup, so a feed that never succeeds after a restart also fires.

Refused rounds are never written: a round older than the feed's budget
(`outcome="stale"`), or one whose `answeredInRound < roundId`
(`outcome="carried_forward"`), would otherwise land as a new
observation of a price that was not published at that time.

## Symptoms

- `time() - stellarindex_chainlink_feed_last_success_unix{pair="…"}` above 1800.
- Indexer log: `chainlink feed poll failed … pair=<pair> outcome=<outcome>`.
- `stellarindex_oracle_stale{source="chainlink"}` for the same asset may
  follow once the pair's oracle staleness budget is exceeded.

## Quick diagnosis (≤ 5 min)

1. Which outcome is the feed reporting?

   ```promql
   sum by (outcome) (increase(stellarindex_chainlink_feed_polls_total{pair="<pair>"}[15m]))
   ```

2. Act on the outcome:

   | Outcome | Meaning | Action |
   | ------- | ------- | ------ |
   | `stale` | The proxy's latest `updatedAt` is older than the feed's budget. Chainlink has paused, deprecated or retired the feed, or the budget is set tighter than the heartbeat. | Check the feed on data.chain.link. If it is retired, remove it from `[external.chainlink].feed_map`, or move to the replacement proxy. If the heartbeat changed, set `max_age_hours` from the new heartbeat. Never widen the budget just to silence the alert. |
   | `carried_forward` | `answeredInRound < roundId`: the aggregator carried an old answer forward. | Usually transient. If it persists, treat it like `stale`. |
   | `error` | RPC failure, decode failure, decimals refusal or projection failure. | Read the WARN line's `err`. For a decimals refusal, see [chainlink-feed-decimals](chainlink-feed-decimals.md). For RPC errors, see [external-poller-stale](external-poller-stale.md). |
   | only `0` samples | The gauge is seeded but no poll has run for this pair. | Check that the indexer is ticking the chainlink poller (`stellarindex_external_poller_polls_total{source="chainlink"}`). |

## Resolution

The alert clears on the first poll that returns a round within budget.
A retired feed stays dark until it is removed from config. That is correct:
the last round must not be re-served as current.

## Related

- [external-poller-stale](external-poller-stale.md) — the whole poller has stopped succeeding.
- [chainlink-feed-decimals](chainlink-feed-decimals.md) — a feed refused for a scale disagreement.
- [oracle-stale](oracle-stale.md) — per-asset oracle publication age.
- `internal/sources/external/chainlink/poller.go` — `checkRoundCurrent`, `recordFeedOutcome`.
