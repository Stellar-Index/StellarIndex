---
title: Runbook — admin-audit-write-failing
last_verified: 2026-07-26
status: draft
severity: P3
---

# Runbook — `stellarindex_admin_audit_write_failing`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_admin_audit_write_failing` (P3 / ticket) |
| Detected by | Prometheus rule in `deploy/monitoring/rules/api.yml` and the R1 single-host overlay `configs/prometheus/rules.r1/api.yml`. |
| Typical MTTR | 15–60 min. The *recovery* (reconstructing the lost rows from application logs) is time-boxed by log retention, so start it before fixing the cause. |
| Impact | **Compliance / forensics, not availability.** A privileged mutation is live with no durable record of the actor, the reason, or the previous values. Nothing is broken for customers. |

## What this fires on

Every privileged mutation on the admin + Stripe surfaces appends its
audit row **best-effort**: by the time the append runs, the mutation has
already committed, so a failure is logged and swallowed rather than
propagated. Un-doing a committed tier change because the audit store
blipped would be strictly worse.

Before C3-067 (audit-2026-07-23) the only trace was one
`logger.Warn("… audit append failed (best-effort)")` line.
`stellarindex_admin_audit_write_failures_total` now increments with a
`surface` label naming which mutation class lost its row:

| `surface` | Mutation | What is now unrecorded |
| --- | --- | --- |
| `account_override` | `PATCH /v1/admin/accounts/{id}` | Tier / rate-limit / quota override, its reason, and the before-values |
| `key_mint` | `POST /v1/admin/keys` | A **live credential** and who minted it for whom |
| `key_revoke` | Admin key revoke | Which credential was revoked and by whom |
| `status_notice` | `POST /v1/admin/status-notices` (+ resolve) | A change to the **public** status page |
| `stripe_plan_upgrade` | Stripe webhook plan change | The link between a paid Stripe event and the plan it granted |
| `stripe_dead_letter` | Stripe dead-letter conclusion | The record that money landed and nothing was provisioned |
| `staff_customer_lookup` | `GET /v1/account/admin/lookup` (**read**) | That a staff member read a customer's billing email, tier/status and every user's email + last-login |

`staff_customer_lookup` is the only **read** in the table (C3-056). It
mutates nothing, so there is no "the change is live but unrecorded"
problem — the problem is the mirror image: an access to another
customer's PII happened and the durable record of *who* looked at
*whose* data is missing. It cannot be reconstructed from the target
object (nothing changed), only from the API request log.

The rule uses `increase(...[1h]) > 0` with `for: 5m`, not `rate() > 0`:
these are rare, bursty events and the required response is triggered by a
**single** occurrence, not by a sustained rate.

## Quick diagnosis (≤ 5 min)

1. **Which surface?** The alert carries `{{ $labels.surface }}`. Use the
   table above to know what kind of record is missing.
2. **Why did the append fail?** The paired log line carries the cause and
   the target id:

   ```sh
   journalctl -u stellarindex-api --since '-2h' | grep 'audit append failed'
   ```

   Expect one of: Postgres unreachable / in recovery, the `audit_log`
   table's disk full, a permissions regression on the table (see
   `migrations/README.md` rule 7 — an object created as the superuser
   instead of the `stellarindex` app role), or a statement timeout.
3. **Is it still failing?**
   `sum by (surface) (increase(stellarindex_admin_audit_write_failures_total[15m]))`.
   Zero = the store recovered and this is now purely a recovery task.

## Remediation

**Do the recovery first — it is the part with a deadline.**

1. **Reconstruct the missing rows from the application logs.** Every site
   logs the target id alongside the failure (`minted_key_id`,
   `account_id`, `notice_id`, `event_id`). Together with the request log
   line (actor key id, identifier, `X-Reason`) that is enough to
   reconstitute the audit entry. Do this before the journal's retention
   window closes. For `staff_customer_lookup` the paired line is
   `staff customer lookup: audit append failed (best-effort)` and carries
   `actor` (the staff email) + `account_id` — the two fields the missing
   row exists to record.
2. **Fix the store.** Postgres availability / disk / permissions per the
   cause found above. `SELECT count(*) FROM audit_log WHERE created_at >
   now() - interval '1 day';` confirms writes are landing again.
3. **Confirm silence.** The alert clears once no failure has been counted
   for an hour.

## Do NOT

- **Do not roll back the mutation.** The change committed successfully;
  only its record is missing. Reverting a tier override or revoking a
  minted key to "clean up" would be an unaudited mutation of its own.
- **Do not make the audit append blocking** to "fix" this. That turns an
  audit-store blip into a failed admin operation and — on the Stripe path
  — into a webhook retry storm that re-applies the same plan change.
  Best-effort is deliberate; this alert exists so the gap is *visible*.

## Related

- [monthly-quota-fail-open](monthly-quota-fail-open.md) — the other
  best-effort path whose silence the same audit wave closed.
- [stripe-dead-letter](stripe-dead-letter.md) — fires when a paid Stripe
  event provisioned nothing; `surface="stripe_dead_letter"` here means
  even *that* conclusion went unrecorded.
- [stripe-platform-sync-errors](stripe-platform-sync-errors.md) — the
  adjacent Stripe-bridge degradation signal.
