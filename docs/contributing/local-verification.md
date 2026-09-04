---
title: Local verification before CI
last_verified: 2026-09-04
status: living doc
---

# Local verification before CI

CI is the protected, independent authority. Local verification exists to make
its result predictable before a push, not to replace branch protection or
GitHub-hosted checks.

## The workflow

Use the smallest loop while editing, then verify one committed candidate:

```sh
make fix                 # only when formatting/generated output needs updating
make check               # fast, read-only feedback while editing
git diff --cached --stat # inspect exactly what will be committed
git commit
make prepush             # clean committed HEAD, then push once
```

`make prepush` refuses a dirty worktree. It exports committed `HEAD` to a
temporary checkout, so generators and formatters cannot repair the candidate
while judging it. Range-sensitive checks still inspect the real Git history.

## Capability profiles

| Profile | Command | Intended use | Push clearance |
|---|---|---|---|
| Portable | `make check` | Any supported workstation with Go and repo tools | No |
| Automatic | `make prepush` | Default; chooses the container when Docker is available, otherwise native | Yes, with `ALL REQUIRED CHECKS PASSED` |
| Native | `make prepush VERIFY_PROFILE=native` | A machine with every tool reported by the native doctor | Yes |
| Container | `make prepush VERIFY_PROFILE=container` | Pinned Debian/Go/Node tooling on Docker Desktop or Docker Engine | Yes |
| Full | `make prepush VERIFY_PROFILE=full` | Container lane plus integration tests regardless of changed paths | Yes |

Run `make doctor VERIFY_PROFILE=<profile>` to see requirements and versions.
A missing capability fails a clearance profile. `make verify` remains the
underlying sequential gate, but it reports `VERIFY INCOMPLETE` if any check is
deferred; only its literal `ALL CHECKS PASSED` line is a pass.

## macOS and other machines

On macOS, `VERIFY_PROFILE=auto` normally selects Docker. The verifier image is
built for Docker's native architecture (`linux/arm64` on Apple Silicon and
`linux/amd64` on Intel and most Linux hosts); it does not force x86 emulation.
After the first run Docker keeps the image, and five named volumes carry the
Go build cache, Go modules, the pnpm store and both `node_modules` trees
between runs: `stellarindex-verify-go-build`, `stellarindex-verify-go-mod`,
`stellarindex-verify-pnpm`, `stellarindex-verify-explorer-modules` and
`stellarindex-verify-status-modules`. `make prepush-clean` removes all five.

Linux maintainers with GNU tar and the full toolchain can use the native lane.
Machines without Docker can still run `make check`; they must not describe that
result as pre-push clearance. This keeps contribution possible on modest
machines without lowering the maintainer standard.

## Integration selection

With `VERIFY_INTEGRATION=auto` (the default), pre-push adds parallel
Docker-backed integration shards when the commit range touches:

- `migrations/`, `test/fixtures/`, or `test/integration/`
- `internal/storage/`, `internal/pipeline/`, `internal/projector/`
- `internal/dispatcher/` or `internal/sources/`

Use `VERIFY_INTEGRATION=always` when behavior crosses those boundaries without
changing them. `VERIFY_INTEGRATION=never` is reserved for the isolated Linux
lane (`make prepush-linux`); that command does not claim complete clearance.
Adjust parallelism with `LOCAL_INT_SHARDS=4` when Docker has enough resources.

## What remains CI-only

CI still provides a clean independent runner, event and permission context,
the full integration matrix on code PRs, coverage reporting, and protected
status checks. External-service, deployment, load and live-data probes remain
explicit procedures rather than ordinary pre-push work. A local pass predicts
the deterministic build and test lanes; it cannot certify GitHub or production
state.

## Measuring the workflow

Do not claim a speed-up without recording the command and elapsed seconds:

```sh
/usr/bin/time -p make check
/usr/bin/time -p make prepush
/usr/bin/time -p make prepush VERIFY_PROFILE=full LOCAL_INT_SHARDS=4
```

The first container build includes tool downloads. Compare warm-cache runs
separately and state the machine architecture and shard count.
