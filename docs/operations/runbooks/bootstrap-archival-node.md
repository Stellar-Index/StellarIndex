---
title: Bootstrap — Archival node from bare Ubuntu
last_verified: 2026-08-29
status: current — re-scoped 2026-08-29
---

# Runbook — Bootstrap an Archival Node

> **Canonical bring-up recipe:
> [archival-node-bringup.md](../archival-node-bringup.md)**
> (CLAUDE.md-designated). This runbook remains for the
> hardware-inventory + vault-bootstrap detail; sections below were
> re-scoped 2026-08-29 to match what the `archival-node` ansible
> role actually deploys today (no stellar-core / stellar-rpc units
> on a default apply, role-managed MinIO buckets + IAM).

**Purpose:** take a fresh Hetzner box from `root@<ip>:~#` to a fully
configured Stellar Index archival node syncing pubnet. ~15 min of
Ansible + ~2–5 h background catchup.

**Pairs with:** `configs/ansible/roles/archival-node/` +
[archival-node-spec.md](../../architecture/infrastructure/archival-node-spec.md).

**Applies to:** EX63 / auction-i9-13900 / future AX Ryzen — the role
is hardware-agnostic. Different hardware means different
`zfs_data_devices`, `zfs_os_drives_needing_data_partition`, and
`zfs_data_pool_type` in inventory, plus the per-box pinned
`minio_release_sha256` / `mc_release_sha256` values (required by
`tasks/09-minio.yml`; there is no default on purpose).

---

## 0. Before you start

- [ ] You have SSH access as `root` to the new box.
- [ ] You have the box's public IP.
- [ ] The repo is checked out on your workstation at
      `~/code/stellarindex`.
- [ ] You've installed Ansible:
      `pip install --user "ansible-core>=2.16"` and
      `ansible-galaxy collection install -r configs/ansible/requirements.yml`.

---

## 1. Smoke-test SSH + inventory the hardware

First thing — verify SSH works, then list the NVMe devices so we can
put their stable IDs in inventory:

```sh
ssh root@<ip> "lsblk -d -o NAME,ROTA,TYPE,MODEL,SIZE && ls -la /dev/disk/by-id/ | grep nvme"
```

Record:
- The four big drives (`3.84 TB` or `7.68 TB` depending on box).
- Their stable `/dev/disk/by-id/nvme-*` paths (those are idempotent
  across reboots; `/dev/nvme0n1` can change).
- Confirm `ROTA` column is `0` (SSD) for all four.
- Which drives carry installimage OS partitions (ESP / swap / root) —
  those go in `zfs_os_drives_needing_data_partition` so the role
  carves a data partition instead of consuming the whole disk.

If any drive is `ROTA=1` (rotational): stop. Wrong box.

---

## 2. Populate inventory + secrets

```sh
cd ~/code/stellarindex/configs/ansible
cp inventory/r1.example.yml inventory/r1.yml
$EDITOR inventory/r1.yml
```

Fill in:
- `ansible_host`: the public IP.
- `ansible_ssh_private_key_file`: typically `~/.ssh/id_ed25519`.
- `zfs_data_devices`: the four `/dev/disk/by-id/nvme-*` paths
  (+ `zfs_os_drives_needing_data_partition` / `zfs_data_pool_type`
  per §1).
- `admin_ssh_keys`: contents of `~/.ssh/id_ed25519.pub`.
- `minio_release_sha256` + `mc_version` / `mc_release_sha256`:
  pinned SHAs for the MinIO server + client downloads —
  `tasks/09-minio.yml` asserts these are set and refuses to run
  without them.

Then create the vault-encrypted secrets:

```sh
ansible-vault create inventory/r1.secrets.yml
```

The authoritative list of required secrets lives in
[archival-node-bringup.md § Prerequisites](../archival-node-bringup.md#prerequisites) —
use that table, not a copy here. In summary the vault must carry:

- `postgres_pass_stellarindex` — the `stellarindex` DB role
  (`tasks/05-postgres.yml`).
- `postgres_pass_core` — **only when `run_stellar_core: true`**
  (Phase-3 validator); on a default apply the core DB role isn't
  created and this key can be omitted.
- `minio_root_user` / `minio_root_password` — MinIO admin identity.
- `galexie_s3_access_key` / `galexie_s3_secret_key` — the
  **bucket-scoped `galexie-writer` IAM user** the role creates in
  `tasks/09-minio.yml`. This is deliberately NOT the MinIO root
  identity (least-privilege; the pre-2026 draft aliased it to
  `minio_root_user`, which defeated the point).
- `galexie_archive_s3_access_key` / `galexie_archive_s3_secret_key` —
  the `galexie-archive-writer` user (write-only to
  `galexie-archive`).
- `vault_stellarindex_reader_secret_key` — the read-only
  `stellarindex-reader` user the indexer/ops binaries use.
- `vault_clickhouse_serving_password` — the ClickHouse serving-tier
  password (`tasks/20-clickhouse-serving-profile.yml`).
- `healthcheck_ping_url` (+ the per-unit Healthchecks.io ping URLs
  used by `tasks/13-healthcheck.yml` / `17-stellarindex-healthchecks.yml`).

Generate strong passwords with `openssl rand -base64 32`.

---

## 3. Dry-run (check mode)

```sh
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
  --ask-vault-pass --check --diff
```

Expected: "ok" for everything that already exists, "changed" for
every task that would make a change. Error output → fix the
inventory or open an issue before applying.

---

## 4. Apply

```sh
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
  --ask-vault-pass
```

Runtime: ~10–20 min. Output:
- `PLAY RECAP` at the bottom shows `changed=<N> failed=0 unreachable=0`.
- If any `failed=>0` — re-read the error, fix root cause, re-apply.
  The role is idempotent.

---

## 5. Post-apply verification

On a **default apply** (`run_stellar_core: false`, the shipped
default) there is **no `stellar-core.service` and no
`stellar-rpc.service`** — stellar-core exists only as galexie's
embedded captive-core subprocess, and the stellar-rpc task +
templates were deleted from the role on 2026-05-22. Do not look for
those units; their absence is correct.

Smoke tests against the running box:

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

Expected state immediately after bootstrap:
- **galexie** → running; the embedded captive-core catches up, then
  fresh objects start landing in `galexie-live`.
- **MinIO** → healthy, three buckets already created by the role
  (§6).
- **Postgres / Redis / ClickHouse** → active; `stellarindex` DB
  present with migrations applied.
- **Firewall** → public base is 22 (SSH) + 80/443 (Caddy TLS
  front); 11625 (SCP peer) is appended **only when
  `run_stellar_core: true`**. Internal service ports are
  LAN/loopback-gated.

---

## 6. Verify MinIO buckets + IAM (role-managed)

The role creates everything itself in `tasks/09-minio.yml` — the
three buckets AND the three least-privilege IAM users. There is
nothing to create by hand (the pre-2026 draft's manual
`apt install -y mc` step was doubly wrong: Debian's `mc` package is
**Midnight Commander**, not the MinIO client — the role installs the
pinned real `mc` at `/usr/local/bin/mc`). Just verify:

```sh
mc ls local/
# Three buckets: galexie-live, galexie-archive, backups

mc admin user list local
# Three users: galexie-writer, galexie-archive-writer, stellarindex-reader
```

Galexie will start writing ledger meta to `galexie-live` within a
few seconds of its captive-core reaching the live tail.

---

## 7. Catchup timeline expectations

| Milestone | Time on EX63 / i9-auction | Monitor via |
| --------- | ------------------------- | ----------- |
| Galexie captive-core catchup → live tail | 10–30 min (NVMe) | `journalctl -u galexie -f` |
| Galexie exporting current ledgers | ~1 min after catchup | `mc ls local/galexie-live/` growing |

**Genesis-to-tip galexie backfill is a separate, much longer
phase** — the table above is just live-tail catchup. Plan
for an additional **8–14 h** for serial galexie scan-and-fill,
or **~1.5 days** with 8-worker parallel scan-and-fill (recipe
in
[galexie-backfill.md § Tuning](../galexie-backfill.md#tuning--when-60-ledgerssec-isnt-enough)).
The galexie backfill is the long pole when budgeting bring-up
time for an archival node — see
[archival-node-spec.md § 3.3.4](../../architecture/infrastructure/archival-node-spec.md#334-galexie-backfill-time-genesis--live-tip)
for the per-tier breakdown.

---

## 8. First failures to expect (and what they mean)

### Galexie fails with "access denied" to MinIO

Access keys wrong in `/etc/default/galexie` (vault
`galexie_s3_access_key`/`_secret_key` out of sync with the
`galexie-writer` IAM user) → re-render + re-create via
`ansible-playbook ... --tags galexie,minio`. Buckets themselves are
role-created (§6), so "bucket doesn't exist" means the `minio` tag
never ran, not a missing manual step.

### Firewall locked us out

The playbook applies nftables before hardening SSH. If you lose
access: reboot the box via Hetzner Robot's KVM, set
`allowed_ssh_cidrs` wider, re-run `--tags firewall`.

---

## 9. What happens next

The node is now producing ledger meta and all supporting services
are up. The remaining work — historical galexie mirror, archive
sweep/heal, integrity verification, setting the live seam, and
starting the indexer — is
[archival-node-bringup.md](../archival-node-bringup.md) **steps
4–6**. Follow that document; it is the canonical sequence (the
"Week-2 ingestion" and "pgBackRest (Week 3)" items the pre-2026
draft listed here landed long ago — pgBackRest scheduling is part
of the role, `tasks/18-pgbackrest-backup.yml`).

---

## 10. Teardown / redo

If something goes catastrophically wrong and you want a clean slate:

```sh
# On the box, as root
systemctl stop 'stellarindex-*' galexie minio postgresql@15-main clickhouse-server redis-server
zpool destroy data
```

(There is no `rpool` — the OS root lives on plain nvme partitions
laid down by installimage, not on ZFS. `data` is the only pool.)

Then re-run the Ansible playbook. The role is idempotent; a clean
ZFS destroy + re-apply takes ~10 min.

---

## 11. References

- [archival-node-bringup.md](../archival-node-bringup.md) — the
  canonical end-to-end bring-up + disaster-recovery recipe.
- [archival-node-spec.md](../../architecture/infrastructure/archival-node-spec.md)
- [multi-region-topology.md](../../architecture/infrastructure/multi-region-topology.md)
- [validator-rollout.md](../../architecture/infrastructure/validator-rollout.md)
- [hosting-options.md](../../architecture/infrastructure/hosting-options.md)
- `configs/ansible/README.md`

---

## Changelog

- 2026-08-29 — re-scoped against the role at HEAD. Front banner
  points at archival-node-bringup.md as the canonical recipe. §5
  dropped the `stellar-core` / `stellar-rpc` unit checks (neither
  unit exists on a default apply: `run_stellar_core` defaults to
  false and the stellar-rpc task + templates were deleted from the
  role 2026-05-22) in favour of
  `systemctl is-active galexie minio postgresql@15-main
  redis-server clickhouse-server` + `pgrep -af "galexie append"`;
  psql check now targets the `stellarindex` DB via
  `runuser -u postgres`. §6 manual bucket creation replaced with
  verification — the role creates the three buckets + three IAM
  users itself (`tasks/09-minio.yml`), and the old `apt install -y
  mc` instruction installed Midnight Commander, not the MinIO
  client. §2 vault list fixed (`galexie_s3_access_key` is the
  bucket-scoped writer, NOT the MinIO root identity;
  `postgres_pass_core` gated on `run_stellar_core`; added the
  missing stellarindex/reader/ClickHouse/healthcheck secrets +
  pinned minio/mc SHAs) and defers to bringup § Prerequisites as
  authoritative. §7 timeline trimmed to galexie + genesis-backfill.
  §8/§10 core/rpc failure modes + teardown rows removed; teardown
  destroys only pool `data` (`rpool` never existed). §1/§ intro:
  hardware differences also mean
  `zfs_os_drives_needing_data_partition`, `zfs_data_pool_type`,
  and per-box minio/mc SHAs. §9 "Week-2 ingestion (in flight)" /
  "pgBackRest (Week 3)" replaced with a pointer to bringup steps
  4–6.
- 2026-05-03 — initial draft (pre-dates the r1 core/rpc trim being
  reflected in the role; superseded content described a five-unit
  stack with manual MinIO bootstrap).
