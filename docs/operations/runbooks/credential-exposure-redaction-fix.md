---
title: Runbook — credential-exposure redaction fix
last_verified: 2026-11-21
status: ratified
severity: P2
---

# Runbook — credential-exposure redaction fix

## At a glance

A credential was exposed in output or logs; fix the redaction, rotate the credential.

Manual trigger (no metric exists) whenever a redaction-class fix ships: a change that stops a credential (API key, session/bearer token, DSN password) or PII (email, phone) from being written in full to a log store. Find them by grepping PRs/commits for "redact", "log" plus a credential or PII field name. Declared per [`sev-playbook.md` §6.5](../sev-playbook.md#65-credentialpii-exposure-incidents). P2: no ongoing customer impact, but past exposure with unresolved rotation/notification. Owner: the engineer who lands the fix, or the IC under an active SEV. ~1-2 h for the scan and record. Skipped, the exposed credentials stay valid and nobody is told, which is worse than the original bug because it now looks closed.

## Steps

1. Identify the exposure window: first deploy that logged the value unredacted to the fix's deploy time. If it was always logged, the window starts when the log field existed (`git log --follow` on the call site).
2. Scan the log store's full retention window for the value (e.g. a targeted LogQL query counting matches for `email=` or the header name), and the host journal for the same window (`journalctl -u <unit> --since <window start>`), since journald can outlive or predate Loki. Zero matches: it rolled off; record the scanned line count. Non-zero: a targeted purge of those lines is needed before closing; no Loki purge procedure exists yet, so escalate.
3. File the incident record under `internal/incidents/data/` per the template, with the scan result in the timeline. Required even when the scan is clean.
4. Classify and rotate/notify:
   - API key: identify every subject authenticated in the window (usage rollup / access logs, scoped to the route or header). Tell each what to do and why; a revoke takes every key minted from the revoked one with it, so a replacement minted from the exposed key (`POST /v1/account/keys`) dies with it. An exposed child key: revoke it with `DELETE /v1/account/keys/{key_id}`, authenticated with its minter or itself. An exposed root key: first get an independent root (a dashboard user mints one with `POST /v1/dashboard/keys` or on the Keys page; a `/v1/register`-only account has no self-service way, so an operator mints one for the same identifier with `POST /v1/admin/keys` or `stellarindex-ops mint-key`, with a reason, and re-applies any staff limit override), move clients over, then revoke the exposed root with itself (`DELETE /v1/account/keys/{key_id}` sent with that key, 204). Do not force-revoke without notice unless misuse shows.
   - Session token: revoking an API key does not touch sessions. Revoke the affected users' sessions now, without waiting for misuse: a replayed cookie stays valid up to 30 days (7 idle) and active use keeps it alive. No operator route does this short of closing the account, so run the same statement the store's `RevokeAllUserSessions` runs, for each user id found in the window, in a transaction and check the row count:

     ```sql
     BEGIN;
     UPDATE sessions SET revoked_at = now() WHERE user_id = '<user_id>' AND revoked_at IS NULL;
     -- UPDATE n must match the user's open sessions. If not, ROLLBACK; otherwise COMMIT.
     COMMIT;
     ```

     Revoking sessions does not remove what a session created: API keys, webhooks and passkeys outlive it, and a passkey can mint a fresh session at will. List everything each user did in the window:

     ```sql
     SELECT ts, action, target_kind, target_id, ip
       FROM audit_log
      WHERE actor_user_id = '<user_id>'
        AND action IN ('key.mint', 'webhook.create', 'webhook.update',
                       'webhook.rotate_secret', 'passkey.register')
        AND ts >= '<window start>'::timestamptz
      ORDER BY ts;
     ```

     The audit write is best-effort, so also list the user's `webauthn_credentials` and the account's `customer_webhooks` and keys created since the window start. Tell the user, with that list. Every row they do not recognise must be removed by them, signed in fresh: `DELETE /v1/dashboard/keys/{id}`, `DELETE /v1/dashboard/webhooks/{id}`, `DELETE /v1/auth/passkey/credentials/{id}` (or the dashboard). There is no operator command that revokes another user's keys, webhooks or passkeys; if the user cannot act promptly and the list is not empty, escalate rather than editing those tables by hand. Do not close the record while an unrecognised row remains.
   - DSN password or other internal secret: rotate it in this cycle, before the record is closed, following [credential-rotation.md](../credential-rotation.md) (e.g. `postgres_pass_stellarindex` and `postgres_pass_stellarindex_api` under step 4 of the full batch). There is no notice-and-offer path: the credential is internal and valid until rotated.
   - PII: notify affected customers via the incident-comms path (sev-playbook §5.3): what was written to an internal log store, which staff can read it, for how long. Regulator notification is decided by [`sev-playbook.md` §6.6](../sev-playbook.md#66-personal-data-breach-notification-assessment); its 72-hour clock started when the exposure was found.
5. Update the record's timeline with the completion time and who/what was notified (aggregate count only, never identities in the customer-facing post).

## Related

[`sev-playbook.md` §6.5](../sev-playbook.md#65-credentialpii-exposure-incidents), [`internal/incidents/data/_template.md`](../../../internal/incidents/data/_template.md), `internal/api/v1/account.go` (`AccountStore.RevokeKeyByID`)
