---
title: r1 archival node — historical bringup snapshot (2026-04/05)
last_verified: 2026-05-12
status: historical snapshot
---

# r1-01 (FSN1) deployment state

> **HISTORICAL SNAPSHOT, not current node state.** Captured at bringup
> (2026-04/05), not maintained per session. Known-stale: API `auth_mode` is
> now `apikey_optional` (item 5f), not `none`; "15 migrations" is the bringup
> count (`ls migrations/ | tail` for head); the `data` pool is **raidz1**
> (see §Disk layout). For live state query the host. Deployed binary versions:
> [deployed-versions.md](deployed-versions.md). Bringing up a new node:
> [archival-node-bringup.md](archival-node-bringup.md).

Hetzner FSN1 dedicated; public IP is in `configs/ansible/inventory/r1.yml`
(gitignored).

## Hardware

- Intel Core Ultra 7 265 (20 cores, 8 P + 12 E), 192 GB DDR5 ECC
- 4 x 7.68 TB Samsung PM9A3 NVMe (1 DWPD / 14 PBW)
- Ubuntu 24.04.3 LTS noble

## Disk layout

```
nvme0n1 / nvme1n1 : OS mirror (mdadm RAID1)
                     p1  /boot/efi  512M  (per-drive vfat, not RAID)
                     p2  swap       4G    (md0, raid1)
                     p3  /          50G   (md1, raid1)
                     p4  ~7.63T     (ZFS data-pool member)
nvme2n1 / nvme3n1 : untouched, full 7.68T (ZFS data-pool members)
```

ZFS pool `data`: a single **raidz1** vdev (SINGLE parity: tolerates exactly
ONE drive failure), ~18.3 TB / ~16.8 TiB usable. Datasets:
- `data/os` -> `/var/lib/stellarindex`
- `data/postgres` -> `/var/lib/postgresql` (recordsize=8K, logbias=throughput)
- `data/galexie` -> `/var/lib/galexie`
- `data/minio` -> `/var/lib/minio`
- `data/archive` -> `/srv/history-archive`

> Authority for the topology is `zfs_data_pool_type` in
> `configs/ansible/inventory/r1.example.yml`; `scripts/ci/lint-docs.sh` §18
> lints every r1-scoped file against it (earlier text said raidz2; the
> live pool is raidz1 and the measured footprint, ~16.8 T, does not fit two-parity). Evidence:
> [storage-considerations.md](../architecture/storage-considerations.md).

`data/core` and `data/rpc` are gated behind `run_stellar_core` /
`run_stellar_rpc` in the ansible role (`defaults/main.yml`); both default
false and neither dataset exists on r1. Re-enable `run_stellar_core` only
for Phase-3 validator work.

## Services (systemd)

State as of 2026-05-03.

| Service | State | Notes |
|---------|-------|-------|
| postgresql@15-main | active | TimescaleDB installed; `stellarindex` role + DB |
| redis-server | active | single node |
| ~~stellar-core~~ | REMOVED 2026-04-23 | archive pipeline doesn't need it; revival = Phase 3 Tier-1 validator |
| ~~stellar-rpc~~ | REMOVED 2026-04-23 | our indexer consumes galexie's MinIO output directly (`ingest.ApplyLedgerMetadata`); see §Architecture |
| galexie | active | own captive-core; one `FC4A....xdr.zst` object per ledger into MinIO `galexie-live`. **The single stellar-core on the box.** |
| minio | active | buckets `galexie-live`, `galexie-archive`, `backups`; `stellarindex-reader` user (read-only on both galexie buckets), secret in `/etc/default/stellarindex` |
| stellarindex-indexer | active | reads galexie-live via S3 GetObject, cursor-resumable; 11 source decoders -> `trades` + `oracle_updates`; /metrics `127.0.0.1:9464` |
| stellarindex-aggregator | active | VWAP/TWAP/divergence/freeze/supply tick; writes `vwap:<pair>:<window>` to Redis + `prices_1m` CAGG; /metrics `127.0.0.1:9465` |
| stellarindex-api | active | REST + SSE on `0.0.0.0:3000`; /v1/healthz + /v1/readyz |
| node_exporter | active | :9100 |
| ~~stellar-core-prometheus-exporter~~ | REMOVED 2026-04-23 | captives expose no /info |
| node-healthcheck.timer | active | 5-min push to Healthchecks.io |

### 2026-05-03 first application bringup

Sequence for R2 / R3 to follow:

1. `apt install redis-server`.
2. `apt install timescaledb-2-postgresql-15` (PackageCloud repo); add
   `timescaledb` to `shared_preload_libraries`; restart postgres;
   `CREATE EXTENSION timescaledb`.
3. Generate the `stellarindex` postgres password into
   `/etc/stellarindex/postgres-password.txt` (mode 600).
4. Rotate the `stellarindex-reader` MinIO secret into `/etc/default/stellarindex`.
5. Copy the 4 binaries + migrations dir to r1.
6. `stellarindex-migrate up` (migration 0005 needed the partition column
   `time` in its TimescaleDB unique index).
7. Write `/etc/systemd/system/stellarindex-{indexer,aggregator,api}.service`
   reading env from `/etc/default/stellarindex`.
8. `systemctl enable --now` in order: indexer, aggregator, api.
9. Smoke: `/v1/price?asset=native&quote=USDC:GA5Z…` returns a closed-bucket VWAP.
10. Historical backfill `L50,457,424 -> L62,400,000` via
    `nohup /usr/local/bin/run-historical-backfill.sh`, log
    `/var/log/stellarindex/backfill.log`; idempotent (the `trades` unique
    index dedupes).

### Architecture after 2026-04-23 trim

```
Stellar pubnet -(SCP)-> galexie's captive-core -> galexie -> MinIO galexie-live
                                                                  |
                                                                  v
                                       cmd/stellarindex-indexer (Galexie -> ledgerstream -> dispatcher)
                                                                  |
                                                                  v
                                                             TimescaleDB
                                                                  |
                                                                  v
                                                          `/v1/{price,vwap,twap,ohlc,...}` API
```

One stellar-core on the box; everything downstream of MinIO is a
batch/stream consumer.

## Stellar quorum set (trust anchors)

21 tier-1 validators across 7 orgs, identical to SDF's canonical
`packages/docs/examples/pubnet-validator-full/stellar-core.cfg` (fetched
2026-04-23): publicnode.org (3), lobstr.co (3), www.franklintempleton.com (3;
archive frequently 404s upstream), satoshipay.io (3), stellar.creit.tech (3),
www.stellar.org (3), stellar.blockdaemon.com (3).

## What's running in background

- ~~`stellar-archivist mirror`~~ completed: `/srv/history-archive` (7.0 TB at
  the time) is the full pubnet history, the trusted reference for
  `verify-archive` Tier B.
- `galexie.service`: live tail, one `.xdr.zst` per closed ledger into
  `galexie-live/`.
- `galexie-archive` bucket: historical fill complete 2026-04-26 (4.76 TB, 974
  partitions, ledgers 1 -> ~62.3 M), mirrored from the AWS public bucket by
  per-partition `mc mirror`; recovery runbook and the
  `mc mirror --overwrite=false` gotcha in
  [galexie-backfill.md](galexie-backfill.md).
- Healthchecks.io push every 5 min: service health, ledger age, ZFS health,
  disk space (watches galexie, minio, postgresql, node_exporter).

### Prometheus-exporter installation (F-0152, repo-side)

The `prometheus.r1.yml` scrape jobs for redis / postgres / pgbackrest
exporters were placeholders (exporters never installed), so `cache.yml`,
`storage.yml` and pgbackrest.* rules in `infra.yml` were silently
`absent_over_time`; only the F-0085 meta-alert surfaced it. Tasks:
`configs/ansible/roles/archival-node/tasks/16-prometheus-exporters.yml`
(Debian packages for redis_exporter + postgres_exporter, pinned tarball for
pgbackrest_exporter), tag `exporters`, after `10-observability.yml`.

Operator steps:

1. Set `pgbackrest_exporter_release_sha256: "<64 hex>"` in
   `configs/ansible/inventory/r1.secrets.yml` (or non-secret vars); default
   `pgbackrest_exporter_version: "0.18.0"` in `defaults/main.yml`; SHA from
   https://github.com/woblerr/pgbackrest_exporter/releases/tag/v0.18.0.
2. Confirm `universe` is enabled in apt sources (`apt-cache policy
   prometheus-redis-exporter` shows a candidate; else `add-apt-repository universe`).
3. Run:
   ```sh
   ansible-playbook -i configs/ansible/inventory/r1.yml \
     configs/ansible/playbooks/archival-node.yml \
     --tags exporters
   ```
4. Optional least-privilege postgres_exporter (default is peer auth as the
   `postgres` superuser):
   ```sql
   CREATE USER postgres_exporter;
   GRANT pg_monitor TO postgres_exporter;
   GRANT CONNECT ON DATABASE stellarindex TO postgres_exporter;
   ```
   then swap `user=postgres` -> `user=postgres_exporter` in
   `/etc/default/prometheus-postgres-exporter` and reload.
5. Verify (F-0085 `up{job="<exporter>"} == 0` alerts clear within one scrape):
   ```sh
   curl -s http://127.0.0.1:9121/metrics | head -5   # redis
   curl -s http://127.0.0.1:9187/metrics | head -5   # postgres
   curl -s http://127.0.0.1:9854/metrics | head -5   # pgbackrest
   curl -s http://127.0.0.1:9090/api/v1/targets | jq '.data.activeTargets[] | select(.labels.job | test("exporter")) | {job: .labels.job, health}'
   ```

## Known gaps / next-session priorities

### Unblocked
1. **Galexie PEER_PORT collision fixed.** `PEER_PORT = 0` in captive-core.cfg
   is stripped by the go-stellar-sdk toml marshaller (omitempty), so
   stellar-core fell back to pubnet's 11625, collided with the primary and
   SIGABRTed. Fix (commit `507e4de`): distinct non-zero PEER_PORT per captive
   (primary 11625, stellar-rpc captive 11725, galexie captive 11726) in
   separate `/etc/stellar/captive-core*.cfg`.

2. **SCVal decoders + per-WASM-hash audits done.** All source decoders have
   real bodies; audits completed 2026-04-29 (soroswap, aquarius, phoenix,
   comet, reflector-{dex,cex,fx}, redstone, band) and blend 2026-05-02 (5h4m
   walk over [50457424, 62249727], 11 contracts / 3 unique WASMs, no
   mid-life upgrades), so `BackfillSafe: true` is set in
   `internal/sources/external/registry.go`. Evidence:
   `docs/operations/wasm-audits/{<source>.md,evidence/}`,
   `docs/operations/wasm-audits/r1-walk-2026-05-01.md`, and
   `docs/operations/wasm-audits/blend.md §"Phase 2 results"`. Per-WASM-hash
   discipline still applies to any future upgrade:
   [contract-schema-evolution.md](../architecture/contract-schema-evolution.md).

5e. **Tagged-release deploy.** `gh workflow run deploy.yml -f region=r1 -f
   version=vX.Y.Z` downloads SHA256-verified binaries from the GitHub
   Release, ships them over SSH and runs
   `configs/ansible/playbooks/deploy-binary.yml`: stage, backup, atomic
   install, restart, /v1/healthz probe, automatic rollback on failure.
   Backups: `/usr/local/bin/<binary>.prev-<previous-tag>` (5 retained);
   running tag per binary in `/var/lib/stellarindex/deployed-versions/<binary>`.
   Needs 4 GitHub secrets per region (`R1_HOST`, `R1_USER`,
   `DEPLOY_SSH_PRIVATE_KEY`, `R1_SSH_KNOWN_HOSTS`). Runbook:
   [deploy-workflow.md](deploy-workflow.md).

5f. **Self-service signup + `apikey_optional` auth.** `POST /v1/signup
   {"email": "..."}` returns a Starter-tier key (1000 req/min), used as
   `Authorization: Bearer <key>`. r1: `[api].auth_mode = "apikey_optional"`
   in `/etc/stellarindex.toml`; anonymous public surface keeps the 60/min
   anon tier; invalid key -> 401. Release artifacts are linux/amd64 only;
   `cmd/` is six binaries: stellarindex-{indexer, aggregator, api, ops,
   migrate, sla-probe}. Verified: signup -> key; /v1/healthz anon 200;
   /v1/account/me anon 401, with key 200, garbage key 401; /v1/price anon 200.
   Gaps: Stripe webhook to lift `RateLimitPerMin` not built (Pro/Business
   needs `stellarindex-ops mint-key` with `-rate-limit-per-min`); email
   verification not enforced.

### Important but not urgent
3. **Firewall + SSH hardening (phase 3)**: superseded by §F-1201 host
   firewall below.

3a. **`galexie-archive` is frozen at the historical-fill tip; `galexie-live`
   keeps advancing in a different bucket.** The historical fill stopped
   2026-04-28 at a verified tip of **62,249,727**; live galexie writes to
   `galexie-live` (`/etc/galexie/galexie.toml`). Anything reading
   `galexie-archive` (`stellarindex-ops wasm-history`, `backfill`,
   `verify-archive`) MUST bound `-to` <= 62,249,727 or fail with "ledger
   object … does not exist" on the partial trailing partition
   (FC49CDFF--62272000-62335999, 24,695/64,000 files). `detect-gaps`
   (2026-04-28) found 242 gaps totalling 459,966 missing ledgers, all in the
   40M-41M and 44M-45M ranges (`/var/lib/galexie/detect-gaps.json`); the
   [50,457,424, 62,249,727] range is gap-free. Fix options: periodic
   `mc cp --recursive local/galexie-live/ local/galexie-archive/`; or point
   live galexie at `galexie-archive` and drop `galexie-live`; or make
   `internal/datastore` walkers read both buckets. ch-backfill historical
   reads need `-bucket galexie-archive` (it defaults to the trimmed live bucket).

4. **Layer-2 monitoring (Prometheus on a separate box)**: roles
   `configs/ansible/roles/prometheus/` (+ AlertManager) and Loki/Promtail
   exist; notes in `docs/architecture/prometheus-ansible-role-design-note.md`
   and `docs/architecture/loki-ansible-role-design-note.md`. Layer-1
   (Healthchecks.io) catches total death only.

5. ~~pgBackRest~~ **RESOLVED (verified 2026-06-30).** Config
   `/etc/pgbackrest/pgbackrest.conf` (PG15), `pgbackrest-backup.timer` daily
   via `/usr/local/bin/pgbackrest-backup.sh`; weekly full + daily diff +
   continuous WAL; `pgbackrest info --stanza=stellarindex` = status ok. The
   stanza is **`stellarindex`**, not `main` (`main` reports a spurious
   "missing stanza path"). A stale `/etc/pgbackrest.conf` with a commented
   PG13 `pg1-path` lingers, harmless.

5a. ~~Aggregator emitting zero VWAP rows on-chain pairs~~ **RESOLVED
   2026-05-04.** Fixes: stablecoin-fiat-proxy expansion reads
   `[trades].usd_pegged_classic_assets` (a5cb70575); `defaultPairs()` emits
   both `crypto:XLM/fiat:*` and `native/fiat:*` (b00c8f3dc); `/v1/price`
   Redis-VWAP fallback serves direct rewrites, since for
   aggregator-rewritten pairs the Redis `vwap:` key is the source of truth
   (28cdf55ab); fallback lookup 1m -> 5m because default windows are
   `[5m, 1h, 24h]` (3d529fd35). r1 `/etc/stellarindex.toml`:
   ```toml
   [aggregate]
   enable_stablecoin_fiat_proxy = true
   min_usd_volume = 0  # default 10000; stop-gap while r1 was on-chain-only

   [trades]
   usd_pegged_classic_assets = [
     "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
   ]
   ```
   Indexer restart needed for `[trades]`. Open at the time:
   `/v1/price/tip?asset=native&quote=fiat:USD` 404 (different reader path,
   no Redis fallback).

5c. ~~Discovery sink dropping ~3k hits/min~~ **RESOLVED (111a1573a).** The
   async discovery sink keeps a process-local `(contract_id, event_type)`
   seen-set and skips repeats (the recorder upserts on that key); the mark
   rolls back on real buffer-saturation drops; restart resets the set.

5b. **`issuers` seeded from `classic_assets`** by direct SQL (25,256 rows) so
   `/v1/issuers/{g_strkey}` returns data; the accounts decoder overwrites
   with auth flags + home_domain.

5d. **CEX/aggregator/sanity connectors enabled (2026-05-05).** `[external]`
   was absent so every venue defaulted to `enabled=false`; r1 now enables
   six free venues (no paid-tier keys):
   ```toml
   [external]
     [external.binance]   enabled = true   # 4 pairs
     [external.kraken]    enabled = true   # 8 pairs
     [external.bitstamp]  enabled = true   # 7 pairs
     [external.coinbase]  enabled = true   # 3 pairs
     [external.coingecko] enabled = true   # 9 pairs (poller, divergence)
     [external.ecb]       enabled = true   # 9 pairs (poller, daily anchor)
   ```
   Backup `/etc/stellarindex.toml.bak.pre-cex-20260505-123044`. This closed
   the `crypto:XLM/fiat:USD` 404. Gaps then: Binance XLMUSDT absent from the
   `crypto:XLM/fiat:USD` sources (USDT->USD proxy may collapse only on-chain
   trades); `/v1/price?asset=USDC:GA5Z…&quote=fiat:USD` 404 (no USDC/USD ~ 1.0
   synthesis); `min_usd_volume = 0` can revert to `10000` once CEX volume is
   sustained.

6. **`stellar-archivist mirror` -> MinIO sync.** The mirror lands on ZFS
   (`/srv/history-archive/`), not MinIO; `aws s3 sync` into `galexie-archive`
   for durability remains a TODO.

### Backlog
7. ~~Dedicated write-only MinIO users~~ **Done:** `galexie-writer`
   (write-only `galexie-live`), `galexie-archive-writer` (write-only
   `galexie-archive`), `stellarindex-reader` (read on both).

8. Galexie's wrapper restarts from `archive_tip - 128` every time; long-term
   should probe MinIO for the last exported ledger and resume past it.

## Configuration pitfalls captured during first deploy

All fixed in the role; kept so the lessons survive:

1. With a separate primary stellar-core on the box, every captive-core child
   MUST set `HTTP_PORT=0` and a **non-zero, distinct** `PEER_PORT` (see item 1
   above; `PeerPort` has `toml:"PEER_PORT,omitempty"` in go-stellar-sdk
   toml.go:90). Symptom: `std::system_error(98, "Address already in use",
   "bind: …")` -> SIGABRT -> restart loop. Each captive needs its own
   captive-core.cfg; the parent must also pass
   `STELLAR_CAPTIVE_CORE_HTTP_PORT=0` (stellar-rpc validates agreement).
2. Galexie's config schema is `[datastore_config]` / `[stellar_core_config]`;
   match `config/config.example.toml` at the pinned tag.
3. stellar-core's config parser has no "return to root": top-level directives
   (KNOWN_PEERS, NETWORK_PASSPHRASE, DATABASE, ...) must precede any
   `[...]` / `[[...]]` block.
4. `apt-stellar-rpc`'s `/etc/default/stellar-rpc` hard-codes futurenet and
   systemd `EnvironmentFile` beats `--config-path`; blank the file or drop
   `EnvironmentFile` (the role does the latter).
5. Galexie's append subcommand needs an explicit `--start`; the wrapper must
   query an archive's `.well-known/stellar-history.json` (NOT stellar-core's
   live tip; archives lag 5-15 min) and subtract a safety margin.
6. The stellar-galexie tag format is `galexie-vX.Y.Z`; no prebuilt binaries.
   `go install github.com/stellar/stellar-galexie@galexie-vX.Y.Z`, then copy
   (NOT symlink) from `/root/go/bin/` to `/usr/local/bin/` (`/root` is 0700).
7. `[HISTORY.stellarindex]` (local archive block with `put`/`mkdir`) must
   appear ONLY in the PRIMARY stellar-core config. A captive that sees
   `put`/`mkdir` tries to publish checkpoints, loops "Activating publish for
   ledger X" every 2s and stalls ingestion. The template gates it on
   `cfg_mode=='full'`.
8. stellar-rpc v26.0.0-189 requires `CAPTIVE_CORE_CONFIG_PATH`;
   `SERVE_LEDGERS_FROM_DATASTORE` only augments historical reads. This is why
   stellar-rpc was dropped in favour of our own consumer around
   `ingest.ApplyLedgerMetadata` on the galexie datastore.

### 2026-05-12 audit remediation deploy (v0.5.0-rc.49)

- Indexer/aggregator/api on `v0.5.0-rc.49` via `gh workflow run deploy.yml`.
- Prometheus (F-1219/F-1220/F-1252): `/etc/prometheus/prometheus.yml` replaced
  by `configs/prometheus/prometheus.r1.yml` (rule_files glob
  `/etc/prometheus/rules.r1/*.yml`; scrape jobs for redis_exporter,
  alertmanager, postgres_exporter, pgbackrest_exporter, minio); 18 rule
  families copied from `configs/prometheus/rules.r1/` (loaded 6 -> 19);
  backups `/etc/prometheus/{prometheus.yml,rules.d}.bak-pre-rc49`;
  `/etc/prometheus/minio.token` empty until a real `mc admin prometheus
  generate` token is wired (until then `up{job=minio}` is 0).
- TOML (F-1266): `[supply]` with 8 `watched_classic_assets` (USDC / EURC /
  AQUA / yXLM / VELO / BLND / PHO / KALE, mirroring
  `internal/currency/data/seed.yaml`); `watched_sep41_contracts` and
  `sdf_reserve_accounts` empty (opt-in); backup
  `/etc/stellarindex.toml.bak-pre-supply`.
- Still operator-discretion then: F-1213 (Redis ACL lockdown, opt-in flag in
  the ansible role); F-1265 (1-year `prices_1m` backfill per
  [backfill-procedure.md](backfill-procedure.md)); F-1267 (p95 over SLA
  target; needs multi-region cutover per
  [r2-r3-bringup.md](../architecture/r2-r3-bringup.md)).

### F-1223 Caddyfile roll

r1 served `/metrics` publicly (200). Roll: `scp configs/caddy/Caddyfile.api
root@…:/etc/caddy/Caddyfile.new`; `caddy validate --config
/etc/caddy/Caddyfile.new`; backup `/etc/caddy/Caddyfile.bak-pre-f1223-<ts>`;
`mv` into place; `systemctl reload caddy`. Post-roll `/metrics` -> 404,
`/v1/healthz` -> 200. The file also sets `client_ip_headers CF-Connecting-IP,
X-Forwarded-For` (F-1224 real-client-IP).

### F-1205 evidence timers installed

Installed from `deploy/systemd/` to `/etc/systemd/system/`
(`{sla-probe,archive-completeness,supply-snapshot,verify-archive-tier-a}.{service,timer}`),
created `/var/lib/node_exporter/textfile_collector/`, and added
`/etc/systemd/system/<svc>.service.d/override.conf` with `User=root` (no
`stellarindex` user on r1; every daemon runs as root), then `systemctl
daemon-reload && systemctl enable --now <timer>`.

- `stellarindex-sla-probe.timer` every 15 min. If anon rate-limits bite,
  mint `STELLARINDEX_PROBE_API_KEY` at Partner/Operator tier in
  `/etc/default/stellarindex-healthchecks`. The unit's `PAIRS` must be
  quoted: `Environment="PAIRS=..."`.
- `archive-completeness.timer` every 4 h; `-to <ledger>` is computed at
  start by `/usr/local/sbin/compute-archive-to.sh` (drop-in `ExecStartPre`:
  indexer cursor minus 64 ledgers, written to
  `/run/archive-completeness.env`).
- `supply-snapshot.timer` daily. Timescale (PG16 / TS 2.16) rejects
  `ON CONFLICT (cols)` inference against UNIQUE INDEXES on hypertables, so
  migration 0030 promotes the index to a UNIQUE CONSTRAINT (DROP INDEX + ADD
  CONSTRAINT; `USING INDEX` is also rejected) and Go uses
  `ON CONFLICT ON CONSTRAINT asset_supply_history_asset_ledger_idx`.
- `verify-archive-tier-a.timer` weekly (~10k ledgers/sec).

### F-1201 host firewall

nftables was inactive with an empty ruleset; MinIO 9000/9001, Prometheus
9090, Loki 3100, Promtail 9080/38563, node_exporter 9100 and Galexie 6061 were
public. A default-deny `/etc/nftables.conf` now allows only: 22/tcp (rate
limited, now **30/minute** new connections, also in
`configs/ansible/roles/archival-node/templates/nftables.conf.j2`; 4/min locked
out operator sessions), 80/443 (Caddy), 11625/11626/11725/11726 (stellar-core
SCP), ICMP 10/sec, loopback unrestricted. `nftables.service` enabled. The
Ansible `nftables.conf.j2` (`internal_cidrs` allow-lists) supersedes the
minimal file on the next full role run.

### F-1209 +16G swap

Memory was ~95% with the 4G swap exhausted (Postgres `shared_buffers=48GB`
+ minio + galexie captive-core). Added `/swap_f1209` (`fallocate -l 16G`,
`mkswap`, `swapon`, `/etc/fstab`: `/swap_f1209 none swap sw,pri=1 0 0`); swap
now 19Gi. A stopgap: the real fix is a capacity decision (more RAM or lower
`shared_buffers`), tracked under F-1209 (see `MemoryHigh` in the
`stellarindex-{indexer,aggregator,api}.service.j2` units).

## Credentials (pointers, not values)

- Vault password: maintainer's password manager
- MinIO root and Postgres stellar-core role password: `inventory/r1.secrets.yml` (vaulted)
- Healthchecks.io ping URL: `/etc/default/node-healthcheck` on the box (mode
  0600); also to be added to `r1.secrets.yml` via the vault helper
- SSH admin key: ed25519 public key in `inventory/r1.yml`
