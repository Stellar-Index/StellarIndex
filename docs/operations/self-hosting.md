---
title: Self-hosting Stellar Index — end-to-end operator guide
last_verified: 2026-07-10
status: living doc
---

# Self-hosting Stellar Index

Stellar Index is Apache-2.0. Nothing in the stack requires our
hosted API or our infrastructure — this doc is the path for an
outside operator standing up their own instance, from an empty box
to a serving `/v1/price` endpoint.

It adapts the internal bring-up recipe
([`archival-node-bringup.md`](archival-node-bringup.md)) for someone
who has never seen this repo. That doc (and the Ansible role behind
it) is the fastest path if you're comfortable running Ansible against
a fresh Hetzner-class box — read [§ Advanced path](#advanced-path-the-ansible-role)
for how to use it directly. This guide is the manual, one-command-at-
a-time walkthrough for everyone else, and it's also the one to read
first even if you'll eventually run the Ansible role, because it
explains what each piece is for.

**What this doc deliberately does not cover:** running your own
tier-1 validator set, multi-region deployment, and disaster recovery
for an existing node. See [§ What's not covered](#whats-not-covered).

---

## 1. What you get

One deployment gives you the full pricing pipeline described in
[docs/architecture/overview.md](../architecture/overview.md):

```
                    ┌──────────────┐
  Stellar network → │ Galexie      │  (captive stellar-core; exports
                    │ (your host)  │   LedgerCloseMeta to object storage)
                    └──────┬───────┘
                           ▼
                    ┌──────────────┐
                    │ MinIO (S3)   │  galexie-live / galexie-archive buckets
                    └──────┬───────┘
                           ▼
                    ┌──────────────┐
                    │  indexer     │  ledgerstream → dispatcher → decoders
                    └──┬────────┬──┘
                       ▼        ▼
              ┌────────────┐ ┌─────────────┐
              │ ClickHouse │ │ TimescaleDB │  raw lake (ADR-0034) /
              │ raw lake   │ │ served tier │  served tier (recent
              └────────────┘ └──────┬──────┘  working set + CAGGs)
                                     ▼
                              ┌─────────────┐
                              │ aggregator  │  VWAP/TWAP/confidence → Redis
                              └──────┬──────┘
                                     ▼
                              ┌─────────────┐
                              │  api        │  REST + SSE, /v1/*
                              └─────────────┘
```

Six binaries make up the stack (`cmd/`); the ones you run
continuously are `stellarindex-indexer`, `stellarindex-aggregator`,
and `stellarindex-api`. `stellarindex-migrate` applies schema once;
`stellarindex-ops` is the admin CLI for backfills, verification, and
one-shot jobs; `stellarindex-sla-probe` is an optional latency/
freshness proof harness. There is no Horizon and no `stellar-rpc` in
the production ingest path — see
[ADR-0001](../adr/0001-horizon-deprecated.md) and CLAUDE.md invariant
6; ingest reads Galexie's MinIO output directly.

ClickHouse (the Tier-1 raw lake,
[ADR-0034](../adr/0034-tiered-clickhouse-architecture.md)) is the
certified full history of every ledger; TimescaleDB is the **served**
tier — the recent working set the API actually queries. You can run
the indexer with the ClickHouse sink disabled
(`clickhouse_live_sink = false`) if you only want the pricing API and
don't need the raw-lake/completeness-verdict story — see
[§ 2 light mode](#light-mode-recent-window-only) for what you give
up.

---

## 2. Hardware and disk expectations

Be honest with yourself about which of these two shapes you're
building before you provision anything.

### Full-history archival node

This is what [archival-node-bringup.md](archival-node-bringup.md)
builds and what r1 runs today. Real, measured numbers from that
node ([r1-deployment-state.md](r1-deployment-state.md),
[archival-node-spec.md](../architecture/infrastructure/archival-node-spec.md)):

| Component | Measured size |
|---|---|
| SDF history archive mirror (`stellar-archivist mirror`, genesis→tip) | **~7.0 TB** |
| Galexie historical ledger-meta mirror (`galexie-archive`, 974 partitions as of 2026-04-26) | **~4.76 TB** |
| r1's total ZFS pool (raidz2, 4× 7.68 TB NVMe) | ~13.3 TB usable |
| r1's RAM | 192 GB DDR5 ECC |

The [hardware spec doc](../architecture/infrastructure/archival-node-spec.md)
tiers this explicitly rather than quoting one number — pick the tier
that matches your ambition, not a five-year ceiling:

| Tier | Disk | Covers |
|---|---|---|
| Minimum viable | 2 TB | `CATCHUP_RECENT` + 30-day Galexie + Postgres only |
| Comfortable Phase A/B | 4 TB | + 90-day Galexie retention |
| Full `CATCHUP_COMPLETE` | 8 TB | full history archive + Galexie meta + Postgres, ~2-year runway |
| Long-runway (r1's actual shape) | 16 TB | 3+ years before re-provisioning |

CPU/RAM tiers from the same doc: 8c/32 GB is the `stellar-core`-alone
floor; 32c/128 GB ("all-in-one") is what's needed to colocate core +
Galexie + the indexer + Postgres + ClickHouse on one box the way r1
does. ClickHouse itself is capped to ~32–48 GB resident on r1
(a "resource-limited good neighbour" to Postgres per ADR-0034) — plan
for that headroom on top of the core/Galexie/Postgres numbers above
if you're running the full lake.

**Bring-up wall-clock**, per the internal recipe: **~10–13 hours**
end-to-end for a from-genesis mirror (dominated by network transfer,
not compute) — see the
[time budget table](archival-node-bringup.md#time-budget-summary)
for the step-by-step breakdown.

### Light mode (recent-window-only)

If you don't need certified full history — you just want a live,
correct pricing feed from today forward — the code supports starting
the indexer at (near) the current network tip instead of genesis,
skipping the multi-TB archive mirror and the `galexie-archive-fill`
step entirely:

- **Galexie itself defaults to this.** A fresh `galexie-append.sh`
  invocation with no prior cursor in MinIO starts exporting from the
  current network tip (queried from `stellar-core` on service start),
  not from genesis — see
  [`galexie.service.j2`](../../configs/ansible/roles/archival-node/templates/systemd/galexie.service.j2).
  You only get history older than "when you first started this node"
  by deliberately mirroring it (steps 2–4 of the bring-up recipe).
- **The indexer has an explicit "no persisted cursor" config knob for
  this.** `ingestion.backfill_from_ledger` (`internal/config/config.go`
  `IngestionConfig.BackfillFromLedger`) is read once, at first boot,
  when there's no cursor row yet; set it to the current network tip
  (or any recent ledger) instead of `2` and the indexer never touches
  history before that point. `ingestion.live_seam_ledger = 0` (the
  default) means "no archive bucket" — the indexer reads only
  `galexie-live`, so there's no `galexie-archive` mirror to keep
  around at all.
- **What you honestly lose:** the ADR-0033 completeness verdict
  (`GET /v1/coverage`, `.complete`) for any source can only be true
  from your `backfill_from_ledger` forward — a source's `genesis_ledger`
  in that verdict is the protocol's real on-chain genesis, so a
  light-mode node's coverage percentage will legitimately never reach
  100% against pre-existing history. `/v1/history/since-inception`
  and long OHLC/VWAP windows (30d/1y) will be empty or short until
  enough live time has accrued. This is an honest trade, not a bug —
  document it to your own API consumers if you run this mode
  publicly.

There's no first-class "`--light`" flag; the above is exactly how the
existing knobs behave, described accurately rather than promised as a
named feature.

---

## 3. Prerequisites

| Need | Notes |
|---|---|
| A host | Ubuntu 22.04+/24.04+ per the [hardware spec](../architecture/infrastructure/archival-node-spec.md); NVMe strongly recommended (SATA pushes catchup from hours to days) |
| Docker Engine 24+ / Docker Desktop + Compose v2 | for the local dependency stack (`make dev`) |
| Go ≥ 1.25 | to build the six binaries (`go version`) |
| `stellar-core` + Galexie | installed from `apt.stellar.org` / the [`stellar-galexie`](https://github.com/stellar/stellar-galexie) release — **not part of this repo**; see [ADR-0002](../adr/0002-minio-s3-compat-storage.md) for why we don't run Horizon or ship our own core build |
| An S3-compatible object store | MinIO by default (bundled in `make dev`); AWS S3, GCS, Cloudflare R2, Backblaze B2, Wasabi all work via `endpoint_url` — **never** Galexie's local-filesystem backend in production (silently drops 9 metadata keys + is multi-writer-unsafe; see [ADR-0002](../adr/0002-minio-s3-compat-storage.md)) |
| ClickHouse server (optional, for the full raw lake) | `clickhouse-server` from the [official ClickHouse install](https://clickhouse.com/docs/getting-started/quick-start) or the `clickhouse/clickhouse-server` Docker image — **this repo does not package or install ClickHouse itself**; it ships the schema (`deploy/clickhouse/tier1_schema.sql`) and the indexer-side dual-sink code only |
| `stellar-archivist` (optional, only for a full history mirror) | from [`stellar/go-stellar-archivist`](https://github.com/stellar/go-stellar-archivist) — not installed by anything in this repo either |

---

## 4. Step-by-step bring-up

This is the manual path: one host, systemd units from `deploy/systemd/`,
no Ansible. Every command below is real and lifted from the files
cited — nothing here is invented shorthand.

### 4.1 Dependency stack

For local development / evaluation, the bundled Compose file brings
up Postgres+TimescaleDB, Redis, and MinIO (see
[`deploy/docker-compose/README.md`](../../deploy/docker-compose/README.md)
for the full walkthrough — it is **not** production-shaped: no HA, no
TLS, no backups):

```sh
git clone https://github.com/Stellar-Index/StellarIndex.git
cd stellar-index
cp deploy/docker-compose/.env.example deploy/docker-compose/.env
make dev              # timescale + redis + minio, docker compose
```

For a production host, install Postgres 15 + the TimescaleDB
extension, Redis 7, and MinIO natively (or point at your own S3-
compatible service + managed Postgres/Redis) — the Compose file's
`init/00-timescale-extension.sql` shows the one extension statement
you need (`CREATE EXTENSION IF NOT EXISTS timescaledb;`). Create the
three buckets `galexie-live`, `galexie-archive`, `backups` the same
way `minio-init` does (`mc mb -p local/<bucket>`).

### 4.2 Build the binaries

```sh
make build            # bin/stellarindex-{indexer,aggregator,api,ops,migrate,sla-probe}
```

### 4.3 Config file

```sh
cp configs/example.toml /etc/stellarindex.toml
```

Every field is annotated in place; the generated field-by-field
reference is [`docs/reference/config/README.md`](../reference/config/README.md)
(regenerate with `make docs-config` after any `internal/config/config.go`
change). At minimum, edit:

- `[stellar] network` — `pubnet` for mainnet.
- `[storage] postgres_dsn`, `redis_addr`, `s3_endpoint` / `s3_bucket_archive`
  / `s3_bucket_live` — point at what you brought up in 4.1.
- `[ingestion] enabled_sources` — which on-chain decoders to run
  (defaults to `["soroswap", "aquarius", "phoenix"]`; see
  `internal/config/validate.go`'s `KnownSources` for the full list).
  Each requires a per-WASM-hash decoder audit before it's safe for
  historical backfill — see
  [docs/operations/wasm-audits/README.md](wasm-audits/README.md).
- `[storage] clickhouse_addr` / `clickhouse_live_sink` — leave
  `clickhouse_live_sink = true` (the default) only if you're also
  standing up ClickHouse (§4.5); otherwise set it to `false`.

Secrets never belong in this file — see §5.

### 4.4 MinIO + Galexie (captive stellar-core)

Galexie is a separate SDF project; install it per its own docs, then
point it at your MinIO with a config matching
[`galexie.toml.j2`](../../configs/ansible/roles/archival-node/templates/galexie.toml.j2)'s
shape:

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

`captive_core_toml_path` needs a standard stellar-core config —
`[[HOME_DOMAINS]]` / `[[VALIDATORS]]` / `[HISTORY.*]` /
`NETWORK_PASSPHRASE` — SDF's own
[`stellar-core_example.cfg`](https://github.com/stellar/stellar-core/blob/master/docs/stellar-core_example.cfg)
is the reference; this repo doesn't ship a quorum set of its own for
external operators to copy (the templated one at
`configs/ansible/roles/archival-node/templates/stellar-core.cfg.j2`
is r1-specific and vault-gated).

```sh
galexie append --config-file /etc/galexie.toml --start <START_LEDGER>
```

`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` env vars carry your
MinIO credentials for Galexie's S3 client. A fresh MinIO bucket with
no prior export makes Galexie start at `<START_LEDGER>` = whatever
you pass; a restart against an already-populated bucket resumes from
`last_exported + 1` automatically. Decide here whether you're doing
full history (start = 2, then also run the archive-mirror steps in
[archival-node-bringup.md §2-4](archival-node-bringup.md#2-mirror-the-sdf-history-archive-34-h-wall-7-tb))
or light mode (start = current network tip — query it from any public
Horizon-free source, e.g. `stellar-core`'s own `/info` endpoint, or a
public `stellar-rpc` if you have one handy for this one read).

### 4.5 ClickHouse raw lake (optional)

Install `clickhouse-server` (native package or Docker), then apply
the schema:

```sh
clickhouse-client < deploy/clickhouse/tier1_schema.sql   # CREATE ... IF NOT EXISTS — safe to re-run
```

The indexer's dual-sink dials ClickHouse's **native protocol** port
(`storage.clickhouse_addr`, default `127.0.0.1:9300` — not the 8123
HTTP port). See [ADR-0034](../adr/0034-tiered-clickhouse-architecture.md)
and [`docs/architecture/clickhouse-migration-plan.md`](../architecture/clickhouse-migration-plan.md)
for the full tiering rationale and what's populated vs. not yet
(`ledger_entry_changes` is schema'd but not yet written — see that
plan's §"Accepted exclusion").

If you skip ClickHouse, set `storage.clickhouse_live_sink = false`
and `ingestion.clickhouse_projector_source = false`; the pricing path
(trades → VWAP → API) is unaffected, you just don't get the raw-lake
completeness verdict or lake-derived supply figures.

**Serving-query isolation (ADR-0048 D4, optional).** By default the
API authenticates to ClickHouse as the unauthenticated `default` user
— the same connection every other CH client in this repo uses, fine
for a single-operator or low-traffic deployment. Once explorer traffic
matters (public GET /v1/accounts/{g}/movements and friends), provision
a dedicated bounded settings profile + user so a burst of public reads
can never queue behind a heavy backfill or a background merge on the
same box: `configs/ansible/roles/archival-node/tasks/20-clickhouse-serving-profile.yml`
is the reference (ansible-managed on r1; hand-apply the equivalent
`users.d` XML drop-in — see that file's comments for the exact
settings + rationale — on a non-ansible deployment). Then set
`storage.clickhouse_serving_user` / the
`STELLARINDEX_CLICKHOUSE_SERVING_PASSWORD` env var to point the API at
it; both empty (the default) is the unauthenticated pre-D4 behavior.

### 4.6 Migrations

```sh
export STELLARINDEX_POSTGRES_DSN="postgres://stellarindex:<password>@127.0.0.1:5432/stellarindex?sslmode=disable"
./bin/stellarindex-migrate -migrations migrations up
./bin/stellarindex-migrate -migrations migrations status   # confirm: migrated to version <N> (dirty=false)
```

`<N>` tracks the highest-numbered file under `migrations/` — check
`ls migrations/*.up.sql | sort | tail -1` rather than hard-coding a
number, since this repo adds migrations over time (74 numbered
migrations as of this writing; see
[`migrations/README.md`](../../migrations/README.md) for what each one
adds). Migrations run as the `stellarindex` app role — never as a
Postgres superuser (see rule 7 in that README); running as superuser
leaves objects superuser-owned and the app loses access to them at
runtime.

### 4.7 Indexer, aggregator, API — systemd units

The unit files in `deploy/systemd/` are the canonical wiring; each
has an in-file comment block describing its dependencies and
config. Copy, enable, and start them in this order (each depends on
the previous one being healthy):

```sh
sudo cp bin/stellarindex-indexer bin/stellarindex-aggregator bin/stellarindex-api /usr/local/bin/
sudo cp deploy/systemd/stellarindex-{indexer,aggregator,api}.service /etc/systemd/system/
sudo useradd --system --no-create-home stellarindex   # if not already present
sudo mkdir -p /var/lib/stellarindex && sudo chown stellarindex:stellarindex /var/lib/stellarindex

# Secrets env file the units load (EnvironmentFile=-/etc/default/stellarindex-ops
# in every one of the three unit files):
sudo tee /etc/default/stellarindex-ops >/dev/null <<'EOF'
STELLARINDEX_POSTGRES_DSN=postgres://stellarindex:<password>@127.0.0.1:5432/stellarindex?sslmode=disable
STELLARINDEX_S3_ACCESS_KEY=<minio-access-key>
STELLARINDEX_S3_SECRET_KEY=<minio-secret-key>
AWS_ACCESS_KEY_ID=<minio-access-key>
AWS_SECRET_ACCESS_KEY=<minio-secret-key>
AWS_ENDPOINT_URL=http://127.0.0.1:9000
AWS_REGION=us-east-1
EOF
sudo chmod 640 /etc/default/stellarindex-ops

sudo systemctl daemon-reload
sudo systemctl enable --now stellarindex-indexer.service
# wait for the archive/live handoff (or, in light mode, for the
# first live trades) — watch:
journalctl -fu stellarindex-indexer

sudo systemctl enable --now stellarindex-aggregator.service
sudo systemctl enable --now stellarindex-api.service
```

The env-var names above match `internal/config/load.go`'s
`ApplyEnvOverrides` (`STELLARINDEX_POSTGRES_DSN`,
`STELLARINDEX_S3_ACCESS_KEY` / `_SECRET_KEY` — the *names* configured
under `[storage] s3_access_key_env` / `s3_secret_key_env` in your TOML)
plus the plain `AWS_*` vars the S3 SDK client reads directly for the
Galexie-bucket read path.

---

## 5. Configuration reference pointers

- **Full generated reference:** [`docs/reference/config/README.md`](../reference/config/README.md)
  (`make docs-config`, sourced from `internal/config/config.go` struct
  tags — every field, its TOML key, its env-var override if any, and
  its default).
- **Annotated example:** [`configs/example.toml`](../../configs/example.toml)
  — copy this, not the generated reference, as your starting file;
  every block has prose explaining the trade-offs (CORS, trusted
  proxies, stablecoin fiat-proxy expansion, supply observers, etc).
- **Secrets never go in the TOML.** Every secret-shaped field is a
  `*_env` field naming an environment variable — `s3_access_key_env`,
  `redis_password_env`, and so on — set the actual value via your
  own secret manager (Vault, AWS Secrets Manager, or a root-owned
  `/etc/default/stellarindex-ops` per §4.7) before starting the binary.
- **Known on-chain sources** (`[ingestion] enabled_sources`):
  `internal/config/validate.go`'s `KnownSources` list is the
  authoritative whitelist; adding a new one is documented in
  [`docs/contributing/add-onchain-source.md`](../contributing/add-onchain-source.md).
- **Off-chain (CEX/FX) connectors:** each lives under its own
  `[external.<venue>]` block, `enabled = false` by default — flip on
  what you want per venue; see the prose in `configs/example.toml`'s
  `[external]` section for which need paid API keys.

---

## 6. Verification

Once the API is up (default `0.0.0.0:3000`), run the same smoke
battery r1 runs every 5 minutes via `stellarindex-smoke.timer`:

```sh
API_BASE_URL=http://localhost:3000 bash scripts/dev/r1-smoke.sh
```

It's 13+ independent `GET`s across health, catalogue, pricing,
VWAP/TWAP, oracle passthrough, and diagnostics, each with a `jq`
shape assertion; **exit code is the number of failed checks**, so it
composes directly with cron / Healthchecks.io. A clean run prints
`All checks passed.`

Two endpoints worth checking by hand as you bring the node up:

```sh
curl -s localhost:3000/v1/healthz | jq            # liveness — .data.status == "ok"
curl -s localhost:3000/v1/readyz  | jq            # deeper — per-dependency .checks (Postgres/Redis)
curl -s localhost:3000/v1/coverage | jq            # ADR-0033 completeness verdict per source —
                                                    # .complete, .substrate_ok/.recognition_ok/.projection_ok,
                                                    # .coverage_pct (watermark vs. tip)
```

`/v1/coverage`'s `complete: true` is the honest "did I actually
capture everything since genesis for this source" claim — in light
mode (§2) expect `complete: false` / a `coverage_pct` under 100 for
every source until you've decided to backfill, or forever if you
never do; that's expected, not broken.

---

## 7. Operational notes

- **Backups.** r1 runs `pgbackrest` daily via a systemd timer
  (`pgbackrest-backup.timer` → `/usr/local/bin/pgbackrest-backup.sh`)
  against the `backups` MinIO bucket — this repo doesn't ship that
  script for external use today; `pgBackRest`'s own docs cover the
  setup against any S3-compatible target. At minimum, back up
  Postgres (the served tier is not re-derivable from ClickHouse for
  every table — see the "Accepted exclusion" note in
  [ADR-0034](../adr/0034-tiered-clickhouse-architecture.md)) and your
  MinIO `galexie-archive` bucket (if you did a full mirror, that's
  your only local copy short of re-pulling from AWS's public
  blockchain bucket or SDF's history archive).
- **Monitoring.** Prometheus alert rules ship in
  [`deploy/monitoring/rules/`](../../deploy/monitoring/rules/) (the
  multi-host set — use this one, not `configs/prometheus/rules.r1/`,
  which is r1's single-host overlay with r1-specific job-name
  rewrites). Every alert cites a runbook under
  [`docs/operations/runbooks/`](runbooks/); the alert catalogue is
  [`docs/operations/alerts-catalog.md`](alerts-catalog.md). Metrics
  reference: [`docs/reference/metrics/README.md`](../reference/metrics/README.md).
- **Heavy one-shot jobs.** Any bulk operation — a re-derive, a
  backfill, a big ad-hoc SQL query — should run under a hard
  memory/IO-priority cap so it can't starve the indexer or Postgres.
  r1's Ansible role installs this wrapper at
  `/usr/local/sbin/run-heavy-job.sh` (a `systemd-run --scope` with
  `MemoryMax=20G MemorySwapMax=0` + batch-class CPU/IO weights — see
  `configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml`
  for the exact script). If you're not running the Ansible role,
  reproduce the same shape by hand before running
  `stellarindex-ops` against a large ledger range: an unwindowed
  re-derive on an under-provisioned box can balloon memory and take
  down colocated services (this happened on r1 on 2026-07-05 — see
  CLAUDE.md's "Heavy one-shot jobs on r1" section for the full
  incident).
- **Backfilling / catching up after downtime:** see
  [`docs/operations/backfill-procedure.md`](backfill-procedure.md)
  and, for Soroban-derived sources specifically,
  `stellarindex-ops projector-replay -source <name> -from <ledger>`
  (never a bespoke `<source>-backfill` command — those were removed;
  see CLAUDE.md invariant 7).

---

## Advanced path: the Ansible role

If you're standing up a full archival-shaped node (the "full-history"
tier above) and are comfortable running Ansible against a fresh box,
[`configs/ansible/roles/archival-node/`](../../configs/ansible/roles/archival-node/)
does everything in §4 for you — ZFS pool + datasets, MinIO + IAM,
Postgres + TimescaleDB, Galexie, all six binaries cross-compiled and
copied up, migrations applied, systemd units installed. Follow
[`archival-node-bringup.md`](archival-node-bringup.md) top to bottom;
it's the exact recipe r1 was built from, generalized with
`<host>` / `<SEAM>` placeholders. The role does **not** install
`stellar-archivist` or ClickHouse — those remain manual steps (see
that doc's Prerequisites and §4.5 above respectively).

---

## What's not covered

- **Running your own tier-1 validator set.** This guide builds a
  non-voting archival node. Promoting to a validator (HSM-backed
  signing keys, SCP participation) is a distinct, later step — see
  [ADR-0004](../adr/0004-tier1-validator-aspiration.md).
- **Multi-region deployment / cross-region consistency.** Our own
  three-region topology (R1 Hetzner / R2 AWS / R3 Vultr) and its
  per-region storage-shape trade-offs are documented in
  [ADR-0016](../adr/0016-per-region-storage-strategy.md) and
  [`archival-node-bringup.md`'s per-region section](archival-node-bringup.md#per-region-variations-r2-aws--r3-vultr--per-adr-0016),
  but running more than one node of your own, and keeping them
  consistent, is out of scope here.
- **Disaster recovery for an existing node.** Covered separately in
  [`archival-node-bringup.md`'s Disaster recovery section](archival-node-bringup.md#disaster-recovery)
  — corrupt history archive, wiped Postgres, lost MinIO data dir.
- **HA / failover.** See [`docs/architecture/ha-plan.md`](../architecture/ha-plan.md).
- **The dashboard / customer platform** (`internal/platform`, API-key
  self-service, billing). This guide covers the pricing/explorer data
  plane only.

---

## References

- [`docs/architecture/overview.md`](../architecture/overview.md) — 10-minute architecture orientation.
- [`docs/architecture/ingest-pipeline.md`](../architecture/ingest-pipeline.md) — the binding rules for the ingest path.
- [ADR-0001](../adr/0001-horizon-deprecated.md) — no Horizon.
- [ADR-0002](../adr/0002-minio-s3-compat-storage.md) — S3-compatible storage, not local filesystem.
- [ADR-0034](../adr/0034-tiered-clickhouse-architecture.md) — ClickHouse raw lake / Postgres served tier.
- [`docs/architecture/infrastructure/archival-node-spec.md`](../architecture/infrastructure/archival-node-spec.md) — hardware tiers in full.
- [`archival-node-bringup.md`](archival-node-bringup.md) — the internal recipe this guide adapts.
- [`migrations/README.md`](../../migrations/README.md) — schema migration rules + full index.
- [`deploy/docker-compose/README.md`](../../deploy/docker-compose/README.md) — local dev stack detail.
