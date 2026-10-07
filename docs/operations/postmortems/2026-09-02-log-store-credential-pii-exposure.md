---
title: "[SEV-2] Customer emails and API keys written to the log store unredacted — 2026-09-02"
date: 2026-09-02
severity: SEV-2
status: draft
authors: [maintainer]
---

# [SEV-2] Customer emails and API keys written to the log store unredacted — 2026-09-02

Customer-facing record:
[`internal/incidents/data/2026-09-02-log-store-credential-pii-exposure.md`](../../../internal/incidents/data/2026-09-02-log-store-credential-pii-exposure.md).
Procedure this incident motivated:
[`runbooks/credential-exposure-redaction-fix.md`](../runbooks/credential-exposure-redaction-fix.md).

## Summary

The edge (Caddy) access log wrote two sensitive values in full to
journald and on into Loki: customer email addresses from the staff
look-up route's `?email=` query parameter, and complete customer API
keys from the `X-API-Key` request header. Anyone with Grafana read
access could read, and replay, both. The query-string filter enumerated
the parameters to redact, so any parameter added after it was written
leaked by default; the header filter had no entry for `X-API-Key`
because Caddy's built-in credential redaction does not know that name.
The leak was closed in four commits over 2026-09-01/03, and a full scan
of Loki's 30-day retention found no remaining `email=` line. The
key-rotation offer and customer notification required by
[`sev-playbook.md` §6.5](../sev-playbook.md#65-credentialpii-exposure-incidents)
have not been sent.

## Timeline

All times UTC, taken from commit timestamps. Customer-record time
`2026-09-02 00:55` is the same commit in local time (UTC+1).

| When | What |
|---|---|
| 2026-04-27 | `X-API-Key` accepted as an alternative to `Authorization: Bearer` (`internal/api/v1/middleware/auth.go`, `HeaderAPIKey`). Caddy has no redaction rule for it. |
| 2026-06-19 | Staff customer look-up goes live, taking the customer's email as a GET query parameter. |
| 2026-08-04 | `f98f75a6e` adds query redaction to the access log, enumerating the parameters to mask (magic-link and session tokens). `email` is not in the list. |
| 2026-09-01T23:55:56Z | `f07866b70` (cited elsewhere as `7843f129`): query redaction rebuilt as `?<redacted>` on the whole query; `X-API-Key`, `X-Reason`, `Referer` and the `Location` response header deleted before logging; boot-time config errors stop formatting credentials. Effective on the host only after an ansible run with `--tags caddy`. |
| 2026-09-02T21:43:09Z | `766a03c02`: the same filter added to Caddy's global logger. The first fix covered the site access logger only; `reverse_proxy` warnings and the catch-all default logger still emitted an unfiltered `request` object (journal: 5 lines with a query string, 11 with a `Referer`, 4 catch-all lines with a query, per 24 h). |
| 2026-09-02T23:05:22Z | `13b052d9d`: the Caddyfile regression test had started checking the global block instead of the site block (which carries 15,450 of 15,473 access lines). Stripping every header delete from the site block left the suite green; the test now asserts both blocks. |
| 2026-09-03T04:58:47Z | `3a5b84961`: the look-up term moves to a POST body and `?email=` is rejected with 400, so the address no longer travels in a URL (browser history, intermediate proxies, `Referer`). Read-only check on the host: 79,812 redaction markers in 7 days; Loki over 30 days across all jobs, 1,069,477 lines, zero `email=` and zero `postgres://`. |
| 2026-09-21 | Customer-facing incident record filed, with §6.5 added to the SEV playbook in the same change. |
| 2026-09-24 | §6.6 (personal-data breach notification assessment) added to the SEV playbook. |

## Impact

- **Data written to the log store:** customer email addresses (staff
  look-up route) and live customer API keys (any request authenticated
  with `X-API-Key`).
- **Who could read it:** staff with Grafana/Loki read access and
  anyone with journal access on the host. Not public.
- **Exposure window:** its start is not established from the record.
  What was readable at the fix is bounded by Loki's 30-day retention;
  journald's effective local retention was about ten hours.
- **Affected customers:** not counted. The population is every customer
  looked up by staff and every `X-API-Key`-authenticated customer inside
  the retention window before the fix reached the host.
- **Data correctness:** none. No served data was lost or altered.
- **Residual:** a key is still valid if it was not rotated, whether or
  not its log line has aged out. The retention scan proves the store is
  clean; it does not prove no key was read while it was there.

## Root causes

1. **Deny-list redaction.** The query filter named the parameters to
   mask. Every parameter added later, including `email`, was logged by
   default.
2. **Custom credential header outside the logger's knowledge.** Caddy
   redacts the credential headers it knows. `X-API-Key` is our name for
   a live key, and no rule deleted it.
3. **Filter scoped to one logger.** A `log` block inside a site
   replaces the default for that site; it does not cover the global
   loggers the same process also writes a `request` object to.

## Contributing factors

- PII in a GET query string. Redacting our own log cannot reach the
  other places a URL is copied to.
- Nothing in the Go build read the edge config, so the two Caddyfiles
  stayed identical only by hand, and no test pinned what the access log
  may contain.
- The first regression test could stop guarding the site logger without
  failing (`13b052d9d`).
- The fix is config, not code: the repo read as fixed before the host
  was.

## What went well

- The fix changed the filter's shape rather than adding `email` to the
  list: everything after `?` is dropped, so a future parameter is
  redacted without anyone remembering to add it.
- It was validated with `caddy validate` against the deployed Caddy
  version before rollout, since a malformed filter fails the reload.
- Redaction was checked against the live store, not argued: marker
  counts and a full-retention scan.
- The same day's review found both the second unfiltered logger and the
  test that had stopped guarding the site block.

## What went poorly

- The rotation offer and notification were not run in the same release
  cycle as the fix, as §6.5 now requires; the measurement that showed
  no `X-API-Key` customer in scope came a month later.
- The incident record was filed 19 days after the fix.
- The recorded retention scan covered `email=` and `postgres://`. No
  scan for logged `X-API-Key` values is recorded.
- No breach assessment was recorded (see below).

## Breach assessment

Per [`sev-playbook.md` §6.6](../sev-playbook.md#66-personal-data-breach-notification-assessment).

- **Awareness:** no later than 2026-09-01T23:55:56Z, the first fix
  commit. The exact time of discovery is not in the record.
- **Facts:** customer emails and live API keys were readable in the
  internal log store by staff with log access; see Impact.
- **Effects:** phishing aimed at a known customer email; account misuse
  through a usable, unrotated key.
- **Remediation:** redaction on every Caddy logger, the look-up term out
  of the URL, and a clean full-retention scan for `email=`.
- **Decision:** not yet made. The 72-hour window from the latest
  possible awareness time closed at 2026-09-04T23:55:56Z with no
  decision recorded, before §6.6 existed. §6.6 step 3 defaults an
  undecided assessment to notify.
- **Sent:** nothing yet.

## Action items

A postmortem is not complete until every item has an owner and a due
date (§6.2). Due dates are set at ratification unless stated.

- [x] Offer targeted key rotation to every `X-API-Key`-authenticated
      customer in the exposure window — **no customer in scope**:
      measured 2026-10-02, the only pre-fix key that authenticated in
      the window belongs to the internal `launch-verify-agent` account
      and was used once, at creation. Evidence: the closed checkbox in
      the [incident record](../../../internal/incidents/data/2026-09-02-log-store-credential-pii-exposure.md).
- [x] Correct the customer-facing record — it now states the measured
      "no customer in scope" finding instead of an unevidenced "no
      action required"; same incident record.
- [ ] Notify customers whose email was looked up by staff in the
      window (runbook step 4, PII). Not evidenced either way: the
      incident record measures `X-API-Key` use only. Open (INV-2772).
- [ ] Complete and record the §6.6 assessment above (decision,
      reasoning, what was sent to whom). Open (INV-2772).
- [x] Scan Loki's retention window for logged `X-API-Key` values and
      record the count — **0 values**: measured 2026-10-07 over the
      30-day retention (2026-09-07 to 2026-10-07), all jobs. One line
      names the header, a CORS preflight's
      `Access-Control-Request-Headers: x-api-key`, which carries no
      value. 2026-09-02 to 2026-09-06 had aged out before the scan, so
      it shows the store is clean, not what was readable then.
- [ ] Ratify this postmortem: set the incident record's `postmortem:`
      front-matter field and give each open item an owner and due date
      (§6.2). Open (INV-2772).
- [x] Allow-list-shaped query redaction and credential header deletes
      on the site logger — `f07866b70`.
- [x] The same filter on the global logger — `766a03c02`.
- [x] Regression test asserts both Caddy log blocks and fails if either
      is missing — `13b052d9d`.
- [x] Staff look-up term moved out of the URL — `3a5b84961`.
- [x] Rotation/notification runbook and §6.5/§6.6 playbook sections.

## Lessons

- Redact by allow-list. A filter that names what to hide leaks
  everything nobody has named yet.
- A credential header we invented is invisible to every tool's built-in
  redaction; strip it explicitly on every logger.
- A clean log store closes the leak, not the exposure. A key that was
  readable stays usable until it is rotated.

## Related

- Code paths: `configs/caddy/Caddyfile.api`,
  `configs/ansible/roles/archival-node/templates/Caddyfile.j2`,
  `internal/config/caddy_access_log_test.go`,
  `internal/api/v1/middleware/auth.go`,
  `internal/api/v1/dashboardauth/handlers_admin.go`.
- Fixes: `f07866b70`, `766a03c02`, `13b052d9d`, `3a5b84961`.
