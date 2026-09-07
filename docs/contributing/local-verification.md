---
title: Local verification before CI
last_verified: 2026-09-07
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

## A new or changed test is run more than once

A single green run says the test passed once, on an idle machine, at one
moment of the wall clock. Two defects that cost a day each in September 2026
were invisible to exactly that check and visible to these two, so a new or
changed test is put through both before it is pushed:

```sh
go test ./internal/<pkg>/ -count=5           # repetition
bash scripts/ci/<name>-test.sh               # ... ten times, for a shell test
pnpm --dir web/explorer test                 # ... five times, for the web suite
```

Repetition finds a test that carries state between runs or that reads the
clock. Six Go tests asserted absolute values of process-global Prometheus
vectors that nothing resets, so each was asserting that the binary had run it
exactly once; `-count=2` failed all six. `zfs-snapshot-test.sh` asserted that
two snapshot calls landed in the same minute, which is a property of when the
run started, not of the script — it failed one run in three.

Contention finds a test whose mock is keyed on arrival order rather than on
what was asked. `fetchPrice.test.ts` drove its mock from a call counter while
the subject raced two triangulation legs through `Promise.all`, so under load
the wrong leg was marked and the test failed pointing at an unrelated patch.
Run the suite against a loaded machine — a background loop per core is enough
— rather than only against an idle one:

```sh
for i in $(seq 1 "$(sysctl -n hw.ncpu)"); do (while :; do :; done) & done
```

Both classes fail with the subject behaving perfectly, which is what makes
them expensive: the failure names whatever was being changed at the time.

last_verified: 2026-09-07
## Changed-file lints, before a commit exists

`make lint-changed` is a dispatcher over the lints in `scripts/ci/`, not a new
lint: it looks at the diff — staged files, or `origin/main...HEAD` when
nothing is staged — and for each changed file's type runs only the gates that
apply, scoped to the changed files where the gate accepts a file list,
cheapest first.

| changed file | runs |
|---|---|
| `*.sh` | `bash -n`; `lint-shell-sigpipe` scoped to the pipefail subset; `shellcheck -x`; a `*-test.sh` is also run, last |
| `*.go` | `gofumpt -l`, `goimports -l` on the files; `lint-lexicon`, `lint-i128`, `lint-imports` (whole tree); `lint-http-timeouts` scoped to the touched package dirs; `go vet` and `go build` on those packages |
| `.github/workflows/*.yml` | `lint-actions-pinning` scoped; `actionlint`; `zizmor --offline` |
| `migrations/*.sql` | `lint-migrations`, `lint-migration-immutability`, `lint-migration-commands`, `lint-migration-compat` |
| `*.go`/`*.sql` naming a duplicate-bearing table | `lint-lake-dedup` |
| `scripts/dev/verify.sh`, `.github/workflows/ci.yml` | `check-verify-parity` |
| `*.md` | nothing sub-5 s exists for markdown; the run reports it as **deferred**, naming `lint-doc-links` and `lint-docs`, rather than silently reading as linted |

A missing optional tool (`shellcheck`, `actionlint`, `zizmor`) defers its own
step, counted in the summary line, and does not fail the run — this is the
loop before a commit exists; `scripts/dev/verify.sh` remains the gate.
`gofumpt`/`goimports` are the exception: `make fmt` calls them by absolute
path, so their absence is a broken checkout (`make deps`), and the step
fails.

Measured 2026-09-07, this machine: `scripts/dev/lint-changed-test.sh` (its
own fixture-repository self-test) — 99 passed, 0 failed, in 3.4 s. A diff
touching one `.sh` file with a planted defect (`… | head -1` under
`set -o pipefail`), one `.go` file and one `.md` file: 3.07 s, exit 1, naming
the exact file and line of the sigpipe defect. An empty diff: well under a
tenth of a second.

`make hooks` installs `lint-changed.sh --staged` as a pre-commit hook —
**opt-in**, never forced on a clone; `git commit --no-verify` skips it for
one commit, and `make hooks-remove` takes it out. It honours an existing
`core.hooksPath` and refuses to overwrite a pre-commit hook it did not write,
printing the one line to add to it instead.

## Which gate for a given change

Gate depth follows blast radius; state which row a diff falls in before
running anything, and do not run a deeper row than the diff needs:

| change touches | gate |
|---|---|
| `internal/storage/**`, `internal/pipeline/**`, `internal/sources/**`, `internal/api/**`, `migrations/**` | `make prepush` with integration shards (the existing integration-policy above already routes these there) |
| other `cmd/**`, `internal/**` | `make prepush` — container lane, shards as policy decides |
| `scripts/**`, `.github/**`, `configs/**`, `docs/**`, `web/**` tests, `*_test.go` only | `make verify` — the same sequential gate, no container, no clean-tree export — then push |
| `CHANGELOG.md`, `*.md` only | `make lint-changed` + `./scripts/ci/lint-docs.sh`, then push |

A one-line shell or doc edit that only ever needed the third or fourth row
does not owe a 15-20 minute `prepush` cycle; running one anyway is not extra
safety, it is minutes spent re-confirming what `make verify` already covers.

## `verify.sh` runs cheapest first

`scripts/dev/verify.sh` is reordered so cost rises monotonically: every
section measured under five seconds now runs in one block immediately after
the preconditions check, before `make fmt` and the Go build — the same
assertions, the same sections, the same `ALL CHECKS PASSED` / `VERIFY
INCOMPLETE` sentinel, only WHEN each one is reached. `make lint-changed`'s
dispatch table above is exactly this same set of gates, scoped to a diff
instead of the whole tree.

The measurement that forced it, 2026-09-07: `lint-shell-sigpipe.sh` runs in
2.5-2.8 s standalone and sat at section ~95 of ~103, reached only after
about ten minutes of Go build, doc lints and web builds — so a one-line
shell edit that tripped it was told so at minute ten, not second three. On
the reordered script, measured the same day on the same machine, it answers
in **about 26 seconds** from `bash scripts/dev/verify.sh`'s first line, and
roughly sixty other sub-5-second checks finish alongside it in **about a
minute** combined, before the first `make fmt`/`go vet`/`go build` step
runs. Nothing here changes total wall-clock time — the same slow sections
(`Lint`, `Docs`, `Doc links self-test`, `Monitoring`, `Test`, the web
builds) still run and still dominate it; what changed is which part of the
run you wait through before a fast defect is reported.

`scripts/ci/check-verify-parity.sh` — the gate that would catch a dropped
section — stays green: the reorder was verified by diffing the full set of
section markers a real run produces before and after, and every section
that ran before still runs after, in the same conditional branches, with
nothing added or removed. The macOS `VERIFY INCOMPLETE: 1 check(s)
deferred` terminal line is unchanged and is not a failure — it is the
GNU-tar-dependent ansible self-test deferring by design, exactly as before.

## CI's own fast-fail gate and path filtering

`.github/workflows/ci.yml` runs a `preflight` job first: the sub-5-second
lints (`lint-shell-sigpipe`, `lint-lexicon`, `lint-actions-pinning`,
`lint-baseline-growth`, `lint-migration-immutability`,
`lint-go-toolchain-parity`, `gofumpt -l`, `goimports -l`, and `shellcheck` on
the diff's changed `*.sh` files) run there, before `lint`, `test`, `build`,
`integration-test-shard`, `vuln`, `web-explorer`, `web-status` and
`ansible-check` — a lint slip costs the runner-seconds `preflight` needs, not
the full matrix already in flight.

`preflight` also classifies the diff (via `dorny/paths-filter`, cross-checked
against `scripts/ci/check-change-class.sh` so the two rule copies cannot
silently drift) and every job above skips the WORK it has nothing to do for,
without ever reporting a `skipped` job conclusion itself:

| diff touches | jobs that do real work |
|---|---|
| `internal/storage/**`, `internal/pipeline/**`, `internal/sources/**`, `internal/api/**`, `migrations/**`, `test/integration/**`, `go.mod` | `integration-test-shard` (Docker) |
| any `*.go`, `go.mod`, `go.sum` | `lint`, `test`, `build`, `vuln`'s `govulncheck` step, `fuzz-smoke` |
| `web/**`, `openapi/**` | `web-explorer`, `web-status` |
| `configs/ansible/**` | `ansible-check` |

Each gated job still runs to completion — the filtering is a STEP-level
`if:` on the work inside it, not a job-level skip — so its conclusion is
always `success` or `failure`, never `skipped`. `vuln`'s `gitleaks` step is
never filtered: a leaked credential is exactly as real in a YAML or shell
diff as a Go one.

A `preflight` step also refuses a Dependabot-authored PR that touches the
`go`/`toolchain` line of `go.mod` or `engines`/`packageManager` in any
`package.json` — a language-toolchain bump is a deliberate decision, never a
grouped dependency patch (see the step's comment in ci.yml for the incident
that motivated it).

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
