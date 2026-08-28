---
title: ClickHouse ops-batch profile — heavy stellarindex-ops jobs run low-priority
last_verified: 2026-08-28
status: living procedure
---

# ClickHouse ops-batch profile — heavy stellarindex-ops jobs run low-priority

Companion to [ADR-0048](../adr/0048-serve-by-query-shape.md) D4's
`api_serving` profile. That profile protects the API's per-request
reads; this one protects everything ELSE on the host (the aggregator's
supply refresher, the indexer's live sink) from a batch job, by giving
`stellarindex-ops` its own LOW-priority ClickHouse identity.

## Why (incident, 2026-08-28, r1)

A runbook-prescribed `stellarindex-ops ch-rebuild -sep41` dry-run over
2M ledgers, launched the way every heavy job is launched
(`run-heavy-job.sh`: `CPUWeight=50`, `IOWeight=50`, `MemoryMax=20G`),
drove host load to 12.9 and starved the aggregator's supply refresher.
`stellarindex_aggregator_supply_refresh_error_dominant` fired for all
39 watched contracts within 3 minutes; killing the job cleared it.

The cgroup caps bound the ops *process*. The contention was *inside*
`clickhouse-server`: the job's heavy-`FINAL` scans and the aggregator's
supply reads both authenticated as ClickHouse's unauthenticated
`default` user, so the query scheduler had no signal that one of them
was a batch job it should yield with. `api_serving` (priority 1) only
protects the API binary — nothing protected the aggregator.

## What the profile is

`configs/ansible/roles/archival-node/tasks/20-clickhouse-serving-profile.yml`
provisions, alongside `api_serving`, a settings profile + user
`ops_batch` (`/etc/clickhouse-server/users.d/ops-batch.xml`):

| knob | value | why |
|---|---|---|
| `priority` | 100 (`clickhouse_ops_batch_priority`) | CH priority queue: lower number wins. `api_serving` is 1, every un-opted-in connection (indexer sink, aggregator readers) is 0 — so both are preferred over ops jobs under CPU contention. Pinned `<readonly/>` in `<constraints>` so a per-query `SETTINGS` clause cannot promote itself. |
| `os_thread_priority` | 5 | Linux nice on the query's own threads; positive so a batch scan also yields to background merges (nice 0). |
| `max_threads` / `max_memory_usage` / `max_concurrent_queries_for_user` | 2 / 8 GiB / 8 | DEFAULTS, not ceilings (`readonly=0`): the ops openers that set their own class limits (`gate.go`'s heavy-`FINAL` class: 3 threads / 10 GiB / external-sort spill) keep winning. They bound every ops read that sets nothing. |
| `readonly` | 0 | Ops jobs write (Sink, participant / account-movements / entry-change inserts). |
| no `max_execution_time` | — | A `FINAL` stream over a full-history window legitimately runs for hours; openers that want a ceiling set one. |

Same discipline as `api_serving`: vault password
(`vault_clickhouse_ops_batch_password`, asserted non-empty), SHA-256
digest only in the drop-in, loopback-only, pinned to the `stellar`
database, `access_management=0`.

## How jobs pick it up

`internal/storage/clickhouse/ops_auth.go` resolves the identity for
every ops-side connection builder in that package — `openRead` (the
heavy-`FINAL` gate/reconcile/verify readers), the `Sink`, participant,
account-movements and entry-change writers, and the no-credential
`NewExplorerReader` / `NewSupplyReader` constructors the ops
subcommands use — from two environment variables:

```
STELLARINDEX_CLICKHOUSE_OPS_USER=ops_batch
STELLARINDEX_CLICKHOUSE_OPS_PASSWORD=<vault>
```

`09-minio.yml` templates them into `/etc/default/stellarindex-ops`
(mode `0640 root:stellarindex`) — the env file every heavy job already
sources: interactive `set -a; source /etc/default/stellarindex-ops`,
`run-heavy-job.sh` runs launched from such a shell, the
`verify-archive-tier-*` / `ch-schema-*` / `restore-drill` units'
`EnvironmentFile=`, and the `scripts/ops/*.sh` wrappers. They are
emitted only when `clickhouse_ops_batch_profile_enabled` is true AND
the vault password exists, so a partial apply can never hand jobs a
credential ClickHouse does not know. Unset = ClickHouse `default`
user, the pre-fix behaviour. Environment, never argv: a password in
argv is world-readable via `/proc` and lands in the journal through
`run-heavy-job.sh`'s `systemd-run` line.

**Never copy these two variables into `/etc/default/stellarindex`.**
The indexer / aggregator / API services source that file, and the same
package-level seam would demote the live sink and the supply refresher
to lowest priority — the exact inverse of the incident fix.

## Operator apply (codify-only in the PR; run `--check --diff` first)

1. `ansible-vault edit inventory/r1.secrets.yml` → add
   `vault_clickhouse_ops_batch_password: "<generated>"`.
2. Apply the CH user AND the env file together (a user without the
   env is inert; an env without the user breaks the next ops job):

   ```
   ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
     --tags clickhouse-ops-batch-profile,minio --check --diff
   ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
     --tags clickhouse-ops-batch-profile,minio
   ```

   The handler restarts `clickhouse-server`; the archival-node role's
   existing restart discipline applies (no restart mid-backfill).
3. Verify on r1 — a heavy job must show up as `ops_batch`:

   ```
   set -a; source /etc/default/stellarindex-ops; set +a
   stellarindex-ops ch-gate -ch 127.0.0.1:9300 ...   # any ops read
   clickhouse-client -q "SELECT user, priority, query_id FROM system.processes WHERE user = 'ops_batch'"
   ```

   and, from a different shell, that the aggregator's queries still run
   as `default`. Then re-run the 2026-08-28 workload
   (`ch-rebuild -sep41` dry-run) while watching
   `stellarindex_aggregator_supply_refresh_error_dominant` — the alert
   staying quiet is the acceptance test.

## Rule

Every heavy `stellarindex-ops` job (ch-rebuild, ch-backfill,
classic-movements-backfill, verify-*, gates, replays) runs under
`run-heavy-job.sh` from a shell that has sourced
`/etc/default/stellarindex-ops`. Both halves matter: the cgroup caps
bound the process; the ops-batch identity bounds what it can do to
ClickHouse's other users.
