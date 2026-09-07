---
title: Operations that succeed at doing nothing — verdict helpers and read budgets
last_verified: 2026-09-07
status: living procedure
---

# Operations that succeed at doing nothing

Covers `scripts/ops/ops-verdict.sh` (shell helpers) and the read-count
budgets in `internal/api/v1/read_budget_test.go`.

A gate that never ran reports the same thing as a gate that passed:
nothing. Four instances of that shape were found on 2026-09-07, and all
four had produced a confident, wrong answer at least once:

1. a wait loop on a `Type=oneshot` unit that exited before the unit had
   started, then quoted a result from hours earlier;
2. a `Result=success` read from a unit that had never executed;
3. a backgrounded `verify.sh` that reported exit 0 twice while its
   `ALL CHECKS PASSED` line was absent — it had died on a missing
   toolchain;
4. a SQL disarm that matched zero rows and exited 0, and an ansible
   apply path that skipped its own verdict step and counted as success.

The API surface has the same class in a different dialect: a handler
went from one reader call to twenty-four under an unchanged 8 s
deadline, and every gate passed, because fixtures answer in
microseconds and a fan-out costs zero test-milliseconds.

## Part 1 — `scripts/ops/ops-verdict.sh`

A sourced library. It sets no shell options (a `set -euo pipefail` in a
sourced file changes the caller's shell) and every function returns
rather than exits.

```sh
. scripts/ops/ops-verdict.sh
```

Self-test: `bash scripts/ops/ops-verdict-test.sh` — 39 assertions
against a stubbed `systemctl`, starting nothing and restarting nothing.

### Why `systemctl is-active` cannot be waited on

```sh
while systemctl is-active --quiet "$unit"; do sleep 30; done   # WRONG
systemctl show "$unit" -p Result                               # …previous run
```

`is-active` exits 0 only for `ActiveState=active` (or `reloading`). A
`Type=oneshot` unit with `RemainAfterExit=no` goes
`inactive → activating → inactive` and is **never** `active`, so the
loop makes zero iterations and falls straight through to a `Result` that
still belongs to the last run.

Measured on r1, 2026-09-07:

| Fact | Value |
| --- | --- |
| Loaded `Type=oneshot` `RemainAfterExit=no` service units | 93 |
| …whose `ActiveEnterTimestampMonotonic` is 0 (never once `active`) | **93 of 93** |
| …that have never completed a run since boot and still report `Result=success` | **41 of 93** |

`creators-rollup.service` ran 11:32:26 → 11:48:29 CEST that day — 16
minutes — and its `ActiveEnterTimestamp` is still empty. Every
`RemainAfterExit=yes` oneshot on the same host (`apparmor.service`,
`disable-thp.service`, …) carries a populated one, which is the contrast
that makes the rule specific rather than folkloric.

`Result` is not evidence that a unit ran. It is `success` by default:
`restore-drill-offsite.service` on r1 reports `Result=success`,
`ExecMainStatus=0` and `InactiveEnterTimestampMonotonic=0` — a
success verdict for zero executions.

`Type=notify` units are unaffected: `verify-archive-tier-a.service`
does pass through `active` and carries a real `ActiveEnterTimestamp`.

### `wait_for_oneshot`

```sh
base=$(oneshot_baseline creators-rollup.service)
systemctl start --no-block creators-rollup.service
wait_for_oneshot creators-rollup.service 3600 "$base"
```

`oneshot_baseline` prints the unit's completion token — the monotonic
microsecond stamp of the moment it last finished
(`InactiveEnterTimestampMonotonic`, or `ActiveEnterTimestampMonotonic`
for a `RemainAfterExit=yes` unit). `0` means it has not completed once
since boot.

`wait_for_oneshot` polls `ActiveState`, and **refuses to report a result
unless that token has advanced past the baseline**. That refusal is the
load-bearing part: nothing else on the unit separates this run from the
last one, and a monotonic stamp that went backwards (a reboot mid-wait)
fails the same way, closed.

Omitting the baseline takes one on entry, which is correct while the
unit is running or about to start, and fails closed when the run had
already finished.

| Exit | Meaning |
| --- | --- |
| 0 | a NEW run completed, `Result=success` |
| 1 | a NEW run completed and failed (`Result` / `ExecMainStatus` reported) |
| 2 | timed out — still `activating` when the timeout expired |
| 3 | refused — idle, but the completion stamp did not advance |
| 4 | usage or precondition error (no unit, not loaded, no systemctl) |

A distinct code per outcome, so a caller's `if` cannot inherit a meaning
that was assigned to something else.

A blocking `systemctl start <oneshot>` needs none of this — it waits and
returns the job's own result. The helper is for the runs that cannot be
started that way: timer-driven runs, `--no-block`, a run started in
another session, an ansible task that fires and moves on, and any poll
of a remote host. For those, point the helper at the host:

```sh
OPS_VERDICT_SYSTEMCTL="ssh root@136.243.90.96 systemctl" \
  wait_for_oneshot creators-rollup.service 3600 "$base"
```

Defaults: `OPS_VERDICT_TIMEOUT=3600`, `OPS_VERDICT_POLL=5`. The timeout
is generous on purpose — measured run times that day were 963 s
(creators) and 704 s (sponsors), and a wait helper whose own default is
the thing that fails gets deleted by the next operator.

### `gate_passed`

```sh
gate_passed /tmp/verify.out "$GATE_SENTINEL_VERIFY"
```

Greps the gate's own sentinel out of its log and never consults an exit
code. The three this tree emits are exported as names so a caller cannot
misspell one into a grep that can never match:

| Constant | Literal | Emitted by |
| --- | --- | --- |
| `GATE_SENTINEL_VERIFY` | `ALL CHECKS PASSED` | `scripts/dev/verify.sh` |
| `GATE_SENTINEL_PREPUSH` | `ALL REQUIRED CHECKS PASSED` | `scripts/dev/prepush.sh` |
| `GATE_SENTINEL_CONTAINER` | `ALL LINUX CHECKS PASSED` | `docker/verify/entrypoint.sh` |

| Exit | Meaning |
| --- | --- |
| 0 | present exactly `expected_count` times (default 1) |
| 1 | absent — the gate never reached its verdict |
| 2 | the log is missing, unreadable or empty |
| 3 | present a different number of times than expected |

Exit 3 exists because `ALL CHECKS PASSED` is emitted by three scripts,
not one: `scripts/dev/verify.sh` prints it bare, while
`scripts/ci/site-crawl-check.sh` and `scripts/ops/dns-perimeter-check.sh`
print it behind their own prefix. A substring match over a combined log
cannot tell them apart, so an unexpected count is surfaced rather than
accepted.

`verify.sh` also has a non-failing incomplete state — it ends
`VERIFY INCOMPLETE: N check(s) deferred` — which the sentinel catches
and an exit code does not distinguish from a toolchain crash.

### `require_output` and `require_affected`

For the class where silence is the failure: a disarm that matched no
job, a sweep that found no chunk, a reconcile that compared nothing.

```sh
require_output   "disarm" 1 -- psql -tAq -f disarm-returning.sql
require_affected "disarm" 1 -- psql -f disarm.sql
```

`require_output` fails when the command produced fewer than `min_rows`
non-blank lines, and also when the command itself failed — a strict
superset of the bare invocation, so substituting it can never weaken a
caller. It prints a self-accounting line (`N row(s) of a required
minimum of M`) so a run that did nothing says so in its own log.

`require_affected` reads the psql command tag (`UPDATE 3`, `DELETE 0`,
`SELECT 0`, `INSERT 0 7`) for statements that cannot `RETURNING`. Run
psql **without** `-q` so the tag is printed; output carrying no tag at
all returns 3, because nothing can be proven from it.

Prefer `RETURNING` plus `require_output` where the statement allows it —
a returned row names *what* changed, not just how much.

### Where the runbooks currently say the wrong thing

These are unchanged by this work and are listed so they can be fixed
deliberately rather than found again:

- `docs/operations/runbooks/galexie-archive-tip-lag.md` lines 45-46 read
  `systemctl show galexie-archive-fill.service -p Result` under the
  heading "last run, exit code", with no timestamp beside it. That unit
  is `Type=oneshot` `RemainAfterExit=no` on r1, so between the manual
  `systemctl start` at line 93 and the unit's completion, the line
  reports the previous run.
- `docs/operations/runbooks/supply-snapshot-never-initialized.md` line 53
  annotates `systemctl status supply-snapshot.service` as
  "most recent run". The alert this runbook serves fires exactly when
  there has been no run, and 41 of r1's 93 such units report
  `Result=success` having never executed.
- `docs/operations/runbooks/restore-drill-offsite-stale.md` verifies an
  off-site drill through the textfile metric's
  `stellarindex_restore_drill_last_success_unix`, which is correct and
  worth copying — it carries its own freshness. The unit beside it is
  the live example of why the systemd properties do not:
  `Result=success`, zero runs since boot.

Already correct, and the precedent to follow: `AGENTS.md` lines 138-139
("never report a gate as passing on its exit code alone"),
`docs/contributing/procedures/verify-done.md` line 20, and
`scripts/dev/cut-release.sh` line 201, which all read the sentinel out
of a log file.

### Not yet wired into `verify.sh`

`scripts/ops/ops-verdict-test.sh` is not called from
`scripts/dev/verify.sh`, which is where `zfs-snapshot-test.sh`,
`restore-drill-test.sh` and `restore-drill-run-test.sh` are registered.
Adding it needs one line in that file:

```sh
echo "=== ops-verdict helpers self-test ===" && bash scripts/ops/ops-verdict-test.sh
```

Until that lands the self-test runs only when invoked directly.

## Part 2 — read-count budgets for API handlers

`internal/api/v1/read_budget_test.go` makes reader-call counts
assertable. `countingHistoryReader` and `countingCoverageFloorReader`
are decorators, not mocks: they count and delegate, so the counts come
from the handler's own control flow and the data still comes from the
package's existing stubs.

### The construction being measured

A budget measured against the wrong construction is worth nothing, so
the tests build the server the way `cmd/stellarindex-api` does for these
surfaces. Two pieces of operator configuration decide the walk's length,
both taken from r1's `/etc/stellarindex.toml`:

- `trades.usd_pegged_classic_assets` — exactly one entry, Circle's
  classic USDC, which is already this package's `testUSDCIssuer`;
- `[supply.sac_wrappers]` — the entry mapping that peg's SAC contract
  back to the classic asset, which makes `canonical.AssetAliases` return
  two forms of the peg instead of one. The contract id is *derived* from
  the classic asset in the test and compared against the deployed
  literal, so the fixture cannot drift into a plausible-looking fiction.

Without the registry the same request issues 21 reads rather than 24 —
which is the state every other test in the package runs in. Both numbers
are pinned, so the gap between a bare unit test and the deployment stays
an asserted fact.

The reader is also wrapped in `v1.NewCachedHistoryReader` at
production's 2 m TTL. That wrapper caches `LatestTradePerSource` only
and passes every other method through; the wrapped and unwrapped counts
are asserted equal rather than assumed so, because if a chart method
ever becomes cached there, every budget below has to be re-derived.

### The pinned numbers

Cold — the store answers nothing, which is the state the 8 s ceiling is
spent in and the common one here (59 of the 60 largest assets are
carried entirely by a proxy read, so every proxy read runs on an empty
merge):

| Request | Reader calls | Method | Floor probes |
| --- | --- | --- | --- |
| `/v1/chart?asset=native&quote=fiat:USD&timeframe=24h` | 24 | `HistoryPointsInRange` | 7 |
| …`&price_type=twap` | 24 | `TWAPPointsInRange` | 0 |
| `/v1/chart?asset=native&quote=crypto:USDT&timeframe=24h` | 3 | `HistoryPointsInRange` | 1 |
| `/v1/history/since-inception?asset=native&quote=fiat:USD&granularity=1d` | 24 | `HistoryPoints` | 0 |
| `/v1/ohlc?base=native&quote=fiat:USD&interval=1h` | 24 | `OHLCSeries` | 8 |

Where 24 comes from: 3 spellings of the base (`native`, `crypto:XLM`,
and XLM's SAC — the unconditional three-way split) times the quote
spellings the walk reads, which for a `fiat:USD` quote are the literal
one, the operator's classic peg, five abstract USD backers, and the
peg's SAC wrapper. A crypto quote has no proxy list at all, which is why
it costs 3.

Warm — the literal pair holds a series covering the requested window,
so the walk stops at its first read:

| Request | Reader calls | Floor probes |
| --- | --- | --- |
| `/v1/chart?…&timeframe=24h` (vwap and twap) | 1 | 0 |

That 1 and that 24 are the two ends of the regression this file exists
for. Pinning only the cold number would let a change that broke the
early stop — making every warm request pay the full walk — pass
unnoticed.

The coverage-floor probe is counted separately because it is a *second*
fan-out on the same request: a floor is consulted only when the serving
read came back empty, which is exactly the case that also pays the full
walk.

### Changing one of these numbers

They are measurements, not targets. Raising one is allowed; raising one
silently is not. The walk's only runtime guard is `chartWalkBudget`, two
seconds of wall clock, which a fixture can never exhaust — so if a
change moves a count, re-measure the request against the deployment
before moving the constant, and record what the new number buys.
