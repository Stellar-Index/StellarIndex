---
title: SLA proof procedure (Task #77)
last_verified: 2026-09-09
status: ratified
related:
  - test/load/scenarios/06-mixed-realistic.js
  - scripts/ci/render-sla-proof.sh
  - docs/architecture/k6-load-tests-design-note.md §"How the proof report (#77) is generated"
  - docs/architecture/launch-readiness-backlog.md L5.* / L6.*
---

# SLA proof procedure

Step-by-step recipe for producing the **monthly + pre-launch SLA
proof report** that Task #77 in the launch-readiness backlog
demands. The output of this procedure is a checked-in markdown
file at `docs/operations/sla-proof-<YYYY-MM-DD>.md` with PASS /
FAIL against each SLO threshold from
[ADR-0009 `multi-window SLO`](../adr/0009-latency-budget.md).

> **The report is generated, not written** (#378, 2026-09-09).
> [`scripts/ci/render-sla-proof.sh`](../../scripts/ci/render-sla-proof.sh)
> renders it from the k6 `--summary-export`, and
> [`k6-weekly.yml`](../../.github/workflows/k6-weekly.yml) runs the
> renderer and commits the result every Sunday. There is nothing to
> transcribe by hand, and hand-editing a generated report is how a
> number ends up in the tree that no export backs.
>
> **One thing is blocked on an operator and nothing else is:** the
> repo secrets `K6_TARGET_STAGING` + `STELLARINDEX_LOAD_API_KEY`
> are unset, so there is no target to measure. Until they exist the
> weekly run correctly goes red and files an `sla-evidence` issue.
> See [§Blocked on an operator](#blocked-on-an-operator).

The k6 scenarios themselves are
[Task #74](../architecture/launch-readiness-backlog.md) and live at
[`test/load/scenarios/`](../../test/load/scenarios/). The design
note backing the scenarios is
[`docs/architecture/k6-load-tests-design-note.md`](../architecture/k6-load-tests-design-note.md).

## What we're proving

Per ADR-0009 (the service SLA targets):

| Metric | Target | Source of truth |
| --- | --- | --- |
| `/v1/price` p95 | ≤ 200 ms | `06-mixed-realistic.js` `http_req_duration{endpoint="price"}` |
| `/v1/price` p99 | ≤ 500 ms | same, p99 |
| Error rate (5xx + non-2xx) | < 0.1 % over 10 min | k6 `http_req_failed` rate |
| Sustained load | 300 rps for 10 min uninterrupted | scenario soak window |
| Concurrent ingest active | yes | grafana panel "indexer ledgers/min" non-zero |

A run that fails any of these is **not** a valid SLA proof; the
operator either reruns after fixing the regression or files the
gap into the launch-readiness backlog before relaunching the
proof.

## Pre-flight checklist

Before kicking off the run:

- [ ] Staging stack is the **same configuration shape as
      production** — same Patroni / HAProxy / Redis-Sentinel
      ansible roles applied (`make ansible-staging-apply` if
      drift suspected).
- [ ] Indexer is actively ingesting (`/v1/readyz` shows
      `indexer.lag_seconds < 60`).
- [ ] Aggregator is publishing into the live serving cache —
      `/v1/price?asset=native&quote=fiat:USD` returns a fresh
      timestamp (within last 60 s).
- [ ] Status page shows operational; no active incident.
- [ ] No coincident chaos drill, deploy, or maintenance window
      scheduled in the run window (the proof is meaningless if
      the stack is being perturbed).

## Run

Normally you do not run this by hand at all — `k6-weekly.yml`
does, every Sunday at 02:00 UTC, and commits the report. Run it
manually only for an off-cadence proof (a pre-launch or
post-incident one). Two ways, and they produce the same file:

**Dispatch the workflow** (preferred — it already knows the
provenance):

```sh
gh workflow run k6-weekly.yml
# optionally: -f scenario=06-mixed-realistic.js
```

**Or locally**, which is the only path when the run must happen
from somewhere the runner cannot reach:

```sh
export K6_TARGET=https://api.staging.stellarindex.io/v1
export STELLARINDEX_LOAD_API_KEY="<paste from vault — load-test key, not a production key>"

started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# The canonical proof run (~13 min total: 30s ramp + 2m ramp +
# 10m soak + 30s drain). --summary-trend-stats is NOT optional:
# k6 omits p(99) from its default export, and ADR-0009 claims
# both p95 ≤ 200 ms AND p99 ≤ 500 ms, so an export taken without
# it produces evidence for half the claim. The renderer refuses
# such an export rather than emitting a report with "n/a" in it.
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

The renderer writes
`docs/operations/sla-proof-<end-date>.md` and prints the path.
Commit it; that committed file is the deliverable, and it is the
only thing
[`check-sla-evidence.sh`](../../scripts/ci/check-sla-evidence.sh)
counts as evidence.

`make test-load-mixed` still exists and is the right target for
an exploratory run, but it takes no `--summary-export`, so its
output cannot become a proof report.

### What the renderer refuses

It exits 2 and writes **nothing** when the run cannot support a
labelled claim. Each refusal is a real failure mode, not a
formality:

| Refusal | Why |
| --- | --- |
| A provenance variable is unset | A number with no target, window, method, scenario, commit or instrument attached proves nothing. |
| The export has no `p(99)` | k6's default trend stats omit it; the report would read "n/a" for half of ADR-0009 while looking complete. |
| The export records 0 requests | A run that issued nothing measured nothing. |
| The export declares no thresholds | Nothing in it asserts a pass or a fail, so it is not the canonical proof run. |
| The export is absent, empty or unparseable | The scenario did not complete. |
| `SLA_PROOF_TARGET` carries `user:pass@` | The report is committed to a public repo and records the target verbatim. |

A **breached** threshold is not a refusal. It renders (exit 1)
with `Verdict: FAIL`, because a failing proof is a real
measurement and is exactly the report an operator needs to see.
The run goes red; the document is retained.

## Optional enrichment — BLOCKED, and not required

Two capture steps in the pre-#378 version of this procedure
cannot be performed today, and neither is load-bearing for the
report:

- **Grafana snapshots.** The procedure directed the operator at
  `grafana.staging.stellarindex.io/d/load-proof/…`. **That host
  does not exist.** No load-proof dashboard is provisioned
  anywhere in `deploy/monitoring/`.
- **Promql reads over `k6_*` series.** Those series only exist
  when k6 streams to a remote-write sink, i.e. when
  `K6_PROMETHEUS_RW_SERVER_URL` is set. It is unset, and k6
  defaults it to the runner's own localhost — so passing
  `--out experimental-prometheus-rw` without it silently discards
  the run.

Both would have narrowed the percentiles to the **soak window**
alone. The generated report is a **whole-run** aggregate instead
and says so in its own "does not prove" section; that biases the
numbers conservative (the ramp is the least-warm part of the
run), so the claim is not overstated by the difference. If a sink
is ever provisioned, the soak-window narrowing is the upgrade to
make.

## Cadence

| Trigger | Required |
| --- | --- |
| Pre-launch (Task #77 closure) | yes — first proof; bumps L5.* / L6.* status |
| Monthly | yes — drift detection per design note §"Where this runs" |
| After any major release (semver minor or major) | yes |
| After any infra change touching API / storage / cache layer | yes |
| Post-incident | yes if the incident root cause was capacity-shaped |

A failing run **does not** invalidate the previous month's
proof; the previous proof remains the most-recent passing one.
But a failing run **does** require either a fix or an explicit
"why we're shipping anyway" annotation in the launch-readiness
backlog.

## Automation (`k6-weekly`)

[`.github/workflows/k6-weekly.yml`](../../.github/workflows/k6-weekly.yml)
is the scheduled half of this procedure. What it does:

- **Scenario compile-check** (`make test-load-check`) runs as its own
  job on every trigger, including a `pull_request` touching
  `test/load/**`. It needs no target and no secrets. Making it
  mandatory first required fixing the scenarios: `04-batch.js` and
  `06-mixed-realistic.js` merged headers with object spread, which the
  pinned k6 (0.50.0, babel) cannot parse, so the canonical proof
  scenario had never compiled — invisible for as long as the gate sat
  behind secrets that do not exist. Header merges now use
  `Object.assign`.
- **The Sunday 02:00 UTC run** consults
  [`scripts/ci/check-sla-evidence.sh`](../../scripts/ci/check-sla-evidence.sh)
  first. No target secrets → nothing runs (rc 1). No
  `sla-proof-<YYYY-MM-DD>.md` inside the 45-day window in
  `docs/operations/` → the run still executes, but the feed is
  incomplete (rc 2). Either verdict opens (or comments on) an
  `sla-evidence` tracking issue **and fails the run**; the issue
  closes itself once a run executes against a configured target and a
  dated proof report is inside the window.
- **It renders and commits the report itself.**
  [`scripts/ci/render-sla-proof.sh`](../../scripts/ci/render-sla-proof.sh)
  turns the export into `docs/operations/sla-proof-<end-date>.md`, and
  the run commits it through the contents API with the job token (no
  credentialed checkout; every checkout in that workflow stays
  `persist-credentials: false`). Failing to retain a measurement the run
  actually took is itself a failure and reddens the run.
- **The verdict is recomputed after the report lands.** Taken before, a
  first healthy run would report "no proof inside the window" for the
  absence of the file it had just written, and the feed would need two
  weeks to call itself healthy once.
- **A breached threshold is retained, not discarded.** k6 exits 99 on a
  breach; the run step publishes that code instead of aborting, so the
  breaching run still renders its `Verdict: FAIL` report. The run then
  goes red on the code. Aborting at the k6 step would have meant the one
  run that mattered most left nothing behind.
- **The two jobs do not gate each other.** The evidence run carries no
  `needs:` on the compile job, and its issue/fail steps run with
  `always()`. Both are deliberate: a scenario syntax error, or a k6 run
  that dies mid-soak, must not skip the verdict and leave the operator
  looking at a parse error instead of at the missing target.

Until 2026-08-29 (#316) an unconfigured target printed a `::notice::`
and exited **green**, so every scheduled run since the cron was
restored reported success with `Install k6` / `Compile-check` / `Run
scenario` all skipped — and no `sla-proof-<date>.md` has ever landed.
A green badge for an alarm that has never measured anything is worse
than no badge.

The 2026-09-09 pass (#378) closed the other half of that: the run could
measure, but it had no implementation of the promotion step — the
instruction was to visit a Grafana host that does not exist — so even a
successful run would have left nothing durable behind.

The workflow deliberately does **not** point at production:
`test/load/scenarios/lib/env.js` refuses production hosts, and
mixed-realistic is a 300 rps × 10 min soak against a single
production host.

## Blocked on an operator

Exactly two repo secrets, and nothing else:

| Secret | State | Consequence |
| --- | --- | --- |
| `K6_TARGET_STAGING` | **unset** | No target. Nothing measures the claim; the weekly run goes red and files an `sla-evidence` issue. |
| `STELLARINDEX_LOAD_API_KEY` | **unset** | Same; the scenarios refuse to start without a key. |

Two further secrets are genuinely optional and their absence is
handled, not silently swallowed:

| Secret | State | Consequence |
| --- | --- | --- |
| `K6_PROMETHEUS_RW_SERVER_URL` | unset | No remote-write sink. The run warns and relies on the summary export, which is the primary artefact anyway. |
| `ALERTMANAGER_URL_STAGING` | unset | Only the spike scenario uses it; the weekly runs mixed-realistic. |

**Before minting `K6_TARGET_STAGING`:** its value is recorded
**verbatim** in the proof report, which is committed to a public
repo. That is deliberate — a proof that will not say what it
measured proves nothing — so choose a hostname that is fine to
publish. The credential is never recorded; it travels only in
`STELLARINDEX_LOAD_API_KEY`, and the renderer refuses a target URL
carrying `user:pass@`.

The target must be **production-shaped and not production**:
`test/load/scenarios/lib/env.js` refuses production hosts at
scenario-init time, and mixed-realistic is a 300 rps × 10 min soak.
Retiring the workflow is the other legitimate way to close the
`sla-evidence` issue — but it means accepting no load-at-volume and
no edge-path evidence at all, since `stellarindex-sla-probe`
measures `localhost:3000` at concurrency 1, inside the box, past
Caddy, TLS and DNS.

## What if staging isn't available?

If staging access is delayed (vendor / DNS / paperwork), the
launch-readiness backlog L5.4 already documents the
"documented-acceptance" path: the mixed-realistic scenario
covers the ingest-side soak via its own metrics, and a
synthetic proof run against the local docker-compose dev stack
produces directional numbers (lower fidelity, useful as a
sanity check). Document the fallback path in the proof report's
"Caveats" section so the reader knows the run wasn't against
production-shape infra.

## References

- Scenario:
  [`test/load/scenarios/06-mixed-realistic.js`](../../test/load/scenarios/06-mixed-realistic.js)
- Design note:
  [`docs/architecture/k6-load-tests-design-note.md`](../architecture/k6-load-tests-design-note.md)
- Report generator (the source of truth for the report's shape):
  [`scripts/ci/render-sla-proof.sh`](../../scripts/ci/render-sla-proof.sh)
- Report generator self-test:
  [`scripts/ci/render-sla-proof-test.sh`](../../scripts/ci/render-sla-proof-test.sh)
- Report template (historical; superseded by the generator):
  [`sla-proof-template.md`](sla-proof-template.md)
- ADR-0009 multi-window SLO:
  [`../adr/0009-latency-budget.md`](../adr/0009-latency-budget.md)
- Reports directory README:
  [`../../test/load/reports/README.md`](../../test/load/reports/README.md)
