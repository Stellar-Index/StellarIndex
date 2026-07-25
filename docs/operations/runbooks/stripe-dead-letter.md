# Stripe dead-letter — a customer paid and provisioning failed

## At a glance
- **Severity:** page — a paying customer is without their entitlement
- **Gauge:** `stellarindex_stripe_dead_letters_open` (boot + 60s re-seed from `stripe_event_log`)
- **First move:** run the triage query below; the `dead_letter_reason` column names the failure mode
- **Resolve by:** re-sending the event from Stripe after fixing the cause — never by manual row edits

**Alert:** `stellarindex_stripe_dead_letter_open` (page).
**Meaning:** `stripe_event_log` holds rows with `dead_lettered_at`
set and `dead_letter_resolved_at` NULL — money arrived, the tier/key
upgrade did not complete, and nothing will retry it automatically
beyond Stripe's own delivery retries (the event row stays
`processed_at NULL`, so a re-delivery re-runs the work).

## Triage
```sql
SELECT stripe_event_id, event_type, dead_letter_reason,
       dead_lettered_at, error
FROM stripe_event_log
WHERE dead_lettered_at IS NOT NULL AND dead_letter_resolved_at IS NULL
ORDER BY dead_lettered_at;
```
Reasons: `no_keys` (paid account has no credentials to upgrade — mint
or link keys, then re-send the event from the Stripe dashboard);
`key_upgrade_failed` (store write failed — check Postgres/Redis
health, then re-send the event).

## Resolution
Re-send the event from Stripe (Developers → Events → Resend). A
successful re-run stamps `processed_at` AND `dead_letter_resolved_at`
in the same statement; the gauge decrements within 60s (periodic
re-seed). Manual closure without re-provisioning is NOT offered by
design — resolve means the customer got what they paid for.

## Related
- [alerts-catalog](../alerts-catalog.md) — severity conventions
- `migrations/0118_stripe_event_dead_letter.up.sql` — the schema
- `internal/api/v1/stripe_webhook.go` `deadLetterStripeEvent` — the open path
- F-1322 reprocessable-event contract (processed_at NULL => re-send re-runs)
