---
title: Incident (SEV) Playbook
last_verified: 2026-09-24
status: ratified
---

# Incident (SEV) Playbook

Binds every incident responder. Ratified 2026-04-22.

SLA this satisfies: SEV-1 detect <= 15 min / respond <= 30 min / hourly updates; SEV-2 detect <= 30 min /
respond <= 60 min / triage <= 240 min / daily updates.

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

Detect = monitoring catches it. Respond = responder acknowledges the page. Triage = root cause known plus an
action plan. Update = public status page plus user notifications.

---

## 3. Detection channels

> **Discord fanout is wired; PagerDuty is not.** `chat-page` (`severity=page`) and
> `chat-default`/`chat-informational` route through real `discord_configs` in
> [`configs/alertmanager/alertmanager.r1.yml`](../../configs/alertmanager/alertmanager.r1.yml)
> (see [runbooks/wire-paging.md](runbooks/wire-paging.md)). That file has no `pagerduty_configs` for any
> severity, so there is no 5-min ack timer, no secondary escalation and no maintainer fallback: a page is a
> Discord message someone has to be watching. §4 and §7 describe this Discord-only flow.

| Channel | What it catches | Fires |
| ------- | --------------- | ----- |
| Prometheus / AlertManager | Every alert in [alerts-catalog.md](alerts-catalog.md) | Instantly |
| Synthetic probes (curl every 30 s from 3 regions) | Public outages that bypass internal metrics | <= 30 s |
| Cloudflare load-balancer health | Region-level failures | <= 45 s |
| User report (email, Discord) | Whatever we missed | Variable |
| On-call rotation dashboard | Passive; reviewed every 15 min during oncall | - |

Every SEV-1 needs at least two independent detection channels (our alert plus a probe, or plus Cloudflare health).
If only one fires, assume false positive after a 30 s re-probe.

---

## 4. Response flow

1. Alert lands in Discord **#stellarindex-pages**. Re-probe after 30 s or check a second detector; if real, continue.
2. Acknowledge (no timer), open `#incident-<id>`, declare severity, post the initial status update.
3. Follow the runbook; mitigate (fix or fail over); post "contained" at triage-complete.
4. Root-cause, resolve, post final "all clear", schedule the postmortem (§6).

**If personal data may be involved**, at any severity (including an exposure found in code review rather than
paged), the IC also starts the [§6.6 breach-notification assessment](#66-personal-data-breach-notification-assessment)
at declaration. Its regulator deadline runs from when we became aware, not from resolution or the postmortem.

### 4.1 Acknowledgement (SEV-1/2)

No PagerDuty leg (§3): the page is a Discord message with no ack timer and no automatic backup escalation.
Whoever is on the nightly rotation (§7) must be watching the channel. Acknowledging means "I've seen it and I'm
on it", not resolved.

### 4.2 Incident channel

Every SEV-1/SEV-2 gets `#incident-<YYYYMMDD>-<short-slug>` (e.g. `#incident-20260515-timescale-primary-down`)
under the incidents category, created by the on-call responder. Topic carries severity + declared-at timestamp,
commander (IC), latest action + ETA.

### 4.3 Roles

- **Incident Commander (IC):** first responder until a manager takes over.
- **Communications lead:** status-page updates and user Discord messages.
- **Technical lead:** diagnostics and fixes. On SEV-2 the IC may also be tech lead; on SEV-1 split them.

Only the tech lead commits changes to production during an incident.

### 4.4 Mitigation vs resolution

Mitigate first, understand later. A SEV-1 mitigated in 15 min and root-caused an hour later is a good outcome.
Per-runbook files hold the concrete tactics.

### 4.5 Fixing vs reverting

Triggered by a recent deploy: revert first, investigate after. Latent bug weeks old: forward-fix only.

- Deploy within 4 h: likely cause; revert, observe 15 min.
- Deploy within 24 h: possible; check metrics for deploy-time correlation before reverting.
- No recent deploy: forward-fix; look at infrastructure, upstream changes, load shift.

---

## 5. Public communication

### 5.1 Status page

`https://stellarindex.io/status`, a static Next.js export at [`web/status/`](../../web/status/), deployed to
Cloudflare Pages on every push to `main`. It is separate from the API so it survives an API outage. The old
cstate/Upptime scaffolds are gone; [`status-page-setup.md`](status-page-setup.md) and
[`rollback.md`](rollback.md) may still describe them.

**Source of truth:** `web/status/` (site + components). Post an incident by editing
`internal/incidents/data/<slug>.md` (the same corpus is embedded in the API binary, so `/v1/incidents` and
`web/status/` stay in lockstep) and pushing to `main`.

**How to post:** [`runbooks/sev-status-page-update.md`](runbooks/sev-status-page-update.md), the binding
runbook for every SEV-1/SEV-2 update (cadence, safe-to-publish detail, workstation-down fallback).

**Webhook fan-out** (F-1249): dashboard hooks `incident.sev1` and `incident.resolved`. The corpus is embedded at
build time, so fan-out is operator-triggered:

```sh
# After deploying the binary that includes the new .md:
stellarindex-ops emit-incident \
  -config /etc/stellarindex.toml \
  -slug 2026-05-12-redis-blip \
  -event sev1

# Later, after deploying the .md update with status=resolved:
stellarindex-ops emit-incident \
  -config /etc/stellarindex.toml \
  -slug 2026-05-12-redis-blip \
  -event resolved
```

It refuses impossible combinations (sev1 on a resolved incident, resolved on an investigating one, sev1 on a
non-SEV-1 entry) before any network I/O. Zero subscribers is a successful no-op with a stderr line.

Shipped behaviour (decisions:
[`status-page-hosting-comparison.md`](../architecture/status-page-hosting-comparison.md#open-questions-for-the-implementer--resolved)):

- **Severity to state:** `severity:` sets the card: SEV-1 major, SEV-2 minor, SEV-3 maintenance. No PagerDuty or vendor webhook.
- **Subscribers:** no email list (zero PII). Atom feed `GET /v1/incidents.atom`; dashboard webhooks for push.
- **Retention:** the git corpus is the permanent record.
- **Maintenance:** before a deploy that silences alerts, post a `severity: maintenance` notice
  (`POST /v1/admin/status-notices`) and resolve it when the window closes.
- **Page unavailable or unpublishable:** post in the public Discord (§7 step 1).

### 5.2 Update templates

Longer customer email and status-page body: [`deploy/comms/incident-update.md`](../../deploy/comms/incident-update.md).

**Initial (SEV-1):** We're investigating an incident affecting the Stellar Index API. Requests may fail or
return stale data. We acknowledged this at {time} and will post an update within the hour.

**Investigating:** Update: we've identified that {subsystem} is affected. {mitigation being attempted}.
Current impact: {scope}. Next update by {time}.

**Resolved:** The incident is resolved as of {time}. Service is fully restored. Root cause: {one-line summary}.
We'll post a full postmortem within {SEV-1: 72 h, SEV-2: 5 business days}.

### 5.3 User Discord

`#stellarindex-ops` (internal) is primary. Major users have a direct channel for real-time updates. Cadence
matches the status page.

### 5.4 What we do NOT say

- No root-cause speculation before triage is complete.
- No blame on individuals (blameless).
- No internal metric values that could expose attack surfaces.
- No timelines we can't keep.

---

## 6. After the incident

### 6.1 Postmortem

Required for every SEV-1 and SEV-2. One file per incident: `docs/operations/postmortems/<date>-<slug>.md`.

- SEV-1: draft within 72 h, ratified within 1 week.
- SEV-2: draft within 5 business days, ratified within 2 weeks.

Mandatory sections: Summary (3-5 sentences); Timeline (ISO-8601); Impact (measured: requests failed, users
affected, data loss); Root cause(s); Contributing factors; What went well; What went poorly; Action items (§6.2).

### 6.2 Action items

Each is a GitHub issue labelled `postmortem-action` with owner and due date; the weekly Monday ops review triages
the open ones. A postmortem is not complete until every item has an owner and due date. "No action needed" is
valid only if stated explicitly.

### 6.3 Retired postmortems

`docs/operations/postmortems/` holds one file per open incident; it is not an archive. Once a postmortem is
`status: resolved` and every action item is done or names a tracking issue, delete the file and append a line
below (date retired, slug, one-line cause, permalink to the last version at the SHA it was removed). An item
with no issue number keeps the postmortem open.

**Retired:**

| Retired | Slug | Cause | Last version |
| ------- | ---- | ----- | ------------ |
| - | - | - | (none yet; both current postmortems still have open, unfiled action items) |

### 6.4 Blameless policy

Postmortems cover system failures, not individuals: ask why the system let the bad deploy through CI, not who
pushed it. No section names an individual; action items are system changes.

### 6.5 Credential/PII exposure incidents

A redaction-class fix (one that stops a credential or PII value reaching the log store, e.g. `7843f129`:
customer emails and `X-API-Key` in Caddy access logs) is not complete when the leak closes. Three follow-ups are
mandatory in the SAME release cycle:

1. **File the incident record** under `internal/incidents/data/` (§5.1 template) even if nothing paged. "Nothing
   paged" and "nothing was exposed" are different claims.
2. **Run the key-rotation / notification runbook:**
   [`runbooks/credential-exposure-redaction-fix.md`](runbooks/credential-exposure-redaction-fix.md). A clean
   retention-window scan proves only that the LOG STORE no longer holds the value, not that nobody with log access
   read it. Any exposed credential (API key, session token) gets targeted rotation offered to its owner; PII
   exposure gets an affected-customer notification.
3. **Run the [§6.6 breach-notification assessment](#66-personal-data-breach-notification-assessment)** for any PII
   exposure. Its 72-hour clock started when the exposure was found, not when the fix shipped; an
   affected-customer notice does not settle whether a regulator must be told.

### 6.6 Personal-data breach notification assessment

A personal-data breach is any security incident leading to personal data we hold being accessed, disclosed,
altered, lost or made unavailable without authorisation (GDPR Art. 4(12)). Staff reading data they had no need to
see counts (the 2026-09-02 log-store exposure,
`internal/incidents/data/2026-09-02-log-store-credential-pii-exposure.md`), as does losing a table we cannot
restore.

**What we hold** (`migrations/0027_platform_v1_schema.up.sql`): account billing and user emails, display names,
IP addresses of sessions, magic-link requests, API-key use, audit-log entries and API usage events, and
credentials stored only as SHA-256 hashes (`api_keys.key_hash`, `magic_link_tokens.token_hash`, MFA recovery
codes). Logs can hold whatever a request carried (§6.5).

**Jurisdiction.** Terms are governed by England and Wales law (`/terms` §9); the privacy policy names the ICO as
lead authority. Data is processed on Hetzner in Germany and sign-up is worldwide, so assess every breach against
both **UK GDPR** (ICO) and **EU GDPR** (the supervisory authority of each member state where affected people live;
we have no EU establishment, so no lead authority) and apply the stricter outcome. List other places affected
people live (e.g. US states, each with its own triggers and deadlines) in the assessment and take legal advice
before the 72-hour mark; meeting GDPR does not mean meeting their laws.

**The clock.** Notify the regulator without undue delay and within **72 hours** of becoming aware (Art. 33(1)).
"Aware" means reasonably certain personal data was affected, not that investigation or the fix is finished. Record
that timestamp at declaration. If a processor (hosting, email delivery) reports a breach to us, the clock starts at
their report. Without every fact by the deadline, notify with what we know and send the rest later (Art. 33(4));
if the deadline is missed, the notification must say why.

**Assessment.** The IC owns it and completes it within 24 hours of awareness:

1. **Scope.** Data categories affected (list above), roughly how many people, where they live (billing country,
   IP geolocation as fallback), exposure window, who could have read it (staff with access, staff without, the
   public, unknown third party).
2. **Risk to people.** Identity theft, phishing aimed at a known customer email, account takeover from a usable
   credential. A hash with no plaintext exposed is lower risk; data readable only by staff already authorised is
   lower risk; an email tied to a paying account is not.
3. **Decide:**
   - **Unlikely to result in a risk:** no regulator notification; record the reasoning (step 4).
   - **Risk:** notify the ICO and each affected EU authority under Art. 33: nature of the breach, categories and
     approximate numbers of people and records, contact point, likely consequences, what we have done.
   - **High risk:** also tell affected people without undue delay, in plain language (Art. 34), via the
     incident-comms path in §5.3 and the customer steps in
     [`runbooks/credential-exposure-redaction-fix.md`](runbooks/credential-exposure-redaction-fix.md).
     Art. 34(3) exemptions apply only when the data was unintelligible to whoever got it, our later measures
     removed the high risk, or individual notices would take disproportionate effort (then a public announcement).
   - **Undecided 48 hours after awareness:** notify. The default fails closed; a missed notification breaches
     Art. 33 in its own right.
4. **Record every breach, notified or not** (Art. 33(5)), under a `Breach assessment` heading in the incident's
   postmortem (§6.1), which we write for any breach even below SEV-2: awareness timestamp, facts, effects,
   remediation, decision and reasoning, what was sent to whom and when. The customer-facing record under
   `internal/incidents/data/` holds only what §5.4 allows.

---

## 7. Escalation chain

No PagerDuty rotation (§3); oncall coverage is tracked manually. Nightly coverage is maintainer-primary /
@alex-backup.

If all oncall are unreachable for > 30 min during a SEV-1:

1. Declare the incident in the public Discord anyway.
2. There is **no sealed break-glass credential** and no two-operator unseal procedure. Production secrets live
   only in the ansible-vault encrypted inventory ([credential-rotation.md](credential-rotation.md)); acting on
   them needs an operator's local copy of that untracked file plus the vault password. Until a break-glass
   mechanism is built, step 1 is the whole fallback.

---

## 8. Drills

- **Monthly tabletop** (30 min): scripted scenario on paper, no systems touched; tests the playbook.
- **Quarterly live chaos** (2 h window): pre-announced, staging-only; break something real and observe detection
  and response.
- **Annual DR exercise** (4 h window): simulated total-primary failure, flip to cloud DR, serve 1 h, flip back.
  Procedure: [`runbooks/dr-activation.md`](runbooks/dr-activation.md).

Log a row (date, tier, scenario, outcome, open action items) in [`drills/README.md`](drills/README.md), with
postmortem-style action-item discipline.

---

## 9. References

- [alerts-catalog.md](alerts-catalog.md), [runbooks/](runbooks/)
- [runbooks/operator-unblock-2026-05-08.md](runbooks/operator-unblock-2026-05-08.md): GH Actions spending-cap unblock procedure
- [HA plan](../architecture/ha-plan.md); [ADR-0006](../adr/0006-timescaledb-for-price-time-series.md); [ADR-0007](../adr/0007-redis-cache-schema.md)

---

## 10. Versioning

Versioned via `last_verified`. Revisions need sign-off from the maintainer and the current primary oncall. A
revision that weakens a contractual timeline (§2) requires an ADR; strengthening does not.
