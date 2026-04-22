# Self-hosted Stellar nodes (core, rpc, archives)

**Status:** 🧪 Our primary hot-path and long-term north-star decision
surface.

**Verified against:**
- `stellar-docs/docs/validators/README.mdx`
- `stellar-docs/docs/validators/admin-guide/prerequisites.mdx`
- `stellar-docs/docs/validators/admin-guide/installation.mdx`
- `stellar-docs/docs/validators/admin-guide/running-node.mdx`
- `stellar-docs/docs/validators/tier-1-orgs.mdx`
- `stellar-rpc/cmd/stellar-rpc/internal/config/options.go`
- `stellar-rpc/cmd/stellar-rpc/internal/db/db.go`

## Taxonomy — what the "self-hosted Stellar node" is, exactly

Stellar's docs distinguish three distinct node roles:

| Role                   | Purpose                                                                        | Runs stellar-core? | Votes in SCP? | Publishes history? |
| ---------------------- | ------------------------------------------------------------------------------ | ------------------ | ------------- | ------------------ |
| **Basic Validator**    | Signs ledgers in consensus; asset issuer endorsement                           | Yes (native)       | Yes           | No                 |
| **Full Validator**     | Basic validator + publishes public history archive                             | Yes (native)       | Yes           | Yes                |
| **Stellar RPC**        | Client-facing tx submission + state queries                                    | Yes (captive-core) | No            | No                 |
| **Galexie**            | Bulk ledger-meta export to data lake (no tx submission)                        | Yes (captive-core) | No            | No                 |

A single CTX Rates deployment will need **at minimum**:
- 1× Galexie (for historical / Silver table materialisation).
- 1× Stellar RPC (for live events + tx submission if we ever offer it).

Validators are **not** required to ingest data — a watcher (non-validating
core) or captive-core inside RPC/Galexie is enough. Validator status is
about *contributing to the network*, not about reading from it.

## Hardware & disk — the verified baseline

From `prerequisites.mdx` (SDF's own recommendation, April 2024 revision):

| Role                 | CPU                       | RAM    | Disk                       | AWS SKU     | GCP SKU         |
| -------------------- | ------------------------- | ------ | -------------------------- | ----------- | --------------- |
| Core Validator Node  | 8× Intel Xeon @ 3.4 GHz   | 16 GB  | 100 GB NVMe SSD (10k iops) | c5d.2xlarge | n4-highcpu-8    |

SDF note: *"Assuming a 30-day retention window for data storage."*

Three things to read out of that number:

1. **100 GB is for a 30-day retention, not full history.** Full-history
   is multi-TB (see below).
2. **Current bucket size ≈ 10 GB** (as of April 2024). Expect steady
   growth as the network produces more state.
3. **BucketListDB is the default database since August 2024**, backed by
   SQLite or Postgres. Postgres is recommended for validators. It still
   also uses a `buckets/` directory for hashing/history.

For reference, our existing Dell R640 co-lo machine (well-resourced, per
the ctx-proposal interlude) exceeds the validator baseline comfortably.
Full-history multi-TB is feasible with additional NVMe.

## Ports

From `prerequisites.mdx`:

| Port  | Direction | Purpose                                                             |
| ----- | --------- | ------------------------------------------------------------------- |
| 11625 | in + out  | `PEER_PORT` — P2P overlay to the rest of the network (TCP)          |
| 11626 | in (LAN)  | `HTTP_PORT` — admin / metrics / tx-submit from RPC. **Never expose publicly.** Use reverse proxy with auth if LAN exposure is needed. |

## Catchup modes (verified, `running-node.mdx:96-100`)

- **`CATCHUP_COMPLETE = true`** — replay the entire network history
  from genesis. Stellar docs say this can take **"weeks"**. Speedup:
  pre-seed the `buckets/` and history archive from another validator
  using `stellar-archivist mirror` (see
  [stellar-archivist.md](stellar-archivist.md)).
- **`CATCHUP_RECENT = N`** — start from N ledgers ago (or "as recent as
  possible" if set to the default). Order-of-magnitude faster.
- Mutually exclusive. Default: `CATCHUP_RECENT` — fastest-to-sync.

**Our Phase-1 approach:** bootstrap with `CATCHUP_RECENT` so we're
live in hours, not weeks. Then, in parallel, mirror the full public
archive via `rs-stellar-archivist` and upgrade a specific instance to
`CATCHUP_COMPLETE` offline. The SDEX backfill for pricing runs from
Galexie against the data lake, not against the live core — so we don't
block on full catchup to start producing historical OHLC.

## Installation — three supported paths

From `installation.mdx`:

1. **Debian packages** (`apt.stellar.org`, signed by SDF key
   `AEAF 01EE A6CA FCEF DDAE 8AA7 0463 8272 A136 B5A6`). Packages:
   `stellar-core`, `stellar-core-utils`, `stellar-core-prometheus-exporter`,
   `stellar-core-postgres`, `stellar-archivist`, `stellar-rpc`. Recommended
   for production. **Our default path.**
2. **From source** (C++20, autotools). Only when the packages don't
   match our OS/arch.
3. **Docker images** via `stellar/quickstart`. Good for dev; the
   quickstart bundles core+rpc+horizon+galexie+friendbot+lab in one
   image + supervisord + Postgres 12. Too monolithic for production.

Note: `stellar-archivist` is distributed as a Debian package even
though the Go source is archived. Worth confirming: package is
currently the **Go** archivist; the Rust `rs-stellar-archivist` is
separate and not yet packaged. We build Rust from source for now.

Kubernetes: **not recommended for validators** by SDF. Reasons:
secrets management, per-pod unique validator keys, public DNS /
inbound ports, bucket storage semantics. We'll run validators on
dedicated baremetal / VM hosts, not k8s.

## stellar-rpc specifics (code-verified)

Verified from `stellar-rpc/cmd/stellar-rpc/internal/config/options.go`
and `internal/db/db.go`:

- **Database: SQLite only.** Driver: `_ "github.com/mattn/go-sqlite3"`
  (db.go line 15). No Postgres option today. Default path:
  `soroban_rpc.sqlite` (TODO comment says "deprecate and rename to
  stellar_rpc.sqlite"). This is a real constraint we need to account
  for — heavy event history on SQLite has known scaling limits.
- **Network:** `network = pubnet | testnet | futurenet`.
  Default captive-core configs are baked in (see options.go:269-280).
  Overrideable via `STELLAR_CORE_BINARY_PATH`, `CAPTIVE_CORE_CONFIG_PATH`,
  `CAPTIVE_CORE_STORAGE_PATH`, `HISTORY_ARCHIVE_URLS`.
- **Captive-core is required.** stellar-rpc runs it in-process.
- **Retention window** — single knob:

  ```
  history-retention-window    default 120960 = 17280 × 7 = ~7 days
  classic-fee-stats-retention-window   default     10 ledgers
  soroban-fee-stats-retention-window   default     50 ledgers
  ```

  `OneDayOfLedgers = 17280` (options.go:26), confirming a ~5 s ledger
  close time (86400/17280 = 5).

  To get full event history: set `HISTORY_RETENTION_WINDOW` to a large
  number. For 5 years of pubnet: ~31.5M ledgers. **Open question:** can
  SQLite realistically serve getEvents over 31M-ledger event history
  with p95 ≤ 200 ms? Almost certainly no. Likely answer: we serve
  live/recent events from stellar-rpc, and historical events from our
  own TimescaleDB built from Galexie.

- **Query limits** (pagination caps):

  ```
  max-events-limit         10000   default-events-limit        100
  max-transactions-limit     200   default-transactions-limit   50
  max-ledgers-limit          200   default-ledgers-limit        50
  ```

- **Ingestion timeout:** 50 min default.

## Stellar-core database: Postgres vs SQLite

BucketListDB (Aug 2024+) is the primary store. SQL is still needed for
metadata / historical queries / operator tooling.

- **SQLite** — works, lower operational overhead. Fine for Basic
  Validator.
- **Postgres** — recommended for Full Validators and anyone running
  history publishing. Recommended PG tuning from the docs:

  ```
  # DB connection should be over a Unix domain socket
  shared_buffers = 25% of system RAM
  effective_cache_size = 50% of system RAM
  max_wal_size = 5GB
  max_connections = 150
  ```

**Our choice:** Postgres for all production cores (validator-track), via
the `stellar-core-postgres` Debian package. Unix-socket connection. We
already run Postgres — no added operational surface.

## Version numbering

Stellar-core uses `protocol_version.release_number.patch_number`
(e.g. v26.0.1 = protocol 26, initial release, first patch). All
releases are 100% backward compatible.

Relevant versions at audit time (2026-04-22):

- stellar-core: v26.0.1 (2026-04-13)
- stellar-rpc:  v26.0.0 (2026-04-03)
- galexie:      v26.0.0 (2026-04-01)

## Tier-1 / three-full-validators aspiration

From `tier-1-orgs.mdx` — our long-term direction (per decision
2026-04-22 in [decisions.md](../decisions.md)):

- **Three Full Validators per org**, geographically dispersed. Rationale:
  quorum-set members can require 2-of-3; one validator down = no
  org-level impact. "If they're in the same data center, they run the
  risk of going down at the same time."
- **Three independent history archives.** One per validator. Each
  archive is a public internet-facing blob store (S3/GCS/MinIO-over-HTTP).
- **Quorum set coordination.** Copy / adopt the current Tier 1 quorum
  set configuration; communicate changes with other Tier 1 orgs.
- **SEP-20 self-verification.** Validator home domain → our website →
  `stellar.toml` with validator pubkeys + contact. We already own
  the domain.
- **Participate in channels.** `#validators` on the Stellar Developers'
  Discord, the `stellar-validators` Google Group email list, and the
  `#validators` channel on Keybase `stellar.public`.
- **Vote in protocol upgrades and Soroban-settings changes** as they
  arise.
- **Tier 1 status is not granted by SDF.** It's emergent — enough
  other orgs must include us in their quorum sets. Path: prove
  reliability + uptime, coordinate, engage publicly.

Hardware budget for the three-validator stance (rough, to be sized):

- 3× machines matching or exceeding `c5d.2xlarge` baseline.
- If Full (history-publishing): each machine needs enough disk for its
  own bucket store (~100 GB + growth) **plus** its own history archive
  (measured in TB for full genesis-forward history). Or we can offload
  the archive to MinIO/S3 and serve it behind nginx per SDF guidance.
- Dedicated network with inbound `PEER_PORT` (11625) publicly reachable
  from all three locations.

## Phase 1 plan (before validator status)

For Phase 1 we're **not** a validator. We run:

1. **One watcher / non-validating stellar-core** on our co-lo R640.
   `CATCHUP_RECENT` default. Postgres backend via the
   `stellar-core-postgres` package.
2. **One stellar-rpc** against that core, `HISTORY_RETENTION_WINDOW`
   set to whatever keeps SQLite happy (probably 30-day initially,
   tunable up).
3. **One Galexie** against the same core, exporting zstd-XDR to our
   MinIO data lake. Config per [galexie.md](galexie.md).
4. **Secondary cloud failover** — either a second colo machine or a
   cloud instance for redundancy, running the same stack.

Validator track (Phase post-launch) builds on this by:

- Adding a seed key, quorum set, and publishing endpoint.
- Acquiring two more geographically-separated hosts.
- Submitting for Tier 1 inclusion after demonstrating uptime.

## Open items

- [ ] Measure real full-genesis bucket + Postgres DB size for pubnet as
      of 2026-04. SDF's "100 GB with 30-day retention" and "10 GB
      bucket" numbers are ~2 years old — likely higher now.
- [ ] Measure `CATCHUP_RECENT` sync time on our hardware from SDF's
      public archive mirror. Rough target: <12 h to live.
- [ ] Confirm whether `stellar-archivist` apt package is the Go or
      Rust implementation. Likely Go until `rs-stellar-archivist`
      cuts a tagged release.
- [ ] Benchmark stellar-rpc SQLite `getEvents` performance at
      `HISTORY_RETENTION_WINDOW = 30 days` / `90 days` / `1 year` on
      the R640. Identify the cliff.
- [ ] Decide which three geographic regions make sense for our
      validator trio (Vancouver colo is one; candidates for the other
      two TBD).
- [ ] Decide whether our history archive goes on MinIO-behind-nginx
      (cheap, matches our storage decision) or AWS S3 (more standard
      but egress cost).
- [ ] Plan for validator seed key management — HSM? Airgapped signing?
      This is a serious security decision before we ever sign a
      validator key in production.

## References

- Validator intro: `docs/validators/README.mdx`
- Prerequisites / hardware: `docs/validators/admin-guide/prerequisites.mdx`
- Installation: `docs/validators/admin-guide/installation.mdx`
- Running / catchup: `docs/validators/admin-guide/running-node.mdx`
- Tier 1: `docs/validators/tier-1-orgs.mdx`
- SEP-20 (self-verification): <https://github.com/stellar/stellar-protocol/blob/master/ecosystem/sep-0020.md>
- Related audits in this repo:
  [galexie.md](galexie.md), [stellar-archivist.md](stellar-archivist.md),
  [stellar-ledger-data-indexer.md](stellar-ledger-data-indexer.md).
