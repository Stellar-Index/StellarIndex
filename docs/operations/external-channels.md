---
title: External channels — which are switched on, and what the docs may promise
last_verified: 2026-09-03
status: living doc
---

# External channels

Some of the channels this project publishes live **outside the
repository** — GitHub repository settings, DNS names — where no gate in
the tree can observe them. That is how three of them came to be
documented as available while switched off, each 404ing for the first
reader who followed the pointer:

- `SUPPORT.md` answered "How do I…" with a link to GitHub Discussions on
  a repository that has Discussions **disabled**
  (`has_discussions: false`), and `.github/ISSUE_TEMPLATE/config.yml`
  sent every would-be issue reporter to the same 404 first.
- `SECURITY.md` offered GitHub's private vulnerability reporting as the
  fallback for "if the mailbox is not yet provisioned", on a repository
  where that setting is **disabled** (`enabled: false`). Both documented
  disclosure routes were therefore dead at once, so a researcher holding
  a real finding had nowhere to send it.
- `openapi/stellar-index.v1.yaml` declared a `Staging` server for
  `api.staging.stellarindex.io`, a name with no DNS record. Every client
  importing the spec inherited a dead entry in its server picker.

The first two are switches. Their state lives in the table below, which
`scripts/ci/lint-external-channels.sh` reads and enforces: no tracked
file may contain a string a channel forbids while it is `disabled`.
Switching one on is therefore a **one-line revert** — flip its row to
`enabled`, and the same commit may restore the wording the gate
currently forbids.

The third was not a switch but a promise made ahead of a deployment, so
it was removed rather than softened. `scripts/ci/lint-openapi-urls`
keeps it out: a `servers:` entry may only name a host in that linter's
`servedHosts`, because every generated client, Postman import and try-it
console offers a server entry as a selectable target.

---

## State

Column 4 holds the literal strings that must not appear in a shipped
file while column 2 says `disabled`. They are matched as fixed strings,
comma-separated, so a pattern may not itself contain a comma.

| id | state | where it is switched on | strings forbidden while disabled |
|---|---|---|---|
| `discussions` | `disabled` | GitHub → Settings → General → Features → **Discussions** | `github.com/Stellar-Index/StellarIndex/discussions` |
| `private-vulnerability-reporting` | `disabled` | GitHub → Settings → Advanced Security → **Private vulnerability reporting** → Enable | `Report a vulnerability` |

Check both without opening the UI:

```sh
gh api repos/Stellar-Index/StellarIndex --jq .has_discussions
gh api repos/Stellar-Index/StellarIndex/private-vulnerability-reporting --jq .enabled
```

Both print `false` today. Only the account owner can change either;
neither is reachable from CI, and nothing in this repo attempts it.

---

## What to restore once each is on

**`discussions`** — flip the row, then put the Discussions link back as
the headline answer in `SUPPORT.md` and restore the "Question about
using the API" contact link in `.github/ISSUE_TEMPLATE/config.yml`. Seed
one pinned thread before announcing it: an empty Discussions tab reads
worse than no tab.

**`private-vulnerability-reporting`** — flip the row, then name the
Security-tab form in `SECURITY.md` as the private route, and drop the
contactless-issue fallback standing in for it today. This is the sharper
of the two, because it is the only private route that does not depend on
mail delivery — and `security@stellarindex.io` is undeliverable until
the Cloudflare Email Routing destination is verified (see
[dns-email-perimeter.md](dns-email-perimeter.md), "Open: two things only
the account owner can finish"). Enabling this setting closes the
disclosure gap on its own, ahead of the mailbox.

---

## Adding a channel to this file

Add a row when the repository starts pointing readers at something only
the account owner can switch on — a sponsorship page, a package
registry, a status-page provider, a support mailbox. The row costs one
line; skipping it costs a 404 in front of the first reader who follows
the pointer, which is how every entry above got here.
