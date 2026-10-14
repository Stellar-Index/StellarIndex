---
title: Archive completeness — invariants, bootstrap, daily cron
last_verified: 2026-10-05
status: living procedure
---

# Archive completeness — invariants, bootstrap, daily cron

Operational companion to [ADR-0017](../adr/0017-archive-completeness-invariants.md), which
holds the rationale and the four contracts. Implementation:
`internal/ops/archive/` + `internal/archivecompleteness/` (the
`stellarindex-ops archive-completeness` subcommand) and `galexie-archive-fill.sh`.

**Scope:** the shipped command enforces and repairs the **cross-anchor** archive only; a
green run is not proof that the primary `galexie-archive` checks ran.

## The two archives

**Primary — `galexie-archive/` MinIO bucket (R1).** One XDR meta file per ledger (~62 M
objects) in 64,000-ledger partitions (`<HASH>--<N>-<N+63999>/<HEX>--<ledger>.xdr.zst`). The
indexer's source of rate data, filled from
`s3://aws-public-blockchain/v1.1/stellar/ledgers/pubnet/` via `mc mirror` and
`galexie-archive-fill`.

**Cross-anchor — `/srv/history-archive/` (R1).** The canonical Stellar history archive.
`ledger/XX/YY/ZZ/ledger-XXYYZZWW.xdr.gz` holds 64 `LedgerHeaderHistoryEntry` records and is
the **verification anchor**: `verify-archive -tier checkpoint` compares each checkpoint's
signed hash with our LCM-derived hash. Source:
`https://history.stellar.org/prd/core-live/core_live_001`, mirrored at bootstrap
(`stellar-archivist` or `mc mirror`) and refreshed by the daily cron.

## Per-region behaviour

Per [ADR-0016](../adr/0016-per-region-storage-strategy.md). R2/R3 are deferred; R1
(Frankfurt) is the only host and holds both archives:

```
stellarindex-ops archive-completeness verify -from <floor> -to <tip> -workers 8
```

> **`-to 0` does NOT resolve the tip**: the binary refuses it. The unit's
> `ExecStartPre=/usr/local/sbin/compute-archive-to.sh` computes the tip from
> `ingestion_cursors` into `ARCHIVE_TO` in `/run/archive-completeness.env`. By hand, pass `-to`.

`-write` is required to fetch/repair; without it `fix`/`verify` are a fail-closed dry run
(#1191); the shipped units pass it. If repair fails the run exits non-zero and alerts fire.

R2/R3 (target design, not shipped; `-range` / `-checks` / `-trust-leader` do not exist) would trust R1 and set the API `ReducedRedundancy` flag if R1's `archive_completeness_last_success_timestamp` is older than **26 h** (ADR-0017).

## Bootstrap procedure (one-shot)

Required before the daily timer enforces anything on a node.
1. **Diagnose primary gaps** (~2.5 h on r1):

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

   Map each start to its hex-prefixed partition name into `/tmp/partials.txt` (no script ships).

3. **Re-mirror those partitions:**

   ```sh
   PARTIALS="$(cat /tmp/partials.txt)" galexie-archive-fill
   ```

   Deletes each named partition, then re-mirrors it from AWS (8 workers; `PARALLEL` env);
   see [galexie-backfill.md "mc mirror gotcha"](galexie-backfill.md#highest-impact-lever-in-practice-mirror-from-the-aws-public-bucket).

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
   (ADR-0027). 

5. **End-to-end verify** — must exit 0 before enabling the timer:

   ```sh
   stellarindex-ops verify-archive -config /etc/stellarindex.toml \
     -tier all -from 2 -to <network_head> \
     -fail-on-missed
   ```


6. **Enable the daily timer:** `systemctl enable --now archive-completeness.timer`

## Daily completeness cron (steady state)

`archive-completeness.timer` (`OnCalendar=*-*-* 02:17:00 UTC`, `RandomizedDelaySec=300`,
`Persistent=true`) fires `archive-completeness.service`:

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

The unit stays `User=root` (`run-heavy-job.sh` scopes memory and disk only for root). A clean day takes ~70 s.

## Cross-anchor sources

Tried in order per missing file (`DefaultCrossAnchorSources`,
`internal/archivecompleteness/cross_anchor_fill.go`). **Pubnet only:** a non-pubnet
`-network` makes the fill phase refuse.

```
sdf-core-live-001/002/003  https://history.stellar.org/prd/core-live/core_live_00{1,2,3}
publicnode-bootes|lyra|hercules  https://{bootes,lyra,hercules}-history.publicnode.org
lobstr-v1|v2|v5            https://archive.v{1,2,5}.stellar.lobstr.co
```

Each name is the `source` label on the repair counters. There is no captive-core replay
layer: if all nine miss a checkpoint the run exits 1 and the gap needs the
`galexie-archive-fill` / bringup path.

## Prometheus surface

```
archive_files_missing{archive="galexie-archive"}     gauge
archive_files_missing{archive="cross-anchor"}        gauge
archive_completeness_run_duration_seconds            gauge     # not a histogram
archive_completeness_repair_attempts_total{source="..."} counter
archive_completeness_repair_failures_total{source="..."} counter
archive_completeness_last_success_timestamp           gauge
```

`verify` rewrites the textfile wholesale, so it carries state forward: the last-success
timestamp is re-emitted every run with the last **clean** run's time (`0` = never clean,
which correctly fires the staleness alerts; an absent series would silence them), and the
two `repair_*_total` counters accumulate, so `increase()` is valid.

### Alert rules

`deploy/monitoring/rules/archive-completeness.yml` (catalog: [alerts-catalog.md](alerts-catalog.md)):

| Alert | Condition | Severity |
|---|---|---|
| `stellarindex_archive_files_missing` | `archive_files_missing > 0` for 4h | ticket |
| `stellarindex_archive_completeness_stale` | last success > 26h, for 5m | ticket |
| `stellarindex_archive_completeness_critical_stale` | last success > 48h, for 5m | page |
| `stellarindex_archive_repair_source_degraded` | per-source failure ratio over 25h > 0.10, for 30m | informational |

Runbooks: [runbooks/archive.md](runbooks/archive.md#stellarindex_archive_files_missing), anchored by alert name.

## Status page (planned)

"Historical data integrity" ([sev-playbook.md](sev-playbook.md#51-status-page)): degraded at
26–48 h since a region's last success or missing files on a non-leader; partial outage at R1
older than 48 h or >100 missing on R1; major outage with no R1 success in 7 days. Degraded
or worse sets `flags.reduced_redundancy = true` on API responses.

## Tool reference

Modes `check | fix | verify` (`internal/ops/archive/archive_completeness.go`): `check` is a
read-only inventory, `fix` repairs without re-checking, `verify` checks then fills.

```
FLAGS (stellarindex-ops archive-completeness verify)
  -archive-root PATH    cross-anchor archive root (default /srv/history-archive)
  -from N               first ledger, inclusive (default 2)
  -to N                 last ledger, inclusive; REQUIRED, non-zero (0 is refused)
  -workers N            parallel fetch workers (default 8)
  -owner-user/-owner-group  owner of placed files (default stellar)
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

[ADR-0017](../adr/0017-archive-completeness-invariants.md) (policy),
[ADR-0016](../adr/0016-per-region-storage-strategy.md) (per-region storage),
[ADR-0015](../adr/0015-last-closed-bucket-rate-serving.md) (the contract these invariants protect),
[galexie-backfill.md](galexie-backfill.md) (`galexie-archive-fill` repair),
[alerts-catalog.md](alerts-catalog.md), [sev-playbook.md](sev-playbook.md).
