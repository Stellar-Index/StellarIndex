---
title: Add a Prometheus metric
last_verified: 2026-09-03
status: living doc
---

# Add a Prometheus metric
Canonical checklist: `docs/contributing/add-metric.md` + the recipe
in AGENTS.md. This skill is the execution order + the full guard
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
- **A new textfile producer must be added to
  `scripts/ci/textfile-producers.manifest`** (path, the `.prom` it
  writes, and the self-test that drives its shipped bytes, or `-`).
  `scripts/ci/lint-textfile-exposition.sh` reconciles that list against
  the tree and sweeps every producer in it.
- **Every value a producer writes must be a number by construction.**
  node_exporter parses each `.prom` whole: one unparseable line and it
  rejects the entire file, so a single bad value costs every unrelated
  family that shares it (r1 2026-09-10 — 127 `stellarindex_timescale_*`
  series went dark because one field held the word `SET`). A value
  captured from an external command is *not* a number until it has been
  checked; `${x:-0}` does not check it. Guard it (`is_num` in the
  TimescaleDB probe, the digits-only `case` in `data-freshness.sh`,
  `[[ "$live" =~ ^[0-9]+$ ]]` in `galexie-archive-tip-lag.sh`), omit the
  sample when it fails, and say so through a health gauge.
- **Never put two statements in one command string handed to a
  tuples-only SQL client.** `psql -At` prints a command tag for every
  statement returning no rows and does not suppress it, so
  `SET x; SELECT y` puts the word `SET` on the stream you are parsing.
  Session settings go in `PGOPTIONS`. The gate above fails CI on this.

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

Finish with **docs/contributing/procedures/verify-done.md**.
