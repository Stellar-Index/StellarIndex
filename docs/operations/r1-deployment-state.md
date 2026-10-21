---
title: r1 archival node — historical bringup snapshot (2026-04/05)
last_verified: 2026-10-21
status: historical snapshot
---

# r1-01 (FSN1) deployment state

> **HISTORICAL SNAPSHOT, not current node state.** Known-stale: API
> `auth_mode` is now `apikey_optional` and the API binds `127.0.0.1:3000`
> behind Caddy; the `data` pool is **raidz1** (see §Disk layout). For live state query the
> host. Binary versions: [deployed-versions.md](deployed-versions.md). New
> node: [archival-node-bringup.md](archival-node-bringup.md).

Hetzner FSN1 dedicated; public IP is in `configs/ansible/inventory/r1.yml`
(gitignored).

## Hardware

Intel Core Ultra 7 265 (20 cores), 192 GB DDR5 ECC, 4 x 7.68 TB Samsung PM9A3
NVMe, Ubuntu 24.04.3 LTS noble.

## Disk layout

```
nvme0n1 / nvme1n1 : OS mirror (mdadm RAID1)
                     p1  /boot/efi  512M  (per-drive vfat, not RAID)
                     p2  swap       4G    (md0, raid1)
                     p3  /          50G   (md1, raid1)
                     p4  ~7.63T     (ZFS data-pool member)
nvme2n1 / nvme3n1 : untouched, full 7.68T (ZFS data-pool members)
```

ZFS pool `data`: a single **raidz1** vdev over 4 NVMe (SINGLE parity: tolerates
exactly ONE drive failure). Datasets (`zfs_datasets` in `defaults/main.yml`):
- `data/os` -> `/var/lib/stellarindex`
- `data/postgres` -> `/var/lib/postgresql` (recordsize=8K, logbias=throughput);
  child `data/postgres/wal` -> `/var/lib/postgresql/wal`
- `data/galexie` -> `/var/lib/galexie`
- `data/minio` -> `/var/lib/minio`
- `data/archive` -> `/srv/history-archive`
- also `data/{clickhouse,pgbackrest,prometheus,loki,restore-drill}`

> Authority: `zfs_data_pool_type` in `configs/ansible/inventory/r1.example.yml`;
> `scripts/ci/lint-docs.sh` §18 lints every r1-scoped file against it.
> Evidence: [storage-considerations.md](../architecture/storage-considerations.md).

A `data/core` dataset is gated behind `run_stellar_core` (`defaults/main.yml`,
default false); it does not exist on r1.

## Services (systemd)

| Service | State | Notes |
|---------|-------|-------|
| postgresql@15-main | active | TimescaleDB installed; `stellarindex` role + DB |
| redis-server | active | single node |
| ~~stellar-core~~ | REMOVED |
| ~~stellar-rpc~~ | REMOVED (masked) | indexer consumes galexie's MinIO output directly; see §Architecture |
| galexie | active | own captive-core; one `FC4A....xdr.zst` object per ledger into MinIO `galexie-live`. **The single stellar-core on the box.** |
| minio | active | buckets `galexie-live`, `galexie-archive`, `backups`; users `galexie-writer` (write-only `galexie-live`), `galexie-archive-writer` (write-only `galexie-archive`), `stellarindex-reader` (read on both); secret in `/etc/default/stellarindex` |
| stellarindex-indexer | active | reads galexie-live via S3 GetObject, cursor-resumable; source decoders -> `trades` + `oracle_updates`; /metrics `127.0.0.1:9464` |
| stellarindex-aggregator | active | VWAP/TWAP/divergence/freeze/supply tick; writes `vwap:<pair>:<window>` to Redis + `prices_1m` CAGG; /metrics `127.0.0.1:9465` |
| stellarindex-api | active | REST + SSE on `127.0.0.1:3000` (Caddy fronts it); /v1/healthz + /v1/readyz |
| prometheus-node-exporter | active | :9100 |
| ~~stellar-core-prometheus-exporter~~ | REMOVED | no /info on captives |
| node-healthcheck.timer | active | 5-min push to Healthchecks.io |

### First application bringup (R2 / R3)

Procedure: [archival-node-bringup.md](archival-node-bringup.md). Smoke:
`/v1/price?asset=native&quote=USDC:GA5Z…`. Historical backfill
`L50,457,424 -> L62,400,000` via `nohup /usr/local/bin/run-historical-backfill.sh`
(log `/var/log/stellarindex/backfill.log`; idempotent). The postgres password
is `/etc/stellarindex/postgres-password.txt` (mode 600); unit env is
`/etc/default/stellarindex`.

### Architecture after 2026-04-23 trim

```
pubnet -(SCP)-> galexie captive-core -> galexie -> MinIO galexie-live
  -> cmd/stellarindex-indexer (ledgerstream -> dispatcher) -> TimescaleDB
  -> `/v1/{price,vwap,twap,ohlc,...}` API
```

One stellar-core on the box; everything downstream of MinIO is a
batch/stream consumer.

## Stellar quorum set (trust anchors)

21 tier-1 validators across 7 orgs, identical to SDF's canonical
`packages/docs/examples/pubnet-validator-full/stellar-core.cfg`. The
www.franklintempleton.com archive frequently 404s upstream.

## What's running in background

- `/srv/history-archive` (`stellar-archivist mirror`, complete, 7.0 TB) is the
  full pubnet history and the trusted reference for `verify-archive` Tier B.
  It lands on ZFS, not MinIO.
- `galexie-archive` bucket: mirrored from the AWS public bucket to the tip by
  `galexie-archive-fill.timer` (hourly, per-partition `mc mirror`). Local copy
  starts at `stellarindex_archive_hot_floor` (49984000); older ledgers are on the
  cold tier (ADR-0027). Gotchas in [galexie-backfill.md](galexie-backfill.md).
- Healthchecks.io push every 5 min (service health, ledger age, ZFS, disk).

### Prometheus exporters

Tasks: `configs/ansible/roles/archival-node/tasks/16-prometheus-exporters.yml`,
tag `exporters`, after `10-observability.yml`. Until installed, the redis /
postgres / pgbackrest scrape jobs in `prometheus.r1.yml` are dead and their
rules sit on `absent_over_time`.

1. Set `pgbackrest_exporter_release_sha256: "<64 hex>"` in
   `configs/ansible/inventory/r1.secrets.yml` (default
   `pgbackrest_exporter_version: "0.23.0"` in `defaults/main.yml`; SHA from
   https://github.com/woblerr/pgbackrest_exporter/releases/tag/v0.23.0), and
   confirm apt `universe` is enabled.
2. Run the exporters tag:
   ```sh
   ansible-playbook -i configs/ansible/inventory/r1.yml \
     configs/ansible/playbooks/archival-node.yml --tags exporters
   ```
3. Optional least-privilege postgres_exporter: a `pg_monitor` role with
   CONNECT on `stellarindex`, then `user=` in
   `/etc/default/prometheus-postgres-exporter` (default is peer auth as `postgres`).
4. Verify `/metrics` on `127.0.0.1:9121` (redis), `:9187` (postgres), `:9854`
   (pgbackrest).

## Operating notes

1. **Captive-core ports.** `PEER_PORT = 0` is stripped by the go-stellar-sdk
   marshaller, so a captive falls back to 11625 and collides with the primary
   (SIGABRT loop). Every captive-core needs `HTTP_PORT=0` and its own non-zero PEER_PORT in a
   separate `/etc/stellar/captive-core*.cfg` (primary 11625, galexie captive 11726,
   backfill captive 11727; `defaults/main.yml`).
2. **Decoders are `BackfillSafe`** (`internal/sources/external/registry.go`)
   after per-WASM-hash audits (`docs/operations/wasm-audits/`); any upgrade
   needs the same:
   [ingest-pipeline.md#contract-schema-evolution](../architecture/ingest-pipeline.md#contract-schema-evolution).
3. **Deploy.** `gh workflow run deploy.yml -f region=r1 -f version=vX.Y.Z`;
   backups `/usr/local/bin/<binary>.prev-<previous-tag>` (latest 5 kept), running
   tag per binary in `/var/lib/stellarindex/deployed-versions/<binary>`;
   secrets `R1_HOST`, `R1_USER`, `DEPLOY_SSH_PRIVATE_KEY`, `R1_SSH_KNOWN_HOSTS`; per-region binary set in
   `scripts/dev/region-binaries.tsv`.
   Runbook: [deploy-workflow.md](deploy-workflow.md). linux/amd64 only;
   `cmd/` binaries: stellarindex-{indexer, aggregator, api, ops,
   migrate, sla-probe}.
4. **Auth.** r1 runs `[api].auth_mode = "apikey_optional"` in
   `/etc/stellarindex.toml`: `POST /v1/signup {"email": "..."}` returns a
   Starter key (1000 req/min) for `Authorization: Bearer <key>`; anonymous
   keeps 60/min; invalid key -> 401. Pro/Business needs `stellarindex-ops
   mint-key -rate-limit-per-min`.
5. **`galexie-archive` trails `galexie-live`.** `galexie-archive-fill.timer`
   extends it hourly; its tip is the `galexie_archive_tip_ledger` metric.
   Anything reading `galexie-archive` (`stellarindex-ops wasm-history`,
   `backfill`, `verify-archive`) MUST bound `-to` at or below that tip or fail
   on the partial trailing partition. `galexie-archive-contiguity.timer` scans
   it hourly; `/var/lib/galexie/detect-gaps.json` is an April snapshot.
   ch-backfill historical reads need `-bucket galexie-archive`.
6. **Monitoring.** Healthchecks.io catches total death only; Prometheus +
   AlertManager and Loki/Promtail are separate roles
   (`configs/ansible/roles/prometheus/`; `docs/architecture/ha-plan.md` §7.1, §7.2).
7. **pgBackRest.** `/etc/pgbackrest/pgbackrest.conf` (PG15),
   `pgbackrest-backup.timer` daily via `/usr/local/bin/pgbackrest-backup.sh`;
   weekly full + daily diff + continuous WAL. The stanza is **`stellarindex`**,
   not `main`; `pgbackrest info --stanza=stellarindex` = status ok.
8. **`/etc/stellarindex.toml`** carries:
   ```toml
   [aggregate]
   enable_stablecoin_fiat_proxy = true
   min_usd_volume = 0  # default 10000; can revert once CEX volume is sustained

   [trades]   # indexer restart needed
   usd_pegged_classic_assets = [
     "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
   ]

   [external]
     [external.binance]   enabled = true   # 4 pairs
     [external.kraken]    enabled = true   # 8 pairs
     [external.bitstamp]  enabled = true   # 7 pairs
     [external.coinbase]  enabled = true   # 3 pairs
     [external.coingecko] enabled = true   # 9 pairs (poller, divergence)
     [external.ecb]       enabled = true   # 9 pairs (poller, daily anchor)
   ```
   `[supply]` watches 8 `watched_classic_assets` (USDC / EURC / AQUA / yXLM /
   VELO / BLND / PHO / KALE); `watched_sep41_contracts` and
   `sdf_reserve_accounts` are empty (opt-in).
9. **Open.** Redis ACL lockdown (opt-in role flag); 1-year `prices_1m`
   backfill ([backfill-procedure.md](backfill-procedure.md)); p95 over SLA
   needs [multi-region cutover](../architecture/ha-plan.md#10-roadmap-and-launch-checklist);
   `/etc/prometheus/minio.token` (minted by `16-prometheus-exporters.yml`) must be non-empty or `up{job=minio}` is 0.

## Prometheus and Caddy

`/etc/prometheus/prometheus.yml` is `configs/prometheus/prometheus.r1.yml`
(rule_files `/etc/prometheus/rules.r1/*.yml`, copied from
`configs/prometheus/rules.r1/`). Caddy: `configs/caddy/Caddyfile.api` ->
`/etc/caddy/Caddyfile` (`caddy validate`, back up, `mv`, `systemctl reload
caddy`); `/metrics` must return 404, `/v1/healthz` 200; it sets
`client_ip_headers CF-Connecting-IP, X-Forwarded-For`.

## Evidence timers

Units from `deploy/systemd/` in `/etc/systemd/system/`
(`{sla-probe,archive-completeness,supply-snapshot,verify-archive-tier-a}.{service,timer}`),
`/var/lib/node_exporter/textfile_collector/`, and
`<svc>.service.d/override.conf` with `User=root` (no `stellarindex` user on r1).

- `stellarindex-sla-probe.timer` every 15 min. If anon rate-limits bite,
  mint `STELLARINDEX_PROBE_API_KEY` at Partner/Operator tier in
  `/etc/default/stellarindex-healthchecks`. Quote `PAIRS`:
  `Environment="PAIRS=..."`.
- `archive-completeness.timer` daily 02:17 UTC; `-to` = indexer cursor minus 64 from
  `/usr/local/sbin/compute-archive-to.sh` (`ExecStartPre`, writes
  `/run/archive-completeness.env`).
- `supply-snapshot.timer` daily (upserts via
  `ON CONFLICT ON CONSTRAINT asset_supply_history_asset_ledger_idx`; Timescale
  rejects column inference on hypertable unique indexes).
- `verify-archive-tier-a.timer` daily 03:23 UTC.

## Host firewall

Default-deny `/etc/nftables.conf` (`nftables.service` enabled) allows only:
22/tcp (**30/minute** per source, `ssh_allowed_v4` only;
`configs/ansible/roles/archival-node/templates/nftables.conf.j2`), 80/443
(Caddy), ICMP 10/sec, loopback. 11625 (SCP) opens only when
`run_stellar_core=true` (`public_allow_ports` in `defaults/main.yml`);
captive-cores dial out.

## Swap

`/swap_f1209`, 4G (fstab `/swap_f1209 none swap sw,pri=1 0 0`) plus the 4G
md0 swap: 8G. A stopgap; the real fix is more RAM or lower `shared_buffers`.

## Config pitfalls (all fixed in the role)

- The parent stellar-rpc must also pass `STELLAR_CAPTIVE_CORE_HTTP_PORT=0`.
- Galexie's append needs an explicit `--start` from an archive's
  `.well-known/stellar-history.json` minus a margin; install with `go install`
  at tag `galexie-vX.Y.Z`, then copy (NOT symlink) from `/root/go/bin/`.
- `[HISTORY.stellarindex]` (`put`/`mkdir`) belongs ONLY in the PRIMARY
  stellar-core config (`cfg_mode=='full'`); a captive that sees it stalls.

## Credentials (pointers, not values)

- Vault password: maintainer's password manager
- MinIO root and Postgres stellar-core role password: `inventory/r1.secrets.yml` (vaulted)
- Healthchecks.io ping URL: `/etc/default/node-healthcheck` (mode 0600)
- SSH admin key: ed25519 public key in `inventory/r1.yml`
