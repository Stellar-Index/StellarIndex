---
title: Local preflight — answer CI's questions on the machine you are sitting at
last_verified: 2026-09-07
status: operational
---

# Local preflight

Each failure below was knowable locally in seconds but surfaced in GitHub
Actions or on a deploy host minutes later.

| Question | Command | Cost |
| --- | --- | --- |
| Will `deploy.yml` accept this dispatch? | `make preflight-deploy REGION=r1 VERSION=vX.Y.Z` | a few seconds; two read-only SSH reads |
| Can this checkout run `verify.sh` at all? | `make bootstrap-worktree` | ~11 s measured, both web apps from cold |
| Which preconditions is `verify.sh` about to fail on? | it says so itself, at the top of the run | ~0.1 s measured |
| Can a release be cut from this checkout? | `scripts/dev/cut-release.sh vX.Y.Z --dry-run` | under a second to the first failure; the ~20 min gate runs only once all three pass |

## `make preflight-deploy REGION=… VERSION=…`

Runs [`scripts/dev/preflight-deploy.sh`](../../scripts/dev/preflight-deploy.sh).
Answers every question `.github/workflows/deploy.yml` asks and ends with the
exact `gh workflow run deploy.yml` line for the region. Exit is non-zero
while an operator decision is outstanding; the command is printed either way.

**1. Config-apply gate.** `scripts/ci/config-apply-gate.sh` fails a deploy
whose release changed a config surface a binary deploy does not apply and
nothing has cleared. The gate decides the surfaces, in
[three cases](deploy-config-apply.md#three-cases-not-two) (comment-only,
already applied, substantive); the preflight reads both the surface list and
the classifier out of the gate script, so it reports the same names.

It adds the host half: for a `deploy/clickhouse/*.sql` diff verifiable by
object existence, it asks the target whether every created object is in
`system.tables` and passes the satisfied files to the gate as `[applied]`,
the same evidence `deploy.yml` produces.

The emitted dispatch **never** carries `-f config_acknowledged=true`: that
is an operator asserting a surface no checker can read has been applied,
which the script cannot assert for them. ("All comment-only, so
acknowledge" would have acknowledged v0.61.1..v0.62.0's unapplied
`CREATE TABLE`.)

Limits:

- A file type with no known comment convention (`.md`, anything
  unrecognised) is **substantive by default**.
- Comment-only means no rendered behaviour differs, not that host bytes
  match: a comment-only `.j2` change still shows in the weekly
  `ansible-drift` job until applied.
- "Already applied" is answered only for ClickHouse DDL adding whole new
  statements. A column added inside an existing `CREATE TABLE IF NOT
  EXISTS`, an `ALTER`, a systemd unit or an ansible template stays
  substantive.

**2. The region's real binary set.** `deploy.yml` defaults to six binaries
for three regions; dispatching that at futurenet (no
`stellarindex-aggregator` unit) failed the health probe and left
`/usr/local/bin/stellarindex-aggregator.failed-v0.62.0`.

[`scripts/dev/region-binaries.tsv`](../../scripts/dev/region-binaries.tsv) is
the manifest, derived from the hosts:

- A **daemon** binary is deployable where systemd has its unit and has not
  been masked. `configs/ansible/tasks/deploy-one-binary.yml` restarts
  `<binary>.service` and requires `systemctl is-active`, so an absent or
  masked unit fails by construction. Enablement alone is the wrong test
  (testnet's aggregator is `disabled` at boot yet `active`). Refused: unit
  absent or masked, or switched off *and* not running.
- A **CLI** binary (the three on `deploy-binary.yml`'s `cli_binaries`
  deny-list: `ops`, `migrate`, `sla-probe`) has no unit or health probe;
  the playbook stats the file. The test is installation.

Refresh a row with `--refresh-manifest` and commit it. Every run that can
reach the host re-derives the set and **uses the host's answer**; a stale
row is reported as drift and blocks the run until refreshed. `deploy.yml`
reads the same file
([deploy-workflow.md](deploy-workflow.md#the-region-binary-manifest)).

The emitted `-f binaries=` is always the region's **whole** deployable set;
a partial dispatch left `stellarindex-migrate` and
`stellarindex-sla-probe` behind and tripped
`stellarindex_binary_version_skew`.

**3. Skew and migrations.** Per binary, the live version (from
`/var/lib/stellarindex/deployed-versions/`) against the release. A binary
ahead of the release makes the dispatch a rollback: blocked, pointing at
[rollback.md](rollback.md) (migrations do not roll back with it, CS-099; a
previous-tag dispatch must set `migrations_skip=true`).
Migrations in the range are listed, checked with
`scripts/ci/lint-migration-compat.sh --staged` over the deploying tag's
`migrations/`, and block until `--migrations-ack` records an old-binary
compatibility read. A migration declaring `-- REQUIRED-FOLLOWUP:` commands
([`migrations/README.md`](../../migrations/README.md) rule 12) runs
deploy.yml's follow-up gate: commands are listed and the run blocks until
`--followups-ack`, which also adds `-f followups_acknowledged=true` to the
printed dispatch.

Flags: `--no-host` (no SSH; ancestry baseline, said so), `--refresh-manifest`,
`--migrations-ack`, `--followups-ack`.

Host reads are read-only: `cat` of the deploy sidecars, `systemctl
is-enabled` / `is-active`, `test -x`, and `SELECT … FROM system.tables` for
the already-applied question.

## `make bootstrap-worktree`

Runs [`scripts/dev/bootstrap-worktree.sh`](../../scripts/dev/bootstrap-worktree.sh).
Makes a fresh checkout or linked worktree able to pass `verify.sh` and
reports **every** gap in one pass. It checks the executable each gate
invokes (`openapi-typescript`, `tsc`), not the directory, since a
half-finished install leaves `node_modules` present and the binary absent.

- **blocking**: `verify.sh` hard-fails without it; exit non-zero while any remains.
- **clearance**: `verify.sh` defers it and ends `VERIFY INCOMPLETE`, while
  `scripts/dev/doctor.sh --profile native` (run by `make prepush` and
  `cut-release.sh`) requires it. Reported with its install command; does
  not fail the script.

Missing pinned Go tools are installed by `make deps`, which owns the pins.
`~/go/bin` is not on every PATH and the Makefile calls `gofumpt`,
`goimports`, `golangci-lint` there by absolute path, so the survey resolves
them the same way (`command -v` would false-miss). A tool at the **wrong**
version is reported, never silently corrected (`~/go/bin` is shared across
checkouts); `make deps` makes them all match.

`make bootstrap-worktree-check` surveys and installs nothing.

## `verify.sh` preconditions

`scripts/dev/verify.sh` resolves its preconditions in its first five
seconds, lists every missing one, and points at `make bootstrap-worktree`.
Each preamble condition is the same as the section that depends on it. It
also prints which checks **will** defer; under `VERIFY_FAIL_ON_SKIP=1`
(set by `make prepush`) a deferrable tool is blocking.

`VERIFY INCOMPLETE: 1 check(s) deferred` with exit 1 at the end of a macOS
run is **by design**: the deploy migrations-sync self-test runs an ansible
task file whose `unarchive --diff` needs GNU tar, and macOS ships bsdtar.
Use `VERIFY_PROFILE=container` or let CI's toolchain-gates job run it.
`make prepush` issues push clearance.

## `cut-release.sh` preconditions

Branch, clean tree and origin-sync are evaluated together before anything
else; every failure is named, the first output line names the failing
precondition.

## See also

- [deploy-workflow.md](deploy-workflow.md) — what `deploy.yml` does once dispatched
- [deploy-config-apply.md](deploy-config-apply.md) — the apply procedure per surface
- [deployed-versions.md](deployed-versions.md) — the sidecars the baseline is read from
- [release-process.md](release-process.md) — the runbook `cut-release.sh` implements
- [rollback.md](rollback.md) — why a rollback is not a deploy dispatch
