---
title: SLA proof procedure (Task #77)
last_verified: 2026-09-25
status: ratified
related:
  - scripts/ops/sla-proof-from-probe.sh
  - scripts/ci/render-sla-proof.sh
  - test/load/scenarios/06-mixed-realistic.js
  - docs/architecture/ha-plan.md §7.3
---

# SLA proof procedure

Produces the checked-in `docs/operations/sla-proof-<YYYY-MM-DD>.md`, with PASS / FAIL
against each SLO threshold in [ADR-0009](../adr/0009-latency-budget.md).

> **The report is generated, never hand-written or hand-edited.** Two generators write the
> same file from different measurements; the `generator:` front-matter key and the
> Provenance table say which.
>
> | | Weekly, automatic | On demand |
> | --- | --- | --- |
> | Generator | [`scripts/ops/sla-proof-from-probe.sh`](../../scripts/ops/sla-proof-from-probe.sh) | [`scripts/ci/render-sla-proof.sh`](../../scripts/ci/render-sla-proof.sh) |
> | Workflow | [`sla-proof-weekly.yml`](../../.github/workflows/sla-proof-weekly.yml) | [`k6-weekly.yml`](../../.github/workflows/k6-weekly.yml) (dispatch) |
> | Measures | served latency, from the SLA probe on the API host | client-observed latency under synthetic concurrency |
> | Needs | ssh path to the probe host (provisioned) | a production-shaped load target (**does not exist**) |
>
> Nothing blocks the weekly proof; only the load-at-volume half is blocked. See
> [§Blocked on an operator](#blocked-on-an-operator).

Scenarios: [`test/load/scenarios/`](../../test/load/scenarios/); design in
[`ha-plan.md` §7.3](../architecture/ha-plan.md#73-load-testing-k6).

## What we're proving

| Metric | Target | Probe aggregate (weekly) | k6 load run (dispatch) |
| --- | --- | --- | --- |
| p95 | ≤ 200 ms | largest per-run `stellarindex_sla_probe_latency_ms{quantile="0.95"}` in the window, per endpoint | `06-mixed-realistic.js` `http_req_duration` p95 |
| p99 | ≤ 500 ms | same at `quantile="0.99"` | same, p99 |
| Availability | ≥ 99.9 % | `stellarindex_sla_probe_availability_pct`, each run weighted by the time its result stood | - |
| Error rate (5xx + non-2xx) | < 0.1 % over 10 min | - | k6 `http_req_failed` rate |
| Sustained load | 300 rps for 10 min uninterrupted | **not exercised** (probe concurrency 1) | scenario soak window |
| Freshness | ≤ 30 s (`/price/tip`) | `stellarindex_sla_probe_freshness_sec`, reported not gated | - |

The probe exports one p50/p95/p99 per run, never samples, so a pooled percentile is not
recoverable. The generator publishes the **largest per-run percentile**, a true upper bound on
the pooled one. `PROVEN` means the pooled percentile cannot have exceeded target; `NOT PROVEN`
means the bound cannot settle it, **not** that the target was missed. ADR-0009's enforcement
clause (a load test at volume) is what the blank probe cells leave unevidenced.

- Availability is a window ratio weighted by wall-clock time, not request count (an outage collapses the count).
- Freshness carries no verdict: the alert in
  [`deploy/monitoring/rules/sla-probe.yml`](../../deploy/monitoring/rules/sla-probe.yml) needs a
  30-minute sustain because one over-bound run is in-contract (`computeTip` falls back to the
  closed bucket, 60-120 s `observed_at`; ADR-0018).
- Nothing is rolled up across endpoints; the verdict is the conjunction of per-endpoint bounds.

A failing run is not a valid proof: rerun after the fix, or file the gap in the
launch-readiness backlog first.

## Pre-flight checklist (k6 load run only)

- [ ] Staging matches production shape (roles `configs/ansible/roles/{patroni,haproxy,redis-sentinel}`).
      No `make ansible-staging-apply` target exists; diff roles against the staging host by hand.
- [ ] Indexer ingesting: `/v1/readyz` `indexer.lag_seconds < 60`.
- [ ] `/v1/price?asset=native&quote=fiat:USD` returns a timestamp within 60 s.
- [ ] Status page operational, no active incident.
- [ ] No chaos drill, deploy or maintenance window in the run window.

## Run

Normally [`sla-proof-weekly.yml`](../../.github/workflows/sla-proof-weekly.yml) does it every
Sunday 02:00 UTC and commits the report, pruning all but the newest four dated reports in
the same commit. Run by hand only for an off-cadence proof (pre-launch, post-incident).

### The weekly proof, off-cadence

Preferred: dispatch (it reads the probe's live config):

```sh
gh workflow run sla-proof-weekly.yml
# optionally: -f window=14d   (clamped to what Prometheus retains)
```

Or from the probe host (Prometheus on loopback):

```sh
SLA_PROOF_PROBE_TARGET=http://localhost:3000/v1 \
  SLA_PROOF_PROBE_CONCURRENCY=1 \
  SLA_PROOF_COMMIT="$(git rev-parse HEAD)" \
  scripts/ops/sla-proof-from-probe.sh \
    --prom-url http://127.0.0.1:9090 \
    --window 7d \
    --extract-out sla-probe-extract.json
```

The provenance values must match the probe's real config. Read them off the host (empty
output means no override; defaults are in `configs/healthchecks/sla-probe.sh`):

```sh
sed -n 's/^SLA_PROBE_BASE_URL=//p'   /etc/default/stellarindex-healthchecks
sed -n 's/^SLA_PROBE_CONCURRENCY=//p' /etc/default/stellarindex-healthchecks
```

`--extract-out` saves every query result and its promql; `--extract <file>` re-renders the
identical numbers offline. From anywhere else, tunnel first (Prometheus binds loopback only):

```sh
ssh -N -L 19090:127.0.0.1:9090 <probe-host> &
# ...then --prom-url http://127.0.0.1:19090
```

### The load-at-volume proof (k6)

No target exists yet. When one does:

```sh
gh workflow run k6-weekly.yml
# optionally: -f scenario=06-mixed-realistic.js
```

Or locally, the only path when the runner cannot reach the target:

```sh
export K6_TARGET=https://api.staging.stellarindex.io/v1
export STELLARINDEX_LOAD_API_KEY="<paste from vault — load-test key, not a production key>"

started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# Canonical proof run, ~13 min. --summary-trend-stats is NOT optional: k6 omits
# p(99) by default and the renderer refuses an export without it.
k6 run \
  --summary-export summary.json \
  --summary-trend-stats 'avg,min,med,max,p(90),p(95),p(99)' \
  test/load/scenarios/06-mixed-realistic.js
ended_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

SLA_PROOF_TARGET="$K6_TARGET" \
SLA_PROOF_SCENARIO=test/load/scenarios/06-mixed-realistic.js \
SLA_PROOF_K6_VERSION="$(k6 version)" \
SLA_PROOF_COMMIT="$(git rev-parse HEAD)" \
SLA_PROOF_STARTED_AT="$started_at" \
SLA_PROOF_ENDED_AT="$ended_at" \
  scripts/ci/render-sla-proof.sh --summary summary.json
```

The renderer writes `docs/operations/sla-proof-<end-date>.md` and prints the path. Commit
it: that file is the deliverable and the only thing
[`check-sla-evidence.sh`](../../scripts/ci/check-sla-evidence.sh) counts as evidence.
`make test-load SCENARIO=mixed` is exploratory only (cannot become a proof report).

### What the renderers refuse (exit 2, write nothing)

Both: a provenance variable unset (`SLA_PROOF_TARGET`, or `SLA_PROOF_PROBE_TARGET` /
`SLA_PROOF_PROBE_CONCURRENCY` / `SLA_PROOF_COMMIT`); a target carrying `user:pass@` (the
report is committed to a public repo and records it verbatim); nothing measured.

- k6: no `p(99)` in the export; 0 requests; no declared thresholds; export absent, empty or unparseable.
- Probe: Prometheus unreachable or rejecting a query; a headline series (p95, p99, availability,
  sample count) absent; series spanning more than one host (pass `--host <name>`); retained
  window under `--min-window` (default 24h; Prometheus retention is 15 days).

A **breached** threshold is not a refusal: it renders (exit 1) with `Verdict: FAIL` and the
document is retained. A probe window with **holes** renders as `Verdict: NOT PROVEN`: a gap in
`stellarindex_sla_probe_run_duration_seconds` means scraping stopped; a frozen
`last_pass_timestamp` means the probe stopped while the textfile collector kept re-serving its
last output. An **unevaluable headline cell** renders `n/a`, is named in the report and keeps
the verdict `NOT PROVEN` (exit 1).

## Where the numbers come from

No Grafana-snapshot or `k6_*` promql capture exists; `k6_*` series need a remote-write sink
whose secret is unset (without it `--out experimental-prometheus-rw` silently discards the
run). The weekly generator saves the **extract** as a run artifact. k6 percentiles are
**whole-run**, not soak-window, which biases them conservative; the report says so.

## Cadence

| Trigger | Required | Who |
| --- | --- | --- |
| Weekly | yes, drift detection | `sla-proof-weekly.yml`, automatic |
| Pre-launch (Task #77 closure) | yes, first proof; bumps L5.* / L6.* status | dispatch |
| After any minor or major release | yes | dispatch, after the deploy settles |
| After infra change touching API / storage / cache | yes | dispatch |
| Post-incident | if root cause was capacity-shaped | dispatch |

The staleness threshold in [`check-sla-evidence.sh`](../../scripts/ci/check-sla-evidence.sh) is
still 45 days (from the monthly cadence); read it as "the feed has not been dead for 45 days".
A failing run does not invalidate the previous week's proof, but needs a fix or an explicit
"why we're shipping anyway" annotation in the launch-readiness backlog.

## Automation

### `sla-proof-weekly`: the scheduled producer

[`.github/workflows/sla-proof-weekly.yml`](../../.github/workflows/sla-proof-weekly.yml),
Sundays 02:00 UTC.

- Uses `DEPLOY_SSH_PRIVATE_KEY`, `R1_HOST`, `R1_SSH_KNOWN_HOSTS` (shared with `deploy.yml` and
  `ansible-drift.yml`); read-only: one ssh reads two config values, one port-forward carries
  Prometheus queries.
- Consults the decision core with `SLA_EVIDENCE_SOURCE=probe`, so the absent k6 target cannot turn it red.
- Renders and commits the report itself, pruning all but the newest four in the same commit
  (Git Data API, job token). Failing to retain a measurement is a failure.
- Recomputes the verdict after the report lands. A `NOT PROVEN` week is committed first, then the
  run goes red; the `sla-evidence` tracking issue still closes (it tracks whether the feed
  produces evidence, not whether the SLA held).
- Declares `scheduled-control: reports-by-failing`: a red run is usually the control working.

### `k6-weekly`: the retained load capability

[`.github/workflows/k6-weekly.yml`](../../.github/workflows/k6-weekly.yml) has **no
`schedule:` trigger** (a control that cannot run is not a control). It keeps:

- **Scenario compile-check** (`make test-load-check`): own job on every trigger, including
  `pull_request` touching `test/load/**`. Header merges use `Object.assign` because the pinned
  k6 (0.50.0) cannot parse object spread.
- **Load run on `workflow_dispatch`**: first consults
  [`check-sla-evidence.sh`](../../scripts/ci/check-sla-evidence.sh) with `SLA_EVIDENCE_SOURCE=k6`.
  No target secrets: nothing runs (rc 1). No `sla-proof-<YYYY-MM-DD>.md` inside the 45-day
  window: runs, feed incomplete (rc 2).
- Renders, commits and prunes via [`render-sla-proof.sh`](../../scripts/ci/render-sla-proof.sh),
  then recomputes the verdict. k6 exits 99 on breach; the step publishes the code so the
  `Verdict: FAIL` report renders, then the run goes red.
- The two jobs do not gate each other (no `needs:`; issue/fail steps `always()`).

It deliberately does not target production: `test/load/scenarios/lib/env.js` refuses production
hosts, and mixed-realistic is a 300 rps x 10 min soak.

## Blocked on an operator

Nothing blocks the weekly proof. Blocked is the load-at-volume half:

| Secret | State | Consequence |
| --- | --- | --- |
| `K6_TARGET_STAGING` | **unset** | No load target; the weekly report names the gap; `k6-weekly.yml` stays dispatch-only. |
| `STELLARINDEX_LOAD_API_KEY` | set | Scenarios refuse to start without a key. |
| `K6_PROMETHEUS_RW_SERVER_URL` | unset (optional) | No remote-write sink; the summary export is the primary artefact. |
| `ALERTMANAGER_URL_STAGING` | unset (optional) | Only the spike scenario uses it. |

**Before minting `K6_TARGET_STAGING`:** its value is recorded verbatim in the committed, public
report, so pick a publishable hostname. The credential travels only in `STELLARINDEX_LOAD_API_KEY`.
The target must be production-shaped and not production.

Do not retire the k6 scenarios: they cost nothing while waiting and are the only thing that can
measure load at volume or the edge path (the probe reads `localhost:3000` at concurrency 1,
past Caddy, TLS and DNS).

## If a load target never arrives

The weekly proof is unaffected. The 300 rps soak row in [§What we're proving](#what-were-proving)
stays unevidenced; the launch-readiness backlog L5.4 "documented-acceptance" path closes Task #77
without it. The gap belongs in the report's "does not prove" section (auto-written), never a blank cell.

## References

- Probe generator: [`scripts/ops/sla-proof-from-probe.sh`](../../scripts/ops/sla-proof-from-probe.sh), self-test
  [`sla-proof-from-probe-test.sh`](../../scripts/ops/sla-proof-from-probe-test.sh)
- Probe: [`cmd/stellarindex-sla-probe/main.go`](../../cmd/stellarindex-sla-probe/main.go),
  alerts [`sla-probe.yml`](../../deploy/monitoring/rules/sla-probe.yml)
- Report generator (k6): [`render-sla-proof.sh`](../../scripts/ci/render-sla-proof.sh), self-test
  [`render-sla-proof-test.sh`](../../scripts/ci/render-sla-proof-test.sh)
- Scenario [`06-mixed-realistic.js`](../../test/load/scenarios/06-mixed-realistic.js);
  [ADR-0009](../adr/0009-latency-budget.md); [`test/load/reports/README.md`](../../test/load/reports/README.md)
