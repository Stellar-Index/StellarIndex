# R1 single-host Prometheus rules

These are R1-tuned copies of [`deploy/monitoring/rules/`](../../../deploy/monitoring/rules/),
adapted for the single-host scrape config in [`prometheus.r1.yml`](../prometheus.r1.yml):

| Source rule | R1 rule | Adaptation |
|-------------|---------|------------|
| `api.yml` | `api.yml` | `job="api"` → `job="stellaratlas-api"` |
| `aggregator.yml` | `aggregator.yml` | `job="aggregator"` → `job="stellaratlas-aggregator"` |
| `ingestion.yml` | `ingestion.yml` | `job="indexer"` → `job="stellaratlas-indexer"`. Source-stopped window widened to 30 min rate / 15 min for (F-1212b — see file header for rationale). |
| `infra.yml` | `infra.yml` | `job="node"` → `job="node_exporter"` |
| `meta.yml` | `meta.yml` | scrape regex narrowed to R1 jobs |
| `slo.yml` | `slo.yml` | `job="api"` → `job="stellaratlas-api"` |
| `anomaly.yml` | `anomaly.yml` | as-is (no job-label refs) |
| `divergence.yml` | `divergence.yml` | as-is |
| `external-pollers.yml` | `external-pollers.yml` | as-is |
| `supply.yml` | `supply.yml` | as-is |
| `supply-snapshot.yml` | `supply-snapshot.yml` | as-is |
| `supply-refresh.yml` | `supply-refresh.yml` | as-is |
| `archive-completeness.yml` | `archive-completeness.yml` | requires node_exporter `--collector.textfile` + `/var/lib/node_exporter/textfile_collector/` (provisioned by the archival-node role's `10-observability.yml` task). |
| `verify-archive.yml` | `verify-archive.yml` | requires node_exporter `--collector.systemd` (already on). |
| `sla-probe.yml` | `sla-probe.yml` | requires textfile_collector + `-textfile-output` arg on the probe binary (wired in `configs/healthchecks/sla-probe.sh`). |

The remaining files in `deploy/monitoring/rules/` are still
intentionally NOT shipped here:

- `cache.yml` / `storage.yml` — assume `redis_exporter` /
  `postgres_exporter` (not deployed on R1) and reference HA labels
  (`role="master"`, `role="primary"`, replication metrics). When
  those exporters land in the archival-node role, copy these
  files over and add the corresponding scrape jobs to
  `prometheus.r1.yml`.
- `stellar.yml` — references `stellar-core-prometheus-exporter`,
  which is only installed when `run_stellar_core` is true (post
  Phase-3 Tier-1 validator rollout per ADR-0004). Stays inert
  until then.

Each file added here is a strict subset of the multi-host rule set;
adding a previously-skipped file is a deliberate operator action,
not a default.

**Recent additions (rc.49):** anomaly / divergence / external-pollers
/ supply / supply-snapshot / supply-refresh / archive-completeness /
verify-archive / sla-probe — wired up to close audit-2026-05-12
findings F-1219 + F-1220 + F-1221 + F-1252. The SLA-evidence chain
(probe writes textfile → node_exporter `--collector.textfile` reads
it → Prometheus scrapes → sla-probe rules → alertmanager) is now
end-to-end after the ansible role provisions the collector dir
and the wrapper script defaults `SLA_PROBE_TEXTFILE_OUTPUT`.

## Apply to R1

```sh
scp configs/prometheus/rules.r1/*.yml root@136.243.90.96:/etc/prometheus/rules.r1/
ssh root@136.243.90.96 'systemctl reload prometheus'
```

`prometheus.r1.yml` loads `/etc/prometheus/rules.r1/*.yml`
(matches the source-tree directory name), so no Prometheus
config change is needed. F-1268 (2026-05-13) corrected an
earlier README that pointed at the wrong `/etc/prometheus/rules.d/`
target.

## Migrate to multi-host

When R2 / R3 land, switch to
`configs/ansible/roles/prometheus/files/rules/` (the unmodified
multi-host set in `deploy/monitoring/rules/`) and decommission
this directory.
