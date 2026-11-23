# Ansible role — `patroni`

Deploy a Patroni-managed Postgres + TimescaleDB cluster with
etcd as the DCS. Implements the topology pinned in
[`docs/architecture/ha-plan.md §3.3`](../../../../docs/architecture/ha-plan.md):

- 1 primary + 2 synchronous replicas across `db-01` / `db-02` /
  `db-03`.
- 3-node etcd quorum.
- `synchronous_commit=remote_apply`; Patroni's `synchronous_mode`
  writes `synchronous_standby_names` itself.
- Failover RTO target 60 s.

This role is **stage 1** of a two-stage Postgres front
(stage 2 is the PgBouncer / HAProxy front role, separate).
Implementing this role makes the
[`postgres.md`](../../../../docs/operations/runbooks/postgres.md#stellarindex_timescale_primary_down)
runbook's *§A Automatic Patroni failover* path the actual default,
rather than aspirational.

Design rationale lives in
[`docs/architecture/ha-plan.md` §3.3](../../../../docs/architecture/ha-plan.md#33-postgres--timescaledb).

## Prerequisites

- Three hosts named per inventory (`db-01` / `02` / `03` by
  default). Each needs:
  - Ubuntu 24.04 LTS (or 22.04).
  - Network reachability between them on ports 5432 (Postgres),
    8008 (Patroni REST), 2379 + 2380 (etcd).
  - The PostgreSQL + TimescaleDB apt repositories present (the
    `archival-node` role's postgres repo tasks add them). The hosts
    must not be in `archival_nodes`: both roles own the whole
    `/etc/nftables.conf`, and preflight refuses an archival node.
  - Patroni 3.0.4 or newer (Ubuntu 24.04's package, or PGDG on
    22.04); preflight refuses older ones.

- Vault contents:
  - `patroni_replicator_password` — Postgres replication user.
  - `patroni_postgres_password` — Postgres superuser.
  - `patroni_rest_basic_auth_user` + `patroni_rest_basic_auth_password` —
    Patroni REST API Basic Auth. Required: the role fails at
    06-patroni-configure if either is empty.

- **etcd TLS is on by default** (`etcd_tls_enabled: true`): client and
  peer ports use HTTPS with mutual client-cert auth, and Patroni
  connects with `protocol: https` plus a client cert. Supply PEM
  contents from vault: `etcd_tls_ca_pem`, per-host `etcd_tls_cert_pem` /
  `etcd_tls_key_pem` (serverAuth + clientAuth EKUs, SANs for the host's
  `ansible_host` and `127.0.0.1`), and `patroni_etcd_client_cert_pem` /
  `patroni_etcd_client_key_pem`. The role fails if any is missing and
  never generates or stores key material. `etcd_tls_enabled: false`
  restores plaintext for throwaway lab clusters only. Enabling TLS on
  an already-running plaintext cluster needs a rolling re-bootstrap of
  etcd (peer URLs change), not a plain re-apply.

- **Inventory must set `etcd_release_sha256`** (F-1280, codex
  audit-2026-05-13). The role pins etcd to `{{ etcd_release_sha256 }}`
  on download and the 02-etcd-install task asserts at role-start
  that the variable is set, exactly 64 chars, and not the legacy
  `REPLACE_WITH_RELEASE_SHA` placeholder. The SHA is per-architecture
  (amd64 vs arm64; different values per release) and changes with
  every etcd version bump, so it is not defaulted in
  `defaults/main.yml` — it must come from inventory. Pull the value
  from the matching SHA256SUMS line on
  `https://github.com/etcd-io/etcd/releases/tag/v{{ etcd_version }}`.

## Inventory model

Set in your `inventory/<region>.yml`:

```yaml
all:
  children:
    postgres_cluster:
      hosts:
        db-01: { ansible_host: 10.0.0.11, patroni_role: bootstrap, etcd_role: bootstrap }
        db-02: { ansible_host: 10.0.0.12, patroni_role: replica,   etcd_role: peer }
        db-03: { ansible_host: 10.0.0.13, patroni_role: replica,   etcd_role: peer }
      vars:
        patroni_cluster_name: stellarindex-r1
        etcd_cluster_token: stellarindex-etcd-r1
        patroni_postgres_version: 15
        patroni_data_dir: /var/lib/postgresql/15/main
```

`patroni_role: bootstrap` only sets the *initial bootstrap
state* — once running, Patroni may fail over and a replica
becomes primary. The role's idempotency check skips bootstrap
on subsequent runs.

## Running

F-1266 (2026-05-13): `playbooks/postgres-cluster.yml` is on
the L4 cutover backlog and hasn't landed yet; the role is
applied ad-hoc via tag-filtered plays today. When the playbook
lands, restore the original commands below.

```sh
cd configs/ansible
# Bring up a fresh cluster (first run) — playbook TBD:
# ansible-playbook -i inventory/r1.yml playbooks/postgres-cluster.yml --tags patroni

# Re-apply config without restarting — playbook TBD:
# ansible-playbook -i inventory/r1.yml playbooks/postgres-cluster.yml --tags patroni,config --skip-tags restart

# Promote a specific replica (operator action — not covered by
# this role's playbook):
ssh db-02 patronictl -c /etc/patroni/patroni.yml failover stellarindex-r1
```

## Restore-from-backup path

Not supported yet: `patroni_bootstrap_method: pgbackrest` fails at 08.
Patroni treats a data dir restored outside it as crashed, strips
`restore_command` and the recovery target from `postgresql.auto.conf` and
removes `recovery.signal`, so the WAL after the backup would never be
replayed. The restore has to move into a Patroni custom bootstrap method
(`bootstrap.method: pgbackrest` with `keep_existing_recovery_conf` and a
`recovery_conf.restore_command`), validated on a real cluster, before the
role accepts it.

## Replacing an etcd member

A host that comes back with an empty `/var/lib/etcd` must join the
existing quorum, not bootstrap a new one; the role refuses to start it
with `ETCD_INITIAL_CLUSTER_STATE=new` when another member answers with
quorum or when not every `postgres_cluster` host is in the play. Per the
etcd runtime-reconfiguration guide:

```sh
# on a surviving member (with the etcd client TLS flags)
etcdctl member list
etcdctl member remove <old-member-id>
etcdctl member add db-02 --peer-urls=https://10.0.0.12:2380
# then, against the replaced host only
ansible-playbook ... --limit db-02 -e etcd_initial_cluster_state=existing
```

Once a host's etcd data dir holds a member, `/etc/default/etcd` keeps the
initial state that member was started with, whatever the variable says, so
later full runs do not need the `-e` and do not restart etcd over it.
Patroni's unit only `Wants=` etcd, so an etcd restart never restarts Patroni.

The role also refuses `new` when a peer cannot be asked at all (TLS,
firewall, timeout), because that is not evidence that no cluster exists.

Finishing a half-done first bootstrap is the one case where `new` beside a
live quorum is correct: the other members started from the same
`initial-cluster` and token, and this listed member has never run etcd and
was never removed. Re-run against it with
`-e etcd_allow_new_bootstrap=true` (default `false`). Never set it for a
replaced host: that one needs `member add` and `existing` as above.

## Smoke test

After the playbook finishes:

```sh
# Cluster state
ssh db-01 patronictl -c /etc/patroni/patroni.yml list

# Expected: 3 rows, one with role=Leader, two with role=Replica,
# all running.
```

## What this role does, at a glance

1. **Preflight** — verify OS, RAM ≥ 32 GB, a 3-host `postgres_cluster`
   group, and the vault passwords. It does not check that the postgres
   package is installed.
2. **etcd** — install + configure + start the 3-node quorum.
3. **Patroni** — install + render config + start.
4. **Bootstrap** — before anything is stopped, every host refuses a
   data dir that holds a cluster Patroni did not create (the distro
   auto-creates one at the default path; drop it with
   `pg_dropcluster --stop <ver> main`). The only exception is `db-01`
   restoring over it from pgBackRest with
   `patroni_adopt_existing_data_dir: true`. Then the distro's
   `postgresql@<ver>-main` unit is masked on every host. On `db-01`,
   initialise the cluster (a pgBackRest restore is refused; see
   Restore-from-backup path); on `db-02` /
   `03`, join as replicas and wait until each answers Patroni's
   `/replica` check.
5. **Firewall** — open 5432 / 8008 / 2379 / 2380 to the internal
   network only.
6. **Monitoring** — wire `node_exporter` textfile collectors so
   the existing `stellarindex_timescale_primary_down` alert
   continues firing.

## When this role IS NOT enough

- **PgBouncer / HAProxy front** — separate role (Task #72 part 3).
  Without it, API and indexer clients connect directly to
  Patroni's primary IP — which moves on failover. The
  Patroni-aware client wrapper does work, but PgBouncer is the
  recommended productionising step.
- **TimescaleDB CAGG / hypertable bootstrap** — handled by
  `cmd/stellarindex-migrate`, not this role.
- **pgBackRest** — not installed by this role. `archive_command`
  calls `archive-push` on every WAL segment, so preflight refuses a
  host without the `pgbackrest` binary and its config, and 08 runs
  `stanza-create` and `check` on the primary after bootstrap.
- **Backups** — this role schedules no pgBackRest full or differential
  backup and has no working restore path; both must come from outside it.
- **WAL archive across a failover** — `archive_mode` is `on`, so a
  promoted standby archives only WAL it generates. Segments the failed
  primary had not yet pushed are never archived, and the pgBackRest chain
  has a gap a point-in-time restore stops at. Take a full backup after
  every failover.
- **Slot-held WAL** — `patroni_max_slot_wal_keep_size` (default `64GB`)
  caps the WAL a member's replication slot keeps on the primary; preflight
  refuses an empty or `-1` value. A replica that falls further behind
  loses its slot and must be rebuilt with `patronictl reinit`.

## See also

- [`docs/architecture/ha-plan.md §3.3`](../../../../docs/architecture/ha-plan.md) — topology.
- [`docs/operations/runbooks/postgres.md#stellarindex_timescale_primary_down`](../../../../docs/operations/runbooks/postgres.md#stellarindex_timescale_primary_down) — the runbook this role makes work.
- [`docs/operations/drills/scenarios/sev1-timescale-primary-failover.md`](../../../../docs/operations/drills/scenarios/sev1-timescale-primary-failover.md) — the SEV-1 drill scenario whose Validation #6 closes when this role lands.
- ADR-0008 (HA topology) — the architectural ratification of the 3-node-DB-cluster choice.
