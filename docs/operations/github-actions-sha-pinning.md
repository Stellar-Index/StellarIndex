---
title: GitHub Actions SHA-pinning policy
last_verified: 2026-05-12
status: operator runbook
---

# GitHub Actions SHA-pinning

F-1216 (codex audit-2026-05-12) flagged that the repo's CI/CD
workflows reference third-party GitHub Actions by mutable tags
(`@v3`, `@v6`) rather than immutable commit SHAs. A tag on a
third-party action repo can be rewritten or hijacked after we've
adopted it; a SHA is content-addressed and immutable.

## What's already enforced

- **CI gate** (`.github/workflows/ci.yml` → `actions-pinning`
  job, wave-11 of the audit remediation):
  - Warns on every existing tag-pinned third-party action.
  - HARD-FAILS in PR-diff mode when a new tag-pinned third-party
    `uses:` line is introduced.
  - First-party `actions/*` and `github/*` namespaces are
    permitted to remain tag-pinned (they're hosted by GitHub
    itself, single trust boundary).
- **Dependabot** is configured for the `github-actions`
  ecosystem (see `.github/dependabot.yml`), so SHA bumps queue
  automatically as upstream cuts new versions.

## What remains for the operator

Two pieces live in the GitHub admin UI, not in the repo:

### 1. Repository policy: allowed actions

Settings → Actions → General → Actions permissions →
"Allow select actions and reusable workflows". Add a comma-
separated list of approved publishers — typically
`actions/*, github/*, cloudflare/*, docker/*, golangci/*,
grafana/*, pnpm/*, stoplightio/*`. Update when a new third-
party action lands in a workflow.

### 2. Repository policy: require SHA pin

Same page, scroll down to "Allow actions created by GitHub"
checkbox group: enable "Require approval for first-time
contributors" and the related "Fork pull request workflows from
outside collaborators" gate. The SHA-pin requirement itself is
enforced by the in-repo CI gate above; the UI controls cover the
external-contributor case the gate alone can't catch.

## Migrating an existing tag to a SHA

Dependabot opens a PR with the current tag bumped to the latest
version. The operator reviewing the PR:

1. Reads the upstream changelog (linked in the Dependabot PR body).
2. Runs:
   ```sh
   gh api repos/<owner>/<repo>/commits/<tag> --jq .sha
   ```
   to resolve the tag to an immutable SHA.
3. Edits the `uses:` line in the workflow:
   ```yaml
   - uses: cloudflare/wrangler-action@<40-char-sha>  # v3.7.0
   ```
   Keep the version as a trailing comment so future operators
   can read "what version this SHA corresponds to" without
   running `gh api` again.
4. Updates the Dependabot PR (or opens a new one with the SHA).

## When the CI gate hard-fails

The actions-pinning job fails when a PR introduces a new
tag-pinned third-party `uses:` line. The error message tells the
operator the exact line + the namespace; resolve by following
the SHA-pin procedure above before merging.

The job also prints WARN lines for every existing tag-pinned
third-party action — these are informational and don't fail the
build until the matching line shows up in `git diff main`.
This is the gradient-migration shape: existing pins migrate
opportunistically via Dependabot; no new tag pins can land.
