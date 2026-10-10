---
title: ClickHouse ops-batch profile — heavy stellarindex-ops jobs run low-priority
last_verified: 2026-08-29
status: living procedure
---

# ClickHouse ops-batch profile — heavy stellarindex-ops jobs run low-priority

Companion to [ADR-0048](../adr/0048-serve-by-query-shape.md) D4's
`api_serving` profile, which protects the API's reads. This one protects
everything else on the host (the aggregator's supply refresher, the indexer's
live sink) from batch jobs by giving `stellarindex-ops` its own LOW-priority
ClickHouse identity.

## Why

A `stellarindex-ops ch-rebuild -sep41` dry-run over 2M ledgers under
`run-heavy-job.sh` (`CPUWeight=50`, `IOWeight=50`, `MemoryMax=20G`) drove r1
load to 12.9 and starved the supply refresher;
`stellarindex_aggregator_supply_refresh_error_dominant` fired for all 39
watched contracts within 3 minutes. The cgroup caps bound the ops *process*;
the contention was *inside* `clickhouse-server`, where the job's heavy-`FINAL`
scans and the aggregator's reads both ran as the unauthenticated `default`
user, so the scheduler could not tell which to yield.

## What the profile is

`configs/ansible/roles/archival-node/tasks/20-clickhouse-serving-profile.yml`
provisions, alongside `api_serving`, a settings profile + user `ops_batch`
(`/etc/clickhouse-server/users.d/ops-batch.xml`):

| knob | value | why |
|---|---|---|
| `priority` | 100 (`clickhouse_ops_batch_priority`) | Lower number wins. `api_serving` is 1, every un-opted-in connection (indexer sink, aggregator readers) is 0, so both beat ops jobs under CPU contention. Pinned `<readonly/>` in `<constraints>` so a per-query `SETTINGS` clause cannot promote itself. |
| `os_thread_priority` | 5 | Linux nice on the query's threads; positive so a batch scan also yields to background merges (nice 0). |
| `max_threads` / `max_memory_usage` / `max_concurrent_queries_for_user` | 2 / 8 GiB / 8 | DEFAULTS, not ceilings (`readonly=0`): openers that set their own class limits (`gate.go`'s heavy-`FINAL` class: 3 threads / 10 GiB / external-sort spill) still win. They bound every ops read that sets nothing. |
| `readonly` | 0 | Ops jobs write (Sink, participant / account-movements / entry-change inserts). |
| no `max_execution_time` | — | A full-history `FINAL` stream legitimately runs for hours; openers that want a ceiling set one. |

Same discipline as `api_serving`: vault password
(`vault_clickhouse_ops_batch_password`, asserted non-empty), SHA-256 digest
only in the drop-in, loopback-only, pinned to the `stellar` database,
`access_management=0`.

## How jobs pick it up

`internal/storage/clickhouse/ops_auth.go` resolves the identity for every
ops-side connection builder in that package (`openRead`, the `Sink`, the
participant / account-movements / entry-change writers, and the
no-credential `NewExplorerReader` / `NewSupplyReader` constructors) from:

```
STELLARINDEX_CLICKHOUSE_OPS_USER=ops_batch
STELLARINDEX_CLICKHOUSE_OPS_PASSWORD=<vault>
```

`09-minio.yml` templates them into `/etc/default/stellarindex-ops` (mode
`0640 root:stellarindex`), only when `clickhouse_ops_batch_profile_enabled`
is true AND the vault password exists, so a partial apply never hands jobs a
credential ClickHouse does not know. Unset = ClickHouse `default` user.
Environment, never argv: argv is world-readable via `/proc` and lands in the
journal through `run-heavy-job.sh`'s `systemd-run` line.

Three launch paths reach that file:

- **`run-heavy-job.sh` imports the pair itself** (root and non-root, before
  the `systemd-run` scope), reading ONLY those two variables from
  `/etc/default/stellarindex-ops` when the caller has not set them. This is
  the path that matters: the heavy-job runbooks
  ([sep41-mint-recovery](sep41-mint-recovery.md),
  [usd-volume-rederive](usd-volume-rederive-2026-08.md),
  [fx-history-missing](runbooks/data-freshness.md#fx-history-missing),
  [lcm-cache-tiering](lcm-cache-tiering.md)) source
  `/etc/default/stellarindex`, not `-ops`. Every launch prints one stderr
  line naming the identity, or `WARNING ... CH 'default' user at SERVING
  priority` when the pair is absent.
- The `verify-archive-tier-*` / `ch-schema-*` / `restore-drill` units'
  `EnvironmentFile=/etc/default/stellarindex-ops`.
- Interactive `set -a; source /etc/default/stellarindex-ops; set +a` for
  one-off reads outside the wrapper.

`config-assertions.sh` (hourly, `stellarindex_config_assertion_ok`) asserts
the two halves moved together (the `ops_batch` drop-in in `users.d` implies
the pair in `/etc/default/stellarindex-ops`) and that
`/etc/default/stellarindex` never carries it.

**Never copy these two variables into `/etc/default/stellarindex`.** The
ansible-templated indexer / aggregator / API units source it, and the
package-level seam would demote the live sink and supply refresher to
lowest priority.

**deploy/systemd self-hosts:** `deploy/systemd/stellarindex-{indexer,aggregator,api}.service`
source `/etc/default/stellarindex-ops` as their ONLY env file. Writing the
pair there is safe because each of those three units carries

```
UnsetEnvironment=STELLARINDEX_CLICKHOUSE_OPS_USER STELLARINDEX_CLICKHOUSE_OPS_PASSWORD
```

which systemd applies AFTER every `Environment=` / `EnvironmentFile=`
(systemd.exec(5); systemd ≥ 235; Ubuntu 22.04/24.04 ship 249/255). Keep that
line in hand-rolled units; the batch units deliberately do NOT strip it.
`TestOpsBatchIdentityNeverReachesLiveDaemons`
(`internal/storage/clickhouse/ops_auth_systemd_test.go`) pins both halves.
Alternatively, export the pair in the launching shell, or point
`run-heavy-job.sh` at a private env file via `HEAVY_JOB_OPS_ENV=<path>`.

## Operator apply (run `--check --diff` first)

`clickhouse_ops_batch_profile_enabled` defaults to **false**: enabling it
asserts the vault password, and the weekly `ansible-drift.yml` `--check`
against r1 runs with the real vault, so a default of true would keep it red
until every inventory had the password. Enable per host, in one PR:

1. `ansible-vault edit inventory/r1.secrets.yml` → add
   `vault_clickhouse_ops_batch_password: "<generated>"`, generated as hex
   (`openssl rand -hex 32`) so it survives the interactive `source` path,
   where the shell expands `$`, quotes and `#`. Set
   `clickhouse_ops_batch_profile_enabled: true` in `inventory/r1.yml` in the
   same PR (the assert fails on enable-without-password, by design).
2. Apply the CH user, the env file AND the heavy-job wrapper together (a user
   without the env is inert; an env without the user breaks the next ops
   job; `heavy-job-wrapper` re-renders `/usr/local/sbin/run-heavy-job.sh` so
   it imports the pair, without a full `--tags stellarindex` pass):

   ```
   ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
     --tags clickhouse-ops-batch-profile,minio,heavy-job-wrapper --check --diff
   ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
     --tags clickhouse-ops-batch-profile,minio,heavy-job-wrapper
   ```

   **Nothing restarts.** `users.d` hot-reloads; the task runs `SYSTEM RELOAD
   CONFIG`, then asserts `system.users` / `system.settings_profiles` each
   carry exactly one `ops_batch` (retried over the reloader's poll; skipped
   under `--check`). If the recap shows a `Restart clickhouse-server` handler
   for these tags, stop: that is a regression.
3. Verify on r1. **Not yet exercised against a live ClickHouse**: the
   `<constraints><priority><readonly/>` block and the `priority` /
   `os_thread_priority` behaviour are unproven. A malformed drop-in does not
   take the server down (the reloader keeps the previous users config and
   logs the error); the verify task then fails with `0,1` / `0,0` instead of
   `1,1`. Keep the drop-in-free state one `rm users.d/ops-batch.xml` away:

   ```
   clickhouse-client --port 9300 --user ops_batch --password "$(sed -n 's/^STELLARINDEX_CLICKHOUSE_OPS_PASSWORD=//p' /etc/default/stellarindex-ops)" -q "SELECT currentUser()"
   run-heavy-job.sh ops-batch-probe stellarindex-ops ch-gate -config /etc/stellarindex.toml -ch-addr 127.0.0.1:9300 -from <n> -to <n>   # any ops read; stderr names the identity
   clickhouse-client --port 9300 -q "SELECT user, priority, query_id FROM system.processes WHERE user = 'ops_batch'"
   ```

   From another shell, confirm the aggregator's queries still run as
   `default`. Acceptance test: re-run the incident workload (`ch-rebuild
   -sep41` dry-run per [sep41-mint-recovery](sep41-mint-recovery.md), from a
   shell that sourced `/etc/default/stellarindex`, NOT `-ops`) and
   `stellarindex_aggregator_supply_refresh_error_dominant` stays quiet.

## Rule

Every heavy `stellarindex-ops` job (ch-rebuild, ch-backfill,
classic-movements-backfill, verify-*, gates, replays) runs under
`run-heavy-job.sh`, which supplies the identity. The cgroup caps bound the
process; the ops-batch identity bounds its effect on ClickHouse's other
users. If the wrapper's first stderr line says `WARNING ... CH 'default'
user`, stop and apply the profile before a multi-hour scan.

Heavy shell scripts that call `clickhouse-client` directly
(`scripts/ops/ch-supply-flows-seed.sh`, `ch-live-catchup.sh`,
and `d3-lecur-v2-rebuild.sh`) export `STELLARINDEX_CLICKHOUSE_OPS_USER` and its password as
`CLICKHOUSE_USER`/`CLICKHOUSE_PASSWORD` when set, never on argv.
`scripts/ops/ch-ops-user-test.sh` (CI) holds them to that contract.
