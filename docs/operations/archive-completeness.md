---
title: Archive completeness — invariants, bootstrap, daily cron
last_verified: 2026-10-05
status: living procedure
---

# Archive completeness — invariants, bootstrap, daily cron

Operational companion to [ADR-0017](../adr/0017-archive-completeness-invariants.md).
Implementation: `internal/ops/archive/` + `internal/archivecompleteness/` (the
`stellarindex-ops archive-completeness` subcommand) and the ansible-shipped
`galexie-archive-fill.sh`.

**Scope:** the shipped command enforces and repairs the **cross-anchor** archive only.
The ADR-0017 four-contract model is the target; a green run is not proof that the
primary `galexie-archive` checks ran.

## The two archives

### Primary — `galexie-archive/` MinIO bucket (R1)

One XDR meta file per ledger (~62 M objects, pubnet history):

```
galexie-archive/
└── <HASH>--<N>-<N+63999>/         (partition, 64,000 ledgers)
    ├── <HEX>--<N+63999>.xdr.zst   (latest ledger in partition)
    ├── ...
    └── <HEX>--<N>.xdr.zst         (earliest ledger in partition)
```

The indexer's source of rate data. Filled from
`s3://aws-public-blockchain/v1.1/stellar/ledgers/pubnet/` via `mc mirror` and the
`galexie-archive-fill` script.

### Cross-anchor — `/srv/history-archive/` (R1)

Canonical Stellar history archive:

```
/srv/history-archive/
├── bucket/        (state snapshots, ~256 hex-prefix subdirs)
├── history/       (checkpoint summary JSONs)
├── ledger/XX/YY/ZZ/ledger-XXYYZZWW.xdr.gz   ← what verify-archive reads
├── results/
├── scp/
├── transactions/
└── .well-known/
```

Each `ledger-<hex>.xdr.gz` holds 64 `LedgerHeaderHistoryEntry` records. This is the
**verification anchor**: `stellarindex-ops verify-archive -tier checkpoint` compares each
checkpoint's signed hash with our LCM-derived hash. Source:
`https://history.stellar.org/prd/core-live/core_live_001`, mirrored at bootstrap
(`stellar-archivist` or `mc mirror`) and refreshed by the daily cron.

## Per-region behaviour

Storage shapes per [ADR-0016](../adr/0016-per-region-storage-strategy.md). R2/R3 are
deferred; today R1 is the only host.

### R1 Frankfurt — integrity leader

Holds both archives and runs the cross-anchor control:

```
stellarindex-ops archive-completeness verify -from <floor> -to <tip> -workers 8
  ├─ stat each expected /srv/history-archive/ledger-*.xdr.gz
  └─ fetch/repair missing cross-anchor checkpoint files
```

> ⚠️ **`-to 0` does NOT resolve the tip** — the binary refuses it (`-to is required (pass
> the network head ledger sequence)`). The unit's
> `ExecStartPre=/usr/local/sbin/compute-archive-to.sh` computes the tip from
> `ingestion_cursors` and writes `ARCHIVE_TO=<n>` to `/run/archive-completeness.env`. By
> hand, supply `-to` yourself.

`-write` is required to fetch/repair; without it `fix`/`verify` are a fail-closed dry run
(#1191). The shipped units pass it. On failure the run tries the nine cross-anchor sources
(below); if repair fails it exits non-zero and the alerts fire.

### R2 US-East — AWS hybrid, trusts R1 (target design, not shipped)

No local archive: the indexer streams from `aws-public-blockchain` S3.

```
# TARGET — -checks/-trust-leader do not exist on the current binary.
stellarindex-ops archive-completeness verify -checks chain-link,multi-peer \
  -trust-leader r1
```

- **Chain-link (contract 2):** walk yesterday's ledgers through galexie's S3 reader;
  confirm `prev.LedgerHash == curr.PreviousLedgerHash`.
- **Multi-peer (Tier D):** sample 20 checkpoints from yesterday, fetch each from 6 tier-1
  validator archives, confirm the hashes agree (catches forks).
- **Delegation:** read R1's `archive_completeness_last_success_timestamp`; if older than
  **26 h**, R2 marks itself reduced-redundancy and sets the API's `ReducedRedundancy`
  envelope flag on every response.

### R3 Singapore — Vultr hybrid, trusts R1 (target design, not shipped)

Primary archive on Vultr Object Storage, no cross-anchor. As R2 plus contract 1
(structural) on its local copy:

```
# TARGET — same caveat as R2.
stellarindex-ops archive-completeness verify \
  -checks structural,chain-link,multi-peer \
  -trust-leader r1
```

Repair is `mc mirror` from R1's MinIO over the WAN (165 ms RTT to Frankfurt).

## Bootstrap procedure (one-shot)

Required before the daily timer enforces anything on a node.

1. **Diagnose primary gaps** (~2.5 h on r1, 32 workers × 100K ledgers):

   ```sh
   AWS_ACCESS_KEY_ID=<minio root user> \
   AWS_SECRET_ACCESS_KEY=<minio root secret> \
   AWS_ENDPOINT_URL=http://127.0.0.1:9000 \
     galexie detect-gaps \
       --config-file /etc/galexie/galexie-backfill.toml \
       --start 2 --end <network_head> \
       --output-file /var/lib/galexie/detect-gaps.json
   ```

2. **Group missing ledgers into partition starts:**

   ```sh
   jq -r '.gaps[] | "\(.start) \(.end)"' /var/lib/galexie/detect-gaps.json \
     | awk '{for(i=$1;i<=$2;i++) printf "%d\n", int(i/64000)*64000}' \
     | sort -un \
     > /tmp/partials-starts.txt
   ```

   Map each start to its hex-prefixed partition name (`partition-name-from-ledger.sh` is
   planned, not shipped) into `/tmp/partials.txt`.

3. **Re-mirror those partitions:**

   ```sh
   PARTIALS="$(cat /tmp/partials.txt)" galexie-archive-fill
   ```

   Deletes each named partition, then re-mirrors it from AWS (8 workers; `PARALLEL` env).
   See [galexie-backfill.md "mc mirror gotcha"](galexie-backfill.md#highest-impact-lever-in-practice-mirror-from-the-aws-public-bucket).

4. **Diagnose + fill cross-anchor gaps** (`verify` = check, then fill; see
   [Tool reference](#tool-reference)):

   ```sh
   TO=$(psql "$STELLARINDEX_POSTGRES_DSN" -tA \
     -c 'SELECT GREATEST(MAX(last_ledger) - 64, 2) FROM ingestion_cursors WHERE last_ledger > 0')
   /usr/local/sbin/run-heavy-job.sh archive-completeness-manual \
     /usr/local/bin/stellarindex-ops archive-completeness verify \
       -write \
       -from "$(grep -oE '[0-9]+' /etc/default/galexie-archive-fill | head -1)" \
       -to "$TO" -workers 16 \
       -output-file /var/lib/galexie/cross-anchor-gaps.json
   ```

   `compute-archive-to.sh` uses the same query; `-from` is the node's hot floor
   (ADR-0027). ~100 ms per missing file; ~7K files take ~1 min at 16 workers.

5. **End-to-end verify** — must exit 0 before enabling the timer:

   ```sh
   stellarindex-ops verify-archive -config /etc/stellarindex.toml \
     -tier all -from 2 -to <network_head> \
     -fail-on-missed
   ```

   `-fail-on-missed` makes `checkpointsMissed > 0` a hard failure.

6. **Enable the daily timer:** `systemctl enable --now archive-completeness.timer`

## Daily completeness cron (steady state)

`archive-completeness.timer` (`OnCalendar=*-*-* 02:17:00 UTC`, `RandomizedDelaySec=300`,
`Persistent=true`) fires `archive-completeness.service`
(`configs/ansible/roles/archival-node/templates/systemd/archive-completeness.service.j2`):

```sh
# ExecStartPre=/usr/local/sbin/compute-archive-to.sh  → ARCHIVE_TO in /run/archive-completeness.env
run-heavy-job.sh archive-completeness \
  stellarindex-ops archive-completeness verify \
    -write \
    -from ${ARCHIVE_FROM} -to ${ARCHIVE_TO} -workers ${WORKERS} \
    -network {{ stellar_network | default('pubnet') }} \
    -textfile-output ${TEXTFILE_OUTPUT} \
    -output-file ${REPORT_OUTPUT}
```

The unit stays `User=root`: `run-heavy-job.sh` creates its memory scope and disk watchdog
only for a root caller.

### Wall-clock budget per run

| Step | Operation | Time |
|---|---|---|
| 1 | LIST yesterday's primary partitions vs expected count | ~5 s |
| 2 | Stat each expected cross-anchor checkpoint file | ~1 s |
| 3 | Fetch missing primary files from AWS | ~50 ms/file |
| 4 | Fetch missing cross-anchor files from the nine sources | ~100 ms/file |
| 5 | Chain-link walk of yesterday's range (Tier A) | ~30 s |
| 6 | Cross-anchor verify of yesterday's range (Tier B) | ~30 s |
| 7 | Emit Prometheus gauges, exit | <1 s |

~70 s clean day; ~2–5 min with 100 missing files. Both fit inside the 26-h staleness budget R2 + R3 use for their `ReducedRedundancy` flag.

## Cross-anchor sources

Tried in order per missing file, from `DefaultCrossAnchorSources` in
`internal/archivecompleteness/cross_anchor_fill.go`. **Pubnet only:** a non-pubnet
`-network` makes the fill phase refuse.

```
1. sdf-core-live-001     https://history.stellar.org/prd/core-live/core_live_001
2. sdf-core-live-002     https://history.stellar.org/prd/core-live/core_live_002
3. sdf-core-live-003     https://history.stellar.org/prd/core-live/core_live_003
4. publicnode-bootes     https://bootes-history.publicnode.org
5. publicnode-lyra       https://lyra-history.publicnode.org
6. publicnode-hercules   https://hercules-history.publicnode.org
7. lobstr-v1             https://archive.v1.stellar.lobstr.co
8. lobstr-v2             https://archive.v2.stellar.lobstr.co
9. lobstr-v5             https://archive.v5.stellar.lobstr.co
```

Each name is the `source` label on the repair counters; every try counts as an attempt,
including ones a later source rescued. There is no captive-core replay layer: if all nine
miss a checkpoint the run exits 1 and the gap needs the `galexie-archive-fill` / bringup
path. The primary archive is filled by the separate `galexie-archive-fill.sh` timer, not
this chain.

## Prometheus surface

```
archive_files_missing{archive="galexie-archive"}     gauge
archive_files_missing{archive="cross-anchor"}        gauge
archive_completeness_run_duration_seconds            gauge     # NOT a histogram
archive_completeness_repair_attempts_total{source="sdf-core-live-001|publicnode-bootes|lobstr-v1|..."} counter
archive_completeness_repair_failures_total{source="..."} counter
archive_completeness_last_success_timestamp           gauge
```

`verify` rewrites the textfile wholesale (`.tmp` → rename), so it reads the old file back
and carries state forward:

- `archive_completeness_last_success_timestamp` is re-emitted every run with the last
  **clean** run's time; `0` = never clean on this host, which correctly fires the
  staleness alerts. (An absent series would silence them.)
- the two `repair_*_total` counters accumulate across runs, so `increase()` is valid.

### Alert rules

`deploy/monitoring/rules/archive-completeness.yml` (catalog: [alerts-catalog.md](alerts-catalog.md)):

| Alert | Condition | Severity | Runbook |
|---|---|---|---|
| `stellarindex_archive_files_missing` | `archive_files_missing > 0` for 4h | ticket | [archive-files-missing](runbooks/archive-files-missing.md) |
| `stellarindex_archive_completeness_stale` | last success > 26h, for 5m | ticket | [archive-completeness-stale](runbooks/archive-completeness-stale.md) |
| `stellarindex_archive_completeness_critical_stale` | last success > 48h, for 5m | page | [archive-completeness-stale](runbooks/archive-completeness-stale.md) |
| `stellarindex_archive_repair_source_degraded` | per source `increase(failures[25h]) / increase(attempts[25h]) > 0.10`, for 30m | informational | [archive-repair-source-degraded](runbooks/archive-repair-source-degraded.md) |

## Status-page integration (planned)

Status page `https://stellarindex.io/status` per
[sev-playbook.md §5.3](sev-playbook.md#51-status-page). Component "Historical data
integrity":

- **Operational** — every region's last success within 26 h and `archive_files_missing == 0` everywhere.
- **Degraded performance** — any region 26–48 h, or `archive_files_missing > 0` on a non-leader.
- **Partial outage** — R1 older than 48 h, or `archive_files_missing > 100` on R1.
- **Major outage** — no R1 success in 7 days.

At *Degraded* or worse the API sets `flags.reduced_redundancy = true` on every response
(ADR-0017). A worker scrapes the regions' Prometheus federations every 60 s and needs ≥ 2
consecutive samples to change state; its operator-editable note follows
[sev-playbook.md §5.4](sev-playbook.md#54-what-we-do-not-say).

## Tool reference

Modes `check | fix | verify` (`internal/ops/archive/archive_completeness.go`; unknown modes
are rejected). `check` is a read-only inventory, `fix` repairs without re-checking,
`verify` checks then fills. `-range` / `-checks` / `-trust-leader` are target ADR-0017 flags
and do not exist.

```
USAGE
  stellarindex-ops archive-completeness verify [flags]

FLAGS (shipped)
  -archive-root PATH    cross-anchor archive root (default /srv/history-archive)
  -from N               first ledger, inclusive (default 2)
  -to N                 last ledger, inclusive; REQUIRED, non-zero (0 is refused)
  -workers N            parallel fetch workers (default 8)
  -owner-user USER      owner for placed files (default stellar)
  -owner-group GROUP    group for placed files (default stellar)
  -network NAME         pubnet (default) | testnet | futurenet; fill is pubnet-only
  -output-file PATH     JSON gap report (empty = stdout)
  -textfile-output PATH node_exporter textfile (empty = no metrics)
  -write                apply changes; without it fix/verify are a dry run (#1191)

EXIT CODES
  0   clean — no missing files after the fill pass
  1   residual missing files (all nine cross-anchor sources exhausted on some)
  other  I/O error
```

## Cross-references

- [ADR-0017](../adr/0017-archive-completeness-invariants.md) — policy (4 contracts, per-region trust).
- [ADR-0016](../adr/0016-per-region-storage-strategy.md) — per-region storage shapes.
- [ADR-0015](../adr/0015-last-closed-bucket-rate-serving.md) — the closed-bucket contract these invariants protect.
- [galexie-backfill.md](galexie-backfill.md) — the `galexie-archive-fill` primary repair.
- [alerts-catalog.md](alerts-catalog.md) — the `stellarindex_archive_*` family.
- [sev-playbook.md](sev-playbook.md) — incident process and status-page conventions.
