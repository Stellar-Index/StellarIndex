---
title: Local preflight — answer CI's questions on the machine you are sitting at
last_verified: 2026-09-07
status: operational
---

# Local preflight

Every failure below was discovered in GitHub Actions or on a deploy host,
minutes to tens of minutes after it became knowable locally. Measured on
2026-09-07 alone: **four failed deploys, two failed twelve-minute `verify.sh`
runs, one failed release cut** — each one answerable on this machine in
seconds beforehand.

| Question | Command | Cost |
| --- | --- | --- |
| Will `deploy.yml` accept this dispatch? | `make preflight-deploy REGION=r1 VERSION=vX.Y.Z` | a few seconds; two read-only SSH reads |
| Can this checkout run `verify.sh` at all? | `make bootstrap-worktree` | ~11 s measured, both web apps from cold |
| Which preconditions is `verify.sh` about to fail on? | it says so itself, at the top of the run | ~0.1 s measured |
| Can a release be cut from this checkout? | `scripts/dev/cut-release.sh vX.Y.Z --dry-run` | under a second to the first failure; the ~20 min gate runs only once all three pass |

## `make preflight-deploy REGION=… VERSION=…`

Runs [`scripts/dev/preflight-deploy.sh`](../../scripts/dev/preflight-deploy.sh).
Answers, in one run, every question `.github/workflows/deploy.yml` asks, and
ends with the exact `gh workflow run deploy.yml` line for that region. Exit is
non-zero whenever an operator decision is outstanding; the command is printed
either way, because knowing the command and knowing it is not yet safe are
separate facts.

The three failure classes it removes:

**1. Config-apply gate.** The gate (`scripts/ci/config-apply-gate.sh`) fails a
deploy whose release changed a config surface a binary deploy does not apply,
unless `-f config_acknowledged=true` was passed. Whether that acknowledgement
is honest or a rubber stamp depends on what actually changed, and the deploy
run is a slow place to find out — two releases in a row failed the gate, and
both turned out to be comment-only.

The preflight reads the surface list **out of** the gate script (one copy),
runs the gate for its own verdict, and adds the classification the gate does
not make: every added and removed line of each changed file is checked against
that file type's comment syntax. All comment-only ⇒ `config_acknowledged=true`
asserts something true, and the flag is emitted. Any substantive change ⇒ the
flag is withheld, the offending lines are printed, and the run exits non-zero
pointing at [deploy-config-apply.md](deploy-config-apply.md).

Two limits, stated because they matter:

- A file type with no comment convention known to the script (`.md`, anything
  unrecognised) is **substantive by default**. The failure to avoid is a
  rubber-stamped acknowledgement, so the ambiguous case falls to a human.
- Comment-only means *no rendered behaviour differs*. It does not mean the
  bytes on the host match: a comment-only `.j2` change still leaves a textual
  difference the weekly `ansible-drift` job will report until it is applied.

**2. The region's real binary set.** No per-region manifest existed anywhere:
`deploy.yml` carries one default list of six binaries for three regions.
Dispatching that list at futurenet, which has no `stellarindex-aggregator`
unit, failed the health probe and rolled the binary back — the residue is on
that host now as `/usr/local/bin/stellarindex-aggregator.failed-v0.62.0`.

[`scripts/dev/region-binaries.tsv`](../../scripts/dev/region-binaries.tsv) is
the manifest, derived from the hosts rather than declared:

- A **daemon** binary is deployable where systemd has its unit and has not
  been told to keep it off. `configs/ansible/tasks/deploy-one-binary.yml`
  restarts `<binary>.service` and then requires `systemctl is-active` to
  report active, so an absent or masked unit is a failed deploy by
  construction. Enablement alone is the wrong test and testnet is the
  counter-example: its aggregator is `disabled` at boot and `active` right
  now, and this workflow deployed it there at v0.62.0. What is refused is a
  unit that is absent or masked, or one that is switched off *and* not
  running.
- A **CLI** binary — the three on `deploy-binary.yml`'s `cli_binaries`
  deny-list (`ops`, `migrate`, `sla-probe`) — has no unit to restart and no
  health probe; the playbook stats the file. The test is installation.

Refresh a row with `--refresh-manifest` and commit the result. Every run that
can reach the host re-derives the set anyway and **uses the host's answer**,
so a stale row can never be what gets dispatched — it is reported as drift and
blocks the run until the row is refreshed.

The emitted `-f binaries=` list is always the region's **whole** deployable
set. A partial dispatch is what left `stellarindex-migrate` and
`stellarindex-sla-probe` behind and tripped `stellarindex_binary_version_skew`.

**3. Skew and migrations.** Per binary, the version live on the target against
the release, read from `/var/lib/stellarindex/deployed-versions/`. A binary
ahead of the release makes the dispatch a rollback, which is blocked with a
pointer to [rollback.md](rollback.md) — migrations do not roll back with it
(CS-099). Migrations in the range are listed with that same caveat, checked
with `scripts/ci/lint-migration-compat.sh --staged` over the deploying tag's
own `migrations/`, and block the run until `--migrations-ack` records that
they have been read for old-binary compatibility.

Flags: `--no-host` makes no SSH connection at all and falls back to the
ancestry baseline, saying so; `--refresh-manifest` rewrites the region row;
`--migrations-ack` records the CS-099 read.

Everything read from a host is read-only: `cat` of the deploy sidecars,
`systemctl is-enabled` / `is-active`, and a `test -x`.

## `make bootstrap-worktree`

Runs [`scripts/dev/bootstrap-worktree.sh`](../../scripts/dev/bootstrap-worktree.sh).
Makes a fresh checkout or linked worktree able to pass `verify.sh`, and
reports **every** gap in one pass rather than one per twelve-minute run.

The two runs it would have saved on 2026-09-07 both died about ten minutes in,
in sections unrelated to the change under test:
`openapi-typescript: command not found` (`web/explorer` had no `node_modules`),
then `tsc: command not found` (`web/status` had none). Both apps are installed
here, and the check is for the executable each gate invokes, not for the
directory — a half-finished install leaves the directory present and the
binary absent.

Severity is explicit:

- **blocking** — `verify.sh` hard-fails without it. Exit is non-zero while any
  remains.
- **clearance** — `verify.sh` defers the check and ends `VERIFY INCOMPLETE`,
  while `scripts/dev/doctor.sh --profile native` (which `make prepush` and
  `cut-release.sh` both run) treats it as required. Reported with its install
  command; does not fail the script.

Missing pinned Go tools are installed by `make deps`, which owns the pins —
this script repeats no version number. `~/go/bin` is not on every shell's
PATH, and the Makefile calls `gofumpt`, `goimports` and `golangci-lint` there
by absolute path, so the survey resolves them the same way instead of through
`command -v`, which would report a false miss.

A tool present at the **wrong** version is reported and never silently
corrected: `~/go/bin` is shared with every other checkout on the machine, so
downgrading one is the operator's decision and `make deps` is the one command
that makes them all match.

`make bootstrap-worktree-check` surveys and installs nothing.

## `verify.sh` preconditions

`scripts/dev/verify.sh` now resolves its preconditions in its first five
seconds and stops there if any is missing, listing all of them at once and
pointing at `make bootstrap-worktree`. Nothing about **what** any check
asserts changed — each condition in the preamble is the same condition as the
section that depends on it, so a gap named there is a gate that would have
failed later, and a gap absent there is not.

The preamble also prints which checks **will** defer, so the end state is
known at the start. Under `VERIFY_FAIL_ON_SKIP=1` — what `make prepush` sets —
a deferrable tool is blocking, and the preamble treats it as such: the same
verdict, ten minutes sooner.

`VERIFY INCOMPLETE: 1 check(s) deferred` with exit 1 at the end of a macOS run
is **by design**, not a defect. The deploy migrations-sync self-test runs an
ansible task file whose `unarchive --diff` needs GNU tar, and macOS ships
bsdtar. Use `VERIFY_PROFILE=container`, or let CI's ansible-check job run it.
`make prepush` is the command that issues push clearance.

## `cut-release.sh` preconditions

Branch, clean working tree and origin-sync are one question — is this checkout
in a state a release can be cut from? — and they are evaluated as a set before
anything else, with every failure named and the first line of output naming
the failing precondition. Asking them one at a time cost a round trip per
answer: fix the branch, re-run, discover the tree is dirty, re-run, discover
you are behind origin.

## See also

- [deploy-workflow.md](deploy-workflow.md) — what `deploy.yml` does once dispatched
- [deploy-config-apply.md](deploy-config-apply.md) — the apply procedure per surface
- [deployed-versions.md](deployed-versions.md) — the sidecars the baseline is read from
- [release-process.md](release-process.md) — the runbook `cut-release.sh` implements
- [rollback.md](rollback.md) — why a rollback is not a deploy dispatch
