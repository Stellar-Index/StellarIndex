---
title: Maintainer workflow — running the reference deployment
last_verified: 2026-09-02
status: current
---

# Maintainer workflow — running the reference deployment

How the **reference deployment** (`r1`) is operated: config application,
heavy jobs, releases and deploys, and the working cadence of a long agent
session. It is separate from [AGENTS.md](../../AGENTS.md) (project
invariants, true for any fork); this file is true for *our* boxes. If you
run your own deployment, treat it as a worked example. The parts that
generalise: S3-compatible storage (ADR-0002), the ansible roles under
`configs/ansible/`, the release script's guard rails.

---

## r1 configuration is ansible-managed — codify every host change

The archival-node playbook applies cleanly to r1 and a weekly
`ansible-drift.yml` workflow fails on divergence. **Any config change on r1
lands in `configs/ansible/` in the same PR** (secrets: the vault file
`inventory/r1.secrets.yml`, edited through the vault helper, never printed).
Hand fixes without codification WILL page Monday morning. Apply with
`ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags
<area>`, always `--check --diff` first. Findings log:
docs/operations/r1-ansible-drift-2026-07-03.md.

## Heavy one-shot jobs on r1 — ALWAYS use the wrapper

Any ops one-shot on r1 (re-derives, backfills, bulk SQL, census walks) runs
under `/usr/local/sbin/run-heavy-job.sh <name> <cmd…>`: a systemd scope with
MemoryMax=20G, MemorySwapMax=0 and batch-class CPU/IO weights. Never run a
heavy binary raw: an unwindowed re-derive ballooned silently, swapped
galexie's captive core into an `invalid local state` wedge and froze the
lake for 11 hours. Galexie carries MemoryLow=16G + elevated CPU/IO weight;
`stellarindex_galexie_catchup_refused` pages if the core refuses catchup
again. One heavy job at a time.

**Timer units.** `restore-drill` and `restore-drill-offsite` run under the
wrapper (`HEAVY_JOB_CLASS=scheduled`, `SuccessExitStatus=75` for a lock
skip). The drill refuses up front unless pool free space covers the restore
plus the watchdog's 300 GiB floor. Other timer units deliberately run bare:

| Unit | Why not wrapped |
|---|---|
| `ch-lake-backup` | The ClickHouse server owns the `BACKUP … ASYNC` and writes it off-host; a watchdog stop would kill only the polling script and orphan the backup. The script refuses a concurrent backup itself. |
| `zfs-snapshot` | It is the pool min-free guard and prunes snapshots to free space; a low-pool watchdog would stop the job that relieves it. Snapshot create/destroy is metadata-only. |
| `ch-schema-snapshot`, `ch-schema-drift`, `galexie-archive-tip-lag`, `galexie-archive-contiguity`, `memory-mappings` | Megabyte-scale probes or dumps, already `Nice`d and time-bounded; a lock skip or watchdog stop would only blind the staleness alerts. |
| `stellar-core-auto-upgrade` | Runs `apt-get`; a watchdog kill mid-dpkg can leave packages half-configured. Bounded by its own timeout and rollback. |
| Non-root services in `14-stellarindex-services.yml` | The wrapper's memory cap and disk watchdog are inactive for non-root callers (only the lock applies); each self-guards. |

---

### "Cut a release"

SemVer `vX.Y.Z`; bump rules in
[semver-policy.md](../architecture/semver-policy.md). Operator side:

1. **CHANGELOG + release PR.** Produce the `## [vX.Y.Z] — YYYY-MM-DD`
   section per [release-process.md](release-process.md) (CHANGELOG rules:
   [CONTRIBUTING.md](../../CONTRIBUTING.md#changelog)). Title the PR
   `release: vX.Y.Z`; squash-merge once CI is green.
2. **Cut the tag** via the guard-rail script:
   ```sh
   git checkout main && git pull --ff-only origin main
   bash scripts/dev/cut-release.sh vX.Y.Z --yes
   ```
   `--yes` skips the confirmation; from a non-TTY shell the prompt hits EOF
   and the script exits 2 unless `--yes` or `--dry-run` is passed. The script
   verifies branch + clean tree + sync + non-empty CHANGELOG section + green
   `verify.sh` before tagging and pushing. Use `--dry-run` first.
3. **`release.yml` fires on the tag push:** cross-compiles every `cmd/`
   binary for `linux/amd64` only (every region is amd64; re-add arm64 when a
   host exists), computes SHA256SUMS, extracts the CHANGELOG section as
   notes, creates the GitHub Release (`--prerelease` if the tag has a
   `-suffix`). It does **NOT** publish container images (no consumer).

Full runbook + manual fallback: [release-process.md](release-process.md).

### "Deploy a release to R1"

Operator-triggered, never automatic on tag.

```sh
gh workflow run deploy.yml \
  -f region=r1 \
  -f version=vX.Y.Z \
  -f binaries=stellarindex-indexer,stellarindex-aggregator,stellarindex-api
```

If the release touched any config-bearing surface (ansible
`stellarindex.toml`, Prometheus rules, systemd units, DB schema), add
`-f config_acknowledged=true` and mean it: the deploy swaps BINARIES ONLY,
so a feature those surfaces gate ships dead and silent until the config is
applied, and the post-deploy config-apply gate fails the job to force the
question. See [deploy-config-apply.md](deploy-config-apply.md).

The workflow downloads binaries from the GitHub Release, verifies
SHA256SUMS, and runs an Ansible playbook over SSH: **stage → backup →
atomic install → restart → health probe → automatic rollback on failure**.
Backups: `/usr/local/bin/<binary>.prev-<previous-tag>`, 5 retained.
Setup (4 GitHub secrets per region) and rollback path:
[deploy-workflow.md](deploy-workflow.md). R2 / R3 are deferred (4 secrets +
~4 lines of workflow YAML, no playbook changes).

---

## Working in a long session: commit-merge-repeat, not stack-then-split

For a multi-hour agent task (e.g. `/loop keep going`) the default is **one
PR → one merge → next PR**. Don't accumulate several narrative PRs of
uncommitted work: shared files (`cmd/stellarindex-indexer/main.go`,
`internal/config/*`, `AGENTS.md`) get touched by several of them and can't
be split without hunk surgery.

1. Pick one logical unit of work.
2. Make it build + its tests pass.
3. Commit, push, open PR, merge (`gh pr merge --squash` once CI is green;
   merge past failing optional checks only if the failure is pre-existing CI
   infra). Push and PR are one step, never two sessions: a pushed branch
   with no PR is loss, not work
   ([CONTRIBUTING.md — No orphan work](../../CONTRIBUTING.md#no-orphan-work--the-contract);
   run its prior-art check before touching an alert or finding).
4. Pull main, branch again, next unit.

Never plan a 3–4 PR pipeline before the first lands; split a bigger task
into linear merge-as-you-go units. Exception: the user says "don't merge
yet, I want to see the whole thing first."

An explicit agreement to the contrary (e.g. the issues-only / one-batch-PR
arrangement in the launch plan's process addenda,
`docs/operations/v1-launch-plan.md`) overrides this cadence; "no orphan
work" still applies (an issue filed instead of a PR is fine, a pushed branch
nobody can see is not).

---

_This file is hand-maintained. If a fact here is no longer true, update it in
the same PR as the change that invalidated it. Freshness checked in CI._
