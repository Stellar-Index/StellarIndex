---
title: Runbook — DR (disaster-recovery) activation
last_verified: 2026-08-28
status: ratified
related:
  - docs/architecture/ha-plan.md
  - docs/adr/0008-ha-topology.md
  - docs/adr/0050-multi-region-ha-architecture.md
  - docs/adr/0016-per-region-storage-strategy.md
  - docs/operations/sev-playbook.md
  - docs/operations/archival-node-bringup.md
  - docs/operations/runbooks/postgres.md#stellarindex_timescale_primary_down
---

# Runbook — DR (disaster-recovery) activation

Cut traffic over to a standby region when the primary (R1) is partially or fully unrecoverable. Sister to [`postgres.md#stellarindex_timescale_primary_down`](postgres.md#stellarindex_timescale_primary_down) §D (complete cluster loss). SEV-1: open the incident channel and follow [`sev-playbook.md`](../sev-playbook.md) §4. Architecture and roadmap: [`ha-plan.md`](../../architecture/ha-plan.md) (ADR-0050).

**Status.** This runbook has NOT been executed end-to-end: no standby region exists (R2/R3 unprovisioned; single-host r1 is the whole deployment, no Patroni, Sentinel, HAProxy or Cloudflare LB pool; `api.stellarindex.io` is a bare A record to r1's Caddy). The only restore evidence is the drill, `scripts/ops/restore-drill.sh` on `restore-drill.timer`, logged in [`drills/restore-drills.md`](../drills/restore-drills.md); confirm a scheduled PASS in `journalctl -u restore-drill.service`. Multi-region steps are untested design. Primary-minted API keys return 401 in a DR region (`ha-plan.md` §3.3).

> **The backups are on the box you are recovering from.** pgBackRest `repo1` is a ZFS dataset on r1's own `data` pool (`pgbackrest_offsite_ack: true` in `configs/ansible/inventory/r1.yml` acknowledges the missing off-site repo); ClickHouse has no DATA backup, only a local schema snapshot (`ch-schema-snapshot.timer`, ADR-0043 §2.1). Logical corruption with a readable pool: pgBackRest restore is available. Pool or host loss: the backups and the galexie archive die with it (ledgers already trimmed to the AWS cold tier under ADR-0027 survive), and the path is a rebuild from a public history archive, days to weeks (`ha-plan.md` §8, `docs/operations/off-site-backup-plan.md`).

## 1. When to activate DR

Activate when ANY holds AND no faster recovery exists:

- Primary storage unrecoverable: no promotable replica, pgBackRest restore failing or estimated > 4 h ([`postgres.md#stellarindex_timescale_primary_down`](postgres.md#stellarindex_timescale_primary_down) §A cannot promote).
- Primary region partitioned > 30 min, confirmed regional by the Cloudflare probe plus a third-party status site.
- Primary host fleet gone (provider-side failure; Hetzner status red on R1's location, no ETA).
- Corruption on primary AND replica: restore from pgBackRest into the DR region.

Do not activate (once Patroni/Sentinel exist) when Patroni is mid-failover (give it 60 s), a single Timescale primary is down with a healthy replica ([`postgres.md#stellarindex_timescale_primary_down`](postgres.md#stellarindex_timescale_primary_down) §A), Redis lost a master ([`scenarios/sev2-redis-sentinel-failover.md`](../drills/scenarios/sev2-redis-sentinel-failover.md)), or R2/R3 is degraded while R1 is healthy (independent-ingest, [`ha-plan.md`](../../architecture/ha-plan.md#1-decisions-that-still-bind) §1). Bias to activate: a wrong activation costs ~5 s RPO and a DNS blip.

## 2. DR region pre-flight (before any traffic flip)

The IC's tech lead runs these while the comms lead drafts the status update.

### 2.1 Storage is current

```sh
# From a DR-region operator workstation:
ssh root@<dr-region-postgres-primary>

# Check pgBackRest archive freshness
pgbackrest --stanza=stellarindex info | head -20
# → "archive (current)" + most-recent backup/WAL within ~5 min

# If WAL replication lag was ≤ 5 s (the SLO target):
psql -c "SELECT pg_last_wal_receive_lsn(), pg_last_wal_replay_lsn();"
# → both LSNs should be the same or within a handful of bytes
```

DR storage > 30 min behind and primary still partially serving: let WAL ship the gap first. Primary gone: the lag is the RPO.

### 2.2 MinIO archive is intact

```sh
# On the DR-region archival host. Read the last report …
curl -sS http://127.0.0.1:3000/v1/diagnostics/archive | jq '{scanned_at, range, cross_anchor: {expected, found, missing_count}}'
# → scanned_at recent + cross_anchor.missing_count == 0

# … or force a fresh run (archive-completeness.service runs
# `stellarindex-ops archive-completeness verify -from $ARCHIVE_FROM -to $ARCHIVE_TO
#  -workers 8 -network pubnet -textfile-output … -output-file
#  /var/lib/galexie/last-completeness-report.json` under run-heavy-job.sh)
sudo systemctl start archive-completeness.service && journalctl -u archive-completeness.service -n 30 --no-pager
# → exit 0 and missing_count == 0 in the report
```

It checks the cross-anchor history archive only, not MinIO bucket structure. Gaps degrade since-inception data; they do not block the flip.

### 2.3 DR hosts are reachable

```sh
# From DR-region SSH bastion:
for host in <dr-api> <dr-aggregator> <dr-indexer>; do
    ssh root@$host 'systemctl status stellarindex-api stellarindex-aggregator stellarindex-indexer galexie stellar-core minio --no-pager' | grep -E "Active|service"
    # ops jobs are timer-driven and NOT named stellarindex-*, so a glob misses them:
    ssh root@$host 'systemctl list-timers --all --no-pager' | grep -E 'archive-completeness|galexie-archive|verify-archive-tier|supply-snapshot|restore-drill|pgbackrest-backup'
done
# → every service "active (running)" or "active (waiting)"; every timer with a NEXT fire
```

Also confirm `stellarindex_binary_version_skew` / `stellarindex_binary_version_probe_degraded` (`configs/prometheus/rules.r1/binary-version-skew.yml`) are not firing on the target region (mismatched binaries serve byte-divergent closed buckets). A host down: replace it ([`archival-node-bringup.md`](../archival-node-bringup.md)) or flip with reduced redundancy and say so on the status page.

## 3. Traffic flip

### 3.1 Status page

Comms lead posts **Investigating** per [`sev-status-page-update.md`](sev-status-page-update.md): "We've identified an issue with our primary region and are failing over to our disaster-recovery region. API requests may briefly fail or return stale data during the flip (typically < 60 seconds)." Name no internal infrastructure; no cause speculation.

### 3.2 DNS flip

Design only today: no Cloudflare LB pool, HAProxy VIP or `cf-cli` exists; `api.stellarindex.io` is an A record to r1's Caddy (`136.243.90.96`). TODO(maintainer): decide the interim mechanism and where the Cloudflare API token lives.

- **A. Cloudflare load balancer (preferred, per [ADR-0008](../../adr/0008-ha-topology.md)):** Cloudflare, Traffic, Load Balancers, `api-prod`; edit the primary pool, set `enabled: false`, save. Traffic shifts in ~30 s.
- **B. Manual DNS swap (fallback):**

```sh
# Requires the Cloudflare API token (location: TODO(maintainer) — no ops-keys doc exists in-tree).
# No cf-cli wrapper exists; use the Cloudflare dashboard or the raw API to
# repoint the api.stellarindex.io A record at the DR origin.
# TTL is 60s by design; full propagation < 2 min
```

### 3.3 Verify API is serving from DR

```sh
# Liveness — /v1/healthz carries NO region field
curl -sS -H 'X-Request-Id: dr-flip-verify' https://api.stellarindex.io/v1/healthz
# → {"status":"ok","uptime":"…","status_root":"/v1/status"}

# Origin identity — the region label lives on /v1/status
curl -sS https://api.stellarindex.io/v1/status | jq '.region'
# → {"name": "<dr region_name>", "deployment": "…"}

# Spot-check a real query
curl -sS 'https://api.stellarindex.io/v1/price?asset=native' | jq '.data.price, .as_of'
# → non-empty price + recent timestamp
```

`.region.name` (`[region] name` in `stellarindex.toml`; r1 is `Hetzner-r1`) confirms the origin. A primary region name means a CDN edge still caches DNS: wait for TTL expiry.

### 3.4 Status page

Post **Identified**: one-line cause, mitigation deployed. "We've completed failover to our disaster-recovery region. API requests are now serving normally. Some historical data may temporarily reflect a brief gap during the flip; we'll post full service confirmation within the hour."

## 4. Post-flip monitoring (first hour)

### 4.1 SLO and probe alerts

```sh
# On the DR serving host (Prometheus :9090 is firewalled to internal addresses)
curl -sS http://127.0.0.1:9090/api/v1/alerts | jq -r '.data.alerts[] | select(.state=="firing") | .labels.alertname'
```

Watch (rules under `configs/prometheus/rules.r1/`): p95 < 200 ms (`stellarindex_api_latency_p95_high`, `stellarindex_slo_latency_burn_fast`); 5xx rate < 0.1 % (`stellarindex_api_error_rate_high` / `_critical`, `stellarindex_slo_availability_burn_fast`); the external probe (`stellarindex_sla_probe_p95_breach` / `_freshness_breach` / `_unit_failed_alert` / `_stale`, `sla-probe.yml`); price freshness (`stellarindex_price_staleness_seconds` near baseline, `stellarindex_api_price_stale` quiet; a rising gauge means upstream sources are not all reaching DR).

### 4.2 Aggregator and ingest

```sh
# On the DR host. Should match primary's pre-incident steady state within ~5 min
stellarindex-ops list-cursors -config /etc/stellarindex.toml
# → every source's last_ledger advancing
stellarindex-ops detect-gaps -config /etc/stellarindex.toml
# → exit 0 (exit 1 = a source lags > 100 ledgers behind the network tip)
```

Cursors stuck: check Galexie's read connectivity to the DR MinIO bucket ([`ha-plan.md`](../../architecture/ha-plan.md#23-planned-regions) §2.3).

### 4.3 Freeze rates

```sh
# Same expression as alert stellarindex_anomaly_freeze_engaged, rules.r1/anomaly.yml
curl -sS http://127.0.0.1:9090/api/v1/query \
    --data-urlencode 'query=sum by (class) (rate(stellarindex_anomaly_freeze_engaged_total[5m]))' | jq
```

A SUSTAINED rate above baseline means DR's source set is incomplete.

## 5. Status-page resolution

When SLA, ingest and flag rates are at baseline for >= 30 min, post **Resolved** ("Service is fully restored as of <UTC>. Total customer impact: <duration>; post-mortem within 72 hours"). Post-mortem per [`sev-playbook.md`](../sev-playbook.md) §6.

## 6. Failback to primary

Do not fail back in the same shift unless the failure was clearly transient; run on DR 24-72 h to validate the primary's fix. When the primary is verified healthy:

1. Catch-up check; the primary must be a streaming replica of DR:
   ```sh
   # Check WAL streaming from DR back to primary
   psql -h <primary-postgres> -c "SELECT pg_is_in_recovery(), pg_last_wal_replay_lsn();"
   # → in_recovery=true, replay_lsn closely tracking DR's send_lsn
   ```
   `pg_is_in_recovery()` false = a diverged former primary: escalate to the IC (`pg_rewind` or full restore from DR's pgBackRest), not a simple flip.
2. Reverse the §3.2 flip (DR pool down, primary pool up) and re-run §3.3.
3. Monitor as §4 on the primary; replication lag shows as `stellarindex_api_price_stale` firing or `stellarindex_price_staleness_seconds` rising on /v1/price.

## 7. Escalation

If a §3 step fails (DNS will not propagate, DR healthz returns 5xx): do not roll back (the primary is presumed lost); page secondary on-call and the engineering manager; post "extended outage during failover" without speculating on cause; fall back to a fresh archival node in a third region ([`archival-node-bringup.md`](../archival-node-bringup.md)), hours not minutes.

## 8. Not covered

Fresh region bring-up ([`archival-node-bringup.md`](../archival-node-bringup.md)); per-component HA failover ([`postgres.md#stellarindex_timescale_primary_down`](postgres.md#stellarindex_timescale_primary_down), [`scenarios/sev2-redis-sentinel-failover.md`](../drills/scenarios/sev2-redis-sentinel-failover.md)); the annual DR exercise ([`sev-playbook.md`](../sev-playbook.md#8-drills)); incident overlay ([`sev-playbook.md`](../sev-playbook.md) §4).

## 9. Drift signals

Quarterly: DR `/v1/status` `.region.name` is right; Cloudflare LB pools have current host IPs; pgBackRest stanza is fresh in BOTH regions; this runbook still matches [`ha-plan.md`](../../architecture/ha-plan.md) / ADR-0050. A failed signal is a launch-readiness regression: file an issue with the `dr-readiness` label.
