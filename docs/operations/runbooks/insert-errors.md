---
title: Runbook — insert-errors
last_verified: 2026-08-29
status: current
severity: P2
---

# Runbook — `stellarindex_ingestion_insert_errors`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingestion_insert_errors` (the sensitive `stellarindex_ingestion_persist_drop` sibling on the money-flow kinds shares this runbook) |
| Severity | P2 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/ingestion.yml` (group `stellarindex.ingestion`, `severity: ticket`, `for: 5m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/ingestion.yml`. |
| Typical MTTR | 15–60 min |
| Impact | Events failing to persist. Post-ADR-0041 (2026-07-06) infrastructure faults are NOT lost — they retry with backpressure (the cursor stalls; see [trade-insert-backpressure.md](trade-insert-backpressure.md)). A firing here therefore means **GENUINE loss**: a permanent data fault (`kind=trade`) or external retry-buffer overflow (`kind=dropped`) — mirroring the alert's own annotation. Downstream: price staleness, missing rows. |

## Symptoms

- `stellarindex_source_insert_errors_total{source=...,kind=...}` rises above 6/min sustained. `kind` is not just `trade|oracle`: the counter also carries `panic` (unhandled decode/persist panic, recovered in the sink), `dropped` (external retry-buffer overflow, ADR-0041), and the per-domain persist kinds — of which the money-flow set `soroswap_router_swap` / `defindex_flow_strategy` / `defindex_flow_vault` has its own SENSITIVE any-nonzero tripwire (`stellarindex_ingestion_persist_drop`, `increase(...[15m]) > 0`) that shares this runbook_url, because a low-rate silent drop sits below the 0.1/s threshold here.
- `stellarindex_source_events_total` may still rise — the consumer is pulling events, it's the writer that's failing.
- Dashboard view: *Ingestion → Insert errors* panel non-zero for > 5 min.
- The offending source's `stellarindex_source_last_event_unix` may freeze (if persistence blocks until retry).

## Quick diagnosis (≤ 5 min)

```sh
# Which source + which kind is failing? (9464 = the indexer's
# metrics port on r1; the API serves its own metrics on :3000.)
ssh root@136.243.90.96 'curl -s localhost:9464/metrics | grep stellarindex_source_insert_errors_total'

# Is it the storage layer itself?
stellarindex-ops rpc-probe https://mainnet.sorobanrpc.com   # rules out upstream — r1 has no local stellar-rpc, point at a public endpoint
ssh root@136.243.90.96 'runuser -u postgres -- psql -d stellarindex -c "SELECT now(), pg_is_in_recovery();"'

# Actual failure reason is in the indexer's logs:
journalctl -u stellarindex-indexer --since -2h | grep -E "insert (trade|oracle update) failed" | tail
```

If the log line says:
- `connection refused` → Timescale is down or network partitioned. Jump to `timescale-primary-down.md`. Note: for `kind=trade` an infrastructure fault like this now lands in `stellarindex_ingestion_trade_insert_backpressure` (the ADR-0041 retry path), not here — if you ARE seeing it here, the retry buffer overflowed (`kind=dropped`).
- `disk full` / `no space` → Timescale volume out of space. Free space on the ZFS pool or evict old chunks (see db-disk-full.md).
- `duplicate key value` → should be impossible; the idempotent ON CONFLICT swallows these. If you see this, the primary-key invariant is broken and this is a data-integrity incident, not a capacity one. Escalate.
- `violates check constraint` → a source sent malformed data (negative amounts, bad tx_hash). Decode bug, not a storage bug; check `stellarindex_source_decode_errors_total` on the same source.

## Mitigation (≤ 15 min)

Post-ADR-0041 (2026-07-06) infra-fault writes retry with backpressure and are NOT lost — the cursor stalls instead (see [trade-insert-backpressure.md](trade-insert-backpressure.md)). Events counted HERE are genuinely lost: a permanent data fault (`kind=trade`) or retry-buffer overflow (`kind=dropped`) — see the alert's own annotation. Prioritise:

- [ ] Step 1 — stop the bleeding. If Timescale is the root cause, follow `timescale-primary-down.md` first; insert errors are a symptom.
- [ ] Step 2 — if disk-full: extend the underlying volume (the
      production deployment uses bare-metal NVMe + ZFS per
      [ADR-0008](../../adr/0008-ha-topology.md), not Kubernetes —
      grow via `zpool` / Hetzner volume-resize console). Let the
      indexer auto-retry once `df` reports headroom; then backfill
      the gap:
  ```sh
  # 1. Identify the affected range. detect-gaps is cursor-vs-tip
  #    only; find-data-gaps reads the data tables and reports the
  #    actual missing (from, to) ledger ranges.
  stellarindex-ops detect-gaps -config /etc/stellarindex.toml \
      -threshold 50
  stellarindex-ops find-data-gaps -config /etc/stellarindex.toml \
      -source <SOURCE_NAME> -output text
  # 2. Backfill the named range — dry-run first to confirm scope.
  stellarindex-ops backfill -config /etc/stellarindex.toml \
      -from <FIRST_LEDGER> -to <LAST_LEDGER> \
      -source <SOURCE_NAME> -dry-run
  # 3. Drop -dry-run to commit.
  stellarindex-ops backfill -config /etc/stellarindex.toml \
      -from <FIRST_LEDGER> -to <LAST_LEDGER> \
      -source <SOURCE_NAME> -resume
  ```
- [ ] Step 3 — if the underlying issue is fixed: watch the rate decline. Alert clears when the 5m rate drops below 0.1/s.
- [ ] Verification: `stellarindex_source_insert_errors_total` stops incrementing (use `rate()[1m]` to see it go to 0).

## Root cause analysis

For the postmortem, gather:
- Timescale logs from `/var/log/postgresql/` covering the affected window.
- `stellarindex_source_insert_errors_total` series by `(source, kind)` over the incident.
- Indexer stderr with the full error strings (Timescale wraps them verbosely).
- Did the alert fire for ONE source or ALL? One-source = decode/schema issue; all-sources = shared storage issue.

## Known false-positive patterns

- None yet. This alert's threshold (6/min sustained 5 min) was chosen to ride through a 1-commit-at-a-time restart; we haven't seen legitimate brief spikes.

## Related

- `trade-insert-backpressure.md` — the ADR-0041 retry path; where an infra fault on `kind=trade` surfaces now.
- `timescale-primary-down.md` — root cause when the shared storage layer is the issue.
- `decode-errors.md` — decode failures look similar but are upstream (source-side).
- ADR-0003 (i128 precision) — check-constraint violations here indicate a decoder is sending values that violate NUMERIC bounds.

## Changelog

- 2026-04-23 — initial draft after the `SourceInsertErrorsTotal` alert landed.
- 2026-04-30 — rpc-probe URL points at a public stellar-rpc; r1
  doesn't run its own (removed 2026-04-23).
- 2026-08-29 — re-verified against HEAD: post-ADR-0041 loss model
  ("events are LOST" was stale — infra faults retry with
  backpressure; a firing means genuine loss), `kind` label set
  extended (panic / dropped / persist_drop siblings), r1 command
  shapes (indexer :9464, runuser psql), config path corrected to
  `/etc/stellarindex.toml`, find-data-gaps added as the
  data-derived range tool, dual-tree Detected-by. Status draft →
  current.
