---
title: GitHub Actions SHA-pinning policy
last_verified: 2026-10-05
status: operator runbook
---

# GitHub Actions SHA-pinning

Tags on third-party actions can be rewritten; a commit SHA cannot.

## Enforced in the repo

- `ci.yml` `actions-pinning` job: warns on existing tag-pinned third-party actions,
  hard-fails a PR that adds a new one. `actions/*` and `github/*` may stay tag-pinned.
- Dependabot (`github-actions` ecosystem, `.github/dependabot.yml`) queues bumps.

## Admin UI only (not in the repo)

- Settings, Actions, General: "Allow select actions and reusable workflows" with an
  approved-publisher list (`actions/*, github/*, cloudflare/*, docker/*, golangci/*,
  grafana/*, pnpm/*, stoplightio/*`); update it when a new third-party action lands.
- Same page: enable "Require approval for first-time contributors" and the fork-PR
  workflow gate for outside collaborators.

## Pin a tag

On the Dependabot PR: read the linked changelog, resolve the SHA, edit the `uses:` line
and keep the version as a trailing comment.

```sh
gh api repos/<owner>/<repo>/commits/<tag> --jq .sha
```

```yaml
- uses: cloudflare/wrangler-action@<40-char-sha>  # v3.7.0
```

When the gate fails, the error names the line and namespace: pin it before merging.
