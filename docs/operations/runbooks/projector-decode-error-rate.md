---
title: Runbook — projector decode-error rate high
last_verified: 2026-08-14
status: draft
severity: P3
---

# Runbook — `stellarindex_projector_decode_error_rate_high`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_projector_decode_error_rate_high` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/projector.yml` + `configs/prometheus/rules.r1/projector.yml` — `sum by (source) (rate(stellarindex_projector_events_decoded_total{outcome="decode_error"}[10m])) > 0.1` for 15m |
| Typical MTTR | minutes to confirm the regression; a decoder fix + re-drive to fully recover |
| Impact | Every event of the affected class is SILENTLY dropped from the served per-source tier while the cursor advances past it. Not data loss (the raw events still live in the ClickHouse lake), but a real, growing gap in projected/served data until the decoder is fixed and the range is re-driven. |

## Why this exists

DATA-6 / NS-2 (audit-2026-08-14). The ADR-0032 projector decodes raw lake
rows with the SAME per-source decoders as live ingest. A decode soft-fail
— a returned decode error or a recovered decoder panic
(`internal/projector/projector.go`, `processEventSafely`) — is counted as
`stellarindex_projector_events_decoded_total{source,outcome="decode_error"}`,
the row is SKIPPED, and the cursor advances past it.

Skipping is correct for a rare, scattered poison row (bad on-chain data
that would re-fail on every retry) and holding instead would re-wedge a
sole-writer source on a deterministic failure (COR-11). But a shipped
decoder **regression** breaks a whole *class* of valid events the same way
— the proven phoenix 5,161-orphaned-swap / I-L4 class — draining every
affected event from the served tier while the cursor sails to tip. The
per-cycle signal (`stellarindex_projector_runs_total{outcome=
"decode_degraded"}`) marks such a cycle as no-longer-`ok`, but the cursor
still advances by design. This alert is the defence that distinguishes a
regression (a **sustained** decode_error rate) from the odd poison row
(below threshold) and pages before a reconcile notices months later.

## Symptoms

- `rate(stellarindex_projector_events_decoded_total{source="X",outcome="decode_error"}[10m])`
  is sustained above `0.1/s` for one or more sources.
- `stellarindex_projector_runs_total{source="X",outcome="decode_degraded"}`
  is incrementing (cycles that advanced the cursor while dropping rows).
- Indexer journal: `projector decode panicked; skipping row` and/or the
  cycle summary log line shows a nonzero `decode_errors` for the source.
- Projector lag (`stellarindex_projector_lag_ledgers`) may look HEALTHY —
  the cursor is advancing normally; that is exactly the trap this alert
  closes.

## Quick diagnosis (≤ 5 min)

```sh
# Which sources are spiking, and how fast?
curl -s http://indexer:9464/metrics | grep 'stellarindex_projector_events_decoded_total{.*decode_error'

# The decode failures themselves — panic stacks name the offending decoder.
journalctl -u stellarindex-indexer --since -1h \
  | grep -iE "projector decode panicked|decode_errors=[1-9]"
```

A single source spiking right after a deploy that changed a decoder is the
textbook regression. Multiple sources at once points at a shared
decode/reconstruct helper.

## Mitigation

- [ ] Identify the offending decoder from the panic/error in the journal
      (`internal/sources/<protocol>/decode.go`). If the spike started at a
      deploy, diff that decoder against the previous release.
- [ ] Fix the decoder regression (or roll back the offending change).
      Re-driving without fixing it just re-drops the same class.
- [ ] Re-drive the affected range once fixed:
      `stellarindex-ops projector-replay -source <X> -from <ledger>`
      (the raw events are still in the ClickHouse lake — nothing is lost).
- [ ] Verification: `decode_error` rate returns to ~0 for the source, no
      new `decode_degraded` cycles, and the ADR-0033 completeness verdict
      for the source returns to `complete=true` after the next
      `compute-completeness` run.

## Root cause analysis

`decode_error` is always a SECOND-order signal — the root cause is
whatever made a whole class of valid events undecodable: a field-mapping
regression (the phoenix 7-field swap class), an unhandled upgraded-WASM
event shape, or a panic on a specific payload. The projector runs the same
decoders as ingest, so a regression here usually has a sibling effect on
the live ingest path (`stellarindex_ingestion_decode_error`); check both.

## Known false-positive patterns

- A brief burst during a genuinely novel on-chain contract deployment
  (new event shape no decoder yet handles) can trip the threshold. That is
  still worth a ticket — it means a real class of events is going
  unprojected — but the fix is a decoder *addition*, not a regression
  rollback.

## Related

- `projector-row-quarantined.md` — the sink-side sibling (a downstream
  write, not a decode, was the un-processable failure).
- `projector-lag.md` — the projector's lag/cycle-error alerts; note this
  alert can fire while lag looks healthy.
- `completeness-incomplete.md` — a sustained decode drop on one source
  eventually shows up as `complete=false` on the ADR-0033 verdict.
- `internal/projector/projector.go` (`processEventSafely`,
  `cycleOneSource`) — the implementation.

## Changelog

- 2026-08-14 — initial draft, added alongside the alert (DATA-6 / NS-2,
  audit-2026-08-14 decode-regression remediation).
