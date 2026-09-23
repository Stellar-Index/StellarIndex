---
title: Runbook — dispatcher-tx-skips
last_verified: 2026-09-23
status: current
severity: P3
---

# Runbook — `stellarindex_ingestion_dispatcher_tx_skips`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingestion_dispatcher_tx_skips` |
| Severity | P3 (`severity: ticket`) |
| Detected by | `deploy/monitoring/rules/ingestion.yml` and the R1 overlay `configs/prometheus/rules.r1/ingestion.yml` (group `stellarindex.ingestion`, `for: 0m`) |
| Typical MTTR | 15–60 min |
| Impact | The dispatcher (`internal/dispatcher/dispatcher.go`) skipped a whole transaction rather than decoding it. Downstream this reads as an empty ledger, not a failure — the completeness reconcile would still pass. |

## What this fires on

Three process-wide counters, each incremented when `ProcessLedger` gives up
on a transaction instead of decoding it:

| Metric | Where it's incremented | Meaning |
| ------ | ----------------------- | ------- |
| `stellarindex_dispatcher_tx_read_errors_total` | `internal/dispatcher/dispatcher.go` (tx read) | The transaction itself was malformed and was skipped. |
| `stellarindex_dispatcher_tx_event_read_errors_total` | `internal/dispatcher/dispatcher.go` (`GetTransactionEvents`) | That transaction's Soroban events failed to read (G15-06) — every Soroban event in it is dropped. |
| `stellarindex_dispatcher_entry_meta_unsupported_total` | `internal/dispatcher/dispatcher.go` (apply-phase entry-change walk) | An unhandled `TransactionMeta` version stopped the entry-change walk for that tx — every classic balance/trustline/offer/LP change in it is skipped. |

`internal/dispatcher/statsflush/flusher.go` mirrors each counter's delta as
a WARN log on every 5-minute flush window (RLT-135); this alert is the
Prometheus-side signal so a sustained climb doesn't depend on someone
tailing logs.

## Quick diagnosis (≤ 5 min)

```sh
# Which of the three counters moved, and by how much?
ssh root@136.243.90.96 'curl -s localhost:9464/metrics | grep -E "stellarindex_dispatcher_(tx_read_errors|tx_event_read_errors|entry_meta_unsupported)_total"'

# The exact failing ledger/tx is in the indexer's logs — statsflush's WARN
# fires the same window this alert does.
journalctl -u stellarindex-indexer --since -2h | grep -E "dispatcher: (tx-read errors|tx-event read errors|unsupported TransactionMeta)"
```

- `tx_read_errors` climbing → a malformed transaction is reaching the
  dispatcher. Check whether the upstream ledger source (captive core /
  RPC) is serving a corrupt or truncated ledger.
- `tx_event_read_errors` climbing → `GetTransactionEvents` is failing,
  most often a stellar-go XDR/SDK version behind a protocol upgrade that
  changed the Soroban events encoding. Check the deployed `stellar-go`
  version against the network's current protocol.
- `entry_meta_unsupported` climbing → a `TransactionMeta` version the
  apply-phase walk doesn't handle, almost always a protocol upgrade that
  shipped a new meta version ahead of a stellar-go/dispatcher bump.

## Mitigation (≤ 15 min)

- [ ] Step 1 — confirm which counter(s) moved and correlate with any
      recent Stellar protocol upgrade or captive-core/RPC version change.
- [ ] Step 2 — if a protocol upgrade shipped a new `TransactionMeta`
      version or events encoding: bump the vendored `stellar-go` /
      protocol-support version and ship a dispatcher release that handles
      it. This is a code fix, not an operational mitigation — the skip is
      permanent until the dispatcher is patched.
- [ ] Step 3 — once the fix ships, re-derive the affected ledger range.
      The raw Soroban events are durable in the CH lake (ADR-0034); the
      apply-phase entry changes require a replay from the affected ledger
      (`stellarindex-ops projector-replay` per ADR-0032, or a full
      re-ingest of the range if the classic-side tables need it too).

## Root cause analysis

For the postmortem, gather:
- The exact ledger range the WARN logs cover (`ledger_from`/context in the
  surrounding dispatcher logs).
- The stellar-go / protocol-support version deployed at the time.
- Whether the network had a protocol upgrade in the affected window.

## Known false-positive patterns

- None yet. All three counters are zero in steady state; any nonzero
  increase is a dispatcher gap, not a rate to tolerate.

## Related

- `insert-errors.md` — storage-layer write failures; a different failure
  mode (the row decoded fine but couldn't persist).
- ADR-0029 (raw-event landing zone) / ADR-0032 (projector cursor replay) /
  ADR-0034 (CH lake re-derive).

## Changelog

- 2026-09-23 — created (RLT-143 / #615: the counters were promoted to
  Prometheus by RLT-135 but had no alert or runbook).
