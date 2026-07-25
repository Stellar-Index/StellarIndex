# Stripe dead-letter — a customer paid and provisioning failed

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
