---
title: Runbook — uncorroborated-calls
last_verified: 2026-09-28
status: current
severity: P3
---

# Runbook — `stellarindex_ingestion_uncorroborated_calls`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingestion_uncorroborated_calls` |
| Severity | P3 (`severity: ticket`) |
| Detected by | `deploy/monitoring/rules/ingestion.yml` and the R1 overlay `configs/prometheus/rules.r1/ingestion.yml` (group `stellarindex.ingestion`, `for: 0m`) |
| Typical MTTR | 15–60 min |
| Impact | An oracle-class `ContractCall` was declared in a transaction's auth tree but never executed, so the dispatcher (`internal/dispatcher/dispatcher.go`) refused it before `Decode` (W8.4a). Either a price-forgery attempt was rejected, or the legitimate routing shape changed and started refusing calls that should decode. |

## What this fires on

`stellarindex_source_uncorroborated_calls_total`, a per-source counter
incremented in `internal/dispatcher/dispatcher.go`'s `bumpUncorroborated`
whenever the auth-tree walk finds an oracle-class `ContractCallDecoder`
invocation declared but never corroborated by an executed call in the
same transaction — the defence W8.4a added against a forged auth entry
naming an oracle contract with fake price args.

`internal/dispatcher/statsflush/flusher.go` mirrors each source's delta
as a WARN log on every 5-minute flush window; this alert is the
Prometheus-side signal so a rejected forgery attempt doesn't depend on
someone tailing logs.

## Quick diagnosis (≤ 5 min)

```sh
# Which source(s) moved, and by how much?
ssh root@136.243.90.96 'curl -s localhost:9464/metrics | grep stellarindex_source_uncorroborated_calls_total'

# The exact tx is in the indexer's logs — statsflush's WARN fires the
# same window this alert does.
journalctl -u stellarindex-indexer --since -2h | grep "dispatcher: uncorroborated oracle calls"
```

- A single isolated increment on `band` (the only current oracle-class
  source) with no accompanying routing or deploy change → treat as a
  rejected forgery attempt. The dispatcher already refused it; no data
  was corrupted. Confirm by cross-referencing the tx hash from the WARN
  against the source ledger for anything else unusual in the same auth
  tree.
- A sustained climb correlated with a recent contract upgrade or a new
  legitimate call pattern → the routing shape changed and the
  corroboration check (W8.4a) is now false-positiving on real calls.
  Check the oracle contract's current WASM against the corroboration
  logic's assumptions.

## Mitigation (≤ 15 min)

- [ ] Step 1 — pull the WARN-logged tx hash(es) for the flush window and
      inspect the auth tree: does the declared call plausibly belong to
      an attacker, or does it look like normal traffic the corroboration
      check now misclassifies?
- [ ] Step 2 — forgery attempt: no mitigation needed, the call was
      already refused; file it for the security log and move on.
- [ ] Step 3 — routing-shape change: this is a code fix (adjust the
      corroboration check for the new legitimate shape), not an
      operational mitigation. Ship a dispatcher release once confirmed.

## Root cause analysis

For the postmortem, gather:
- The WARN-logged tx hash(es) and their full auth trees.
- Whether the oracle contract's WASM changed recently (ADR-0035: gate on
  contract identity, not topic alone — a WASM upgrade can change the
  shape corroboration expects).
- Whether the increment was isolated (one-off, consistent with a probed
  and rejected attack) or sustained (consistent with a routing change).

## Known false-positive patterns

- None yet. Steady state is zero; any nonzero increase needs eyes —
  either outcome (forgery or routing change) warrants review, so this
  alert does not distinguish them at fire time.

## Related

- `dispatcher-tx-skips.md` — the sibling dispatcher-level tripwire for
  whole-transaction skips; this one is per-source and oracle-specific.
- ADR-0035 (contract-identity gating) — why a shared topic across
  deployments can't be trusted alone, the same class of assumption
  W8.4a's corroboration check protects.

## Changelog

- 2026-09-28 — created (the counter existed with no metric, WARN, or
  alert consumer; added all three).
