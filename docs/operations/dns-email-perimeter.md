---
title: DNS + email perimeter — the intended record set for stellarindex.io
last_verified: 2026-09-03
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
| `stellarindex.io` | MX 32/48/66 | `route3/route1/route2.mx.cloudflare.net` | Cloudflare Email Routing — inbound for `security@`, `hello@` |
| `stellarindex.io` | TXT | `v=spf1 include:amazonses.com include:_spf.mx.cloudflare.net -all` | apex SPF |
| `_dmarc` | TXT | `v=DMARC1; p=quarantine; rua=mailto:dmarc@stellarindex.io; fo=1; pct=100` | anti-spoofing policy |
| `resend._domainkey` | TXT | `p=…` | Resend DKIM (`d=stellarindex.io`) |
| `cf2024-1._domainkey` | TXT | `v=DKIM1; …` | Cloudflare Email Routing DKIM |
| `send` | MX 10 | `feedback-smtp.us-east-1.amazonses.com` | Resend bounce/complaint feedback |
| `send` | TXT | `v=spf1 include:amazonses.com ~all` | Resend Return-Path authorisation |
| `stellarindex.io` | CAA | `issue`/`issuewild` for `letsencrypt.org`, `pki.goog; cansignhttpexchanges=yes`, `ssl.com`, `sectigo.com`, `comodoca.com`, `digicert.com; cansignhttpexchanges=yes` | restrict certificate issuance |

---

## The four decisions worth knowing, and why they went the way they did

### 1. Apex SPF is `-all`, and it is safe here specifically because nothing sends from the apex

Resend's `Return-Path` is `@send.stellarindex.io`, not the apex, so the
apex SPF record governs **only** mail claiming an apex `MAIL FROM` — of
which we send none. A hard fail there rejects spoofers and leaves every
legitimate message untouched, including forwarded ones (a forwarded
Resend message still carries the `send.` Return-Path, governed by that
subdomain's `~all`).

Both includes resolve to flat `ip4`/`ip6` lists, so the record costs
**two** of SPF's ten permitted DNS lookups.

**Relax it to `~all`** the moment anything begins sending with an apex
`MAIL FROM` — a Google Workspace mailbox, a ticketing system, a
newsletter tool. Add the sender's `include:` first, and only then
consider softening the qualifier.

**There must be exactly one apex SPF record.** Two is a `permerror`,
which does not degrade to "the stricter one wins" — it disables SPF
evaluation entirely. The check asserts the record as an exact string
rather than a substring for this reason. Cloudflare Email Routing offers
to add its own apex SPF during setup; that offer was declined and the
include folded into the single record by hand.

### 2. DMARC alignment is relaxed, deliberately

The original issue proposed `adkim=s; aspf=s`. That is wrong for this
topology. Resend sends `From: …@stellarindex.io` with a
`Return-Path: …@send.stellarindex.io`, so **strict** SPF alignment fails
on every legitimate message we send. DKIM (`d=stellarindex.io`) aligns
strictly either way and DMARC passes if either mechanism aligns, so the
mail would still have been delivered — but the SPF half would have been
silently dead, and the failure would only have surfaced the day DKIM
broke. Relaxed alignment is correct and is asserted by the check.

`p=quarantine` rather than `p=none` was chosen because the only sender is
Resend and its DKIM already aligns, so there is no tuning period to wait
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

**1. Verify the Email Routing destination.** Cloudflare has sent a
verification link to `ash@ashfrancis.com`. Until it is clicked, the MX
records exist but no rule can be created, and mail to `security@` is
rejected at SMTP time with a clear bounce rather than being silently
dropped (the catch-all is deliberately left disabled so the failure is
loud). Once verified, create the forwarding rules for `security@`,
`hello@`, `dmarc@`, `abuse@` and `postmaster@`.

This is the sharpest half of #334: `security@stellarindex.io` is the
RFC 9116 `Contact:` on the live `.well-known/security.txt`, so a
researcher following the published disclosure channel currently gets a
bounce. `SECURITY.md` hedges that the mailbox may not be provisioned;
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
