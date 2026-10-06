---
title: Operations that succeed at doing nothing — verdict helpers and read budgets
last_verified: 2026-09-07
status: living procedure
---

# Operations that succeed at doing nothing

A gate that never ran reports the same thing as a gate that passed:
nothing. This page covers the shell helpers in `scripts/ops/ops-verdict.sh`
and the API read-count budgets in `internal/api/v1/read_budget_test.go`.

Shapes that have each produced a confident wrong answer: a wait loop on a
`Type=oneshot` unit that fell through and quoted the previous run's
result; `Result=success` on a unit that never executed; a backgrounded
`verify.sh` that exited 0 without `ALL CHECKS PASSED` (missing
toolchain); a SQL disarm matching zero rows; an ansible path that skipped
its verdict step. In the API, a handler went from 1 reader call to 24
under an unchanged 8 s deadline and every gate passed, because fixtures
answer in microseconds.

## Part 1 — `scripts/ops/ops-verdict.sh`

A sourced library: sets no shell options, every function returns rather
than exits. Self-test (stubbed `systemctl`, starts nothing):
`bash scripts/ops/ops-verdict-test.sh`, run by `scripts/dev/verify.sh`.

```sh
. scripts/ops/ops-verdict.sh
```

### Why `systemctl is-active` cannot be waited on

```sh
while systemctl is-active --quiet "$unit"; do sleep 30; done   # WRONG
systemctl show "$unit" -p Result                               # …previous run
```

`is-active` exits 0 only for `ActiveState=active`/`reloading`. A
`Type=oneshot` `RemainAfterExit=no` unit goes `inactive → activating →
inactive`, never `active`, so the loop makes zero iterations and reads a
`Result` from the last run. On r1 all 93 such units had
`ActiveEnterTimestampMonotonic=0`, and 41 reported `Result=success`
without ever completing a run since boot (`restore-drill-offsite.service`:
`Result=success`, `ExecMainStatus=0`, `InactiveEnterTimestampMonotonic=0`).
`Result` defaults to `success`; it is not evidence a unit ran.
`RemainAfterExit=yes` oneshots and `Type=notify` units (e.g.
`verify-archive-tier-a.service`) do pass through `active`.

### `wait_for_oneshot`

```sh
base=$(oneshot_baseline creators-rollup.service)
systemctl start --no-block creators-rollup.service
wait_for_oneshot creators-rollup.service 3600 "$base"
```

`oneshot_baseline` prints the completion token: the monotonic µs stamp of
the last finish (`InactiveEnterTimestampMonotonic`, or
`ActiveEnterTimestampMonotonic` for `RemainAfterExit=yes`); `0` = never
completed since boot. `wait_for_oneshot` polls `ActiveState` and
**refuses to report unless the token advanced past the baseline** (a
backwards stamp from a mid-wait reboot also fails closed). Omitting the
baseline takes one on entry: correct while the unit runs or is about to
start, fails closed if the run already finished.

| Exit | Meaning |
| --- | --- |
| 0 | a NEW run completed, `Result=success` |
| 1 | a NEW run completed and failed (`Result` / `ExecMainStatus` reported) |
| 2 | timed out — still `activating` |
| 3 | refused — idle, but the completion stamp did not advance |
| 4 | usage or precondition error (no unit, not loaded, no systemctl) |
| 5 | a NEW run completed with `ExecMainStatus=75` — `run-heavy-job.sh` found the lock held and skipped; the payload never ran |

A blocking `systemctl start <oneshot>` returns the job's own result and
needs none of this, except under `run-heavy-job.sh`
(`SuccessExitStatus=75`: a lock skip "succeeds"; read `ExecMainStatus`
or use the helper). Use the helper for timer-driven runs, `--no-block`,
runs started elsewhere, fire-and-forget ansible tasks and remote hosts:

```sh
OPS_VERDICT_SYSTEMCTL="ssh root@136.243.90.96 systemctl" \
  wait_for_oneshot creators-rollup.service 3600 "$base"
```

Defaults: `OPS_VERDICT_TIMEOUT=3600`, `OPS_VERDICT_POLL=5` (generous on
purpose: creators ran 963 s, sponsors 704 s).

### `gate_passed`

```sh
gate_passed /tmp/verify.out "$GATE_SENTINEL_VERIFY"
```

Greps the gate's own sentinel from its log; never consults an exit code.

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

Exit 3 exists because `scripts/ci/site-crawl-check.sh` and
`scripts/ops/dns-perimeter-check.sh` also print `ALL CHECKS PASSED`
behind a prefix. `verify.sh`'s `VERIFY INCOMPLETE: N check(s) deferred`
end state is caught by the sentinel, not by an exit code.

### `require_output` and `require_affected`

For when silence is the failure (a disarm that matched nothing, a sweep
that found no chunk):

```sh
require_output   "disarm" 1 -- psql -tAq -f disarm-returning.sql
require_affected "disarm" 1 -- psql -f disarm.sql
```

- `require_output` fails on fewer than `min_rows` non-blank lines or on
  command failure (a strict superset of the bare call), and logs
  `N row(s) of a required minimum of M`.
- `require_affected` reads the psql command tag (`UPDATE 3`, `DELETE 0`,
  `SELECT 0`, `INSERT 0 7`). Run psql **without** `-q`; no tag returns 3.
- Prefer `RETURNING` + `require_output`: a row names *what* changed.

### Runbooks still reading the wrong thing

- `docs/operations/runbooks/supply.md#stellarindex_supply_snapshot_never_initialized`
  annotates `systemctl status supply-snapshot.service` as "most recent
  run", but its alert fires exactly when there has been no run.
- Precedent to follow: `runbooks/restore-drill.md#stellarindex_restore_drill_offsite_stale` reads
  `stellarindex_restore_drill_last_success_unix` (carries its own
  freshness); AGENTS.md "Working style", `docs/contributing/procedures/verify-done.md`
  and `scripts/dev/cut-release.sh` read the sentinel from a log.

## Part 2 — read-count budgets for API handlers

`countingHistoryReader` and `countingCoverageFloorReader` are decorators
(count and delegate), so counts come from the handler's control flow and
data from the package's existing stubs.

### The construction being measured

The tests build the server as `cmd/stellarindex-api` does, with r1's
`/etc/stellarindex.toml` settings that set the walk length:

- `trades.usd_pegged_classic_assets` = Circle's classic USDC (the
  package's `testUSDCIssuer`);
- `[supply.sac_wrappers]` mapping that peg's SAC back to it, so
  `canonical.AssetAliases` returns two forms. The contract id is derived
  in the test and compared to the deployed literal.

Without the registry the request costs 21 reads, not 24; both are pinned.
The reader is wrapped in `v1.NewCachedHistoryReader` at production's 2 m
TTL, which caches only `LatestTradePerSource`; wrapped and unwrapped
counts are asserted equal (if a chart method becomes cached, re-derive
every budget).

### The pinned numbers

Cold (store answers nothing; where the 8 s ceiling is spent, and common:
59 of the 60 largest assets are carried entirely by a proxy read):

| Request | Reader calls | Method | Floor probes |
| --- | --- | --- | --- |
| `/v1/chart?asset=native&quote=fiat:USD&timeframe=24h` | 24 | `HistoryPointsInRange` | 7 |
| …`&price_type=twap` | 24 | `TWAPPointsInRange` | 0 |
| `/v1/chart?asset=native&quote=crypto:USDT&timeframe=24h` | 3 | `HistoryPointsInRange` | 1 |
| `/v1/history/since-inception?asset=native&quote=fiat:USD&granularity=1d` | 24 | `HistoryPoints` | 0 |
| `/v1/ohlc?base=native&quote=fiat:USD&interval=1h` | 24 | `OHLCSeries` | 8 |

24 = 3 base spellings (`native`, `crypto:XLM`, XLM's SAC) × 8 `fiat:USD`
quote spellings (literal, classic peg, five abstract USD backers, the
peg's SAC). A crypto quote has no proxy list, hence 3.

Warm (literal pair covers the window; the walk stops at its first read):
`/v1/chart?…&timeframe=24h` (vwap and twap) = 1 reader call, 0 floor
probes. Pinning both ends catches a change that breaks the early stop.

Floor probes are counted separately: a second fan-out, consulted only
when the serving read came back empty, i.e. exactly when the full walk
is paid.

### Changing one of these numbers

They are measurements, not targets. Raising one is allowed; raising one
silently is not. The only runtime guard is `chartWalkBudget` (2 s wall
clock), which a fixture never exhausts: re-measure against the deployment
before moving a constant and record what the new number buys.
