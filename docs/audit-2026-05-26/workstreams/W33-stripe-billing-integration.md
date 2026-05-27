# W33 — Stripe billing integration

## Scope

Stripe webhook + billing surface:

- `internal/api/v1/stripe_webhook.go`
- `internal/platform/billing.go`
- `internal/platform/account.go`
- `internal/platform/apikey.go` (tier-upgrade path)
- `internal/usage/counter.go` (usage metering)
- `cmd/ratesengine-ops/mint_key.go`, `upgrade_key.go`
- Stripe webhook signature verification + secret management
- idempotency

## Inputs

- `internal/api/v1/stripe_webhook.go`
- `internal/platform/billing.go` + tests
- `internal/api/v1/server.go` route registration
- env vars: STRIPE_API_KEY, STRIPE_WEBHOOK_SECRET (verify in
  config schema)
- `web/dashboard/` Stripe surface

## Checks

| # | Check | Method |
| --- | --- | --- |
| W33.1 | Stripe webhook handler verifies `Stripe-Signature` HMAC against `STRIPE_WEBHOOK_SECRET` | handler code |
| W33.2 | Verification rejects malformed signature with 401 | tests |
| W33.3 | Unsigned request rejected with 401 | tests |
| W33.4 | Each Stripe event Id tracked → replay rejected | idempotency-key path |
| W33.5 | Subscription.updated path correctly maps Stripe price → our tier | mapping table |
| W33.6 | Charge.succeeded path triggers credit grant + audit log | code |
| W33.7 | Charge.refunded path triggers tier downgrade or credit revoke | code |
| W33.8 | Invoice.paid path increments next billing cycle | code |
| W33.9 | NO Stripe secret in code (env var only) | grep + lint-imports |
| W33.10 | NO Stripe customer Id in log lines (PII) | log audit |
| W33.11 | Webhook timeout returns 200 to Stripe within Stripe's window (5s typical) | handler timing |
| W33.12 | Webhook errors return Stripe's expected 4xx for retry vs 5xx for poison | response codes |
| W33.13 | Per-key tier enforcement on rate-limit/usage uses the latest webhook-applied tier | rate-limit middleware |
| W33.14 | mint_key.go cannot mint without operator confirmation of tier | flag handling |
| W33.15 | upgrade_key.go re-uses Stripe path or admin override path? | code |
| W33.16 | usage counter recorded per-key; idempotent across restarts | counter implementation |
| W33.17 | Daily/monthly usage reset matches Stripe billing cycle | cycle logic |
| W33.18 | Refund-bypass via race: customer refunds, then new charge before our DB reconciles | state machine |
| W33.19 | Stripe live-mode key separation from test-mode key (audit verifies test-mode only — see X-0002) | env separation |
| W33.20 | dashboard surface shows accurate billing state | web/dashboard audit |

## Closure criteria

Every check terminal. Findings on:

- ANY signature-verification bypass → `critical`
- replay-attack → `critical`
- tier-upgrade-without-charge → `critical`
- refund-bypass → `critical`
- billing meter divergence from Stripe → `high`
- Stripe secret in code/logs → `critical`
- dashboard showing wrong tier → `medium`
