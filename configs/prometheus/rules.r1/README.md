# R1 single-host Prometheus rules

These are R1-tuned copies of [`deploy/monitoring/rules/`](../../../deploy/monitoring/rules/),
adapted for the single-host scrape config in [`prometheus.r1.yml`](../prometheus.r1.yml):

| Source rule | R1 rule | Adaptation |
|-------------|---------|------------|
| `api.yml` | `api.yml` | `job="api"` → `job="ratesengine-api"` |
| `aggregator.yml` | `aggregator.yml` | `job="aggregator"` → `job="ratesengine-aggregator"` |
| `ingestion.yml` | `ingestion.yml` | `job="indexer"` → `job="ratesengine-indexer"` |
| `infra.yml` | `infra.yml` | `job="node"` → `job="node_exporter"` |
| `meta.yml` | `meta.yml` | scrape regex narrowed to R1 jobs |
| `slo.yml` | `slo.yml` | `job="api"` → `job="ratesengine-api"` |

The remaining files in `deploy/monitoring/rules/` are intentionally
NOT shipped here:

- `cache.yml` / `storage.yml` — assume `redis_exporter` /
  `postgres_exporter` (not deployed on R1) and reference HA labels
  (`role="master"`, `role="primary"`, replication metrics).
- `archive-completeness.yml` / `verify-archive.yml` — assume the
  `archivecompleteness` / `verify-archive` daemons; not running on
  R1 yet.
- `divergence.yml` / `anomaly.yml` / `supply*.yml` / `stellar.yml` —
  reference metrics that the current binaries don't all emit;
  skipped to avoid alerts that quietly never fire.
- `sla-probe.yml` — `ratesengine-sla-probe` is a one-shot CLI on
  R1, not a scraped service.

Each file added here is a strict subset of the multi-host rule set;
adding a previously-skipped file is a deliberate operator action,
not a default.

## Apply to R1

```sh
scp configs/prometheus/rules.r1/*.yml root@136.243.90.96:/etc/prometheus/rules.d/
ssh root@136.243.90.96 'systemctl reload prometheus'
```

`prometheus.r1.yml` already loads `/etc/prometheus/rules.d/*.yml`,
so no Prometheus config change is needed.

## Migrate to multi-host

When R2 / R3 land, switch to
`configs/ansible/roles/prometheus/files/rules/` (the unmodified
multi-host set in `deploy/monitoring/rules/`) and decommission
this directory.
