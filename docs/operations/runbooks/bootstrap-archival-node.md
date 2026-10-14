---
title: Bootstrap — Archival node from bare Ubuntu
last_verified: 2026-08-29
status: current — re-scoped 2026-08-29
---

# Runbook — Bootstrap an Archival Node

Canonical bring-up recipe: [archival-node-bringup.md](../archival-node-bringup.md). This runbook keeps the hardware-inventory and vault-bootstrap detail. Takes a fresh Hetzner box from `root@<ip>:~#` to a configured archival node syncing pubnet: ~15 min of Ansible plus 2-5 h background catchup. Role: `configs/ansible/roles/archival-node/`; spec: [archival-node-spec.md](../../architecture/infrastructure/archival-node-spec.md).

Different hardware means different `zfs_data_devices`, `zfs_os_drives_needing_data_partition`, `zfs_data_pool_type`, and per-box pinned `minio_release_sha256` / `mc_release_sha256` (required by `tasks/09-minio.yml`; no default on purpose).

## 0. Before you start

- SSH access as `root` and the public IP of the new box.
- Repo checked out at `~/code/stellarindex`.
- Ansible: `pip install --user "ansible-core>=2.16"` and `ansible-galaxy collection install -r configs/ansible/requirements.yml`.

## 1. Smoke-test SSH + inventory the hardware

```sh
ssh root@<ip> "lsblk -d -o NAME,ROTA,TYPE,MODEL,SIZE && ls -la /dev/disk/by-id/ | grep nvme"
```

Record the four big drives (`3.84 TB` or `7.68 TB`), their stable `/dev/disk/by-id/nvme-*` paths (`/dev/nvme0n1` can change), and which carry installimage OS partitions (ESP / swap / root): those go in `zfs_os_drives_needing_data_partition`. Any `ROTA=1` drive: stop, wrong box.

## 2. Populate inventory + secrets

```sh
cd ~/code/stellarindex/configs/ansible
cp inventory/r1.example.yml inventory/r1.yml
$EDITOR inventory/r1.yml
```

Fill in `ansible_host` (public IP), `ansible_ssh_private_key_file`, `zfs_data_devices` (the four by-id paths, plus the two settings from §1), `admin_ssh_keys` (contents of `~/.ssh/id_ed25519.pub`), and `minio_release_sha256` + `mc_release_sha256` (pinned SHAs extracted from `minio_image`; `tasks/09-minio.yml` also asserts `minio_image` is digest-pinned). Then:

```sh
ansible-vault create inventory/r1.secrets.yml
```

The authoritative secrets table is [archival-node-bringup.md § Prerequisites](../archival-node-bringup.md#prerequisites). The vault carries:

- `postgres_pass_stellarindex` (`tasks/05-postgres.yml`); `postgres_pass_core` only when `run_stellar_core: true`.
- `minio_root_user` / `minio_root_password`.
- `galexie_s3_access_key` / `galexie_s3_secret_key`: the bucket-scoped `galexie-writer` IAM user from `tasks/09-minio.yml`, NOT the MinIO root identity.
- `galexie_archive_s3_access_key` / `galexie_archive_s3_secret_key`: `galexie-archive-writer` (write-only to `galexie-archive`).
- `vault_stellarindex_reader_secret_key` (read-only `stellarindex-reader`), `vault_clickhouse_serving_password` (`tasks/20-clickhouse-serving-profile.yml`).
- `healthcheck_ping_url` plus the per-unit Healthchecks.io URLs (`tasks/13-healthcheck.yml`, `17-stellarindex-healthchecks.yml`).

Generate passwords with `openssl rand -hex 32` (the preflight rejects shell metacharacters in env-file secrets; base64 `/` and `+` break hand-pasted URLs).

## 3. Dry-run (check mode)

```sh
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
  --ask-vault-pass --check --diff
```

Errors: fix the inventory or open an issue before applying.

## 4. Apply

```sh
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
  --ask-vault-pass
```

~10-20 min. `PLAY RECAP` must show `failed=0 unreachable=0`; otherwise fix the root cause and re-apply (idempotent).

## 5. Post-apply verification

A default apply (`run_stellar_core: false`) has no `stellar-core.service` or `stellar-rpc.service`; stellar-core exists only as galexie's embedded captive-core. Their absence is correct.

```sh
ssh root@<ip>

# ZFS pool + datasets
zpool status data
zfs list -t all

# The services a default apply actually manages
systemctl is-active galexie minio postgresql@15-main redis-server clickhouse-server
# All five should print: active

# Galexie's embedded captive-core (the only stellar-core on the box)
pgrep -af "galexie append"
journalctl -u galexie -n 30 --no-pager

# Postgres — the stellarindex DB (stellar_core DB exists only when
# run_stellar_core=true)
runuser -u postgres -- psql -c '\l' | grep stellarindex

# MinIO
curl -sI http://127.0.0.1:9000/minio/health/live

# Firewall
nft list ruleset | head -40
```

Expected: galexie catches up, then fresh objects land in `galexie-live`; MinIO healthy with three role-created buckets; Postgres, Redis, ClickHouse active with `stellarindex` migrated; firewall public ports 22 and 80/443 only (11625 only when `run_stellar_core: true`).

## 6. Verify MinIO buckets + IAM (role-managed)

`tasks/09-minio.yml` creates the buckets and IAM users; nothing is created by hand (the role installs the pinned `mc` at `/usr/local/bin/mc`; Debian's `mc` package is Midnight Commander).

```sh
mc ls local/
# Three buckets: galexie-live, galexie-archive, backups

mc admin user list local
# Three users: galexie-writer, galexie-archive-writer, stellarindex-reader
```

## 7. Catchup timeline

| Milestone | Time on EX63 / i9-auction | Monitor via |
| --------- | ------------------------- | ----------- |
| Galexie captive-core catchup → live tail | 10–30 min (NVMe) | `journalctl -u galexie -f` |
| Galexie exporting current ledgers | ~1 min after catchup | `mc ls local/galexie-live/` growing |

Genesis-to-tip galexie backfill is a separate, longer phase: 8-14 h serial, ~1.5 days with 8-worker parallel ([galexie-backfill.md § Tuning](../galexie-backfill.md#tuning--when-60-ledgerssec-isnt-enough); figures in [archival-node-spec.md § 3.4](../../architecture/infrastructure/archival-node-spec.md#34-galexie-backfill-time)).

## 8. First failures to expect

- Galexie "access denied" to MinIO: keys in `/etc/default/galexie` out of sync with the `galexie-writer` IAM user. Re-render with `ansible-playbook ... --tags galexie,minio`. A missing bucket means the `minio` tag never ran.
- Locked out by the firewall (nftables is applied before SSH hardening): reboot via Hetzner Robot KVM, widen `allowed_ssh_cidrs`, re-run `--tags firewall`.

## 9. What happens next

Historical galexie mirror, archive sweep/heal, integrity verification, setting the live seam and starting the indexer are [archival-node-bringup.md](../archival-node-bringup.md) steps 4-6. pgBackRest scheduling is part of the role (`tasks/18-pgbackrest-backup.yml`).

## 10. Teardown / redo

```sh
# On the box, as root
systemctl stop 'stellarindex-*' galexie minio postgresql@15-main clickhouse-server redis-server
zpool destroy data
```

`data` is the only pool (the OS root is plain nvme partitions). Re-run the playbook; ~10 min.

## 11. References

[archival-node-bringup.md](../archival-node-bringup.md), [archival-node-spec.md](../../architecture/infrastructure/archival-node-spec.md), [validator-rollout.md](../../architecture/infrastructure/validator-rollout.md), [ADR-0008](../../adr/0008-ha-topology.md), [ha-plan.md](../../architecture/ha-plan.md), `configs/ansible/README.md`.
