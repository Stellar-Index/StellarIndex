---
title: SEV-1 tabletop — Anomaly freeze stuck-engaged on a major pair
last_verified: 2026-10-05
status: ratified
severity: P1
exercises_runbook: ../../runbooks/anomaly-freeze-engaged.md
playbook_section: ../../sev-playbook.md#4-response-flow
---

# SEV-1 tabletop — Anomaly freeze stuck-engaged

~30 min, 3 people. Exercises the ADR-0019 chain (`internal/aggregate/anomaly`, `baseline`, `freeze`) and `/v1/price`'s `flags.frozen`.
A stuck freeze on a major pair (XLM/USD) means every consumer reads a frozen last-known-good price while real moves are suppressed.

## Setup and trigger

All services up, aggregator producing closed-bucket VWAPs each minute, `flags.frozen` <0.1% baseline. 22:15 UTC Friday, light traffic.

> 22:17 UTC a CEX feed (Binance) goes down for 90 s. Class diversity for XLM/USD drops below `class_diversity_min=3`,
> `anomaly.ActionFreeze` fires and `freeze.Writer` publishes `freeze:native:fiat:USD` with TTL 600 s. The feed returns at 22:18:30
> and diversity recovers, but the writer's contract is set-on-engage, clear-by-operator (ADR-0019 Phase 1), so the marker stays.
> `/v1/price?asset=native` returns the same value with `flags.frozen=true` for ~10 minutes.

## Beats (T+ min:sec)

| T+ | Beat |
| --- | --- |
| 0:00 | Freeze-dwell page on XLM/USD (frozen >5 closed buckets); `stellarindex_anomaly_freeze_engaged` (`anomaly.yml`, ticket, 1m after engage) and `stellarindex_anomaly_freeze_active` (`freeze-lifecycle.yml:349`, informational, >0 for 5m) are the early signals; neither pages. Only `stellarindex_anomaly_freeze_escalated` (`freeze-lifecycle.yml:146`, page) wakes anyone, and it fires on escalation to operator review, not on a stuck marker |
| 0:30 | Customer team asks if the feed is broken (same number for 8 min) |
| 3:00 | `redis-cli GET freeze:native:fiat:USD` shows `engaged_at=...,reason=class_diversity_drop` |
| 5:00 | Source-class diversity query: dropped to 2 at 22:17, back to 4 at 22:18:30 |
| 8:00 | Second customer stopped trading XLM/USD for 12 min |
| 12:00 | Marker has ~7 min TTL left; operator weighs waiting vs clearing |

## Expected response

- **5 min:** acknowledge; open `#incident-<YYYY-MM-DD>-freeze-stuck`; post "stuck-frozen flag on XLM/USD, price feed may not update"; status page *Degraded performance* on API.
- **10 min, diagnose** ([anomaly-freeze-engaged.md](../../runbooks/anomaly-freeze-engaged.md)): read the marker; check `engaged_at` and `reason`
  against the alert; check the class-diversity gauge for the last 15 min. If recovered, the freeze is stuck (Phase 1 does not auto-clear).
  Verify upstream recovery before clearing.
- **20 min, mitigate:** operator clear:
  ```
  stellarindex-ops freeze-unfreeze -config /etc/stellarindex.toml \
    -asset native -quote fiat:USD \
    -reason "drill: escalated freeze, oracle verified healthy by hand" \
    -write
  ```
  `-reason` is required (an unfreeze overrides an automated safety control on a money surface); `-list` shows open freezes, `-dry-run` rehearses.
  **Never `redis-cli DEL freeze:native:fiat:USD`**: since migration 0119 the durable ladder leaves `freeze_events.recovered_at` NULL, reads the missing
  marker as "Redis lost it", rehydrates and re-writes it next tick, so a bare DEL is inert for escalated freezes.
  Verify next tick: `curl -sS https://api.stellarindex.io/v1/price?asset=native | jq '.flags'` shows `frozen` false. If it re-engages, the source condition
  is not recovered; investigate the source.
- **30 min:** status *Mitigated*: "A stuck price-freeze flag on XLM/USD has been cleared; live updates resumed at <UTC>."
- **24 h postmortem** covers: why Phase 1 chose operator-clear over auto-clear (flapping defence); whether the 600 s TTL suits major pairs; whether the runbook states TTL semantics.

## Pass criteria

1. Classified SEV-1 (frozen feed on a major pair is effective service-down for that pair).
2. Read `freeze:native:fiat:USD` directly, not just the metric.
3. Distinguished stuck from legitimate freeze via the class-diversity timeline.
4. Identified operator-clear as ADR-0019 Phase 1 design, not a bug.
5. Used `stellarindex-ops freeze-unfreeze` (never `redis-cli DEL`) instead of waiting for TTL.
6. Verified `flags.frozen` false on the next tick.
7. Postmortem recorded the clear-policy rationale rather than "just auto-clear".
8. Status-page wording stayed factual until *Identified*.

## Variants

Phase 2 freeze (multi-window MAD baseline fires on a real market move; oncall should defend the freeze, not clear it); cascade freeze
(a stuck XLM/USD also freezes every triangulated pair; clear the root cause, not the leaves).
