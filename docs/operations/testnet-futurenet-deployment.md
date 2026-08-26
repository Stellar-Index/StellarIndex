# Testnet + Futurenet deployment

How to stand up StellarIndex Testnet and Futurenet instances. Companion:
[testnet-futurenet-reset-runbook.md](./testnet-futurenet-reset-runbook.md).

## Topology decision

- **One Hetzner box, two Proxmox VMs** — one VM per network. Testnet +
  Futurenet share the box; **Mainnet is the only tier with HA / multiple
  regions** (r1 + future r2/r3).
- **Box:** Hetzner Server Auction **Xeon E-2176G** (6c/12t, 64 GB DDR4
  **ECC**, 2×960 GB **U.2 NVMe Datacenter**), Frankfurt, ~€83/mo, €0 setup
  (AuctionID 3059162-class). IP 95.217.126.13.
- **Storage:** the two NVMe are **RAID0-striped** (ZFS stripe, ~1.9 TB) —
  test-net data is disposable/re-ingestable, so no mirror; a drive failure
  just means re-ingest from genesis. The **host** owns ZFS (stripe + zstd);
  each **VM** gets a plain virtual disk (no ZFS-on-ZFS).
- **Why 1-box-2-VMs, not co-tenant bare metal:** the stack isn't
  instance-templated (ports, DB names, galexie buckets, ~40 systemd units
  would all collide), so two VMs reuse the single-network ansible unchanged.

## What makes the code network-correct

A `stellar.network = testnet|futurenet` switch drives everything, because
these were network-parameterized (commit `6b3859d7`, audit 2026-08-26):

| Concern | Mechanism |
| --- | --- |
| Passphrase | `cfg.Stellar.Passphrase()`; ansible `stellar_passphrase` feeds core.cfg + galexie |
| SAC contract addresses | `canonical.InstallNetworkPassphrase` at API start-up |
| Movements feed floor | `soroban_genesis_ledger` / `movements_floor_ledger` = **1** on test nets (else the feed floors above the whole chain) |
| cap67 follow daemon | `-floor-ledger` = `stellar_movements_floor_ledger` |
| History archive | `stellar_history_archive_url` = core-testnet / core-futurenet; a pubnet (core-live) URL on a test net is **rejected** by config validation |
| Cross-anchor archive fill | refuses to write pubnet ledgers into a test-net archive |
| DEX/oracle + aggregator | not deployed; `stellarindex_enabled_sources: []` |

## Deploy set

`galexie + stellarindex-indexer + stellarindex-api + stellarindex-ops`.
**No aggregator** (no real USD markets off pubnet). `stellarindex-ops` is
**required** — it runs the `cap67-movements` follow daemon that writes the
`/v1/accounts/{g}/movements` feed (not the aggregator).

## Step 1 — Host (Proxmox + RAID0)

1. Install the `si_deploy` pubkey on the box (`/root/.ssh/authorized_keys`):
   `ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFusEIOFA2VH3xvFWIMA8q60iXb1DZvDHrD21wjnv32u`
2. Install Proxmox VE (no-subscription repo, free).
3. Create the striped pool over both NVMe:
   `zpool create -o ashift=12 -O compression=zstd -O atime=off vmdata <nvme0> <nvme1>`
   (no `mirror`/`raidz` keyword → **stripe**; ~1.9 TB usable).
4. Point Proxmox storage at the `vmdata` zpool (ZFS storage type).

## Step 2 — Two VMs

Create `testnet` and `futurenet` VMs (Debian/Ubuntu matching r1's OS):

- ~3 vCPU, ~26 GB RAM each (ClickHouse queries hard-cap 8 GiB/query; each VM
  runs captive-core + CH + PG + indexer + api + cap67-movements). ~8 GB left
  for the host.
- One virtual disk each (~800 GB thin) on `vmdata`, mounted at `/var/lib`.
- Install the `si_deploy` pubkey in each VM's `root` authorized_keys.
- Give each VM a reachable IP (Hetzner additional IP, or host IP + port map).

## Step 3 — Inventory

```sh
cp configs/ansible/inventory/testnet.example.yml  configs/ansible/inventory/testnet.yml
cp configs/ansible/inventory/futurenet.example.yml configs/ansible/inventory/futurenet.yml
```

Fill the `X.X.X.X` VM IPs, `allowed_ssh_cidrs`, and create the vault
secrets (`testnet.secrets.yml` / `futurenet.secrets.yml`) with DB / MinIO /
CH-serving passwords (same shape as `r1.secrets.yml`).

**Testnet is Phase 1** (fully working). **Futurenet is Phase 2** — galexie
has no built-in futurenet preset, so its captive core needs an explicit
passphrase + history archives; provision + test that on the VM before
first run (see the futurenet inventory's flagged TODOs).

## Step 4 — Deploy + bring-up

Run the archival-node role against the inventory (via `deploy.yml` or
directly). Order per VM:

1. galexie captive-core catch-up from **genesis** (fast — tiny nets) →
   writes `galexie-live`.
2. indexer reads `galexie-live` (live-only, seam 0) → ClickHouse + PG.
3. `cap67-movements` follow daemon derives `account_movements` from genesis
   (`-floor-ledger 1`).
4. api serves `/v1/*`.

## Step 5 — Verify

- [ ] Ingest advancing from genesis: cursor climbing, no stall.
- [ ] `curl :3000/v1/version` on each VM.
- [ ] `/v1/accounts/{g}/movements` returns rows for an active test-net
      account (proves `movements_floor_ledger=1` took effect — this is the
      thing the network-abstraction fixed).
- [ ] `/v1/assets/{id}` shows a **testnet** SAC contract address (not the
      pubnet one).
- [ ] No aggregator noise in logs (aggregator not deployed).

## Related

- [testnet-futurenet-reset-runbook.md](./testnet-futurenet-reset-runbook.md) — reset handling.
- `configs/ansible/inventory/testnet.example.yml`, `futurenet.example.yml`.
- `internal/config/config.go` `StellarConfig` — the network knobs.
- Protocol-upgrade pipeline: Futurenet → Testnet → Mainnet (futurenet is
  the early-warning network for new protocol op/event types).
