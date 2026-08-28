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
(mode `0640 root:stellarindex`). They are emitted only when
`clickhouse_ops_batch_profile_enabled` is true AND the vault password
exists, so a partial apply can never hand jobs a credential ClickHouse
does not know. Unset = ClickHouse `default` user, the pre-fix
behaviour. Environment, never argv: a password in argv is
world-readable via `/proc` and lands in the journal through
`run-heavy-job.sh`'s `systemd-run` line.

Three launch paths reach that file, and the first is the one that
matters:

- **`run-heavy-job.sh` imports the pair itself** (root and non-root
  paths alike, before the `systemd-run` scope), reading ONLY
  `STELLARINDEX_CLICKHOUSE_OPS_USER` / `_PASSWORD` out of
  `/etc/default/stellarindex-ops` when the caller has not already set
  them. The runbooks that prescribe the heavy jobs
  ([sep41-mint-recovery](sep41-mint-recovery.md),
  [usd-volume-rederive](usd-volume-rederive-2026-08.md),
  [fx-history-missing](runbooks/fx-history-missing.md),
  [lcm-cache-tiering](lcm-cache-tiering.md)) source
  `/etc/default/stellarindex` — the PG / MinIO service identity — not
  `-ops`, and the 2026-08-28 job was launched exactly that way; a
  design that relied on the caller's shell was a silent no-op on the
  incident path. Every launch prints one stderr line naming the
  identity it will use, or a `WARNING ... CH 'default' user at
  SERVING priority` line when the pair is absent, so the gap can
  never be quiet again.
- The `verify-archive-tier-*` / `ch-schema-*` / `restore-drill` units'
  `EnvironmentFile=/etc/default/stellarindex-ops`.
- Interactive `set -a; source /etc/default/stellarindex-ops; set +a`
  for one-off `stellarindex-ops` reads outside the wrapper.

`config-assertions.sh` (hourly, `stellarindex_config_assertion_ok`)
asserts the two halves moved together — the `ops_batch` drop-in in
`users.d` implies the pair in `/etc/default/stellarindex-ops` — and
that `/etc/default/stellarindex` never carries it.

**Never copy these two variables into `/etc/default/stellarindex`.**
The ansible-templated indexer / aggregator / API units source that
file, and the same package-level seam would demote the live sink and
the supply refresher to lowest priority — the exact inverse of the
incident fix.

**deploy/systemd self-hosts:** the reference units in
`deploy/systemd/stellarindex-{indexer,aggregator,api}.service` source
`/etc/default/stellarindex-ops` as their ONLY env file (there is no
`/etc/default/stellarindex` on such a host). There the pair must NOT
be written to that file either — export it in the batch job's shell
(`export STELLARINDEX_CLICKHOUSE_OPS_USER=ops_batch ...`) or in a
private env file that only `run-heavy-job.sh` reads via
`HEAVY_JOB_OPS_ENV=<path>`.

## Operator apply (codify-only in the PR; run `--check --diff` first)

`clickhouse_ops_batch_profile_enabled` defaults to **false**: enabling
it asserts the vault password, and the weekly
`ansible-drift.yml` `--check` against r1 runs the full playbook with
the real vault — a default of true would keep that workflow red until
every inventory had the password. Enable per host, in one PR:

1. `ansible-vault edit inventory/r1.secrets.yml` → add
   `vault_clickhouse_ops_batch_password: "<generated>"` (generate it
   as hex — `openssl rand -hex 32` — so the value survives every
   consumer: `run-heavy-job.sh` reads it verbatim, but the interactive
   `set -a; source /etc/default/stellarindex-ops` path lets the shell
   expand `$`, quotes and `#`), and set
   `clickhouse_ops_batch_profile_enabled: true` in `inventory/r1.yml`
   (same PR — the assert fails on enable-without-password, by design).
2. Apply the CH user, the env file AND the heavy-job wrapper together
   (a user without the env is inert; an env without the user breaks
   the next ops job; a wrapper rendered before 2026-08-28 does not
   import the pair, so `heavy-job-wrapper` re-renders
   `/usr/local/sbin/run-heavy-job.sh` without a full `--tags
   stellarindex` build+deploy pass):

   ```
   ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
     --tags clickhouse-ops-batch-profile,minio,heavy-job-wrapper --check --diff
   ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
     --tags clickhouse-ops-batch-profile,minio,heavy-job-wrapper
   ```

   The handler restarts `clickhouse-server`; the archival-node role's
   existing restart discipline applies (no restart mid-backfill).
3. Verify on r1 — the drop-in parses, the user logs in, and a heavy
   job shows up as `ops_batch`. **As of 2026-08-28 none of this has
   been exercised against a live ClickHouse** (the profile was
   codified without host access): the `<constraints><priority>
   <readonly/>` block and the `priority` / `os_thread_priority`
   behaviour are unproven, and a malformed `users.d` drop-in fails
   `clickhouse-server` on the handler restart — so do the `--check
   --diff` first and keep the previous drop-in-free state one
   `rm users.d/ops-batch.xml` away:

   ```
   clickhouse-client --port 9300 --user ops_batch --password "$(sed -n 's/^STELLARINDEX_CLICKHOUSE_OPS_PASSWORD=//p' /etc/default/stellarindex-ops)" -q "SELECT currentUser()"
   run-heavy-job.sh ops-batch-probe stellarindex-ops ch-gate -ch 127.0.0.1:9300 ...   # any ops read; stderr names the identity
   clickhouse-client --port 9300 -q "SELECT user, priority, query_id FROM system.processes WHERE user = 'ops_batch'"
   ```

   and, from a different shell, that the aggregator's queries still run
   as `default`. Then re-run the 2026-08-28 workload
   (`ch-rebuild -sep41` dry-run, launched per
   [sep41-mint-recovery](sep41-mint-recovery.md) — i.e. from a shell
   that sourced `/etc/default/stellarindex`, NOT `-ops`) while watching
   `stellarindex_aggregator_supply_refresh_error_dominant` — the alert
   staying quiet is the acceptance test.

## Rule

Every heavy `stellarindex-ops` job (ch-rebuild, ch-backfill,
classic-movements-backfill, verify-*, gates, replays) runs under
`run-heavy-job.sh`, which supplies the identity itself. Both halves
matter: the cgroup caps bound the process; the ops-batch identity
bounds what it can do to ClickHouse's other users. If the wrapper's
first stderr line says `WARNING ... CH 'default' user`, stop and apply
the profile before running a multi-hour scan.

Not yet covered: the heavy shell scripts that call `clickhouse-client`
directly (`scripts/ops/ch-supply-flows-seed.sh`, `ch-live-catchup.sh`,
`d3-lecur-v2-rebuild.sh`, `d2-ordinal-reproject.sh`) still run as
`default` — the pair is in their environment, but they do not pass
`--user/--password` to `clickhouse-client`. Follow-up.
