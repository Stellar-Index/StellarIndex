# W14 — Observability, metrics, alerts, SLA

## Scope

Every metric. Every Prometheus rule (multi-host + R1 overlay).
Every Alertmanager route. Every runbook. SLA probe contract.

## Inputs

- `internal/obs/metrics.go` (canonical metric registration)
- `deploy/monitoring/rules/*.yml` (multi-host rule set)
- `configs/prometheus/rules.r1/*.yml` (R1 overlay)
- `configs/alertmanager/alertmanager.r1.yml`
- `configs/prometheus/prometheus.r1.yml`
- `configs/healthchecks/`
- `docs/operations/alerts-catalog.md`
- `docs/operations/runbooks/*.md`
- `cmd/ratesengine-sla-probe/`

## Checks (per-alert loop §10)

| Check | Result | Evidence |
| --- | --- | --- |
| 1. Expression provability (metric name exists) | | |
| 2. Threshold defensibility (ADR/SLA/runbook origin) | | |
| 3. Severity tier (Alertmanager route maps) | | |
| 4. Runbook link annotation | | |
| 5. Runbook content (diagnosis, dashboard, escalation) | | |
| 6. Firing test (ever fired in r1?) | | |
| 7. Multi-host ↔ R1 overlay pairing | | |

## Catalogue checks

| # | Check | Method |
| --- | --- | --- |
| W14.1 | Every metric in `metrics.go` is documented in `docs/reference/metrics/README.md` | per-metric |
| W14.2 | Every alert rule references a metric that exists | grep |
| W14.3 | Every alert has runbook_url annotation pointing to a real file | per-alert |
| W14.4 | Every runbook describes diagnosis + dashboard + escalation + postmortem template | per-runbook |
| W14.5 | Alertmanager route tree: severity-routing for page / ticket / informational | config audit |
| W14.6 | Deadmansswitch heartbeat configured + receiver healthy | config + runbook |
| W14.7 | SLA probe writes textfile collector format; node_exporter scrapes; rules.r1 fires on breach | end-to-end |
| W14.8 | NEW: customer_webhook_delivery metric + alert + runbook (W32) | per-source check |
| W14.9 | NEW: aggregator_supply_refresh metric + alert + runbook (wave 90) | per-source check |
| W14.10 | NEW: divergence_refresh metric + alert + runbook (wave 89) | per-source check |
| W14.11 | NEW: anomaly_freeze_recovery_sweep metric + alert + runbook (wave 91) | per-source check |
| W14.12 | Loki: log retention + scrape rate per host | config audit |
| W14.13 | Promtail: every binary's logs are scraped | config audit |
| W14.14 | NEW: soroban_events drop metric — does one exist? Should it? | gap inspection |
| W14.15 | r1 probe: scrape success on every job; no `lastError`; no rule eval errors | R1-P16 |
| W14.16 | r1 probe: active alerts state | R1-P16 |

## Closure criteria

Every alert + runbook + metric in catalogue terminal. Findings on:
- any metric named in an alert that doesn't exist
- any alert without a runbook
- any runbook citing a dashboard URL that doesn't resolve
- any "dead" alert (never fired in principle, never fired in
  practice — could be a misconfigured threshold)
