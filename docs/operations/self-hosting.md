---
title: Self-hosting Stellar Index — end-to-end operator guide
last_verified: 2026-07-10
status: living doc
---

# Self-hosting Stellar Index

Apache-2.0; nothing requires our hosted API. This is the path from an empty
box to a serving `/v1/price`, as manual one-command-at-a-time steps. It
adapts [`archival-node-bringup.md`](archival-node-bringup.md); if you are
comfortable with Ansible on a fresh Hetzner-class box use
[§ Advanced path](#advanced-path-the-ansible-role). Read this guide first
either way: it explains what each piece is for. Not covered: see
[§ What's not covered](#whats-not-covered).

---

## 1. What you get

The pipeline in [docs/architecture/overview.md](../architecture/overview.md):

```
Stellar network -> Galexie (captive stellar-core) -> MinIO (galexie-live / galexie-archive)
  -> indexer (ledgerstream -> dispatcher -> decoders)
       -> ClickHouse raw lake (ADR-0034) + TimescaleDB served tier (recent working set + CAGGs)
  -> aggregator (VWAP/TWAP/confidence -> Redis) -> api (REST + SSE, /v1/*)
```

Six binaries (`cmd/`). Run continuously: `stellarindex-indexer`,
`stellarindex-aggregator`, `stellarindex-api`. `stellarindex-migrate`
applies schema; `stellarindex-ops` is the admin CLI (backfills,
verification, one-shots); `stellarindex-sla-probe` is an optional
latency/freshness harness. No Horizon and no `stellar-rpc` in the
production ingest path ([ADR-0001](../adr/0001-horizon-deprecated.md),
AGENTS.md invariant 6); ingest reads Galexie's MinIO output directly.

ClickHouse ([ADR-0034](../adr/0034-tiered-clickhouse-architecture.md)) is the
certified full history; TimescaleDB is the **served** tier the API queries.
With `clickhouse_live_sink = false` you get the price endpoints without the
raw-lake / completeness-verdict story (see [§ 2 light mode](#light-mode-recent-window-only)).

---

## 2. Hardware and disk expectations

Decide which shape you are building before provisioning.

### Full-history archival node

What [archival-node-bringup.md](archival-node-bringup.md) builds and r1 runs
([r1-deployment-state.md](r1-deployment-state.md),
[archival-node-spec.md](../architecture/infrastructure/archival-node-spec.md)):

| Component | Measured size |
|---|---|
| SDF history archive mirror (`stellar-archivist mirror`, genesis→tip) | **~7.0 TB** |
| Galexie historical ledger-meta mirror (`galexie-archive`, 974 partitions as of 2026-04-26) | **~4.76 TB** |
| r1's total ZFS pool (raidz1 — single parity, 4× 7.68 TB NVMe) | ~18.3 TB (~16.8 TiB) usable |
| r1's RAM | 192 GB DDR5 ECC |

| Tier | Disk | Covers |
|---|---|---|
| Minimum viable | 2 TB | `CATCHUP_RECENT` + 30-day Galexie + Postgres only |
| Comfortable Phase A/B | 4 TB | + 90-day Galexie retention |
| Full `CATCHUP_COMPLETE` | 8 TB | full history archive + Galexie meta + Postgres, ~2-year runway |
| Long-runway (r1's shape) | 16 TB | 3+ years before re-provisioning |

CPU/RAM: 8c/32 GB is the `stellar-core`-alone floor; 32c/128 GB
("all-in-one") colocates core + Galexie + indexer + Postgres + ClickHouse as
r1 does. ClickHouse is capped to ~32–48 GB resident on r1 (ADR-0034); budget
that on top if you run the full lake.

Bring-up wall-clock for a from-genesis mirror: **~10–13 hours**, dominated by
network transfer ([time budget](archival-node-bringup.md#time-budget-summary)).

### Light mode (recent-window-only)

For a live, correct feed from today forward, start the indexer near the tip
and skip the multi-TB mirror and `galexie-archive-fill`:

- **Galexie defaults to this.** A fresh `galexie-append.sh` with no prior
  cursor in MinIO exports from the current network tip (queried from
  `stellar-core` at service start), per
  [`galexie.service.j2`](../../configs/ansible/roles/archival-node/templates/systemd/galexie.service.j2).
  Older history only comes from deliberately mirroring it (bring-up steps 2–4).
- **Indexer knob.** `ingestion.backfill_from_ledger`
  (`internal/config/config.go` `IngestionConfig.BackfillFromLedger`) is read
  once at first boot, when no cursor row exists; set it to the current tip
  instead of `2`. `ingestion.live_seam_ledger = 0` (default) means no archive
  bucket: the indexer reads only `galexie-live`.
- **What you lose.** The ADR-0033 completeness verdict (`GET /v1/coverage`,
  `.complete`) is only true from `backfill_from_ledger` forward, since a
  source's `genesis_ledger` is the protocol's real on-chain genesis; coverage
  will legitimately never reach 100% against older history.
  `/v1/history/since-inception` and long OHLC/VWAP windows (30d/1y) are empty
  or short until live time accrues. Tell your API consumers if you serve this
  publicly.

There is no `--light` flag; this is how the existing knobs behave.

---

## 3. Prerequisites

| Need | Notes |
|---|---|
| A host | Ubuntu 22.04+/24.04+ per the [hardware spec](../architecture/infrastructure/archival-node-spec.md); NVMe strongly recommended (SATA turns catchup from hours into days) |
| Docker Engine 24+ / Docker Desktop + Compose v2 | for the local dependency stack (`make dev`) |
| Go ≥ 1.25 | to build the six binaries (`go version`) |
| `stellar-core` + Galexie | from `apt.stellar.org` / the [`stellar-galexie`](https://github.com/stellar/stellar-galexie) release; **not part of this repo** ([ADR-0002](../adr/0002-minio-s3-compat-storage.md)) |
| An S3-compatible object store | MinIO by default (bundled in `make dev`); AWS S3, GCS, Cloudflare R2, Backblaze B2, Wasabi work via `endpoint_url`. **Never** Galexie's local-filesystem backend (drops 9 metadata keys, multi-writer-unsafe; ADR-0002) |
| ClickHouse server (raw lake, on by default; §4.5 says how to opt out) | `clickhouse-server` from the [official install](https://clickhouse.com/docs/getting-started/quick-start) or the `clickhouse/clickhouse-server` image; this repo ships only the schema (`deploy/clickhouse/tier1_schema.sql`) and the indexer dual-sink |
| `stellar-archivist` (optional, full history mirror only) | from [`stellar/go-stellar-archivist`](https://github.com/stellar/go-stellar-archivist); not installed by this repo |

---

## 4. Step-by-step bring-up

Manual path: one host, systemd units from `deploy/systemd/`, no Ansible.

### 4.1 Dependency stack

The bundled Compose file brings up Postgres+TimescaleDB, Redis and MinIO
([`deploy/docker-compose/README.md`](../../deploy/docker-compose/README.md)).
It is **not** production-shaped: no HA, TLS or backups.

```sh
git clone https://github.com/Stellar-Index/StellarIndex.git
cd StellarIndex
cp deploy/docker-compose/.env.example deploy/docker-compose/.env
make dev              # timescale + redis + minio, docker compose
```

`make dev` brings up **only** those three containers: no API, no ClickHouse,
no docs site. The app binaries run on the host (§4.2, §4.7); the docs site is
a static build (`make docs-api`).

For production, install Postgres 15 + TimescaleDB, Redis 7 and MinIO natively
(or use your own S3-compatible service and managed Postgres/Redis). The one
extension statement is `CREATE EXTENSION IF NOT EXISTS timescaledb;`
(`init/00-timescale-extension.sql`). Create buckets `galexie-live`,
`galexie-archive`, `backups` as `minio-init` does (`mc mb -p local/<bucket>`).

### 4.2 Build the binaries

```sh
make build            # bin/stellarindex-{indexer,aggregator,api,ops,migrate,sla-probe}
```

### 4.3 Config file

```sh
cp configs/example.toml /etc/stellarindex.toml
```

Every field is annotated in place; the generated reference is
[`docs/reference/config/README.md`](../reference/config/README.md)
(`make docs-config` after any `internal/config/config.go` change). Edit at
minimum:

- `[stellar] network` — `pubnet` for mainnet.
- `[storage] postgres_dsn`, `redis_addr`, `s3_endpoint` / `s3_bucket_archive`
  / `s3_bucket_live` — what you brought up in 4.1.
- `[ingestion] enabled_sources` — which on-chain decoders to run. The example
  ships `["soroswap"]`; the default when absent is
  `["soroswap", "aquarius", "phoenix"]`; the full list is `KnownSources` in
  `internal/config/validate.go`. Each needs a per-WASM-hash decoder audit
  before historical backfill: [wasm-audits/README.md](wasm-audits/README.md).
- `[storage] clickhouse_addr`, `[storage] clickhouse_live_sink` and
  `[storage] clickhouse_projector_source` — all commented out in the example,
  so the defaults apply: both switches `true` (ADR-0041) against
  `clickhouse_addr = "127.0.0.1:9300"`. **Without ClickHouse (§4.5),
  uncomment the two switches and set them `false`**: the indexer dials that
  address at boot and refuses to start when nothing answers.
- `[api] external_base_url` — **your own** public `/v1` root, e.g.
  `https://api.example.com/v1`. Sign-in and verification emails build their
  links from this value alone (the `Host` header is never used). The upstream
  default `https://api.stellarindex.io/v1` emails your users' live tokens to
  our host. Empty or non-`http(s)` suppresses the email
  (`email_verification_sent: false`).

Secrets never belong in this file (§5).

Do not re-copy `example.toml` over an edited config on upgrade; diff new keys
in by hand. A key a later release deleted is logged and ignored at boot
(`config: retired keys present, ignoring`) if registered on
`internal/config.RetiredKeys`; anything else hard-fails with
`config: unknown keys in ...`: remove it.

### 4.4 MinIO + Galexie (captive stellar-core)

Install Galexie per its own docs and point it at your MinIO, matching
[`galexie.toml.j2`](../../configs/ansible/roles/archival-node/templates/galexie.toml.j2):

```toml
admin_port = 8090

[datastore_config]
type = "S3"

[datastore_config.params]
destination_bucket_path = "galexie-live/"
region                  = "us-east-1"          # any string MinIO accepts
endpoint_url            = "http://127.0.0.1:9000"

[datastore_config.schema]
ledgers_per_file    = 1
files_per_partition = 64000

[stellar_core_config]
network                  = "pubnet"
captive_core_toml_path   = "/etc/stellar/captive-core-galexie.cfg"
stellar_core_binary_path = "/usr/bin/stellar-core"
```

`captive_core_toml_path` needs a standard stellar-core config
(`[[HOME_DOMAINS]]` / `[[VALIDATORS]]` / `[HISTORY.*]` / `NETWORK_PASSPHRASE`);
use SDF's
[`stellar-core_example.cfg`](https://github.com/stellar/stellar-core/blob/master/docs/stellar-core_example.cfg).
The repo's `stellar-core.cfg.j2` is r1-specific and vault-gated.

```sh
galexie append --config-file /etc/galexie.toml --start <START_LEDGER>
```

`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` carry the MinIO credentials. A
fresh bucket starts at `<START_LEDGER>`; a restart against a populated bucket
resumes at `last_exported + 1`. Full history: start = 2, then run the
archive-mirror steps in
[archival-node-bringup.md §2-4](archival-node-bringup.md#2-mirror-the-sdf-history-archive-34-h-wall-7-tb).
Light mode: start = current network tip, read from `stellar-core`'s own
`/info` endpoint.

### 4.5 ClickHouse raw lake (opt-out, not opt-in)

Since ADR-0041 the lake is substrate: skipping it means turning two switches
off, not leaving them alone.

Install `clickhouse-server` (native package or Docker). Before starting it,
move its native port off 9000, which MinIO (§4.4) holds. Put this in
`/etc/clickhouse-server/config.d/si-override.xml` (Docker: mount into that
directory); the ansible role writes the same, `clickhouse_tcp_port`
defaulting to 9300:

```xml
<clickhouse>
    <tcp_port>9300</tcp_port>
    <listen_host>127.0.0.1</listen_host>
</clickhouse>
```

Restart `clickhouse-server`, then apply the schema. `clickhouse-client` also
defaults to 9000, so pass the port every time:

```sh
clickhouse-client --port 9300 < deploy/clickhouse/tier1_schema.sql   # CREATE ... IF NOT EXISTS — safe to re-run
```

The indexer dials the **native protocol** port (`storage.clickhouse_addr`,
default `127.0.0.1:9300`, not 8123 HTTP); if you pick another `<tcp_port>`,
set `storage.clickhouse_addr` to match. Tiering and what is populated:
[ADR-0034](../adr/0034-tiered-clickhouse-architecture.md),
[`storage-considerations.md`](../architecture/storage-considerations.md).

The reference `stellarindex-api` unit is `After=clickhouse-server.service`
(ordering only; a no-op for remote/Docker ClickHouse). Before listening, the
API dials both lake readers concurrently and retries for up to seven seconds
(last attempt ~six seconds in). A ClickHouse that has not answered by then
leaves lake-backed endpoints on 503 and
`stellarindex_dependency_up{dependency="clickhouse"}` at 0 until you restart
the API.

Without ClickHouse, set `storage.clickhouse_live_sink = false` and
`storage.clickhouse_projector_source = false`. Both are under `[storage]`;
`internal/config/load.go` rejects an unknown key as a hard error, so a
misfiled one stops every binary. Both default to `true` (ADR-0041), so leaving
them on with no ClickHouse fails the indexer at boot. The pricing path
(trades → VWAP → API) is unaffected; you lose the raw-lake completeness
verdict and lake-derived supply figures.

**Serving-query isolation (ADR-0048 D4, optional).** By default the API
connects to ClickHouse as the unauthenticated `default` user, fine for
low-traffic. Once public reads matter (e.g. `GET /v1/accounts/{g}/movements`),
provision a dedicated bounded settings profile + user so public reads never
queue behind a backfill or merge. Reference:
`configs/ansible/roles/archival-node/tasks/20-clickhouse-serving-profile.yml`
(on a non-ansible host hand-apply the equivalent `users.d` XML drop-in; the
file's comments give the settings). Then set `storage.clickhouse_serving_user`
and `STELLARINDEX_CLICKHOUSE_SERVING_PASSWORD`; both empty (default) is the
unauthenticated behaviour.

### 4.6 Migrations

```sh
export STELLARINDEX_POSTGRES_DSN="postgres://stellarindex:<password>@127.0.0.1:5432/stellarindex?sslmode=disable"
./bin/stellarindex-migrate -migrations migrations up
./bin/stellarindex-migrate -migrations migrations status   # confirm: migrated to version <N> (dirty=false)
```

`<N>` is the highest file under `migrations/`
(`ls migrations/*.up.sql | sort | tail -1`; index in
[`migrations/README.md`](../../migrations/README.md)). Run migrations as the
`stellarindex` app role, never a Postgres superuser (rule 7 of that README):
superuser-owned objects become inaccessible to the app at runtime.

### 4.7 Indexer, aggregator, API — systemd units

`deploy/systemd/` is the canonical wiring (each unit documents its
dependencies). Start in this order, each healthy before the next:

```sh
sudo cp bin/stellarindex-indexer bin/stellarindex-aggregator bin/stellarindex-api /usr/local/bin/
sudo cp deploy/systemd/stellarindex-{indexer,aggregator,api}.service /etc/systemd/system/
sudo useradd --system --no-create-home stellarindex   # if not already present
sudo mkdir -p /var/lib/stellarindex && sudo chown stellarindex:stellarindex /var/lib/stellarindex

# Secrets env file the units load (EnvironmentFile=-/etc/default/stellarindex-ops
# in every one of the three unit files):
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
# wait for the archive/live handoff (light mode: the first live trades):
journalctl -fu stellarindex-indexer

sudo systemctl enable --now stellarindex-aggregator.service
sudo systemctl enable --now stellarindex-api.service
```

The `STELLARINDEX_*` names match `ApplyEnvOverrides` in
`internal/config/load.go` (the names set by `[storage] s3_access_key_env` /
`s3_secret_key_env`); the plain `AWS_*` vars are read directly by the S3 SDK
for the Galexie-bucket path.

---

## 5. Configuration reference pointers

- **Generated reference:** [`docs/reference/config/README.md`](../reference/config/README.md)
  (`make docs-config`; every field, TOML key, env override, default).
- **Annotated example:** [`configs/example.toml`](../../configs/example.toml);
  copy this as your starting file (CORS, trusted proxies, stablecoin
  fiat-proxy expansion, supply observers, and so on).
- **Secrets never go in the TOML.** A `*_env` field names the environment
  variable holding the secret (`s3_access_key_env`, `resend_api_key_env`, …).
  `redis_password` and `clickhouse_serving_password` hold the value itself, so
  leave them unset and inject `STELLARINDEX_REDIS_PASSWORD` /
  `STELLARINDEX_CLICKHOUSE_SERVING_PASSWORD`. Set values via your secret
  manager (Vault, AWS Secrets Manager) or a root-owned
  `/etc/default/stellarindex-ops` (§4.7). The old `redis_password_env` and
  `clickhouse_serving_password_env` still load with a deprecation warning.
- **Known on-chain sources** (`[ingestion] enabled_sources`):
  `KnownSources` in `internal/config/validate.go` is the authoritative
  whitelist; adding one: [`add-onchain-source.md`](../contributing/add-onchain-source.md).
- **Off-chain (CEX/FX) connectors:** one `[external.<venue>]` block each,
  `enabled = false` by default; `configs/example.toml`'s `[external]` section
  says which need paid API keys.

---

## 6. Verification

With the API up (default `0.0.0.0:3000`), run the smoke battery r1 runs every
5 minutes via `stellarindex-smoke.timer`:

```sh
API_BASE_URL=http://localhost:3000 bash scripts/dev/r1-smoke.sh
```

13+ independent `GET`s (health, catalogue, pricing, VWAP/TWAP, oracle
passthrough, diagnostics), each with a `jq` shape assertion; **exit code is
the number of failed checks**, so it composes with cron / Healthchecks.io. A
clean run prints `All checks passed.`

By hand:

```sh
curl -s localhost:3000/v1/healthz | jq            # liveness — .data.status == "ok"
curl -s localhost:3000/v1/readyz  | jq            # deeper — per-dependency .checks (Postgres/Redis)
curl -s localhost:3000/v1/coverage | jq            # ADR-0033 completeness verdict per source —
                                                    # .complete, .substrate_ok/.recognition_ok/.projection_ok,
                                                    # .coverage_pct (watermark vs. tip)
```

`complete: true` claims everything since genesis was captured for that source.
In light mode (§2) expect `complete: false` and `coverage_pct` under 100 for
every source until you backfill, or forever: expected, not broken.

---

## 7. Operational notes

- **Backups.** r1 runs `pgbackrest` daily (`pgbackrest-backup.timer` →
  `/usr/local/bin/pgbackrest-backup.sh`) against the `backups` bucket; the
  script is not shipped for external use, so follow pgBackRest's own docs for
  any S3-compatible target. Back up at minimum Postgres (the served tier is
  not re-derivable from ClickHouse for every table; ADR-0034 "Accepted
  exclusion") and, after a full mirror, `galexie-archive` (your only local copy
  short of re-pulling from AWS's public blockchain bucket or SDF's archive).
- **Monitoring.** Alert rules: [`deploy/monitoring/rules/`](../../deploy/monitoring/rules/)
  (the multi-host set, not r1's single-host overlay `configs/prometheus/rules.r1/`).
  Every alert cites a runbook under [`runbooks/`](runbooks/); catalogue:
  [`alerts-catalog.md`](alerts-catalog.md); metrics:
  [`docs/reference/metrics/README.md`](../reference/metrics/README.md).
- **Heavy one-shot jobs.** Run any re-derive, backfill or big ad-hoc SQL under
  a hard memory/IO cap so it cannot starve the indexer or Postgres. r1's role
  installs `/usr/local/sbin/run-heavy-job.sh` (`systemd-run --scope`,
  `MemoryMax=20G MemorySwapMax=0`, batch-class CPU/IO weights; see
  `configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml`).
  Without the role, reproduce that shape before running `stellarindex-ops`
  over a large ledger range: an unwindowed re-derive on a small box took down
  colocated services on r1 (docs/operations/maintainer-workflow.md, "Heavy
  one-shot jobs").
- **Catching up after downtime:** [`backfill-procedure.md`](backfill-procedure.md).
  A projected Soroban source: `stellarindex-ops projector-replay -config PATH -source <name> -from <ledger> -write`
  (fail-closed: without `-write` it reports and writes nothing). A
  non-projected one (`sdex`, contract calls): `ch-rebuild`. Never a bespoke
  `<source>-backfill` (removed; AGENTS.md invariant 7). `ch-backfill` reads
  the live bucket (trimmed) by default; historical ranges need
  `-bucket galexie-archive`.

---

## Advanced path: the Ansible role

For a full archival-shaped node on a fresh box,
[`configs/ansible/roles/archival-node/`](../../configs/ansible/roles/archival-node/)
does all of §4: ZFS pool + datasets, MinIO + IAM, Postgres + TimescaleDB,
Galexie, all six binaries cross-compiled and copied, migrations, systemd
units. Follow [`archival-node-bringup.md`](archival-node-bringup.md) top to
bottom (r1's recipe with `<host>` / `<SEAM>` placeholders). The role does
**not** install `stellar-archivist` or ClickHouse (manual; see that doc's
Prerequisites and §4.5).

---

## What's not covered

- **Your own tier-1 validator set.** This guide builds a non-voting archival
  node; validators (HSM keys, SCP) are later: [ADR-0004](../adr/0004-tier1-validator-aspiration.md).
- **Multi-region deployment.** Our R1/R2/R3 topology:
  [ADR-0016](../adr/0016-per-region-storage-strategy.md) and
  [`archival-node-bringup.md`'s per-region section](archival-node-bringup.md#per-region-variations-r2--r3--️-historical-adr-0016-is-superseded).
  Running and reconciling several nodes is out of scope.
- **Disaster recovery.** [`archival-node-bringup.md`'s Disaster recovery section](archival-node-bringup.md#disaster-recovery).
- **HA / failover.** [`docs/architecture/ha-plan.md`](../architecture/ha-plan.md).
- **Dashboard / customer platform** (`internal/platform`, API-key self-service,
  billing): this guide covers the data plane only.

---

## References

- [`docs/architecture/overview.md`](../architecture/overview.md) — architecture orientation.
- [`docs/architecture/ingest-pipeline.md`](../architecture/ingest-pipeline.md) — binding ingest rules.
- [ADR-0001](../adr/0001-horizon-deprecated.md), [ADR-0002](../adr/0002-minio-s3-compat-storage.md), [ADR-0034](../adr/0034-tiered-clickhouse-architecture.md).
- [`archival-node-spec.md`](../architecture/infrastructure/archival-node-spec.md) — hardware tiers.
- [`archival-node-bringup.md`](archival-node-bringup.md) — the recipe this adapts.
- [`migrations/README.md`](../../migrations/README.md); [`deploy/docker-compose/README.md`](../../deploy/docker-compose/README.md).
