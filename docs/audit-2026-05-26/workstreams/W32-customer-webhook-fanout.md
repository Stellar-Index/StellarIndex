# W32 — Customer webhook fanout

## Scope

The customer-facing webhook fanout (push notifications when
prices change, anomaly freezes, etc.):

- `internal/customerwebhook/worker.go`
- `internal/customerwebhook/fanout.go`
- `internal/customerwebhook/ssrf.go`
- `internal/customerwebhook/worker_test.go`
- DB schema for customer_webhook_subscriptions +
  customer_webhook_deliveries (verify migration source)
- billing intersection (does fanout charge per delivery?)
- per-source events that trigger fanout

## Inputs

- `internal/customerwebhook/` (entire package)
- migration that creates the subscription + delivery tables
- `internal/platform/webhook.go` (related — verify)
- `internal/notify/` (related — email vs webhook)
- `docs/operations/runbooks/customer-webhook-delivery-failing.md`

## Checks

| # | Check | Method |
| --- | --- | --- |
| W32.1 | Subscription model: who owns it, what events it claims, what URL gets the post | schema + worker code |
| W32.2 | Worker dispatches events to the URL with retries on 5xx + timeouts | worker code |
| W32.3 | SSRF defence rejects localhost, 127.0.0.0/8, 169.254.169.254, 10/8, 172.16/12, 192.168/16, IPv6 link-local | `ssrf.go` + test |
| W32.4 | SSRF defence pins DNS resolution to prevent rebinding | `ssrf.go` |
| W32.5 | Failed deliveries don't block ingest | worker design |
| W32.6 | Delivery outcomes recorded in customer_webhook_deliveries | DB write path |
| W32.7 | Per-customer max subscriptions enforced | rate-limit + schema |
| W32.8 | Webhook signing: requests include `X-RatesEngine-Signature` HMAC | fanout.go |
| W32.9 | Subscription URL update flow re-validates SSRF | API handler |
| W32.10 | Delivery retry budget bounded (exponential backoff, max attempts) | worker code |
| W32.11 | Dead-letter handling for permanently-failing subscriptions | worker code |
| W32.12 | Metric: `customer_webhook_delivery_total{outcome}` + duration histogram | obs.metrics + per-binary registration |
| W32.13 | Alert `customer-webhook-delivery-failing` references metric + runbook | rule + runbook |
| W32.14 | runbook explains diagnosis + escalation | doc audit |
| W32.15 | Billing: each delivery counts as usage event? Or free? | `internal/platform/billing.go` |
| W32.16 | Auth required to add subscription (API key scope) | handler auth gate |
| W32.17 | Customer can list / delete their own subscriptions only | scope check |
| W32.18 | Test: `worker_test.go` covers happy path + SSRF + retry | test inspection |
| W32.19 | NEW: integration test covers a malicious subscriber URL doesn't crash the worker | test gap |
| W32.20 | NEW: webhook delivery to a customer URL that responds with attacker-controlled redirects (302 → private IP) is followed but re-checked | SSRF post-redirect |

## Closure criteria

Every check terminal. Findings on:

- any SSRF bypass (private-IP, DNS-rebind, redirect-follow)
- any per-customer DoS (1000 subscriptions × 1000 deliveries)
- any signing gap
- any retry-storm gap (exponential backoff missing)
- any billing-meter bypass (customer triggers free deliveries
  via cheap event subscription)
