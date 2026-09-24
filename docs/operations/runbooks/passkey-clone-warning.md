---
title: Runbook — passkey-clone-warning
last_verified: 2026-09-23
status: draft
severity: P3
---

# Runbook — `stellarindex_passkey_clone_warning`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_passkey_clone_warning` (P3 / ticket) |
| Detected by | Prometheus rule in `deploy/monitoring/rules/api.yml` and the R1 single-host overlay `configs/prometheus/rules.r1/api.yml`. |
| Typical MTTR | 15–60 min to contact the account owner. |
| Impact | **One account's passkey may have been copied.** The sign-in that tripped the check was refused; no session was issued. |

## Why this exists

A passkey assertion carries a signature counter that a genuine
authenticator only ever increases. `POST /v1/auth/passkey/finish-login`
refuses an assertion whose signature verifies but whose counter is at
or below the stored one: two copies of the private key are signing
independently. That branch used to write only a log line. It now
increments `stellarindex_passkey_login_refusals_total{reason="clone_warning"}`
and appends a `passkey.clone_warning` row to `audit_log`.

Authenticators that report a counter of 0 forever (iCloud and most
synced passkeys) are exempt from the check, so this alert can only
come from a hardware or device-bound key.

## Quick diagnosis (≤ 5 min)

```sql
-- Which account and credential, from where.
SELECT ts, account_id, target_id AS credential_row_id, ip, user_agent, metadata
FROM audit_log
WHERE action = 'passkey.clone_warning'
ORDER BY ts DESC
LIMIT 20;
```

`metadata` carries `credential_owner_user_id`, `credential_name`,
`stored_sign_count` and `presented_sign_count`. The same account's
`passkey.register`, `passkey.delete` and `passkey.login_replay` rows
show when that credential was added and whether it has been presented
unusually:

```sql
SELECT ts, action, actor_kind, actor_user_id, target_id, ip, metadata
FROM audit_log
WHERE account_id = '<account_id>' AND action LIKE 'passkey.%'
ORDER BY ts DESC;
```

## Mitigation (≤ 15 min)

- [ ] Contact the account owner (the user named by
      `credential_owner_user_id`) and ask them to remove the named
      passkey from the dashboard's security settings and register a
      new one.
- [ ] If the owner cannot be reached and the IPs look hostile, delete
      the credential row (`webauthn_credentials.id = target_id`) and
      revoke the user's sessions. Both are account changes: record who
      did them and why.
- [ ] Verification: no new `passkey.clone_warning` rows for that
      credential. The alert clears an hour after the last refusal.

## Do NOT

- **Do not treat this as a platform outage.** The refusal is the
  control working; one account is affected.
- **Do not "fix" it by resetting the stored sign count.** That hides
  the signal and lets the copied key sign in.

## Related

- [admin-audit-write-failing](admin-audit-write-failing.md) — fires
  with `surface="passkey_clone_warning"` when this event's audit row
  failed to land, which leaves the metric as the only record.
- [ADR-0049](../../adr/0049-anonymous-access-and-passkey-auth.md) —
  passkey sign-in design.
