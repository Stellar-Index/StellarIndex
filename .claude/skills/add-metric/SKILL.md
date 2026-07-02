---
name: add-metric
description: Add a Prometheus metric, alert, and runbook to Stellar Index — the paired counter/histogram pattern, BOTH rule trees, the doc-code link chain, and the guards that fail CI when any piece is missing. Use when instrumenting a worker, adding an alert, or when a metric/alert/runbook lint fails.
---

# /add-metric

Canonical checklist: `docs/contributing/add-metric.md` + the recipe
in CLAUDE.md. This skill is the execution order + the full guard
chain (five different lints care about this change).

## 1. The metric

- Declare the `*Vec` in `internal/obs/metrics.go`; register in
  `registerAppMetrics()`/`registerAppMetricsTail()` (NOT `init()` —
  funlen split).
- Worker IO pattern is PAIRED: `FooTotal{outcome}` counter +
  `FooDurationSeconds{outcome}` histogram (copy
  `DivergenceRefreshTotal`); warm every outcome label at register
  time so scrapes show the series.
- Naming: `stellarindex_<subsystem>_<noun>_<unit>`.
- Textfile-collector metrics (ops subcommands / cron scripts) skip
  obs and write atomically (temp+rename) to
  `/var/lib/node_exporter/textfile_collector/` — copy
  `data-freshness.sh` or `verify-served-values`.

## 2. The alert (if warranted)

- Add the rule to **BOTH trees**: `deploy/monitoring/rules/<area>.yml`
  AND `configs/prometheus/rules.r1/<area>.yml` (job labels:
  underscored vs hyphenated). The semantic-equivalence differ fails
  CI if they diverge; intentional host-shape differences go in
  `scripts/ci/rule-equivalence.baseline` (growth needs a
  `Baseline-Growth:` trailer).
- Every alert needs `runbook_url` + a row in
  `docs/operations/alerts-catalog.md`.

## 3. The runbook

`docs/operations/runbooks/<alert-name>.md` with the template
sections (At a glance / description / when / user impact /
investigate / mitigate / escalate / post-mortem notes / Related) —
the template-presence lint fails on missing sections, the orphan
lint fails if nothing links to it, and the metric-freshness lint
fails if it cites a metric with no emitter.

## 4. Tests + docs

- Regression test with `obstest.HistogramSampleCount` for per-label
  histogram children; add the metric to
  `internal/obs/metrics_test.go`'s expected-scrape list.
- `make docs-metrics` (obs metrics) — document WHEN TO LOOK AT IT,
  not what it counts.

## 5. The guard chain (run all)

```sh
go test ./internal/obs/
make monitoring-check          # promtool both trees + dead-ref + equivalence
bash scripts/ci/lint-docs.sh   # catalog row, runbook sections, orphans, metric refs
```

Probe the alert once if feasible: fire the condition (or promtool
unit test), see it evaluate. An alert that has never evaluated true
is decorative (the F-1329 dead-alert class).

Finish with **/verify-done**.
