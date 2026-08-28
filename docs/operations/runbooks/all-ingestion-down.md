---
title: Runbook — all ingestion sources down
last_verified: 2026-08-28
status: ratified
severity: P1
---

# Runbook — `stellarindex_ingestion_all_sources_stopped`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingestion_all_sources_stopped` |
| Severity | **P1** (SEV-1) |
| Detected by | `sum(rate(stellarindex_source_events_total[5m]))` = 0 for > 3 min |
| Typical MTTR | 5–20 min depending on root cause |
| Impact | Price staleness begins at the 60 s cache TTL; API sets `stale_flag=true` globally. If the outage lasts > 30 min we breach the Freighter 30 s freshness SLA. |

## Symptoms

- Alert `stellarindex_ingestion_all_sources_stopped` fires.
- `stellarindex_api_price_stale` (ticket) follows ~7 min later
  (rule: `stellarindex_price_staleness_seconds > 120` with `for: 5m`,
  `configs/prometheus/rules.r1/api.yml`).
- Indexer logs show no activity, repeated MinIO read errors, or
  Galexie producing no fresh objects in `galexie-live`.

> The legacy `stellarindex_ingestion_lag_high` companion alert was
> retired with the move off the orchestrator topology
> ([alerts-catalog.md](../alerts-catalog.md) §Ingestion historical
> note); don't expect to see it fire today.

## Quick diagnosis (≤ 5 min)

> **Architecture reminder.** Production ingest reads Galexie's MinIO
> output directly via `go-stellar-sdk/ingest.ApplyLedgerMetadata`
> ([architecture/ingest-pipeline.md](../../architecture/ingest-pipeline.md));
> stellar-rpc was removed from r1 on 2026-04-23 and is no longer in
> the data path. The "shared upstream" is now Galexie + MinIO, not
> stellar-rpc. Sections C / D of the legacy "stellar-rpc is the
> problem" branch below are retained as future-tense for any
> deployment that still routes through RPC; on r1 today, jump to
> Galexie / MinIO checks first.

The "all sources down" shape usually means one of three common roots: Galexie's MinIO output (the shared upstream), the shared storage (Timescale), or the indexer process itself.

```sh
# 1. Is the indexer running?
systemctl status stellarindex-indexer      # systemd-managed binary on r1

# 2. What do its logs say?
journalctl -u stellarindex-indexer -n 200 --no-pager | tail -40
# Look for: "ledgerstream exited with error" (cmd/stellarindex-indexer/main.go),
# "insert trade failed" (internal/pipeline/sink.go),
# "postgres ping failed 3x — pool may be wedged" (main.go), and the
# ledgerstream tier-read outcome "both_missing" (see
# ledgerstream-tier-both-missing.md).

# 3. Is Galexie producing fresh ledger objects?
sudo journalctl -u galexie -n 50 --no-pager
mc ls --json --recursive local/galexie-live/ \
  | jq -r 'select(.key|test("\\.xdr\\.zst$")) | .lastModified' | sort -r | head -1
# newest ledger object; one lands every ~5 s (galexie_ledgers_per_file: 1).
# The mc alias on r1 is `local` (roles/archival-node/tasks/09-minio.yml);
# a non-recursive `mc ls` only lists partition prefixes, not freshness.
# OR if you suspect a network-state issue, query upstream directly
# (r1 has no local stellar-rpc; point at a public endpoint):
stellarindex-ops rpc-probe https://mainnet.sorobanrpc.com
# Expect: version info + latest ledger close time within 60s.

# 4. Is Timescale reachable + writable?
# Postgres is local to r1 (127.0.0.1); use peer auth as the postgres OS user.
sudo -u postgres psql -d stellarindex -c "INSERT INTO ingestion_cursors (source, sub_source, last_ledger) VALUES ('probe', 'healthcheck', 0) ON CONFLICT DO NOTHING;"
sudo -u postgres psql -d stellarindex -c "DELETE FROM ingestion_cursors WHERE source='probe';"  # remove the probe row (it otherwise shows in `stellarindex-ops list-cursors`)
```

Route by the result:

- Galexie isn't producing fresh objects in `galexie-live` → galexie's captive-core stalled or upstream network issue. Check `journalctl -u galexie`; if galexie itself is healthy, fall back to the public-rpc probe to confirm the network is closing ledgers.
- Galexie healthy + fresh objects in MinIO but indexer not reading → networking issue between indexer and MinIO, or indexer's MinIO credentials / endpoint config wrong. Check firewall, DNS, the `[storage]` section of `/etc/stellarindex.toml` (`s3_endpoint`, `s3_bucket_live`) and `STELLARINDEX_S3_ACCESS_KEY` / `STELLARINDEX_S3_SECRET_KEY` in `/etc/default/stellarindex`.
- psql INSERT fails → Timescale issue. Jump to [timescale-primary-down](timescale-primary-down.md).
- All probes pass but indexer produces no events → the indexer is alive but wedged. Likely deadlock or internal bug.

## Mitigation (≤ 15 min)

### A. Galexie / MinIO is the upstream problem

- Confirm galexie itself is healthy: `systemctl status galexie`,
  `journalctl -u galexie --since="10 min ago"`. galexie embeds its
  own captive-core; recoverable hangs typically clear with a
  service restart. Check disk pressure on `/var/lib/galexie` (ZFS dataset): `df -h /var/lib/galexie`.
- Confirm fresh objects are landing in MinIO (same command as
  step 3 above; `mc ls --json --recursive local/galexie-live/ | jq …`).
  The newest object should be within ~1 minute. If MinIO itself is
  unhealthy: `systemctl status minio`; MinIO exporter-down →
  [exporter-down](exporter-down.md); MinIO scrape 403 →
  [minio-metrics-403](minio-metrics-403.md). There is no
  minio-down runbook yet.
- Wider network problem? `stellarindex-ops rpc-probe https://mainnet.sorobanrpc.com`
  confirms ledgers are still closing on the network — if the
  network is fine but galexie has stalled, capture logs and
  restart galexie.
- *Future-tense:* if a deployment routes through stellar-rpc
  rather than direct-MinIO ingest, [rpc-lag](rpc-lag.md) covers
  that path. r1 does not run stellar-rpc as of 2026-04-23.

### B. Timescale is the problem

- Proceed to [timescale-primary-down](timescale-primary-down.md).

### C. Indexer itself is wedged

- Capture a goroutine dump before restarting:
  ```sh
  # NOTE: `pgrep stellarindex-indexer` (no -f) matches nothing — the
  # kernel comm is truncated to 15 chars. Signal via systemd instead.
  systemctl kill --signal=SIGQUIT stellarindex-indexer.service   # Go default SIGQUIT handler: goroutine dump to journal, then exit(2)
  sleep 3
  journalctl -u stellarindex-indexer -n 300 --no-pager > /tmp/indexer-dump-$(date +%s).log
  ```
  The unit has `Restart=on-failure` / `RestartSec=10s`, so it
  self-restarts ~10 s after the dump; the explicit restart below is
  only needed if it did not come back (`systemctl is-active
  stellarindex-indexer`).
- Restart the indexer:
  ```sh
  systemctl restart stellarindex-indexer
  ```
- Confirm recovery:
  - `stellarindex_source_events_total` rate > 0 within 60 s.
  - Alert clears within 3 min.

### D. Recent deploy broke the indexer

- Check deploy history: last 4 h.
  ```sh
  # Tag history of releases — SemVer vX.Y.Z (e.g. v0.47.2)
  git tag --sort=-creatordate | head -10
  # Running versions on r1 are the on-host sidecars (source of record;
  # see docs/operations/deployed-versions.md):
  ssh root@r1 'for f in /var/lib/stellarindex/deployed-versions/stellarindex-*; do echo "$f: $(cat $f)"; done'
  ssh root@r1 'ls -lh /usr/local/bin/stellarindex-indexer.prev-*'
  ```
- **Revert** to the previous release per
  [`release-process.md`](../release-process.md) §"Rollback".
  The indexer ships as a systemd-managed binary, not a
  containerised service — so the revert is a single binary
  swap. F-1222 (codex audit-2026-05-12): the deploy task keeps
  the last 5 previous binaries as
  `/usr/local/bin/<binary>.prev-<previous-tag>` and records
  the running version under
  `/var/lib/stellarindex/deployed-versions/<binary>` (NOT under
  `/opt/stellarindex/release-<tag>/` — the `goreleaser`-style
  release archive doesn't exist on this host).

  The preferred path is to trigger the deploy workflow with
  the previous tag (`gh workflow run deploy.yml -f region=r1
  -f version=<previous> -f binaries=stellarindex-indexer`, or
  omit `binaries` to roll the whole managed set back); for the
  manual fallback:
  ```sh
  # r1 is a single host (inventory: one entry in the `archival_nodes` group).
  PREVIOUS=v0.47.1                         # whichever tag was healthy
  ssh root@r1 <<EOF
    set -euxo pipefail
    test -f /usr/local/bin/stellarindex-indexer.prev-${PREVIOUS}
    systemctl stop stellarindex-indexer
    install -m 0755 /usr/local/bin/stellarindex-indexer.prev-${PREVIOUS} /usr/local/bin/stellarindex-indexer
    echo ${PREVIOUS} > /var/lib/stellarindex/deployed-versions/stellarindex-indexer
    systemctl start stellarindex-indexer
    systemctl status stellarindex-indexer --no-pager
  EOF
  # If the wanted .prev-<tag> is older than 5 releases
  # back the operator rebuilds it from the tag on a build host
  # (`git checkout <tag> && make build`) — see
  # release-process.md §Rollback for the full recipe.
  ```
  Rolling back ONLY the indexer leaves the other managed binaries
  on the newer tag, which trips `stellarindex_binary_version_skew`
  (ticket, `for: 45m`) — expected; ack it or roll back the full set.
  See [binary-version-skew](binary-version-skew.md).

  Then file a SEV-2 minimum + a postmortem in
  `docs/operations/postmortems/` per release-process.md §Post-flight
  (postmortem requirement) / §Post-rollback.
- After revert, re-run diagnostics in step C.

## Root cause analysis

Gather:

- Goroutine dump from step C.
- Indexer logs `journalctl -u stellarindex-indexer --since "30 min ago"`.
- Grafana screenshots of `stellarindex_source_events_total` broken down by source — does it cliff-edge at a specific timestamp, or decay?
- Recent deploys — git log of `cmd/stellarindex-indexer/` in the last 72 h.
- Postgres `pg_stat_activity` during the window — were inserts blocked on locks?
- Galexie / MinIO state during the window: `journalctl -u galexie`, `journalctl -u minio`, and `stellarindex_ledgerstream_tier_read_total{outcome="both_missing"}`.

Patterns observed:

1. **Shared upstream down** — Galexie/MinIO live export stalled (captive-core hang, MinIO down, or bucket credentials). Mitigation: restart galexie / minio per §A; `stellarindex-ops rpc-probe` against a public endpoint tells you whether the network itself is closing ledgers.
2. **Shared storage backpressure** — Timescale insert latency spiked; indexer's output channel filled; all source goroutines blocked on `out <- evt`. Mitigation: indexer needs a buffered channel with drop-oldest policy for slow-consumer safety.
3. **Config-change caused source registry to be empty** — `ingestion.enabled_sources` accidentally set to `[]`. Note: `internal/config/validate.go` rejects empty *entries* in `enabled_sources` and unknown names, but an empty list `[]` is still accepted — check the effective config with `stellarindex-indexer -config /etc/stellarindex.toml` startup logs.
4. **Panic in a source** — bad decoder blows up one goroutine + the watch goroutine waits forever. Mitigation: defer-recover in the dispatcher / pipeline stages; the unit's `Restart=on-failure` covers a crash (the per-source orchestrator was retired 2026-04-23).

## Known false-positive patterns

- **Ledger range not yet in the live bucket** — galexie is behind (cold catchup after a restart takes ~9 min on mainnet) so the reader sees `both_missing` while it waits. The indexer correctly stops producing trade events until objects appear. Alert can fire spuriously. The staleness-age metrics `stellarindex_source_last_event_unix` / `stellarindex_source_last_insert_unix` (internal/obs/metrics.go) exist and already feed `stellarindex_ingestion_source_insert_stale`; this alert still uses the raw rate.
- **Midnight UTC continuous-aggregate refresh** — the aggregator's heavy CAGG refresh briefly blocks trade inserts. Indexer queues up, then drains. Alert might fire at the window if duration is short. Tune `for: 3m → for: 5m` if this recurs.

## Related

- [rpc-lag](rpc-lag.md) — only for deployments that still route through stellar-rpc (not r1).
- [ledgerstream-tier-both-missing](ledgerstream-tier-both-missing.md) — reader can find the ledger in neither MinIO tier.
- [exporter-down](exporter-down.md) / [minio-metrics-403](minio-metrics-403.md) — MinIO monitoring.
- [binary-version-skew](binary-version-skew.md) — expected after a partial rollback.
- [timescale-primary-down](timescale-primary-down.md) — next step when DB is the root cause.
- [ingestion-lag](ingestion-lag.md) — single-source-lag runbook.
- [cursor-stuck](cursor-stuck.md) — cursor-specific diagnosis.
- Internal docs:
  - `internal/dispatcher/` + `internal/pipeline/` — dispatcher hot path and sinks (the orchestrator was retired 2026-04-23; see `internal/consumer/doc.go`).
  - `cmd/stellarindex-indexer/main.go` — wiring + shutdown.

## Changelog

- 2026-04-22 — initial draft. @ash.
- 2026-04-30 — quick-diagnosis + Mitigation A rewritten around
  Galexie + MinIO (the actual r1 upstream); rpc-probe URL points
  at a public stellar-rpc since r1 doesn't run its own
  (removed 2026-04-23). Symptoms drop the retired
  `stellarindex_ingestion_lag_high` reference.
- 2026-08-28 — re-verified against HEAD: SIGQUIT via `systemctl kill`
  (bare `pgrep` never matches the truncated comm); psql via local
  peer auth (no `db-primary.internal`); `mc` alias `local` +
  recursive freshness listing; `[storage]` not `[ledgerstream]`;
  price_stale lag ~7 min; SemVer tags + deployed-versions sidecars
  replace CalVer / `r1-deployment-state.md`; single-host `root@r1`
  rollback writes the sidecar and expects `binary_version_skew`;
  orchestrator/stellar-rpc patterns rewritten for Galexie+MinIO.
