---
title: r1 ↔ ansible drift audit
last_verified: 2026-07-06
status: current
---

# r1 ↔ ansible drift audit (2026-07-03)

Why `codified ≠ live` is a failure class here: the 2026-06-11 rsyslog
suppression rules were codified in ansible but never applied to r1, and r1
also carried hand fixes a playbook run would have erased. The audit diffed
every `dest:` in `configs/ansible/roles/*/tasks` plus the r1 overlays
(prometheus, alertmanager, caddy, systemd units) against the live host in both
directions.

## ⚠️ Standing rule

The full playbook applies cleanly to r1 and IS the deployment path for host
config. Binaries stay with deploy.yml (`manage_stellarindex_binaries=false`).

- Always `--check --diff` before an apply.
- Every hand fix on r1 is codified in the same PR.
- Guardrails: the hourly `config-assertions.sh` timer (load-bearing subset),
  the weekly `ansible-drift.yml` workflow, and the CI ansible syntax/lint job.
- `ansible-drift.yml` fails on any changed task not listed in
  `scripts/ci/ansible-drift.baseline` (each entry needs a reason; repo-ahead or
  live-ahead drift never belongs there). An apply is `workflow_dispatch` with
  `apply=true`, under deploy.yml's approval gate.

## What a drift report means

- **Live-only on r1** (an apply would ERASE it): codify it. The 2026-07-03 set
  (`[supply]` reserve accounts/balances in `/etc/stellarindex.toml`, Redis
  `maxmemory 1gb`, nftables drop-log + rsyslog/logrotate pair, Caddy, non-root
  systemd units, sshd `ssh_permit_root_login: "prohibit-password"`) is all
  codified. nftables `11625 accept` is gated on `run_stellar_core` (`false` on
  r1).
- **Repo-ahead-of-r1**: clears on the next apply.
- **Inherently non-idempotent** (`Sync migrations` rsync itemize, `disable-thp`
  oneshot `state: started`): baseline entries.

Lessons from the first apply, each now guarded: the role would have downgraded
r1's upstream OpenZFS 2.3.4 to Ubuntu's 2.2.2 (apt removed the dkms module
before failing; packages held, role gated, assertions added); galexie ran on
MinIO root creds (now the galexie-writer user); Postgres `max_wal_size` was
inert behind a `postgresql.auto.conf` override (auto.conf reset; the conf file
is single-source).

## Phantom drift (false positives)

- Raw-text diffs against `.j2` templates miss loop/var-rendered content (the
  nftables 80/443/SSH-limit rules come from `public_allow_ports_base`). Render
  with `ansible-playbook --check --diff` before believing a template gap.
- `community.general.zfs` reads props with `zfs get -p`, so `zfs_datasets`
  recordsize must be the byte form (`131072`/`8192`/`1048576`). The
  `Create ZFS datasets` task is item-driven: it manages only the properties
  each entry declares (forcing an inherited property re-reports every run);
  per-item `dir_mode` (`pgbackrest` uses `"0750"`).
- `postgresql_set` string-compares against `pg_settings`, so
  `shared_preload_libraries` must be the stored form
  `timescaledb, pg_stat_statements` (with the space).

## Comment-only drift is reported, not counted

`scripts/ci/check-ansible-drift.sh` classifies a changed task as
**comment-only** when its `--diff` hunks' removed and added lines are the same
multiset after stripping trailing comments (`#`, `--`, `//`) and blanks, and a
changed handler as **consequential** when every other non-allowed changed task
is comment-only. Both print (`≈` / `↳`) in the job summary and neither fails the
run. A task that reports changed with **no diff** (`diff: false`, e.g.
`Install pgBackRest core config`, which carries repo credentials) stays drift.

Clearing the pgBackRest config and the `Restart postgres` handler it keeps in
the drift set takes an `apply=true` run in a window where a Postgres restart is
acceptable, or a by-hand comparison of `/etc/pgbackrest/pgbackrest.conf` with
the template.
