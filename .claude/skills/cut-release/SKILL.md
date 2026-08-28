---
name: cut-release
description: Cut a Stellar Index release (vX.Y.Z tag) — CHANGELOG curation, the promote commit, the guard-rail script, and the one-RC-per-session discipline. Use when asked to release, tag, or promote the CHANGELOG.
---

# /cut-release

Full runbook: `docs/operations/release-process.md`;
SemVer policy: `docs/architecture/semver-policy.md`.

## Discipline first

- **One release per session.** release.yml is the heaviest workflow
  (6 binaries × cross-compile); bundle the session's fixes into ONE
  tag, never four.
- Releases are tags; deploys are separate and operator-triggered
  (see /deploy-r1). Never assume a tag reaches r1.

## Steps

1. **Curate `[Unreleased]`**: every entry present (entries were
   added inline with their commits — verify nothing landed without
   one: skim `git log <last-tag>..HEAD --oneline` against the
   section), empty subsections deleted, BREAKING changes flagged.
2. **Promote commit**: replace `## [Unreleased]` with
   `## [vX.Y.Z] — YYYY-MM-DD`, add a fresh empty `[Unreleased]`
   above. Pick the bump per SemVer policy (pkg/client breaking =
   major consideration; pre-v1 minor for features).
3. **Tag via the guard-rail script** (never `git tag` by hand):
   ```sh
   git checkout main && git pull --ff-only origin main
   bash scripts/dev/cut-release.sh vX.Y.Z --dry-run   # read the plan

Non-interactive runs (agents, CI, `nohup`): pass `--yes` — the script refuses to run when stdin is not a TTY unless `--yes` or `--dry-run` is given (exit 2, loud message). This closed the 2026-08-28 incident where a backgrounded run hit EOF at the confirmation prompt, aborted, and `| tail` masked the exit so a "release" was announced with no tag on origin. Never gate on a piped exit code; check the tag on origin.
   bash scripts/dev/cut-release.sh vX.Y.Z
   ```
   It verifies branch, clean tree, remote sync, non-empty CHANGELOG
   section, and green verify.sh before tagging.
4. **release.yml fires on the tag**: linux/amd64 only, SHA256SUMS,
   release notes auto-extracted from the CHANGELOG section, GitHub
   Release created. It does NOT publish containers (dropped
   deliberately) and does NOT deploy.
5. Do not sit monitoring the workflow during a /loop session — check
   it when next convenient.

## After

If the release is meant for r1, hand off to **/deploy-r1**. If any
release-notes step fails, the manual fallback is in
release-process.md — do not hand-craft a GitHub release without
SHA256SUMS.
