---
title: DNS + email perimeter — the intended record set for stellarindex.io
last_verified: 2026-10-05
status: living doc
---

# DNS + email perimeter

Zone `stellarindex.io` (Cloudflare). Drift check: `bash scripts/ops/dns-perimeter-check.sh`
(exit code = number of failed assertions; run daily by `dns-perimeter-check.yml`).
The perimeter lives outside the repo, so this file is the intended state: change a
record, change this file and the check script in the same commit.

## Intended records

| name | type | value | why |
|---|---|---|---|
| `stellarindex.io` | MX 1 | `smtp.google.com` | Google Workspace inbound for `security@`, `hello@` |
| `stellarindex.io` | TXT | `v=spf1 include:amazonses.com include:_spf.google.com -all` | apex SPF |
| `_dmarc` | TXT | `v=DMARC1; p=quarantine; rua=mailto:dmarc@stellarindex.io; fo=1; pct=100` | anti-spoofing policy |
| `resend._domainkey` | TXT | `p=…` | Resend DKIM (`d=stellarindex.io`) |
| `google._domainkey` | TXT | `v=DKIM1; k=rsa; p=…` | Google Workspace DKIM |
| `send` | MX 10 | `feedback-smtp.us-east-1.amazonses.com` | Resend bounce/complaint feedback |
| `send` | TXT | `v=spf1 include:amazonses.com ~all` | Resend Return-Path authorisation |
| `stellarindex.io` | CAA | `issue`/`issuewild` for `letsencrypt.org`, `pki.goog; cansignhttpexchanges=yes`, `ssl.com`, `sectigo.com`, `comodoca.com`, `digicert.com; cansignhttpexchanges=yes` | restrict issuance |

## Rules that bite

- **Apex SPF is `-all`; add an apex sender's `include:` first.** A new apex sender
  without one is rejected outright. Resend's Return-Path is `send.stellarindex.io`
  (governed by that `~all`); Google Workspace is the only apex `MAIL FROM`. The record
  costs 2 of SPF's 10 lookups. Relax to `~all` only for a sender that cannot be an
  `include:`. Exactly one apex SPF record: two is a `permerror` that disables SPF, so
  the check asserts the exact string. Decline Google's offer to add its own apex SPF.
- **DMARC alignment stays relaxed.** Strict SPF alignment fails every Resend message
  (From apex, Return-Path `send.`). `p=quarantine` because both senders' DKIM aligns;
  move to `p=reject` after a clean week of aggregate reports.
- **No `ruf=`.** Forensic reports carry full failed-message content (third-party personal data).
- **CAA is asserted as a superset, never equality.** Cloudflare adds its own CAs
  (`comodoca.com`, `digicert.com`). `letsencrypt.org` is load-bearing (r1 Caddy renews
  `api.stellarindex.io` with it; omission fails at the next renewal ~60 days later);
  `pki.goog` likewise for the Cloudflare Pages hosts.

## Owner-side state (2026-09-30)

- Mailboxes deliver: test mail to `security@` and `abuse@` reached the Workspace groups,
  so the RFC 9116 `Contact:` in `.well-known/security.txt` is real. `hello@`, `dmarc@`,
  `postmaster@` route the same way and are checked in the Workspace console.
- DS record published at the registrar (`2371 13 2 E6A7D241…681D22B4`); `dig +dnssec
  stellarindex.io @1.1.1.1` returns the `ad` flag. A wrong DS makes the domain
  unresolvable: paste the value Cloudflare shows, never retype it.
