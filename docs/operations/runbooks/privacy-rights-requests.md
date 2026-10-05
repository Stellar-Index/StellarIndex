---
title: Runbook — privacy rights requests
last_verified: 2026-10-02
status: draft
severity: P3
---

# Runbook — privacy rights requests

## At a glance

| Field | Value |
| ----- | ----- |
| Trigger | An email to security@stellarindex.io asking to see, correct, export, restrict, close or erase personal data, objecting to processing, or complaining about how a request was handled |
| Severity | P3 (a legal response deadline, not an outage) |
| Detected by | The security@ mailbox |
| Deadline | Acknowledge within 72 hours and answer within one month, as `/privacy` §8 promises. A complex or repeated request may take two more months only if the requester is told why within the first month. No fee. |
| Impact | One person's data. A wrong identity check discloses or destroys someone else's. |

Retention is decided: account data and audit-log entries are kept
indefinitely, and an erasure pseudonymises what it keeps instead of
deleting it. Answer every request against that and against the public
wording in `web/explorer/src/app/privacy/page.tsx`; if the two disagree,
fix the page or the code, not the reply.

Keep addresses out of shell history, tickets and logs: run every lookup
below in an interactive `psql` (or `redis-cli`) session on r1. The mailbox
thread is the record of the request.

## 1. Who is asking, and for what

**Never ask for an API key, a sign-in link or code, a session cookie or a
passkey.** None is needed, and a requester who sends one has leaked it:
revoke it (`DELETE /v1/admin/keys/{keyID}`) and tell them.

Pick the route by what the account holds:

- **Dashboard account (has a `users` row).** The request must come from
  that row's `email`. A request from anywhere else gets one reply asking
  them to write from the address on the account, and nothing more: no
  confirmation that an account exists.
  ```sql
  SELECT u.id, u.account_id, u.role, a.slug, a.status
    FROM users u JOIN accounts a ON a.id = u.account_id
   WHERE u.email = '<address>';
  ```
- **`POST /v1/register` account (no `users` row).** Its `billing_email` is
  unverified and proves nothing. Use the possession route: send a random
  one-time string, ask for the `key_id` (not the key) and one request made
  with the key and that string as its user agent, e.g.
  `curl -A '<string>' -H 'Authorization: Bearer <their key>' https://api.stellarindex.io/v1/account/me`.
  Then check:
  ```sql
  SELECT account_id, last_used_at FROM api_keys
   WHERE id = '<key_id>' AND revoked_at IS NULL AND last_used_user_agent = '<string>';
  ```
  The last-use write is skipped within 5 minutes of the key's previous
  one; if no row matches, ask them to pause other traffic on the key for
  5 minutes and repeat.
- **`POST /v1/signup` key (no account).** The only record is a hash of the
  address. The request must come from that address.

**Shared accounts.** Only an `owner` may act for the whole account (export,
restriction, closure, erasure, account-name correction); the dashboard
enforces the same rule. Any member may act for their own data: their
`users` row, sessions, passkeys and the audit entries they caused. A
member asking for the whole account is answered for their own data and
told to ask an owner.

## 2. Access and portability

- **Owner:** point them at `GET /v1/dashboard/account/export` (signed in
  within 10 minutes); it is the complete account record as JSON. Add by
  hand what it leaves out if they ask: the `/v1/signup` hash (below) and
  that cache counters and expired sign-in tokens exist.
- **Member, or a request by email:** assemble their rows and reply with
  them. Leave out `sessions.token_hash` and passkey public keys (secrets,
  as in the export).
  ```sql
  SELECT id, email, display_name, role, email_verified_at, last_login_at, mfa_enabled, created_at
    FROM users WHERE id = '<user_id>';
  SELECT id, created_at, last_seen_at, expires_at, revoked_at, ip_first_seen, ip_last_seen, user_agent,
         geo_first_seen, geo_last_seen
    FROM sessions WHERE user_id = '<user_id>';
  SELECT id, name, encode(aaguid, 'hex') AS aaguid, transports, backup_eligible, backup_state,
         created_at, last_used_at
    FROM webauthn_credentials WHERE user_id = '<user_id>';
  SELECT action, target_kind, target_id, metadata, ip, user_agent, ts
    FROM audit_log WHERE actor_user_id = '<user_id>' ORDER BY ts;
  ```
- **`/v1/signup` hash:** in `psql`,
  `SELECT encode(sha256(convert_to(lower('<address>'), 'UTF8')), 'hex');`,
  then in `redis-cli`, `GET signup:email:<hash>`. The value is the key id
  minted for that address.

## 3. Correction

There is no self-service edit, so correct in `psql`, one row, inside a
transaction, and quote the before and after values back in the reply.

- `users.display_name` and `accounts.name`: update directly (account name:
  owner only).
- `users.email` is the sign-in identity. Change it only after the request
  comes from the old address **and** a reply from the new address confirms
  it, so the new address cannot be a third party's. The unique index on
  `users.email` refuses an address another account already holds.
- `accounts.billing_email` on a register account: possession route, then
  update.
- `accounts.slug` is not corrected in place: usage counters are keyed on
  it and an erased slug is tombstoned. Offer erasure and a new account.

## 4. Restriction and objection

**Restriction** (keep the data, stop using it) is account suspension:

```sh
curl -X PATCH https://api.stellarindex.io/v1/admin/accounts/<account_id> \
  -H "Authorization: Bearer $OPERATOR_KEY" -H 'X-Reason: rights request: restriction' \
  -H 'Content-Type: application/json' -d '{"status":"suspended","suspended_reason":"restriction requested"}'
```

Every key and dashboard session of the account is refused until the
status returns to `active` by the same call. Tell the requester that a
suspended account cannot be used. Keep personal data out of `X-Reason` and
`suspended_reason`: both are written to the audit log.

**Objection** covers only the legitimate-interests processing listed in
`/privacy` §3. Answer each item asked about:

- Audit log, request logs, rate-limit and invalid-key counters, sign-in
  request records, CDN edge logs: keep, on compelling grounds (the security
  of the service and of their own account), and say so with the retention
  from `/privacy` §6.
- The `/v1/signup` hash: delete it (`DEL signup:email:<hash>`) and revoke
  the key it names with `DELETE /v1/admin/keys/{keyID}`. The address can
  then request a key again.
- The sign-in device cookie: they can clear it in their browser.

## 5. Closure and erasure

- **Closure** keeps the records indefinitely: `PATCH` as above with
  `{"status":"closed"}`. It is terminal and revokes every key and session.
  Use it only when the owner asks to stop using the account but not to be
  forgotten; "delete my account" means erasure.
- **Erasure of a whole account** (owner): self-service or operator, per
  [`account-erasure.md`](account-erasure.md). Its "Copies outside the live
  database" table is what the reply may say about backups and logs.
- **Erasure of one member of a shared account**: there is no command, so
  in one `psql` transaction. The count must be at least 1: if it is 0 the
  member is the account's last `owner`, so `ROLLBACK` and offer erasure of
  the whole account instead.
  ```sql
  BEGIN;
  SELECT count(*) FROM users
    WHERE account_id = '<account_id>' AND role = 'owner' AND id <> '<user_id>';
  -- 0 → ROLLBACK; and stop.
  DELETE FROM invites WHERE invited_by_user_id = '<user_id>' OR lower(email::text) = lower('<address>');
  DELETE FROM magic_link_tokens WHERE lower(email::text) = lower('<address>');
  DELETE FROM login_code_lockouts WHERE lower(email) = lower('<address>');
  DELETE FROM users WHERE id = '<user_id>' AND account_id = '<account_id>';   -- cascades sessions and passkeys
  COMMIT;
  ```
  Their audit entries lose the link to the user row but keep their IP,
  user agent and any address in the details: the audit log only accepts
  the pseudonymising scrub while erasing a whole account. Tell the
  requester that, and that the entries are scrubbed if the account is
  later erased.

## 6. Complaints

A complaint about a handled request gets a review by someone other than
the person who handled it, inside the same one-month deadline. Every
final reply names the right to complain to the ICO (UK) or the
requester's national supervisory authority (EEA), as `/privacy` §8 does.
When a request is closed, delete the thread if the requester asked for
that (`/privacy` §6).

## Related

- [`account-erasure.md`](account-erasure.md) — whole-account erasure and export, and what an erasure cannot reach
- [`api.md#stellarindex_admin_audit_write_failing`](api.md#stellarindex_admin_audit_write_failing) — the audit sink the admin calls above write to
- [`web/explorer/src/app/privacy/page.tsx`](../../../web/explorer/src/app/privacy/page.tsx) — the public promise this runbook carries out
- [`migrations/0188_account_erasure.up.sql`](../../../migrations/0188_account_erasure.up.sql) — the only audit-log rewrite the database permits
