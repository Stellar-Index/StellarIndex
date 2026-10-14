---
title: Testnet + Futurenet deployment
last_verified: 2026-10-05
status: current
---

# Testnet + Futurenet deployment

How to stand up StellarIndex Testnet and Futurenet instances. Companion:
[testnet-futurenet-reset-runbook.md](./testnet-futurenet-reset-runbook.md).

## Topology decision

- **One Hetzner box, two libvirt/KVM VMs**, one per network.
- **Box:** Hetzner Server Auction Xeon E-2176G (6c/12t, 64 GB ECC, 2×960 GB U.2 NVMe), Helsinki. IP 95.217.126.13.
- **Storage:** the NVMe pair is mdadm RAID0 (LVM `vg0` ~1.74 TB). Data is re-ingestable, so no mirror. Each VM gets a plain virtual disk.
- **Why VMs:** the stack isn't instance-templated (ports, DB names, buckets, ~40 systemd units would collide), so two VMs reuse the single-network ansible unchanged.

## What makes the code network-correct

`stellar.network = testnet|futurenet` drives everything:

| Concern | Mechanism |
| --- | --- |
| Passphrase / SAC addresses | `cfg.Stellar.Passphrase()`; ansible `stellar_passphrase`; `canonical.InstallNetworkPassphrase` at API start-up |
| Movements feed floor | `soroban_genesis_ledger` = **1**, `movements_floor_ledger` = **2** on test nets; the cap67 daemon's `-floor-ledger` = `stellar_movements_floor_ledger` |
| History archive | `stellar_history_archive_url` = core-testnet / core-futurenet; a pubnet (core-live) URL on a test net is **rejected** by config validation |
| Sources | **SDEX only** (`stellarindex_enabled_sources: [sdex]`). No aggregator (`run_aggregator: false`) |
| Start ledger | `galexie_start_ledger` **==** `stellarindex_backfill_from_ledger`, a recent ledger, not genesis. **Update both together after a reset** |

## Deploy set

`galexie + stellarindex-indexer + stellarindex-api + stellarindex-ops`. **No aggregator.** `stellarindex-ops` is required: it runs the `cap67-movements` follow daemon that writes `/v1/accounts/{g}/movements`.

## Steps 1–2 — Host + VMs (libvirt/KVM)

Per [../../configs/libvirt/README.md](../../configs/libvirt/README.md): Debian 12 via installimage (`configs/libvirt/installimage-host.conf`), then libvirt/KVM + two Ubuntu 24.04 VMs (`si-testnet` 192.168.122.10, `si-futurenet` 192.168.122.20) from `configs/libvirt/provision-vms.sh` with cloud-init. Each VM: 4 vCPU, 20 GB RAM, a 600 GB LV, on libvirt's private NAT, reached via ProxyJump through the host (encoded in the inventory).

## Step 3 — Inventory

```sh
cp configs/ansible/inventory/testnet.example.yml  configs/ansible/inventory/testnet.yml
cp configs/ansible/inventory/futurenet.example.yml configs/ansible/inventory/futurenet.yml
```

Fill the `X.X.X.X` VM IPs, `allowed_ssh_cidrs`, and create the vault secrets (`testnet.secrets.yml` / `futurenet.secrets.yml`; same shape as `r1.secrets.yml`). Filled inventories are untracked.

Pin the host keys before the first run — both SSH hops verify against `configs/ansible/inventory/known_hosts` (untracked) and fail on a mismatch:

```sh
ssh-keyscan -t ed25519 HOST_PUBLIC_IP > configs/ansible/inventory/known_hosts
ssh root@HOST_PUBLIC_IP 'ssh-keyscan -t ed25519 192.168.122.10 192.168.122.20' \
  >> configs/ansible/inventory/known_hosts
```

Re-capture a line only after confirming why the key changed.

## Step 4 — Deploy + bring-up

Run the archival-node role (via `deploy.yml` or directly); ClickHouse (`run_clickhouse`, default **false**), Redis and the CH schema apply are opt-in flags set only in test-net inventories. Order per VM:

1. galexie captive-core catches up to `galexie_start_ledger` via the network archive, then exports start → tip → live into `galexie-live`. On restart it **resumes** from the last exported ledger; `GALEXIE_START` is honoured only on an empty bucket, so wipe `galexie-live` before changing it.

   > **Keep `galexie_start_ledger` within a few hundred ledgers of the live tip**, or galexie parks ("Waiting for trigger ledger") and never exports. **Recovery:** stop galexie+indexer, wipe `galexie-live`, `truncate ingestion_cursors` (+ `account_observer_watermark`), set `galexie_start_ledger`/`backfill_from_ledger` a few hundred below the current tip, re-render (`--tags galexie,stellarindex`).
2. The indexer reads `galexie-live` from `stellarindex_backfill_from_ledger` → ClickHouse + PG. Start it after galexie exports the first object at/after that ledger.
3. `cap67-movements` derives `account_movements` from the recent start.
4. The API serves `/v1/*` on `stellarindex_api_listen_addr` (0.0.0.0:3000 so the host's Caddy reaches it).

## Step 5 — Host reverse proxy + DNS/TLS

The VMs have no public IP; one Caddy on the host fronts every subdomain:

| Record | Cloudflare | Why |
| --- | --- | --- |
| `api.testnet.stellarindex.io` → A 95.217.126.13 | **DNS-only (grey)** | SSE breaks behind the proxy's buffering; grey also lets Caddy solve ACME HTTP-01 for a real LE cert |
| `api.futurenet.stellarindex.io` → A | grey | same |
| `testnet.stellarindex.io` | **proxied (orange)** | static Next.js explorer; live data from the grey `api.*` origin |
| `futurenet.stellarindex.io` | orange | same |

```sh
rsync -a configs/libvirt/ root@95.217.126.13:/root/libvirt/
ssh root@95.217.126.13 'cd /root/libvirt && ./setup-host-proxy.sh'
```

`setup-host-proxy.sh` installs Caddy and deploys `configs/libvirt/host-Caddyfile` (LE for grey `api.*`, `flush_interval -1` for SSE). Orange origins use a Cloudflare Origin-CA cert.

## Step 6 — Verify

- [ ] `/v1/ledger/tip`: `latest_ledger` climbing, `stale:false`, `lag_seconds` low. galexie earliest exported == `backfill_from_ledger`.
- [ ] `curl https://api.testnet.stellarindex.io/v1/ledger/tip` returns 200 (DNS → Caddy → LE cert → VM API).
- [ ] No aggregator in logs; `enabled_sources` is `[sdex]` only.

## Step 7 — Failed units on a fresh test net

Triage `systemctl list-units --state=failed` first. Gated on `stellar_network == 'pubnet'` (so omitted here): `galexie-archive-fill`, `verify-archive-tier-b` (`verify_archive_tier_b_enabled`), `galexie-archive-trim` (`galexie_archive_trim_enabled`), `pgbackrest-backup` (`pgbackrest_backup_enabled`). `config-assertions` gates ZFS and mainnet compression checks on `have_zfs()` / `is_pubnet()`.

- `verify-archive-tier-a`: passes once the `galexie-archive` bucket exists; `systemctl start verify-archive-tier-a` clears the state.
- `archive-completeness`: scan exceeds `TimeoutStartUSec=30min`. Open.
- `stellar-core` (`run_stellar_core: false`) and `stellarindex-aggregator`: residue, `reset-failed`.
- A `disabled` unit in `failed` state is residue.
- **Never rebuild a unit's environment on the command line.** `sudo -u stellarindex env $(grep ... | xargs) <cmd>` puts every secret into argv, which sudo logs to the journal (shipped to Loki). Use `systemctl start <unit>` or `sudo -u <user> bash -c 'set -a; . /etc/default/<file>; set +a; exec <cmd>'`.

## Keeping the test nets in step with r1

Test nets take releases after r1, by a separate `deploy.yml` dispatch per region. Two checks catch drift, both running [`scripts/ci/check-fleet-release-drift.sh`](../../scripts/ci/check-fleet-release-drift.sh):

| Check | When | What it does |
| --- | --- | --- |
| `deploy.yml` → **Fleet release parity** step | after every deploy | Reads `/v1/version` on both test nets against the tag just deployed and prints the catch-up dispatches (`::warning::`, never a failure). |
| [`fleet-release-drift.yml`](../../.github/workflows/fleet-release-drift.yml) | daily 07:25 UTC | Compares against live r1 with a **24h grace** per test net, from the **oldest r1 release it lacks**. Past it, opens/updates one issue (`Fleet release drift: a test net is behind r1`, label `fleet-release-drift`), fails the run, and closes the issue once in step. An unreadable host is `DEGRADED`. |

- Reads `GET /v1/version` on the `api.*` names (the bare names 301 to mainnet; redirects are never followed). An HTTP error or a body without `.data.version` is `DEGRADED`. It is a tripwire: it trusts what the host says it runs.
- `git describe` suffixes (`-3-gabc1234`, `-dirty`) compare equal to the tag; `-rc.N` sorts below its release. While r1 serves a pre-release, a test net not on the identical string is `DRIFT` with no grace.

Catch up with one dispatch per region (binary set from `scripts/dev/region-binaries.tsv`: no aggregator; sla-probe and migrate included, or deploy.yml refuses):

```sh
gh workflow run deploy.yml -f region=testnet   -f version=vX.Y.Z -f binaries=stellarindex-indexer,stellarindex-api,stellarindex-sla-probe,stellarindex-ops,stellarindex-migrate
gh workflow run deploy.yml -f region=futurenet -f version=vX.Y.Z -f binaries=stellarindex-indexer,stellarindex-api,stellarindex-sla-probe,stellarindex-ops,stellarindex-migrate
```

Verify: `curl -sf https://api.testnet.stellarindex.io/v1/version | jq -r .data.version` equals r1's.

## Known gaps

- `/v1/markets` on test nets is empty (empty `prices_1d` CAGG); use `/v1/pools`.

## Related

- [testnet-futurenet-reset-runbook.md](./testnet-futurenet-reset-runbook.md); `configs/ansible/inventory/{testnet,futurenet}.example.yml`; `internal/config/config.go` `StellarConfig`.
