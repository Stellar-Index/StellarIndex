# `test/load/reports/` — k6 run artefacts

Where the k6 load suite lands its output. Two different kinds of thing
live here, and conflating them is how a run that measured nothing came
to publish an all-green artefact under its own name (#378).

## Layout

```
reports/
├── README.md            (this file)
├── 2026-06-13/          CHECKED IN — historical evidence, kept on purpose
│   └── 00-acceptance.json     the AC2 acceptance export (p95 54.4 ms)
└── run-<run_id>/        gitignored — output of one k6-weekly run
    └── summary.json           `k6 run --summary-export`
```

Dated directories (`2026-*/` …) are **deliberately checked in** —
`.gitignore` un-ignores them — because they are the historical record the
current numbers are compared against. Everything else is gitignored.

**Consequence, and the reason this section exists:** this directory is
never empty, so a workflow step that uploads it *wholesale* publishes the
checked-in June export no matter what the run did. `k6-weekly.yml`
therefore writes to, and uploads, only `run-<run_id>/`.

## How a run becomes evidence

1. `k6-weekly.yml` writes `run-<run_id>/summary.json` via
   `--summary-export`, with `--summary-trend-stats` explicitly including
   `p(99)` — k6 does **not** export p99 by default, and ADR-0009 promises
   both p95 ≤ 200 ms *and* p99 ≤ 500 ms.
2. `scripts/ci/render-sla-proof.sh` renders that export into
   `docs/operations/sla-proof-<end-date>.md` — a dated report carrying
   its own provenance (target, run window, method, scenario, commit, k6
   version, and the sha256 of the export it was rendered from). It
   REFUSES to write anything when a headline metric or a provenance
   field is missing, so an unlabelled number cannot be published as a
   proof.
3. The workflow commits that report and uploads
   `run-<run_id>/` (summary + a copy of the report + the published
   commit sha) as `k6-summary-<run_id>`.

Step 2→3 is what makes it *durable*: a run summary and an artifact both
age out, a committed report does not. `scripts/ci/check-sla-evidence.sh`
measures the committed report, not the export — which is why the feed
could be "running" and still be producing no evidence.

Until 2026-09-09 (#378) step 3 was a manual instruction rather than an
implementation, and the procedure it pointed at directed the operator to
mint Grafana snapshots from a host that does not exist. No dated proof
report has ever landed on any branch.

## Not a durable store

Prometheus remote-write (`--out experimental-prometheus-rw`) is wired only
when `K6_PROMETHEUS_RW_SERVER_URL` is set. It is unset today, and k6
defaults that URL to the runner's own localhost, so it must never be
assumed to be the durable store. `summary.json` is the primary artefact.
