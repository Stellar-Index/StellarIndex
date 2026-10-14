---
title: Runbook — credential-exposure redaction fix
last_verified: 2026-09-24
status: ratified
severity: P2
---

# Runbook — credential-exposure redaction fix

## At a glance

A credential was exposed in output or logs; fix the redaction, rotate the credential.

Manual trigger (no metric exists) whenever a redaction-class fix ships: a change that stops a credential (API key, session/bearer token, DSN password) or PII (email, phone) from being written in full to a log store. Find them by grepping PRs/commits for "redact", "log" plus a credential or PII field name. Declared per [`sev-playbook.md` §6.5](../sev-playbook.md#65-credentialpii-exposure-incidents). P2: no ongoing customer impact, but past exposure with unresolved rotation/notification. Owner: the engineer who lands the fix, or the IC under an active SEV. ~1-2 h for the scan and record. Skipped, the exposed credentials stay valid and nobody is told, which is worse than the original bug because it now looks closed.

## Steps

1. Identify the exposure window: first deploy that logged the value unredacted to the fix's deploy time. If it was always logged, the window starts when the log field existed (`git log --follow` on the call site).
2. Scan the log store's full retention window for the value (e.g. a targeted LogQL query counting matches for `email=` or the header name). Zero matches: it rolled off; record the scanned line count. Non-zero: a targeted purge of those lines is needed before closing; no Loki purge procedure exists yet, so escalate.
3. File the incident record under `internal/incidents/data/` per the template, with the scan result in the timeline. Required even when the scan is clean.
4. Classify and rotate/notify:
   - API key / session token: identify every subject authenticated in the window (usage rollup / access logs, scoped to the route or header). Offer each a rotated key (`POST /v1/account/keys` to mint, `DELETE /v1/account/keys/{key_id}` (`AccountStore.RevokeKeyByID`) to revoke the exposed one) and say why. Do not force-revoke without notice unless misuse shows.
   - PII: notify affected customers via the incident-comms path (sev-playbook §5.3): what was written to an internal log store, which staff can read it, for how long. Regulator notification is decided by [`sev-playbook.md` §6.6](../sev-playbook.md#66-personal-data-breach-notification-assessment); its 72-hour clock started when the exposure was found.
5. Update the record's timeline with the completion time and who/what was notified (aggregate count only, never identities in the customer-facing post).

## Related

[`sev-playbook.md` §6.5](../sev-playbook.md#65-credentialpii-exposure-incidents), [`internal/incidents/data/_template.md`](../../../internal/incidents/data/_template.md), `internal/api/v1/account.go` (`AccountStore.RevokeKeyByID`)
