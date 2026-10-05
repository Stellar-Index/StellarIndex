---
title: Runbook — account erasure and data export
last_verified: 2026-09-28
status: draft
severity: P3
---

# Runbook — account erasure and data export

## At a glance

| Field | Value |
| ----- | ----- |
| Trigger | A customer asks for their account to be deleted or exported outside the dashboard, a dashboard erasure answered 409, or an erasure's Redis cleanup did not finish |
| Severity | P3 (a request with a legal response deadline, not an outage) |
| Detected by | Support request; `account erasure failed` / `account erased notice not sent` in the API log; `stellarindex_notify_sends_total{template="account-erased",result="failed"}` |
| Typical MTTR | 10 min |
| Impact | None to other customers. A missed or partial erasure keeps one customer's data. |

The code path is GH #809: `internal/accounterasure`, used by both
`DELETE /v1/dashboard/account` and `stellarindex-ops account-erase`.

## Self-service first

An owner can do both from the dashboard: `GET /v1/dashboard/account/export`
downloads everything, `DELETE /v1/dashboard/account` erases the account.
Both need a session created within the last 10 minutes; an older one gets
`reauth-required` and the owner signs in again. Point the customer there
before running anything by hand.

## What an erasure removes, and what it keeps

One Postgres transaction closes the account and deletes its members
(sessions and passkeys cascade), API keys, webhooks and their deliveries,
price alerts, invites it sent or that were addressed to any member, sign-in
tokens and code lockouts for the members' addresses, `api_usage_events`,
and the account row. A failure rolls all of it back, so the owner can retry.

Kept, unlinked:

- **`audit_log`** rows stay (0179/0188). Their `metadata` loses
  `account_slug`, `target_identifier`, `actor_identifier`, `label`, `name`,
  `suspended_reason` and `email`; customer rows lose `ip` and
  `user_agent`. **Staff rows keep the staff member's address and agent.**
  One `account.erase` row records the counts, with the old account uuid as
  `target_id` and nothing else.
- **`usage_daily`** rows are renamed to one fresh `erased:<uuid>` subject
  and age out under the 12-month retention policy (0167).
- **`erased_account_slugs`** keeps `sha256(slug)`, so the slug is never
  reissued. An unsalted hash of a slug is pseudonymous: someone holding
  the table can confirm a guessed slug.

After the commit the eraser deletes the account's Redis API-key records,
the Postgres keys' validator records and cache rows, and every usage
counter, then does it a second time, then re-runs the usage rename.
Rate-limit counters (`rl:*`) and touch-debounce keys (`touch:apikey:*`)
expire within minutes and are left to their TTL.

## Operator erasure

For a request received by email, or a dashboard erasure that answered 409:

```sh
stellarindex-ops account-erase -config /etc/stellarindex.toml -account-id <uuid>
stellarindex-ops account-erase -config /etc/stellarindex.toml -account-id <uuid> -write
```

The first line is a dry run and prints member and key counts. Find the
uuid from the address the request came from, in an interactive `psql`
session on r1 so the address stays out of shell history, tickets and logs:

```sql
SELECT account_id FROM users WHERE email = '<address>';
```

The operator path sends no confirmation mail; reply to the request
yourself. The `account.erase` row records `actor_kind = staff`.

### 409 `account-erasure-blocked`

The account has billing state (`stripe_customer_id` or a `subscriptions`
row) or a staff member. Billing has no writer on main, so billing state
means someone set it by hand: delete the Stripe customer in the Stripe
dashboard, clear the column and the rows, then re-run. A staff member is
moved to another account (or has `is_staff` cleared) before the erasure,
so no staff audit trail is scrubbed.

## Finishing a Redis cleanup

If the API log shows `account erasure: redis pass` or `post-commit usage
rename` errors, the Postgres erasure committed and Redis still holds keys
or counters. They cannot authenticate (the account is gone and the slug is
tombstoned), but they are the customer's data:

```sh
stellarindex-ops account-erase -config /etc/stellarindex.toml -finish-slug <slug> -write
```

It refuses a slug that was never erased. The slug is in the error line's
context or the customer's request; it is not recoverable from Postgres.

## Copies outside the live database

These are the facts an erasure cannot reach. They were measured on r1
2026-09-28; re-measure before quoting any of them to a customer.

| Store | Holds | Expiry today |
| ----- | ----- | ------------ |
| pgBackRest repo1 (local) | full database, unencrypted (`cipher-type=none`) | `repo1-retention-full=2`: up to ~15 days after the erasure |
| pgBackRest repo2 (S3, `aes-256-cbc`) | full database | full/diff expire in ~14 days, but **WAL expiry is unset on r1** (`repo2-retention-archive-type=diff`, no `repo2-retention-archive`); the fix in `configs/ansible` `pgbackrest.conf.j2` is not applied, so WAL holding the deleted rows is kept indefinitely |
| ZFS `auto-*` snapshots of `postgres` | full database, dataset `encryption=off` | 3 days; manual snapshots are never pruned — check `zfs list -t snapshot` |
| Redis RDB dump | Redis keyspace | replaced at the next `save` point (at most ~1 hour) |
| API process memory | idempotency cache of a replayed `DELETE` response; validator account-status cache | 10 minutes; 30 seconds |
| Loki | API logs with `account_id` (emails are masked) and Caddy access logs with client IPs | 720 hours |
| journald | the same, on the host | `MaxRetentionSec=14d`, `SystemMaxUse=500M` |
| Resend | the confirmation email and past sign-in emails | Resend's retention, outside our control |

Until repo2's WAL expiry is applied, do not tell anyone that backups
expire.

## After a point-in-time restore

A restore to a time before an erasure brings the account back. Before
restoring, save the erased account ids from the database being replaced:

```sh
psql "$STELLARINDEX_POSTGRES_DSN" -At -c "SELECT target_id FROM audit_log WHERE action = 'account.erase'" > erased-accounts.txt
```

If that database is gone, take the ids from the `account erased` log lines
in Loki (`account_id`). After the restore, and before the API serves, run
`stellarindex-ops account-erase -config /etc/stellarindex.toml -account-id <uuid> -write`
for each id that exists again. Redis is not restored with Postgres, so its
half of the erasure already happened.

## Related

- [`privacy-rights-requests.md`](privacy-rights-requests.md) — identity checks and every other rights request (access, correction, restriction, objection, single-member erasure)
- [`docs/architecture/platform-spec.md` §8.3](../../architecture/platform-spec.md) — the built behaviour
- [`migrations/0188_account_erasure.up.sql`](../../../migrations/0188_account_erasure.up.sql) — the audit exception and the slug tombstone
- [`admin-audit-write-failing.md`](admin-audit-write-failing.md) — the audit sink the export and staff rows depend on
- [`backup-offsite-stale.md`](backup-offsite-stale.md) — repo2 health
