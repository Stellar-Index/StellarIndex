---
title: "[SEV-2] Customer emails and API keys written to the log store unredacted — 2026-09-02"
date: 2026-09-02
severity: SEV-2
status: resolved
started_at: 2026-09-02T00:00:00Z
resolved_at: 2026-09-03T00:00:00Z
affected_components:
  - api
postmortem:
---

# [SEV-2] Customer emails and API keys written to the log store unredacted

## Identification

Two of our edge logging paths wrote sensitive values in full to
journald/Loki instead of redacting them:

- `GET /v1/account/admin/lookup?email=...` — the staff email
  look-up route logged the customer's email address in the
  request URL.
- Any request carrying `X-API-Key` — our name for a customer's
  live API key — was logged complete and replayable to anyone
  with Grafana read access, because Caddy's built-in credential
  redaction has no knowledge of that header name.

No customer action was required; this was an internal exposure
into our own log store, not a public leak.

## Impact

- **Endpoints affected:** `GET /v1/account/admin/lookup`, and any
  authenticated request that used `X-API-Key` rather than the
  `Authorization: Bearer` form.
- **Customers affected:** any customer whose email was looked up
  by staff, or who authenticated with `X-API-Key`, while the
  unredacted filter was live.
- **Data correctness:** no data was lost or corrupted; the
  exposure was read-access to log lines by staff with existing
  Grafana access, not a public disclosure.
- **Workaround:** none needed — see remediation below.

## Timeline

All times UTC.

| Time (UTC) | Update |
|-----------|--------|
| 2026-09-02 00:55 | **Identified.** Commit `7843f129` fixes the edge redaction filter (rebuilt allow-list-shaped instead of enumerated) and deletes `X-API-Key` from logged headers. |
| 2026-09-02 | **Monitoring.** Caddy filter and the ansible template regenerated; boot-time credential formatting in `internal/config/validate.go` fixed alongside it (same class of defect). |
| 2026-09-03 | **Resolved.** Loki's 30-day retention window scanned in full (1,069,477 lines): zero unredacted `email=` lines remained, so the exposure window had already rolled off naturally. No targeted purge was necessary. |

## What we did

Commit `7843f129` replaced the enumerated Caddy query-redaction
filter with an allow-list-shaped one (`?<redacted>` on the whole
query string), and added `X-API-Key`, `X-Reason` and `Referer` to
the set of headers stripped before logging. We then verified the
existing 30-day Loki retention window against the fix: zero
unredacted `email=` lines across the full scanned corpus, so nothing
required manual purging.

## Post-exposure remediation

The redaction fix and the retention-window verification closed the
**forward-looking** and **retroactive-scan** legs of this incident.

Operational follow-ups:

- [x] File the customer-facing incident record — this file, filed
      retroactively per
      [`docs/operations/sev-playbook.md` §6.5](../../../docs/operations/sev-playbook.md#65-credentialpii-exposure-incidents).
- [x] **Targeted API-key rotation / customer notification — no
      customer in scope.** Measured on the production database
      2026-10-02: of the keys created before `7843f129`, exactly one
      authenticated a request in the exposure window. It belongs to
      the internal `launch-verify-agent` account (free tier, created
      2026-08-13 by the launch verification run through self-serve
      registration, no billing email, no users) and was used once,
      at creation. No `X-API-Key` customer was active in the window,
      so there is no one to notify. This checkbox is a structural
      closure gate: `scripts/ci/lint-docs.sh` §15 fails CI once this
      incident is 30 days old if it is still unchecked.

## Postmortem

Draft:
[`docs/operations/postmortems/2026-09-02-log-store-credential-pii-exposure.md`](../../../docs/operations/postmortems/2026-09-02-log-store-credential-pii-exposure.md).
The front-matter `postmortem:` field is set once it is ratified
(`docs/operations/sev-playbook.md` §6.1).
