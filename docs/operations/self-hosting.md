---
title: Self-hosting Stellar Index — end-to-end operator guide
last_verified: 2026-07-10
status: living doc
---

# Self-hosting Stellar Index

Apache-2.0; nothing requires our hosted API. Path from an empty box to a
serving `/v1/price`, adapted from
[`archival-node-bringup.md`](archival-node-bringup.md). With Ansible on a fresh
box use [§ Advanced path](#advanced-path-the-ansible-role). Not covered:
[§ What's not covered](#whats-not-covered).

## 1. What you get

```
Stellar network -> Galexie -> MinIO (galexie-live / galexie-archive)
  -> indexer -> ClickHouse raw lake (ADR-0034) + TimescaleDB served tier
  -> aggregator (VWAP/TWAP -> Redis) -> api (REST + SSE, /v1/*)
```

Six binaries (`cmd/`): `stellarindex-indexer`, `-aggregator`, `-api`
(long-running), `-migrate`, `-ops` (admin CLI), `-sla-probe` (optional). No
Horizon and no `stellar-rpc` in ingest
([ADR-0001](../adr/0001-horizon-deprecated.md), AGENTS.md invariant 6).
ClickHouse ([ADR-0034](../adr/0034-tiered-clickhouse-architecture.md)) is the
full history; TimescaleDB is the **served** tier the API queries
([light mode](#light-mode-recent-window-only) runs without the lake).

## 2. Hardware and disk expectations

### Full-history archival node

Specs: [archival-node-spec.md](../architecture/infrastructure/archival-node-spec.md),
[r1-deployment-state.md](r1-deployment-state.md). Measured: SDF history mirror
**~7.0 TB**; `galexie-archive` **~4.76 TB**; r1's pool is raidz1 (single
parity, 4× 7.68 TB NVMe), ~16.8 TiB usable. Disk: 2 TB minimum (recent
window), 8 TB full history; 8c/32 GB floor, 32c/128 GB colocated. A
from-genesis mirror takes **~10–13 hours**
([time budget](archival-node-bringup.md#time-budget-summary)).

### Light mode (recent-window-only)

For a live feed from today forward, skip the multi-TB mirror and
`galexie-archive-fill` (there is no `--light` flag):

- Galexie with no cursor in MinIO exports from the current tip
  ([`galexie.service.j2`](../../configs/ansible/roles/archival-node/templates/systemd/galexie.service.j2)).
- `ingestion.backfill_from_ledger` (read once at first boot, when no cursor
  row exists) = the current tip instead of `2`. `ingestion.live_seam_ledger = 0`
  (default) = no archive bucket; the indexer reads only `galexie-live`.
- The ADR-0033 verdict (`GET /v1/coverage`, `.complete`) holds only from
  `backfill_from_ledger` forward; long OHLC/VWAP windows stay short.

## 3. Prerequisites

- Ubuntu 22.04+/24.04+ (NVMe), Docker Engine 24+ with Compose v2, Go ≥ 1.25.
- `stellar-core` + Galexie from `apt.stellar.org` / the
  [`stellar-galexie`](https://github.com/stellar/stellar-galexie) release; not
  in this repo ([ADR-0002](../adr/0002-minio-s3-compat-storage.md)).
- An S3-compatible store: MinIO (bundled in `make dev`), or AWS S3, GCS, R2,
  B2, Wasabi via `endpoint_url`. **Never** Galexie's local-filesystem backend
  (ADR-0002).
- ClickHouse server (on by default; §4.5 for opt-out).
- Full mirror only: [`stellar-archivist`](https://github.com/stellar/go-stellar-archivist).

## 4. Step-by-step bring-up


### 4.1 Dependency stack

Compose brings up Postgres+TimescaleDB, Redis and MinIO, not production-shaped
([README](../../deploy/docker-compose/README.md)).

```sh
git clone https://github.com/Stellar-Index/StellarIndex.git
cd StellarIndex
cp deploy/docker-compose/.env.example deploy/docker-compose/.env
make dev              # timescale + redis + minio, docker compose
```

`make dev` starts **only** those three containers; the binaries run on the
host (§4.2, §4.7). In production use native or managed Postgres 15 + TimescaleDB
(`CREATE EXTENSION IF NOT EXISTS timescaledb;`), Redis 7 and MinIO, with
buckets `galexie-live`, `galexie-archive`, `backups` (`mc mb -p local/<bucket>`).

### 4.2 Build the binaries

```sh
make build            # bin/stellarindex-{indexer,aggregator,api,ops,migrate,sla-probe}
```

### 4.3 Config file

```sh
cp configs/example.toml /etc/stellarindex.toml
```

Reference: [`docs/reference/config/README.md`](../reference/config/README.md).
Edit at minimum:

- `[stellar] network` — `pubnet` for mainnet.
- `[storage] postgres_dsn`, `redis_addr`, `s3_endpoint`, `s3_bucket_archive`,
  `s3_bucket_live` — from 4.1.
- `[ingestion] enabled_sources` — on-chain decoders (default
  `["soroswap", "aquarius", "phoenix"]`; all: `KnownSources` in
  `internal/config/validate.go`). Each needs a per-WASM-hash audit before
  historical backfill: [wasm-audits/README.md](wasm-audits/README.md).
- `[storage] clickhouse_addr`, `[storage] clickhouse_live_sink` and
  `[storage] clickhouse_projector_source` — commented out in the example, so
  both switches default `true` (ADR-0041) against
  `clickhouse_addr = "127.0.0.1:9300"`. **Without ClickHouse (§4.5), set both
  switches `false`**: the indexer dials that address at boot and refuses to
  start when nothing answers.
- `[api] external_base_url` — **your own** public `/v1` root, e.g.
  `https://api.example.com/v1`. Emails build their links from it alone; the
  upstream default would email your users' live tokens to our host.

Secrets never go in this file (§5). On upgrade diff new keys in rather than
re-copying; unknown keys hard-fail (`config: unknown keys in ...`).

### 4.4 MinIO + Galexie (captive stellar-core)

Point Galexie at your MinIO, matching
[`galexie.toml.j2`](../../configs/ansible/roles/archival-node/templates/galexie.toml.j2):

```toml
admin_port = 8090

[datastore_config]
type = "S3"

[datastore_config.params]
destination_bucket_path = "galexie-live/"
region                  = "us-east-1"
endpoint_url            = "http://127.0.0.1:9000"

[datastore_config.schema]
ledgers_per_file    = 1
files_per_partition = 64000

[stellar_core_config]
network                  = "pubnet"
captive_core_toml_path   = "/etc/stellar/captive-core-galexie.cfg"
stellar_core_binary_path = "/usr/bin/stellar-core"
```

`captive_core_toml_path` needs a stellar-core config; start from SDF's [`stellar-core_example.cfg`](https://github.com/stellar/stellar-core/blob/master/docs/stellar-core_example.cfg).

```sh
galexie append --config-file /etc/galexie.toml --start <START_LEDGER>
```

`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` carry the MinIO credentials; a
populated bucket resumes at `last_exported + 1`. Full history: start = 2, then
[archival-node-bringup.md §2-4](archival-node-bringup.md#2-mirror-the-sdf-history-archive-34-h-wall-7-tb).
Light mode: start = current tip, from `stellar-core`'s `/info`.

### 4.5 ClickHouse raw lake (opt-out, not opt-in)

Since ADR-0041 skipping the lake means turning two switches off. Install `clickhouse-server` and, before starting it, move its native port
off 9000 (MinIO's) in `/etc/clickhouse-server/config.d/si-override.xml` (the
ansible role writes the same; `clickhouse_tcp_port` defaults to 9300):

```xml
<clickhouse>
    <tcp_port>9300</tcp_port>
    <listen_host>127.0.0.1</listen_host>
</clickhouse>
```

Restart it, then apply the schema (`clickhouse-client` also defaults to 9000,
so pass the port every time):

```sh
clickhouse-client --port 9300 < deploy/clickhouse/tier1_schema.sql   # CREATE ... IF NOT EXISTS — safe to re-run
```

The indexer dials the **native protocol** port (`storage.clickhouse_addr`,
default `127.0.0.1:9300`, not 8123 HTTP); if you pick another `<tcp_port>`,
set `storage.clickhouse_addr` to match
([`storage-considerations.md`](../architecture/storage-considerations.md)).
The API retries ClickHouse for ~seven seconds at start; if it has not
answered, lake-backed endpoints stay 503 until you restart the API.

Without ClickHouse set `storage.clickhouse_live_sink = false` and
`storage.clickhouse_projector_source = false`. Pricing is unaffected; you lose
the completeness verdict and lake-derived supply figures.

**Serving-query isolation (optional, ADR-0048 D4).** Provision a bounded
ClickHouse profile + user so public reads never queue behind a backfill
(`configs/ansible/roles/archival-node/tasks/20-clickhouse-serving-profile.yml`),
then set `storage.clickhouse_serving_user` and
`STELLARINDEX_CLICKHOUSE_SERVING_PASSWORD`.

### 4.6 Migrations

```sh
export STELLARINDEX_POSTGRES_DSN="postgres://stellarindex:<password>@127.0.0.1:5432/stellarindex?sslmode=disable"
./bin/stellarindex-migrate -migrations migrations up
./bin/stellarindex-migrate -migrations migrations status   # expect: migrated to version <N> (dirty=false)
```

`<N>` = highest file under `migrations/`. Run as the `stellarindex` app role,
never a Postgres superuser (its objects become inaccessible to the app).

### 4.7 Indexer, aggregator, API — systemd units

Start in order, each healthy before the next:

```sh
sudo cp bin/stellarindex-indexer bin/stellarindex-aggregator bin/stellarindex-api /usr/local/bin/
sudo cp deploy/systemd/stellarindex-{indexer,aggregator,api}.service /etc/systemd/system/
sudo useradd --system --no-create-home stellarindex   # if not already present
sudo mkdir -p /var/lib/stellarindex && sudo chown stellarindex:stellarindex /var/lib/stellarindex

# env file all three units load:
sudo tee /etc/default/stellarindex-ops >/dev/null <<'EOT'
STELLARINDEX_POSTGRES_DSN=postgres://stellarindex:<password>@127.0.0.1:5432/stellarindex?sslmode=disable
STELLARINDEX_S3_ACCESS_KEY=<minio-access-key>
STELLARINDEX_S3_SECRET_KEY=<minio-secret-key>
AWS_ACCESS_KEY_ID=<minio-access-key>
AWS_SECRET_ACCESS_KEY=<minio-secret-key>
AWS_ENDPOINT_URL=http://127.0.0.1:9000
AWS_REGION=us-east-1
EOT
sudo chmod 640 /etc/default/stellarindex-ops

sudo systemctl daemon-reload
sudo systemctl enable --now stellarindex-indexer.service
# wait for the archive/live handoff:
journalctl -fu stellarindex-indexer

sudo systemctl enable --now stellarindex-aggregator.service
sudo systemctl enable --now stellarindex-api.service
```

`STELLARINDEX_*` match `ApplyEnvOverrides` in `internal/config/load.go`; `AWS_*`
are read by the S3 SDK.

## 5. Configuration reference pointers

- [`docs/reference/config/README.md`](../reference/config/README.md);
  [`configs/example.toml`](../../configs/example.toml).
- **Secrets never go in the TOML.** A `*_env` field names the environment
  variable holding the secret; `redis_password` and
  `clickhouse_serving_password` hold the value itself, so inject
  `STELLARINDEX_REDIS_PASSWORD` / `STELLARINDEX_CLICKHOUSE_SERVING_PASSWORD`
  instead (§4.7).
- New on-chain source: [`add-onchain-source.md`](../contributing/add-onchain-source.md).
  CEX/FX connectors: `[external.<venue>]` blocks, `enabled = false` by default.

## 6. Verification

The smoke battery r1 runs every 5 minutes (`stellarindex-smoke.timer`):

```sh
API_BASE_URL=http://localhost:3000 bash scripts/dev/r1-smoke.sh
```

**Exit code = number of failed checks.** By hand:

```sh
curl -s localhost:3000/v1/healthz | jq     # .data.status == "ok"
curl -s localhost:3000/v1/readyz  | jq     # per-dependency .checks (Postgres/Redis)
curl -s localhost:3000/v1/coverage | jq    # ADR-0033 verdict per source: .complete, .coverage_pct
```

`complete: true` claims everything since genesis was captured; in light mode
(§2) expect `false`.

## 7. Operational notes

- **Backups.** r1 runs `pgbackrest` daily (`pgbackrest-backup.timer`) against
  the `backups` bucket. Back up Postgres (ADR-0034 "Accepted exclusion") and,
  after a full mirror, `galexie-archive`.
- **Monitoring.** [`deploy/monitoring/rules/`](../../deploy/monitoring/rules/), [`runbooks/`](runbooks/).
- **Heavy one-shot jobs** run under a memory/IO cap: r1's role installs
  `/usr/local/sbin/run-heavy-job.sh` (`systemd-run --scope`, `MemoryMax=20G
  MemorySwapMax=0`; `configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml`).
- **Catching up:** [`backfill-procedure.md`](backfill-procedure.md). Projected
  Soroban source: `stellarindex-ops projector-replay -config PATH -source <name> -from <ledger> -write`
  (no `-write`, no writes). Non-projected (`sdex`, contract
  calls): `ch-rebuild`. Never a bespoke `<source>-backfill` (AGENTS.md
  invariant 7). `ch-backfill` reads the trimmed live bucket by default;
  historical ranges need `-bucket galexie-archive`.

## Advanced path: the Ansible role

[`configs/ansible/roles/archival-node/`](../../configs/ansible/roles/archival-node/)
does all of §4: follow [`archival-node-bringup.md`](archival-node-bringup.md).
It does **not** install `stellar-archivist` or ClickHouse (§4.5).

## What's not covered

Your own validator set ([ADR-0004](../adr/0004-tier1-validator-aspiration.md));
multi-region ([ADR-0016](../adr/0016-per-region-storage-strategy.md));
[disaster recovery](archival-node-bringup.md#disaster-recovery);
[HA / failover](../architecture/ha-plan.md); the customer platform.
