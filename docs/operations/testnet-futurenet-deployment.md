# Testnet + Futurenet deployment

How to stand up StellarIndex Testnet and Futurenet instances. Companion:
[testnet-futurenet-reset-runbook.md](./testnet-futurenet-reset-runbook.md).

## Topology decision

- **One Hetzner box, two libvirt/KVM VMs** — one VM per network. Testnet +
  Futurenet share the box; **Mainnet is the only tier with HA / multiple
  regions** (r1 + future r2/r3).
- **Box:** Hetzner Server Auction **Xeon E-2176G** (6c/12t, 64 GB DDR4
  **ECC**, 2×960 GB **U.2 NVMe Datacenter**), Frankfurt, ~€83/mo, €0 setup
  (AuctionID 3059162-class). IP 95.217.126.13.
- **Storage:** the two NVMe are **RAID0-striped** (mdadm RAID0 via installimage, LVM `vg0` ~1.74 TB) —
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
| Sources | **SDEX only** (`stellarindex_enabled_sources: [sdex]`): the native Stellar DEX exists on every network, so the explorer shows real on-chain trades. Bespoke Soroban DEX/AMM (soroswap/aquarius/phoenix/blend/comet), oracles (reflector/redstone/band), external CEX/FX markets, and the aggregator/pricing are all **cut** (`run_aggregator: false`) — every asset is $0 off pubnet |
| Start ledger | **coherent recent start** — `galexie_start_ledger` **==** `stellarindex_backfill_from_ledger`. galexie's captive core catches up to that ledger via the archive (fast) and the indexer's live-only floor matches it, so there is no leading gap. NOT genesis: testnet is ~4.3M ledgers (a genesis replay is hours). **Update both together after a reset** (ledgers restart at 1) |

## Deploy set

`galexie + stellarindex-indexer + stellarindex-api + stellarindex-ops`.
**No aggregator** (no real USD markets off pubnet). `stellarindex-ops` is
**required** — it runs the `cap67-movements` follow daemon that writes the
`/v1/accounts/{g}/movements` feed (not the aggregator).

## Steps 1–2 — Host + VMs (libvirt/KVM)

Provisioned reproducibly per
[../../configs/libvirt/README.md](../../configs/libvirt/README.md): Debian 12 via
installimage (`configs/libvirt/installimage-host.conf` — mdadm RAID0 + mirrored
`/boot` + LVM `vg0`), then libvirt/KVM + two Ubuntu 24.04 VMs (`si-testnet`
192.168.122.10, `si-futurenet` 192.168.122.20) created by
`configs/libvirt/provision-vms.sh` with cloud-init (static IPs, both SSH keys).
Each VM: 4 vCPU, 20 GB RAM, a 600 GB LV; on libvirt's private NAT, reached via
ProxyJump through the host (encoded in the inventory). Ubuntu 24.04 matches r1
so the ansible role deploys unchanged. **Not Proxmox** — plain libvirt/KVM is
lighter and keeps the host CLI/ansible-managed.

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
directly). The role is r1-safe: ClickHouse (`run_clickhouse`, default **false**)
and Redis installs, the ClickHouse schema apply, the galexie drift-guard
skips, and the from-source binary build are all opt-in flags set only in the
test-net inventory — r1 is never touched. Order per VM:

1. galexie captive-core catches up to `galexie_start_ledger` via the network
   archive, then exports `galexie_start_ledger` → tip → live into `galexie-live`.
   (Fresh/empty bucket only; on restart it **resumes** from the last exported
   ledger. `GALEXIE_START` is honored only on a fresh bucket — if you change it,
   wipe `galexie-live` first, or the resume wins.)

   > ⚠ **Keep `galexie_start_ledger` CLOSE to the live tip** (a few hundred
   > ledgers). captive-core replays start→tip as a *buffered* catchup only when
   > that gap is small; a large gap (thousands of ledgers) makes it do an
   > **online-catchup that SKIPS to live**, leaving galexie parked — it tracks
   > consensus but never exports, cycling "Waiting for trigger ledger". This
   > also bites when a **full role re-deploy restarts galexie repeatedly**: each
   > restart resumes from the last exported ledger, and if the tip has run far
   > ahead meanwhile, galexie strands. **Recovery** = stop galexie+indexer, wipe
   > `galexie-live`, `truncate ingestion_cursors` (+ `account_observer_watermark`),
   > set `galexie_start_ledger`/`backfill_from_ledger` a few hundred below the
   > CURRENT tip, then re-render (`--tags galexie,stellarindex`) so it starts
   > fresh. Binary-only deploys (deploy.yml) don't touch galexie and are safe.
2. indexer reads `galexie-live` live-only from `stellarindex_backfill_from_ledger`
   (== `galexie_start_ledger`) → ClickHouse + PG. Start it once galexie has
   exported the first object at/after that ledger, so its SDK backend does not
   error against an empty bucket.
3. `cap67-movements` follow daemon derives `account_movements` (`-floor-ledger`
   = `stellar_movements_floor_ledger`); the data begins at the recent start.
4. api serves `/v1/*`, bound to `stellarindex_api_listen_addr` (0.0.0.0:3000 on
   the VMs so the host's Caddy can reach it over the NAT).

## Step 5 — Host reverse proxy + DNS/TLS

The VMs have no public IP; the **host** runs one Caddy that fronts all
subdomains. DNS split (deliberate):

| Record | Cloudflare | Why |
| --- | --- | --- |
| `api.testnet.stellarindex.io` → A 95.217.126.13 | **DNS-only (grey)** | API serves SSE (live ledger/movements); proxying buffers and breaks it. Grey also lets Caddy solve ACME HTTP-01 → real LE cert here |
| `api.futurenet.stellarindex.io` → A | DNS-only (grey) | same (Phase 2) |
| `testnet.stellarindex.io` | **proxied (orange)** | serves the static Next.js explorer (cacheable/DDoS-protected); its live data comes from the grey `api.*` origin |
| `futurenet.stellarindex.io` | proxied (orange) | same (Phase 2) |

Install + deploy on the host:

```sh
rsync -a configs/libvirt/ root@95.217.126.13:/root/libvirt/
ssh root@95.217.126.13 'cd /root/libvirt && ./setup-host-proxy.sh'
```

`setup-host-proxy.sh` installs Caddy from its official apt repo and deploys
`configs/libvirt/host-Caddyfile` (LE for the grey `api.*`, `flush_interval -1`
for SSE). The explorer (orange) origins are wired when the explorer is
deployed, with a Cloudflare Origin-CA cert (orange domains can't win HTTP-01).

## Step 6 — Verify

- [ ] Ingest advancing: `/v1/ledger/tip` `latest_ledger` climbing, `stale:false`,
      `lag_seconds` low. galexie earliest exported == `backfill_from_ledger`.
- [ ] External: `curl https://api.testnet.stellarindex.io/v1/ledger/tip` returns
      200 with live data (proves DNS → host Caddy → LE cert → VM API over NAT).
- [ ] `/v1/assets/{id}` shows a **testnet** SAC contract address (not pubnet).
- [ ] No aggregator noise in logs (aggregator not deployed); `enabled_sources`
      is `[sdex]` only.

## Related

- [testnet-futurenet-reset-runbook.md](./testnet-futurenet-reset-runbook.md) — reset handling.
- `configs/ansible/inventory/testnet.example.yml`, `futurenet.example.yml`.
- `internal/config/config.go` `StellarConfig` — the network knobs.
- Protocol-upgrade pipeline: Futurenet → Testnet → Mainnet (futurenet is
  the early-warning network for new protocol op/event types).
