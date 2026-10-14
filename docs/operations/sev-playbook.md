---
title: Incident (SEV) Playbook
last_verified: 2026-09-24
status: ratified
---

# Incident (SEV) Playbook

Binds every incident responder. Ratified 2026-04-22.

---

## 1. Severity definitions

**SEV-1**, service downtime:

- API 5xx on > 5 % of requests for > 2 min, OR
- API p95 > 1 s sustained > 2 min, OR
- complete ingestion halt (every source lagging > 15 min), OR
- data loss suspected (Timescale primary unrecoverable / backup gap).

**SEV-2**, degraded but serving:

- API p95 > 500 ms sustained > 5 min, OR
- 2+ sources halted, others OK, OR
- single region unavailable (multi-region absorbing load), OR
- Redis master down (Sentinel failover in progress), OR
- cross-region replication lag > 30 min on async replica.

**SEV-3**, internal degradation: single source lagging 5-15 min, OR backup/restore drill failure (no production impact).

**SEV-4**, informational: "watch" alerts, filed to tickets, not paged; reviewed weekly.

---

## 2. Timelines (the SLA promises)

| Severity | Detect by | Respond (ack) by | Triage complete by | Status update cadence |
| -------- | --------- | ---------------- | ------------------ | --------------------- |
| SEV-1    | <= 15 min | <= 30 min        | <= 60 min          | hourly                |
| SEV-2    | <= 30 min | <= 60 min        | <= 240 min         | daily                 |
| SEV-3    | <= 60 min | next business day | within 1 business day | weekly via tickets |
| SEV-4    | <= 24 h   | next weekly review | -                | weekly digest         |

Detect = monitoring catches it. Respond = responder acknowledges. Triage = root cause known plus an action plan.

---

## 3. Detection channels

> **Discord fanout is wired; PagerDuty is not.** Pages route through `discord_configs` in
> [`configs/alertmanager/alertmanager.r1.yml`](../../configs/alertmanager/alertmanager.r1.yml)
> ([runbooks/wire-paging.md](runbooks/wire-paging.md)): no ack timer, no secondary escalation, no maintainer
> fallback. A page is a Discord message someone has to be watching.

| Channel | What it catches | Fires |
| ------- | --------------- | ----- |
| Prometheus / AlertManager | Every alert in [alerts-catalog.md](alerts-catalog.md) | Instantly |
| Synthetic probes (curl every 30 s from 3 regions) | Public outages that bypass internal metrics | <= 30 s |
| Cloudflare load-balancer health | Region-level failures | <= 45 s |
| User report (email, Discord) | Whatever we missed | Variable |
| On-call rotation dashboard | Passive; reviewed every 15 min during oncall | - |

Every SEV-1 needs at least two independent detection channels. If only one fires, assume false positive after a
30 s re-probe.

---

## 4. Response flow

1. Alert lands in Discord **#stellarindex-pages**. Re-probe after 30 s or check a second detector; if real, continue.
2. Acknowledge (no timer; whoever is on the nightly rotation, §7, must be watching), open `#incident-<id>`,
   declare severity, post the initial status update. Acknowledging means "seen and on it", not resolved.
3. Follow the runbook; mitigate (fix or fail over); post "contained" at triage-complete.
4. Root-cause, resolve, post final "all clear", schedule the postmortem (§6).

**If personal data may be involved**, at any severity (including an exposure found in code review), the IC also
starts the [§6.6 breach-notification assessment](#66-personal-data-breach-notification-assessment) at
declaration. Its regulator deadline runs from when we became aware, not from resolution.

### 4.1 Incident channel

Every SEV-1/SEV-2 gets `#incident-<YYYYMMDD>-<short-slug>` under the incidents category, created by the on-call
responder. Topic: severity + declared-at timestamp, IC, latest action + ETA.

### 4.2 Roles

- **Incident Commander (IC):** first responder until a manager takes over.
- **Communications lead:** status-page updates and user Discord messages.
- **Technical lead:** diagnostics and fixes. On SEV-2 the IC may also be tech lead; on SEV-1 split them.

Only the tech lead commits changes to production during an incident.

### 4.3 Fixing vs reverting

Triggered by a recent deploy: revert first, investigate after. Latent bug weeks old: forward-fix only.

- Deploy within 4 h: likely cause; revert, observe 15 min.
- Deploy within 24 h: possible; check metrics for deploy-time correlation before reverting.
- No recent deploy: forward-fix; look at infrastructure, upstream changes, load shift.

---

## 5. Public communication

### 5.1 Status page

`https://status.stellarindex.io` is a static export of [`web/status/`](../../web/status/) on its own Cloudflare Pages project (`status-page.yml`); it makes no API call, so it publishes during an API outage: post there first. Source of truth is `internal/incidents/data/<slug>.md` (also rendered by `/v1/incidents`): push to `main`, then `gh workflow run status-page.yml --ref main`. How to post: [`runbooks/sev-status-page-update.md`](runbooks/sev-status-page-update.md), binding for every SEV-1/SEV-2 update.

**Webhook fan-out** (`incident.sev1`, `incident.resolved`) is operator-triggered, after deploying the binary that embeds the new `.md`:

```sh
stellarindex-ops emit-incident -config /etc/stellarindex.toml \
  -slug 2026-05-12-redis-blip -event sev1 -write
# later, after deploying the .md with status=resolved:
stellarindex-ops emit-incident -config /etc/stellarindex.toml \
  -slug 2026-05-12-redis-blip -event resolved -write
```

It refuses impossible combinations before any network I/O.

- **Subscribers:** no email list; Atom feed `GET /v1/incidents.atom`; dashboard webhooks. SEV-1 maps to major, SEV-2 minor, SEV-3 maintenance ([`ha-plan.md` §7.4](../architecture/ha-plan.md#74-status-page)).
- **Maintenance:** before a deploy that silences alerts, post a `severity: maintenance` notice (`POST /v1/admin/status-notices`) and resolve it afterwards.
- **Page unpublishable:** post in the public Discord (§7 step 1).

### 5.2 Update templates

Longer: [`deploy/comms/incident-update.md`](../../deploy/comms/incident-update.md).

**Initial (SEV-1):** We're investigating an incident affecting the Stellar Index API. Requests may fail or
return stale data. We acknowledged this at {time} and will post an update within the hour.

**Investigating:** Update: {subsystem} is affected. {mitigation}. Current impact: {scope}. Next update by {time}.

**Resolved:** Resolved as of {time}; service fully restored. Root cause: {one line}. Full postmortem within
{SEV-1: 72 h, SEV-2: 5 business days}.

### 5.3 User Discord

`#stellarindex-ops` (internal) is primary; major users have a direct channel. Cadence matches the status page.

### 5.4 What we do NOT say

- No root-cause speculation before triage is complete.
- No blame on individuals (blameless).
- No internal metric values that could expose attack surfaces.
- No timelines we can't keep.

---

## 6. After the incident

### 6.1 Postmortem

Required for every SEV-1 and SEV-2: `docs/operations/postmortems/<date>-<slug>.md`.

- SEV-1: draft within 72 h, ratified within 1 week.
- SEV-2: draft within 5 business days, ratified within 2 weeks.

Sections: Summary; Timeline (ISO-8601); Impact (measured); Root cause(s); Contributing factors; What went well; What went poorly; Action items (§6.2).

### 6.2 Action items

Each is a GitHub issue labelled `postmortem-action` with owner and due date, triaged at the weekly Monday ops review. A postmortem is incomplete until every item has both; "no action needed" must be explicit.

### 6.3 Retired postmortems

`docs/operations/postmortems/` holds one file per open incident; it is not an archive. Once a postmortem is
`status: resolved` and every action item is done or names a tracking issue, delete the file and append a row
(date retired, slug, one-line cause, permalink to the last version at the removal SHA).

| Retired | Slug | Cause | Last version |
| ------- | ---- | ----- | ------------ |
| - | - | - | (none yet) |

### 6.4 Blameless policy

Postmortems cover system failures, not individuals; no section names one.

### 6.5 Credential/PII exposure incidents

A redaction-class fix (one that stops a credential or PII value reaching the log store) is not complete when the
leak closes. In the SAME release cycle:

1. **File the incident record** under `internal/incidents/data/` (§5.1) even if nothing paged.
2. **Run** [`runbooks/credential-exposure-redaction-fix.md`](runbooks/credential-exposure-redaction-fix.md): a
   clean log scan proves only that the store no longer holds the value, so offer exposed credentials' owners
   rotation and notify customers of PII exposure.
3. **Run the [§6.6 breach-notification assessment](#66-personal-data-breach-notification-assessment)** for any PII
   exposure; its 72-hour clock started when the exposure was found.

### 6.6 Personal-data breach notification assessment

A personal-data breach is any security incident leading to personal data we hold being accessed, disclosed, altered, lost or made unavailable without authorisation (GDPR Art. 4(12)); staff reading data they had no need to see counts. We hold billing and user emails, names, session IPs, API-key use, audit and usage events, SHA-256 credential hashes (`migrations/0027_platform_v1_schema.up.sql`); logs can hold whatever a request carried (§6.5).

**Jurisdiction and clock.** Assess against both **UK GDPR** (ICO) and **EU GDPR** (each affected member state's authority; no EU establishment, so no lead authority) and apply the stricter outcome; list other places affected people live (e.g. US states) and take legal advice before hour 72. Notify the regulator without undue delay and within **72 hours** of becoming aware (Art. 33(1)): reasonably certain personal data was affected, not that the fix is finished. Record that timestamp at declaration; a processor's report starts the clock. Notify with partial facts rather than miss the deadline (Art. 33(4)).

**Assessment.** The IC completes it within 24 hours of awareness:

1. **Scope.** Data categories, roughly how many people and where they live, exposure window, who could have read it.
2. **Risk to people.** Identity theft, phishing, account takeover. A hash with no plaintext, or data readable only
   by already-authorised staff, is lower risk; an email tied to a paying account is not.
3. **Decide:**
   - **Unlikely to result in a risk:** no regulator notification; record the reasoning (step 4).
   - **Risk:** notify the ICO and each affected EU authority (Art. 33): nature, categories and approximate
     numbers, contact point, likely consequences, measures taken.
   - **High risk:** also tell affected people in plain language (Art. 34), via §5.3 and
     [`runbooks/credential-exposure-redaction-fix.md`](runbooks/credential-exposure-redaction-fix.md). Exempt only
     if the data was unintelligible, later measures removed the high risk, or notices would be disproportionate
     (then a public announcement).
   - **Undecided 48 hours after awareness:** notify.
4. **Record every breach, notified or not** (Art. 33(5)), under a `Breach assessment` heading in the postmortem
   (§6.1; write one even below SEV-2): awareness timestamp, facts, effects, remediation, decision and reasoning,
   what was sent to whom. The customer-facing record holds only what §5.4 allows.

---

## 7. Escalation chain

No PagerDuty rotation (§3); oncall coverage is tracked manually. Nightly coverage is maintainer-primary /
@alex-backup.

If all oncall are unreachable for > 30 min during a SEV-1:

1. Declare the incident in the public Discord anyway.
2. There is **no sealed break-glass credential**. Production secrets live only in the ansible-vault encrypted
   inventory ([credential-rotation.md](credential-rotation.md)); acting on them needs an operator's local copy
   plus the vault password. Until a break-glass mechanism exists, step 1 is the whole fallback.

---

## 8. Drills

- **Monthly tabletop** (30 min): scripted scenario on paper, no systems touched; tests the playbook.
- **Quarterly live chaos** (2 h window): pre-announced, staging-only; break something real and observe detection
  and response.
- **Annual DR exercise** (4 h window): simulated total-primary failure, flip to cloud DR, serve 1 h, flip back.
  Procedure: [`runbooks/dr-activation.md`](runbooks/dr-activation.md).

Log a row (date, tier, scenario, outcome, open action items) in [`drills/README.md`](drills/README.md).

---

## 9. References

- [alerts-catalog.md](alerts-catalog.md), [runbooks/](runbooks/)
- GitHub Actions spending cap: when every run fails with "The job was not started because an Actions budget is preventing further use", raise the limit at <https://github.com/organizations/Stellar-Index/settings/billing/spending_limit>.
- [HA plan](../architecture/ha-plan.md); [ADR-0006](../adr/0006-timescaledb-for-price-time-series.md); [ADR-0007](../adr/0007-redis-cache-schema.md)

---

## 10. Versioning

Versioned via `last_verified`. Revisions need sign-off from the maintainer and the current primary oncall. A
revision that weakens a contractual timeline (§2) requires an ADR; strengthening does not.
