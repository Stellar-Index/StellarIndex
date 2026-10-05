---
title: Testnet + Futurenet deployment
last_verified: 2026-10-05
status: current
---

# Testnet + Futurenet deployment

How to stand up StellarIndex Testnet and Futurenet instances. Companion:
[testnet-futurenet-reset-runbook.md](./testnet-futurenet-reset-runbook.md).

## Topology decision

- **One Hetzner box, two libvirt/KVM VMs**, one per network. Mainnet is the only tier with HA / multiple regions (r1 + future r2/r3).
- **Box:** Hetzner Server Auction Xeon E-2176G (6c/12t, 64 GB ECC, 2×960 GB U.2 NVMe), Helsinki (Finland), ~€83/mo. IP 95.217.126.13.
- **Storage:** the NVMe pair is mdadm RAID0 (installimage, LVM `vg0` ~1.74 TB). Test-net data is re-ingestable, so no mirror; a drive failure means re-ingest. Each VM gets a plain virtual disk (no ZFS-on-ZFS).
- **Why VMs, not co-tenant bare metal:** the stack isn't instance-templated (ports, DB names, galexie buckets, ~40 systemd units would collide), so two VMs reuse the single-network ansible unchanged.

## What makes the code network-correct

`stellar.network = testnet|futurenet` drives everything:

| Concern | Mechanism |
| --- | --- |
| Passphrase | `cfg.Stellar.Passphrase()`; ansible `stellar_passphrase` feeds core.cfg + galexie |
| SAC contract addresses | `canonical.InstallNetworkPassphrase` at API start-up |
| Movements feed floor | `soroban_genesis_ledger` = **1** and `movements_floor_ledger` = **2** on test nets (else the feed floors above the whole chain; 2 because no lake holds genesis — the cap67 daemon also clamps up to `min(ledger_seq)` itself) |
| cap67 follow daemon | `-floor-ledger` = `stellar_movements_floor_ledger` |
| History archive | `stellar_history_archive_url` = core-testnet / core-futurenet; a pubnet (core-live) URL on a test net is **rejected** by config validation |
| Cross-anchor archive fill | refuses to write pubnet ledgers into a test-net archive |
| Sources | **SDEX only** (`stellarindex_enabled_sources: [sdex]`). Soroban DEX/AMM, oracles, CEX/FX and pricing are cut (`run_aggregator: false`); every asset is $0 off pubnet |
| Start ledger | `galexie_start_ledger` **==** `stellarindex_backfill_from_ledger`, a recent ledger, not genesis (testnet is ~4.3M ledgers). **Update both together after a reset** (ledgers restart at 1) |

## Deploy set

`galexie + stellarindex-indexer + stellarindex-api + stellarindex-ops`. **No aggregator.** `stellarindex-ops` is required: it runs the `cap67-movements` follow daemon that writes `/v1/accounts/{g}/movements`.

## Steps 1–2 — Host + VMs (libvirt/KVM)

Per [../../configs/libvirt/README.md](../../configs/libvirt/README.md): Debian 12 via installimage (`configs/libvirt/installimage-host.conf` — mdadm RAID0 + mirrored `/boot` + LVM `vg0`), then libvirt/KVM + two Ubuntu 24.04 VMs (`si-testnet` 192.168.122.10, `si-futurenet` 192.168.122.20) from `configs/libvirt/provision-vms.sh` with cloud-init. Each VM: 4 vCPU, 20 GB RAM, a 600 GB LV, on libvirt's private NAT, reached via ProxyJump through the host (encoded in the inventory). Ubuntu 24.04 matches r1 so the role deploys unchanged. Plain libvirt, not Proxmox.

## Step 3 — Inventory

```sh
cp configs/ansible/inventory/testnet.example.yml  configs/ansible/inventory/testnet.yml
cp configs/ansible/inventory/futurenet.example.yml configs/ansible/inventory/futurenet.yml
```

Fill the `X.X.X.X` VM IPs, `allowed_ssh_cidrs`, and create the vault secrets (`testnet.secrets.yml` / `futurenet.secrets.yml`) with DB / MinIO / CH-serving passwords (same shape as `r1.secrets.yml`). Filled inventories are untracked like `r1.yml` (`.gitignore` keeps only `*.example.yml`).

Pin the host keys before the first run — both SSH hops verify against `configs/ansible/inventory/known_hosts` (untracked) and fail on a mismatch:

```sh
ssh-keyscan -t ed25519 HOST_PUBLIC_IP > configs/ansible/inventory/known_hosts
ssh root@HOST_PUBLIC_IP 'ssh-keyscan -t ed25519 192.168.122.10 192.168.122.20' \
  >> configs/ansible/inventory/known_hosts
```

Re-capture a line only after confirming out of band why the key changed (reprovisioned VM, reinstalled host). Futurenet's captive-core passphrase and history archives are set in role defaults (galexie has no futurenet preset).

## Step 4 — Deploy + bring-up

Run the archival-node role against the inventory (via `deploy.yml` or directly). The role is r1-safe: ClickHouse (`run_clickhouse`, default **false**), Redis, the CH schema apply, the galexie drift-guard skips and the from-source build are opt-in flags set only in test-net inventories. Order per VM:

1. galexie captive-core catches up to `galexie_start_ledger` via the network archive, then exports start → tip → live into `galexie-live`. On restart it **resumes** from the last exported ledger; `GALEXIE_START` is honoured only on an empty bucket, so wipe `galexie-live` before changing it.

   > ⚠ **Keep `galexie_start_ledger` within a few hundred ledgers of the live tip.** A large gap makes captive-core do an online catchup that skips to live, leaving galexie parked ("Waiting for trigger ledger") and never exporting. A full role re-deploy that restarts galexie repeatedly while the tip runs ahead strands it the same way. **Recovery:** stop galexie+indexer, wipe `galexie-live`, `truncate ingestion_cursors` (+ `account_observer_watermark`), set `galexie_start_ledger`/`backfill_from_ledger` a few hundred below the current tip, re-render (`--tags galexie,stellarindex`). Binary-only deploys (deploy.yml) don't touch galexie.
2. The indexer reads `galexie-live` live-only from `stellarindex_backfill_from_ledger` → ClickHouse + PG. Start it after galexie exports the first object at/after that ledger, or its SDK backend errors on an empty bucket.
3. `cap67-movements` derives `account_movements` (`-floor-ledger` = `stellar_movements_floor_ledger`) from the recent start.
4. The API serves `/v1/*` on `stellarindex_api_listen_addr` (0.0.0.0:3000 on the VMs, so the host's Caddy reaches it over the NAT).

## Step 5 — Host reverse proxy + DNS/TLS

The VMs have no public IP; one Caddy on the host fronts every subdomain:

| Record | Cloudflare | Why |
| --- | --- | --- |
| `api.testnet.stellarindex.io` → A 95.217.126.13 | **DNS-only (grey)** | SSE breaks behind the proxy's buffering; grey also lets Caddy solve ACME HTTP-01 for a real LE cert |
| `api.futurenet.stellarindex.io` → A | DNS-only (grey) | same |
| `testnet.stellarindex.io` | **proxied (orange)** | static Next.js explorer; live data from the grey `api.*` origin |
| `futurenet.stellarindex.io` | proxied (orange) | same |

```sh
rsync -a configs/libvirt/ root@95.217.126.13:/root/libvirt/
ssh root@95.217.126.13 'cd /root/libvirt && ./setup-host-proxy.sh'
```

`setup-host-proxy.sh` installs Caddy from its apt repo and deploys `configs/libvirt/host-Caddyfile` (LE for grey `api.*`, `flush_interval -1` for SSE). Orange explorer origins use a Cloudflare Origin-CA cert (orange domains can't win HTTP-01).

## Step 6 — Verify

- [ ] `/v1/ledger/tip`: `latest_ledger` climbing, `stale:false`, `lag_seconds` low. galexie earliest exported == `backfill_from_ledger`.
- [ ] `curl https://api.testnet.stellarindex.io/v1/ledger/tip` returns 200 with live data (DNS → Caddy → LE cert → VM API).
- [ ] `/v1/assets/{id}` shows a **testnet** SAC contract address.
- [ ] No aggregator in logs; `enabled_sources` is `[sdex]` only.

## Step 7 — Failed units on a fresh test net

Triage `systemctl list-units --state=failed` before "fixing" anything:

| Unit | Cause | Disposition |
| ---- | ----- | ----------- |
| `galexie-archive-fill` | Mirrors AWS Public Blockchain Data's `.../ledgers/pubnet/`; AWS publishes no testnet/futurenet dataset. | **Omitted** — gated on `stellar_network == 'pubnet'`, with a removal block for older nets. |
| `config-assertions` | Asserted ZFS integrity + mainnet compression wants on a no-ZFS VM. | **Fixed** — `have_zfs()` / `is_pubnet()` gates + an explicit `_skipped` series. |
| `verify-archive-tier-a` | Startup ordering: the 03:23 timer fired before `galexie-archive` existed (MinIO answers `AccessDenied` for a missing bucket). | **No fix** — passes once the archive exists; `systemctl start verify-archive-tier-a` clears the state. |
| `verify-archive-tier-b` | Anchors against a local archivist mirror (`VERIFY_ARCHIVE_ROOT`, `/srv/history-archive`) only populated for pubnet → mismatch at ledger 63. | **Omitted** — `verify_archive_tier_b_enabled` defaults to `stellar_network == 'pubnet'`. To enable, build a network-correct mirror with stellar-archivist from `stellar_history_archive_url_set`, then set the flag. |
| `galexie-archive-trim` | Needs `vault_stellarindex_archive_trimmer_secret_key`, which only r1's vault defines. | **Omitted** — `galexie_archive_trim_enabled` defaults to `stellar_network == 'pubnet'`. |
| `archive-completeness` | Scan exceeds `TimeoutStartUSec=30min` → SIGTERM. `compute-archive-to.sh` already scans to the tip, so it won't self-resolve. | **Open** — longer timeout on the lean VMs, or a windowed scan. |
| `pgbackrest-backup` | Exit 127: the role installs pgBackRest nowhere. | **Omitted** — `pgbackrest_backup_enabled` defaults to `stellar_network == 'pubnet'` (set explicitly in all four test-net inventories); the offsite-backup refusal assert is gated on the same flag. |
| `stellar-core` | Expected: `run_stellar_core: false`. | **Residue** — `reset-failed`. |
| `stellarindex-aggregator` | Exit 203: not deployed on SDEX-only nets. | **Residue** — `reset-failed`. |

- **A `disabled` unit in `failed` state is residue.** Check `systemctl list-timers --all`; only timer-driven units recur.
- **Never reproduce a unit by rebuilding its environment on the command line.** `sudo -u stellarindex env $(grep ... | xargs) <cmd>` puts every secret into argv, which sudo logs to the journal (shipped to Loki). Use `systemctl start <unit>` or `sudo -u <user> bash -c 'set -a; . /etc/default/<file>; set +a; exec <cmd>'`. A credential-less run returns `AccessDenied` and looks like a MinIO policy gap when the policy is fine.

## Keeping the test nets in step with r1

Test nets take releases after r1, by a separate `deploy.yml` dispatch per region, and r1's Prometheus cannot see the NAT-only VMs. Two checks catch drift, both running [`scripts/ci/check-fleet-release-drift.sh`](../../scripts/ci/check-fleet-release-drift.sh) (fixture-tested in CI; its header documents version comparison, pre-release and sanitisation rules):

| Check | When | What it does |
| --- | --- | --- |
| `deploy.yml` → **Fleet release parity** step | after every deploy | Reads `/v1/version` on both test nets against the tag just deployed and prints the catch-up dispatches. A `::warning::`, never a failure — r1 goes first by design. |
| [`fleet-release-drift.yml`](../../.github/workflows/fleet-release-drift.yml) | daily 07:25 UTC | Compares against live r1 with a **24h grace** per test net, measured from the **oldest r1 release it lacks**. Past the grace it opens/updates one issue (`Fleet release drift: a test net is behind r1`, label `fleet-release-drift`), fails the run, and closes the issue once every test net serves r1's release. An unreadable host is `DEGRADED`, never in step. |

- **What it reads:** `GET /v1/version` on the `api.*` names (the binary's `git describe`, answered whether or not the binary is ready). An HTTP error, a redirect, or a body without `.data.version` is `DEGRADED`. The bare `testnet.stellarindex.io` / `futurenet.stellarindex.io` names 301 to mainnet's API; redirects are never followed.
- **What it does not prove:** the version is what the host says it runs; this is a tripwire against a region nobody redeployed, not a control against a lying host. Per host: up to 3 attempts (`--retry 2`), 5 s apart, 20 s cap each.
- **Migrations are derived from tags** (`git ls-tree <tag> migrations/`), since `deploy-binary.yml` applies a tag's migrations before the binary swap. The blind spot is `migrations_skip=true`: that host fails `/v1/readyz` (`schema` check) but still reads as in step.
- **Pre-releases:** `git describe` labels on a tag (`-3-gabc1234`, `-dirty`) compare equal to it; `-rc.N` sorts below its release. While r1 serves a pre-release, every test net not on the identical version string is `DRIFT` with no grace.

Catch up with one dispatch per region (test-net binary set from `scripts/dev/region-binaries.tsv`; no aggregator, sla-probe and migrate included, or deploy.yml refuses the split release):

```sh
gh workflow run deploy.yml -f region=testnet   -f version=vX.Y.Z -f binaries=stellarindex-indexer,stellarindex-api,stellarindex-sla-probe,stellarindex-ops,stellarindex-migrate
gh workflow run deploy.yml -f region=futurenet -f version=vX.Y.Z -f binaries=stellarindex-indexer,stellarindex-api,stellarindex-sla-probe,stellarindex-ops,stellarindex-migrate
```

Verify: `curl -sf https://api.testnet.stellarindex.io/v1/version | jq -r .data.version` equals r1's; the next scheduled run closes the issue.

## Known gaps

- `/v1/markets` on test nets is empty: it builds its pair set from the empty `prices_1d` CAGG (use `/v1/pools`).
- The test-net indexer heartbeat is wired by INV-1469 (closed 10-05).

## Related

- [testnet-futurenet-reset-runbook.md](./testnet-futurenet-reset-runbook.md) — reset handling.
- `configs/ansible/inventory/testnet.example.yml`, `futurenet.example.yml`.
- `internal/config/config.go` `StellarConfig` — the network knobs.
- Protocol-upgrade pipeline: Futurenet → Testnet → Mainnet (futurenet is the early warning for new op/event types).
