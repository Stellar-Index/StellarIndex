# Severity Rubric

Severity is *not* a feeling — it is a deterministic mapping from a
finding's worst-case business impact to one of five tiers.

When in doubt, a finding goes one tier *higher*, not lower. We
will downgrade later with evidence rather than upgrade later with
regret.

## Tiers

### `critical`

A finding qualifies as critical if any of the following is true:

- **Data loss or corruption** that cannot be reconstructed from
  galexie + immutable upstream archives within the documented
  RTO/RPO (per `docs/architecture/ha-plan.md`).
- **Security breach** where a remote attacker can read or write
  data not authorised to them (data ex-filtration, key compromise,
  privilege escalation, billing meter bypass at scale).
- **Money loss** — billing surface that allows free use of paid
  capacity, or charges users for capacity they didn't consume,
  or refunds bypassable.
- **Brand-ending incident** — public, sustained wrong-data serving
  that would land in trade-press coverage.
- **Customer-contract violation** — a documented commitment in
  RFPs or proposal that we cannot meet on launch day.
- **Ingest correctness gap that re-occurs** — e.g. a fresh
  AsyncSink-style cursor-drop incoherence that we already
  documented but didn't gate.

Wave 0 (block public flip).

### `high`

A finding qualifies as high if any of the following is true:

- **Silent bad data.** API returns plausible but wrong values
  with no `divergence_warning` / `stale` / `reduced_redundancy`
  flag. The user can't tell.
- **Prolonged outage with no detection.** The system breaks but
  no alert fires, no runbook covers it, no metric drifts.
- **Operability gap that gates the public flip.** Operators
  cannot recover from a documented failure mode.
- **CG/CMC parity gap on a launch-headline feature.** A surface
  the launch announcement claims we offer is not actually
  shipping.
- **Stellar-depth gap** that makes us look indistinguishable from
  CG/CMC on the differentiator surfaces.
- **Lurking ADR violation** that is not currently exploited but
  would be exploited the first time the surface is reused.
- **Granular-coverage gap (W35)** — a decoder claims a contract
  but classifies only some of its emitted events.

Wave 0 or Wave 1.

### `medium`

A finding qualifies as medium if any of the following is true:

- **Degraded UX.** API contract is met but the response is harder
  to consume than necessary (inconsistent naming, missing field
  in some responses, slow latency under load).
- **Observability gap.** A metric exists but no alert; an alert
  exists but no runbook; a runbook exists but no firing test;
  a structured-log field expected by Loki dashboards isn't
  emitted.
- **Operator confusion.** Two paths achieve the same outcome
  with different semantics; documentation says one is preferred
  but reality is the other.
- **Generated-artifact drift.** OpenAPI doesn't match handlers;
  Postman doesn't match curl examples.
- **Pre-launch issue, not launch-blocking.** Something that
  embarrasses us on day 30, not day 1.

Wave 1 or Wave 2.

### `low`

A finding qualifies as low if any of the following is true:

- **Doc drift.** Comments contradict code; ADR mentions a
  function that's been renamed; README points to an outdated
  command.
- **Naming inconsistency.** Two packages use different
  conventions for the same concept.
- **Minor inefficiency.** A query could use an index that exists;
  a CAGG could be materialized in fewer steps.
- **Dead code.** Unused exports, retired packages still
  importable.

Wave 2 or Wave 3.

### `note`

Informational. No action required. Examples:

- A design decision that we accept as a trade-off
- A risk we explicitly accept (with operator confirmation)
- Context for future auditors

## Adversarial Multipliers

These modifiers tip a finding one tier higher (or sometimes two):

| Modifier | Effect |
| --- | --- |
| **Discovery cost asymmetry.** Easy for an attacker / hard for us to detect. | +1 tier |
| **Public mention by competitor.** Already publicly known weakness in our space. | +1 tier |
| **Money-touching.** Anywhere billing/usage/credits could be manipulated. | +1 tier (minimum medium) |
| **Boundary surface.** First entry point for external data (vendor API, ledger XDR, customer webhook URL). | +1 tier |
| **Repeat offender.** Same class of bug we've already shipped a fix for. | +1 tier |
| **Cross-cutting.** Bug affects multiple sources / multiple endpoints / multiple regions. | +1 tier |

A finding's final severity = max(base, base + modifiers).

## Anti-Patterns (do NOT do these)

- "It's only theoretical" — if reachable in principle, base it
  on reachability, not exploitability today.
- "Operator will catch it" — if it requires operator vigilance
  to prevent harm, that's an operability gap, not a defence.
- "Customer can flag it" — silent bad data is not the customer's
  job to detect.
- "Tests would catch a regression" — only if tests are gating
  AND assert the specific invariant.
- "It's documented" — documentation is a hint, not a defence.

## Examples (calibration)

- **critical:** soroban_events PushEvent silently drops 0.4% of
  rows AND cursor advances past dropped sequences → SQL backfill
  inherits gap → 100% density mission impossible. (Closed by
  rc.80 back-pressure fix; if it re-occurred via a similar
  pattern in a new sink, that's `critical`.)
- **critical:** API key exfiltration via debug endpoint.
- **high:** A decoder for Source X claims a contract and
  silently drops 3 of 5 emitted events → silent bad data on
  every consumer query against Source X (this is the W35
  framing).
- **high:** Stripe webhook accepts replays → potential refund
  bypass.
- **high:** verify-archive walker doesn't tolerate trailing-edge
  → timer disabled for an extended period → chain integrity
  silently unmonitored. (Closed by rc.81 TolerateTrailingMissing.)
- **medium:** Prometheus rule references metric name that
  doesn't exist post-rename.
- **medium:** runbook at `docs/operations/runbooks/X.md` cites
  dashboards that don't exist in our Grafana provisioning.
- **low:** ADR-0026 references a function name renamed in
  rc.71.
- **low:** generated `docs/reference/api/` lags the OpenAPI
  spec by 2 commits.
- **note:** explicit operator preference to commit per-unit on
  main during the live-in-development phase (memory entry
  `feedback_commit_cadence`). Not a finding; informational.
