---
title: Runbook — customer-webhook-delivery-failing
last_verified: 2026-08-29
status: current
severity: P3
---

# Runbook — `stellarindex_customer_webhook_delivery_*`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_customer_webhook_delivery_failing` (P3) / `stellarindex_customer_webhook_delivery_exhausted` (informational) / `stellarindex_customer_webhook_mark_errors` (P3) |
| Detected by | Prometheus rules in `configs/prometheus/rules.r1/api.yml` (the r1 overlay, loaded from `/etc/prometheus/rules.r1/*.yml`) + the multi-host twin `deploy/monitoring/rules/api.yml` (F-1270 audit-2026-05-12) |
| Typical MTTR | 5–30 min for a single-customer outage; longer when the worker itself is the problem |
| Impact | One or more customers aren't receiving the webhook callbacks they registered for. SEV-1 incident pings to their Slack / Discord / paging — failing — until their endpoint comes back up or they update the URL. The Stellar Index API itself is unaffected. |

## What this fires on

Three alerts:

- **`_failing`** — the delivery worker's
  `stellarindex_customer_webhook_delivery_attempts_total` counter
  has been recording `outcome="server_error"` or
  `outcome="network_error"` at > 0.1 attempts/s for 15+ min.
  Translation: one customer's endpoint is sustained-down (5xx
  or TCP/TLS errors) and we keep retrying with exponential
  backoff (30s → 1h cap, 15-attempt budget over ~8h).
- **`_exhausted`** — a delivery hit the retry budget and was
  marked terminally failed. The customer hasn't received the
  event AT ALL; if it was a SEV-1 incident notification, they
  may not know we declared the incident.
- **`_mark_errors`** — a DIFFERENT failure from the two above,
  and the one to read first when it fires alongside them. The
  POST reached a decision (2xx / 4xx / 5xx / network fault) and
  the store write recording that decision failed. Because
  `MarkDelivered` / `MarkAttemptFailed` is the only write that
  advances `attempt_count`, the row keeps the 5-minute claim
  lease `ListPendingDeliveries` set and the SAME payload is
  re-POSTed to the customer on every lease — indefinitely, with
  no retry budget to end it. A customer sees duplicate
  deliveries of one event; we see a delivery that never
  completes. Added #368 M6.

## Quick diagnosis (≤ 5 min)

```sh
# Which webhook is failing? Group recent failures by webhook_id.
ssh r1 'sudo -u postgres psql stellarindex -c "
  SELECT webhook_id,
         COUNT(*)                              AS attempts,
         MAX(last_response_status)             AS worst_status,
         MAX(last_error)                       AS sample_error,
         BOOL_OR(delivered_at IS NOT NULL)     AS any_delivered_recently
    FROM webhook_deliveries
   WHERE created_at > now() - interval '\''30 min'\''
     AND (last_response_status >= 500 OR last_error LIKE '\''POST%'\'')
   GROUP BY webhook_id
   ORDER BY attempts DESC
   LIMIT 10;
"'

# Look up the customer + URL for the top offender:
ssh r1 'sudo -u postgres psql stellarindex -c "
  SELECT w.id, w.account_id, a.name, a.billing_email, w.url, w.enabled
    FROM customer_webhooks w JOIN accounts a ON a.id = w.account_id
   WHERE w.id = '\''<webhook_id from above>'\'';
"'

# Confirm the worker itself is healthy (other webhooks succeeding):
ssh r1 'curl -s http://localhost:3000/metrics | grep stellarindex_customer_webhook_delivery_attempts_total | head -10'

# Latency posture (per-outcome p95/p99): if `delivered` p99 is
# climbing but the failing-rate alert hasn't tripped, a customer
# endpoint is going slow, not failing — different problem shape.
ssh r1 'curl -s http://localhost:3000/metrics | grep stellarindex_customer_webhook_delivery_duration_seconds | head -20'
```

Decision tree:

| `_failing` only, one webhook accounts for all failures | Other webhooks delivering normally | Action |
|---|---|---|
| Yes | Yes | Single-customer outage; reach out to the affected account |
| Yes | No (zero `delivered` outcomes) | The worker may be the problem — check the worker's logs (`journalctl -u stellarindex-api -g "customer-webhook"`) |
| No, many webhooks failing | n/a | Investigate the worker / upstream networking — is egress from R1 broken? Try a sample webhook URL from the box (`curl -X POST https://example.com/hook -d '{}'`). |

## Mitigation

- [ ] **Step 1 — Identify the affected webhook(s).** Use the
      SQL above. Capture: webhook ID, account ID, customer
      email, URL, the most-recent `last_error`.

- [ ] **Step 2 — If it's a single customer:** reach out via
      their billing_email or dashboard owner contact. Sample
      template:
      > Hi — our webhook delivery worker has been unable to
      > reach `<URL>` since `<timestamp>` (HTTP `<status>`).
      > After our 15-attempt retry budget elapses (~8h) we
      > mark the delivery permanently failed; missed events
      > include SEV-1 incident pings. Could you check the
      > endpoint? Updating the URL via
      > `PATCH /v1/dashboard/webhooks/{id}` re-arms delivery.

- [ ] **Step 3 — If the worker itself is failing** (zero
      `outcome="delivered"` over the same window):
  ```sh
  # Inspect logs for the underlying error (timeout / TLS / DNS).
  ssh r1 'journalctl -u stellarindex-api --since "30 min ago" \
    | grep -E "customer-webhook|delivery (delivered|failed)"'
  # Restart only as a last resort — the worker is in-process
  # with the API; restart paths cycle every cached state.
  ssh r1 'systemctl restart stellarindex-api'
  ```

- [ ] **Step 4 — For `_exhausted` alerts:** the budget hit.
      The row stays in `webhook_deliveries` for forensics; the
      customer will see the failed attempt in their dashboard.
      No retry is automatic — operator decides whether to
      manually re-enqueue. The cleanest path is to ask the
      customer to PATCH the webhook URL (if broken) or trigger
      a fresh event from their side.

## `_mark_errors` specifically

The counter advances at three sites in
`internal/customerwebhook/worker.go` (`classifyResponse`,
`handleFailure`, `markTerminal`); all three log a WARN naming the
`delivery_id`.

```sh
# Which deliveries are wedged? A high attempt-lease churn with a
# STATIC attempt_count is the signature.
ssh r1 'sudo -u postgres psql stellarindex -c "
  SELECT id, webhook_id, event_type, attempt_count, next_attempt_at, updated_at
  FROM webhook_deliveries
  WHERE next_attempt_at IS NOT NULL AND next_attempt_at < now() + interval '"'"'10 min'"'"'
  ORDER BY updated_at DESC LIMIT 20"'

# The worker's own account of it.
ssh r1 'journalctl -u stellarindex-api --since -2h | grep -i "customer-webhook.*Mark"'
```

Almost always this is Postgres: unreachable, in recovery, or the
statement is being cancelled. Restore the database and the next
lease writes the outcome and the loop ends by itself. If Postgres
is healthy, the write is being cancelled by its own context — the
worker shares the per-attempt HTTP deadline with the store write
(#368 M6, code half outstanding), so a POST that consumes the whole
attempt budget leaves no time for the mark.

## Related

- `internal/customerwebhook/worker.go` — implementation
- `docs/operations/runbooks/anomaly-freeze-engaged.md` — the
  upstream event that fires SEV-1 → customer webhook
- [`docs/architecture/platform-spec.md`](../../architecture/platform-spec.md)
  — the platform design covering both webhook directions. This
  runbook is for the OUTBOUND deliveries (us → customer); the
  Stripe billing bridge is the INBOUND surface (Stripe → us).
  Operators should know about both — a degraded Stripe bridge can
  leave customer dashboards stale even while the OUTBOUND worker
  is green.
- F-1270 audit register entry — context for the customer-
  facing webhook feature

## Changelog

- 2026-08-29 — re-verified against HEAD: all alert exprs, thresholds,
  retry-budget figures, SQL and metric names check out. Detected-by
  reordered r1-primary; dead `stripe-platform-sync-errors.md` link
  dropped — the outbound-vs-inbound webhook contrast now points at
  `docs/architecture/platform-spec.md` (no Stripe-sync runbook exists).
- 2026-05-12 — initial draft alongside the alert wiring (F-1270).
