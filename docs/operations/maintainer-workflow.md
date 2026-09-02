---
title: Maintainer workflow — running the reference deployment
last_verified: 2026-09-02
status: current
---

# Maintainer workflow — running the reference deployment

This file describes how the **reference deployment** (`r1`) is
operated: how its configuration is applied, how heavy jobs are run on
it, how releases are cut and deployed, and the working cadence a
long-running agent session follows.

It is deliberately separate from [CLAUDE.md](../../CLAUDE.md) and
[AGENTS.md](../../AGENTS.md). Those two describe the **project** —
its invariants, its architecture, the traps in the domain — and are
true for anyone who clones or forks this repo. What is below is true
for *our* boxes and *our* release process. A contributor changing a
decoder needs the former and not the latter; keeping them mixed made
the orientation docs read as though you had to adopt one maintainer's
operational setup to contribute.

If you are running your own deployment, treat this as a worked example
rather than a specification. The parts that generalise
(S3-compatible storage per ADR-0002, the ansible roles under
`configs/ansible/`, the release script's guard rails) are called out
where they apply.

---

## r1 configuration is ansible-managed — codify every host change

Since 2026-07-03 the archival-node playbook applies cleanly to r1 and a
weekly `ansible-drift.yml` workflow fails on divergence. The rule:
**any config change on r1 lands in `configs/ansible/` in the same PR**
(secrets → `ansible-vault edit inventory/r1.secrets.yml`). Hand fixes
without codification WILL page Monday morning. Apply changes via
`ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml
--tags <area>` — always `--check --diff` first. Findings log:
docs/operations/r1-ansible-drift-2026-07-03.md.

## Heavy one-shot jobs on r1 — ALWAYS use the wrapper

Any ops one-shot on r1 (re-derives, backfills, bulk SQL, census
walks) runs under `/usr/local/sbin/run-heavy-job.sh <name> <cmd…>` —
a systemd scope with MemoryMax=20G, MemorySwapMax=0, and batch-class
CPU/IO weights. Never run a heavy binary raw: on 2026-07-05 an
unwindowed re-derive ballooned silently, swapped galexie's captive
core into an `invalid local state` wedge, and froze the lake for 11
hours. The wrapper kills a ballooning job before it can starve the
consensus-critical processes; galexie itself carries MemoryLow=16G +
elevated CPU/IO weight, and `stellarindex_galexie_catchup_refused`
pages if the core ever refuses catchup again. One heavy job at a
time remains the rule.

---

### "Cut a release"

We use SemVer (`vX.Y.Z`) for binary releases — see
[docs/architecture/semver-policy.md](../architecture/semver-policy.md)
for what bumps the major / minor / patch.

End-to-end (operator side):

1. **Curate `CHANGELOG.md` `[Unreleased]`** — make sure every entry
   cites a PR, every empty section is deleted.
2. **Promote the section** in a one-commit PR: replace `## [Unreleased]`
   with `## [vX.Y.Z] — YYYY-MM-DD` and add a fresh empty `[Unreleased]`
   block above it. Title the PR `release: vX.Y.Z`. Squash-merge once
   CI is green.
3. **Cut the tag** via the guard-rail script:
   ```sh
   git checkout main && git pull --ff-only origin main
   bash scripts/dev/cut-release.sh vX.Y.Z --yes
   ```

   `--yes` skips the interactive confirmation. From a non-TTY shell the
   prompt hits EOF, so without `--yes` (or `--dry-run`) the script exits
   2 immediately — pass it whenever you are not at a terminal.
   The script verifies branch + clean tree + sync + non-empty CHANGELOG
   section + green `verify.sh` before tagging and pushing. Pass
   `--dry-run` first to see the plan.
4. **`release.yml` fires automatically** on the tag push:
   - Cross-compiles every binary in `cmd/` for `linux/amd64`
     (arm64 was dropped 2026-05-08 — every region is amd64; re-add
     when an arm64 host is provisioned).
   - Computes SHA256SUMS
   - Auto-extracts the matching CHANGELOG section as release notes
   - Creates the GitHub Release (marked `--prerelease` if the tag
     contains a `-suffix`)
   - **Does NOT publish container images.** The previous GHCR job
     was dropped (no consumer existed); see
     `docs/operations/release-process.md` for the rationale.
     F-1221 (2026-05-13): pre-fix this paragraph claimed both
     amd64+arm64 and GHCR pushes — both were stale.

Full runbook + manual fallback in
[docs/operations/release-process.md](release-process.md).

### "Deploy a release to R1"

Deploys are operator-triggered, never automatic on tag.

```sh
gh workflow run deploy.yml \
  -f region=r1 \
  -f version=vX.Y.Z \
  -f binaries=stellarindex-indexer,stellarindex-aggregator,stellarindex-api
```

If the release touched any config-bearing surface (the ansible
`stellarindex.toml`, Prometheus rules, systemd units, DB schema), add
`-f config_acknowledged=true` — and mean it. `deploy-binary.yml` swaps
BINARIES ONLY, so a feature those surfaces gate ships dead and silent
until an operator applies the config; the post-deploy config-apply gate
fails the job to force the question. A config-free release passes it
either way, so the flag is not boilerplate. See
[deploy-config-apply.md](deploy-config-apply.md).

The workflow downloads the binaries from the GitHub Release,
verifies SHA256SUMS, and runs an Ansible playbook over SSH that
does **stage → backup → atomic install → restart → health probe →
automatic rollback on failure**. Backups land at
`/usr/local/bin/<binary>.prev-<previous-tag>` with the most-recent
5 retained.

One-time setup: 4 GitHub secrets per region. Full operator runbook
including the rollback path: [docs/operations/deploy-workflow.md](deploy-workflow.md).

R2 / R3 are deferred — adding them is mechanical (4 secrets +
~4 lines of workflow YAML), no playbook changes needed.

---

---

## Working in a long session: commit-merge-repeat, not stack-then-split

If you are an AI agent running a multi-hour task (e.g. `/loop keep
going`), the default cadence is **one PR → one merge → next PR**.
Do NOT accumulate multiple narrative PRs of uncommitted work in the
tree and try to split them later — shared files
(`cmd/stellarindex-indexer/main.go`, `internal/config/*`,
`CHANGELOG.md`, `CLAUDE.md`) will be touched by several narrative
PRs and cannot be cleanly split into per-PR commits without hunk
surgery.

The rule:

1. Pick one logical unit of work.
2. Make it build + its tests pass.
3. Commit, push, open PR, merge. (`gh pr merge --squash` once CI is
   green; merge with failing optional checks only if the failure is
   pre-existing CI infra, not caused by this PR.) Push and PR are one
   step, never two sessions — a pushed branch with no PR is loss, not
   work: [AGENTS.md — No orphan work](../../AGENTS.md#no-orphan-work--the-contract)
   (run its prior-art check before touching an alert or finding).
4. Pull main, branch again, return to step 1 for the NEXT unit.

Never plan a pipeline of 3–4 PRs before the first one has landed.
If you realise a task is bigger than one PR, split it into linear,
merge-as-you-go units — not parallel branches that will collide on
shared files.

The one exception: if the user is actively reviewing mid-session
and explicitly says "don't merge yet, I want to see the whole thing
first." Otherwise, merge.

This is the DEFAULT, not a law of nature: if your session runs under
an explicit agreement to the contrary — e.g. the issues-only /
one-batch-PR arrangement recorded in the launch plan's process
addenda (`docs/operations/v1-launch-plan.md`) — that agreement
overrides the cadence above. Everything else still applies, "no
orphan work" above all: an issue filed instead of a PR is fine, a
pushed branch nobody can see is not.

---

_This file is hand-maintained. If you find a fact here that is no
longer true, update this file in the same PR as the change that
invalidated it. Freshness checked in CI._

