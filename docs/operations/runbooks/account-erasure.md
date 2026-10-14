---
title: Runbook — account erasure and data export
last_verified: 2026-09-28
status: draft
severity: P3
---

# Runbook — account erasure and data export

## At a glance

P3. Erasure or export outside the dashboard.

Trigger: a customer asks for deletion or export outside the dashboard, a dashboard erasure answered 409, or an erasure's Redis cleanup did not finish. Signals: `account erasure failed`, `account erased notice not sent`, `stellarindex_notify_sends_total{template="account-erased",result="failed"}`. Code: `internal/accounterasure`, used by `DELETE /v1/dashboard/account` and `stellarindex-ops account-erase`.

## Self-service first

An owner can export (`GET /v1/dashboard/account/export`) and erase (`DELETE /v1/dashboard/account`) from the dashboard. Both need a session created within 10 minutes (older gets `reauth-required`; sign in again).

## What an erasure removes and keeps

One Postgres transaction closes the account and deletes members (sessions and passkeys cascade), API keys, webhooks and deliveries, price alerts, invites it sent or addressed to any member, sign-in tokens and code lockouts for member addresses, `api_usage_events`, and the account row. A failure rolls back; retry.

Kept, unlinked:

- `audit_log` rows: `metadata` loses `account_slug`, `target_identifier`, `actor_identifier`, `label`, `name`, `suspended_reason`, `email`; customer rows lose `ip` and `user_agent`. Staff rows keep the staff member's address and agent. One `account.erase` row records counts with the old account uuid as `target_id`.
- `usage_daily` is renamed to one fresh `erased:<uuid>` subject and ages out under the 12-month policy (0167).
- `erased_account_slugs` keeps `sha256(slug)` so the slug is never reissued.

After the commit the eraser deletes the account's Redis API-key records, the validator records and cache rows of its Postgres keys, and every usage counter, twice, then re-runs the usage rename.

## Operator erasure

For an emailed request, or a dashboard erasure that answered 409:

```sh
stellarindex-ops account-erase -config /etc/stellarindex.toml -account-id <uuid>
stellarindex-ops account-erase -config /etc/stellarindex.toml -account-id <uuid> -write
```

The first is a dry run (prints member and key counts). Find the uuid in an interactive `psql` session on r1 so the address stays out of history, tickets and logs:

```sql
SELECT account_id FROM users WHERE email = '<address>';
```

No mail is sent; reply yourself. The `account.erase` row records `actor_kind = staff`.

### 409 `account-erasure-blocked`

The account has billing state (`stripe_customer_id` or a `subscriptions` row) or a staff member. Billing has no writer on main, so it was set by hand: delete the Stripe customer in the Stripe dashboard, clear the column and rows, re-run. Move a staff member to another account (or clear `is_staff`) first, so no staff audit trail is scrubbed.

## Finishing a Redis cleanup

If the API log shows `account erasure: redis pass` or `post-commit usage rename` errors, Postgres committed but Redis still holds keys or counters (unusable, but the customer's data):

```sh
stellarindex-ops account-erase -config /etc/stellarindex.toml -finish-slug <slug> -write
```

It refuses a slug that was never erased. The slug is in the error line or the request; it is not recoverable from Postgres.

## Copies outside the live database

Measured on r1; re-measure before quoting.

| Store | Holds | Expiry today |
| ----- | ----- | ------------ |
| pgBackRest repo1 (local) | full database, unencrypted (`cipher-type=none`) | `repo1-retention-full=2`: up to ~15 days |
| pgBackRest repo2 (S3, `aes-256-cbc`) | full database | full/diff ~14 days, but **WAL expiry is unset on r1** (`repo2-retention-archive-type=diff`, no `repo2-retention-archive`; the `configs/ansible` `pgbackrest.conf.j2` fix is not applied), so WAL holding deleted rows is kept indefinitely |
| ZFS `auto-*` snapshots of `postgres` | full database, `encryption=off` | 3 days; manual snapshots never pruned (`zfs list -t snapshot`) |
| Redis RDB dump | Redis keyspace | next `save` (<= ~1 hour) |
| API process memory | replayed `DELETE` idempotency cache; validator status cache | 10 minutes; 30 seconds |
| Loki | API logs with `account_id` (emails masked), Caddy logs with client IPs | 720 hours |
| journald | same, on host | `MaxRetentionSec=14d`, `SystemMaxUse=500M` |
| Resend | confirmation and past sign-in emails | Resend's retention |

Until repo2 WAL expiry is applied, never say backups expire.

## After a point-in-time restore

A restore to before an erasure brings the account back. Before restoring, save the erased ids from the database being replaced:

```sh
psql "$STELLARINDEX_POSTGRES_DSN" -At -c "SELECT target_id FROM audit_log WHERE action = 'account.erase'" > erased-accounts.txt
```

If that database is gone, take ids from the `account erased` log lines in Loki (`account_id`). After the restore, before the API serves, run `stellarindex-ops account-erase -config /etc/stellarindex.toml -account-id <uuid> -write` for each id that exists again. Redis is not restored with Postgres.

## Related

[`privacy-rights-requests.md`](privacy-rights-requests.md), [`migrations/0188_account_erasure.up.sql`](../../../migrations/0188_account_erasure.up.sql), [`api.md#stellarindex_admin_audit_write_failing`](api.md#stellarindex_admin_audit_write_failing), [`backup-offsite.md#stellarindex_backup_offsite_stale`](backup-offsite.md#stellarindex_backup_offsite_stale)
