---
title: External channels — which are switched on, and what the docs may promise
last_verified: 2026-10-05
status: living doc
---

# External channels

Some channels live outside the repo (GitHub settings, DNS) where no gate sees them.
`scripts/ci/lint-external-channels.sh` reads the table below: no tracked file may
contain a string a channel forbids while it is `disabled`. Switching one on is a
one-line revert: flip the row to `enabled`, and the same commit may restore the
forbidden wording. A server entry for a name with no DNS is not a switch: it is kept
out by `scripts/ci/lint-openapi-urls` (`servers:` may only name a host in its `servedHosts`).

## State

Column 4 holds the literal strings that must not appear in a shipped
file while column 2 says `disabled`. They are matched as fixed strings,
comma-separated, so a pattern may not itself contain a comma.

| id | state | where it is switched on | strings forbidden while disabled |
|---|---|---|---|
| `discussions` | `enabled` | GitHub → Settings → General → Features → **Discussions** | `github.com/Stellar-Index/StellarIndex/discussions` |
| `private-vulnerability-reporting` | `enabled` | GitHub → Settings → Advanced Security → **Private vulnerability reporting** → Enable | `Report a vulnerability` |

Check both without opening the UI:

```sh
gh api repos/Stellar-Index/StellarIndex --jq .has_discussions
gh api repos/Stellar-Index/StellarIndex/private-vulnerability-reporting --jq .enabled
```

Both print `true`. Only the account owner can change either;
neither is reachable from CI, and nothing in this repo attempts it.

## What switching on restored

- `discussions`: put the Discussions link back as the headline answer in `SUPPORT.md`
  and restore the "Question about using the API" contact link in
  `.github/ISSUE_TEMPLATE/config.yml`. Seed one pinned thread first.
- `private-vulnerability-reporting`: name the Security-tab form in `SECURITY.md` as the
  private route and drop the contactless-issue fallback. `security@stellarindex.io`
  delivers ([dns-email-perimeter.md](dns-email-perimeter.md)), so this is a second route.

## Adding a channel

Add a row when the repo points readers at something only the account owner can switch
on (sponsorship page, registry, status page, mailbox).
