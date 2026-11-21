---
title: Runbook — privacy rights requests
last_verified: 2026-11-20
status: draft
severity: P3
---

# Runbook — privacy rights requests

## At a glance

P3. Answer within one month.

Trigger: an email to security@stellarindex.io asking to see, correct, export, restrict, close or erase personal data, objecting to processing, or complaining about a request. P3 (a legal deadline, not an outage). Deadline: acknowledge within 72 hours, answer within one month (two more only if the requester is told why in the first month), no fee (`/privacy` §8).

Retention is decided: account data and audit-log entries are kept indefinitely; an erasure pseudonymises what it keeps. Answer against that and the public wording in `web/explorer/src/app/privacy/page.tsx`; if they disagree, fix the page or code, not the reply. Run every lookup in an interactive `psql`/`redis-cli` session on r1 (addresses stay out of history and logs).

## 1. Who is asking

Never ask for an API key, sign-in link or code, session cookie or passkey. If one is sent, revoke it and tell them. A plaintext key's id is `SELECT id, account_id FROM api_keys WHERE key_hash = sha256(convert_to('<key>', 'UTF8'))` (run it in the interactive session); then revoke it with the operator key: `curl -X DELETE 'https://api.stellarindex.io/v1/admin/keys/<key_id>?identifier=acct:<account slug>' -H "Authorization: Bearer $OPERATOR_KEY" -H 'X-Reason: rights request: credential sent by email'` (400 without `identifier` or `X-Reason`, 404 if the identifier is not the key's owner).

- **Dashboard account (has a `users` row).** The From header alone proves nothing. Look the row up, then send a random one-time string to that row's `email` (never to the From or Reply-To address of the request) and act only on a reply from that address that returns the string. A request whose headers show no DKIM, SPF or DMARC pass is unverified until the challenge is returned. Otherwise one reply asking them to write from the account's address, confirming nothing. This challenge precedes every disclosure, correction, restriction, closure and erasure below.
  ```sql
  SELECT u.id, u.account_id, u.role, a.slug, a.status
    FROM users u JOIN accounts a ON a.id = u.account_id
   WHERE u.email = '<address>';
  ```
- **`POST /v1/register` account (no `users` row).** `billing_email` is unverified. Possession route: send a random one-time string, ask for the `key_id` (not the key) and one request made with the key and that string as user agent, e.g. `curl -A '<string>' -H 'Authorization: Bearer <their key>' https://api.stellarindex.io/v1/account/me`. Then check (the last-use write is skipped within 5 minutes of the previous one):
  ```sql
  SELECT account_id, last_used_at FROM api_keys
   WHERE id = '<key_id>' AND revoked_at IS NULL AND last_used_user_agent = '<string>';
  ```
- **`POST /v1/signup` key (no account).** The only record is a hash of the address; the request must come from that address.

Shared accounts: only an `owner` may act for the whole account (export, restriction, closure, erasure, account-name correction). Any member may act for their own data (their `users` row, sessions, passkeys, audit entries they caused); a member asking for the whole account gets their own data and is told to ask an owner.

## 2. Access and portability

- **Owner:** point them at `GET /v1/dashboard/account/export` (signed in within 10 minutes), the complete account record as JSON.
- **Member, or by email:** reply with their rows, omitting `sessions.token_hash` and passkey public keys.
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
- **`/v1/signup` hash:** in `psql`, `SELECT encode(sha256(convert_to(lower('<address>'), 'UTF8')), 'hex');`, then in `redis-cli`, `GET signup:email:<hash>` (the value is the key id minted for that address).

## 3. Correction

No self-service edit: correct in `psql`, one row, in a transaction, and quote before and after values in the reply.

- `users.display_name`, `accounts.name` (owner only): update directly.
- `users.email` (the sign-in identity): only after the challenge in section 1 was returned from the old address (the one on the row) AND a reply from the new address confirms it. The new address alone proves nothing.
- `accounts.billing_email` on a register account: possession route, then update.
- `accounts.slug` is never corrected in place: offer erasure and a new account.

## 4. Restriction and objection

Restriction is account suspension:

```sh
curl -X PATCH https://api.stellarindex.io/v1/admin/accounts/<account_id> \
  -H "Authorization: Bearer $OPERATOR_KEY" -H 'X-Reason: rights request: restriction' \
  -H 'Content-Type: application/json' -d '{"status":"suspended","suspended_reason":"restriction requested"}'
```

Every key and dashboard session is refused until the same call sets `active`; tell the requester.

Objection covers only the legitimate-interests processing in `/privacy` §3:

- Audit log, request logs, rate-limit and invalid-key counters, sign-in request records, CDN edge logs: keep, on compelling security grounds, citing the retention in `/privacy` §6.
- `/v1/signup` hash: `DEL signup:email:<hash>` and revoke the key it names with the `DELETE /v1/admin/keys/<key_id>?identifier=…` call in section 1, with `identifier` set to the key's owner (a signup key's owner is `signup-…`, not `acct:`).
- Sign-in device cookie: they can clear it in their browser.

## 5. Closure and erasure

- **Closure** keeps records: `PATCH` as above with `{"status":"closed"}`. Terminal; revokes every key and session. Only when the owner wants to stop using the account but not be forgotten; "delete my account" means erasure.
- **Whole-account erasure** (owner): [`account-erasure.md`](account-erasure.md); its "Copies outside the live database" table is what the reply may say about backups and logs.
- **One member of a shared account:** no command; one `psql` transaction. If the count is 0 the member is the last `owner`: `ROLLBACK` and offer whole-account erasure.
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
  Nothing records this erasure: write `user_id`, `account_id` and the UTC time of the `COMMIT` in the request's ticket (not the address). After a point-in-time restore that predates it, [`account-erasure.md`](account-erasure.md#after-a-point-in-time-restore) re-erases only whole accounts, so re-run this transaction for each recorded member whose `users` row exists again.
  Their audit entries lose the user link but keep IP, user agent and any address in the details (the scrub is only allowed while erasing a whole account). Tell the requester.

## 6. Complaints

A complaint gets a review by someone other than the handler, inside the same one-month deadline. Every final reply names the right to complain to the ICO (UK) or the national supervisory authority (EEA), as `/privacy` §8.

## Related

[`account-erasure.md`](account-erasure.md), [`api.md#stellarindex_admin_audit_write_failing`](api.md#stellarindex_admin_audit_write_failing), [`web/explorer/src/app/privacy/page.tsx`](../../../web/explorer/src/app/privacy/page.tsx), [`migrations/0188_account_erasure.up.sql`](../../../migrations/0188_account_erasure.up.sql)
