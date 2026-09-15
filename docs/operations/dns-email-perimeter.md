---
title: DNS + email perimeter — the intended record set for stellarindex.io
last_verified: 2026-09-15
status: living doc
---

# DNS + email perimeter

**Zone:** `stellarindex.io` (Cloudflare). **Drift check:**
`bash scripts/ops/dns-perimeter-check.sh` — exit code is the number of
failed assertions, so cron and Healthchecks.io consume it the same way
`scripts/dev/r1-smoke.sh` is consumed.

This file exists because the email perimeter lives **outside the repo**,
where none of the repo's gate machinery can see it. That is exactly how
the domain came to be sending magic-link authentication email for months
with **no MX, no SPF, no DMARC and no CAA** (#334): DKIM was published
because Resend's onboarding requires it, and nothing else ever asked.

The record set below is the intended state. If you change a record,
change this file and the check script in the same commit.

---

## Intended records

| name | type | value | why |
|---|---|---|---|
| `stellarindex.io` | MX 1 | `smtp.google.com` | Google Workspace — inbound for `security@`, `hello@` |
| `stellarindex.io` | TXT | `v=spf1 include:amazonses.com include:_spf.google.com -all` | apex SPF |
| `_dmarc` | TXT | `v=DMARC1; p=quarantine; rua=mailto:dmarc@stellarindex.io; fo=1; pct=100` | anti-spoofing policy |
| `resend._domainkey` | TXT | `p=…` | Resend DKIM (`d=stellarindex.io`) |
| `google._domainkey` | TXT | `v=DKIM1; k=rsa; p=…` | Google Workspace DKIM |
| `send` | MX 10 | `feedback-smtp.us-east-1.amazonses.com` | Resend bounce/complaint feedback |
| `send` | TXT | `v=spf1 include:amazonses.com ~all` | Resend Return-Path authorisation |
| `stellarindex.io` | CAA | `issue`/`issuewild` for `letsencrypt.org`, `pki.goog; cansignhttpexchanges=yes`, `ssl.com`, `sectigo.com`, `comodoca.com`, `digicert.com; cansignhttpexchanges=yes` | restrict certificate issuance |

---

## The four decisions worth knowing, and why they went the way they did

### 1. Apex SPF is `-all`, and every apex sender must be inside it before it can stay that way

Resend's `Return-Path` is `@send.stellarindex.io`, not the apex, so it is
governed by that subdomain's `~all` and not by this record — including
when a message is forwarded, which keeps the `send.` Return-Path. Google
Workspace is the one sender that does claim an apex `MAIL FROM`, and
`include:_spf.google.com` is what authorises it.

That ordering is the whole rule: **a hard fail is only safe while every
apex sender is already inside the record.** Add the sender's `include:`
first; a new apex sender added without one is rejected outright, not
soft-failed, and the bounces start immediately.

Both includes resolve to flat `ip4`/`ip6` lists, so the record costs
**two** of SPF's ten permitted DNS lookups.

**Relax it to `~all`** only if a sender appears that cannot be expressed
as an `include:` at all.

**There must be exactly one apex SPF record.** Two is a `permerror`,
which does not degrade to "the stricter one wins" — it disables SPF
evaluation entirely. The check asserts the record as an exact string
rather than a substring for this reason. Google Workspace offers to add its own apex SPF during setup; that offer
was declined and the include folded into the single record by hand.

### 2. DMARC alignment is relaxed, deliberately

The original issue proposed `adkim=s; aspf=s`. That is wrong for this
topology. Resend sends `From: …@stellarindex.io` with a
`Return-Path: …@send.stellarindex.io`, so **strict** SPF alignment fails
on every legitimate message we send. DKIM (`d=stellarindex.io`) aligns
strictly either way and DMARC passes if either mechanism aligns, so the
mail would still have been delivered — but the SPF half would have been
silently dead, and the failure would only have surfaced the day DKIM
broke. Relaxed alignment is correct and is asserted by the check.

`p=quarantine` rather than `p=none` was chosen because both senders
publish DKIM that aligns — `resend._domainkey` and `google._domainkey`,
each signing `d=stellarindex.io` — so there is no tuning period to wait
out. Move to `p=reject` once the aggregate reports show a clean week.

### 3. No `ruf=`

Forensic reports carry the **full content of failed messages**, which
means third-party personal data arriving in an inbox we would then be
accountable for. Aggregate reports (`rua`) are enough to tune the policy.
This is the same judgement #346 applies to our own logs.

### 4. CAA is asserted as a superset, never as equality

Cloudflare injects its own CA set on top of whatever you publish — it
added `comodoca.com` and `digicert.com` to the four this repo asked for.
Pinning equality would go red the next time Cloudflare rotates a partner,
which is a false alarm, so the check asserts only that the two
load-bearing entries are present.

**`letsencrypt.org` is load-bearing.** r1's Caddy renews
`api.stellarindex.io` through Let's Encrypt. A CAA set that omitted it
would not fail today, or tomorrow — it would fail at the next renewal,
roughly sixty days later, taking the API TLS-dark with no obvious
connection to the DNS change that caused it. `pki.goog` is the same story
for the Cloudflare Pages hosts.

---

## Open: two things only the account owner can finish

**1. Confirm the published mailboxes deliver.** Inbound moved from
Cloudflare Email Routing to Google Workspace, and DNS can only show that
mail is routed there — not that a mailbox exists behind it. The check
asserts the MX destination and stops, because the next assertion would
have to send mail. Confirm from the Workspace console that `security@`,
`hello@`, `dmarc@`, `abuse@` and `postmaster@` each resolve to a real
mailbox or group.

This is the sharpest half of #334: `security@stellarindex.io` is the
RFC 9116 `Contact:` on the live `.well-known/security.txt`, so a
researcher following the published disclosure channel has nowhere else
to go. `SECURITY.md` hedges that the mailbox may not be provisioned;
`security.txt` makes no such hedge and cannot.

**2. Publish the DS record at the registrar.** DNSSEC is enabled in
Cloudflare and is **inert** until the registrar publishes this:

```
stellarindex.io. 3600 IN DS 2371 13 2 E6A7D241A651639E6F9745CB217768DDE50451AED12616EC8D84CA24681D22B4
```

Algorithm 13 (ECDSA P-256 SHA-256), digest type 2 (SHA-256), key tag
2371. A wrong DS takes the entire domain unresolvable, so paste it, do
not retype it, and check with `dig DS stellarindex.io @1.1.1.1`
afterwards. The check script reports the missing DS as a note rather than
a failure, and will start asserting it once it is published.
