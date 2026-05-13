---
title: Runbook — <alert-name>
last_verified: YYYY-MM-DD
status: draft | ratified | superseded
severity: P1 | P2 | P3
---

# Runbook — `<alert_name>`

<!--
Template for every per-alert runbook. Copy to
docs/operations/runbooks/<alert-name>.md and replace the
placeholder sections. Keep it short — a responder woken at 3 AM
reads this first, so structure and speed matter more than
completeness.

The two universally-required sections are `## At a glance` and
`## Related` — orphan-runbook lint at scripts/ci/lint-docs.sh
fails any runbook with no inbound references; "At a glance" is
the contract every other runbook expects when linking in. The
remaining sections below are the typical shape, but vary by
alert class — drop or reshape as needed.

When the alert covers a CLASS of failures that vary by source,
tier, or operation label, add a per-class reference matrix
instead of (or alongside) the "Quick diagnosis" section. See
`decode-errors.md` (per-source decode-regression matrix),
`source-stopped.md` (per-source cadence reference),
`external-poller-error-rate-high.md` (vendor-specific 429
patterns), and `stripe-platform-sync-errors.md`
(per-`operation` triage paths) for the established pattern.

When the alert is for a NEW metric/seam that didn't exist
before, add a `## Why this exists` section explaining the
operator question that motivated wiring it (see
`stripe-platform-sync-errors.md`).

When this runbook has a companion runbook covering the
adjacent surface (inbound vs outbound, classic vs Soroban,
producer vs consumer), cross-link from BOTH sides — wave 75
caught this drift between the two webhook runbooks.
-->

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `<alert_name>` |
| Severity | P1 / P2 / P3 |
| Detected by | Prometheus rule in `deploy/monitoring/rules/<area>.yml` (and `configs/prometheus/rules.r1/<area>.yml` if R1-overlay applies) |
| Typical MTTR | X min |
| Impact | One sentence describing customer impact. |

## Symptoms

What the alert is telling you. 1–3 bullets. Include the expected
dashboard view and the specific metric value range.

## Quick diagnosis (≤ 5 min)

Three or four commands / checks to run first. Each should produce
a clear signal of "yes this is real" or "false alarm."

```sh
# example
systemctl status <unit> / journalctl -u <unit> / psql ... / curl ...
```

## Mitigation (≤ 15 min)

Steps to bring the service back to green. Prefer reversible
mitigations (fail-over, drain, reset) over forward-fixes during
the incident window.

- [ ] Step 1 — verb + target.
- [ ] Step 2.
- [ ] Verification: the metric that should clear the alert within
      {N} seconds after mitigation.

## Root cause analysis

What to gather for the postmortem. Log files, metric screenshots,
subsystem-specific diagnostics that'd take > 5 min to run.

## Known false-positive patterns

Scenarios where this alert fires but no customer impact exists.
Each documented here is one less 3 AM page for the next responder.

## Related

- Implementation file (`internal/<package>/...`).
- Companion runbook(s) covering adjacent surfaces — name the
  inbound/outbound or producer/consumer relationship explicitly.
- Postmortems tagged `<alert-name>` — `docs/operations/postmortems/`.
- Upstream docs / ADRs.

## Changelog

- YYYY-MM-DD — initial draft by @x.
- YYYY-MM-DD — revised mitigation step 2 after incident NNN.
