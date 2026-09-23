---
title: Runbook — credential-exposure redaction fix
last_verified: 2026-09-21
status: ratified
severity: P2
---

# Runbook — credential-exposure redaction fix

Triggered whenever a **redaction-class fix** ships: a change that
stops a credential (API key, session/bearer token, DSN password) or
PII value (email, phone) from being written to a log store it was
previously reaching in full. `7843f129` (customer emails and
`X-API-Key` unredacted in Caddy access logs) is the reference case.
Not a Prometheus alert — there is no metric for "a secret used to be
in the logs" — so this is a manual trigger off the PR/commit itself,
declared per
[`sev-playbook.md` §6.5](../sev-playbook.md#65-credentialpii-exposure-incidents).

## At a glance

| Field | Value |
| ----- | ----- |
| Trigger | A redaction-class fix merges (grep the PR/commit for "redact", "log", plus a credential or PII field name) |
| Severity | P2 — no ongoing customer impact, but a confirmed past exposure with unresolved rotation/notification legs |
| Owner | The engineer who lands the redaction fix, or the incident commander if it shipped under an active SEV |
| Typical time | 1–2 h for the retention scan + incident record; rotation/notification timeline depends on the affected population |
| Impact if skipped | The log store stops leaking going forward, but any credential already exposed during the window stays valid and nobody who saw it is told to stop using it — the disclosed-but-unrotated state is worse than the original bug once the fix ships, because it now looks closed |

## Steps

1. **Identify the exposure window.** First deploy that started
   writing the value unredacted → the redaction fix's deploy time.
   If the value was ALWAYS logged (no regression, just never
   redacted), the window is "since that log field existed" —
   check `git log --follow` on the logging call site.

2. **Scan the log store's full retention window for the value.**
   Loki example (mirrors the verification already done for
   `7843f129`, see `CHANGELOG.md`): a targeted LogQL query across
   the retention period, counting matches for the field pattern
   (`email=`, the header name, etc). Zero matches means the value
   has already rolled off naturally — record the scanned line
   count and the result. A non-zero match means a **targeted
   purge** of those log lines is needed before this runbook is
   closed (see `docs/operations/runbooks/` for the Loki purge
   procedure, or escalate — there is none yet if this is the first
   time it's needed).

3. **File the incident record** under `internal/incidents/data/`
   per the template — the retention scan result goes in the
   timeline. This is required even when the scan comes back clean;
   "clean" is a finding about the log store, not permission to skip
   the record.

4. **Classify what was exposed and rotate/notify accordingly:**
   - **API key / session token:** identify every subject
     authenticated during the exposure window (usage rollup /
     access logs, scoped to the affected route or header). Offer
     each one a rotated key — the self-service path is `POST
     /v1/account/keys` to mint a replacement and `DELETE
     /v1/account/keys/{key_id}` (`AccountStore.RevokeKeyByID`) to
     revoke the exposed one — and notify them of why. Do not
     force-revoke without notice unless the key shows signs of
     misuse; forcing rotation on a live production key without
     warning breaks the customer's integration.
   - **PII (email, phone, etc):** notify the affected customers per
     the incident-comms path (§5.3 of the sev-playbook) that their
     data was written to an internal log store the given class of
     staff can read, what staff access that log store, and how long
     the exposure lasted.

5. **Close the loop in the incident record.** Once rotation/
   notification is sent, update the record's timeline with the
   completion timestamp and who/what was notified (aggregate count,
   never customer identities in the customer-facing post).

## Related

- [`sev-playbook.md` §6.5](../sev-playbook.md#65-credentialpii-exposure-incidents) — when this runbook is mandatory.
- [`internal/incidents/data/_template.md`](../../../internal/incidents/data/_template.md) — incident-record template.
- `internal/api/v1/account.go` (`AccountStore.RevokeKeyByID`) — the self-service key-revocation path customers use to rotate.
