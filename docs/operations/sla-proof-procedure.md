---
title: SLA proof procedure (Task #77)
last_verified: 2026-09-15
status: ratified
related:
  - scripts/ops/sla-proof-from-probe.sh
  - scripts/ci/render-sla-proof.sh
  - test/load/scenarios/06-mixed-realistic.js
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

> **The report is generated, not written.** There is nothing to
> transcribe by hand, and hand-editing a generated report is how a
> number ends up in the tree that no export backs.
>
> **Two generators write the same file, from two different
> measurements.** Which one produced a given report is recorded in its
> own `generator:` front-matter key and in its Provenance table.
>
> | | Weekly, automatic | On demand |
> | --- | --- | --- |
> | Generator | [`scripts/ops/sla-proof-from-probe.sh`](../../scripts/ops/sla-proof-from-probe.sh) | [`scripts/ci/render-sla-proof.sh`](../../scripts/ci/render-sla-proof.sh) |
> | Workflow | [`sla-proof-weekly.yml`](../../.github/workflows/sla-proof-weekly.yml) | [`k6-weekly.yml`](../../.github/workflows/k6-weekly.yml) (dispatch) |
> | Measures | served latency, from the SLA probe already running on the API host | client-observed latency under synthetic concurrency |
> | Needs | an ssh path to the probe host — already provisioned | a production-shaped load target — **does not exist** |
>
> **Nothing blocks the weekly proof.** What is still blocked is the
> load-at-volume half: `K6_TARGET_STAGING` is unset, so the k6 run has
> nowhere to point. That is a real gap and the weekly report names it in
> its own "does not prove" section rather than papering over it. See
> [§Blocked on an operator](#blocked-on-an-operator).

The k6 scenarios themselves are
[Task #74](../architecture/launch-readiness-backlog.md) and live at
[`test/load/scenarios/`](../../test/load/scenarios/). The design
note backing the scenarios is
[`docs/architecture/k6-load-tests-design-note.md`](../architecture/k6-load-tests-design-note.md).

## What we're proving

Per ADR-0009 (the service SLA targets):

| Metric | Target | Probe aggregate (weekly) | k6 load run (on dispatch) |
| --- | --- | --- | --- |
| p95 | ≤ 200 ms | largest per-run `stellarindex_sla_probe_latency_ms{quantile="0.95"}` in the window, per endpoint | `06-mixed-realistic.js` `http_req_duration` p95 |
| p99 | ≤ 500 ms | same at `quantile="0.99"` | same, p99 |
| Availability | ≥ 99.9 % | `stellarindex_sla_probe_availability_pct`, weighted by each run's sample count | — |
| Error rate (5xx + non-2xx) | < 0.1 % over 10 min | — | k6 `http_req_failed` rate |
| Sustained load | 300 rps for 10 min uninterrupted | **not exercised** — the probe runs at concurrency 1 | scenario soak window |
| Freshness | ≤ 30 s (`/price/tip`) | `stellarindex_sla_probe_freshness_sec`, reported and not gated | — |

ADR-0009 states p95 ≤ 200 ms and p99 ≤ 500 ms **unconditionally** — the
target is not qualified by a request rate — so the probe aggregate does
test the stated number. What it does not exercise is ADR-0009's
*enforcement* clause, which calls for a load test at volume, and that row
is blank in the probe column on purpose rather than quietly filled from a
measurement that cannot support it.

### Percentiles do not average, and the weekly figure says which one it is

The probe exports one p50/p95/p99 **per run** and never the underlying
samples, so the request-level percentile for a week is not recoverable
from these series. The generator does not compute one. What it publishes
is the **largest per-run percentile**, which is a true upper bound on the
pooled one: every run has at least 95 % of its samples at or below its
own p95, so pooling runs keeps at least 95 % at or below the largest of
those values — for any run sizes, with no uniformity assumed.

The bound runs one way, and reading it the other way is the mistake to
avoid. `PROVEN` means the pooled percentile cannot have exceeded the
target. `NOT PROVEN` means this bound cannot settle it, **not** that the
target was missed.

Availability gets the opposite treatment because it is a ratio rather
than a percentile: it is a window ratio weighted by each run's own sample
count. Gating it on the worst single 30-second run would publish `NOT
PROVEN` for a window the SLO was comfortably met in.

Freshness carries no verdict at all. The deployed bound in
[`deploy/monitoring/rules/sla-probe.yml`](../../deploy/monitoring/rules/sla-probe.yml)
requires a 30-minute sustain, because a single run over bound is
in-contract — `computeTip` escalates its window and falls back to the
closed bucket, so a pair with no recent trade legitimately serves a
60–120 s `observed_at` (ADR-0018). The report states the numbers and
points at the alert rather than inventing a stricter bar.

Nothing is rolled up across endpoints. There is no "overall p95" in the
document; the verdict is the conjunction of the per-endpoint bounds.

A run that fails any of these is **not** a valid SLA proof; the
operator either reruns after fixing the regression or files the
gap into the launch-readiness backlog before relaunching the
proof.

## Pre-flight checklist — the k6 load run only

The weekly probe aggregate has no pre-flight: it reads a window that has
already happened, and if the stack was perturbed during it the report
says so (coverage, holes in the series, how many API builds were live).
This checklist is for the k6 run, which perturbs the stack itself.

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

Normally you do not run this by hand at all —
[`sla-proof-weekly.yml`](../../.github/workflows/sla-proof-weekly.yml)
does, every Sunday at 02:00 UTC, and commits the report. Run something by
hand only for an off-cadence proof (a pre-launch or post-incident one).

### The weekly proof, off-cadence

**Dispatch the workflow** (preferred — it reads the probe's live
configuration and already knows the provenance):

```sh
gh workflow run sla-proof-weekly.yml
# optionally: -f window=14d   (clamped to what Prometheus retains)
```

**Or from the probe host**, where Prometheus is on loopback and no
port-forward is needed:

```sh
SLA_PROOF_PROBE_TARGET=http://localhost:3000/v1 \
  SLA_PROOF_PROBE_CONCURRENCY=1 \
  SLA_PROOF_COMMIT="$(git rev-parse HEAD)" \
  scripts/ops/sla-proof-from-probe.sh \
    --prom-url http://127.0.0.1:9090 \
    --window 7d \
    --extract-out sla-probe-extract.json
```

The two provenance values must describe the probe as it is actually
configured: the report derives its network-path caveat from the target
(a loopback target means DNS, TLS, the reverse proxy and any CDN are
outside the measurement) and names the concurrency as the load condition
that was not exercised. Read them off the host rather than copying them
from here:

```sh
sed -n 's/^SLA_PROBE_BASE_URL=//p'   /etc/default/stellarindex-healthchecks
sed -n 's/^SLA_PROBE_CONCURRENCY=//p' /etc/default/stellarindex-healthchecks
# Empty output means no override; the defaults are in
# configs/healthchecks/sla-probe.sh.
```

`--extract-out` saves every query result the report was rendered from,
including the promql. Re-rendering it with `--extract <file>` reproduces
every number with no network access at all, which is what makes a
committed report checkable later rather than merely readable.

**From anywhere else**, tunnel to the probe host's Prometheus first —
it binds loopback there and is not exposed:

```sh
ssh -N -L 19090:127.0.0.1:9090 <probe-host> &
# ...then --prom-url http://127.0.0.1:19090
```

### The load-at-volume proof (k6)

This is the half that has no target. When one exists, dispatch it:

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

### What the probe-aggregate generator refuses

Same contract, same exit codes, different failure modes — because it
reads a window of history rather than a single run:

| Refusal | Why |
| --- | --- |
| A provenance variable is unset | `SLA_PROOF_PROBE_TARGET`, `SLA_PROOF_PROBE_CONCURRENCY` and `SLA_PROOF_COMMIT` are not recoverable from the series, and two of the report's caveats are derived from them. |
| Prometheus is unreachable, or rejects a query | A half-read window must never be published as a whole one. |
| A headline series is absent from the window | A report missing p95, p99, availability or the sample count reads "n/a" for part of the SLA while looking complete. |
| Every endpoint records 0 samples per run | A probe that issued nothing measured nothing. |
| The series span more than one host | One table describing two deployments describes neither. Pass `--host <name>`. |
| The retained window is under `--min-window` (default 24h) | Retention on the probe host is Prometheus' 15-day default, not unbounded, and the unit has been restarted. Shortening the claim silently to fit the data is the mislabelling this refuses. |
| `SLA_PROOF_PROBE_TARGET` carries `user:pass@` | The report is committed to a public repo and records the target verbatim. |

A window with **holes** in it is not a refusal. It renders (exit 1) with
`Verdict: NOT PROVEN` and says which kind of hole it had — a gap in the
series means scraping stopped, while a frozen `last_pass_timestamp` means
the probe stopped and node_exporter's textfile collector kept re-serving
its last output, which leaves a perfectly continuous series and is
invisible unless asked for directly.

A single **unevaluable headline cell** is not a refusal either, and it
is not a pass. The family refusal above fires only when a series is
absent for every endpoint; one endpoint missing from one family — a
probe relabel, an endpoint that exports latency but not availability, a
zero denominator, a value that is not a finite number — leaves that
cell with no value. It renders `n/a` beside `NOT PROVEN`, the report
names the cell, the verdict is `NOT PROVEN` (exit 1) while any such
cell stands, and an endpoint that has a sample count but no bound at
all stays in the table with empty cells rather than vanishing from it.

## Where the numbers come from, and what is not in them

Two capture steps in the pre-#378 version of this procedure told the
operator to mint Grafana snapshots from `grafana.staging.stellarindex.io`
and to read promql over `k6_*` series. **Neither could be performed by
anyone.** That host has never existed and no load-proof dashboard is
provisioned anywhere in `deploy/monitoring/`; the `k6_*` series exist only
when k6 streams to a remote-write sink, and the sink secret is unset — k6
defaults it to the runner's own localhost, so passing
`--out experimental-prometheus-rw` without it silently discards the run.
Both instructions are removed rather than softened: a step nobody can
follow is not an optional enrichment, it is a procedure that does not
work.

What replaces them is not a screenshot. The weekly generator saves an
**extract** — every query result the report was rendered from, with the
promql of every query — and the run uploads it as an artifact.
`--extract <file>` re-renders the identical numbers offline, so a
committed report is checkable against its own inputs months later, which
is more than a snapshot ever offered.

The k6 percentiles remain **whole-run** aggregates rather than
soak-window ones, and the generated report says so in its own "does not
prove" section. That biases them conservative — the ramp is the least-warm
part of the run — so the claim is not overstated by the difference. If a
remote-write sink is ever provisioned, narrowing to the soak window is
the upgrade to make.

## Cadence

| Trigger | Required | Who does it |
| --- | --- | --- |
| Weekly | yes — drift detection | `sla-proof-weekly.yml`, automatically |
| Pre-launch (Task #77 closure) | yes — first proof; bumps L5.* / L6.* status | dispatch |
| After any major release (semver minor or major) | yes | dispatch, after the deploy settles |
| After any infra change touching API / storage / cache layer | yes | dispatch |
| Post-incident | yes if the incident root cause was capacity-shaped | dispatch |

The cadence is now weekly rather than monthly because the producer is a
scheduled workflow rather than an operator with a checklist. The
staleness threshold in
[`check-sla-evidence.sh`](../../scripts/ci/check-sla-evidence.sh) is still
45 days, which was chosen for the monthly cadence and now tolerates six
missed weekly runs. Tightening it is a decision for this document to make
and has not been made; until it is, read that leg as "the feed has not
been dead for a month and a half" rather than as "last Sunday ran".

A failing run **does not** invalidate the previous week's
proof; the previous proof remains the most-recent passing one.
But a failing run **does** require either a fix or an explicit
"why we're shipping anyway" annotation in the launch-readiness
backlog.

## Automation

### `sla-proof-weekly` — the scheduled producer

[`.github/workflows/sla-proof-weekly.yml`](../../.github/workflows/sla-proof-weekly.yml)
runs Sundays at 02:00 UTC and is what actually keeps the feed alive.

- **It needs nothing minted.** `DEPLOY_SSH_PRIVATE_KEY`, `R1_HOST` and
  `R1_SSH_KNOWN_HOSTS` already exist and are already used by `deploy.yml`
  and `ansible-drift.yml`. The run writes nothing to the host: one ssh
  reads two config values, one local port-forward carries read-only
  Prometheus queries.
- **It consults the decision core with `SLA_EVIDENCE_SOURCE=probe`**, so
  the absence of a k6 load target — which it does not use and will not
  have — cannot report it red.
- **It renders and commits the report itself**, through the contents API
  with the job token; the checkout stays `persist-credentials: false`, so
  no step in a job that already carries an ssh key also carries a push
  token. Failing to retain a measurement the run actually took is itself
  a failure and reddens the run.
- **The verdict is recomputed after the report lands.** Taken before, a
  first healthy run would report "no proof inside the window" for the
  absence of the file it had just written, and the feed would need two
  weeks to call itself healthy once.
- **A `NOT PROVEN` week is retained, not discarded.** The report is
  committed first and the run goes red second, because the run whose
  finding matters most must not be the one that leaves nothing behind.
  The `sla-evidence` tracking issue still closes on it: that issue tracks
  whether the feed produces evidence, not whether the SLA held.
- **It declares `scheduled-control: reports-by-failing`.** A red run here
  is usually the control working, so the scheduled-control sweep names it
  as a finding to read rather than as plumbing to repair. The declaration
  buys no quiet — still flagged, still counted, still failing
  `ci-health.yml`.
- **The probe's target and concurrency are read off the host**, not
  hard-coded, because the report derives two of its caveats from them.
  Where the host carries no override the documented default is used and
  the report says the value came from the default rather than from the
  host.

### `k6-weekly` — the retained load capability

[`.github/workflows/k6-weekly.yml`](../../.github/workflows/k6-weekly.yml)
**no longer carries a `schedule:` trigger.** A scheduled control that
cannot run is not a control, and this one needed a target that does not
exist. What it kept:

- **Scenario compile-check** (`make test-load-check`) runs as its own
  job on every trigger, including a `pull_request` touching
  `test/load/**`. It needs no target and no secrets. Making it
  mandatory first required fixing the scenarios: `04-batch.js` and
  `06-mixed-realistic.js` merged headers with object spread, which the
  pinned k6 (0.50.0, babel) cannot parse, so the canonical proof
  scenario had never compiled — invisible for as long as the gate sat
  behind secrets that do not exist. Header merges now use
  `Object.assign`.
- **The load run, on `workflow_dispatch`,** consults
  [`scripts/ci/check-sla-evidence.sh`](../../scripts/ci/check-sla-evidence.sh)
  first. No target secrets → nothing runs (rc 1). No
  `sla-proof-<YYYY-MM-DD>.md` inside the 45-day window in
  `docs/operations/` → the run still executes, but the feed is
  incomplete (rc 2). It asks with `SLA_EVIDENCE_SOURCE=k6`, so it is
  answered about the load target and not about the probe source, which
  would otherwise let a dispatch start k6 with nowhere to point.
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
scenario` all skipped — and no `sla-proof-<date>.md` had ever landed.
A green badge for an alarm that has never measured anything is worse
than no badge.

The 2026-09-09 pass (#378) closed the other half of that: the run could
measure, but it had no implementation of the promotion step — the
instruction was to visit a Grafana host that does not exist — so even a
successful run would have left nothing durable behind. It did not,
however, give the schedule anything to measure, which is why the cron was
retired in favour of the probe aggregate.

The workflow deliberately does **not** point at production:
`test/load/scenarios/lib/env.js` refuses production hosts, and
mixed-realistic is a 300 rps × 10 min soak against a single
production host. That refusal is the reason the weekly proof reads the
probe instead: measuring what the box already serves is not the same
evidence as load at volume, and the report says so rather than closing
the gap by assertion.

## Blocked on an operator

**Nothing blocks the weekly proof.** It runs on secrets that already
exist. What is blocked is the load-at-volume half, and only that:

| Secret | State | Consequence |
| --- | --- | --- |
| `K6_TARGET_STAGING` | **unset** | No load target. Nothing measures behaviour under concurrency; the weekly probe report names that gap in its own "does not prove" section, and `k6-weekly.yml` stays dispatch-only. |
| `STELLARINDEX_LOAD_API_KEY` | set | Would be used by a k6 run; the scenarios refuse to start without a key. |

Two further secrets are genuinely optional and their absence is
handled, not silently swallowed:

| Secret | State | Consequence |
| --- | --- | --- |
| `K6_PROMETHEUS_RW_SERVER_URL` | unset | No remote-write sink for a k6 run. It warns and relies on the summary export, which is the primary artefact anyway. |
| `ALERTMANAGER_URL_STAGING` | unset | Only the spike scenario uses it. |

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

Retiring the k6 scenarios entirely is **not** the other way out any more.
The weekly feed no longer depends on them, so they cost nothing while
they wait; deleting them would throw away the only thing in the tree that
can ever measure load at volume or the edge path, which the probe cannot
— it reads `localhost:3000` at concurrency 1, inside the box, past Caddy,
TLS and DNS.

## What if a load target never arrives?

The weekly proof is unaffected: it measures served latency and
availability and will keep producing a dated report every Sunday. What
stays unevidenced is the 300 rps soak row in
[§What we're proving](#what-were-proving), and the launch-readiness
backlog L5.4 "documented-acceptance" path is the recorded way to close
Task #77 without it. Whichever way that goes, the gap belongs in the
report's own "does not prove" section — which the generator writes
automatically — and in the backlog, never in a blank cell that reads like
a pass.

## References

- Weekly proof generator (the source of truth for the weekly report's
  shape):
  [`scripts/ops/sla-proof-from-probe.sh`](../../scripts/ops/sla-proof-from-probe.sh)
- Weekly proof generator self-test:
  [`scripts/ops/sla-proof-from-probe-test.sh`](../../scripts/ops/sla-proof-from-probe-test.sh)
- The probe the weekly proof aggregates:
  [`cmd/stellarindex-sla-probe/main.go`](../../cmd/stellarindex-sla-probe/main.go)
  and its alerts
  [`deploy/monitoring/rules/sla-probe.yml`](../../deploy/monitoring/rules/sla-probe.yml)
- Scheduled producer:
  [`.github/workflows/sla-proof-weekly.yml`](../../.github/workflows/sla-proof-weekly.yml)
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
