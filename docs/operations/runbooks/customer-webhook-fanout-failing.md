---
title: Runbook — customer-webhook-fanout-failing
last_verified: 2026-07-26
status: draft
severity: P3
---

# Runbook — `stellarindex_customer_webhook_fanout_failing`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_customer_webhook_fanout_failing` (P3 / ticket) |
| Detected by | Prometheus rules in `deploy/monitoring/rules/aggregator.yml` + `configs/prometheus/rules.r1/aggregator.yml` (C3-023, audit-2026-07-23) |
| Typical MTTR | Minutes once the Postgres write path is healthy; the *re-emit* is the slow part |
| Impact | A customer subscribed to an event did not get a delivery row written for it. Nothing will retry — the event is permanently lost from their perspective until an operator re-emits. |

## What this fires on

`stellarindex_customer_webhook_fanout_failures_total` increments in
`customerwebhook.Fanout.Publish` — the **producer** side of the
customer-webhook chain, which runs in the **aggregator** binary (freeze
and divergence hot paths) and in `stellarindex-ops emit-incident`.

This is deliberately a different alert from
[customer-webhook-delivery-failing](customer-webhook-delivery-failing.md).
That one watches delivery **attempts**, which only exist once a
`webhook_deliveries` row has been written, and every failure there is
retried on a 15-attempt / ~72 h budget. A fan-out failure happens
*before* that row exists:

| `reason` | What happened | Blast radius |
| --- | --- | --- |
| `enqueue` | One subscriber's `EnqueueDelivery` INSERT failed | That subscriber only — the counter counts lost deliveries |
| `list_subscribers` | `ListWebhooksSubscribedTo` errored | **Every** subscriber of that event type |
| `invalid_payload` | The producer handed `Publish` non-JSON | Every subscriber; this is a code bug, not an outage |

There is no retry row, no dead-letter, and nothing downstream that
re-derives the delivery. Which is why one occurrence is worth a ticket
rather than a sustained-rate threshold.

Before C3-023, `Publish` had no return value at all: a fan-out that
enqueued zero of five subscribers was indistinguishable from a healthy
one at the call site, and the only trace was a WARN line.

## Quick diagnosis (≤ 5 min)

1. **Which event type and which failure mode?** The alert carries both:
   `{{ $labels.event_type }}` / `{{ $labels.reason }}`.

   ```promql
   sum by (event_type, reason) (
     increase(stellarindex_customer_webhook_fanout_failures_total[1h])
   )
   ```

2. **Read the producer's log line.** Every increment is paired with a
   WARN inside the fan-out plus an ERROR at the call site naming the
   event that was lost:

   ```sh
   journalctl -u stellarindex-aggregator --since '-2h' \
     | grep -E 'customerwebhook.fanout|fan-out lost'
   ```

   The ERROR line carries the identifying dimension — `asset`/`quote`
   for `anomaly.freeze`, `pair` for `divergence.firing` — plus
   `subscribers` / `enqueued` / `failed`.

3. **Is Postgres the cause?** `reason=enqueue` and
   `reason=list_subscribers` are both `webhook_deliveries` /
   `customer_webhooks` access failures. Check the platform Postgres:
   connection saturation, disk, a statement timeout, or the app role's
   grants (`migrations/README.md` rule 7).

   ```sh
   ssh r1 'sudo -u postgres psql stellarindex -c "
     SELECT count(*) FROM webhook_deliveries WHERE created_at > now() - interval '\''1 hour'\'';
   "'
   ```

## Remediation

1. **Fix the store.** Nothing else is worth doing while inserts are
   still failing.
2. **Identify what was lost.** The triggering events are durable in
   their own tables even though the customer copies are not:

   | `event_type` | Where the source of truth lives |
   | --- | --- |
   | `anomaly.freeze` | `freeze_events` |
   | `divergence.firing` | `divergence_runs` (+ the Redis cached result) |
   | `incident.sev1` / `incident.resolved` | The incident markdown under `deploy/comms/` |
   | `price.alert` | `price_alerts` (this producer does not use `Fanout`; see below) |

   Cross-reference the window from step 2 against
   `webhook_deliveries` — rows that *should* exist for a subscriber of
   that event type and don't are the losses.
3. **Re-emit.** For incidents, re-run
   `stellarindex-ops emit-incident -slug <slug> -event <sev1|resolved>`;
   since C3-023 that command exits non-zero if the fan-out loses
   anything, so a zero exit is your confirmation. Freeze / divergence
   events have no re-emit command — if a customer materially depends on
   them, contact them directly rather than pretending the gap
   self-heals.
4. **`invalid_payload` is a code bug.** No amount of store health fixes
   it: a producer is calling `Publish` with bytes that are not JSON.
   Find the call site from the log line and fix the marshalling.

## Do NOT

- **Do not treat this as covered by the delivery alerts.** They watch a
  table this failure never wrote to. Zero delivery failures alongside a
  firing fan-out alert is the *expected* combination, not a
  contradiction.
- **Do not make the fan-out blocking** to "fix" this. The producers are
  the aggregator's freeze and divergence hot paths; failing them on a
  webhook-store blip would trade a lost customer notification for a
  stalled price pipeline. The error return exists to be *logged and
  counted*, not to abort the producer.

## Related

- [customer-webhook-delivery-failing](customer-webhook-delivery-failing.md)
  — the same chain, one step later, where retries do exist.
- [admin-audit-write-failing](admin-audit-write-failing.md) — the other
  best-effort write path whose silence the same audit wave closed.

## Notes

- `price.alert` fan-out is emitted by `internal/pricealerts/worker.go`,
  which enqueues deliveries directly rather than through
  `customerwebhook.Fanout`. Its series are pre-seeded for completeness
  but will not move until that producer is migrated onto `Fanout`.
- The `stellarindex-ops emit-incident` process is short-lived and is
  never scraped, so its increments do not reach Prometheus. That call
  site returns the error to the operator's shell instead.
