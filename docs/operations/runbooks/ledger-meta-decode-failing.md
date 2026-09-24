---
title: Runbook — stellarindex_ledger_meta_decode_failing
last_verified: 2026-09-24
status: active
severity: P1 | P3
---

# Runbook — `stellarindex_ledger_meta_decode_failing` / `stellarindex_ledger_meta_decode_probe_stale`

## At a glance

| | |
|---|---|
| **Alerts** | `stellarindex_ledger_meta_decode_failing` (`stellarindex_ledger_meta_decode_failures_total > 0` for 10m, page) · `stellarindex_ledger_meta_decode_probe_stale` (probe metric age > 1h for 30m, ticket) |
| **Detected by** | `deploy/monitoring/rules/stellar-stack-version.yml` (and `configs/prometheus/rules.r1/stellar-stack-version.yml`) |
| **What it means** | A component cannot decode ledger meta the network is now producing — almost always "we are behind a protocol upgrade": the installed galexie / indexer predates an XDR change (e.g. P28's `ParallelTxExecutionStage`) and refuses the ledger. |
| **Data loss** | None — the decode is fail-closed. The affected component has STOPPED ingesting and will not resume on its own. |
| **Probe-stale variant** | `stellarindex_ledger_meta_decode_probe_stale` means the probe itself hasn't reported in over an hour, so a real decode failure would go unseen. Check the `ledger-meta-decode-probe.timer` / `.service` units. |

## Fix

1. Identify the affected unit from the `unit` label.
2. Check `stellarindex_stellar_stack_version_lag` (see the
   [stellar-stack-version-lag runbook](stellar-stack-version-lag.md)) to
   confirm which component and target version.
3. Follow the [protocol-upgrade procedure](../protocol-upgrades.md) to
   bump `galexie_version` / `stellar_core_version` / the release
   binary's `go-stellar-sdk` and re-apply.
4. Confirm `stellarindex_ledger_meta_decode_failures_total` stops
   increasing and the component's ingest cursor resumes advancing.

## Related

- [stellar-stack-version-lag runbook](stellar-stack-version-lag.md) —
  the proactive signal this alert backstops.
- [Protocol upgrades](../protocol-upgrades.md) — the upgrade procedure.
- [Alerts catalog](../alerts-catalog.md).
