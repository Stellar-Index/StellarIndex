---
title: Runbook — metrics-registry-absent
last_verified: 2026-07-16
status: draft
severity: P3
---

# Runbook — `stellarindex_metrics_registry_absent`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_metrics_registry_absent` |
| Severity | P3 (informational — monitoring-coverage gap, not an active outage) |
| Detected by | Prometheus rule in `deploy/monitoring/rules/metrics-registry.yml` |
| Typical MTTR | code change + deploy (this is a wiring regression, not an incident) |
| Impact | A component is running WITHOUT a Prometheus Registry, so the metrics it would export are never registered and any alert built on them can NEVER fire. A silent hole in monitoring coverage. |

## Background (audit-2026-07-16 C4-4)

Some components accept an optional `*prometheus.Registry` and, when it
is nil, simply skip registering their metrics. That is convenient for
tests but dangerous in production: an alert whose source metric is
never registered is DEAD — it evaluates against "no data" forever and
can never fire, so the failure it was meant to catch goes unnoticed.

`internal/obs/metrics.go` exports the gauge
`stellarindex_metrics_registry_present{component}` — set to `1` at boot
when the named component received a Registry, `0` when it is running
Registry-less. This alert fires on a present `0`. An ABSENT series
means "this binary doesn't use the component" and is intentionally not
alerted.

## The known case: `component="ledgerstream"`

`internal/ledgerstream` registers its SDK `BufferedStorageBackend`
buffer metrics (`buffer_fetch_latency_seconds` etc., via the SDK's
`WithMetrics` / `ApplyLedgerMetadata`) ONLY when `Config.Registry != nil`.

The production builder `pipeline.LedgerstreamConfig` leaves `Registry`
nil **on purpose**: the live indexer calls `ledgerstream.Stream`
repeatedly (archive range → live tail → each ch-live-catchup
tip-extend), and the SDK's metric registration is not idempotent — the
second call with the same registry panics with a duplicate-registration
error. Leaving the registry nil is the current way to avoid that panic.

Consequence: the SDK buffer metrics (`buffer_fetch_latency_seconds`
etc.) are not exported in production. That is a low-value operational
coverage gap, not a dead page.

> **NOTE (W5-mon-3):** this alert USED to also mean the
> `TieredDataStore` metrics were dead and the ledgerstream-tier
> `both_missing` P1 page was inert. That is **no longer true.**
> `stellarindex_ledgerstream_tier_read_total` and
> `stellarindex_ledgerstream_cold_read_duration_seconds` are now
> `internal/obs` package-level metrics registered unconditionally at
> boot, so the `both_missing` page is **live in production regardless of
> this gauge's value**. This alert now flags only the SDK buffer-metric
> coverage gap.

## What to do

1. Confirm which component: check the `component` label on the firing
   series (`stellarindex_metrics_registry_present == 0`).
2. For `ledgerstream`, this is the known state, not a new regression,
   and it now affects only the SDK buffer metrics (the `both_missing`
   page is unaffected — see the note above). If you want the buffer
   metrics too, the fix is a code change, not an ops action:
   - Make the SDK metric registration idempotent — gate the SDK
     `WithMetrics` / `ApplyLedgerMetadata` calls behind a package-level
     `sync.Once` or an `AlreadyRegisteredError`-tolerant register, so
     repeated `Stream` calls don't panic.
   - Then wire `obs.Registry` (+ a `RegistryNamespace`) through
     `pipeline.LedgerstreamConfig`.
   - After deploy, `stellarindex_metrics_registry_present{component="ledgerstream"}`
     flips to `1` and this alert clears.
3. For any other component that starts reporting `0`, treat it as a
   wiring regression: something stopped passing the Registry into that
   component's constructor. Restore the wiring.

## Verifying the fix

After the change, `curl` the indexer's `/metrics` and confirm:

- `stellarindex_metrics_registry_present{component="ledgerstream"} 1`
- the SDK buffer metric `stellarindex_ledgerstream_buffer_fetch_latency_seconds`
  (or the SDK's namespaced equivalent) is present.

(`stellarindex_ledgerstream_tier_read_total` is present independent of
this gauge — it is registered at boot regardless.)

## Related

- `ledgerstream-tier-both-missing.md` — the P1 page that is now LIVE
  regardless of this gauge (W5-mon-3), no longer gated on it.
- `internal/pipeline/datastore.go` — `LedgerstreamConfig`, the builder
  that leaves `Registry` nil (affects only the SDK buffer metrics now).
- `internal/ledgerstream/tiered.go` — the `TieredDataStore`, whose tier
  metrics are sourced from `internal/obs` (always registered).

